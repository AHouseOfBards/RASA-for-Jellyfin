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
	"sort"
	"strings"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
)

// UPnP Internet Gateway Device discovery and query.
//
// This package only *reads* from the gateway: it discovers the device and asks
// for its external address, which is what the CGNAT comparison in SPEC.md §5
// needs. Creating mappings is task 5.

const ssdpAddr = "239.255.255.250:1900"

// searchTargets are the search types sent, most specific first.
//
// Asking only for InternetGatewayDevice:1 was a version assumption, and a
// wrong one: a router that implements IGD version 2 is under no obligation to
// answer a search for version 1, and newer hardware increasingly implements
// only 2. The symptom is total silence -- indistinguishable from UPnP being
// switched off, which is exactly how it was reported, with a screenshot of the
// setting plainly enabled.
//
// upnp:rootdevice is the backstop. Everything that speaks UPnP at all answers
// it, including a router whose device type is spelled in some way nobody
// anticipated. It also brings in televisions and printers, which is why a
// candidate is only accepted once its description turns out to have a WAN
// connection service in it.
var searchTargets = []string{
	"urn:schemas-upnp-org:device:InternetGatewayDevice:2",
	"urn:schemas-upnp-org:device:InternetGatewayDevice:1",
	"upnp:rootdevice",
}

// wanServicePrefixes match the port-mapping service at any version, for the
// same reason: pinning to :1 rejected a perfectly good :2 service as "this
// router does not offer port opening".
var wanServicePrefixes = []string{
	"urn:schemas-upnp-org:service:WANIPConnection:",
	"urn:schemas-upnp-org:service:WANPPPConnection:",
}

func isWANService(t string) bool {
	for _, p := range wanServicePrefixes {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

// RouterProber discovers the local gateway.
type RouterProber struct {
	// SearchTimeout bounds how long to wait for SSDP replies. Routers answer
	// within a second or two when they answer at all.
	SearchTimeout time.Duration
	Timeout       time.Duration
	// BannerTimeout bounds the admin-page read that identifies a router with
	// UPnP switched off. Separate from Timeout because it is spent only in the
	// case where UPnP already failed, and because it is the one part of this
	// probe that talks to a device that may simply never answer.
	BannerTimeout time.Duration
	Log           *logging.Logger
}

// NewRouterProber returns a prober with default timeouts.
func NewRouterProber(log *logging.Logger) *RouterProber {
	if log == nil {
		log = logging.Discard()
	}
	return &RouterProber{
		SearchTimeout: 3 * time.Second,
		Timeout:       5 * time.Second,
		BannerTimeout: DefaultBannerTimeout,
		Log:           log,
	}
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

	p.queryIGD(ctx, &out)

	// Only when UPnP told us nothing. A router that answered SSDP has already
	// named its own manufacturer, which is the more reliable tier of the two,
	// and fetching an admin page nobody is going to read costs the user
	// seconds on a screen they are waiting in front of.
	if out.Vendor == "" && out.Model == "" {
		var adminURL string
		out.Banner, adminURL = readBanner(ctx, out.Gateway, p.BannerTimeout)
		// Whichever address answered is the settings page, which is worth more
		// than the banner on its own: the instructions used to tell everyone to
		// open http://<gateway>, and a router that serves on any other port —
		// Verizon on 450, Synology on 8000 — was sending the user somewhere
		// with nothing on it.
		if out.AdminURL == "" {
			out.AdminURL = adminURL
		}
		p.Log.Debug("read gateway banner",
			slog.String("banner", out.Banner), slog.String("admin_url", out.AdminURL))
	}

	p.Log.Debug("router probe complete",
		slog.String("vendor", out.Vendor),
		slog.String("model", out.Model),
		slog.Bool("banner_read", out.Banner != ""),
		slog.String("upnp_status", string(out.UPnPStatus)),
		slog.Bool("mapping_available", out.PortMappingAvailable),
		slog.Bool("wan_address_known", out.WANAddress.IsValid()),
	)
	return out
}

// queryIGD fills in everything that only UPnP can tell us. Every failure is
// soft: a router that does not speak IGD leaves out untouched.
func (p *RouterProber) queryIGD(ctx context.Context, out *Router) {
	found, err := p.discover(ctx, out.Gateway)
	if err != nil {
		// Info, not debug. This is the single most asked question about this
		// step -- "why isn't UPnP working for me?" -- and at debug level the
		// answer was absent from every normal run's log.
		out.UPnPStatus = UPnPNoReply
		p.Log.Info("no router answered the automatic port opening request",
			slog.String("meaning", "the setting is off, or the request never reached the router"),
			slog.Any("err", err))
		return
	}
	// Something on the network speaks UPnP, so the search is getting out.
	out.Reachable = true

	// Every answer is tried, not only the first. The backstop search type is
	// answered by televisions and printers as well as by routers, and a home
	// network usually has more of the former.
	var ctrl, svcType string
	var described bool
	for _, c := range found {
		desc, err := p.describe(ctx, c.location)
		if err != nil {
			p.Log.Debug("a device would not describe itself",
				slog.String("location", c.location), slog.Any("err", err))
			continue
		}
		wanURL, wanType := findWANService(&desc.Device, c.location)
		if wanURL == "" {
			if !described && c.gateway {
				// Worth remembering: a device that called itself a gateway and
				// has no port-mapping service is the "UPnP is on but it is the
				// media sharing kind" case, and its name belongs in the log.
				described = true
				out.Vendor = strings.TrimSpace(desc.Device.Manufacturer)
				out.Model = strings.TrimSpace(firstNonEmpty(desc.Device.ModelName, desc.Device.ModelNumber, desc.Device.FriendlyName))
				// It is still the router, and it still knows where its own
				// settings page is. That is the one thing the manual
				// instructions need most from a router that cannot help.
				out.AdminURL = presentationURL(desc, c.location)
			}
			continue
		}
		described = true
		out.Vendor = strings.TrimSpace(desc.Device.Manufacturer)
		out.Model = strings.TrimSpace(firstNonEmpty(desc.Device.ModelName, desc.Device.ModelNumber, desc.Device.FriendlyName))
		out.AdminURL = presentationURL(desc, c.location)
		if c.from.IsValid() {
			// The address it replied from is its LAN address, which is more
			// reliable than parsing a routing table.
			out.Gateway = c.from
		}
		ctrl, svcType = wanURL, wanType
		break
	}

	if ctrl == "" {
		if !described {
			out.UPnPStatus = UPnPNoDescription
			p.Log.Info("something answered but would not describe itself",
				slog.Int("devices", len(found)))
			return
		}
		// The router speaks UPnP but not the port-opening half of it. Usually
		// this is a "UPnP" switch that turns on media sharing and nothing else.
		out.UPnPStatus = UPnPNoPortService
		p.Log.Info("the router speaks UPnP but offers no port opening service",
			slog.String("vendor", out.Vendor), slog.String("model", out.Model),
			slog.String("meaning", "the UPnP setting that is on may be the media sharing one"))
		return
	}
	out.ControlURL, out.ServiceType = ctrl, svcType
	// The device speaks IGD, so a mapping request is at least plausible. It
	// may still be refused, which is why task 5 verifies externally rather
	// than trusting this.
	out.PortMappingAvailable = true
	out.UPnPStatus = UPnPAvailable

	if addr, err := p.externalAddress(ctx, ctrl, svcType); err == nil {
		out.WANAddress = addr
	} else {
		p.Log.Debug("GetExternalIPAddress failed", slog.Any("err", err))
	}

}

// discover searches for gateways and returns everything that answered, best
// first.
func (p *RouterProber) discover(ctx context.Context, gw netip.Addr) ([]candidate, error) {
	// Bound to the address that faces the router, not to whatever the system
	// would pick.
	//
	// Discovery is a multicast datagram, and the interface it leaves by comes
	// from the routing table. A VPN adapter usually holds the lowest metric on
	// the machine, so with a tunnel up the search goes out of the tunnel and
	// the router never hears it — and the symptom is that UPnP looks switched
	// off while its own settings page plainly says it is on. Reported exactly
	// that way, with a screenshot of the setting enabled.
	local := ":0"
	if src := addressFacing(gw); src.IsValid() {
		local = netip.AddrPortFrom(src, 0).String()
	}
	conn, err := net.ListenPacket("udp4", local)
	if err != nil {
		// Falling back rather than failing: binding to a specific address can
		// be refused, and an unbound socket is what this always used to do.
		p.Log.Debug("could not bind discovery to the router-facing address",
			slog.String("address", local), slog.Any("err", err))
		conn, err = net.ListenPacket("udp4", ":0")
		if err != nil {
			return nil, err
		}
	}
	defer conn.Close()

	dst, err := net.ResolveUDPAddr("udp4", ssdpAddr)
	if err != nil {
		return nil, err
	}

	// The gateway is asked directly as well as over multicast. A unicast
	// M-SEARCH to port 1900 is answered by essentially every IGD there is, and
	// it needs no multicast routing to work at all — so it still arrives when
	// the multicast has gone out of the wrong adapter.
	targets := []net.Addr{dst}
	if gw.IsValid() {
		targets = append(targets, &net.UDPAddr{IP: net.IP(gw.AsSlice()), Port: 1900})
	}

	deadline := time.Now().Add(p.SearchTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}

	// Every search type, to every target, twice.
	//
	// A send that fails is not fatal. Multicast can be refused outright on a
	// machine whose routing makes no sense for it, and the unicast question to
	// the gateway is the one likely to be answered in exactly that case, so
	// giving up on the first failure would throw away the attempt most likely
	// to work. SSDP is UDP and a single datagram is genuinely lost often
	// enough on busy wireless to be worth sending twice.
	var sent int
	var lastErr error
	for i := 0; i < 2; i++ {
		for _, st := range searchTargets {
			// MX is the longest a device may wait before replying; keep it
			// below SearchTimeout or well-behaved routers answer after we have
			// stopped listening.
			msg := "M-SEARCH * HTTP/1.1\r\n" +
				"HOST: " + ssdpAddr + "\r\n" +
				"MAN: \"ssdp:discover\"\r\n" +
				"MX: 2\r\n" +
				"ST: " + st + "\r\n\r\n"
			for _, t := range targets {
				if _, err := conn.WriteTo([]byte(msg), t); err != nil {
					lastErr = err
					continue
				}
				sent++
			}
		}
	}
	if sent == 0 {
		return nil, fmt.Errorf("could not send a discovery request: %w", lastErr)
	}

	// Replies are collected rather than the first one taken.
	//
	// upnp:rootdevice is answered by everything on the network that speaks
	// UPnP at all, so the first reply is frequently a television. A reply
	// whose own search type names an InternetGatewayDevice is worth stopping
	// for; anything else is a candidate to be checked only if nothing better
	// arrives before the deadline.
	var found []candidate
	seen := map[string]bool{}
	buf := make([]byte, 2048)
	for len(found) < maxCandidates {
		n, src, err := conn.ReadFrom(buf)
		if err != nil {
			break
		}
		loc := ssdpHeader(buf[:n], "LOCATION")
		if loc == "" || seen[loc] {
			continue
		}
		seen[loc] = true

		var addr netip.Addr
		if ua, ok := src.(*net.UDPAddr); ok {
			if a, ok := netip.AddrFromSlice(ua.IP); ok {
				addr = a.Unmap()
			}
		}
		c := candidate{location: loc, from: addr}
		// ST on a reply, USN as the fallback: some devices leave ST off and
		// carry the type only in the USN.
		kind := ssdpHeader(buf[:n], "ST") + " " + ssdpHeader(buf[:n], "USN")
		c.gateway = strings.Contains(kind, "InternetGatewayDevice")
		found = append(found, c)
		if c.gateway {
			// Unambiguous. No reason to spend the rest of the budget.
			break
		}
	}

	if len(found) == 0 {
		return nil, fmt.Errorf("no device replied to the discovery request")
	}
	// Gateways first, so a television never costs a description fetch ahead of
	// the router.
	sort.SliceStable(found, func(i, j int) bool { return found[i].gateway && !found[j].gateway })
	return found, nil
}

// candidate is one device that answered discovery.
type candidate struct {
	location string
	from     netip.Addr
	// gateway records that the reply named itself an InternetGatewayDevice,
	// rather than merely being something that speaks UPnP.
	gateway bool
}

// maxCandidates bounds how many devices are considered. A home network has one
// router; the rest of the replies are media players.
const maxCandidates = 8

// ssdpHeader extracts one header from an SSDP reply.
func ssdpHeader(b []byte, name string) string {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(k), name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// upnpDevice mirrors the parts of a device description RASA reads. Devices
// nest, so this is recursive.
type upnpDevice struct {
	DeviceType   string `xml:"deviceType"`
	FriendlyName string `xml:"friendlyName"`
	// PresentationURL is the device's own settings page. For a router this is
	// the authoritative answer to "where do I sign in?", which beats assuming
	// http on port 80 of the gateway.
	PresentationURL string       `xml:"presentationURL"`
	Manufacturer    string       `xml:"manufacturer"`
	ModelName       string       `xml:"modelName"`
	ModelNumber     string       `xml:"modelNumber"`
	Services        []upnpSvc    `xml:"serviceList>service"`
	Devices         []upnpDevice `xml:"deviceList>device"`
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
		if isWANService(s.ServiceType) {
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

// presentationURL is the router's own settings page, resolved against where
// its description was fetched from. Devices report it as a full URL, an
// absolute path, or not at all.
func presentationURL(desc *upnpRoot, base string) string {
	p := strings.TrimSpace(desc.Device.PresentationURL)
	if p == "" {
		return ""
	}
	return absoluteURL(base, p)
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
