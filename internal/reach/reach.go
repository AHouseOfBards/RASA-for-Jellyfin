// Package reach answers the one question that cannot be answered from inside
// the network: is this port reachable from the internet?
//
// SPEC.md §19 calls this "the hard one", and it deserves care because the
// obvious implementation lies. Dialling your own public address from behind
// your own router exercises NAT hairpinning, which many consumer routers do
// not support — so a failed self-check means "could not confirm", not "not
// reachable". Reporting that as failure would send users to fix a working
// setup.
//
// Hence three outcomes rather than a boolean. Inconclusive is a real answer
// and the UI must be able to say so honestly.
package reach

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
)

// Status is the outcome of a reachability check.
type Status int

const (
	// Unknown means no check ran.
	Unknown Status = iota
	// Reachable means traffic from outside arrived. This is only ever
	// reported on positive proof — our own token came back.
	Reachable
	// Unreachable means something answered that was not us, or the path is
	// demonstrably blocked.
	Unreachable
	// Inconclusive means the check could not decide. The most common cause is
	// a router that does not hairpin, which says nothing about whether real
	// outside traffic would arrive.
	Inconclusive
)

func (s Status) String() string {
	switch s {
	case Reachable:
		return "reachable"
	case Unreachable:
		return "unreachable"
	case Inconclusive:
		return "inconclusive"
	}
	return "unknown"
}

// Result is a reachability answer.
type Result struct {
	Status Status
	// Method names how the answer was reached, for the log.
	Method string
	// Detail is technical context. Never shown to a user.
	Detail string
}

// UserMessage renders the result in plain language.
//
// The inconclusive wording matters most: it must not imply failure, because
// the usual cause is a router limitation rather than a broken setup.
func (r Result) UserMessage() string {
	switch r.Status {
	case Reachable:
		return "Your server can be reached from outside your network."
	case Unreachable:
		return "Your server could not be reached from outside your network yet."
	case Inconclusive:
		return "This network can't test itself from the outside, so this step couldn't be confirmed here. " +
			"Try opening your address on a phone using mobile data."
	}
	return "Not checked."
}

// OK reports whether the result is good enough to proceed. Inconclusive counts
// as proceed: blocking setup on a test that cannot run would strand every user
// whose router lacks hairpinning.
func (r Result) OK() bool { return r.Status == Reachable || r.Status == Inconclusive }

// Prober checks whether a port accepts traffic from the internet.
type Prober struct {
	// PublicAddr is the address observed by the outside world.
	PublicAddr netip.Addr
	Timeout    time.Duration
	Log        *logging.Logger

	client *http.Client
}

// New returns a Prober.
func New(publicAddr netip.Addr, log *logging.Logger) *Prober {
	if log == nil {
		log = logging.Discard()
	}
	return &Prober{PublicAddr: publicAddr, Timeout: 8 * time.Second, Log: log}
}

// SelfCheck binds the port, serves a random token, and tries to fetch that
// token back via the public address.
//
// This is the "Test again" button behind the port-forwarding instructions. It
// must be run before Caddy owns the port, since it needs to bind.
//
// The token is what makes a positive answer trustworthy. Merely connecting
// proves nothing: an ISP filter, a captive portal, or the router's own admin
// page can accept a connection on 443. Only our own token coming back proves
// the path reaches this machine.
func (p *Prober) SelfCheck(ctx context.Context, port int) Result {
	if p.Timeout == 0 {
		p.Timeout = 8 * time.Second
	}
	if !p.PublicAddr.IsValid() {
		return Result{Status: Inconclusive, Method: "self-check",
			Detail: "no public address known"}
	}

	token, err := newToken()
	if err != nil {
		return Result{Status: Inconclusive, Method: "self-check", Detail: err.Error()}
	}
	path := "/" + token

	// Exclusive ownership must be confirmed on loopback as well as the
	// wildcard address. On Windows a wildcard bind succeeds alongside a
	// process already listening on 127.0.0.1, and the token fetch below —
	// which dials loopback when the public address hairpins — would then reach
	// *their* server and be scored as a wrong responder. The same quirk bit
	// probe.portFree.
	if probe, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port)); err != nil {
		return Result{Status: Inconclusive, Method: "self-check",
			Detail: fmt.Sprintf("could not bind port %d: %v", port, err)}
	} else {
		_ = probe.Close()
	}

	ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		// Something already holds the port. That is a real finding, but not a
		// reachability answer.
		return Result{Status: Inconclusive, Method: "self-check",
			Detail: fmt.Sprintf("could not bind port %d: %v", port, err)}
	}

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == path {
				io.WriteString(w, token)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	return p.fetchToken(ctx, p.PublicAddr, port, path, token)
}

func (p *Prober) fetchToken(ctx context.Context, addr netip.Addr, port int, path, token string) Result {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()

	if p.client == nil {
		p.client = &http.Client{Timeout: p.Timeout}
	}

	url := "http://" + net.JoinHostPort(addr.String(), strconv.Itoa(port)) + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Result{Status: Inconclusive, Method: "self-check", Detail: err.Error()}
	}

	start := time.Now()
	resp, err := p.client.Do(req)
	dur := time.Since(start)

	if err != nil {
		// This is the case that must not be reported as failure. A router
		// without hairpinning refuses or drops the connection regardless of
		// whether outside traffic would arrive.
		p.Log.Debug("self-check could not connect",
			slog.Duration("duration", dur), slog.Any("err", err))
		return Result{
			Status: Inconclusive,
			Method: "self-check",
			Detail: "could not connect to own public address (router may not support hairpin NAT): " + err.Error(),
		}
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	got := strings.TrimSpace(string(body))

	switch {
	case resp.StatusCode == http.StatusOK && got == token:
		p.Log.Debug("self-check succeeded", slog.Duration("duration", dur))
		return Result{Status: Reachable, Method: "self-check"}
	case got != "" || resp.StatusCode != http.StatusOK:
		// Something answered, but it was not us. An ISP filter page, a captive
		// portal, or the router's own web interface all look like this — and
		// all of them mean traffic is not arriving here.
		return Result{
			Status: Unreachable,
			Method: "self-check",
			Detail: fmt.Sprintf("something else answered on port %d (status %d)", port, resp.StatusCode),
		}
	default:
		return Result{Status: Inconclusive, Method: "self-check",
			Detail: "empty response"}
	}
}

// CheckURL fetches a URL that should be served by the finished setup and
// confirms it answers.
//
// This is Phase 7 verification: once Caddy is serving, the proof is fetching
// Jellyfin's public endpoint over the real public path. Unlike SelfCheck it
// binds nothing, so it works with the service already running — but it is
// still subject to hairpinning, so a connection failure is inconclusive.
func (p *Prober) CheckURL(ctx context.Context, url string, mustContain string) Result {
	if p.Timeout == 0 {
		p.Timeout = 8 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()

	if p.client == nil {
		p.client = &http.Client{Timeout: p.Timeout}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Result{Status: Inconclusive, Method: "url-check", Detail: err.Error()}
	}

	resp, err := p.client.Do(req)
	if err != nil {
		// A rejected certificate proves the path works — the opposite
		// conclusion from a timeout — so it is a verdict, not an inconclusive.
		var ce *tls.CertificateVerificationError
		if errors.As(err, &ce) {
			return Result{Status: Unreachable, Method: "url-check",
				Detail: "certificate not accepted: " + err.Error()}
		}
		return Result{Status: Inconclusive, Method: "url-check",
			Detail: "could not connect (router may not support hairpin NAT): " + err.Error()}
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return Result{Status: Unreachable, Method: "url-check",
			Detail: fmt.Sprintf("status %d", resp.StatusCode)}
	}
	if mustContain != "" && !strings.Contains(string(body), mustContain) {
		return Result{Status: Unreachable, Method: "url-check",
			Detail: "response did not look like the expected server"}
	}
	return Result{Status: Reachable, Method: "url-check"}
}

func newToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("could not generate a verification token")
	}
	return hex.EncodeToString(b[:]), nil
}
