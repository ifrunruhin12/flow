package config

import (
	"testing"
	"time"
)

func TestLoadValid(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("FLOW_HTTP_ADDR", "127.0.0.1:9090")
	t.Setenv("FLOW_NAVIDROME_TIMEOUT", "3s")
	t.Setenv("FLOW_NAVIDROME_STREAM_HEADER_TIMEOUT", "11s")
	t.Setenv("FLOW_LOG_LEVEL", "debug")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.HTTPAddr != "127.0.0.1:9090" || cfg.NavidromeTimeout != 3*time.Second || cfg.NavidromeStreamHeaderTimeout != 11*time.Second {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadRejectsPartialCredentials(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("FLOW_NAVIDROME_ADMIN_USERNAME", "admin")
	t.Setenv("FLOW_NAVIDROME_ADMIN_PASSWORD", "")

	if _, err := Load(); err == nil {
		t.Fatal("Load() expected an error")
	}
}

func TestLoadRejectsURLUserInfo(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("FLOW_NAVIDROME_URL", "http://user:secret@localhost:4533")

	if _, err := Load(); err == nil {
		t.Fatal("Load() expected an error")
	}
}

func TestLoadDerivesCookieSecurityFromOrigin(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("FLOW_PUBLIC_ORIGIN", "https://flow.example.test")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.CookieSecure {
		t.Fatal("CookieSecure = false, want true")
	}
}

func TestLoadRejectsShortFlowAdminPassword(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("FLOW_ADMIN_PASSWORD", "too-short")
	if _, err := Load(); err == nil {
		t.Fatal("Load() expected an error")
	}
}

func TestLoadRejectsNonLoopbackHTTPPublicOrigin(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("FLOW_PUBLIC_ORIGIN", "http://flow.example.test")
	if _, err := Load(); err == nil {
		t.Fatal("Load() expected an error")
	}
}

func TestLoadRejectsUnsafeFlowAdminUsername(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("FLOW_ADMIN_USERNAME", "../admin")
	if _, err := Load(); err == nil {
		t.Fatal("Load() expected an error")
	}
}

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("FLOW_DATABASE_URL", "postgres://flow:flow@localhost:5432/flow?sslmode=disable")
	t.Setenv("FLOW_NAVIDROME_URL", "http://localhost:4533")
	t.Setenv("FLOW_NAVIDROME_ADMIN_USERNAME", "")
	t.Setenv("FLOW_NAVIDROME_ADMIN_PASSWORD", "")
	t.Setenv("FLOW_ADMIN_USERNAME", "flow-admin")
	t.Setenv("FLOW_ADMIN_PASSWORD", "flow-admin-password")
	t.Setenv("FLOW_CREDENTIAL_KEYRING_FILE", "/run/secrets/flow-keyring.json")
	t.Setenv("FLOW_PUBLIC_ORIGIN", "http://127.0.0.1:5173")
}
