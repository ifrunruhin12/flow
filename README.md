# Flow — Private Music Streaming & Listening Platform

A privately hosted music platform that sits on top of [Navidrome](https://github.com/navidrome/navidrome), providing a custom application experience, authentication, membership management, administration, and two distinct listening modes: individual and synchronized.

## What Flow Does

Flow is **not** a public streaming service. It is a single admin-curated music library with read-only listener access for approved members. The admin owns the catalog; members stream it, never modify it.

- One central music library, owned by a single administrator
- Two listening modes: **Individual** (independent playback) and **Synchronized** (shared, server-coordinated playback)
- Registration requests approved manually by the admin
- Members are read-only listeners
- Distinctive, themeable visual identity
- Web client in V1.0.0; Android and Tauri desktop in later milestones

## Repository Layout

```text
flow/
├── README.md                  ← this file
├── docs/                      ← all engineering documents
│   ├── PRODUCT_SPEC.md
│   ├── REQUIREMENTS.md
│   ├── ARCHITECTURE.md
│   ├── DATABASE_SCHEMA.md
│   ├── API_SPECIFICATION.md
│   ├── PLAYBACK_SYSTEM.md
│   ├── NAVIDROME_INTEGRATION.md
│   ├── SECURITY.md
│   ├── DEPLOYMENT.md
│   ├── DEVELOPMENT_GUIDE.md
│   └── ROADMAP.md
├── backend/                   ← Go modular monolith (auth, users, music proxy, sync engine, themes, admin)
│   ├── cmd/
│   ├── internal/
│   │   ├── auth/              ← session management, JWT signing/verification, password hashing
│   │   ├── users/             ← user CRUD, status lifecycle, admin actions
│   │   ├── membership/        ← registration requests, approval/rejection, suspension
│   │   ├── library/           ← read-only music metadata, search, favorites (proxy to Navidrome)
│   │   ├── navidrome/         ← Navidrome HTTP client: Subsonic, native API, config
│   │   ├── playlists/         ← admin playlist CRUD, member read access
│   │   ├── playback/          ← individual session state, shared session state, WebSocket hub
│   │   ├── sync/              ← synchronized playback coordinator: event broadcast, drift correction
│   │   ├── themes/            ← theme definitions, user theme preferences
│   │   ├── admin/             ← admin-only actions: mode switching, member management, settings
│   │   ├── uploads/           ← file write to shared volume, scan trigger
│   │   ├── api/               ← HTTP routes, middleware, request/response types
│   │   ├── ws/                ← WebSocket hub, auth, event channels
│   │   └── config/            ← config loading, secrets, database pool
│   └── migrations/            ← Postgres migrations
├── web/                       ← React + TypeScript + Vite + Tailwind
│   ├── src/
│   │   ├── components/
│   │   ├── pages/
│   │   ├── player/            ← persistent player component
│   │   ├── sync/              ← synchronized mode UI
│   │   ├── themes/            ← theme definitions (CSS custom properties)
│   │   ├── hooks/
│   │   └── lib/
│   └── ...
├── android/                   ← Kotlin + Compose + Media3 (post-V1)
├── desktop/                   ← Tauri 2 wrapper (post-V1)
├── deploy/
│   ├── docker-compose.yml     ← production compose: navidrome, flow-backend, flow-web, caddy
│   ├── caddy/
│   │   └── Caddyfile
│   └── migrations/            ← optional migration scripts
└── .github/                   ← CI/CD workflows
```

## Build & Run (V1.0.0, Web only)

**Prerequisites:** Docker, a music directory containing FLAC/MP3/AAC files, a Navidrome install (or the bundled compose file in `deploy/`).

### 1. Navidrome

Point Navidrome at your music folder. Flow's Docker Compose includes a Navidrome service by default; configure the volume mount in `deploy/docker-compose.yml`:

```yaml
navidrome:
  volumes:
    - /path/to/your/music:/music:ro
    - navidrome_data:/data
```

Navidrome must be reachable from the Flow backend on the Docker internal network. No external ports needed.

### 2. Flow Backend

```bash
docker compose -f deploy/docker-compose.yml up -d
```

The backend runs Postgres migrations on startup, bootstraps the admin account from environment variables (`FLOW_ADMIN_USERNAME`, `FLOW_ADMIN_PASSWORD`), and creates the corresponding Navidrome admin user via the native API.

### 3. Flow Web

Build the web app and serve it:

```bash
cd web && npm install && npm run build
```

The built static files are mounted by Caddy in production. For development:

```bash
cd web && npm run dev   # Vite dev server, proxies API to backend
```

### 4. First Login

Navigate to your Caddy domain. Log in with the admin credentials. The membership request form is visible to anonymous visitors from the login screen.

## Key Architecture Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Backend | Go modular monolith | Single deployable, clear internal boundaries, user already experienced |
| Database | PostgreSQL | Durable, multi-user, supports Flow's own auth/membership/state alongside Navidrome's SQLite |
| Navidrome access | Internal network only | Members never touch Navidrome's admin endpoints; Navidrome publishes **zero** public ports (compose invariant + deployment test) |
| Per-user credentials | Flow mints short-lived, per-user Navidrome stream URLs (member's own cached JWT) | Members can never see admin credentials or other members' credentials |
| Synchronized playback | In-memory engine + Postgres checkpoints | Single instance in V1; no Redis; crash-safe via checkpoint-or-end |
| Web serving | Caddy-served static bundle (separate from the API container) | Caddy handles TLS + routing + security headers; SPA is a plain static site |
| Upload | Shared volume write + scan trigger | Navidrome watches the folder; no custom file endpoint needed; original files stay intact |
| Client in V1.0.0 | Web only | Reduce delivery risk; same API contract supports Android/desktop later |

## V1.0.0 Scope

**In:**
- Admin authentication and account bootstrap
- Membership request → approval → member account lifecycle
- Read-only music library (browse, search, stream)
- Persistent player with queue
- Admin-managed playlists, read-only for members
- Individual playback mode
- Synchronized playback mode (WebSocket, server-authoritative)
- Basic theme system (5 starter themes, per-user selection)
- FLAC/MP3/AAC support
- Docker Compose deployment with Caddy
- Admin upload via shared volume

**Deferred to V1.x / V2.0.0:**
- Android app (background playback, offline)
- Tauri desktop app
- Offline downloads for members
- Community themes
- Multiple admin accounts / roles beyond admin + member
- Advanced social features (chat, listening history sharing)

See `docs/ROADMAP.md` for the full milestone breakdown.

## Documentation Index

| File | Contents |
|---|---|
| `docs/PRODUCT_SPEC.md` | Product vision, user stories, listening modes, theme philosophy |
| `docs/REQUIREMENTS.md` | Functional and non-functional requirements, traceable to V1 scope |
| `docs/ARCHITECTURE.md` | System architecture, component boundaries, tech choices, trade-offs |
| `docs/DATABASE_SCHEMA.md` | Complete Postgres schema, migrations, relationships |
| `docs/API_SPECIFICATION.md` | REST + WebSocket contracts, all endpoints, event schemas |
| `docs/PLAYBACK_SYSTEM.md` | Synchronized playback protocol, drift model, reconnection, mode transitions |
| `docs/NAVIDROME_INTEGRATION.md` | Subsonic API usage, auth strategy, user provisioning, limitations |
| `docs/SECURITY.md` | Auth model, credential isolation, API protection, upload validation, threat model |
| `docs/DEPLOYMENT.md` | Docker Compose, Caddy config, environment variables, backup strategy |
| `docs/DEVELOPMENT_GUIDE.md` | Local dev workflow, naming, branching, CI/CD, testing strategy |
| `docs/ROADMAP.md` | V1.0.0 → V1.x → V2.0.0 milestones and task breakdown |
