package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// The handler-level tests matter more than the Redactor unit tests: this is
// the path every real log call takes, and it is where a bypass would hide.

func TestHandlerRedactsMessage(t *testing.T) {
	var buf bytes.Buffer
	l := New(Options{Writer: &buf})
	const key = "dynu-api-key-abcdef123456"
	l.Redactor().RegisterSecret(key)

	l.Info("calling Dynu with " + key)

	if strings.Contains(buf.String(), key) {
		t.Fatalf("secret leaked into log message: %s", buf.String())
	}
}

func TestHandlerRedactsStringAttr(t *testing.T) {
	var buf bytes.Buffer
	l := New(Options{Writer: &buf})
	const key = "dynu-api-key-abcdef123456"
	l.Redactor().RegisterSecret(key)

	l.Info("request", slog.String("token", key))

	if strings.Contains(buf.String(), key) {
		t.Fatalf("secret leaked into attribute: %s", buf.String())
	}
}

func TestHandlerRedactsWrappedError(t *testing.T) {
	// Errors carrying a raw API response are the most likely leak path.
	var buf bytes.Buffer
	l := New(Options{Writer: &buf})
	const key = "dynu-api-key-abcdef123456"
	l.Redactor().RegisterSecret(key)

	err := errors.New("POST /dns failed: API-Key " + key + " rejected")
	l.Error("dynu call failed", slog.Any("err", err))

	if strings.Contains(buf.String(), key) {
		t.Fatalf("secret leaked through error value: %s", buf.String())
	}
}

func TestHandlerRedactsInsideGroup(t *testing.T) {
	var buf bytes.Buffer
	l := New(Options{Writer: &buf})
	const key = "dynu-api-key-abcdef123456"
	l.Redactor().RegisterSecret(key)

	l.Info("request", slog.Group("http",
		slog.String("url", "https://api.dynu.com/v2/dns"),
		slog.String("auth", key),
	))

	if strings.Contains(buf.String(), key) {
		t.Fatalf("secret leaked from inside group: %s", buf.String())
	}
}

func TestHandlerRedactsAttrsAttachedWithWith(t *testing.T) {
	// A secret bound once via With must not be written on every later line.
	var buf bytes.Buffer
	l := New(Options{Writer: &buf})
	const key = "dynu-api-key-abcdef123456"
	l.Redactor().RegisterSecret(key)

	sub := l.With(slog.String("token", key))
	sub.Info("first")
	sub.Info("second")

	if strings.Contains(buf.String(), key) {
		t.Fatalf("secret leaked via With: %s", buf.String())
	}
}

func TestRunIDOnEveryLine(t *testing.T) {
	var buf bytes.Buffer
	l := New(Options{Writer: &buf})
	l.Info("one")
	l.Info("two")

	for _, line := range nonEmptyLines(buf.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line is not JSON: %v", err)
		}
		if m["run_id"] != l.RunID() {
			t.Fatalf("missing or wrong run_id: %v", m["run_id"])
		}
	}
}

func TestWithPhaseTagsLines(t *testing.T) {
	var buf bytes.Buffer
	l := New(Options{Writer: &buf}).WithPhase("probe")
	l.Info("checking")

	var m map[string]any
	if err := json.Unmarshal([]byte(nonEmptyLines(buf.String())[0]), &m); err != nil {
		t.Fatal(err)
	}
	if m["phase"] != "probe" {
		t.Fatalf("expected phase probe, got %v", m["phase"])
	}
	if l.Phase() != "probe" {
		t.Fatalf("Phase() = %q", l.Phase())
	}
}

func TestDecisionRecordsReason(t *testing.T) {
	var buf bytes.Buffer
	l := New(Options{Writer: &buf})
	l.Decision("mode", "A", "router WAN matches observed address")

	var m map[string]any
	if err := json.Unmarshal([]byte(nonEmptyLines(buf.String())[0]), &m); err != nil {
		t.Fatal(err)
	}
	if m["chose"] != "A" || m["because"] == "" {
		t.Fatalf("decision not recorded with reason: %v", m)
	}
}

func TestEventsAreEmittedAndRedacted(t *testing.T) {
	var buf bytes.Buffer
	l := New(Options{Writer: &buf, EventBuffer: 4})
	const key = "dynu-api-key-abcdef123456"
	l.Redactor().RegisterSecret(key)

	l.Step("registering address")
	l.Warned("lease is finite " + key)

	got := drain(l.Events(), 2)
	if got[0].Kind != EventStep || got[0].Text != "registering address" {
		t.Fatalf("unexpected first event: %+v", got[0])
	}
	if got[1].Kind != EventWarn || strings.Contains(got[1].Text, key) {
		t.Fatalf("secret leaked into user-visible event: %+v", got[1])
	}
}

func TestEventsNeverBlockWhenNobodyReads(t *testing.T) {
	// Setup must not stall because the UI is slow or absent.
	var buf bytes.Buffer
	l := New(Options{Writer: &buf, EventBuffer: 1})
	for i := 0; i < 50; i++ {
		l.Step("tick")
	}
}

func TestNilEventsChannelIsSafe(t *testing.T) {
	var buf bytes.Buffer
	l := New(Options{Writer: &buf}) // EventBuffer unset
	l.Step("no events configured")
	if l.Events() != nil {
		t.Fatal("expected nil events channel")
	}
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func drain(ch <-chan Event, n int) []Event {
	out := make([]Event, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, <-ch)
	}
	return out
}
