package rasaerr

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// all returns every error the catalogue can produce, so the copy rules below
// are enforced across the whole product rather than spot-checked.
func all() []*Error {
	cause := errors.New("Post \"https://api.dynu.com/v2/dns\": 401 Unauthorized")
	return []*Error{
		ACMERateLimited(cause),
		DNSNotVisible("mymedia.freeddns.org", 2*time.Minute, cause),
		PortMappingConflict(443, cause),
		JellyfinAuthRejected("localhost:8096", cause),
		JellyfinNotFound(cause),
		JellyfinTooOld("10.10.3", "10.11.5"),
		CarrierGradeNAT("100.87.4.12", "203.0.113.5"),
		PortHeldLocally(443, "IIS", cause),
		HostnameTaken("mymedia.freeddns.org", []string{"mymedia.kozow.com"}),
		BlockedParentDomain("dynu.com"),
		DynuAuthRejected(cause),
		NoRouteToInternet(cause),
	}
}

func TestUserProjectionNeverCarriesTechnicalDetail(t *testing.T) {
	// The structural guarantee: whatever a call site puts in Detail or in the
	// wrapped cause must not be reachable from what the UI renders.
	for _, e := range all() {
		u := e.User()
		joined := u.Message + " " + u.Why + " " + u.Partial
		if e.Detail != "" && strings.Contains(joined, e.Detail) {
			t.Errorf("%s: Detail leaked into user text", e.Code)
		}
		if e.wrapped != nil && strings.Contains(joined, e.wrapped.Error()) {
			t.Errorf("%s: wrapped cause leaked into user text", e.Code)
		}
	}
}

func TestUserTextAvoidsJargonAndCodes(t *testing.T) {
	// SPEC.md §15: no error codes, stack traces, API names, or jargon in
	// anything the user reads.
	banned := []string{
		"401", "403", "429", "500", "718", // status and IGD codes
		"HTTP", "http://", "https://api", // endpoints and protocols
		"nil", "panic", "exception", "errno", "stack",
		"AddPortMapping", "AuthenticateByName", "IGD", "ACME",
		"Public Suffix", "OpenAPI", "JSON",
	}
	for _, e := range all() {
		u := e.User()
		joined := u.Message + " " + u.Why
		for _, b := range banned {
			if strings.Contains(joined, b) {
				t.Errorf("%s: user text contains %q: %q", e.Code, b, joined)
			}
		}
	}
}

func TestEveryCatalogueEntryIsUsable(t *testing.T) {
	for _, e := range all() {
		u := e.User()
		if u.Code == "" {
			t.Errorf("entry with empty code: %+v", u)
		}
		if strings.TrimSpace(u.Message) == "" {
			t.Errorf("%s: empty user message", u.Code)
		}
		if !strings.HasSuffix(strings.TrimSpace(u.Message), ".") {
			t.Errorf("%s: message should be a complete sentence: %q", u.Code, u.Message)
		}
		if strings.Contains(u.Message, string(u.Code)) {
			t.Errorf("%s: message contains its own code", u.Code)
		}
		if len(u.Actions) == 0 {
			t.Errorf("%s: no recovery action offered — SPEC forbids dead ends", u.Code)
		}
		for _, a := range u.Actions {
			if a.ID == "" || a.Label == "" {
				t.Errorf("%s: action missing id or label: %+v", u.Code, a)
			}
		}
	}
}

func TestErrorStringIsTechnicalAndKeepsCause(t *testing.T) {
	cause := errors.New("dial tcp: connection refused")
	e := NoRouteToInternet(cause).WithPhase("probe")

	s := e.Error()
	for _, want := range []string{string(CodeNoRouteToInternet), "probe", "connection refused"} {
		if !strings.Contains(s, want) {
			t.Errorf("Error() missing %q: %q", want, s)
		}
	}
}

func TestUnwrapSupportsErrorsIs(t *testing.T) {
	sentinel := errors.New("sentinel")
	e := ACMERateLimited(sentinel)
	if !errors.Is(e, sentinel) {
		t.Fatal("errors.Is could not reach the wrapped cause")
	}
}

func TestAsExtractsFromChain(t *testing.T) {
	e := DynuAuthRejected(nil)
	wrapped := errors.Join(errors.New("context"), e)

	got, ok := As(wrapped)
	if !ok || got.Code != CodeDynuAuth {
		t.Fatalf("As failed to extract: %v %v", got, ok)
	}
}

func TestUserMessageFallbackDoesNotLeakRawError(t *testing.T) {
	// An unrecognised error must not put a Go error string in front of a user.
	raw := errors.New("dial tcp 203.0.113.5:443: i/o timeout")
	u := UserMessage(raw)

	if strings.Contains(u.Message+u.Why, raw.Error()) {
		t.Fatalf("raw error leaked to user: %+v", u)
	}
	if u.Code != CodeUnexpected {
		t.Fatalf("expected %s, got %s", CodeUnexpected, u.Code)
	}
	// And it must still not be the banned "unknown error occurred".
	if strings.Contains(strings.ToLower(u.Message), "unknown error") {
		t.Fatalf("fallback used banned phrasing: %q", u.Message)
	}
	if len(u.Actions) == 0 {
		t.Fatal("fallback offered no way forward")
	}
}

func TestUserMessagePrefersTypedError(t *testing.T) {
	e := JellyfinTooOld("10.10.3", "10.11.5")
	u := UserMessage(e)
	if u.Code != CodeJellyfinTooOld || !strings.Contains(u.Message, "10.10.3") {
		t.Fatalf("typed error not preferred: %+v", u)
	}
}

func TestUserMessageNilIsZero(t *testing.T) {
	if u := UserMessage(nil); u.Code != "" || u.Message != "" {
		t.Fatalf("expected zero value for nil error, got %+v", u)
	}
}

func TestPartialStateIsSurfacedToUser(t *testing.T) {
	// SPEC.md §15: an error must say whether anything was left half-configured.
	e := ACMERateLimited(nil).WithPartial("Your address was created and is still yours.")
	if u := e.User(); u.Partial == "" {
		t.Fatal("partial state not surfaced to the user")
	}
}

func TestRetryableReflectsActions(t *testing.T) {
	if !ACMERateLimited(nil).Retryable() {
		t.Error("rate limit should be retryable")
	}
	if BlockedParentDomain("dynu.com").Retryable() {
		t.Error("blocked domain should not offer a plain retry")
	}
}

func TestWithDetailAccumulates(t *testing.T) {
	e := NoRouteToInternet(nil).WithDetail("probe %d failed", 1).WithDetail("probe %d failed", 2)
	if !strings.Contains(e.Detail, "probe 1") || !strings.Contains(e.Detail, "probe 2") {
		t.Fatalf("detail did not accumulate: %q", e.Detail)
	}
}
