// Package portkeep keeps an automatic port opening alive.
//
// A router that refuses a permanent mapping gets a lease instead, and the
// longest lease UPnP can give is seven days — 604800 seconds is the ceiling
// miniupnpd enforces, and miniupnpd is the UPnP daemon in most consumer
// routers. Nothing renewed it. Remote access set up entirely successfully
// therefore stopped working a week later, and the health check could not see
// it: it watches the address and the certificate, neither of which changes
// when a router quietly drops a mapping.
//
// So the address syncer renews it, every ten minutes, for the life of the
// machine. That is the only component still installed once the setup app is
// gone.
//
// # Everything is rediscovered
//
// Nothing here trusts a remembered value. The events this exists to survive
// are exactly the ones that invalidate what was remembered: a router restart
// commonly moves the port its description is served from, and a DHCP lease
// renewal moves this machine's own address. A mapping renewed against a stale
// control URL fails, and one pointed at a stale client address sends the
// user's traffic to whatever holds that address now, which is worse than
// failing.
//
// # Nothing here may break the syncer
//
// Renewal is the third job of a program whose first two are keeping the
// address current and reporting health. A router that has been replaced, taken
// off the network, or had UPnP switched off must not stop those happening, so
// every failure in this package is reported and none is returned as fatal.
package portkeep

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/portmap"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/probe"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/state"
)

// Mapper is the port-mapping half of portmap.Mapper, as a seam for tests.
type Mapper interface {
	Add(ctx context.Context, req portmap.Request) (*portmap.Result, error)
	Get(ctx context.Context, externalPort int, protocol string) (portmap.Mapping, bool, error)
}

// Finder locates the router's mapping service. A seam for the same reason.
type Finder func(ctx context.Context) (probe.MappingTarget, error)

// Keeper renews the mapping recorded at setup.
type Keeper struct {
	Log *logging.Logger
	// Find locates the router. Defaults to a real SSDP search.
	Find Finder
	// NewMapper builds a mapper for a discovered service. Defaults to the real
	// one.
	NewMapper func(controlURL, serviceType string, log *logging.Logger) Mapper
	// Timeout bounds the whole attempt, so a router that accepts a connection
	// and then says nothing cannot hold up the address sync behind it.
	Timeout time.Duration
}

// DefaultTimeout bounds one renewal. Generous next to a ten-minute interval,
// and short next to the twenty seconds a scheduled task may reasonably take.
const DefaultTimeout = 20 * time.Second

// Result is what happened, in a shape the health file can render.
type Result struct {
	// Applicable is false when there is no automatic mapping to keep alive:
	// a manual forward, a mesh-mode setup, or a setup that never opened a
	// port. Everything else on Result is meaningless when this is false.
	Applicable bool
	// OK is true when the mapping is in place at the end of the run, whether
	// or not this run is what put it there.
	OK bool
	// Renewed is true when a mapping was actually created or refreshed, as
	// opposed to already being present and permanent.
	Renewed bool
	// ExternalPort and LeaseSeconds describe the mapping as it now stands.
	ExternalPort int
	LeaseSeconds int
	// Client is the address the mapping points at.
	Client string
	// Detail is one line for the health file and the log.
	Detail string
	// Err is why it failed, for the log only.
	Err error
	// LastGood is when the mapping was last confirmed, from before this run.
	// Zero when it never has been.
	//
	// It is what separates a blip from a fault. SSDP is multicast UDP over a
	// home network and a single round can simply be lost; a mapping renewed
	// twenty minutes ago has days of lease left, so one miss is not worth
	// telling anyone about. Without this, one lost datagram turns the health
	// file red and fires an alert.
	LastGood time.Time
}

// GraceWindow is how long renewal may keep failing before it is reported as a
// fault rather than a blip.
//
// Six hours is thirty-six attempts at the ten-minute interval, and still leaves
// most of a seven-day lease in hand. Anything that survives that is not a lost
// datagram.
const GraceWindow = 6 * time.Hour

// Settled reports whether a failure has gone on long enough to be worth
// telling the user about.
func (r Result) Settled(now time.Time) bool {
	if r.OK || !r.Applicable {
		return true
	}
	if r.LastGood.IsZero() {
		return true
	}
	return now.Sub(r.LastGood) > GraceWindow
}

// Run renews the mapping if there is one to renew.
//
// st is updated in place when the mapping changes, so the caller can persist
// it. Nothing is written here: this package does not own the state file, and a
// renewal that cannot be saved is still a renewal that happened.
func (k *Keeper) Run(ctx context.Context, st *state.State) Result {
	if k.Log == nil {
		k.Log = logging.Discard()
	}
	log := k.Log.WithPhase("portmap")

	m := st.PortMapping
	lastGood := time.Time{}
	if m != nil {
		lastGood = m.RenewedAt
	}
	switch {
	case m == nil:
		return Result{Detail: "No port was opened during setup."}
	case m.Method != "upnp":
		// A forward the user typed into their own router is theirs. RASA did
		// not create it, cannot see it, and has no business renewing it.
		return Result{Detail: "The port was forwarded by hand, so there is nothing to renew."}
	case m.ExternalPort == 0:
		return Result{Detail: "No port was recorded."}
	}

	timeout := k.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	find := k.Find
	if find == nil {
		find = func(ctx context.Context) (probe.MappingTarget, error) {
			return probe.NewRouterProber(log).FindMappingTarget(ctx)
		}
	}
	target, err := find(ctx)
	if err != nil {
		return Result{
			Applicable: true, Err: err, ExternalPort: m.ExternalPort, LastGood: lastGood,
			Detail: "Your router is not offering to open ports at the moment.",
		}
	}
	if !target.Usable() {
		return Result{
			Applicable: true, ExternalPort: m.ExternalPort, LastGood: lastGood,
			Err:    errors.New("no usable mapping service"),
			Detail: "Your router is not offering to open ports at the moment.",
		}
	}

	newMapper := k.NewMapper
	if newMapper == nil {
		newMapper = func(controlURL, serviceType string, log *logging.Logger) Mapper {
			return portmap.New(controlURL, serviceType, log)
		}
	}
	mapper := newMapper(target.ControlURL, target.ServiceType, log)

	return k.renew(ctx, log, mapper, target, m)
}

func (k *Keeper) renew(ctx context.Context, log *logging.Logger, mapper Mapper,
	target probe.MappingTarget, m *state.PortMapping) Result {

	res := Result{
		Applicable:   true,
		ExternalPort: m.ExternalPort,
		Client:       target.LANAddress.String(),
		LastGood:     m.RenewedAt,
	}

	// What is there already, before touching anything. A permanent mapping
	// pointing at this machine needs nothing done to it, and re-adding one
	// every ten minutes would rewrite a router's stored configuration for no
	// reason.
	existing, found, getErr := mapper.Get(ctx, m.ExternalPort, portmap.TCP)
	if getErr != nil {
		log.Debug("could not read the existing mapping", slog.Any("err", getErr))
	}
	if found && existing.Permanent() && sameClient(existing.InternalClient, target.LANAddress) {
		res.OK = true
		res.LeaseSeconds = 0
		res.Detail = fmt.Sprintf("Port %d is open permanently.", m.ExternalPort)
		syncState(m, existing, target)
		return res
	}

	// Someone else's mapping. Overwriting it would take a port another device
	// on the network is using, which is not RASA's to take.
	if found && !sameClient(existing.InternalClient, target.LANAddress) {
		res.Err = fmt.Errorf("port %d is mapped to %s", m.ExternalPort, existing.InternalClient)
		res.Detail = fmt.Sprintf(
			"Port %d on your router now points at %s instead of this computer.",
			m.ExternalPort, existing.InternalClient)
		return res
	}

	out, err := mapper.Add(ctx, portmap.Request{
		ExternalPort:   m.ExternalPort,
		InternalPort:   m.InternalPort,
		InternalClient: target.LANAddress,
		Protocol:       portmap.TCP,
	})
	if err != nil {
		var ue *portmap.UPnPError
		if errors.As(err, &ue) && ue.IsConflict() {
			res.Err = err
			res.Detail = fmt.Sprintf(
				"Your router is already sending port %d to a different device.", m.ExternalPort)
			return res
		}
		res.Err = err
		res.Detail = fmt.Sprintf("Your router refused to reopen port %d.", m.ExternalPort)
		return res
	}

	res.OK = true
	res.Renewed = true
	res.LeaseSeconds = out.Mapping.LeaseSeconds
	if out.Mapping.Permanent() {
		res.Detail = fmt.Sprintf("Port %d is open permanently.", m.ExternalPort)
	} else {
		res.Detail = fmt.Sprintf("Port %d is open, and renewed automatically every ten minutes.",
			m.ExternalPort)
	}
	syncState(m, out.Mapping, target)

	// Info only when something changed. This runs every ten minutes forever,
	// and a line per run is fifty thousand a year in a file nobody is reading
	// until the day they need to.
	if !found {
		log.Info("reopened the port on the router",
			slog.Int("port", m.ExternalPort),
			slog.String("client", target.LANAddress.String()),
			slog.Bool("permanent", out.Mapping.Permanent()))
	} else {
		log.Debug("renewed the port mapping",
			slog.Int("port", m.ExternalPort),
			slog.Int("lease", out.Mapping.LeaseSeconds))
	}
	return res
}

// syncState brings the recorded mapping into line with what the router now
// says, so the recovery file and the setup app agree with reality.
func syncState(m *state.PortMapping, got portmap.Mapping, target probe.MappingTarget) {
	if got.ExternalPort != 0 {
		m.ExternalPort = got.ExternalPort
	}
	if got.InternalPort != 0 {
		m.InternalPort = got.InternalPort
	}
	m.LeaseSeconds = got.LeaseSeconds
	m.Permanent = got.Permanent()
	m.RenewedAt = time.Now().UTC()
	if target.LANAddress.IsValid() {
		m.InternalClient = target.LANAddress.String()
	}
}

// sameClient compares a router-reported client address with ours.
//
// Routers report this back in whatever form they please, and a string compare
// against a netip.Addr trips over "192.168.001.050" and similar. An address
// that cannot be parsed is treated as different, which errs towards leaving a
// mapping alone rather than towards overwriting one that might not be ours.
func sameClient(reported string, ours netip.Addr) bool {
	if reported == "" || !ours.IsValid() {
		return false
	}
	got, err := netip.ParseAddr(reported)
	if err != nil {
		return false
	}
	return got.Unmap() == ours.Unmap()
}
