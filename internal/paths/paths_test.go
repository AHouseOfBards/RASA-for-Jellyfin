package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// files returns every path the layout names, so a test can assert something
// about all of them at once rather than listing them and going out of date.
func files(l Layout) map[string]string {
	return map[string]string{
		"state":     l.StateFile(),
		"lock":      l.LockFile(),
		"recovery":  l.RecoveryFile(),
		"rasa log":  l.RASALog(),
		"caddy log": l.CaddyLog(),
		"access":    l.CaddyAccessLog(),
		"sync log":  l.SyncLog(),
		"health":    l.LastSyncFile(),
		"alerts":    l.AlertStateFile(),
		"caddyfile": l.CaddyfilePath(),
		"env":       l.EnvFile(),
		"secret":    l.SecretFile(),
	}
}

// The whole point of this package: the uninstaller removes the application and
// must leave logs, state and the recovery file behind, because that is exactly
// when they become most valuable.
func TestNothingLivesInTheInstallDirectory(t *testing.T) {
	l := Default()
	roots := []string{l.Root, l.LogDir, l.StateDir, l.SecretDir}
	for name, p := range files(l) {
		var inside bool
		for _, r := range roots {
			if strings.HasPrefix(filepath.Clean(p), filepath.Clean(r)) {
				inside = true
			}
		}
		if !inside {
			t.Errorf("%s (%s) is outside every directory the layout owns", name, p)
		}
	}
}

func TestEveryPathIsDistinct(t *testing.T) {
	seen := map[string]string{}
	for name, p := range files(UnderRoot(t.TempDir())) {
		if prev, dup := seen[p]; dup {
			t.Errorf("%s and %s are the same file: %s", prev, name, p)
		}
		seen[p] = name
	}
}

// A credential must never land in the directory a diagnostic bundle collects
// from, or in the one the recovery file tells a user to open.
func TestSecretsAreNotInTheLogOrStateDirectory(t *testing.T) {
	l := Default()
	for _, p := range []string{l.SecretFile(), l.EnvFile()} {
		if dir := filepath.Dir(p); dir == filepath.Clean(l.LogDir) {
			t.Errorf("%s sits in the log directory", p)
		}
		if runtime.GOOS != "windows" && filepath.Dir(p) == filepath.Clean(l.StateDir) {
			// On Windows everything shares ProgramData\RASA except secrets,
			// which get their own subdirectory; on Linux they are in /etc.
			t.Errorf("%s sits in the state directory", p)
		}
	}
}

func TestEnsureDirsCreatesEverythingAndTightensSecrets(t *testing.T) {
	l := UnderRoot(t.TempDir())
	if err := l.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{l.Root, l.LogDir, l.StateDir, l.SecretDir, l.BinDir(), l.CaddyDataDir()} {
		fi, err := os.Stat(d)
		if err != nil {
			t.Errorf("%s was not created: %v", d, err)
			continue
		}
		if !fi.IsDir() {
			t.Errorf("%s is not a directory", d)
		}
	}

	if runtime.GOOS == "windows" {
		// Permissions are a no-op there; DPAPI provides the actual protection.
		return
	}
	fi, err := os.Stat(l.SecretDir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("secret directory is %v, want no group or world access", perm)
	}
}

// Running it twice must not fail: setup is safe to re-run, and a repair on a
// machine that already has these directories is the common case.
func TestEnsureDirsIsSafeToRepeat(t *testing.T) {
	l := UnderRoot(t.TempDir())
	for i := 0; i < 3; i++ {
		if err := l.EnsureDirs(); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
}

// The binaries that outlive RASA go here, so it must not be somewhere an
// uninstaller would reasonably sweep.
func TestTheDurableBinariesAreNotUnderTheLogDirectory(t *testing.T) {
	l := Default()
	if strings.HasPrefix(filepath.Clean(l.BinDir()), filepath.Clean(l.LogDir)) {
		t.Errorf("BinDir %s is inside LogDir %s", l.BinDir(), l.LogDir)
	}
	if strings.HasPrefix(filepath.Clean(l.CaddyDataDir()), filepath.Clean(l.LogDir)) {
		t.Errorf("certificate storage %s is inside LogDir %s", l.CaddyDataDir(), l.LogDir)
	}
}

// UnderRoot is what -root gives a developer, and what container mode uses. It
// has to keep everything inside the directory it was handed.
func TestUnderRootKeepsEverythingInsideIt(t *testing.T) {
	root := t.TempDir()
	l := UnderRoot(root)
	for name, p := range files(l) {
		if !strings.HasPrefix(filepath.Clean(p), filepath.Clean(root)) {
			t.Errorf("%s escaped the root: %s", name, p)
		}
	}
}

func TestWindowsFallsBackWhenProgramDataIsUnset(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows only")
	}
	t.Setenv("ProgramData", "")
	if got := Default().Root; !strings.HasPrefix(got, `C:\ProgramData`) {
		t.Errorf("root = %q, want the documented fallback", got)
	}
}
