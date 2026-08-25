//go:build !windows

package ui

import (
	"fmt"
	"os"
)

// Notify prints, which away from Windows is the whole story: RASA is started
// from a terminal with sudo, so there is always somewhere for this to go and
// no reason to reach for a desktop toolkit to say one sentence.
func Notify(title, body string) {
	fmt.Fprintf(os.Stderr, "\n%s\n%s\n\n", title, body)
}
