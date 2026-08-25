// Package ui serves the wizard as a local web application.
//
// SPEC.md decision 4 calls for a GUI, and the "Why Go" note reaches for a
// webview wrapper. This is the half of that which does not depend on one: the
// flow is rendered as HTML served from loopback, so the same code works
// wrapped in a native webview, opened in the user's browser, or driven by a
// test with an HTTP client. Nothing here knows which.
//
// # Why this is not simply "a web server"
//
// It listens on loopback with an ephemeral port, and every request carries a
// per-run token. Without that, any page in any browser tab could post to
// http://127.0.0.1:PORT and drive an administrator-privileged installer.
// Browsers will not attach a custom header cross-origin without a preflight
// this server refuses, so the token cannot be forged by a page that has not
// been served it.
package ui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/rasaerr"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/wizard"
)

//go:embed assets/*
var assets embed.FS

// HeaderToken is the per-run credential every API request must carry.
const HeaderToken = "X-RASA-Token"

// Server hosts the wizard.
type Server struct {
	w     *wizard.Wizard
	log   *logging.Logger
	token string

	mux *http.ServeMux
	srv *http.Server
	ln  net.Listener

	// done is closed when the user finishes or quits, so main can wait on it.
	done     chan struct{}
	doneOnce sync.Once

	// visited records that the wizard page was actually served at least once.
	//
	// Whether a browser appeared is the one thing the launcher cannot report:
	// it hands the URL to the shell and returns success immediately. This is
	// the only evidence that anybody is looking at the wizard.
	visited atomic.Bool
}

// New builds a server. It does not listen until Start.
func New(w *wizard.Wizard, log *logging.Logger) (*Server, error) {
	if w == nil {
		return nil, errors.New("ui needs a wizard")
	}
	if log == nil {
		log = logging.Discard()
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generating a session token: %w", err)
	}

	s := &Server{
		w:     w,
		log:   log,
		token: hex.EncodeToString(raw),
		mux:   http.NewServeMux(),
		done:  make(chan struct{}),
	}
	s.routes()
	// The token is a secret for the lifetime of the run: it appears in the URL
	// that launches the browser, which means it can appear in a log line if
	// nothing stops it.
	log.Redactor().RegisterSecret(s.token)
	return s, nil
}

// Start binds a loopback listener and begins serving.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listening on loopback: %w", err)
	}
	s.ln = ln
	s.srv = &http.Server{
		Handler: s.mux,
		// Generous, because the event stream is a long-lived response and the
		// setup pipeline holds requests open for minutes.
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("ui server stopped", slog.Any("err", err))
		}
	}()
	s.log.Info("wizard interface listening", slog.String("addr", ln.Addr().String()))
	return nil
}

// URL is the address to open, token included.
func (s *Server) URL() string {
	if s.ln == nil {
		return ""
	}
	return fmt.Sprintf("http://%s/?t=%s", s.ln.Addr().String(), s.token)
}

// Done is closed when the user finishes or closes the wizard.
func (s *Server) Done() <-chan struct{} { return s.done }

// Visited reports whether the wizard page has been served to a browser.
func (s *Server) Visited() bool { return s.visited.Load() }

// Close stops serving.
func (s *Server) Close(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	// Release the event streams first.
	//
	// Shutdown waits for connections to become idle, and an SSE stream never
	// does: it is an open response by design. With a browser attached — which
	// is always — Shutdown therefore burned its entire timeout before giving
	// up, so clicking Finish left the console sitting there for a measured
	// 3.2 seconds against 0.2 with no stream open. finish is idempotent, so
	// this is safe whether the user quit or the run was interrupted.
	s.finish()
	return s.srv.Shutdown(ctx)
}

func (s *Server) finish() {
	s.doneOnce.Do(func() { close(s.done) })
}

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.Handle("/assets/", http.FileServer(http.FS(assets)))

	s.mux.HandleFunc("/api/state", s.guard(s.handleState))
	s.mux.HandleFunc("/api/events", s.guard(s.handleEvents))
	s.mux.HandleFunc("/api/start", s.guard(s.async("start", func(ctx context.Context, body request) error {
		return s.w.Start(ctx)
	})))
	s.mux.HandleFunc("/api/jellyfin/signin", s.guard(s.async("signin", func(ctx context.Context, body request) error {
		if body.APIKey != "" {
			return s.w.UseAPIKey(ctx, body.APIKey)
		}
		return s.w.SignIn(ctx, body.Username, body.Password)
	})))
	s.mux.HandleFunc("/api/dynu/key", s.guard(s.async("dynu", func(ctx context.Context, body request) error {
		return s.w.SetDynuKey(ctx, body.Key)
	})))
	s.mux.HandleFunc("/api/back", s.guard(s.async("back", func(ctx context.Context, body request) error {
		return s.w.Back(ctx)
	})))
	s.mux.HandleFunc("/api/dynu/check", s.guard(s.handleDynuCheck))
	s.mux.HandleFunc("/api/name/check", s.guard(s.handleNameCheck))
	s.mux.HandleFunc("/api/name", s.guard(s.async("name", func(ctx context.Context, body request) error {
		return s.w.ClaimName(ctx, body.Label, body.Parent)
	})))
	s.mux.HandleFunc("/api/port/open", s.guard(s.async("port", func(ctx context.Context, body request) error {
		return s.w.OpenPort(ctx)
	})))
	s.mux.HandleFunc("/api/port/skip", s.guard(s.async("port", func(ctx context.Context, body request) error {
		return s.w.SkipPort(ctx)
	})))
	s.mux.HandleFunc("/api/install", s.guard(s.async("install", func(ctx context.Context, body request) error {
		return s.w.Install(ctx)
	})))
	s.mux.HandleFunc("/api/remove", s.guard(s.async("remove", func(ctx context.Context, body request) error {
		return s.w.RemoveRemoteAccess(ctx)
	})))
	s.mux.HandleFunc("/api/quit", s.guard(s.handleQuit))
}

// request is every field any endpoint accepts. One shape keeps the client
// simple; unknown fields for a given endpoint are simply unused.
type request struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
	Key      string `json:"key,omitempty"`
	Label    string `json:"label,omitempty"`
	Parent   string `json:"parent,omitempty"`
}

// guard authenticates the request and refuses cross-origin callers.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(wr http.ResponseWriter, r *http.Request) {
		token := r.Header.Get(HeaderToken)
		if token == "" {
			// The event stream is opened by EventSource, which cannot set
			// headers. It carries the token in the query instead, which is
			// safe here because the URL never leaves loopback.
			token = r.URL.Query().Get("t")
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
			http.Error(wr, "not authorised", http.StatusForbidden)
			return
		}
		// A cross-origin page cannot read this response, but refusing outright
		// means it cannot cause the side effect either.
		if origin := r.Header.Get("Origin"); origin != "" && !s.sameOrigin(origin) {
			http.Error(wr, "not authorised", http.StatusForbidden)
			return
		}
		next(wr, r)
	}
}

func (s *Server) sameOrigin(origin string) bool {
	if s.ln == nil {
		return false
	}
	return origin == "http://"+s.ln.Addr().String()
}

func (s *Server) handleIndex(wr http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(wr, r)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("t")), []byte(s.token)) != 1 {
		http.Error(wr, "This page was opened without its one-time key. Close it and start RASA again.", http.StatusForbidden)
		return
	}

	page, err := assets.ReadFile("assets/index.html")
	if err != nil {
		http.Error(wr, "interface unavailable", http.StatusInternalServerError)
		return
	}
	// The token reaches the page here rather than being kept in a cookie: a
	// cookie would be attached to forged cross-site requests automatically,
	// which is the exact thing the header check prevents.
	body := strings.Replace(string(page), "{{TOKEN}}", s.token, 1)

	wr.Header().Set("Content-Type", "text/html; charset=utf-8")
	wr.Header().Set("Cache-Control", "no-store")
	// Nothing on this page loads from anywhere else, and saying so means a
	// compromised dependency cannot appear later without the policy stopping
	// it. There are no dependencies to compromise, which is rather the point.
	wr.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; form-action 'none'; base-uri 'none'")
	wr.Header().Set("Referrer-Policy", "no-referrer")
	wr.Write([]byte(body))

	// Set only after a successful serve with a valid token, so a stray probe
	// on the port does not count as "the user is looking at the wizard".
	s.visited.Store(true)
}

func (s *Server) handleState(wr http.ResponseWriter, r *http.Request) {
	writeJSON(wr, http.StatusOK, s.w.Model())
}

// handleEvents streams model snapshots as server-sent events.
func (s *Server) handleEvents(wr http.ResponseWriter, r *http.Request) {
	flusher, ok := wr.(http.Flusher)
	if !ok {
		http.Error(wr, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	wr.Header().Set("Content-Type", "text/event-stream")
	wr.Header().Set("Cache-Control", "no-store")
	wr.Header().Set("Connection", "keep-alive")
	wr.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, stop := s.w.Subscribe()
	defer stop()

	// A keepalive comment stops proxies and impatient clients from deciding a
	// quiet stretch — the two-minute install — is a dead connection.
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			// The wizard is finished or shutting down. Returning here is what
			// lets the connection go idle so Shutdown can complete promptly
			// rather than waiting out its timeout on a stream that will never
			// end on its own.
			return
		case <-ping.C:
			fmt.Fprint(wr, ": keepalive\n\n")
			flusher.Flush()
		case m, open := <-ch:
			if !open {
				return
			}
			body, err := json.Marshal(m)
			if err != nil {
				s.log.Error("could not encode a model snapshot", slog.Any("err", err))
				continue
			}
			fmt.Fprintf(wr, "data: %s\n\n", body)
			flusher.Flush()
		}
	}
}

// handleDynuCheck validates a key without committing to it.
//
// POST with the key in the body rather than GET with it in the query, which is
// what the name check does: a query string is the one part of a request that
// routinely gets written down — history, logs, referrers — and this one is a
// credential.
func (s *Server) handleDynuCheck(wr http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(wr, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body request
	if r.ContentLength != 0 {
		dec := json.NewDecoder(http.MaxBytesReader(wr, r.Body, 1<<16))
		if err := dec.Decode(&body); err != nil {
			writeJSON(wr, http.StatusBadRequest, map[string]string{"error": "malformed request"})
			return
		}
	}
	writeJSON(wr, http.StatusOK, s.w.CheckDynuKey(r.Context(), body.Key))
}

func (s *Server) handleNameCheck(wr http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	view := s.w.CheckName(r.Context(), q.Get("label"), q.Get("parent"))
	writeJSON(wr, http.StatusOK, view)
}

func (s *Server) handleQuit(wr http.ResponseWriter, r *http.Request) {
	writeJSON(wr, http.StatusOK, map[string]string{"status": "closing"})
	s.finish()
}

// async runs a wizard operation in the background and answers immediately.
//
// The operations here take minutes. Holding the request open for one would
// make the interface look frozen and would tie progress to a connection that
// a browser is free to drop; the client watches the event stream instead, which
// is also what makes a reconnecting page pick up mid-install.
func (s *Server) async(phase string, fn func(context.Context, request) error) http.HandlerFunc {
	return func(wr http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(wr, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body request
		if r.ContentLength != 0 {
			// Bounded: this is a local interface, but an unbounded decode is
			// an unbounded decode.
			dec := json.NewDecoder(http.MaxBytesReader(wr, r.Body, 1<<16))
			if err := dec.Decode(&body); err != nil {
				writeJSON(wr, http.StatusBadRequest, map[string]string{"error": "malformed request"})
				return
			}
		}

		// Detached from the request context on purpose: a browser that
		// navigates away or a tab that is closed must not cancel an install
		// halfway through registering services.
		ctx := context.WithoutCancel(r.Context())
		go func() {
			if err := fn(ctx, body); err != nil && !errors.Is(err, wizard.ErrBusy) {
				// The failure is already on the model; this is the log copy,
				// which is the only place technical detail is allowed.
				s.log.WithPhase(phase).Debug("operation failed", slog.String("detail", err.Error()))
			}
		}()
		writeJSON(wr, http.StatusAccepted, map[string]string{"status": "running"})
	}
}

func writeJSON(wr http.ResponseWriter, code int, v any) {
	wr.Header().Set("Content-Type", "application/json")
	wr.Header().Set("Cache-Control", "no-store")
	wr.WriteHeader(code)
	if err := json.NewEncoder(wr).Encode(v); err != nil {
		return
	}
}

// UserError renders an error for a client that asked for one synchronously.
// Only the safe projection crosses the wire.
func UserError(err error) rasaerr.UserFacing {
	if re, ok := rasaerr.As(err); ok {
		return re.User()
	}
	return rasaerr.UserFacing{
		Code:    rasaerr.CodeUnexpected,
		Message: "Something went wrong.",
	}
}
