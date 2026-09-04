//go:build !windows

package alert

import (
	"context"
	"fmt"
	"os"
)

// On Linux the channel is already there: systemd captures a unit's standard
// error into the journal, tagged with the unit name. `journalctl -u
// rasa-sync` is then the equivalent of filtering the Application log, and a
// failing timer already shows up in `systemctl list-timers` beside it.
//
// Writing to stderr rather than calling `logger` keeps this working when the
// command is run by hand, in a container, or under cron — all of which are
// supported ways to run the syncer (SPEC.md §17), and none of which have a
// journal to write to.
func raise(ctx context.Context, l Level, subject, body string) error {
	prefix := "RASA ERROR:"
	if l == LevelInfo {
		prefix = "RASA:"
	}
	_, err := fmt.Fprintf(os.Stderr, "%s %s %s\n", prefix, subject, body)
	return err
}
