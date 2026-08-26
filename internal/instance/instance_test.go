package instance

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestSecondInstanceIsRefusedWhileTheFirstIsServing(t *testing.T) {
	// Stand in for a running wizard: without a token it answers 403, which is
	// the signature the lock probes for.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not authorised", http.StatusForbidden)
	}))
	defer srv.Close()
	addr := srv.Listener.Addr().String()

	path := filepath.Join(t.TempDir(), "running.lock")
	first, err := Acquire(path, addr)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer first.Release()

	if _, err := Acquire(path, "127.0.0.1:1"); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Acquire = %v, want ErrAlreadyRunning", err)
	}
}

// A lock left by a crash or a kill must not lock the user out of their own
// installer: refusing because of a file belonging to a process that no longer
// exists is worse than the problem being solved.
func TestStaleLockIsTakenOver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "running.lock")
	// Port 1 on loopback: nothing is listening, so the probe finds nothing.
	if err := os.WriteFile(path, []byte("999999\n127.0.0.1:1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(path, "127.0.0.1:2")
	if err != nil {
		t.Fatalf("a stale lock blocked startup: %v", err)
	}
	defer lock.Release()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "127.0.0.1:2"; !contains(string(body), want) {
		t.Errorf("lock still names the dead instance: %q", body)
	}
}

// An ephemeral port freed by a crashed run can be reused by anything. Treating
// an unrelated program as a running RASA would lock the user out with no way
// back, so the probe checks for the wizard's own refusal, not for "something
// is listening".
func TestSomethingElseOnThePortIsNotMistakenForRASA(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // A different program, answering happily.
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "running.lock")
	if err := os.WriteFile(path, []byte("1\n"+srv.Listener.Addr().String()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(path, "127.0.0.1:2")
	if err != nil {
		t.Fatalf("an unrelated listener was mistaken for a running RASA: %v", err)
	}
	lock.Release()
}

// The data directory is world-readable on Windows, and the token is the one
// thing between any local user and an administrator-privileged installer.
func TestLockNeverRecordsTheToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "running.lock")
	lock, err := Acquire(path, "127.0.0.1:1234")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if contains(string(body), "?t=") || contains(string(body), "token") {
		t.Errorf("the lock file carries a token: %q", body)
	}
}

func TestReleaseOnNilIsSafe(t *testing.T) {
	var l *Lock
	if err := l.Release(); err != nil {
		t.Errorf("Release on nil: %v", err)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		(haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
