// Command rasa is the RASA for Jellyfin setup application.
//
// This is the skeleton from SPEC.md task 1: it wires up the layout, logger,
// state store and secret store, and reports what it finds. The wizard itself
// (task 9) and the setup phases (tasks 2-8) are not implemented yet, so a run
// currently reports status and exits rather than configuring anything.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"runtime"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/mode"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/paths"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/portmap"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/probe"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/routerguide"
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

	if err := run(context.Background(), *root, *verbose); err != nil {
		// Nothing user-facing has a UI yet; once the wizard exists this becomes
		// rasaerr.UserMessage(err) rendered on screen, never a raw error.
		fmt.Fprintln(os.Stderr, "rasa:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, root string, verbose bool) error {
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

	// Phase 2: discover and probe. Everything downstream branches on this,
	// so it runs even on a repeat launch — the network may have changed.
	plog := log.WithPhase("probe")
	res := probe.New(plog).Run(ctx)
	decision := mode.Choose(res)

	plog.Decision("mode", string(decision.Mode), decision.Reason,
		slog.Int("listen_port", decision.ListenPort),
		slog.Bool("needs_mapping", decision.NeedsPortMapping),
		slog.Bool("needs_manual_forward", decision.NeedsManualForward),
		slog.String("blocker", string(decision.Blocker)),
	)

	reportProbe(res, decision)

	// Phase 4: open the path. Automatic where the router allows it, guided
	// otherwise — never a dead end.
	if !decision.Blocked() && decision.Mode != state.ModeMesh {
		mapping, guide := openPath(ctx, log.WithPhase("portmap"), res, decision)
		if mapping != nil {
			st.PortMapping = mapping
		}
		if guide != nil {
			fmt.Println("\n" + guide.PlainText())
		}
	}

	if err := st.Advance(state.Probed); err != nil {
		log.Warn("could not record probe phase", slog.Any("err", err))
	}
	st.JellyfinAddress = res.Jellyfin.Address
	st.JellyfinVersion = res.Jellyfin.Version
	st.Mode = decision.Mode
	st.ListenPort = decision.ListenPort
	for _, w := range decision.StateWarnings() {
		st.AddWarning(w.Code, w.Text)
	}

	if err := store.Save(st); err != nil {
		return fmt.Errorf("saving state: %w", err)
	}
	log.Info("rasa exiting", slog.String("phase", string(st.Phase)))
	return nil
}

// openPath tries to open the router port, falling back to guided manual
// instructions. It returns whatever mapping now exists and, when the user has
// work to do, the instructions for it.
//
// A mapping that exists but is not permanent still produces instructions: the
// lease will lapse on a reboot, and a static forward is the more durable
// ending (SPEC.md §6).
func openPath(ctx context.Context, log *logging.Logger, res probe.Result, d mode.Decision) (*state.PortMapping, *routerguide.Instructions) {
	var stored *state.PortMapping

	if d.NeedsPortMapping && res.Router.ControlURL != "" && res.Host.LANAddress.IsValid() {
		m := portmap.New(res.Router.ControlURL, res.Router.ServiceType, log)
		out, err := m.Add(ctx, portmap.Request{
			ExternalPort:   d.ListenPort,
			InternalPort:   d.ListenPort,
			InternalClient: res.Host.LANAddress,
			Protocol:       portmap.TCP,
		})
		switch {
		case err != nil:
			var ue *portmap.UPnPError
			if errors.As(err, &ue) && ue.IsConflict() {
				log.Warned("Your router is already sending that port to a different device.")
			} else {
				log.Warned("The port could not be opened automatically.")
			}
			log.Debug("mapping failed", slog.Any("err", err))
		default:
			stored = &state.PortMapping{
				ExternalPort: out.Mapping.ExternalPort,
				InternalPort: out.Mapping.InternalPort,
				Method:       "upnp",
				Permanent:    out.Mapping.Permanent(),
				LeaseSeconds: out.Mapping.LeaseSeconds,
			}
			if out.Mapping.Permanent() && out.VerifiedByReadback {
				log.OK("Port opened on your router.")
				return stored, nil
			}
			log.Warned("The port was opened, but your router may clear it when it restarts.")
		}
	}

	// Either mapping was unavailable, failed, or produced something that will
	// not survive a reboot. All three end the same way: show the user how to
	// make it permanent themselves.
	cat, err := routerguide.Embedded()
	if err != nil {
		log.Error("router catalogue unavailable", slog.Any("err", err))
		return stored, nil
	}
	entry := cat.Match(routerguide.Identity{
		Vendor: res.Router.Vendor,
		Model:  res.Router.Model,
		MAC:    res.Router.MAC,
	})
	ins := routerguide.Build(entry, routerguide.Values{
		Gateway:       res.Router.Gateway,
		InternalIP:    res.Host.LANAddress,
		Port:          d.ListenPort,
		AddressIsDHCP: res.Host.AddressIsDHCP,
	})
	log.Info("rendered port forwarding guide",
		slog.String("router", ins.RouterName),
		slog.Bool("generic", ins.Generic),
		slog.Bool("reservation_required", ins.ReservationRequired),
	)
	return stored, &ins
}

// reportProbe prints the four pre-flight lines from journey step 5, then what
// was decided from them. The wizard will render this; for now it is stdout.
func reportProbe(res probe.Result, d mode.Decision) {
	s := res.Summarize()
	fmt.Println("\nChecking things over")
	for _, line := range []string{s.Jellyfin, s.Internet, s.Router, s.Ports} {
		fmt.Printf("  %s\n", line)
	}

	fmt.Println("\nDecision")
	if d.Blocked() {
		fmt.Printf("  blocked: %s\n", d.Reason)
	} else {
		fmt.Printf("  mode %s on port %d\n", d.Mode, d.ListenPort)
		fmt.Printf("  because %s\n", d.Reason)
		switch {
		case d.NeedsPortMapping:
			fmt.Println("  next: open the port automatically")
		case d.NeedsManualForward:
			fmt.Println("  next: guided manual port forwarding")
		}
	}
	for _, w := range d.Warnings {
		fmt.Printf("  warning: %s\n", w.Text)
	}
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
