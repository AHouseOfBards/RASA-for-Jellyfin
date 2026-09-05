package health

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"math"
	"net"
	"strconv"
	"time"
)

// RenewalWindow is how much life a certificate must have left before RASA
// treats it as a problem.
//
// Caddy starts renewing at 30 days remaining and keeps trying. Below 14 days
// it has had a fortnight of attempts and is still failing, which is no longer
// something that will sort itself out — it is a revoked credential, a DNS
// account that has been emptied, or a hostname that has moved. Alerting at 30
// would fire during every perfectly normal renewal.
const RenewalWindow = 14 * 24 * time.Hour

// CheckAddress turns the address syncer's result into a user-facing check.
//
// syncErr is whatever the sync run returned; nil means it worked.
func CheckAddress(syncErr error) Check {
	c := Check{Name: "Your address points at this connection"}
	if syncErr == nil {
		c.OK = true
		c.Detail = "Checked with Dynu and it is up to date."
		return c
	}
	c.Detail = "RASA could not confirm the address with Dynu."
	c.Advice = "If this keeps failing, remote access will stop working the next " +
		"time your internet address changes. The usual cause is that the Dynu " +
		"API key was changed or removed."
	return c
}

// CheckProxy connects to the local proxy and reads the certificate it is
// serving.
//
// This is deliberately the served certificate rather than the expiry recorded
// at setup. The recorded value answers "what was true months ago"; the served
// one answers the question actually being asked, and it proves three things at
// once — the proxy is running, it is listening on the expected port, and
// renewal is keeping up.
//
// The connection is made to loopback rather than to the public address on
// purpose. Many routers will not let a machine reach its own public address
// (SPEC.md §11 covers the same hairpin problem during setup), so a public
// dial would report a healthy server as broken.
func CheckProxy(ctx context.Context, hostname string, port int) (Check, time.Time) {
	c := Check{Name: "The secure connection is running"}
	if port == 0 {
		port = 443
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	// Bounded, and cancellable: this runs inside a scheduled task that is
	// expected to finish and exit.
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	d := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 10 * time.Second},
		Config: &tls.Config{
			ServerName: hostname,
			// The certificate is inspected rather than trusted. Verification
			// would reject an expired one, and an expired certificate is
			// precisely the condition this check exists to report.
			InsecureSkipVerify: true,
		},
	}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		c.Detail = fmt.Sprintf("Nothing answered on port %d of this computer.", port)
		c.Advice = "The proxy service has stopped. Its log will say why."
		return c, time.Time{}
	}
	defer conn.Close()

	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		c.Detail = "The proxy answered but offered no certificate."
		c.Advice = "This usually means it is still starting up. If it persists, read the proxy log."
		return c, time.Time{}
	}
	return certCheck(c, certs[0], hostname, port, time.Now())
}

// certCheck grades a certificate. Split out from the dialling so the grading
// can be tested without a listener.
func certCheck(c Check, cert *x509.Certificate, hostname string, port int, now time.Time) (Check, time.Time) {
	expiry := cert.NotAfter
	switch {
	case now.After(expiry):
		c.Detail = fmt.Sprintf("The security certificate expired on %s.", expiry.Format("2 January 2006"))
		c.Advice = "Browsers and Jellyfin apps will refuse to connect until it is renewed."
		return c, expiry

	case now.Add(RenewalWindow).After(expiry):
		c.Detail = fmt.Sprintf("The security certificate runs out in %d days and has not renewed.",
			daysLeft(expiry, now))
		c.Advice = "Renewal normally happens a month ahead, so this means it is failing. " +
			"The usual cause is that the Dynu API key was changed or removed."
		return c, expiry
	}

	// A certificate for the wrong name is served by a proxy that is running
	// but pointed somewhere else — which looks identical to "working" from
	// here and identical to "broken" from a phone.
	if hostname != "" && cert.VerifyHostname(hostname) != nil {
		c.Detail = "The proxy is serving a certificate for a different address."
		c.Advice = "Re-run the RASA setup app to reissue it."
		return c, expiry
	}

	c.OK = true
	// The expiry is not repeated here: the report prints it on its own line
	// with the days remaining, which is the form somebody scanning the file
	// is actually looking for.
	c.Detail = fmt.Sprintf("Answering on port %d with a valid certificate.", port)
	return c, expiry
}

// daysLeft counts whole days, rounded rather than truncated.
//
// Shared so that the check and the report cannot disagree. Truncating two
// timestamps taken microseconds apart is enough to print "runs out in 4 days"
// directly above "(3 days)", which reads as a bug in something a user is
// already worried about.
func daysLeft(until, from time.Time) int {
	return int(math.Round(until.Sub(from).Hours() / 24))
}
