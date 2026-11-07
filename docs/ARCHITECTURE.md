# Flow — Architecture

Version: 1.1.0-draft (supersedes 1.0.0-draft)
Key changes in this revision: Flow is the **only** client-facing streaming gateway (ADR-001); browser authentication uses **opaque server-side sessions** (ADR-002); bootstrap and membership follow ADR-003/004; future synchronized playback follows ADR-005/006; M3 individual persistence and browser lifecycle follow ADR-007.

---

## 1. System Overview

Flow is a modular monolith (Go) that adds a product layer on top of an unmodified Navidrome instance. The two systems communicate over HTTP on a private Docker network. Clients (web today; Android and Tauri later) talk **only** to Flow. **All audio flows through Flow**: the streaming gateway is the only path from a client to media (ADR-001).

```
                                   ┌─────────────────────────────┐
                                   │           Caddy              │  public (TLS)
                                   │  443/80 → route by prefix   │
            ┌──────────────┐      │                               │
            │  Web client  │──────►  /            → flow-web (static)
            │ (browser)    │      │  /api, /audio, /ws → flow-backend
            └──────────────┘      │                               │
                                   │  (navidrome routes: NONE — Navidrome
            ┌──────────────┐      │   is not mounted on any public
            │  Android    │──────►  endpoint)
            │ (V1.x)      │      │                               │
            └──────────────┘      │  flow-backend ──► Navidrome  │  internal-only
            ┌──────────────┐      │    (Subsonic / native API)   │
            │  Tauri (V1.x)│──────►  flow-backend ──► Postgres  │
            └──────────────┘      │    flow-backend ──► music volume (rw)
                                   └─────────────────────────────┘
```

Key properties:

- **Navidrome has no public ingress in V1.0.0.** The Caddyfile contains no route for Navidrome, and no Navidrome port is published. The Flow backend is the only process that can reach Navidrome's HTTP surface.
- **No client ever receives Navidrome credentials, Navidrome JWTs, or Navidrome internal addresses.** Audio responses are proxied by Flow with the appropriate server-side credentials attached after the proxy connection is established (ADR-001).
- **The Flow admin is not created by Flow in Navidrome.** The operator initializes Navidrome's first admin through Navidrome's own first-run mechanism, then hands those credentials to Flow (ADR-003).

### 1.1 Why a separate Flow backend at all (and not just customizing Navidrome's UI)?

1. **Authentication and membership are Flow's domain.** Navidrome's user API is admin-scoped and its auth is username/password or external-auth passthrough. Flow needs: registration requests, approval workflow, account suspension, audit — none of which exist in Navidrome.
2. **Synchronized playback has no home in Navidrome.** Its `jukeboxControl` endpoint targets hardware audio devices (`core/playback`), not peer-to-peer listener synchronization.
3. **Ownership and identity.** Flow's UI, themes, and product identity must be independent of Navidrome's release cycle.
4. **Streaming security.** A reverse-proxy or signed-URL arrangement would still hand Navidrome credentials to members. A gateway that authorizes per-request and attaches credentials server-side is the only model where members (and captured URLs) carry zero Navidrome material (ADR-001).

Navidrome remains the streaming engine, metadata index, and media store. Flow is the application and the only client-facing surface.

## 2. Component Boundaries (Go backend)

```
backend/internal/
├── api/          HTTP routes (standard `net/http`), middleware, request/response DTOs
├── auth/         argon2 hashing, opaque session tokens, hashed session storage, revocation
├── users/        user records, role/status lifecycle (active/suspended/deleted)
├── membership/   registration requests, approval/rejection, durable provisioning state machine (ADR-004)
├── streaming/    streaming gateway (ADR-001): per-request authn/z, Range proxy, credential attachment, active-stream registry
├── navidrome/    Navidrome HTTP client: Subsonic + native calls, admin credential cache, health
├── library/      read-only proxy to Navidrome Subsonic (browse, search, media)
├── playlist/     playlist CRUD (admin write, member read) via Navidrome
├── playback/     individual session state (Postgres-persisted per user, revision control)
├── sync/         shared session state machine + drift coordinator (single-writer executor, ADR-005/006)
├── ws/           WebSocket hub: authenticated connections, per-user channels, event protocol
├── uploads/      staged upload pipeline: staging dir → validation → atomic promote → scan trigger
├── themes/       theme catalog, per-user theme preferences
├── admin/        admin-only services: mode switching, member management, settings, scan trigger
└── config/       config loading, secrets, database pool, migrations
```

### 2.1 Dependency rule

- Leaf packages (`navidrome`, `auth`, `config`) know nothing about routes.
- `api` and `ws` are the only packages that build HTTP handlers.
- Domain packages (`users`, `membership`, `sync`, `playback`, `streaming`) are plain Go: no `http.Handler` in their public API. This keeps future native clients trivially servable and keeps packages testable.
- `sync` exposes a small event bus to `ws`; `ws` does not import `sync` internals — it subscribes to `sync.Events`.
- `streaming` owns the active-stream registry: every stream handle is indexed by both Flow session and user. Logout aborts streams opened by that session; suspension/deletion aborts every stream for the user (ADR-001).

### 2.2 What is in-memory vs. persisted

| Data | In-memory | Postgres | Notes |
|---|---|---|---|
| Opaque sessions (token hash → user, expiry) | no authoritative cache in V1 | `sessions` table is the source of truth | Indexed lookup by hash; revocation = row delete |
| Individual session (queue, position, volume, shuffle, intent, revision) | browser snapshot only | yes | One row per account; direct database reads and conditional revision UPDATE; no backend hot cache (ADR-007) |
| Shared sync session | yes | yes (checkpoint on every mutation, authoritative) | V1: crash ⇒ session ended safely (ADR-005); checkpoints are the recovery boundary |
| DJ lease | active connection + raw token | generation + token hash + connection id + grace deadline | Reconnect rotates the server-issued token/generation and fences the old socket; restart ends the session |
| Sync event sequence | yes | `sync_events` (bounded, per-session) | One authoritative state event per `event_seq`; every event carries current content version |
| Active stream handles | yes | no | Indexed by session + user; session flush on logout, user flush on suspend/delete |
| Navidrome JWT cache | yes | no | Per Flow user; JWT `exp` is parsed, refreshes are single-flight, and eviction advances a generation so an in-flight refresh cannot restore a suspended user's token |
| Per-user WS connections | yes | no | Transient; reconnect = re-auth + snapshot |
| Drift measurements | yes | no | Heartbeat data is not persisted |
| Users, requests, provisioning, uploads, settings, audit | no | yes | |

## 3. Technology Choices (evaluated, with alternatives)

| Concern | Chosen | Rejected / Alternative | Why |
|---|---|---|---|
| Language | Go (pinned toolchain, see DEVELOPMENT_GUIDE §1) | Rust, Node | User's expertise; single static binary; Navidrome is Go (shared idioms). Rust adds toolchain friction for no benefit at this scale. |
| HTTP routing | Go 1.27 `net/http` `ServeMux` | chi, gin | Current stdlib routing supports methods and path variables, keeps M0 dependency-light, and is sufficient for the modular monolith. Introduce a router dependency only if a measured need appears. |
| WebSocket lib | gorilla/websocket | nhooyr, coder/websocket | Most proven; Flow's WS surface is small. Auth happens on the initial HTTP upgrade (cookie session + Origin check), no special WS auth library needed. |
| Browser auth | Opaque server-side sessions (random tokens, SHA-256 hash stored in Postgres, HttpOnly SameSite=Lax cookies) | Stateless JWTs | Immediate revocation (logout/suspend) is a row delete, not a "revocation list bolted onto JWTs"; no token signing key to manage; CSRF handled by SameSite cookie (ADR-002). Future native clients get a separately defined bearer-session mechanism (same token model, bearer transport). |
| Database | PostgreSQL 16 | SQLite (Flow side) | Flow's data (users, sessions, provisioning, sync state) is relational with multi-row transactional flows. **Navidrome itself uses SQLite only** (its `db` package registers only a SQLite driver — verified in `db/db.go`), and it stays on its own database: two DBs, two ownerships, soft references across them. |
| Migrations | embedded, sequential SQL runner + PostgreSQL advisory lock | External migration framework | M0 needs a small reviewed runner, a ledger, per-migration transactions, and concurrency protection without another dependency. Policy in DEVELOPMENT_GUIDE §7. |
| Frontend | React 18 + TypeScript + Vite | Vue, Svelte, Solid | Spec preference; ecosystem fit for audio components and theming. |
| Styling | Tailwind CSS v4 + CSS custom properties | CSS-in-JS, styled-components | Tailwind utility classes consume token CSS variables; themes = token files, zero runtime cost. CSS-in-JS would conflict with the token architecture and add runtime. |
| Web state | TanStack Query + React context/controller | Additional client state libraries | Query owns library/auth data. One M3 controller adopts acknowledged CAS snapshots and owns local audio; context survives navigation (ADR-007). |
| Android (V1.x) | Kotlin + Compose + Media3 ExoPlayer | WebView wrapper | Explicit requirement: no WebView wrapper. Media3 gives background playback, notifications, Bluetooth controls. |
| Desktop (V1.x) | Tauri 2 (reuses web bundle) | Electron, native per-OS | Smaller binaries, reuses the web app; system media keys via Tauri's media events (V1.x). |
| Process model | Docker Compose, 4 services | k8s, systemd, bare binaries | Personal hosting; compose + Caddy is the documented Navidrome deployment model too. |
| Reverse proxy | Caddy | Nginx, Traefik | Spec preference; automatic HTTPS (ACME), clean route syntax, first-class WebSocket proxying. |
| Cache | None in V1 | Redis | Single instance + Postgres checkpoints make Redis unjustified. Revisit only for multi-instance (V2, NFR-SCALE-002). |
| Logging | slog (structured, JSON) | zap | stdlib; sufficient for the monolith; no framework lock-in. |
| Testing | `go test`; injected fake HTTP transport; disposable PostgreSQL; separate pinned-Navidrome contract job (DEVELOPMENT_GUIDE §6) | mock-heavy unit tests | State machines need unit coverage, while migration and external contracts require real dependency checks. |

### 3.1 Navidrome client strategy

Flow talks to Navidrome two ways, always server-side:

1. **Subsonic API (`/rest`)** — library browsing, search, stars, playlist operations, scan control, and **audio** (`stream`). Chosen over the native API for media because it is the stable, versioned, client-agnostic surface, supports per-user authentication, `timeOffset`, and transcoding parameters.
2. **Native API (`/api`)** — admin provisioning (user CRUD — Subsonic's `createUser`/`updateUser`/`deleteUser` are deliberately **501 Not Implemented**, verified in `server/subsonic/api.go`), user-library assignment, metadata refresh.

Credential model (server-side only, never client-visible — ADR-001/002):

- **Navidrome admin credential**: supplied by the operator (ADR-003). M0 verifies it from runtime secrets only. M1 persists ciphertext + key version in PostgreSQL; master keys remain in an external mounted keyring. The cached Navidrome JWT is memory-only.
- **Per-member Navidrome accounts**: provisioning encrypts and commits the generated password before remote creation (ADR-004). Retries reuse it. PostgreSQL never stores encryption key material.
- **Clients**: see Flow session cookies (browsers) / future bearer tokens (native). They never see Navidrome usernames, passwords, salts, or JWTs.

## 4. Deployment Architecture

```
┌────────────────────────────────────────────────────────────────┐
│ host                                                           │
│  ┌──────────────┐   ┌─────────────────┐   ┌────────────────┐  │
│  │ caddy:443    │ serves web bundle  │ navidrome      │       │
│  │ (TLS, routes)│ from /srv/flow-web │ (internal:4533)│       │
│  └──────┬───────┘                    └───────┬────────┘       │
│         │                                          │          │
│         ▼                                          ▼          │
│  ┌─────────────────┐   shared volume     ┌─────────────────┐  │
│  │ flow-backend:8080│◄──────────────────►│ Postgres:5432    │  │
│  │ (internal)       │   /music (rw)      │ (internal)       │  │
│  └──────────────────┘                    └─────────────────┘  │
└────────────────────────────────────────────────────────────────┘
filesystem root:
  music-root/library:       rw → flow-backend, ro → navidrome as /music
  music-root/.flow-staging: rw → flow-backend only (same filesystem, outside watched mount)
  nd_data:   rw → navidrome
  flow_pg:   rw → postgres
```

- Only Caddy publishes ports (80/443).
- `flow-backend`, `navidrome`, `postgres` are on the compose internal network; **none publish ports to the host**.
- `music-root/library` is the media source of truth. Flow writes only by atomically renaming a completely validated new album directory from the same-filesystem sibling `.flow-staging`; Navidrome sees only `library`.
- Separate staging and library volumes are not allowed in the canonical topology because they cannot guarantee atomic rename.

See DEPLOYMENT.md for the full compose file and Caddyfile.

## 5. Data Flow Diagrams

### 5.1 Bootstrap, Authentication, Approval

```
Operator (first run, once — ADR-003):
  1. initialize Navidrome's first admin via Navidrome's own first-run mechanism
     (verify the exact mechanism against the pinned version; see NAVIDROME_INTEGRATION §5)
  2. set FLOW_NAVIDROME_ADMIN_USERNAME / FLOW_NAVIDROME_ADMIN_PASSWORD in .env
Flow backend boot:
  - M0: migrate; health-probe Navidrome; log in as the supplied admin and verify login + native-user + gate read-back without persisting credentials
  - M1: encrypt/persist the verified credential and create/refresh the Flow admin user row (idempotent)
  - admin login: argon2 verify → issue opaque session (random token; SHA-256 hash in PG)
Visitor ──POST /api/auth/register-request──► Flow (membership_requests, status=pending)
Admin ──POST /api/admin/requests/{id}/approve──► durable provisioning state machine (ADR-004):
  pending → credential-ready (ciphertext committed) → creating-nd-user → linking → complete
  (any step: idempotent retry; failure: stuck-at-step with audit + admin retry;
   never active without `complete`)
Member ──POST /api/auth/login──► Flow: argon2 verify + status=active check → session cookie
```

### 5.2 Streaming Gateway (ADR-001)

```
Client (Flow session cookie)
  GET /api/library/albums?cursor=…  ─► Flow → Navidrome Subsonic (server-side member credential)
  GET /audio/{mediaId}?format=original  (optional standard Range header)
        Flow:
          1. verify the Flow session + current user status/provisioning state
          2. atomically register a cancelable handle by Flow session + user
             (a blocked/suspended user cannot register; fixed M2 cap: 100/user)
          3. re-read the authoritative Flow session/account after registration
          4. resolve media through the member-scoped adapter (404 if gone)
          5. open Navidrome /rest/stream with the registered context and member credential
             (`format=raw` for original; Range when supported; `timeOffset` for an initial transcode seek)
          6. stream bytes Navidrome→Flow→client with io.Copy (bounded incremental buffer)
        on client abort: cancel upstream (context cancel)
        on member suspension/delete: block registration + cancel all user handles + generation-evict JWT
        on reactivation: unblock registration (the next credential is acquired lazily)
  Client ──PUT /api/playback/session──► Flow → Postgres (revision CAS)
```

The register-before-recheck order is deliberate. Opening upstream before registration leaves a suspension window in which a new stream can escape cancellation. With the M2 order, a concurrent suspension either wins the registry lock and rejects the request, or sees and cancels the new handle; the post-registration account read closes the database-commit/lifecycle-callback window.

### 5.3 Synchronized Playback

```
Admin client ──► Flow sync service: ACTION (play/pause/seek/next/queue-change)
Flow sync (single-writer executor goroutine — the ONLY writer):
  1. validate state (409 on version mismatch, 410 if ended)
  2. mutate shared session (version++ only for content changes; event_seq++ once)
  3. Postgres checkpoint + one composite sync_events row in the same transaction
  4. M4B consumes the committed result and broadcasts envelope {seq, version, type, payload, ts}
Member client (audio via /audio gateway, control via WS):
  - on event: adjust player (seek to target = refTs + offset; or pause)
  - every 5 s: WS heartbeat {positionMs (monotonic-derived), bufferMs}
Flow sync coordinator:
  - drift_i = heartbeat.position - expected(t_now)   (see PLAYBACK_SYSTEM §3.3)
  - |drift_i| > threshold → send {type:"seek-correction", target} to that client only
Reconnect:
  - WS hub registers/buffers the subscriber before reading the committed snapshot
  - send snapshot, drain buffered seq > snapshot.eventSeq, then switch live
  - client drops seq <= snapshot.eventSeq; any later gap starts a new subscription cycle
```

### 5.4 Mode Transition

```
admin POST /api/platform/mode {mode:"synchronized", seed?}
Flow:
  - settings.mode = synchronized (PG)
  - sync.CreateSession(seed) (PG checkpoint v0)
  - commit and broadcast one composite `session-started` authoritative event (v0/seq1), which includes the new mode and full session state
members:
  - stop individual playback (auto-persisted) → begin shared session
admin POST /api/platform/mode {mode:"individual"}
Flow:
  - sync.EndSession(); settings.mode = individual
  - WS broadcast {session-ended}
  - members: restore individual sessions from PG
DJ disconnect (sync active):
  - future M4B WS DJ channel closes → trusted sync.DisconnectDJ() (freeze + persisted 90 s grace deadline, ADR-005)
  - WS broadcast {dj-disconnected, graceUntil}
Backend restart (V1 — ADR-005):
  - active shared sessions end safely; platform returns to Individual Mode
  - individual sessions remain persisted and restorable
```

M4A stops after the service/API/persistence boundary. `AcquireLease` accepts
only a server-issued connection ID supplied by the future authenticated M4B
hub; it is intentionally not a public REST route. `POST /api/platform/mode` is
the sole public start/end transition. Mode, shared checkpoint, durable event,
and `mode.changed` audit entry commit atomically.

## 6. Frontend Architecture

```
web/src/
├── app/            providers (QueryClient, Auth, Theme, Sync), router
├── features/
│   ├── auth/       login, register-request screens
│   ├── library/    album/artist/browse/search screens (Query-backed)
│   ├── player/     persistent player bar: transport controls, queue drawer, Media Session API
│   ├── sync/       sync-session UI: shared queue, DJ status, "you're 1.8s behind" indicator
│   ├── playlists/  admin CRUD + member read-only list
│   ├── admin/      requests, members, settings, uploads, scan
│   └── themes/     theme picker + token application
├── components/     shared UI primitives (Card, Button, AlbumGrid, …)
├── lib/            api client (fetch + cookies), ws client (auto-reconnect + resync), media (audio element wrapper, format negotiation: FLAC native or Opus transcode fallback)
└── themes/         token CSS files (5 themes)
```

M3 uses one account-keyed `PlayerProvider`, one `PlayerController`, and one
`<audio>` element. Library views consume the shared context; application navigation
preserves both the element and source. The controller serializes mutations and
adopts acknowledged state. A 409 or newer REST snapshot pauses local audio and
restores the authoritative state without an automatic retry or queue merge.

Format selection uses `HTMLMediaElement.canPlayType` for the song's content type,
then FLAC/Opus capability, with MP3 fallback. Original seeks use supported native
Range behavior; a transcode reloads `/audio/{id}` with integer `offsetSeconds`.
Replacing/retrying a source and provider teardown cancel the old stream with
pause/remove-src/load. Persisted preferences remain M13. Optional Media Session
actions invoke the same persisted controls and are cleaned up with the provider.

Position events render only. The provider checkpoints active media every 10
seconds and refreshes REST every 15 seconds/on visible focus or online. Commands
persist immediately. Restore has no age cutoff; a known completed track resumes
locally from zero, and restored playing intent never implies autoplay. Failed
stream loads preserve the queue and offer explicit retry. Multi-device control
coordinates durable state, with no exclusive device lease or remote audio timing.
API §4 and PLAYBACK_SYSTEM §2 own the detailed contracts.

The component trees also describe later V1 milestones. No synchronized executor,
WebSocket, shared state, upload, or theme package is implemented as part of M3.

## 7. Key Architectural Invariants

1. **Server authority.** Mode, permissions, sync state: Flow decides. Clients render.
2. **Flow is the only gateway to audio and to Navidrome.** No client ever holds or sees Navidrome credentials, JWTs, or internal addresses (ADR-001).
3. **Browser sessions are opaque and revocable.** Token hashes live in Postgres; revocation is immediate; no signed client tokens in V1 (ADR-002).
4. **Two databases, two ownerships.** Flow's Postgres: users, sessions, provisioning, sync state, settings. Navidrome's SQLite: media index, its own users. Never a shared schema; cross-system consistency is via the durable state machine (ADR-004), not distributed transactions.
5. **Single writer for sync state.** One executor goroutine mutates the shared session; every command commits one checkpoint + one authoritative event before broadcast. `version` tracks content; `event_seq` tracks every authoritative event (ADRs 005/006).
6. **V1 restart policy: shared sessions end safely; individual sessions persist.** No auto-resume of shared sessions across backend restarts (ADR-005).
7. **Feature flags, not code paths, for V2 capabilities** (downloads, themes, roles): the `settings` table carries booleans; a flag-off feature is unimplemented on the client side but the API shape is reserved.

## 8. Configuration Precedence (deployment vs. runtime vs. Navidrome)

| Layer | What | Where | Who changes it |
|---|---|---|---|
| Deployment config | DSN, Navidrome URL, secret-file paths, ports, log level, music root | env vars + mounted secrets | Operator, at deploy time |
| Runtime settings | platform mode, default theme, sync tuning (drift threshold, heartbeat interval, DJ grace period), feature flags | Postgres `settings` (singleton row) | Admin, via UI/API, live |
| Navidrome config | music folder, transcoding, download/sharing/artwork gates, auth rate limits | Navidrome env / config file | Operator, at Navidrome deploy time (invariant: `ND_ENABLEDOWNLOADS=false`, `ND_ENABLEARTWORKUPLOAD=false`, `ND_ENABLESHARING=false` until Flow-gated downloads ship) |

Rules:

- Deployment env/mounted secrets win over compiled defaults; PostgreSQL never stores master encryption keys.
- Runtime settings have **no** env override (except at bootstrap: `settings` seeds defaults, admin edits thereafter). There is exactly one source of truth per setting — env **or** DB, never both.
- Navidrome config is owned by the operator; Flow documents its invariants (NAVIDROME_INTEGRATION §7) and verifies them at bootstrap (reads back the relevant flags; fails loudly if a gate is open).
