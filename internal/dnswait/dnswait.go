// Package dnswait blocks until a DNS record is actually visible.
//
// SPEC.md §10 lists this as the step the original draft was missing: it
// created a hostname and invoked the ACME client immediately. A freshly
// created record is not instantly resolvable, and Let's Encrypt caps failed
// validations at five per hostname per hour — so racing it does not merely
// fail, it burns the budget needed to retry.
//
// Queries go to the zone's authoritative nameservers rather than the system
// resolver. The local resolver caches, and it caches negative answers too: a
// lookup made a second after the record was created can pin a NXDOMAIN in
// cache for minutes, so the system resolver would report "not there yet" long
// after it is.
package dnswait

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
)

// Defaults for polling. The interval is deliberately unhurried: propagation
// takes one to two minutes and hammering nameservers gains nothing.
const (
	DefaultInterval = 5 * time.Second
	DefaultTimeout  = 3 * time.Minute
)

// Waiter polls for a record.
type Waiter struct {
	Interval time.Duration
	Timeout  time.Duration
	Log      *logging.Logger

	// Servers overrides authoritative discovery. For tests.
	Servers []string
	// SystemResolver is used to discover nameservers; overridable for tests.
	SystemResolver *net.Resolver
}

// New returns a Waiter with defaults.
func New(log *logging.Logger) *Waiter {
	if log == nil {
		log = logging.Discard()
	}
	return &Waiter{Interval: DefaultInterval, Timeout: DefaultTimeout, Log: log}
}

// ErrTimeout is returned when the record never appeared.
var ErrTimeout = errors.New("record did not become visible in time")

// Progress reports one polling round, so the wizard can show that something is
// happening during a wait long enough to look like a hang.
type Progress struct {
	Attempt   int
	Elapsed   time.Duration
	ServersOK int
	Servers   int
}

// WaitForA blocks until every authoritative nameserver returns want for
// hostname.
//
// Every server must agree. A record visible on one nameserver but not another
// is exactly the state that makes ACME validation flaky, because Let's
// Encrypt does not promise which one it will ask.
func (w *Waiter) WaitForA(ctx context.Context, hostname string, want netip.Addr, onProgress func(Progress)) error {
	check := func(r *net.Resolver) (bool, error) {
		addrs, err := r.LookupNetIP(ctx, ipNetwork(want), hostname)
		if err != nil {
			return false, err
		}
		for _, a := range addrs {
			if a.Unmap() == want.Unmap() {
				return true, nil
			}
		}
		return false, nil
	}
	return w.wait(ctx, hostname, "A/AAAA", check, onProgress)
}

// WaitForTXT blocks until every authoritative nameserver returns a TXT record
// containing value.
//
// Used before an ACME DNS-01 challenge is handed to the CA.
func (w *Waiter) WaitForTXT(ctx context.Context, name, value string, onProgress func(Progress)) error {
	check := func(r *net.Resolver) (bool, error) {
		vals, err := r.LookupTXT(ctx, name)
		if err != nil {
			return false, err
		}
		for _, v := range vals {
			if strings.TrimSpace(v) == value {
				return true, nil
			}
		}
		return false, nil
	}
	return w.wait(ctx, name, "TXT", check, onProgress)
}

func (w *Waiter) wait(ctx context.Context, name, kind string,
	check func(*net.Resolver) (bool, error), onProgress func(Progress)) error {

	if w.Interval == 0 {
		w.Interval = DefaultInterval
	}
	if w.Timeout == 0 {
		w.Timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, w.Timeout)
	defer cancel()

	servers, err := w.Nameservers(ctx, name)
	if err != nil || len(servers) == 0 {
		// Without authoritative servers there is nothing trustworthy to poll.
		// Falling back to the system resolver would reintroduce the negative
		// caching this package exists to avoid, so this is a real failure.
		return fmt.Errorf("could not find nameservers for %s: %w", name, err)
	}
	w.Log.Debug("polling authoritative nameservers",
		slog.String("name", name), slog.String("type", kind), slog.Int("servers", len(servers)))

	resolvers := make([]*net.Resolver, 0, len(servers))
	for _, s := range servers {
		resolvers = append(resolvers, resolverFor(s))
	}

	start := time.Now()
	for attempt := 1; ; attempt++ {
		ok := 0
		for _, r := range resolvers {
			if found, err := check(r); err == nil && found {
				ok++
			}
		}
		if onProgress != nil {
			onProgress(Progress{Attempt: attempt, Elapsed: time.Since(start),
				ServersOK: ok, Servers: len(resolvers)})
		}
		if ok == len(resolvers) {
			w.Log.Info("record is visible on every authoritative nameserver",
				slog.String("name", name),
				slog.String("type", kind),
				slog.Duration("took", time.Since(start)),
				slog.Int("attempts", attempt))
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %s visible on %d of %d nameservers after %s",
				ErrTimeout, kind, ok, len(resolvers), time.Since(start).Round(time.Second))
		case <-time.After(w.Interval):
		}
	}
}

// Nameservers returns the addresses of the zone's authoritative nameservers,
// as host:port ready to dial.
//
// The zone is found by walking up from the hostname: for a Dynu DDNS hostname
// the NS records sit at the hostname itself, whereas for a normal subdomain
// they sit on a parent. Walking handles both without needing to know which.
//
// Exported because the ACME client needs the same list for the same reason.
// Its DNS-01 propagation check asks whether the challenge record is visible
// yet, and asking a caching resolver that question is how you get told "no"
// for half an hour after the answer became yes — see the package comment.
func (w *Waiter) Nameservers(ctx context.Context, name string) ([]string, error) {
	if len(w.Servers) > 0 {
		return w.Servers, nil
	}
	sys := w.SystemResolver
	if sys == nil {
		sys = net.DefaultResolver
	}

	labels := strings.Split(strings.TrimSuffix(strings.TrimSpace(name), "."), ".")
	var lastErr error
	for i := 0; i < len(labels)-1; i++ {
		zone := strings.Join(labels[i:], ".")
		nsRecords, err := sys.LookupNS(ctx, zone)
		if err != nil {
			lastErr = err
			continue
		}
		var out []string
		for _, ns := range nsRecords {
			host := strings.TrimSuffix(ns.Host, ".")
			ips, err := sys.LookupHost(ctx, host)
			if err != nil {
				continue
			}
			for _, ip := range ips {
				// Nameserver addresses are dialled directly, so they need a
				// port. 53 is the only one that applies.
				out = append(out, net.JoinHostPort(ip, "53"))
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no NS records found")
	}
	return nil, lastErr
}

// resolverFor returns a resolver that talks to one specific nameserver.
//
// PreferGo is required: without it the platform resolver is used and the
// custom dialer is ignored on some systems, silently sending the query to the
// system resolver and reintroducing caching.
func resolverFor(server string) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, network, server)
		},
	}
}

func ipNetwork(a netip.Addr) string {
	if a.Is4() {
		return "ip4"
	}
	return "ip6"
}
