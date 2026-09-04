package health

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// RepeatAfter is how long a fault must keep failing before it is reported a
// second time.
//
// The check runs every ten minutes. Reporting each run would put fifty
// thousand entries a year into the Windows Event Log — the same unbounded
// noise that made sync.log useless, in a place the user cannot roll. Once a
// day is often enough that a fault is not forgotten and rare enough that the
// log stays readable.
const RepeatAfter = 24 * time.Hour

// Escalator decides when a report is worth interrupting somebody over.
//
// It exists because "notice the failure" and "shout about the failure" are
// different jobs with different rules: the health file is rewritten every run
// regardless, while an alert is raised only when something changed or when a
// fault has been ignored for a day.
type Escalator struct {
	// StatePath holds what was last reported. Kept beside the health file so
	// the two are removed together.
	StatePath string
	// Raise delivers the alert. Injected so a test never writes to the real
	// Windows Event Log.
	Raise func(ctx context.Context, l AlertLevel, subject, body string) error
	// Now defaults to time.Now.
	Now func() time.Time
	// Repeat defaults to RepeatAfter.
	Repeat time.Duration
}

// AlertLevel mirrors alert.Level without importing it.
//
// health stays free of the platform-specific package so that the decision of
// *when* to interrupt somebody can be tested anywhere, while the decision of
// *how* stays in one file per operating system.
type AlertLevel string

const (
	// LevelError is a fault the user has to act on.
	LevelError AlertLevel = "error"
	// LevelInfo is a return to normal.
	LevelInfo AlertLevel = "info"
)

type escalationState struct {
	// Signature is which checks were failing when the last alert went out.
	// "ok" means the last thing reported was a recovery.
	Signature string `json:"signature"`
	// Raised is when that alert went out.
	Raised time.Time `json:"raised"`
}

// Consider raises an alert if this report warrants one.
//
// Three cases produce an alert and nothing else does: a new fault, a fault
// that has been failing for longer than Repeat, and a recovery after a fault
// was reported. In particular a machine that has been broken since Tuesday
// stays quiet, and a machine that has always been fine never says anything at
// all — the health file is where "everything is fine" is recorded.
func (e *Escalator) Consider(ctx context.Context, r Report) error {
	now := time.Now
	if e.Now != nil {
		now = e.Now
	}
	repeat := e.Repeat
	if repeat == 0 {
		repeat = RepeatAfter
	}

	last := e.load()
	sig := r.Signature()

	var level AlertLevel
	var subject, body string

	switch {
	case sig == "ok":
		// Silence is the right answer unless the user was told there was a
		// problem. Then they are owed the other half of the message.
		if last.Signature == "" || last.Signature == "ok" {
			return nil
		}
		level = LevelInfo
		subject = "Remote access to Jellyfin is working again."
		body = "RASA checked " + r.Hostname + " and everything it reported earlier has cleared."

	case sig != last.Signature:
		// Something new broke, or a different thing broke. Say so now.
		level = LevelError
		subject = "Remote access to Jellyfin has stopped working."
		body = r.Alert()

	case now().Sub(last.Raised) >= repeat:
		level = LevelError
		subject = "Remote access to Jellyfin is still not working."
		body = r.Alert()

	default:
		return nil
	}

	raiseErr := e.raise(ctx, level, subject, body)

	// Recorded whether or not the channel accepted it, and before the error is
	// returned. A machine that will not take an event log entry — an
	// unelevated developer run, a locked-down host — would otherwise retry
	// every ten minutes forever, which is the noise this whole mechanism
	// exists to avoid, just moved somewhere the user cannot see it. The health
	// file is the channel that always works; this one is the escalation.
	if err := e.save(escalationState{Signature: sig, Raised: now()}); err != nil && raiseErr == nil {
		return err
	}
	return raiseErr
}

func (e *Escalator) raise(ctx context.Context, l AlertLevel, subject, body string) error {
	if e.Raise == nil {
		return nil
	}
	return e.Raise(ctx, l, subject, body)
}

// load reads the last escalation. A missing or damaged file reads as "nothing
// has ever been reported", which at worst raises one duplicate alert — far
// better than the alternative failure, where an unreadable file silences a
// real fault forever.
func (e *Escalator) load() escalationState {
	var s escalationState
	if e.StatePath == "" {
		return s
	}
	b, err := os.ReadFile(e.StatePath)
	if err != nil {
		return s
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return escalationState{}
	}
	return s
}

func (e *Escalator) save(s escalationState) error {
	if e.StatePath == "" {
		return nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(e.StatePath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(e.StatePath, b, 0o644)
}
