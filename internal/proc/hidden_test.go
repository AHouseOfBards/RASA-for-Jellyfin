package proc_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Anything RASA starts and reads the output of must go through proc.Hidden.
//
// On Windows the wizard is built for the GUI subsystem, so starting a console
// program throws a real window onto the screen for as long as it runs. Two
// PowerShell calls in the probe did exactly that, at startup and again on every
// "Test again" -- reported as "two big blue blank windows" by someone who
// reasonably read them as a crash.
//
// This is a source scan rather than a runtime check because the failure only
// happens on Windows, only in a GUI-subsystem build, and only where a human is
// watching: nothing else in the suite would ever catch it.
func TestEverySubprocessIsHidden(t *testing.T) {
	// The launchers are the exception, and the only one. Opening a browser and
	// starting the uninstaller exist to put something on screen; hiding those
	// would defeat the point.
	allowed := map[string]bool{
		filepath.FromSlash("internal/ui/open.go"):   true,
		filepath.FromSlash("internal/ui/server.go"): true,
	}

	// exec.Command(...) or exec.CommandContext(...) whose result is used
	// directly, rather than being handed to proc.
	call := regexp.MustCompile(`exec\.Command(Context)?\(`)
	wrapped := regexp.MustCompile(`proc\.(Hidden|Run|Output|CombinedOutput)\(`)

	root := filepath.Join("..", "..")
	var offenders []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if name := info.Name(); name == ".git" || name == "dist" || name == ".devdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if allowed[rel] || strings.HasPrefix(rel, filepath.FromSlash("internal/proc")) {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// A file that cannot compile for Windows cannot show a Windows console
		// window. Read the build constraint rather than guessing from the
		// filename, so a renamed file is still judged correctly.
		if excludesWindows(string(b)) {
			return nil
		}
		for n, line := range strings.Split(string(b), "\n") {
			if call.MatchString(line) && !wrapped.MatchString(line) {
				// Two-line form: cmd := exec.Command(...) on its own, used
				// through proc later in the same file, is fine.
				if strings.Contains(line, ":=") && wrapped.Match(b) {
					continue
				}
				offenders = append(offenders, rel+":"+itoa(n+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(offenders) > 0 {
		t.Errorf("these start a program without proc.Hidden, so on Windows each one flashes a console window:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// excludesWindows reports whether a file's build constraint keeps it off
// Windows entirely.
func excludesWindows(src string) bool {
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "//go:build") {
			expr := strings.TrimPrefix(line, "//go:build")
			// "!windows", and equally "linux" or "unix": a positive constraint
			// that never names windows is not built there either.
			return strings.Contains(expr, "!windows") || !strings.Contains(expr, "windows")
		}
		// The constraint, if there is one, comes before the package clause.
		if strings.HasPrefix(line, "package ") {
			return false
		}
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
