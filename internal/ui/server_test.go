package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/paths"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/state"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/wizard"
)

func newServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	layout := paths.UnderRoot(dir)
	if err := layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	w, err := wizard.New(wizard.Options{
		Layout:  layout,
		Log:     logging.Discard(),
		Store:   state.NewStore(layout.StateFile()),
		Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(w, logging.Discard())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.Close(ctx)
	})
	return s
}

func (s *Server) base() string { return "http://" + s.ln.Addr().String() }

// Any page in any browser tab can reach loopback. Without the token, one of
// them could drive an administrator-privileged installer.
func TestAPIRefusesRequestsWithoutTheToken(t *testing.T) {
	s := newServer(t)

	for _, path := range []string{"/api/state", "/api/start", "/api/install", "/api/events"} {
		res, err := http.Get(s.base() + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%s without a token returned %d, want 403", path, res.StatusCode)
		}
	}
}

func TestAPIRefusesTheWrongToken(t *testing.T) {
	s := newServer(t)
	req, _ := http.NewRequest("GET", s.base()+"/api/state", nil)
	req.Header.Set(HeaderToken, strings.Repeat("a", 64))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("wrong token returned %d, want 403", res.StatusCode)
	}
}

// A cross-origin page cannot read the response, but it must not be able to
// cause the side effect either.
func TestAPIRefusesForeignOrigins(t *testing.T) {
	s := newServer(t)
	req, _ := http.NewRequest("POST", s.base()+"/api/start", nil)
	req.Header.Set(HeaderToken, s.token)
	req.Header.Set("Origin", "https://evil.example")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("foreign origin returned %d, want 403", res.StatusCode)
	}
}

func TestIndexNeedsTheToken(t *testing.T) {
	s := newServer(t)

	res, err := http.Get(s.base() + "/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("index without a token returned %d, want 403", res.StatusCode)
	}

	res, err = http.Get(s.URL())
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("index with the token returned %d", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), s.token) {
		t.Error("the page was served without its token, so no API call it makes can succeed")
	}
	if strings.Contains(string(body), "{{TOKEN}}") {
		t.Error("the token placeholder was not substituted")
	}
	if res.Header.Get("Content-Security-Policy") == "" {
		t.Error("the page was served without a content policy")
	}
}

func TestStateEndpointReturnsTheModel(t *testing.T) {
	s := newServer(t)
	req, _ := http.NewRequest("GET", s.base()+"/api/state", nil)
	req.Header.Set(HeaderToken, s.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	var m wizard.Model
	if err := json.NewDecoder(res.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m.Screen != wizard.ScreenWelcome {
		t.Errorf("screen = %q, want welcome", m.Screen)
	}
	if len(m.Domains) == 0 {
		t.Error("the address picker was served with no domains")
	}
}

// The stream must deliver the current state on connect, not merely on the next
// change — a page reloaded mid-install has to be able to catch up.
func TestEventStreamOpensWithTheCurrentState(t *testing.T) {
	s := newServer(t)

	req, _ := http.NewRequest("GET", s.base()+"/api/events?t="+s.token, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type = %q", ct)
	}

	line, err := bufio.NewReader(res.Body).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "data: ") {
		t.Fatalf("first line = %q, want a data frame", line)
	}
	var m wizard.Model
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m); err != nil {
		t.Fatal(err)
	}
	if m.Screen != wizard.ScreenWelcome {
		t.Errorf("first frame screen = %q", m.Screen)
	}
}

// A long operation must not hold the request open. The client watches the
// stream instead, which is what lets it survive a reload.
func TestOperationsAnswerImmediately(t *testing.T) {
	s := newServer(t)
	req, _ := http.NewRequest("POST", s.base()+"/api/start", strings.NewReader("{}"))
	req.Header.Set(HeaderToken, s.token)

	start := time.Now()
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	if res.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want 202", res.StatusCode)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("the request was held open for %s", elapsed)
	}
}

func TestQuitClosesDone(t *testing.T) {
	s := newServer(t)
	req, _ := http.NewRequest("POST", s.base()+"/api/quit", nil)
	req.Header.Set(HeaderToken, s.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("quit did not signal completion")
	}
}

// The page must be reachable at the URL the wizard prints, token included.
func TestURLCarriesTheToken(t *testing.T) {
	s := newServer(t)
	if !strings.Contains(s.URL(), "t="+s.token) {
		t.Errorf("URL %q does not carry the token", s.URL())
	}
	if !strings.HasPrefix(s.URL(), "http://127.0.0.1:") {
		t.Errorf("URL %q is not on loopback", s.URL())
	}
}

// Shutdown waits for connections to become idle, and an event stream never
// becomes idle: it is an open response by design. A browser is always attached
// by the time anyone clicks Finish, so without the stream being released the
// wizard burned its whole shutdown timeout every single time - measured at
// 3.2 seconds against 0.2 with no stream open, which is a console window
// sitting there after the user thinks they are done.
func TestQuittingDoesNotWaitOutTheShutdownTimeoutOnAnEventStream(t *testing.T) {
	s := newServer(t)

	req, err := http.NewRequest(http.MethodGet, s.base()+"/api/events?t="+s.token, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	// Read the first snapshot so the stream is definitely established rather
	// than merely requested.
	if _, err := bufio.NewReader(res.Body).ReadString('\n'); err != nil {
		t.Fatalf("event stream never delivered anything: %v", err)
	}

	// The same timeout main uses, so a regression fails here rather than
	// silently costing the user three seconds.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	start := time.Now()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("Close took %v with an event stream open; it waited out the timeout instead of releasing the stream", took)
	}
}
