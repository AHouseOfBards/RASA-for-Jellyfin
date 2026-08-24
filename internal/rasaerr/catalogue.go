package rasaerr

import "fmt"

// The catalogue. Every failure RASA can show a user lives here, so the copy is
// reviewable in one place and no call site invents its own wording.
//
// The user-facing text follows the contract in SPEC.md §15: what happened,
// why, what now. Note what is absent from every Message below — error codes,
// HTTP status numbers, API endpoint names, and any suggestion that the user
// did something wrong. Nearly every failure here is the network's fault.

const (
	CodeUnexpected          Code = "unexpected"
	CodeACMERateLimited     Code = "acme_rate_limited"
	CodeDNSNotVisible       Code = "dns_not_visible"
	CodePortMappingConflict Code = "port_mapping_conflict"
	CodeJellyfinAuth        Code = "jellyfin_auth_rejected"
	CodeJellyfinNotFound    Code = "jellyfin_not_found"
	CodeJellyfinTooOld      Code = "jellyfin_too_old"
	CodeCarrierGradeNAT     Code = "carrier_grade_nat"
	CodePortHeldLocally     Code = "port_held_locally"
	CodeHostnameTaken       Code = "hostname_taken"
	CodeBlockedParentDomain Code = "blocked_parent_domain"
	CodeDynuAuth            Code = "dynu_auth_rejected"
	CodeNoRouteToInternet   Code = "no_route_to_internet"
	CodeInvalidHostname     Code = "invalid_hostname"
)

// HostnameProblem is why a name was refused before RASA tried to claim it.
//
// The enum exists so that the copy for each case lives here with the rest of
// the catalogue rather than being assembled at the call site. Validation runs
// on every keystroke, so these strings are read more often than any other
// message in the product.
type HostnameProblem string

const (
	HostnameEmpty          HostnameProblem = "empty"
	HostnameTooShort       HostnameProblem = "too_short"
	HostnameTooLong        HostnameProblem = "too_long"
	HostnameBadCharacters  HostnameProblem = "bad_characters"
	HostnameEdgeHyphen     HostnameProblem = "edge_hyphen"
	HostnameReservedPrefix HostnameProblem = "reserved_prefix"
)

// ACMERateLimited is returned when Let's Encrypt declines to issue because the
// account or hostname has hit a limit. Emphasis on "nothing is broken": users
// read a certificate failure as catastrophic when it is merely a wait.
func ACMERateLimited(cause error) *Error {
	return &Error{
		Code:    CodeACMERateLimited,
		Message: "Let's Encrypt is briefly limiting new certificates for this address.",
		Why:     "This usually means setup was retried a few times. Nothing is broken — the limit clears on its own in about an hour.",
		Actions: []Action{
			{ID: "retry", Label: "Try again later", Kind: ActionRetry},
			{ID: "rename", Label: "Use a different name", Kind: ActionAlternate},
		},
		Detail:  "ACME issuance refused by rate limit",
		wrapped: cause,
	}
}

// DNSNotVisible is returned while a freshly created record has not yet
// propagated. It is a wait, not a failure, and the copy says so.
func DNSNotVisible(hostname string, waited fmt.Stringer, cause error) *Error {
	return &Error{
		Code:    CodeDNSNotVisible,
		Message: "Your new address isn't visible on the internet yet.",
		Why:     "This usually takes one to two minutes after the address is created.",
		Actions: []Action{
			{ID: "wait", Label: "Keep waiting", Kind: ActionRetry},
			{ID: "rename", Label: "Pick a different name", Kind: ActionAlternate},
		},
		Detail:  fmt.Sprintf("authoritative NS did not return record for %s after %s", hostname, waited),
		wrapped: cause,
	}
}

// PortMappingConflict is returned when the router already forwards the port to
// something else. The 8443 fallback makes this recoverable without the user
// touching anything.
func PortMappingConflict(port int, cause error) *Error {
	return &Error{
		Code:    CodePortMappingConflict,
		Message: fmt.Sprintf("Your router is already sending port %d to a different device.", port),
		Actions: []Action{
			{ID: "alt_port", Label: "Use port 8443 instead", Kind: ActionAlternate},
			{ID: "manual", Label: "Show me how to change this", Kind: ActionExternal},
		},
		Detail:  fmt.Sprintf("IGD AddPortMapping conflict on external port %d", port),
		wrapped: cause,
	}
}

// JellyfinAuthRejected disambiguates the two accounts in play — users routinely
// try their Dynu credentials here.
func JellyfinAuthRejected(localURL string, cause error) *Error {
	return &Error{
		Code:    CodeJellyfinAuth,
		Message: "That username or password didn't work.",
		Why:     fmt.Sprintf("This is your Jellyfin login — the same one you use at %s, not your Dynu account.", localURL),
		Actions: []Action{
			{ID: "retry", Label: "Try again", Kind: ActionRetry},
			{ID: "apikey", Label: "Use an API key instead", Kind: ActionAlternate},
		},
		Detail:  "Jellyfin AuthenticateByName rejected credentials",
		wrapped: cause,
	}
}

// JellyfinNotFound is returned when no Jellyfin server answers locally.
func JellyfinNotFound(cause error) *Error {
	return &Error{
		Code:    CodeJellyfinNotFound,
		Message: "RASA couldn't find Jellyfin on this computer.",
		Why:     "Jellyfin needs to be installed and running before remote access can be set up.",
		Actions: []Action{
			{ID: "retry", Label: "Check again", Kind: ActionRetry},
			{ID: "manual_addr", Label: "Jellyfin is on another computer", Kind: ActionAlternate},
		},
		Detail:  "no Jellyfin instance responded on loopback or configured address",
		wrapped: cause,
	}
}

// JellyfinTooOld enforces the 10.11.5 floor from decision 12.
func JellyfinTooOld(found, minimum string) *Error {
	return &Error{
		Code:    CodeJellyfinTooOld,
		Message: fmt.Sprintf("Your Jellyfin is version %s, and RASA needs %s or newer.", found, minimum),
		Why:     "Older versions store their network settings differently, so RASA can't configure them safely.",
		Actions: []Action{
			{ID: "recheck", Label: "I've updated — check again", Kind: ActionRetry},
		},
		Detail: fmt.Sprintf("version %s below floor %s", found, minimum),
	}
}

// CarrierGradeNAT is not really an error — it is a routing decision that the
// user should understand. It names the condition without jargon first, then
// gives it a name they can search for.
func CarrierGradeNAT(wanAddr, observedAddr string) *Error {
	return &Error{
		Code:    CodeCarrierGradeNAT,
		Message: "Your internet provider doesn't give this connection a direct address.",
		Why:     "This is common on mobile and some fibre plans, and it's called CGNAT. Opening a port can't work here, so RASA will set up a private connection instead.",
		Actions: []Action{
			{ID: "continue", Label: "Continue", Kind: ActionAlternate},
		},
		Detail: fmt.Sprintf("router WAN %s != observed public %s", wanAddr, observedAddr),
	}
}

// PortHeldLocally is returned when something on this machine already owns the
// port. Naming the holder turns an opaque failure into an obvious one.
func PortHeldLocally(port int, holder string, cause error) *Error {
	why := "RASA will use port 8443 instead, so your address will end in :8443."
	if holder != "" {
		why = fmt.Sprintf("It looks like %s. %s", holder, why)
	}
	return &Error{
		Code:    CodePortHeldLocally,
		Message: fmt.Sprintf("Something else on this computer is already using port %d.", port),
		Why:     why,
		Actions: []Action{
			{ID: "alt_port", Label: "Continue with 8443", Kind: ActionAlternate},
			{ID: "stop_other", Label: "Let me stop it first", Kind: ActionExternal},
		},
		Detail:  fmt.Sprintf("bind refused on port %d, holder=%q", port, holder),
		wrapped: cause,
	}
}

// HostnameTaken drives the cross-domain suggestion flow from SPEC.md §8 —
// the same name on a different parent domain, never a numeric suffix.
func HostnameTaken(hostname string, suggestions []string) *Error {
	return &Error{
		Code:    CodeHostnameTaken,
		Message: fmt.Sprintf("%s is already taken.", hostname),
		Why:     "Someone else is using that name. The same name is free on other addresses.",
		Actions: []Action{
			{ID: "suggestions", Label: "Choose another", Kind: ActionAlternate},
		},
		Detail: fmt.Sprintf("hostname %s unavailable, %d alternatives offered", hostname, len(suggestions)),
	}
}

// BlockedParentDomain enforces the PSL allowlist from SPEC.md §8. The user
// never sees the words "Public Suffix List" — they see a consequence.
func BlockedParentDomain(domain string) *Error {
	return &Error{
		Code:    CodeBlockedParentDomain,
		Message: fmt.Sprintf("Addresses ending in %s can't be used for a secure connection.", domain),
		Why:     "Certificates for that address are shared with every other user of it, which makes setup fail unpredictably. Pick one of the offered addresses instead.",
		Actions: []Action{
			{ID: "pick_domain", Label: "Choose a different address", Kind: ActionAlternate},
		},
		Detail: fmt.Sprintf("parent domain %s not on PSL allowlist", domain),
	}
}

// DynuAuthRejected covers a mistyped or revoked API key.
func DynuAuthRejected(cause error) *Error {
	return &Error{
		Code:    CodeDynuAuth,
		Message: "That Dynu API key wasn't accepted.",
		Why:     "The key may have been copied incompletely. It's on the API Credentials page of your Dynu account.",
		Actions: []Action{
			{ID: "reopen", Label: "Open Dynu again", Kind: ActionRetry},
		},
		Detail:  "Dynu API rejected the supplied key",
		wrapped: cause,
	}
}

// NoRouteToInternet is the pre-flight failure where nothing else can proceed.
func NoRouteToInternet(cause error) *Error {
	return &Error{
		Code:    CodeNoRouteToInternet,
		Message: "RASA couldn't reach the internet.",
		Why:     "Setup needs a working connection to register your address and get a certificate.",
		Actions: []Action{
			{ID: "retry", Label: "Try again", Kind: ActionRetry},
		},
		Detail:  "public address resolution failed",
		wrapped: cause,
	}
}

// InvalidHostname refuses a name the user is still typing.
//
// Unlike the rest of the catalogue these appear while a field has focus, so
// they are phrased as a rule rather than a failure — "can only contain" rather
// than "could not be used". None of them offer an action: the action is to
// keep typing, and a button saying so would be noise.
// limit carries the length rule that applies to the problem, so the numbers
// stay owned by the validator while the wording stays owned by the catalogue.
func InvalidHostname(label string, p HostnameProblem, limit int) *Error {
	e := &Error{
		Code:   CodeInvalidHostname,
		Detail: fmt.Sprintf("hostname label %q rejected: %s", label, p),
	}
	switch p {
	case HostnameEmpty:
		e.Message = "Give your server a name."
		e.Why = "This becomes the web address you'll type to reach it."
	case HostnameTooShort:
		e.Message = fmt.Sprintf("That name is a little short — use at least %d characters.", limit)
		e.Why = "Very short names on these addresses were claimed years ago, so a longer one is far more likely to be free."
	case HostnameTooLong:
		e.Message = fmt.Sprintf("That name is too long — keep it to %d characters or fewer.", limit)
	case HostnameBadCharacters:
		e.Message = "Use only letters, numbers and hyphens."
		e.Why = "Spaces, dots and other punctuation aren't allowed in a web address."
	case HostnameEdgeHyphen:
		e.Message = "A name can't start or end with a hyphen."
	case HostnameReservedPrefix:
		e.Message = "Names starting with \"xn--\" are reserved."
		e.Why = "That prefix is how web addresses encode non-English characters, so it can't be used directly."
	default:
		e.Message = "That name can't be used."
	}
	return e
}
