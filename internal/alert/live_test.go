package alert

import (
	"context"
	"os"
	"testing"
)

// A real write to the platform's channel. Skipped by default because it puts
// an entry in the machine's event log, and on Windows it needs the elevation
// rasa-sync has as SYSTEM and a developer shell usually does not.
//
//	RASA_ALERT_LIVE=1 go test ./internal/alert/ -run Live -v
func TestLiveRaise(t *testing.T) {
	if os.Getenv("RASA_ALERT_LIVE") == "" {
		t.Skip("set RASA_ALERT_LIVE=1 to write a real entry")
	}
	if err := Raise(context.Background(), LevelInfo,
		"RASA self-test.", "This entry was written by a test and means nothing."); err != nil {
		t.Fatalf("Raise: %v", err)
	}
}
