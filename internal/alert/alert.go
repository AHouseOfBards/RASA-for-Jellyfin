// Package alert reports an unattended failure somewhere a person will find it.
//
// # Why this exists
//
// RASA is uninstalled once setup finishes (SPEC.md §3), so there is no app to
// put a warning in. What survives is a proxy, a scheduled command, and some
// files — and until now, when remote access broke, all three stayed silent.
// The two realistic failures are both slow and both invisible: a revoked Dynu
// credential stops the certificate renewing and breaks everything ninety days
// later, and a failing address sync leaves the hostname pointing at an old
// connection until somebody tries to watch something from a hotel.
//
// The address syncer is the only component still running, so it is the only
// thing positioned to notice. It cannot show a dialog — on Windows it runs as
// SYSTEM in session 0, which has no desktop — so it reports on the channel the
// operating system already keeps for unattended work.
package alert

import (
	"context"
	"strings"
	"unicode/utf8"
)

// Level is how loud the report is.
type Level string

const (
	// LevelError is something the user has to act on.
	LevelError Level = "error"
	// LevelInfo is a return to normal. Worth saying once, so nobody spends an
	// evening chasing a problem that has already fixed itself.
	LevelInfo Level = "info"
)

// Raise reports one event.
//
// Best effort in every case. Nothing about a sync should fail because the
// machine would not accept a log entry, and the caller has no useful response
// to that anyway — the health file is written regardless.
func Raise(ctx context.Context, l Level, subject, body string) error {
	return raise(ctx, l, subject, flatten(body))
}

// maxBody is a conservative cap. The Windows channel is fed through a
// command-line argument and long values are silently refused.
const maxBody = 900

// flatten turns a paragraph into something that survives a single-line
// transport without losing its sentence breaks.
func flatten(s string) string {
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
	if len(s) <= maxBody {
		return s
	}
	// Trimmed on a rune boundary. A byte-sliced string can end mid-character,
	// and the Windows console decodes what it is given rather than repairing
	// it, so the last thing in the entry would be a replacement glyph.
	const ellipsis = "…"
	cut := maxBody - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}
