package wizard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/domains"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/dynu"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/mode"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/paths"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/portmap"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/probe"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/rasaerr"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/routerguide"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/secrets"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/service"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/state"
)

// ErrBusy is returned when an operation is already running. The UI disables
// its controls while Model.Busy is set, so reaching this means a second client
// is connected or someone got ahead of the render.
var ErrBusy = errors.New("something is already running")

// Options configures a Wizard. Only the first group is required; the rest are
// seams that default to the real implementations.
type Options struct {
	Layout  paths.Layout
	Log     *logging.Logger
	Store   *state.Store
	Secrets secrets.Store
	Catalog *domains.Catalog
	Version string

	// ACMECA overrides the certificate authority. Development builds pass the
	// staging directory, because five failed validations per hostname per hour
	// is not many when you are debugging (SPEC.md §19).
	ACMECA string
	// Email is optional and only used for expiry notices.
	Email string
	// CaddyBinary is the bundled proxy. Resolved by the caller so that a
	// missing binary is a startup complaint rather than a failure two minutes
	// into setup.
	CaddyBinary string
	// SyncBinary is the address-sync helper registered as a scheduled task.
	SyncBinary string

	Probe        func(ctx context.Context) probe.Result
	NewDynu      func(apiKey string) DynuAPI
	NewJellyfin  func(baseURL, apiKey string) JellyfinAPI
	NewMapper    func(controlURL, serviceType string) PortMapper
	NewServices  func() (service.Manager, error)
	NewReach     func(addr netip.Addr) Reacher
	DNSWait      DNSWaiter
	CertWait     CertWaiter
	Availability AvailabilityChecker
	Proxy        ProxyInstaller
	// RemoveFirewall takes the inbound rule down. A seam because the real one
	// shells out to netsh, which a test must not be allowed to do.
	RemoveFirewall func(ctx context.Context) error
	Now            func() time.Time
}

// Wizard owns the setup flow and the model the UI renders.
type Wizard struct {
	opts Options
	log  *logging.Logger

	mu    sync.Mutex
	model Model
	st    *state.State

	// probe result and decision, carried between screens.
	probed   probe.Result
	decision mode.Decision

	// live clients, held because authentication lives inside them.
	jf   JellyfinAPI
	dyn  DynuAPI
	dkey string

	// claimed is the Dynu hostname record once it exists.
	claimed *dynu.Domain

	// guide is the rendered port-forwarding instruction set, kept for the
	// recovery file — the user needs these values again if they ever reset
	// their router, long after RASA is gone.
	guideText string

	// portSwitched records that setup has already moved to the fallback port
	// after a failed reachability check, so it cannot do it twice and cannot
	// loop.
	portSwitched bool

	// wantGenericGuide is set once the user says the router-specific guide does
	// not match what is in front of them. Their eyes beat a UPnP vendor string.
	wantGenericGuide bool

	busy bool
	subs map[chan Model]struct{}
}

// New builds a Wizard and reads any existing state.
func New(opts Options) (*Wizard, error) {
	if opts.Log == nil {
		opts.Log = logging.Discard()
	}
	if opts.Store == nil {
		return nil, errors.New("wizard needs a state store")
	}
	if opts.Catalog == nil {
		c, err := domains.Embedded()
		if err != nil {
			return nil, err
		}
		opts.Catalog = c
	}
	opts.defaults()

	w := &Wizard{
		opts: opts,
		log:  opts.Log,
		subs: make(map[chan Model]struct{}),
	}

	st, err := opts.Store.Load()
	repair := false
	switch {
	case errors.Is(err, state.ErrNotFound):
		st = state.NewState(opts.Log.RunID())
	case err != nil:
		// A damaged state file must not block repair. Starting clean loses
		// the record of a previous install, which is worse than nothing but
		// far better than refusing to run on the machine that needs fixing.
		opts.Log.Warn("existing state could not be read; starting fresh", slog.Any("err", err))
		st = state.NewState(opts.Log.RunID())
	default:
		// A hostname is the thing that makes a run repairable. Phase alone is
		// too loose: a user who quit during the probe has a state file at
		// PROBED and nothing configured, and telling them "you've set this up
		// before" — then offering to remove it — describes a machine that does
		// not exist.
		repair = st.Hostname != ""
	}
	w.st = st

	w.model = Model{
		Screen:  ScreenWelcome,
		Version: opts.Version,
		Repair:  repair,
		Phase:   st.Phase,
		Checks:  initialChecks(),
		Setup:   initialSetup(),
		Domains: domainOptions(opts.Catalog),
		Name:    NameView{Parent: opts.Catalog.Default().Name},
	}
	if st.Hostname != "" {
		if label, parent, ok := opts.Catalog.Split(st.Hostname); ok {
			w.model.Name.Label, w.model.Name.Parent = label, parent
		}
		w.model.Name.Hostname = st.Hostname
		opts.Log.Redactor().RegisterAddress(st.Hostname)
	}
	w.model.Result.URL = st.URL()
	w.model.Result.RecoveryFile = opts.Layout.RecoveryFile()
	w.model.Result.LogFile = opts.Layout.RASALog()
	for _, warn := range st.Warnings {
		w.model.Warnings = append(w.model.Warnings, Warning{Code: warn.Code, Text: warn.Text})
	}

	// A stored credential means a previous run got as far as the Dynu screen.
	// Loading it here is what lets a repair skip straight past signup.
	if opts.Secrets != nil {
		if key, err := opts.Secrets.Get(secrets.DynuAPIKey); err == nil && key != "" {
			w.dkey = key
			w.dyn = opts.NewDynu(key)
			w.model.DynuKey = true
			opts.Log.Redactor().RegisterSecret(key)
		}
	}
	if w.model.Result.URL != "" {
		w.setQR()
	}
	return w, nil
}

// Model returns a snapshot.
func (w *Wizard) Model() Model {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.model.clone()
}

// Subscribe returns a channel of snapshots and a function to stop it.
//
// The channel holds one snapshot and the newest wins: a slow client should see
// the current state late, never a queue of stale ones. Since every snapshot is
// complete, dropping intermediate frames loses nothing.
func (w *Wizard) Subscribe() (<-chan Model, func()) {
	ch := make(chan Model, 1)
	w.mu.Lock()
	w.subs[ch] = struct{}{}
	snapshot := w.model.clone()
	w.mu.Unlock()

	ch <- snapshot
	return ch, func() {
		w.mu.Lock()
		if _, ok := w.subs[ch]; ok {
			delete(w.subs, ch)
			close(ch)
		}
		w.mu.Unlock()
	}
}

// update mutates the model under lock and publishes the result.
func (w *Wizard) update(fn func(m *Model)) {
	w.mu.Lock()
	fn(&w.model)
	// Centrally, so no caller can forget it and leave a back button offering a
	// journey the wizard has already made irreversible.
	w.model.CanBack = previousScreen(w.model.Screen, w.model.Repair) != ""
	w.model.Revision++
	snapshot := w.model.clone()
	for ch := range w.subs {
		select {
		case ch <- snapshot:
		default:
			// Full: replace what is queued so the newest snapshot is the one
			// waiting, rather than blocking the setup pipeline on a client.
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- snapshot:
			default:
			}
		}
	}
	w.mu.Unlock()
}

func (w *Wizard) begin() error {
	w.mu.Lock()
	if w.busy {
		w.mu.Unlock()
		return ErrBusy
	}
	w.busy = true
	w.mu.Unlock()
	w.update(func(m *Model) { m.Busy = true; m.Err = nil })
	return nil
}

func (w *Wizard) end() {
	w.mu.Lock()
	w.busy = false
	w.mu.Unlock()
	w.update(func(m *Model) { m.Busy = false })
}

// fail records a failure on the model and in the log, keeping the two
// audiences apart: the technical string goes to the file, the user projection
// goes to the screen (SPEC.md §15).
func (w *Wizard) fail(phase string, err error) error {
	re, ok := rasaerr.As(err)
	if !ok {
		re = &rasaerr.Error{
			Code:    rasaerr.CodeUnexpected,
			Message: "Something went wrong during setup.",
			Why:     "The details were written to the log, and setup can be run again safely.",
			Detail:  err.Error(),
			Actions: []rasaerr.Action{{ID: "retry", Label: "Try again", Kind: rasaerr.ActionRetry}},
		}
	}
	if re.Phase == "" {
		re.Phase = phase
	}
	w.log.WithPhase(phase).Error("setup step failed", slog.String("code", string(re.Code)), slog.String("detail", re.Error()))
	uf := re.User()
	w.update(func(m *Model) { m.Err = &uf })
	return re
}

func (w *Wizard) setStep(list string, id string, st StepState, note string) {
	w.update(func(m *Model) {
		steps := m.Checks
		if list == "setup" {
			steps = m.Setup
		}
		for i := range steps {
			if steps[i].ID == id {
				steps[i].State = st
				if note != "" {
					steps[i].Note = note
				}
				return
			}
		}
	})
}

func (w *Wizard) check(id string, st StepState, note string) { w.setStep("checks", id, st, note) }
func (w *Wizard) step(id string, st StepState, note string)  { w.setStep("setup", id, st, note) }

// save persists state. A failure here is logged but never stops setup: losing
// the ability to offer repair later is worse than nothing, and much better
// than abandoning a machine halfway through.
func (w *Wizard) save() {
	w.mu.Lock()
	st := w.st
	w.mu.Unlock()
	if err := w.opts.Store.Save(st); err != nil {
		w.log.Warn("could not save setup state", slog.Any("err", err))
	}
	w.update(func(m *Model) { m.Phase = st.Phase })
}

func (w *Wizard) advance(to state.Phase) {
	w.mu.Lock()
	// A resumed run re-executes earlier steps idempotently and then tries to
	// record a phase it is already past. That is success, not a fault, so it
	// is silent rather than a warning about an "illegal transition".
	if !w.st.Reached(to) {
		if err := w.st.Advance(to); err != nil {
			w.log.Warn("could not record phase", slog.String("to", string(to)), slog.Any("err", err))
		}
	}
	w.mu.Unlock()
	w.save()
}

// ---------------------------------------------------------------------------
// Journey step 5: checking things over.

// Start runs the pre-flight probe and decides the mode.
//
// It runs on every launch including a repair, because the thing most likely to
// have changed since the last run is the network.
func (w *Wizard) Start(ctx context.Context) error {
	if err := w.begin(); err != nil {
		return err
	}
	defer w.end()

	w.update(func(m *Model) {
		m.Screen = ScreenChecking
		m.Checks = initialChecks()
		for i := range m.Checks {
			m.Checks[i].State = StepRunning
		}
	})

	res := w.opts.Probe(ctx)
	w.reclaimOwnPort(ctx, &res)
	d := mode.Choose(res)

	w.mu.Lock()
	w.probed, w.decision = res, d
	w.st.JellyfinAddress = res.Jellyfin.Address
	w.st.JellyfinVersion = res.Jellyfin.Version
	w.st.Mode = d.Mode
	w.st.ListenPort = d.ListenPort
	for _, warn := range d.StateWarnings() {
		w.st.AddWarning(warn.Code, warn.Text)
	}
	w.mu.Unlock()

	w.log.WithPhase("probe").Decision("mode", string(d.Mode), d.Reason,
		slog.Int("listen_port", d.ListenPort),
		slog.Bool("needs_mapping", d.NeedsPortMapping),
		slog.Bool("needs_manual_forward", d.NeedsManualForward),
		slog.String("blocker", string(d.Blocker)),
	)

	sum := res.Summarize()
	w.check(CheckJellyfin, stateFor(res.Jellyfin.Found && res.Jellyfin.MeetsMinimum), sum.Jellyfin)
	w.check(CheckInternet, stateFor(res.Internet.Reachable), sum.Internet)
	w.check(CheckRouter, stateFor(res.Router.Reachable), sum.Router)
	w.check(CheckPorts, stateFor(d.Blocker != mode.BlockerPortsUnavailable), sum.Ports)

	w.update(func(m *Model) {
		m.Mode = d.Mode
		m.Explanation = d.Reason
		m.ListenPort = d.ListenPort
		m.Jellyfin = JellyfinView{
			Found:   res.Jellyfin.Found,
			Address: res.Jellyfin.Address,
			Version: res.Jellyfin.Version,
		}
		m.Warnings = nil
		for _, warn := range d.Warnings {
			m.Warnings = append(m.Warnings, Warning{Code: warn.Code, Text: warn.Text})
		}
	})

	w.advance(state.Probed)

	if d.Blocked() {
		err := blockerError(d, res)
		w.update(func(m *Model) { m.Screen = ScreenBlocked })
		return w.fail("probe", err)
	}
	if !res.Jellyfin.Found {
		w.update(func(m *Model) { m.Screen = ScreenBlocked })
		return w.fail("probe", rasaerr.JellyfinNotFound(nil))
	}
	if !res.Jellyfin.MeetsMinimum {
		w.update(func(m *Model) { m.Screen = ScreenBlocked })
		return w.fail("probe", rasaerr.JellyfinTooOld(res.Jellyfin.Version, probe.MinimumJellyfinVersion))
	}

	// Look the server up so the sign-in screen can name it. A failure here is
	// not fatal: the credentials screen still works, it is just less friendly.
	w.mu.Lock()
	w.jf = w.opts.NewJellyfin("http://"+res.Jellyfin.Address, "")
	jf := w.jf
	w.mu.Unlock()
	if info, err := jf.PublicInfo(ctx); err == nil {
		w.update(func(m *Model) {
			m.Jellyfin.ServerName = info.ServerName
			if info.Version != "" {
				m.Jellyfin.Version = info.Version
			}
		})
	}

	w.update(func(m *Model) { m.Screen = ScreenJellyfin })
	return nil
}

// reclaimOwnPort stops RASA treating its own proxy as a port conflict.
//
// On a repair run the Caddy service installed by the previous run is still
// listening on the chosen port, so the probe correctly reports it busy and the
// mode router falls back to 8443. The user then gets a worse address than they
// had, a warning blaming "another program", and a second listener — all
// because RASA did not recognise itself. Observed on a real repair, which
// turned a working https://name:443 into https://name:8443.
//
// The test is deliberately narrow: the port must be the one already recorded
// in state, and our own service must be running. Anything else is a genuine
// conflict and still falls back.
func (w *Wizard) reclaimOwnPort(ctx context.Context, res *probe.Result) {
	w.mu.Lock()
	port := w.st.ListenPort
	w.mu.Unlock()

	if port == 0 || res.Ports.Free == nil || res.Ports.Free[port] {
		return
	}
	mgr, err := w.opts.NewServices()
	if err != nil {
		return
	}
	status, err := mgr.ServiceStatus(ctx, service.CaddyServiceName)
	if err != nil || status != service.StatusRunning {
		return
	}

	res.Ports.Free[port] = true
	delete(res.Ports.Holder, port)
	w.log.WithPhase("probe").Decision("port "+strconv.Itoa(port), "kept",
		"it is held by RASA's own proxy from a previous run, not by another program")
}

func stateFor(ok bool) StepState {
	if ok {
		return StepDone
	}
	return StepFailed
}

func blockerError(d mode.Decision, res probe.Result) error {
	switch d.Blocker {
	case mode.BlockerNoInternet:
		return rasaerr.NoRouteToInternet(nil)
	case mode.BlockerPortsUnavailable:
		return rasaerr.PortHeldLocally(probe.PortPreferred, d.PortHolder, nil)
	default:
		return fmt.Errorf("setup cannot continue: %s", d.Reason)
	}
}

// ---------------------------------------------------------------------------
// Journey step 6: sign in to Jellyfin.

// SignIn authenticates against the local Jellyfin server.
//
// The account must be an administrator: a regular account authenticates
// happily and then cannot write network configuration, which would otherwise
// fail four screens later with nothing pointing at the cause.
func (w *Wizard) SignIn(ctx context.Context, username, password string) error {
	if err := w.begin(); err != nil {
		return err
	}
	defer w.end()

	w.mu.Lock()
	jf, addr := w.jf, w.probed.Jellyfin.Address
	w.mu.Unlock()
	if jf == nil {
		return w.fail("jellyfin", rasaerr.JellyfinNotFound(nil))
	}

	auth, err := jf.AuthenticateByName(ctx, username, password)
	if err != nil {
		return w.fail("jellyfin", rasaerr.JellyfinAuthRejected("http://"+addr, err))
	}
	if !auth.IsAdmin {
		return w.fail("jellyfin", &rasaerr.Error{
			Code:    rasaerr.CodeJellyfinAuth,
			Message: "That account isn't an administrator.",
			Why:     "Changing your server's network settings needs an admin account. Sign in with the account you set Jellyfin up with.",
			Detail:  fmt.Sprintf("user %s authenticated but is not an administrator", auth.UserID),
			Actions: []rasaerr.Action{{ID: "retry", Label: "Try a different account", Kind: rasaerr.ActionRetry}},
		})
	}

	// Read now rather than at the proxy step, so the address previewed on the
	// name screen is the one the user will actually type. A server with a base
	// path answers only under it.
	w.readJellyfinBase(ctx)

	w.log.WithPhase("jellyfin").OK("Signed in to Jellyfin.")
	w.update(func(m *Model) {
		m.Jellyfin.SignedIn = true
		m.Jellyfin.Username = auth.UserName
		m.Screen = ScreenDynu
		if m.DynuKey {
			// A repair already has the credential; skipping signup is the
			// whole reason it was loaded at startup.
			m.Screen = ScreenName
		}
	})
	return nil
}

// UseAPIKey accepts a Jellyfin API key instead of a username and password,
// which decision 3 keeps as an option for users who would rather not type
// their password into an installer.
func (w *Wizard) UseAPIKey(ctx context.Context, key string) error {
	if err := w.begin(); err != nil {
		return err
	}
	defer w.end()

	w.mu.Lock()
	addr := w.probed.Jellyfin.Address
	w.mu.Unlock()
	if addr == "" {
		return w.fail("jellyfin", rasaerr.JellyfinNotFound(nil))
	}

	key = strings.TrimSpace(key)
	if key == "" {
		return w.fail("jellyfin", rasaerr.JellyfinAuthRejected("http://"+addr, errors.New("no key supplied")))
	}
	w.opts.Log.Redactor().RegisterSecret(key)
	jf := w.opts.NewJellyfin("http://"+addr, key)

	// An API key carries no identity, so the only way to know it works is to
	// use it. PublicInfo would pass with any key at all because it needs
	// none; reading the network configuration is the same access the setup
	// later writes with, so a key that passes here will not fail there.
	cfg, err := jf.NetworkConfig(ctx)
	if err != nil {
		return w.fail("jellyfin", rasaerr.JellyfinAuthRejected("http://"+addr, err))
	}

	w.mu.Lock()
	w.jf = jf
	w.mu.Unlock()
	// Already in hand from the validation call above.
	w.storeJellyfinBase(cfg)
	w.log.WithPhase("jellyfin").OK("Jellyfin API key accepted.")
	w.update(func(m *Model) {
		m.Jellyfin.SignedIn = true
		m.Screen = ScreenDynu
		if m.DynuKey {
			m.Screen = ScreenName
		}
	})
	return nil
}

// ---------------------------------------------------------------------------
// Journey step 7: the Dynu account.

// SetDynuKey validates and stores the API key the user pasted.
//
// Validation is a live call rather than a format check. A key that looks right
// and is not accepted is exactly the failure this screen exists to catch, and
// catching it here rather than at the claim keeps the user next to the page
// they copied it from.
func (w *Wizard) SetDynuKey(ctx context.Context, key string) error {
	if err := w.begin(); err != nil {
		return err
	}
	defer w.end()

	key = strings.TrimSpace(key)
	if key == "" {
		return w.fail("dynu", rasaerr.DynuAuthRejected(errors.New("no key supplied")))
	}
	w.opts.Log.Redactor().RegisterSecret(key)

	client := w.opts.NewDynu(key)
	if _, err := client.ListDomains(ctx); err != nil {
		return w.fail("dynu", rasaerr.DynuAuthRejected(err))
	}

	if w.opts.Secrets != nil {
		if err := w.opts.Secrets.Set(secrets.DynuAPIKey, key); err != nil {
			// The credential is what keeps the address current after RASA is
			// gone (SPEC.md §3). Setup could continue without storing it, but
			// the result would silently stop working at the next address
			// change, so this is fatal.
			return w.fail("dynu", &rasaerr.Error{
				Code:    rasaerr.CodeUnexpected,
				Message: "Your Dynu key couldn't be saved securely.",
				Why:     "Without it, your address would stop updating the next time your connection changes. Running setup as an administrator usually fixes this.",
				Detail:  err.Error(),
				Actions: []rasaerr.Action{{ID: "retry", Label: "Try again", Kind: rasaerr.ActionRetry}},
			})
		}
	}

	w.mu.Lock()
	w.dyn, w.dkey = client, key
	w.mu.Unlock()
	w.log.WithPhase("dynu").OK("Dynu account connected.")
	w.update(func(m *Model) {
		m.DynuKey = true
		m.Screen = ScreenName
	})
	return nil
}

// previousScreen is where "back" goes from here, or "" where it cannot go
// anywhere.
//
// Only the screens before a name is claimed have an answer. Everything from
// the port step onwards is downstream of w.claim, which creates a real
// hostname on the user's Dynu account: there is no undo for that inside RASA,
// and offering one that quietly spent a second of their free hostnames would
// be worse than offering none. The confirmation on the name screen exists
// because this boundary is where reversibility ends.
func previousScreen(current Screen, repair bool) Screen {
	switch current {
	case ScreenJellyfin:
		return ScreenWelcome
	case ScreenDynu:
		return ScreenJellyfin
	case ScreenName:
		// A repair run already has the key and never showed the Dynu screen,
		// so going "back" to it would be going somewhere the user has not been.
		if repair {
			return ScreenJellyfin
		}
		return ScreenDynu
	default:
		return ""
	}
}

// Back returns to the previous screen.
//
// Nothing is undone, because nothing before this boundary needs undoing:
// signing in again replaces the session, and entering a key again replaces the
// key. It is the screen that moves, not the state.
func (w *Wizard) Back(ctx context.Context) error {
	if err := w.begin(); err != nil {
		return err
	}
	defer w.end()

	w.mu.Lock()
	prev := previousScreen(w.model.Screen, w.model.Repair)
	w.mu.Unlock()
	if prev == "" {
		return nil
	}
	w.update(func(m *Model) {
		m.Screen = prev
		// The error belonged to the screen being left. Carrying it back makes
		// the previous step look broken when it is not.
		m.Err = nil
	})
	return nil
}

// minCheckableKey is the length below which a key is not worth testing.
//
// This is a redactor guard, not a validation rule. Constructing a Dynu client
// registers its key as a secret to be scrubbed from logs, so testing a
// half-typed key would register "a" as a secret and redact that letter from
// every log line in the product. A real key is 32 characters; nothing shorter
// than this is a complete one, and the realistic action here is a paste that
// arrives whole.
const minCheckableKey = 24

// CheckDynuKey reports whether a key is accepted, without storing it or
// advancing the wizard.
//
// Deliberately outside the busy lock: this runs while the user is typing, and
// it must never block, fail, or interfere with a real operation in flight.
func (w *Wizard) CheckDynuKey(ctx context.Context, key string) KeyCheckView {
	key = strings.TrimSpace(key)
	if len(key) < minCheckableKey {
		return KeyCheckView{State: "unknown"}
	}

	client := w.opts.NewDynu(key)
	if _, err := client.ListDomains(ctx); err != nil {
		return KeyCheckView{
			State:   "rejected",
			Message: "Dynu didn't accept that key. Check you copied all of it.",
		}
	}
	return KeyCheckView{State: "valid", Message: "That key works."}
}

// ---------------------------------------------------------------------------
// Journey step 8: pick a name.

// CheckName is the debounced advisory lookup behind the name field. It never
// promises availability (SPEC.md §8) and never blocks the user from trying.
func (w *Wizard) CheckName(ctx context.Context, label, parent string) NameView {
	view := NameView{Label: label, Parent: parent}
	if parent == "" {
		view.Parent = w.opts.Catalog.Default().Name
	}

	if err := domains.ValidateLabel(label); err != nil {
		if re, ok := rasaerr.As(err); ok {
			view.Advice = re.Message
		}
		return view
	}
	if !w.opts.Catalog.Allows(view.Parent) {
		if re, ok := rasaerr.As(rasaerr.BlockedParentDomain(view.Parent)); ok {
			view.Advice = re.Message
		}
		return view
	}

	hostname := domains.Hostname(label, view.Parent)
	avail := w.opts.Availability.Check(ctx, hostname)
	view.Availability = avail.String()
	view.Advice = avail.Message()
	if avail == domains.InUse {
		view.Suggestions = w.opts.Catalog.Suggest(label, view.Parent, 4)
	}
	return view
}

// ClaimName creates the hostname on the user's Dynu account.
//
// SPEC.md §9 puts the claim inside the unattended stretch at step 10. Doing it
// here instead is a deliberate departure: the creation call is the only
// authoritative availability answer there is, and a collision discovered two
// minutes into an install is a far worse experience than one discovered while
// the name field still has focus. The pipeline still performs the claim, where
// it finds the work already done — which the idempotency requirement demanded
// anyway.
func (w *Wizard) ClaimName(ctx context.Context, label, parent string) error {
	if err := w.begin(); err != nil {
		return err
	}
	defer w.end()

	if parent == "" {
		parent = w.opts.Catalog.Default().Name
	}
	if err := domains.ValidateLabel(label); err != nil {
		return w.fail("domain", err)
	}
	if !w.opts.Catalog.Allows(parent) {
		return w.fail("domain", rasaerr.BlockedParentDomain(parent))
	}
	hostname := domains.Hostname(label, parent)

	w.mu.Lock()
	dyn, res := w.dyn, w.probed
	w.mu.Unlock()
	if dyn == nil {
		return w.fail("domain", rasaerr.DynuAuthRejected(errors.New("no api key set")))
	}

	w.update(func(m *Model) {
		m.Name.Label, m.Name.Parent = label, parent
		m.Name.Suggestions = nil
	})

	dom, err := w.claim(ctx, dyn, hostname, res)
	if err != nil {
		// Quota is checked before "taken": both refuse the name, but only one
		// of them is fixed by picking a different one, and offering
		// suggestions to somebody whose account is full sends them round the
		// same wall four more times.
		if dynu.IsQuotaExhausted(err) {
			return w.fail("domain", w.quotaExhausted(ctx, dyn, err))
		}
		if isTaken(err) {
			return w.fail("domain", rasaerr.HostnameTaken(hostname, w.suggest(label, parent)))
		}
		return w.fail("domain", err)
	}

	w.opts.Log.Redactor().RegisterAddress(hostname)
	if dom.Token != "" {
		w.opts.Log.Redactor().RegisterSecret(dom.Token)
	}

	w.mu.Lock()
	w.claimed = dom
	w.st.Hostname = hostname
	w.st.ParentDomain = dynu.ParentDomain(hostname)
	w.mu.Unlock()
	w.advance(state.DomainClaimed)

	w.log.WithPhase("domain").OK("Your web address is reserved.")
	// Empty until installProxy reads it from Jellyfin, which is fine: the
	// address shown here is a preview, and verify rewrites it once the real
	// one is known.
	base := w.currentBasePath()
	w.update(func(m *Model) {
		m.Name.Hostname = hostname
		m.Name.Availability = domains.Mine.String()
		m.Name.Advice = ""
		m.Result.URL = publicURL(hostname, m.ListenPort, base)
		m.Screen = ScreenPort
	})

	// Journey step 9 starts on its own. SPEC.md §9: every fork happens
	// invisibly and the user is told the outcome, never asked to begin it —
	// and a screen that waits for a click it never asked for, while its own
	// text says it is working, is the worst of both.
	//
	// openPort rather than OpenPort: the busy flag is already held.
	return w.openPort(ctx)
}

func (w *Wizard) suggest(label, parent string) []string {
	s := w.opts.Catalog.Suggest(label, parent, 4)
	w.update(func(m *Model) { m.Name.Suggestions = s })
	return s
}

// isTaken recognises Dynu's answer to a name somebody else owns.
func isTaken(err error) bool { return dynu.IsNameUnavailable(err) }

// quotaExhausted builds the account-is-full failure, naming the hostnames the
// user already has.
//
// The list is what makes the message actionable. Dynu's free tier allows four
// addresses and says nothing about which ones you are using, so a user who set
// this up months ago — or who has been testing — is told their account is full
// with no idea what is in it. Listing the names also surfaces the shortcut
// most people want: typing a name they already own updates it in place rather
// than needing a slot at all.
//
// The lookup is best effort. It has just failed once, and a second failure
// must not replace a precise message with a vague one.
func (w *Wizard) quotaExhausted(ctx context.Context, dyn DynuAPI, cause error) error {
	var names []string
	if ds, err := dyn.ListDomains(ctx); err == nil {
		for _, d := range ds {
			names = append(names, d.Name)
		}
		sort.Strings(names)
	} else {
		w.log.Warn("could not list existing hostnames for the quota message", slog.Any("err", err))
	}
	return rasaerr.DynuQuotaExhausted(names, cause)
}

// currentBasePath returns Jellyfin's normalised base path as currently known.
func (w *Wizard) currentBasePath() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.st.BasePath()
}

// publicURL renders the address a browser should be pointed at.
//
// It carries the port when it is not the default — the 8443 fallback is
// invisible in the hostname and users will otherwise type the address and get
// nothing — and Jellyfin's base path when it has one, for the same reason: a
// server with a base path answers only under it, so the address without it is
// one nobody can use.
//
// base must already be normalised; use state.BasePath.
func publicURL(hostname string, port int, base string) string {
	if hostname == "" {
		return ""
	}
	if port == 0 || port == 443 {
		return "https://" + hostname + base
	}
	return fmt.Sprintf("https://%s:%d%s", hostname, port, base)
}

// ---------------------------------------------------------------------------
// Journey step 9: open the port.

// OpenPort attempts an automatic mapping and falls back to guided
// instructions. It is safe to call repeatedly: the "Test again" button on the
// manual instructions calls exactly this.
func (w *Wizard) OpenPort(ctx context.Context) error {
	if err := w.begin(); err != nil {
		return err
	}
	defer w.end()

	// Re-probe the network first, because this is the retry.
	//
	// "Test again" used to re-run the mapping against the probe taken minutes
	// earlier at the checking step, so nothing the user changed in between
	// could ever be picked up. Reported from a real run: setup was started
	// with a VPN connected, which made the forwarding address a tunnel
	// address; disconnecting the VPN and pressing Test again kept using the
	// stale one, and there was no way to correct it short of starting over.
	//
	// Turning UPnP on in the router's settings is the same shape of problem,
	// and it is the single most likely thing for a user to have just changed
	// when they press this button.
	w.reprobe(ctx)

	return w.openPort(ctx)
}

// reprobe refreshes the network picture, keeping everything already decided.
//
// Best-effort: a probe that comes back worse than the one held must not lose
// a working Jellyfin connection or an address already claimed, so only the
// network-shaped parts are replaced.
func (w *Wizard) reprobe(ctx context.Context) {
	if w.opts.Probe == nil {
		return
	}
	fresh := w.opts.Probe(ctx)

	w.mu.Lock()
	before := w.probed.Host.LANAddress
	w.probed.Router = fresh.Router
	w.probed.Host = fresh.Host
	w.probed.Ports = fresh.Ports
	w.probed.Internet = fresh.Internet
	after := w.probed.Host.LANAddress
	w.mu.Unlock()

	log := w.log.WithPhase("portmap")
	if before != after {
		log.Info("this computer's address on the network changed since the last check",
			slog.String("was", before.String()), slog.String("now", after.String()))
	}
	if fresh.Router.PortMappingAvailable {
		log.Debug("router reports automatic port mapping is available")
	}
}

func (w *Wizard) openPort(ctx context.Context) error {
	w.mu.Lock()
	res, d := w.probed, w.decision
	w.mu.Unlock()

	if d.Mode == state.ModeMesh || (!d.NeedsPortMapping && !d.NeedsManualForward) {
		w.update(func(m *Model) {
			m.Port = PortView{Needed: false, Open: true}
			m.Screen = ScreenSetup
		})
		w.advance(state.PortsMapped)
		return nil
	}

	log := w.log.WithPhase("portmap")
	var mapped *state.PortMapping

	if d.NeedsPortMapping && res.Router.ControlURL != "" && res.Host.LANAddress.IsValid() {
		mapper := w.opts.NewMapper(res.Router.ControlURL, res.Router.ServiceType)
		out, err := mapper.Add(ctx, portmap.Request{
			ExternalPort:   d.ListenPort,
			InternalPort:   d.ListenPort,
			InternalClient: res.Host.LANAddress,
			Protocol:       portmap.TCP,
		})

		// A conflict means the router is already forwarding that port to
		// something else, which the local port probe cannot see: it binds a
		// socket on this machine, so a port another device on the network
		// already owns looks completely free.
		//
		// Only the router knows, and it has just said so. Taking the fallback
		// port at that point costs one more request and is the difference
		// between finishing and handing the user manual instructions for the
		// very port that was refused.
		var ue *portmap.UPnPError
		if err != nil && errors.As(err, &ue) && ue.IsConflict() && d.ListenPort != mode.PortFallback {
			log.Info("that port is already forwarded to another device; trying the alternative",
				slog.Int("was", d.ListenPort), slog.Int("now", mode.PortFallback))
			alt, altErr := mapper.Add(ctx, portmap.Request{
				ExternalPort:   mode.PortFallback,
				InternalPort:   mode.PortFallback,
				InternalClient: res.Host.LANAddress,
				Protocol:       portmap.TCP,
			})
			if altErr == nil {
				out, err = alt, nil
				d.ListenPort = mode.PortFallback
				w.mu.Lock()
				w.decision.ListenPort = mode.PortFallback
				w.mu.Unlock()
				log.OK("Used the alternative port, because your first choice was already taken.")
			}
		}

		switch {
		case err != nil:
			if errors.As(err, &ue) && ue.IsConflict() {
				log.Warned("Your router is already sending that port to a different device.")
			} else {
				log.Warned("The port could not be opened automatically.")
			}
			log.Debug("mapping failed", slog.Any("err", err))
		default:
			mapped = &state.PortMapping{
				ExternalPort: out.Mapping.ExternalPort,
				InternalPort: out.Mapping.InternalPort,
				Method:       "upnp",
				Permanent:    out.Mapping.Permanent(),
				LeaseSeconds: out.Mapping.LeaseSeconds,
			}
			w.mu.Lock()
			w.st.PortMapping = mapped
			w.mu.Unlock()

			if out.Mapping.Permanent() && out.VerifiedByReadback {
				log.OK("Port opened on your router.")
				w.update(func(m *Model) {
					m.Port = PortView{
						Needed: true, Open: true, Automatic: true, Permanent: true,
						External: out.Mapping.ExternalPort, Internal: out.Mapping.InternalPort,
					}
					m.Screen = ScreenSetup
				})
				w.advance(state.PortsMapped)
				return nil
			}
			log.Warned("The port was opened, but your router may clear it when it restarts.")
		}
	}

	// Everything else ends the same way: show the user how to make it
	// permanent themselves. A temporary mapping is included, because a lease
	// the router will clear on reboot is not an ending.
	w.showGuide(res, d, mapped)
	return nil
}

func (w *Wizard) showGuide(res probe.Result, d mode.Decision, mapped *state.PortMapping) {
	cat, err := routerguide.Embedded()
	if err != nil {
		w.log.Error("router catalogue unavailable", slog.Any("err", err))
		return
	}
	entry := cat.Match(routerguide.Identity{
		Vendor: res.Router.Vendor,
		Model:  res.Router.Model,
		MAC:    res.Router.MAC,
	})
	// The user has looked at the specific guide and said it does not match
	// their router. Their eyes beat a UPnP vendor string.
	w.mu.Lock()
	generic := w.wantGenericGuide
	w.mu.Unlock()
	if generic {
		entry = cat.Generic()
	}
	ins := routerguide.Build(entry, routerguide.Values{
		Gateway:       res.Router.Gateway,
		InternalIP:    res.Host.LANAddress,
		Port:          d.ListenPort,
		AddressIsDHCP: res.Host.AddressIsDHCP,
	})

	w.mu.Lock()
	w.guideText = ins.PlainText()
	w.mu.Unlock()

	w.log.Info("rendered port forwarding guide",
		slog.String("router", ins.RouterName),
		slog.Bool("generic", ins.Generic),
		slog.Bool("reservation_required", ins.ReservationRequired),
	)

	view := PortView{
		Needed:           true,
		RouterName:       ins.RouterName,
		RouterNote:       ins.Note,
		RouterGuessed:    !ins.Generic,
		GenericRequested: generic,
		// Only when the router never offered it. A router that offered it and
		// then refused the mapping is a different problem, and telling that
		// user to go and enable a setting they already have on wastes their
		// time on the screen where they have least patience for it.
		AutomaticOff: mapped == nil && !res.Router.PortMappingAvailable,
	}
	if mapped != nil {
		view.Open = true
		view.Automatic = true
		view.Permanent = mapped.Permanent
		view.External, view.Internal = mapped.ExternalPort, mapped.InternalPort
	}
	for _, s := range ins.Steps {
		view.Instructions = append(view.Instructions, GuideStep{Text: s})
	}
	for _, f := range ins.Fields {
		view.Values = append(view.Values, GuideValue{Label: f.Label, Value: f.Value})
	}
	if ins.ReservationRequired {
		// A static forward aimed at a leased address works for weeks and then
		// stops, which SPEC.md §6 names as the most common cause of remote
		// access dying silently. Saying so here is cheaper than the support
		// thread it prevents.
		view.ReservationNote = "This computer's local address is assigned automatically and will eventually change, which would break the forwarding rule. Reserve it first, under " + ins.ReservationPath + "."
	}
	w.update(func(m *Model) {
		m.Port = view
		m.Screen = ScreenPort
	})
}

// SkipPort accepts the port situation as it stands and moves on.
//
// It exists because refusing to continue would be worse than continuing: a
// user who cannot open a port still gets a working certificate, a working
// address, and a recovery file telling them exactly what remains. Refusing
// would leave them with none of it.
func (w *Wizard) SkipPort(ctx context.Context) error {
	if err := w.begin(); err != nil {
		return err
	}
	defer w.end()

	w.mu.Lock()
	w.st.AddWarning("port_not_confirmed", "Your router port was not confirmed open. If your server can't be reached from outside, the instructions in your recovery file will fix it.")
	w.mu.Unlock()
	w.advance(state.PortsMapped)
	w.update(func(m *Model) {
		m.Warnings = append(m.Warnings, Warning{
			Code: "port_not_confirmed",
			Text: "Your router port was not confirmed open. If your server can't be reached from outside, the instructions in your recovery file will fix it.",
		})
		m.Screen = ScreenSetup
	})
	return nil
}

// UseGenericGuide switches the port screen to the generic instructions.
//
// Matching a router is a guess made from a UPnP vendor string or an admin page
// title, and the catalogue is verified against vendor documentation rather than
// hardware. When the guess is wrong the specific guide is worse than useless:
// it sends the user hunting for a menu their model does not have. This is the
// way back, and it is one-way on purpose — someone who has said "that is not my
// router" should not have it argued with on the next render.
func (w *Wizard) UseGenericGuide(ctx context.Context) error {
	if err := w.begin(); err != nil {
		return err
	}
	defer w.end()

	w.mu.Lock()
	w.wantGenericGuide = true
	res, d := w.probed, w.decision
	w.mu.Unlock()

	w.log.WithPhase("portmap").Info("user asked for the generic port forwarding guide")
	w.showGuide(res, d, w.currentMapping())
	return nil
}

func (w *Wizard) currentMapping() *state.PortMapping {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.st.PortMapping
}
