package domains

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
)

// Availability is what DNS could tell us about a candidate hostname.
//
// There is no availability endpoint (SPEC.md §8), so this is inference from a
// lookup and is advisory only. The three-way answer is deliberate: a name that
// does not resolve is *not* known to be free, because a hostname can be
// claimed with no address published, and telling a user "available!" on that
// evidence produces a confident promise that the next screen breaks.
type Availability int

const (
	// Undetermined means the lookup gave no usable signal, or has not run.
	Undetermined Availability = iota
	// InUse means the name resolves, so somebody has it.
	InUse
	// Unclaimed means the name does not resolve. Encouraging, not conclusive.
	Unclaimed
	// Mine means the name resolves to this network's own public address,
	// which is what a re-run over an existing setup looks like.
	Mine
)

func (a Availability) String() string {
	switch a {
	case InUse:
		return "in_use"
	case Unclaimed:
		return "unclaimed"
	case Mine:
		return "mine"
	default:
		return "undetermined"
	}
}

// Message is the advisory line shown under the name field. Note that Unclaimed
// promises nothing.
func (a Availability) Message() string {
	switch a {
	case InUse:
		return "That name is already taken."
	case Unclaimed:
		return "That name looks free."
	case Mine:
		return "That name already points at this network."
	default:
		return ""
	}
}

// Usable reports whether setup may proceed with a name in this state.
//
// Mine is usable: re-running setup with the hostname you already own is the
// repair path, not a collision.
func (a Availability) Usable() bool { return a == Unclaimed || a == Mine || a == Undetermined }

// Resolver is the lookup Check performs. *net.Resolver satisfies it, and so
// does a test double — the classification below is the part worth testing and
// it should not need a network to exercise.
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// Checker performs the debounced lookup behind the name field.
type Checker struct {
	Resolver Resolver
	Timeout  time.Duration
	Log      *logging.Logger

	// OwnAddresses are this network's public addresses. A candidate that
	// resolves to one of them is the user's own, not someone else's.
	OwnAddresses []string
}

// NewChecker returns a Checker with sensible timeouts.
func NewChecker(log *logging.Logger) *Checker {
	return &Checker{Resolver: net.DefaultResolver, Timeout: 3 * time.Second, Log: log}
}

// Check looks the candidate up.
//
// The system resolver is used rather than the authoritative servers that
// dnswait queries, because here a cached answer is fine and speed matters more
// than freshness — this fires while someone is typing.
func (ch *Checker) Check(ctx context.Context, hostname string) Availability {
	hostname = normalize(hostname)
	if hostname == "" {
		return Undetermined
	}

	timeout := ch.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res := ch.Resolver
	if res == nil {
		res = net.DefaultResolver
	}

	addrs, err := res.LookupHost(ctx, hostname)
	switch {
	case err == nil && len(addrs) > 0:
		for _, a := range addrs {
			for _, own := range ch.OwnAddresses {
				if own != "" && strings.EqualFold(a, own) {
					ch.log("hostname resolves to this network", hostname, "mine")
					return Mine
				}
			}
		}
		ch.log("hostname already resolves", hostname, "in_use")
		return InUse
	case isNotFound(err):
		ch.log("hostname does not resolve", hostname, "unclaimed")
		return Unclaimed
	default:
		// A timeout, a broken resolver, or a network that just dropped. None
		// of those say anything about the name, so neither do we.
		ch.log("availability lookup inconclusive", hostname, "undetermined")
		return Undetermined
	}
}

func isNotFound(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

func (ch *Checker) log(msg, hostname, result string) {
	if ch.Log == nil {
		return
	}
	// The hostname is registered for address redaction elsewhere; at this
	// point it is a candidate the user typed, not yet theirs.
	ch.Log.Debug(msg, slog.String("candidate", hostname), slog.String("result", result))
}
