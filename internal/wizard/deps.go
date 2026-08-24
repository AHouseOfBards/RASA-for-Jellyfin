package wizard

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/caddy"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/dnswait"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/domains"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/dynu"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/jellyfin"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/portmap"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/probe"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/reach"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/service"
)

// The interfaces below are the wizard's whole view of the outside world.
//
// They exist because every one of them fronts something that cannot run in a
// test: a router that must be present, a Jellyfin server that must be
// installed, an account that costs a real DNS record to exercise. Narrowing
// each to the handful of calls the flow actually makes is what lets the
// sequencing — the part most likely to be wrong — be tested at all.

// DynuAPI is the subset of the Dynu client the flow uses.
type DynuAPI interface {
	ListDomains(ctx context.Context) ([]dynu.Domain, error)
	FindDomain(ctx context.Context, name string) (*dynu.Domain, error)
	CreateDomain(ctx context.Context, req dynu.CreateDomainRequest) (*dynu.Domain, error)
	UpdateAddresses(ctx context.Context, id int64, name string, v4, v6 netip.Addr) (*dynu.Domain, error)
}

// JellyfinAPI is the subset of the Jellyfin client the flow uses.
type JellyfinAPI interface {
	PublicInfo(ctx context.Context) (*jellyfin.PublicInfo, error)
	NetworkConfig(ctx context.Context) (jellyfin.Config, error)
	AuthenticateByName(ctx context.Context, username, password string) (*jellyfin.AuthResult, error)
	Authenticated() bool
	Apply(ctx context.Context, s jellyfin.Settings) (*jellyfin.Result, error)
	Restart(ctx context.Context) error
	WaitUntilReady(ctx context.Context, timeout time.Duration) error
}

// PortMapper opens a port on the router.
type PortMapper interface {
	Add(ctx context.Context, req portmap.Request) (*portmap.Result, error)
}

// DNSWaiter blocks until an address record is visible on every authoritative
// nameserver.
type DNSWaiter interface {
	WaitForA(ctx context.Context, hostname string, want netip.Addr, onProgress func(dnswait.Progress)) error
}

// Reacher answers whether the outside world can get in.
type Reacher interface {
	CheckURL(ctx context.Context, url, mustContain string) reach.Result
}

// ProxyInstaller writes the proxy configuration and registers it as a service.
type ProxyInstaller interface {
	Install(ctx context.Context, cfg caddy.Config, env map[string]string) error
	Version(ctx context.Context) string
}

// CertWaiter blocks until the proxy is serving a real certificate for the
// hostname, and reports when it expires.
type CertWaiter interface {
	Wait(ctx context.Context, hostname string, port int, onProgress func(elapsed time.Duration)) (time.Time, error)
}

// AvailabilityChecker is the advisory lookup behind the name field.
type AvailabilityChecker interface {
	Check(ctx context.Context, hostname string) domains.Availability
}

// ---------------------------------------------------------------------------
// Real implementations.

type realCertWaiter struct {
	Log      *logging.Logger
	Interval time.Duration
	Timeout  time.Duration
}

// Wait dials the proxy over loopback with the public hostname as SNI and
// inspects what it presents.
//
// Loopback rather than the public address on purpose: this asks "has the
// certificate been issued", which is a question about the proxy, and routing it
// through the internet would conflate it with the separate question of whether
// the port is open. Those fail for different reasons and deserve separate
// lines on the screen.
//
// The interesting case is not a failed handshake but a successful one against
// Caddy's internal authority: when issuance fails, Caddy still answers, with a
// certificate no browser will accept. A check that only looked for "TLS works"
// would call that success.
func (w realCertWaiter) Wait(ctx context.Context, hostname string, port int, onProgress func(time.Duration)) (time.Time, error) {
	interval := w.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	timeout := w.Timeout
	if timeout <= 0 {
		// DNS-01 issuance normally lands well inside two minutes; the extra
		// room covers a slow propagation check inside Caddy itself.
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := time.Now()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	var last error
	for {
		expiry, err := peekCertificate(ctx, addr, hostname)
		if err == nil {
			return expiry, nil
		}
		last = err
		if onProgress != nil {
			onProgress(time.Since(started))
		}
		select {
		case <-ctx.Done():
			return time.Time{}, fmt.Errorf("no certificate after %s: %w", time.Since(started).Round(time.Second), last)
		case <-time.After(interval):
		}
	}
}

// peekCertificate completes a handshake without verifying, then applies its own
// judgement about what was presented.
func peekCertificate(ctx context.Context, addr, serverName string) (time.Time, error) {
	d := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 5 * time.Second},
		Config: &tls.Config{
			ServerName: serverName,
			// The chain is inspected below rather than verified here: an
			// internally-signed certificate must be recognised and rejected,
			// and verification would only say "untrusted" without saying why.
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS12,
		},
	}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return time.Time{}, err
	}
	defer conn.Close()

	tc, ok := conn.(*tls.Conn)
	if !ok {
		return time.Time{}, fmt.Errorf("unexpected connection type")
	}
	certs := tc.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return time.Time{}, fmt.Errorf("no certificate presented")
	}
	leaf := certs[0]
	if isInternalCA(leaf) {
		return time.Time{}, fmt.Errorf("proxy is still serving its internal certificate")
	}
	if err := leaf.VerifyHostname(serverName); err != nil {
		return time.Time{}, fmt.Errorf("certificate is not for this address: %w", err)
	}
	return leaf.NotAfter, nil
}

// isInternalCA recognises Caddy's local authority, which it serves while
// issuance has not succeeded.
func isInternalCA(c *x509.Certificate) bool {
	iss := c.Issuer.CommonName
	for _, marker := range []string{"Caddy Local Authority", "localhost"} {
		if iss == marker || (len(iss) >= len(marker) && iss[:len(marker)] == marker) {
			return true
		}
	}
	return false
}

// defaults fills in the real implementations for anything the caller left
// unset, so production wiring is the zero-configuration path and tests replace
// only what they need.
func (o *Options) defaults() {
	if o.Probe == nil {
		log := o.Log
		o.Probe = func(ctx context.Context) probe.Result {
			return probe.New(log.WithPhase("probe")).Run(ctx)
		}
	}
	if o.NewDynu == nil {
		log := o.Log
		o.NewDynu = func(apiKey string) DynuAPI {
			return dynu.New(apiKey, dynu.WithLogger(log.WithPhase("dynu")))
		}
	}
	if o.NewJellyfin == nil {
		log := o.Log
		o.NewJellyfin = func(baseURL, apiKey string) JellyfinAPI {
			opts := []jellyfin.Option{jellyfin.WithLogger(log.WithPhase("jellyfin"))}
			if apiKey != "" {
				opts = append(opts, jellyfin.WithAPIKey(apiKey))
			}
			return jellyfin.New(baseURL, opts...)
		}
	}
	if o.NewMapper == nil {
		log := o.Log
		o.NewMapper = func(controlURL, serviceType string) PortMapper {
			return portmap.New(controlURL, serviceType, log.WithPhase("portmap"))
		}
	}
	if o.DNSWait == nil {
		o.DNSWait = dnswait.New(o.Log.WithPhase("dns"))
	}
	if o.NewReach == nil {
		log := o.Log
		o.NewReach = func(addr netip.Addr) Reacher {
			return reach.New(addr, log.WithPhase("verify"))
		}
	}
	if o.CertWait == nil {
		o.CertWait = realCertWaiter{Log: o.Log}
	}
	if o.Availability == nil {
		o.Availability = domains.NewChecker(o.Log)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.NewServices == nil {
		log := o.Log
		o.NewServices = func() (service.Manager, error) { return service.New(log.WithPhase("service")) }
	}
}
