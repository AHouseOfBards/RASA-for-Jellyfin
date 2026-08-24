// Command rasa-sync keeps the published address pointed at this connection.
//
// This is the one piece of RASA that stays installed. SPEC.md §3 requires the
// address record to track a WAN address that changes without warning, and that
// cannot be done by something that has been uninstalled.
//
// It is not a daemon. The OS scheduler runs it, it compares, it updates only
// if needed, it writes a heartbeat, and it exits. Nothing supervises it and it
// holds no state of its own beyond what the setup app left behind.
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

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/ddns"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/dynu"
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
	syncer := ddns.New(client, st.Hostname, layout.LastSyncFile(), log)

	if interval > 0 && !flagPassed("once") {
		return loop(ctx, syncer, interval, log)
	}

	out := syncer.RunOnce(ctx)
	if out.Err != nil {
		return out.Err
	}
	return nil
}

// loop is the fallback for platforms without a usable scheduler, and for
// running inside a container where there is no cron or Task Scheduler
// (SPEC.md §17).
func loop(ctx context.Context, s *ddns.Syncer, every time.Duration, log *logging.Logger) error {
	log.Info("running continuously", slog.Duration("interval", every))
	t := time.NewTicker(every)
	defer t.Stop()

	// Check immediately: waiting a full interval on startup leaves the record
	// stale exactly after a reboot, when it is most likely wrong.
	s.RunOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.RunOnce(ctx)
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
