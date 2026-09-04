package health

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// raised records what an Escalator tried to send.
type raised struct {
	Level   AlertLevel
	Subject string
	Body    string
}

type spy struct {
	sent []raised
	err  error
}

func (s *spy) raise(_ context.Context, l AlertLevel, subject, body string) error {
	s.sent = append(s.sent, raised{l, subject, body})
	return s.err
}

func escalator(t *testing.T, s *spy, clock *time.Time) *Escalator {
	t.Helper()
	return &Escalator{
		StatePath: filepath.Join(t.TempDir(), "alert-state.json"),
		Raise:     s.raise,
		Now:       func() time.Time { return *clock },
	}
}

func TestAWorkingSystemNeverSaysAnything(t *testing.T) {
	s := &spy{}
	clock := now()
	e := escalator(t, s, &clock)

	for i := 0; i < 100; i++ {
		if err := e.Consider(context.Background(), report(ok("address"), ok("proxy"))); err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(10 * time.Minute)
	}
	if len(s.sent) != 0 {
		t.Errorf("a healthy machine raised %d alerts, want none: %+v", len(s.sent), s.sent)
	}
}

// The fix for silence must not be noise. Every ten minutes forever is fifty
// thousand entries a year in a log the user cannot roll — the same failure
// that made sync.log useless, moved somewhere worse.
func TestAFaultIsReportedOnceNotEveryTenMinutes(t *testing.T) {
	s := &spy{}
	clock := now()
	e := escalator(t, s, &clock)

	// Six hours of failing every ten minutes.
	for i := 0; i < 36; i++ {
		if err := e.Consider(context.Background(), report(ok("address"), bad("proxy"))); err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(10 * time.Minute)
	}
	if len(s.sent) != 1 {
		t.Fatalf("raised %d alerts in six hours, want 1", len(s.sent))
	}
	if s.sent[0].Level != LevelError {
		t.Errorf("level = %v, want an error", s.sent[0].Level)
	}
	if !strings.Contains(s.sent[0].Subject, "stopped working") {
		t.Errorf("subject = %q", s.sent[0].Subject)
	}
}

// Quiet is not the same as forgotten. A fault nobody has fixed should speak up
// again the next day, so it is not lost among a week of other entries.
func TestALingeringFaultIsRepeatedDaily(t *testing.T) {
	s := &spy{}
	clock := now()
	e := escalator(t, s, &clock)

	for i := 0; i < 3*24*6; i++ { // three days at ten-minute intervals
		if err := e.Consider(context.Background(), report(bad("proxy"))); err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(10 * time.Minute)
	}
	if len(s.sent) != 3 {
		t.Errorf("raised %d alerts over three days, want 3", len(s.sent))
	}
	if len(s.sent) > 1 && !strings.Contains(s.sent[1].Subject, "still not working") {
		t.Errorf("the repeat does not read as a repeat: %q", s.sent[1].Subject)
	}
}

// A second, different fault is news even though something was already broken.
func TestANewFaultIsHeardImmediately(t *testing.T) {
	s := &spy{}
	clock := now()
	e := escalator(t, s, &clock)
	ctx := context.Background()

	if err := e.Consider(ctx, report(bad("proxy"))); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(10 * time.Minute)
	if err := e.Consider(ctx, report(bad("proxy"), bad("address"))); err != nil {
		t.Fatal(err)
	}
	if len(s.sent) != 2 {
		t.Errorf("raised %d alerts, want 2: a new fault is not the old one", len(s.sent))
	}
}

// Someone who was told their remote access broke is owed the other half of the
// message, or they spend an evening chasing a problem that has already gone.
func TestARecoveryIsAnnouncedButOnlyAfterAFault(t *testing.T) {
	s := &spy{}
	clock := now()
	e := escalator(t, s, &clock)
	ctx := context.Background()

	if err := e.Consider(ctx, report(bad("proxy"))); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(10 * time.Minute)
	if err := e.Consider(ctx, report(ok("proxy"))); err != nil {
		t.Fatal(err)
	}
	if len(s.sent) != 2 {
		t.Fatalf("raised %d alerts, want a fault and a recovery", len(s.sent))
	}
	if s.sent[1].Level != LevelInfo {
		t.Errorf("recovery level = %v, want info", s.sent[1].Level)
	}
	if !strings.Contains(s.sent[1].Subject, "working again") {
		t.Errorf("recovery subject = %q", s.sent[1].Subject)
	}

	// And it says so once, not on every healthy run afterwards.
	for i := 0; i < 20; i++ {
		clock = clock.Add(10 * time.Minute)
		if err := e.Consider(ctx, report(ok("proxy"))); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.sent) != 2 {
		t.Errorf("raised %d alerts, want the recovery announced once", len(s.sent))
	}
}

// A damaged state file must not be able to silence a real fault. Losing the
// record should at worst repeat an alert.
func TestADamagedStateFileDoesNotSwallowAFault(t *testing.T) {
	s := &spy{}
	clock := now()
	e := escalator(t, s, &clock)
	ctx := context.Background()

	if err := e.Consider(ctx, report(bad("proxy"))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.StatePath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(10 * time.Minute)
	if err := e.Consider(ctx, report(bad("proxy"))); err != nil {
		t.Fatal(err)
	}
	if len(s.sent) != 2 {
		t.Errorf("raised %d alerts, want the fault reported again after the record was lost", len(s.sent))
	}
}

// ---- the live proxy check ------------------------------------------------

// tlsListener starts a real TLS listener serving cert, and returns its port.
func tlsListener(t *testing.T, host string, notAfter time.Time) int {
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

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// The handshake has to complete for the client to see the
			// certificate, and reading is what drives it on the server side.
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				var b [1]byte
				_, _ = conn.Read(b[:])
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// The check has to work against a listener that is actually running, not only
// against a parsed certificate — this is the part that proves the proxy is up.
func TestCheckProxyReadsTheCertificateFromALiveListener(t *testing.T) {
	port := tlsListener(t, testHost, time.Now().Add(60*24*time.Hour))

	c, expiry := CheckProxy(context.Background(), testHost, port)
	if !c.OK {
		t.Fatalf("a running proxy with a valid certificate should pass: %+v", c)
	}
	if expiry.IsZero() {
		t.Error("no expiry was read from the served certificate")
	}
}

func TestCheckProxyReportsAProxyThatIsNotRunning(t *testing.T) {
	// Bound and closed, so the port is real and certainly refuses.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	c, expiry := CheckProxy(context.Background(), testHost, port)
	if c.OK {
		t.Fatal("a dead proxy must not report OK")
	}
	if !strings.Contains(c.Detail, "Nothing answered") {
		t.Errorf("detail = %q", c.Detail)
	}
	if !expiry.IsZero() {
		t.Errorf("expiry = %v, want zero when nothing could be read", expiry)
	}
}

// An expired certificate must still be readable. Verifying the chain would
// reject it and report "nothing is listening", which sends the user to the
// wrong log entirely.
func TestCheckProxyStillReadsAnExpiredCertificate(t *testing.T) {
	port := tlsListener(t, testHost, time.Now().Add(-24*time.Hour))

	c, expiry := CheckProxy(context.Background(), testHost, port)
	if c.OK {
		t.Fatal("an expired certificate must not report OK")
	}
	if !strings.Contains(c.Detail, "expired") {
		t.Errorf("detail = %q, want it to name the expiry rather than the connection", c.Detail)
	}
	if expiry.IsZero() {
		t.Error("the expiry date was not read back")
	}
}
