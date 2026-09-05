// Command rasa-sync keeps the published address pointed at this connection,
// and is the only thing left that can notice when remote access breaks.
//
// This is the one piece of RASA that stays installed. SPEC.md §3 requires the
// address record to track a WAN address that changes without warning, and that
// cannot be done by something that has been uninstalled.
//
// It is not a daemon. The OS scheduler runs it, it compares, it updates only
// if needed, it writes down what it found, and it exits. Nothing supervises it
// and it holds no state of its own beyond what the setup app left behind.
//
// # The second job
//
// Because it is the only survivor, it is also the health check. Every ten
// minutes it confirms the address is current and that the proxy is answering
// with a certificate that is not about to run out, writes the answer to a file
// the recovery notes point at, and — when something is actually wrong — says
// so on the operating system's own channel for unattended failures. Without
// that, a revoked credential is silent for the ninety days it takes the
// certificate to expire.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/alert"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/ddns"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/dynu"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/health"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/paths"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/secrets"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/state"
)

var version = "dev"

// TokenEnvVar is where the scheduled task supplies the credential, matching
// what the service installer writes.
const TokenEnvVar = "RASA_DYNU_TOKEN"

func main() {
	var (
		once     = flag.Bool("once", true, "check the address once and exit")
		interval = flag.Duration("interval", 0, "run continuously at this interval instead of once")
		root     = flag.String("root", "", "override the data directory")
		showVer  = flag.Bool("version", false, "print version and exit")
		verbose  = flag.Bool("v", false, "log at debug level")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("rasa-sync %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := run(ctx, *root, *once, *interval, *verbose); err != nil {
		fmt.Fprintln(os.Stderr, "rasa-sync:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, root string, once bool, interval time.Duration, verbose bool) error {
	layout := paths.Default()
	if root != "" {
		layout = paths.UnderRoot(root)
	}

	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	log, err := logging.Open(layout.SyncLog(), logging.Options{Level: level})
	if err != nil {
		return fmt.Errorf("opening log: %w", err)
	}
	defer log.Close()

	st, err := state.NewStore(layout.StateFile()).Load()
	if err != nil {
		return fmt.Errorf("reading setup record: %w", err)
	}
	if st.Hostname == "" {
		return fmt.Errorf("no hostname recorded; setup has not completed")
	}
	log.Redactor().RegisterAddress(st.Hostname)

	token, err := credential(layout)
	if err != nil {
		return err
	}
	log.Redactor().RegisterSecret(token)

	client := dynu.New(token, dynu.WithLogger(log))
	syncer := ddns.New(client, st.Hostname, log)

	check := &checker{
		syncer:    syncer,
		layout:    layout,
		hostname:  st.Hostname,
		url:       st.URL(),
		port:      st.ListenPort,
		log:       log,
		escalator: &health.Escalator{StatePath: layout.AlertStateFile(), Raise: raiseAlert},
	}

	if interval > 0 && !flagPassed("once") {
		return loop(ctx, check, interval, log)
	}
	return check.run(ctx)
}

// checker runs one round: sync the address, look at the proxy, write the
// health file, and escalate if it is worth escalating.
type checker struct {
	syncer    *ddns.Syncer
	layout    paths.Layout
	hostname  string
	url       string
	port      int
	log       *logging.Logger
	escalator *health.Escalator
}

func (c *checker) run(ctx context.Context) error {
	out := c.syncer.RunOnce(ctx)

	proxy, expiry := health.CheckProxy(ctx, c.hostname, c.port)
	report := health.Report{
		Checked:    out.Checked,
		Hostname:   c.hostname,
		URL:        c.url,
		CertExpiry: expiry,
		File:       c.layout.LastSyncFile(),
		Checks: []health.Check{
			health.CheckAddress(out.Err),
			proxy,
		},
	}

	// Both of the following are best effort, and neither may turn a working
	// sync into a failed one. A machine that will not accept a file or a log
	// entry still has working remote access, and reporting otherwise would
	// send the user chasing the wrong thing.
	if err := health.Write(report.File, report); err != nil {
		c.log.Warn("could not write the health file", slog.Any("err", err))
	}
	if err := c.escalator.Consider(ctx, report); err != nil {
		c.log.Warn("could not report the problem to the system log", slog.Any("err", err))
	}

	if !report.Healthy() {
		c.log.Error("remote access is not healthy", slog.String("problems", report.Signature()))
	}
	return out.Err
}

// raiseAlert bridges the health package's platform-free level to the alert
// package, which is where the per-OS channel lives.
func raiseAlert(ctx context.Context, l health.AlertLevel, subject, body string) error {
	level := alert.LevelError
	if l == health.LevelInfo {
		level = alert.LevelInfo
	}
	return alert.Raise(ctx, level, subject, body)
}

// loop is the fallback for platforms without a usable scheduler, and for
// running inside a container where there is no cron or Task Scheduler
// (SPEC.md §17).
func loop(ctx context.Context, c *checker, every time.Duration, log *logging.Logger) error {
	log.Info("running continuously", slog.Duration("interval", every))
	t := time.NewTicker(every)
	defer t.Stop()

	// Check immediately: waiting a full interval on startup leaves the record
	// stale exactly after a reboot, when it is most likely wrong.
	_ = c.run(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			_ = c.run(ctx)
		}
	}
}

// credential reads the Dynu token.
//
// The environment is checked first because that is how both the scheduled task
// and a container supply it. The protected file is the fallback for a manual
// run, where no environment was set up.
func credential(layout paths.Layout) (string, error) {
	if v := os.Getenv(TokenEnvVar); v != "" {
		return v, nil
	}
	store := secrets.NewFileStore(layout.SecretFile())
	token, err := store.Get(secrets.DynuAPIKey)
	if err != nil {
		return "", fmt.Errorf("no credential found in %s or the stored credentials: %w", TokenEnvVar, err)
	}
	return token, nil
}

func flagPassed(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}
