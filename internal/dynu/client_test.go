package dynu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/rasaerr"
)

const testKey = "test-api-key-abcdef1234567890"

// Response fixtures reproduce the exact shapes captured from the live API on
// 2026-08-24. If Dynu changes them, these tests are what will notice.
const (
	fixtureDomains = `{"statusCode":200,"domains":[{"id":123,"name":"mymedia.freeddns.org",
		"unicodeName":"mymedia.freeddns.org","token":"per-hostname-secret-9876",
		"state":"true","group":"","ipv4Address":"203.0.113.5","ipv6Address":null,
		"ttl":60,"ipv4":true,"ipv6":false,"ipv4WildcardAlias":false,
		"ipv6WildcardAlias":false,"createdOn":"2026-01-01T00:00:00.000Z",
		"updatedOn":"2026-08-01T00:00:00.000Z"}]}`

	fixtureRoot = `{"statusCode":200,"id":123,"domainName":"freeddns.org",
		"hostname":"mymedia.freeddns.org","node":"mymedia"}`

	fixtureRecords = `{"statusCode":200,"dnsRecords":[{"id":9,"domainId":123,
		"domainName":"freeddns.org","nodeName":"mymedia","hostname":"mymedia.freeddns.org",
		"recordType":"A","ttl":60,"state":true,"content":"203.0.113.5",
		"updatedOn":"2026-08-01T00:00:00.000Z"}]}`
)

func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *bytes.Buffer) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	var logBuf bytes.Buffer
	log := logging.New(logging.Options{Writer: &logBuf, Level: -4}) // debug
	c := New(testKey,
		WithBaseURL(srv.URL),
		WithLogger(log),
		WithRetry(3, time.Millisecond),
	)
	return c, &logBuf
}

func TestListDomainsParsesLiveShape(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, fixtureDomains)
	})

	got, err := c.ListDomains(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d domains", len(got))
	}
	d := got[0]
	if d.ID != 123 || d.Name != "mymedia.freeddns.org" {
		t.Fatalf("identity not parsed: %+v", d)
	}
	if d.IPv4Address != "203.0.113.5" || !d.IPv4 {
		t.Fatalf("IPv4 fields not parsed: %+v", d)
	}
	if d.IPv6 {
		t.Fatal("IPv6 should be false")
	}
	if d.TTL != 60 {
		t.Fatalf("ttl = %d", d.TTL)
	}
}

func TestNullIPv6AddressIsHandled(t *testing.T) {
	// The live account returns null, not "", for an unset IPv6 address.
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, fixtureDomains)
	})
	got, err := c.ListDomains(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got[0].IPv6Address != "" {
		t.Fatalf("null should decode to empty, got %q", got[0].IPv6Address)
	}
}

func TestPerHostnameTokenIsRedactedFromLogs(t *testing.T) {
	// Each Domain carries a token used by the legacy update protocol. It
	// arrives unprompted in an ordinary list response, so it must be
	// registered as a secret the moment it is seen.
	c, logBuf := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, fixtureDomains)
	})

	got, err := c.ListDomains(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Now that the token has been seen, logging it must not leak it.
	c.log.Info("dumping domain", "token", got[0].Token)
	if strings.Contains(logBuf.String(), "per-hostname-secret-9876") {
		t.Fatalf("per-hostname token leaked into the log: %s", logBuf.String())
	}
}

func TestAPIKeyNeverReachesTheLog(t *testing.T) {
	c, logBuf := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, fixtureDomains)
	})
	if _, err := c.ListDomains(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.log.Info("request detail", "header", "API-Key: "+testKey)

	if strings.Contains(logBuf.String(), testKey) {
		t.Fatalf("api key leaked: %s", logBuf.String())
	}
}

func TestAPIKeyHeaderIsSent(t *testing.T) {
	var seen string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("API-Key")
		io.WriteString(w, fixtureDomains)
	})
	if _, err := c.ListDomains(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seen != testKey {
		t.Fatalf("API-Key header = %q", seen)
	}
}

func TestGetRootParsesZoneAndNode(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/dns/getroot/") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		io.WriteString(w, fixtureRoot)
	})

	got, err := c.GetRoot(context.Background(), "mymedia.freeddns.org")
	if err != nil {
		t.Fatal(err)
	}
	if got.DomainName != "freeddns.org" || got.Node != "mymedia" {
		t.Fatalf("zone/node not parsed: %+v", got)
	}
}

func TestListRecordsParsesLiveShape(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, fixtureRecords)
	})
	got, err := c.ListRecords(context.Background(), 123)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RecordType != "A" || got[0].Content != "203.0.113.5" {
		t.Fatalf("record not parsed: %+v", got)
	}
	if !got[0].State {
		t.Fatal("state should decode as bool true")
	}
}

func TestUpdateAddressesSendsBothFamilies(t *testing.T) {
	var body CreateDomainRequest
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, `{"statusCode":200,"id":123,"name":"mymedia.freeddns.org"}`)
	})

	_, err := c.UpdateAddresses(context.Background(), 123, "mymedia.freeddns.org",
		netip.MustParseAddr("203.0.113.5"), netip.MustParseAddr("2001:db8::1"))
	if err != nil {
		t.Fatal(err)
	}
	if !body.IPv4 || body.IPv4Address != "203.0.113.5" {
		t.Fatalf("IPv4 not sent: %+v", body)
	}
	if !body.IPv6 || body.IPv6Address != "2001:db8::1" {
		t.Fatalf("IPv6 not sent: %+v", body)
	}
}

func TestUpdateAddressesV4OnlyDoesNotEnableV6(t *testing.T) {
	// Setting an address without the matching boolean does nothing, and
	// enabling a family with no address would publish an empty record.
	var body CreateDomainRequest
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, `{"statusCode":200,"id":123}`)
	})

	_, err := c.UpdateAddresses(context.Background(), 123, "n", netip.MustParseAddr("203.0.113.5"), netip.Addr{})
	if err != nil {
		t.Fatal(err)
	}
	if body.IPv6 || body.IPv6Address != "" {
		t.Fatalf("IPv6 should be untouched: %+v", body)
	}
}

func TestUpdateAddressesRejectsNoValidAddress(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should be made")
	})
	if _, err := c.UpdateAddresses(context.Background(), 1, "n", netip.Addr{}, netip.Addr{}); err == nil {
		t.Fatal("expected an error when no address is supplied")
	}
}

func TestErrorInsideHTTP200IsDetected(t *testing.T) {
	// Dynu reports some failures with HTTP 200 and an error statusCode in the
	// body — exactly what the 404 probe returned during schema capture.
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"statusCode":404,"type":"Request Exception","message":"Invalid."}`)
	})

	_, err := c.GetRoot(context.Background(), "nope.freeddns.org")
	if err == nil {
		t.Fatal("expected an error carried inside a 200 response")
	}
	var ae *APIError
	if !errors.As(err, &ae) || ae.StatusCode != 404 {
		t.Fatalf("not classified as a 404: %v", err)
	}
}

func TestUnauthorizedMapsToTypedUserError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"statusCode":401,"message":"Unauthorized"}`)
	})

	_, err := c.ListDomains(context.Background())
	re, ok := rasaerr.As(err)
	if !ok || re.Code != rasaerr.CodeDynuAuth {
		t.Fatalf("expected a typed auth error, got %v", err)
	}
	// And the user-facing copy must not carry the status code.
	if strings.Contains(re.User().Message, "401") {
		t.Fatalf("status code leaked to user copy: %q", re.User().Message)
	}
}

func TestRetriesOnServerErrorThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		io.WriteString(w, fixtureDomains)
	})

	got, err := c.ListDomains(context.Background())
	if err != nil {
		t.Fatalf("should have recovered: %v", err)
	}
	if len(got) != 1 {
		t.Fatal("no domains after retry")
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("expected 3 attempts, got %d", n)
	}
}

func TestRetriesOnRateLimit(t *testing.T) {
	var calls atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		io.WriteString(w, fixtureDomains)
	})
	if _, err := c.ListDomains(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected a retry after 429, got %d calls", calls.Load())
	}
}

func TestDoesNotRetryClientErrors(t *testing.T) {
	// Retrying a 404 wastes time and, against a rate-limited API, budget.
	var calls atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"statusCode":404,"message":"Invalid."}`)
	})

	if _, err := c.GetDomain(context.Background(), 1); err == nil {
		t.Fatal("expected an error")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("404 should not be retried, got %d calls", n)
	}
}

func TestContextCancellationStopsRetrying(t *testing.T) {
	var calls atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.ListDomains(ctx); err == nil {
		t.Fatal("expected cancellation error")
	}
	if n := calls.Load(); n > 1 {
		t.Fatalf("kept retrying after cancellation: %d calls", n)
	}
}

func TestDeleteAbsentRecordSucceeds(t *testing.T) {
	// Cleanup after a failed run must be safe to repeat.
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"statusCode":404,"message":"Invalid."}`)
	})
	if err := c.DeleteRecord(context.Background(), 123, 9); err != nil {
		t.Fatalf("deleting an absent record should succeed: %v", err)
	}
}

func TestAddTXTRecordPopulatesBothContentFields(t *testing.T) {
	var body RecordRequest
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, `{"statusCode":200,"id":9,"domainId":123}`)
	})

	_, err := c.AddRecord(context.Background(), 123, RecordRequest{
		NodeName: "_acme-challenge", RecordType: RecordTXT, State: true, Content: "challenge-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if body.TextData != "challenge-token" || body.Content != "challenge-token" {
		t.Fatalf("TXT payload not populated in both fields: %+v", body)
	}
	if body.TTL != DefaultTTL {
		t.Fatalf("ttl defaulting failed: %d", body.TTL)
	}
}

func TestFindDomainMatchesCaseInsensitively(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, fixtureDomains)
	})
	got, err := c.FindDomain(context.Background(), "MyMedia.FreeDDNS.org")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != 123 {
		t.Fatalf("case-insensitive match failed: %+v", got)
	}
}

func TestFindDomainReturnsNilWhenAbsent(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"statusCode":200,"domains":[]}`)
	})
	got, err := c.FindDomain(context.Background(), "absent.freeddns.org")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

func TestRequestsAreLoggedWithStatusAndAttempt(t *testing.T) {
	// SPEC.md §15: every external call records method, target, status,
	// duration and retry count.
	c, logBuf := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, fixtureDomains)
	})
	if _, err := c.ListDomains(context.Background()); err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, line := range strings.Split(logBuf.String(), "\n") {
		if !strings.Contains(line, "dynu request") {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		for _, k := range []string{"method", "path", "status", "duration", "attempt"} {
			if _, ok := m[k]; !ok {
				t.Errorf("log line missing %q: %s", k, line)
			}
		}
		found = true
	}
	if !found {
		t.Fatalf("no request log line written: %s", logBuf.String())
	}
}
