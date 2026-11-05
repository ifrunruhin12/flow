package navidrome

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestVerifyAdmin(t *testing.T) {
	var sawAuthorization atomic.Bool
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/ping":
			return response(http.StatusOK, "", nil), nil
		case "/auth/login":
			var credentials map[string]string
			if err := json.NewDecoder(r.Body).Decode(&credentials); err != nil {
				t.Fatal(err)
			}
			if credentials["username"] != "admin" || credentials["password"] != "secret" {
				t.Fatalf("unexpected credentials: %#v", credentials)
			}
			return response(http.StatusOK, `{"id":"u1","username":"admin","isAdmin":true,"token":"token-one"}`, nil), nil
		case "/api/user/":
			sawAuthorization.Store(r.Header.Get(authorizationHeader) == "Bearer token-one")
			headers := make(http.Header)
			headers.Set(authorizationHeader, "Bearer token-two")
			return response(http.StatusOK, `[{"id":"u1","userName":"admin","isAdmin":true}]`, headers), nil
		case "/api/config/config":
			if got := r.Header.Get(authorizationHeader); got != "Bearer token-two" {
				t.Fatalf("refreshed token not used: %q", got)
			}
			return response(http.StatusOK, `{"config":{"EnableDownloads":false,"EnableArtworkUpload":false,"EnableSharing":false}}`, nil), nil
		default:
			return response(http.StatusNotFound, "", nil), nil
		}
	})}

	client, err := NewWithHTTPClient("http://navidrome.test", httpClient)
	if err != nil {
		t.Fatal(err)
	}
	report, err := client.VerifyAdmin(context.Background(), "admin", "secret")
	if err != nil {
		t.Fatalf("VerifyAdmin() error = %v", err)
	}
	if report.Username != "admin" || !sawAuthorization.Load() {
		t.Fatalf("unexpected report: %+v", report)
	}
}

func TestVerifyAdminRejectsOpenGate(t *testing.T) {
	client := fakeVerificationClient(t, `{"EnableDownloads":true,"EnableArtworkUpload":false,"EnableSharing":false}`)
	if _, err := client.VerifyAdmin(context.Background(), "admin", "secret"); err == nil || !strings.Contains(err.Error(), "gates") {
		t.Fatalf("expected gate error, got %v", err)
	}
}

func TestLoginErrorDoesNotExposeBody(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusUnauthorized, "password=super-secret", nil), nil
	})}
	client, _ := NewWithHTTPClient("http://navidrome.test", httpClient)
	_, _, err := client.Login(context.Background(), "admin", "super-secret")
	if err == nil || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestNewRejectsURLCredentials(t *testing.T) {
	if _, err := New("http://admin:secret@example.test", time.Second); err == nil {
		t.Fatal("New() expected an error")
	}
}

func TestPingHonorsContextCancellation(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	client, err := NewWithHTTPClient("http://navidrome.test", httpClient)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.Ping(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Ping() error = %v", err)
	}
}

func TestTransportErrorRedactsInternalURL(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Get", URL: "http://navidrome.internal:4533/ping", Err: errors.New("connection refused")}
	})}
	client, err := NewWithHTTPClient("http://navidrome.internal:4533", httpClient)
	if err != nil {
		t.Fatal(err)
	}
	pingErr := client.Ping(context.Background())
	if pingErr == nil || strings.Contains(pingErr.Error(), "navidrome.internal") {
		t.Fatalf("unsafe transport error: %v", pingErr)
	}
}

func fakeVerificationClient(t *testing.T, gates string) *Client {
	t.Helper()
	httpClient := &http.Client{Timeout: time.Second, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/ping":
			return response(http.StatusOK, "", nil), nil
		case "/auth/login":
			return response(http.StatusOK, `{"id":"u1","username":"admin","isAdmin":true,"token":"token"}`, nil), nil
		case "/api/user/":
			return response(http.StatusOK, `[{"id":"u1","userName":"admin","isAdmin":true}]`, nil), nil
		case "/api/config/config":
			return response(http.StatusOK, `{"config":`+gates+`}`, nil), nil
		default:
			return response(http.StatusNotFound, "", nil), nil
		}
	})}
	client, err := NewWithHTTPClient("http://navidrome.test", httpClient)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func response(status int, body string, headers http.Header) *http.Response {
	if headers == nil {
		headers = make(http.Header)
	}
	return &http.Response{
		StatusCode: status,
		Header:     headers,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
