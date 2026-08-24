// Package probe describes what RASA learns about the machine and network
// before it decides anything.
//
// This file holds the result types only. The probing itself is task 3; the
// types exist first because the mode router (task 4) is a pure function over
// them and can be built and tested without touching a network.
//
// Everything in a Result is an observation, never a conclusion. "The router
// reports WAN address X" belongs here; "therefore we are behind CGNAT" is the
// router's job, so that the reasoning lives in one tested place.
package probe

import "net/netip"

// Deployment describes how Jellyfin is running, which changes both the address
// to proxy to and the value written into KnownProxies (SPEC.md §13).
type Deployment string

const (
	DeploymentUnknown Deployment = ""
	// DeploymentNative is Jellyfin installed directly on this host.
	DeploymentNative Deployment = "native"
	// DeploymentContainer is Jellyfin in a container on this host. It sees the
	// proxy arriving from the bridge gateway, not from 127.0.0.1.
	DeploymentContainer Deployment = "container"
	// DeploymentRemote is Jellyfin on another machine on the LAN.
	DeploymentRemote Deployment = "remote"
)

// Jellyfin is what was found of the media server.
type Jellyfin struct {
	Found bool
	// Address is host:port as RASA will proxy to it.
	Address string
	// Version as reported by the server, e.g. "10.11.7".
	Version string
	// MeetsMinimum records whether Version satisfies the supported floor.
	MeetsMinimum bool
	Deployment   Deployment
	// ProxySourceAddress is what Jellyfin will see connections coming from,
	// and therefore what belongs in KnownProxies.
	ProxySourceAddress string
}

// Internet is what the outside world reports about this connection.
type Internet struct {
	// Reachable is false when no public address could be resolved at all.
	Reachable bool
	// PublicV4 is the IPv4 address observed by an external service. Invalid
	// if the connection has no IPv4 path.
	PublicV4 netip.Addr
	// PublicV6 is the globally routable IPv6 address, if any.
	PublicV6 netip.Addr
}

// HasV4 reports whether a public IPv4 address was observed.
func (i Internet) HasV4() bool { return i.PublicV4.IsValid() }

// HasV6 reports whether a globally routable IPv6 address was observed.
//
// An IPv4-mapped address (::ffff:203.0.113.5) is an IPv4 address wearing an
// IPv6 costume and must not count. Mode A6 exists precisely because IPv6 is a
// separate path to the host, so treating a mapped address as IPv6 would route
// a connection that has none down it.
func (i Internet) HasV6() bool {
	return i.PublicV6.IsValid() &&
		i.PublicV6.Is6() &&
		!i.PublicV6.Is4In6() &&
		i.PublicV6.IsGlobalUnicast()
}

// Router is what the local gateway will admit to.
type Router struct {
	// Reachable is false when no gateway responded at all.
	Reachable bool
	Gateway   netip.Addr
	// WANAddress is what the router reports as its own external address via
	// IGD GetExternalIPAddress. Invalid when UPnP is unavailable, in which
	// case the CGNAT comparison cannot be made.
	WANAddress netip.Addr
	// PortMappingAvailable is true when the gateway accepted an IGD or NAT-PMP
	// conversation. It does not promise a mapping will be granted.
	PortMappingAvailable bool
	// Vendor and Model drive the router-specific instructions in routers.json.
	Vendor string
	Model  string

	// ControlURL and ServiceType address the WAN connection service that
	// mapping requests are sent to. They are carried here so task 5 does not
	// repeat SSDP discovery — which costs seconds and can fail on a busy
	// wireless network even when it just succeeded.
	ControlURL  string
	ServiceType string

	// MAC is the gateway's hardware address, used to identify the vendor when
	// UPnP is off entirely. It is the only identification tier that needs no
	// cooperation from the router.
	MAC string
}

// Ports records local availability. This is about what is bound on this
// machine, never about what is reachable from outside — that cannot be
// determined from inside the network.
type Ports struct {
	// Free maps a port number to whether nothing local holds it.
	Free map[int]bool
	// Holder names the process occupying a port, where it could be identified.
	// Naming it turns an opaque failure into an obvious one (SPEC.md §15).
	Holder map[int]string
}

// IsFree reports whether the port is available locally. An unprobed port is
// treated as unavailable: assuming a port is free is the dangerous direction.
func (p Ports) IsFree(port int) bool {
	if p.Free == nil {
		return false
	}
	return p.Free[port]
}

// HolderOf returns the process holding a port, or "".
func (p Ports) HolderOf(port int) string {
	if p.Holder == nil {
		return ""
	}
	return p.Holder[port]
}

// Host is the machine RASA is running on.
type Host struct {
	// LANAddress is this machine's address on the local network, needed for
	// port-forwarding instructions.
	LANAddress netip.Addr
	// AddressIsDHCP is true when LANAddress came from a DHCP lease rather than
	// static configuration. A leased address will eventually change and break
	// a static port forward, which is the most common cause of a forward that
	// works for weeks and then stops (SPEC.md §6).
	AddressIsDHCP bool
	// InContainer is true when RASA itself is running inside a container,
	// where loopback discovery and SSDP multicast do not behave normally.
	InContainer bool
}

// Result is everything the pre-flight probe learned.
type Result struct {
	Jellyfin Jellyfin
	Internet Internet
	Router   Router
	Ports    Ports
	Host     Host
}
