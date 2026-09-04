// Package ddns keeps the published address pointed at this connection.
//
// This is the third job from SPEC.md §3 that must keep working after RASA is
// removed. It is a command run on a timer, not a daemon: it wakes, compares,
// updates only if needed, reports what it found, and exits.
package ddns

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/dynu"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/probe"
)

// Outcome describes what one run did.
type Outcome struct {
	Checked   time.Time
	IPv4      netip.Addr
	IPv6      netip.Addr
	Updated   bool
	Unchanged bool
	Err       error
}

// Syncer performs one address check.
//
// It reports what it found and writes nothing. Recording the run is the
// caller's job, because the address is only one of the things that has to keep
// working and a file describing just this half would answer "is remote access
// still up?" with half an answer — see internal/health.
type Syncer struct {
	Client   *dynu.Client
	Hostname string
	Log      *logging.Logger

	internet *probe.InternetProber
}

// New returns a Syncer.
func New(client *dynu.Client, hostname string, log *logging.Logger) *Syncer {
	if log == nil {
		log = logging.Discard()
	}
	return &Syncer{
		Client:   client,
		Hostname: hostname,
		Log:      log,
		internet: probe.NewInternetProber(log),
	}
}

// RunOnce checks the current address and updates the record if it moved.
//
// The comparison is against what Dynu currently holds rather than a locally
// cached value. A cache would drift: if a previous update failed, or someone
// changed the record in Dynu's web panel, a cache says "nothing to do" while
// the record points somewhere wrong — and nothing would ever correct it.
func (s *Syncer) RunOnce(ctx context.Context) Outcome {
	out := Outcome{Checked: time.Now().UTC()}

	net := s.internet.Probe(ctx)
	if !net.Reachable {
		out.Err = fmt.Errorf("no public address could be resolved")
		s.Log.Warn("ddns sync could not resolve a public address")
		return out
	}
	out.IPv4, out.IPv6 = net.PublicV4, net.PublicV6

	domain, err := s.Client.FindDomain(ctx, s.Hostname)
	if err != nil {
		out.Err = fmt.Errorf("looking up hostname: %w", err)
		return out
	}
	if domain == nil {
		out.Err = fmt.Errorf("hostname %s is no longer on the account", s.Hostname)
		return out
	}

	if !changed(domain, net) {
		out.Unchanged = true
		s.Log.Debug("ddns address unchanged")
		return out
	}

	if _, err := s.Client.UpdateAddresses(ctx, domain.ID, s.Hostname, net.PublicV4, net.PublicV6); err != nil {
		out.Err = fmt.Errorf("updating address: %w", err)
		s.Log.Error("ddns update failed", slog.Any("err", err))
		return out
	}
	out.Updated = true
	s.Log.Info("ddns address updated",
		slog.Bool("has_v4", net.HasV4()), slog.Bool("has_v6", net.HasV6()))
	return out
}

// changed reports whether the published record differs from what was observed.
func changed(d *dynu.Domain, net probe.Internet) bool {
	if net.HasV4() {
		if !d.IPv4 || d.IPv4Address != net.PublicV4.String() {
			return true
		}
	}
	if net.HasV6() {
		if !d.IPv6 || d.IPv6Address != net.PublicV6.String() {
			return true
		}
	}
	return false
}
