package routerguide

import (
	"net/netip"
	"strings"
	"testing"
)

func catalog(t *testing.T) *Catalog {
	t.Helper()
	c, err := Embedded()
	if err != nil {
		t.Fatalf("embedded catalogue does not load: %v", err)
	}
	return c
}

func TestEmbeddedCatalogueIsValid(t *testing.T) {
	c := catalog(t)
	if c.Len() < 5 {
		t.Fatalf("only %d routers known", c.Len())
	}
	// Every entry must be usable: a name and a menu path, or the instructions
	// render as blanks.
	for _, k := range c.order {
		e := c.entries[k]
		if strings.TrimSpace(e.Name) == "" {
			t.Errorf("%s: no name", k)
		}
		if strings.TrimSpace(e.Path) == "" {
			t.Errorf("%s: no menu path", k)
		}
		if !e.IsDefault() && len(e.Match.Vendor) == 0 && len(e.Match.Banner) == 0 && len(e.Match.OUI) == 0 {
			t.Errorf("%s: no way to match it", k)
		}
	}
}

func TestMetadataKeysAreSkipped(t *testing.T) {
	// The catalogue carries a _comment block for contributors; it must not
	// become a router.
	c := catalog(t)
	if _, ok := c.entries["_comment"]; ok {
		t.Fatal("_comment was loaded as a router entry")
	}
}

func TestMatchByUPnPVendor(t *testing.T) {
	c := catalog(t)
	// The exact string a real ASUS router reports.
	e := c.Match(Identity{Vendor: "ASUSTeK Computer Inc."})
	if e.IsDefault() {
		t.Fatal("ASUSTeK should have matched")
	}
	if e.Name != "ASUS" {
		t.Fatalf("matched %q", e.Name)
	}
}

func TestMatchByBannerWhenUPnPIsOff(t *testing.T) {
	c := catalog(t)
	e := c.Match(Identity{Banner: "FRITZ!Box 7590 - Login"})
	if e.IsDefault() || !strings.Contains(e.Name, "FRITZ") {
		t.Fatalf("matched %q", e.Name)
	}
}

func TestMatchByModelWhenVendorIsBlank(t *testing.T) {
	// Some routers leave the manufacturer field empty but put the brand in
	// the model.
	c := catalog(t)
	e := c.Match(Identity{Model: "Archer AX55"})
	if e.IsDefault() {
		t.Fatal("expected a TP-Link match from the model string")
	}
}

func TestMatchIsCaseInsensitive(t *testing.T) {
	c := catalog(t)
	if c.Match(Identity{Vendor: "netgear"}).IsDefault() {
		t.Fatal("lowercase vendor should still match")
	}
	if c.Match(Identity{Vendor: "NETGEAR Inc"}).IsDefault() {
		t.Fatal("uppercase vendor should still match")
	}
}

func TestUnknownRouterFallsBackToGeneric(t *testing.T) {
	// An unmatched router must still get usable instructions — "look for Port
	// Forwarding" beats an empty screen.
	c := catalog(t)
	e := c.Match(Identity{Vendor: "Some Obscure Brand"})
	if !e.IsDefault() {
		t.Fatalf("expected the fallback, got %q", e.Name)
	}
	if e.Path == "" {
		t.Fatal("the fallback must still carry a menu hint")
	}
}

func TestEmptyIdentityFallsBackToGeneric(t *testing.T) {
	if !catalog(t).Match(Identity{}).IsDefault() {
		t.Fatal("nothing known should yield the fallback")
	}
}

func TestMatchIsDeterministic(t *testing.T) {
	// Two runs on the same hardware must never produce different instructions.
	c := catalog(t)
	id := Identity{Vendor: "ASUSTeK Computer Inc."}
	first := c.Match(id).Key()
	for i := 0; i < 50; i++ {
		if got := c.Match(id).Key(); got != first {
			t.Fatalf("match is unstable: %q then %q", first, got)
		}
	}
}

func TestNormalizeOUI(t *testing.T) {
	cases := map[string]string{
		"00:1F:C6:AA:BB:CC": "00:1f:c6",
		"00-1f-c6-aa-bb-cc": "00:1f:c6",
		"00:1f:c6":          "00:1f:c6",
		"garbage":           "",
		"":                  "",
		"00:1":              "",
	}
	for in, want := range cases {
		if got := normalizeOUI(in); got != want {
			t.Errorf("normalizeOUI(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOUIMatchingWhenPresent(t *testing.T) {
	// The embedded catalogue ships few OUIs on purpose, so this uses its own.
	c, err := Load([]byte(`{
	  "acme": {"name":"Acme","match":{"oui":["00:1F:C6"]},"path":"somewhere"},
	  "_default": {"name":"your router","match":{},"path":"look around"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Match(Identity{MAC: "00-1f-c6-11-22-33"}); got.Name != "Acme" {
		t.Fatalf("OUI match failed, got %q", got.Name)
	}
}

func TestVendorBeatsOUI(t *testing.T) {
	// Vendor comes from UPnP and is trustworthy; OUI identifies only the
	// manufacturer, so it must not override a better signal.
	c, err := Load([]byte(`{
	  "byoui":    {"name":"ByOUI","match":{"oui":["00:1F:C6"]},"path":"a"},
	  "byvendor": {"name":"ByVendor","match":{"vendor":["Netgear"]},"path":"b"},
	  "_default": {"name":"your router","match":{},"path":"c"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	got := c.Match(Identity{Vendor: "Netgear", MAC: "00:1f:c6:11:22:33"})
	if got.Name != "ByVendor" {
		t.Fatalf("vendor should win, got %q", got.Name)
	}
}

func TestCatalogueWithoutDefaultIsRejected(t *testing.T) {
	// Without a fallback, an unmatched router would render blank instructions.
	if _, err := Load([]byte(`{"asus":{"name":"ASUS","path":"x"}}`)); err == nil {
		t.Fatal("a catalogue with no _default must be rejected")
	}
}

func TestInvalidJSONIsRejected(t *testing.T) {
	if _, err := Load([]byte(`{not json`)); err == nil {
		t.Fatal("expected a parse error")
	}
}

// ---------- rendering ----------

func values(dhcp bool) Values {
	return Values{
		Gateway:       netip.MustParseAddr("192.168.1.1"),
		InternalIP:    netip.MustParseAddr("192.168.1.10"),
		Port:          8443,
		AddressIsDHCP: dhcp,
	}
}

func TestBuildFillsInEveryValue(t *testing.T) {
	// The values are what users get wrong, so none may be left blank.
	ins := Build(catalog(t).Match(Identity{Vendor: "ASUSTeK"}), values(false))

	if ins.AdminURL != "http://192.168.1.1" {
		t.Errorf("admin url = %q", ins.AdminURL)
	}
	joined := ""
	for _, f := range ins.Fields {
		if strings.TrimSpace(f.Value) == "" {
			t.Errorf("field %q has no value", f.Label)
		}
		joined += f.Label + "=" + f.Value + ";"
	}
	for _, want := range []string{"8443", "192.168.1.10", "TCP", ServiceLabel} {
		if !strings.Contains(joined, want) {
			t.Errorf("fields missing %q: %s", want, joined)
		}
	}
}

func TestBuildIncludesReservationWhenLeased(t *testing.T) {
	// SPEC.md §6: the reservation is part of the instructions, not an
	// afterthought — a forward pointed at a leased address breaks later.
	ins := Build(catalog(t).Match(Identity{Vendor: "ASUSTeK"}), values(true))
	if !ins.ReservationRequired {
		t.Fatal("a leased address must require a reservation")
	}

	var found bool
	for _, s := range ins.Steps {
		if strings.Contains(strings.ToLower(s), "reserve") {
			found = true
			// It must explain the consequence, not just issue an order.
			if !strings.Contains(strings.ToLower(s), "stop working") {
				t.Errorf("reservation step does not explain why: %q", s)
			}
		}
	}
	if !found {
		t.Fatal("no reservation step in the instructions")
	}
}

func TestBuildOmitsReservationWhenStatic(t *testing.T) {
	ins := Build(catalog(t).Match(Identity{Vendor: "ASUSTeK"}), values(false))
	for _, s := range ins.Steps {
		if strings.Contains(strings.ToLower(s), "reserve") {
			t.Fatalf("static address should not be told to reserve: %q", s)
		}
	}
}

func TestBuildAlwaysEndsWithVerification(t *testing.T) {
	// SPEC.md §6: never end on "you should be all set".
	for _, dhcp := range []bool{true, false} {
		ins := Build(catalog(t).Match(Identity{}), values(dhcp))
		last := ins.Steps[len(ins.Steps)-1]
		if !strings.Contains(strings.ToLower(last), "test again") {
			t.Errorf("dhcp=%v: instructions end with %q, not a verification step", dhcp, last)
		}
	}
}

func TestBuildHandlesUnknownGateway(t *testing.T) {
	v := values(false)
	v.Gateway = netip.Addr{}
	ins := Build(catalog(t).Match(Identity{}), v)

	if ins.AdminURL != "" {
		t.Errorf("no gateway known, so no admin url: %q", ins.AdminURL)
	}
	if !strings.Contains(strings.ToLower(ins.Steps[0]), "router") {
		t.Errorf("first step should still tell them where to go: %q", ins.Steps[0])
	}
}

func TestBuildStepsAreUserFacingLanguage(t *testing.T) {
	ins := Build(catalog(t).Match(Identity{Vendor: "ASUSTeK"}), values(true))
	all := strings.Join(ins.Steps, " ") + " " + ins.Note
	for _, banned := range []string{"UPnP", "IGD", "SOAP", "NAT-PMP", "nil", "error", "SSDP"} {
		if strings.Contains(all, banned) {
			t.Errorf("instructions contain jargon %q", banned)
		}
	}
}

func TestPlainTextRendersForRecoveryFile(t *testing.T) {
	// The recovery file is the only support artifact that outlives RASA, so
	// it must carry the values, not just the steps.
	txt := Build(catalog(t).Match(Identity{Vendor: "ASUSTeK"}), values(true)).PlainText()

	for _, want := range []string{"8443", "192.168.1.10", "192.168.1.1", "TCP", "ASUS"} {
		if !strings.Contains(txt, want) {
			t.Errorf("recovery text missing %q", want)
		}
	}
	if !strings.Contains(txt, "1.") {
		t.Error("steps are not numbered")
	}
}

func TestGenericEntryIsFlagged(t *testing.T) {
	// The UI should be able to soften its wording when it is guessing.
	if !Build(catalog(t).Match(Identity{Vendor: "Nope"}), values(false)).Generic {
		t.Fatal("fallback instructions should be flagged as generic")
	}
	if Build(catalog(t).Match(Identity{Vendor: "NETGEAR"}), values(false)).Generic {
		t.Fatal("a recognised router should not be flagged as generic")
	}
}

func TestStepsReadAsSentences(t *testing.T) {
	// Menu paths are interpolated into steps like "Go to <path>." and
	// "reserve this computer's address in <path>." A path phrased as its own
	// instruction produces "Go to look for 'Port Forwarding'", which is how
	// the generic entry originally read. Every entry must be a noun phrase.
	c := catalog(t)
	for _, k := range c.order {
		e := c.entries[k]
		for _, field := range []struct{ name, val string }{
			{"path", e.Path},
			{"reservationPath", e.ReservationPath},
		} {
			if field.val == "" {
				continue
			}
			first := strings.ToLower(strings.Fields(field.val)[0])
			for _, verb := range []string{"look", "go", "open", "find", "click", "navigate", "check"} {
				if first == verb {
					t.Errorf("%s.%s starts with the verb %q; it is interpolated after "+
						"\"Go to\" and must be a noun phrase: %q", k, field.name, verb, field.val)
				}
			}
		}
	}
}

func TestRenderedStepsHaveNoDoubledVerbs(t *testing.T) {
	c := catalog(t)
	for _, k := range c.order {
		ins := Build(c.entries[k], values(true))
		for _, s := range ins.Steps {
			low := strings.ToLower(s)
			for _, bad := range []string{"go to look", "go to go to", "address in look for"} {
				if strings.Contains(low, bad) {
					t.Errorf("%s: step reads badly (%q): %q", k, bad, s)
				}
			}
		}
	}
}

// Real brands that are deliberately not in the catalogue. Each must land on
// the generic fallback with usable instructions rather than matching something
// else by accident: a Verizon path shown to a Sagemcom owner is worse than the
// generic one, which is the standard the catalogue header sets.
func TestUnlistedBrandsGetTheGenericGuide(t *testing.T) {
	c := catalog(t)
	unlisted := []string{
		"Sagemcom", "Technicolor", "Arris", "Hitron", "ZyXEL",
		"D-Link", "Huawei", "Xiaomi", "Amazon", "Vodafone",
	}
	for _, vendor := range unlisted {
		e := c.Match(Identity{Vendor: vendor})
		if !e.IsDefault() {
			t.Errorf("%s matched %q; an unlisted brand must fall back, not borrow another vendor's menus", vendor, e.Name)
			continue
		}
		if e.Path == "" || e.ReservationPath == "" {
			t.Errorf("%s fell back to an entry with nothing usable in it", vendor)
		}
	}
}

// The catalogue header says an entry without a source has not been verified
// and should not be trusted over the generic fallback. Enforced here, because
// the first version of this file was written from memory and read exactly as
// confidently as a checked one.
func TestEveryEntryNamesWhereItWasVerified(t *testing.T) {
	c := catalog(t)
	for _, e := range c.entries {
		if e.IsDefault() {
			continue // Nothing to verify: it names no vendor's menus.
		}
		if e.Source == "" {
			t.Errorf("%s has no source; either verify it against the vendor's documentation or remove it", e.Name)
		}
	}
}

// Each entry must match its own hardware. ISP gateways are the risk: they are
// rebadged from a handful of manufacturers, so a needle like "Sagemcom" or
// "Arris" would claim half a dozen unrelated ISPs' customers.
func TestISPGatewaysMatchTheirOwnHardwareOnly(t *testing.T) {
	c := catalog(t)
	for _, tc := range []struct {
		id   Identity
		want string
	}{
		{Identity{Vendor: "Comcast"}, "Xfinity (Comcast)"},
		{Identity{Banner: "XB8"}, "Xfinity (Comcast)"},
		{Identity{Banner: "BGW320-505"}, "AT&T"},
		{Identity{Vendor: "Charter"}, "Spectrum (Charter)"},
		{Identity{Banner: "BT Smart Hub"}, "BT Hub"},
		{Identity{Banner: "BT Home Hub 5"}, "BT Hub"},
		{Identity{Banner: "Bell Home Hub"}, "Bell Home Hub"},
		{Identity{Vendor: "Orange"}, "Orange Livebox"},
		{Identity{Banner: "Freebox Server"}, "Freebox"},
		{Identity{Vendor: "Telstra"}, "Telstra Smart Modem"},
		{Identity{Vendor: "Rogers"}, "Rogers Ignite"},
		{Identity{Vendor: "Virgin Media"}, "Virgin Media Hub"},
		{Identity{Banner: "Sky Hub"}, "Sky Hub"},
		{Identity{Vendor: "Verizon"}, "Verizon (Fios)"},
		{Identity{Vendor: "Actiontec"}, "Verizon (Fios)"},
	} {
		if got := c.Match(tc.id).Name; got != tc.want {
			t.Errorf("%+v matched %q, want %q", tc.id, got, tc.want)
		}
	}
}

// The manufacturers behind rebadged ISP gateways, plus brands from markets with
// no entry. None may match: the manufacturer of an ISP box says nothing about
// whose menus the customer is looking at, and a confidently wrong guide is
// worse than the generic one.
func TestRebadgedAndForeignHardwareFallsBack(t *testing.T) {
	c := catalog(t)
	for _, v := range []string{
		"Sagemcom", "Technicolor", "Vantiva", "Arris", "Hitron", "Humax", "Askey",
		"Bouygues", "SFR", "Movistar", "TIM", "Telekom", "Speedport", "Vodafone",
		"Ziggo", "KPN", "Telia", "TalkTalk", "Plusnet", "Telus", "Optus",
		"D-Link", "ZyXEL", "Huawei", "Xiaomi", "Tenda", "Amazon", "Skyworth",
	} {
		if e := c.Match(Identity{Vendor: v}); !e.IsDefault() {
			t.Errorf("vendor %q matched %q; it must fall back", v, e.Name)
		}
		if e := c.Match(Identity{Banner: v}); !e.IsDefault() {
			t.Errorf("banner %q matched %q; it must fall back", v, e.Name)
		}
	}
}

// Firmware generations outlive their menus. Someone on a router bought years
// ago is exactly the person who needs the guide, so an entry whose vendor has
// moved the page has to name the old path too.
func TestEntriesWhoseVendorMovedThePageNameTheOldOne(t *testing.T) {
	c := catalog(t)
	for _, key := range []string{"asus", "tplink", "netgear", "fritzbox", "linksys", "ubiquiti", "verizon", "xfinity", "google", "sky"} {
		e, ok := c.entries[key]
		if !ok {
			t.Fatalf("no entry %q", key)
		}
		if !strings.Contains(strings.ToLower(e.Note), "older") {
			t.Errorf("%s: note does not mention the older firmware path", e.Name)
		}
	}
}
