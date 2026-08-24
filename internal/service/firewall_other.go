//go:build !windows

package service

import (
	"context"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
)

// FirewallRuleName is the inbound rule RASA maintains, where the platform has
// one RASA knows how to manage.
const FirewallRuleName = "RASA for Jellyfin"

// AllowProgram is a no-op outside Windows.
//
// Linux hosts run ufw, firewalld, nftables, plain iptables, or nothing at all,
// and a distribution that ships a firewall enabled by default is the exception
// rather than the rule. Writing rules blind into whichever of those happens to
// be present is a good way to break a machine's networking on behalf of a user
// who never asked; the recovery file names the port instead, which is the same
// answer a hosting guide would give.
func AllowProgram(ctx context.Context, exePath string, log *logging.Logger) error {
	if log != nil {
		log.Debug("no firewall rule written on this platform")
	}
	return nil
}

// RemoveFirewallRule is a no-op outside Windows, matching AllowProgram.
func RemoveFirewallRule(ctx context.Context) error { return nil }
