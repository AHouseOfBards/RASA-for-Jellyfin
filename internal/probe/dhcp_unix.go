//go:build !windows

package probe

import (
	"context"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// addressIsDHCP reports whether the address came from a DHCP lease.
//
// Linux has no single authority for this, so several sources are consulted and
// anything inconclusive is treated as DHCP — see ProbeHost for why the unknown
// case leans that way.
func addressIsDHCP(ctx context.Context, addr netip.Addr) bool {
	if !addr.IsValid() {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// networkd and NetworkManager both record the protocol that configured an
	// address, which `ip` reports as "dynamic" for DHCP.
	if out, err := exec.CommandContext(ctx, "ip", "-o", "addr", "show").Output(); err == nil {
		want := addr.String() + "/"
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.Contains(line, want) {
				continue
			}
			return strings.Contains(line, "dynamic")
		}
	}

	// A lease file naming the address is strong evidence on systems using
	// dhclient or dhcpcd.
	for _, dir := range []string{"/var/lib/dhcp", "/var/lib/dhcpcd", "/var/lib/NetworkManager"} {
		matches, _ := filepath.Glob(filepath.Join(dir, "*"))
		for _, m := range matches {
			b, err := os.ReadFile(m)
			if err != nil {
				continue
			}
			if strings.Contains(string(b), addr.String()) {
				return true
			}
		}
	}
	return true
}
