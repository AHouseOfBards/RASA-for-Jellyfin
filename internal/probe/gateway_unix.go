//go:build !windows

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

	// `ip route get` asks the kernel which route would actually be used,
	// rather than making us parse and rank the whole table.
	if out, err := exec.CommandContext(ctx, "ip", "route", "get", "192.0.2.1").Output(); err == nil {
		if a, ok := parseAfter(string(out), "via"); ok {
			return a, true
		}
	}
	// BSD and macOS.
	if out, err := exec.CommandContext(ctx, "route", "-n", "get", "default").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "gateway" {
				if addr, err := netip.ParseAddr(strings.TrimSpace(v)); err == nil {
					return addr.Unmap(), true
				}
			}
		}
	}
	return netip.Addr{}, false
}

// parseAfter returns the address following the given token.
func parseAfter(s, token string) (netip.Addr, bool) {
	fields := strings.Fields(s)
	for i, f := range fields {
		if f == token && i+1 < len(fields) {
			if addr, err := netip.ParseAddr(fields[i+1]); err == nil {
				return addr.Unmap(), true
			}
		}
	}
	return netip.Addr{}, false
}
