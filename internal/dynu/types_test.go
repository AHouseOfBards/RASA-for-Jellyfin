package dynu

import "testing"

// ParentDomain is what the PSL allowlist in SPEC.md §8 is checked against, and
// therefore what decides which Let's Encrypt rate-limit bucket a certificate
// falls into. Getting it wrong means either refusing a valid name or accepting
// one that shares a quota with every other user of the parent domain.

func TestParentDomain(t *testing.T) {
	cases := map[string]string{
		"mymedia.freeddns.org":    "freeddns.org",
		"MyMedia.FreeDDNS.ORG":    "freeddns.org",
		"a.b.kozow.com":           "b.kozow.com",
		"mymedia.freeddns.org.":   "freeddns.org",
		"  mymedia.mywire.org  ":  "mywire.org",
		"freeddns.org":            "org",
		"single":                  "",
		"":                        "",
		"trailingdot.":            "",
		"mymedia.webredirect.org": "webredirect.org",
	}
	for in, want := range cases {
		if got := ParentDomain(in); got != want {
			t.Errorf("ParentDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNodeOf(t *testing.T) {
	cases := map[string]string{
		"mymedia.freeddns.org": "mymedia",
		"MyMedia.freeddns.org": "mymedia",
		"a.b.kozow.com":        "a",
		"single":               "single",
	}
	for in, want := range cases {
		if got := NodeOf(in); got != want {
			t.Errorf("NodeOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsApex(t *testing.T) {
	// The normal case for a DDNS hostname.
	if !(Root{DomainName: "mymedia.freeddns.org", Node: ""}).IsApex() {
		t.Error("empty node should be apex")
	}
	if (Root{DomainName: "freeddns.org", Node: "mymedia"}).IsApex() {
		t.Error("populated node is not apex")
	}
}

func TestParentDomainIgnoresGetRootAnswer(t *testing.T) {
	// Guards the trap: GetRoot returns the whole hostname as DomainName, so
	// using it for the allowlist check would compare the wrong string.
	root := Root{DomainName: "mymedia.freeddns.org", Hostname: "mymedia.freeddns.org", Node: ""}
	if got := ParentDomain(root.Hostname); got == root.DomainName {
		t.Fatal("ParentDomain must not agree with GetRoot's DomainName for a DDNS hostname")
	} else if got != "freeddns.org" {
		t.Fatalf("got %q", got)
	}
}
