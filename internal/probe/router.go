package probe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
)

// UPnP Internet Gateway Device discovery and query.
//
// This package only *reads* from the gateway: it discovers the device and asks
// for its external address, which is what the CGNAT comparison in SPEC.md §5
// needs. Creating mappings is task 5.

const (
	ssdpAddr      = "239.255.255.250:1900"
	igdDeviceType = "urn:schemas-upnp-org:device:InternetGatewayDevice:1"

	svcWANIPConnection  = "urn:schemas-upnp-org:service:WANIPConnection:1"
	svcWANPPPConnection = "urn:schemas-upnp-org:service:WANPPPConnection:1"
)

// RouterProber discovers the local gateway.
type RouterProber struct {
	// SearchTimeout bounds how long to wait for SSDP replies. Routers answer
	// within a second or two when they answer at all.
	SearchTimeout time.Duration
	Timeout       time.Duration
	Log           *logging.Logger
}

// NewRouterProber returns a prober with default timeouts.
func NewRouterProber(log *logging.Logger) *RouterProber {
	if log == nil {
		log = logging.Discard()
	}
	return &RouterProber{SearchTimeout: 3 * time.Second, Timeout: 5 * time.Second, Log: log}
}

// Probe discovers the gateway and asks for its external address.
//
// Every failure here is soft. A router that does not speak UPnP is a normal,
// supported configuration — it means guided manual forwarding rather than a
// dead end — so this reports what it learned and never returns an error.
func (p *RouterProber) Probe(ctx context.Context) Router {
	if p.SearchTimeout == 0 {
		p.SearchTimeout = 3 * time.Second
	}
	if p.Timeout == 0 {
		p.Timeout = 5 * time.Second
	}

	out := Router{}

	// The gateway address is useful even without UPnP: it is the admin page
	// link in the manual forwarding instructions.
	if gw, ok := defaultGateway(ctx); ok {
		out.Gateway = gw
		out.Reachable = true
		// The hardware address identifies the vendor even when the router
		// refuses every other kind of question.
		out.MAC = gatewayMAC(ctx, gw)
	}

	loc, from, err := p.discover(ctx)
	if err != nil {
		p.Log.Debug("no igd discovered", slog.Any("err", err))
		return out
	}
	// A router that answered SSDP is reachable, and the reply source is its
	// LAN address — more reliable than parsing a routing table.
	out.Reachable = true
	if from.IsValid() {
		out.Gateway = from
	}

	desc, err := p.describe(ctx, loc)
	if err != nil {
		p.Log.Debug("igd description failed", slog.Any("err", err))
		return out
	}
	out.Vendor = strings.TrimSpace(desc.Device.Manufacturer)
	out.Model = strings.TrimSpace(firstNonEmpty(desc.Device.ModelName, desc.Device.ModelNumber, desc.Device.FriendlyName))

	ctrl, svcType := findWANService(&desc.Device, loc)
	if ctrl == "" {
		p.Log.Debug("igd has no WAN connection service")
		return out
	}
	out.ControlURL, out.ServiceType = ctrl, svcType
	// The device speaks IGD, so a mapping request is at least plausible. It
	// may still be refused, which is why task 5 verifies externally rather
	// than trusting this.
	out.PortMappingAvailable = true

	if addr, err := p.externalAddress(ctx, ctrl, svcType); err == nil {
		out.WANAddress = addr
	} else {
		p.Log.Debug("GetExternalIPAddress failed", slog.Any("err", err))
	}

	p.Log.Debug("router probe complete",
		slog.String("vendor", out.Vendor),
		slog.String("model", out.Model),
		slog.Bool("mapping_available", out.PortMappingAvailable),
		slog.Bool("wan_address_known", out.WANAddress.IsValid()),
	)
	return out
}

// discover sends an SSDP M-SEARCH and returns the first IGD location found,
// along with the address it replied from.
func (p *RouterProber) discover(ctx context.Context) (location string, from netip.Addr, err error) {
	conn, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return "", netip.Addr{}, err
	}
	defer conn.Close()

	dst, err := net.ResolveUDPAddr("udp4", ssdpAddr)
	if err != nil {
		return "", netip.Addr{}, err
	}

	// MX is the maximum delay a device may wait before replying; keep it
	// below SearchTimeout or well-behaved routers answer after we stop
	// listening.
	msg := "M-SEARCH * HTTP/1.1\r\n" +
		"HOST: " + ssdpAddr + "\r\n" +
		"MAN: \"ssdp:discover\"\r\n" +
		"MX: 2\r\n" +
		"ST: " + igdDeviceType + "\r\n\r\n"

	deadline := time.Now().Add(p.SearchTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return "", netip.Addr{}, err
	}

	// Send twice: SSDP is UDP multicast and a single datagram is genuinely
	// lost often enough to matter on busy wireless networks.
	for i := 0; i < 2; i++ {
		if _, err := conn.WriteTo([]byte(msg), dst); err != nil {
			return "", netip.Addr{}, err
		}
	}

	buf := make([]byte, 2048)
	for {
		n, src, err := conn.ReadFrom(buf)
		if err != nil {
			return "", netip.Addr{}, fmt.Errorf("no igd replied: %w", err)
		}
		loc := ssdpLocation(buf[:n])
		if loc == "" {
			continue
		}
		var addr netip.Addr
		if ua, ok := src.(*net.UDPAddr); ok {
			if a, ok := netip.AddrFromSlice(ua.IP); ok {
				addr = a.Unmap()
			}
		}
		return loc, addr, nil
	}
}

// ssdpLocation extracts the LOCATION header from an SSDP reply.
func ssdpLocation(b []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := sc.Text()
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(k), "LOCATION") {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// upnpDevice mirrors the parts of a device description RASA reads. Devices
// nest, so this is recursive.
type upnpDevice struct {
	DeviceType   string       `xml:"deviceType"`
	FriendlyName string       `xml:"friendlyName"`
	Manufacturer string       `xml:"manufacturer"`
	ModelName    string       `xml:"modelName"`
	ModelNumber  string       `xml:"modelNumber"`
	Services     []upnpSvc    `xml:"serviceList>service"`
	Devices      []upnpDevice `xml:"deviceList>device"`
}

type upnpSvc struct {
	ServiceType string `xml:"serviceType"`
	ControlURL  string `xml:"controlURL"`
}

type upnpRoot struct {
	XMLName xml.Name   `xml:"root"`
	Device  upnpDevice `xml:"device"`
}

func (p *RouterProber) describe(ctx context.Context, location string) (*upnpRoot, error) {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var root upnpRoot
	if err := xml.Unmarshal(b, &root); err != nil {
		return nil, err
	}
	return &root, nil
}

// findWANService walks the device tree for a WAN connection service and
// returns an absolute control URL plus the service type to invoke it with.
func findWANService(d *upnpDevice, base string) (controlURL, serviceType string) {
	for _, s := range d.Services {
		if s.ServiceType == svcWANIPConnection || s.ServiceType == svcWANPPPConnection {
			return absoluteURL(base, s.ControlURL), s.ServiceType
		}
	}
	for i := range d.Devices {
		if c, t := findWANService(&d.Devices[i], base); c != "" {
			return c, t
		}
	}
	return "", ""
}

// absoluteURL resolves a control URL, which devices report as an absolute
// path, a relative path, or occasionally a full URL.
func absoluteURL(base, ref string) string {
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

type soapEnvelope struct {
	XMLName xml.Name `xml:"Envelope"`
	Body    struct {
		ExternalIP struct {
			Address string `xml:"NewExternalIPAddress"`
		} `xml:"GetExternalIPAddressResponse"`
	} `xml:"Body"`
}

// externalAddress asks the gateway what it believes its own external address
// is. Comparing that to the address the outside world observes is the CGNAT
// check — see mode.BehindCGNAT.
func (p *RouterProber) externalAddress(ctx context.Context, controlURL, serviceType string) (netip.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()

	body := `<?xml version="1.0"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"` +
		` s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">` +
		`<s:Body><u:GetExternalIPAddress xmlns:u="` + serviceType + `"/></s:Body>` +
		`</s:Envelope>`

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, controlURL, strings.NewReader(body))
	if err != nil {
		return netip.Addr{}, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"`+serviceType+`#GetExternalIPAddress"`)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return netip.Addr{}, err
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return netip.Addr{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return netip.Addr{}, fmt.Errorf("status %d", resp.StatusCode)
	}

	var env soapEnvelope
	if err := xml.Unmarshal(b, &env); err != nil {
		return netip.Addr{}, err
	}
	s := strings.TrimSpace(env.Body.ExternalIP.Address)
	if s == "" {
		return netip.Addr{}, fmt.Errorf("no address in response")
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, err
	}
	// A gateway that has not obtained a WAN lease reports 0.0.0.0. Treating
	// that as a real address would make the CGNAT comparison claim a mismatch.
	if addr.IsUnspecified() {
		return netip.Addr{}, fmt.Errorf("gateway reports no external address yet")
	}
	return addr.Unmap(), nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
