package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestHappyPathTransitions(t *testing.T) {
	s := NewState("run1")
	seq := []Phase{Probed, DomainClaimed, PortsMapped, DNSVisible, CertIssued, JellyfinConfigured, Verified, Running}
	for _, p := range seq {
		if err := s.Advance(p); err != nil {
			t.Fatalf("advance to %s: %v", p, err)
		}
	}
	if s.Phase != Running {
		t.Fatalf("ended at %s", s.Phase)
	}
}

func TestAdvanceIsIdempotent(t *testing.T) {
	// SPEC.md §10: every transition must be safe to re-run. A resumed run
	// replays steps, so re-advancing to the phase already held must succeed.
	s := NewState("run1")
	if err := s.Advance(Probed); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.Advance(Probed); err != nil {
			t.Fatalf("re-advance %d rejected: %v", i, err)
		}
	}
	if s.Phase != Probed {
		t.Fatalf("phase drifted to %s", s.Phase)
	}
}

func TestIllegalTransitionRejected(t *testing.T) {
	s := NewState("run1")
	if err := s.Advance(CertIssued); err == nil {
		t.Fatal("expected NEW -> CERT_ISSUED to be rejected")
	}
	if s.Phase != New {
		t.Fatalf("phase changed despite rejection: %s", s.Phase)
	}
}

func TestDegradedIsRecoverable(t *testing.T) {
	// Degraded is a running state, not a terminal failure.
	s := NewState("run1")
	s.Reset(Running)
	if err := s.Advance(Degraded); err != nil {
		t.Fatal(err)
	}
	if !s.IsComplete() {
		t.Fatal("degraded should still count as serving")
	}
	if err := s.Advance(Running); err != nil {
		t.Fatalf("degraded -> running: %v", err)
	}
}

func TestDegradedCanRewindToProbe(t *testing.T) {
	s := NewState("run1")
	s.Reset(Degraded)
	if err := s.Advance(Probed); err != nil {
		t.Fatalf("degraded -> probed should be allowed for repair: %v", err)
	}
}

func TestURLOmitsDefaultPort(t *testing.T) {
	s := NewState("run1")
	s.Hostname = "mymedia.freeddns.org"
	s.ListenPort = 443
	if got := s.URL(); got != "https://mymedia.freeddns.org" {
		t.Fatalf("got %q", got)
	}
	s.ListenPort = 8443
	if got := s.URL(); got != "https://mymedia.freeddns.org:8443" {
		t.Fatalf("got %q", got)
	}
}

func TestURLEmptyBeforeHostnameKnown(t *testing.T) {
	if got := NewState("run1").URL(); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestAddWarningDeduplicatesByCode(t *testing.T) {
	// Resumed runs re-emit the same warnings; the recovery file should not
	// accumulate duplicates.
	s := NewState("run1")
	s.AddWarning("finite_lease", "first text")
	s.AddWarning("finite_lease", "second text")
	s.AddWarning("dhcp_unreserved", "other")

	if len(s.Warnings) != 2 {
		t.Fatalf("expected 2 warnings, got %d", len(s.Warnings))
	}
	if s.Warnings[0].Text != "second text" {
		t.Fatalf("warning not updated: %q", s.Warnings[0].Text)
	}
}

func TestStateHoldsNoSecrets(t *testing.T) {
	// A structural guard for SPEC.md §14: the credential belongs to the
	// services that outlive RASA, never to this file. This catches a future
	// field named Token, APIKey, Password and so on.
	b, err := json.Marshal(populated())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	banned := regexp.MustCompile(`(?i)token|api_?key|password|secret|credential`)
	var walk func(string, any)
	walk = func(path string, v any) {
		if mm, ok := v.(map[string]any); ok {
			for k, vv := range mm {
				if banned.MatchString(k) {
					t.Errorf("state exposes a secret-shaped field: %s%s", path, k)
				}
				walk(path+k+".", vv)
			}
		}
	}
	walk("", m)
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(filepath.Join(dir, "state.json"))

	want := populated()
	if err := st.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Hostname != want.Hostname || got.Phase != want.Phase || got.Mode != want.Mode {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
	if got.PortMapping == nil || got.PortMapping.Permanent != want.PortMapping.Permanent {
		t.Fatal("port mapping did not survive round trip")
	}
	if len(got.Warnings) != len(want.Warnings) {
		t.Fatal("warnings did not survive round trip")
	}
}

func TestLoadMissingReturnsErrNotFound(t *testing.T) {
	st := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if _, err := st.Load(); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if st.Exists() {
		t.Fatal("Exists should be false")
	}
}

func TestSaveOverwritesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st := NewStore(path)

	first := NewState("run1")
	first.Hostname = "first.freeddns.org"
	if err := st.Save(first); err != nil {
		t.Fatal(err)
	}
	second := NewState("run2")
	second.Hostname = "second.freeddns.org"
	if err := st.Save(second); err != nil {
		t.Fatal(err)
	}

	got, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Hostname != "second.freeddns.org" {
		t.Fatalf("overwrite failed: %q", got.Hostname)
	}
	// No temporary files may be left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestLoadCorruptFileReportsPlainly(t *testing.T) {
	// A corrupt state file must not brick repair mode.
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := NewStore(path).Load()
	if err == nil || err == ErrNotFound {
		t.Fatalf("expected a distinct read error, got %v", err)
	}
	if !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

func TestLoadNewerSchemaIsReported(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st := NewStore(path)
	future := NewState("run1")
	future.Version = CurrentVersion + 1
	if err := st.Save(future); err != nil {
		t.Fatal(err)
	}
	got, err := st.Load()
	if err == nil {
		t.Fatal("expected an error for a newer schema version")
	}
	if got == nil {
		t.Fatal("state should still be returned so repair can decide what to do")
	}
}

func populated() *State {
	s := NewState("run-abc123")
	s.Phase = Running
	s.Mode = ModePublic
	s.Hostname = "mymedia.freeddns.org"
	s.ParentDomain = "freeddns.org"
	s.ListenPort = 8443
	s.JellyfinAddress = "127.0.0.1:8096"
	s.JellyfinVersion = "10.11.7"
	s.CaddyVersion = "2.10.0"
	s.CertExpiry = time.Now().Add(60 * 24 * time.Hour).UTC()
	s.PortMapping = &PortMapping{
		ExternalPort: 8443, InternalPort: 8443, Method: "upnp", Permanent: false, LeaseSeconds: 604800,
	}
	s.AddWarning("finite_lease", "Your router granted a temporary port mapping.")
	return s
}
