package service

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
)

// FirewallRuleName is the inbound rule RASA maintains.
const FirewallRuleName = "RASA for Jellyfin"

// AllowProgram permits inbound connections to one executable.
//
// Scoped to the program rather than to a port, for two reasons. The listener
// port is not settled until the mode router has run and may still move to 8443
// (SPEC.md §9), so a port-scoped rule would have to be written twice or written
// wrongly. And a program-scoped rule is tighter: it admits connections to this
// proxy, not to whatever else might later bind the same port.
//
// It is written here rather than by the installer because the installer does
// not know where the binary will end up. RASA copies it out of the install
// directory so that uninstalling the wizard does not remove the running proxy,
// and the rule has to name the copy that actually runs.
func AllowProgram(ctx context.Context, exePath string, log *logging.Logger) error {
	if exePath == "" {
		return fmt.Errorf("no program to allow through the firewall")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Deleted first so a re-run replaces the rule rather than accumulating
	// duplicates: netsh happily adds a second rule with the same name, and a
	// machine that has run setup five times ends up with five.
	del := exec.CommandContext(ctx, "netsh", "advfirewall", "firewall", "delete", "rule",
		"name="+FirewallRuleName)
	if out, err := del.CombinedOutput(); err != nil && log != nil {
		// Deleting a rule that does not exist fails, which is the normal case
		// on a first run. Debug, not warn.
		log.Debug("no existing firewall rule to replace", slog.String("detail", strings.TrimSpace(string(out))))
	}

	add := exec.CommandContext(ctx, "netsh", "advfirewall", "firewall", "add", "rule",
		"name="+FirewallRuleName,
		"dir=in", "action=allow",
		"program="+exePath,
		"enable=yes", "profile=any",
	)
	out, err := add.CombinedOutput()
	text := strings.ToLower(strings.TrimSpace(string(out)))
	if strings.Contains(text, "requires elevation") {
		return ErrNeedsPrivileges
	}
	if err != nil {
		return fmt.Errorf("adding a firewall rule for %s: %w: %s", exePath, err, text)
	}
	if log != nil {
		log.Info("firewall rule in place", slog.String("program", exePath))
	}
	return nil
}

// RemoveFirewallRule deletes the rule. Part of removing remote access, never
// part of uninstalling the wizard — the proxy keeps running after that.
func RemoveFirewallRule(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "netsh", "advfirewall", "firewall", "delete", "rule",
		"name="+FirewallRuleName)
	out, err := cmd.CombinedOutput()
	text := strings.ToLower(strings.TrimSpace(string(out)))

	// netsh reports both of the interesting outcomes on stdout with exit
	// status 0, so the text is the only signal there is.
	switch {
	case strings.Contains(text, "requires elevation"):
		return ErrNeedsPrivileges
	case strings.Contains(text, "no rules match"):
		// Removing a rule that is not there is what a second removal looks
		// like, and it succeeded at the thing that was asked for.
		return nil
	case err != nil:
		return fmt.Errorf("removing the firewall rule: %w: %s", err, text)
	}
	return nil
}
