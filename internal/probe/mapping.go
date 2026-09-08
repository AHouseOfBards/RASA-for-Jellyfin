package probe

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"time"
)

// Finding the router again, months later, from the scheduled task.
//
// The setup app discovers all of this once and moves on. The address syncer
// has to do it every ten minutes for the life of the machine, because a UPnP
// mapping that a router refused to make permanent lapses within a week and
// nothing else will renew it.
//
// It cannot reuse Probe for that. Probe reads the hardware address, which
// shells out to arp, reads the admin page when UPnP says nothing, and asks the
// router for its external address — all of it useful once, all of it waste
// every ten minutes forever.

// MappingTarget is everything needed to create a port mapping, and nothing
// else.
type MappingTarget struct {
	// Gateway is the router's address on the local network.
	Gateway netip.Addr
	// LANAddress is this machine's address on the router's own network, which
	// is what a forward has to point at. It is looked up fresh every time
	// because DHCP moves it, and a mapping pointing at the address this
	// machine had last week sends traffic to whatever holds it now.
	LANAddress netip.Addr
	// ControlURL and ServiceType address the router's port-mapping service.
	ControlURL  string
	ServiceType string
}

// Usable reports whether a mapping can actually be attempted.
func (t MappingTarget) Usable() bool {
	return t.ControlURL != "" && t.ServiceType != "" && t.LANAddress.IsValid()
}

// FindMappingTarget locates the router's port-mapping service.
//
// Everything here is rediscovered rather than remembered. A router that has
// restarted commonly serves its description from a different port, and this
// machine's own address moves whenever its DHCP lease does; a stored control
// URL and a stored client address are both wrong after exactly the events this
// exists to survive.
func (p *RouterProber) FindMappingTarget(ctx context.Context) (MappingTarget, error) {
	var out MappingTarget
	if p.SearchTimeout == 0 {
		p.SearchTimeout = 3 * time.Second
	}
	if p.Timeout == 0 {
		p.Timeout = 5 * time.Second
	}

	if gw, ok := defaultGateway(ctx); ok {
		out.Gateway = gw
	}
	out.LANAddress = addressFacing(out.Gateway)

	var r Router
	r.Gateway = out.Gateway
	p.discoverService(ctx, &r)
	out.ControlURL, out.ServiceType = r.ControlURL, r.ServiceType
	if r.Gateway.IsValid() && r.Gateway != out.Gateway {
		// Discovery is the better answer: the reply came from the router.
		out.Gateway = r.Gateway
		out.LANAddress = addressFacing(out.Gateway)
	}

	if out.ControlURL == "" {
		return out, fmt.Errorf("no router offered port mapping: %s", r.UPnPStatus)
	}
	p.Log.Debug("found the port mapping service",
		slog.String("control_url", out.ControlURL),
		slog.String("service", out.ServiceType),
		slog.String("client", out.LANAddress.String()))
	return out, nil
}
