//go:build windows

package probe

import (
	"context"
	"net/netip"
	"os/exec"
	"strings"
	"time"
)

// addressIsDHCP reports whether the address came from a DHCP lease.
//
// Windows records this directly as the address's PrefixOrigin, so no parsing
// of ipconfig output is needed. Anything other than a confident "Manual" is
// treated as DHCP: see ProbeHost for why the unknown case leans that way.
func addressIsDHCP(ctx context.Context, addr netip.Addr) bool {
	if !addr.IsValid() {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command",
		"(Get-NetIPAddress -IPAddress '"+addr.String()+"' -ErrorAction SilentlyContinue).PrefixOrigin")
	out, err := cmd.Output()
	if err != nil {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(string(out))) {
	case "manual":
		return false
	case "dhcp":
		return true
	}
	return true
}
