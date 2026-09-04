package jellyfin

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// Configuration keys RASA touches. They are named constants because a typo in
// a map key is silent — the write would succeed and change nothing.
const (
	KeyKnownProxies       = "KnownProxies"
	KeyEnableRemoteAccess = "EnableRemoteAccess"
	KeyPublishedServerURI = "PublishedServerUriBySubnet"
	KeyEnableUPnP         = "EnableUPnP"
	KeyBaseURL            = "BaseUrl"
	KeyRequireHTTPS       = "RequireHttps"

	// Jellyfin's own allow/deny list for remote clients. RASA reads it and
	// never writes it: it is a security decision the user made deliberately,
	// and quietly widening it on their behalf would be the wrong kind of
	// helpful. Reporting it matters though, because a filter that excludes
	// the internet blocks every remote client while every other part of
	// setup succeeds.
	KeyRemoteIPFilter     = "RemoteIPFilter"
	KeyRemoteFilterIsDeny = "IsRemoteIPFilterBlacklist"
)

// Settings are what RASA needs Jellyfin to believe.
type Settings struct {
	// PublicURL is the address clients will connect to, e.g.
	// "https://mymedia.freeddns.org" or with a port suffix.
	PublicURL string
	// ProxySources are the addresses Jellyfin will see the proxy arriving
	// from. Loopback for a native install; the bridge gateway for a
	// containerised one (SPEC.md §13).
	ProxySources []string
}

// Change records one modification, for the log and the summary.
type Change struct {
	Key  string
	From string
	To   string
}

// Result describes what Apply did.
type Result struct {
	Changes []Change
	// BaseURL is Jellyfin's configured base path, read and never modified.
	// The generated Caddyfile must match it.
	BaseURL string
	// Warnings are things RASA found and deliberately did not change, but
	// which will stop remote access working.
	Warnings []string
	// RestartRequired is true when a changed key needs a restart to take
	// effect.
	RestartRequired bool
}

// Changed reports whether anything was modified.
func (r Result) Changed() bool { return len(r.Changes) > 0 }

// Apply reads the network configuration, changes only what RASA requires, and
// writes it back.
//
// Idempotent by construction: a second run finds every value already correct
// and writes nothing, which is what SPEC.md §10 requires of every step so a
// resumed setup can replay cleanly.
func (c *Client) Apply(ctx context.Context, s Settings) (*Result, error) {
	cfg, err := c.NetworkConfig(ctx)
	if err != nil {
		return nil, err
	}

	res := &Result{BaseURL: asString(cfg[KeyBaseURL])}

	// Known proxies are merged rather than replaced. A user may have their own
	// entries — another reverse proxy, a VPN gateway — and silently dropping
	// them would break something that was working.
	wantProxies := s.ProxySources
	if len(wantProxies) == 0 {
		wantProxies = []string{"127.0.0.1", "::1"}
	}
	existing := asStringSlice(cfg[KeyKnownProxies])
	merged := mergeUnique(existing, wantProxies)
	if !sameSet(existing, merged) {
		res.Changes = append(res.Changes, Change{
			Key: KeyKnownProxies, From: join(existing), To: join(merged),
		})
		cfg[KeyKnownProxies] = merged
	}

	// Remote access must be on, or nothing works regardless of the proxy.
	if !asBool(cfg[KeyEnableRemoteAccess]) {
		res.Changes = append(res.Changes, Change{Key: KeyEnableRemoteAccess, From: "false", To: "true"})
		cfg[KeyEnableRemoteAccess] = true
		res.RestartRequired = true
	}

	// Without this Jellyfin hands clients its internal address, and playback
	// breaks off-network in a way that looks like a proxy fault.
	if s.PublicURL != "" {
		want := "all=" + s.PublicURL
		published := asStringSlice(cfg[KeyPublishedServerURI])
		if !containsPrefix(published, "all=") || !contains(published, want) {
			updated := replacePrefix(published, "all=", want)
			res.Changes = append(res.Changes, Change{
				Key: KeyPublishedServerURI, From: join(published), To: join(updated),
			})
			cfg[KeyPublishedServerURI] = updated
		}
	}

	// Jellyfin's own port mapping would fight the one RASA manages.
	if asBool(cfg[KeyEnableUPnP]) {
		res.Changes = append(res.Changes, Change{Key: KeyEnableUPnP, From: "true", To: "false"})
		cfg[KeyEnableUPnP] = false
	}

	// TLS terminates at the proxy. Leaving Jellyfin's own HTTPS on alongside
	// it produces a redirect loop.
	//
	// Only RequireHttps is touched, deliberately. Jellyfin's other TLS key,
	// EnableHttps, merely opens its own HTTPS listener on a separate port;
	// RASA proxies to the HTTP port and never reaches it, so turning it off
	// would change a user's setting to no effect.
	if asBool(cfg[KeyRequireHTTPS]) {
		res.Changes = append(res.Changes, Change{Key: KeyRequireHTTPS, From: "true", To: "false"})
		cfg[KeyRequireHTTPS] = false
		res.RestartRequired = true
	}

	// Read, never written. An allow-list that does not include the internet
	// blocks every remote client, and it does it after RASA has reported
	// success on every other step: the address resolves, the certificate is
	// valid, the proxy answers, and Jellyfin refuses the request. Nothing
	// else in the product would explain that.
	if filter := asStringSlice(cfg[KeyRemoteIPFilter]); len(filter) > 0 {
		if asBool(cfg[KeyRemoteFilterIsDeny]) {
			res.Warnings = append(res.Warnings,
				"Jellyfin has a list of blocked addresses ("+join(filter)+"). "+
					"Anyone on those addresses will not be able to reach your server.")
		} else {
			res.Warnings = append(res.Warnings,
				"Jellyfin is set to allow only these addresses ("+join(filter)+"), "+
					"which will block everyone else including you when you are away from home. "+
					"RASA has left it alone because it is your security setting. "+
					"Clear it in Jellyfin under Networking if you want remote access to work.")
		}
	}

	if !res.Changed() {
		c.log.Info("jellyfin configuration already correct")
		return res, nil
	}

	for _, ch := range res.Changes {
		c.log.Info("jellyfin setting changed",
			slog.String("key", ch.Key),
			slog.String("from", ch.From),
			slog.String("to", ch.To),
		)
	}
	if err := c.SetNetworkConfig(ctx, cfg); err != nil {
		return nil, err
	}

	// Read back. A write that silently changes nothing is a real failure mode
	// when the key names have moved between versions, and it would otherwise
	// only surface as remote access mysteriously not working.
	if err := c.verify(ctx, s); err != nil {
		return res, err
	}
	return res, nil
}

// verify re-reads the configuration and confirms the settings took.
func (c *Client) verify(ctx context.Context, s Settings) error {
	cfg, err := c.NetworkConfig(ctx)
	if err != nil {
		return fmt.Errorf("could not re-read configuration to confirm it applied: %w", err)
	}
	if !asBool(cfg[KeyEnableRemoteAccess]) {
		return fmt.Errorf("remote access did not stay enabled — this Jellyfin may use a different setting name")
	}
	want := s.ProxySources
	if len(want) == 0 {
		want = []string{"127.0.0.1"}
	}
	got := asStringSlice(cfg[KeyKnownProxies])
	if !contains(got, want[0]) {
		return fmt.Errorf("known proxies did not include %s after writing — this Jellyfin may use a different setting name", want[0])
	}
	c.log.Info("jellyfin configuration verified")
	return nil
}

// ---- tolerant readers ----
//
// JSON numbers decode as float64 and absent keys as nil, so every accessor
// below tolerates the shapes a real server can produce rather than asserting a
// type and panicking on someone else's configuration.

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(t, "true")
	}
	return false
}

func asStringSlice(v any) []string {
	switch t := v.(type) {
	case []string:
		return append([]string{}, t...)
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func mergeUnique(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range append(append([]string{}, a...), b...) {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string{}, a...), append([]string{}, b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func containsPrefix(list []string, prefix string) bool {
	for _, v := range list {
		if strings.HasPrefix(v, prefix) {
			return true
		}
	}
	return false
}

// replacePrefix swaps the entry starting with prefix, preserving any others.
func replacePrefix(list []string, prefix, replacement string) []string {
	out := make([]string, 0, len(list)+1)
	replaced := false
	for _, v := range list {
		if strings.HasPrefix(v, prefix) {
			if !replaced {
				out = append(out, replacement)
				replaced = true
			}
			continue
		}
		out = append(out, v)
	}
	if !replaced {
		out = append(out, replacement)
	}
	return out
}

func join(v []string) string {
	if len(v) == 0 {
		return "(none)"
	}
	return strings.Join(v, ", ")
}
