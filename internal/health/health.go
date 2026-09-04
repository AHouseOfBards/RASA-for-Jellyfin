// Package health reports whether remote access is still working.
//
// # What it is for
//
// RASA records everything needed to spot a failure and, until this package,
// read none of it. The certificate expiry was written once at setup and never
// looked at again; the sync heartbeat was written every ten minutes and only
// ever read by a human who already suspected something.
//
// Both realistic failures are silent and delayed. A revoked Dynu credential
// stops Caddy renewing and breaks everything ninety days later. A failing sync
// leaves the address stale until the connection's WAN address next changes,
// which could be tonight or in six months. Neither announces itself, and by
// the time anyone notices, the thing that could have explained it — RASA — has
// been uninstalled.
//
// The address syncer runs every ten minutes for the life of the machine, so it
// is the only component in a position to check. This package is what it checks
// with, and what it writes down afterwards.
package health

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Check is one thing that was looked at.
type Check struct {
	// Name is what was checked, in the user's terms: "Your address",
	// "The secure connection".
	Name string
	// OK is whether it passed. A check that could not be performed is not OK:
	// "I could not tell" and "it is fine" must never render the same.
	OK bool
	// Detail is one line describing what was found, pass or fail.
	Detail string
	// Advice is what to do about it. Empty when OK.
	Advice string
}

// Report is the result of one round of checks.
type Report struct {
	Checked  time.Time
	Hostname string
	// URL is the address a user would type, including a non-default port.
	URL    string
	Checks []Check
	// CertExpiry is when the certificate currently being served runs out.
	// Zero when it could not be read.
	CertExpiry time.Time
	// File is where this report is written. It appears in the alert so that a
	// one-line event log entry can point at the full explanation.
	File string
}

// Healthy reports whether everything passed.
func (r Report) Healthy() bool {
	for _, c := range r.Checks {
		if !c.OK {
			return false
		}
	}
	return true
}

// Problems returns the failing checks, in the order they were run.
func (r Report) Problems() []Check {
	var out []Check
	for _, c := range r.Checks {
		if !c.OK {
			out = append(out, c)
		}
	}
	return out
}

// Signature identifies which things are wrong, ignoring how long they have
// been wrong for.
//
// This is what stops the escalation becoming the problem it reports on. A
// syncer that alerts on every run raises fifty thousand events a year — the
// same unbounded-noise failure the log rolling exists to prevent, in a place
// the user cannot roll. Alerting on a change of signature means a new fault
// is heard immediately and an old one does not shout every ten minutes.
func (r Report) Signature() string {
	var names []string
	for _, c := range r.Problems() {
		names = append(names, c.Name)
	}
	if len(names) == 0 {
		return "ok"
	}
	return strings.Join(names, "|")
}

// Headline is the first line of the file, and the only line most people read.
func (r Report) Headline() string {
	if r.Healthy() {
		return "STATUS: working"
	}
	return "STATUS: ACTION NEEDED"
}

// Alert is the escalation body. Empty when healthy.
//
// What is wrong, then where the full explanation is. Deliberately not the
// advice as well: two failing checks usually share a cause, so including both
// pieces of advice says the same sentence twice in a place that shows one line
// at a glance. The file has room to explain; this has to be readable in a log
// viewer's summary column.
func (r Report) Alert() string {
	problems := r.Problems()
	if len(problems) == 0 {
		return ""
	}
	var parts []string
	for _, c := range problems {
		parts = append(parts, c.Detail)
	}
	out := strings.Join(parts, " ")
	if r.File != "" {
		out += " What to do about it is in " + r.File
	}
	return out
}

// Text renders the file the user opens.
//
// Deliberately plain: no RASA to render it, no terminal assumed, and it has to
// be readable by someone who has forgotten what any of this is. The headline
// is at the top because the question being asked is "is this still working?"
// and the answer should not require reading.
func (r Report) Text() string {
	const rule = "  --------------------------------------------------------------"
	var b strings.Builder

	b.WriteString("  RASA for Jellyfin — remote access health\n")
	b.WriteString(rule + "\n\n")
	b.WriteString("  " + r.Headline() + "\n")
	fmt.Fprintf(&b, "  Last checked: %s\n", r.Checked.Format("2006-01-02 15:04:05 MST"))
	if r.URL != "" {
		fmt.Fprintf(&b, "  Your address: %s\n", r.URL)
	} else if r.Hostname != "" {
		fmt.Fprintf(&b, "  Your address: %s\n", r.Hostname)
	}
	b.WriteString("\n")

	const indent = "         "
	for _, c := range r.Checks {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
		}
		fmt.Fprintf(&b, "  [%s] %s\n", mark, c.Name)
		for _, line := range []string{c.Detail, c.Advice} {
			if line != "" {
				fmt.Fprintf(&b, "%s%s\n", indent, wrap(line, 60, indent))
			}
		}
	}
	b.WriteString("\n")

	if !r.CertExpiry.IsZero() {
		// Measured from when the check ran, not from now: the same reference
		// the check itself used, so the two cannot print different numbers.
		fmt.Fprintf(&b, "  Certificate valid until %s (%d days)\n\n",
			r.CertExpiry.Format("2006-01-02"), daysLeft(r.CertExpiry, r.Checked))
	}

	if r.Healthy() {
		b.WriteString("  Nothing to do. This file is rewritten every ten minutes by\n")
		b.WriteString("  the address sync task, so an old date here means that task\n")
		b.WriteString("  has stopped running.\n")
	} else {
		b.WriteString("  Re-running the RASA setup app repairs all of the above. It\n")
		b.WriteString("  detects what is already set up and fixes only what is broken.\n")
	}
	return b.String()
}

// wrap breaks a sentence to width, indenting continuations.
//
// A duplicate of the one in internal/recovery, kept rather than shared: both
// are ten lines, and a package that exists to say whether remote access works
// should not depend on the package that writes the setup notes.
func wrap(s string, width int, contIndent string) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	var out strings.Builder
	line := words[0]
	for _, w := range words[1:] {
		if len(line)+1+len(w) > width {
			out.WriteString(line + "\n" + contIndent)
			line = w
			continue
		}
		line += " " + w
	}
	out.WriteString(line)
	return out.String()
}

// Write saves the report to path.
//
// World-readable on purpose, and it contains no secrets: this is the file a
// user is told to open when remote access stops working, months after RASA
// was removed, possibly from an account that is not the one setup ran under.
func Write(path string, r Report) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(r.Text()), 0o644)
}
