package dynu

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// Live smoke tests against the real Dynu API.
//
// Skipped unless RASA_LIVE_DYNU=1 and a key file is present, so `go test ./...`
// stays hermetic and offline. These exist because recorded fixtures only prove
// RASA parses what it was told to expect — they cannot notice Dynu changing a
// field name. Run them before a release:
//
//	RASA_LIVE_DYNU=1 go test ./internal/dynu/ -run Live -v
//
// Every call here is read-only. Nothing in this file creates, updates, or
// deletes anything on the account.

const liveKeyFile = "../../.devdata/dynu-key.txt"

func liveClient(t *testing.T) *Client {
	t.Helper()
	if os.Getenv("RASA_LIVE_DYNU") != "1" {
		t.Skip("set RASA_LIVE_DYNU=1 to run live API tests")
	}
	b, err := os.ReadFile(liveKeyFile)
	if err != nil {
		t.Skipf("no key file at %s", liveKeyFile)
	}
	key := strings.TrimSpace(string(b))
	if key == "" {
		t.Skip("key file is empty")
	}
	return New(key, WithRetry(2, time.Second))
}

func TestLiveListDomains(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	domains, err := c.ListDomains(ctx)
	if err != nil {
		t.Fatalf("ListDomains: %v", err)
	}
	if len(domains) == 0 {
		t.Skip("account has no hostnames; nothing to verify")
	}

	// Assert the fields RASA depends on are actually populated, without
	// printing the hostname itself.
	d := domains[0]
	if d.ID == 0 {
		t.Error("id missing — RASA addresses every later call by id")
	}
	if d.Name == "" {
		t.Error("name missing")
	}
	if d.TTL == 0 {
		t.Error("ttl missing")
	}
	if !d.IPv4 && !d.IPv6 {
		t.Error("neither address family enabled — the booleans may have been renamed")
	}
	t.Logf("parsed %d hostname(s); first has id set, ttl=%d, ipv4=%v ipv6=%v",
		len(domains), d.TTL, d.IPv4, d.IPv6)
}

func TestLiveGetDomainAndRoot(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	domains, err := c.ListDomains(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(domains) == 0 {
		t.Skip("account has no hostnames")
	}

	got, err := c.GetDomain(ctx, domains[0].ID)
	if err != nil {
		t.Fatalf("GetDomain: %v", err)
	}
	if got.ID != domains[0].ID {
		t.Errorf("GetDomain returned a different id")
	}

	root, err := c.GetRoot(ctx, domains[0].Name)
	if err != nil {
		t.Fatalf("GetRoot: %v", err)
	}
	if root.DomainName == "" {
		t.Error("zone missing from getroot")
	}

	// A DDNS hostname is its own zone apex: getroot returns the whole
	// hostname as the domain and an empty node. Assert that, because if Dynu
	// ever changes it, TXT record placement for DNS-01 silently moves.
	if !root.IsApex() {
		t.Errorf("expected an apex record (empty node), got a node of %d chars", len(root.Node))
	}
	if !strings.EqualFold(root.DomainName, domains[0].Name) {
		t.Errorf("expected getroot to return the hostname itself as the zone")
	}

	// And therefore the PSL check must not use getroot's answer.
	parent := ParentDomain(domains[0].Name)
	if parent == "" || strings.EqualFold(parent, root.DomainName) {
		t.Errorf("ParentDomain must yield the shared parent, not the hostname")
	}
	if !strings.HasSuffix(strings.ToLower(domains[0].Name), parent) {
		t.Errorf("derived parent is not a suffix of the hostname")
	}
	t.Logf("apex confirmed: zone == hostname, node empty; derived parent has %d chars", len(parent))
}

func TestLiveListRecords(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	domains, err := c.ListDomains(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(domains) == 0 {
		t.Skip("account has no hostnames")
	}

	records, err := c.ListRecords(ctx, domains[0].ID)
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	for _, r := range records {
		if r.RecordType == "" {
			t.Error("recordType missing — DNS-01 cleanup matches on this")
		}
		if r.ID == 0 {
			t.Error("record id missing — deletion addresses by id")
		}
	}
	t.Logf("parsed %d record(s)", len(records))
}

func TestLiveUnknownHostnameIsAnError(t *testing.T) {
	// Confirms the error-inside-HTTP-200 handling against the real API.
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := c.GetRoot(ctx, "definitely-not-a-real-host-9f3a2b.freeddns.org"); err == nil {
		t.Fatal("expected an error for an unknown hostname")
	} else {
		t.Logf("correctly rejected: %v", err)
	}
}
