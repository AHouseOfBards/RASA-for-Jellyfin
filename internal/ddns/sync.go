// Package ddns keeps the published address pointed at this connection.
//
// This is the third job from SPEC.md §3 that must keep working after RASA is
// removed. It is a command run on a timer, not a daemon: it wakes, compares,
// updates only if needed, writes a heartbeat, and exits.
package ddns

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
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
type Syncer struct {
	Client   *dynu.Client
	Hostname string
	// HeartbeatPath receives a record of every run, successful or not.
	HeartbeatPath string
	Log           *logging.Logger

	internet *probe.InternetProber
}

// New returns a Syncer.
func New(client *dynu.Client, hostname, heartbeatPath string, log *logging.Logger) *Syncer {
	if log == nil {
		log = logging.Discard()
	}
	return &Syncer{
		Client:        client,
		Hostname:      hostname,
		HeartbeatPath: heartbeatPath,
		Log:           log,
		internet:      probe.NewInternetProber(log),
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
	defer func() { s.writeHeartbeat(out) }()

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

// writeHeartbeat records the run.
//
// SPEC.md §15: a scheduled task that quietly fails for six months is the most
// plausible silent failure in this design. Writing on *every* run, successes
// included, is what makes "is this still working?" answerable by opening one
// file with no tooling and no RASA.
func (s *Syncer) writeHeartbeat(o Outcome) {
	if s.HeartbeatPath == "" {
		return
	}
	status := "unchanged"
	switch {
	case o.Err != nil:
		status = "FAILED"
	case o.Updated:
		status = "updated"
	}

	var b []byte
	b = fmt.Appendf(b, "RASA address sync\n")
	b = fmt.Appendf(b, "last run:  %s\n", o.Checked.Format(time.RFC3339))
	b = fmt.Appendf(b, "result:    %s\n", status)
	b = fmt.Appendf(b, "hostname:  %s\n", s.Hostname)
	if o.IPv4.IsValid() {
		b = fmt.Appendf(b, "IPv4:      %s\n", o.IPv4)
	}
	if o.IPv6.IsValid() {
		b = fmt.Appendf(b, "IPv6:      %s\n", o.IPv6)
	}
	if o.Err != nil {
		b = fmt.Appendf(b, "error:     %s\n", o.Err)
		b = fmt.Appendf(b, "\nIf this keeps failing, remote access will stop working when your\n")
		b = fmt.Appendf(b, "internet address next changes. Re-run the RASA setup app to fix it.\n")
	}

	if err := os.MkdirAll(filepath.Dir(s.HeartbeatPath), 0o755); err != nil {
		return
	}
	// Best effort by design: a heartbeat that cannot be written must never
	// turn a successful sync into a failure.
	_ = os.WriteFile(s.HeartbeatPath, b, 0o644)
}
