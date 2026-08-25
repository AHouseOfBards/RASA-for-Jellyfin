// Command rasa is the RASA for Jellyfin setup application.
//
// It is a wizard, not a service. It runs once, configures a proxy, a
// certificate, a router mapping, a DNS record and Jellyfin itself, registers
// the two things that must outlive it, and then can be uninstalled without any
// of that stopping (SPEC.md §3).
//
// The interface is served from loopback and displayed in a browser. Everything
// it can do is in internal/wizard; this file resolves paths, opens the log,
// finds the bundled binaries, and gets out of the way.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/caddy"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/paths"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/recovery"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/secrets"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/service"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/state"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/ui"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/wizard"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

// staging selects Let's Encrypt's staging endpoint, and defaults to on.
//
// SPEC.md §19 is emphatic about this: production allows five failed
// validations per hostname per hour, and a day of debugging against it leaves
// you locked out of the thing you are debugging. So an unadorned "go build"
// gets staging, and a release sets -X main.staging=0 deliberately. The default
// is the safe one because the unsafe one has to be chosen.
var staging = "1"

func main() {
	var (
		root    = flag.String("root", "", "override the data directory (development use; default is the system location)")
		verbose = flag.Bool("v", false, "log at debug level")
		showVer = flag.Bool("version", false, "print version and exit")
		diag    = flag.Bool("diagnostics", false, "write a redacted diagnostic bundle and exit")
		withAdr = flag.Bool("include-address", false, "include your web address in the diagnostic bundle")
		noOpen  = flag.Bool("no-browser", false, "print the wizard's address instead of opening a browser")
		prod    = flag.Bool("production-certificates", false, "use Let's Encrypt production rather than staging")
		email   = flag.String("email", "", "optional address for certificate expiry notices")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("RASA for Jellyfin %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
		return
	}

	if *diag {
		if err := runDiagnostics(*root, *withAdr); err != nil {
			fmt.Fprintln(os.Stderr, "rasa:", err)
			os.Exit(1)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := run(ctx, options{
		root:       *root,
		verbose:    *verbose,
		openWindow: !*noOpen,
		production: *prod,
		email:      *email,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "rasa:", err)
		os.Exit(1)
	}
}

type options struct {
	root       string
	verbose    bool
	openWindow bool
	production bool
	email      string
}

func run(ctx context.Context, o options) error {
	layout := paths.Default()
	if o.root != "" {
		layout = paths.UnderRoot(o.root)
	}
	if err := layout.EnsureDirs(); err != nil {
		return fmt.Errorf("preparing %s: %w (administrator rights are required, or pass -root)", layout.Root, err)
	}

	level := slog.LevelInfo
	if o.verbose {
		level = slog.LevelDebug
	}
	log, err := logging.Open(layout.RASALog(), logging.Options{Level: level, EventBuffer: 32})
	if err != nil {
		return fmt.Errorf("opening log: %w", err)
	}
	defer log.Close()

	log.Info("rasa starting",
		slog.String("version", version),
		slog.String("os", runtime.GOOS),
		slog.String("arch", runtime.GOARCH),
		slog.String("root", layout.Root),
	)

	acme := caddy.ACMEProduction
	if staging == "1" && !o.production {
		acme = caddy.ACMEStaging
	}
	log.Decision("certificate authority", authorityName(acme),
		"development builds default to staging so that debugging cannot exhaust the production rate limit")

	w, err := wizard.New(wizard.Options{
		Layout:      layout,
		Log:         log,
		Store:       state.NewStore(layout.StateFile()),
		Secrets:     secrets.NewFileStore(layout.SecretFile()),
		Version:     version,
		ACMECA:      acme,
		Email:       o.email,
		CaddyBinary: findCaddy(layout, log),
		SyncBinary:  findSync(layout, log),
	})
	if err != nil {
		return err
	}

	srv, err := ui.New(w, log)
	if err != nil {
		return err
	}
	if err := srv.Start(); err != nil {
		return err
	}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Close(shutdown)
	}()

	// The address is printed before any attempt to open a browser, and always.
	//
	// Starting a browser is a fire-and-forget handoff to the OS: the launcher
	// returns success as soon as it has been started, which says nothing about
	// whether a window ever appeared. Running elevated is exactly where that
	// goes wrong, and a user left looking at an empty console with no address
	// has no way forward at all. Printing first costs one line and removes the
	// dead end.
	fmt.Println()
	fmt.Println("  Setup is running at:")
	fmt.Println("   ", srv.URL())
	fmt.Println()
	fmt.Println("  If a browser window did not open, copy that address into one.")
	fmt.Println("  Leave this window open until setup finishes.")
	fmt.Println()

	if o.openWindow {
		if err := ui.Open(srv.URL()); err != nil {
			log.Warn("could not open a browser", slog.Any("err", err))
		}
	}

	select {
	case <-srv.Done():
		log.Info("wizard finished")
	case <-ctx.Done():
		log.Info("wizard interrupted")
	}

	fmt.Printf("\nDetails saved to %s\n", layout.RecoveryFile())
	return nil
}

func authorityName(ca string) string {
	if ca == caddy.ACMEStaging {
		return "staging"
	}
	return "production"
}

// findCaddy locates the bundled proxy.
//
// A missing binary is not fatal here. Setup gets far enough to claim an address
// and write state before it needs one, and reporting the failure through the
// wizard is better than refusing to start with a message about packaging.
func findCaddy(layout paths.Layout, log *logging.Logger) string {
	exeDir := executableDir()
	path, err := caddy.FindBinary(layout.BinDir(), exeDir, filepath.Join(exeDir, "bin"))
	if err != nil {
		log.Warn("no bundled proxy binary was found", slog.Any("err", err))
		return ""
	}
	// Copied out of the install directory so that uninstalling RASA does not
	// take the running proxy with it (SPEC.md §3).
	staged, err := caddy.Stage(path, layout.BinDir())
	if err != nil {
		log.Warn("could not stage the proxy binary", slog.Any("err", err))
		return path
	}
	log.Info("proxy binary ready", slog.String("path", staged))
	return staged
}

// findSync locates the address-sync helper and stages it alongside the proxy,
// for the same reason: it is registered as a scheduled task that must keep
// running after RASA is removed.
func findSync(layout paths.Layout, log *logging.Logger) string {
	name := "rasa-sync"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	exeDir := executableDir()
	for _, dir := range []string{layout.BinDir(), exeDir, filepath.Join(exeDir, "bin")} {
		candidate := filepath.Join(dir, name)
		if fi, err := os.Stat(candidate); err != nil || fi.IsDir() {
			continue
		}
		if dir == layout.BinDir() {
			return candidate
		}
		staged, err := service.StageBinary(candidate, layout.BinDir(), name)
		if err != nil {
			log.Warn("could not stage the sync helper", slog.Any("err", err))
			return candidate
		}
		return staged
	}
	log.Warn("no address sync helper was found; the address will not follow a changing connection")
	return ""
}

func executableDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

// runDiagnostics writes a bundle without performing setup, so it works after
// RASA has been reinstalled purely to collect one.
func runDiagnostics(root string, includeAddresses bool) error {
	layout := paths.Default()
	if root != "" {
		layout = paths.UnderRoot(root)
	}
	log, err := logging.Open(layout.RASALog(), logging.Options{})
	if err != nil {
		return err
	}
	defer log.Close()

	// Register the stored credential so it cannot survive into the bundle even
	// though this path never otherwise reads it.
	if tok, err := secrets.NewFileStore(layout.SecretFile()).Get(secrets.DynuAPIKey); err == nil {
		log.Redactor().RegisterSecret(tok)
	}
	if st, err := state.NewStore(layout.StateFile()).Load(); err == nil && st.Hostname != "" {
		log.Redactor().RegisterAddress(st.Hostname)
	}
	return bundle(layout, log, includeAddresses)
}

// bundle produces a diagnostic zip on the desktop, or the working directory
// when there is no desktop. Task 13.
func bundle(layout paths.Layout, log *logging.Logger, includeAddresses bool) error {
	dest, err := os.UserHomeDir()
	if err != nil {
		dest = "."
	} else if d := filepath.Join(dest, "Desktop"); dirExists(d) {
		dest = d
	}

	path, err := recovery.WriteBundle(dest, recovery.BundleOptions{
		Layout:           layout,
		Redactor:         log.Redactor(),
		IncludeAddresses: includeAddresses,
		Version:          version,
	})
	if err != nil {
		return err
	}
	fmt.Printf("\nDiagnostics saved to:\n  %s\n\n", path)
	fmt.Println("Attach it to an issue at:")
	fmt.Println("  https://github.com/AHouseOfBards/RASA-for-Jellyfin/issues")
	return nil
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
