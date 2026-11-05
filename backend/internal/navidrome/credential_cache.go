package navidrome

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var (
	ErrCredentialUnavailable = errors.New("navidrome credential is unavailable")
	ErrCredentialInvalidated = errors.New("navidrome credential refresh was invalidated")
)

// PasswordCredential is the durable, server-side credential loaded from
// Flow's encrypted credential store. It must never be serialized to a client.
type PasswordCredential struct {
	Username string
	Password string
}

// TokenCredential is an in-memory Navidrome session used only for server-side
// requests. Tokens are proactively refreshed before their JWT expiry.
type TokenCredential struct {
	Username  string
	Token     string
	ExpiresAt time.Time
}

type CredentialSource interface {
	LoadNavidromeCredential(context.Context, int64) (PasswordCredential, error)
}

type loginClient interface {
	Login(context.Context, string, string) (LoginResult, *Session, error)
}

type credentialEntry struct {
	credential TokenCredential
	generation uint64
}

type credentialCall struct {
	done       chan struct{}
	credential TokenCredential
	err        error
	generation uint64
}

// CredentialCache is the single process-wide member/admin Navidrome JWT
// cache. One authentication request per Flow user may be in flight at a time.
// Eviction increments a generation so a refresh that started before
// suspension cannot repopulate the cache or return a usable stale result.
type CredentialCache struct {
	mu            sync.Mutex
	source        CredentialSource
	client        loginClient
	now           func() time.Time
	refreshBefore time.Duration
	entries       map[int64]credentialEntry
	inflight      map[int64]*credentialCall
	generations   map[int64]uint64
}

func NewCredentialCache(source CredentialSource, client loginClient) *CredentialCache {
	return &CredentialCache{
		source:        source,
		client:        client,
		now:           time.Now,
		refreshBefore: 30 * time.Second,
		entries:       make(map[int64]credentialEntry),
		inflight:      make(map[int64]*credentialCall),
		generations:   make(map[int64]uint64),
	}
}

func (c *CredentialCache) Get(ctx context.Context, userID int64) (TokenCredential, error) {
	if userID < 1 {
		return TokenCredential{}, ErrCredentialUnavailable
	}
	for {
		c.mu.Lock()
		generation := c.generations[userID]
		if entry, ok := c.entries[userID]; ok && entry.generation == generation && c.usable(entry.credential) {
			credential := entry.credential
			c.mu.Unlock()
			return credential, nil
		}
		if call, ok := c.inflight[userID]; ok {
			done := call.done
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return TokenCredential{}, ctx.Err()
			case <-done:
				if call.err != nil {
					if errors.Is(call.err, context.Canceled) && ctx.Err() == nil {
						continue
					}
					return TokenCredential{}, call.err
				}
				return call.credential, nil
			}
		}

		call := &credentialCall{done: make(chan struct{}), generation: generation}
		c.inflight[userID] = call
		c.mu.Unlock()

		credential, err := c.refresh(ctx, userID)

		c.mu.Lock()
		if err == nil && c.generations[userID] != generation {
			err = ErrCredentialInvalidated
		}
		if err == nil {
			c.entries[userID] = credentialEntry{credential: credential, generation: generation}
		}
		call.credential = credential
		call.err = err
		delete(c.inflight, userID)
		close(call.done)
		c.mu.Unlock()
		return credential, err
	}
}

func (c *CredentialCache) usable(credential TokenCredential) bool {
	return credential.Token != "" && credential.Username != "" && credential.ExpiresAt.After(c.now().UTC().Add(c.refreshBefore))
}

func (c *CredentialCache) refresh(ctx context.Context, userID int64) (TokenCredential, error) {
	password, err := c.source.LoadNavidromeCredential(ctx, userID)
	if err != nil {
		return TokenCredential{}, err
	}
	if password.Username == "" || password.Password == "" {
		return TokenCredential{}, ErrCredentialUnavailable
	}
	login, _, err := c.client.Login(ctx, password.Username, password.Password)
	if err != nil {
		var httpError *HTTPError
		if errors.As(err, &httpError) && (httpError.StatusCode == 401 || httpError.StatusCode == 403) {
			return TokenCredential{}, ErrAuthentication
		}
		return TokenCredential{}, fmt.Errorf("authenticate navidrome user: %w", err)
	}
	if login.Username != password.Username || login.Token == "" {
		return TokenCredential{}, errors.New("navidrome login response did not match the credential identity")
	}
	expiresAt, err := jwtExpiry(login.Token)
	if err != nil {
		return TokenCredential{}, fmt.Errorf("read navidrome token expiry: %w", err)
	}
	if !expiresAt.After(c.now().UTC().Add(time.Second)) {
		return TokenCredential{}, errors.New("navidrome returned an expired token")
	}
	return TokenCredential{Username: login.Username, Token: login.Token, ExpiresAt: expiresAt}, nil
}

// Evict invalidates cached and in-flight credentials. It is used for
// suspension/deletion and therefore always advances the generation, even when
// no completed entry exists.
func (c *CredentialCache) Evict(userID int64) {
	c.mu.Lock()
	c.generations[userID]++
	delete(c.entries, userID)
	c.mu.Unlock()
}

// InvalidateIfMatch drops an upstream-rejected token without invalidating a
// newer refresh another request may already have completed.
func (c *CredentialCache) InvalidateIfMatch(userID int64, token string) {
	c.mu.Lock()
	entry, ok := c.entries[userID]
	if ok && entry.credential.Token == token {
		c.generations[userID]++
		delete(c.entries, userID)
	}
	c.mu.Unlock()
}

func jwtExpiry(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, errors.New("token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, errors.New("token payload is not base64url")
	}
	var claims struct {
		ExpiresAt json.Number `json:"exp"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.UseNumber()
	if err := decoder.Decode(&claims); err != nil {
		return time.Time{}, errors.New("token payload is invalid JSON")
	}
	seconds, err := claims.ExpiresAt.Int64()
	if err != nil || seconds <= 0 {
		return time.Time{}, errors.New("token omitted a valid expiry")
	}
	return time.Unix(seconds, 0).UTC(), nil
}
