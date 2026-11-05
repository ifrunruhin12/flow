package navidrome

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const authorizationHeader = "X-ND-Authorization"

type Client struct {
	baseURL    *url.URL
	http       *http.Client
	streamHTTP *http.Client
}

type Session struct {
	client *Client
	mu     sync.RWMutex
	token  string
}

type LoginResult struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Username string `json:"username"`
	IsAdmin  bool   `json:"isAdmin"`
	Token    string `json:"token"`
}

type GateConfig struct {
	EnableDownloads     bool
	EnableArtworkUpload bool
	EnableSharing       bool
}

type Verification struct {
	UserID   string
	Username string
	Gates    GateConfig
}

type User struct {
	ID       string `json:"id"`
	Username string `json:"userName"`
	IsAdmin  bool   `json:"isAdmin"`
}

type HTTPError struct {
	Operation  string
	StatusCode int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("navidrome %s returned HTTP %d", e.Operation, e.StatusCode)
}

func New(rawBaseURL string, timeout time.Duration) (*Client, error) {
	return NewWithTimeouts(rawBaseURL, timeout, timeout)
}

// NewWithTimeouts applies a bounded timeout to ordinary API calls and a
// response-header timeout to streams. Audio bodies deliberately have no total
// wall-clock timeout: request cancellation, client disconnect, suspension, or
// process shutdown cancels them instead.
func NewWithTimeouts(rawBaseURL string, requestTimeout, streamHeaderTimeout time.Duration) (*Client, error) {
	if requestTimeout <= 0 || streamHeaderTimeout <= 0 {
		return nil, errors.New("navidrome timeouts must be positive")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: requestTimeout, KeepAlive: 30 * time.Second}).DialContext
	transport.ResponseHeaderTimeout = streamHeaderTimeout
	transport.TLSHandshakeTimeout = requestTimeout
	transport.ExpectContinueTimeout = time.Second
	return NewWithHTTPClient(rawBaseURL, &http.Client{Timeout: requestTimeout, Transport: transport})
}

// NewWithHTTPClient constructs a client with an injected HTTP client. It is
// primarily useful for deterministic adapter tests and transports that dial a
// Unix socket while preserving the same request and response contracts.
func NewWithHTTPClient(rawBaseURL string, httpClient *http.Client) (*Client, error) {
	baseURL, err := url.Parse(rawBaseURL)
	if err != nil || (baseURL.Scheme != "http" && baseURL.Scheme != "https") || baseURL.Host == "" || baseURL.User != nil {
		return nil, errors.New("navidrome base URL must be an http(s) URL without user information")
	}
	if httpClient == nil {
		return nil, errors.New("navidrome HTTP client is required")
	}
	baseURL.Path = strings.TrimRight(baseURL.Path, "/") + "/"
	clientCopy := *httpClient
	clientCopy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	streamCopy := clientCopy
	streamCopy.Timeout = 0
	return &Client{
		baseURL:    baseURL,
		http:       &clientCopy,
		streamHTTP: &streamCopy,
	}, nil
}

func (c *Client) Ping(ctx context.Context) error {
	request, err := c.request(ctx, http.MethodGet, "ping", nil)
	if err != nil {
		return err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return sanitizeTransportError("ping", err)
	}
	defer response.Body.Close()
	_, _ = io.CopyN(io.Discard, response.Body, 1024)
	if response.StatusCode != http.StatusOK {
		return &HTTPError{Operation: "ping", StatusCode: response.StatusCode}
	}
	return nil
}

func (c *Client) Login(ctx context.Context, username, password string) (LoginResult, *Session, error) {
	body, err := json.Marshal(map[string]string{"username": username, "password": password})
	if err != nil {
		return LoginResult{}, nil, fmt.Errorf("encode navidrome login: %w", err)
	}
	request, err := c.request(ctx, http.MethodPost, "auth/login", bytes.NewReader(body))
	if err != nil {
		return LoginResult{}, nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return LoginResult{}, nil, sanitizeTransportError("login", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.CopyN(io.Discard, response.Body, 8<<10)
		return LoginResult{}, nil, &HTTPError{Operation: "login", StatusCode: response.StatusCode}
	}
	var result LoginResult
	if err := decodeJSON(response.Body, &result); err != nil {
		return LoginResult{}, nil, fmt.Errorf("decode navidrome login: %w", err)
	}
	if result.Token == "" || result.Username == "" {
		return LoginResult{}, nil, errors.New("navidrome login response omitted required fields")
	}
	session := &Session{client: c, token: result.Token}
	return result, session, nil
}

func (s *Session) ListUsers(ctx context.Context) ([]User, error) {
	response, err := s.do(ctx, http.MethodGet, "api/user/", nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.CopyN(io.Discard, response.Body, 8<<10)
		return nil, &HTTPError{Operation: "list users", StatusCode: response.StatusCode}
	}
	var payload []User
	if err := decodeJSON(response.Body, &payload); err != nil {
		return nil, fmt.Errorf("decode navidrome users: %w", err)
	}
	for _, user := range payload {
		if user.ID == "" || user.Username == "" {
			return nil, errors.New("navidrome user response omitted required fields")
		}
	}
	return payload, nil
}

func (s *Session) ReadGates(ctx context.Context) (GateConfig, error) {
	response, err := s.do(ctx, http.MethodGet, "api/config/config", nil)
	if err != nil {
		return GateConfig{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.CopyN(io.Discard, response.Body, 8<<10)
		return GateConfig{}, &HTTPError{Operation: "read config", StatusCode: response.StatusCode}
	}
	var payload struct {
		Config map[string]any `json:"config"`
	}
	if err := decodeJSON(response.Body, &payload); err != nil {
		return GateConfig{}, fmt.Errorf("decode navidrome config: %w", err)
	}
	gates := GateConfig{}
	var ok bool
	if gates.EnableDownloads, ok = boolField(payload.Config, "EnableDownloads"); !ok {
		return GateConfig{}, errors.New("navidrome config omitted EnableDownloads")
	}
	if gates.EnableArtworkUpload, ok = boolField(payload.Config, "EnableArtworkUpload"); !ok {
		return GateConfig{}, errors.New("navidrome config omitted EnableArtworkUpload")
	}
	if gates.EnableSharing, ok = boolField(payload.Config, "EnableSharing"); !ok {
		return GateConfig{}, errors.New("navidrome config omitted EnableSharing")
	}
	return gates, nil
}

func (c *Client) VerifyAdmin(ctx context.Context, username, password string) (Verification, error) {
	if err := c.Ping(ctx); err != nil {
		return Verification{}, err
	}
	login, session, err := c.Login(ctx, username, password)
	if err != nil {
		return Verification{}, err
	}
	if !login.IsAdmin {
		return Verification{}, errors.New("navidrome credential is not an administrator")
	}
	users, err := session.ListUsers(ctx)
	if err != nil {
		return Verification{}, err
	}
	linkedAdmin := false
	for _, user := range users {
		if user.ID == login.ID && user.Username == login.Username && user.IsAdmin {
			linkedAdmin = true
			break
		}
	}
	if !linkedAdmin {
		return Verification{}, errors.New("navidrome administrator was not present in native user read-back")
	}
	gates, err := session.ReadGates(ctx)
	if err != nil {
		return Verification{}, err
	}
	if gates.EnableDownloads || gates.EnableArtworkUpload || gates.EnableSharing {
		return Verification{}, errors.New("navidrome security gates are not all disabled")
	}
	return Verification{UserID: login.ID, Username: login.Username, Gates: gates}, nil
}

func (s *Session) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	request, err := s.client.request(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	s.mu.RLock()
	token := s.token
	s.mu.RUnlock()
	request.Header.Set(authorizationHeader, "Bearer "+token)
	response, err := s.client.http.Do(request)
	if err != nil {
		return nil, sanitizeTransportError("authenticated request", err)
	}
	if refreshed := strings.TrimSpace(response.Header.Get(authorizationHeader)); refreshed != "" {
		const prefix = "Bearer "
		if len(refreshed) > len(prefix) && subtle.ConstantTimeCompare([]byte(strings.ToLower(refreshed[:len(prefix)])), []byte(strings.ToLower(prefix))) == 1 {
			s.mu.Lock()
			s.token = refreshed[len(prefix):]
			s.mu.Unlock()
		}
	}
	return response, nil
}

func (c *Client) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	reference, err := url.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("build navidrome URL: %w", err)
	}
	if reference.IsAbs() || reference.Host != "" || strings.HasPrefix(path, "//") {
		return nil, errors.New("navidrome request path must be relative")
	}
	endpoint := c.baseURL.ResolveReference(reference)
	return http.NewRequestWithContext(ctx, method, endpoint.String(), body)
}

func decodeJSON(reader io.Reader, target any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, 2<<20))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return nil
}

func boolField(values map[string]any, name string) (bool, bool) {
	value, ok := values[name]
	if !ok {
		return false, false
	}
	result, ok := value.(bool)
	return result, ok
}

func sanitizeTransportError(operation string, err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("navidrome %s: %w", operation, context.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("navidrome %s: %w", operation, context.DeadlineExceeded)
	default:
		return fmt.Errorf("navidrome %s request failed: %w", operation, ErrUnavailable)
	}
}
