package mode

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/probe"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/state"
)

// SPEC.md §5 requires that every branch terminates in a working mode. These
// tests exist mostly to prove there is no failure leaf.

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

// direct builds a healthy, unremarkable home network: real public IPv4, router
// agrees about its WAN address, UPnP works, both ports free.
func direct() probe.Result {
	return probe.Result{
		Internet: probe.Internet{
			Reachable: true,
			PublicV4:  addr("203.0.113.5"),
		},
		Router: probe.Router{
			Reachable:            true,
			Gateway:              addr("192.168.1.1"),
			WANAddress:           addr("203.0.113.5"),
			PortMappingAvailable: true,
		},
		Ports: probe.Ports{
			Free:   map[int]bool{PortPreferred: true, PortFallback: true},
			Holder: map[int]string{},
		},
	}
}

func TestDirectConnectionChoosesPublicOn443(t *testing.T) {
	d := Choose(direct())

	if d.Mode != state.ModePublic {
		t.Fatalf("mode = %s, want A", d.Mode)
	}
	if d.ListenPort != PortPreferred {
		t.Fatalf("port = %d, want %d", d.ListenPort, PortPreferred)
	}
	if !d.NeedsPortMapping || d.NeedsManualForward {
		t.Fatalf("expected automatic mapping: %+v", d)
	}
	if d.Blocked() {
		t.Fatalf("unexpected blocker: %s", d.Blocker)
	}
}

func TestCGNATWithIPv6ChoosesIPv6Mode(t *testing.T) {
	// The case that matters: IPv4 is carrier-NATed, but native IPv6 makes the
	// server reachable anyway.
	r := direct()
	r.Router.WANAddress = addr("100.87.4.12") // RFC 6598 shared address space
	r.Internet.PublicV6 = addr("2001:db8::1")

	d := Choose(r)
	if d.Mode != state.ModeIPv6 {
		t.Fatalf("mode = %s, want A6", d.Mode)
	}
	if d.NeedsPortMapping {
		t.Fatal("IPv6 mode should not request an IPv4 port mapping")
	}
	if !hasWarning(d, WarnIPv6Only) {
		t.Fatal("user should be warned that IPv4-only guests cannot connect")
	}
}

func TestCGNATWithoutIPv6FallsBackToMesh(t *testing.T) {
	r := direct()
	r.Router.WANAddress = addr("100.87.4.12")

	d := Choose(r)
	if d.Mode != state.ModeMesh {
		t.Fatalf("mode = %s, want B", d.Mode)
	}
	if d.Blocked() {
		t.Fatal("CGNAT must not be a dead end — mesh is the universal fallback")
	}
	if !hasWarning(d, WarnCGNATNoIPv4) {
		t.Fatal("expected a CGNAT warning")
	}
}

func TestDoubleNATIsDetectedLikeCGNAT(t *testing.T) {
	// A router behind another router looks the same and fails the same way.
	r := direct()
	r.Router.WANAddress = addr("192.168.0.50")

	behind, known := BehindCGNAT(r)
	if !known || !behind {
		t.Fatalf("double NAT not detected: behind=%v known=%v", behind, known)
	}
	if d := Choose(r); d.Mode == state.ModePublic {
		t.Fatal("should not attempt public mode behind double NAT")
	}
}

func TestPort443BusyFallsBackTo8443(t *testing.T) {
	r := direct()
	r.Ports.Free[PortPreferred] = false
	r.Ports.Holder[PortPreferred] = "IIS"

	d := Choose(r)
	if d.Mode != state.ModePublic {
		t.Fatalf("a busy 443 must not change the mode, got %s", d.Mode)
	}
	if d.ListenPort != PortFallback {
		t.Fatalf("port = %d, want %d", d.ListenPort, PortFallback)
	}
	if !hasWarning(d, WarnNonStandardPort) {
		t.Fatal("user should be told their address needs a port on the end")
	}
}

func TestBothPortsBusyBlocksWithNamedHolder(t *testing.T) {
	r := direct()
	r.Ports.Free[PortPreferred] = false
	r.Ports.Free[PortFallback] = false
	r.Ports.Holder[PortPreferred] = "IIS"

	d := Choose(r)
	if !d.Blocked() || d.Blocker != BlockerPortsUnavailable {
		t.Fatalf("expected a ports blocker, got %+v", d)
	}
	if d.PortHolder != "IIS" {
		t.Fatalf("holder should be surfaced so the user can act: %q", d.PortHolder)
	}
	if !strings.Contains(d.Reason, "IIS") {
		t.Fatalf("reason should name the holder: %q", d.Reason)
	}
}

func TestNoUPnPStillReachesPublicModeViaManualForward(t *testing.T) {
	// This is the refinement over the SPEC §5 diagram: UPnP being unavailable
	// is not a reason to fall back to mesh when the connection is direct. A
	// manual static forward is more durable than a lease anyway (SPEC.md §6).
	r := direct()
	r.Router.PortMappingAvailable = false

	d := Choose(r)
	if d.Mode != state.ModePublic {
		t.Fatalf("mode = %s, want A with manual forwarding", d.Mode)
	}
	if !d.NeedsManualForward || d.NeedsPortMapping {
		t.Fatalf("expected manual forwarding: %+v", d)
	}
	if !hasWarning(d, WarnNoUPnP) {
		t.Fatal("expected a warning that automatic setup is off")
	}
}

func TestUnreachableRouterProceedsWithManualForward(t *testing.T) {
	r := direct()
	r.Router = probe.Router{Reachable: false}

	d := Choose(r)
	if d.Mode != state.ModePublic || !d.NeedsManualForward {
		t.Fatalf("should still proceed manually: %+v", d)
	}
	if !hasWarning(d, WarnRouterUnreachable) {
		t.Fatal("expected a router-unreachable warning")
	}
	// CGNAT could not be ruled out, and the reason must say so rather than
	// implying a clean direct connection.
	if !strings.Contains(d.Reason, "CGNAT could not be ruled out") {
		t.Fatalf("reason hides the uncertainty: %q", d.Reason)
	}
}

func TestNoInternetIsBlocked(t *testing.T) {
	r := direct()
	r.Internet = probe.Internet{Reachable: false}

	d := Choose(r)
	if !d.Blocked() || d.Blocker != BlockerNoInternet {
		t.Fatalf("expected a no-internet blocker, got %+v", d)
	}
}

func TestCGNATUnknownWithoutRouterWANAddress(t *testing.T) {
	// Without UPnP the router will not report its WAN address. An unknown
	// answer must not be silently treated as "not behind CGNAT".
	r := direct()
	r.Router.WANAddress = netip.Addr{}

	behind, known := BehindCGNAT(r)
	if known {
		t.Fatalf("comparison should be impossible, got behind=%v", behind)
	}
}

func TestCGNATUnknownWithoutPublicIPv4(t *testing.T) {
	r := direct()
	r.Internet.PublicV4 = netip.Addr{}
	r.Internet.PublicV6 = addr("2001:db8::1")

	if _, known := BehindCGNAT(r); known {
		t.Fatal("cannot compare WAN address without an observed IPv4 address")
	}
}

func TestIPv6OnlyConnectionIsNotMistakenForCGNAT(t *testing.T) {
	// No IPv4 at all is a different condition from IPv4-behind-CGNAT, and
	// must not silently route to mesh.
	r := direct()
	r.Internet.PublicV4 = netip.Addr{}
	r.Internet.PublicV6 = addr("2001:db8::1")
	r.Router.WANAddress = netip.Addr{}

	d := Choose(r)
	if d.Mode == state.ModeMesh {
		t.Fatal("an IPv6-capable connection should not fall back to mesh")
	}
}

func TestDirectIPv4OnlyWarnsAboutMissingIPv6(t *testing.T) {
	if d := Choose(direct()); !hasWarning(d, WarnNoIPv6) {
		t.Fatal("expected a no-IPv6 note")
	}
}

func TestUnprobedPortIsTreatedAsBusy(t *testing.T) {
	// Assuming a port is free is the dangerous direction.
	r := direct()
	r.Ports = probe.Ports{}

	d := Choose(r)
	if !d.Blocked() {
		t.Fatal("an unprobed port map must not be read as 'everything is free'")
	}
}

func TestEveryOutcomeCarriesAReason(t *testing.T) {
	// SPEC.md §15: log why a branch was taken, not merely which one.
	cases := map[string]probe.Result{
		"direct":       direct(),
		"cgnat_v6":     withCGNAT(direct(), true),
		"cgnat_no_v6":  withCGNAT(direct(), false),
		"no_upnp":      withoutUPnP(direct()),
		"no_internet":  func() probe.Result { r := direct(); r.Internet.Reachable = false; return r }(),
		"ports_in_use": bothPortsBusy(direct()),
	}
	for name, r := range cases {
		if d := Choose(r); strings.TrimSpace(d.Reason) == "" {
			t.Errorf("%s: decision carries no reason", name)
		}
	}
}

func TestNoScenarioProducesAnUnhandledOutcome(t *testing.T) {
	// A decision must always be actionable: either a mode to proceed with, or
	// an explicit blocker. Never both empty.
	for name, r := range map[string]probe.Result{
		"direct":      direct(),
		"cgnat_v6":    withCGNAT(direct(), true),
		"cgnat_no_v6": withCGNAT(direct(), false),
		"no_upnp":     withoutUPnP(direct()),
		"no_router":   func() probe.Result { r := direct(); r.Router = probe.Router{}; return r }(),
	} {
		d := Choose(r)
		if d.Mode == state.ModeUnknown && !d.Blocked() {
			t.Errorf("%s: no mode and no blocker — this is the dead end SPEC forbids", name)
		}
	}
}

func TestStateWarningsConvert(t *testing.T) {
	d := Choose(withCGNAT(direct(), false))
	sw := d.StateWarnings()
	if len(sw) != len(d.Warnings) {
		t.Fatalf("got %d state warnings, want %d", len(sw), len(d.Warnings))
	}
	if len(sw) > 0 && (sw[0].Code == "" || sw[0].Text == "") {
		t.Fatalf("warning lost content in conversion: %+v", sw[0])
	}
}

func withCGNAT(r probe.Result, withV6 bool) probe.Result {
	r.Router.WANAddress = addr("100.87.4.12")
	if withV6 {
		r.Internet.PublicV6 = addr("2001:db8::1")
	}
	return r
}

func withoutUPnP(r probe.Result) probe.Result {
	r.Router.PortMappingAvailable = false
	return r
}

func bothPortsBusy(r probe.Result) probe.Result {
	r.Ports.Free[PortPreferred] = false
	r.Ports.Free[PortFallback] = false
	return r
}

func hasWarning(d Decision, code string) bool {
	for _, w := range d.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}
