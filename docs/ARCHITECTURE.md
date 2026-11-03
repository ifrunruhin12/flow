# Flow — Architecture

Version: 1.0.0-draft

---

## 1. System Overview

Flow is a modular monolith (Go) that adds a product layer on top of an unmodified Navidrome instance. The two systems communicate over HTTP on a private Docker network. Clients (web today; Android and Tauri later) talk **only** to Flow.

```
                                   ┌─────────────────────────────┐
                                   │           Caddy              │  public (TLS)
                                   │  443:80 → route by prefix   │
            ┌──────────────┐      │                               │
            │  Web client  │──────►  /            → flow-web (static)
            │ (browser)    │      │  /api, /ws  → flow-backend
            └──────────────┘      │                               │
                                   │  (navidrome routes: NONE — Navidrome
            ┌──────────────┐      │   is not mounted on any public
            │  Android    │──────►  endpoint)
            │ (V1.x)      │      │                               │
            └──────────────┘      │  flow-backend ──► Navidrome  │  internal
            ┌──────────────┐      │    (REST / Subsonic)        │
            │  Tauri (V1.x)│──────►  flow-backend ──► Postgres  │
            └──────────────┘      │    flow-backend ──► music volume (rw)
                                   └─────────────────────────────┘
```

Key property: **Navidrome has no public ingress in V1.0.0.** The Caddyfile contains no route for Navidrome. The Flow backend is the only process that can reach Navidrome's HTTP surface, using per-user credentials created by Flow.

### 1.1 Why a separate Flow backend at all (and not just customizing Navidrome's UI)?

1. **Authentication and membership are Flow's domain.** Navidrome's user API is admin-scoped and its auth is username/password or external-auth passthrough. Flow needs: registration requests, approval workflow, account suspension, audit — none of which exist in Navidrome.
2. **Synchronized playback has no home in Navidrome.** Its `jukeboxControl` endpoint targets hardware audio devices (`core/playback`), not peer-to-peer listener synchronization.
3. **Ownership and identity.** Flow's UI, themes, and product identity must be independent of Navidrome's release cycle.

Navidrome remains the streaming engine, metadata index, and media store. Flow is the application.

## 2. Component Boundaries (Go backend)

```
backend/internal/
├── api/          HTTP routes (chi), middleware, request/response DTOs
├── ws/           WebSocket hub (gorilla/websocket), per-user channels, event protocol
├── auth/         argon2 hashing, JWT issuing/verification, session revocation store
├── users/        user records, role/status lifecycle (pending/active/suspended/deleted)
├── membership/   registration requests, approval/rejection, provisioning orchestration
├── admin/        admin-only services: mode switching, member management, settings, scan trigger
├── library/      read-only proxy to Navidrome Subsonic (browse, search, media)
├── playlists/    playlist CRUD (admin write, member read) via Navidrome
├── playback/     individual session state (Postgres-persisted per user)
├── sync/         shared session state machine + drift coordinator (in-memory, Postgres snapshots)
├── navidrome/    Navidrome HTTP client: per-user Subsonic calls, admin native calls, health
├── uploads/      file writes into the music volume + scan trigger
├── themes/       theme catalog, per-user theme preferences
├── settings/     platform settings (mode, flags, drift threshold, default theme)
└── db/           Postgres pool, migrations (golang-migrate or embedded SQL)
```

### 2.1 Dependency rule

- Leaf packages (`navidrome`, `auth`, `db`) know nothing about routes.
- `api` and `ws` are the only packages that build HTTP handlers.
- Domain packages (`users`, `membership`, `sync`, `playback`) are plain Go: no `http.Handler` in their public API. This keeps the Android/desktop backends (same API) trivially servable and keeps packages testable.
- `sync` exposes a small event bus to `ws`; `ws` does not import `sync` internals — it subscribes to `sync.Events`.

### 2.2 What is in-memory vs. persisted

| Data | In-memory | Postgres | Notes |
|---|---|---|---|
| Individual session (queue, position, volume) | yes | yes | Postgres is source of truth; memory is hot cache |
| Shared sync session | yes | yes (snapshot on every admin action) | Crash → restore from last snapshot, or end session safely |
| Per-user WS connections | yes | no | Transient |
| Drift measurements | yes | no | Heartbeat data is not persisted |
| Users, requests, playlists pointer, settings | no | yes | |

## 3. Technology Choices (evaluated, with alternatives)

| Concern | Chosen | Rejected / Alternative | Why |
|---|---|---|---|
| Language | Go 1.22+ | Rust, Node | User's expertise; single static binary; Navidrome is Go (shared mental model, similar idioms). Rust adds toolchain friction for no benefit at this scale. |
| Web framework | chi v5 | net/http raw, gorilla/mux, gin | Navidrome uses chi (code similarity); small, no hidden magic; router groups + middleware fit the layered auth model. |
| WebSocket lib | gorilla/websocket | nhooyr, coder/websocket | Most proven; Flow's WS surface is small. `nhooyr` is nicer but less battle-tested for auth-on-upgrade patterns. |
| Database | PostgreSQL 16 | SQLite (Flow side) | Flow's data (users, requests, sessions, audit) is relational with real multi-row transactional flows (approval = user + provisioning state). **Navidrome itself uses SQLite only** (its `db` package registers only a SQLite driver — verified in source, `db/db.go`), and it stays on its own database: two DBs, two ownerships, soft references across them. |
| Migrations | embedded SQL + golang-migrate | Schemer, hand-rolled | Simple, reviewable, single-file-per-change. |
| Frontend | React 18 + TypeScript + Vite | Vue, Svelte, Solid | Spec preference; ecosystem fit for audio components and theming. |
| Styling | Tailwind CSS v4 + CSS custom properties | CSS-in-JS, styled-components | Tailwind utility classes consume token CSS variables; themes = token files, zero runtime cost. CSS-in-JS would conflict with the token architecture and add runtime. |
| Web state | TanStack Query (server state) + Zustand (client state) | Redux, Jotai | Query caches the read-heavy library APIs well and gives retry/backoff for real-time-ish lists. Minimal store for player/sync state. |
| Android (V1.x) | Kotlin + Compose + Media3 ExoPlayer | WebView wrapper | Explicit requirement: no WebView wrapper. Media3 gives background playback, notifications, Bluetooth controls. |
| Desktop (V1.x) | Tauri 2 (reuses web bundle) | Electron, native per-OS | Smaller binaries, reuses the web app; system media keys via Tauri's media events (V1.x). |
| Process model | Docker Compose, 4 services | k8s, systemd, bare binaries | Personal hosting; compose + Caddy is the documented Navidrome deployment model too. |
| Reverse proxy | Caddy | Nginx, Traefik | Spec preference; automatic HTTPS (ACME), clean route syntax, first-class WebSocket proxying (`forward` handles WS upgrades natively). |
| Cache | None in V1 | Redis | Spec says "only if justified." Single instance + Postgres snapshots make Redis unjustified. Revisit only if multi-instance is required. |
| Logging | slog (structured, JSON) | zap | stdlib in 1.21+; sufficient for the monolith; no framework lock-in. |
| Testing | go test + httptest + testcontainers (Postgres) | mock-heavy unit tests | Sync engine and membership flow are state machines; integration tests against real Postgres catch what mocks hide. |

### 3.1 Navidrome client strategy (decision with trade-offs)

Flow talks to Navidrome two ways:

1. **Subsonic API (`/rest`)** — for library browsing, search, streaming URL construction, playlist operations, scan control. Reason: it is the stable, versioned, client-agnostic API that Navidrome guarantees for third-party clients. It supports per-user authentication (each member's own credentials), so Flow can proxy user-scoped reads without ever holding a member's password.
2. **Native API (`/api`)** — only for admin provisioning: creating/deleting member users (`POST /api/user`, `DELETE /api/user/{id}`), user-library assignment, metadata refresh, scan control. Reason: Subsonic's `createUser`/`updateUser`/`deleteUser` are deliberately **501 Not Implemented** in Navidrome (verified in `server/subsonic/api.go`); the native API is where user management actually lives.

Credential model:
- Flow stores **one admin** Navidrome account (created at bootstrap; its Navidrome JWT is held server-side only, in the backend process, refreshed via Navidrome's `/auth/login`).
- For each member, Flow creates a Navidrome user with a server-generated random password, stored **hashed** in Flow's DB (argon2id), decrypted to the exact random string only at call time via a stored ciphertext. Wait — cleaner: Flow calls Navidrome's `/auth/login` as the member to obtain a short-lived Navidrome JWT per session, and **caches the JWT server-side** (per user, in memory + encrypted at rest optional). Members' Navidrome credentials are never delivered to any client.

**Trade-off documented:** storing random per-member Navidrome passwords server-side is a necessary evil because Navidrome users are provisioned programmatically. It is mitigated by: passwords generated with `crypto/rand`, stored with AES-GCM under a key from the `FLOW_NAVIDROME_KEY` env var, used only inside the backend process. Alternative considered: external-auth (reverse-proxy) mode for Navidrome — rejected because it requires the same trusted-source network configuration as our chosen model, and it would grant every Flow member a working Navidrome session that could reach admin endpoints if Navidrome were ever exposed. The credential-isolation model above keeps members' Navidrome access scoped to stream/search/browse only, because Flow never hands out Navidrome tokens.

## 4. Deployment Architecture

```
┌────────────────────────────────────────────────────────────────┐
│ host                                                           │
│  ┌──────────────┐   ┌─────────────────┐   ┌────────────────┐  │
│  │ caddy:443    │──►│ flow-web:80    │   │ navidrome      │  │
│  │ (TLS, routes)│   │ (static only)  │   │ (internal:4533)│  │
│  └──────┬───────┘   └─────────────────┘   └───────┬────────┘  │
│         │                                          │          │
│         ▼                                          ▼          │
│  ┌─────────────────┐   shared volume     ┌─────────────────┐  │
│  │ flow-backend:8080│◄──────────────────►│ Postgres:5432    │  │
│  │ (internal)       │   /music (rw)      │ (internal)       │  │
│  └──────────────────┘                    └─────────────────┘  │
└────────────────────────────────────────────────────────────────┘
volumes:
  music:     rw → flow-backend (uploads), ro → navidrome (indexing/streaming)
  nd_data:   rw → navidrome
  flow_pg:   rw → postgres
```

- Only Caddy publishes ports (80/443, or host-mapped 80/443).
- `flow-backend`, `navidrome`, `postgres` are on the compose internal network; none publish ports to the host.
- `/music` is the single source of truth for audio files. Flow writes; Navidrome reads and streams.

See DEPLOYMENT.md for the full compose file and Caddyfile.

## 5. Data Flow Diagrams

### 5.1 Authentication and Approval

```
Visitor ──POST /api/auth/register-request──► Flow (users table, status=pending)
                                                    │
Admin ──GET  /api/admin/requests/pending◄──────────┘
Admin ──POST /api/admin/requests/{id}/approve
        Flow:
          1. users: pending → approved (PG)
          2. navidrome: POST /api/user (admin JWT) create user, libs=default
          3. store provisioned flag + navidrome_user_id (PG, same tx as #1)
          4. if Navidrome call fails → status stays pending, retry allowed
Member ──POST /api/auth/login (Flow)
        Flow: verify argon2 hash, issue Flow JWT, cache Navidrome JWT for user
```

### 5.2 Individual Playback

```
Client (authenticated Flow JWT)
  GET /api/library/albums?cursor=…     ─► Flow → Navidrome Subsonic (member JWT)
  GET /api/streams/{mediaId}          ─► Flow returns signed short-lived stream URL
                                           (Navidrome /rest/stream?u=…&p=…&s=…&c=flow,
                                            15-min expiry, user-scoped)
Client ──▶ Navidrome /rest/stream ──▶ audio
Client ──PATCH /api/playback/session  (queue, position, volume)  ─► Flow → Postgres
```

### 5.3 Synchronized Playback

```
Admin client ──▶ Flow sync service: ACTION (play/pause/seek/next/queue-change)
Flow sync:
  1. mutate shared session (version++, reference wall-clock t0)
  2. persist snapshot (PG)
  3. broadcast WS event to all connected members {type, sessionVersion, payload, serverTs}
Member client:
  - on event: adjust player (seek to target = t0 + offset; or pause)
  - every 5 s: POST heartbeat {positionMs, bufferMs} (or WS message)
Flow sync coordinator:
  - drift_i = heartbeat.position - expected(t_now)
  - |drift_i| > threshold → send {type:"seek", target} to that client only
Reconnect:
  - client GETs /api/sync/state (full snapshot + version + serverTs)
  - client discards any queued events older than snapshot.version
```

### 5.4 Mode Transition

```
admin POST /api/admin/mode {mode:"synchronized", seedTrackId?, seedPositionMs?}
Flow:
  - settings.mode = synchronized (PG)
  - sync.CreateSession(seed) (PG snapshot v0)
  - WS broadcast {type:"mode-changed", mode:"synchronized", session}
members:
  - stop/queue individual playback → restore on session end
  - begin playing session.currentTrack at session.position
admin POST /api/admin/mode {mode:"individual"}
Flow:
  - sync.EndSession(); settings.mode = individual
  - WS broadcast {type:"session-ended"}
  - members: restore individual sessions from PG
Admin disconnect (sync active):
  - WS admin channel closes → sync.MarkDJDisconnected() → WS broadcast {type:"dj-paused"}
  - 90 s timer → sync.EndSession(force) → mode fallback (rule FR-SYN-008)
```

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
├── lib/            api client (fetch), ws client (auto-reconnect + resync), media (audio element wrapper, format-negotiation: FLAC native or Opus transcode fallback)
└── themes/         token CSS files (5 themes)
```

Audio playback in the browser: a single `<audio>` element owned by the player module (context so it survives route changes). The sync module drives the same element when mode = synchronized. Format negotiation: if `FLAC` is decodable (`Audio.isTypeSupported('audio/flac')`), stream original; otherwise request Navidrome transcoding to `ogg` (Opus) at 128 kbps — a per-user decision, cached.

## 7. Key Architectural Invariants

1. **Server authority.** Mode, permissions, sync state: Flow decides. Clients render.
2. **No Navidrome in the public surface.** Clients cannot reach Navidrome. Stream URLs are Navidrome URLs, but members only ever receive URLs built *by Flow* with *member-scoped credentials*.
3. **Two databases, two ownerships.** Flow's Postgres: users, requests, sessions, settings. Navidrome's SQLite: media index, its own users. Never a shared schema.
4. **In-memory sync, Postgres checkpoint.** The shared session object is process-local; every admin mutation checkpoints to Postgres. Restart = restore-or-end, never corrupt.
5. **Feature flags, not code paths, for V2 capabilities** (downloads, themes, roles): the `settings` table carries booleans; a flag-off feature is unimplemented on the client side but the API shape is reserved.
