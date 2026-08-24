package probe

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strings"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
)

// ProbeHost describes the machine RASA is running on.
func ProbeHost(ctx context.Context, log *logging.Logger) Host {
	if log == nil {
		log = logging.Discard()
	}
	h := Host{
		LANAddress:  localAddress(),
		InContainer: inContainer(),
	}
	// Unknown provenance is treated as DHCP on purpose. Advising a reservation
	// unnecessarily costs the user one extra click; failing to advise one lets
	// a static port forward break weeks later when the lease moves, which is
	// the most common cause of remote access silently dying (SPEC.md §6).
	h.AddressIsDHCP = addressIsDHCP(ctx, h.LANAddress)

	log.Debug("host probe",
		slog.Bool("have_lan_address", h.LANAddress.IsValid()),
		slog.Bool("dhcp", h.AddressIsDHCP),
		slog.Bool("in_container", h.InContainer),
	)
	return h
}

// localAddress returns this machine's address on the local network.
//
// The UDP dial sends no packets; it only asks the routing table which source
// address would be used to reach the internet. That is more reliable than
// enumerating interfaces, which cannot tell which of several is the default
// route on a machine with a VPN, a VM bridge, or Docker's virtual adapters.
func localAddress() netip.Addr {
	c, err := net.Dial("udp4", "192.0.2.1:9") // TEST-NET-1, never routed
	if err != nil {
		return fallbackAddress()
	}
	defer c.Close()

	if ua, ok := c.LocalAddr().(*net.UDPAddr); ok {
		if a, ok := netip.AddrFromSlice(ua.IP); ok {
			return a.Unmap()
		}
	}
	return fallbackAddress()
}

// fallbackAddress picks the first private, non-loopback IPv4 address.
func fallbackAddress() netip.Addr {
	ifaces, err := net.Interfaces()
	if err != nil {
		return netip.Addr{}
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			addr, ok := netip.AddrFromSlice(ipn.IP)
			if !ok {
				continue
			}
			addr = addr.Unmap()
			if addr.Is4() && addr.IsPrivate() {
				return addr
			}
		}
	}
	return netip.Addr{}
}

// inContainer reports whether RASA itself is running inside a container, where
// loopback discovery and SSDP multicast do not behave normally (SPEC.md §17).
func inContainer() bool {
	// Docker and Podman both leave a marker file.
	for _, p := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	// Fall back to the control groups of PID 1.
	if b, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		s := string(b)
		for _, marker := range []string{"docker", "containerd", "kubepods", "lxc", "podman"} {
			if strings.Contains(s, marker) {
				return true
			}
		}
	}
	// Kubernetes always injects this.
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return true
	}
	return false
}
