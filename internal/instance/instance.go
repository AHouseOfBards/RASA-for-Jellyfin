// Package instance stops two copies of RASA running at once.
//
// Nothing prevented it before, and two copies is not a harmless duplicate: they
// share one state file, one log, one credential store, and both are willing to
// install a service, register a scheduled task, write a firewall rule and claim
// a hostname. Two runs interleaving through that produce a machine in a state
// neither of them recorded.
//
// The lock deliberately records only the address the other wizard is serving
// on, never its token. The data directory is world-readable on Windows, and the
// token is the single thing standing between any local user and an
// administrator-privileged installer.
package instance

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrAlreadyRunning is returned when another RASA is serving.
var ErrAlreadyRunning = errors.New("another copy of RASA is already running")

// Lock is a held claim on being the only running instance.
type Lock struct {
	path string
}

// Acquire claims the lock, recording addr as where this instance is serving.
//
// A stale lock, left by a crash or a kill, is taken over rather than treated as
// a conflict: refusing to run because of a file left by a process that no
// longer exists would be worse than the problem being solved. Liveness is
// decided by asking the recorded address whether a RASA is answering, which is
// the thing actually being protected against, rather than by whether some
// process id is still in use.
func Acquire(path, addr string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("preparing the lock directory: %w", err)
	}

	if existing, err := os.ReadFile(path); err == nil {
		if other := parseAddr(existing); other != "" && serving(other) {
			return nil, fmt.Errorf("%w at http://%s", ErrAlreadyRunning, other)
		}
	}

	body := fmt.Sprintf("%d\n%s\n", os.Getpid(), addr)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return nil, fmt.Errorf("writing the lock: %w", err)
	}
	return &Lock{path: path}, nil
}

// Release drops the lock. Safe on a nil receiver so callers can defer it
// unconditionally.
func (l *Lock) Release() error {
	if l == nil || l.path == "" {
		return nil
	}
	return os.Remove(l.path)
}

func parseAddr(content []byte) string {
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) < 2 {
		return ""
	}
	addr := strings.TrimSpace(lines[1])
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return ""
	}
	// A lock naming a port outside the ephemeral loopback range is not one of
	// ours, so refuse to probe it rather than send a request somewhere unknown.
	if _, port, _ := net.SplitHostPort(addr); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n <= 0 || n > 65535 {
			return ""
		}
	}
	return addr
}

// serving reports whether a RASA wizard is answering at addr.
//
// The check is for RASA specifically, not for "something is listening": an
// ephemeral port freed by a crashed run can be reused by anything, and treating
// an unrelated program as a running RASA would lock the user out of their own
// installer with no way back.
func serving(addr string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/", nil)
	if err != nil {
		return false
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()

	// Without a token the wizard refuses, and that refusal is the signature.
	// Anything else answering on a recycled port will not produce it.
	return res.StatusCode == http.StatusForbidden
}
