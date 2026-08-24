package dynu

import (
	"context"
	"fmt"
	"math/rand"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"
)

// Live tests for the WRITE endpoints.
//
// These are gated behind a separate variable from the read-only live tests,
// because they create and delete real records on a real account:
//
//	RASA_LIVE_DYNU_WRITE=1 go test ./internal/dynu/ -run LiveWrite -v
//
// Safety rules this file follows, and any future addition to it must too:
//
//   - Every mutation happens on a hostname this test created itself. The
//     account's existing hostnames are never read into a mutation, and their
//     address records are never touched.
//   - Cleanup is deferred immediately after creation, so an assertion failure
//     partway through still removes the hostname.
//   - The name is obviously disposable and randomised, so a leaked one is
//     recognisable and cannot collide with a real hostname.
//
// No certificates are issued here, so Let's Encrypt rate limits are not
// involved.

// testParentDomain is on the PSL allowlist from SPEC.md §8.
const testParentDomain = "freeddns.org"

func liveWriteClient(t *testing.T) *Client {
	t.Helper()
	if os.Getenv("RASA_LIVE_DYNU_WRITE") != "1" {
		t.Skip("set RASA_LIVE_DYNU_WRITE=1 to run live write tests (creates and deletes real records)")
	}
	b, err := os.ReadFile(liveKeyFile)
	if err != nil {
		t.Skipf("no key file at %s", liveKeyFile)
	}
	key := strings.TrimSpace(string(b))
	if key == "" {
		t.Skip("key file is empty")
	}
	return New(key, WithRetry(3, time.Second))
}

func disposableHostname() string {
	return fmt.Sprintf("rasa-verify-%06d.%s", rand.Intn(1000000), testParentDomain)
}

// TestLiveWriteFullLifecycle exercises every write path RASA depends on, in
// the order setup performs them.
func TestLiveWriteFullLifecycle(t *testing.T) {
	c := liveWriteClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	name := disposableHostname()
	t.Logf("using disposable hostname with %d chars under %s", len(name), testParentDomain)

	// ---- create ----
	created, err := c.CreateDomain(ctx, CreateDomainRequest{
		Name:        name,
		IPv4:        true,
		IPv4Address: "203.0.113.5", // TEST-NET-3, never routed
		TTL:         DefaultTTL,
	})
	if err != nil {
		t.Fatalf("CreateDomain: %v", err)
	}
	if created.ID == 0 {
		t.Fatal("CreateDomain returned no id — later calls address by id")
	}
	id := created.ID

	// Registered before any assertion can fail, so a failure still cleans up.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := c.DeleteDomain(cleanupCtx, id); err != nil {
			t.Errorf("CLEANUP FAILED — hostname %s (id %d) may still exist on the account: %v", name, id, err)
		} else {
			t.Log("cleanup: disposable hostname deleted")
		}
	}()

	// ---- read back ----
	got, err := c.GetDomain(ctx, id)
	if err != nil {
		t.Fatalf("GetDomain after create: %v", err)
	}
	if !strings.EqualFold(got.Name, name) {
		t.Errorf("created name not echoed back: got %d chars", len(got.Name))
	}
	if got.IPv4Address != "203.0.113.5" {
		t.Errorf("ipv4Address = %q, want the address just set", got.IPv4Address)
	}
	if !got.IPv4 {
		t.Error("ipv4 flag not set — an address without the flag publishes nothing")
	}
	t.Logf("create verified: ipv4=%v ttl=%d", got.IPv4, got.TTL)

	// ---- FindDomain must see it ----
	found, err := c.FindDomain(ctx, name)
	if err != nil {
		t.Fatalf("FindDomain: %v", err)
	}
	if found == nil || found.ID != id {
		t.Error("FindDomain did not locate the hostname just created")
	}

	// ---- update addresses (both families) ----
	updated, err := c.UpdateAddresses(ctx, id, name,
		netip.MustParseAddr("203.0.113.9"),
		netip.MustParseAddr("2001:db8::1"))
	if err != nil {
		t.Fatalf("UpdateAddresses: %v", err)
	}
	_ = updated

	afterUpdate, err := c.GetDomain(ctx, id)
	if err != nil {
		t.Fatalf("GetDomain after update: %v", err)
	}
	if afterUpdate.IPv4Address != "203.0.113.9" {
		t.Errorf("IPv4 not updated: %q", afterUpdate.IPv4Address)
	}
	if !afterUpdate.IPv6 {
		t.Error("ipv6 flag not set after supplying an IPv6 address")
	}
	if afterUpdate.IPv6Address == "" {
		t.Error("ipv6Address empty after update — Mode A6 depends on this working")
	}
	t.Logf("update verified: ipv4=%v ipv6=%v", afterUpdate.IPv4, afterUpdate.IPv6)

	// ---- add a TXT record, as DNS-01 does ----
	//
	// The hostname is its own zone apex, so the challenge node is bare
	// "_acme-challenge" rather than "_acme-challenge.<node>".
	const challenge = "rasa-verification-token-not-a-real-challenge"
	rec, err := c.AddRecord(ctx, id, RecordRequest{
		NodeName:   "_acme-challenge",
		RecordType: RecordTXT,
		State:      true,
		Content:    challenge,
		TTL:        DefaultTTL,
	})
	if err != nil {
		t.Fatalf("AddRecord(TXT): %v", err)
	}
	if rec.ID == 0 {
		t.Fatal("AddRecord returned no id — cleanup addresses by id")
	}

	records, err := c.ListRecords(ctx, id)
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	var txt *Record
	for i := range records {
		if records[i].RecordType == RecordTXT {
			txt = &records[i]
			break
		}
	}
	if txt == nil {
		t.Fatal("TXT record not present after AddRecord — DNS-01 would fail")
	}
	if !strings.Contains(txt.Content, challenge) {
		t.Errorf("TXT content not stored as sent: got %d chars", len(txt.Content))
	}
	if txt.NodeName != "_acme-challenge" {
		t.Errorf("nodeName = %q, want _acme-challenge at the apex", txt.NodeName)
	}
	t.Logf("TXT verified: node=%q type=%s", txt.NodeName, txt.RecordType)

	// ---- delete the TXT record ----
	if err := c.DeleteRecord(ctx, id, txt.ID); err != nil {
		t.Fatalf("DeleteRecord: %v", err)
	}
	after, err := c.ListRecords(ctx, id)
	if err != nil {
		t.Fatalf("ListRecords after delete: %v", err)
	}
	for _, r := range after {
		if r.ID == txt.ID {
			t.Error("TXT record survived deletion — DNS-01 cleanup would leave litter")
		}
	}
	t.Log("TXT deletion verified")

	// ---- deleting it again must succeed ----
	if err := c.DeleteRecord(ctx, id, txt.ID); err != nil {
		t.Errorf("second DeleteRecord should be a no-op, got: %v", err)
	}
}

// TestLiveWriteCreateIsIdempotent checks that claiming a hostname the account
// already owns succeeds rather than failing, which SPEC.md §10 requires of
// every transition so a resumed run can replay steps.
func TestLiveWriteCreateIsIdempotent(t *testing.T) {
	c := liveWriteClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	name := disposableHostname()
	req := CreateDomainRequest{Name: name, IPv4: true, IPv4Address: "203.0.113.5", TTL: DefaultTTL}

	first, err := c.CreateDomain(ctx, req)
	if err != nil {
		t.Fatalf("first CreateDomain: %v", err)
	}
	id := first.ID
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := c.DeleteDomain(cleanupCtx, id); err != nil {
			t.Errorf("CLEANUP FAILED — hostname %s (id %d) may still exist: %v", name, id, err)
		}
	}()

	second, err := c.CreateDomain(ctx, req)
	if err != nil {
		t.Fatalf("re-claiming an owned hostname should succeed, got: %v", err)
	}
	if second.ID != id {
		t.Errorf("re-claim produced a different id (%d vs %d) — a duplicate may have been created", second.ID, id)
	}

	// And no duplicate may appear on the account.
	all, err := c.ListDomains(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, d := range all {
		if strings.EqualFold(d.Name, name) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("expected exactly one hostname named %s, found %d", name, n)
	}
	t.Log("re-claim verified as idempotent")
}

// TestLiveWriteDeleteIsIdempotent confirms cleanup can be repeated, which
// matters because uninstall and failed-run cleanup both re-run it.
func TestLiveWriteDeleteIsIdempotent(t *testing.T) {
	c := liveWriteClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	name := disposableHostname()
	created, err := c.CreateDomain(ctx, CreateDomainRequest{
		Name: name, IPv4: true, IPv4Address: "203.0.113.5", TTL: DefaultTTL,
	})
	if err != nil {
		t.Fatalf("CreateDomain: %v", err)
	}

	if err := c.DeleteDomain(ctx, created.ID); err != nil {
		t.Fatalf("first DeleteDomain: %v", err)
	}
	if err := c.DeleteDomain(ctx, created.ID); err != nil {
		t.Errorf("second DeleteDomain should be a no-op, got: %v", err)
	}

	gone, err := c.FindDomain(ctx, name)
	if err != nil {
		t.Fatalf("FindDomain after delete: %v", err)
	}
	if gone != nil {
		t.Errorf("hostname still present after deletion (id %d)", gone.ID)
	}
	t.Log("delete verified as idempotent and effective")
}
