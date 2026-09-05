package probe

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// Reading the router's own web server to find out what it is.
//
// This is the second of the three identification tiers in SPEC.md §6 — the one
// the catalogue describes as "used when UPnP is off". It was in routers.json
// from the start and nothing ever filled it in: probe.Router carried no Banner
// field, so Match was only ever given a vendor string and a MAC.
//
// That left the tier dead in exactly the case it was written for. A router with
// UPnP switched off reports no vendor at all, so identification fell straight
// through to the MAC, and the OUI lists are sparse on purpose — which meant
// almost every user who most needed router-specific instructions got the
// generic guide instead.

// bannerBody bounds the read. A login page is a few kilobytes; anything much
// larger is not a page whose title is going to identify a router.
const bannerBody = 128 << 10

// DefaultBannerTimeout is the whole tier's time, both schemes included. It
// runs only after UPnP has already failed, behind the "asking your router what
// it can do" step, so it is deliberately far shorter than the probe's other
// timeouts.
const DefaultBannerTimeout = 4 * time.Second

var titlePattern = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// readBanner asks the gateway's web server who it is, and returns a string to
// match the catalogue against. Empty when the gateway serves nothing useful.
//
// Best effort throughout: a gateway with no admin page, one that refuses
// unauthenticated requests, and one that is not listening on either port are
// all normal and all mean "no banner", never an error.
func readBanner(ctx context.Context, gw netip.Addr, budget time.Duration) string {
	if !gw.IsValid() {
		return ""
	}
	host := gw.String()
	return raceBanners(ctx, bannerClient(), []string{
		"http://" + host + "/",
		"https://" + host + "/",
	}, budget)
}

func bannerClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			// Router admin certificates are self-signed without exception, so
			// verification would reject every https gateway there is. Nothing
			// is sent (no credentials, no query) and nothing is trusted: the
			// response is used only as a substring to match a menu path
			// against. The worst an attacker on the LAN path can achieve is
			// wrong instructions on a screen that already offers "not my
			// router".
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
}

// raceBanners asks every address at once and takes the first real answer.
//
// Raced rather than tried in turn, because sequentially the first address can
// spend the entire budget and leave the second none. That is not hypothetical:
// the first network this ran against had a gateway that answered http in 3.8
// seconds, which produced a banner when asked on its own and nothing at all
// from inside a probe. Every address here is the same device, so whichever
// names it first is the answer.
func raceBanners(ctx context.Context, client *http.Client, urls []string, budget time.Duration) string {
	if budget <= 0 {
		budget = DefaultBannerTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	// Buffered to the full width: an attempt that finishes after an earlier
	// one has already won must not block on a channel nobody is reading.
	found := make(chan string, len(urls))
	for _, u := range urls {
		go func(u string) { found <- fetchBanner(ctx, client, u) }(u)
	}
	for range urls {
		select {
		case b := <-found:
			if b != "" {
				return b
			}
		case <-ctx.Done():
			return ""
		}
	}
	return ""
}

func fetchBanner(ctx context.Context, client *http.Client, url string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ""
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	// Deliberately not checking the status. A 401 with a realm of "FRITZ!Box"
	// and a 403 login page both identify the router perfectly well, and a
	// gateway that answers at all has told us something.
	var parts []string
	if s := resp.Header.Get("Server"); s != "" {
		parts = append(parts, s)
	}
	// Basic-auth realms are frequently the model name and nothing else.
	if a := resp.Header.Get("WWW-Authenticate"); a != "" {
		parts = append(parts, a)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, bannerBody))
	if err == nil {
		if m := titlePattern.FindSubmatch(body); m != nil {
			parts = append(parts, string(m[1]))
		}
	}

	return cleanBanner(strings.Join(parts, " "))
}

// cleanBanner makes a banner safe to log and to show.
//
// It reaches the log file so that "my router was not recognised" bug reports
// carry the one string a catalogue entry needs, and log files are meant to be
// attachable to a public issue — so control characters and unbounded length
// are not acceptable in it.
func cleanBanner(s string) string {
	var b strings.Builder
	space := true // leading whitespace is dropped
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !space {
				b.WriteByte(' ')
				space = true
			}
			continue
		}
		if !unicode.IsPrint(r) {
			continue
		}
		b.WriteRune(r)
		space = false
	}
	out := strings.TrimSpace(b.String())

	// A generous cap on a field that only ever holds a product name. Cut on a
	// rune boundary, since a router in a non-English locale is the normal case
	// for half the catalogue.
	const max = 200
	if len([]rune(out)) > max {
		out = strings.TrimSpace(string([]rune(out)[:max]))
	}
	return out
}
