//go:build linux

package service

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
)

// systemd implementation. Units are written as files and activated with
// systemctl, which is the documented interface and needs no D-Bus binding.

const unitDir = "/etc/systemd/system"

type linuxManager struct{ log *logging.Logger }

func newManager(log *logging.Logger) (Manager, error) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil, fmt.Errorf("%w: systemd was not found", ErrUnsupported)
	}
	return &linuxManager{log: log}, nil
}

func (m *linuxManager) InstallService(ctx context.Context, d Definition) error {
	path := filepath.Join(unitDir, d.Name+".service")
	if err := writeUnit(path, renderServiceUnit(d)); err != nil {
		return err
	}
	if err := m.reload(ctx); err != nil {
		return err
	}
	if out, err := m.run(ctx, "systemctl", "enable", d.Name+".service"); err != nil {
		return wrapPrivilege(fmt.Errorf("enabling %s: %w (%s)", d.Name, err, out))
	}
	m.log.Info("systemd service installed", slog.String("name", d.Name), slog.String("unit", path))
	return nil
}

func (m *linuxManager) StartService(ctx context.Context, name string) error {
	if out, err := m.run(ctx, "systemctl", "start", name+".service"); err != nil {
		return wrapPrivilege(fmt.Errorf("starting %s: %w (%s)", name, err, out))
	}
	return nil
}

func (m *linuxManager) StopService(ctx context.Context, name string) error {
	st, _ := m.ServiceStatus(ctx, name)
	if st == StatusNotPresent || st == StatusStopped {
		return nil
	}
	if out, err := m.run(ctx, "systemctl", "stop", name+".service"); err != nil {
		return wrapPrivilege(fmt.Errorf("stopping %s: %w (%s)", name, err, out))
	}
	return nil
}

func (m *linuxManager) RemoveService(ctx context.Context, name string) error {
	st, _ := m.ServiceStatus(ctx, name)
	if st == StatusNotPresent {
		return nil
	}
	_ = m.StopService(ctx, name)
	_, _ = m.run(ctx, "systemctl", "disable", name+".service")
	if err := os.Remove(filepath.Join(unitDir, name+".service")); err != nil && !os.IsNotExist(err) {
		return wrapPrivilege(err)
	}
	return m.reload(ctx)
}

func (m *linuxManager) ServiceStatus(ctx context.Context, name string) (Status, error) {
	if _, err := os.Stat(filepath.Join(unitDir, name+".service")); os.IsNotExist(err) {
		return StatusNotPresent, nil
	}
	out, _ := m.run(ctx, "systemctl", "is-active", name+".service")
	switch strings.TrimSpace(out) {
	case "active":
		return StatusRunning, nil
	case "inactive", "failed":
		return StatusStopped, nil
	}
	return StatusUnknown, nil
}

func (m *linuxManager) InstallTimer(ctx context.Context, t Timer) error {
	if err := writeUnit(filepath.Join(unitDir, t.Name+".service"), renderTimerService(t)); err != nil {
		return err
	}
	if err := writeUnit(filepath.Join(unitDir, t.Name+".timer"), renderTimerUnit(t)); err != nil {
		return err
	}
	if err := m.reload(ctx); err != nil {
		return err
	}
	if out, err := m.run(ctx, "systemctl", "enable", "--now", t.Name+".timer"); err != nil {
		return wrapPrivilege(fmt.Errorf("enabling timer %s: %w (%s)", t.Name, err, out))
	}
	m.log.Info("systemd timer installed",
		slog.String("name", t.Name), slog.Duration("interval", t.Interval))
	return nil
}

func (m *linuxManager) RemoveTimer(ctx context.Context, name string) error {
	ok, _ := m.TimerInstalled(ctx, name)
	if !ok {
		return nil
	}
	_, _ = m.run(ctx, "systemctl", "disable", "--now", name+".timer")
	for _, suffix := range []string{".timer", ".service"} {
		if err := os.Remove(filepath.Join(unitDir, name+suffix)); err != nil && !os.IsNotExist(err) {
			return wrapPrivilege(err)
		}
	}
	return m.reload(ctx)
}

func (m *linuxManager) TimerInstalled(ctx context.Context, name string) (bool, error) {
	_, err := os.Stat(filepath.Join(unitDir, name+".timer"))
	return err == nil, nil
}

func (m *linuxManager) reload(ctx context.Context) error {
	if out, err := m.run(ctx, "systemctl", "daemon-reload"); err != nil {
		return wrapPrivilege(fmt.Errorf("reloading systemd: %w (%s)", err, out))
	}
	return nil
}

func (m *linuxManager) run(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

func writeUnit(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return wrapPrivilege(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return wrapPrivilege(err)
	}
	return nil
}

func wrapPrivilege(err error) error {
	if err == nil {
		return nil
	}
	low := strings.ToLower(err.Error())
	if strings.Contains(low, "permission denied") || strings.Contains(low, "access denied") {
		return fmt.Errorf("%w: %v", ErrNeedsPrivileges, err)
	}
	return err
}
