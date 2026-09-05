package wizard

import (
	"context"
	"strings"
	"testing"

	"net/netip"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/mode"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/portmap"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/reach"
)

// A Jellyfin server with a base path answers only under it. Everything that
// names a URL has to agree, and every one of them was separately wrong: the
// proxy route, the address published to Jellyfin, the address shown to the
// user, and the URL the reachability check fetched.
func withBasePath(t *testing.T, base string) *harness {
	t.Helper()
	h := newHarness(t, nil)
	h.jf.baseURL = base
	h.runHappyPath(t)
	return h
}

func TestTheProxyIsToldAboutJellyfinsBasePath(t *testing.T) {
	h := withBasePath(t, "/jellyfin")

	if len(h.prox.installed) == 0 {
		t.Fatal("the proxy was never installed")
	}
	cfg := h.prox.installed[len(h.prox.installed)-1]
	if cfg.BaseURL != "/jellyfin" {
		// Without this the generated Caddyfile forwards paths Jellyfin 404s,
		// through a proxy that starts cleanly and a setup that says it worked.
		t.Errorf("proxy config BaseURL = %q, want /jellyfin", cfg.BaseURL)
	}
}

func TestTheAddressShownToTheUserIncludesTheBasePath(t *testing.T) {
	h := withBasePath(t, "/jellyfin")

	// This is the address on the done screen, in the QR code, and in the
	// recovery file. Without the base path it is an address that 404s.
	if got := h.w.Model().Result.URL; got != "https://mymedia.freeddns.org/jellyfin" {
		t.Errorf("URL = %q, want the base path included", got)
	}
}

func TestJellyfinIsPublishedTheAddressClientsActuallyUse(t *testing.T) {
	h := withBasePath(t, "/jellyfin")

	if len(h.jf.applied) == 0 {
		t.Fatal("Jellyfin was never configured")
	}
	got := h.jf.applied[len(h.jf.applied)-1].PublicURL
	if got != "https://mymedia.freeddns.org/jellyfin" {
		// Jellyfin hands this to apps as the address to come back on. Wrong
		// here means playback breaks off-network in a way that looks like a
		// proxy fault.
		t.Errorf("published URL = %q, want the base path included", got)
	}
}

// The verification fetch has to ask for a URL the server answers. Asking for
// the root of a server with a base path gets a 404, which reach scores as
// "something else replied" -- so setup would move to 8443 and then blame the
// user's port forwarding for a problem RASA created.
func TestVerificationFetchesAURLTheServerAnswers(t *testing.T) {
	var asked []string
	h := newHarness(t, func(o *Options) {
		o.NewReach = func(netip.Addr) Reacher { return &recordingReach{urls: &asked} }
	})
	h.jf.baseURL = "/jellyfin"
	h.runHappyPath(t)

	if len(asked) == 0 {
		t.Fatal("nothing was verified")
	}
	for _, u := range asked {
		if !strings.Contains(u, "/jellyfin/System/Info/Public") {
			t.Errorf("verified %q, which a server with a base path 404s", u)
		}
	}
	if got := h.w.Model().Result.URL; strings.Contains(got, ":8443") {
		t.Errorf("address is %q: setup moved ports over its own bad URL", got)
	}
}

func TestNoBasePathLeavesEveryAddressUnchanged(t *testing.T) {
	h := withBasePath(t, "")

	if got := h.w.Model().Result.URL; got != "https://mymedia.freeddns.org" {
		t.Errorf("URL = %q", got)
	}
	if cfg := h.prox.installed[len(h.prox.installed)-1]; cfg.BaseURL != "" {
		t.Errorf("proxy config BaseURL = %q, want empty", cfg.BaseURL)
	}
}

// Jellyfin's settings page accepts the value with or without slashes, and the
// stored value goes straight into a URL.
func TestAnUntidyBasePathIsNormalisedEverywhere(t *testing.T) {
	for _, raw := range []string{"jellyfin", "/jellyfin/", " /jellyfin "} {
		h := newHarness(t, nil)
		h.jf.baseURL = raw
		h.runHappyPath(t)

		if got := h.w.Model().Result.URL; got != "https://mymedia.freeddns.org/jellyfin" {
			t.Errorf("base %q produced URL %q", raw, got)
		}
	}
}

// ---------------------------------------------------------------------------

// The fallback port switch happens because something else on the network
// already answers on 443 -- which means the router forwards 443 somewhere
// else. Moving the listener without asking the router to send 8443 here just
// changes which port nothing arrives on.
func TestMovingToTheFallbackPortAsksTheRouterToForwardIt(t *testing.T) {
	mapper := &recordingMapper{}
	h := newHarness(t, func(o *Options) {
		o.NewReach = func(netip.Addr) Reacher {
			return &switchingReach{failPort: mode.PortPreferred}
		}
		o.NewMapper = func(string, string) PortMapper { return mapper }
	})
	h.runHappyPath(t)

	var forwarded []int
	for _, r := range mapper.requests {
		forwarded = append(forwarded, r.ExternalPort)
	}
	if !containsPort(forwarded, mode.PortFallback) {
		t.Errorf("router was asked to forward %v, never the fallback port %d",
			forwarded, mode.PortFallback)
	}
	if got := h.w.Model().Result.URL; !strings.Contains(got, ":8443") {
		t.Errorf("address is %q, want the fallback port", got)
	}
}

func containsPort(ports []int, want int) bool {
	for _, p := range ports {
		if p == want {
			return true
		}
	}
	return false
}

// recordingReach reports success and remembers every URL it was given.
type recordingReach struct{ urls *[]string }

func (r *recordingReach) CheckURL(ctx context.Context, url, contains string) reach.Result {
	*r.urls = append(*r.urls, url)
	return reach.Result{Status: reach.Reachable, Method: "fake"}
}

// recordingMapper answers like the real thing and remembers what it was asked.
type recordingMapper struct{ requests []portmap.Request }

func (m *recordingMapper) Add(ctx context.Context, r portmap.Request) (*portmap.Result, error) {
	m.requests = append(m.requests, r)
	return &portmap.Result{
		Mapping: portmap.Mapping{
			ExternalPort: r.ExternalPort, InternalPort: r.InternalPort,
		},
		PermanentRequested: true,
		VerifiedByReadback: true,
	}, nil
}

// The name screen shows the address the user is about to commit to, and the
// confirmation step asks them to approve it. Reading the base path only at the
// proxy step meant both showed an address that would not work.
func TestTheAddressIsRightBeforeTheNameIsClaimed(t *testing.T) {
	h := newHarness(t, nil)
	h.jf.baseURL = "/jellyfin"

	ctx := context.Background()
	if err := h.w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.w.SignIn(ctx, "admin", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if err := h.w.SetDynuKey(ctx, testKey); err != nil {
		t.Fatal(err)
	}
	if err := h.w.ClaimName(ctx, "mymedia", "freeddns.org"); err != nil {
		t.Fatal(err)
	}

	if got := h.w.Model().Result.URL; got != "https://mymedia.freeddns.org/jellyfin" {
		t.Errorf("URL on the name screen = %q, want the base path included", got)
	}
}

// The API-key path validates the key by reading the network configuration, so
// the base path is already in hand — asking for it again would be a second
// round trip for a value it is holding.
func TestTheAPIKeyPathLearnsTheBasePathToo(t *testing.T) {
	h := newHarness(t, nil)
	h.jf.baseURL = "/jellyfin"

	ctx := context.Background()
	if err := h.w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.w.UseAPIKey(ctx, "a-jellyfin-api-key"); err != nil {
		t.Fatal(err)
	}
	if err := h.w.SetDynuKey(ctx, testKey); err != nil {
		t.Fatal(err)
	}
	if err := h.w.ClaimName(ctx, "mymedia", "freeddns.org"); err != nil {
		t.Fatal(err)
	}

	if got := h.w.Model().Result.URL; got != "https://mymedia.freeddns.org/jellyfin" {
		t.Errorf("URL = %q, want the base path included", got)
	}
}
