package jellyfin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/rasaerr"
)

// fakeServer is a Jellyfin stand-in holding a network configuration that the
// client can round-trip, so the read-modify-write behaviour is exercised for
// real rather than asserted against a canned response.
type fakeServer struct {
	mu       sync.Mutex
	cfg      Config
	writes   int
	restarts int
	token    string
	admin    bool
	srv      *httptest.Server
}

func newFake(t *testing.T, cfg Config) *fakeServer {
	t.Helper()
	f := &fakeServer{cfg: cfg, token: "server-issued-token-abc123", admin: true}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case r.URL.Path == "/System/Info/Public":
		json.NewEncoder(w).Encode(PublicInfo{
			ProductName: "Jellyfin Server", Version: "10.11.7", ServerName: "Home",
		})

	case r.URL.Path == "/Users/AuthenticateByName":
		var req authRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Pw != "correct-password" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		resp := authResponse{AccessToken: f.token}
		resp.User.Name = req.Username
		resp.User.ID = "user-1"
		resp.User.Policy.IsAdministrator = f.admin
		json.NewEncoder(w).Encode(resp)

	case r.URL.Path == "/System/Configuration/network" && r.Method == http.MethodGet:
		if !f.authed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(f.cfg)

	case r.URL.Path == "/System/Configuration/network" && r.Method == http.MethodPost:
		if !f.authed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var in Config
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.cfg = in
		f.writes++

	case r.URL.Path == "/System/Restart":
		f.restarts++

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeServer) authed(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Authorization"), f.token)
}

func (f *fakeServer) config() Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg
}

func client(t *testing.T, f *fakeServer) *Client {
	t.Helper()
	return New(f.srv.URL, WithLogger(logging.Discard()))
}

func login(t *testing.T, c *Client) {
	t.Helper()
	if _, err := c.AuthenticateByName(context.Background(), "admin", "correct-password"); err != nil {
		t.Fatalf("login: %v", err)
	}
}

func defaultConfig() Config {
	return Config{
		KeyKnownProxies:       []any{},
		KeyEnableRemoteAccess: false,
		KeyPublishedServerURI: []any{},
		KeyEnableUPnP:         true,
		KeyBaseURL:            "",
		// A key RASA knows nothing about, to prove it survives the round trip.
		"SomeFutureSetting": "must-survive",
	}
}

// ---------- authentication ----------

func TestAuthenticateStoresToken(t *testing.T) {
	f := newFake(t, defaultConfig())
	c := client(t, f)

	res, err := c.AuthenticateByName(context.Background(), "admin", "correct-password")
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsAdmin {
		t.Error("should report admin")
	}
	if !c.Authenticated() {
		t.Error("token not stored")
	}
}

func TestAuthenticateWrongPasswordGivesTypedError(t *testing.T) {
	f := newFake(t, defaultConfig())
	c := client(t, f)

	_, err := c.AuthenticateByName(context.Background(), "admin", "wrong")
	re, ok := rasaerr.As(err)
	if !ok || re.Code != rasaerr.CodeJellyfinAuth {
		t.Fatalf("expected a typed auth error, got %v", err)
	}
	// The user copy must distinguish the two accounts in play.
	if !strings.Contains(re.User().Why, "Jellyfin login") {
		t.Errorf("copy should disambiguate from the Dynu account: %q", re.User().Why)
	}
}

func TestNonAdminIsReported(t *testing.T) {
	// A standard account logs in fine and is only refused several steps later,
	// where the cause is far less obvious. Catching it here is the point.
	f := newFake(t, defaultConfig())
	f.admin = false
	c := client(t, f)

	res, err := c.AuthenticateByName(context.Background(), "bob", "correct-password")
	if err != nil {
		t.Fatal(err)
	}
	if res.IsAdmin {
		t.Fatal("non-admin reported as admin")
	}
}

func TestPasswordAndTokenAreRedacted(t *testing.T) {
	var buf bytes.Buffer
	log := logging.New(logging.Options{Writer: &buf, Level: -4})

	f := newFake(t, defaultConfig())
	c := New(f.srv.URL, WithLogger(log))
	if _, err := c.AuthenticateByName(context.Background(), "admin", "correct-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.NetworkConfig(context.Background()); err != nil {
		t.Fatal(err)
	}

	log.Info("dump", "auth", c.authHeader(), "pw", "correct-password")
	out := buf.String()
	if strings.Contains(out, "correct-password") {
		t.Error("password leaked into the log")
	}
	if strings.Contains(out, f.token) {
		t.Error("access token leaked into the log")
	}
}

func TestAPIKeyOptionAuthenticates(t *testing.T) {
	f := newFake(t, defaultConfig())
	c := New(f.srv.URL, WithLogger(logging.Discard()), WithAPIKey(f.token))

	if !c.Authenticated() {
		t.Fatal("api key should count as authenticated")
	}
	if _, err := c.NetworkConfig(context.Background()); err != nil {
		t.Fatalf("api key was not accepted: %v", err)
	}
}

func TestUnauthenticatedCallsRefuse(t *testing.T) {
	f := newFake(t, defaultConfig())
	c := client(t, f)
	if _, err := c.NetworkConfig(context.Background()); err == nil {
		t.Error("expected a refusal before login")
	}
	if err := c.SetNetworkConfig(context.Background(), Config{}); err == nil {
		t.Error("expected a refusal before login")
	}
}

// ---------- configuration ----------

func TestApplySetsEverythingRequired(t *testing.T) {
	f := newFake(t, defaultConfig())
	c := client(t, f)
	login(t, c)

	res, err := c.Apply(context.Background(), Settings{
		PublicURL:    "https://mymedia.freeddns.org",
		ProxySources: []string{"127.0.0.1", "::1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed() {
		t.Fatal("expected changes")
	}

	got := f.config()
	if !asBool(got[KeyEnableRemoteAccess]) {
		t.Error("remote access not enabled")
	}
	if asBool(got[KeyEnableUPnP]) {
		t.Error("jellyfin's own UPnP should be disabled")
	}
	proxies := asStringSlice(got[KeyKnownProxies])
	if !contains(proxies, "127.0.0.1") || !contains(proxies, "::1") {
		t.Errorf("known proxies = %v; ::1 matters when the proxy dials IPv6 loopback", proxies)
	}
	if !contains(asStringSlice(got[KeyPublishedServerURI]), "all=https://mymedia.freeddns.org") {
		t.Errorf("published uri = %v", got[KeyPublishedServerURI])
	}
}

func TestApplyPreservesUnknownKeys(t *testing.T) {
	// The reason Config is a map: a typed struct would silently discard every
	// key it does not know, wiping the user's other settings.
	f := newFake(t, defaultConfig())
	c := client(t, f)
	login(t, c)

	if _, err := c.Apply(context.Background(), Settings{PublicURL: "https://x.freeddns.org"}); err != nil {
		t.Fatal(err)
	}
	if got := asString(f.config()["SomeFutureSetting"]); got != "must-survive" {
		t.Fatalf("unknown key was discarded: %q", got)
	}
}

func TestApplyMergesExistingKnownProxies(t *testing.T) {
	// A user may have their own entries — another proxy, a VPN gateway — and
	// replacing the list would break something that was working.
	cfg := defaultConfig()
	cfg[KeyKnownProxies] = []any{"10.0.0.5"}
	f := newFake(t, cfg)
	c := client(t, f)
	login(t, c)

	if _, err := c.Apply(context.Background(), Settings{ProxySources: []string{"127.0.0.1"}}); err != nil {
		t.Fatal(err)
	}
	got := asStringSlice(f.config()[KeyKnownProxies])
	if !contains(got, "10.0.0.5") {
		t.Errorf("existing proxy entry was dropped: %v", got)
	}
	if !contains(got, "127.0.0.1") {
		t.Errorf("new proxy entry missing: %v", got)
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	// SPEC.md §10: a resumed run replays steps, so a second Apply must find
	// everything correct and write nothing.
	f := newFake(t, defaultConfig())
	c := client(t, f)
	login(t, c)

	s := Settings{PublicURL: "https://mymedia.freeddns.org", ProxySources: []string{"127.0.0.1", "::1"}}
	if _, err := c.Apply(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	writesAfterFirst := f.writes

	res, err := c.Apply(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed() {
		t.Errorf("second apply reported changes: %+v", res.Changes)
	}
	if f.writes != writesAfterFirst {
		t.Errorf("second apply wrote again (%d then %d)", writesAfterFirst, f.writes)
	}
}

func TestApplyReadsButDoesNotChangeBaseURL(t *testing.T) {
	// A base path the user set must be reported so the generated Caddyfile can
	// match it, and must never be overwritten.
	cfg := defaultConfig()
	cfg[KeyBaseURL] = "/jellyfin"
	f := newFake(t, cfg)
	c := client(t, f)
	login(t, c)

	res, err := c.Apply(context.Background(), Settings{PublicURL: "https://x.freeddns.org"})
	if err != nil {
		t.Fatal(err)
	}
	if res.BaseURL != "/jellyfin" {
		t.Errorf("base url = %q, want it reported", res.BaseURL)
	}
	if got := asString(f.config()[KeyBaseURL]); got != "/jellyfin" {
		t.Errorf("base url was modified to %q", got)
	}
}

func TestApplyReplacesStalePublishedURI(t *testing.T) {
	cfg := defaultConfig()
	cfg[KeyPublishedServerURI] = []any{"all=https://old.freeddns.org", "192.168.1.0/24=http://192.168.1.10:8096"}
	f := newFake(t, cfg)
	c := client(t, f)
	login(t, c)

	if _, err := c.Apply(context.Background(), Settings{PublicURL: "https://new.freeddns.org"}); err != nil {
		t.Fatal(err)
	}
	got := asStringSlice(f.config()[KeyPublishedServerURI])
	if !contains(got, "all=https://new.freeddns.org") {
		t.Errorf("new uri missing: %v", got)
	}
	if contains(got, "all=https://old.freeddns.org") {
		t.Errorf("stale uri kept: %v", got)
	}
	// A subnet-specific entry is the user's, not ours.
	if !contains(got, "192.168.1.0/24=http://192.168.1.10:8096") {
		t.Errorf("user's subnet entry was dropped: %v", got)
	}
}

func TestApplyDisablesRequireHTTPS(t *testing.T) {
	// TLS terminates at the proxy; both on produces a redirect loop.
	cfg := defaultConfig()
	cfg[KeyRequireHTTPS] = true
	f := newFake(t, cfg)
	c := client(t, f)
	login(t, c)

	res, err := c.Apply(context.Background(), Settings{PublicURL: "https://x.freeddns.org"})
	if err != nil {
		t.Fatal(err)
	}
	if asBool(f.config()[KeyRequireHTTPS]) {
		t.Error("RequireHttps should be disabled")
	}
	if !res.RestartRequired {
		t.Error("that change needs a restart")
	}
}

func TestApplyFailsLoudlyWhenWriteDoesNotStick(t *testing.T) {
	// The real risk with a renamed key: the write succeeds and changes
	// nothing, and remote access mysteriously does not work. The read-back is
	// what turns that into a clear failure.
	f := newFake(t, defaultConfig())
	// Server accepts writes but discards them.
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/Users/AuthenticateByName":
			resp := authResponse{AccessToken: f.token}
			resp.User.Policy.IsAdministrator = true
			json.NewEncoder(w).Encode(resp)
		case r.URL.Path == "/System/Configuration/network" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(defaultConfig())
		case r.URL.Path == "/System/Configuration/network" && r.Method == http.MethodPost:
			io.Copy(io.Discard, r.Body) // accept and drop
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	c := client(t, f)
	login(t, c)
	_, err := c.Apply(context.Background(), Settings{PublicURL: "https://x.freeddns.org"})
	if err == nil {
		t.Fatal("a write that changed nothing must be reported")
	}
	if !strings.Contains(err.Error(), "different setting name") {
		t.Errorf("error should suggest the likely cause: %v", err)
	}
}

func TestApplyDefaultsProxySources(t *testing.T) {
	f := newFake(t, defaultConfig())
	c := client(t, f)
	login(t, c)

	if _, err := c.Apply(context.Background(), Settings{PublicURL: "https://x.freeddns.org"}); err != nil {
		t.Fatal(err)
	}
	got := asStringSlice(f.config()[KeyKnownProxies])
	if !contains(got, "127.0.0.1") || !contains(got, "::1") {
		t.Errorf("defaults should cover both loopback families: %v", got)
	}
}

// ---------- tolerant readers ----------

func TestAccessorsTolerateRealJSONShapes(t *testing.T) {
	// JSON numbers arrive as float64 and absent keys as nil; asserting a type
	// would panic on someone else's configuration.
	if asBool(nil) || asBool("nope") || asBool(3.0) {
		t.Error("asBool too permissive")
	}
	if !asBool(true) || !asBool("true") || !asBool("True") {
		t.Error("asBool too strict")
	}
	if asString(nil) != "" || asString(42.0) != "" {
		t.Error("asString should tolerate non-strings")
	}
	if got := asStringSlice([]any{"a", 1.0, "b"}); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("asStringSlice should skip non-strings: %v", got)
	}
	if asStringSlice(nil) != nil || asStringSlice("x") != nil {
		t.Error("asStringSlice should return nil for wrong shapes")
	}
}

func TestReplacePrefixKeepsOtherEntries(t *testing.T) {
	got := replacePrefix([]string{"all=old", "net=keep"}, "all=", "all=new")
	if !contains(got, "all=new") || !contains(got, "net=keep") || contains(got, "all=old") {
		t.Fatalf("got %v", got)
	}
}

func TestReplacePrefixAppendsWhenAbsent(t *testing.T) {
	if got := replacePrefix([]string{"net=keep"}, "all=", "all=new"); len(got) != 2 || !contains(got, "all=new") {
		t.Fatalf("got %v", got)
	}
}

func TestMergeUniqueDropsBlanksAndDuplicates(t *testing.T) {
	got := mergeUnique([]string{"a", "", " "}, []string{"a", "b"})
	if len(got) != 2 || !contains(got, "a") || !contains(got, "b") {
		t.Fatalf("got %v", got)
	}
}

func TestSameSetIgnoresOrder(t *testing.T) {
	if !sameSet([]string{"a", "b"}, []string{"b", "a"}) {
		t.Error("order should not matter")
	}
	if sameSet([]string{"a"}, []string{"a", "b"}) {
		t.Error("different lengths are different sets")
	}
}
