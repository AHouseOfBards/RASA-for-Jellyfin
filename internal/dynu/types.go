package dynu

import (
	"strings"
	"time"
)

// The field names below are not guesses. They were read from live API
// responses on 2026-08-24 against a real account, because Dynu publishes no
// reachable OpenAPI document — every documented location returns 404, and
// SPEC.md §12 flagged these names as unverified. Response shapes captured:
//
//	GET /v2/dns                    -> {statusCode, domains: [Domain]}
//	GET /v2/dns/{id}               -> {statusCode, ...Domain}   (flattened)
//	GET /v2/dns/{id}/record        -> {statusCode, dnsRecords: [Record]}
//	GET /v2/dns/getroot/{hostname} -> {statusCode, id, domainName, hostname, node}

// Domain is a DDNS hostname owned by the account.
//
// Dynu calls these "domains" even though they are usually a hostname under a
// shared parent such as freeddns.org.
type Domain struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	UnicodeName string `json:"unicodeName,omitempty"`

	// Token is a per-hostname secret used by the legacy update protocol.
	// It is registered for redaction as soon as it is seen; it must never
	// reach a log line or a diagnostic bundle.
	Token string `json:"token,omitempty"`

	State string `json:"state,omitempty"`
	Group string `json:"group,omitempty"`

	// IPv4Address and IPv6Address are the published addresses. Either may be
	// empty; IPv6Address is null on accounts that have never set one.
	IPv4Address string `json:"ipv4Address,omitempty"`
	IPv6Address string `json:"ipv6Address,omitempty"`

	TTL int `json:"ttl,omitempty"`

	// IPv4 and IPv6 control whether each address family is published at all.
	// Setting IPv6Address without IPv6=true does nothing, which is a quiet
	// way to lose Mode A6.
	IPv4 bool `json:"ipv4"`
	IPv6 bool `json:"ipv6"`

	IPv4WildcardAlias bool `json:"ipv4WildcardAlias"`
	IPv6WildcardAlias bool `json:"ipv6WildcardAlias"`

	CreatedOn string `json:"createdOn,omitempty"`
	UpdatedOn string `json:"updatedOn,omitempty"`
}

// Record is a DNS record within a domain.
type Record struct {
	ID         int64  `json:"id"`
	DomainID   int64  `json:"domainId"`
	DomainName string `json:"domainName,omitempty"`
	NodeName   string `json:"nodeName"`
	Hostname   string `json:"hostname,omitempty"`
	RecordType string `json:"recordType"`
	TTL        int    `json:"ttl,omitempty"`

	// State is whether the record is enabled. Note it is a bool here but a
	// string on Domain — the API is not consistent about this.
	State bool `json:"state"`

	// Content holds the record value for the simple types RASA uses (TXT, A,
	// AAAA). SOA records use the dedicated fields below instead.
	Content   string `json:"content,omitempty"`
	UpdatedOn string `json:"updatedOn,omitempty"`

	MasterName      string `json:"masterName,omitempty"`
	ResponsibleName string `json:"responsibleName,omitempty"`
	Refresh         int    `json:"refresh,omitempty"`
	Retry           int    `json:"retry,omitempty"`
	Expire          int    `json:"expire,omitempty"`
	NegativeTTL     int    `json:"negativeTTL,omitempty"`
}

// Root identifies the zone a hostname belongs to, and the node within it.
//
// Beware the obvious reading. For a DDNS hostname such as
// "mymedia.freeddns.org", Dynu does NOT return DomainName "freeddns.org" with
// Node "mymedia". It returns:
//
//	DomainName: "mymedia.freeddns.org"   (the whole hostname)
//	Node:       ""                       (empty — the record sits at the apex)
//
// In Dynu's model the user's DDNS hostname *is* their zone; the shared parent
// is Dynu's, not theirs. Verified against a live account on 2026-08-24.
//
// Two consequences:
//
//   - TXT records for DNS-01 use NodeName "_acme-challenge" relative to this
//     zone, not "_acme-challenge.mymedia".
//   - DomainName must NOT be used for the PSL allowlist check in SPEC.md §8.
//     It returns the full hostname, so the check would compare the wrong
//     string. Use ParentDomain instead, which derives the parent from the name.
//
// This is the same ambiguity that caddy-dns/dynu exposes as its own_domain
// option, and it will need setting when the Caddyfile is generated (task 6).
type Root struct {
	ID         int64  `json:"id"`
	DomainName string `json:"domainName"`
	Hostname   string `json:"hostname"`
	Node       string `json:"node"`
}

// IsApex reports whether the hostname is the zone apex, which is the normal
// case for a DDNS hostname.
func (r Root) IsApex() bool { return r.Node == "" }

// ParentDomain returns the shared parent of a hostname — "freeddns.org" for
// "mymedia.freeddns.org".
//
// This deliberately derives the parent from the name rather than asking the
// API, because GetRoot reports the whole hostname as the domain. The PSL
// allowlist in SPEC.md §8 must be checked against this, since that is what
// determines which Let's Encrypt rate-limit bucket the certificate falls into.
func ParentDomain(hostname string) string {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(hostname)), ".")
	i := strings.Index(h, ".")
	if i < 0 || i == len(h)-1 {
		return ""
	}
	return h[i+1:]
}

// NodeOf returns the leftmost label of a hostname — "mymedia" for
// "mymedia.freeddns.org".
func NodeOf(hostname string) string {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(hostname)), ".")
	if i := strings.Index(h, "."); i > 0 {
		return h[:i]
	}
	return h
}

// Record types RASA uses.
const (
	RecordA    = "A"
	RecordAAAA = "AAAA"
	RecordTXT  = "TXT"
)

// DefaultTTL is deliberately short. A DDNS hostname exists to track a changing
// address, and a long TTL means clients keep resolving to a dead one after the
// WAN address moves.
const DefaultTTL = 60

// envelope is the common response wrapper. Dynu returns HTTP 200 with an error
// carried in statusCode for some failures, so both must be checked.
type envelope struct {
	StatusCode int    `json:"statusCode"`
	Type       string `json:"type,omitempty"`
	Message    string `json:"message,omitempty"`
}

type domainsResponse struct {
	envelope
	Domains []Domain `json:"domains"`
}

type domainResponse struct {
	envelope
	Domain
}

type recordsResponse struct {
	envelope
	DNSRecords []Record `json:"dnsRecords"`
}

type rootResponse struct {
	envelope
	Root
}

type recordResponse struct {
	envelope
	Record
}

// CreateDomainRequest creates or updates a hostname.
//
// Only Name is required; the rest are applied when set. The IPv4/IPv6 booleans
// must be set explicitly to publish an address, so callers should use the
// helper constructors rather than building this by hand.
type CreateDomainRequest struct {
	Name        string `json:"name"`
	IPv4Address string `json:"ipv4Address,omitempty"`
	IPv6Address string `json:"ipv6Address,omitempty"`
	TTL         int    `json:"ttl,omitempty"`
	IPv4        bool   `json:"ipv4"`
	IPv6        bool   `json:"ipv6"`
}

// RecordRequest adds a DNS record.
type RecordRequest struct {
	NodeName   string `json:"nodeName"`
	RecordType string `json:"recordType"`
	TTL        int    `json:"ttl,omitempty"`
	State      bool   `json:"state"`
	Content    string `json:"content,omitempty"`

	// TextData carries TXT payloads. Dynu accepts textData for TXT records
	// alongside the generic content field; both are sent so the record works
	// regardless of which the endpoint honours.
	TextData string `json:"textData,omitempty"`
}

// Age reports how long ago the domain was updated, or zero if unknown.
func (d Domain) Age() time.Duration {
	t, err := time.Parse(time.RFC3339, d.UpdatedOn)
	if err != nil {
		return 0
	}
	return time.Since(t)
}
