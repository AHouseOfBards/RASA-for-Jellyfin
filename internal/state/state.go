// Package state models RASA's setup progress and persists it.
//
// SPEC.md §10 requires a resumable, idempotent state machine rather than a
// linear pipeline: installs get interrupted, networks drop, and step 10 is a
// two-minute unattended stretch that users will force-quit. The state file also
// outlives RASA itself — a later run reads it to offer repair, and the
// uninstaller must not remove it (SPEC.md, decision 15).
//
// Nothing in this package may hold a secret. The Dynu API key belongs to the
// services that outlive RASA (SPEC.md §14), not to this file.
package state

import (
	"fmt"
	"time"
)

// Phase is a point in the setup sequence.
type Phase string

const (
	New                Phase = "NEW"
	Probed             Phase = "PROBED"
	DomainClaimed      Phase = "DOMAIN_CLAIMED"
	PortsMapped        Phase = "PORTS_MAPPED"
	DNSVisible         Phase = "DNS_VISIBLE"
	CertIssued         Phase = "CERT_ISSUED"
	JellyfinConfigured Phase = "JELLYFIN_CONFIGURED"
	Verified           Phase = "VERIFIED"
	Running            Phase = "RUNNING"
	// Degraded is a running state, not a failure: an expired port mapping or
	// a stale DNS record, repairable while still serving.
	Degraded Phase = "DEGRADED"
)

// transitions is the allowed successor set for each phase.
var transitions = map[Phase][]Phase{
	New:                {Probed},
	Probed:             {DomainClaimed},
	DomainClaimed:      {PortsMapped},
	PortsMapped:        {DNSVisible},
	DNSVisible:         {CertIssued},
	CertIssued:         {JellyfinConfigured},
	JellyfinConfigured: {Verified},
	Verified:           {Running},
	Running:            {Degraded},
	Degraded:           {Running, Probed},
}

// order is the sequence a successful setup passes through. Phases outside it
// (Degraded) are not comparable and are treated as "not reached".
var order = []Phase{
	New, Probed, DomainClaimed, PortsMapped, DNSVisible,
	CertIssued, JellyfinConfigured, Verified, Running,
}

func indexOf(p Phase) int {
	for i, q := range order {
		if q == p {
			return i
		}
	}
	return -1
}

// Reached reports whether setup is already at or past p.
//
// This exists because the pipeline is re-runnable and each step records its
// own phase. A run that resumes part-way through re-executes earlier steps
// idempotently and then tries to record a phase it has already passed, which
// Advance correctly rejects as a backwards transition. That produced a
// confusing "illegal transition PORTS_MAPPED -> DOMAIN_CLAIMED" warning on a
// perfectly healthy run: nothing was wrong, the work had simply been done.
func (s *State) Reached(p Phase) bool {
	here, want := indexOf(s.Phase), indexOf(p)
	if here < 0 || want < 0 {
		return false
	}
	return here >= want
}

// Mode is the access strategy chosen by the mode router (SPEC.md §5).
type Mode string

const (
	ModeUnknown Mode = ""
	ModePublic  Mode = "A"  // A record, TLS on 443 or 8443
	ModeIPv6    Mode = "A6" // AAAA only, for CGNAT with native IPv6
	ModeMesh    Mode = "B"  // Tailscale, no open ports
)

// PortMapping records what the router actually granted, which is not
// necessarily what was asked for.
type PortMapping struct {
	ExternalPort int    `json:"external_port"`
	InternalPort int    `json:"internal_port"`
	Method       string `json:"method"` // "upnp", "natpmp", "manual"
	// Permanent is false when the router capped the lease. That is the
	// difference between "survives a reboot" and "does not".
	Permanent bool `json:"permanent"`
	// LeaseSeconds is 0 for a permanent mapping.
	LeaseSeconds int `json:"lease_seconds,omitempty"`
}

// Warning is something that succeeded but will bite later.
//
// These are persisted because "later" is exactly when RASA no longer exists;
// the recovery file is regenerated from them (SPEC.md §15).
type Warning struct {
	Code string    `json:"code"`
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// State is the persisted setup record. Every field here is safe to include in
// a diagnostic bundle after address redaction.
type State struct {
	// Version allows the schema to change without breaking an old install
	// that a later RASA is asked to repair.
	Version   int       `json:"version"`
	Phase     Phase     `json:"phase"`
	RunID     string    `json:"run_id"`
	UpdatedAt time.Time `json:"updated_at"`

	Mode         Mode   `json:"mode,omitempty"`
	Hostname     string `json:"hostname,omitempty"`
	ParentDomain string `json:"parent_domain,omitempty"`
	ListenPort   int    `json:"listen_port,omitempty"`

	JellyfinAddress string `json:"jellyfin_address,omitempty"`
	JellyfinVersion string `json:"jellyfin_version,omitempty"`

	PortMapping *PortMapping `json:"port_mapping,omitempty"`

	CaddyVersion string    `json:"caddy_version,omitempty"`
	CertExpiry   time.Time `json:"cert_expiry,omitempty"`

	Warnings []Warning `json:"warnings,omitempty"`
}

// CurrentVersion is the schema version written by this build.
const CurrentVersion = 1

// NewState returns a fresh state for a run.
func NewState(runID string) *State {
	return &State{
		Version:   CurrentVersion,
		Phase:     New,
		RunID:     runID,
		UpdatedAt: time.Now().UTC(),
	}
}

// CanAdvance reports whether to is a legal successor of the current phase.
// Advancing to the current phase is always legal and is a no-op.
func (s *State) CanAdvance(to Phase) bool {
	if s.Phase == to {
		return true
	}
	for _, p := range transitions[s.Phase] {
		if p == to {
			return true
		}
	}
	return false
}

// Advance moves to the next phase.
//
// Re-advancing to the phase already held succeeds and changes nothing, so a
// resumed run can replay steps without special-casing: claiming a hostname you
// already own, or mapping an already-mapped port, both land here.
func (s *State) Advance(to Phase) error {
	if s.Phase == to {
		return nil
	}
	if !s.CanAdvance(to) {
		return fmt.Errorf("illegal transition %s -> %s", s.Phase, to)
	}
	s.Phase = to
	s.UpdatedAt = time.Now().UTC()
	return nil
}

// Reset moves back to an earlier phase for repair. Unlike Advance this is not
// restricted by the transition table, because repair legitimately rewinds.
func (s *State) Reset(to Phase) {
	s.Phase = to
	s.UpdatedAt = time.Now().UTC()
}

// AddWarning records a warning once. Repeated calls with the same code replace
// the earlier text rather than accumulating duplicates across resumed runs.
func (s *State) AddWarning(code, text string) {
	for i := range s.Warnings {
		if s.Warnings[i].Code == code {
			s.Warnings[i].Text = text
			s.Warnings[i].At = time.Now().UTC()
			return
		}
	}
	s.Warnings = append(s.Warnings, Warning{Code: code, Text: text, At: time.Now().UTC()})
}

// IsComplete reports whether setup reached a serving state.
func (s *State) IsComplete() bool {
	return s.Phase == Verified || s.Phase == Running || s.Phase == Degraded
}

// URL returns the address a user connects to, or "" if not yet known.
func (s *State) URL() string {
	if s.Hostname == "" {
		return ""
	}
	if s.ListenPort == 0 || s.ListenPort == 443 {
		return "https://" + s.Hostname
	}
	return fmt.Sprintf("https://%s:%d", s.Hostname, s.ListenPort)
}
