package navidrome

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type credentialSourceFunc func(context.Context, int64) (PasswordCredential, error)

func (f credentialSourceFunc) LoadNavidromeCredential(ctx context.Context, userID int64) (PasswordCredential, error) {
	return f(ctx, userID)
}

type loginClientFunc func(context.Context, string, string) (LoginResult, *Session, error)

func (f loginClientFunc) Login(ctx context.Context, username, password string) (LoginResult, *Session, error) {
	return f(ctx, username, password)
}

func TestCredentialCacheCollapsesConcurrentRefresh(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var logins atomic.Int32
	cache := NewCredentialCache(credentialSourceFunc(func(_ context.Context, userID int64) (PasswordCredential, error) {
		if userID != 44 {
			t.Fatalf("credential source user = %d", userID)
		}
		return PasswordCredential{Username: "member", Password: "secret"}, nil
	}), loginClientFunc(func(_ context.Context, username, _ string) (LoginResult, *Session, error) {
		logins.Add(1)
		once.Do(func() { close(started) })
		<-release
		return LoginResult{Username: username, Token: testJWT(now.Add(time.Hour))}, nil, nil
	}))
	cache.now = func() time.Time { return now }

	const callers = 32
	results := make(chan error, callers)
	for range callers {
		go func() {
			credential, err := cache.Get(context.Background(), 44)
			if err == nil && credential.Username != "member" {
				err = errors.New("wrong credential identity")
			}
			results <- err
		}()
	}
	<-started
	close(release)
	for range callers {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if got := logins.Load(); got != 1 {
		t.Fatalf("login calls = %d, want 1", got)
	}
}

func TestCredentialCacheRefreshesNearExpiry(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var logins atomic.Int32
	cache := NewCredentialCache(credentialSourceFunc(func(context.Context, int64) (PasswordCredential, error) {
		return PasswordCredential{Username: "member", Password: "secret"}, nil
	}), loginClientFunc(func(_ context.Context, username, _ string) (LoginResult, *Session, error) {
		call := logins.Add(1)
		expiry := now.Add(20 * time.Second)
		if call > 1 {
			expiry = now.Add(time.Hour)
		}
		return LoginResult{Username: username, Token: testJWT(expiry)}, nil, nil
	}))
	cache.now = func() time.Time { return now }
	first, err := cache.Get(context.Background(), 8)
	if err != nil {
		t.Fatal(err)
	}
	second, err := cache.Get(context.Background(), 8)
	if err != nil {
		t.Fatal(err)
	}
	if first.Token == second.Token || logins.Load() != 2 {
		t.Fatalf("near-expiry token was not refreshed: calls=%d", logins.Load())
	}
}

func TestCredentialCacheEvictionFencesInflightRefresh(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	started := make(chan struct{})
	release := make(chan struct{})
	var logins atomic.Int32
	cache := NewCredentialCache(credentialSourceFunc(func(context.Context, int64) (PasswordCredential, error) {
		return PasswordCredential{Username: "member", Password: "secret"}, nil
	}), loginClientFunc(func(_ context.Context, username, _ string) (LoginResult, *Session, error) {
		call := logins.Add(1)
		if call == 1 {
			close(started)
			<-release
		}
		return LoginResult{Username: username, Token: testJWT(now.Add(time.Duration(call) * time.Hour))}, nil, nil
	}))
	cache.now = func() time.Time { return now }

	result := make(chan error, 1)
	go func() {
		_, err := cache.Get(context.Background(), 15)
		result <- err
	}()
	<-started
	cache.Evict(15)
	close(release)
	if err := <-result; !errors.Is(err, ErrCredentialInvalidated) {
		t.Fatalf("in-flight Get() error = %v", err)
	}
	credential, err := cache.Get(context.Background(), 15)
	if err != nil {
		t.Fatal(err)
	}
	if credential.ExpiresAt != now.Add(2*time.Hour) || logins.Load() != 2 {
		t.Fatalf("post-eviction credential=%#v calls=%d", credential, logins.Load())
	}
}

func TestCredentialCacheWaiterRetriesWhenRefreshOwnerCancels(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	firstStarted := make(chan struct{})
	var calls atomic.Int32
	cache := NewCredentialCache(credentialSourceFunc(func(context.Context, int64) (PasswordCredential, error) {
		return PasswordCredential{Username: "member", Password: "secret"}, nil
	}), loginClientFunc(func(ctx context.Context, username, _ string) (LoginResult, *Session, error) {
		if calls.Add(1) == 1 {
			close(firstStarted)
			<-ctx.Done()
			return LoginResult{}, nil, ctx.Err()
		}
		return LoginResult{Username: username, Token: testJWT(now.Add(time.Hour))}, nil, nil
	}))
	cache.now = func() time.Time { return now }
	ownerContext, cancelOwner := context.WithCancel(context.Background())
	owner := make(chan error, 1)
	go func() {
		_, err := cache.Get(ownerContext, 18)
		owner <- err
	}()
	<-firstStarted
	waiter := make(chan error, 1)
	go func() {
		_, err := cache.Get(context.Background(), 18)
		waiter <- err
	}()
	cancelOwner()
	if err := <-owner; !errors.Is(err, context.Canceled) {
		t.Fatalf("owner error = %v", err)
	}
	if err := <-waiter; err != nil {
		t.Fatalf("waiter error = %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("login calls = %d, want 2", calls.Load())
	}
}

func TestCredentialCacheRejectsMalformedAndExpiredJWT(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	tokens := []string{"not-a-jwt", testJWT(now.Add(-time.Minute))}
	for _, token := range tokens {
		cache := NewCredentialCache(credentialSourceFunc(func(context.Context, int64) (PasswordCredential, error) {
			return PasswordCredential{Username: "member", Password: "secret"}, nil
		}), loginClientFunc(func(_ context.Context, username, _ string) (LoginResult, *Session, error) {
			return LoginResult{Username: username, Token: token}, nil, nil
		}))
		cache.now = func() time.Time { return now }
		if _, err := cache.Get(context.Background(), 1); err == nil {
			t.Fatalf("Get() accepted token %q", token)
		}
	}
}

func TestCredentialCacheMapsRejectedLoginToAuthenticationFailure(t *testing.T) {
	for _, statusCode := range []int{401, 403} {
		cache := NewCredentialCache(credentialSourceFunc(func(context.Context, int64) (PasswordCredential, error) {
			return PasswordCredential{Username: "member", Password: "secret"}, nil
		}), loginClientFunc(func(context.Context, string, string) (LoginResult, *Session, error) {
			return LoginResult{}, nil, &HTTPError{Operation: "login", StatusCode: statusCode}
		}))
		if _, err := cache.Get(context.Background(), 1); !errors.Is(err, ErrAuthentication) {
			t.Fatalf("login HTTP %d error = %v", statusCode, err)
		}
	}
}

func testJWT(expiry time.Time) string {
	payload, _ := json.Marshal(map[string]int64{"exp": expiry.Unix()})
	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}
