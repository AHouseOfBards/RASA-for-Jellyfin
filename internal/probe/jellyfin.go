package probe

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
)

// MinimumJellyfinVersion is the supported floor (SPEC.md decision 12). Older
// releases store network settings differently, so RASA refuses rather than
// writing configuration it cannot verify.
const MinimumJellyfinVersion = "10.11.5"

// DefaultJellyfinPort is only a starting guess. The real port is read from
// network.xml, because users change it and hardcoding 8096 is how a working
// install gets reported as missing.
const DefaultJellyfinPort = 8096

// systemInfoPublic is the unauthenticated endpoint Jellyfin exposes. It is
// also what Phase 7 fetches over the public address to prove the whole chain
// works.
const systemInfoPublic = "/System/Info/Public"

type publicInfo struct {
	LocalAddress    string `json:"LocalAddress"`
	ServerName      string `json:"ServerName"`
	Version         string `json:"Version"`
	ProductName     string `json:"ProductName"`
	OperatingSystem string `json:"OperatingSystem"`
	ID              string `json:"Id"`
}

// JellyfinProber locates the local Jellyfin server.
type JellyfinProber struct {
	// Addresses to try, in order. When empty, candidates are derived from
	// network.xml and the default port.
	Addresses []string
	Timeout   time.Duration
	Log       *logging.Logger

	client *http.Client
}

// NewJellyfinProber returns a prober with default settings.
func NewJellyfinProber(log *logging.Logger) *JellyfinProber {
	if log == nil {
		log = logging.Discard()
	}
	return &JellyfinProber{Timeout: 5 * time.Second, Log: log}
}

// Probe finds Jellyfin and reports what it is.
func (p *JellyfinProber) Probe(ctx context.Context) Jellyfin {
	if p.Timeout == 0 {
		p.Timeout = 5 * time.Second
	}
	if p.client == nil {
		p.client = &http.Client{Timeout: p.Timeout}
	}

	candidates := p.Addresses
	if len(candidates) == 0 {
		candidates = defaultCandidates(p.Log)
	}

	for _, addr := range candidates {
		info, err := p.query(ctx, addr)
		if err != nil {
			p.Log.Debug("jellyfin probe miss", slog.String("address", addr), slog.Any("err", err))
			continue
		}

		j := Jellyfin{
			Found:        true,
			Address:      addr,
			Version:      info.Version,
			MeetsMinimum: CompareVersions(info.Version, MinimumJellyfinVersion) >= 0,
		}
		j.Deployment, j.ProxySourceAddress = classifyDeployment(addr, info)

		p.Log.Info("jellyfin found",
			slog.String("version", info.Version),
			slog.Bool("meets_minimum", j.MeetsMinimum),
			slog.String("deployment", string(j.Deployment)),
		)
		return j
	}

	p.Log.Info("jellyfin not found", slog.Int("candidates_tried", len(candidates)))
	return Jellyfin{}
}

func (p *JellyfinProber) query(ctx context.Context, addr string) (*publicInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()

	url := addr
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = "http://" + url
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+systemInfoPublic, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	var info publicInfo
	if err := json.Unmarshal(b, &info); err != nil {
		return nil, err
	}
	// Something else answering on 8096 must not be mistaken for Jellyfin.
	if info.Version == "" || !strings.Contains(strings.ToLower(info.ProductName), "jellyfin") {
		return nil, fmt.Errorf("not a jellyfin server")
	}
	return &info, nil
}

// defaultCandidates builds the address list: the port from network.xml first,
// then the default, over both loopback families.
func defaultCandidates(log *logging.Logger) []string {
	ports := []int{}
	if p, path, ok := PortFromConfig(); ok {
		log.Debug("read jellyfin port from config", slog.Int("port", p), slog.String("path", path))
		ports = append(ports, p)
	}
	if len(ports) == 0 || ports[0] != DefaultJellyfinPort {
		ports = append(ports, DefaultJellyfinPort)
	}

	var out []string
	for _, p := range ports {
		out = append(out,
			net.JoinHostPort("127.0.0.1", strconv.Itoa(p)),
			net.JoinHostPort("::1", strconv.Itoa(p)),
		)
	}
	return out
}

// networkConfig mirrors the parts of Jellyfin's network.xml RASA needs.
//
// The element has been spelled several ways across releases, so all known
// spellings are accepted rather than assuming the current one.
type networkConfig struct {
	XMLName          xml.Name `xml:"NetworkConfiguration"`
	InternalHttpPort int      `xml:"InternalHttpPort"`
	HTTPServerPort   int      `xml:"HttpServerPortNumber"`
	PublicHttpPort   int      `xml:"PublicHttpPort"`
}

// PortFromConfig reads Jellyfin's configured HTTP port from network.xml.
//
// SPEC.md §10 is explicit that 8096 must not be hardcoded: users change it,
// and a changed port makes a working install look absent.
func PortFromConfig() (port int, path string, ok bool) {
	for _, p := range ConfigPaths() {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cfg networkConfig
		if xml.Unmarshal(b, &cfg) != nil {
			continue
		}
		for _, v := range []int{cfg.InternalHttpPort, cfg.HTTPServerPort, cfg.PublicHttpPort} {
			if v > 0 && v < 65536 {
				return v, p, true
			}
		}
	}
	return 0, "", false
}

// ConfigPaths lists where network.xml lives on this platform.
func ConfigPaths() []string {
	switch runtime.GOOS {
	case "windows":
		pd := os.Getenv("ProgramData")
		if pd == "" {
			pd = `C:\ProgramData`
		}
		return []string{
			filepath.Join(pd, "Jellyfin", "Server", "config", "network.xml"),
			filepath.Join(os.Getenv("LOCALAPPDATA"), "jellyfin", "config", "network.xml"),
		}
	default:
		return []string{
			"/etc/jellyfin/network.xml",
			"/var/lib/jellyfin/config/network.xml",
			"/config/network.xml", // the conventional container mount
			filepath.Join(os.Getenv("HOME"), ".config", "jellyfin", "network.xml"),
		}
	}
}

// classifyDeployment works out how Jellyfin is running and, from that, what
// address it will see the proxy arriving from.
//
// This matters because KnownProxies must match that address exactly. A
// containerised Jellyfin sees connections from the bridge gateway, not from
// 127.0.0.1, and configuring loopback there produces a server that silently
// logs every user with the wrong address (SPEC.md §13).
func classifyDeployment(addr string, info *publicInfo) (Deployment, string) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return DeploymentUnknown, host
	}

	if !a.IsLoopback() {
		// Reached over the network: another machine, so the proxy will appear
		// from this host's LAN address.
		return DeploymentRemote, localAddress().String()
	}

	// Reached over loopback. If Jellyfin reports a local address in container
	// space, it is containerised with published ports and will see the bridge
	// gateway rather than loopback.
	if la := strings.TrimSpace(info.LocalAddress); la != "" {
		if ip := extractHost(la); ip.IsValid() && isContainerAddress(ip) {
			return DeploymentContainer, ip.String()
		}
	}
	return DeploymentNative, a.String()
}

// extractHost pulls an address out of a bare host, host:port, or URL.
func extractHost(s string) netip.Addr {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "http://"), "https://")
	s = strings.TrimSuffix(s, "/")
	if h, _, err := net.SplitHostPort(s); err == nil {
		s = h
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}

// containerRanges are the defaults Docker and Podman allocate bridge networks
// from. A private address here is a strong hint, not a proof.
var containerRanges = []netip.Prefix{
	netip.MustParsePrefix("172.17.0.0/16"),
	netip.MustParsePrefix("172.18.0.0/16"),
	netip.MustParsePrefix("10.88.0.0/16"),
}

func isContainerAddress(a netip.Addr) bool {
	for _, p := range containerRanges {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// CompareVersions compares dotted numeric versions, returning -1, 0 or 1.
//
// Pre-release suffixes are ignored, so "10.11.5-rc1" compares equal to
// "10.11.5". Being lenient in that direction is deliberate: refusing to
// configure a release candidate the user is deliberately running would be
// unhelpful, while the floor still keeps genuinely old versions out.
func CompareVersions(a, b string) int {
	as, bs := splitVersion(a), splitVersion(b)
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func splitVersion(v string) []int {
	v = strings.TrimSpace(v)
	if i := strings.IndexAny(v, "-+ "); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			break
		}
		out = append(out, n)
	}
	return out
}
