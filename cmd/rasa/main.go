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
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/caddy"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/instance"
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
		replace = flag.Bool("replace", false, "close any copy of RASA already running instead of asking")
	)
	// Before any output. A release build on Windows is linked -H=windowsgui and
	// has no console of its own; this reattaches to the shell's when there is
	// one, so running from a terminal still prints and double-clicking still
	// shows no window.
	outputVisible = attachParentConsole()

	flag.Parse()

	if *showVer {
		fmt.Printf("RASA for Jellyfin %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
		return
	}

	if *diag {
		if err := runDiagnostics(*root, *withAdr); err != nil {
			fatal(err)
		}
		return
	}

	// Setup needs administrator rights end to end, so ask for them before any
	// work rather than partway through it. A -root run is development and
	// needs nothing, which is the point of the flag.
	if *root == "" {
		relaunched, err := ensureElevated()
		if err != nil {
			fatal(err)
		}
		if relaunched {
			return
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := run(ctx, options{
		root:       *root,
		verbose:    *verbose,
		openWindow: !*noOpen,
		production: *prod,
		email:      *email,
		replace:    *replace,
	}); err != nil {
		fatal(err)
	}
}

type options struct {
	root       string
	verbose    bool
	openWindow bool
	production bool
	email      string
	replace    bool
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

	// After Start, because the lock records where this wizard is serving and
	// there is no address until the listener is bound.
	//
	// Two copies is not a harmless duplicate: they share one state file, one
	// log and one credential store, and both are willing to install a service,
	// register a scheduled task and claim a hostname. The loser stops here,
	// having bound only a loopback port it is about to release.
	lock, err := instance.Acquire(layout.LockFile(), srv.Addr())

	// Offer to close the other one rather than only naming it.
	//
	// The commonest case by far is a newer RASA being started while an older
	// one sits in the background with no window, which is most of what testing
	// a new build looks like. Sending someone to Task Manager for that is a
	// poor answer.
	//
	// Ending a run part-way is safe by construction: every step is idempotent
	// and state is written as each phase completes, because a wizard has always
	// had to survive a closed window, a reboot or a power cut. Starting again
	// resumes from where it stopped.
	var running *instance.AlreadyRunning
	if errors.As(err, &running) && (o.replace || askToReplace(running)) {
		log.Info("closing the copy already running",
			slog.Int("pid", running.PID), slog.String("addr", running.Addr))
		if stopErr := instance.Stop(running, 10*time.Second); stopErr != nil {
			return fmt.Errorf("could not close the copy already running: %w", stopErr)
		}
		lock, err = instance.Acquire(layout.LockFile(), srv.Addr())
	}

	if err != nil {
		if errors.Is(err, instance.ErrAlreadyRunning) {
			// Declined, or nothing to ask with. Naming Task Manager is not
			// hand-holding, it is the only way out: a release build shows no
			// console window, so a RASA left running in the background is
			// invisible, and its address is no use either because the browser
			// needs the one-time key that went with it, which is deliberately
			// written nowhere.
			return fmt.Errorf("%w\n\n"+
				"  Look for the browser tab it opened and finish there.\n\n"+
				"  If there is no tab, RASA is running in the background with no window.\n"+
				"  Open Task Manager, end the task called %s, then start setup again.\n"+
				"  Or start it with -replace to close the other copy automatically.",
				err, processName())
		}
		return err
	}
	defer lock.Release()
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
		go watchForABrowser(ctx, srv, log)
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

// browserGracePeriod is how long to wait for a browser to load the wizard
// before assuming none is coming. Long enough for a cold browser start on a
// slow machine, short enough that nobody has given up first.
const browserGracePeriod = 25 * time.Second

// watchForABrowser tells the user where the wizard is if no browser ever
// reached it.
//
// Whether a window appeared is the one thing the launcher cannot report: it
// hands the URL to the shell and returns success immediately, and on an
// elevated process it has silently gone nowhere before. Printing the address
// used to be the way out of that, and release builds on Windows now have no
// console to print to, so the only remaining evidence is whether the page was
// ever fetched.
func watchForABrowser(ctx context.Context, srv *ui.Server, log *logging.Logger) {
	select {
	case <-ctx.Done():
		return
	case <-srv.Done():
		return
	case <-time.After(browserGracePeriod):
	}
	if srv.Visited() {
		return
	}
	log.Warn("no browser loaded the wizard", slog.Duration("after", browserGracePeriod))
	ui.Notify("RASA for Jellyfin",
		"Setup is running, but no browser opened.\n\n"+
			"Copy this address into a browser to continue:\n\n"+
			srv.URL()+
			"\n\nLeave RASA running while you do.")
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
	// Say where it came from, and check it is the right one.
	//
	// FindBinary falls back to whatever "caddy" is on PATH, which on a machine
	// that already runs Caddy for something else is a stock build with neither
	// module the generated configuration needs. That is caught later, when the
	// configuration is validated, but "later" is after a hostname has been
	// claimed and a DNS record has propagated. Asking now costs one process
	// start and the user has lost nothing yet.
	log.Info("found a proxy binary", slog.String("path", path))
	checkCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	switch missing, err := caddy.MissingModules(checkCtx, path); {
	case err != nil:
		// Could not ask. Not the same as the modules being absent, so it is not
		// reported as if it were.
		log.Warn("could not check the proxy binary's modules", slog.Any("err", err))
	case len(missing) > 0:
		log.Warn("the proxy binary is missing modules RASA needs",
			slog.String("path", path),
			slog.String("missing", strings.Join(missing, ", ")),
			slog.String("likely_cause", "this looks like a stock Caddy from the system rather than the one RASA ships"),
		)
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

// outputVisible records whether anything printed will actually be seen.
//
// A release build on Windows is linked -H=windowsgui and, started by
// double-click, has no console at all. Every fmt.Fprintln to stderr on a fatal
// path then writes to an invalid handle and disappears, so the program exits
// non-zero having said nothing. The user double-clicks and watches nothing
// happen.
var outputVisible = true

// fatal reports an error and exits.
//
// It prints, always, because a terminal run should read normally. Where there
// is no terminal it also raises a dialog, because "exited quietly with status
// 1" is not a way to tell somebody their setup did not start.
func fatal(err error) {
	fmt.Fprintln(os.Stderr, "rasa:", err)
	if !outputVisible {
		ui.Notify("RASA for Jellyfin", err.Error())
	}
	os.Exit(1)
}

// processName is what this program is called in a task list, so the
// already-running message can name the thing to end rather than describing it.
func processName() string {
	exe, err := os.Executable()
	if err != nil {
		if runtime.GOOS == "windows" {
			return "rasa.exe"
		}
		return "rasa"
	}
	return filepath.Base(exe)
}

// askToReplace asks whether to close the copy already running.
//
// Two ways of asking, because there are two ways of starting: a terminal run
// gets a prompt, and a double-clicked release build -- which has no console at
// all -- gets a dialog. Anything else answers no, so an unattended run never
// silently kills another one; -replace is how a script says yes.
func askToReplace(running *instance.AlreadyRunning) bool {
	const question = "RASA is already running.\n\n" +
		"Close it and start this one instead?\n\n" +
		"Any setup it was part-way through will carry on from where it stopped."

	if !outputVisible {
		return ui.Ask("RASA for Jellyfin", question)
	}
	if !stdinIsTerminal() {
		return false
	}

	fmt.Println()
	fmt.Println("  RASA is already running (process", running.PID, "at", running.Addr+").")
	fmt.Println()
	fmt.Print("  Close it and start this one instead? [y/N] ")
	var answer string
	if _, err := fmt.Scanln(&answer); err != nil {
		return false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}

// stdinIsTerminal reports whether there is somebody there to answer.
//
// A pipe or a file is not a person: prompting into one blocks forever or reads
// EOF, and neither is a decision to close somebody else's running install.
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
