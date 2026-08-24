package recovery

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/paths"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/state"
)

func testInfo(t *testing.T) Info {
	t.Helper()
	st := state.NewState("run-abc")
	st.Phase = state.Running
	st.Hostname = "mymedia.freeddns.org"
	st.ListenPort = 8443
	st.AddWarning("finite_lease", "Your router granted a temporary port opening, so it may be cleared when the router restarts.")
	st.AddWarning("dhcp_unreserved", "This computer's address is not reserved.")

	return Info{
		State:            st,
		Layout:           paths.UnderRoot(t.TempDir()),
		ServiceMechanism: "a Windows Service and a Scheduled Task",
		Version:          "1.0.0",
		ForwardingText:   "PORT FORWARDING — ASUS\n  1. Open http://192.168.1.1\n  External port  8443\n",
	}
}

func TestRecoveryFileCarriesTheEssentials(t *testing.T) {
	txt := Render(testInfo(t))
	for _, want := range []string{
		"https://mymedia.freeddns.org:8443", // the address
		"uninstall",                         // that RASA can go
		"IF REMOTE ACCESS STOPS WORKING",    // the troubleshooting section
		"last-sync.txt",                     // where to check
		"8443",                              // the forwarding values
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("recovery file missing %q", want)
		}
	}
}

func TestWarningsSurviveIntoTheFile(t *testing.T) {
	// The whole reason the file exists: warnings matter months later, when
	// RASA is gone and cannot repeat them.
	txt := Render(testInfo(t))
	if !strings.Contains(txt, "temporary port opening") {
		t.Error("finite lease warning not carried")
	}
	if !strings.Contains(txt, "not reserved") {
		t.Error("dhcp warning not carried")
	}
}

func TestRouterValuesAreKept(t *testing.T) {
	// Needed again if the router is replaced or factory reset.
	txt := Render(testInfo(t))
	if !strings.Contains(txt, "YOUR ROUTER SETTINGS") || !strings.Contains(txt, "192.168.1.1") {
		t.Errorf("forwarding values not preserved:\n%s", txt)
	}
}

func TestForwardingSectionOmittedWhenAutomatic(t *testing.T) {
	info := testInfo(t)
	info.ForwardingText = ""
	if txt := Render(info); strings.Contains(txt, "YOUR ROUTER SETTINGS") {
		t.Error("no manual forwarding was needed, so the section should be absent")
	}
}

func TestNoJargonInRecoveryFile(t *testing.T) {
	// Read by a non-technical person with no context.
	txt := Render(testInfo(t))
	for _, jargon := range []string{"UPnP", "IGD", "CGNAT", "DNS-01", "ACME", "SSDP", "nil"} {
		if strings.Contains(txt, jargon) {
			t.Errorf("recovery file contains jargon %q", jargon)
		}
	}
}

func TestRenderSurvivesEmptyState(t *testing.T) {
	// Written even when setup failed partway, which is when it matters most.
	txt := Render(Info{Layout: paths.UnderRoot(t.TempDir())})
	if strings.TrimSpace(txt) == "" {
		t.Fatal("empty output")
	}
	if !strings.Contains(txt, "IF REMOTE ACCESS STOPS WORKING") {
		t.Error("troubleshooting should be present regardless")
	}
}

func TestWriteFileCreatesIt(t *testing.T) {
	info := testInfo(t)
	if err := WriteFile(info); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(info.Layout.RecoveryFile())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "mymedia.freeddns.org") {
		t.Error("file content wrong")
	}
}

// ---------- diagnostic bundle ----------

func seedLogs(t *testing.T, l paths.Layout, secret, hostname string) {
	t.Helper()
	if err := l.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(l.RASALog(), `{"msg":"calling dynu","token":"`+secret+`","host":"`+hostname+`"}`+"\n")
	write(l.CaddyLog(), `{"msg":"certificate obtained","host":"`+hostname+`"}`+"\n")
	write(l.LastSyncFile(), "last run: 2026-08-24\nresult: unchanged\n")
	write(l.StateFile(), `{"hostname":"`+hostname+`"}`)
}

func readZip(t *testing.T, path string) map[string]string {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out := map[string]string{}
	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		out[f.Name] = string(b)
	}
	return out
}

func TestBundleRedactsSecrets(t *testing.T) {
	// SPEC.md §15: assume every bundle is pasted into a public issue, because
	// it is the only support channel.
	const secret = "dynu-api-key-abcdef123456"
	l := paths.UnderRoot(t.TempDir())
	seedLogs(t, l, secret, "mymedia.freeddns.org")

	red := logging.NewRedactor()
	red.RegisterSecret(secret)

	path, err := WriteBundle(t.TempDir(), BundleOptions{Layout: l, Redactor: red})
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range readZip(t, path) {
		if strings.Contains(body, secret) {
			t.Fatalf("secret leaked in %s", name)
		}
	}
}

func TestBundleHidesAddressesByDefault(t *testing.T) {
	l := paths.UnderRoot(t.TempDir())
	seedLogs(t, l, "s3cr3tvalue123", "mymedia.freeddns.org")

	red := logging.NewRedactor()
	red.RegisterAddress("mymedia.freeddns.org")

	path, err := WriteBundle(t.TempDir(), BundleOptions{Layout: l, Redactor: red})
	if err != nil {
		t.Fatal(err)
	}
	files := readZip(t, path)
	if strings.Contains(files["rasa.log"], "mymedia.freeddns.org") {
		t.Error("hostname should be hidden by default")
	}
	if !strings.Contains(files["README.txt"], "hidden") {
		t.Error("README should say the address was hidden")
	}
}

func TestBundleCanIncludeAddresses(t *testing.T) {
	l := paths.UnderRoot(t.TempDir())
	seedLogs(t, l, "s3cr3tvalue123", "mymedia.freeddns.org")

	red := logging.NewRedactor()
	red.RegisterAddress("mymedia.freeddns.org")

	path, err := WriteBundle(t.TempDir(), BundleOptions{Layout: l, Redactor: red, IncludeAddresses: true})
	if err != nil {
		t.Fatal(err)
	}
	files := readZip(t, path)
	if !strings.Contains(files["rasa.log"], "mymedia.freeddns.org") {
		t.Error("address should be present when explicitly included")
	}
	// And the README must warn about what that means.
	if !strings.Contains(files["README.txt"], "identify your server") {
		t.Error("README should warn about including the address")
	}
}

func TestBundleToleratesMissingFiles(t *testing.T) {
	// Caddy may not be installed yet. A missing file must not abort the bundle
	// the user is trying to send.
	l := paths.UnderRoot(t.TempDir())
	if err := l.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	path, err := WriteBundle(t.TempDir(), BundleOptions{Layout: l})
	if err != nil {
		t.Fatalf("bundle should still be produced: %v", err)
	}
	files := readZip(t, path)
	if _, ok := files["README.txt"]; !ok {
		t.Error("README missing")
	}
	if _, ok := files["caddy.log.missing"]; !ok {
		t.Error("absent file should be noted rather than silently skipped")
	}
}

func TestBundleTruncatesLargeLogsKeepingTheTail(t *testing.T) {
	// Recent events are what matter for diagnosis, and the bundle has to stay
	// attachable to an issue.
	l := paths.UnderRoot(t.TempDir())
	if err := l.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("old line\n", 5000) + "MOST-RECENT-ENTRY\n"
	if err := os.WriteFile(l.RASALog(), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}

	path, err := WriteBundle(t.TempDir(), BundleOptions{Layout: l, MaxLogBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	got := readZip(t, path)["rasa.log"]
	if !strings.Contains(got, "MOST-RECENT-ENTRY") {
		t.Error("the newest entries must be the ones kept")
	}
	if !strings.Contains(got, "earlier entries omitted") {
		t.Error("truncation should be stated, not silent")
	}
}

func TestEnvironmentReportNamesVariablesWithoutValues(t *testing.T) {
	// An environment variable is exactly where the Dynu credential lives.
	t.Setenv("RASA_DYNU_TOKEN", "super-secret-value-123")
	got := environmentReport("1.0.0")

	if !strings.Contains(got, "RASA_DYNU_TOKEN is set") {
		t.Error("variable should be named")
	}
	if strings.Contains(got, "super-secret-value-123") {
		t.Fatal("value leaked into the environment report")
	}
}

func TestBundleFileNameIsDated(t *testing.T) {
	l := paths.UnderRoot(t.TempDir())
	_ = l.EnsureDirs()
	path, err := WriteBundle(t.TempDir(), BundleOptions{Layout: l})
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(path)
	if !strings.HasPrefix(name, "rasa-diagnostics-") || !strings.HasSuffix(name, ".zip") {
		t.Fatalf("unexpected bundle name %q", name)
	}
}

func TestWrapKeepsWordsIntact(t *testing.T) {
	got := wrap("one two three four five six seven eight nine ten", 20, "  ")
	for _, w := range strings.Fields("one two three four five six seven eight nine ten") {
		if !strings.Contains(got, w) {
			t.Errorf("word %q lost in wrapping", w)
		}
	}
	for _, line := range strings.Split(got, "\n") {
		if len(line) > 24 {
			t.Errorf("line too long: %q", line)
		}
	}
}
