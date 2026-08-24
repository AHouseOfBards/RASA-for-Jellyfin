// Package service registers the things that must outlive RASA.
//
// SPEC.md §3 lists three jobs that keep running forever: TLS termination,
// certificate renewal, and DDNS address sync. Caddy owns the first two by
// itself. The third is a scheduled command rather than a daemon — it runs,
// updates, and exits.
//
// # What "disposable" actually means
//
// RASA's wizard is disposable; a small amount of installed machinery is not,
// and cannot be. Something has to hold the Dynu credential and talk to the API
// when the address changes, so the installer places a `rasa-sync` helper
// alongside Caddy. It is not a daemon and nothing supervises it — the OS
// scheduler runs it on a timer. Uninstalling the wizard leaves both in place,
// which is the entire point.
package service

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
)

// Status is the state of an installed service.
type Status string

const (
	StatusUnknown    Status = "unknown"
	StatusRunning    Status = "running"
	StatusStopped    Status = "stopped"
	StatusNotPresent Status = "not installed"
)

// Definition describes a service to install.
type Definition struct {
	// Name is the OS-level identifier: the Windows service name, or the
	// systemd unit name without its suffix.
	Name string
	// DisplayName is what a person sees in a service list.
	DisplayName string
	Description string

	// Executable and Args form the command line.
	Executable string
	Args       []string

	// WorkingDir is optional.
	WorkingDir string

	// Environment holds variables the service needs. The Dynu credential
	// arrives this way rather than on the command line, which is visible in
	// process listings and service manager UIs.
	Environment map[string]string

	// EnvironmentFile is a path the service reads variables from. Preferred
	// over Environment for secrets on Linux, where systemd can read a
	// root-owned 0600 file the unit itself does not expose.
	EnvironmentFile string
}

// Timer describes a scheduled command.
type Timer struct {
	Name        string
	Description string
	Executable  string
	Args        []string
	// Interval between runs.
	Interval time.Duration
	// RunAtStartup makes the command run once shortly after boot, which
	// matters because a WAN address most often changes across a reboot.
	RunAtStartup bool
	Environment  map[string]string
}

// Manager installs and controls OS services and timers.
type Manager interface {
	// InstallService registers a service, replacing any existing definition
	// with the same name.
	InstallService(ctx context.Context, d Definition) error
	// StartService starts it, or does nothing if already running.
	StartService(ctx context.Context, name string) error
	// StopService stops it, or does nothing if already stopped.
	StopService(ctx context.Context, name string) error
	// RemoveService deletes it. Removing an absent service succeeds.
	RemoveService(ctx context.Context, name string) error
	// ServiceStatus reports the current state.
	ServiceStatus(ctx context.Context, name string) (Status, error)

	// InstallTimer registers a scheduled command.
	InstallTimer(ctx context.Context, t Timer) error
	// RemoveTimer deletes it. Removing an absent timer succeeds.
	RemoveTimer(ctx context.Context, name string) error
	// TimerInstalled reports whether it exists.
	TimerInstalled(ctx context.Context, name string) (bool, error)
}

// ErrUnsupported is returned on platforms with no implementation.
var ErrUnsupported = errors.New("service management is not supported on this platform")

// ErrNeedsPrivileges indicates the operation requires elevation.
var ErrNeedsPrivileges = errors.New("administrator rights are required")

// New returns the Manager for this platform.
func New(log *logging.Logger) (Manager, error) {
	if log == nil {
		log = logging.Discard()
	}
	m, err := newManager(log)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// Well-known names RASA installs under.
const (
	CaddyServiceName = "RASACaddy"
	CaddyDisplayName = "RASA for Jellyfin — Secure Proxy"
	CaddyDescription = "Provides secure remote access to Jellyfin and renews its own certificate."

	SyncTimerName        = "RASADynuSync"
	SyncTimerDescription = "Keeps the Jellyfin remote address pointed at this connection."

	// SyncInterval is how often the address is checked. Every ten minutes
	// bounds the outage after an address change to roughly that long, without
	// polling hard enough to matter.
	SyncInterval = 10 * time.Minute
)

// CaddyDefinition builds the service definition for the bundled Caddy.
func CaddyDefinition(caddyPath, caddyfilePath, workDir string, env map[string]string) Definition {
	return Definition{
		Name:        CaddyServiceName,
		DisplayName: CaddyDisplayName,
		Description: CaddyDescription,
		Executable:  caddyPath,
		Args:        []string{"run", "--config", caddyfilePath, "--adapter", "caddyfile"},
		WorkingDir:  workDir,
		Environment: env,
	}
}

// SyncTimerDefinition builds the timer definition for the address sync helper.
func SyncTimerDefinition(syncPath string, env map[string]string) Timer {
	return Timer{
		Name:         SyncTimerName,
		Description:  SyncTimerDescription,
		Executable:   syncPath,
		Args:         []string{"--once"},
		Interval:     SyncInterval,
		RunAtStartup: true,
		Environment:  env,
	}
}

// Describe returns a short human description of the platform's mechanism, for
// the recovery file — a user reading it months later needs to know where to
// look.
func Describe() string {
	switch runtime.GOOS {
	case "windows":
		return "a Windows Service and a Scheduled Task"
	case "linux":
		return "a systemd service and timer"
	}
	return fmt.Sprintf("OS services on %s", runtime.GOOS)
}
