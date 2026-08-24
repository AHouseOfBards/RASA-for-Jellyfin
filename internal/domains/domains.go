// Package domains holds the parent domains RASA will offer, and the rules for
// building a hostname under one.
//
// SPEC.md §8: there is no Dynu endpoint that enumerates what is on offer and
// none that checks availability, so the list ships with the app. Membership is
// not a matter of taste — a parent domain qualifies only if it appears on the
// Public Suffix List, because Let's Encrypt's 50-certificates-per-week limit is
// counted per registered domain. On a parent that the PSL does not list, every
// RASA user in the world would share one bucket and hit a wall none of them
// could diagnose. Three of Dynu's twelve free domains fail that test.
//
// The catalogue is data so that the CI audit described in SPEC.md §8 can check
// it against the live PSL without touching code.
package domains

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

//go:embed domains.json
var embeddedJSON []byte

// MaxLabelLength is the DNS limit on a single label. Dynu is stricter in
// practice, but this is the hard ceiling.
const MaxLabelLength = 63

// MinLabelLength is RASA's own floor. One- and two-character names are all
// long gone on these domains, and letting someone spend three attempts
// discovering that is worse than declining up front.
const MinLabelLength = 3

// Domain is a parent domain a hostname can be created under.
type Domain struct {
	Name string `json:"name"`
	// Default marks the one pre-selected in the picker. Exactly one entry
	// carries it.
	Default bool `json:"default,omitempty"`
	// Rank orders the dropdown. Lower comes first.
	Rank int `json:"rank"`
}

// Blocked is a domain RASA deliberately refuses, and why.
//
// Recording the rejects alongside the accepts is what lets the picker explain
// itself when a user in advanced mode types one of them by hand.
type Blocked struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Catalog is the loaded domain set.
type Catalog struct {
	Version int    `json:"version"`
	Updated string `json:"updated"`
	Note    string `json:"note,omitempty"`

	domains []Domain
	blocked map[string]string
}

type catalogFile struct {
	Version int       `json:"version"`
	Updated string    `json:"updated"`
	Note    string    `json:"note"`
	Domains []Domain  `json:"domains"`
	Blocked []Blocked `json:"blocked"`
}

// Embedded returns the catalogue compiled into the binary.
//
// SPEC.md §8 wants a runtime refresh with this as the fallback. Load parses a
// fetched copy; a caller that cannot fetch one is never worse off than this.
func Embedded() (*Catalog, error) { return Load(embeddedJSON) }

// Load parses a catalogue.
func Load(data []byte) (*Catalog, error) {
	var f catalogFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parsing domain catalogue: %w", err)
	}
	if len(f.Domains) == 0 {
		return nil, fmt.Errorf("domain catalogue is empty")
	}

	c := &Catalog{
		Version: f.Version,
		Updated: f.Updated,
		Note:    f.Note,
		blocked: make(map[string]string, len(f.Blocked)),
	}
	seen := make(map[string]bool, len(f.Domains))
	defaults := 0
	for _, d := range f.Domains {
		d.Name = normalize(d.Name)
		if d.Name == "" {
			return nil, fmt.Errorf("domain catalogue has an entry with no name")
		}
		if seen[d.Name] {
			return nil, fmt.Errorf("domain catalogue lists %s twice", d.Name)
		}
		seen[d.Name] = true
		if d.Default {
			defaults++
		}
		c.domains = append(c.domains, d)
	}
	if defaults != 1 {
		// A picker with no default focuses nothing and a picker with two is
		// ambiguous; both are bugs in the data file, caught at load rather
		// than as odd behaviour on screen.
		return nil, fmt.Errorf("domain catalogue must mark exactly one default, found %d", defaults)
	}
	for _, b := range f.Blocked {
		name := normalize(b.Name)
		if seen[name] {
			return nil, fmt.Errorf("domain catalogue both offers and blocks %s", name)
		}
		c.blocked[name] = b.Reason
	}

	sort.SliceStable(c.domains, func(i, j int) bool { return c.domains[i].Rank < c.domains[j].Rank })
	return c, nil
}

// All returns the offered domains in display order.
func (c *Catalog) All() []Domain {
	out := make([]Domain, len(c.domains))
	copy(out, c.domains)
	return out
}

// Names returns the offered domain names in display order.
func (c *Catalog) Names() []string {
	out := make([]string, len(c.domains))
	for i, d := range c.domains {
		out[i] = d.Name
	}
	return out
}

// Default returns the pre-selected domain.
func (c *Catalog) Default() Domain {
	for _, d := range c.domains {
		if d.Default {
			return d
		}
	}
	// Load guarantees one exists; this keeps the signature total.
	return c.domains[0]
}

// Allows reports whether a parent domain may be used.
func (c *Catalog) Allows(name string) bool {
	name = normalize(name)
	for _, d := range c.domains {
		if d.Name == name {
			return true
		}
	}
	return false
}

// BlockedReason returns why a domain is refused, if it is one RASA knows to
// refuse. An unknown domain is not "blocked" — it is simply not offered, and
// advanced mode may still permit it (SPEC.md §16).
func (c *Catalog) BlockedReason(name string) (string, bool) {
	r, ok := c.blocked[normalize(name)]
	return r, ok
}

// Others returns every offered domain except the one named, in display order.
// These are the cross-domain suggestions a collision falls back to.
func (c *Catalog) Others(name string) []Domain {
	name = normalize(name)
	var out []Domain
	for _, d := range c.domains {
		if d.Name != name {
			out = append(out, d)
		}
	}
	return out
}

// Suggest offers the same label on other parent domains, which SPEC.md §8
// prefers over mangling the name into "mymedia47". The user asked for a name;
// giving them that name somewhere else respects the request more than giving
// them a different name in the same place.
//
// limit caps the list; zero or less means all of them.
func (c *Catalog) Suggest(label, exclude string, limit int) []string {
	label = normalize(label)
	if label == "" {
		return nil
	}
	var out []string
	for _, d := range c.Others(exclude) {
		out = append(out, label+"."+d.Name)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}

// Split separates a hostname into its label and parent domain using the
// catalogue, so that "mymedia.freeddns.org" yields "mymedia" and
// "freeddns.org" rather than the wrong answer a naive dot-split gives on a
// two-part public suffix.
//
// The parent is matched against the catalogue rather than guessed, which is
// the only reliable way: nothing in the string itself says where the label
// ends.
func (c *Catalog) Split(hostname string) (label, parent string, ok bool) {
	hostname = normalize(hostname)
	for _, d := range c.domains {
		suffix := "." + d.Name
		if strings.HasSuffix(hostname, suffix) {
			return strings.TrimSuffix(hostname, suffix), d.Name, true
		}
	}
	return "", "", false
}

// Hostname joins a label and parent domain.
func Hostname(label, parent string) string {
	return normalize(label) + "." + normalize(parent)
}

func normalize(s string) string { return strings.ToLower(strings.TrimSpace(strings.Trim(s, "."))) }
