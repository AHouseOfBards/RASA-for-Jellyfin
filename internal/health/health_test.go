package health

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ok(name string) Check  { return Check{Name: name, OK: true, Detail: "fine"} }
func bad(name string) Check { return Check{Name: name, Detail: "broken", Advice: "do the thing"} }
func now() time.Time        { return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC) }
func report(cs ...Check) Report {
	return Report{Checked: now(), Hostname: "mymedia.freeddns.org", Checks: cs}
}

func TestOneFailedCheckMakesTheWholeReportUnhealthy(t *testing.T) {
	r := report(ok("address"), bad("proxy"))
	if r.Healthy() {
		t.Error("a report with a failing check is not healthy")
	}
	if len(r.Problems()) != 1 {
		t.Errorf("Problems() = %v, want just the proxy", r.Problems())
	}
}

// The headline is the only line most people read, so the difference between
// working and broken has to be visible without reading a word of the rest.
func TestTheHeadlineSaysWhichItIsWithoutReadingFurther(t *testing.T) {
	if h := report(ok("a")).Headline(); !strings.Contains(h, "working") {
		t.Errorf("healthy headline = %q", h)
	}
	h := report(bad("a")).Headline()
	if !strings.Contains(h, "ACTION NEEDED") {
		t.Errorf("unhealthy headline = %q", h)
	}
	if strings.Contains(h, "working") {
		t.Errorf("the broken headline contains the word working: %q", h)
	}
}

// The whole point of the file is that it is read by someone who has forgotten
// what RASA was, months after it was uninstalled.
func TestTheFileNamesTheProblemAndTheFix(t *testing.T) {
	r := report(
		ok("Your address points at this connection"),
		Check{
			Name:   "The secure connection is running",
			Detail: "The security certificate expired on 1 August 2026.",
			Advice: "Browsers and Jellyfin apps will refuse to connect until it is renewed.",
		},
	)
	txt := r.Text()
	for _, want := range []string{
		"ACTION NEEDED",
		"expired on 1 August 2026",
		"refuse to connect",
		"Re-running the RASA setup app",
		"mymedia.freeddns.org",
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("the file does not contain %q:\n%s", want, txt)
		}
	}
}

// A healthy file has to say when it was written, because a stale date is the
// only evidence that the checker itself has died.
func TestAHealthyFileStillSaysWhenItWasChecked(t *testing.T) {
	txt := report(ok("a")).Text()
	if !strings.Contains(txt, "2026-09-03") {
		t.Errorf("no check date in a healthy report:\n%s", txt)
	}
	if !strings.Contains(txt, "ten minutes") {
		t.Errorf("the file does not say how often it is rewritten:\n%s", txt)
	}
}

func TestWriteProducesAReadableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "last-sync.txt")
	if err := Write(path, report(ok("a"))); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "STATUS") {
		t.Errorf("unexpected content:\n%s", b)
	}
}

// ---- checks -------------------------------------------------------------

func TestAFailedSyncIsReportedWithItsConsequence(t *testing.T) {
	c := CheckAddress("mymedia.freeddns.org", errors.New("looking up hostname: 401"))
	if c.OK {
		t.Fatal("a failed sync must not report OK")
	}
	// Naming the consequence is what turns this from noise into something
	// worth acting on before the address actually moves.
	if !strings.Contains(c.Advice, "stop working") {
		t.Errorf("advice does not say what happens next: %q", c.Advice)
	}
	if !strings.Contains(c.Advice, "Dynu") {
		t.Errorf("advice does not name the likely cause: %q", c.Advice)
	}
}

// testCert builds a certificate with a chosen lifetime.
func testCert(t *testing.T, host string, notAfter time.Time) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    notAfter.Add(-90 * 24 * time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

const testHost = "mymedia.freeddns.org"

func TestAHealthyCertificatePasses(t *testing.T) {
	cert := testCert(t, testHost, now().Add(60*24*time.Hour))
	c, expiry := certCheck(Check{}, cert, testHost, 443, now())
	if !c.OK {
		t.Errorf("a certificate with 60 days left should pass: %+v", c)
	}
	if !expiry.Equal(cert.NotAfter) {
		t.Errorf("expiry = %v, want %v", expiry, cert.NotAfter)
	}
}

// This is the failure the whole package exists for: renewal has been failing
// silently and the certificate is running out.
func TestACertificateThatIsNotRenewingIsReportedBeforeItExpires(t *testing.T) {
	cert := testCert(t, testHost, now().Add(5*24*time.Hour))
	c, _ := certCheck(Check{}, cert, testHost, 443, now())
	if c.OK {
		t.Fatal("a certificate with five days left must not report OK")
	}
	if !strings.Contains(c.Detail, "5 days") {
		t.Errorf("detail does not say how long is left: %q", c.Detail)
	}
	if !strings.Contains(c.Advice, "Dynu") {
		t.Errorf("advice does not name the likely cause: %q", c.Advice)
	}
}

// Caddy renews at 30 days remaining and keeps trying. Alerting there would
// fire during every normal renewal, which is how an alert gets ignored.
func TestANormalRenewalWindowIsNotAnAlarm(t *testing.T) {
	cert := testCert(t, testHost, now().Add(25*24*time.Hour))
	if c, _ := certCheck(Check{}, cert, testHost, 443, now()); !c.OK {
		t.Errorf("25 days left is a normal renewal, not a fault: %+v", c)
	}
}

func TestAnExpiredCertificateSaysSoPlainly(t *testing.T) {
	cert := testCert(t, testHost, now().Add(-24*time.Hour))
	c, _ := certCheck(Check{}, cert, testHost, 443, now())
	if c.OK {
		t.Fatal("an expired certificate must not report OK")
	}
	if !strings.Contains(c.Detail, "expired") {
		t.Errorf("detail = %q", c.Detail)
	}
}

// A proxy serving the wrong name looks fine from this machine and broken from
// a phone, which is the hardest kind of failure to diagnose remotely.
func TestACertificateForADifferentAddressIsAFailure(t *testing.T) {
	cert := testCert(t, "someone-else.freeddns.org", now().Add(60*24*time.Hour))
	c, _ := certCheck(Check{}, cert, testHost, 443, now())
	if c.OK {
		t.Fatal("a certificate for another address must not report OK")
	}
	if !strings.Contains(c.Detail, "different address") {
		t.Errorf("detail = %q", c.Detail)
	}
}
