package service

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The binaries a service runs must not live in RASA's install directory:
// uninstalling RASA would take the running proxy and the sync helper with it,
// and both are required to keep working afterwards.
func TestStageBinaryCopiesIntoADurableDirectory(t *testing.T) {
	root := t.TempDir()
	src := write(t, filepath.Join(root, "install", "caddy"), "the proxy")
	binDir := filepath.Join(root, "data", "bin")

	dst, err := StageBinary(src, binDir, "caddy")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(dst) != binDir {
		t.Errorf("staged to %s, want a file in %s", dst, binDir)
	}
	if got := read(t, dst); got != "the proxy" {
		t.Errorf("content = %q", got)
	}

	// And the original is untouched, because the uninstaller is what removes
	// it and it has to still be there to remove.
	if got := read(t, src); got != "the proxy" {
		t.Errorf("the source was disturbed: %q", got)
	}
}

// Setup is safe to re-run, so staging a newer binary over an older one has to
// work — including while the old one is the service that is currently running.
func TestStageBinaryReplacesAnOlderCopy(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	write(t, filepath.Join(binDir, "rasa-sync"), "version 1")

	src := write(t, filepath.Join(root, "new", "rasa-sync"), "version 2")
	dst, err := StageBinary(src, binDir, "rasa-sync")
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, dst); got != "version 2" {
		t.Errorf("content = %q, want the new binary", got)
	}
	// Windows holds a running executable open, so the copy is written beside
	// the target and renamed. The scratch file must not survive.
	if _, err := os.Stat(dst + ".new"); !os.IsNotExist(err) {
		t.Errorf("%s.new was left behind", dst)
	}
}

// The Linux installer puts RASA's own binary in the same directory it stages
// into, so this is a real path rather than a defensive one: copying a file
// onto itself with O_TRUNC empties it.
func TestStagingAFileOntoItselfDoesNotDestroyIt(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "caddy"), "the proxy")

	dst, err := StageBinary(src, dir, "caddy")
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, dst); got != "the proxy" {
		t.Errorf("content = %q — the binary was truncated", got)
	}
}

func TestStagedBinariesAreExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not carry an execute bit")
	}
	root := t.TempDir()
	src := write(t, filepath.Join(root, "src", "caddy"), "x")
	dst, err := StageBinary(src, filepath.Join(root, "bin"), "caddy")
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("mode = %v, want the execute bit set", fi.Mode().Perm())
	}
}

func TestStagingAMissingSourceIsAnError(t *testing.T) {
	root := t.TempDir()
	if _, err := StageBinary(filepath.Join(root, "nope"), filepath.Join(root, "bin"), "caddy"); err == nil {
		t.Error("expected an error for a source that does not exist")
	}
}

// ---- definitions ---------------------------------------------------------

// A stock Caddy reads a different config format, so the adapter flag is not
// optional — without it the proxy starts and serves nothing.
func TestTheProxyIsToldToReadACaddyfile(t *testing.T) {
	d := CaddyDefinition("/bin/caddy", "/etc/Caddyfile", "/var/lib/rasa", nil)
	joined := strings.Join(d.Args, " ")
	for _, want := range []string{"run", "--config /etc/Caddyfile", "--adapter caddyfile"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %v are missing %q", d.Args, want)
		}
	}
	if d.Name != CaddyServiceName {
		t.Errorf("name = %q, want %q", d.Name, CaddyServiceName)
	}
}

// --once matters: without it the helper would keep running under a scheduler
// that expects it to exit, and the next tick would start a second copy.
func TestTheSyncHelperRunsOnceAndExits(t *testing.T) {
	tm := SyncTimerDefinition("/bin/rasa-sync", nil)
	if len(tm.Args) != 1 || tm.Args[0] != "--once" {
		t.Errorf("args = %v, want exactly --once", tm.Args)
	}
	if tm.Interval != SyncInterval {
		t.Errorf("interval = %v, want SyncInterval", tm.Interval)
	}
	// Bounds the outage after an address change to roughly ten minutes.
	if SyncInterval != 10*time.Minute {
		t.Errorf("SyncInterval = %v, want ten minutes", SyncInterval)
	}
	if !tm.RunAtStartup {
		t.Error("the helper does not run at boot, when the address is most likely stale")
	}
}

// The recovery file tells a user where to look months later, so it has to name
// the mechanism this build actually installed.
func TestDescribeNamesThisPlatformsMechanism(t *testing.T) {
	got := Describe()
	want := map[string]string{"windows": "Scheduled Task", "linux": "systemd"}[runtime.GOOS]
	if want != "" && !strings.Contains(got, want) {
		t.Errorf("Describe() = %q, want it to mention %q", got, want)
	}
	if got == "" {
		t.Error("Describe() returned nothing for the recovery file")
	}
}
