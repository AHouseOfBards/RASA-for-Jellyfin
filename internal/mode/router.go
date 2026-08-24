// Package mode decides how remote access will be reached.
//
// This is SPEC.md §5: pre-flight is a router, not a diagnostic gate. It probes,
// picks a mode, and proceeds — every branch terminates in a working mode, and
// the user is told the outcome rather than asked to decide.
//
// Choose is a pure function over probe.Result so the whole decision is
// testable without a network. It is also where bugs will hide, because it
// encodes every assumption about home networks that the rest of RASA relies on.
package mode

import (
	"fmt"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/probe"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/state"
)

// Preferred and fallback listener ports, re-exported so decision logic reads
// naturally here. They are defined in probe because probing them happens
// first and this package already depends on it.
const (
	PortPreferred = probe.PortPreferred
	PortFallback  = probe.PortFallback
)

// Blocker explains why a mode could not be reached, for cases the user must
// resolve before setup can continue.
type Blocker string

const (
	BlockerNone Blocker = ""
	// BlockerNoInternet means no public address could be resolved.
	BlockerNoInternet Blocker = "no_internet"
	// BlockerPortsUnavailable means both candidate ports are held locally.
	BlockerPortsUnavailable Blocker = "ports_unavailable"
)

// Decision is the outcome of the mode router.
type Decision struct {
	Mode state.Mode
	// ListenPort is the external port Caddy will serve on. Zero when blocked.
	ListenPort int

	// NeedsPortMapping is true when a router mapping should be attempted.
	NeedsPortMapping bool
	// NeedsManualForward is true when UPnP is unavailable but the connection
	// is direct, so a guided manual forward is the path forward.
	NeedsManualForward bool

	// Blocker is set when the user must resolve something first.
	Blocker Blocker
	// PortHolder names what is occupying the ports, when blocked by them.
	PortHolder string

	// Reason is the human-readable justification, logged verbatim via
	// Logger.Decision. SPEC.md §15: record why a branch was taken, not merely
	// which one — this single line answers most support questions.
	Reason string

	// Warnings are conditions that succeeded but will bite later. Each must
	// also reach the recovery file, because RASA will not exist when they
	// matter.
	Warnings []Warning
}

// Warning is a persisted caution carrying a stable code.
type Warning struct {
	Code string
	Text string
}

// Warning codes, stable across releases so the recovery file and support
// threads can refer to them.
const (
	WarnCGNATNoIPv4       = "cgnat_no_ipv4"
	WarnNonStandardPort   = "non_standard_port"
	WarnNoUPnP            = "no_upnp"
	WarnRouterUnreachable = "router_unreachable"
	WarnIPv6Only          = "ipv6_only"
	WarnNoIPv6            = "no_ipv6"
)

// BehindCGNAT reports whether the connection is behind carrier-grade or double
// NAT.
//
// The check that works is a comparison, not a lookup: if the router's own idea
// of its external address differs from what the outside world observes, there
// is another layer of NAT above it and no port forward on this router can ever
// succeed — regardless of what its admin page claims.
//
// The second return value reports whether the comparison could be made at all.
// Without UPnP the router will not report its WAN address, and an unknown
// answer must not be silently treated as "not behind CGNAT".
func BehindCGNAT(r probe.Result) (behind, known bool) {
	if !r.Router.Reachable || !r.Router.WANAddress.IsValid() {
		return false, false
	}
	if !r.Internet.HasV4() {
		return false, false
	}
	return r.Router.WANAddress != r.Internet.PublicV4, true
}

// Choose picks the access mode.
func Choose(r probe.Result) Decision {
	if !r.Internet.Reachable {
		return Decision{
			Mode:    state.ModeUnknown,
			Blocker: BlockerNoInternet,
			Reason:  "no public address could be resolved, so nothing can be set up yet",
		}
	}

	behindCGNAT, known := BehindCGNAT(r)

	// Behind CGNAT, no IPv4 port forward can work. IPv6 is frequently the only
	// path that does, which is the entire basis of Mode A6.
	if known && behindCGNAT {
		if r.Internet.HasV6() {
			return Decision{
				Mode:             state.ModeIPv6,
				ListenPort:       PortPreferred,
				NeedsPortMapping: false,
				Reason: fmt.Sprintf(
					"behind CGNAT (router WAN %s differs from observed %s) but this connection has native IPv6, so remote access will use IPv6 only",
					r.Router.WANAddress, r.Internet.PublicV4),
				Warnings: []Warning{{
					Code: WarnIPv6Only,
					Text: "Your server is reachable over IPv6 only. Guests on IPv4-only networks will not be able to connect.",
				}},
			}
		}
		return Decision{
			Mode:   state.ModeMesh,
			Reason: "behind CGNAT with no native IPv6, so no port can be opened and a private connection is the only option",
			Warnings: []Warning{{
				Code: WarnCGNATNoIPv4,
				Text: "Your internet provider does not give this connection a direct address, so guests will need to be invited to a private network.",
			}},
		}
	}

	// Direct connection, or CGNAT status unknown. Pick a port first: this is
	// about local availability only.
	port, holder := choosePort(r)
	if port == 0 {
		return Decision{
			Mode:       state.ModeUnknown,
			Blocker:    BlockerPortsUnavailable,
			PortHolder: holder,
			Reason: fmt.Sprintf(
				"ports %d and %d are both in use on this computer (%s)",
				PortPreferred, PortFallback, fallbackHolder(holder)),
		}
	}

	d := Decision{Mode: state.ModePublic, ListenPort: port}

	if port != PortPreferred {
		d.Warnings = append(d.Warnings, Warning{
			Code: WarnNonStandardPort,
			Text: fmt.Sprintf("Your address needs :%d on the end, because port %d is already in use on this computer.", port, PortPreferred),
		})
	}

	switch {
	case !r.Router.Reachable:
		// No gateway answered. The connection works, so setup can proceed, but
		// nothing can be mapped automatically and CGNAT could not be ruled out.
		d.NeedsManualForward = true
		d.Reason = fmt.Sprintf("no router responded, so port %d must be forwarded by hand", port)
		d.Warnings = append(d.Warnings, Warning{
			Code: WarnRouterUnreachable,
			Text: "RASA could not talk to your router, so the port was not opened automatically.",
		})
	case r.Router.PortMappingAvailable:
		d.NeedsPortMapping = true
		d.Reason = fmt.Sprintf("direct connection and the router accepts port mapping, so port %d will be opened automatically", port)
	default:
		// Direct connection but UPnP is off. This is emphatically not a dead
		// end: a manual static forward is more durable than a UPnP lease
		// anyway, because it is a stored setting rather than a lease.
		d.NeedsManualForward = true
		d.Reason = fmt.Sprintf("direct connection but port mapping is unavailable, so port %d will be forwarded with guided instructions", port)
		d.Warnings = append(d.Warnings, Warning{
			Code: WarnNoUPnP,
			Text: "Automatic port setup is turned off on your router, so you will be shown how to open the port yourself.",
		})
	}

	// A direct IPv4 connection with no IPv6 is fine, but worth recording:
	// IPv6-only guests are increasingly common on mobile networks.
	if !r.Internet.HasV6() {
		d.Warnings = append(d.Warnings, Warning{
			Code: WarnNoIPv6,
			Text: "This connection has no IPv6 address, so only an IPv4 address will be published.",
		})
	}

	if !known {
		d.Reason += "; CGNAT could not be ruled out because the router did not report its external address"
	}

	return d
}

// choosePort returns the first locally free candidate port, or 0 with the
// holder of the preferred port when neither is available.
func choosePort(r probe.Result) (port int, holder string) {
	if r.Ports.IsFree(PortPreferred) {
		return PortPreferred, ""
	}
	if r.Ports.IsFree(PortFallback) {
		return PortFallback, r.Ports.HolderOf(PortPreferred)
	}
	h := r.Ports.HolderOf(PortPreferred)
	if h == "" {
		h = r.Ports.HolderOf(PortFallback)
	}
	return 0, h
}

func fallbackHolder(h string) string {
	if h == "" {
		return "the programs using them could not be identified"
	}
	return h
}

// Warnings returns the decision's warnings as state warnings, ready to persist.
func (d Decision) StateWarnings() []state.Warning {
	out := make([]state.Warning, 0, len(d.Warnings))
	for _, w := range d.Warnings {
		out = append(out, state.Warning{Code: w.Code, Text: w.Text})
	}
	return out
}

// Blocked reports whether the user must resolve something before continuing.
func (d Decision) Blocked() bool { return d.Blocker != BlockerNone }
