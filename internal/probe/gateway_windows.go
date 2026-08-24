//go:build windows

package probe

import (
	"context"
	"net/netip"
	"os/exec"
	"strings"
	"time"
)

// defaultGateway finds the LAN address of the default route.
//
// Used only as a fallback: when SSDP succeeds, the address the router replied
// from is more reliable than a routing table, because it is the interface the
// gateway actually answers on.
func defaultGateway(ctx context.Context) (netip.Addr, bool) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Lowest metric wins, matching how Windows itself picks the route.
	const ps = `(Get-NetRoute -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue |` +
		` Sort-Object RouteMetric | Select-Object -First 1).NextHop`

	out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", ps).Output()
	if err != nil {
		return netip.Addr{}, false
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(string(out)))
	if err != nil || addr.IsUnspecified() {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}
