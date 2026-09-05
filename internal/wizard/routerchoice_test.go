package wizard

import (
	"context"
	"strings"
	"testing"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/portmap"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/probe"
)

// reachThePortScreen walks to the manual guide, which is where all of this is
// shown: a router that refuses to map is the only way to see it.
func reachThePortScreen(t *testing.T, h *harness) Model {
	t.Helper()
	ctx := context.Background()
	if err := h.w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.w.SignIn(ctx, "admin", "pw"); err != nil {
		t.Fatal(err)
	}
	if err := h.w.SetDynuKey(ctx, testKey); err != nil {
		t.Fatal(err)
	}
	if err := h.w.ClaimName(ctx, "mymedia", "freeddns.org"); err != nil {
		t.Fatal(err)
	}
	if err := h.w.OpenPort(ctx); err != nil {
		t.Fatal(err)
	}
	m := h.w.Model()
	if m.Screen != ScreenPort {
		t.Fatalf("screen = %s, want the port screen", m.Screen)
	}
	return m
}

// A router with UPnP switched off reports no vendor at all, which is exactly
// the case this screen exists for. The banner is the tier written for it, and
// until now nothing filled it in: identification fell straight through to the
// MAC, and the OUI lists are sparse on purpose.
func TestARouterThatOnlyNamesItselfOnItsAdminPageIsStillIdentified(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.NewMapper = func(string, string) PortMapper {
			return &fakeMapper{err: &portmap.UPnPError{Code: 718}}
		}
	})
	// UPnP off: no vendor, no model, nothing but what the admin page said.
	h.seed.Router.Vendor = ""
	h.seed.Router.Model = ""
	h.seed.Router.MAC = ""
	h.seed.Router.PortMappingAvailable = false
	h.seed.Router.Banner = "FRITZ!Box 7590 - Login"

	m := reachThePortScreen(t, h)
	if !m.Port.RouterGuessed {
		t.Fatal("the admin page named the router and RASA still showed the generic guide")
	}
	if !strings.Contains(m.Port.RouterName, "FRITZ") {
		t.Errorf("router name = %q, want the FRITZ!Box entry", m.Port.RouterName)
	}
}

// When nothing identifies the router the screen used to say "your router" and
// offer general steps with no way to improve on them. The user can read the
// label on the box.
func TestAnUnidentifiedRouterIsOfferedAsAChoice(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.NewMapper = func(string, string) PortMapper {
			return &fakeMapper{err: &portmap.UPnPError{Code: 718}}
		}
	})
	h.seed.Router = probe.Router{Reachable: true, Gateway: h.seed.Router.Gateway}

	m := reachThePortScreen(t, h)
	if m.Port.RouterGuessed {
		t.Fatal("nothing identified the router and RASA claimed to have guessed it")
	}
	if len(m.Port.RouterOptions) < 5 {
		t.Fatalf("only %d routers offered to pick from", len(m.Port.RouterOptions))
	}
	if m.Port.AdminURL == "" {
		t.Error("the gateway address is known but was not passed to the screen")
	}

	ctx := context.Background()
	if err := h.w.ChooseRouter(ctx, "netgear"); err != nil {
		t.Fatalf("ChooseRouter: %v", err)
	}
	m = h.w.Model()
	if m.Port.RouterChosen != "netgear" {
		t.Fatalf("RouterChosen = %q", m.Port.RouterChosen)
	}
	if !strings.Contains(m.Port.RouterName, "NETGEAR") {
		t.Errorf("router name = %q after choosing NETGEAR", m.Port.RouterName)
	}
	// The hedge belongs to a guess. The user just told RASA what it is.
	if m.Port.RouterGuessed {
		t.Error("a router the user chose is reported as a guess")
	}
	// The reason the picker is worth having on this screen at all.
	if m.Port.UPnPPath == "" {
		t.Error("no UPnP path for a router whose catalogue entry has one")
	}
	var sawPath bool
	for _, s := range m.Port.Instructions {
		if strings.Contains(s.Text, "Advanced Setup") {
			sawPath = true
		}
	}
	if !sawPath {
		t.Errorf("the steps are not NETGEAR's: %+v", m.Port.Instructions)
	}
}

// The two ways of overriding identification are opposites, so each has to
// clear the other. Otherwise a router chosen earlier keeps winning over a
// later request for the general steps.
func TestAskingForTheGeneralStepsDropsAChosenRouter(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.NewMapper = func(string, string) PortMapper {
			return &fakeMapper{err: &portmap.UPnPError{Code: 718}}
		}
	})
	reachThePortScreen(t, h)

	ctx := context.Background()
	if err := h.w.ChooseRouter(ctx, "tplink"); err != nil {
		t.Fatal(err)
	}
	if err := h.w.UseGenericGuide(ctx); err != nil {
		t.Fatal(err)
	}
	m := h.w.Model()
	if m.Port.RouterChosen != "" {
		t.Errorf("RouterChosen = %q after asking for the general steps", m.Port.RouterChosen)
	}
	if strings.Contains(m.Port.RouterName, "TP-Link") {
		t.Errorf("still showing the chosen router: %q", m.Port.RouterName)
	}

	// And back the other way: choosing after going generic has to take.
	if err := h.w.ChooseRouter(ctx, "tplink"); err != nil {
		t.Fatal(err)
	}
	if m := h.w.Model(); !strings.Contains(m.Port.RouterName, "TP-Link") {
		t.Errorf("router name = %q after choosing TP-Link", m.Port.RouterName)
	}
}

// A key from a stale page, or one that never existed, must not quietly hand
// the user another router's menu path.
func TestChoosingARouterThatIsNotInTheCatalogueIsRefused(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.NewMapper = func(string, string) PortMapper {
			return &fakeMapper{err: &portmap.UPnPError{Code: 718}}
		}
	})
	before := reachThePortScreen(t, h)

	ctx := context.Background()
	for _, key := range []string{"nosuchrouter", "_default", ""} {
		if err := h.w.ChooseRouter(ctx, key); err == nil {
			t.Errorf("ChooseRouter(%q) was accepted", key)
		}
	}
	if got := h.w.Model().Port.RouterName; got != before.Port.RouterName {
		t.Errorf("the guide changed to %q", got)
	}
}
