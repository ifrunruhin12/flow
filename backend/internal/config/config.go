package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var flowUsernamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{2,31}$`)

const (
	defaultHTTPAddr          = "127.0.0.1:8080"
	defaultHTTPTimeout       = 5 * time.Second
	defaultIdleTimeout       = 60 * time.Second
	defaultShutdownTimeout   = 10 * time.Second
	defaultNavidromeTimeout  = 5 * time.Second
	defaultStreamHeadTimeout = 10 * time.Second
	defaultMigrationTimeout  = 30 * time.Second
	defaultDatabaseMaxConns  = int32(10)
	defaultSessionTTL        = 8 * time.Hour
	defaultProvisionTimeout  = 30 * time.Second
)

type Config struct {
	HTTPAddr                     string
	HTTPReadHeaderTimeout        time.Duration
	HTTPIdleTimeout              time.Duration
	ShutdownTimeout              time.Duration
	DatabaseURL                  string
	DatabaseMaxConns             int32
	MigrationTimeout             time.Duration
	NavidromeURL                 string
	NavidromeTimeout             time.Duration
	NavidromeStreamHeaderTimeout time.Duration
	NavidromeAdminUsername       string
	NavidromeAdminPassword       string
	FlowAdminUsername            string
	FlowAdminPassword            string
	CredentialKeyringFile        string
	PublicOrigin                 string
	CookieSecure                 bool
	SessionTTL                   time.Duration
	ProvisionTimeout             time.Duration
	LogLevel                     slog.Level
}

func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:               envOr("FLOW_HTTP_ADDR", defaultHTTPAddr),
		DatabaseURL:            strings.TrimSpace(os.Getenv("FLOW_DATABASE_URL")),
		NavidromeURL:           strings.TrimSpace(os.Getenv("FLOW_NAVIDROME_URL")),
		NavidromeAdminUsername: strings.TrimSpace(os.Getenv("FLOW_NAVIDROME_ADMIN_USERNAME")),
		NavidromeAdminPassword: os.Getenv("FLOW_NAVIDROME_ADMIN_PASSWORD"),
		FlowAdminUsername:      strings.TrimSpace(os.Getenv("FLOW_ADMIN_USERNAME")),
		FlowAdminPassword:      os.Getenv("FLOW_ADMIN_PASSWORD"),
		CredentialKeyringFile:  strings.TrimSpace(os.Getenv("FLOW_CREDENTIAL_KEYRING_FILE")),
		PublicOrigin:           strings.TrimRight(strings.TrimSpace(os.Getenv("FLOW_PUBLIC_ORIGIN")), "/"),
		DatabaseMaxConns:       defaultDatabaseMaxConns,
	}

	var err error
	if cfg.HTTPReadHeaderTimeout, err = durationEnv("FLOW_HTTP_READ_HEADER_TIMEOUT", defaultHTTPTimeout); err != nil {
		return Config{}, err
	}
	if cfg.HTTPIdleTimeout, err = durationEnv("FLOW_HTTP_IDLE_TIMEOUT", defaultIdleTimeout); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = durationEnv("FLOW_SHUTDOWN_TIMEOUT", defaultShutdownTimeout); err != nil {
		return Config{}, err
	}
	if cfg.NavidromeTimeout, err = durationEnv("FLOW_NAVIDROME_TIMEOUT", defaultNavidromeTimeout); err != nil {
		return Config{}, err
	}
	if cfg.NavidromeStreamHeaderTimeout, err = durationEnv("FLOW_NAVIDROME_STREAM_HEADER_TIMEOUT", defaultStreamHeadTimeout); err != nil {
		return Config{}, err
	}
	if cfg.MigrationTimeout, err = durationEnv("FLOW_MIGRATION_TIMEOUT", defaultMigrationTimeout); err != nil {
		return Config{}, err
	}
	if cfg.SessionTTL, err = durationEnv("FLOW_SESSION_TTL", defaultSessionTTL); err != nil {
		return Config{}, err
	}
	if cfg.ProvisionTimeout, err = durationEnv("FLOW_PROVISION_TIMEOUT", defaultProvisionTimeout); err != nil {
		return Config{}, err
	}
	if cfg.DatabaseMaxConns, err = int32Env("FLOW_DATABASE_MAX_CONNS", defaultDatabaseMaxConns); err != nil {
		return Config{}, err
	}
	if cfg.LogLevel, err = logLevelEnv("FLOW_LOG_LEVEL", slog.LevelInfo); err != nil {
		return Config{}, err
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	origin, _ := url.Parse(cfg.PublicOrigin)
	cfg.CookieSecure = origin.Scheme == "https"
	return cfg, nil
}

func (c Config) Validate() error {
	var problems []error
	if strings.TrimSpace(c.DatabaseURL) == "" {
		problems = append(problems, errors.New("FLOW_DATABASE_URL is required"))
	} else if u, err := url.Parse(c.DatabaseURL); err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		problems = append(problems, errors.New("FLOW_DATABASE_URL must be a valid postgres URL"))
	}
	if err := validateHTTPURL("FLOW_NAVIDROME_URL", c.NavidromeURL); err != nil {
		problems = append(problems, err)
	}
	if _, _, err := net.SplitHostPort(c.HTTPAddr); err != nil {
		problems = append(problems, fmt.Errorf("FLOW_HTTP_ADDR must be host:port: %w", err))
	}
	if (c.NavidromeAdminUsername == "") != (c.NavidromeAdminPassword == "") {
		problems = append(problems, errors.New("FLOW_NAVIDROME_ADMIN_USERNAME and FLOW_NAVIDROME_ADMIN_PASSWORD must be set together"))
	}
	if c.FlowAdminUsername == "" {
		problems = append(problems, errors.New("FLOW_ADMIN_USERNAME is required"))
	} else if !flowUsernamePattern.MatchString(c.FlowAdminUsername) {
		problems = append(problems, errors.New("FLOW_ADMIN_USERNAME must be 3-32 characters using letters, numbers, dot, underscore, or hyphen"))
	}
	if c.FlowAdminPassword == "" {
		problems = append(problems, errors.New("FLOW_ADMIN_PASSWORD is required"))
	} else if length := len([]rune(c.FlowAdminPassword)); length < 12 || length > 128 {
		problems = append(problems, errors.New("FLOW_ADMIN_PASSWORD must be between 12 and 128 characters"))
	}
	if c.CredentialKeyringFile == "" {
		problems = append(problems, errors.New("FLOW_CREDENTIAL_KEYRING_FILE is required"))
	}
	if err := validateOrigin("FLOW_PUBLIC_ORIGIN", c.PublicOrigin); err != nil {
		problems = append(problems, err)
	}
	if c.DatabaseMaxConns < 1 {
		problems = append(problems, errors.New("FLOW_DATABASE_MAX_CONNS must be positive"))
	}
	return errors.Join(problems...)
}

func validateOrigin(name, raw string) error {
	if raw == "" {
		return fmt.Errorf("%s is required", name)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s must be an http(s) origin without path, credentials, query, or fragment", name)
	}
	if u.Scheme == "http" {
		hostname := u.Hostname()
		address := net.ParseIP(hostname)
		if hostname != "localhost" && (address == nil || !address.IsLoopback()) {
			return fmt.Errorf("%s must use https unless it is a loopback development origin", name)
		}
	}
	return nil
}

func (c Config) NavidromeCredentialsConfigured() bool {
	return c.NavidromeAdminUsername != "" && c.NavidromeAdminPassword != ""
}

func validateHTTPURL(name, raw string) error {
	if raw == "" {
		return fmt.Errorf("%s is required", name)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return fmt.Errorf("%s must be an http(s) URL without user information", name)
	}
	return nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return value, nil
}

func int32Env(name string, fallback int32) (int32, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || value < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return int32(value), nil
}

func logLevelEnv(name string, fallback slog.Level) (slog.Level, error) {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	if raw == "" {
		return fallback, nil
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(raw)); err != nil {
		return 0, fmt.Errorf("%s must be debug, info, warn, or error", name)
	}
	return level, nil
}
