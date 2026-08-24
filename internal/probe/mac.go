package probe

import (
	"context"
	"net"
	"net/netip"
	"regexp"
	"strings"
	"time"
)

// macPattern matches both separator styles the platform tools emit:
// 00-1f-c6-aa-bb-cc on Windows, 00:1f:c6:aa:bb:cc elsewhere.
var macPattern = regexp.MustCompile(`(?i)\b([0-9a-f]{2}[:-]){5}[0-9a-f]{2}\b`)

// gatewayMAC returns the gateway's hardware address, or "".
//
// This is the last of the three identification tiers in SPEC.md §6, and the
// only one that works with no cooperation from the router at all: the vendor
// prefix is visible from the ARP table whether or not UPnP is enabled and
// whether or not the admin page answers.
//
// Best effort. The ARP entry may not exist yet on a freshly booted machine, so
// a datagram is sent first to provoke one.
func gatewayMAC(ctx context.Context, gw netip.Addr) string {
	if !gw.IsValid() {
		return ""
	}
	// Nudge the ARP cache. Nothing listens on discard, and the reply is
	// irrelevant — the point is that the kernel had to resolve the hardware
	// address to send at all.
	if c, err := net.DialTimeout("udp", net.JoinHostPort(gw.String(), "9"), time.Second); err == nil {
		_, _ = c.Write([]byte{0})
		_ = c.Close()
	}

	out, err := arpTable(ctx)
	if err != nil {
		return ""
	}
	want := gw.String()
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, want) {
			continue
		}
		// Guard against a partial address match: 192.168.1.1 must not match a
		// line for 192.168.1.10.
		if !hasAddressField(line, want) {
			continue
		}
		if m := macPattern.FindString(line); m != "" {
			return NormalizeMAC(m)
		}
	}
	return ""
}

// hasAddressField reports whether the line contains the address as a whole
// field rather than as a prefix of a longer one.
func hasAddressField(line, addr string) bool {
	for _, f := range strings.Fields(line) {
		if strings.Trim(f, "()") == addr {
			return true
		}
	}
	return false
}

// NormalizeMAC renders a hardware address as lowercase colon-separated, so
// values from different platforms compare equal.
func NormalizeMAC(s string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "-", ":"))
}

// OUI returns the vendor prefix — the first three octets — of a MAC address,
// lowercase and colon-separated. Returns "" if the input is not a MAC.
func OUI(mac string) string {
	m := NormalizeMAC(mac)
	parts := strings.Split(m, ":")
	if len(parts) < 3 {
		return ""
	}
	return strings.Join(parts[:3], ":")
}
