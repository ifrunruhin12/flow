package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ifrunruhin12/flow/backend/internal/auth"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresStore struct{ pool *pgxpool.Pool }

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

const sessionColumns = `id, version, event_seq, state, dj_user_id,
    dj_lease_generation, dj_lease_token_hash, dj_connection_id::text,
    dj_connected, dj_disconnected_at, dj_grace_deadline, current_media_id, position_ms, ref_ts,
    queue, queue_index, created_at, updated_at, ended_at, end_reason`

func (s *PostgresStore) Load(ctx context.Context) (Snapshot, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Snapshot{}, fmt.Errorf("begin synchronized snapshot: %w", err)
	}
	defer tx.Rollback(ctx)
	var snapshot Snapshot
	if err := tx.QueryRow(ctx, `SELECT mode FROM settings WHERE id=1`).Scan(&snapshot.Mode); err != nil {
		return Snapshot{}, fmt.Errorf("read platform mode: %w", err)
	}
	session, err := scanSession(tx.QueryRow(ctx, `SELECT `+sessionColumns+`
		FROM shared_sessions WHERE state <> 'ended' ORDER BY id DESC LIMIT 1`))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, fmt.Errorf("read active shared session: %w", err)
	}
	if err == nil {
		snapshot.Session = &session
		snapshot.ActiveSharedSessionID = &session.ID
	}
	if err := tx.Commit(ctx); err != nil {
		return Snapshot{}, fmt.Errorf("commit synchronized snapshot: %w", err)
	}
	return snapshot, nil
}

func (s *PostgresStore) Start(ctx context.Context, principal auth.Principal, proposed Session, individualRevision *int64) (Result, error) {
	tx, err := s.authorizedTransaction(ctx, principal)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback(ctx)
	var mode string
	if err := tx.QueryRow(ctx, `SELECT mode FROM settings WHERE id=1 FOR UPDATE`).Scan(&mode); err != nil {
		return Result{}, fmt.Errorf("lock platform mode: %w", err)
	}
	if mode != ModeIndividual {
		return Result{}, ErrModeConflict
	}
	if individualRevision != nil {
		var currentRevision int64
		if err := tx.QueryRow(ctx, `SELECT revision FROM session_state WHERE user_id=$1 FOR SHARE`, principal.User.ID).Scan(&currentRevision); err != nil {
			return Result{}, fmt.Errorf("recheck synchronized seed: %w", err)
		}
		if currentRevision != *individualRevision {
			return Result{}, ErrStateConflict
		}
	}
	queue, err := json.Marshal(proposed.Queue)
	if err != nil {
		return Result{}, fmt.Errorf("encode shared queue: %w", err)
	}
	session, err := scanSession(tx.QueryRow(ctx, `INSERT INTO shared_sessions(
		version,event_seq,state,dj_user_id,dj_lease_generation,dj_connected,
		current_media_id,position_ms,ref_ts,queue,queue_index,created_at,updated_at)
		VALUES(0,1,$1,$2,0,false,$3,$4,$5,$6::jsonb,$7,$5,$5)
		RETURNING `+sessionColumns,
		proposed.State, proposed.DJUserID, proposed.CurrentMediaID, proposed.PositionMS,
		proposed.RefTS, queue, proposed.QueueIndex))
	if isUniqueViolation(err) {
		return Result{}, ErrModeConflict
	}
	if err != nil {
		return Result{}, fmt.Errorf("create shared session: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE settings SET mode='synchronized', updated_at=$1 WHERE id=1`, proposed.RefTS); err != nil {
		return Result{}, fmt.Errorf("enter synchronized mode: %w", err)
	}
	event, err := insertEvent(ctx, tx, session, EventDraft{Type: "session-started", Payload: map[string]any{
		"mode": ModeSynchronized, "session": session,
	}}, proposed.RefTS)
	if err != nil {
		return Result{}, err
	}
	if err := insertModeAudit(ctx, tx, &principal.User.ID, correlationIDFrom(ctx, fmt.Sprintf("sync-session-%d-event-%d", session.ID, session.EventSeq)), ModeIndividual, ModeSynchronized, session.ID, "admin"); err != nil {
		return Result{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("commit shared session start: %w", err)
	}
	return result(ModeSynchronized, session, event), nil
}

func (s *PostgresStore) Commit(ctx context.Context, principal *auth.Principal, expected Expected, next Session, draft EventDraft, nextMode *string) (Result, error) {
	var tx pgx.Tx
	var err error
	if principal == nil {
		tx, err = s.pool.Begin(ctx)
	} else {
		tx, err = s.authorizedTransaction(ctx, *principal)
	}
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback(ctx)
	var mode string
	if err := tx.QueryRow(ctx, `SELECT mode FROM settings WHERE id=1 FOR UPDATE`).Scan(&mode); err != nil {
		return Result{}, fmt.Errorf("lock platform mode: %w", err)
	}
	if mode != ModeSynchronized {
		return Result{}, ErrModeConflict
	}
	current, err := scanSession(tx.QueryRow(ctx, `SELECT `+sessionColumns+` FROM shared_sessions WHERE id=$1 FOR UPDATE`, expected.SessionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, ErrNoActiveSession
	}
	if err != nil {
		return Result{}, fmt.Errorf("lock shared session: %w", err)
	}
	if current.Version != expected.Version {
		return Result{}, ErrVersionConflict
	}
	if current.EventSeq != expected.EventSeq || current.State == StateEnded {
		return Result{}, ErrStateConflict
	}
	if next.ID != current.ID || next.EventSeq != current.EventSeq+1 || next.Version < current.Version || next.Version > current.Version+1 {
		return Result{}, ErrInvalidInput
	}
	queue, err := json.Marshal(next.Queue)
	if err != nil {
		return Result{}, fmt.Errorf("encode shared queue: %w", err)
	}
	accepted, err := scanSession(tx.QueryRow(ctx, `UPDATE shared_sessions SET
		version=$4,event_seq=$5,state=$6,dj_lease_generation=$7,
		dj_lease_token_hash=$8,dj_connection_id=$9,dj_connected=$10,
		dj_disconnected_at=$11,dj_grace_deadline=$12,current_media_id=$13,position_ms=$14,ref_ts=$15,
		queue=$16::jsonb,queue_index=$17,updated_at=$18,ended_at=$19,end_reason=$20
		WHERE id=$1 AND version=$2 AND event_seq=$3 AND state <> 'ended'
		RETURNING `+sessionColumns,
		expected.SessionID, expected.Version, expected.EventSeq, next.Version, next.EventSeq,
		next.State, next.DJLeaseGeneration, nullableBytes(next.DJLeaseTokenHash),
		next.DJConnectionID, next.DJConnected, next.DJDisconnectedAt, next.DJGraceDeadline, next.CurrentMediaID,
		next.PositionMS, next.RefTS, queue, next.QueueIndex, next.UpdatedAt,
		next.EndedAt, next.EndReason))
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, ErrStateConflict
	}
	if err != nil {
		return Result{}, fmt.Errorf("checkpoint shared session: %w", err)
	}
	resultMode := mode
	if nextMode != nil {
		if *nextMode != ModeIndividual && *nextMode != ModeSynchronized {
			return Result{}, ErrInvalidInput
		}
		if _, err := tx.Exec(ctx, `UPDATE settings SET mode=$1, updated_at=$2 WHERE id=1`, *nextMode, next.UpdatedAt); err != nil {
			return Result{}, fmt.Errorf("update platform mode: %w", err)
		}
		resultMode = *nextMode
	}
	event, err := insertEvent(ctx, tx, accepted, draft, next.UpdatedAt)
	if err != nil {
		return Result{}, err
	}
	if nextMode != nil && *nextMode != mode {
		var actorID *int64
		if principal != nil {
			actorID = &principal.User.ID
		}
		if err := insertModeAudit(ctx, tx, actorID, correlationIDFrom(ctx, fmt.Sprintf("sync-session-%d-event-%d", accepted.ID, accepted.EventSeq)), mode, *nextMode, accepted.ID, stringValue(accepted.EndReason)); err != nil {
			return Result{}, err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sync_events
		WHERE shared_session_id=$1 AND event_seq <= $2`, accepted.ID, accepted.EventSeq-500); err != nil {
		return Result{}, fmt.Errorf("bound synchronized event log: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("commit shared transition: %w", err)
	}
	return result(resultMode, accepted, event), nil
}

func (s *PostgresStore) Recover(ctx context.Context, now time.Time) ([]Result, error) {
	now = now.UTC()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin synchronized recovery: %w", err)
	}
	defer tx.Rollback(ctx)
	var mode string
	if err := tx.QueryRow(ctx, `SELECT mode FROM settings WHERE id=1 FOR UPDATE`).Scan(&mode); err != nil {
		return nil, fmt.Errorf("lock recovery mode: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT `+sessionColumns+` FROM shared_sessions WHERE state <> 'ended' ORDER BY id FOR UPDATE`)
	if err != nil {
		return nil, fmt.Errorf("scan recoverable shared sessions: %w", err)
	}
	var sessions []Session
	for rows.Next() {
		session, scanErr := scanSession(rows)
		if scanErr != nil {
			rows.Close()
			return nil, fmt.Errorf("scan recoverable shared session: %w", scanErr)
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	results := make([]Result, 0, len(sessions))
	for _, current := range sessions {
		next := current
		next.PositionMS = CurrentPosition(current, now)
		next.RefTS = now
		next.State = StateEnded
		next.EventSeq++
		next.DJConnected = false
		next.DJDisconnectedAt = nil
		next.DJGraceDeadline = nil
		next.DJConnectionID = nil
		next.DJLeaseTokenHash = nil
		next.UpdatedAt = now
		next.EndedAt = &now
		reason := "platform-restart"
		next.EndReason = &reason
		accepted, err := updateRecoverySession(ctx, tx, current, next)
		if err != nil {
			return nil, err
		}
		event, err := insertEvent(ctx, tx, accepted, EventDraft{Type: "session-ended", Payload: map[string]any{
			"reason": reason, "mode": ModeIndividual,
		}}, now)
		if err != nil {
			return nil, err
		}
		results = append(results, result(ModeIndividual, accepted, event))
	}
	if mode != ModeIndividual {
		if _, err := tx.Exec(ctx, `UPDATE settings SET mode='individual', updated_at=$1 WHERE id=1`, now); err != nil {
			return nil, fmt.Errorf("restore individual mode: %w", err)
		}
		var sessionID int64
		if len(sessions) > 0 {
			sessionID = sessions[len(sessions)-1].ID
		}
		if err := insertModeAudit(ctx, tx, nil, fmt.Sprintf("sync-recovery-%d", now.UnixNano()), mode, ModeIndividual, sessionID, "platform-restart"); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit synchronized recovery: %w", err)
	}
	return results, nil
}

func updateRecoverySession(ctx context.Context, tx pgx.Tx, current, next Session) (Session, error) {
	return scanSession(tx.QueryRow(ctx, `UPDATE shared_sessions SET
		state='ended',event_seq=$2,dj_lease_token_hash=NULL,dj_connection_id=NULL,
		dj_connected=false,dj_disconnected_at=NULL,dj_grace_deadline=NULL,position_ms=$3,ref_ts=$4,
		updated_at=$4,ended_at=$4,end_reason='platform-restart'
		WHERE id=$1 AND state <> 'ended' RETURNING `+sessionColumns,
		current.ID, next.EventSeq, next.PositionMS, next.RefTS))
}

func (s *PostgresStore) authorizedTransaction(ctx context.Context, principal auth.Principal) (pgx.Tx, error) {
	if err := validPrincipal(principal); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin synchronized transaction: %w", err)
	}
	success := false
	defer func() {
		if !success {
			_ = tx.Rollback(ctx)
		}
	}()
	var role, status, provisioning string
	var mustChange bool
	err = tx.QueryRow(ctx, `SELECT role,status,provisioning_state,must_change_password
		FROM users WHERE id=$1 FOR SHARE`, principal.User.ID).Scan(&role, &status, &provisioning, &mustChange)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, auth.ErrUnauthenticated
	}
	if err != nil {
		return nil, fmt.Errorf("recheck synchronized account: %w", err)
	}
	var exists int
	err = tx.QueryRow(ctx, `SELECT 1 FROM sessions
		WHERE user_id=$1 AND token_hash=$2 AND expires_at>now() FOR SHARE`, principal.User.ID, principal.TokenHash).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, auth.ErrUnauthenticated
	}
	if err != nil {
		return nil, fmt.Errorf("recheck synchronized session: %w", err)
	}
	if status != "active" || provisioning != "complete" {
		return nil, auth.ErrAccountNotActive
	}
	if mustChange || role != principal.User.Role {
		return nil, ErrForbidden
	}
	success = true
	return tx, nil
}

func insertEvent(ctx context.Context, tx pgx.Tx, session Session, draft EventDraft, serverTS time.Time) (Event, error) {
	payload, err := json.Marshal(draft.Payload)
	if err != nil {
		return Event{}, fmt.Errorf("encode synchronized event: %w", err)
	}
	var event Event
	event.SharedSessionID = session.ID
	event.EventSeq = session.EventSeq
	event.Version = session.Version
	event.Type = draft.Type
	event.Payload = payload
	event.ServerTS = serverTS.UTC()
	err = tx.QueryRow(ctx, `INSERT INTO sync_events(shared_session_id,event_seq,version,type,payload,server_ts)
		VALUES($1,$2,$3,$4,$5::jsonb,$6) RETURNING id`, event.SharedSessionID,
		event.EventSeq, event.Version, event.Type, event.Payload, event.ServerTS).Scan(&event.ID)
	if err != nil {
		return Event{}, fmt.Errorf("persist synchronized event: %w", err)
	}
	return event, nil
}

func insertModeAudit(ctx context.Context, tx pgx.Tx, actorID *int64, correlation, from, to string, sessionID int64, reason string) error {
	detail, err := json.Marshal(map[string]any{
		"from": from, "to": to, "sharedSessionId": sessionID, "reason": reason,
	})
	if err != nil {
		return fmt.Errorf("encode mode audit: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO audit_log(correlation_id,actor_user_id,action,entity,outcome,detail)
		VALUES($1,$2,'mode.changed','platform','ok',$3::jsonb)`, correlation, actorID, detail); err != nil {
		return fmt.Errorf("persist mode audit: %w", err)
	}
	return nil
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func scanSession(row pgx.Row) (Session, error) {
	var session Session
	var queue []byte
	err := row.Scan(&session.ID, &session.Version, &session.EventSeq, &session.State,
		&session.DJUserID, &session.DJLeaseGeneration, &session.DJLeaseTokenHash,
		&session.DJConnectionID, &session.DJConnected, &session.DJDisconnectedAt, &session.DJGraceDeadline,
		&session.CurrentMediaID, &session.PositionMS, &session.RefTS, &queue,
		&session.QueueIndex, &session.CreatedAt, &session.UpdatedAt, &session.EndedAt,
		&session.EndReason)
	if err != nil {
		return Session{}, err
	}
	if err := json.Unmarshal(queue, &session.Queue); err != nil {
		return Session{}, fmt.Errorf("decode shared queue: %w", err)
	}
	normalizeSessionTimes(&session)
	return session, nil
}

func normalizeSessionTimes(session *Session) {
	session.RefTS = session.RefTS.UTC()
	session.CreatedAt = session.CreatedAt.UTC()
	session.UpdatedAt = session.UpdatedAt.UTC()
	if session.DJGraceDeadline != nil {
		value := session.DJGraceDeadline.UTC()
		session.DJGraceDeadline = &value
	}
	if session.DJDisconnectedAt != nil {
		value := session.DJDisconnectedAt.UTC()
		session.DJDisconnectedAt = &value
	}
	if session.EndedAt != nil {
		value := session.EndedAt.UTC()
		session.EndedAt = &value
	}
}

func result(mode string, session Session, event Event) Result {
	snapshot := Snapshot{Mode: mode, Session: &session}
	if session.State != StateEnded {
		snapshot.ActiveSharedSessionID = &session.ID
	}
	return Result{Snapshot: snapshot, Event: event}
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func isUniqueViolation(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23505"
}
