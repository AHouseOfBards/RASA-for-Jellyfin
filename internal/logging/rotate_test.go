package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sizeOf(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}

// The failure this exists to prevent: the address syncer runs every ten
// minutes forever, and on a broken credential it logs an error every single
// run. Without rolling, the file only ever grows.
func TestABrokenSyncerCannotGrowTheLogWithoutBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync.log")
	log, err := Open(path, Options{MaxBytes: 64 << 10, Keep: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	// Roughly a year of a syncer failing every ten minutes.
	for i := 0; i < 52_000; i++ {
		log.Error("ddns update failed", "attempt", i)
	}

	live := sizeOf(t, path)
	if live > 64<<10 {
		t.Errorf("live log is %d bytes, want no more than the 64KiB limit", live)
	}

	var total int64 = live
	for i := 1; ; i++ {
		fi, err := os.Stat(fmt.Sprintf("%s.%d", path, i))
		if err != nil {
			break
		}
		total += fi.Size()
	}
	// Keep=2 means three files at most, each bounded by MaxBytes.
	if max := int64(3 * 64 << 10); total > max {
		t.Errorf("everything on disk is %d bytes, want no more than %d", total, max)
	}
}

func TestRollingKeepsTheMostRecentLinesInTheLiveFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync.log")
	log, err := Open(path, Options{MaxBytes: 512, Keep: 1})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 200; i++ {
		log.Info("line", "n", i)
	}
	log.Close()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The whole point of rolling towards the newest: the file a user opens
	// after a failure has to hold what just happened, not what happened first.
	if !strings.Contains(string(b), `"n":199`) {
		t.Errorf("the live log does not contain the last line written:\n%s", b)
	}
	if strings.Contains(string(b), `"n":0,`) {
		t.Error("the live log still contains the first line, so nothing rolled")
	}
}

func TestOldBackupsAreDiscardedRatherThanAccumulating(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sync.log")
	log, err := Open(path, Options{MaxBytes: 256, Keep: 2})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		log.Info("line", "n", i)
	}
	log.Close()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("found %d files (%v), want the live log plus 2 backups", len(entries), names)
	}
}

// An existing log must not be discarded on open: a user runs setup, it fails,
// they run it again, and the bundle needs both runs.
func TestOpeningAnExistingLogAppendsRatherThanTruncating(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rasa.log")
	if err := os.WriteFile(path, []byte("{\"msg\":\"from the first run\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	log, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	log.Info("from the second run")
	log.Close()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"from the first run", "from the second run"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("log is missing %q:\n%s", want, b)
		}
	}
}

// A single record larger than the limit must still be written. Rolling an
// empty file to make room for something that will never fit loops forever.
func TestARecordLargerThanTheLimitIsStillWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.log")
	log, err := Open(path, Options{MaxBytes: 64, Keep: 1})
	if err != nil {
		t.Fatal(err)
	}
	log.Info(strings.Repeat("x", 500))
	log.Close()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), strings.Repeat("x", 500)) {
		t.Errorf("the oversized record was dropped:\n%s", b)
	}
}

// Reopening has to pick up where the last process left off. rasa-sync exits
// after every run, so if the size were assumed to be zero on open, nothing
// would ever roll.
func TestSizeIsCarriedAcrossAReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync.log")
	for run := 0; run < 40; run++ {
		log, err := Open(path, Options{MaxBytes: 1024, Keep: 1})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 10; i++ {
			log.Error("ddns update failed", "run", run, "i", i)
		}
		log.Close()
	}
	if n := sizeOf(t, path); n > 1024 {
		t.Errorf("live log is %d bytes after 40 separate runs, want no more than 1024", n)
	}
}
