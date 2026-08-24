package reach

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
)

// The property that matters most here: a failed self-check must never be
// reported as "unreachable". Many consumer routers do not hairpin, so the
// failure says nothing about whether outside traffic would arrive — and
// telling those users their setup is broken would send them to fix something
// that works.

func TestSelfCheckSucceedsOnLoopback(t *testing.T) {
	// 127.0.0.1 always "hairpins", which exercises the success path.
	p := New(netip.MustParseAddr("127.0.0.1"), logging.Discard())
	p.Timeout = 3 * time.Second

	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	res := p.SelfCheck(context.Background(), port)

	if res.Status != Reachable {
		t.Fatalf("status = %s (%s)", res.Status, res.Detail)
	}
	if !res.OK() {
		t.Error("reachable must count as OK")
	}
}

func TestSelfCheckUnroutableAddressIsInconclusive(t *testing.T) {
	// A public address we cannot dial back is exactly the no-hairpin case.
	p := New(netip.MustParseAddr("192.0.2.1"), logging.Discard()) // TEST-NET-1
	p.Timeout = 800 * time.Millisecond

	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	res := p.SelfCheck(context.Background(), port)

	if res.Status != Inconclusive {
		t.Fatalf("a router without hairpin must be inconclusive, got %s", res.Status)
	}
	if !res.OK() {
		t.Error("inconclusive must not block setup")
	}
	if !strings.Contains(strings.ToLower(res.Detail), "hairpin") {
		t.Errorf("detail should name the likely cause: %q", res.Detail)
	}
}

func TestSelfCheckWithNoPublicAddressIsInconclusive(t *testing.T) {
	p := New(netip.Addr{}, logging.Discard())
	if res := p.SelfCheck(context.Background(), 65000); res.Status != Inconclusive {
		t.Fatalf("status = %s", res.Status)
	}
}

func TestSelfCheckOnBoundPortIsInconclusive(t *testing.T) {
	// Something already holds the port. That is a real finding, but not a
	// reachability answer, so it must not read as "unreachable".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	port := portOf(t, srv.URL)

	p := New(netip.MustParseAddr("127.0.0.1"), logging.Discard())
	p.Timeout = time.Second

	res := p.SelfCheck(context.Background(), port)
	if res.Status != Inconclusive {
		t.Fatalf("status = %s (%s)", res.Status, res.Detail)
	}
	if !strings.Contains(res.Detail, "bind") {
		t.Errorf("detail should explain the bind failure: %q", res.Detail)
	}
}

func TestUserMessagesAreUsable(t *testing.T) {
	for _, s := range []Status{Reachable, Unreachable, Inconclusive, Unknown} {
		msg := Result{Status: s}.UserMessage()
		if strings.TrimSpace(msg) == "" {
			t.Errorf("%s: empty message", s)
		}
		for _, jargon := range []string{"hairpin", "NAT", "TCP", "HTTP", "nil", "token"} {
			if strings.Contains(msg, jargon) {
				t.Errorf("%s: user message contains jargon %q: %q", s, jargon, msg)
			}
		}
	}
}

func TestInconclusiveMessageDoesNotImplyFailure(t *testing.T) {
	// The wording is the whole point: the usual cause is a router limitation,
	// not a broken setup, and it should suggest how to check for real.
	msg := strings.ToLower(Result{Status: Inconclusive}.UserMessage())
	for _, bad := range []string{"failed", "error", "broken", "wrong"} {
		if strings.Contains(msg, bad) {
			t.Errorf("inconclusive message reads as failure (%q): %q", bad, msg)
		}
	}
	if !strings.Contains(msg, "mobile data") {
		t.Errorf("should suggest a way to verify for real: %q", msg)
	}
}

// ---------- CheckURL, the Phase 7 verification ----------

func TestCheckURLReachableWhenContentMatches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ProductName":"Jellyfin Server","Version":"10.11.7"}`)
	}))
	defer srv.Close()

	p := New(netip.MustParseAddr("127.0.0.1"), logging.Discard())
	res := p.CheckURL(context.Background(), srv.URL, "Jellyfin")
	if res.Status != Reachable {
		t.Fatalf("status = %s (%s)", res.Status, res.Detail)
	}
}

func TestCheckURLRejectsWrongServer(t *testing.T) {
	// Something answered, but it is not the server we expect — an ISP filter
	// page or the router's own admin interface.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "<html>Router Login</html>")
	}))
	defer srv.Close()

	p := New(netip.MustParseAddr("127.0.0.1"), logging.Discard())
	res := p.CheckURL(context.Background(), srv.URL, "Jellyfin")
	if res.Status != Unreachable {
		t.Fatalf("a wrong responder must be unreachable, got %s", res.Status)
	}
}

func TestCheckURLNonOKStatusIsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	p := New(netip.MustParseAddr("127.0.0.1"), logging.Discard())
	if res := p.CheckURL(context.Background(), srv.URL, ""); res.Status != Unreachable {
		t.Fatalf("status = %s", res.Status)
	}
}

func TestCheckURLConnectionFailureIsInconclusive(t *testing.T) {
	p := New(netip.MustParseAddr("127.0.0.1"), logging.Discard())
	p.Timeout = 600 * time.Millisecond

	res := p.CheckURL(context.Background(), "http://192.0.2.1:81/x", "")
	if res.Status != Inconclusive {
		t.Fatalf("an unreachable host must be inconclusive, not a verdict: %s", res.Status)
	}
}

func TestStatusStrings(t *testing.T) {
	for s, want := range map[Status]string{
		Reachable: "reachable", Unreachable: "unreachable",
		Inconclusive: "inconclusive", Unknown: "unknown",
	} {
		if s.String() != want {
			t.Errorf("%d.String() = %q, want %q", s, s.String(), want)
		}
	}
}

func TestOnlyReachableAndInconclusiveProceed(t *testing.T) {
	if (Result{Status: Unreachable}).OK() {
		t.Error("unreachable must not proceed")
	}
	if (Result{Status: Unknown}).OK() {
		t.Error("unknown must not proceed")
	}
}

// helpers

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func portOf(t *testing.T, url string) int {
	t.Helper()
	i := strings.LastIndex(url, ":")
	p := 0
	for _, c := range url[i+1:] {
		if c < '0' || c > '9' {
			break
		}
		p = p*10 + int(c-'0')
	}
	if p == 0 {
		t.Fatalf("could not parse port from %q", url)
	}
	return p
}
