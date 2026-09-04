package service

import (
	"strings"
	"testing"
	"time"
)

// syncTimer is the timer RASA actually installs on Linux: the credential comes
// from a root-owned file, never from the unit.
func syncTimer() Timer {
	t := SyncTimerDefinition("/usr/local/lib/rasa/rasa-sync", nil)
	t.EnvironmentFile = "/etc/rasa/rasa.env"
	return t
}

func lines(s string) []string { return strings.Split(s, "\n") }

func hasLine(s, want string) bool {
	for _, l := range lines(s) {
		if strings.TrimSpace(l) == want {
			return true
		}
	}
	return false
}

// The bug this package had no test for. Timer.EnvironmentFile is documented as
// "how the DDNS token stays out of a world-readable unit file", the installer
// sets it on every Linux run, and the generated unit dropped it on the floor —
// so the sync task was registered with no credential at all.
func TestTheTimerUnitCarriesTheCredentialFile(t *testing.T) {
	unit := renderTimerService(syncTimer())
	if !hasLine(unit, "EnvironmentFile=/etc/rasa/rasa.env") {
		t.Errorf("the timer's service unit does not reference the credential file:\n%s", unit)
	}
}

// The reason the file is used at all: a systemd unit is world-readable, and
// `systemctl show` prints Environment= values to anyone who asks.
func TestASecretIsNeverBakedIntoAUnitWhenAFileIsGiven(t *testing.T) {
	tm := syncTimer()
	unit := renderTimerService(tm)
	if strings.Contains(unit, "hunter2") {
		t.Fatalf("a secret reached the unit file:\n%s", unit)
	}

	// And the same for a long-running service.
	d := CaddyDefinition("/usr/local/lib/rasa/caddy", "/var/lib/rasa/Caddyfile", "/var/lib/rasa", nil)
	d.EnvironmentFile = "/etc/rasa/rasa.env"
	if !hasLine(renderServiceUnit(d), "EnvironmentFile=/etc/rasa/rasa.env") {
		t.Errorf("the proxy unit does not reference the credential file:\n%s", renderServiceUnit(d))
	}
}

// Re-running setup must produce the same file, or every run looks like a
// change to anyone diffing /etc.
func TestUnitsAreByteIdenticalAcrossRuns(t *testing.T) {
	d := CaddyDefinition("/usr/local/lib/rasa/caddy", "/var/lib/rasa/Caddyfile", "/var/lib/rasa",
		map[string]string{"B": "2", "A": "1", "C": "3"})
	first := renderServiceUnit(d)
	for i := 0; i < 20; i++ {
		if got := renderServiceUnit(d); got != first {
			t.Fatalf("unit changed between renders:\n--- first ---\n%s\n--- later ---\n%s", first, got)
		}
	}
}

func TestTheProxyUnitCanBindLowPortsWithoutRoot(t *testing.T) {
	unit := renderServiceUnit(CaddyDefinition("/usr/local/lib/rasa/caddy", "/var/lib/rasa/Caddyfile", "", nil))
	// Without this the proxy cannot listen on 443 at all, which is the entire
	// product.
	if !hasLine(unit, "AmbientCapabilities=CAP_NET_BIND_SERVICE") {
		t.Errorf("the proxy unit cannot bind port 443:\n%s", unit)
	}
	if !hasLine(unit, "WantedBy=multi-user.target") {
		t.Errorf("the proxy unit will not start at boot:\n%s", unit)
	}
}

func TestTheTimerRunsAtBootAndOnItsInterval(t *testing.T) {
	unit := renderTimerUnit(syncTimer())
	// A WAN address most often changes across a reboot, so a timer that only
	// fires on its interval leaves the record stale exactly when it is wrong.
	if !hasLine(unit, "OnBootSec=1min") {
		t.Errorf("no boot trigger:\n%s", unit)
	}
	if !hasLine(unit, "OnUnitActiveSec=600s") {
		t.Errorf("interval is not the ten minutes SyncInterval declares:\n%s", unit)
	}
	if !hasLine(unit, "Persistent=true") {
		t.Errorf("a missed run will not be caught up after downtime:\n%s", unit)
	}
}

// A sub-minute interval would make systemd spin. The floor is not cosmetic.
func TestASillyIntervalIsClampedRatherThanWritten(t *testing.T) {
	tm := syncTimer()
	tm.Interval = time.Second
	if !hasLine(renderTimerUnit(tm), "OnUnitActiveSec=60s") {
		t.Errorf("a one-second interval was written through:\n%s", renderTimerUnit(tm))
	}
}

// ---- command lines ------------------------------------------------------

// "C:\Program Files\..." is the default install location for most of what RASA
// touches, and an unquoted path breaks sc.exe and systemd in the same way.
func TestAPathWithSpacesIsQuotedInEveryCommandLine(t *testing.T) {
	exe := `C:\Program Files\RASA\caddy.exe`
	cfg := `C:\ProgramData\RASA\Caddyfile`
	d := CaddyDefinition(exe, cfg, "", nil)

	args := scCreateArgs(d)
	bin := valueAfter(args, "binPath=")
	if !strings.HasPrefix(bin, `"`+exe+`"`) {
		t.Errorf("binPath does not quote the executable: %q", bin)
	}
	if !strings.Contains(bin, "run --config") {
		t.Errorf("binPath lost the arguments: %q", bin)
	}
	if strings.Contains(bin, `""`) {
		t.Errorf("binPath double-quoted something: %q", bin)
	}
}

// The empty-looking "binPath=" followed by a separate value is how sc.exe's
// parser wants it. Collapsing them into "binPath=<value>" silently creates a
// service with no command.
func TestScArgumentsKeepTheSpaceAfterEachEquals(t *testing.T) {
	args := scCreateArgs(CaddyDefinition("caddy", "Caddyfile", "", nil))
	for _, want := range []string{"binPath=", "start=", "DisplayName="} {
		if !contains(args, want) {
			t.Errorf("%q is not a separate argument: %v", want, args)
		}
	}
	if valueAfter(args, "start=") != "auto" {
		t.Errorf("the service will not start at boot: %v", args)
	}
}

func TestTheScheduledTaskRunsAsSystemEveryTenMinutes(t *testing.T) {
	args := schtasksCreateArgs(SyncTimerDefinition(`C:\ProgramData\RASA\bin\rasa-sync.exe`, nil))
	// SYSTEM so it works with nobody signed in, and so it can read the
	// machine-scope DPAPI credential.
	if valueAfter(args, "/RU") != "SYSTEM" {
		t.Errorf("not running as SYSTEM: %v", args)
	}
	if valueAfter(args, "/MO") != "10" {
		t.Errorf("interval is not the ten minutes SyncInterval declares: %v", args)
	}
	if valueAfter(args, "/SC") != "MINUTE" {
		t.Errorf("schedule is not minute-based: %v", args)
	}
	if !contains(args, "/F") {
		t.Errorf("re-running setup would fail on an existing task: %v", args)
	}
}

// Uninstall walks the same suffix. If the two ever disagree, removing remote
// access leaves a task behind that runs a binary the uninstaller deleted.
func TestTheBootTaskNameMatchesWhatRemovalLooksFor(t *testing.T) {
	tm := SyncTimerDefinition("rasa-sync.exe", nil)
	got := valueAfter(schtasksBootArgs(tm), "/TN")
	if want := tm.Name + bootTaskSuffix; got != want {
		t.Errorf("boot task is named %q, want %q", got, want)
	}
	if valueAfter(schtasksBootArgs(tm), "/SC") != "ONSTART" {
		t.Errorf("the boot task is not a boot trigger: %v", schtasksBootArgs(tm))
	}
}

// ---- sc.exe status parsing ----------------------------------------------

func TestServiceStatusReadsRealScOutput(t *testing.T) {
	running := `SERVICE_NAME: RASACaddy
        TYPE               : 10  WIN32_OWN_PROCESS
        STATE              : 4  RUNNING
                                (STOPPABLE, PAUSABLE, ACCEPTS_SHUTDOWN)`
	stopped := strings.Replace(running, "4  RUNNING", "1  STOPPED", 1)

	if got := parseServiceStatus(running, nil); got != StatusRunning {
		t.Errorf("running service read as %q", got)
	}
	if got := parseServiceStatus(stopped, nil); got != StatusStopped {
		t.Errorf("stopped service read as %q", got)
	}
}

// "Does not exist" is an answer, not a failure. Reading it as unknown makes an
// install refuse to replace a service that was never there.
func TestAnAbsentServiceIsNotPresentRatherThanUnknown(t *testing.T) {
	out := "[SC] EnumQueryServicesStatus:OpenService FAILED 1060:\n\nThe specified service does not exist as an installed service."
	if got := parseServiceStatus(out, errFake); got != StatusNotPresent {
		t.Errorf("absent service read as %q, want %q", got, StatusNotPresent)
	}
}

// Anything else must stay unknown. Guessing "stopped" here would make the
// installer try to start a service it cannot see.
func TestAnUnreadableAnswerStaysUnknown(t *testing.T) {
	if got := parseServiceStatus("[SC] OpenSCManager FAILED 5:\n\nAccess is denied.", errFake); got != StatusUnknown {
		t.Errorf("access-denied read as %q, want %q", got, StatusUnknown)
	}
}

// ---- helpers -------------------------------------------------------------

var errFake = &fakeErr{}

type fakeErr struct{}

func (*fakeErr) Error() string { return "exit status 1" }

func contains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func valueAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
