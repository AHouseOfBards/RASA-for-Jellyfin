// Package recovery writes the two artifacts that outlive RASA.
//
// SPEC.md §15: RASA deletes itself, there is no backend, and the things it
// installs run for years. So a warning shown once on a summary screen is gone
// by the time it matters — which may be months later, after a router reboot,
// when remote access has stopped and the user has no idea why.
//
// The recovery file is what they find instead. The diagnostic bundle is the
// only support channel that exists.
package recovery

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/paths"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/state"
)

// Info is everything the recovery file records.
type Info struct {
	State  *state.State
	Layout paths.Layout
	// ForwardingText is the rendered port-forwarding guide, if the user had to
	// do it by hand.
	ForwardingText string
	// ServiceMechanism describes how the persistent parts were installed.
	ServiceMechanism string
	Version          string
}

// WriteFile writes the plain-text recovery file.
//
// Deliberately plain text, not JSON or HTML: it must be readable by someone
// who opens it in Notepad a year from now with no context and no tooling.
func WriteFile(info Info) error {
	path := info.Layout.RecoveryFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(Render(info)), 0o644)
}

// Render builds the recovery file contents.
func Render(info Info) string {
	var b strings.Builder
	st := info.State

	rule := strings.Repeat("=", 64)
	b.WriteString(rule + "\n")
	b.WriteString("  REMOTE ACCESS FOR JELLYFIN — set up by RASA\n")
	b.WriteString(rule + "\n\n")

	if st != nil && st.URL() != "" {
		fmt.Fprintf(&b, "  Your address:  %s\n\n", st.URL())
	}
	fmt.Fprintf(&b, "  Set up on:     %s\n", time.Now().Format("2 January 2006"))
	if info.Version != "" {
		fmt.Fprintf(&b, "  RASA version:  %s\n", info.Version)
	}
	fmt.Fprintf(&b, "  This computer: %s\n\n", runtime.GOOS)

	b.WriteString("  You can uninstall the RASA app. Remote access keeps working\n")
	b.WriteString("  without it — this file explains what was left behind.\n\n")

	// What is still installed. Someone deciding whether to remove things
	// needs to know what they are looking at.
	b.WriteString(rule + "\n  WHAT IS STILL RUNNING\n" + rule + "\n\n")
	if info.ServiceMechanism != "" {
		fmt.Fprintf(&b, "  Installed as %s.\n\n", info.ServiceMechanism)
	}
	b.WriteString("  1. A secure proxy (Caddy) that handles the connection and\n")
	b.WriteString("     renews its own security certificate. Nothing to maintain.\n\n")
	b.WriteString("  2. A scheduled task that keeps your address pointed at this\n")
	b.WriteString("     connection when your internet provider changes it.\n\n")
	b.WriteString("  3. Settings written into Jellyfin itself.\n\n")

	if st != nil && len(st.Warnings) > 0 {
		// The whole reason this file exists: warnings matter later, and RASA
		// will not be here to repeat them.
		b.WriteString(rule + "\n  THINGS TO KNOW\n" + rule + "\n\n")
		for _, w := range st.Warnings {
			b.WriteString("  - " + wrap(w.Text, 62, "    ") + "\n\n")
		}
	}

	if strings.TrimSpace(info.ForwardingText) != "" {
		b.WriteString(rule + "\n  YOUR ROUTER SETTINGS\n" + rule + "\n\n")
		b.WriteString("  Keep these. You will need them if you replace your router\n")
		b.WriteString("  or reset it to factory settings.\n\n")
		b.WriteString(indent(info.ForwardingText, "  "))
		b.WriteString("\n")
	}

	b.WriteString(rule + "\n  IF REMOTE ACCESS STOPS WORKING\n" + rule + "\n\n")
	b.WriteString("  Check these three things, in order:\n\n")
	b.WriteString("  1. Did your router restart recently?\n")
	b.WriteString("     Some routers forget port forwarding when they restart.\n")
	b.WriteString("     Re-enter the router settings above.\n\n")
	b.WriteString("  2. Is the address still up to date?\n")
	fmt.Fprintf(&b, "     Open: %s\n", info.Layout.LastSyncFile())
	b.WriteString("     It shows when the address was last checked. If the date\n")
	b.WriteString("     is old or it says FAILED, the scheduled task has stopped.\n\n")
	b.WriteString("  3. Is the proxy still running?\n")
	fmt.Fprintf(&b, "     Its log is at: %s\n\n", info.Layout.CaddyLog())
	b.WriteString("  Re-running the RASA setup app fixes all three. It will detect\n")
	b.WriteString("  what is already set up and repair only what is broken.\n\n")

	b.WriteString(rule + "\n  FILES AND LOGS\n" + rule + "\n\n")
	fmt.Fprintf(&b, "  Setup log:     %s\n", info.Layout.RASALog())
	fmt.Fprintf(&b, "  Proxy log:     %s\n", info.Layout.CaddyLog())
	fmt.Fprintf(&b, "  Address sync:  %s\n", info.Layout.SyncLog())
	fmt.Fprintf(&b, "  Last sync:     %s\n", info.Layout.LastSyncFile())
	fmt.Fprintf(&b, "  Setup record:  %s\n\n", info.Layout.StateFile())
	b.WriteString("  These are kept on purpose when RASA is uninstalled — they are\n")
	b.WriteString("  what makes a problem diagnosable later.\n\n")

	b.WriteString(rule + "\n")
	b.WriteString("  Report problems: https://github.com/AHouseOfBards/RASA-for-Jellyfin/issues\n")
	b.WriteString(rule + "\n")
	return b.String()
}

// BundleOptions configures a diagnostic bundle.
type BundleOptions struct {
	Layout paths.Layout
	// IncludeAddresses controls whether the hostname and public address are
	// left readable. Off by default: the bundle is destined for a public
	// issue, and those values identify the user's home server.
	IncludeAddresses bool
	Redactor         *logging.Redactor
	Version          string
	// MaxLogBytes caps how much of each log is copied, newest first.
	MaxLogBytes int64
}

// DefaultMaxLogBytes keeps a bundle attachable to an issue.
const DefaultMaxLogBytes = 2 << 20 // 2 MiB

// WriteBundle produces a redacted zip of everything needed to diagnose a
// problem, and returns its path.
//
// Because RASA may already be uninstalled when this is needed, everything it
// gathers is read from the shared directory rather than from the app.
func WriteBundle(destDir string, o BundleOptions) (string, error) {
	if o.MaxLogBytes <= 0 {
		o.MaxLogBytes = DefaultMaxLogBytes
	}
	red := o.Redactor
	if red == nil {
		red = logging.NewRedactor()
	}
	red.SetRedactAddresses(!o.IncludeAddresses)

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("rasa-diagnostics-%s.zip", time.Now().Format("2006-01-02-1504"))
	path := filepath.Join(destDir, name)

	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	defer zw.Close()

	if err := addText(zw, "README.txt", bundleReadme(o)); err != nil {
		return "", err
	}
	if err := addText(zw, "environment.txt", environmentReport(o.Version)); err != nil {
		return "", err
	}

	for _, item := range []struct{ name, src string }{
		{"rasa.log", o.Layout.RASALog()},
		{"caddy.log", o.Layout.CaddyLog()},
		{"sync.log", o.Layout.SyncLog()},
		{"last-sync.txt", o.Layout.LastSyncFile()},
		{"state.json", o.Layout.StateFile()},
		{"remote-access-info.txt", o.Layout.RecoveryFile()},
	} {
		// A missing file is normal — Caddy may not be installed yet — and must
		// not abort the bundle the user is trying to send.
		if err := addRedactedFile(zw, item.name, item.src, red, o.MaxLogBytes); err != nil {
			_ = addText(zw, item.name+".missing", fmt.Sprintf("could not be read: %v\n", err))
		}
	}
	return path, nil
}

func bundleReadme(o BundleOptions) string {
	var b strings.Builder
	b.WriteString("RASA for Jellyfin — diagnostic bundle\n")
	b.WriteString("=====================================\n\n")
	fmt.Fprintf(&b, "Created: %s\n\n", time.Now().Format(time.RFC3339))
	b.WriteString("Attach this file to an issue at:\n")
	b.WriteString("  https://github.com/AHouseOfBards/RASA-for-Jellyfin/issues\n\n")
	b.WriteString("Passwords and API keys have been removed from these files.\n")
	if o.IncludeAddresses {
		b.WriteString("\nYour web address and internet address ARE included, because you\n")
		b.WriteString("chose to include them. They identify your server to anyone who\n")
		b.WriteString("reads the issue. Remove them by hand if you change your mind.\n")
	} else {
		b.WriteString("Your web address and internet address have also been hidden.\n")
		b.WriteString("If a problem turns out to need them, you can create a new bundle\n")
		b.WriteString("with them included.\n")
	}
	return b.String()
}

func environmentReport(version string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "rasa version: %s\n", version)
	fmt.Fprintf(&b, "os:           %s\n", runtime.GOOS)
	fmt.Fprintf(&b, "arch:         %s\n", runtime.GOARCH)
	fmt.Fprintf(&b, "go:           %s\n", runtime.Version())
	fmt.Fprintf(&b, "time:         %s\n", time.Now().Format(time.RFC3339))

	env := os.Environ()
	sort.Strings(env)
	b.WriteString("\nrelevant environment:\n")
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		// Names only, never values: an environment variable is exactly where
		// the Dynu credential lives.
		if strings.HasPrefix(strings.ToUpper(k), "RASA_") {
			fmt.Fprintf(&b, "  %s is set\n", k)
		}
	}
	return b.String()
}

func addText(zw *zip.Writer, name, content string) error {
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, content)
	return err
}

// addRedactedFile copies a file through the redactor, keeping the tail when it
// exceeds the cap — recent events are what matter for diagnosis.
func addRedactedFile(zw *zip.Writer, name, src string, red *logging.Redactor, max int64) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()

	var truncated bool
	if info.Size() > max {
		if _, err := f.Seek(info.Size()-max, io.SeekStart); err != nil {
			return err
		}
		truncated = true
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return err
	}

	body := red.Redact(string(raw))
	if truncated {
		body = fmt.Sprintf("... earlier entries omitted (%d bytes) ...\n%s", info.Size()-max, body)
	}
	return addText(zw, name, body)
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) != "" {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

// wrap breaks text to width, indenting continuation lines.
func wrap(s string, width int, contIndent string) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	var out strings.Builder
	line := words[0]
	for _, w := range words[1:] {
		if len(line)+1+len(w) > width {
			out.WriteString(line + "\n" + contIndent)
			line = w
			continue
		}
		line += " " + w
	}
	out.WriteString(line)
	return out.String()
}
