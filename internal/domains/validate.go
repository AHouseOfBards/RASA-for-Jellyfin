package domains

import (
	"strings"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/rasaerr"
)

// ValidateLabel checks the name portion of a hostname.
//
// This runs on every keystroke in the picker, so it must be pure, fast and
// silent about anything the user has not finished typing yet. It returns a
// *rasaerr.Error whose User() projection is what the field renders.
//
// The rules are DNS's, not Dynu's. Dynu applies its own on creation and RASA
// treats that response as authoritative (SPEC.md §8) — this is the cheap check
// that keeps obvious mistakes from costing a round trip.
func ValidateLabel(label string) error {
	label = strings.TrimSpace(label)

	switch {
	case label == "":
		return rasaerr.InvalidHostname(label, rasaerr.HostnameEmpty, 0)
	case len(label) > MaxLabelLength:
		return rasaerr.InvalidHostname(label, rasaerr.HostnameTooLong, MaxLabelLength)
	}

	// Character check before length. Someone typing "my media" should be told
	// about the space rather than about length, even when the name is also
	// short — the space is the thing they can act on.
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
		default:
			return rasaerr.InvalidHostname(label, rasaerr.HostnameBadCharacters, 0)
		}
	}
	if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
		return rasaerr.InvalidHostname(label, rasaerr.HostnameEdgeHyphen, 0)
	}
	// "xn--" is the punycode marker. A label starting with it is interpreted
	// as encoded Unicode, and one that is not actually punycode resolves
	// nowhere in a way nothing explains.
	if strings.HasPrefix(strings.ToLower(label), "xn--") {
		return rasaerr.InvalidHostname(label, rasaerr.HostnameReservedPrefix, 0)
	}
	if len(label) < MinLabelLength {
		return rasaerr.InvalidHostname(label, rasaerr.HostnameTooShort, MinLabelLength)
	}
	return nil
}

// ValidateHostname checks a full hostname against the catalogue: the label
// must be usable and the parent domain must be one RASA offers.
//
// Advanced mode (SPEC.md §16) may let a user bring a domain of their own,
// which is why the parent check lives here rather than inside ValidateLabel —
// the label rules always apply, the catalogue rule does not.
func (c *Catalog) ValidateHostname(hostname string) error {
	label, _, ok := c.Split(hostname)
	if !ok {
		// Either the parent is one RASA refuses, or it is simply not on the
		// list. Those are different messages: the first has a reason worth
		// giving, the second is just not on offer.
		name := normalize(hostname)
		if i := strings.IndexByte(name, '.'); i >= 0 {
			suffix := name[i+1:]
			for suffix != "" {
				if _, blocked := c.BlockedReason(suffix); blocked {
					return rasaerr.BlockedParentDomain(suffix)
				}
				j := strings.IndexByte(suffix, '.')
				if j < 0 {
					break
				}
				suffix = suffix[j+1:]
			}
			return rasaerr.BlockedParentDomain(name[i+1:])
		}
		return rasaerr.InvalidHostname(name, rasaerr.HostnameBadCharacters, 0)
	}
	return ValidateLabel(label)
}
