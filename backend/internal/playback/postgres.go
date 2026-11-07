package playback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ifrunruhin12/flow/backend/internal/auth"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresStore struct{ pool *pgxpool.Pool }

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

const columns = `queue, queue_index, current_media_id, position_ms, volume,
                 shuffle, playback_status, revision, updated_at`

func (s *PostgresStore) Load(ctx context.Context, principal auth.Principal) (Session, error) {
	tx, err := s.authorizedTransaction(ctx, principal)
	if err != nil {
		return Session{}, err
	}
	defer tx.Rollback(ctx)
	session, err := readSession(ctx, tx, principal.User.ID)
	if err != nil {
		return Session{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Session{}, fmt.Errorf("commit playback read: %w", err)
	}
	return session, nil
}

func (s *PostgresStore) CompareAndSwap(ctx context.Context, principal auth.Principal, revision int64, next Session) (Session, error) {
	return s.compareAndSwap(ctx, principal, revision, next, "")
}

func (s *PostgresStore) CompareAndSwapInMode(ctx context.Context, principal auth.Principal, revision int64, next Session, requiredMode string) (Session, error) {
	if requiredMode != "individual" {
		return Session{}, ErrInvalidInput
	}
	return s.compareAndSwap(ctx, principal, revision, next, requiredMode)
}

func (s *PostgresStore) compareAndSwap(ctx context.Context, principal auth.Principal, revision int64, next Session, requiredMode string) (Session, error) {
	tx, err := s.authorizedTransaction(ctx, principal)
	if err != nil {
		return Session{}, err
	}
	defer tx.Rollback(ctx)
	if requiredMode != "" {
		var mode string
		if err := tx.QueryRow(ctx, `SELECT mode FROM settings WHERE id=1 FOR SHARE`).Scan(&mode); err != nil {
			return Session{}, fmt.Errorf("lock playback mode: %w", err)
		}
		if mode != requiredMode {
			return Session{}, ErrSynchronizedMode
		}
	}
	queue, err := json.Marshal(next.Queue)
	if err != nil {
		return Session{}, fmt.Errorf("encode playback queue: %w", err)
	}
	row := tx.QueryRow(ctx, `UPDATE session_state
		SET queue=$3::jsonb, queue_index=$4, current_media_id=$5, position_ms=$6,
		    volume=$7, shuffle=$8, playback_status=$9, revision=revision+1,
		    updated_at=clock_timestamp()
		WHERE user_id=$1 AND revision=$2
		RETURNING `+columns, principal.User.ID, revision, queue, next.QueueIndex,
		next.CurrentMediaID, next.PositionMS, next.Volume, next.Shuffle, next.Status)
	accepted, err := scanSession(row)
	if errors.Is(err, pgx.ErrNoRows) {
		current, readErr := readSession(ctx, tx, principal.User.ID)
		if readErr != nil {
			return Session{}, readErr
		}
		return Session{}, &ConflictError{Current: current}
	}
	if err != nil {
		return Session{}, fmt.Errorf("compare-and-swap playback: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Session{}, fmt.Errorf("commit playback mutation: %w", err)
	}
	return accepted, nil
}

// Locks follow membership's user -> session order. Lifecycle revocation either
// commits first and rejects this command, or waits for this command to commit.
// No database lock is held while calling Navidrome.
func (s *PostgresStore) authorizedTransaction(ctx context.Context, principal auth.Principal) (pgx.Tx, error) {
	if err := validPrincipal(principal); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin playback transaction: %w", err)
	}
	success := false
	defer func() {
		if !success {
			_ = tx.Rollback(ctx)
		}
	}()
	var status, provisioning string
	var mustChange bool
	err = tx.QueryRow(ctx, `SELECT status, provisioning_state, must_change_password
		FROM users WHERE id=$1 FOR SHARE`, principal.User.ID).Scan(&status, &provisioning, &mustChange)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, auth.ErrUnauthenticated
	}
	if err != nil {
		return nil, fmt.Errorf("recheck playback account: %w", err)
	}
	var exists int
	err = tx.QueryRow(ctx, `SELECT 1 FROM sessions
		WHERE user_id=$1 AND token_hash=$2 AND expires_at>now() FOR SHARE`,
		principal.User.ID, principal.TokenHash).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, auth.ErrUnauthenticated
	}
	if err != nil {
		return nil, fmt.Errorf("recheck playback session: %w", err)
	}
	if status != "active" || provisioning != "complete" {
		return nil, auth.ErrAccountNotActive
	}
	if mustChange {
		return nil, ErrPasswordChangeRequired
	}
	success = true
	return tx, nil
}

func readSession(ctx context.Context, tx pgx.Tx, userID int64) (Session, error) {
	state, err := scanSession(tx.QueryRow(ctx, `SELECT `+columns+` FROM session_state WHERE user_id=$1`, userID))
	if err != nil {
		return Session{}, fmt.Errorf("read individual session: %w", err)
	}
	return state, nil
}

func scanSession(row pgx.Row) (Session, error) {
	var result Session
	var queue []byte
	err := row.Scan(&queue, &result.QueueIndex, &result.CurrentMediaID, &result.PositionMS,
		&result.Volume, &result.Shuffle, &result.Status, &result.Revision, &result.UpdatedAt)
	if err != nil {
		return Session{}, err
	}
	if err := json.Unmarshal(queue, &result.Queue); err != nil {
		return Session{}, fmt.Errorf("decode individual queue: %w", err)
	}
	result.UpdatedAt = result.UpdatedAt.UTC()
	return result, nil
}
