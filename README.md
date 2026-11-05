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
├── backend/                   ← Go modular monolith
│   ├── cmd/flowd/             ← process composition and graceful lifecycle
│   ├── internal/api/          ← JSON API, auth/CSRF/role middleware, M2 routes
│   ├── internal/auth/         ← password hashing and opaque sessions
│   ├── internal/bootstrap/    ← encrypted admin-link bootstrap and rotation
│   ├── internal/config/       ← environment loading and validation
│   ├── internal/credentials/  ← external AES-GCM keyring
│   ├── internal/database/     ← PostgreSQL pool and advisory-locked migrations
│   ├── internal/httpserver/   ← health/readiness endpoints
│   ├── internal/membership/   ← durable member provisioning and lifecycle
│   ├── internal/library/      ← Flow-owned library models and pagination
│   ├── internal/navidrome/    ← redacting Navidrome adapter and JWT cache
│   ├── internal/playlist/     ← member reads and admin-curated playlist logic
│   ├── internal/streaming/    ← secure audio/artwork gateway and lifecycle registry
│   ├── integration/           ← opt-in real Navidrome contract suite
│   └── migrations/            ← embedded milestone-owned SQL
├── web/                       ← React/TypeScript M2 browser application and local player
├── deploy/
│   ├── docker-compose.dev.yml ← development PostgreSQL + Navidrome only
│   └── .env.example           ← backend development configuration template
├── dev/                       ← isolated fixture music root and helper script
├── docs/                      ← product, architecture, contracts, and ADRs
├── .github/workflows/         ← fast backend and pinned-Navidrome jobs
└── CHANGELOG.md
```

Production Compose/Caddy and later domain packages are introduced by their roadmap milestones. M2 contains the library and one browser-local player; it intentionally contains no M3 playback persistence/queue state, uploads, themes, or synchronized-playback placeholders.

## Current Status

Phase 1 architecture reconciliation, M0, and the M1 authentication/membership implementation are present. M2 library, artwork, playlists, and secure streaming are implemented with fast security/race tests green. The M2 completion gate remains pending one live run of the extended contract suite against the pinned Navidrome image; Docker was unavailable in the latest implementation environment.

## Run M2 Locally

Prerequisites: Go 1.27.3, Node 20.18.2, Docker with Compose, and FFmpeg only if generating the deterministic audio fixture.

```bash
cp deploy/.env.example deploy/.env.local
./dev/create-audio-fixture.sh
./dev/create-credential-keyring.sh
docker compose --env-file deploy/.env.local -f deploy/docker-compose.dev.yml up -d postgres navidrome
```

Open `http://127.0.0.1:4533`, create the first Navidrome administrator through Navidrome's first-run UI, then set `FLOW_NAVIDROME_ADMIN_USERNAME`, `FLOW_NAVIDROME_ADMIN_PASSWORD`, `FLOW_ADMIN_USERNAME`, and a development-only `FLOW_ADMIN_PASSWORD` in `deploy/.env.local`. Keep `FLOW_CREDENTIAL_KEYRING_FILE=../deploy/secrets/flow-credential-keyring.json` because the backend is started from `backend/`. Never commit the env file or generated keyring.

```bash
cd backend
set -a
source ../deploy/.env.local
set +a
go run ./cmd/flowd
```

In a third terminal, run the Vite client. Its same-origin development proxy forwards `/api` and `/audio` to the backend:

```bash
cd web
npm install
npm run dev
```

Open `http://127.0.0.1:5173`. The development PostgreSQL port defaults to `127.0.0.1:55432`; change `FLOW_DEV_POSTGRES_PORT` and the matching port in `FLOW_DATABASE_URL` together if needed. The backend applies migrations under a PostgreSQL advisory lock, verifies/bootstrap-links the configured administrators, and listens on `127.0.0.1:8080`.

```bash
curl -fsS http://127.0.0.1:8080/livez
curl -fsS http://127.0.0.1:8080/readyz
curl -fsS http://127.0.0.1:8080/healthz
```

Run the fast checks from `backend/`:

```bash
gofmt -d .
go vet ./...
go test -race ./...
go build ./cmd/flowd
```

Run the web checks from `web/`:

```bash
npm run typecheck
npm run lint
npm run build
```

See [DEVELOPMENT_GUIDE.md](docs/DEVELOPMENT_GUIDE.md) for the PostgreSQL migration test and the explicitly destructive, disposable-only real Navidrome contract suite.

## Key Architecture Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Backend | Go modular monolith | Single deployable, clear internal boundaries, user already experienced |
| Database | PostgreSQL | Durable, multi-user, supports Flow's own auth/membership/state alongside Navidrome's SQLite |
| Streaming | **Flow is the only client-facing streaming gateway** (ADR-001) | Navidrome stays on the private Docker network; no client ever receives Navidrome credentials, JWTs, or internal addresses; Flow authorizes every stream request and proxies audio (Range-aware, streamed, no whole-file buffering) |
| Browser auth | Opaque server-side sessions (ADR-002) | Random tokens, SHA-256 hashes in Postgres, HttpOnly/SameSite cookies, immediate server-side revocation; future native clients get a separately defined bearer mechanism |
| Navidrome admin bootstrap | Operator-assisted first-run (ADR-003) | Flow cannot create Navidrome's first admin through an authenticated API; the operator initializes Navidrome, then hands credentials to Flow |
| Membership provisioning | Durable, retry-safe state machine (ADR-004) | Postgres and Navidrome cannot share one transaction; provisioning has explicit states, idempotent steps, compensation, and audit |
| Synchronized playback | In-memory engine + Postgres checkpoints, safe-end on restart (ADRs 005/006) | Single instance in V1; no Redis; serial executor, version + event sequence, persisted DJ lease |
| Web serving | Caddy-served static bundle (separate from the API container) | Caddy handles TLS + routing + security headers; SPA is a plain static site |
| Uploads | Same-filesystem sibling staging, full validation/fsync, atomic new-album directory rename, scan trigger | Separate volumes are not treated as atomic |
| Client in V1.0.0 | Web only | Reduce delivery risk; same API contract supports Android/desktop later |

## V1.0.0 Scope

**In:**
- Operator-assisted Navidrome admin bootstrap + admin Flow authentication (opaque sessions)
- Membership request → approval → durable provisioning → member account lifecycle
- Read-only music library (browse, search)
- Secure streaming gateway: all audio flows through Flow (no client-side Navidrome credentials)
- Persistent player with queue, multi-device concurrency (revision control)
- Admin-managed playlists, read-only for members
- Individual playback mode
- Synchronized playback mode (WebSocket, server-authoritative; safe-end on restart)
- Admin uploads via validated staging pipeline + scan trigger
- Basic theme system (5 starter themes, per-user selection)
- FLAC/MP3/AAC support
- Docker Compose deployment with Caddy

**Deferred to V1.x / V2.0.0:**
- Android app (background playback, offline)
- Tauri desktop app
- Native-client bearer-session mechanism (V1 is browser-only sessions; shape reserved, ADR-002)
- Offline downloads for members
- Community themes
- Multiple admin accounts / roles beyond admin + member
- Advanced social features (chat, listening history sharing)
- Advanced metadata editing and destructive library operations (moved out of the V1 critical path; see ROADMAP)
- Sync session auto-resume across restarts (V1 ends shared sessions safely; NFR-REL-004)

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
| `docs/decisions/ADR-001…006.md` | Architecture Decision Records: streaming gateway, sessions, admin bootstrap, provisioning, restart policy, event ordering |
| `docs/ARCHITECTURE_STATUS.md` | Reconciliation decisions, contradictions resolved, and evidence status |
| `docs/CONTRACT_MATRIX.md` | Normative contract ownership and cross-document references |
| `CHANGELOG.md` | Repository changes and remaining verification work |
