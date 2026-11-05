package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ifrunruhin12/flow/backend/internal/auth"
	"github.com/ifrunruhin12/flow/backend/internal/membership"
)

type fakeAuth struct {
	principal auth.Principal
	authErr   error
	login     auth.LoginResult
	loginErr  error
	changeErr error
	loggedOut bool
}

func (f *fakeAuth) Login(context.Context, string, string, string) (auth.LoginResult, error) {
	return f.login, f.loginErr
}

func (f *fakeAuth) Authenticate(context.Context, string) (auth.Principal, error) {
	return f.principal, f.authErr
}

func (f *fakeAuth) AuthenticateStream(context.Context, string) (auth.Principal, error) {
	return f.principal, f.authErr
}

func (f *fakeAuth) Logout(context.Context, string) error {
	f.loggedOut = true
	return nil
}

func (f *fakeAuth) ChangePassword(context.Context, auth.Principal, string, string) error {
	return f.changeErr
}

type fakeMembership struct{}

func (*fakeMembership) RegisterRequest(context.Context, string, string, string) (int64, error) {
	return 1, nil
}
func (*fakeMembership) ListRequests(context.Context, string) ([]membership.Request, error) {
	return []membership.Request{}, nil
}
func (*fakeMembership) ApproveRequest(context.Context, int64, int64, string, string, string) (membership.User, error) {
	return membership.User{}, nil
}
func (*fakeMembership) RejectRequest(context.Context, int64, int64, string, string) error {
	return nil
}
func (*fakeMembership) CreateMember(context.Context, int64, membership.NewMember, string) (membership.User, error) {
	return membership.User{}, nil
}
func (*fakeMembership) ListUsers(context.Context, membership.UserFilter) ([]membership.User, error) {
	return []membership.User{}, nil
}
func (*fakeMembership) Suspend(context.Context, int64, int64, string, string) error { return nil }
func (*fakeMembership) Reactivate(context.Context, int64, int64, string) error      { return nil }
func (*fakeMembership) Provision(context.Context, int64, int64, string) error       { return nil }
func (*fakeMembership) ResetPassword(context.Context, int64, int64, string, string) error {
	return nil
}
func (*fakeMembership) Delete(context.Context, int64, int64, bool, string) error { return nil }

func TestLoginSetsOpaqueSessionCookie(t *testing.T) {
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	authService := &fakeAuth{login: auth.LoginResult{
		User:  auth.User{ID: 1, Username: "admin", Role: "admin", Status: "active", ProvisioningState: "complete"},
		Token: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", ExpiresAt: expires,
	}}
	server := testServer(authService)
	request := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"username":"admin","password":"correct horse battery staple"}`))
	request.RemoteAddr = "127.0.0.1:1234"
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookie || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("unexpected cookies: %#v", cookies)
	}
	if cookies[0].Secure {
		t.Fatal("development HTTP cookie unexpectedly marked Secure")
	}
	if strings.Contains(response.Body.String(), authService.login.Token) {
		t.Fatal("opaque session token leaked in response body")
	}
}

func TestAdminRouteRejectsMember(t *testing.T) {
	authService := &fakeAuth{principal: activePrincipal("member", false)}
	server := testServer(authService)
	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/admin/requests"},
		{http.MethodPost, "/api/admin/requests/1/approve"},
		{http.MethodPost, "/api/admin/requests/1/reject"},
		{http.MethodGet, "/api/admin/users"},
		{http.MethodPost, "/api/admin/users"},
		{http.MethodPost, "/api/admin/users/1/suspend"},
		{http.MethodPost, "/api/admin/users/1/reactivate"},
		{http.MethodPost, "/api/admin/users/1/retry-provisioning"},
		{http.MethodPost, "/api/admin/users/1/reset-password"},
		{http.MethodDelete, "/api/admin/users/1"},
	}
	for _, route := range routes {
		origin := ""
		if route.method != http.MethodGet {
			origin = "http://127.0.0.1:5173"
		}
		response := performAuthenticated(server, route.method, route.path, origin)
		if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"forbidden"`) {
			t.Fatalf("%s %s status=%d body=%s", route.method, route.path, response.Code, response.Body.String())
		}
	}
}

func TestRestrictedFirstLoginSessionOnlyAllowsAuthRoutes(t *testing.T) {
	authService := &fakeAuth{principal: activePrincipal("member", true)}
	response := performAuthenticated(testServer(authService), http.MethodGet, "/api/admin/users", "")
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "password_change_required") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	response = performAuthenticated(testServer(authService), http.MethodGet, "/api/auth/me", "")
	if response.Code != http.StatusOK {
		t.Fatalf("me status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestLogoutRequiresSameOrigin(t *testing.T) {
	authService := &fakeAuth{principal: activePrincipal("admin", false)}
	server := testServer(authService)
	foreign := performAuthenticated(server, http.MethodPost, "/api/auth/logout", "https://evil.example")
	if foreign.Code != http.StatusForbidden || authService.loggedOut {
		t.Fatalf("foreign logout status=%d loggedOut=%t", foreign.Code, authService.loggedOut)
	}
	sameOrigin := performAuthenticated(server, http.MethodPost, "/api/auth/logout", "http://127.0.0.1:5173")
	if sameOrigin.Code != http.StatusNoContent || !authService.loggedOut {
		t.Fatalf("same-origin logout status=%d loggedOut=%t", sameOrigin.Code, authService.loggedOut)
	}
}

func TestAdministratorPasswordIsDeploymentManaged(t *testing.T) {
	authService := &fakeAuth{principal: activePrincipal("admin", false), changeErr: auth.ErrPasswordExternallyManaged}
	server := testServer(authService)
	request := httptest.NewRequest(http.MethodPost, "/api/auth/change-password", strings.NewReader(`{"currentPassword":"admin-password","newPassword":"new-admin-password"}`))
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"})
	request.Header.Set("Origin", "http://127.0.0.1:5173")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestMissingSessionIsUnauthorized(t *testing.T) {
	response := httptest.NewRecorder()
	testServer(&fakeAuth{}).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/auth/me", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestClientIPOnlyTrustsForwardingFromPrivatePeer(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "172.18.0.4:1234"
	request.Header.Set("X-Forwarded-For", "203.0.113.9, 172.18.0.4")
	if got := clientIP(request); got != "203.0.113.9" {
		t.Fatalf("proxied client IP = %q", got)
	}
	request.RemoteAddr = "198.51.100.8:1234"
	request.Header.Set("X-Forwarded-For", "203.0.113.10")
	if got := clientIP(request); got != "198.51.100.8" {
		t.Fatalf("public peer spoofed client IP = %q", got)
	}
}

func testServer(authService *fakeAuth) *Server {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(logger, authService, &fakeMembership{}, "http://127.0.0.1:5173", false, time.Hour)
}

func activePrincipal(role string, mustChange bool) auth.Principal {
	return auth.Principal{User: auth.User{
		ID: 1, Username: "member", Role: role, Status: "active", ProvisioningState: "complete", MustChangePassword: mustChange,
	}, TokenHash: []byte("01234567890123456789012345678901")}
}

func performAuthenticated(server *Server, method, path, origin string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"})
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}
