//go:build !windows

package ui

// Ask has no dialog to show away from Windows.
//
// RASA is started from a terminal there, so the caller prompts on stdin
// instead; this exists only to keep the signature the same, and answers no so
// that a caller which forgets to prompt does nothing rather than something
// destructive.
func Ask(title, body string) bool { return false }
