# Flow — Database Schema

Version: 1.1.0-draft (supersedes 1.0.0-draft)
Database: PostgreSQL 16
Owner: Flow backend (reviewed SQL embedded in the binary and applied sequentially by Flow's advisory-locked migration runner; migration policy in DEVELOPMENT_GUIDE §7)

Design note: Navidrome's SQLite database is **not touched by Flow**. All tables below are Flow's own. Foreign keys reference only Flow tables. Navidrome IDs (user id, media id, playlist id) are stored as plain text — soft references, no FK constraints across databases. **There is no atomicity between Flow's Postgres and Navidrome; cross-system consistency is achieved by durable state machines with idempotent steps (ADR-004), not distributed transactions.**

Changes in this revision: `sessions` (opaque browser auth, ADR-002); `users` provisioning state + credential key version (ADR-004); `shared_sessions` DJ lease + grace deadline + event sequence (ADRs 005/006); `session_state` revision control; `upload_operations`; `audit_log` correlation/outcome; external-key metadata; settings precedence documented (§5).

---

## 1. ER Overview

```
users ──1:N──> sessions (opaque tokens; hash-stored)
users ──1:0..1──> membership_request (approved → user)
users ──1:0..1──> user_settings (theme, stream format, download flag)
users ──1:1────> session_state (individual playback, per user, 1 row, revisioned)
users ──1:N────> sync_participation (per shared session)
users ──1:N────> upload_operations (audit of the upload pipeline)
shared_sessions ──1:N──> sync_participation
shared_sessions ──1:N──> sync_events (authoritative per-sequence events; bounded per session)
users ──1:N────> audit_log
settings ──1:1──> platform (singleton row)
credential_key_versions ──1:N──> encrypted material in users / admin credential (metadata only; keys are external)
```

## 2. Tables

### 2.1 `users`

The Flow account. One row per person on the platform (admin or member).

| Column | Type | Notes |
|---|---|---|
| id | bigint PK | |
| username | text UNIQUE NOT NULL | Flow login name; member usernames also become their Navidrome usernames |
| display_name | text | |
| email | text UNIQUE | |
| password_hash | text NOT NULL | argon2id PHC string (Flow login) |
| must_change_password | boolean NOT NULL default false | True for an admin-issued temporary password. The restricted-session gate allows only `/auth/me`, `/auth/change-password`, and logout until cleared. |
| role | text NOT NULL CHECK (role IN ('admin','member')) | Values ready for more |
| status | text NOT NULL CHECK (status IN ('active','suspended','deleted')) | Auth checks this on every request (ADR-002 §3) |
| provisioning_state | text NOT NULL CHECK (provisioning_state IN ('not_started','credential_ready','creating_nd_user','linking','complete','failed')) | ADR-004 state machine. `credential_ready` proves the encrypted reusable credential committed before any remote create call. **Only `complete` + `active` may log in and stream.** |
| provisioning_failed_step | text NULL | Last retry point when state is `failed`; never contains credentials. |
| navidrome_username | text NULL | Username required with the reusable credential. Members use `username`; the Flow admin login may differ from the operator-supplied Navidrome admin username. |
| navidrome_user_id | text NULL | Soft ref: Navidrome `user.id` (set in `linking`→`complete`) |
| navidrome_password_enc | bytea NULL | Versioned AEAD envelope for this Flow user's Navidrome credential (member or linked admin). The master key is external. |
| navidrome_cred_key_version | int NULL | References key-version metadata; key material is loaded from the external deployment keyring. |
| is_navidrome_admin | boolean NOT NULL default false | True only for the operator-bootstrapped admin link (ADR-003); true users use the admin credential cache exclusively |
| navidrome_cleanup_state | text NOT NULL CHECK (IN ('not_requested','pending','complete','failed')) | Durable hard-delete compensation state. `pending`/`failed` remains visible on the admin member list and a repeated hard delete retries idempotently. |
| created_at | timestamptz NOT NULL default now() | |
| updated_at | timestamptz | |
| last_active_at | timestamptz | Updated on authenticated API use (throttled, ~10 min) |

Constraints:
- `CHECK (provisioning_state <> 'complete' OR (navidrome_username IS NOT NULL AND navidrome_user_id IS NOT NULL))` — a completed provisioning must have a reusable credential identity and link.
- `CHECK (provisioning_state NOT IN ('credential_ready','creating_nd_user','linking','complete') OR (navidrome_username IS NOT NULL AND navidrome_password_enc IS NOT NULL AND navidrome_cred_key_version IS NOT NULL))` — no remote-create-capable state exists without a durable credential identity and envelope.
- `CHECK (NOT is_navidrome_admin OR (role = 'admin' AND provisioning_state = 'complete'))` — the operator-linked admin skips the member provisioning machine; `not_started` remains a valid transient member state before the durable credential transaction commits.
- `CHECK (status = 'deleted' OR navidrome_cleanup_state = 'not_requested')` — remote cleanup is never scheduled for a live account.
- `provisioning_state='failed'` iff `provisioning_failed_step` is non-null; usernames/display names are non-blank.
Indexes: unique(username), unique(email), one partial unique admin row, (status, role, provisioning_state).

### 2.2 `sessions` (opaque browser authentication — ADR-002)

| Column | Type | Notes |
|---|---|---|
| token_hash | bytea PK | SHA-256 of the 32-byte random token (64 hex chars on the wire). **The raw token is never stored.** |
| user_id | bigint FK→users NOT NULL | |
| created_at | timestamptz NOT NULL | |
| expires_at | timestamptz NOT NULL | Default 8 h (FR-AUTH-004); refreshed on activity only for API calls, not for audio (per request design below) |
| last_seen_at | timestamptz | Activity tracking |
| user_agent_hint | text | Observability only |

- One row per browser login; multiple tabs share the cookie. Logout/delete/suspend = row delete → immediate revocation everywhere (no in-memory set to fall out of sync).
- `token_hash` is exactly 32 bytes and `expires_at > created_at`.
- Expiry sweep: `DELETE FROM sessions WHERE expires_at < now()` runs opportunistically (on a background tick or at login), not a cron dependency.
- **Audio requests are not session-refreshed** (a 3-hour FLAC stream must not expire mid-play): the gateway authorizes the stream at request start; long-running bytes flow on the original check.
- Native clients (V1.x+): a separate `client_sessions` table with bearer tokens — same model, different transport (ADR-002). Not in V1 schema; adding the table then is a forward-only migration.

### 2.3 `membership_requests`

| Column | Type | Notes |
|---|---|---|
| id | bigint PK | |
| display_name | text NOT NULL | |
| email | text NOT NULL | |
| desired_username | text NOT NULL | |
| status | text NOT NULL CHECK (status IN ('pending','approved','rejected')) | |
| reject_note | text NULL | Internal in V1 (V1.x: shown to requester) |
| admin_note | text NULL | Internal only |
| user_id | bigint FK→users NULL | Set on approval |
| created_at / decided_at | timestamptz | |
| decided_by | bigint FK→users NULL | Admin who decided |

Constraints: partial unique index `UNIQUE (email) WHERE status = 'pending'` — at most one open request per email.
Decision-state checks require pending requests to have no decision/user link, approved requests to have a deciding admin/time/user link, and rejected requests to have a deciding admin/time but no user link.
Indexes: (status, created_at).

### 2.4 `user_settings`

| Column | Type | Notes |
|---|---|---|
| user_id | PK, FK→users | 1:1 |
| theme_id | text NOT NULL | References `themes`; falls back to `settings.default_theme` |
| preferred_stream_format | text NOT NULL CHECK (IN ('auto','original','opus','mp3')) default 'auto' | Public gateway format names; `original` is representation passthrough and `opus` is the verified Opus transcode request |
| allow_downloads | boolean NOT NULL default false | AND-ed with `settings.allow_downloads`; both must be true |
| updated_at | timestamptz | |

### 2.5 `session_state` (individual playback, one row per user)

| Column | Type | Notes |
|---|---|---|
| user_id | PK, FK→users | 1:1 |
| revision | bigint NOT NULL default 0 | Every write carries `ifRevision`; mismatch ⇒ HTTP 409 with current state. The client reloads or explicitly retries replacement; the server never merges. |
| queue | jsonb NOT NULL default '[]' | Canonical `[{mediaId, addedAt}, …]`; array, at most 200; timestamps are server-owned; duplicate IDs allowed |
| queue_index | int NOT NULL default 0 | Nonnegative; 0 for empty, otherwise in bounds |
| current_media_id | text NULL | |
| position_ms | int NOT NULL default 0 | 0…86,400,000; empty session is 0; service validates authoritative known duration |
| volume | numeric(4,3) NOT NULL default 1.0 CHECK (volume BETWEEN 0 AND 1) | |
| shuffle | boolean NOT NULL default false | |
| playback_status | text NOT NULL default 'paused' CHECK (IN ('paused','playing')) | Saved browser transport intent, not a server audio engine |
| updated_at | timestamptz NOT NULL default now() | Every accepted CAS commits immediately. Active playback checkpoints every 10 s; no unload guarantee. |

Multi-device rule (PLAYBACK_SYSTEM §2): one persisted session per user. A stale client receives 409 + the current row and must either reload or deliberately replace state using the returned revision. V1 defines no automatic queue union or last-writer-wins merge.

Migration `0003_m3_individual_playback` backfills every existing user and installs
an AFTER INSERT user trigger to create exactly one default row transactionally.
`user_id` uses ON DELETE CASCADE; suspension/soft deletion preserve state. Clear
updates the same row through CAS, never deleting it or resetting revision.
Checks enforce nonnegative revision, array/bounded queue, current/index agreement,
and canonical empty state. Service validation checks media IDs, occurrence
timestamps, queue operations, and duration. No media metadata or credentials are
stored here. The commit transaction rechecks/locks current account and Flow
session before the conditional revision UPDATE. No playback cache is authoritative.
The initial read constructs and validates a proposed state; it grants no write
authority. `UPDATE ... WHERE user_id=$owner AND revision=$expected RETURNING ...`
advances revision/timestamp atomically. A zero-row update reads the latest own
snapshot for 409. User/session share locks follow lifecycle lock order and are
never held during Navidrome requests. The API accepts revisions below the largest
JSON-safe integer, leaving room for the increment; all response timestamps are UTC.

`internal/playback/postgres_test.go` checks M1 upgrade, user backfill/trigger,
competing real writes, a checkpoint/track race, revocation, and a new pool/service
restore. The separately tagged live suite restarts the actual Flow process.
These live checks require PostgreSQL and remain unexecuted in the restricted
implementation environment; the PostgreSQL 18.6 single-user SQL smoke is narrower
evidence and does not substitute for the pinned PostgreSQL 16/pgx gate.

### 2.6 `shared_sessions` (synchronized mode)

| Column | Type | Notes |
|---|---|---|
| id | bigint PK | |
| version | int NOT NULL | Bumped on every mutation of shared *content* (ADR-006: version = state content order) |
| event_seq | bigint NOT NULL default 0 | **Monotonic per-session event counter (ADR-006): every emitted authoritative state event (including `dj-disconnected` and `session-ended`, which do *not* bump `version`) increments `event_seq`.** |
| state | text NOT NULL CHECK (state IN ('active','paused','ended')) | |
| dj_user_id | bigint FK→users NOT NULL | The DJ (admin) |
| dj_lease_generation | bigint NOT NULL default 0 | Monotonic generation issued only by the executor. Clients never choose it. |
| dj_lease_token_hash | bytea NULL | SHA-256 of the current opaque DJ lease token. The raw token is delivered only to the active authenticated DJ connection and is never persisted. |
| dj_connection_id | uuid NULL | Server-issued ID of the active DJ WebSocket connection. A takeover/reconnect rotates token + generation and fences the old connection. |
| dj_connected | boolean NOT NULL default false | |
| dj_grace_deadline | timestamptz NULL | Drives grace while the process is alive and remains useful for audit. Any backend restart ends the session immediately in V1. |
| current_media_id | text NOT NULL | |
| position_ms | int NOT NULL | Position at `ref_ts` |
| ref_ts | timestamptz NOT NULL | Server wall-clock at last content mutation |
| queue | jsonb NOT NULL | Media ids (ordered) |
| queue_index | int NOT NULL | |
| created_at / ended_at | timestamptz | |
| end_reason | text NULL CHECK (IN ('admin','dj-disconnect','platform-restart','grace-expiry')) | |

Constraints: `CHECK (dj_grace_deadline IS NULL OR state IN ('paused'))`; `CHECK (state = 'ended' OR end_reason IS NULL)`.
Index: (state) — restart recovery scans non-ended rows once.

**Restart policy (V1, ADR-005):** on backend boot, any row with `state IN ('active','paused')` is marked `ended` with `end_reason = 'platform-restart'` in the same recovery transaction that flips the platform mode to `individual` and records the terminal event. The DJ grace deadline is diagnostic only across a restart; V1 never waits or resumes. Individual sessions are untouched.

### 2.7 `sync_participation`

| Column | Type | Notes |
|---|---|---|
| shared_session_id | FK→shared_sessions, PK(shared_session_id, user_id) | |
| user_id | FK→users | |
| joined_at | timestamptz | |
| last_heartbeat_ts | timestamptz | Debug/audit |
| last_position_ms | int | Last reported position (diagnostics) |

### 2.8 `sync_events` (authoritative, bounded log — ADR-006)

| Column | Type | Notes |
|---|---|---|
| id | bigint PK | |
| shared_session_id | FK→shared_sessions | |
| event_seq | bigint NOT NULL | UNIQUE per session; equals the envelope `seq` of the WS frame |
| version | int NOT NULL | The current content version carried by **every** event, including lifecycle events. |
| type | text NOT NULL | `session-started` / `state-changed` / `dj-disconnected` / `dj-reconnected` / `session-ended`; transient `hello` and per-client corrections are not authoritative event rows. |
| payload | jsonb NOT NULL | Full authoritative state or delta, per type |
| server_ts | timestamptz NOT NULL | |

Invariant (ADR-006): **exactly one row per `(shared_session_id, event_seq)`**, and each executor command commits one checkpoint plus one authoritative event before broadcast. A command that changes queue, track, position, and mode uses one composite event; it does not emit several logical events with the same sequence. Retention is bounded, but reconnect consistency comes from subscribe-before-snapshot buffering, not from assuming WebSocket timing.

### 2.9 `settings` (platform singleton — runtime settings, ARCHITECTURE §8)

| Column | Type | Notes |
|---|---|---|
| id | int PK, CHECK (id = 1) | Singleton |
| mode | text NOT NULL CHECK (IN ('individual','synchronized')) | |
| default_theme | text NOT NULL | |
| allow_downloads | boolean NOT NULL default false | Platform gate |
| sync_drift_threshold_ms | int NOT NULL default 2000 CHECK (>0) | |
| sync_heartbeat_interval_ms | int NOT NULL default 5000 CHECK (>=2000) | |
| dj_grace_period_s | int NOT NULL default 90 CHECK (>0) | |
| max_concurrent_streams_per_user | int NOT NULL default 100 CHECK (>0) | Hard per-user limit on simultaneously registered gateway streams; Navidrome's transcoding limit is separate deploy-time configuration. |
| updated_at | timestamptz | |

Note what is **not** here: Navidrome behavior (transcoding quality, `maxConcurrentStreams` on the Navidrome side, download/sharing gates) — those live in Navidrome config, owned by the operator (ARCHITECTURE §8). Runtime vs. deploy-time settings have exactly one home each.

### 2.10 `audit_log`

| Column | Type | Notes |
|---|---|---|
| id | bigint PK | |
| correlation_id | text NOT NULL | `audit:<uuid>` — one per admin action; carried in the HTTP response header (`X-Correlation-Id`) and in the WS `hello` for session-scoped events; joins a multi-step action (e.g., provisioning) |
| actor_user_id | bigint FK→users | |
| action | text | `request.approved`, `user.suspended`, `mode.changed`, `playlist.created`, `scan.triggered`, `upload.completed`, `settings.updated`, `provisioning.failed`, `admin-credential.stored`, … |
| entity | text | `user:12`, `request:3`, `session:7`, `upload:42`, `platform` |
| outcome | text NOT NULL CHECK (IN ('ok','error','in-progress')) | |
| detail | jsonb | Structured context (never credentials) |
| created_at | timestamptz NOT NULL default now() | |

Retention: all in V1 (append-only, low volume); pruning policy V1.x.

### 2.11 `upload_operations` (upload pipeline audit, task G)

| Column | Type | Notes |
|---|---|---|
| id | bigint PK | |
| admin_user_id | bigint FK→users NOT NULL | Admin who uploaded |
| batch_ref | text NOT NULL | One per upload request (client-supplied idempotency key, or server-generated) |
| file_count | int NOT NULL | |
| total_bytes | bigint NOT NULL | |
| relative_album_path | text NOT NULL | New destination directory, relative to the library root. V1 rejects an existing destination. |
| manifest | jsonb NOT NULL | Expected relative files, sizes, and content hashes used for crash reconciliation. |
| state | text NOT NULL CHECK (state IN ('staging','validating','ready_to_promote','promoted','scan_pending','scanning','done','failed','cleaned')) | Durable pipeline stage (NAVIDROME_INTEGRATION §6). |
| scan_triggered_at | timestamptz NULL | |
| scan_status | text NULL | `ok` / `error:<msg>` |
| error | text NULL | Terminal error detail |
| created_at / finished_at | timestamptz | |

Recovery policy: startup inspects nonterminal rows and the manifest. `.part` files are removed; fully staged batches resume validation/promotion; `ready_to_promote` retries the directory rename; a missing staging directory plus a destination matching the manifest is recorded as promoted; `scan_pending` triggers the missed scan. Cleanup never removes anything under the library root.

### 2.12 `credential_key_versions` (metadata only)

| Column | Type | Notes |
|---|---|---|
| version | int PK | Matches a version in the external keyring. |
| state | text NOT NULL CHECK (state IN ('active','decrypt_only','retired')) | Operational metadata; exactly one `active`. |
| created_at / retired_at | timestamptz | |
| reencrypted_at | timestamptz NULL | Set after all ciphertext rows have moved off this version. |

**No encryption key or wrapped key is stored in PostgreSQL.** The backend loads a root-only mounted JSON keyring (`FLOW_CREDENTIAL_KEYRING_FILE`) with shape `{"activeVersion":1,"keys":{"1":"<base64-encoded 32-byte key>"}}`. Ciphertext uses AES-256-GCM with the credential owner username and purpose as associated data; PostgreSQL stores `nonce || sealed ciphertext` plus the key version. Rotation deploys a new key alongside the old, marks new writes active, re-encrypts rows in bounded transactions, verifies zero ciphertexts remain on the old version, then marks it `decrypt_only`; retirement remains an explicit operator action after backup retention permits removal.

### 2.13 `themes`

| Column | Type | Notes |
|---|---|---|
| id | text PK | `pixel-art`, `cyberpunk`, `dark-gothic`, `ghibli-cozy`, `minimal-audiophile` |
| name | text | |
| builtin | boolean NOT NULL | |
| token_url | text | URL to the token CSS (web asset) — for custom themes later (V1.x) |
| enabled | boolean NOT NULL default true | |

Seeded with the five V1 themes by the M5-owned migration. M0 migration `0001_m0_foundation` establishes and exercises migration infrastructure only; it creates no future-feature tables or seed data.

## 3. Migrations

- File layout: `backend/migrations/0001_m0_foundation.up.sql` / `.down.sql`, then milestone-owned `0002_…` pairs.
- Runner: executed at backend startup; `schema_migrations` ledger table (`version, name, applied_at`) under a dedicated PostgreSQL advisory lock.
- Forward-only in production (DEVELOPMENT_GUIDE §7); `down.sql` is best-effort and only run in dev/CI.
- All timestamps `timestamptz UTC`. All surrogate IDs `bigint`. Navidrome media/playlist/user IDs stay `text` (Navidrome uses string ids).
- No table exists without a feature requirement: every table above maps to FR/NFR/ADR. `credential_key_versions` contains custody metadata only; `upload_operations` supports the staged pipeline.

## 4. What is deliberately NOT in this schema

| Candidate | Where it lives instead | Why |
|---|---|---|
| Media files, albums, artists, genres | Navidrome SQLite | Single source of truth; proxy reads |
| Playlists (content) | Navidrome | CRUD proxied; Flow stores nothing |
| Favorites/stars | Navidrome (`Star`/`Unstar` Subsonic calls) | Per-user stars already exist in Navidrome |
| Per-member Navidrome JWTs | In-memory cache, keyed by user, never persisted | Transient credentials; the durable secret is the encrypted password (users.navidrome_password_enc) |
| Active stream handles | In-memory registry in `streaming` | Transient; indexed by Flow session + user. Logout flushes one session; suspension/deletion flushes the user (ADR-001). |
| Stream URLs | Not stored anywhere | The gateway is the URL; there are no issued-URL artifacts to revoke |
| WS connection state | In-memory hub | Transient; reconnect is snapshot-based |

## 5. Settings precedence (task H)

Single source of truth per setting, by layer:

1. **Deployment config** (env + mounted secrets, operator): DSN, Navidrome URL, credential-keyring path, ports, log level, and the single music root. Key material is never in the DB.
2. **Runtime settings** (Postgres `settings`, admin): mode, default theme, sync tuning, feature flags. No env override in V1 (bootstrap seeds them once; afterwards the DB is the only home).
3. **Navidrome config** (operator, deploy-time): music folder, transcoding, download/sharing/artwork gates, auth rate limits. Flow verifies the invariants (NAVIDROME_INTEGRATION §7) at bootstrap. A failed verification leaves the integration `degraded`: the process remains available for health/bootstrap diagnostics, while product mutations and Navidrome-backed operations remain unavailable until the operator fixes the configuration and restarts.

When a setting appears in two layers, that is a defect — the bootstrap verification (ARCHITECTURE §8) exists to make that impossible silently.
