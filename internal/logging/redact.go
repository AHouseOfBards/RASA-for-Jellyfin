package logging

import (
	"regexp"
	"sort"
	"strings"
	"sync"
)

// minSecretLen guards against registering a value so short that redacting it
// would mangle unrelated output. A two-character "secret" would replace half
// the log file.
const minSecretLen = 8

// Placeholder is what replaces a redacted value.
const Placeholder = "[REDACTED]"

// AddressPlaceholder replaces semi-sensitive values (hostname, public IP) when
// address redaction is on.
const AddressPlaceholder = "[ADDRESS]"

// patterns catch secret-shaped text even when the exact value was never
// registered — defence in depth for the case where a new code path forgets to
// call RegisterSecret. Each pattern must capture the secret in group 1.
var patterns = []*regexp.Regexp{
	// API-Key: abc123  /  "API-Key":"abc123"
	regexp.MustCompile(`(?i)(api[-_]?key"?\s*[:=]\s*"?)([^\s",;}]{6,})`),
	// Authorization: Bearer abc123
	regexp.MustCompile(`(?i)(authorization"?\s*[:=]\s*"?(?:bearer\s+|basic\s+)?)([^\s",;}]{6,})`),
	// "password":"hunter2"  /  password=hunter2
	regexp.MustCompile(`(?i)(pass(?:word|wd)?"?\s*[:=]\s*"?)([^\s",;&}]{4,})`),
	// ?token=abc123 / &access_token=abc123
	regexp.MustCompile(`(?i)([?&](?:access_)?token=)([^\s&"]{6,})`),
}

// Redactor removes secrets from text before it reaches a log file or a
// diagnostic bundle.
//
// SPEC.md §15 treats redaction as a tested feature rather than a convention:
// the diagnostic bundle is the only support channel, so every bundle should be
// assumed to end up pasted into a public GitHub issue.
//
// A Redactor is safe for concurrent use.
type Redactor struct {
	mu sync.RWMutex
	// secrets is kept sorted longest-first so that overlapping values are
	// fully replaced rather than leaving a tail behind.
	secrets   []string
	addresses []string
	redactAdr bool
}

// NewRedactor returns an empty Redactor. Address redaction is on by default:
// the safe direction is to hide the user's hostname and public IP unless they
// explicitly opt in to including them.
func NewRedactor() *Redactor {
	return &Redactor{redactAdr: true}
}

// RegisterSecret marks a value for removal from all future output. Values
// shorter than minSecretLen are ignored, as is the empty string.
//
// Call this the moment a credential enters the process — before the first log
// line that could contain it.
func (r *Redactor) RegisterSecret(v string) {
	if len(v) < minSecretLen {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.secrets {
		if existing == v {
			return
		}
	}
	r.secrets = append(r.secrets, v)
	sort.Slice(r.secrets, func(i, j int) bool {
		return len(r.secrets[i]) > len(r.secrets[j])
	})
}

// RegisterAddress marks a hostname or public IP as semi-sensitive. These are
// needed for debugging but identify the user's home server, so they are hidden
// unless SetRedactAddresses(false) is called.
func (r *Redactor) RegisterAddress(v string) {
	if len(v) < 4 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.addresses {
		if existing == v {
			return
		}
	}
	r.addresses = append(r.addresses, v)
	sort.Slice(r.addresses, func(i, j int) bool {
		return len(r.addresses[i]) > len(r.addresses[j])
	})
}

// SetRedactAddresses controls whether registered addresses are hidden. This is
// the "include my address" toggle on the diagnostic bundle. It never affects
// real secrets, which are always removed.
func (r *Redactor) SetRedactAddresses(on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.redactAdr = on
}

// Redact returns s with every registered secret, every registered address (if
// enabled), and every pattern match replaced.
func (r *Redactor) Redact(s string) string {
	if s == "" {
		return s
	}
	r.mu.RLock()
	secrets := r.secrets
	addresses := r.addresses
	redactAdr := r.redactAdr
	r.mu.RUnlock()

	for _, sec := range secrets {
		s = strings.ReplaceAll(s, sec, Placeholder)
	}

	for _, p := range patterns {
		s = p.ReplaceAllString(s, "${1}"+Placeholder)
	}

	if redactAdr {
		for _, a := range addresses {
			s = strings.ReplaceAll(s, a, AddressPlaceholder)
		}
	}
	return s
}
