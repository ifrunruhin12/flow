# M4A implementation report — 2026-11-07

## 1. M0–M3 audit

The detailed audit is in `M4A_READINESS.md`. Opaque PostgreSQL sessions,
server-side role/status/provisioning checks, member-scoped Navidrome access,
the Flow-only audio gateway, stream cancellation, and M3 revision CAS were
verified in code and deterministic tests. The directly relevant M3 gap was
that individual writes were not mode-gated; M4A now rejects `Update`/`Clear`
in synchronized mode while leaving `Get` and the persisted row intact. The M3
Chromium cleanup race was also fixed with bounded recursive-removal retries.

## 2. Files and modules changed

- `backend/internal/sync`: model, executor, service, PostgreSQL store, unit and
  PostgreSQL integration tests.
- `backend/internal/api/sync.go`, API registration, `/auth/me` mode metadata,
  synchronization API tests, and individual-mode error mapping.
- `backend/internal/playback/service.go`: synchronized-mode mutation gate.
- `backend/cmd/flowd/main.go`: service composition, startup recovery, graceful
  shutdown recovery, and lifecycle closure.
- Migration 0004 and the affected product/architecture/API/schema/security/
  development/roadmap/status documentation.

## 3. Database migration

`0004_m4a_synchronization` creates:

- singleton `settings` with canonical platform mode and sync timing defaults;
- `shared_sessions`, including one-open-session uniqueness, queue/current
  agreement, active lease, grace, and terminal-state constraints;
- `sync_events`, unique by `(shared_session_id,event_seq)`.

The down migration removes only M4A tables. Individual `session_state`, users,
sessions, and M0–M3 data are not changed.

## 4. State machine

The backend implements individual → synchronized creation, active play/pause,
seek, next/previous, queue add/remove/move, DJ-disconnected paused grace,
reconnect, admin end, grace expiry, and restart safe-end. Illegal state,
boundary, empty-queue, inaccessible-media, stale-version, and stale-lease
operations are rejected without advancing checkpoint or event sequence.

`POST /api/platform/mode` is the only public start/end transition. Starting
creates the v0/seq1 active session from an explicit track or the administrator's
individual state. A default seed carries its individual revision into the mode
transaction, so a concurrent individual write makes start return 409 instead
of creating a stale shared session. Ending requires the current version and
active lease.

## 5. Executor and transaction design

One channel-fed executor serializes all state-changing service operations in
the supported single-process deployment. PostgreSQL remains the correctness
boundary: settings and session rows are locked, expected version/event sequence
are rechecked, and constraints prevent a second open session. Checkpoint,
event, mode, and mode-audit writes commit together or roll back together.
Separate-executor PostgreSQL tests exercise races so correctness is not based
only on the in-memory queue.

## 6. DJ lease and reconnect

The service issues a random 256-bit token, stores only its SHA-256 hash, binds
it to the authenticated administrator and a server-issued UUID connection ID,
and increments generation on every acquisition/takeover. Commands require an
exact user/generation/connection/token match. Disconnect freezes the calculated
position, changes to paused without changing content version, persists the
disconnect instant plus a 90-second deadline, and emits one lifecycle event. Reconnect rotates authority
and remains paused. Timer expiry rechecks session ID and exact persisted
deadline and is idempotent/retryable. Acquisition/disconnect are trusted M4B
service boundaries, not unrestricted REST endpoints.

## 7. Version and event semantics

`version` begins at 0 and increments only for play, pause, seek, next,
previous, and queue content mutations. `event_seq` begins at 1 for
`session-started` and increments for every content or lifecycle event. Each
event stores the content version applicable to that sequence. The durable
types are `session-started`, `state-changed`, `dj-disconnected`,
`dj-reconnected`, and `session-ended`; payloads are documented in the API
specification. M4A does not broadcast events.

## 8. Restart recovery

Before accepting HTTP traffic, startup recovery locks the platform state,
marks every non-ended shared session ended with `platform-restart`, freezes the
last calculated position, writes the terminal event, and returns mode to
individual in one transaction. The operation is idempotent and never resumes a
lease/grace period. Normal SIGTERM drains HTTP and performs the same transition
before executor shutdown. Individual rows are untouched.

## 9. Test evidence

Verified by automated deterministic tests:

- legal content transitions, illegal boundaries, queue invariants, duplicate
  occurrence handling, timing/drift decisions, authorization, duration checks;
- concurrent commands, lease takeover/zombie fencing, disconnect/reconnect,
  expiry, idempotent disconnect/recovery, and individual-mode preservation;
- REST authentication/authorization, CSRF Origin, canonical route surface,
  lease/body validation, and conflict/error mapping.

The real PostgreSQL test is implemented and compiles. It covers concurrent
mode starts, lease acquisition and content commands from separate executors,
unique ordered events, event-insert rollback, commit-time account suspension,
fresh-pool recovery, repeated recovery, individual-state preservation, and
migration down/up. In the latest environment it was skipped because
`FLOW_TEST_DATABASE_URL` was unset. An isolated PostgreSQL 18.6 cluster was
then initialized under `/tmp`, but the sandbox rejected Unix-socket creation
with `Operation not permitted`; Docker access was also denied. The temporary
cluster was stopped/removed and no database pass is claimed.

Executed successfully:

- `go test -race -count=1 ./...`
- `go vet ./...`
- `go build -o /tmp/flow-m4a-flowd ./cmd/flowd`
- `go test -tags=integration -count=1 ./...` (external services skip by contract)
- web `npm test` (33/33), `npm run typecheck`, `npm run lint`, `npm run build`
- PostgreSQL 18.6 single-user migration smoke: migrations 0001→0004 up and
  migration 0004 down completed without SQL errors (this validates DDL, not
  pgx locking/concurrency).

Executed with an explicit skip:

- `go test -v -run TestSynchronizationPostgresTransactionsAndRecovery ./internal/sync`
  (`FLOW_TEST_DATABASE_URL` unset)

No manual synchronized browser playback was performed because that client and
WebSocket channel are explicitly M4B.

## 10. Known limitations

- M4A has no WebSocket hub, event broadcast, subscriptions, participation
  rows, heartbeat transport, client drift correction, or synchronized UI.
- The PostgreSQL concurrency suite still needs an environment-backed run before
  M4A can be called fully integration-verified.
- The prior M3 live GitHub gate needs rerun after its Chromium cleanup fix.
- V1 remains single backend instance; PostgreSQL protects transactions, but
  multi-instance executor ownership is intentionally unsupported.

## 11. Documentation

Updated: README, CHANGELOG, REQUIREMENTS, ARCHITECTURE, DATABASE_SCHEMA,
API_SPECIFICATION, PLAYBACK_SYSTEM, SECURITY, DEVELOPMENT_GUIDE, ROADMAP,
ARCHITECTURE_STATUS, and CONTRACT_MATRIX. No accepted ADR changed.

## 12. M4B readiness

The committed checkpoint/event and trusted `AcquireLease`/`DisconnectDJ`
boundaries are ready for M4B to attach an authenticated same-origin WebSocket
hub, subscribe-before-snapshot delivery, heartbeats, and browser playback
coordination. Readiness is conditional on executing the PostgreSQL M4A suite.

The exact next milestone is **M4B — Synchronization WebSocket Delivery & Client
Playback Coordination**. M4B has not started.
