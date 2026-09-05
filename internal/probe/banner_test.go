package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/routerguide"
)

// The banner tier exists so that a router with UPnP switched off can still be
// identified. It has to read what a login page actually carries: the Server
// header, the auth realm, and the title.
func TestBannerReadsWhatARouterLoginPageCarries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "lighttpd/1.4.35")
		w.Header().Set("WWW-Authenticate", `Basic realm="FRITZ!Box 7590"`)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("<html><head><title>FRITZ!Box 7590 - Login</title></head></html>"))
	}))
	defer srv.Close()

	got := fetchBanner(context.Background(), srv.Client(), srv.URL+"/")
	for _, want := range []string{"lighttpd", "FRITZ!Box 7590", "Login"} {
		if !strings.Contains(got, want) {
			t.Errorf("banner %q is missing %q", got, want)
		}
	}
}

// A 401 with a realm and a login page behind a 403 both identify a router
// perfectly well. Requiring 200 would throw away the common case.
func TestBannerIsReadFromAnUnauthorisedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<title>NETGEAR Router</title>"))
	}))
	defer srv.Close()

	if got := fetchBanner(context.Background(), srv.Client(), srv.URL+"/"); !strings.Contains(got, "NETGEAR") {
		t.Errorf("banner = %q, want the title of the refused page", got)
	}
}

func TestBannerIsEmptyWhenTheGatewaySaysNothingUseful(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Server"] = nil
		_, _ = w.Write([]byte("<html><body>hello</body></html>"))
	}))
	defer srv.Close()

	if got := fetchBanner(context.Background(), srv.Client(), srv.URL+"/"); got != "" {
		t.Errorf("banner = %q, want empty", got)
	}
}

func TestNoGatewayMeansNoBanner(t *testing.T) {
	if got := readBanner(context.Background(), netip.Addr{}, 0); got != "" {
		t.Errorf("banner = %q with no gateway address", got)
	}
}

// The banner reaches the log file, and log files are meant to be attachable to
// a public issue. Control characters and an unbounded length are not
// acceptable in something a device on the network chose the contents of.
func TestBannerIsCleanedBeforeItIsKept(t *testing.T) {
	got := cleanBanner("  Router\r\n\tName\x00\x07  ")
	if got != "Router Name" {
		t.Errorf("cleanBanner = %q", got)
	}
	if n := len([]rune(cleanBanner(strings.Repeat("é", 5000)))); n > 200 {
		t.Errorf("cleaned banner is %d runes long", n)
	}
	// Cut on a rune boundary: half the catalogue is routers sold in
	// non-English locales.
	long := cleanBanner(strings.Repeat("é", 5000))
	if strings.ContainsRune(long, '\uFFFD') {
		t.Error("truncation split a character")
	}
}

// The whole point of the tier. This is the string a real FRITZ!Box serves with
// UPnP switched off, and before this it reached nothing: probe.Router had no
// Banner field, so Match was only ever given a vendor string and a MAC.
func TestABannerAloneIdentifiesARouter(t *testing.T) {
	c, err := routerguide.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	e := c.Match(routerguide.Identity{Banner: cleanBanner("<title>FRITZ!Box 7590 - Login</title>")})
	if e.IsDefault() {
		t.Fatal("a banner that names the router still fell through to the generic guide")
	}
}

// A gateway that answers slowly on one address must not consume the whole
// budget and leave the other with none.
//
// This is the failure that showed up the first time the tier ran against a
// real network: the same gateway produced a banner when asked on its own and
// nothing at all from inside a probe, because the schemes were tried in turn.
func TestASlowAddressDoesNotStarveTheOther(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer slow.Close()
	quick := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<title>Archer AX55</title>"))
	}))
	defer quick.Close()

	start := time.Now()
	got := raceBanners(context.Background(), quick.Client(),
		[]string{slow.URL + "/", quick.URL + "/"}, 2*time.Second)
	if !strings.Contains(got, "Archer") {
		t.Fatalf("banner = %q, want the one the reachable address served", got)
	}
	// And it did not wait for the slow one on the way.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %s, so it waited for the address that never answered", elapsed)
	}
}

func TestBannerGivesUpWhenNothingAnswers(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer dead.Close()

	start := time.Now()
	if got := raceBanners(context.Background(), dead.Client(), []string{dead.URL + "/"}, 300*time.Millisecond); got != "" {
		t.Errorf("banner = %q", got)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %s, so the budget is not being enforced", elapsed)
	}
}
