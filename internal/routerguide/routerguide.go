// Package routerguide turns what the probe learned about a router into
// instructions a person can follow.
//
// SPEC.md §6: generic port-forwarding guides fail because they cannot tell the
// user which values to type or where their router hides the setting. RASA
// knows both, so the menu path is the smaller half — the values are what
// people actually get wrong, and they are presented filled in rather than as
// blanks to work out.
//
// The catalogue is data rather than code so that adding a router is a pull
// request, not a release.
package routerguide

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"sort"
	"strings"
)

//go:embed routers.json
var embeddedJSON []byte

// DefaultKey names the fallback entry used when nothing matches.
const DefaultKey = "_default"

// Match describes how to recognise a router. Every field is a list of
// case-insensitive substrings, except OUI which is compared exactly after
// normalisation.
type Match struct {
	// Vendor matches the manufacturer reported over UPnP. Most reliable.
	Vendor []string `json:"vendor,omitempty"`
	// Banner matches the gateway admin page title or Server header. Used when
	// UPnP is off.
	Banner []string `json:"banner,omitempty"`
	// OUI matches the first three octets of the gateway MAC. Works when the
	// router answers nothing at all, but identifies only the vendor.
	OUI []string `json:"oui,omitempty"`
}

// Entry is one router's instructions.
type Entry struct {
	Name  string `json:"name"`
	Match Match  `json:"match"`
	Path  string `json:"path"`
	Note  string `json:"note,omitempty"`
	// ReservationPath is where the DHCP reservation lives. It is part of the
	// instructions rather than an afterthought: a static forward pointed at a
	// leased address breaks weeks later when the lease moves, which is the
	// most common cause of remote access silently dying.
	ReservationPath string `json:"reservationPath,omitempty"`

	key string
}

// Key returns the catalogue key this entry was loaded under.
func (e Entry) Key() string { return e.key }

// IsDefault reports whether this is the generic fallback.
func (e Entry) IsDefault() bool { return e.key == DefaultKey }

// Catalog is the loaded router set.
type Catalog struct {
	entries map[string]Entry
	// order is the deterministic key order used when matching, so two runs on
	// the same hardware never produce different instructions.
	order []string
}

// Embedded returns the catalogue compiled into the binary.
//
// This is the baked-in fallback described in SPEC.md §6; a build may also
// fetch a newer copy at runtime and Load it.
func Embedded() (*Catalog, error) { return Load(embeddedJSON) }

// Load parses a catalogue.
func Load(data []byte) (*Catalog, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("router catalogue is not valid JSON: %w", err)
	}

	c := &Catalog{entries: make(map[string]Entry, len(raw))}
	for k, v := range raw {
		// Keys beginning with an underscore are metadata, except the default
		// entry which is a real fallback.
		if strings.HasPrefix(k, "_") && k != DefaultKey {
			continue
		}
		var e Entry
		if err := json.Unmarshal(v, &e); err != nil {
			return nil, fmt.Errorf("router %q: %w", k, err)
		}
		e.key = k
		if e.Name == "" {
			e.Name = k
		}
		c.entries[k] = e
		c.order = append(c.order, k)
	}
	if _, ok := c.entries[DefaultKey]; !ok {
		return nil, fmt.Errorf("router catalogue has no %s entry", DefaultKey)
	}
	sort.Strings(c.order)
	return c, nil
}

// LoadFrom parses a catalogue from a reader.
func LoadFrom(r io.Reader) (*Catalog, error) {
	b, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return nil, err
	}
	return Load(b)
}

// Len reports how many routers are known, excluding the fallback.
func (c *Catalog) Len() int { return len(c.entries) - 1 }

// Identity is what the probe learned about the router.
type Identity struct {
	Vendor string
	Model  string
	// MAC is the gateway hardware address, in any common format.
	MAC string
	// Banner is the admin page title or Server header, if it was read.
	Banner string
}

// Match finds the best entry for a router.
//
// Tiers are tried in order of reliability. Vendor comes from UPnP and is
// trustworthy; banner is nearly as good; OUI identifies only the manufacturer
// and is last. An unmatched router gets the generic entry rather than nothing,
// because "look for Port Forwarding" still beats an empty screen.
func (c *Catalog) Match(id Identity) Entry {
	if e, ok := c.matchBy(id.Vendor, func(m Match) []string { return m.Vendor }); ok {
		return e
	}
	if e, ok := c.matchBy(id.Banner, func(m Match) []string { return m.Banner }); ok {
		return e
	}
	// The model string sometimes carries the brand when the manufacturer field
	// does not — "Archer AX55" names no vendor but is unmistakably TP-Link. It
	// is checked against both needle sets, since a product line usually
	// appears in the banner list rather than the vendor one.
	if e, ok := c.matchBy(id.Model, func(m Match) []string {
		return append(append([]string{}, m.Vendor...), m.Banner...)
	}); ok {
		return e
	}
	if oui := normalizeOUI(id.MAC); oui != "" {
		for _, k := range c.order {
			for _, want := range c.entries[k].Match.OUI {
				if normalizeOUI(want) == oui {
					return c.entries[k]
				}
			}
		}
	}
	return c.entries[DefaultKey]
}

func (c *Catalog) matchBy(have string, pick func(Match) []string) (Entry, bool) {
	h := strings.ToLower(strings.TrimSpace(have))
	if h == "" {
		return Entry{}, false
	}
	for _, k := range c.order {
		if k == DefaultKey {
			continue
		}
		for _, needle := range pick(c.entries[k].Match) {
			if n := strings.ToLower(strings.TrimSpace(needle)); n != "" && strings.Contains(h, n) {
				return c.entries[k], true
			}
		}
	}
	return Entry{}, false
}

// normalizeOUI renders the first three octets lowercase and colon-separated.
func normalizeOUI(s string) string {
	s = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(s, "-", ":")))
	parts := strings.Split(s, ":")
	if len(parts) < 3 {
		return ""
	}
	for _, p := range parts[:3] {
		if len(p) != 2 {
			return ""
		}
	}
	return strings.Join(parts[:3], ":")
}

// Values are the settings the user must enter. RASA knows all of them, which
// is the whole point — they are presented filled in.
type Values struct {
	// Gateway is the router's address, shown as a clickable admin link.
	Gateway netip.Addr
	// InternalIP is this machine's address on the local network.
	InternalIP netip.Addr
	// Port is the external and internal port; RASA never maps between them.
	Port int
	// AddressIsDHCP drives whether the reservation step is included.
	AddressIsDHCP bool
}

// Field is one labelled value to copy into the router.
type Field struct {
	Label string
	Value string
	// Hint explains a value whose purpose is not obvious.
	Hint string
}

// Instructions is the rendered guide.
type Instructions struct {
	RouterName string
	AdminURL   string
	MenuPath   string
	Note       string
	Fields     []Field
	Steps      []string
	// ReservationRequired is true when this machine's address is leased and
	// would otherwise change, breaking the forward.
	ReservationRequired bool
	ReservationPath     string
	// Generic is true when no specific router was recognised.
	Generic bool
}

// ServiceLabel is what the forwarding entry is named in the router, so it is
// recognisable a year later.
const ServiceLabel = "Jellyfin"

// Build renders instructions for a router.
func Build(e Entry, v Values) Instructions {
	ins := Instructions{
		RouterName:          e.Name,
		MenuPath:            e.Path,
		Note:                e.Note,
		ReservationRequired: v.AddressIsDHCP,
		ReservationPath:     e.ReservationPath,
		Generic:             e.IsDefault(),
	}
	if v.Gateway.IsValid() {
		ins.AdminURL = "http://" + v.Gateway.String()
	}

	ins.Fields = []Field{
		{Label: "Service name", Value: ServiceLabel, Hint: "any name, this is just so you recognise the entry later"},
		{Label: "External port", Value: fmt.Sprint(v.Port)},
		{Label: "Internal port", Value: fmt.Sprint(v.Port), Hint: "the same as the external port"},
		{Label: "Protocol", Value: "TCP"},
	}
	if v.InternalIP.IsValid() {
		ins.Fields = append(ins.Fields, Field{
			Label: "Internal / device IP",
			Value: v.InternalIP.String(),
			Hint:  "this computer",
		})
	}

	ins.Steps = buildSteps(ins, v)
	return ins
}

func buildSteps(ins Instructions, v Values) []string {
	var steps []string

	if ins.AdminURL != "" {
		steps = append(steps, "Open "+ins.AdminURL+" and sign in to your router.")
	} else {
		steps = append(steps, "Open your router's settings page and sign in.")
	}

	steps = append(steps, "Go to "+ins.MenuPath+".")
	steps = append(steps, "Add a new entry using the values below, then save.")

	// The reservation is part of the instructions, not an afterthought: the
	// user is already signed in, and it is usually one menu away.
	if ins.ReservationRequired {
		where := ins.ReservationPath
		if where == "" {
			where = "your router's DHCP or address reservation settings"
		}
		steps = append(steps,
			"While you are here, reserve this computer's address in "+where+
				". Without it your router may give this computer a different address later, and remote access will stop working.")
	}

	steps = append(steps, "Come back and choose Test again.")
	return steps
}

// PlainText renders the instructions for the recovery file, which is the only
// support artifact that outlives RASA (SPEC.md §15).
func (i Instructions) PlainText() string {
	var b strings.Builder

	fmt.Fprintf(&b, "PORT FORWARDING — %s\n", i.RouterName)
	b.WriteString(strings.Repeat("-", 60) + "\n\n")

	for n, s := range i.Steps {
		fmt.Fprintf(&b, "  %d. %s\n", n+1, s)
	}
	b.WriteString("\n  Values to enter:\n")
	for _, f := range i.Fields {
		fmt.Fprintf(&b, "    %-22s %s", f.Label, f.Value)
		if f.Hint != "" {
			fmt.Fprintf(&b, "   (%s)", f.Hint)
		}
		b.WriteString("\n")
	}
	if i.Note != "" {
		fmt.Fprintf(&b, "\n  Note: %s\n", i.Note)
	}
	return b.String()
}
