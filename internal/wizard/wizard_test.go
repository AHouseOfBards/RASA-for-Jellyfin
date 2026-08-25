package wizard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/caddy"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/dnswait"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/domains"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/dynu"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/jellyfin"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/paths"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/portmap"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/probe"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/rasaerr"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/reach"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/secrets"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/service"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/state"
)

const testKey = "dynu-api-key-that-must-never-appear-anywhere"

// ---------------------------------------------------------------------------
// Fakes.

type fakeDynu struct {
	created     []dynu.CreateDomainRequest
	createFn    func(dynu.CreateDomainRequest) (*dynu.Domain, error)
	listErr     error
	deleted     int
	unpublished int
}

func (f *fakeDynu) Unpublish(ctx context.Context, id int64, name string) error {
	f.unpublished++
	return nil
}
func (f *fakeDynu) DeleteDomain(ctx context.Context, id int64) error {
	f.deleted++
	return nil
}

func (f *fakeDynu) ListDomains(ctx context.Context) ([]dynu.Domain, error) {
	return nil, f.listErr
}
func (f *fakeDynu) FindDomain(ctx context.Context, name string) (*dynu.Domain, error) {
	return nil, &dynu.APIError{StatusCode: 501}
}
func (f *fakeDynu) CreateDomain(ctx context.Context, r dynu.CreateDomainRequest) (*dynu.Domain, error) {
	f.created = append(f.created, r)
	if f.createFn != nil {
		return f.createFn(r)
	}
	return &dynu.Domain{ID: 42, Name: r.Name, Token: "per-hostname-token"}, nil
}
func (f *fakeDynu) UpdateAddresses(ctx context.Context, id int64, name string, v4, v6 netip.Addr) (*dynu.Domain, error) {
	return &dynu.Domain{ID: id, Name: name}, nil
}

type fakeJellyfin struct {
	admin      bool
	authErr    error
	applied    []jellyfin.Settings
	applyRes   *jellyfin.Result
	applyErr   error
	configErr  error
	restarted  bool
	restartErr error
}

func (f *fakeJellyfin) PublicInfo(ctx context.Context) (*jellyfin.PublicInfo, error) {
	return &jellyfin.PublicInfo{ServerName: "Living Room", Version: "10.11.7"}, nil
}
func (f *fakeJellyfin) NetworkConfig(ctx context.Context) (jellyfin.Config, error) {
	return jellyfin.Config{}, f.configErr
}
func (f *fakeJellyfin) AuthenticateByName(ctx context.Context, u, p string) (*jellyfin.AuthResult, error) {
	if f.authErr != nil {
		return nil, f.authErr
	}
	return &jellyfin.AuthResult{UserName: u, UserID: "uid", IsAdmin: f.admin}, nil
}
func (f *fakeJellyfin) Authenticated() bool { return true }
func (f *fakeJellyfin) Apply(ctx context.Context, s jellyfin.Settings) (*jellyfin.Result, error) {
	f.applied = append(f.applied, s)
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	if f.applyRes != nil {
		return f.applyRes, nil
	}
	return &jellyfin.Result{Changes: []jellyfin.Change{{}}}, nil
}
func (f *fakeJellyfin) Restart(ctx context.Context) error                         { f.restarted = true; return f.restartErr }
func (f *fakeJellyfin) WaitUntilReady(ctx context.Context, d time.Duration) error { return nil }

type fakeMapper struct {
	result *portmap.Result
	err    error
}

func (f *fakeMapper) Add(ctx context.Context, r portmap.Request) (*portmap.Result, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.result != nil {
		return f.result, nil
	}
	return &portmap.Result{
		Mapping: portmap.Mapping{
			ExternalPort: r.ExternalPort, InternalPort: r.InternalPort, LeaseSeconds: 0,
		},
		PermanentRequested: true,
		VerifiedByReadback: true,
	}, nil
}

type fakeDNS struct {
	calls int
	err   error
}

func (f *fakeDNS) WaitForA(ctx context.Context, hostname string, want netip.Addr, on func(dnswait.Progress)) error {
	f.calls++
	return f.err
}

type fakeReach struct{ status reach.Status }

func (f *fakeReach) CheckURL(ctx context.Context, url, contains string) reach.Result {
	return reach.Result{Status: f.status, Method: "fake"}
}

type fakeProxy struct {
	installed []caddy.Config
	env       []map[string]string
	err       error
}

func (f *fakeProxy) Install(ctx context.Context, c caddy.Config, env map[string]string) error {
	f.installed = append(f.installed, c)
	f.env = append(f.env, env)
	return f.err
}
func (f *fakeProxy) Version(ctx context.Context) string { return "v2.10.0" }

type fakeCert struct {
	expiry time.Time
	err    error
}

func (f *fakeCert) Wait(ctx context.Context, hostname string, port int, on func(time.Duration)) (time.Time, error) {
	if f.err != nil {
		return time.Time{}, f.err
	}
	if f.expiry.IsZero() {
		return time.Now().Add(90 * 24 * time.Hour), nil
	}
	return f.expiry, nil
}

type fakeAvail struct{ result domains.Availability }

func (f *fakeAvail) Check(ctx context.Context, hostname string) domains.Availability { return f.result }

type fakeServices struct {
	services         []service.Definition
	timers           []service.Timer
	removeServiceErr error
	status           service.Status
}

func (f *fakeServices) InstallService(ctx context.Context, d service.Definition) error {
	f.services = append(f.services, d)
	return nil
}
func (f *fakeServices) StartService(ctx context.Context, n string) error { return nil }
func (f *fakeServices) StopService(ctx context.Context, n string) error  { return nil }
func (f *fakeServices) RemoveService(ctx context.Context, n string) error {
	return f.removeServiceErr
}
func (f *fakeServices) ServiceStatus(ctx context.Context, n string) (service.Status, error) {
	if f.status != "" {
		return f.status, nil
	}
	return service.StatusRunning, nil
}
func (f *fakeServices) InstallTimer(ctx context.Context, t service.Timer) error {
	f.timers = append(f.timers, t)
	return nil
}
func (f *fakeServices) RemoveTimer(ctx context.Context, n string) error { return nil }
func (f *fakeServices) TimerInstalled(ctx context.Context, n string) (bool, error) {
	return len(f.timers) > 0, nil
}

// ---------------------------------------------------------------------------
// Harness.

// directProbe is a home connection with a cooperative router: the common case,
// and the one the happy path walks.
func directProbe() probe.Result {
	return probe.Result{
		Jellyfin: probe.Jellyfin{
			Found: true, Address: "127.0.0.1:8096", Version: "10.11.7",
			MeetsMinimum: true, ProxySourceAddress: "127.0.0.1",
		},
		Internet: probe.Internet{Reachable: true, PublicV4: netip.MustParseAddr("203.0.113.7")},
		Router: probe.Router{
			Reachable: true, PortMappingAvailable: true,
			Gateway: netip.MustParseAddr("192.168.1.1"), WANAddress: netip.MustParseAddr("203.0.113.7"),
			ControlURL: "http://192.168.1.1/ctl", ServiceType: "urn:WANIPConnection:1",
			Vendor: "ASUS",
		},
		Host:  probe.Host{LANAddress: netip.MustParseAddr("192.168.1.50")},
		Ports: probe.Ports{Free: map[int]bool{443: true, 8443: true}},
	}
}

type harness struct {
	w    *Wizard
	dynu *fakeDynu
	jf   *fakeJellyfin
	dns  *fakeDNS
	prox *fakeProxy
	svc  *fakeServices
	seed probe.Result
	log  *logging.Logger
	dir  string
}

func newHarness(t *testing.T, tweak func(*Options)) *harness {
	t.Helper()
	dir := t.TempDir()
	layout := paths.UnderRoot(dir)
	if err := layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	h := &harness{
		dynu: &fakeDynu{},
		jf:   &fakeJellyfin{admin: true},
		dns:  &fakeDNS{},
		prox: &fakeProxy{},
		svc:  &fakeServices{},
		seed: directProbe(),
		log:  logging.Discard(),
		dir:  dir,
	}

	opts := Options{
		Layout:         layout,
		Log:            h.log,
		Store:          state.NewStore(layout.StateFile()),
		Secrets:        secrets.NewFileStore(layout.SecretFile()),
		Version:        "test",
		SyncBinary:     filepath.Join(dir, "rasa-sync"),
		Probe:          func(ctx context.Context) probe.Result { return h.seed },
		NewDynu:        func(string) DynuAPI { return h.dynu },
		NewJellyfin:    func(string, string) JellyfinAPI { return h.jf },
		NewMapper:      func(string, string) PortMapper { return &fakeMapper{} },
		NewServices:    func() (service.Manager, error) { return h.svc, nil },
		NewReach:       func(netip.Addr) Reacher { return &fakeReach{status: reach.Reachable} },
		DNSWait:        h.dns,
		CertWait:       &fakeCert{},
		Availability:   &fakeAvail{result: domains.Unclaimed},
		Proxy:          h.prox,
		RemoveFirewall: func(context.Context) error { return nil },
	}
	if tweak != nil {
		tweak(&opts)
	}

	w, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	h.w = w
	return h
}

// runHappyPath walks every screen the way a user would.
func (h *harness) runHappyPath(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if err := h.w.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := h.w.SignIn(ctx, "admin", "hunter2"); err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	if err := h.w.SetDynuKey(ctx, testKey); err != nil {
		t.Fatalf("SetDynuKey: %v", err)
	}
	if err := h.w.ClaimName(ctx, "mymedia", "freeddns.org"); err != nil {
		t.Fatalf("ClaimName: %v", err)
	}
	if err := h.w.OpenPort(ctx); err != nil {
		t.Fatalf("OpenPort: %v", err)
	}
	if err := h.w.Install(ctx); err != nil {
		t.Fatalf("Install: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tests.

func TestHappyPath(t *testing.T) {
	h := newHarness(t, nil)
	h.runHappyPath(t)

	m := h.w.Model()
	if m.Screen != ScreenDone {
		t.Fatalf("ended on %s, want done", m.Screen)
	}
	if m.Result.URL != "https://mymedia.freeddns.org" {
		t.Errorf("URL = %q", m.Result.URL)
	}
	if m.Phase != state.Running {
		t.Errorf("phase = %s, want RUNNING", m.Phase)
	}
	for _, s := range m.Setup {
		if s.State != StepDone && s.State != StepSkipped {
			t.Errorf("setup step %s ended %s", s.ID, s.State)
		}
	}

	// The proxy must be told the hostname is its own zone: a Dynu DDNS name
	// sits at the apex, and without this the DNS challenge looks in the wrong
	// place (SPEC.md §12).
	if len(h.prox.installed) != 1 {
		t.Fatalf("proxy installed %d times", len(h.prox.installed))
	}
	cfg := h.prox.installed[0]
	if cfg.OwnDomain != "mymedia.freeddns.org" {
		t.Errorf("OwnDomain = %q, want the full hostname", cfg.OwnDomain)
	}
	if cfg.UpstreamAddress != "127.0.0.1:8096" {
		t.Errorf("upstream = %q, want the port read from network.xml", cfg.UpstreamAddress)
	}
	if cfg.DynuAPIKeyEnv != TokenEnvVar {
		t.Errorf("the credential is not passed by environment: %q", cfg.DynuAPIKeyEnv)
	}

	// The address record must publish IPv4 with the family flag set, or Dynu
	// accepts the address and publishes nothing.
	if len(h.dynu.created) == 0 {
		t.Fatal("no hostname was created")
	}
	last := h.dynu.created[len(h.dynu.created)-1]
	if !last.IPv4 || last.IPv4Address != "203.0.113.7" {
		t.Errorf("created %+v, want IPv4 published", last)
	}

	if len(h.svc.timers) != 1 {
		t.Errorf("the address sync task was not installed (%d timers)", len(h.svc.timers))
	}
	if len(h.jf.applied) != 1 || h.jf.applied[0].PublicURL != "https://mymedia.freeddns.org" {
		t.Errorf("jellyfin settings = %+v", h.jf.applied)
	}
	if _, err := recoveryStat(h.dir); err != nil {
		t.Errorf("no recovery file was written: %v", err)
	}
}

// The Dynu key is the one secret in the product. It reaches the model layer
// only as a boolean, and this asserts that structurally: serialise the whole
// snapshot and look for it.
func TestSecretsNeverReachTheModel(t *testing.T) {
	h := newHarness(t, nil)
	h.runHappyPath(t)

	body, err := json.Marshal(h.w.Model())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), testKey) {
		t.Fatal("the Dynu API key is present in the model the UI renders")
	}
	if strings.Contains(string(body), "per-hostname-token") {
		t.Fatal("the per-hostname Dynu token is present in the model")
	}
	if !h.w.Model().DynuKey {
		t.Error("the model should still record that a key exists")
	}
}

// A collision is the one failure that must not happen during the unattended
// stretch. It has to land the user back on the name field with alternatives.
func TestTakenNameReturnsToTheNameScreen(t *testing.T) {
	h := newHarness(t, nil)
	h.dynu.createFn = func(r dynu.CreateDomainRequest) (*dynu.Domain, error) {
		return nil, &dynu.APIError{StatusCode: 505, Message: "already exists"}
	}

	ctx := context.Background()
	if err := h.w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.w.SignIn(ctx, "admin", "pw"); err != nil {
		t.Fatal(err)
	}
	if err := h.w.SetDynuKey(ctx, testKey); err != nil {
		t.Fatal(err)
	}

	err := h.w.ClaimName(ctx, "media", "freeddns.org")
	re, ok := rasaerr.As(err)
	if !ok || re.Code != rasaerr.CodeHostnameTaken {
		t.Fatalf("got %v, want a hostname-taken error", err)
	}

	m := h.w.Model()
	if m.Screen != ScreenName {
		t.Errorf("screen = %s, want to stay on the name screen", m.Screen)
	}
	if len(m.Name.Suggestions) == 0 {
		t.Fatal("no alternatives were offered")
	}
	for _, s := range m.Name.Suggestions {
		if !strings.HasPrefix(s, "media.") {
			t.Errorf("suggestion %q changed the name rather than the domain", s)
		}
	}
}

// A regular Jellyfin account authenticates happily and then cannot write
// network settings. Catching it here is the difference between a clear message
// and a failure four screens later.
func TestNonAdminIsRejectedAtSignIn(t *testing.T) {
	h := newHarness(t, nil)
	h.jf.admin = false

	ctx := context.Background()
	if err := h.w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	err := h.w.SignIn(ctx, "guest", "pw")
	re, ok := rasaerr.As(err)
	if !ok || re.Code != rasaerr.CodeJellyfinAuth {
		t.Fatalf("got %v, want an auth error", err)
	}
	if h.w.Model().Jellyfin.SignedIn {
		t.Error("a non-admin was recorded as signed in")
	}
	if strings.Contains(strings.ToLower(re.Message), "403") {
		t.Error("the user-facing message carries a status code")
	}
}

// A second run over a finished install must not send the user back to Dynu's
// signup page. The stored credential is what makes repair bearable.
func TestRepairSkipsTheDynuScreen(t *testing.T) {
	dir := t.TempDir()
	layout := paths.UnderRoot(dir)
	if err := layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout.StateFile())
	creds := secrets.NewFileStore(layout.SecretFile())

	prior := state.NewState("previous-run")
	prior.Reset(state.Running)
	prior.Hostname = "mymedia.freeddns.org"
	prior.ListenPort = 443
	if err := store.Save(prior); err != nil {
		t.Fatal(err)
	}
	if err := creds.Set(secrets.DynuAPIKey, testKey); err != nil {
		t.Fatal(err)
	}

	h := newHarness(t, func(o *Options) {
		o.Layout = layout
		o.Store = store
		o.Secrets = creds
	})

	m := h.w.Model()
	if !m.Repair {
		t.Error("a previous install was not recognised")
	}
	if !m.DynuKey {
		t.Fatal("the stored credential was not loaded")
	}
	if m.Name.Label != "mymedia" || m.Name.Parent != "freeddns.org" {
		t.Errorf("the previous name was not restored: %+v", m.Name)
	}

	ctx := context.Background()
	if err := h.w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.w.SignIn(ctx, "admin", "pw"); err != nil {
		t.Fatal(err)
	}
	if got := h.w.Model().Screen; got != ScreenName {
		t.Errorf("screen after sign-in = %s, want the name screen", got)
	}
}

// A router without NAT hairpinning fails a check made from inside the house
// while working perfectly from outside it. Reporting that as failure would
// send users to fix something that is not broken.
func TestInconclusiveReachabilityStillFinishes(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.NewReach = func(netip.Addr) Reacher { return &fakeReach{status: reach.Inconclusive} }
	})
	h.runHappyPath(t)

	m := h.w.Model()
	if m.Screen != ScreenDone {
		t.Fatalf("screen = %s, want done", m.Screen)
	}
	var verify Step
	for _, s := range m.Setup {
		if s.ID == SetupVerify {
			verify = s
		}
	}
	if verify.State != StepDone {
		t.Errorf("verify step = %s, want done", verify.State)
	}
	if !hasWarning(m, "unverified") {
		t.Error("the user was not told the check was inconclusive")
	}
}

// An IPv6-only connection publishes AAAA and has no A record to wait for.
// Waiting anyway would time out and fail a setup that works.
func TestIPv6OnlySkipsTheAddressWait(t *testing.T) {
	h := newHarness(t, nil)
	seed := directProbe()
	seed.Internet.PublicV4 = netip.Addr{}
	seed.Internet.PublicV6 = netip.MustParseAddr("2001:db8::1")
	seed.Router.WANAddress = netip.Addr{}
	h.seed = seed

	h.runHappyPath(t)

	if h.dns.calls != 0 {
		t.Errorf("waited for an A record %d times on an IPv6-only connection", h.dns.calls)
	}
	var dnsStep Step
	for _, s := range h.w.Model().Setup {
		if s.ID == SetupDNS {
			dnsStep = s
		}
	}
	if dnsStep.State != StepSkipped {
		t.Errorf("DNS step = %s, want skipped", dnsStep.State)
	}
	last := h.dynu.created[len(h.dynu.created)-1]
	if !last.IPv6 || last.IPv6Address != "2001:db8::1" {
		t.Errorf("created %+v, want the IPv6 family flag set", last)
	}
}

// When the router will not open the port, the user gets named instructions
// with the values filled in — never a dead end.
func TestFailedMappingShowsTheGuide(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.NewMapper = func(string, string) PortMapper {
			return &fakeMapper{err: &portmap.UPnPError{Code: 718}}
		}
	})

	ctx := context.Background()
	if err := h.w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.w.SignIn(ctx, "admin", "pw"); err != nil {
		t.Fatal(err)
	}
	if err := h.w.SetDynuKey(ctx, testKey); err != nil {
		t.Fatal(err)
	}
	if err := h.w.ClaimName(ctx, "mymedia", "freeddns.org"); err != nil {
		t.Fatal(err)
	}
	if err := h.w.OpenPort(ctx); err != nil {
		t.Fatal(err)
	}

	m := h.w.Model()
	if m.Screen != ScreenPort {
		t.Fatalf("screen = %s, want the port screen", m.Screen)
	}
	if len(m.Port.Instructions) == 0 {
		t.Fatal("no instructions were rendered")
	}
	if len(m.Port.Values) == 0 {
		t.Fatal("no values were rendered; the values are what users get wrong")
	}
	var sawPort bool
	for _, v := range m.Port.Values {
		if strings.Contains(v.Value, "443") {
			sawPort = true
		}
	}
	if !sawPort {
		t.Errorf("the port to forward is not among the values: %+v", m.Port.Values)
	}
}

// Skipping the port is allowed, because a user who cannot forward one still
// gets a certificate, an address, and a recovery file. Refusing would leave
// them with none of it.
func TestSkipPortCarriesAWarningToTheEnd(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.NewMapper = func(string, string) PortMapper { return &fakeMapper{err: &portmap.UPnPError{Code: 718}} }
		o.NewReach = func(netip.Addr) Reacher { return &fakeReach{status: reach.Unreachable} }
	})

	ctx := context.Background()
	mustAll(t,
		func() error { return h.w.Start(ctx) },
		func() error { return h.w.SignIn(ctx, "admin", "pw") },
		func() error { return h.w.SetDynuKey(ctx, testKey) },
		func() error { return h.w.ClaimName(ctx, "mymedia", "freeddns.org") },
		func() error { return h.w.OpenPort(ctx) },
		func() error { return h.w.SkipPort(ctx) },
		func() error { return h.w.Install(ctx) },
	)

	m := h.w.Model()
	if m.Screen != ScreenDone {
		t.Fatalf("screen = %s, want done", m.Screen)
	}
	if !hasWarning(m, "port_not_confirmed") {
		t.Error("the unconfirmed port warning was lost before the final screen")
	}
	if !hasWarning(m, "unreachable") {
		t.Error("the failed external check produced no warning")
	}
}

// The credential must be stored before anything depends on it, because it is
// what keeps the address current after RASA is gone.
func TestDynuKeyIsPersisted(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	if err := h.w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.w.SignIn(ctx, "admin", "pw"); err != nil {
		t.Fatal(err)
	}
	if err := h.w.SetDynuKey(ctx, testKey); err != nil {
		t.Fatal(err)
	}

	got, err := secrets.NewFileStore(paths.UnderRoot(h.dir).SecretFile()).Get(secrets.DynuAPIKey)
	if err != nil {
		t.Fatalf("credential not stored: %v", err)
	}
	if got != testKey {
		t.Error("stored credential does not round-trip")
	}
}

// A rejected key must be caught on the screen the user copied it from, not
// three screens later.
func TestBadDynuKeyIsRejectedImmediately(t *testing.T) {
	h := newHarness(t, nil)
	h.dynu.listErr = &dynu.APIError{StatusCode: 401, Message: "unauthorized"}

	ctx := context.Background()
	if err := h.w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.w.SignIn(ctx, "admin", "pw"); err != nil {
		t.Fatal(err)
	}
	err := h.w.SetDynuKey(ctx, testKey)
	re, ok := rasaerr.As(err)
	if !ok || re.Code != rasaerr.CodeDynuAuth {
		t.Fatalf("got %v, want a rejected-key error", err)
	}
	if h.w.Model().DynuKey {
		t.Error("a rejected key was recorded as accepted")
	}
	if _, err := secrets.NewFileStore(paths.UnderRoot(h.dir).SecretFile()).Get(secrets.DynuAPIKey); err == nil {
		t.Error("a rejected key was stored")
	}
}

// The name field is advisory. It must never refuse to let someone try, and
// never claim more than a DNS lookup can support.
func TestCheckNameIsAdvisoryOnly(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.Availability = &fakeAvail{result: domains.InUse}
	})
	got := h.w.CheckName(context.Background(), "media", "freeddns.org")
	if got.Availability != "in_use" {
		t.Errorf("availability = %q", got.Availability)
	}
	if len(got.Suggestions) == 0 {
		t.Error("a collision offered no alternatives")
	}

	// A name still being typed gets advice, not an error, and no lookup.
	short := h.w.CheckName(context.Background(), "m", "freeddns.org")
	if short.Advice == "" {
		t.Error("a short name produced no advice")
	}
	if short.Availability != "" {
		t.Error("an invalid name was looked up anyway")
	}
}

func TestBusyRejectsASecondOperation(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, func(o *Options) {
		o.Probe = func(ctx context.Context) probe.Result {
			<-release
			return directProbe()
		}
	})

	done := make(chan error, 1)
	go func() { done <- h.w.Start(context.Background()) }()

	// Wait for Start to take the flag rather than assuming a delay is enough.
	deadline := time.Now().Add(2 * time.Second)
	for !h.w.Model().Busy {
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("Start never reported busy")
		}
		time.Sleep(time.Millisecond)
	}
	if err := h.w.Start(context.Background()); err != ErrBusy {
		t.Errorf("second Start returned %v, want ErrBusy", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Subscribers get the current state on subscribe and never block the pipeline.
func TestSubscribeDeliversTheLatestSnapshot(t *testing.T) {
	h := newHarness(t, nil)
	ch, stop := h.w.Subscribe()
	defer stop()

	first := <-ch
	if first.Screen != ScreenWelcome {
		t.Errorf("first snapshot = %s, want welcome", first.Screen)
	}

	// Produce more updates than the channel can hold. Nothing may block, and
	// the newest snapshot must be the one waiting.
	for i := 0; i < 50; i++ {
		h.w.update(func(m *Model) { m.Explanation = "update" })
	}
	got := <-ch
	if got.Revision < 50 {
		t.Errorf("revision = %d, want a recent snapshot", got.Revision)
	}
}

func TestBlockedProbeStopsWithAnExplanation(t *testing.T) {
	h := newHarness(t, nil)
	seed := directProbe()
	seed.Internet.Reachable = false
	h.seed = seed

	err := h.w.Start(context.Background())
	re, ok := rasaerr.As(err)
	if !ok || re.Code != rasaerr.CodeNoRouteToInternet {
		t.Fatalf("got %v, want a no-internet error", err)
	}
	m := h.w.Model()
	if m.Screen != ScreenBlocked {
		t.Errorf("screen = %s, want blocked", m.Screen)
	}
	if m.Err == nil || m.Err.Message == "" {
		t.Fatal("the blocked screen has nothing to show")
	}
	if len(m.Err.Actions) == 0 {
		t.Error("the blocked screen offers no way forward")
	}
}

func TestOldJellyfinIsRefusedBeforeAnythingIsChanged(t *testing.T) {
	h := newHarness(t, nil)
	seed := directProbe()
	seed.Jellyfin.Version = "10.10.3"
	seed.Jellyfin.MeetsMinimum = false
	h.seed = seed

	err := h.w.Start(context.Background())
	re, ok := rasaerr.As(err)
	if !ok || re.Code != rasaerr.CodeJellyfinTooOld {
		t.Fatalf("got %v, want a version error", err)
	}
	if len(h.dynu.created) != 0 {
		t.Error("a hostname was created before the version check passed")
	}
}

// The state file is what a later repair reads, and it must never hold a
// secret (SPEC.md §14).
func TestStateFileHoldsNoSecrets(t *testing.T) {
	h := newHarness(t, nil)
	h.runHappyPath(t)

	body, err := readFile(filepath.Join(h.dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{testKey, "per-hostname-token"} {
		if strings.Contains(body, secret) {
			t.Errorf("the state file contains a secret")
		}
	}
	if !strings.Contains(body, "mymedia.freeddns.org") {
		t.Error("the state file did not record the hostname")
	}
}

// Re-running Install over a finished setup must not fail. Interrupted installs
// are the expected case, not the exception.
func TestInstallIsIdempotent(t *testing.T) {
	h := newHarness(t, nil)
	h.runHappyPath(t)

	if err := h.w.Install(context.Background()); err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if got := h.w.Model().Phase; got != state.Running {
		t.Errorf("phase after re-run = %s", got)
	}
	if len(h.prox.installed) != 2 {
		t.Errorf("proxy installs = %d, want the configuration rewritten", len(h.prox.installed))
	}
}

// ---------------------------------------------------------------------------

func hasWarning(m Model, code string) bool {
	for _, w := range m.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

func mustAll(t *testing.T, steps ...func() error) {
	t.Helper()
	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("step %d: %v", i+1, err)
		}
	}
}

func readFile(p string) (string, error) {
	b, err := os.ReadFile(p)
	return string(b), err
}

func recoveryStat(dir string) (os.FileInfo, error) {
	return os.Stat(paths.UnderRoot(dir).RecoveryFile())
}

// A state file left at PROBED means setup was started and abandoned before
// anything was configured. Calling that a prior install tells the user their
// machine is in a state it is not, and offers to remove something that was
// never created.
func TestProbedButUnconfiguredIsNotARepair(t *testing.T) {
	dir := t.TempDir()
	layout := paths.UnderRoot(dir)
	if err := layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout.StateFile())

	abandoned := state.NewState("earlier-run")
	abandoned.Reset(state.Probed)
	if err := store.Save(abandoned); err != nil {
		t.Fatal(err)
	}

	h := newHarness(t, func(o *Options) {
		o.Layout = layout
		o.Store = store
	})
	if h.w.Model().Repair {
		t.Error("a probe-only state file was reported as a previous install")
	}

	// Whereas a claimed hostname is a real thing to repair.
	claimed := state.NewState("earlier-run")
	claimed.Reset(state.Running)
	claimed.Hostname = "mymedia.freeddns.org"
	if err := store.Save(claimed); err != nil {
		t.Fatal(err)
	}
	h2 := newHarness(t, func(o *Options) {
		o.Layout = layout
		o.Store = store
	})
	if !h2.w.Model().Repair {
		t.Error("a configured install was not recognised as repairable")
	}
}

// Journey step 9 must begin on its own. A screen that waits for a click the
// user was never asked for — while its own text says it is working — is how
// the port step behaved on first contact with a real network.
func TestClaimingANameStartsThePortStep(t *testing.T) {
	attempted := false
	h := newHarness(t, func(o *Options) {
		o.NewMapper = func(string, string) PortMapper {
			attempted = true
			return &fakeMapper{}
		}
	})

	ctx := context.Background()
	mustAll(t,
		func() error { return h.w.Start(ctx) },
		func() error { return h.w.SignIn(ctx, "admin", "pw") },
		func() error { return h.w.SetDynuKey(ctx, testKey) },
		func() error { return h.w.ClaimName(ctx, "mymedia", "freeddns.org") },
	)

	if !attempted {
		t.Fatal("claiming a name did not attempt the port mapping")
	}
	// A cooperative router means there is nothing to show, so the flow should
	// already have moved past the port screen without the user touching it.
	if got := h.w.Model().Screen; got != ScreenSetup {
		t.Errorf("screen = %s, want the setup screen to have been reached automatically", got)
	}
}

// When the router will not cooperate, the user lands on the port screen with
// instructions already rendered — never on an empty one.
func TestPortScreenIsNeverShownEmpty(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.NewMapper = func(string, string) PortMapper {
			return &fakeMapper{err: &portmap.UPnPError{Code: 718}}
		}
	})

	ctx := context.Background()
	mustAll(t,
		func() error { return h.w.Start(ctx) },
		func() error { return h.w.SignIn(ctx, "admin", "pw") },
		func() error { return h.w.SetDynuKey(ctx, testKey) },
		func() error { return h.w.ClaimName(ctx, "mymedia", "freeddns.org") },
	)

	m := h.w.Model()
	if m.Screen != ScreenPort {
		t.Fatalf("screen = %s, want the port screen", m.Screen)
	}
	if len(m.Port.Instructions) == 0 || len(m.Port.Values) == 0 {
		t.Fatal("the port screen was shown with nothing on it")
	}
}

// A repair run finds its own proxy holding the port. Treating that as a
// conflict downgrades a working https://name:443 to https://name:8443 and
// blames "another program" — observed on a real repair.
func TestRepairKeepsThePortItsOwnProxyHolds(t *testing.T) {
	dir := t.TempDir()
	layout := paths.UnderRoot(dir)
	if err := layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout.StateFile())

	prior := state.NewState("earlier")
	prior.Reset(state.Running)
	prior.Hostname = "mymedia.freeddns.org"
	prior.ListenPort = 443
	if err := store.Save(prior); err != nil {
		t.Fatal(err)
	}

	h := newHarness(t, func(o *Options) {
		o.Layout = layout
		o.Store = store
	})
	// 443 busy, held by our own running service.
	seed := directProbe()
	seed.Ports = probe.Ports{
		Free:   map[int]bool{443: false, 8443: true},
		Holder: map[int]string{443: "caddy.exe"},
	}
	h.seed = seed

	if err := h.w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.w.Model().ListenPort; got != 443 {
		t.Errorf("listen port = %d, want 443 kept rather than falling back", got)
	}
	for _, warn := range h.w.Model().Warnings {
		if warn.Code == "non_standard_port" {
			t.Error("the user was warned about a port conflict with RASA's own proxy")
		}
	}
}

// A port held by something that is not ours must still fall back.
func TestGenuineConflictStillFallsBack(t *testing.T) {
	dir := t.TempDir()
	layout := paths.UnderRoot(dir)
	if err := layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(layout.StateFile())
	prior := state.NewState("earlier")
	prior.Reset(state.Running)
	prior.Hostname = "mymedia.freeddns.org"
	prior.ListenPort = 443
	if err := store.Save(prior); err != nil {
		t.Fatal(err)
	}

	h := newHarness(t, func(o *Options) {
		o.Layout = layout
		o.Store = store
		// Our proxy is NOT running, so whatever holds 443 belongs to someone else.
		o.NewServices = func() (service.Manager, error) {
			return &fakeServices{status: service.StatusStopped}, nil
		}
	})
	seed := directProbe()
	seed.Ports = probe.Ports{
		Free:   map[int]bool{443: false, 8443: true},
		Holder: map[int]string{443: "nginx.exe"},
	}
	h.seed = seed

	if err := h.w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.w.Model().ListenPort; got != 8443 {
		t.Errorf("listen port = %d, want the 8443 fallback for a real conflict", got)
	}
}

// The ceiling shown to the user must be the one actually enforced. Writing the
// number into copy by hand is how a screen ends up promising nine minutes
// while the code gives up at five — which is the bug that produced a red line
// 55 seconds before a real certificate arrived.
func TestCertificateWaitCopyMatchesTheEnforcedTimeout(t *testing.T) {
	if CertificateWait <= caddy.PropagationTimeout {
		t.Fatalf("RASA waits %s but allows Caddy %s for propagation alone; it will "+
			"abandon issuances that are still in progress",
			CertificateWait, caddy.PropagationTimeout)
	}
	want := fmt.Sprintf("%d minutes", int(CertificateWait.Minutes()))
	if got := CertificateWaitText(); got != want {
		t.Errorf("CertificateWaitText() = %q, want %q", got, want)
	}
}

// A slow issuance must say how long it is allowed to take, so that waiting
// does not look like failing. Tested on the pure function rather than through
// the model channel, which holds one snapshot and drops the rest by design —
// subscribing to it made this test pass locally and fail on every CI platform.
func TestSlowCertificateExplainsItself(t *testing.T) {
	early := certProgressNote(10 * time.Second)
	if strings.Contains(early, CertificateWaitText()) {
		t.Errorf("the ceiling appears immediately, which makes a fast issuance look slow: %q", early)
	}
	if !strings.Contains(early, "Still working") {
		t.Errorf("early note = %q", early)
	}

	late := certProgressNote(90 * time.Second)
	if !strings.Contains(late, CertificateWaitText()) {
		t.Errorf("a slow issuance never says how long it may take: %q", late)
	}

	// The boundary itself, so the threshold cannot drift unnoticed.
	if strings.Contains(certProgressNote(certExplainAfter), CertificateWaitText()) {
		t.Error("the ceiling appears at the threshold rather than after it")
	}
	if !strings.Contains(certProgressNote(certExplainAfter+time.Second), CertificateWaitText()) {
		t.Error("the ceiling never appears just past the threshold")
	}
}

// A key is checked as the user types, and constructing a real Dynu client
// registers its key with the log redactor as a secret to be scrubbed. Checking
// a half-typed key would therefore register "d" as a secret and redact that
// letter from every log line in the product, so nothing short may reach the
// constructor at all.
func TestShortDynuKeyIsNeverHandedToAClient(t *testing.T) {
	h := newHarness(t, nil)

	var built []string
	h.w.opts.NewDynu = func(key string) DynuAPI {
		built = append(built, key)
		return h.dynu
	}

	for _, partial := range []string{"", "d", "dynu", "dynu-api-key"} {
		view := h.w.CheckDynuKey(context.Background(), partial)
		if view.State != "unknown" {
			t.Errorf("CheckDynuKey(%q) = %q, want unknown", partial, view.State)
		}
	}
	if len(built) != 0 {
		t.Fatalf("a client was built for a partial key: %q", built)
	}

	// A complete one is checked, and reported without being stored.
	if view := h.w.CheckDynuKey(context.Background(), testKey); view.State != "valid" {
		t.Fatalf("CheckDynuKey(complete) = %q, want valid", view.State)
	}
	if len(built) != 1 {
		t.Fatalf("built %d clients for one complete key, want 1", len(built))
	}
	if h.w.Model().DynuKey {
		t.Error("checking a key marked it as set; checking must not commit")
	}
}

func TestRejectedDynuKeySaysSo(t *testing.T) {
	h := newHarness(t, nil)
	h.dynu.listErr = errors.New("401 unauthorized")

	view := h.w.CheckDynuKey(context.Background(), testKey)
	if view.State != "rejected" {
		t.Fatalf("state = %q, want rejected", view.State)
	}
	if view.Message == "" {
		t.Error("a rejection with no message tells the user nothing")
	}
}
