package dynu

import (
	"context"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
)

// Dynu's free tier allows four hostnames. The fifth create attempt is answered
// with statusCode 503 inside an HTTP 200 body, and 503 in the HTTP range means
// "come back later" — so without the envelope distinction RASA retries a wall
// three times and then reports a raw API error.
const fixtureQuota = `{"statusCode":503,"type":"QuotaException","message":"Quota exception."}`

func TestAFullAccountIsRecognisedAsQuotaNotAsATakenName(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/dns" {
			// The account is full, so the name is definitely not already ours.
			_, _ = w.Write([]byte(`{"statusCode":200,"domains":[]}`))
			return
		}
		_, _ = w.Write([]byte(fixtureQuota))
	})

	_, err := c.CreateDomain(context.Background(), CreateDomainRequest{Name: "mymedia.freeddns.org"})
	if err == nil {
		t.Fatal("expected the create to fail")
	}
	if !IsQuotaExhausted(err) {
		t.Errorf("IsQuotaExhausted(%v) = false, want true", err)
	}
	// Getting this wrong sends the user round the suggestion flow picking new
	// names, every one of which fails for the same reason.
	if IsNameUnavailable(err) {
		t.Errorf("a full account was reported as a taken name: %v", err)
	}
}

func TestAFullAccountIsNotRetried(t *testing.T) {
	var creates atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/dns" {
			_, _ = w.Write([]byte(`{"statusCode":200,"domains":[]}`))
			return
		}
		creates.Add(1)
		_, _ = w.Write([]byte(fixtureQuota))
	})

	_, _ = c.CreateDomain(context.Background(), CreateDomainRequest{Name: "mymedia.freeddns.org"})

	// Retrying sends the identical request and gets the identical answer. All
	// it buys is backoff between the user and the message that explains their
	// account.
	if n := creates.Load(); n != 1 {
		t.Errorf("the create was attempted %d times, want 1: a full account is not a transient failure", n)
	}
}

// A transport-level 503 is the opposite case and must keep its retry: Dynu
// being briefly unavailable is exactly what the retry budget is for.
func TestATransportLevel503IsStillRetried(t *testing.T) {
	var attempts atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("service unavailable"))
			return
		}
		_, _ = w.Write([]byte(fixtureDomains))
	})

	if _, err := c.ListDomains(context.Background()); err != nil {
		t.Fatalf("ListDomains: %v", err)
	}
	if n := attempts.Load(); n != 3 {
		t.Errorf("made %d attempts, want 3: an HTTP 503 is transient", n)
	}
}

// 501 and 505 are Dynu's answers for a name it will not hand over. They are
// just as deterministic as quota, and just as pointless to retry.
func TestATakenNameIsNotRetried(t *testing.T) {
	for _, code := range []int{StatusArgument, StatusValidation} {
		var creates atomic.Int32
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/dns" {
				_, _ = w.Write([]byte(`{"statusCode":200,"domains":[]}`))
				return
			}
			creates.Add(1)
			_, _ = w.Write([]byte(`{"statusCode":` + strconv.Itoa(code) + `,"message":"nope"}`))
		})

		_, err := c.CreateDomain(context.Background(), CreateDomainRequest{Name: "taken.freeddns.org"})
		if !IsNameUnavailable(err) {
			t.Errorf("status %d: IsNameUnavailable = false, want true (%v)", code, err)
		}
		if n := creates.Load(); n != 1 {
			t.Errorf("status %d: attempted %d times, want 1", code, n)
		}
	}
}
