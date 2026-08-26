package probe

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
)

// DefaultTimeout bounds the whole pre-flight. Journey step 5 budgets about
// fifteen seconds, and a probe that hangs longer than this reads to the user
// as a frozen app.
const DefaultTimeout = 20 * time.Second

// Prober runs every pre-flight check.
type Prober struct {
	Internet *InternetProber
	Jellyfin *JellyfinProber
	Router   *RouterProber
	// Ports to check locally. Defaults to the two Caddy might listen on.
	Ports   []int
	Timeout time.Duration
	Log     *logging.Logger
}

// New returns a Prober with defaults.
func New(log *logging.Logger) *Prober {
	if log == nil {
		log = logging.Discard()
	}
	return &Prober{
		Internet: NewInternetProber(log),
		Jellyfin: NewJellyfinProber(log),
		Router:   NewRouterProber(log),
		Ports:    []int{PortPreferred, PortFallback},
		Timeout:  DefaultTimeout,
		Log:      log,
	}
}

// Run performs every check and returns what was observed.
//
// The four checks are independent and run concurrently: the router probe waits
// on SSDP replies and the internet probe on remote services, so running them
// in sequence would roughly triple the time the user spends watching a
// progress screen.
//
// Run never returns an error. Every individual failure is a finding — "no
// router answered" and "Jellyfin is not installed" are results the mode router
// and the wizard act on, not conditions that should abort pre-flight.
func (p *Prober) Run(ctx context.Context) Result {
	if p.Timeout == 0 {
		p.Timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()

	ports := p.Ports
	if len(ports) == 0 {
		ports = []int{PortPreferred, PortFallback}
	}

	var (
		wg  sync.WaitGroup
		res Result
	)
	start := time.Now()

	wg.Add(4)
	go func() { defer wg.Done(); res.Internet = p.Internet.Probe(ctx) }()
	go func() { defer wg.Done(); res.Jellyfin = p.Jellyfin.Probe(ctx) }()
	go func() { defer wg.Done(); res.Router = p.Router.Probe(ctx) }()
	go func() { defer wg.Done(); res.Ports = ProbePorts(ctx, p.Log, ports...) }()
	wg.Wait()

	// After the others, not alongside them: this machine's LAN address is only
	// answerable once the router's address is known, because "the address a
	// port forward should point at" means "the address on the router's
	// network" and nothing else can distinguish that from a VPN's.
	res.Host = ProbeHost(ctx, p.Log, res.Router.Gateway)

	p.Log.Info("pre-flight complete",
		slog.Duration("duration", time.Since(start)),
		slog.Bool("jellyfin_found", res.Jellyfin.Found),
		slog.Bool("internet", res.Internet.Reachable),
		slog.Bool("router", res.Router.Reachable),
		slog.Bool("upnp", res.Router.PortMappingAvailable),
	)
	return res
}

// Summary is a short, user-safe description of what pre-flight found, for the
// four lines journey step 5 shows.
type Summary struct {
	Jellyfin string
	Internet string
	Router   string
	Ports    string
}

// Summarize renders the result as plain language.
//
// These strings are shown to the user verbatim, so they follow the same rules
// as the error catalogue: no jargon, no addresses, nothing that reads as
// blame.
func (r Result) Summarize() Summary {
	s := Summary{}

	switch {
	case !r.Jellyfin.Found:
		s.Jellyfin = "Jellyfin not found on this computer"
	case !r.Jellyfin.MeetsMinimum:
		s.Jellyfin = "Jellyfin " + r.Jellyfin.Version + " found — too old"
	default:
		s.Jellyfin = "Jellyfin found — " + r.Jellyfin.Version
	}

	switch {
	case !r.Internet.Reachable:
		s.Internet = "No internet connection"
	case r.Internet.HasV4() && r.Internet.HasV6():
		s.Internet = "Internet connection OK"
	case r.Internet.HasV6():
		s.Internet = "Internet connection OK (newer-style address only)"
	default:
		s.Internet = "Internet connection OK"
	}

	switch {
	case !r.Router.Reachable:
		s.Router = "Router did not respond"
	case r.Router.PortMappingAvailable:
		s.Router = "Router reachable"
	default:
		s.Router = "Router reachable — automatic setup is off"
	}

	switch {
	case r.Ports.IsFree(PortPreferred):
		s.Ports = "Port 443 available"
	case r.Ports.IsFree(PortFallback):
		s.Ports = "Port 443 in use — will use 8443"
	default:
		s.Ports = "Ports 443 and 8443 are both in use"
	}

	return s
}
