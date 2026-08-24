package probe

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
)

// DefaultV4Endpoints and DefaultV6Endpoints are queried in order until one
// answers. Several are used because any single service can be down, rate
// limited, or blocked by a DNS filter — and a failure here would otherwise
// look to the user like "you have no internet".
var (
	DefaultV4Endpoints = []string{
		"https://api.ipify.org",
		"https://ipv4.icanhazip.com",
		"https://ifconfig.me/ip",
	}
	DefaultV6Endpoints = []string{
		"https://api6.ipify.org",
		"https://ipv6.icanhazip.com",
	}
)

// InternetProber resolves the addresses the outside world sees.
type InternetProber struct {
	V4Endpoints []string
	V6Endpoints []string
	Timeout     time.Duration
	Log         *logging.Logger

	client4, client6 *http.Client
}

// NewInternetProber returns a prober with the default endpoints.
func NewInternetProber(log *logging.Logger) *InternetProber {
	if log == nil {
		log = logging.Discard()
	}
	return &InternetProber{
		V4Endpoints: DefaultV4Endpoints,
		V6Endpoints: DefaultV6Endpoints,
		Timeout:     8 * time.Second,
		Log:         log,
	}
}

// forcedClient returns a client pinned to one address family.
//
// Pinning matters: on a dual-stack host an unpinned request may answer over
// either family, so "what is my IPv4 address" could silently come back with an
// IPv6 one. The CGNAT comparison comes apart if that happens.
func forcedClient(network string, timeout time.Duration) *http.Client {
	d := &net.Dialer{Timeout: timeout}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				return d.DialContext(ctx, network, addr)
			},
			DisableKeepAlives: true,
		},
	}
}

// Probe resolves the public IPv4 and IPv6 addresses concurrently.
//
// The two families are independent: a host may legitimately have one and not
// the other, and an IPv6-only connection must not be reported as "no
// internet".
func (p *InternetProber) Probe(ctx context.Context) Internet {
	if p.Timeout == 0 {
		p.Timeout = 8 * time.Second
	}
	if p.client4 == nil {
		p.client4 = forcedClient("tcp4", p.Timeout)
	}
	if p.client6 == nil {
		p.client6 = forcedClient("tcp6", p.Timeout)
	}

	var (
		wg     sync.WaitGroup
		v4, v6 netip.Addr
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		v4 = p.first(ctx, p.client4, p.V4Endpoints, "ipv4")
	}()
	go func() {
		defer wg.Done()
		v6 = p.first(ctx, p.client6, p.V6Endpoints, "ipv6")
	}()
	wg.Wait()

	out := Internet{Reachable: v4.IsValid() || v6.IsValid()}
	// Guard against an endpoint answering with the wrong family — some
	// services return an IPv4-mapped address over an IPv6 connection.
	if v4.Is4() {
		out.PublicV4 = v4
	}
	if v6.Is6() && !v6.Is4In6() {
		out.PublicV6 = v6
	}

	p.Log.Debug("internet probe complete",
		slog.Bool("reachable", out.Reachable),
		slog.Bool("has_v4", out.HasV4()),
		slog.Bool("has_v6", out.HasV6()),
	)
	return out
}

// first queries endpoints in order and returns the first valid address.
func (p *InternetProber) first(ctx context.Context, c *http.Client, endpoints []string, family string) netip.Addr {
	for _, ep := range endpoints {
		start := time.Now()
		addr, err := fetchAddr(ctx, c, ep)
		p.Log.Debug("public address lookup",
			slog.String("family", family),
			slog.String("endpoint", ep),
			slog.Bool("ok", err == nil),
			slog.Duration("duration", time.Since(start)),
		)
		if err == nil {
			return addr
		}
		if ctx.Err() != nil {
			return netip.Addr{}
		}
	}
	return netip.Addr{}
}

func fetchAddr(ctx context.Context, c *http.Client, endpoint string) (netip.Addr, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return netip.Addr{}, err
	}
	req.Header.Set("Accept", "text/plain")

	resp, err := c.Do(req)
	if err != nil {
		return netip.Addr{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return netip.Addr{}, errors.New("unexpected status")
	}

	// Cap the read: a misconfigured endpoint returning a web page must not be
	// pulled into memory.
	b, err := io.ReadAll(io.LimitReader(resp.Body, 128))
	if err != nil {
		return netip.Addr{}, err
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(string(b)))
	if err != nil {
		return netip.Addr{}, err
	}
	if !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return netip.Addr{}, errors.New("endpoint returned a non-public address")
	}
	return addr.Unmap(), nil
}
