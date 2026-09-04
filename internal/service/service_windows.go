//go:build windows

package service

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
)

// Windows implementation, driven through sc.exe and schtasks.exe.
//
// Both ship with every supported Windows version and need no dependency. The
// alternative — golang.org/x/sys/windows/svc/mgr — would add a module for
// operations RASA performs exactly once.

type windowsManager struct{ log *logging.Logger }

func newManager(log *logging.Logger) (Manager, error) { return &windowsManager{log: log}, nil }

func (m *windowsManager) InstallService(ctx context.Context, d Definition) error {
	// Replace rather than fail: a re-run must be able to correct a definition
	// written by an earlier version (SPEC.md §10).
	if st, _ := m.ServiceStatus(ctx, d.Name); st != StatusNotPresent {
		_ = m.StopService(ctx, d.Name)
		if err := m.RemoveService(ctx, d.Name); err != nil {
			return err
		}
		// sc delete is asynchronous; creating immediately can fail with
		// "marked for deletion".
		if err := m.awaitAbsent(ctx, d.Name, 15*time.Second); err != nil {
			return err
		}
	}

	if out, err := m.run(ctx, "sc.exe", scCreateArgs(d)...); err != nil {
		return wrapPrivilege(fmt.Errorf("creating service %s: %w (%s)", d.Name, err, out))
	}

	if d.Description != "" {
		if out, err := m.run(ctx, "sc.exe", "description", d.Name, d.Description); err != nil {
			// Cosmetic; never fail an install over it.
			m.log.Debug("could not set service description", slog.String("out", out))
		}
	}

	// Restart on failure: 1-minute delay, reset the counter after a day. A
	// public TLS listener that dies at 3am should come back on its own.
	if out, err := m.run(ctx, "sc.exe", "failure", d.Name,
		"reset=", "86400", "actions=", "restart/60000/restart/60000/restart/60000"); err != nil {
		m.log.Debug("could not set failure actions", slog.String("out", out))
	}

	if len(d.Environment) > 0 {
		if err := m.setServiceEnvironment(ctx, d); err != nil {
			return err
		}
	}
	m.log.Info("windows service installed", slog.String("name", d.Name))
	return nil
}

// setServiceEnvironment writes REG_MULTI_SZ Environment under the service key.
//
// Windows services do not inherit a user environment, so the credential has to
// live here. The registry key is readable only by administrators and SYSTEM,
// which is the same protection class as the DPAPI file it came from.
func (m *windowsManager) setServiceEnvironment(ctx context.Context, d Definition) error {
	var pairs []string
	for k, v := range d.Environment {
		pairs = append(pairs, k+"="+v)
	}
	key := `HKLM\SYSTEM\CurrentControlSet\Services\` + d.Name
	args := []string{"add", key, "/v", "Environment", "/t", "REG_MULTI_SZ",
		"/d", strings.Join(pairs, "\\0"), "/f"}

	if out, err := m.run(ctx, "reg.exe", args...); err != nil {
		return wrapPrivilege(fmt.Errorf("setting service environment: %w (%s)", err, out))
	}
	return nil
}

func (m *windowsManager) StartService(ctx context.Context, name string) error {
	st, err := m.ServiceStatus(ctx, name)
	if err != nil {
		return err
	}
	if st == StatusRunning {
		return nil
	}
	if out, err := m.run(ctx, "sc.exe", "start", name); err != nil {
		return wrapPrivilege(fmt.Errorf("starting %s: %w (%s)", name, err, out))
	}
	return m.awaitStatus(ctx, name, StatusRunning, 30*time.Second)
}

func (m *windowsManager) StopService(ctx context.Context, name string) error {
	st, _ := m.ServiceStatus(ctx, name)
	if st == StatusNotPresent || st == StatusStopped {
		return nil
	}
	if out, err := m.run(ctx, "sc.exe", "stop", name); err != nil {
		return wrapPrivilege(fmt.Errorf("stopping %s: %w (%s)", name, err, out))
	}
	return m.awaitStatus(ctx, name, StatusStopped, 30*time.Second)
}

func (m *windowsManager) RemoveService(ctx context.Context, name string) error {
	st, _ := m.ServiceStatus(ctx, name)
	if st == StatusNotPresent {
		return nil
	}
	if out, err := m.run(ctx, "sc.exe", "delete", name); err != nil {
		return wrapPrivilege(fmt.Errorf("removing %s: %w (%s)", name, err, out))
	}
	return nil
}

func (m *windowsManager) ServiceStatus(ctx context.Context, name string) (Status, error) {
	out, err := m.run(ctx, "sc.exe", "query", name)
	return parseServiceStatus(out, err), nil
}

func (m *windowsManager) InstallTimer(ctx context.Context, t Timer) error {
	_ = m.RemoveTimer(ctx, t.Name)

	if out, err := m.run(ctx, "schtasks.exe", schtasksCreateArgs(t)...); err != nil {
		return wrapPrivilege(fmt.Errorf("creating scheduled task %s: %w (%s)", t.Name, err, out))
	}

	if t.RunAtStartup {
		if out, err := m.run(ctx, "schtasks.exe", schtasksBootArgs(t)...); err != nil {
			m.log.Debug("could not create startup trigger", slog.String("out", out))
		}
	}

	m.log.Info("scheduled task installed",
		slog.String("name", t.Name), slog.Duration("interval", t.Interval))
	return nil
}

func (m *windowsManager) RemoveTimer(ctx context.Context, name string) error {
	for _, n := range []string{name, name + bootTaskSuffix} {
		if ok, _ := m.TimerInstalled(ctx, n); !ok {
			continue
		}
		if out, err := m.run(ctx, "schtasks.exe", "/Delete", "/TN", n, "/F"); err != nil {
			return wrapPrivilege(fmt.Errorf("removing task %s: %w (%s)", n, err, out))
		}
	}
	return nil
}

func (m *windowsManager) TimerInstalled(ctx context.Context, name string) (bool, error) {
	_, err := m.run(ctx, "schtasks.exe", "/Query", "/TN", name)
	return err == nil, nil
}

func (m *windowsManager) awaitStatus(ctx context.Context, name string, want Status, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if st, _ := m.ServiceStatus(ctx, name); st == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("service %s did not reach %s in time", name, want)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (m *windowsManager) awaitAbsent(ctx context.Context, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if st, _ := m.ServiceStatus(ctx, name); st == StatusNotPresent {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("service %s is still marked for deletion", name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (m *windowsManager) run(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

func wrapPrivilege(err error) error {
	if err == nil {
		return nil
	}
	low := strings.ToLower(err.Error())
	if strings.Contains(low, "access is denied") || strings.Contains(low, "5:") {
		return fmt.Errorf("%w: %v", ErrNeedsPrivileges, err)
	}
	return err
}
