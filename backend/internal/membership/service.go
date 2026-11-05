package membership

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/ifrunruhin12/flow/backend/internal/auth"
	"github.com/ifrunruhin12/flow/backend/internal/credentials"
	"github.com/ifrunruhin12/flow/backend/internal/navidrome"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const credentialPurpose = "navidrome-password"

var (
	ErrNotFound             = errors.New("not found")
	ErrConflict             = errors.New("conflict")
	ErrInvalidInput         = errors.New("invalid input")
	ErrProvisioningBusy     = errors.New("provisioning already in progress")
	ErrProvisioningState    = errors.New("invalid provisioning state")
	ErrCompensationPending  = errors.New("deletion compensation pending")
	ErrNavidromeUnavailable = errors.New("Navidrome integration is unavailable")
	usernamePattern         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{2,31}$`)
)

type Directory interface {
	EnsureMemberIdentity(context.Context, navidrome.AdminCredential, navidrome.MemberInput) (string, error)
	ResolveMemberIdentity(context.Context, navidrome.AdminCredential, string, string) (string, bool, error)
	AssignAllLibraries(context.Context, navidrome.AdminCredential, string) error
	DeleteMember(context.Context, navidrome.AdminCredential, string) error
}

type Service struct {
	pool        *pgxpool.Pool
	keyring     *credentials.Keyring
	directory   Directory
	admin       navidrome.AdminCredential
	remoteReady bool
	remoteLimit time.Duration
	lifecycle   Lifecycle
}

// Lifecycle connects durable account-state transitions to M2's process-local
// credential cache and active-stream registry. Calls happen only after the
// corresponding database transaction commits.
type Lifecycle interface {
	RevokeUser(userID int64)
	AbortUser(userID int64)
	ReactivateUser(userID int64)
}

type Request struct {
	ID              int64      `json:"id"`
	DisplayName     string     `json:"displayName"`
	Email           string     `json:"email"`
	DesiredUsername string     `json:"desiredUsername"`
	Status          string     `json:"status"`
	RejectNote      *string    `json:"rejectNote,omitempty"`
	AdminNote       *string    `json:"adminNote,omitempty"`
	UserID          *int64     `json:"userId,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	DecidedAt       *time.Time `json:"decidedAt,omitempty"`
	DecidedBy       *int64     `json:"decidedBy,omitempty"`
}

type User struct {
	ID                 int64      `json:"id"`
	Username           string     `json:"username"`
	DisplayName        string     `json:"displayName"`
	Email              *string    `json:"email,omitempty"`
	Role               string     `json:"role"`
	Status             string     `json:"status"`
	ProvisioningState  string     `json:"provisioningState"`
	ProvisioningFailed *string    `json:"provisioningFailedStep,omitempty"`
	MustChangePassword bool       `json:"mustChangePassword"`
	CreatedAt          time.Time  `json:"createdAt"`
	LastActiveAt       *time.Time `json:"lastActiveAt,omitempty"`
	NavidromeUserID    *string    `json:"-"`
	NavidromeCleanup   string     `json:"navidromeCleanupState"`
}

type NewMember struct {
	Username          string
	DisplayName       string
	Email             string
	TemporaryPassword string
}

type UserFilter struct {
	Status            string
	Role              string
	ProvisioningState string
}

func New(pool *pgxpool.Pool, keyring *credentials.Keyring, directory Directory, admin navidrome.AdminCredential, remoteReady bool, remoteLimit time.Duration) *Service {
	return &Service{pool: pool, keyring: keyring, directory: directory, admin: admin, remoteReady: remoteReady, remoteLimit: remoteLimit}
}

// SetLifecycle is called during process composition before requests are served.
func (s *Service) SetLifecycle(lifecycle Lifecycle) {
	s.lifecycle = lifecycle
}

// LoadNavidromeCredential supplies the credential cache with the requesting
// Flow user's own durable Navidrome identity. It deliberately has no
// administrator fallback: a member whose credential is incomplete or whose
// account is inactive cannot borrow the platform administrator credential.
func (s *Service) LoadNavidromeCredential(ctx context.Context, userID int64) (navidrome.PasswordCredential, error) {
	if !s.remoteReady {
		return navidrome.PasswordCredential{}, ErrNavidromeUnavailable
	}
	var username string
	var ciphertext []byte
	var keyVersion int
	err := s.pool.QueryRow(ctx, `
		SELECT navidrome_username, navidrome_password_enc, navidrome_cred_key_version
		FROM users
		WHERE id=$1 AND status='active' AND provisioning_state='complete'
		  AND navidrome_username IS NOT NULL
		  AND navidrome_password_enc IS NOT NULL
		  AND navidrome_cred_key_version IS NOT NULL`, userID).
		Scan(&username, &ciphertext, &keyVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return navidrome.PasswordCredential{}, navidrome.ErrCredentialUnavailable
	}
	if err != nil {
		return navidrome.PasswordCredential{}, fmt.Errorf("load encrypted Navidrome credential: %w", err)
	}
	plaintext, err := s.keyring.Decrypt(username, credentialPurpose, credentials.Envelope{KeyVersion: keyVersion, Ciphertext: ciphertext})
	if err != nil {
		return navidrome.PasswordCredential{}, fmt.Errorf("decrypt Navidrome credential: %w", err)
	}
	if username == "" || len(plaintext) == 0 {
		return navidrome.PasswordCredential{}, navidrome.ErrCredentialUnavailable
	}
	return navidrome.PasswordCredential{Username: username, Password: string(plaintext)}, nil
}

func (s *Service) RegisterRequest(ctx context.Context, displayName, email, desiredUsername string) (int64, error) {
	displayName, email, desiredUsername, err := validateIdentity(displayName, email, desiredUsername)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.pool.QueryRow(ctx, `
		INSERT INTO membership_requests(display_name, email, desired_username, status)
		VALUES ($1, $2, $3, 'pending') RETURNING id`, displayName, email, desiredUsername).Scan(&id)
	if isUniqueViolation(err) {
		return 0, ErrConflict
	}
	if err != nil {
		return 0, fmt.Errorf("create membership request: %w", err)
	}
	return id, nil
}

func (s *Service) ListRequests(ctx context.Context, status string) ([]Request, error) {
	status = strings.TrimSpace(status)
	if status == "" {
		status = "pending"
	}
	if status != "pending" && status != "approved" && status != "rejected" {
		return nil, ErrInvalidInput
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, display_name, email, desired_username, status, reject_note, admin_note,
		       user_id, created_at, decided_at, decided_by
		FROM membership_requests WHERE status=$1 ORDER BY created_at, id`, status)
	if err != nil {
		return nil, fmt.Errorf("list membership requests: %w", err)
	}
	defer rows.Close()
	requests := make([]Request, 0)
	for rows.Next() {
		var item Request
		if err := rows.Scan(&item.ID, &item.DisplayName, &item.Email, &item.DesiredUsername, &item.Status,
			&item.RejectNote, &item.AdminNote, &item.UserID, &item.CreatedAt, &item.DecidedAt, &item.DecidedBy); err != nil {
			return nil, err
		}
		requests = append(requests, item)
	}
	return requests, rows.Err()
}

func (s *Service) ApproveRequest(ctx context.Context, requestID, actorID int64, temporaryPassword, note, correlationID string) (User, error) {
	passwordHash, err := hashTemporaryPassword(temporaryPassword)
	if err != nil {
		return User{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, fmt.Errorf("begin membership approval: %w", err)
	}
	defer tx.Rollback(ctx)
	var input NewMember
	var status string
	if err := tx.QueryRow(ctx, `
		SELECT display_name, email, desired_username, status
		FROM membership_requests WHERE id=$1 FOR UPDATE`, requestID).
		Scan(&input.DisplayName, &input.Email, &input.Username, &status); errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	} else if err != nil {
		return User{}, fmt.Errorf("read membership request: %w", err)
	}
	if status != "pending" {
		return User{}, ErrConflict
	}
	input.DisplayName, input.Email, input.Username, err = validateIdentity(input.DisplayName, input.Email, input.Username)
	if err != nil {
		return User{}, err
	}
	user, err := insertMember(ctx, tx, input, passwordHash)
	if err != nil {
		return User{}, err
	}
	note = nullableText(note)
	if _, err := tx.Exec(ctx, `
		UPDATE membership_requests SET status='approved', admin_note=NULLIF($1, ''), user_id=$2,
		       decided_at=now(), decided_by=$3 WHERE id=$4`, note, user.ID, actorID, requestID); err != nil {
		return User{}, fmt.Errorf("mark membership request approved: %w", err)
	}
	if err := audit(ctx, tx, correlationID, actorID, "request.approved", fmt.Sprintf("membership-request:%d", requestID), "ok", map[string]any{"userId": user.ID}); err != nil {
		return User{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, fmt.Errorf("commit membership approval: %w", err)
	}
	if err := s.Provision(ctx, user.ID, actorID, correlationID); err != nil {
		if current, readErr := s.GetUser(ctx, user.ID); readErr == nil {
			user = current
		}
		return user, err
	}
	return s.GetUser(ctx, user.ID)
}

func (s *Service) RejectRequest(ctx context.Context, requestID, actorID int64, note, correlationID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	command, err := tx.Exec(ctx, `
		UPDATE membership_requests SET status='rejected', reject_note=NULLIF($1, ''),
		       decided_at=now(), decided_by=$2
		WHERE id=$3 AND status='pending'`, nullableText(note), actorID, requestID)
	if err != nil {
		return fmt.Errorf("reject membership request: %w", err)
	}
	if command.RowsAffected() == 0 {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM membership_requests WHERE id=$1)`, requestID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		return ErrConflict
	}
	if err := audit(ctx, tx, correlationID, actorID, "request.rejected", fmt.Sprintf("membership-request:%d", requestID), "ok", nil); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) CreateMember(ctx context.Context, actorID int64, input NewMember, correlationID string) (User, error) {
	var err error
	input.DisplayName, input.Email, input.Username, err = validateMemberIdentity(input.DisplayName, input.Email, input.Username, false)
	if err != nil {
		return User{}, err
	}
	passwordHash, err := hashTemporaryPassword(input.TemporaryPassword)
	if err != nil {
		return User{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback(ctx)
	user, err := insertMember(ctx, tx, input, passwordHash)
	if err != nil {
		return User{}, err
	}
	if err := audit(ctx, tx, correlationID, actorID, "user.created", fmt.Sprintf("user:%d", user.ID), "ok", nil); err != nil {
		return User{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, err
	}
	if err := s.Provision(ctx, user.ID, actorID, correlationID); err != nil {
		if current, readErr := s.GetUser(ctx, user.ID); readErr == nil {
			user = current
		}
		return user, err
	}
	return s.GetUser(ctx, user.ID)
}

func (s *Service) Provision(ctx context.Context, userID, actorID int64, correlationID string) error {
	connection, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire provisioning connection: %w", err)
	}
	defer connection.Release()
	var locked bool
	if err := connection.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended('flow:membership:' || $1::bigint::text, 0))`, userID).Scan(&locked); err != nil {
		return fmt.Errorf("lock provisioning: %w", err)
	}
	if !locked {
		return ErrProvisioningBusy
	}
	defer connection.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtextextended('flow:membership:' || $1::bigint::text, 0))`, userID)

	user, ciphertext, keyVersion, err := loadProvisioningUser(ctx, connection, userID)
	if err != nil {
		return err
	}
	if user.Role != "member" || user.Status == "deleted" {
		return ErrProvisioningState
	}
	state := user.ProvisioningState
	if state == "complete" {
		return nil
	}
	if !s.remoteReady {
		return ErrNavidromeUnavailable
	}
	if state == "failed" {
		if user.ProvisioningFailed == nil {
			return ErrProvisioningState
		}
		state = *user.ProvisioningFailed
	}

	if state == "not_started" {
		rawCredential := make([]byte, 32)
		if _, err := rand.Read(rawCredential); err != nil {
			return fmt.Errorf("generate member credential: %w", err)
		}
		password := base64.RawURLEncoding.EncodeToString(rawCredential)
		envelope, err := s.keyring.Encrypt(user.Username, credentialPurpose, []byte(password))
		if err != nil {
			return err
		}
		if err := inTransaction(ctx, connection, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `
				UPDATE users SET navidrome_username=username, navidrome_password_enc=$1, navidrome_cred_key_version=$2,
				       provisioning_state='credential_ready', provisioning_failed_step=NULL, updated_at=now()
				WHERE id=$3 AND provisioning_state IN ('not_started', 'failed')`, envelope.Ciphertext, envelope.KeyVersion, userID); err != nil {
				return fmt.Errorf("persist durable member credential: %w", err)
			}
			return audit(ctx, tx, correlationID, actorID, "provisioning.credential_ready", fmt.Sprintf("user:%d", userID), "in-progress", nil)
		}); err != nil {
			return err
		}
		ciphertext, keyVersion, state = envelope.Ciphertext, envelope.KeyVersion, "credential_ready"
	}

	if state == "credential_ready" || state == "creating_nd_user" {
		if err := inTransaction(ctx, connection, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE users SET provisioning_state='creating_nd_user', provisioning_failed_step=NULL, updated_at=now() WHERE id=$1`, userID); err != nil {
				return err
			}
			return audit(ctx, tx, correlationID, actorID, "provisioning.creating_nd_user", fmt.Sprintf("user:%d", userID), "in-progress", nil)
		}); err != nil {
			return err
		}
		plaintext, err := s.keyring.Decrypt(user.Username, credentialPurpose, credentials.Envelope{KeyVersion: keyVersion, Ciphertext: ciphertext})
		if err != nil {
			return s.failProvisioning(ctx, connection, userID, actorID, "creating_nd_user", correlationID, err)
		}
		remoteContext, cancel := context.WithTimeout(ctx, s.remoteLimit)
		navidromeID, remoteErr := s.directory.EnsureMemberIdentity(remoteContext, s.admin, navidrome.MemberInput{
			Username: user.Username,
			Name:     user.DisplayName,
			Email:    stringValue(user.Email),
			Password: string(plaintext),
		})
		cancel()
		if remoteErr != nil {
			return s.failProvisioning(ctx, connection, userID, actorID, "creating_nd_user", correlationID, remoteErr)
		}
		if err := inTransaction(ctx, connection, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `
				UPDATE users SET navidrome_user_id=$1, provisioning_state='linking',
				       provisioning_failed_step=NULL, updated_at=now() WHERE id=$2`, navidromeID, userID); err != nil {
				return fmt.Errorf("persist Navidrome member link: %w", err)
			}
			return audit(ctx, tx, correlationID, actorID, "provisioning.creating_nd_user", fmt.Sprintf("user:%d", userID), "ok", map[string]any{"navidromeUserId": navidromeID})
		}); err != nil {
			return err
		}
		user.NavidromeUserID = &navidromeID
		state = "linking"
	}

	if state == "linking" {
		if user.NavidromeUserID == nil || *user.NavidromeUserID == "" {
			return s.failProvisioning(ctx, connection, userID, actorID, "creating_nd_user", correlationID, errors.New("missing Navidrome user id"))
		}
		if err := audit(ctx, connection, correlationID, actorID, "provisioning.linking", fmt.Sprintf("user:%d", userID), "in-progress", nil); err != nil {
			return err
		}
		remoteContext, cancel := context.WithTimeout(ctx, s.remoteLimit)
		remoteErr := s.directory.AssignAllLibraries(remoteContext, s.admin, *user.NavidromeUserID)
		cancel()
		if remoteErr != nil {
			return s.failProvisioning(ctx, connection, userID, actorID, "linking", correlationID, remoteErr)
		}
		if err := inTransaction(ctx, connection, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `
				UPDATE users SET provisioning_state='complete', provisioning_failed_step=NULL, updated_at=now()
				WHERE id=$1`, userID); err != nil {
				return fmt.Errorf("complete member provisioning: %w", err)
			}
			return audit(ctx, tx, correlationID, actorID, "provisioning.complete", fmt.Sprintf("user:%d", userID), "ok", nil)
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) failProvisioning(ctx context.Context, connection *pgxpool.Conn, userID, actorID int64, step, correlationID string, cause error) error {
	if err := inTransaction(ctx, connection, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE users SET provisioning_state='failed', provisioning_failed_step=$1, updated_at=now()
			WHERE id=$2`, step, userID); err != nil {
			return fmt.Errorf("persist provisioning failure: %w", err)
		}
		return audit(ctx, tx, correlationID, actorID, "provisioning.failed", fmt.Sprintf("user:%d", userID), "error", map[string]any{"step": step})
	}); err != nil {
		return errors.Join(cause, err)
	}
	return fmt.Errorf("provisioning %s: %w", step, cause)
}

func inTransaction(ctx context.Context, connection *pgxpool.Conn, operation func(pgx.Tx) error) error {
	tx, err := connection.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := operation(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) ListUsers(ctx context.Context, filter UserFilter) ([]User, error) {
	if !validOptional(filter.Status, "active", "suspended", "deleted") ||
		!validOptional(filter.Role, "admin", "member") ||
		!validOptional(filter.ProvisioningState, "not_started", "credential_ready", "creating_nd_user", "linking", "complete", "failed") {
		return nil, ErrInvalidInput
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, username, display_name, email, role, status, provisioning_state,
		       provisioning_failed_step, must_change_password, created_at, last_active_at,
		       navidrome_user_id, navidrome_cleanup_state
		FROM users
		WHERE ($1='' OR status=$1) AND ($2='' OR role=$2) AND ($3='' OR provisioning_state=$3)
		ORDER BY created_at, id`, filter.Status, filter.Role, filter.ProvisioningState)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := make([]User, 0)
	for rows.Next() {
		user, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	return users, rows.Err()
}

func (s *Service) GetUser(ctx context.Context, userID int64) (User, error) {
	user, err := scanUser(s.pool.QueryRow(ctx, `
		SELECT id, username, display_name, email, role, status, provisioning_state,
		       provisioning_failed_step, must_change_password, created_at, last_active_at,
		       navidrome_user_id, navidrome_cleanup_state
		FROM users WHERE id=$1`, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return user, err
}

func (s *Service) Suspend(ctx context.Context, userID, actorID int64, reason, correlationID string) error {
	return s.setStatus(ctx, userID, actorID, "suspended", "user.suspended", reason, correlationID)
}

func (s *Service) Reactivate(ctx context.Context, userID, actorID int64, correlationID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	command, err := tx.Exec(ctx, `
		UPDATE users SET status='active', updated_at=now()
		WHERE id=$1 AND role='member' AND status='suspended' AND provisioning_state='complete'`, userID)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		var status, state, role string
		if err := tx.QueryRow(ctx, `SELECT status, provisioning_state, role FROM users WHERE id=$1`, userID).Scan(&status, &state, &role); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		return ErrConflict
	}
	if err := audit(ctx, tx, correlationID, actorID, "user.reactivated", fmt.Sprintf("user:%d", userID), "ok", nil); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if s.lifecycle != nil {
		s.lifecycle.ReactivateUser(userID)
	}
	return nil
}

func (s *Service) ResetPassword(ctx context.Context, userID, actorID int64, temporaryPassword, correlationID string) error {
	passwordHash, err := hashTemporaryPassword(temporaryPassword)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	command, err := tx.Exec(ctx, `
		UPDATE users SET password_hash=$1, must_change_password=true, updated_at=now()
		WHERE id=$2 AND role='member' AND status<>'deleted'`, passwordHash, userID)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1`, userID); err != nil {
		return err
	}
	if err := audit(ctx, tx, correlationID, actorID, "user.password_reset", fmt.Sprintf("user:%d", userID), "ok", nil); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if s.lifecycle != nil {
		s.lifecycle.AbortUser(userID)
	}
	return nil
}

func (s *Service) Delete(ctx context.Context, userID, actorID int64, hard bool, correlationID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var role string
	var navidromeUsername *string
	var navidromeID *string
	var ciphertext []byte
	var keyVersion *int
	var existingCleanup string
	if err := tx.QueryRow(ctx, `
		SELECT role, navidrome_username, navidrome_user_id, navidrome_password_enc,
		       navidrome_cred_key_version, navidrome_cleanup_state
		FROM users WHERE id=$1 FOR UPDATE`, userID).
		Scan(&role, &navidromeUsername, &navidromeID, &ciphertext, &keyVersion, &existingCleanup); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if role != "member" {
		return ErrConflict
	}
	cleanupState := existingCleanup
	if hard {
		if (navidromeID == nil || *navidromeID == "") && len(ciphertext) == 0 {
			cleanupState = "complete"
		} else {
			cleanupState = "pending"
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE users SET status='deleted', navidrome_cleanup_state=$2, updated_at=now()
		WHERE id=$1`, userID, cleanupState); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1`, userID); err != nil {
		return err
	}
	if err := audit(ctx, tx, correlationID, actorID, "user.deleted", fmt.Sprintf("user:%d", userID), "ok", map[string]any{"hard": hard}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if s.lifecycle != nil {
		s.lifecycle.RevokeUser(userID)
	}
	if !hard || cleanupState == "complete" {
		return nil
	}
	if !s.remoteReady {
		if err := audit(ctx, s.pool, correlationID, actorID, "user.delete_compensation", fmt.Sprintf("user:%d", userID), "in-progress", map[string]any{"pending": true}); err != nil {
			return errors.Join(ErrCompensationPending, err)
		}
		return ErrCompensationPending
	}
	remoteUserID := ""
	if navidromeID != nil {
		remoteUserID = *navidromeID
	}
	if remoteUserID == "" {
		if navidromeUsername == nil || *navidromeUsername == "" || keyVersion == nil || len(ciphertext) == 0 {
			return s.failCleanup(ctx, userID, actorID, correlationID, errors.New("durable Navidrome identity is incomplete"))
		}
		plaintext, err := s.keyring.Decrypt(*navidromeUsername, credentialPurpose, credentials.Envelope{KeyVersion: *keyVersion, Ciphertext: ciphertext})
		if err != nil {
			return s.failCleanup(ctx, userID, actorID, correlationID, err)
		}
		remoteContext, cancel := context.WithTimeout(ctx, s.remoteLimit)
		resolvedID, exists, resolveErr := s.directory.ResolveMemberIdentity(remoteContext, s.admin, *navidromeUsername, string(plaintext))
		cancel()
		if resolveErr != nil {
			return s.failCleanup(ctx, userID, actorID, correlationID, resolveErr)
		}
		if !exists {
			return s.completeCleanup(ctx, userID, actorID, correlationID)
		}
		remoteUserID = resolvedID
	}
	remoteContext, cancel := context.WithTimeout(ctx, s.remoteLimit)
	remoteErr := s.directory.DeleteMember(remoteContext, s.admin, remoteUserID)
	cancel()
	if remoteErr != nil {
		return s.failCleanup(ctx, userID, actorID, correlationID, remoteErr)
	}
	return s.completeCleanup(ctx, userID, actorID, correlationID)
}

func (s *Service) failCleanup(ctx context.Context, userID, actorID int64, correlationID string, cause error) error {
	failureTx, err := s.pool.Begin(ctx)
	if err == nil {
		if _, err = failureTx.Exec(ctx, `UPDATE users SET navidrome_cleanup_state='failed', updated_at=now() WHERE id=$1`, userID); err == nil {
			err = audit(ctx, failureTx, correlationID, actorID, "user.delete_compensation", fmt.Sprintf("user:%d", userID), "error", map[string]any{"pending": true})
		}
		if err == nil {
			err = failureTx.Commit(ctx)
		} else {
			_ = failureTx.Rollback(ctx)
		}
	}
	compensationErr := fmt.Errorf("%w: %v", ErrCompensationPending, cause)
	if err != nil {
		return errors.Join(compensationErr, fmt.Errorf("persist cleanup failure: %w", err))
	}
	return compensationErr
}

func (s *Service) completeCleanup(ctx context.Context, userID, actorID int64, correlationID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE users SET navidrome_cleanup_state='complete', updated_at=now() WHERE id=$1`, userID); err != nil {
		return err
	}
	if err := audit(ctx, tx, correlationID, actorID, "user.delete_compensation", fmt.Sprintf("user:%d", userID), "ok", nil); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) setStatus(ctx context.Context, userID, actorID int64, status, action, reason, correlationID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	command, err := tx.Exec(ctx, `UPDATE users SET status=$1, updated_at=now() WHERE id=$2 AND role='member' AND status<>'deleted'`, status, userID)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1`, userID); err != nil {
		return err
	}
	detail := map[string]any{}
	if trimmed := nullableText(reason); trimmed != "" {
		detail["reason"] = trimmed
	}
	if err := audit(ctx, tx, correlationID, actorID, action, fmt.Sprintf("user:%d", userID), "ok", detail); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if s.lifecycle != nil {
		s.lifecycle.RevokeUser(userID)
	}
	return nil
}

func insertMember(ctx context.Context, tx pgx.Tx, input NewMember, passwordHash string) (User, error) {
	var user User
	err := tx.QueryRow(ctx, `
		INSERT INTO users(username, display_name, email, password_hash, must_change_password,
		                  role, status, provisioning_state, is_navidrome_admin)
		VALUES ($1, $2, NULLIF($3, ''), $4, true, 'member', 'active', 'not_started', false)
		RETURNING id, username, display_name, email, role, status, provisioning_state,
		          provisioning_failed_step, must_change_password, created_at, last_active_at,
		          navidrome_user_id, navidrome_cleanup_state`,
		input.Username, input.DisplayName, input.Email, passwordHash).Scan(
		&user.ID, &user.Username, &user.DisplayName, &user.Email, &user.Role, &user.Status,
		&user.ProvisioningState, &user.ProvisioningFailed, &user.MustChangePassword,
		&user.CreatedAt, &user.LastActiveAt, &user.NavidromeUserID, &user.NavidromeCleanup)
	if isUniqueViolation(err) {
		return User{}, ErrConflict
	}
	if err != nil {
		return User{}, fmt.Errorf("create member: %w", err)
	}
	return user, nil
}

func loadProvisioningUser(ctx context.Context, connection *pgxpool.Conn, userID int64) (User, []byte, int, error) {
	var user User
	var ciphertext []byte
	var keyVersion *int
	err := connection.QueryRow(ctx, `
		SELECT id, username, display_name, email, role, status, provisioning_state,
		       provisioning_failed_step, navidrome_user_id, navidrome_password_enc, navidrome_cred_key_version
		FROM users WHERE id=$1`, userID).Scan(&user.ID, &user.Username, &user.DisplayName, &user.Email,
		&user.Role, &user.Status, &user.ProvisioningState, &user.ProvisioningFailed,
		&user.NavidromeUserID, &ciphertext, &keyVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, nil, 0, ErrNotFound
	}
	if err != nil {
		return User{}, nil, 0, err
	}
	version := 0
	if keyVersion != nil {
		version = *keyVersion
	}
	return user, ciphertext, version, nil
}

type rowScanner interface {
	Scan(...any) error
}

func scanUser(row rowScanner) (User, error) {
	var user User
	err := row.Scan(&user.ID, &user.Username, &user.DisplayName, &user.Email, &user.Role, &user.Status,
		&user.ProvisioningState, &user.ProvisioningFailed, &user.MustChangePassword,
		&user.CreatedAt, &user.LastActiveAt, &user.NavidromeUserID, &user.NavidromeCleanup)
	return user, err
}

type databaseExecutor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func audit(ctx context.Context, executor databaseExecutor, correlationID string, actorID int64, action, entity, outcome string, detail map[string]any) error {
	if detail == nil {
		detail = map[string]any{}
	}
	payload, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	var actor any
	if actorID != 0 {
		actor = actorID
	}
	_, err = executor.Exec(ctx, `
		INSERT INTO audit_log(correlation_id, actor_user_id, action, entity, outcome, detail)
		VALUES ($1, $2, $3, $4, $5, $6)`, correlationID, actor, action, entity, outcome, payload)
	if err != nil {
		return fmt.Errorf("write audit entry: %w", err)
	}
	return nil
}

func validateIdentity(displayName, email, username string) (string, string, string, error) {
	return validateMemberIdentity(displayName, email, username, true)
}

func validateMemberIdentity(displayName, email, username string, emailRequired bool) (string, string, string, error) {
	displayName = strings.TrimSpace(displayName)
	email = strings.ToLower(strings.TrimSpace(email))
	username = strings.TrimSpace(username)
	if len([]rune(displayName)) < 1 || len([]rune(displayName)) > 100 {
		return "", "", "", fmt.Errorf("%w: displayName must be between 1 and 100 characters", ErrInvalidInput)
	}
	if email != "" {
		address, err := mail.ParseAddress(email)
		if err != nil || !strings.EqualFold(address.Address, email) || len(email) > 254 {
			return "", "", "", fmt.Errorf("%w: email is invalid", ErrInvalidInput)
		}
	} else if emailRequired {
		return "", "", "", fmt.Errorf("%w: email is required", ErrInvalidInput)
	}
	if !usernamePattern.MatchString(username) {
		return "", "", "", fmt.Errorf("%w: username must be 3-32 characters using letters, numbers, dot, underscore, or hyphen", ErrInvalidInput)
	}
	return displayName, email, username, nil
}

func hashTemporaryPassword(password string) (string, error) {
	hash, err := auth.HashPassword(password)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	return hash, nil
}

func validOptional(value string, allowed ...string) bool {
	if value == "" {
		return true
	}
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func nullableText(value string) string {
	value = strings.TrimSpace(value)
	if runes := []rune(value); len(runes) > 1000 {
		value = string(runes[:1000])
	}
	return value
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func isUniqueViolation(err error) bool {
	var databaseError *pgconn.PgError
	return errors.As(err, &databaseError) && databaseError.Code == "23505"
}
