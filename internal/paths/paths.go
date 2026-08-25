// Package paths resolves the on-disk locations RASA uses.
//
// These locations are deliberately NOT under the install directory. RASA is a
// disposable installer (SPEC.md §3): the uninstaller removes the application
// but must leave logs, state, and the recovery file behind, because that is
// exactly when they become most valuable (SPEC.md §15).
package paths

import (
	"os"
	"path/filepath"
	"runtime"
)

// Layout is the set of directories RASA and the services it installs share.
//
// Caddy and the DDNS sync task are pointed at LogDir so that all three log
// sources land in one place a user can be directed to months later.
type Layout struct {
	// Root is the base directory holding everything below.
	Root string
	// LogDir holds rasa.log, caddy.log and sync.log.
	LogDir string
	// StateDir holds state.json and the plain-text recovery file.
	StateDir string
	// SecretDir holds the protected credential file.
	SecretDir string
}

// Default returns the standard layout for the current OS.
func Default() Layout {
	switch runtime.GOOS {
	case "windows":
		// ProgramData survives uninstall and is readable by the service
		// accounts Caddy and the scheduled task run under.
		root := filepath.Join(programData(), "RASA")
		return Layout{
			Root:      root,
			LogDir:    filepath.Join(root, "logs"),
			StateDir:  root,
			SecretDir: filepath.Join(root, "secrets"),
		}
	default:
		return Layout{
			Root:      "/var/lib/rasa",
			LogDir:    "/var/log/rasa",
			StateDir:  "/var/lib/rasa",
			SecretDir: "/etc/rasa",
		}
	}
}

// UnderRoot returns a layout rooted at dir. Used by tests and by container
// mode, where /var/lib is not necessarily writable.
func UnderRoot(dir string) Layout {
	return Layout{
		Root:      dir,
		LogDir:    filepath.Join(dir, "logs"),
		StateDir:  dir,
		SecretDir: filepath.Join(dir, "secrets"),
	}
}

func programData() string {
	if v := os.Getenv("ProgramData"); v != "" {
		return v
	}
	return `C:\ProgramData`
}

// StateFile is the path to the persisted setup state.
func (l Layout) StateFile() string { return filepath.Join(l.StateDir, "state.json") }

// RecoveryFile is the plain-text file left for the user, holding warnings,
// port-forwarding values and log locations (SPEC.md §6, §15).
func (l Layout) RecoveryFile() string {
	return filepath.Join(l.StateDir, "remote-access-info.txt")
}

// RASALog is RASA's own structured setup log.
func (l Layout) RASALog() string { return filepath.Join(l.LogDir, "rasa.log") }

// CaddyLog is the proxy's runtime log: startup, TLS, and everything about
// certificate issuance. This is the file to read when a certificate fails.
func (l Layout) CaddyLog() string { return filepath.Join(l.LogDir, "caddy.log") }

// CaddyAccessLog receives one line per HTTP request. Kept apart from CaddyLog
// so that request volume cannot bury the handful of lines that explain a
// failed certificate.
func (l Layout) CaddyAccessLog() string { return filepath.Join(l.LogDir, "caddy-access.log") }

// SyncLog is where the scheduled DDNS task writes.
func (l Layout) SyncLog() string { return filepath.Join(l.LogDir, "sync.log") }

// LastSyncFile is the DDNS heartbeat: timestamp, address seen, and result,
// rewritten on every run including successes, so "is this still working?" is
// answerable by opening one file (SPEC.md §15).
func (l Layout) LastSyncFile() string { return filepath.Join(l.StateDir, "last-sync.txt") }

// CaddyfilePath is the generated proxy configuration. It lives beside state
// rather than in the install directory because the Caddy service keeps reading
// it after RASA itself is uninstalled (SPEC.md §3).
func (l Layout) CaddyfilePath() string { return filepath.Join(l.StateDir, "Caddyfile") }

// CaddyDataDir is where Caddy stores issued certificates and its ACME account
// key. Losing it means re-issuing, which spends Let's Encrypt quota, so it is
// somewhere durable rather than a temporary directory.
func (l Layout) CaddyDataDir() string { return filepath.Join(l.Root, "caddy") }

// BinDir holds the executables that outlive RASA: the bundled Caddy and the
// address sync helper. The uninstaller must leave this alone.
func (l Layout) BinDir() string { return filepath.Join(l.Root, "bin") }

// EnvFile holds the credential a service reads at startup. Used on Linux,
// where systemd can read a root-owned 0600 file that the unit itself does not
// expose in its environment listing.
func (l Layout) EnvFile() string { return filepath.Join(l.SecretDir, "rasa.env") }

// SecretFile is the protected credential store.
func (l Layout) SecretFile() string { return filepath.Join(l.SecretDir, "credentials.dat") }

// EnsureDirs creates every directory in the layout.
func (l Layout) EnsureDirs() error {
	for _, d := range []string{l.Root, l.LogDir, l.StateDir, l.SecretDir, l.BinDir(), l.CaddyDataDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	// The secret directory is tightened further; on Windows this is a no-op
	// and DPAPI provides the actual protection.
	return os.Chmod(l.SecretDir, 0o700)
}
