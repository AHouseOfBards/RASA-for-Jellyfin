package domains

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/rasaerr"
)

func TestEmbeddedLoads(t *testing.T) {
	c, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded: %v", err)
	}
	if len(c.All()) != 9 {
		t.Fatalf("expected the 9 PSL-listed domains from SPEC.md §8, got %d: %v", len(c.All()), c.Names())
	}
	if got := c.Default().Name; got != "freeddns.org" {
		t.Errorf("default = %q, want freeddns.org", got)
	}
}

// The three domains Dynu offers that the PSL does not list must never become
// selectable. If one appears in the offered list, every RASA user shares one
// Let's Encrypt bucket and issuance fails in a way nobody can diagnose.
func TestUnsafeDomainsAreNotOffered(t *testing.T) {
	c, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"flashhub.net", "ezgateway.net", "remotewire.net", "dynu.com", "dynu.org"} {
		if c.Allows(bad) {
			t.Errorf("%s is offered but is not on the Public Suffix List", bad)
		}
		if _, ok := c.BlockedReason(bad); !ok {
			t.Errorf("%s is neither offered nor recorded as blocked; advanced mode would have nothing to say about it", bad)
		}
	}
}

func TestLoadRejectsBadCatalogues(t *testing.T) {
	cases := map[string]string{
		"no default":   `{"domains":[{"name":"a.org"},{"name":"b.org"}]}`,
		"two defaults": `{"domains":[{"name":"a.org","default":true},{"name":"b.org","default":true}]}`,
		"duplicate":    `{"domains":[{"name":"a.org","default":true},{"name":"a.org"}]}`,
		"empty":        `{"domains":[]}`,
		"offered+blocked": `{"domains":[{"name":"a.org","default":true}],` +
			`"blocked":[{"name":"a.org","reason":"x"}]}`,
	}
	for name, body := range cases {
		if _, err := Load([]byte(body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestSplitUsesTheCatalogueNotDots(t *testing.T) {
	c, _ := Embedded()
	// A naive strings.Split on "." gives "mymedia.freeddns" / "org" here.
	label, parent, ok := c.Split("mymedia.freeddns.org")
	if !ok || label != "mymedia" || parent != "freeddns.org" {
		t.Fatalf("Split = %q, %q, %v", label, parent, ok)
	}
	if _, _, ok := c.Split("mymedia.example.com"); ok {
		t.Error("Split accepted a parent domain that is not in the catalogue")
	}
	if label, _, ok := c.Split("  MyMedia.FreeDDNS.ORG  "); !ok || label != "mymedia" {
		t.Errorf("Split did not normalise case and spacing: %q %v", label, ok)
	}
}

func TestSuggestOffersTheSameNameElsewhere(t *testing.T) {
	c, _ := Embedded()
	got := c.Suggest("mymedia", "freeddns.org", 4)
	if len(got) != 4 {
		t.Fatalf("want 4 suggestions, got %d", len(got))
	}
	for _, s := range got {
		if !strings.HasPrefix(s, "mymedia.") {
			t.Errorf("suggestion %q changed the name the user asked for", s)
		}
		if strings.HasSuffix(s, ".freeddns.org") {
			t.Errorf("suggestion %q repeats the domain that was taken", s)
		}
	}
}

func TestValidateLabel(t *testing.T) {
	cases := []struct {
		label string
		want  rasaerr.HostnameProblem // empty means valid
	}{
		{"mymedia", ""},
		{"my-media-2", ""},
		{"abc", ""},
		{"", rasaerr.HostnameEmpty},
		{"ab", rasaerr.HostnameTooShort},
		{strings.Repeat("a", 64), rasaerr.HostnameTooLong},
		{"my media", rasaerr.HostnameBadCharacters},
		{"my.media", rasaerr.HostnameBadCharacters},
		{"my_media", rasaerr.HostnameBadCharacters},
		{"médias", rasaerr.HostnameBadCharacters},
		{"-media", rasaerr.HostnameEdgeHyphen},
		{"media-", rasaerr.HostnameEdgeHyphen},
		{"xn--media", rasaerr.HostnameReservedPrefix},
	}
	for _, tc := range cases {
		err := ValidateLabel(tc.label)
		if tc.want == "" {
			if err != nil {
				t.Errorf("ValidateLabel(%q) = %v, want valid", tc.label, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("ValidateLabel(%q) accepted an invalid name", tc.label)
			continue
		}
		re, ok := rasaerr.As(err)
		if !ok || re.Code != rasaerr.CodeInvalidHostname {
			t.Errorf("ValidateLabel(%q) returned %v, not a catalogued error", tc.label, err)
			continue
		}
		if !strings.Contains(re.Detail, string(tc.want)) {
			t.Errorf("ValidateLabel(%q) detail = %q, want problem %s", tc.label, re.Detail, tc.want)
		}
	}
}

// A space produces a character complaint, not a length complaint, even though
// the name is also too short. The user can act on the space.
func TestCharacterComplaintWinsOverLength(t *testing.T) {
	err := ValidateLabel("a b")
	re, ok := rasaerr.As(err)
	if !ok || !strings.Contains(re.Detail, string(rasaerr.HostnameBadCharacters)) {
		t.Fatalf("got %v, want a character problem", err)
	}
}

func TestValidateHostnameExplainsBlockedParents(t *testing.T) {
	c, _ := Embedded()
	err := c.ValidateHostname("mymedia.flashhub.net")
	re, ok := rasaerr.As(err)
	if !ok || re.Code != rasaerr.CodeBlockedParentDomain {
		t.Fatalf("got %v, want a blocked-parent error", err)
	}
	if strings.Contains(re.Message, "Public Suffix") {
		t.Error("the user-facing message leaks the reason in jargon")
	}
	if err := c.ValidateHostname("mymedia.freeddns.org"); err != nil {
		t.Errorf("valid hostname rejected: %v", err)
	}
}

// Availability must never promise. "Unclaimed" is the strongest thing DNS
// evidence supports, and it is worded as an observation.
func TestUnclaimedDoesNotPromise(t *testing.T) {
	msg := Unclaimed.Message()
	for _, banned := range []string{"available", "yours", "free!", "confirmed"} {
		if strings.Contains(strings.ToLower(msg), banned) {
			t.Errorf("Unclaimed message %q makes a promise DNS cannot support", msg)
		}
	}
	if !Unclaimed.Usable() || !Mine.Usable() || !Undetermined.Usable() {
		t.Error("only InUse should block proceeding")
	}
	if InUse.Usable() {
		t.Error("a name that already resolves must not be usable")
	}
}

func TestCheckerClassifies(t *testing.T) {
	ch := &Checker{OwnAddresses: []string{"203.0.113.7"}}

	ch.Resolver = fakeResolver(func(host string) ([]string, error) {
		return []string{"198.51.100.4"}, nil
	})
	if got := ch.Check(context.Background(), "taken.freeddns.org"); got != InUse {
		t.Errorf("resolving name = %v, want InUse", got)
	}

	ch.Resolver = fakeResolver(func(host string) ([]string, error) {
		return []string{"203.0.113.7"}, nil
	})
	if got := ch.Check(context.Background(), "mine.freeddns.org"); got != Mine {
		t.Errorf("own address = %v, want Mine", got)
	}

	ch.Resolver = fakeResolver(func(host string) ([]string, error) {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	})
	if got := ch.Check(context.Background(), "free.freeddns.org"); got != Unclaimed {
		t.Errorf("NXDOMAIN = %v, want Unclaimed", got)
	}

	// A broken resolver must not be read as evidence about the name.
	ch.Resolver = fakeResolver(func(host string) ([]string, error) {
		return nil, errors.New("server misbehaving")
	})
	if got := ch.Check(context.Background(), "unknown.freeddns.org"); got != Undetermined {
		t.Errorf("resolver failure = %v, want Undetermined", got)
	}
}

// fakeResolver answers lookups from fn, so classification is tested without
// touching the network.
type fakeResolver func(host string) ([]string, error)

func (f fakeResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	return f(host)
}
