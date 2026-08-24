// Package portmap creates and inspects router port mappings over UPnP IGD.
//
// SPEC.md §6: UPnP is the happy path because it costs the user no effort, but
// a mapping is a *lease*, not a stored setting. This package therefore asks
// for a permanent lease, reads back what the router actually granted, and
// reports the difference — because "the router said yes" and "this will
// survive a reboot" are different claims, and only the second one means the
// user can be left alone afterwards.
package portmap

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
)

// Description is what the mapping is labelled with in the router's UI. It is
// deliberately recognisable: a user looking at their router a year from now
// should be able to tell what created this entry.
const Description = "RASA for Jellyfin"

// FallbackLeaseSeconds is requested when a router refuses a permanent lease.
// A week is long enough to be useful and short enough that a stale mapping
// clears itself if RASA is uninstalled without cleanup.
const FallbackLeaseSeconds = 604800

// Protocol values.
const (
	TCP = "TCP"
	UDP = "UDP"
)

// UPnP error codes worth distinguishing. The rest are reported as-is.
const (
	errConflictInMappingEntry   = 718
	errOnlyPermanentLeases      = 725
	errNoSuchEntryInArray       = 714
	errInvalidArgs              = 402
	errActionFailed             = 501
	errSamePortValuesRequired   = 724
	errExternalPortOnlySupports = 716
)

// Mapping is a port mapping as the router reports it.
type Mapping struct {
	ExternalPort   int
	InternalPort   int
	InternalClient string
	Protocol       string
	Description    string
	// LeaseSeconds is 0 for a permanent mapping.
	LeaseSeconds int
	Enabled      bool
}

// Permanent reports whether the mapping survives without renewal.
//
// This is the distinction that decides whether the user can be left alone or
// should be offered a static forward instead.
func (m Mapping) Permanent() bool { return m.LeaseSeconds == 0 }

// Mapper talks to a router's WAN connection service.
type Mapper struct {
	// ControlURL and ServiceType come from the probe, which already discovered
	// them via SSDP.
	ControlURL  string
	ServiceType string
	Timeout     time.Duration
	Log         *logging.Logger

	client *http.Client
}

// New returns a Mapper for a discovered service.
func New(controlURL, serviceType string, log *logging.Logger) *Mapper {
	if log == nil {
		log = logging.Discard()
	}
	return &Mapper{
		ControlURL:  controlURL,
		ServiceType: serviceType,
		Timeout:     10 * time.Second,
		Log:         log,
	}
}

// Available reports whether a mapping can be attempted at all.
func (m *Mapper) Available() bool { return m.ControlURL != "" && m.ServiceType != "" }

// Request describes a mapping to create.
type Request struct {
	ExternalPort int
	InternalPort int
	// InternalClient is the LAN address traffic should be forwarded to.
	InternalClient netip.Addr
	Protocol       string
	Description    string
}

// Result is the outcome of an Add.
type Result struct {
	// Mapping is what the router reports after the request, read back rather
	// than assumed from the request.
	Mapping Mapping
	// PermanentRequested records that a permanent lease was asked for. When
	// Mapping.Permanent() is false despite this, the router downgraded it.
	PermanentRequested bool
	// VerifiedByReadback is false when the router accepted the mapping but
	// would not report it back. The mapping may still work; it simply cannot
	// be confirmed here, and external verification decides.
	VerifiedByReadback bool
}

// Add creates a port mapping.
//
// A permanent lease is requested first. Routers that reject one — some answer
// 402 Invalid Args, others 725 OnlyPermanentLeasesSupported, which confusingly
// means the opposite of what it sounds like — get a second attempt with a
// finite lease rather than being treated as a failure.
func (m *Mapper) Add(ctx context.Context, req Request) (*Result, error) {
	if !m.Available() {
		return nil, errors.New("no port mapping service discovered")
	}
	if req.Protocol == "" {
		req.Protocol = TCP
	}
	if req.InternalPort == 0 {
		req.InternalPort = req.ExternalPort
	}
	if req.Description == "" {
		req.Description = Description
	}
	if !req.InternalClient.IsValid() {
		return nil, errors.New("internal client address is required")
	}

	res := &Result{PermanentRequested: true}

	err := m.add(ctx, req, 0)
	if err != nil {
		var ue *UPnPError
		if errors.As(err, &ue) && ue.Retryable() {
			m.Log.Debug("permanent lease refused, retrying with a finite one",
				slog.Int("code", ue.Code),
				slog.Int("lease", FallbackLeaseSeconds),
			)
			res.PermanentRequested = false
			err = m.add(ctx, req, FallbackLeaseSeconds)
		}
		if err != nil {
			return nil, err
		}
	}

	// Read back rather than trusting the acceptance. Routers do report success
	// and then do nothing, which is the failure this catches.
	got, ok, gerr := m.Get(ctx, req.ExternalPort, req.Protocol)
	if gerr == nil && ok {
		res.Mapping = got
		res.VerifiedByReadback = true
	} else {
		res.Mapping = Mapping{
			ExternalPort:   req.ExternalPort,
			InternalPort:   req.InternalPort,
			InternalClient: req.InternalClient.String(),
			Protocol:       req.Protocol,
			Description:    req.Description,
			Enabled:        true,
		}
		if !res.PermanentRequested {
			res.Mapping.LeaseSeconds = FallbackLeaseSeconds
		}
		m.Log.Debug("mapping accepted but not confirmed by read-back", slog.Any("err", gerr))
	}

	m.Log.Info("port mapping created",
		slog.Int("external_port", res.Mapping.ExternalPort),
		slog.Bool("permanent", res.Mapping.Permanent()),
		slog.Bool("verified", res.VerifiedByReadback),
	)
	return res, nil
}

func (m *Mapper) add(ctx context.Context, req Request, lease int) error {
	args := []arg{
		{"NewRemoteHost", ""},
		{"NewExternalPort", strconv.Itoa(req.ExternalPort)},
		{"NewProtocol", req.Protocol},
		{"NewInternalPort", strconv.Itoa(req.InternalPort)},
		{"NewInternalClient", req.InternalClient.String()},
		{"NewEnabled", "1"},
		{"NewPortMappingDescription", req.Description},
		{"NewLeaseDuration", strconv.Itoa(lease)},
	}
	_, err := m.call(ctx, "AddPortMapping", args)
	return err
}

// getResponse is unmarshalled from the whole SOAP envelope, so each field
// carries its full path. Without the Body>Response prefix the decoder finds
// nothing and silently yields a zero-valued mapping — which reads as "exists,
// permanent, disabled" and is worse than an error.
type getResponse struct {
	InternalPort   string `xml:"Body>GetSpecificPortMappingEntryResponse>NewInternalPort"`
	InternalClient string `xml:"Body>GetSpecificPortMappingEntryResponse>NewInternalClient"`
	Enabled        string `xml:"Body>GetSpecificPortMappingEntryResponse>NewEnabled"`
	Description    string `xml:"Body>GetSpecificPortMappingEntryResponse>NewPortMappingDescription"`
	LeaseDuration  string `xml:"Body>GetSpecificPortMappingEntryResponse>NewLeaseDuration"`
}

// valid reports whether the decode actually found the response element. A
// mapping with no internal client was not parsed, whatever the decoder said.
func (r getResponse) valid() bool { return strings.TrimSpace(r.InternalClient) != "" }

// Get returns the mapping for an external port, if one exists.
//
// The boolean distinguishes "no such mapping" — a normal answer — from an
// error talking to the router.
func (m *Mapper) Get(ctx context.Context, externalPort int, protocol string) (Mapping, bool, error) {
	if !m.Available() {
		return Mapping{}, false, errors.New("no port mapping service discovered")
	}
	if protocol == "" {
		protocol = TCP
	}

	body, err := m.call(ctx, "GetSpecificPortMappingEntry", []arg{
		{"NewRemoteHost", ""},
		{"NewExternalPort", strconv.Itoa(externalPort)},
		{"NewProtocol", protocol},
	})
	if err != nil {
		var ue *UPnPError
		if errors.As(err, &ue) && ue.Code == errNoSuchEntryInArray {
			return Mapping{}, false, nil
		}
		return Mapping{}, false, err
	}

	var r getResponse
	if err := xml.Unmarshal(body, &r); err != nil {
		return Mapping{}, false, err
	}
	if !r.valid() {
		// The router answered, but not with a mapping we could read. Treat it
		// as absent rather than inventing a zero-valued one.
		return Mapping{}, false, nil
	}

	mp := Mapping{
		ExternalPort:   externalPort,
		InternalPort:   atoi(r.InternalPort),
		InternalClient: strings.TrimSpace(r.InternalClient),
		Protocol:       protocol,
		Description:    strings.TrimSpace(r.Description),
		LeaseSeconds:   atoi(r.LeaseDuration),
		Enabled:        r.Enabled == "1" || strings.EqualFold(r.Enabled, "true"),
	}
	return mp, true, nil
}

// Delete removes a mapping. Removing one that does not exist succeeds, so
// cleanup is safe to repeat.
func (m *Mapper) Delete(ctx context.Context, externalPort int, protocol string) error {
	if !m.Available() {
		return errors.New("no port mapping service discovered")
	}
	if protocol == "" {
		protocol = TCP
	}
	_, err := m.call(ctx, "DeletePortMapping", []arg{
		{"NewRemoteHost", ""},
		{"NewExternalPort", strconv.Itoa(externalPort)},
		{"NewProtocol", protocol},
	})
	var ue *UPnPError
	if errors.As(err, &ue) && ue.Code == errNoSuchEntryInArray {
		return nil
	}
	return err
}

// UPnPError is a SOAP fault from the router.
type UPnPError struct {
	Code        int
	Description string
	Action      string
}

func (e *UPnPError) Error() string {
	return fmt.Sprintf("upnp %s: %d %s", e.Action, e.Code, e.Description)
}

// Retryable reports whether the same request might succeed with a finite
// lease instead of a permanent one.
func (e *UPnPError) Retryable() bool {
	return e.Code == errInvalidArgs || e.Code == errActionFailed || e.Code == errOnlyPermanentLeases
}

// IsConflict reports whether the port is already mapped to another device.
// This is the one failure the user can act on, so it is worth naming.
func (e *UPnPError) IsConflict() bool { return e.Code == errConflictInMappingEntry }

type arg struct{ name, value string }

type soapFault struct {
	Code        int    `xml:"Body>Fault>detail>UPnPError>errorCode"`
	Description string `xml:"Body>Fault>detail>UPnPError>errorDescription"`
}

// call performs a SOAP action and returns the response body.
func (m *Mapper) call(ctx context.Context, action string, args []arg) ([]byte, error) {
	if m.Timeout == 0 {
		m.Timeout = 10 * time.Second
	}
	if m.client == nil {
		m.client = &http.Client{Timeout: m.Timeout}
	}
	ctx, cancel := context.WithTimeout(ctx, m.Timeout)
	defer cancel()

	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0"?>`)
	sb.WriteString(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"`)
	sb.WriteString(` s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	fmt.Fprintf(&sb, `<u:%s xmlns:u="%s">`, action, m.ServiceType)
	for _, a := range args {
		fmt.Fprintf(&sb, "<%s>", a.name)
		xml.EscapeText(&sb, []byte(a.value))
		fmt.Fprintf(&sb, "</%s>", a.name)
	}
	fmt.Fprintf(&sb, `</u:%s></s:Body></s:Envelope>`, action)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.ControlURL, strings.NewReader(sb.String()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"`+m.ServiceType+`#`+action+`"`)

	start := time.Now()
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return nil, err
	}

	m.Log.Debug("upnp call",
		slog.String("action", action),
		slog.Int("status", resp.StatusCode),
		slog.Duration("duration", time.Since(start)),
	)

	// A fault arrives with HTTP 500 and the useful detail in the body, so the
	// body is parsed before the status is judged.
	var f soapFault
	if xml.Unmarshal(raw, &f) == nil && f.Code != 0 {
		return nil, &UPnPError{Code: f.Code, Description: f.Description, Action: action}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("upnp %s: status %d", action, resp.StatusCode)
	}
	return raw, nil
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}
