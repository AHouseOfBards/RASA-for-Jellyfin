package wizard

import (
	"context"
	"strings"
	"testing"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/dynu"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/rasaerr"
)

// quotaRefusal is what Dynu sends once the account's free allowance is gone.
func quotaRefusal() error {
	return &dynu.APIError{
		StatusCode:   dynu.StatusQuota,
		Type:         "QuotaException",
		Message:      "Quota exception.",
		FromEnvelope: true,
		Method:       "POST",
		Path:         "/dns",
	}
}

func atTheNameScreen(t *testing.T, h *harness) context.Context {
	t.Helper()
	ctx := context.Background()
	if err := h.w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.w.SignIn(ctx, "admin", "pw"); err != nil {
		t.Fatal(err)
	}
	if err := h.w.SetDynuKey(ctx, testKey); err != nil {
		t.Fatal(err)
	}
	return ctx
}

// A full account and a taken name both refuse the hostname, and only one of
// them is fixed by choosing a different one. Offering suggestions to somebody
// whose account is full walks them into the same wall four more times.
func TestAFullAccountIsNotReportedAsATakenName(t *testing.T) {
	h := newHarness(t, nil)
	h.dynu.createFn = func(dynu.CreateDomainRequest) (*dynu.Domain, error) {
		return nil, quotaRefusal()
	}

	err := h.w.ClaimName(atTheNameScreen(t, h), "media", "freeddns.org")
	re, ok := rasaerr.As(err)
	if !ok {
		t.Fatalf("got %v, want a typed RASA error", err)
	}
	if re.Code != rasaerr.CodeDynuQuotaExhausted {
		t.Fatalf("code = %s, want %s", re.Code, rasaerr.CodeDynuQuotaExhausted)
	}
	if m := h.w.Model(); m.Screen != ScreenName {
		t.Errorf("screen = %s, want to stay on the name screen", m.Screen)
	}
}

// The message has to say what to delete. "Your account is full" with no list
// leaves a user who set this up months ago with nothing to act on.
func TestTheQuotaMessageNamesWhatIsAlreadyOnTheAccount(t *testing.T) {
	h := newHarness(t, nil)
	h.dynu.domains = []dynu.Domain{
		{ID: 3, Name: "rasatest5.freeddns.org"},
		{ID: 1, Name: "thebardscampfire.webredirect.org"},
		{ID: 2, Name: "rasatest2.freeddns.org"},
	}
	h.dynu.createFn = func(dynu.CreateDomainRequest) (*dynu.Domain, error) {
		return nil, quotaRefusal()
	}

	err := h.w.ClaimName(atTheNameScreen(t, h), "media", "freeddns.org")
	re, _ := rasaerr.As(err)
	uf := re.User()
	for _, want := range []string{
		"rasatest2.freeddns.org",
		"rasatest5.freeddns.org",
		"thebardscampfire.webredirect.org",
	} {
		if !strings.Contains(uf.Why, want) {
			t.Errorf("the message does not mention %s:\n%s", want, uf.Why)
		}
	}
	// Sorted, because the list is read rather than scanned, and API order is
	// whatever Dynu felt like.
	if i, j := strings.Index(uf.Why, "rasatest2"), strings.Index(uf.Why, "rasatest5"); i > j {
		t.Errorf("names are not in a stable order:\n%s", uf.Why)
	}
	if !strings.Contains(uf.Why, "dynu.com") {
		t.Errorf("the message does not say where to delete one:\n%s", uf.Why)
	}
}

// The listing has just failed once already. A second failure must not replace
// a precise message with a vague one.
func TestTheQuotaMessageSurvivesTheListingAlsoFailing(t *testing.T) {
	h := newHarness(t, nil)
	h.dynu.createFn = func(dynu.CreateDomainRequest) (*dynu.Domain, error) {
		return nil, quotaRefusal()
	}

	ctx := atTheNameScreen(t, h)
	// Broken only after the key check has passed, so setup gets far enough to
	// try the claim at all.
	h.dynu.listErr = quotaRefusal()

	err := h.w.ClaimName(ctx, "media", "freeddns.org")
	re, ok := rasaerr.As(err)
	if !ok || re.Code != rasaerr.CodeDynuQuotaExhausted {
		t.Fatalf("got %v, want the quota error", err)
	}
	if uf := re.User(); !strings.Contains(uf.Why, "dynu.com") {
		t.Errorf("the fallback message does not say what to do:\n%s", uf.Why)
	}
}

// Detail is the log projection. The names are the user's own addresses and
// there is no reason for them to be in a file destined for a bug report.
func TestTheQuotaLogLineDoesNotCarryTheHostnames(t *testing.T) {
	names := []string{"secretname.freeddns.org", "another.freeddns.org"}
	e := rasaerr.DynuQuotaExhausted(names, quotaRefusal())
	for _, n := range names {
		if strings.Contains(e.Error(), n) {
			t.Errorf("%s appears in the technical detail:\n%s", n, e.Error())
		}
	}
}
