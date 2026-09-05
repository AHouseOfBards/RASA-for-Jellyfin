package caddy

import (
	"strings"
	"testing"
	"time"
)

func good() Config {
	return Config{
		Hostname:        "mymedia.freeddns.org",
		ListenPort:      443,
		UpstreamAddress: "127.0.0.1:8096",
		DynuAPIKeyEnv:   "RASA_DYNU_TOKEN",
		OwnDomain:       "mymedia.freeddns.org",
		LogPath:         `C:\ProgramData\RASA\logs\caddy.log`,
	}
}

func generate(t *testing.T, c Config) string {
	t.Helper()
	out, err := c.Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return out
}

func TestGenerateBasicShape(t *testing.T) {
	out := generate(t, good())
	for _, want := range []string{
		"mymedia.freeddns.org {",
		"dns dynu",
		"reverse_proxy 127.0.0.1:8096",
		"admin off",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestOwnDomainIsEmitted(t *testing.T) {
	// The finding this whole package hangs on. A Dynu DDNS hostname is its own
	// zone apex, so without own_domain the provider hunts for a parent zone
	// the account does not own and the challenge fails.
	out := generate(t, good())
	if !strings.Contains(out, "own_domain mymedia.freeddns.org") {
		t.Fatalf("own_domain not emitted:\n%s", out)
	}
	// It requires the block form; the inline form cannot carry it.
	if strings.Contains(out, "dns dynu {env.") {
		t.Errorf("inline form used, which cannot carry own_domain:\n%s", out)
	}
	if !strings.Contains(out, "api_token  {env.RASA_DYNU_TOKEN}") {
		t.Errorf("api_token not set inside the block:\n%s", out)
	}
}

func TestInlineFormWhenNoOwnDomain(t *testing.T) {
	c := good()
	c.OwnDomain = ""
	out := generate(t, c)
	if !strings.Contains(out, "dns dynu {env.RASA_DYNU_TOKEN}") {
		t.Fatalf("expected the inline form:\n%s", out)
	}
}

func TestCredentialIsNeverWrittenIntoTheFile(t *testing.T) {
	// The credential belongs to the service, supplied through its environment.
	// A Caddyfile is world-readable on most systems.
	c := good()
	c.DynuAPIKeyEnv = "RASA_DYNU_TOKEN"
	out := generate(t, c)

	if strings.Contains(out, "dynu-api-key") || strings.Contains(out, "secret") {
		t.Fatalf("file appears to contain a credential:\n%s", out)
	}
	if !strings.Contains(out, "{env.RASA_DYNU_TOKEN}") {
		t.Error("credential should be referenced by environment variable")
	}
}

func TestNonStandardPortAppearsInSiteAddress(t *testing.T) {
	c := good()
	c.ListenPort = 8443
	out := generate(t, c)
	if !strings.Contains(out, "mymedia.freeddns.org:8443 {") {
		t.Fatalf("port missing from site address:\n%s", out)
	}
}

func TestDefaultPortIsOmittedFromSiteAddress(t *testing.T) {
	if got := good().SiteAddress(); got != "mymedia.freeddns.org" {
		t.Fatalf("site address = %q; 443 should not be written", got)
	}
	c := good()
	c.ListenPort = 0
	if got := c.SiteAddress(); got != "mymedia.freeddns.org" {
		t.Fatalf("unset port should behave as default, got %q", got)
	}
}

func TestStreamingDirectivesArePresent(t *testing.T) {
	// flush_interval -1 is Caddy's equivalent of nginx proxy_buffering off.
	// Without it playback stalls while the proxy accumulates a buffer, and the
	// long timeouts stop idle SyncPlay websockets being dropped.
	out := generate(t, good())
	for _, want := range []string{"flush_interval -1", "read_timeout  10m", "write_timeout 10m"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing streaming directive %q", want)
		}
	}
}

func TestRateLimitProtectsTheLoginEndpoint(t *testing.T) {
	out := generate(t, good())
	if !strings.Contains(out, "/Users/AuthenticateByName") {
		t.Fatalf("login endpoint is not rate limited:\n%s", out)
	}
}

func TestLogPathWithSpacesIsQuoted(t *testing.T) {
	// Windows paths routinely contain spaces and would otherwise split.
	c := good()
	c.LogPath = `C:\Program Files\RASA\caddy.log`
	out := generate(t, c)
	if !strings.Contains(out, `output file "C:\Program Files\RASA\caddy.log"`) {
		t.Fatalf("path with spaces not quoted:\n%s", out)
	}
}

func TestLogPathWithoutSpacesIsNotQuoted(t *testing.T) {
	c := good()
	c.LogPath = "/var/log/rasa/caddy.log"
	if out := generate(t, c); !strings.Contains(out, "output file /var/log/rasa/caddy.log") {
		t.Fatalf("path was quoted unnecessarily:\n%s", out)
	}
}

func TestLogBlockOmittedWhenNoPath(t *testing.T) {
	c := good()
	c.LogPath = ""
	if out := generate(t, c); strings.Contains(out, "log {") {
		t.Fatalf("log block should be absent:\n%s", out)
	}
}

func TestBaseURLIsRoutedWithoutStrippingIt(t *testing.T) {
	// Jellyfin serves everything under its base path and expects to receive
	// it: /jellyfin/System/Info/Public is the endpoint, /System/Info/Public is
	// a 404. handle_path strips the prefix it matched, so it forwards exactly
	// the path Jellyfin does not answer — through a proxy that started
	// cleanly and a setup that reported success.
	c := good()
	c.BaseURL = "/jellyfin"
	out := generate(t, c)
	if strings.Contains(out, "handle_path") {
		t.Fatalf("handle_path strips the base path Jellyfin needs:\n%s", out)
	}
	if !strings.Contains(out, "handle /jellyfin/* {") {
		t.Fatalf("base url not honoured:\n%s", out)
	}
	if strings.Contains(out, "strip_prefix") {
		t.Fatalf("the base path is being stripped some other way:\n%s", out)
	}
}

func TestBaseURLTrailingSlashIsNormalised(t *testing.T) {
	c := good()
	c.BaseURL = "/jellyfin/"
	if out := generate(t, c); !strings.Contains(out, "handle /jellyfin/* {") {
		t.Fatalf("trailing slash not normalised:\n%s", out)
	}
}

// Jellyfin's own settings page accepts the value either way.
func TestABaseURLWithoutALeadingSlashStillWorks(t *testing.T) {
	c := good()
	c.BaseURL = "jellyfin"
	if out := generate(t, c); !strings.Contains(out, "handle /jellyfin/* {") {
		t.Fatalf("missing leading slash not normalised:\n%s", out)
	}
}

func TestRootBaseURLIsTreatedAsNone(t *testing.T) {
	c := good()
	c.BaseURL = "/"
	out := generate(t, c)
	if strings.Contains(out, "handle /") || strings.Contains(out, "handle_path") {
		t.Fatalf("a root base path should not produce a handle block:\n%s", out)
	}
}

// The rate limiter sees the request as it arrived, so an unprefixed matcher on
// a server with a base path protects nothing at all — while still appearing in
// the generated file, which is the worst kind of security control.
func TestTheLoginRateLimitFollowsTheBasePath(t *testing.T) {
	c := good()
	c.BaseURL = "/jellyfin"
	if out := generate(t, c); !strings.Contains(out, "path /jellyfin/Users/AuthenticateByName") {
		t.Fatalf("the rate limiter does not cover the real login path:\n%s", out)
	}
}

func TestTheLoginRateLimitCoversTheRootCase(t *testing.T) {
	if out := generate(t, good()); !strings.Contains(out, "path /Users/AuthenticateByName") {
		t.Fatalf("the rate limiter does not cover the login path:\n%s", out)
	}
}

// RASA forwards 443 or 8443 at the router and never 80, so Caddy's automatic
// HTTP-to-HTTPS redirect is unreachable from the internet — but the :80 bind
// it needs still collides with anything already holding the port, and Caddy
// refuses to start when it cannot bind a listener.
func TestPortEightyIsNeverBound(t *testing.T) {
	out := generate(t, good())
	if !strings.Contains(out, "auto_https disable_redirects") {
		t.Fatalf("nothing stops Caddy opening a redirect server on port 80:\n%s", out)
	}
	// disable_certs would be the catastrophic typo: it turns off the thing the
	// whole product exists to do.
	if strings.Contains(out, "disable_certs") {
		t.Fatalf("certificate automation was disabled:\n%s", out)
	}
}

func TestStagingCAIsEmitted(t *testing.T) {
	// SPEC.md §19: development builds must default to staging, because five
	// failed validations per hour is easy to exhaust while debugging.
	c := good()
	c.ACMECA = ACMEStaging
	out := generate(t, c)
	if !strings.Contains(out, "acme_ca "+ACMEStaging) {
		t.Fatalf("staging endpoint not set:\n%s", out)
	}
}

func TestProductionOmitsACMEOverride(t *testing.T) {
	c := good()
	c.ACMECA = ACMEProduction
	if out := generate(t, c); strings.Contains(out, "acme_ca") {
		t.Fatalf("production should use Caddy's default:\n%s", out)
	}
}

// Longer than Caddy's two-minute default, because giving up early burns a
// validation attempt against a cap of five per hostname per hour — but written
// from the constant, so the file and the ceiling RASA enforces cannot drift
// apart.
func TestPropagationTimeoutIsGenerousButNamed(t *testing.T) {
	out := generate(t, good())
	if !strings.Contains(out, "propagation_timeout "+PropagationTimeout.String()) {
		t.Fatalf("propagation timeout missing:\n%s", out)
	}
	if PropagationTimeout < 2*time.Minute {
		t.Errorf("propagation timeout is %s, below Caddy's own default", PropagationTimeout)
	}
}

func TestResolversDefaultToPublicServers(t *testing.T) {
	// A router's DNS often serves stale or filtered answers during a challenge.
	out := generate(t, good())
	for _, r := range DefaultResolvers {
		if !strings.Contains(out, r) {
			t.Errorf("default resolver %s missing", r)
		}
	}
}

func TestResolversCanBeOverridden(t *testing.T) {
	c := good()
	c.ExtraResolvers = []string{"8.8.8.8"}
	out := generate(t, c)
	if !strings.Contains(out, "resolvers 8.8.8.8") {
		t.Fatalf("override ignored:\n%s", out)
	}
}

func TestEmailIsOptional(t *testing.T) {
	c := good()
	if out := generate(t, c); strings.Contains(out, "email ") {
		t.Errorf("no email should mean no directive:\n%s", out)
	}
	c.Email = "user@example.com"
	if out := generate(t, c); !strings.Contains(out, "email user@example.com") {
		t.Errorf("email not emitted")
	}
}

func TestGeneratedFileWarnsAgainstHandEditing(t *testing.T) {
	// A re-run overwrites it, so a user's edits would vanish silently.
	out := generate(t, good())
	if !strings.HasPrefix(out, "#") || !strings.Contains(out, "overwrite") {
		t.Fatalf("no warning header:\n%s", out)
	}
}

func TestValidateCatchesMissingFields(t *testing.T) {
	cases := map[string]func(*Config){
		"hostname":                        func(c *Config) { c.Hostname = "" },
		"upstream address":                func(c *Config) { c.UpstreamAddress = "" },
		"listen port":                     func(c *Config) { c.ListenPort = 0 },
		"credential environment variable": func(c *Config) { c.DynuAPIKeyEnv = "" },
	}
	for want, mutate := range cases {
		c := good()
		mutate(&c)
		err := c.Validate()
		if err == nil {
			t.Errorf("%s: expected a validation error", want)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name the missing field %q: %v", want, err)
		}
		if _, gerr := c.Generate(); gerr == nil {
			t.Errorf("%s: Generate should refuse an invalid config", want)
		}
	}
}

func TestValidatePortRange(t *testing.T) {
	c := good()
	c.ListenPort = 70000
	if err := c.Validate(); err == nil {
		t.Fatal("out-of-range port should be rejected")
	}
}

func TestBracesAreBalanced(t *testing.T) {
	// A malformed file is rejected by Caddy at service start, long after RASA
	// reported success.
	for _, c := range []Config{
		good(),
		func() Config { x := good(); x.BaseURL = "/jellyfin"; return x }(),
		func() Config { x := good(); x.OwnDomain = ""; return x }(),
		func() Config { x := good(); x.LogPath = ""; return x }(),
	} {
		out := generate(t, c)
		if open, close := strings.Count(out, "{"), strings.Count(out, "}"); open != close {
			t.Errorf("unbalanced braces (%d open, %d close):\n%s", open, close, out)
		}
	}
}

func TestEnvPlaceholderBracesDoNotUnbalance(t *testing.T) {
	// {env.X} contributes one of each, so the balance check above stays valid.
	out := generate(t, good())
	if !strings.Contains(out, "{env.RASA_DYNU_TOKEN}") {
		t.Fatal("env placeholder missing")
	}
}

// Certificate issuance messages go to Caddy's DEFAULT logger, not to a site's
// access log. Without a log block in the global options they go nowhere at all
// under a Windows service — which is how a stalled issuance produced an empty
// caddy.log and five minutes of unexplained silence on the first real run.
func TestRuntimeLogIsConfiguredGlobally(t *testing.T) {
	cfg := Config{
		Hostname:        "mymedia.freeddns.org",
		ListenPort:      443,
		UpstreamAddress: "127.0.0.1:8096",
		DynuAPIKeyEnv:   "RASA_DYNU_TOKEN",
		OwnDomain:       "mymedia.freeddns.org",
		LogPath:         `C:\ProgramData\RASA\logs\caddy.log`,
	}
	out, err := cfg.Generate()
	if err != nil {
		t.Fatal(err)
	}

	global := out[:strings.Index(out, "mymedia.freeddns.org {")]
	if !strings.Contains(global, "log {") {
		t.Fatalf("the global options block has no log directive, so nothing records issuance:\n%s", global)
	}
	if !strings.Contains(global, "caddy.log") {
		t.Errorf("the global log does not name the runtime log file:\n%s", global)
	}
}

// RASA waits on issuance, so its own deadline has to outlast the one handed to
// Caddy. Both were once five minutes, and a stalled challenge made RASA report
// failure at 4m55s while Caddy was still inside its window.
func TestPropagationTimeoutIsEmittedFromTheConstant(t *testing.T) {
	cfg := Config{
		Hostname:        "mymedia.freeddns.org",
		ListenPort:      443,
		UpstreamAddress: "127.0.0.1:8096",
		DynuAPIKeyEnv:   "RASA_DYNU_TOKEN",
		OwnDomain:       "mymedia.freeddns.org",
	}
	out, err := cfg.Generate()
	if err != nil {
		t.Fatal(err)
	}
	want := "propagation_timeout " + PropagationTimeout.String()
	if !strings.Contains(out, want) {
		t.Errorf("generated file does not carry %q", want)
	}
}

// A user who types the bare address should land on the server, not a blank
// page. Two handle blocks rather than a bare redir, because handle blocks are
// mutually exclusive and first-match.
func TestTheBareAddressRedirectsToTheBasePath(t *testing.T) {
	c := good()
	c.BaseURL = "/jellyfin"
	out := generate(t, c)
	if !strings.Contains(out, "handle / {") || !strings.Contains(out, "redir * /jellyfin/ 302") {
		t.Fatalf("the bare address does not reach the server:\n%s", out)
	}
}

func TestNoRedirectWhenTheServerIsAtTheRoot(t *testing.T) {
	// "redir", not "redir" anywhere — auto_https disable_redirects contains it.
	if out := generate(t, good()); strings.Contains(out, "\tredir ") {
		t.Fatalf("a server at the root needs no redirect:\n%s", out)
	}
}
