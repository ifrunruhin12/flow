# Flow — Database Schema

Version: 1.0.0-draft
Database: PostgreSQL 16
Owner: Flow backend (migrations embedded in the binary, run at startup, `golang-migrate`-style sequential SQL)

Design note: Navidrome's SQLite database is **not touched by Flow**. All tables below are Flow's own. Foreign keys reference only Flow tables. Navidrome IDs (user id, media file id, playlist id) are stored as plain strings/ints — soft references, no FK constraints across databases.

---

## 1. ER Overview

```
users ──1:0..1──> membership_request (approved → user)
users ──1:0..1──> user_settings (theme, download flag)
users ──1:N────> session_state (individual playback, per user, 1 row)
users ──1:N────> sync_participation (per shared session)
shared_sessions ──1:N──> sync_participation
shared_sessions ──1:N──> sync_events (last N for audit/debug, pruned)
users ──1:N────> audit_log
settings ──1:1──> platform (singleton row)
```

## 2. Tables

### 2.1 `users`

The Flow account. One row per person on the platform (admin or member).

| Column | Type | Notes |
|---|---|---|
| id | bigint PK | |
| username | text UNIQUE NOT NULL | Login name; equals Navidrome username |
| display_name | text | |
| email | text UNIQUE | |
| password_hash | text NOT NULL | argon2id PHC string |
| role | text NOT NULL | `admin` \| `member` (values ready for more) |
| status | text NOT NULL | `active` \| `suspended` \| `deleted` |
| navidrome_user_id | text NULL | Soft ref: Navidrome `user.id` (created on provisioning) |
| navidrome_password_enc | bytea NULL | AES-GCM ciphertext of the per-member Navidrome password (see SECURITY.md §4) |
| created_at | timestamptz default now() | |
| updated_at | timestamptz | |
| last_active_at | timestamptz | Updated on authenticated API use (throttled) |

Indexes: unique(username), unique(email), (status, role), GIN trigram on username (V1.x for case-insensitive find).

### 2.2 `membership_requests`

| Column | Type | Notes |
|---|---|---|
| id | bigint PK | |
| display_name | text NOT NULL | |
| email | text NOT NULL | |
| desired_username | text NOT NULL | |
| status | text NOT NULL | `pending` \| `approved` \| `rejected` |
| reject_note | text NULL | Visible to requester on rejection (V1.x: visible; V1 stores only) |
| admin_note | text NULL | Internal only |
| user_id | bigint FK→users NULL | Set on approval |
| created_at / decided_at | timestamptz | |
| decided_by | bigint FK→users NULL | Admin who decided |

Indexes: (status, created_at).

### 2.3 `user_settings`

| Column | Type | Notes |
|---|---|---|
| user_id | PK, FK→users | 1:1 |
| theme_id | text NOT NULL | References theme catalog; default from `platform.default_theme` |
| preferred_stream_format | text | `auto` \| `flac` \| `ogg` (V1: auto + flac; ogg = transcode fallback) |
| allow_downloads | boolean default false | AND-ed with the platform flag; both must be true |
| updated_at | timestamptz | |

### 2.4 `session_state` (individual playback)

| Column | Type | Notes |
|---|---|---|
| user_id | PK, FK→users | 1:1 |
| queue | jsonb NOT NULL | `[{mediaId, addedAt}, …]` in play order |
| queue_index | int NOT NULL default 0 | Position within queue |
| current_media_id | text NULL | |
| position_ms | int NOT NULL default 0 | |
| volume | numeric(4,3) NOT NULL default 1.0 | 0.0–1.0 |
| shuffle | boolean NOT NULL default false | |
| updated_at | timestamptz | Heartbeat throttle: write at most every 10 s and on unload |

### 2.5 `shared_sessions` (synchronized mode)

| Column | Type | Notes |
|---|---|---|
| id | bigint PK | |
| version | int NOT NULL | Bumped on every admin mutation |
| state | text NOT NULL | `active` \| `paused` \| `ended` |
| dj_user_id | bigint FK→users NOT NULL | The admin |
| current_media_id | text NOT NULL | |
| position_ms | int NOT NULL | Position at `ref_ts` |
| ref_ts | timestamptz NOT NULL | Server wall-clock at last mutation (used for target calc) |
| queue | jsonb NOT NULL | `[{mediaId}, …]` after current |
| created_at / ended_at | timestamptz | |

Live in-memory copy is authoritative; Postgres row is a checkpoint written on every admin mutation (transactional, cheap). On restart: `state = ended` rows are pruned; `active`/`paused` rows are offered to the DJ as "resume?" — default: end safely (NFR-REL-004).

### 2.6 `sync_participation`

| Column | Type | Notes |
|---|---|---|
| shared_session_id | FK→shared_sessions | |
| user_id | FK→users | |
| joined_at | timestamptz | |
| last_heartbeat_ts | timestamptz | Debug/audit |
| | PK (shared_session_id, user_id) | |

### 2.7 `sync_events` (bounded log)

Last 500 events per session, pruned on insert (for debugging sync issues and late-join replay within the session):

| Column | Type | Notes |
|---|---|---|
| id | bigint PK | |
| shared_session_id | FK | |
| version | int | |
| type | text | play / pause / seek / next / prev / queue / end / dj_disconnected | |
| payload | jsonb | |
| server_ts | timestamptz | |

Index: (shared_session_id, version DESC).

### 2.8 `settings` (platform singleton)

| Column | Type | Notes |
|---|---|---|
| id | PK, always 1 | |
| mode | text NOT NULL | `individual` \| `synchronized` |
| default_theme | text NOT NULL | |
| allow_downloads | boolean NOT NULL default false | Platform-level gate (admin can turn on later) |
| max_concurrent_streams | int NOT NULL | Passed to Navidrome config at deploy time (config, not runtime) |
| sync_drift_threshold_ms | int NOT NULL default 2000 | |
| sync_heartbeat_interval_ms | int NOT NULL default 5000 | |
| dj_grace_period_s | int NOT NULL default 90 | |
| updated_at | timestamptz | |

### 2.9 `audit_log`

| Column | Type | Notes |
|---|---|---|
| id | bigint PK | |
| actor_user_id | bigint FK→users | |
| action | text | `request.approved`, `user.suspended`, `mode.changed`, `playlist.created`, `scan.triggered`, `upload.completed`, `settings.updated`, … |
| entity | text | `user:12`, `request:3`, `session:7`, `platform`, … |
| detail | jsonb | |
| created_at | timestamptz default now() | |

Retention: all in V1 (append-only, low volume); pruning policy V1.x.

### 2.10 `themes`

Catalog of available themes (shipped + admin-managed later):

| Column | Type | Notes |
|---|---|---|
| id | text PK | `pixel-art`, `cyberpunk`, `dark-gothic`, `ghibli-cozy`, `minimal-audiophile` |
| name | text | |
| builtin | boolean | |
| token_url | text | URL to the token CSS (web asset) — for custom themes later |
| enabled | boolean | |

## 3. Migrations

- File layout: `backend/migrations/0001_init.up.sql` / `0001_init.down.sql`, `0002_…`, etc.
- Runner: executed at backend startup; `migrations` ledger table (`id, version, applied_at`).
- Destructive migrations (e.g., role enum widening) are forward-only by default; `down.sql` is best-effort.
- All timestamps `timestamptz UTC`. All IDs `bigint`. Media/playlist/user Navidrome IDs stay `text` (Navidrome uses string ids).

## 4. What is deliberately NOT in this schema

| Candidate | Where it lives instead | Why |
|---|---|---|
| Media files, albums, artists, genres | Navidrome SQLite | Single source of truth; proxy reads |
| Playlists (content) | Navidrome | CRUD proxied; Flow stores nothing |
| Favorites/stars | Navidrome (`Star`/`Unstar` Subsonic calls) | Per-user stars are already in Navidrome |
| Per-member Navidrome JWT | In-memory cache, encrypted column optional (V1.x) | Transient credential, not durable state |
| Stream URLs | Not stored | Issued on demand, short-lived |
