package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ifrunruhin12/flow/backend/internal/auth"
	"github.com/ifrunruhin12/flow/backend/internal/credentials"
	"github.com/ifrunruhin12/flow/backend/internal/navidrome"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const credentialPurpose = "navidrome-password"

type Service struct {
	pool    *pgxpool.Pool
	keyring *credentials.Keyring
}

func New(pool *pgxpool.Pool, keyring *credentials.Keyring) *Service {
	return &Service{pool: pool, keyring: keyring}
}

func (s *Service) PrepareKeyVersions(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT navidrome_cred_key_version FROM users WHERE navidrome_cred_key_version IS NOT NULL`)
	if err != nil && !isUndefinedTable(err) {
		return fmt.Errorf("read credential key references: %w", err)
	}
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var version int
			if err := rows.Scan(&version); err != nil {
				return err
			}
			if !s.keyring.HasVersion(version) {
				return fmt.Errorf("credential keyring is missing database-referenced version %d", version)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin key metadata sync: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE credential_key_versions SET state='decrypt_only' WHERE state='active'`); err != nil {
		return fmt.Errorf("clear active credential key metadata: %w", err)
	}
	for _, version := range s.keyring.Versions() {
		state := "decrypt_only"
		if version == s.keyring.ActiveVersion() {
			state = "active"
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO credential_key_versions(version, state)
			VALUES ($1, $2)
			ON CONFLICT (version) DO UPDATE SET state=EXCLUDED.state, retired_at=NULL`, version, state); err != nil {
			return fmt.Errorf("sync credential key version %d: %w", version, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit key metadata sync: %w", err)
	}
	return nil
}

func (s *Service) EnsureAdmin(ctx context.Context, flowUsername, flowPassword, navidromePassword string, verification navidrome.Verification, correlationID string) (int64, error) {
	flowUsername = strings.TrimSpace(flowUsername)
	if flowUsername == "" {
		return 0, errors.New("Flow admin username is required")
	}
	if err := auth.ValidatePassword(flowPassword); err != nil {
		return 0, fmt.Errorf("Flow admin password: %w", err)
	}
	envelope, err := s.keyring.Encrypt(verification.Username, credentialPurpose, []byte(navidromePassword))
	if err != nil {
		return 0, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin admin bootstrap: %w", err)
	}
	defer tx.Rollback(ctx)

	var id int64
	var existingUsername string
	var existingHash string
	err = tx.QueryRow(ctx, `SELECT id, username, password_hash FROM users WHERE role='admin' ORDER BY id LIMIT 1 FOR UPDATE`).Scan(&id, &existingUsername, &existingHash)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		passwordHash, err := auth.HashPassword(flowPassword)
		if err != nil {
			return 0, err
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO users(
				username, display_name, password_hash, must_change_password, role, status,
				provisioning_state, navidrome_username, navidrome_user_id, navidrome_password_enc,
				navidrome_cred_key_version, is_navidrome_admin
			) VALUES ($1, $1, $2, false, 'admin', 'active', 'complete', $3, $4, $5, $6, true)
			RETURNING id`, flowUsername, passwordHash, verification.Username, verification.UserID, envelope.Ciphertext, envelope.KeyVersion).Scan(&id)
		if err != nil {
			return 0, fmt.Errorf("insert Flow admin: %w", err)
		}
	case err != nil:
		return 0, fmt.Errorf("read Flow admin: %w", err)
	default:
		passwordHash := existingHash
		passwordMatches, verifyErr := auth.VerifyPassword(existingHash, flowPassword)
		if verifyErr != nil || !passwordMatches {
			passwordHash, err = auth.HashPassword(flowPassword)
			if err != nil {
				return 0, err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1`, id); err != nil {
				return 0, fmt.Errorf("revoke admin sessions after password rotation: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE users SET username=$1, display_name=$1, password_hash=$2,
				status='active', provisioning_state='complete', provisioning_failed_step=NULL,
				navidrome_username=$3, navidrome_user_id=$4, navidrome_password_enc=$5,
				navidrome_cred_key_version=$6, is_navidrome_admin=true, updated_at=now()
			WHERE id=$7`, flowUsername, passwordHash, verification.Username, verification.UserID, envelope.Ciphertext, envelope.KeyVersion, id); err != nil {
			return 0, fmt.Errorf("update Flow admin: %w", err)
		}
		_ = existingUsername
	}
	detail, _ := json.Marshal(map[string]any{"navidromeUserId": verification.UserID})
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_log(correlation_id, actor_user_id, action, entity, outcome, detail)
		VALUES ($1, $2, 'admin.bootstrap', $3, 'ok', $4)`, correlationID, id, fmt.Sprintf("user:%d", id), detail); err != nil {
		return 0, fmt.Errorf("audit admin bootstrap: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit admin bootstrap: %w", err)
	}
	return id, nil
}

func (s *Service) ReencryptCredentials(ctx context.Context) error {
	total := 0
	for {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin credential re-encryption: %w", err)
		}
		rows, err := tx.Query(ctx, `
			SELECT id, navidrome_username, navidrome_password_enc, navidrome_cred_key_version
			FROM users
			WHERE navidrome_password_enc IS NOT NULL AND navidrome_cred_key_version<>$1
			ORDER BY id LIMIT 100 FOR UPDATE`, s.keyring.ActiveVersion())
		if err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("select credentials for re-encryption: %w", err)
		}
		type item struct {
			id         int64
			username   string
			ciphertext []byte
			version    int
		}
		var items []item
		for rows.Next() {
			var current item
			if err := rows.Scan(&current.id, &current.username, &current.ciphertext, &current.version); err != nil {
				rows.Close()
				tx.Rollback(ctx)
				return err
			}
			items = append(items, current)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			tx.Rollback(ctx)
			return err
		}
		rows.Close()
		for _, current := range items {
			plaintext, err := s.keyring.Decrypt(current.username, credentialPurpose, credentials.Envelope{KeyVersion: current.version, Ciphertext: current.ciphertext})
			if err != nil {
				tx.Rollback(ctx)
				return fmt.Errorf("decrypt credential for user %d: %w", current.id, err)
			}
			replacement, err := s.keyring.Encrypt(current.username, credentialPurpose, plaintext)
			if err != nil {
				tx.Rollback(ctx)
				return err
			}
			command, err := tx.Exec(ctx, `
				UPDATE users SET navidrome_password_enc=$1, navidrome_cred_key_version=$2, updated_at=now()
				WHERE id=$3 AND navidrome_cred_key_version=$4`, replacement.Ciphertext, replacement.KeyVersion, current.id, current.version)
			if err != nil {
				tx.Rollback(ctx)
				return fmt.Errorf("update re-encrypted credential for user %d: %w", current.id, err)
			}
			if command.RowsAffected() != 1 {
				tx.Rollback(ctx)
				return fmt.Errorf("credential version changed while re-encrypting user %d", current.id)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit credential re-encryption: %w", err)
		}
		if len(items) == 0 {
			break
		}
		total += len(items)
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE credential_key_versions k SET reencrypted_at=now()
		WHERE state='decrypt_only' AND NOT EXISTS (
			SELECT 1 FROM users u WHERE u.navidrome_cred_key_version=k.version
		)`)
	if err != nil {
		return fmt.Errorf("mark credential versions re-encrypted: %w", err)
	}
	if total > 0 {
		detail, _ := json.Marshal(map[string]any{"count": total, "activeKeyVersion": s.keyring.ActiveVersion()})
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO audit_log(correlation_id, action, entity, outcome, detail)
			VALUES ($1, 'credential.reencrypted', 'credentials', 'ok', $2)`, CorrelationID(), detail); err != nil {
			return fmt.Errorf("audit credential re-encryption: %w", err)
		}
	}
	return nil
}

func (s *Service) DecryptUserCredential(ctx context.Context, userID int64) (string, string, error) {
	var navidromeUsername string
	var ciphertext []byte
	var version int
	if err := s.pool.QueryRow(ctx, `SELECT navidrome_username, navidrome_password_enc, navidrome_cred_key_version FROM users WHERE id=$1`, userID).Scan(&navidromeUsername, &ciphertext, &version); err != nil {
		return "", "", err
	}
	plaintext, err := s.keyring.Decrypt(navidromeUsername, credentialPurpose, credentials.Envelope{KeyVersion: version, Ciphertext: ciphertext})
	if err != nil {
		return "", "", err
	}
	return navidromeUsername, string(plaintext), nil
}

func (s *Service) DecryptAdminCredential(ctx context.Context) (string, string, error) {
	var id int64
	if err := s.pool.QueryRow(ctx, `SELECT id FROM users WHERE role='admin' ORDER BY id LIMIT 1`).Scan(&id); err != nil {
		return "", "", err
	}
	return s.DecryptUserCredential(ctx, id)
}

func isUndefinedTable(err error) bool {
	type sqlState interface{ SQLState() string }
	var state sqlState
	return errors.As(err, &state) && state.SQLState() == "42P01"
}

func CorrelationID() string {
	return fmt.Sprintf("bootstrap:%d", time.Now().UTC().UnixNano())
}
