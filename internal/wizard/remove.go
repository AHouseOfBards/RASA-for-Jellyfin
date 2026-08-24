package wizard

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/dynu"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/secrets"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/service"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/state"
)

// Removal step identifiers.
const (
	RemoveProxy      = "proxy"
	RemoveKeepAlive  = "keepalive"
	RemoveFirewall   = "firewall"
	RemoveAddress    = "address"
	RemoveCredential = "credential"
)

func initialRemoval() []Step {
	return []Step{
		{ID: RemoveProxy, Label: "Stopping the secure connection", State: StepPending},
		{ID: RemoveKeepAlive, Label: "Removing the address updater", State: StepPending},
		{ID: RemoveFirewall, Label: "Removing the firewall rule", State: StepPending},
		{ID: RemoveAddress, Label: "Unpublishing your web address", State: StepPending},
		{ID: RemoveCredential, Label: "Deleting the stored key", State: StepPending},
	}
}

// RemoveRemoteAccess undoes what setup installed.
//
// This is the other half of SPEC.md decision 15. Uninstalling RASA leaves
// remote access running on purpose, which means there has to be somewhere that
// deliberately takes it down — and it has to be here, because by the time
// someone wants it gone the wizard may be the only thing that still knows what
// was installed.
//
// Every step is best-effort and the sequence never stops early. A user asking
// for this wants as much of it gone as possible; refusing to remove the
// scheduled task because the service was already missing would be the worst
// possible reading of the request. What could not be removed is reported.
func (w *Wizard) RemoveRemoteAccess(ctx context.Context) error {
	if err := w.begin(); err != nil {
		return err
	}
	defer w.end()

	w.update(func(m *Model) {
		m.Screen = ScreenRemoving
		m.Removal = initialRemoval()
	})
	log := w.log.WithPhase("remove")

	mgr, mgrErr := w.opts.NewServices()
	if mgrErr != nil {
		log.Warn("no service manager available", slog.Any("err", mgrErr))
	}

	// The proxy.
	w.removeStep(RemoveProxy, StepRunning, "")
	switch {
	case mgrErr != nil:
		w.removeStep(RemoveProxy, StepFailed, "Needs administrator rights")
	default:
		if err := mgr.StopService(ctx, service.CaddyServiceName); err != nil {
			log.Debug("stop failed", slog.Any("err", err))
		}
		if err := mgr.RemoveService(ctx, service.CaddyServiceName); err != nil {
			log.Warn("could not remove the proxy service", slog.Any("err", err))
			w.removeStep(RemoveProxy, StepFailed, "Couldn't remove it")
		} else {
			w.removeStep(RemoveProxy, StepDone, "Stopped and removed")
		}
	}

	// The scheduled address update.
	w.removeStep(RemoveKeepAlive, StepRunning, "")
	switch {
	case mgrErr != nil:
		w.removeStep(RemoveKeepAlive, StepFailed, "Needs administrator rights")
	default:
		if err := mgr.RemoveTimer(ctx, service.SyncTimerName); err != nil {
			log.Warn("could not remove the sync task", slog.Any("err", err))
			w.removeStep(RemoveKeepAlive, StepFailed, "Couldn't remove it")
		} else {
			w.removeStep(RemoveKeepAlive, StepDone, "Removed")
		}
	}

	// The firewall rule. A no-op on platforms where RASA never wrote one.
	w.removeStep(RemoveFirewall, StepRunning, "")
	switch err := w.opts.RemoveFirewall(ctx); {
	case err == nil:
		w.removeStep(RemoveFirewall, StepDone, "Removed")
	case errors.Is(err, service.ErrNeedsPrivileges):
		log.Warn("firewall rule needs elevation to remove", slog.Any("err", err))
		w.removeStep(RemoveFirewall, StepFailed, "Needs administrator rights")
	default:
		log.Warn("could not remove the firewall rule", slog.Any("err", err))
		w.removeStep(RemoveFirewall, StepFailed, "Couldn't remove it")
	}

	// The published address. The hostname stays on the user's own Dynu
	// account: it is theirs, and handing their chosen name back to the pool
	// because they uninstalled an installer would be presumptuous. Clearing
	// the addresses achieves what actually matters — the name stops pointing
	// at their home connection.
	w.removeStep(RemoveAddress, StepRunning, "")
	w.unpublish(ctx)

	// The credential, last, because everything above may need it.
	w.removeStep(RemoveCredential, StepRunning, "")
	w.forgetCredential()

	// The state file is emptied of what was configured but kept, along with
	// the logs: someone removing remote access is often about to ask why it
	// stopped working, and throwing away the record at that exact moment is
	// how support questions become unanswerable.
	w.mu.Lock()
	hostname := w.st.Hostname
	w.st.Reset(state.New)
	w.st.PortMapping = nil
	w.st.CertExpiry = time.Time{}
	w.st.Hostname = ""
	w.st.ParentDomain = ""
	w.st.CaddyVersion = ""
	w.mu.Unlock()
	w.save()

	log.OK("Remote access has been removed.")
	w.update(func(m *Model) {
		m.Screen = ScreenRemoved
		m.Result.URL = ""
		m.Name.Hostname = hostname
	})
	return nil
}

// unpublish clears the address record, if RASA still holds what it needs to.
func (w *Wizard) unpublish(ctx context.Context) {
	w.mu.Lock()
	dyn, hostname, claimed := w.dyn, w.st.Hostname, w.claimed
	w.mu.Unlock()

	if dyn == nil || hostname == "" {
		// No credential and no hostname is the normal state after a partial
		// setup. Nothing was published, so nothing needs unpublishing.
		w.removeStep(RemoveAddress, StepSkipped, "Nothing was published")
		return
	}

	id := int64(0)
	if claimed != nil {
		id = claimed.ID
	}
	if id == 0 {
		found, err := dyn.FindDomain(ctx, hostname)
		if err != nil || found == nil {
			w.log.Warn("could not look up the hostname to unpublish", slog.Any("err", err))
			w.removeStep(RemoveAddress, StepFailed, "Couldn't reach your Dynu account")
			return
		}
		id = found.ID
	}

	unpublisher, ok := dyn.(interface {
		Unpublish(ctx context.Context, id int64, name string) error
	})
	if !ok {
		w.removeStep(RemoveAddress, StepSkipped, "")
		return
	}
	if err := unpublisher.Unpublish(ctx, id, hostname); err != nil {
		w.log.Warn("could not unpublish the address", slog.Any("err", err))
		w.removeStep(RemoveAddress, StepFailed, "Couldn't reach your Dynu account")
		return
	}
	w.removeStep(RemoveAddress, StepDone, hostname+" no longer points here")
	w.addWarning("hostname_retained",
		"Your address "+hostname+" is still on your Dynu account, but no longer points at your home. Delete it there if you don't want it.")
}

func (w *Wizard) forgetCredential() {
	if w.opts.Secrets == nil {
		w.removeStep(RemoveCredential, StepSkipped, "")
		return
	}
	err := w.opts.Secrets.Delete(secrets.DynuAPIKey)
	if err != nil && !errors.Is(err, secrets.ErrNotFound) {
		w.log.Warn("could not delete the stored credential", slog.Any("err", err))
		w.removeStep(RemoveCredential, StepFailed, "Couldn't delete it")
		return
	}

	// The environment file is the other place the token lives on Linux, and
	// leaving it behind would be the same leak in a different file.
	if path := w.opts.Layout.EnvFile(); path != "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			w.log.Warn("could not delete the service environment file", slog.Any("err", err))
		}
	}

	w.mu.Lock()
	w.dyn, w.dkey, w.claimed = nil, "", (*dynu.Domain)(nil)
	w.mu.Unlock()
	w.update(func(m *Model) { m.DynuKey = false })
	w.removeStep(RemoveCredential, StepDone, "Deleted")
}

func (w *Wizard) removeStep(id string, st StepState, note string) {
	w.update(func(m *Model) {
		for i := range m.Removal {
			if m.Removal[i].ID == id {
				m.Removal[i].State = st
				if note != "" {
					m.Removal[i].Note = note
				}
				return
			}
		}
	})
}
