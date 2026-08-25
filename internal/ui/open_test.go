package ui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Open must not kill the launcher it just started.
//
// The first implementation bound the command to a context and deferred its
// cancel, so exec.CommandContext killed the launcher microseconds after Start
// returned. Nothing reported it: Open returned nil, the caller assumed a
// browser had appeared, and the user was left on an empty screen with no
// address to fall back to. Two different launchers were blamed before the
// cause was found.
func TestOpenDoesNotKillTheLauncher(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")

	// Stands in for the real launcher: it takes long enough that a premature
	// cancel kills it before it records anything.
	restore := launcher
	t.Cleanup(func() { launcher = restore })
	launcher = func(ctx context.Context, url string) *exec.Cmd {
		return exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperLauncher")
	}
	t.Setenv("RASA_TEST_LAUNCHER_MARKER", marker)

	if err := Open("http://127.0.0.1:1234/?t=abc"); err != nil {
		t.Fatalf("Open: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			return // it survived long enough to finish
		}
		if time.Now().After(deadline) {
			t.Fatal("the launcher never ran to completion; Open killed it after starting it")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestHelperLauncher is not a test. It is the subprocess the case above starts
// in place of a browser launcher.
func TestHelperLauncher(t *testing.T) {
	marker := os.Getenv("RASA_TEST_LAUNCHER_MARKER")
	if marker == "" {
		t.Skip("not running as the launcher stand-in")
	}
	time.Sleep(500 * time.Millisecond)
	if err := os.WriteFile(marker, []byte("ran"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Open reports only that the launcher started, never that a browser appeared.
// The caller depends on that distinction, so it is stated here too.
func TestOpenSucceedsWithoutProvingAnythingAppeared(t *testing.T) {
	restore := launcher
	t.Cleanup(func() { launcher = restore })
	launcher = func(ctx context.Context, url string) *exec.Cmd {
		return exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperLauncher")
	}
	t.Setenv("RASA_TEST_LAUNCHER_MARKER", filepath.Join(t.TempDir(), "ran"))

	if err := Open("http://127.0.0.1:1/?t=x"); err != nil {
		t.Errorf("Open reported failure for a launcher that started fine: %v", err)
	}
}

func TestOpenReportsAFailureToStart(t *testing.T) {
	restore := launcher
	t.Cleanup(func() { launcher = restore })
	launcher = func(ctx context.Context, url string) *exec.Cmd {
		return exec.CommandContext(ctx, filepath.Join(t.TempDir(), "does-not-exist"))
	}
	if err := Open("http://127.0.0.1:1/?t=x"); err == nil {
		t.Error("Open reported success for a launcher that could not start")
	}
}
