// Package dynu is a client for the Dynu v2 REST API.
//
// SPEC.md §12: the legacy /nic/update endpoint uses HTTP Basic auth and cannot
// create a hostname, while the v2 REST API uses an API-Key header and can.
// Since RASA must create hostnames, and Caddy's DNS-01 module uses the same
// key, everything here speaks v2 and the product carries exactly one
// credential.
package dynu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/rasaerr"
)

// BaseURL is the v2 API root.
const BaseURL = "https://api.dynu.com/v2"

// Client talks to the Dynu v2 API.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
	log     *logging.Logger

	maxAttempts int
	backoffBase time.Duration
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL overrides the API root. For tests.
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") } }

// WithHTTPClient supplies the underlying client.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithLogger attaches a logger. Every call is recorded with method, path,
// status, duration and attempt count (SPEC.md §15) — these four endpoints are
// where most failures originate.
func WithLogger(l *logging.Logger) Option { return func(c *Client) { c.log = l } }

// WithRetry sets the retry budget.
func WithRetry(maxAttempts int, base time.Duration) Option {
	return func(c *Client) { c.maxAttempts = maxAttempts; c.backoffBase = base }
}

// New returns a Client.
//
// The API key is registered for redaction immediately, before any request can
// be logged, so it cannot reach a log line even if a later code path formats a
// whole request.
func New(apiKey string, opts ...Option) *Client {
	c := &Client{
		baseURL:     BaseURL,
		apiKey:      apiKey,
		http:        &http.Client{Timeout: 30 * time.Second},
		log:         logging.Discard(),
		maxAttempts: 4,
		backoffBase: 500 * time.Millisecond,
	}
	for _, o := range opts {
		o(c)
	}
	c.log.Redactor().RegisterSecret(apiKey)
	return c
}

// ListDomains returns every hostname on the account.
func (c *Client) ListDomains(ctx context.Context) ([]Domain, error) {
	var out domainsResponse
	if err := c.do(ctx, http.MethodGet, "/dns", nil, &out); err != nil {
		return nil, err
	}
	c.registerTokens(out.Domains)
	return out.Domains, nil
}

// GetDomain returns one hostname by id.
func (c *Client) GetDomain(ctx context.Context, id int64) (*Domain, error) {
	var out domainResponse
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/dns/%d", id), nil, &out); err != nil {
		return nil, err
	}
	c.registerTokens([]Domain{out.Domain})
	return &out.Domain, nil
}

// GetRoot resolves which zone a hostname belongs to.
//
// This is how RASA discovers the parent domain and node for a name the user
// typed, and it is the check that a chosen parent domain actually exists.
func (c *Client) GetRoot(ctx context.Context, hostname string) (*Root, error) {
	var out rootResponse
	p := "/dns/getroot/" + url.PathEscape(hostname)
	if err := c.do(ctx, http.MethodGet, p, nil, &out); err != nil {
		return nil, err
	}
	return &out.Root, nil
}

// CreateDomain claims a hostname, or updates it if the account already owns it.
func (c *Client) CreateDomain(ctx context.Context, req CreateDomainRequest) (*Domain, error) {
	if req.TTL == 0 {
		req.TTL = DefaultTTL
	}
	var out domainResponse
	if err := c.do(ctx, http.MethodPost, "/dns", req, &out); err != nil {
		return nil, err
	}
	c.registerTokens([]Domain{out.Domain})
	return &out.Domain, nil
}

// UpdateAddresses publishes new A and AAAA addresses for a hostname.
//
// Both families are handled independently: an invalid address turns that
// family off rather than clearing the other, so a v4-only connection does not
// silently drop a working AAAA record (SPEC.md §12).
func (c *Client) UpdateAddresses(ctx context.Context, id int64, name string, v4, v6 netip.Addr) (*Domain, error) {
	req := CreateDomainRequest{
		Name: name,
		TTL:  DefaultTTL,
	}
	if v4.IsValid() && v4.Is4() {
		req.IPv4 = true
		req.IPv4Address = v4.String()
	}
	if v6.IsValid() && v6.Is6() {
		req.IPv6 = true
		req.IPv6Address = v6.String()
	}
	if !req.IPv4 && !req.IPv6 {
		return nil, errors.New("no valid address supplied")
	}

	var out domainResponse
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/dns/%d", id), req, &out); err != nil {
		return nil, err
	}
	c.registerTokens([]Domain{out.Domain})
	return &out.Domain, nil
}

// ListRecords returns the records in a domain.
func (c *Client) ListRecords(ctx context.Context, domainID int64) ([]Record, error) {
	var out recordsResponse
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/dns/%d/record", domainID), nil, &out); err != nil {
		return nil, err
	}
	return out.DNSRecords, nil
}

// AddRecord creates a record.
func (c *Client) AddRecord(ctx context.Context, domainID int64, r RecordRequest) (*Record, error) {
	if r.TTL == 0 {
		r.TTL = DefaultTTL
	}
	if r.RecordType == RecordTXT && r.TextData == "" {
		r.TextData = r.Content
	}
	var out recordResponse
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/dns/%d/record", domainID), r, &out); err != nil {
		return nil, err
	}
	return &out.Record, nil
}

// DeleteRecord removes a record. Deleting an absent record is treated as
// success, so cleanup after a failed run is safe to repeat.
func (c *Client) DeleteRecord(ctx context.Context, domainID, recordID int64) error {
	err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/dns/%d/record/%d", domainID, recordID), nil, nil)
	var ae *APIError
	if errors.As(err, &ae) && ae.StatusCode == http.StatusNotFound {
		return nil
	}
	return err
}

// FindDomain returns the account's hostname matching name, or nil.
func (c *Client) FindDomain(ctx context.Context, name string) (*Domain, error) {
	all, err := c.ListDomains(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if strings.EqualFold(all[i].Name, name) {
			return &all[i], nil
		}
	}
	return nil, nil
}

// registerTokens marks per-hostname tokens as secrets.
//
// Each Domain carries a token used by the legacy update protocol. It is a
// credential, it arrives unprompted in ordinary list responses, and without
// this it would be written to the log by any call that dumped a Domain.
func (c *Client) registerTokens(ds []Domain) {
	r := c.log.Redactor()
	for _, d := range ds {
		if d.Token != "" {
			r.RegisterSecret(d.Token)
		}
	}
}

// APIError is a non-success response from Dynu.
type APIError struct {
	StatusCode int
	Type       string
	Message    string
	Method     string
	Path       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("dynu %s %s: %d %s %s", e.Method, e.Path, e.StatusCode, e.Type, e.Message)
}

// retryable reports whether another attempt could plausibly succeed.
func (e *APIError) retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
	}

	var lastErr error
	for attempt := 1; attempt <= c.maxAttempts; attempt++ {
		start := time.Now()
		err := c.attempt(ctx, method, path, payload, out)
		dur := time.Since(start)

		status := 0
		var ae *APIError
		if errors.As(err, &ae) {
			status = ae.StatusCode
		} else if err == nil {
			status = http.StatusOK
		}

		c.log.Debug("dynu request",
			slog.String("method", method),
			slog.String("path", path),
			slog.Int("status", status),
			slog.Duration("duration", dur),
			slog.Int("attempt", attempt),
		)

		if err == nil {
			return nil
		}
		lastErr = err

		// Context cancellation is never worth retrying.
		if ctx.Err() != nil {
			return err
		}
		if errors.As(err, &ae) && !ae.retryable() {
			return c.classify(err)
		}
		if attempt == c.maxAttempts {
			break
		}

		// Exponential backoff with jitter. Never hot-loop against a rate
		// limit — doing so is how an account gets throttled harder.
		wait := c.backoffBase * time.Duration(1<<(attempt-1))
		wait += time.Duration(rand.Int63n(int64(c.backoffBase)))
		c.log.Debug("dynu retry",
			slog.String("path", path),
			slog.Duration("wait", wait),
			slog.Int("next_attempt", attempt+1),
		)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return c.classify(lastErr)
}

func (c *Client) attempt(ctx context.Context, method, path string, payload []byte, out any) error {
	var rdr io.Reader
	if payload != nil {
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("API-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return apiErrorFrom(resp.StatusCode, raw, method, path)
	}

	// Dynu also reports failures inside a 200 body, so the envelope must be
	// checked even on an HTTP success.
	var env envelope
	if len(raw) > 0 && json.Unmarshal(raw, &env) == nil {
		if env.StatusCode != 0 && (env.StatusCode < 200 || env.StatusCode >= 300) {
			return &APIError{
				StatusCode: env.StatusCode,
				Type:       env.Type,
				Message:    env.Message,
				Method:     method,
				Path:       path,
			}
		}
	}

	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decoding %s %s: %w", method, path, err)
	}
	return nil
}

func apiErrorFrom(status int, raw []byte, method, path string) *APIError {
	e := &APIError{StatusCode: status, Method: method, Path: path}
	var env envelope
	if json.Unmarshal(raw, &env) == nil {
		if env.Type != "" {
			e.Type = env.Type
		}
		if env.Message != "" {
			e.Message = env.Message
		}
		if env.StatusCode != 0 {
			e.StatusCode = env.StatusCode
		}
	}
	return e
}

// classify turns a transport or API failure into a typed RASA error carrying
// user-facing copy, so no call site has to invent wording.
func (c *Client) classify(err error) error {
	if err == nil {
		return nil
	}
	var ae *APIError
	if errors.As(err, &ae) {
		switch {
		case ae.StatusCode == http.StatusUnauthorized, ae.StatusCode == http.StatusForbidden:
			return rasaerr.DynuAuthRejected(err)
		}
		return err
	}
	return err
}
