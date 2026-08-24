package logging

import (
	"strings"
	"testing"
)

// These tests are the enforcement mechanism for SPEC.md §15: "Redaction is a
// tested feature, not a convention." If a secret can survive Redact, the
// diagnostic bundle leaks it into a public GitHub issue.

func TestRedactRegisteredSecret(t *testing.T) {
	r := NewRedactor()
	const key = "dynu-api-key-abcdef123456"
	r.RegisterSecret(key)

	got := r.Redact("calling Dynu with API-Key " + key + " ok")
	if strings.Contains(got, key) {
		t.Fatalf("secret survived redaction: %q", got)
	}
	if !strings.Contains(got, Placeholder) {
		t.Fatalf("expected placeholder, got %q", got)
	}
}

func TestRedactSecretAppearingManyTimes(t *testing.T) {
	r := NewRedactor()
	const pw = "correct-horse-battery"
	r.RegisterSecret(pw)

	got := r.Redact(pw + " and again " + pw + " and " + pw)
	if strings.Contains(got, pw) {
		t.Fatalf("secret survived redaction: %q", got)
	}
	if n := strings.Count(got, Placeholder); n != 3 {
		t.Fatalf("expected 3 placeholders, got %d in %q", n, got)
	}
}

func TestRedactOverlappingSecretsRemovesLongest(t *testing.T) {
	// A short secret that is a prefix of a longer one must not leave the
	// longer one's tail exposed.
	r := NewRedactor()
	r.RegisterSecret("abcdef1234")
	r.RegisterSecret("abcdef1234567890")

	got := r.Redact("token=abcdef1234567890 end")
	if strings.Contains(got, "567890") {
		t.Fatalf("tail of longer secret leaked: %q", got)
	}
}

func TestRedactIgnoresShortValues(t *testing.T) {
	// Registering "a" must not turn every line into placeholders.
	r := NewRedactor()
	r.RegisterSecret("a")
	r.RegisterSecret("")
	r.RegisterSecret("short")

	const msg = "a normal sentence that mentions a short word"
	if got := r.Redact(msg); got != msg {
		t.Fatalf("short value caused mangling: %q", got)
	}
}

func TestRedactPatternsCatchUnregisteredSecrets(t *testing.T) {
	// Defence in depth: a code path that forgets RegisterSecret should still
	// not leak a secret-shaped value.
	r := NewRedactor()

	cases := []struct {
		name string
		in   string
		leak string
	}{
		{"api key header", `API-Key: s3cr3tvalue123`, "s3cr3tvalue123"},
		{"api key json", `{"api_key":"s3cr3tvalue123"}`, "s3cr3tvalue123"},
		{"bearer", `Authorization: Bearer tok3nvalue9876`, "tok3nvalue9876"},
		{"password json", `{"password":"hunter2000"}`, "hunter2000"},
		{"token query", `https://api.example.com/x?token=qwertyuiop123`, "qwertyuiop123"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := r.Redact(c.in)
			if strings.Contains(got, c.leak) {
				t.Fatalf("pattern failed to redact %q: %q", c.leak, got)
			}
		})
	}
}

func TestRedactAddressesOnByDefault(t *testing.T) {
	r := NewRedactor()
	r.RegisterAddress("mymedia.freeddns.org")
	r.RegisterAddress("203.0.113.5")

	got := r.Redact("resolved mymedia.freeddns.org to 203.0.113.5")
	if strings.Contains(got, "mymedia.freeddns.org") || strings.Contains(got, "203.0.113.5") {
		t.Fatalf("address not redacted by default: %q", got)
	}
}

func TestRedactAddressesCanBeIncluded(t *testing.T) {
	// The "include my address" toggle on the diagnostic bundle.
	r := NewRedactor()
	r.RegisterAddress("mymedia.freeddns.org")
	r.SetRedactAddresses(false)

	got := r.Redact("resolved mymedia.freeddns.org")
	if !strings.Contains(got, "mymedia.freeddns.org") {
		t.Fatalf("address should be included when toggle is off: %q", got)
	}
}

func TestAddressToggleNeverExposesRealSecrets(t *testing.T) {
	// Turning address redaction off must not weaken secret redaction.
	r := NewRedactor()
	const key = "dynu-api-key-abcdef123456"
	r.RegisterSecret(key)
	r.SetRedactAddresses(false)

	if got := r.Redact("key " + key); strings.Contains(got, key) {
		t.Fatalf("secret leaked when address redaction disabled: %q", got)
	}
}

func TestRedactEmptyInput(t *testing.T) {
	r := NewRedactor()
	r.RegisterSecret("dynu-api-key-abcdef123456")
	if got := r.Redact(""); got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

func TestRegisterSecretIsIdempotent(t *testing.T) {
	r := NewRedactor()
	const key = "dynu-api-key-abcdef123456"
	r.RegisterSecret(key)
	r.RegisterSecret(key)

	r.mu.RLock()
	n := len(r.secrets)
	r.mu.RUnlock()
	if n != 1 {
		t.Fatalf("expected 1 stored secret, got %d", n)
	}
}
