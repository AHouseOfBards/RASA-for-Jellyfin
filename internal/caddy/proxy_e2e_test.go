package caddy

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// End-to-end through a real Caddy.
//
// Everything else in this package checks the text RASA writes, and text checks
// could not have caught the bug that motivated these: handle_path is spelled
// correctly, parses cleanly, and validates — it just strips the prefix, so the
// proxy forwards exactly the path Jellyfin answers with a 404. Only a request
// that actually arrives somewhere can tell the difference.
//
// Gated on a binary, like the validate test:
//
//	RASA_CADDY_BINARY=$(pwd)/dist/caddy go test ./internal/caddy/ -run EndToEnd -v

// jellyfinStub answers like a Jellyfin server with the given base path: it
// serves its public endpoint under the base and 404s everything else, which is
// exactly the behaviour that made the routing bug invisible from the outside.
type jellyfinStub struct {
	*httptest.Server
	mu   sync.Mutex
	seen []string
}

func newJellyfinStub(t *testing.T, base string) *jellyfinStub {
	t.Helper()
	s := &jellyfinStub{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.seen = append(s.seen, r.URL.Path)
		s.mu.Unlock()

		if r.URL.Path != base+"/System/Info/Public" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"Version":"10.11.5","ServerName":"stub"}`)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *jellyfinStub) paths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

func (s *jellyfinStub) address() string { return strings.TrimPrefix(s.URL, "http://") }

var tlsBlock = regexp.MustCompile(`(?s)\n\ttls \{.*?\n\t\}\n`)

// localise makes the generated file runnable offline.
//
// Two edits and no more: the site address becomes a loopback port, and the
// ACME block becomes Caddy's internal CA, because a test cannot answer a DNS-01
// challenge. Everything that decides where a request goes — the handle blocks,
// the rate limiter, the reverse proxy and its transport — is the generated text
// verbatim, which is the part under test. The caller asserts that.
func localise(t *testing.T, cfg Config, port int) string {
	t.Helper()
	text, err := cfg.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	site := cfg.SiteAddress() + " {"
	if !strings.Contains(text, site) {
		t.Fatalf("could not find the site address %q to replace:\n%s", site, text)
	}
	text = strings.Replace(text, site, "localhost:"+strconv.Itoa(port)+" {", 1)

	if !tlsBlock.MatchString(text) {
		t.Fatalf("could not find the tls block to replace:\n%s", text)
	}
	text = tlsBlock.ReplaceAllString(text, "\n\ttls internal\n")

	// The edits above must not have touched routing.
	for _, want := range []string{"reverse_proxy " + cfg.UpstreamAddress, "flush_interval -1", "rate_limit"} {
		if !strings.Contains(text, want) {
			t.Fatalf("localising the config lost %q:\n%s", want, text)
		}
	}
	return text
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// runCaddy starts the real binary on the given config and waits for it to
// answer, returning a client that trusts its internal CA blindly.
func runCaddy(t *testing.T, binary, text string, port int) *http.Client {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "Caddyfile")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, binary, "run", "--config", path, "--adapter", "caddyfile")
	cmd.Env = append(os.Environ(),
		"XDG_DATA_HOME="+dir, "XDG_CONFIG_HOME="+dir)
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("starting caddy: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("caddy output:\n%s", out.String())
		}
	})

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			// Caddy signs with a CA it generated in the temp directory above.
			// Trusting it is not what is under test.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// Readiness is a full handshake carrying the server name, and both halves
	// matter. A plain TCP connect succeeds before Caddy has issued its
	// certificate, so the first real request races it and fails with "tls:
	// internal error". A handshake without the server name fails against a
	// server that is up and working, because the connection policy is keyed on
	// SNI and dialling an IP sends none.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		c, err := tls.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port), &tls.Config{
			ServerName:         "localhost",
			InsecureSkipVerify: true,
		})
		if err == nil {
			c.Close()
			return client
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("caddy never answered on port %d:\n%s", port, out.String())
	return nil
}

func caddyBinary(t *testing.T) string {
	t.Helper()
	binary := os.Getenv("RASA_CADDY_BINARY")
	if binary == "" {
		if p, err := exec.LookPath(BinaryName()); err == nil {
			binary = p
		} else {
			t.Skip("set RASA_CADDY_BINARY to a caddy built with packaging/caddy/build.sh")
		}
	}
	if _, err := os.Stat(binary); err != nil {
		abs, _ := filepath.Abs(binary)
		t.Fatalf("RASA_CADDY_BINARY=%s does not exist (looked in %s); it has to be an absolute path", binary, abs)
	}
	return binary
}

func get(t *testing.T, c *http.Client, url string) (*http.Response, string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp, string(b)
}

// The bug this file exists for. handle_path validates, parses, and strips the
// prefix — so Jellyfin receives /System/Info/Public, answers 404, and RASA
// concludes the user's router is misconfigured.
func TestEndToEndABasePathReachesJellyfinUnchanged(t *testing.T) {
	binary := caddyBinary(t)
	jf := newJellyfinStub(t, "/jellyfin")
	port := freePort(t)

	cfg := Config{
		Hostname: "localhost", ListenPort: port,
		BaseURL: "/jellyfin", UpstreamAddress: jf.address(),
		DynuAPIKeyEnv: "RASA_DYNU_TOKEN", OwnDomain: "localhost",
	}
	client := runCaddy(t, binary, localise(t, cfg, port), port)

	base := fmt.Sprintf("https://localhost:%d", port)
	resp, body := get(t, client, base+"/jellyfin/System/Info/Public")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, `"Version"`) {
		t.Errorf("body = %q, want Jellyfin's public info", body)
	}

	// The heart of it: what Jellyfin actually received.
	got := jf.paths()
	if len(got) == 0 {
		t.Fatal("the request never reached Jellyfin")
	}
	if got[len(got)-1] != "/jellyfin/System/Info/Public" {
		t.Errorf("Jellyfin received %q, want the path with its base intact — the prefix was stripped", got)
	}
}

// The address RASA shows the user is the one with the base path, but someone
// will type the bare hostname anyway.
func TestEndToEndTheBareAddressRedirectsToTheBasePath(t *testing.T) {
	binary := caddyBinary(t)
	jf := newJellyfinStub(t, "/jellyfin")
	port := freePort(t)

	cfg := Config{
		Hostname: "localhost", ListenPort: port,
		BaseURL: "/jellyfin", UpstreamAddress: jf.address(),
		DynuAPIKeyEnv: "RASA_DYNU_TOKEN", OwnDomain: "localhost",
	}
	client := runCaddy(t, binary, localise(t, cfg, port), port)

	resp, _ := get(t, client, fmt.Sprintf("https://localhost:%d/", port))
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want a 302 to the base path", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/jellyfin/" {
		t.Errorf("Location = %q, want /jellyfin/", loc)
	}
}

// The overwhelmingly common case has to keep working, and a root server must
// not have a base path invented for it.
func TestEndToEndAServerAtTheRootIsProxiedUnchanged(t *testing.T) {
	binary := caddyBinary(t)
	jf := newJellyfinStub(t, "")
	port := freePort(t)

	cfg := Config{
		Hostname: "localhost", ListenPort: port,
		UpstreamAddress: jf.address(),
		DynuAPIKeyEnv:   "RASA_DYNU_TOKEN", OwnDomain: "localhost",
	}
	client := runCaddy(t, binary, localise(t, cfg, port), port)

	resp, body := get(t, client, fmt.Sprintf("https://localhost:%d/System/Info/Public", port))
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, `"Version"`) {
		t.Errorf("body = %q", body)
	}
	if got := jf.paths(); len(got) == 0 || got[len(got)-1] != "/System/Info/Public" {
		t.Errorf("Jellyfin received %v, want /System/Info/Public", got)
	}
}

// The login rate limiter is a security control RASA added because it put the
// login form on the public internet. On a server with a base path the matcher
// has to carry the prefix, or it silently protects nothing while still
// appearing in the file.
func TestEndToEndTheLoginRateLimitActuallyFires(t *testing.T) {
	binary := caddyBinary(t)
	jf := newJellyfinStub(t, "/jellyfin")
	port := freePort(t)

	cfg := Config{
		Hostname: "localhost", ListenPort: port,
		BaseURL: "/jellyfin", UpstreamAddress: jf.address(),
		DynuAPIKeyEnv: "RASA_DYNU_TOKEN", OwnDomain: "localhost",
	}
	client := runCaddy(t, binary, localise(t, cfg, port), port)

	login := fmt.Sprintf("https://localhost:%d/jellyfin/Users/AuthenticateByName", port)
	var limited bool
	// The zone allows 10 events a minute; the eleventh is the one that matters.
	for i := 0; i < 15; i++ {
		resp, _ := get(t, client, login)
		if resp.StatusCode == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Error("15 attempts at the login endpoint were never throttled")
	}
}
