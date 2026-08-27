package caddy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// fakeCaddy compiles a stand-in that prints the given module list.
//
// Compiled rather than scripted because Windows is the priority platform and a
// shell script would skip there, which is precisely where "we only tested this
// on Linux" has bitten this project before.
func fakeCaddy(t *testing.T, modules string) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	program := "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Print(" + strconv.Quote(modules) + ") }\n"
	if err := os.WriteFile(src, []byte(program), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "caddy")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", out, src)
	// Built from the temp directory, not the package under test: a file passed
	// by path while the working directory sits inside this module is rejected as
	// not belonging to it.
	cmd.Dir = dir
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the stand-in: %v\n%s", err, b)
	}
	return out
}

// A machine that already runs Caddy for something else has a stock build on
// PATH, and FindBinary falls back to it. Nothing goes wrong until the
// configuration is validated -- minutes later, after a hostname is claimed and
// a DNS record has propagated -- so the wrong binary has to be recognisable
// before any of that.
func TestStockCaddyIsRecognisedAsMissingModules(t *testing.T) {
	bin := fakeCaddy(t, "dns.providers.cloudflare\nhttp.handlers.file_server\nhttp.handlers.reverse_proxy\n")

	missing, err := MissingModules(context.Background(), bin)
	if err != nil {
		t.Fatalf("MissingModules: %v", err)
	}
	if len(missing) != len(RequiredModules) {
		t.Errorf("missing = %v, want both of %v", missing, RequiredModules)
	}
}

func TestCorrectlyBuiltCaddyReportsNothingMissing(t *testing.T) {
	bin := fakeCaddy(t, "dns.providers.dynu\nhttp.handlers.rate_limit\nhttp.handlers.reverse_proxy\n")

	missing, err := MissingModules(context.Background(), bin)
	if err != nil {
		t.Fatalf("MissingModules: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("missing = %v, want none", missing)
	}
}

// One module present and one absent is the likeliest real failure: a build made
// with the dynu plugin and not the rate limiter.
func TestPartiallyBuiltCaddyNamesOnlyWhatIsMissing(t *testing.T) {
	bin := fakeCaddy(t, "dns.providers.dynu\nhttp.handlers.reverse_proxy\n")

	missing, err := MissingModules(context.Background(), bin)
	if err != nil {
		t.Fatalf("MissingModules: %v", err)
	}
	if len(missing) != 1 || missing[0] != "http.handlers.rate_limit" {
		t.Errorf("missing = %v, want just the rate limiter", missing)
	}
}

// Being unable to ask is not the same as the modules being absent, and must
// not be reported as if it were.
func TestUnaskableBinaryIsAnErrorNotAnAbsence(t *testing.T) {
	missing, err := MissingModules(context.Background(), filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("expected an error for a binary that cannot be run")
	}
	if missing != nil {
		t.Errorf("missing = %v, want nil: a failure to ask is not an answer", missing)
	}
}
