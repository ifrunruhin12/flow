package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalidCredentials        = errors.New("invalid credentials")
	ErrUnauthenticated           = errors.New("unauthenticated")
	ErrAccountNotActive          = errors.New("account not active")
	ErrPasswordUnchanged         = errors.New("new password must differ from current password")
	ErrPasswordExternallyManaged = errors.New("password is managed by deployment configuration")
)

type User struct {
	ID                 int64     `json:"id"`
	Username           string    `json:"username"`
	DisplayName        string    `json:"displayName"`
	Email              *string   `json:"email,omitempty"`
	Role               string    `json:"role"`
	Status             string    `json:"status"`
	ProvisioningState  string    `json:"provisioningState"`
	MustChangePassword bool      `json:"mustChangePassword"`
	LastActiveAt       time.Time `json:"lastActiveAt,omitzero"`
	passwordHash       string
}

type Principal struct {
	User      User
	TokenHash []byte
	ExpiresAt time.Time
}

type LoginResult struct {
	User      User
	Token     string
	ExpiresAt time.Time
}

type Service struct {
	pool       *pgxpool.Pool
	sessionTTL time.Duration
	dummyHash  string
	streams    StreamRevoker
}

// StreamRevoker connects authoritative session revocation to M2's in-memory
// stream registry. Implementations must be safe for concurrent calls.
type StreamRevoker interface {
	AbortSession(sessionID string)
	AbortUserExceptSession(userID int64, sessionID string)
}

func NewService(pool *pgxpool.Pool, sessionTTL time.Duration) (*Service, error) {
	dummyHash, err := HashPassword("flow-invalid-login-password")
	if err != nil {
		return nil, err
	}
	return &Service{pool: pool, sessionTTL: sessionTTL, dummyHash: dummyHash}, nil
}

// SetStreamRevoker is called during process composition before the HTTP server
// begins accepting requests.
func (s *Service) SetStreamRevoker(streams StreamRevoker) {
	s.streams = streams
}

func (s *Service) Login(ctx context.Context, username, password, userAgent string) (LoginResult, error) {
	_, _ = s.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= now()`)
	username = strings.TrimSpace(username)
	if length := len([]rune(username)); length == 0 || length > 128 {
		_, _ = VerifyPassword(s.dummyHash, password)
		return LoginResult{}, ErrInvalidCredentials
	}
	user, err := s.findUserByUsername(ctx, username)
	if errors.Is(err, pgx.ErrNoRows) {
		_, _ = VerifyPassword(s.dummyHash, password)
		return LoginResult{}, ErrInvalidCredentials
	}
	if err != nil {
		return LoginResult{}, fmt.Errorf("find login user: %w", err)
	}
	valid, err := VerifyPassword(user.passwordHash, password)
	if err != nil {
		return LoginResult{}, fmt.Errorf("verify password: %w", err)
	}
	if !valid {
		return LoginResult{}, ErrInvalidCredentials
	}
	if user.Status != "active" || user.ProvisioningState != "complete" {
		return LoginResult{}, ErrAccountNotActive
	}
	token := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, token); err != nil {
		return LoginResult{}, fmt.Errorf("generate session token: %w", err)
	}
	rawToken := hex.EncodeToString(token)
	tokenHash := sha256.Sum256([]byte(rawToken))
	expiresAt := time.Now().UTC().Add(s.sessionTTL)
	if runes := []rune(userAgent); len(runes) > 255 {
		userAgent = string(runes[:255])
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO sessions(token_hash, user_id, expires_at, last_seen_at, user_agent_hint)
		VALUES ($1, $2, $3, now(), $4)`, tokenHash[:], user.ID, expiresAt, userAgent)
	if err != nil {
		return LoginResult{}, fmt.Errorf("create session: %w", err)
	}
	return LoginResult{User: publicUser(user), Token: rawToken, ExpiresAt: expiresAt}, nil
}

func (s *Service) Authenticate(ctx context.Context, rawToken string) (Principal, error) {
	return s.authenticate(ctx, rawToken, true)
}

// AuthenticateStream performs the same authoritative session and account
// lookup as Authenticate without extending the browser session. Audio bytes
// can be long-lived, but opening them must not keep an otherwise idle session
// alive indefinitely.
func (s *Service) AuthenticateStream(ctx context.Context, rawToken string) (Principal, error) {
	return s.authenticate(ctx, rawToken, false)
}

func (s *Service) authenticate(ctx context.Context, rawToken string, refresh bool) (Principal, error) {
	if len(rawToken) != 64 {
		return Principal{}, ErrUnauthenticated
	}
	if _, err := hex.DecodeString(rawToken); err != nil {
		return Principal{}, ErrUnauthenticated
	}
	hash := sha256.Sum256([]byte(rawToken))
	row := s.pool.QueryRow(ctx, `
		SELECT u.id, u.username, u.display_name, u.email, u.password_hash, u.role, u.status,
		       u.provisioning_state, u.must_change_password, u.last_active_at, s.expires_at
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.expires_at > now()`, hash[:])
	user, expiresAt, err := scanSessionUser(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, fmt.Errorf("resolve session: %w", err)
	}
	if user.Status != "active" || user.ProvisioningState != "complete" {
		return Principal{}, ErrAccountNotActive
	}
	if refresh {
		expiresAt = time.Now().UTC().Add(s.sessionTTL)
		command, err := s.pool.Exec(ctx, `UPDATE sessions SET last_seen_at = now(), expires_at = $2 WHERE token_hash = $1`, hash[:], expiresAt)
		if err != nil {
			return Principal{}, fmt.Errorf("refresh session: %w", err)
		}
		if command.RowsAffected() == 0 {
			return Principal{}, ErrUnauthenticated
		}
	}
	_, _ = s.pool.Exec(ctx, `
		UPDATE users SET last_active_at = now()
		WHERE id = $1 AND (last_active_at IS NULL OR last_active_at < now() - interval '10 minutes')`, user.ID)
	return Principal{User: publicUser(user), TokenHash: hash[:], ExpiresAt: expiresAt}, nil
}

func (s *Service) Logout(ctx context.Context, rawToken string) error {
	if rawToken == "" {
		return nil
	}
	hash := sha256.Sum256([]byte(rawToken))
	if _, err := s.pool.Exec(ctx, "DELETE FROM sessions WHERE token_hash = $1", hash[:]); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	if s.streams != nil {
		s.streams.AbortSession(hex.EncodeToString(hash[:]))
	}
	return nil
}

func (s *Service) ChangePassword(ctx context.Context, principal Principal, currentPassword, newPassword string) error {
	if principal.User.Role == "admin" {
		return ErrPasswordExternallyManaged
	}
	if err := ValidatePassword(newPassword); err != nil {
		return err
	}
	var encoded string
	if err := s.pool.QueryRow(ctx, "SELECT password_hash FROM users WHERE id = $1", principal.User.ID).Scan(&encoded); err != nil {
		return fmt.Errorf("read current password: %w", err)
	}
	valid, err := VerifyPassword(encoded, currentPassword)
	if err != nil {
		return fmt.Errorf("verify current password: %w", err)
	}
	if !valid {
		return ErrInvalidCredentials
	}
	same, err := VerifyPassword(encoded, newPassword)
	if err != nil {
		return err
	}
	if same {
		return ErrPasswordUnchanged
	}
	replacement, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin password change: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE users SET password_hash=$1, must_change_password=false, updated_at=now() WHERE id=$2`, replacement, principal.User.ID); err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1 AND token_hash<>$2`, principal.User.ID, principal.TokenHash); err != nil {
		return fmt.Errorf("revoke other sessions: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit password change: %w", err)
	}
	if s.streams != nil {
		s.streams.AbortUserExceptSession(principal.User.ID, hex.EncodeToString(principal.TokenHash))
	}
	return nil
}

func (s *Service) findUserByUsername(ctx context.Context, username string) (User, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, username, display_name, email, password_hash, role, status,
		       provisioning_state, must_change_password, last_active_at
		FROM users WHERE lower(username)=lower($1)`, strings.TrimSpace(username))
	return scanUser(row)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanUser(row rowScanner) (User, error) {
	var user User
	var lastActive *time.Time
	err := row.Scan(&user.ID, &user.Username, &user.DisplayName, &user.Email, &user.passwordHash, &user.Role,
		&user.Status, &user.ProvisioningState, &user.MustChangePassword, &lastActive)
	if lastActive != nil {
		user.LastActiveAt = *lastActive
	}
	return user, err
}

func scanSessionUser(row rowScanner) (User, time.Time, error) {
	var user User
	var lastActive *time.Time
	var expiresAt time.Time
	err := row.Scan(&user.ID, &user.Username, &user.DisplayName, &user.Email, &user.passwordHash, &user.Role,
		&user.Status, &user.ProvisioningState, &user.MustChangePassword, &lastActive, &expiresAt)
	if lastActive != nil {
		user.LastActiveAt = *lastActive
	}
	return user, expiresAt, err
}

func publicUser(user User) User {
	user.passwordHash = ""
	return user
}
