// Package jellyfin configures the media server over its own REST API.
//
// SPEC.md §13 replaced a manual checklist with this. Four settings decide
// whether remote access works, and every one of them fails quietly when wrong:
// a missing KnownProxies entry makes Jellyfin log every user with the proxy's
// address, and a missing published URI makes it hand clients internal
// addresses that only break off-network.
package jellyfin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/rasaerr"
)

// Identity RASA presents to Jellyfin. It appears in the server's device list,
// so it is named for a person reading that list rather than for a machine.
const (
	ClientName    = "RASA for Jellyfin"
	DeviceName    = "Remote Access Setup"
	DeviceID      = "rasa-setup-app"
	ClientVersion = "1.0"
)

// NetworkConfigKey is the named configuration section holding everything RASA
// writes.
const NetworkConfigKey = "network"

// Client talks to a Jellyfin server.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
	log     *logging.Logger
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient supplies the underlying client.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithLogger attaches a logger.
func WithLogger(l *logging.Logger) Option { return func(c *Client) { c.log = l } }

// WithAPIKey authenticates with an existing API key instead of a login.
// Decision 3 offers the user both.
func WithAPIKey(key string) Option {
	return func(c *Client) { c.token = key }
}

// New returns a Client for a server at baseURL ("http://127.0.0.1:8096").
func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 20 * time.Second},
		log:     logging.Discard(),
	}
	if !strings.HasPrefix(c.baseURL, "http://") && !strings.HasPrefix(c.baseURL, "https://") {
		c.baseURL = "http://" + c.baseURL
	}
	for _, o := range opts {
		o(c)
	}
	if c.token != "" {
		c.log.Redactor().RegisterSecret(c.token)
	}
	return c
}

// Authenticated reports whether a token is held.
func (c *Client) Authenticated() bool { return c.token != "" }

// PublicInfo is the unauthenticated server description.
type PublicInfo struct {
	LocalAddress    string `json:"LocalAddress"`
	ServerName      string `json:"ServerName"`
	Version         string `json:"Version"`
	ProductName     string `json:"ProductName"`
	OperatingSystem string `json:"OperatingSystem"`
	ID              string `json:"Id"`
}

// PublicInfo fetches the server description without authenticating.
func (c *Client) PublicInfo(ctx context.Context) (*PublicInfo, error) {
	var out PublicInfo
	if err := c.do(ctx, http.MethodGet, "/System/Info/Public", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type authRequest struct {
	Username string `json:"Username"`
	Pw       string `json:"Pw"`
}

type authResponse struct {
	AccessToken string `json:"AccessToken"`
	ServerID    string `json:"ServerId"`
	User        struct {
		Name   string `json:"Name"`
		ID     string `json:"Id"`
		Policy struct {
			IsAdministrator bool `json:"IsAdministrator"`
		} `json:"Policy"`
	} `json:"User"`
}

// AuthResult describes a successful login.
type AuthResult struct {
	UserName string
	UserID   string
	IsAdmin  bool
}

// AuthenticateByName logs in and stores the resulting token.
//
// Administrator rights are checked here rather than left to fail later: a
// standard account can log in perfectly well and then be refused when the
// configuration write happens, several steps further on, where the cause is
// far less obvious.
func (c *Client) AuthenticateByName(ctx context.Context, username, password string) (*AuthResult, error) {
	// Register the password before the request, so it cannot reach a log line
	// even if the call fails and the body is dumped.
	c.log.Redactor().RegisterSecret(password)

	var out authResponse
	err := c.do(ctx, http.MethodPost, "/Users/AuthenticateByName",
		authRequest{Username: username, Pw: password}, &out)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) && (ae.StatusCode == http.StatusUnauthorized || ae.StatusCode == http.StatusForbidden) {
			return nil, rasaerr.JellyfinAuthRejected(c.baseURL, err)
		}
		return nil, err
	}
	if out.AccessToken == "" {
		return nil, rasaerr.JellyfinAuthRejected(c.baseURL, errors.New("no access token returned"))
	}

	c.token = out.AccessToken
	c.log.Redactor().RegisterSecret(out.AccessToken)

	res := &AuthResult{
		UserName: out.User.Name,
		UserID:   out.User.ID,
		IsAdmin:  out.User.Policy.IsAdministrator,
	}
	c.log.Info("authenticated to jellyfin", slog.Bool("is_admin", res.IsAdmin))
	return res, nil
}

// Config is a Jellyfin configuration section.
//
// It is deliberately a map rather than a typed struct. RASA reads the section,
// changes four fields, and writes it back — and a typed struct would silently
// drop every key it does not know about, discarding the user's other network
// settings and anything a newer Jellyfin added. Round-tripping the raw object
// is the only safe way to edit someone else's configuration.
type Config map[string]any

// NetworkConfig fetches the network configuration section.
func (c *Client) NetworkConfig(ctx context.Context) (Config, error) {
	if !c.Authenticated() {
		return nil, errors.New("not authenticated")
	}
	var out Config
	if err := c.do(ctx, http.MethodGet, "/System/Configuration/"+NetworkConfigKey, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SetNetworkConfig writes the network configuration section back.
func (c *Client) SetNetworkConfig(ctx context.Context, cfg Config) error {
	if !c.Authenticated() {
		return errors.New("not authenticated")
	}
	return c.do(ctx, http.MethodPost, "/System/Configuration/"+NetworkConfigKey, cfg, nil)
}

// Restart asks the server to restart.
func (c *Client) Restart(ctx context.Context) error {
	if !c.Authenticated() {
		return errors.New("not authenticated")
	}
	return c.do(ctx, http.MethodPost, "/System/Restart", nil, nil)
}

// WaitUntilReady polls the public endpoint until the server answers again.
//
// Used after a restart: declaring success while Jellyfin is still coming back
// would make the final verification fail for a reason that has nothing to do
// with the proxy.
func (c *Client) WaitUntilReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := c.PublicInfo(ctx); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("jellyfin did not come back after restarting")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// APIError is a non-success response from Jellyfin.
type APIError struct {
	StatusCode int
	Method     string
	Path       string
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("jellyfin %s %s: status %d %s", e.Method, e.Path, e.StatusCode, e.Body)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, readerOf(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", c.authHeader())

	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}

	c.log.Debug("jellyfin request",
		slog.String("method", method),
		slog.String("path", path),
		slog.Int("status", resp.StatusCode),
		slog.Duration("duration", time.Since(start)),
	)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{
			StatusCode: resp.StatusCode,
			Method:     method,
			Path:       path,
			Body:       strings.TrimSpace(truncate(string(raw), 200)),
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

// authHeader builds Jellyfin's MediaBrowser authorization header. The token is
// omitted before login, when the header still identifies the client.
func (c *Client) authHeader() string {
	h := fmt.Sprintf(`MediaBrowser Client="%s", Device="%s", DeviceId="%s", Version="%s"`,
		ClientName, DeviceName, DeviceID, ClientVersion)
	if c.token != "" {
		h += fmt.Sprintf(`, Token="%s"`, c.token)
	}
	return h
}

func readerOf(b []byte) io.Reader {
	if b == nil {
		return nil
	}
	return bytes.NewReader(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
