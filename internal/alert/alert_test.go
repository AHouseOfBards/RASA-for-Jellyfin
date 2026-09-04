package alert

import (
	"strings"
	"testing"
)

// The Windows channel carries the message as a single command-line argument,
// where a newline is at best ignored and at worst truncates the entry.
func TestAParagraphSurvivesAsOneLine(t *testing.T) {
	got := flatten("The certificate expired.\nRenewal has been failing.\n\nRe-run setup.")
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("line breaks survived: %q", got)
	}
	for _, want := range []string{"The certificate expired.", "Renewal has been failing.", "Re-run setup."} {
		if !strings.Contains(got, want) {
			t.Errorf("%q was lost:\n%s", want, got)
		}
	}
	if strings.Contains(got, "  ") {
		t.Errorf("collapsed whitespace left a double space: %q", got)
	}
}

// Over-long values are refused rather than truncated by the platform, which
// would turn a useful alert into no alert at all.
func TestAnOverlongMessageIsTrimmedRatherThanRefused(t *testing.T) {
	got := flatten(strings.Repeat("word ", 1000))
	if len(got) > maxBody {
		t.Errorf("flattened to %d bytes, want no more than %d", len(got), maxBody)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a trimmed message should show that it was trimmed: %q", got[len(got)-20:])
	}
}
