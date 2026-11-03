# Flow — Requirements Specification

Version: 1.0.0-draft
Method: Each requirement has a stable ID (FR-xxx functional, NFR-xxx non-functional, SR-xxx security). Implementation tasks in the roadmap reference these IDs.

---

## 1. Functional Requirements

### 1.1 Authentication and Accounts

| ID | Requirement | Priority |
|---|---|---|
| FR-AUTH-001 | Admin account is bootstrapped from environment variables on first start (`FLOW_ADMIN_USERNAME`, `FLOW_ADMIN_PASSWORD`). A corresponding Navidrome admin user is created automatically. | V1 must-have |
| FR-AUTH-002 | Admin authenticates via email/password (or username/password) and receives a signed session token. | V1 must-have |
| FR-AUTH-003 | Passwords are hashed (argon2id or bcrypt) and never stored in plaintext in Flow's database. | V1 must-have |
| FR-AUTH-004 | Sessions are stateless JWTs (signed, short-lived, e.g. 8h) plus a server-side revocation list for logout/suspension. | V1 must-have |
| FR-AUTH-005 | Logout invalidates the session server-side; suspended users' sessions are revoked immediately. | V1 must-have |
| FR-AUTH-006 | Admin can reset any member's password from the admin UI. | V1 must-have |
| FR-AUTH-007 | Login is rate-limited per IP and per username (e.g., 10 attempts / 15 min / IP). | V1 must-have |

### 1.2 Membership

| ID | Requirement | Priority |
|---|---|---|
| FR-MEM-001 | Anonymous visitors can submit a membership request (name, email, desired username) from the login screen. | V1 must-have |
| FR-MEM-002 | Requests have states: `pending`, `approved`, `rejected`. State changes are audited (who, when, note). | V1 must-have |
| FR-MEM-003 | Admin can list pending requests, approve, reject (with optional note). | V1 must-have |
| FR-MEM-004 | On approval, a member account is provisioned: Flow user record (ACTIVE) + Navidrome user (read-only) created in one transactional flow with retry. | V1 must-have |
| FR-MEM-005 | Admin can create member accounts directly without a request. | V1 must-have |
| FR-MEM-006 | Admin can suspend a member (login blocked, data retained, sessions revoked). | V1 must-have |
| FR-MEM-007 | Admin can reactivate a suspended member. | V1 must-have |
| FR-MEM-008 | Admin can delete a member account (Navidrome user removed, Flow user deactivated; media ownership unaffected). | V1 must-have |
| FR-MEM-009 | Admin can view all members with status and last-active timestamps. | V1 must-have |

### 1.3 Music Library (proxy to Navidrome)

| ID | Requirement | Priority |
|---|---|---|
| FR-LIB-001 | Browse albums (with artwork, track listing, artist), artists (discography), genres (V1.x). | V1 must-have |
| FR-LIB-002 | Search across songs, albums, artists (Subsonic `search3`/`search2`). | V1 must-have |
| FR-LIB-003 | Random albums/songs (V1.x; low cost, `getRandomAlbums`). | V1.x |
| FR-LIB-004 | Library reflects Navidrome state; metadata changes propagate on re-scan; Flow has no duplicate index. | V1 must-have (constraint) |
| FR-LIB-005 | Favorites: members star/unstar songs and albums; personal only (Navidrome per-user stars). | V1 must-have |

### 1.4 Streaming

| ID | Requirement | Priority |
|---|---|---|
| FR-STRM-001 | Members and admin can stream any library track. Stream URLs are issued by the Flow backend as **short-lived, per-user, scoped** URLs (or streaming proxy) that carry per-user Navidrome credentials. | V1 must-have |
| FR-STRM-002 | Streaming supports original quality (FLAC passthrough) and transcoded formats (MP3, Opus) per client request. | V1 must-have |
| FR-STRM-003 | Members cannot download files in V1.0.0 (`downloadRole` off for members; server config `enableDownloads` restricted to admin). Download capability is behind an admin feature flag, off by default. | V1 must-have (flag off) |
| FR-STRM-004 | Stream URLs contain no long-lived secrets; revoking a member invalidates their streams without restarting Navidrome. | V1 must-have |

### 1.5 Playlists

| ID | Requirement | Priority |
|---|---|---|
| FR-PL-001 | Admin can create, rename, delete, reorder playlists (Subsonic/native playlist API). | V1 must-have |
| FR-PL-002 | All members can list and stream admin playlists; they cannot modify them. Enforced server-side. | V1 must-have |
| FR-PL-003 | Playlist items can be added/removed/ordered by admin. | V1 must-have |
| FR-PL-004 | Smart playlists (query-based) — V2.0.0. | V2 |

### 1.6 Individual Playback

| ID | Requirement | Priority |
|---|---|---|
| FR-IND-001 | Every user has an isolated playback session: queue, position, volume, shuffle. | V1 must-have |
| FR-IND-002 | Playback state is persisted (Postgres) and restored on new session/device (resume where left off). | V1 must-have |
| FR-IND-003 | Controls: play/pause, next/prev, seek (client-side), queue add/remove, volume. | V1 must-have |
| FR-IND-004 | Sessions are independent: no API or event lets one user affect another's session. | V1 must-have (constraint) |

### 1.7 Synchronized Playback

| ID | Requirement | Priority |
|---|---|---|
| FR-SYN-001 | Admin can start a shared session (seeds from admin's current track/position or a chosen track). | V1 must-have |
| FR-SYN-002 | During a shared session, only the admin can: select/queue tracks, play/pause, seek, next/prev, stop session. | V1 must-have (constraint) |
| FR-SYN-003 | All connected members receive state updates in real time via authenticated WebSocket. | V1 must-have |
| FR-SYN-004 | Members stream the current shared track independently from Navidrome; no audio relay through the Flow server. | V1 must-have (constraint) |
| FR-SYN-005 | Late joiners sync to the current track/position with bounded drift (target < 3 s, target tolerance ±2 s). | V1 must-have |
| FR-SYN-006 | Periodic client heartbeats report position; coordinator emits `seek` corrections for clients beyond the drift threshold. | V1 must-have |
| FR-SYN-007 | Client reconnect mid-session: full state snapshot + version; stale events ignored via session version. | V1 must-have |
| FR-SYN-008 | Admin disconnect during active session: session pauses; 90 s grace; auto-fallback to Individual Mode after grace. | V1 must-have |
| FR-SYN-009 | Admin ends session: all members receive `session-ended`; individual sessions restored. | V1 must-have |
| FR-SYN-010 | Member disconnect/rejoin: re-snapshot on reconnect. | V1 must-have |
| FR-SYN-011 | Shared session state survives a Flow backend restart (Postgres persistence). | V1.x (V1 may tolerate loss; documented) |

### 1.8 Platform Mode

| ID | Requirement | Priority |
|---|---|---|
| FR-MODE-001 | Exactly one platform mode: `individual` or `synchronized`. Stored server-side. | V1 must-have |
| FR-MODE-002 | Only the admin can switch modes; all clients are notified via WebSocket. | V1 must-have (constraint) |
| FR-MODE-003 | Transition rules follow PRODUCT_SPEC §3.3 exactly. | V1 must-have |

### 1.9 Administration and Settings

| ID | Requirement | Priority |
|---|---|---|
| FR-ADM-001 | Admin can trigger a library re-scan (Navidrome `startScan`). | V1 must-have |
| FR-ADM-002 | Admin can view scan status. | V1 must-have |
| FR-ADM-003 | Admin can delete tracks/albums/files (Navidrome admin maintenance endpoints; files removed from the music folder when `deleteFilesOnRemove` semantics apply). | V1 must-have |
| FR-ADM-004 | Admin can manage platform settings (default theme, download flag, max concurrent streams, sync drift threshold). | V1 must-have |
| FR-ADM-005 | Admin can upload music via web drop-zone; files land in the music folder; scan is triggered. | V1 must-have |
| FR-ADM-006 | Upload validation: extension/size limits, folder structure (album subfolders), no path traversal, streaming writes. | V1 must-have |
| FR-ADM-007 | Full audit log of admin actions (membership decisions, mode switches, library mutations, settings changes). | V1.x |
| FR-ADM-008 | Multi-admin, fine-grained roles. | V2 |

### 1.10 Themes

| ID | Requirement | Priority |
|---|---|---|
| FR-THM-001 | Five shipped themes (pixel-art, cyberpunk, dark-gothic, ghibli-cozy, minimal-audiophile) as token definitions. | V1 must-have |
| FR-THM-002 | Members pick a personal theme; admin picks a platform default. Choices stored per user. | V1 must-have |
| FR-THM-003 | Theme switching applies without a full reload (token re-resolution). | V1 must-have |
| FR-THM-004 | Custom theme uploads / community themes. | V1.x |

---

## 2. Non-Functional Requirements

### 2.1 Performance

| ID | Requirement | Target |
|---|---|---|
| NFR-PERF-001 | Library browsing API p95 latency (excluding Navidrome internal latency) < 150 ms. | Measured in CI e2e |
| NFR-PERF-002 | WebSocket state delivery p95 < 200 ms on the operator's network (LAN: < 50 ms). | Measured in staging |
| NFR-PERF-003 | Synchronized drift: ≤ 2 s steady-state under 10 concurrent members on a 100 Mbps uplink. | Documented test |
| NFR-PERF-004 | Backend supports 200 concurrent stream URL issuances; Navidrome handles concurrent streams (its `maxConcurrentStreams` config). | Config |

### 2.2 Scalability and Simplicity

| ID | Requirement |
|---|---|
| NFR-SCALE-001 | V1.0.0 is a single Flow backend instance (no Redis, no multi-instance sync engine). |
| NFR-SCALE-002 | Architecture must allow a second Flow instance behind a shared Postgres without rewrite, at the cost of moving the sync event bus to Postgres NOTIFY or Redis (V2 work). |
| NFR-SCALE-003 | No microservices. No service mesh. No queues in V1. |

### 2.3 Reliability

| ID | Requirement |
|---|---|
| NFR-REL-001 | Individual playback state survives browser close and backend restart (Postgres). |
| NFR-REL-002 | Synchronized session survives client disconnect/reconnect without admin action. |
| NFR-REL-003 | Navidrome restart: Flow backend detects it (health probe) and reports "library temporarily unavailable"; playback queues are preserved; no corrupt state. |
| NFR-REL-004 | Flow backend restart mid-shared-session: session state is recoverable from Postgres (V1.x) or the session is safely ended and the platform returns to Individual Mode (V1 acceptable). |

### 2.4 Security (summary — full model in SECURITY.md)

| ID | Requirement |
|---|---|
| NFR-SEC-001 | Navidrome is not publicly reachable. Members cannot call Navidrome APIs directly. |
| NFR-SEC-002 | No Navidrome credentials in client responses, client storage, or URLs that outlive the request. |
| NFR-SEC-003 | All permission decisions are server-side; client "disabled UI" is cosmetic only. |
| NFR-SEC-004 | WebSocket requires the same session authentication as REST; a separate WS token is not required (JWT in query or initial handshake header). |
| NFR-SEC-005 | HTTPS only in production; HSTS set at Caddy. |
| NFR-SEC-006 | No secrets in the web bundle or in client-side storage beyond the session token (httpOnly cookie). |

### 2.5 Maintainability

| ID | Requirement |
|---|---|
| NFR-MAINT-001 | Go backend: clear package boundaries, one module. Linting (golangci-lint) in CI. |
| NFR-MAINT-002 | Web: TypeScript strict, ESLint + Prettier in CI. |
| NFR-MAINT-003 | All configuration via environment variables; no config files at runtime. |
| NFR-MAINT-004 | Database migrations are versioned and reversible where safe; migration runner is part of the backend (no separate tooling in V1). |

### 2.6 Compatibility

| ID | Requirement |
|---|---|
| NFR-COMPAT-001 | Web app: current + one prior versions of Chrome/Edge/Firefox/Safari; mobile browsers work for browsing and streaming (playback UX in browser is best-effort in V1). |
| NFR-COMPAT-002 | FLAC decoding: rely on browser MediaSource/WebCodecs fallback and MP3/Opus transcode when FLAC is not supported (documented matrix). |

---

## 3. Assumptions (documented, not invented)

| ID | Assumption |
|---|---|
| A-001 | The operator runs one Navidrome instance and one Flow stack on a single host (V1). Multi-instance is V2. |
| A-002 | The music folder is a Docker volume shared between Navidrome (ro or rw) and the Flow backend (rw). |
| A-003 | The operator's LAN is trusted; the internet-facing boundary is Caddy. |
| A-004 | Members are known, invited people; malicious member behavior is not a primary threat, but permission bypasses must still be impossible. |
| A-005 | Navidrome version: the checked-out source (`master`, go 1.27, SQLite-only, native API `/api`, Subsonic `/rest`, events SSE `/api/events`). |
| A-006 | One admin for V1; the schema supports multiple admins from day one. |
| A-007 | Timezone is irrelevant to playback; all timestamps are UTC server time. |

## 4. Unresolved Questions (open, to close before first code)

| ID | Question | Default if unanswered |
|---|---|---|
| U-001 | What is the exact Navidrome release to pin in `deploy/` (image tag)? | Latest tagged release at build time; pinned in compose file. |
| U-002 | Should member registration require a valid email (verified) or just a syntactically valid one? | Syntactic validation only in V1. |
| U-003 | Should Flow expose admin-triggered re-scan as a long-running job with progress, or fire-and-forget + status poll? | Fire-and-forget + `GET /admin/scan/status`. |
| U-004 | Session token lifetime: 8h? | 8h, refreshable while app open. |
| U-005 | Sync drift threshold default: 2000 ms? | 2000 ms, admin-configurable. |
| U-006 | Heartbeat interval from clients: 5 s? | 5 s. |
