package ddns

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/dynu"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/probe"
)

const host = "mymedia.freeddns.org"

// fakeDynu serves a single hostname whose published addresses the test can set.
type fakeDynu struct {
	v4, v6  string
	hasV4   bool
	hasV6   bool
	updates atomic.Int32
	srv     *httptest.Server
}

func newFakeDynu(t *testing.T, v4 string, hasV4 bool) *fakeDynu {
	t.Helper()
	f := &fakeDynu{v4: v4, hasV4: hasV4}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/dns" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{
				"statusCode": 200,
				"domains": []map[string]any{{
					"id": 123, "name": host,
					"ipv4Address": f.v4, "ipv6Address": f.v6,
					"ipv4": f.hasV4, "ipv6": f.hasV6, "ttl": 60,
				}},
			})
		case strings.HasPrefix(r.URL.Path, "/dns/") && r.Method == http.MethodPost:
			f.updates.Add(1)
			io.Copy(io.Discard, r.Body)
			io.WriteString(w, `{"statusCode":200}`)
		case strings.HasPrefix(r.URL.Path, "/dns/") && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{
				"statusCode": 200, "id": 123, "name": host,
				"ipv4Address": f.v4, "ipv4": f.hasV4,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// syncer wires a Syncer to a fake Dynu and a fake public-address service.
func syncer(t *testing.T, f *fakeDynu, publicV4 string) *Syncer {
	t.Helper()
	ipsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, publicV4)
	}))
	t.Cleanup(ipsrv.Close)

	c := dynu.New("test-key-abcdef123456", dynu.WithBaseURL(f.srv.URL))
	s := New(c, host, filepath.Join(t.TempDir(), "last-sync.txt"), logging.Discard())

	ip := probe.NewInternetProber(logging.Discard())
	ip.V4Endpoints = []string{ipsrv.URL}
	ip.V6Endpoints = nil
	s.internet = ip
	return s
}

func TestUpdatesWhenAddressChanged(t *testing.T) {
	f := newFakeDynu(t, "198.51.100.7", true) // published address is stale
	s := syncer(t, f, "203.0.113.5")          // observed address differs

	out := s.RunOnce(context.Background())
	if out.Err != nil {
		t.Fatal(out.Err)
	}
	if !out.Updated {
		t.Fatal("should have updated")
	}
	if f.updates.Load() != 1 {
		t.Fatalf("expected one update call, got %d", f.updates.Load())
	}
}

func TestSkipsWhenAddressUnchanged(t *testing.T) {
	// Polls every ten minutes; updating each time would be pointless traffic
	// against a rate-limited API.
	f := newFakeDynu(t, "203.0.113.5", true)
	s := syncer(t, f, "203.0.113.5")

	out := s.RunOnce(context.Background())
	if out.Err != nil {
		t.Fatal(out.Err)
	}
	if out.Updated || !out.Unchanged {
		t.Fatalf("should have been a no-op: %+v", out)
	}
	if f.updates.Load() != 0 {
		t.Fatal("no update call should have been made")
	}
}

func TestUpdatesWhenFamilyDisabledDespiteMatchingAddress(t *testing.T) {
	// The address matches but the family is switched off, so nothing is
	// actually published. Comparing addresses alone would call this unchanged
	// and the record would stay dark forever.
	f := newFakeDynu(t, "203.0.113.5", false)
	s := syncer(t, f, "203.0.113.5")

	if out := s.RunOnce(context.Background()); !out.Updated {
		t.Fatalf("a disabled family must trigger an update: %+v", out)
	}
}

func TestComparesAgainstServerNotACache(t *testing.T) {
	// Someone edited the record in Dynu's web panel. A locally cached value
	// would say "nothing to do" and never correct it.
	f := newFakeDynu(t, "203.0.113.5", true)
	s := syncer(t, f, "203.0.113.5")

	if out := s.RunOnce(context.Background()); !out.Unchanged {
		t.Fatal("baseline should be unchanged")
	}
	f.v4 = "198.51.100.99" // changed behind our back
	if out := s.RunOnce(context.Background()); !out.Updated {
		t.Fatal("an externally changed record must be corrected")
	}
}

func TestHeartbeatWrittenOnSuccess(t *testing.T) {
	// SPEC.md §15: written on every run including successes, so "is this
	// working?" is answerable by opening one file.
	f := newFakeDynu(t, "203.0.113.5", true)
	s := syncer(t, f, "203.0.113.5")
	s.RunOnce(context.Background())

	b, err := os.ReadFile(s.HeartbeatPath)
	if err != nil {
		t.Fatalf("no heartbeat written: %v", err)
	}
	txt := string(b)
	for _, want := range []string{"last run:", "unchanged", host} {
		if !strings.Contains(txt, want) {
			t.Errorf("heartbeat missing %q:\n%s", want, txt)
		}
	}
}

func TestHeartbeatWrittenOnFailure(t *testing.T) {
	f := newFakeDynu(t, "203.0.113.5", true)
	s := syncer(t, f, "not-an-address") // public address lookup fails
	out := s.RunOnce(context.Background())

	if out.Err == nil {
		t.Fatal("expected a failure")
	}
	b, _ := os.ReadFile(s.HeartbeatPath)
	txt := string(b)
	if !strings.Contains(txt, "FAILED") {
		t.Errorf("failure not recorded:\n%s", txt)
	}
	// It must tell the user what a sustained failure means and what to do.
	if !strings.Contains(txt, "stop working") || !strings.Contains(txt, "Re-run") {
		t.Errorf("heartbeat should explain the consequence and the fix:\n%s", txt)
	}
}

func TestMissingHostnameIsReported(t *testing.T) {
	f := newFakeDynu(t, "203.0.113.5", true)
	s := syncer(t, f, "203.0.113.5")
	s.Hostname = "someone-elses.freeddns.org"

	out := s.RunOnce(context.Background())
	if out.Err == nil || !strings.Contains(out.Err.Error(), "no longer on the account") {
		t.Fatalf("expected a clear missing-hostname error, got %v", out.Err)
	}
}

func TestHeartbeatFailureDoesNotFailTheSync(t *testing.T) {
	// A heartbeat that cannot be written must never turn a successful sync
	// into a failure.
	f := newFakeDynu(t, "198.51.100.7", true)
	s := syncer(t, f, "203.0.113.5")
	s.HeartbeatPath = string([]byte{0}) // unwritable

	if out := s.RunOnce(context.Background()); out.Err != nil || !out.Updated {
		t.Fatalf("sync should have succeeded regardless: %+v", out)
	}
}

func TestChangedLogic(t *testing.T) {
	v4 := netip.MustParseAddr("203.0.113.5")
	net4 := probe.Internet{Reachable: true, PublicV4: v4}

	if changed(&dynu.Domain{IPv4: true, IPv4Address: "203.0.113.5"}, net4) {
		t.Error("matching address and enabled family is unchanged")
	}
	if !changed(&dynu.Domain{IPv4: true, IPv4Address: "198.51.100.1"}, net4) {
		t.Error("different address is changed")
	}
	if !changed(&dynu.Domain{IPv4: false, IPv4Address: "203.0.113.5"}, net4) {
		t.Error("disabled family is changed")
	}
	// No observed IPv6 must not force an update just because none is published.
	if changed(&dynu.Domain{IPv4: true, IPv4Address: "203.0.113.5", IPv6: false}, net4) {
		t.Error("absent IPv6 should not trigger an update")
	}
}
