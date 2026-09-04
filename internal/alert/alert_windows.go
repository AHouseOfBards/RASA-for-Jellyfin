//go:build windows

package alert

import (
	"context"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

// The Windows Event Log, written through eventcreate.exe.
//
// Same reasoning as sc.exe and schtasks.exe in internal/service: it ships with
// every supported version of Windows, and the alternative — RegisterEventSource
// and ReportEvent through advapi32, plus registering a message resource in
// HKLM — is a great deal of untestable syscall for something RASA does a
// handful of times a year. eventcreate also registers the source itself on
// first use, which is the part that would otherwise need its own installer
// step.
//
// The Application log is where a home server user looks, and it is where Task
// Scheduler already reports on the very task raising this.
//
// Registering a new source writes under HKLM, so the first call must be
// elevated. The scheduled task runs as SYSTEM with the highest run level
// (internal/service), which satisfies that; a developer running rasa-sync from
// an ordinary shell will get "Access is denied" until the source exists, and
// the alert is dropped rather than failing the sync. To exercise it for real:
//
//	RASA_ALERT_LIVE=1 go test ./internal/alert/ -run Live -v
//
// from an elevated shell.
const (
	// Source is the name the event appears under. Kept short and without
	// spaces: eventcreate is fussy about /SO and this is the string a user
	// will filter the Application log by.
	Source = "RASA"

	// eventFailure and eventRecovered are the two ids RASA uses. eventcreate
	// only accepts 1-1000.
	eventFailure   = 100
	eventRecovered = 101
)

func raise(ctx context.Context, l Level, subject, body string) error {
	kind, id := "ERROR", eventFailure
	if l == LevelInfo {
		kind, id = "INFORMATION", eventRecovered
	}

	// Bounded hard. This runs inside a scheduled task that must finish
	// quickly, and a wedged eventcreate must not hold the sync open.
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "eventcreate.exe",
		"/T", kind,
		"/ID", fmt.Sprint(id),
		"/L", "APPLICATION",
		"/SO", Source,
		"/D", subject+" "+body,
	)
	// No console window: rasa-sync is built for the GUI subsystem and a
	// flashing black box every ten minutes would be worse than the silence
	// this replaces.
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("eventcreate: %w (%s)", err, out)
	}
	return nil
}
