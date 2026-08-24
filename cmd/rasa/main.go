// Command rasa is the RASA for Jellyfin setup application.
//
// This is the skeleton from SPEC.md task 1: it wires up the layout, logger,
// state store and secret store, and reports what it finds. The wizard itself
// (task 9) and the setup phases (tasks 2-8) are not implemented yet, so a run
// currently reports status and exits rather than configuring anything.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"runtime"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/paths"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/secrets"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/state"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var (
		root    = flag.String("root", "", "override the data directory (development use; default is the system location)")
		verbose = flag.Bool("v", false, "log at debug level")
		showVer = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("RASA for Jellyfin %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
		return
	}

	if err := run(*root, *verbose); err != nil {
		// Nothing user-facing has a UI yet; once the wizard exists this becomes
		// rasaerr.UserMessage(err) rendered on screen, never a raw error.
		fmt.Fprintln(os.Stderr, "rasa:", err)
		os.Exit(1)
	}
}

func run(root string, verbose bool) error {
	layout := paths.Default()
	if root != "" {
		layout = paths.UnderRoot(root)
	}
	if err := layout.EnsureDirs(); err != nil {
		return fmt.Errorf("preparing %s: %w (administrator rights are required, or pass -root)", layout.Root, err)
	}

	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	log, err := logging.Open(layout.RASALog(), logging.Options{
		Level:       level,
		EventBuffer: 32,
	})
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

	store := state.NewStore(layout.StateFile())
	creds := secrets.NewFileStore(layout.SecretFile())

	st, err := store.Load()
	switch {
	case err == state.ErrNotFound:
		st = state.NewState(log.RunID())
		log.Info("no previous setup found; this is a first run")
	case err != nil:
		// A damaged state file must not block repair — report and continue
		// with a clean one rather than refusing to start.
		log.Warn("existing state could not be read; starting fresh", slog.Any("err", err))
		st = state.NewState(log.RunID())
	default:
		log.Info("found a previous setup",
			slog.String("phase", string(st.Phase)),
			slog.String("mode", string(st.Mode)),
			slog.Bool("complete", st.IsComplete()),
		)
	}

	names, err := creds.Names()
	if err != nil {
		log.Warn("credential store unreadable", slog.Any("err", err))
	}

	report(layout, creds, st, names)

	if err := store.Save(st); err != nil {
		return fmt.Errorf("saving state: %w", err)
	}
	log.Info("rasa exiting", slog.String("phase", string(st.Phase)))
	return nil
}

// report prints the current status. This stands in for the wizard's welcome
// screen (journey step 4), which decides between a fresh run and repair mode.
func report(layout paths.Layout, creds *secrets.FileStore, st *state.State, names []string) {
	fmt.Printf("RASA for Jellyfin %s\n\n", version)
	fmt.Printf("  data directory   %s\n", layout.Root)
	fmt.Printf("  log              %s\n", layout.RASALog())
	fmt.Printf("  credentials      %s\n", creds.Mechanism())
	fmt.Printf("  stored secrets   %d\n", len(names))
	fmt.Printf("  setup phase      %s\n", st.Phase)

	if url := st.URL(); url != "" {
		fmt.Printf("  address          %s\n", url)
	}
	if len(st.Warnings) > 0 {
		fmt.Printf("\n  warnings carried from the last run:\n")
		for _, w := range st.Warnings {
			fmt.Printf("    - %s\n", w.Text)
		}
	}

	fmt.Println()
	if st.IsComplete() {
		fmt.Println("Setup has already run. A future build will offer repair here.")
	} else {
		fmt.Println("Setup has not completed. The wizard is not implemented yet (task 9).")
	}
}
