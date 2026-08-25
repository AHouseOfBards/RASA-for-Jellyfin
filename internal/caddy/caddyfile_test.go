package caddy

import (
	"strings"
	"testing"
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

func TestBaseURLProducesHandlePath(t *testing.T) {
	// Jellyfin serves everything under its base path; a proxy that ignores it
	// 404s every request.
	c := good()
	c.BaseURL = "/jellyfin"
	out := generate(t, c)
	if !strings.Contains(out, "handle_path /jellyfin/*") {
		t.Fatalf("base url not honoured:\n%s", out)
	}
}

func TestBaseURLTrailingSlashIsNormalised(t *testing.T) {
	c := good()
	c.BaseURL = "/jellyfin/"
	if out := generate(t, c); !strings.Contains(out, "handle_path /jellyfin/*") {
		t.Fatalf("trailing slash not normalised:\n%s", out)
	}
}

func TestRootBaseURLIsTreatedAsNone(t *testing.T) {
	c := good()
	c.BaseURL = "/"
	out := generate(t, c)
	if strings.Contains(out, "handle_path") {
		t.Fatalf("a root base path should not produce handle_path:\n%s", out)
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

func TestPropagationTimeoutIsGenerous(t *testing.T) {
	// Dynu propagation runs to a couple of minutes; the default gives up
	// sooner and burns a validation attempt.
	if out := generate(t, good()); !strings.Contains(out, "propagation_timeout 5m") {
		t.Fatalf("propagation timeout missing:\n%s", out)
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
