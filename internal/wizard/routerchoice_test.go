package wizard

import (
	"context"
	"strings"
	"testing"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/mode"
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

// Pressing Test again must say what it did. A retry that fails re-renders the
// same screen, so without an outcome the button is indistinguishable from a
// no-op -- which is how it was reported: "I enabled UPnP, told RASA to test
// again, then nothing happened."
func TestTestAgainSaysWhatItDid(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.NewMapper = func(string, string) PortMapper {
			return &fakeMapper{err: &portmap.UPnPError{Code: 718}}
		}
	})
	// Arrive with UPnP switched off, which is what puts anyone on this screen.
	h.seed.Router.PortMappingAvailable = false

	// Arrive the way a user does. ClaimName starts the port step itself, so
	// this is the first attempt, not a retry.
	ctx := context.Background()
	for _, step := range []func() error{
		func() error { return h.w.Start(ctx) },
		func() error { return h.w.SignIn(ctx, "admin", "pw") },
		func() error { return h.w.SetDynuKey(ctx, testKey) },
		func() error { return h.w.ClaimName(ctx, "mymedia", "freeddns.org") },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	m := h.w.Model()
	if m.Screen != ScreenPort {
		t.Fatalf("screen = %s, want the port screen", m.Screen)
	}
	if m.Port.RetryOutcome != "" {
		t.Errorf("an outcome was claimed before any retry: %q", m.Port.RetryOutcome)
	}

	// Press it with nothing changed: it still has to answer.
	if err := h.w.OpenPort(ctx); err != nil {
		t.Fatal(err)
	}
	m = h.w.Model()
	if m.Port.RetryOutcome == "" {
		t.Fatal("Test again produced no outcome at all")
	}
	if !strings.Contains(m.Port.RetryOutcome, "isn't offering") {
		t.Errorf("outcome does not say UPnP is still off: %q", m.Port.RetryOutcome)
	}
	if m.Port.CheckedAt == "" {
		t.Error("no time recorded, so a repeated identical answer still looks like nothing happened")
	}

	// Now the user turns UPnP on. The router advertises it but still refuses
	// the mapping -- the outcome has to distinguish those two things, because
	// "did my change take effect?" is the actual question being asked.
	h.seed.Router.PortMappingAvailable = true
	if err := h.w.OpenPort(ctx); err != nil {
		t.Fatal(err)
	}
	m = h.w.Model()
	if !strings.Contains(m.Port.RetryOutcome, "now offering") {
		t.Errorf("turning UPnP on was not reported back: %q", m.Port.RetryOutcome)
	}
	// And the "go and turn UPnP on" notice must stop being shown, since it is
	// on.
	if m.Port.AutomaticOff {
		t.Error("still telling the user to enable UPnP after they enabled it")
	}
}

// "Automatic port opening is unavailable" covers several different situations
// that need completely different advice, and RASA knew which one it was and
// said none of it. The worst case is a router that has a UPnP switch for media
// sharing and no port opening at all: telling that user to go and turn UPnP on
// sends them to a setting they have already turned on.
func TestTheScreenSaysWhyUPnPIsUnavailable(t *testing.T) {
	cases := []struct {
		status probe.UPnPStatus
		want   string
	}{
		{probe.UPnPNoPortService, "media sharing"},
		{probe.UPnPNoReply, "did not answer"},
		{probe.UPnPNoDescription, "would not say what it can do"},
	}
	for _, c := range cases {
		t.Run(string(c.status), func(t *testing.T) {
			h := newHarness(t, func(o *Options) {
				o.NewMapper = func(string, string) PortMapper {
					return &fakeMapper{err: &portmap.UPnPError{Code: 718}}
				}
			})
			h.seed.Router.PortMappingAvailable = false
			h.seed.Router.UPnPStatus = c.status

			m := reachThePortScreen(t, h)
			if !strings.Contains(m.Port.UPnPProblem, c.want) {
				t.Errorf("UPnPProblem = %q, want it to mention %q", m.Port.UPnPProblem, c.want)
			}
		})
	}

	// And nothing is claimed when the router does offer it.
	h := newHarness(t, func(o *Options) {
		o.NewMapper = func(string, string) PortMapper {
			return &fakeMapper{err: &portmap.UPnPError{Code: 718}}
		}
	})
	h.seed.Router.UPnPStatus = probe.UPnPAvailable
	m := reachThePortScreen(t, h)
	if m.Port.UPnPProblem != "" {
		t.Errorf("a problem was reported for a router that offers port opening: %q", m.Port.UPnPProblem)
	}
}

// The address grows a ":8443" that nothing else on the screen explains, and
// the recovery file outlives RASA -- so the reason has to be recorded, and it
// has to be the right reason. mode.Choose raises this warning only when 443 is
// busy on this computer and says so, which is wrong for a port the router is
// already forwarding to a different device.
func TestMovingToTheFallbackPortSaysWhy(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		// 718 is ConflictInMappingEntry: the router already forwards 443
		// somewhere else, which the local port probe cannot see.
		o.NewMapper = func(string, string) PortMapper {
			return &conflictThenOKMapper{}
		}
	})

	ctx := context.Background()
	for _, step := range []func() error{
		func() error { return h.w.Start(ctx) },
		func() error { return h.w.SignIn(ctx, "admin", "pw") },
		func() error { return h.w.SetDynuKey(ctx, testKey) },
		func() error { return h.w.ClaimName(ctx, "mymedia", "freeddns.org") },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}

	var found string
	for _, w := range h.w.Model().Warnings {
		if w.Code == mode.WarnNonStandardPort {
			found = w.Text
		}
	}
	if found == "" {
		t.Fatal("the address moved to the alternative port and nothing said why")
	}
	if !strings.Contains(found, "8443") {
		t.Errorf("the warning does not name the port: %q", found)
	}
	if strings.Contains(found, "on this computer") {
		t.Errorf("blamed this computer for a port the router forwards elsewhere: %q", found)
	}
	if !strings.Contains(found, "network") {
		t.Errorf("the warning does not say where the conflict is: %q", found)
	}
}

// conflictThenOKMapper refuses 443 the way a router with an existing forward
// does, and accepts the alternative.
type conflictThenOKMapper struct{ calls int }

func (m *conflictThenOKMapper) Add(ctx context.Context, req portmap.Request) (*portmap.Result, error) {
	m.calls++
	if req.ExternalPort == mode.PortPreferred {
		return nil, &portmap.UPnPError{Code: 718}
	}
	return &portmap.Result{
		Mapping: portmap.Mapping{
			ExternalPort: req.ExternalPort,
			InternalPort: req.InternalPort,
			LeaseSeconds: 0,
		},
		PermanentRequested: true,
		VerifiedByReadback: true,
	}, nil
}
