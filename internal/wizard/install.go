package wizard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/caddy"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/dnswait"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/dynu"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/jellyfin"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/probe"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/qr"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/rasaerr"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/reach"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/recovery"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/service"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/state"
)

// TokenEnvVar is where both the proxy and the sync task read the Dynu
// credential. It matches what cmd/rasa-sync expects; the value never appears
// in a config file, a command line, or a log (SPEC.md §14).
const TokenEnvVar = "RASA_DYNU_TOKEN"

// Install runs journey step 10 — the long unattended stretch — and then
// verifies.
//
// Every step is written to be safe to re-run, because this is where
// interrupted installs happen (SPEC.md §9). A user who force-quits during the
// DNS wait and relaunches must land here again and pass straight through the
// parts that already succeeded.
func (w *Wizard) Install(ctx context.Context) error {
	if err := w.begin(); err != nil {
		return err
	}
	defer w.end()

	w.update(func(m *Model) {
		m.Screen = ScreenSetup
		m.Setup = initialSetup()
	})

	for _, phase := range []func(context.Context) error{
		w.publishAddress,
		w.waitForDNS,
		w.installProxy,
		w.waitForCertificate,
		w.configureJellyfin,
		w.installSync,
		w.verify,
	} {
		if err := phase(ctx); err != nil {
			w.writeRecovery()
			return err
		}
	}

	w.writeRecovery()
	w.update(func(m *Model) { m.Screen = ScreenDone })
	w.setQR()
	w.log.OK("Remote access is set up.")
	return nil
}

// setQR renders the finished address as a QR code for the handover screen.
//
// Failure is not an error: the address is on screen and copyable either way.
// The QR exists because the phone-on-mobile-data test is the only check a user
// can run that actually proves the setup works from outside their own network,
// and it is exactly the check they skip when it means typing a URL with a port
// number into a phone keyboard.
func (w *Wizard) setQR() {
	w.mu.Lock()
	url := w.st.URL()
	w.mu.Unlock()
	if url == "" {
		return
	}
	uri, err := qr.DataURI(url, 0)
	if err != nil {
		w.log.Warn("could not render the address as a QR code", slog.Any("err", err))
		return
	}
	w.update(func(m *Model) { m.Result.QRPNG = uri })
}

// ---------------------------------------------------------------------------

// claim creates or updates the hostname on the user's account.
//
// CreateDomain already handles the owned-name case by updating in place, so
// this is safe to call on a hostname the user claimed on a previous run — the
// idempotency SPEC.md §10 requires falls out of the client rather than being
// re-implemented here.
func (w *Wizard) claim(ctx context.Context, dyn DynuAPI, hostname string, res probe.Result) (*dynu.Domain, error) {
	req := dynu.CreateDomainRequest{Name: hostname, TTL: dynu.DefaultTTL}
	if res.Internet.HasV4() {
		req.IPv4, req.IPv4Address = true, res.Internet.PublicV4.String()
	}
	if res.Internet.HasV6() {
		// SPEC.md §12: setting an address without the matching family flag
		// does nothing, which is a quiet way to lose Mode A6 entirely.
		req.IPv6, req.IPv6Address = true, res.Internet.PublicV6.String()
	}
	return dyn.CreateDomain(ctx, req)
}

// publishAddress makes sure the hostname exists and points here.
func (w *Wizard) publishAddress(ctx context.Context) error {
	w.step(SetupAddress, StepRunning, "")

	w.mu.Lock()
	dyn, res, hostname := w.dyn, w.probed, w.st.Hostname
	w.mu.Unlock()

	if dyn == nil || hostname == "" {
		return w.fail("domain", errors.New("no hostname was chosen"))
	}

	dom, err := w.claim(ctx, dyn, hostname, res)
	if err != nil {
		w.step(SetupAddress, StepFailed, "")
		if isTaken(err) {
			w.update(func(m *Model) { m.Screen = ScreenName })
			label, parent, _ := w.opts.Catalog.Split(hostname)
			return w.fail("domain", rasaerr.HostnameTaken(hostname, w.suggest(label, parent)))
		}
		return w.fail("domain", err)
	}
	if dom.Token != "" {
		w.opts.Log.Redactor().RegisterSecret(dom.Token)
	}

	w.mu.Lock()
	w.claimed = dom
	w.st.ParentDomain = dynu.ParentDomain(hostname)
	w.mu.Unlock()
	w.advance(state.DomainClaimed)

	note := "Published as " + hostname
	if res.Internet.HasV6() && res.Internet.HasV4() {
		note += " on IPv4 and IPv6"
	}
	w.step(SetupAddress, StepDone, note)
	return nil
}

// waitForDNS blocks until every authoritative nameserver agrees.
//
// This gates ACME rather than merely preceding it. Let's Encrypt allows five
// failed validations per hostname per hour, and a record that is visible on
// one nameserver but not another is exactly the state that burns them
// (SPEC.md §10) — it fails, gets retried, and the quota is gone before anyone
// realises the problem was timing.
func (w *Wizard) waitForDNS(ctx context.Context) error {
	w.mu.Lock()
	hostname, res := w.st.Hostname, w.probed
	w.mu.Unlock()

	if !res.Internet.HasV4() {
		// An IPv6-only connection publishes an AAAA record instead. Waiting
		// for an A record that will never exist would time out and fail a
		// setup that is working.
		w.step(SetupDNS, StepSkipped, "This connection uses IPv6 only")
		w.advance(state.DNSVisible)
		return nil
	}

	w.step(SetupDNS, StepRunning, "")
	started := w.opts.Now()
	err := w.opts.DNSWait.WaitForA(ctx, hostname, res.Internet.PublicV4, func(p dnswait.Progress) {
		w.step(SetupDNS, StepRunning, fmt.Sprintf("%d of %d name servers ready", p.ServersOK, p.Servers))
	})
	if err != nil {
		w.step(SetupDNS, StepFailed, "")
		return w.fail("dns", rasaerr.DNSNotVisible(hostname, w.opts.Now().Sub(started).Round(time.Second), err))
	}

	w.step(SetupDNS, StepDone, "Your address is visible worldwide")
	w.advance(state.DNSVisible)
	return nil
}

// installProxy writes the Caddyfile and registers the service.
func (w *Wizard) installProxy(ctx context.Context) error {
	w.step(SetupProxy, StepRunning, "")

	w.mu.Lock()
	hostname, port, res, key := w.st.Hostname, w.st.ListenPort, w.probed, w.dkey
	w.mu.Unlock()

	proxy, err := w.proxyInstaller()
	if err != nil {
		w.step(SetupProxy, StepFailed, "")
		return w.fail("proxy", err)
	}

	cfg := caddy.Config{
		Hostname:        hostname,
		ListenPort:      port,
		UpstreamAddress: res.Jellyfin.Address,
		DynuAPIKeyEnv:   TokenEnvVar,
		// A Dynu DDNS hostname is its own zone: the record sits at the apex,
		// not under freeddns.org (SPEC.md §12). The provider needs that stated
		// or it looks for the record in the wrong place.
		OwnDomain:     hostname,
		LogPath:       w.opts.Layout.CaddyLog(),
		AccessLogPath: w.opts.Layout.CaddyAccessLog(),
		ACMECA:        w.opts.ACMECA,
		Email:         w.opts.Email,
	}
	if err := proxy.Install(ctx, cfg, map[string]string{TokenEnvVar: key}); err != nil {
		w.step(SetupProxy, StepFailed, "")
		return w.fail("proxy", err)
	}

	if v := proxy.Version(ctx); v != "" {
		w.mu.Lock()
		w.st.CaddyVersion = v
		w.mu.Unlock()
	}
	w.step(SetupProxy, StepDone, "The secure connection is running")
	w.save()
	return nil
}

// proxyInstaller builds the real installer unless a test supplied one.
func (w *Wizard) proxyInstaller() (ProxyInstaller, error) {
	if w.opts.Proxy != nil {
		return w.opts.Proxy, nil
	}
	if w.opts.CaddyBinary == "" {
		return nil, fmt.Errorf("no proxy binary is available: %w", caddy.ErrBinaryNotFound)
	}
	mgr, err := w.opts.NewServices()
	if err != nil {
		return nil, err
	}
	in := &caddy.Installer{
		BinaryPath:    w.opts.CaddyBinary,
		CaddyfilePath: w.opts.Layout.CaddyfilePath(),
		DataDir:       w.opts.Layout.CaddyDataDir(),
		Services:      mgr,
		Log:           w.log.WithPhase("proxy"),
	}
	if runtime.GOOS != "windows" {
		// See caddy.Installer.EnvFile: a systemd unit file is world-readable.
		in.EnvFile = w.opts.Layout.EnvFile()
	}
	return in, nil
}

// waitForCertificate blocks until the proxy is serving a publicly trusted
// certificate rather than its own internal one.
func (w *Wizard) waitForCertificate(ctx context.Context) error {
	w.step(SetupCert, StepRunning, "")

	w.mu.Lock()
	hostname, port := w.st.Hostname, w.st.ListenPort
	w.mu.Unlock()

	expiry, err := w.opts.CertWait.Wait(ctx, hostname, port, func(elapsed time.Duration) {
		// Certificate issuance is the longest silent stretch in the product
		// and the one users read as a hang. Counting up is the difference
		// between "working" and "frozen".
		w.step(SetupCert, StepRunning, fmt.Sprintf("Still working (%s)", elapsed.Round(5*time.Second)))
	})
	if err != nil {
		w.step(SetupCert, StepFailed, "")
		if isRateLimited(err) {
			return w.fail("certificate", rasaerr.ACMERateLimited(err))
		}
		return w.fail("certificate", &rasaerr.Error{
			Code:    rasaerr.CodeUnexpected,
			Message: "Your security certificate couldn't be issued.",
			Why:     "Everything else is in place, so this usually succeeds on a second attempt a few minutes later.",
			Partial: "Your address is published and the secure connection is installed. Running setup again will pick up from here.",
			Detail:  err.Error(),
			Actions: []rasaerr.Action{{ID: "retry", Label: "Try again", Kind: rasaerr.ActionRetry}},
		})
	}

	w.mu.Lock()
	w.st.CertExpiry = expiry
	w.mu.Unlock()
	w.step(SetupCert, StepDone, "Secured until "+expiry.Format("2 January 2006"))
	w.advance(state.CertIssued)
	return nil
}

func isRateLimited(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "too many certificates") ||
		strings.Contains(s, "rate limit") ||
		strings.Contains(s, "ratelimited")
}

// configureJellyfin writes the network settings the proxy needs.
func (w *Wizard) configureJellyfin(ctx context.Context) error {
	w.step(SetupJellyfin, StepRunning, "")

	w.mu.Lock()
	jf, res, hostname, port := w.jf, w.probed, w.st.Hostname, w.st.ListenPort
	w.mu.Unlock()

	if jf == nil {
		w.step(SetupJellyfin, StepFailed, "")
		return w.fail("jellyfin", rasaerr.JellyfinNotFound(nil))
	}

	settings := jellyfin.Settings{
		PublicURL:    publicURL(hostname, port),
		ProxySources: []string{res.Jellyfin.ProxySourceAddress},
	}
	out, err := jf.Apply(ctx, settings)
	if err != nil {
		w.step(SetupJellyfin, StepFailed, "")
		return w.fail("jellyfin", err)
	}

	if out.RestartRequired {
		w.step(SetupJellyfin, StepRunning, "Restarting Jellyfin")
		if err := jf.Restart(ctx); err != nil {
			// A restart RASA could not trigger is not a failed setup: the
			// settings are written and will take effect the next time the
			// server starts. Saying so beats stopping.
			w.log.Warned("Jellyfin needs a restart before the new address works.")
			w.addWarning("jellyfin_restart", "Jellyfin needs to be restarted before your new address works.")
		} else if err := jf.WaitUntilReady(ctx, 2*time.Minute); err != nil {
			w.log.Warned("Jellyfin was restarting and did not come back in time.")
			w.addWarning("jellyfin_slow_restart", "Jellyfin was restarted but took longer than expected to come back. Give it a minute before trying your address.")
		}
	}

	note := "Jellyfin knows its new address"
	if !out.Changed() {
		note = "Jellyfin was already configured"
	}
	w.step(SetupJellyfin, StepDone, note)
	w.advance(state.JellyfinConfigured)
	return nil
}

// installSync registers the scheduled task that keeps the address current.
//
// This is the one piece that must outlive RASA (SPEC.md §3). A home connection
// changes address without warning, and nothing else in the finished system
// notices.
func (w *Wizard) installSync(ctx context.Context) error {
	w.step(SetupKeepAlive, StepRunning, "")

	if w.opts.SyncBinary == "" {
		// Nothing to install means the address will stop matching the
		// connection at the next change, silently. That is a warning rather
		// than a failure, but it is the most consequential warning RASA can
		// produce.
		w.step(SetupKeepAlive, StepFailed, "")
		w.addWarning("no_sync_task", "Your address won't update automatically if your internet connection's address changes. Your recovery file explains how to fix this.")
		return nil
	}

	mgr, err := w.opts.NewServices()
	if err != nil {
		w.step(SetupKeepAlive, StepFailed, "")
		return w.fail("keepalive", err)
	}

	w.mu.Lock()
	key := w.dkey
	w.mu.Unlock()

	timer := service.SyncTimerDefinition(w.opts.SyncBinary, map[string]string{TokenEnvVar: key})
	if runtime.GOOS != "windows" {
		if err := caddy.WriteEnvFile(w.opts.Layout.EnvFile(), map[string]string{TokenEnvVar: key}); err != nil {
			w.step(SetupKeepAlive, StepFailed, "")
			return w.fail("keepalive", err)
		}
		timer.Environment = nil
		timer.EnvironmentFile = w.opts.Layout.EnvFile()
	}

	if err := mgr.InstallTimer(ctx, timer); err != nil {
		w.step(SetupKeepAlive, StepFailed, "")
		return w.fail("keepalive", err)
	}
	w.step(SetupKeepAlive, StepDone, "Your address will follow your connection")
	return nil
}

// verify fetches Jellyfin's public info through the public path.
//
// SPEC.md §9 step 11 insists this goes over the internet rather than loopback,
// because loopback proves only that the proxy is running. The three-valued
// answer matters just as much: a router without NAT hairpinning fails a check
// made from inside the house while working perfectly for everyone outside it,
// and reporting that as "broken" would send users to fix something that is not
// wrong.
func (w *Wizard) verify(ctx context.Context) error {
	w.step(SetupVerify, StepRunning, "")

	w.mu.Lock()
	res, hostname, port := w.probed, w.st.Hostname, w.st.ListenPort
	w.mu.Unlock()

	addr := res.Internet.PublicV4
	if !addr.IsValid() {
		addr = res.Internet.PublicV6
	}
	url := publicURL(hostname, port) + "/System/Info/Public"
	out := w.opts.NewReach(addr).CheckURL(ctx, url, "\"Version\"")

	w.update(func(m *Model) {
		m.Result.URL = publicURL(hostname, port)
		m.Result.Reachable = out.Status.String()
		m.Result.ReachMessage = out.UserMessage()
	})
	w.log.WithPhase("verify").Info("external check finished",
		slog.String("status", out.Status.String()),
		slog.String("method", out.Method),
	)

	switch out.Status {
	case reach.Reachable:
		w.step(SetupVerify, StepDone, "Reached your server from outside")
		w.advance(state.Verified)
	case reach.Inconclusive:
		// Setup proceeds. This is the hairpinning case, and it is common
		// enough that treating it as failure would break more setups than it
		// would catch.
		w.step(SetupVerify, StepDone, "Couldn't check from here, which is normal on many routers")
		w.addWarning("unverified", "RASA couldn't test your address from inside your own network, which many routers don't allow. Try it from a phone on mobile data.")
		w.advance(state.Verified)
	default:
		w.step(SetupVerify, StepFailed, "")
		w.addWarning("unreachable", "Your server couldn't be reached from outside. The port forwarding details in your recovery file are the usual fix.")
		w.advance(state.Verified)
	}

	// Verified is the last automatic transition; Running is what the finished
	// system is in once the user leaves.
	w.advance(state.Running)
	return nil
}

func (w *Wizard) addWarning(code, text string) {
	w.mu.Lock()
	w.st.AddWarning(code, text)
	w.mu.Unlock()
	w.update(func(m *Model) {
		for _, existing := range m.Warnings {
			if existing.Code == code {
				return
			}
		}
		m.Warnings = append(m.Warnings, Warning{Code: code, Text: text})
	})
	w.save()
}

// writeRecovery writes the plain-text file the user is left with.
//
// It runs on failure as well as success, because a half-configured machine is
// exactly when it matters most and RASA may not be there to run again
// (SPEC.md §15).
func (w *Wizard) writeRecovery() {
	w.mu.Lock()
	info := recovery.Info{
		State:            w.st,
		Layout:           w.opts.Layout,
		ServiceMechanism: service.Describe(),
		Version:          w.opts.Version,
		ForwardingText:   w.guideText,
	}
	w.mu.Unlock()

	if err := recovery.WriteFile(info); err != nil {
		w.log.Warn("could not write the recovery file", slog.Any("err", err))
		return
	}
	w.update(func(m *Model) { m.Result.RecoveryFile = w.opts.Layout.RecoveryFile() })
}
