# Flow — Requirements Specification

Version: 1.1.0-draft (supersedes 1.0.0-draft)
Method: Each requirement has a stable ID (FR-xxx functional, NFR-xxx non-functional, SR-xxx security). Implementation tasks in the roadmap reference these IDs.
Revisions in this version: browser authentication is opaque server-side sessions (ADR-002, FR-AUTH-002/004/005, NFR-SEC-004/006); streaming is the Flow gateway (ADR-001, FR-STRM-001/004, NFR-SEC-002); admin bootstrap is operator-assisted (ADR-003, FR-AUTH-001); provisioning is a durable state machine (ADR-004, FR-MEM-004/006/008); V1 restart policy is safe-end (ADR-005, FR-SYN-011, NFR-REL-004); sync events use version + event sequence (ADR-006); individual-session revision control (task F, FR-IND-005); staged upload pipeline (task G, FR-ADM-005/006); advanced metadata editing and destructive library ops moved out of the critical path (FR-ADM-003/009 → V1.x).

---

## 1. Functional Requirements

### 1.1 Authentication and Accounts

| ID | Requirement | Priority |
|---|---|---|
| FR-AUTH-001 | **Operator-assisted bootstrap (ADR-003):** the operator initializes Navidrome's first admin via Navidrome's own first-run mechanism, then supplies `FLOW_NAVIDROME_ADMIN_USERNAME` / `FLOW_NAVIDROME_ADMIN_PASSWORD` to Flow. Flow verifies the integration (login + read-back + config-gate check), stores the credential encrypted, and creates the Flow admin account from `FLOW_ADMIN_USERNAME` / `FLOW_ADMIN_PASSWORD`. Idempotent. Flow does **not** create the Navidrome first admin itself. | V1 must-have |
| FR-AUTH-002 | Admin (and members) authenticate via username/password and receive an **opaque server-side session token** (random; SHA-256 hash stored in Postgres; HttpOnly/Secure/SameSite cookie). No signed client tokens in V1 (ADR-002). | V1 must-have |
| FR-AUTH-003 | Passwords are hashed (argon2id) and never stored in plaintext in Flow's database. | V1 must-have |
| FR-AUTH-004 | Sessions are opaque, stored in Postgres (`sessions`), short-lived (e.g. 8 h), and immediately revocable server-side (logout/suspension = row delete). No stateless-JWT revocation list needed (ADR-002). | V1 must-have |
| FR-AUTH-005 | Logout deletes the server-side session and aborts streams registered to that session. Suspension/deletion revokes all sessions for the user and aborts all of that user's in-flight streams. | V1 must-have |
| FR-AUTH-006 | Admin can reset any member's password from the admin UI. | V1 must-have |
| FR-AUTH-007 | Login is rate-limited per IP and per username (e.g., 10 attempts / 15 min / IP). | V1 must-have |
| FR-AUTH-008 | Native clients (Android/desktop, V1.x) authenticate via a separately defined **bearer-session** mechanism (same opaque token model, bearer transport). Not implemented in V1 (reserved, ADR-002). | V1.x |
| FR-AUTH-009 | Approval, direct member creation, and admin password reset require an admin-supplied temporary Flow password communicated out-of-band. First login issues a restricted browser session with `mustChangePassword=true`; only `/auth/me`, `/auth/change-password`, and logout are available until the member replaces it. Temporary passwords are never returned by the API or written to audit/log output. | V1 must-have |

### 1.2 Membership

| ID | Requirement | Priority |
|---|---|---|
| FR-MEM-001 | Anonymous visitors can submit a membership request (name, email, desired username) from the login screen. | V1 must-have |
| FR-MEM-002 | Requests have states: `pending`, `approved`, `rejected`. State changes are audited (who, when, note). | V1 must-have |
| FR-MEM-003 | Admin can list pending requests, approve, reject (with optional note). | V1 must-have |
| FR-MEM-004 | On approval, a member account is provisioned via a **durable, retry-safe state machine** (ADR-004). Flow generates and encrypts the member's Navidrome credential and commits its ciphertext + key version **before** the first remote create call. Retries reuse that credential; an uncertain create is reconciled by username plus authentication with the durable credential. A user is active-capable only when `provisioning_state = complete`. | V1 must-have |
| FR-MEM-005 | Admin can create member accounts directly without a request. | V1 must-have |
| FR-MEM-006 | Admin can suspend a member (login blocked, data retained, all Flow sessions revoked immediately, in-flight streams aborted, cached Navidrome JWT evicted). | V1 must-have |
| FR-MEM-007 | Admin can reactivate a suspended member. | V1 must-have |
| FR-MEM-008 | Admin can delete a member account. Flow access is revoked immediately; Navidrome deprovisioning is a durable, retryable cleanup step whose pending/failed state remains visible until reconciled. Media files are never affected. | V1 must-have |
| FR-MEM-009 | Admin can view all members with status and last-active timestamps. | V1 must-have |

### 1.3 Music Library (proxy to Navidrome)

| ID | Requirement | Priority |
|---|---|---|
| FR-LIB-001 | Browse albums (with artwork, track listing, artist) and artists (discography). | V1 must-have |
| FR-LIB-002 | Search across songs, albums, artists (Subsonic `search3`/`search2`). | V1 must-have |
| FR-LIB-003 | Random albums/songs (V1.x; low cost, `getRandomAlbums`). | V1.x |
| FR-LIB-004 | Library reflects Navidrome state; metadata changes propagate on re-scan; Flow has no duplicate index. | V1 must-have (constraint) |
| FR-LIB-005 | Favorites: members star/unstar songs and albums; personal only (Navidrome per-user stars). | V1 must-have |
| FR-LIB-006 | Browse genres. No V1 route is registered; delivered in V1.x. | V1.x |

### 1.4 Streaming (via the Flow gateway — ADR-001)

| ID | Requirement | Priority |
|---|---|---|
| FR-STRM-001 | Members and admin can stream any library track **through the Flow streaming gateway** (`/audio/…`). Flow authenticates and authorizes every stream request and attaches the appropriate **server-side** Navidrome credential; clients never receive Navidrome credentials, JWTs, or internal addresses. | V1 must-have |
| FR-STRM-002 | Streaming supports original quality (`format=original` → pinned Navidrome `format=raw` passthrough) and transcoded formats (MP3, Opus) per client request; original files are never transcoded at rest. | V1 must-have |
| FR-STRM-003 | Members cannot download files in V1.0.0 (`downloadRole` off for members; deploy invariant `ND_ENABLEDOWNLOADS=false`). Download capability is behind an admin feature flag, off by default. | V1 must-have (flag off) |
| FR-STRM-004 | No client-side credential: a captured audio request is re-authorized per request against the member's own Flow session + status; suspending a member terminates their in-flight streams immediately without restarting anything. | V1 must-have |
| FR-STRM-005 | The gateway streams incrementally with cancellation/backpressure and faithfully forwards upstream status and safe representation headers. It preserves Range/206 seeking **when the selected upstream representation supports byte ranges** and must not synthesize `Accept-Ranges: bytes` for non-seekable transcoded output. | V1 must-have |

M2 implementation constraints: registration precedes every Navidrome lookup/open and is followed by an authoritative Flow account recheck; suspension is a registry block + active cancellation + generation-fenced JWT eviction. `offsetSeconds` maps only to Navidrome's integer `timeOffset` for an initial transcode seek and is not treated as HTTP Range. No M2 database migration or duplicate media index is permitted. The fixed M2 concurrent-stream cap is 100/user; runtime configurability remains FR-ADM-004/M5.

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
| FR-IND-005 | **One persisted individual session per user** shared across all that user's devices, with **optimistic concurrency (revision CAS)**: a stale write is rejected with 409 + the current session. V1 performs no automatic queue union or last-writer-wins merge; the client must reload or deliberately resubmit a replacement against the new revision. | V1 must-have |
| FR-IND-006 | Queue, current-track/index, seek, pause, track-end, shuffle, and volume changes are persisted immediately. While actively playing, position checkpoints may be client-throttled to at most one accepted write per 10 seconds. Browser unload/page-hide is best-effort only and is never the sole durability mechanism. | V1 must-have |

### 1.7 Synchronized Playback

| ID | Requirement | Priority |
|---|---|---|
| FR-SYN-001 | Admin can start a shared session (seeds from admin's current track/position or a chosen track). | V1 must-have |
| FR-SYN-002 | During a shared session, only the admin can: select/queue tracks, play/pause, seek, next/prev, stop session. | V1 must-have (constraint) |
| FR-SYN-003 | All connected members receive state updates in real time via authenticated WebSocket (session cookie + Origin check; no tokens in the query string — ADR-002). | V1 must-have |
| FR-SYN-004 | Members stream the current shared track **through the Flow gateway**; no audio relay through the Flow sync bus; no client holds Navidrome credentials (ADR-001). | V1 must-have (constraint) |
| FR-SYN-005 | Late joiners sync to the current track/position with bounded drift (target < 3 s, target tolerance ±2 s). | V1 must-have |
| FR-SYN-006 | Periodic client heartbeats report position; coordinator emits `seek` corrections for clients beyond the drift threshold. | V1 must-have |
| FR-SYN-007 | Client reconnect mid-session uses an **atomic subscribe-then-snapshot-and-drain** protocol: the server registers the connection for ordered events before reading the committed snapshot, sends that snapshot, drains buffered events above its `event_seq`, then switches the connection live. Stale events and gaps follow ADR-006. | V1 must-have |
| FR-SYN-008 | DJ disconnect during active session: playback position frozen, persisted 90 s grace deadline, auto-fallback to Individual Mode after grace. Lease generations are server-issued and every DJ command is fenced by the active authenticated connection plus a rotated opaque lease token; generation alone is insufficient (ADR-005). | V1 must-have |
| FR-SYN-009 | Admin ends session: all members receive `session-ended`; individual sessions restored. | V1 must-have |
| FR-SYN-010 | Member disconnect/rejoin: re-snapshot on reconnect. | V1 must-have |
| FR-SYN-011 | **V1: active shared sessions end safely on a Flow backend restart** (individual sessions persist and remain restorable) (ADR-005). Auto-resume of a shared session across restart = V1.x. | V1: safe-end (V1.x: auto-resume) |

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
| FR-ADM-003 | Admin can delete Navidrome index entries and, only through a separately confirmed operation, physical media files. Both behaviors are V1.x; V1 exposes neither a placeholder nor an ambiguous `deleteFile` switch. | V1.x |
| FR-ADM-004 | Admin can manage platform settings (default theme, reserved download flag, hard per-user concurrent-stream cap, sync drift threshold). | V1 must-have |
| FR-ADM-005 | Admin can upload one new album directory via web drop-zone. Staging is a sibling of the watched library on the **same filesystem**, outside Navidrome's mounted/watched path. After full validation and fsync, the album directory is atomically renamed into the library; existing destinations are rejected; scan status and crash recovery are recorded. | V1 must-have |
| FR-ADM-006 | Upload validation: extension/size limits, folder structure (album subfolders), no path traversal, no symlink, no collision-with-original, no partial-file visibility, streaming writes. | V1 must-have |
| FR-ADM-007 | V1 writes admin actions (membership decisions, mode switches, library mutations, settings changes) to an append-only audit log with correlation ID and outcome. V1.x adds the audit viewer and pruning controls. | V1 must-have (write path) / V1.x (viewer + pruning) |
| FR-ADM-008 | Multi-admin, fine-grained roles. | V2 |
| FR-ADM-009 | Advanced metadata editing (tag write + affected-album re-scan). **Moved out of the V1 critical path** — V1.x. | V1.x |

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
| NFR-PERF-004 | M6 establishes and documents the measured concurrent-stream ceiling of the release on the reference deployment. No capacity number is promised before that test. Per-user caps are enforced by Flow; Navidrome transcoding limits remain deploy-time configuration. | Measured at M6 |

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
| NFR-REL-004 | Flow backend restart mid-shared-session (V1, ADR-005): the shared session **ends safely** and the platform returns to Individual Mode; individual sessions remain persisted and restorable. Auto-resume of a shared session is V1.x. |

### 2.4 Security (summary — full model in SECURITY.md)

| ID | Requirement |
|---|---|
| NFR-SEC-001 | Navidrome is not publicly reachable. Members cannot call Navidrome APIs directly. |
| NFR-SEC-002 | No Navidrome credentials, JWTs, or internal addresses in client responses, client storage, or URLs. Audio flows through the Flow gateway with server-side credentials (ADR-001). |
| NFR-SEC-003 | All permission decisions are server-side; client "disabled UI" is cosmetic only. |
| NFR-SEC-004 | WebSocket requires the same session authentication as REST (opaque session cookie, validated server-side) **plus Origin validation**; no separate WS token, no token in the query string (ADR-002). |
| NFR-SEC-005 | HTTPS only in production; HSTS set at Caddy. |
| NFR-SEC-006 | No secrets in the web bundle or in client-side storage beyond the session cookie (HttpOnly). No signed tokens in V1. |
| NFR-SEC-007 | PostgreSQL stores credential ciphertext and key-version metadata only. Encryption master keys live in an external mounted secret/keyring; a database backup without that external keyring cannot decrypt Navidrome credentials. |
| NFR-SEC-008 | Key rotation adds a new external key version, switches new writes to it, re-encrypts all credential rows transactionally in bounded batches, verifies completion, and retires the old key only after restore requirements for retained backups are satisfied. |

### 2.5 Maintainability

| ID | Requirement |
|---|---|
| NFR-MAINT-001 | Go backend: clear package boundaries, one module. Linting (golangci-lint) in CI. |
| NFR-MAINT-002 | Web: TypeScript strict, ESLint + Prettier in CI. |
| NFR-MAINT-003 | Deployment configuration comes from environment variables and mounted secret files; runtime product settings live only in PostgreSQL. Secret key material is never stored in PostgreSQL. |
| NFR-MAINT-004 | Database migrations are versioned and reversible where safe; the startup runner acquires a PostgreSQL advisory lock so only one process migrates at a time. |

### 2.6 Compatibility

| ID | Requirement |
|---|---|
| NFR-COMPAT-001 | Web app: current + one prior versions of Chrome/Edge/Firefox/Safari; mobile browsers work for browsing and streaming (playback UX in browser is best-effort in V1). |
| NFR-COMPAT-002 | The web client requests original audio only when its tested browser capability check supports the representation; otherwise it requests a verified Navidrome MP3/Opus transcode. |

---

## 3. Assumptions (documented, not invented)

| ID | Assumption |
|---|---|
| A-001 | The operator runs one Navidrome instance and one Flow stack on a single host (V1). Multi-instance is V2. |
| A-002 | A single host filesystem root contains sibling `library/` and `.flow-staging/` directories. Flow mounts the root read-write; Navidrome mounts only `library/` read-only. This makes the final rename genuinely atomic and keeps staging outside the scanner. |
| A-003 | The operator's LAN is trusted; the internet-facing boundary is Caddy. |
| A-004 | Members are known, invited people; malicious member behavior is not a primary threat, but permission bypasses must still be impossible. |
| A-005 | Navidrome V1 target is release `v0.64.2` (source commit `101145742f4762164202cb4858c9126258bdc463`), pinned as `deluan/navidrome:0.64.2@sha256:38dc2727bfcfd5ede290f8ada114fc90368146f265ae4701ddddbcbe2a44ee52`. Source and required container-runtime contracts are verified separately. |
| A-006 | One admin for V1; the schema supports multiple admins from day one. |
| A-007 | Timezone is irrelevant to playback; all timestamps are UTC server time. |

## 4. Fixed V1 policy values and remaining verification

- Registration requires syntactically valid email only; email verification is not a V1 feature.
- Scan initiation is fire-and-forget plus status polling.
- Browser sessions last 8 hours with sliding refresh on ordinary API activity, not audio bytes.
- Default sync drift threshold is 2000 ms, heartbeat interval is 5 seconds, and DJ grace is 90 seconds; all three are database-backed runtime settings.
- Navidrome `v0.64.2` source behavior is verified at the tagged commit. The required runtime contract suite passed against the immutable image pin recorded in A-005; future claims outside that suite remain explicitly unverified until tested.
