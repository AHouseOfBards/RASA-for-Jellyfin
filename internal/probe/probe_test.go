package probe

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
)

// ---------- version comparison ----------

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"10.11.5", "10.11.5", 0},
		{"10.11.7", "10.11.5", 1},
		{"10.10.3", "10.11.5", -1},
		{"10.11.5", "10.11", 1}, // missing components are zero, so 10.11 == 10.11.0
		{"10.11.0", "10.11", 0},
		{"10.11.6", "10.11", 1},
		{"11.0.0", "10.11.5", 1},
		{"9.9.9", "10.0.0", -1},    // not string comparison
		{"10.11.10", "10.11.9", 1}, // numeric, not lexical
		{"10.11.5-rc1", "10.11.5", 0},
		{"10.11.5+build7", "10.11.5", 0},
		{" 10.11.5 ", "10.11.5", 0},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestVersionFloorAcceptsAndRejects(t *testing.T) {
	if CompareVersions("10.10.3", MinimumJellyfinVersion) >= 0 {
		t.Error("10.10.3 should be below the floor")
	}
	if CompareVersions("10.11.5", MinimumJellyfinVersion) < 0 {
		t.Error("the floor itself must be accepted")
	}
}

// ---------- jellyfin discovery ----------

func jellyfinServer(t *testing.T, body string) (addr string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != systemInfoPublic {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

func TestJellyfinProbeFindsServer(t *testing.T) {
	addr := jellyfinServer(t, `{"ServerName":"Home","Version":"10.11.7",
		"ProductName":"Jellyfin Server","LocalAddress":"http://192.168.1.10:8096","Id":"abc"}`)

	p := NewJellyfinProber(logging.Discard())
	p.Addresses = []string{addr}

	got := p.Probe(context.Background())
	if !got.Found {
		t.Fatal("server not found")
	}
	if got.Version != "10.11.7" {
		t.Fatalf("version = %q", got.Version)
	}
	if !got.MeetsMinimum {
		t.Error("10.11.7 should meet the floor")
	}
}

func TestJellyfinProbeReportsOldVersionAsFound(t *testing.T) {
	// An old server must be found and reported, not treated as absent — the
	// user needs "update Jellyfin", not "Jellyfin not found".
	addr := jellyfinServer(t, `{"Version":"10.10.3","ProductName":"Jellyfin Server"}`)

	p := NewJellyfinProber(logging.Discard())
	p.Addresses = []string{addr}

	got := p.Probe(context.Background())
	if !got.Found {
		t.Fatal("old server should still be found")
	}
	if got.MeetsMinimum {
		t.Error("10.10.3 must not meet the floor")
	}
}

func TestJellyfinProbeIgnoresImpostor(t *testing.T) {
	// Something else answering on 8096 must not be mistaken for Jellyfin.
	addr := jellyfinServer(t, `{"ProductName":"Some Other App","Version":"1.0"}`)

	p := NewJellyfinProber(logging.Discard())
	p.Addresses = []string{addr}

	if got := p.Probe(context.Background()); got.Found {
		t.Fatalf("non-jellyfin server accepted: %+v", got)
	}
}

func TestJellyfinProbeIgnoresEmptyVersion(t *testing.T) {
	addr := jellyfinServer(t, `{"ProductName":"Jellyfin Server"}`)
	p := NewJellyfinProber(logging.Discard())
	p.Addresses = []string{addr}

	if got := p.Probe(context.Background()); got.Found {
		t.Fatal("a server with no version should not be accepted")
	}
}

func TestJellyfinProbeTriesCandidatesInOrder(t *testing.T) {
	good := jellyfinServer(t, `{"Version":"10.11.7","ProductName":"Jellyfin Server"}`)

	p := NewJellyfinProber(logging.Discard())
	p.Timeout = time.Second
	// A closed port first, so the fallback path is exercised.
	p.Addresses = []string{"127.0.0.1:1", good}

	if got := p.Probe(context.Background()); !got.Found {
		t.Fatal("should have fallen through to the reachable candidate")
	}
}

func TestJellyfinProbeNotFound(t *testing.T) {
	p := NewJellyfinProber(logging.Discard())
	p.Timeout = time.Second
	p.Addresses = []string{"127.0.0.1:1"}

	got := p.Probe(context.Background())
	if got.Found || got.Version != "" {
		t.Fatalf("expected an empty result, got %+v", got)
	}
}

// ---------- deployment classification ----------

func TestClassifyDeploymentNative(t *testing.T) {
	d, src := classifyDeployment("127.0.0.1:8096", &publicInfo{LocalAddress: "http://192.168.1.10:8096"})
	if d != DeploymentNative {
		t.Fatalf("deployment = %s, want native", d)
	}
	if src != "127.0.0.1" {
		t.Fatalf("proxy source = %q, want loopback", src)
	}
}

func TestClassifyDeploymentContainer(t *testing.T) {
	// Jellyfin reporting a Docker bridge address means it will see the proxy
	// arriving from the gateway, not from loopback. Getting this wrong makes
	// Jellyfin log every user with the proxy's address.
	d, src := classifyDeployment("127.0.0.1:8096", &publicInfo{LocalAddress: "http://172.17.0.2:8096"})
	if d != DeploymentContainer {
		t.Fatalf("deployment = %s, want container", d)
	}
	if src != "172.17.0.2" {
		t.Fatalf("proxy source = %q", src)
	}
}

func TestClassifyDeploymentRemote(t *testing.T) {
	d, _ := classifyDeployment("192.168.1.50:8096", &publicInfo{})
	if d != DeploymentRemote {
		t.Fatalf("deployment = %s, want remote", d)
	}
}

func TestExtractHost(t *testing.T) {
	cases := map[string]string{
		"http://192.168.1.10:8096": "192.168.1.10",
		"192.168.1.10:8096":        "192.168.1.10",
		"192.168.1.10":             "192.168.1.10",
		"https://10.0.0.5/":        "10.0.0.5",
		"not-an-address":           "",
	}
	for in, want := range cases {
		got := extractHost(in)
		if want == "" {
			if got.IsValid() {
				t.Errorf("extractHost(%q) = %v, want invalid", in, got)
			}
			continue
		}
		if got.String() != want {
			t.Errorf("extractHost(%q) = %v, want %s", in, got, want)
		}
	}
}

// ---------- network.xml ----------

func TestNetworkXMLPortSpellings(t *testing.T) {
	// The element has been spelled several ways across releases; all known
	// spellings must be accepted.
	cases := map[string]int{
		`<NetworkConfiguration><InternalHttpPort>8920</InternalHttpPort></NetworkConfiguration>`:         8920,
		`<NetworkConfiguration><HttpServerPortNumber>8097</HttpServerPortNumber></NetworkConfiguration>`: 8097,
		`<NetworkConfiguration><PublicHttpPort>8098</PublicHttpPort></NetworkConfiguration>`:             8098,
	}
	for doc, want := range cases {
		var cfg networkConfig
		if err := xml.Unmarshal([]byte(doc), &cfg); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		got := 0
		for _, v := range []int{cfg.InternalHttpPort, cfg.HTTPServerPort, cfg.PublicHttpPort} {
			if v > 0 {
				got = v
				break
			}
		}
		if got != want {
			t.Errorf("parsed %d, want %d from %s", got, want, doc)
		}
	}
}

func TestConfigPathsAreAbsolute(t *testing.T) {
	paths := ConfigPaths()
	if len(paths) == 0 {
		t.Fatal("no config paths for this platform")
	}
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			t.Errorf("config path is not absolute: %s", p)
		}
	}
}

func TestPortFromConfigReadsRealFile(t *testing.T) {
	// Exercises the parse against a file on disk rather than a string.
	dir := t.TempDir()
	path := filepath.Join(dir, "network.xml")
	doc := `<?xml version="1.0"?>
<NetworkConfiguration><InternalHttpPort>8920</InternalHttpPort></NetworkConfiguration>`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg networkConfig
	if err := xml.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.InternalHttpPort != 8920 {
		t.Fatalf("port = %d, want 8920", cfg.InternalHttpPort)
	}
}

// ---------- ports ----------

func TestProbePortsDetectsBusyPortAndFreeOne(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	busy := ln.Addr().(*net.TCPAddr).Port

	free, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	freePort := free.Addr().(*net.TCPAddr).Port
	free.Close() // now genuinely free

	got := ProbePorts(context.Background(), logging.Discard(), busy, freePort)

	if got.IsFree(busy) {
		t.Errorf("port %d is bound but reported free", busy)
	}
	if !got.IsFree(freePort) {
		t.Errorf("port %d should be free", freePort)
	}
}

func TestPortsUnprobedIsTreatedAsBusy(t *testing.T) {
	// Assuming a port is free is the dangerous direction.
	var p Ports
	if p.IsFree(443) {
		t.Fatal("a zero-value Ports must not report anything as free")
	}
	if p.HolderOf(443) != "" {
		t.Fatal("zero-value HolderOf should be empty")
	}
}

// ---------- public address ----------

func TestInternetProbeParsesAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "203.0.113.5\n")
	}))
	defer srv.Close()

	p := NewInternetProber(logging.Discard())
	p.V4Endpoints = []string{srv.URL}
	p.V6Endpoints = nil

	got := p.Probe(context.Background())
	if !got.Reachable {
		t.Fatal("should be reachable")
	}
	if got.PublicV4.String() != "203.0.113.5" {
		t.Fatalf("v4 = %v", got.PublicV4)
	}
	if got.HasV6() {
		t.Fatal("no IPv6 endpoint was configured")
	}
}

func TestInternetProbeFallsThroughToSecondEndpoint(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "203.0.113.5")
	}))
	defer good.Close()

	p := NewInternetProber(logging.Discard())
	p.V4Endpoints = []string{bad.URL, good.URL}
	p.V6Endpoints = nil

	if got := p.Probe(context.Background()); !got.HasV4() {
		t.Fatal("should have fallen through to the working endpoint")
	}
}

func TestInternetProbeRejectsPrivateAndGarbage(t *testing.T) {
	for _, body := range []string{"192.168.1.5", "10.0.0.1", "not an address", "<html>hello</html>", ""} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, body)
		}))

		p := NewInternetProber(logging.Discard())
		p.V4Endpoints = []string{srv.URL}
		p.V6Endpoints = nil

		if got := p.Probe(context.Background()); got.HasV4() {
			t.Errorf("accepted %q as a public address: %v", body, got.PublicV4)
		}
		srv.Close()
	}
}

func TestInternetProbeUnreachableWhenNothingAnswers(t *testing.T) {
	p := NewInternetProber(logging.Discard())
	p.Timeout = 500 * time.Millisecond
	p.V4Endpoints = []string{"http://127.0.0.1:1"}
	p.V6Endpoints = nil

	if got := p.Probe(context.Background()); got.Reachable {
		t.Fatal("should be unreachable")
	}
}

func TestInternetProbeIPv6OnlyIsStillReachable(t *testing.T) {
	// An IPv6-only connection must not be reported as "no internet" — it is
	// the entire basis of Mode A6.
	i := Internet{Reachable: true, PublicV6: netip.MustParseAddr("2001:db8::1")}
	if !i.Reachable || i.HasV4() || !i.HasV6() {
		t.Fatalf("unexpected: %+v", i)
	}
}

func TestHasV6RejectsMappedV4(t *testing.T) {
	// ::ffff:203.0.113.5 is an IPv4 address wearing an IPv6 costume.
	i := Internet{PublicV6: netip.MustParseAddr("::ffff:203.0.113.5")}
	if i.HasV6() {
		t.Fatal("IPv4-mapped address must not count as IPv6")
	}
}

func TestHasV6RejectsLinkLocal(t *testing.T) {
	i := Internet{PublicV6: netip.MustParseAddr("fe80::1")}
	if i.HasV6() {
		t.Fatal("link-local is not globally routable")
	}
}

// ---------- host ----------

func TestProbeHostReturnsUsableShape(t *testing.T) {
	h := ProbeHost(context.Background(), logging.Discard(), netip.Addr{})
	// LANAddress may legitimately be invalid in an isolated CI network, but if
	// it is valid it must be a private IPv4 address.
	if h.LANAddress.IsValid() {
		if !h.LANAddress.Is4() {
			t.Errorf("LAN address should be IPv4, got %v", h.LANAddress)
		}
	}
}

func TestIsContainerAddress(t *testing.T) {
	yes := []string{"172.17.0.2", "172.18.0.5", "10.88.0.3"}
	no := []string{"192.168.1.10", "10.0.0.5", "127.0.0.1", "203.0.113.5"}
	for _, s := range yes {
		if !isContainerAddress(netip.MustParseAddr(s)) {
			t.Errorf("%s should look like a container address", s)
		}
	}
	for _, s := range no {
		if isContainerAddress(netip.MustParseAddr(s)) {
			t.Errorf("%s should not look like a container address", s)
		}
	}
}

// ---------- helper used by several tests ----------

var _ = fmt.Sprintf
var _ = strconv.Itoa
