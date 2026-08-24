package wizard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/paths"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/secrets"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/state"
)

func TestRemoveRemoteAccessTakesEverythingDown(t *testing.T) {
	h := newHarness(t, nil)
	h.runHappyPath(t)

	if err := h.w.RemoveRemoteAccess(context.Background()); err != nil {
		t.Fatalf("RemoveRemoteAccess: %v", err)
	}

	m := h.w.Model()
	if m.Screen != ScreenRemoved {
		t.Fatalf("screen = %s, want removed", m.Screen)
	}
	for _, s := range m.Removal {
		if s.State == StepFailed {
			t.Errorf("removal step %s failed: %s", s.ID, s.Note)
		}
	}

	// The credential is the one thing that must not survive.
	store := secrets.NewFileStore(paths.UnderRoot(h.dir).SecretFile())
	if _, err := store.Get(secrets.DynuAPIKey); !errors.Is(err, secrets.ErrNotFound) {
		t.Error("the Dynu key survived removal")
	}
	if m.DynuKey {
		t.Error("the model still reports a stored key")
	}

	// The state file stays, emptied. Someone removing remote access is often
	// about to ask why it stopped working.
	if _, err := os.Stat(filepath.Join(h.dir, "state.json")); err != nil {
		t.Errorf("the state file was deleted: %v", err)
	}
	if m.Phase != state.New {
		t.Errorf("phase = %s, want NEW", m.Phase)
	}
}

// The hostname is on the user's own account. Removing remote access must stop
// it pointing at their home without handing their chosen name back to the pool.
func TestRemovalUnpublishesButDoesNotDeleteTheHostname(t *testing.T) {
	h := newHarness(t, nil)
	h.runHappyPath(t)

	if err := h.w.RemoveRemoteAccess(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.dynu.deleted != 0 {
		t.Errorf("DeleteDomain was called %d times; the hostname belongs to the user", h.dynu.deleted)
	}
	if h.dynu.unpublished != 1 {
		t.Errorf("the address was unpublished %d times, want once", h.dynu.unpublished)
	}
	if !hasWarning(h.w.Model(), "hostname_retained") {
		t.Error("the user was not told their address is still on their account")
	}
}

// A user asking for this wants as much of it gone as possible. One failure
// must not stop the rest.
func TestRemovalContinuesAfterAFailure(t *testing.T) {
	h := newHarness(t, nil)
	h.runHappyPath(t)
	h.svc.removeServiceErr = errors.New("access denied")

	if err := h.w.RemoveRemoteAccess(context.Background()); err != nil {
		t.Fatal(err)
	}

	m := h.w.Model()
	var proxy, credential Step
	for _, s := range m.Removal {
		switch s.ID {
		case RemoveProxy:
			proxy = s
		case RemoveCredential:
			credential = s
		}
	}
	if proxy.State != StepFailed {
		t.Errorf("proxy step = %s, want failed", proxy.State)
	}
	if credential.State != StepDone {
		t.Errorf("credential step = %s; a failure earlier stopped the sequence", credential.State)
	}
	if m.Screen != ScreenRemoved {
		t.Errorf("screen = %s; a partial removal must still finish", m.Screen)
	}
}

// Removal on a machine where setup never completed must not fail.
func TestRemovalOnAFreshMachine(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.w.RemoveRemoteAccess(context.Background()); err != nil {
		t.Fatalf("RemoveRemoteAccess on a fresh machine: %v", err)
	}
	var addr Step
	for _, s := range h.w.Model().Removal {
		if s.ID == RemoveAddress {
			addr = s
		}
	}
	if addr.State != StepSkipped {
		t.Errorf("address step = %s, want skipped when nothing was published", addr.State)
	}
}
