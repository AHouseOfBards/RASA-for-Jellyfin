package probe

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
)

const igdDescription = `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <device>
    <deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:1</deviceType>
    <friendlyName>Test Router</friendlyName>
    <manufacturer>ASUSTeK Computer Inc.</manufacturer>
    <modelName>RT-AX88U</modelName>
    <modelNumber>1.0</modelNumber>
    <deviceList>
      <device>
        <deviceType>urn:schemas-upnp-org:device:WANDevice:1</deviceType>
        <deviceList>
          <device>
            <deviceType>urn:schemas-upnp-org:device:WANConnectionDevice:1</deviceType>
            <serviceList>
              <service>
                <serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType>
                <controlURL>/ctl/IPConn</controlURL>
              </service>
            </serviceList>
          </device>
        </deviceList>
      </device>
    </deviceList>
  </device>
</root>`

func soapReply(addr string) string {
	return `<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/">
 <s:Body>
  <u:GetExternalIPAddressResponse xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1">
   <NewExternalIPAddress>` + addr + `</NewExternalIPAddress>
  </u:GetExternalIPAddressResponse>
 </s:Body>
</s:Envelope>`
}

func TestSSDPLocationHeader(t *testing.T) {
	reply := "HTTP/1.1 200 OK\r\n" +
		"CACHE-CONTROL: max-age=120\r\n" +
		"LOCATION: http://192.168.1.1:5000/rootDesc.xml\r\n" +
		"ST: urn:schemas-upnp-org:device:InternetGatewayDevice:1\r\n\r\n"

	if got := ssdpLocation([]byte(reply)); got != "http://192.168.1.1:5000/rootDesc.xml" {
		t.Fatalf("got %q", got)
	}
}

func TestSSDPLocationIsCaseInsensitive(t *testing.T) {
	// Real routers vary the header case, and some send "Location".
	reply := "HTTP/1.1 200 OK\r\nlocation: http://10.0.0.1/desc.xml\r\n\r\n"
	if got := ssdpLocation([]byte(reply)); got != "http://10.0.0.1/desc.xml" {
		t.Fatalf("got %q", got)
	}
}

func TestSSDPLocationMissing(t *testing.T) {
	if got := ssdpLocation([]byte("HTTP/1.1 200 OK\r\nST: something\r\n\r\n")); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestFindWANServiceWalksNestedDevices(t *testing.T) {
	// The connection service is two device levels down, which is where a
	// non-recursive search would miss it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, igdDescription)
	}))
	defer srv.Close()

	p := NewRouterProber(logging.Discard())
	desc, err := p.describe(context.Background(), srv.URL+"/rootDesc.xml")
	if err != nil {
		t.Fatal(err)
	}
	ctrl, svcType := findWANService(&desc.Device, srv.URL+"/rootDesc.xml")
	if ctrl != srv.URL+"/ctl/IPConn" {
		t.Fatalf("control url = %q", ctrl)
	}
	if svcType != svcWANIPConnection {
		t.Fatalf("service type = %q", svcType)
	}
}

func TestDescribeExtractsVendorAndModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, igdDescription)
	}))
	defer srv.Close()

	p := NewRouterProber(logging.Discard())
	desc, err := p.describe(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(desc.Device.Manufacturer, "ASUSTeK") {
		t.Fatalf("manufacturer = %q", desc.Device.Manufacturer)
	}
	if desc.Device.ModelName != "RT-AX88U" {
		t.Fatalf("model = %q", desc.Device.ModelName)
	}
}

func TestAbsoluteURL(t *testing.T) {
	base := "http://192.168.1.1:5000/rootDesc.xml"
	cases := map[string]string{
		"/ctl/IPConn":                      "http://192.168.1.1:5000/ctl/IPConn",
		"ctl/IPConn":                       "http://192.168.1.1:5000/ctl/IPConn",
		"http://192.168.1.1:5000/x/IPConn": "http://192.168.1.1:5000/x/IPConn",
	}
	for ref, want := range cases {
		if got := absoluteURL(base, ref); got != want {
			t.Errorf("absoluteURL(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestExternalAddressParsesSOAP(t *testing.T) {
	var action string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action = r.Header.Get("SOAPAction")
		w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
		io.WriteString(w, soapReply("203.0.113.5"))
	}))
	defer srv.Close()

	p := NewRouterProber(logging.Discard())
	addr, err := p.externalAddress(context.Background(), srv.URL, svcWANIPConnection)
	if err != nil {
		t.Fatal(err)
	}
	if addr.String() != "203.0.113.5" {
		t.Fatalf("addr = %v", addr)
	}
	if !strings.Contains(action, "GetExternalIPAddress") {
		t.Fatalf("SOAPAction header = %q", action)
	}
}

func TestExternalAddressRejectsUnspecified(t *testing.T) {
	// A gateway with no WAN lease reports 0.0.0.0. Accepting it would make the
	// CGNAT comparison see a mismatch and wrongly route to mesh mode.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, soapReply("0.0.0.0"))
	}))
	defer srv.Close()

	p := NewRouterProber(logging.Discard())
	if _, err := p.externalAddress(context.Background(), srv.URL, svcWANIPConnection); err == nil {
		t.Fatal("0.0.0.0 must not be accepted as an external address")
	}
}

func TestExternalAddressRejectsGarbage(t *testing.T) {
	for _, body := range []string{soapReply("not-an-ip"), soapReply(""), "<html>nope</html>"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, body)
		}))
		p := NewRouterProber(logging.Discard())
		if _, err := p.externalAddress(context.Background(), srv.URL, svcWANIPConnection); err == nil {
			t.Errorf("accepted %q", body)
		}
		srv.Close()
	}
}

func TestRouterProbeSurvivesNoRouter(t *testing.T) {
	// A router that does not speak UPnP is a supported configuration, not an
	// error: it means guided manual forwarding.
	p := NewRouterProber(logging.Discard())
	p.SearchTimeout = 300 * time.Millisecond
	// This is the one probe that talks to whatever gateway the machine running
	// the tests happens to sit behind. The banner tier reads its admin page,
	// and a gateway that answers slowly would otherwise spend the full default
	// budget here every run.
	p.BannerTimeout = 200 * time.Millisecond

	got := p.Probe(context.Background())
	if got.PortMappingAvailable && !got.WANAddress.IsValid() {
		t.Error("claimed mapping is available without finding a service")
	}
}

// ---------- orchestrator ----------

func TestProberRunReturnsAllSections(t *testing.T) {
	p := New(logging.Discard())
	p.Timeout = 3 * time.Second
	p.Router.SearchTimeout = 200 * time.Millisecond
	p.Internet.Timeout = 300 * time.Millisecond
	p.Internet.V4Endpoints = []string{"http://127.0.0.1:1"}
	p.Internet.V6Endpoints = nil
	p.Jellyfin.Timeout = 300 * time.Millisecond
	p.Jellyfin.Addresses = []string{"127.0.0.1:1"}
	p.Ports = []int{PortPreferred, PortFallback}

	res := p.Run(context.Background())

	// Nothing was reachable, but every section must still be populated rather
	// than left as an ambiguous zero value.
	if res.Internet.Reachable {
		t.Error("internet should be unreachable in this test")
	}
	if res.Jellyfin.Found {
		t.Error("jellyfin should not be found")
	}
	if res.Ports.Free == nil {
		t.Error("ports were not probed")
	}
}

func TestProberRunRespectsTimeout(t *testing.T) {
	p := New(logging.Discard())
	p.Timeout = time.Second
	p.Router.SearchTimeout = 200 * time.Millisecond
	p.Internet.V4Endpoints = []string{"http://192.0.2.1:81"} // black hole
	p.Internet.V6Endpoints = nil
	p.Internet.Timeout = 400 * time.Millisecond
	p.Jellyfin.Addresses = []string{"192.0.2.1:81"}
	p.Jellyfin.Timeout = 400 * time.Millisecond

	start := time.Now()
	p.Run(context.Background())
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("run took %v, timeout not honoured", elapsed)
	}
}

func TestSummarizeIsPlainLanguage(t *testing.T) {
	res := Result{
		Jellyfin: Jellyfin{Found: true, Version: "10.11.7", MeetsMinimum: true},
		Internet: Internet{Reachable: true},
		Router:   Router{Reachable: true, PortMappingAvailable: true},
		Ports:    Ports{Free: map[int]bool{PortPreferred: true}},
	}
	s := res.Summarize()

	for _, line := range []string{s.Jellyfin, s.Internet, s.Router, s.Ports} {
		if strings.TrimSpace(line) == "" {
			t.Error("empty summary line")
		}
		for _, banned := range []string{"UPnP", "IGD", "CGNAT", "SSDP", "nil", "error"} {
			if strings.Contains(line, banned) {
				t.Errorf("summary contains jargon %q: %q", banned, line)
			}
		}
	}
	if !strings.Contains(s.Ports, "443") {
		t.Errorf("port summary should name the port: %q", s.Ports)
	}
}

func TestSummarizeOldJellyfinIsDistinctFromMissing(t *testing.T) {
	missing := Result{}.Summarize()
	old := Result{Jellyfin: Jellyfin{Found: true, Version: "10.10.3"}}.Summarize()

	if missing.Jellyfin == old.Jellyfin {
		t.Fatal("an old server must not read the same as a missing one")
	}
	if !strings.Contains(old.Jellyfin, "10.10.3") {
		t.Errorf("the version the user has should be named: %q", old.Jellyfin)
	}
}

func TestSummarizePortFallback(t *testing.T) {
	res := Result{Ports: Ports{Free: map[int]bool{PortPreferred: false, PortFallback: true}}}
	if s := res.Summarize(); !strings.Contains(s.Ports, "8443") {
		t.Fatalf("fallback should be explained: %q", s.Ports)
	}
}
