# Flow — Development Guide

Version: 1.1.0-draft (supersedes 1.0.0-draft)

Revisions in this version: **one** backend instance in dev (direct-host Go; Compose runs PostgreSQL and Navidrome only); a **single pinned Go toolchain**; **separated** fake-Navidrome fast tests from real pinned-Navidrome integration tests; explicit **migration policy** (forward-only in prod, up/down in CI/dev); **sync executor shutdown/failure handling**; and a clean **env-config vs. DB-settings** split (ARCHITECTURE §8).

---

## 1. Toolchain — one version everywhere (task 4)

| Tool | Pinned version | Where enforced |
|---|---|---|
| Go | **1.27.3**, recorded in `backend/go.mod` and `.go-version` | local dev and CI; future Docker build must match |
| Node.js | **20.18.2**, recorded in root `.nvmrc` and `web/package.json` engines | local web development and CI |
| Postgres | 16 | Dev Compose and CI service; production image is finalized in M6 |
| Navidrome | `v0.64.2`, image `0.64.2@sha256:38dc2727bfcfd5ede290f8ada114fc90368146f265ae4701ddddbcbe2a44ee52` | Compose + the **real-Navidrome** integration job only |

Rule: **there is exactly one Go toolchain version** — the one in `backend/go.mod` and root `.go-version`. Local Go and CI match it now; the backend Docker build introduced in M6 must use the same version.

## 2. Local Development Workflow (task 4 — one backend, direct-host)

### 2.1 Layout

```
flow/
├── backend/   Go module (single go.mod)
├── web/       React 18 + TypeScript + Vite browser application (introduced in M1)
├── deploy/    M0 dev Compose + env template; production Caddy/Compose arrive in M6
└── docs/      these documents
```

### 2.2 Start the local stack — **exactly one** backend instance

```bash
# one-time local setup
cp deploy/.env.example deploy/.env.local
./dev/create-audio-fixture.sh
./dev/create-credential-keyring.sh

# terminal 1: infra only (loopback-published for the direct-host backend)
docker compose --env-file deploy/.env.local -f deploy/docker-compose.dev.yml up -d postgres navidrome

# open http://127.0.0.1:4533 and create the first Navidrome admin, then put
# that username/password in deploy/.env.local. Also set the Flow admin
# username/password. Never commit this file or the generated keyring.

# terminal 2: the Flow backend, DIRECT ON THE HOST (the only Flow backend process)
cd backend
set -a; source ../deploy/.env.local; set +a
go run ./cmd/flowd             # migrations, dependency probes, listen :8080

# terminal 3: the M3 browser client
cd ../web
npm install
npm run dev                    # http://127.0.0.1:5173; /api and /audio proxy to :8080
```

`docker-compose.dev.yml` contains only PostgreSQL and Navidrome and binds them to loopback (`127.0.0.1:55432` by default and `127.0.0.1:4533`) because the backend runs on the host. `FLOW_DEV_POSTGRES_PORT` can change the PostgreSQL host port; its value must match the port in `FLOW_DATABASE_URL`. The current normal dev Navidrome mount is `/home/ruhin/Music:/music:ro`. Contract tests use `docker-compose.playback-test.yml` to mount only generated `dev/music-root/library` fixtures in a separate Compose project. The documented future upload topology uses the sibling `.flow-staging` on the same filesystem. `create-audio-fixture.sh` creates one deterministic FLAC only when it is absent and never overwrites an existing file.

**Why the application processes run directly on the host (rather than duplicating the backend in Compose):**
- One backend process ⇒ the single-writer sync executor, the active-stream registry, and the Navidrome JWT cache are unambiguously global. Running a second, containerized backend would create **two executors** for one Postgres `shared_sessions` row — the exact failure the single-instance design forbids (ADR-005). The dev workflow therefore never starts two backends.
- Direct `go run` gives fast rebuilds + `dlv`/breakpoints; Vite gives HMR. Postgres/Navidrome stay in Compose so their config (volumes, health, pinned image) matches production.
- A future fully containerized development variant, if added, must run exactly one backend and must never be started alongside the host process.

### 2.3 Navidrome pinning

The compose file and real-integration workflow use `deluan/navidrome:0.64.2@sha256:38dc2727bfcfd5ede290f8ada114fc90368146f265ae4701ddddbcbe2a44ee52`. Updating Navidrome requires a new source review, real-contract run, and digest update in the same change.

### 2.4 Configuration: environment vs. database settings (task 4 + ARCHITECTURE §8)

Two disjoint sets, never overlapping:

- **Deployment config = env vars + mounted secrets**: DSN, Navidrome URL/admin credential, Flow bootstrap admin credential, public browser origin, session/provisioning timeouts, listen/shutdown/HTTP timeouts (including `FLOW_NAVIDROME_STREAM_HEADER_TIMEOUT`), music root, log level, and `FLOW_CREDENTIAL_KEYRING_FILE`. PostgreSQL never stores the master keyring.
- **Runtime settings = Postgres `settings`** (admin-editable, live): platform mode, default theme, sync tuning (drift threshold, heartbeat interval, DJ grace), feature flags. Admin UI writes these; there is **no env override** in V1.

Decision rule when adding a knob: "does an operator change it at deploy time (env) or does an admin change it while the platform runs (DB)?" One home per knob. A knob that appears in both layers is a defect the bootstrap check (ARCHITECTURE §8) should catch.

## 3. Code Conventions

### 3.1 Backend (Go)

- Module: single `go.mod` at `backend/`; packages under `internal/` (no external import).
- Package names: singular (`sync`, `playback`, `streaming`, `membership`), directories match.
- Errors: sentinel errors in the owning package (`sync.ErrSessionEnded`, `streaming.ErrAborted`), wrapped with context; the HTTP layer maps via one `mapError` function (no error-type sprawl in `api`).
- Context first arg, always.
- **The only process-wide singletons** (documented in their package, behind small interfaces for tests): the **sync executor** (single writer, ADR-005/006), the **Navidrome credential cache** (per-user admin/member JWTs, ADR-001), and the **active-stream registry** (indexed by Flow session + user, ADR-001). Everything else is request-scoped.
- New long-lived goroutines require a doc comment explaining lifetime + cancellation + how they observe shutdown (see §5).

### 3.2 Web (TypeScript)

- `strict: true`, no `any` (eslint rule; use `unknown` + narrowing).
- Feature-folder layout (ARCHITECTURE §6), one barrel per feature. The app composes providers; library pages consume the shared player hook through its barrel (ADR-007).
- Library/auth server state uses TanStack Query. Playback uses one context/controller for CAS acknowledgements and local media transport. All fetches use `lib/api.ts` with Flow cookies.
- Audio: `features/player/PlayerProvider.tsx` owns the account's single element. Navigation keeps it mounted. Replacement/retry/teardown cancel its source; progress renders locally and checkpoints are periodic. No Navidrome URL/credential is constructed on the client (ADR-001/007).
- Theming: tokens in `themes/*.css` (custom properties); components reference tokens **only** (`var(--flow-accent)`), never raw hex in components.

## 4. Branching and Commits

- **Trunk-based.** `main` is always deployable (tagged releases). No long-lived feature branches (> 2 days).
- Feature work: short-lived `topic/xxx` branches → PR → squash-merge to `main`.
- Release: tag `v1.0.0` from `main` → CI builds + publishes images → `deploy/` pins the tag.
- Commit messages: [Conventional Commits](https://www.conventionalcommits.org/): `feat(sync): …`, `fix(auth): …`, `docs: …`, `test: …`. One logical change per commit.
- Versioning: semver on the product. `v0.x` during development; **`v1.0.0` is the first stable release** (no API back-compat promise before then; after v1.0.0: major for breaking API, minor for features, patch for fixes).

## 5. Sync Executor: Graceful Shutdown and Failure Handling (task 4)

The single-writer executor is the liveness heart of synchronized mode. Its lifecycle is defined, not ad-hoc:

- **Normal shutdown (SIGTERM):** the process coordinator signals the executor to **flush**: finish the in-flight mutation, write its checkpoint, then (per ADR-005 V1 policy) emit `session-ended {reason: platform-restart}` and persist `state=ended`. The WS hub closes all connections with a close frame. Only then does the process exit. No checkpoint is lost; no broadcast outruns its checkpoint (the "checkpoint-before-broadcast" invariant, ADR-006, holds even at shutdown).
- **Crash (kill -9 / OOM):** no graceful path runs. Postgres holds the last committed checkpoint. On next boot the executor scans `shared_sessions` for non-ended rows and **ends them safely** (ADR-005 §3.6) — it does not attempt to resume.
- **Executor panic:** the executor's recover is a deliberate crash (a writer that half-applied a mutation is worse than dead). The process exits non-zero; compose restarts it; boot-time safe-end applies. A recovered-but-stale executor is a corrupt-state risk, so it is not recovered.
- **Navidrome down while the executor runs:** sync *state* is unaffected (it's Flow's own data). The gateway's upstream calls fail (`navidrome_unavailable`); members see "stream unavailable" per-client; the coordinator keeps running and keeps correcting drift against reported positions.
- **Grace-deadline timer:** while the process is alive, the persisted instant drives T3→T5. Any backend restart applies ADR-005 safe-end immediately; it never waits out or resumes the grace window.

## 6. Testing Strategy (task 4 — fast vs. real)

| Layer | What | Tools | Navidrome? |
|---|---|---|---|
| **Unit** (fast, always on PR) | implemented auth/provisioning/library/streaming/playback logic and frontend controller/audio lifecycle | `go test`, TypeScript compiler + Node built-in `node:test` | **none** |
| **Integration — fake Navidrome** (fast, always on PR) | the narrow adapter's request paths, authentication headers, JSON decoding, cancellation, redacted transport errors, and HTTP error mapping | deterministic injected `http.RoundTripper`; PostgreSQL integration uses a disposable real server supplied by CI or `FLOW_TEST_DATABASE_URL` | **fake** |
| **Integration — real Navidrome** (manual/nightly/tag) | executable contract suite against release `0.64.2`: bootstrap/login, users/libraries, browse/search, member scope, playlists, stream Range/transcode/seek, scan, and config gates | pinned image when Docker is available; the suite requires an explicit disposable-instance mutation flag. A local binary built from the exact tag is a development fallback but does not establish the image digest | **real, pinned release** |
| **M3 live E2E** (PR/main/manual) | actual Flow executable, PostgreSQL 16, pinned Navidrome, compiled SPA, Chromium decoding/progress, two tabs/accounts and a process restart | `playback-live.yml`, test-only DevTools pipe client | real |

M4 drift/state-machine/WebSocket/Playwright checks remain future milestone work.
M3's test-only browser driver adds no production dependency. Neither mocked audio
nor a skipped live suite is evidence that browser playback works.

Rules:

- The **fake-Navidrome** suite gates every PR and covers only calls present in the adapter; it does not claim Navidrome compatibility.
- The **real-Navidrome** suite is the *verification* layer: it confirms the pinned image actually behaves like the fake (catches fake drift and Navidrome behavior change). It runs nightly and on release tags, not on every push.
- A PR that changes a call's shape must update **both** the fixture and pass the real-Navidrome job before merge.
- The real suite creates and removes a temporary Navidrome member and playlist and triggers a scan. It must run only against a disposable Navidrome data volume with `FLOW_REAL_NAVIDROME_ALLOW_MUTATION=true`; never point it at an operator's live instance.

### 6.1 Executable M0 checks

Fast checks, from `backend/`:

```bash
gofmt -d .
go vet ./...
go test -race ./...
go test -tags=integration ./...
go build ./cmd/flowd
```

The tagged integration command compiles the real suite but skips its live contract test unless all `FLOW_REAL_NAVIDROME_*` variables are supplied. The M2 extension covers the Flow adapter's member-scoped album/artist/song lists and details, search/scan, raw Range, MP3 and Opus offset transcodes, artwork, star read/write, and admin-public-playlist create/update/replace plus member list/detail reads.

Run the migration concurrency/up/down test only against a dedicated disposable database, never the normal `flow` development or production database:

```bash
docker compose --env-file deploy/.env.local -f deploy/docker-compose.dev.yml exec -T postgres \
  dropdb --if-exists -U flow flow_migration_test
docker compose --env-file deploy/.env.local -f deploy/docker-compose.dev.yml exec -T postgres \
  createdb -U flow flow_migration_test

cd backend
FLOW_TEST_DATABASE_URL='postgres://flow:flow-dev-only@127.0.0.1:55432/flow_migration_test?sslmode=disable' \
  go test -race -run 'TestMigrationsAgainstPostgres|TestRequestApprovalProvisioningLoginPasswordChangeAndSuspend' \
  ./internal/database ./internal/membership
cd ..

docker compose --env-file deploy/.env.local -f deploy/docker-compose.dev.yml exec -T postgres \
  dropdb --if-exists -U flow flow_migration_test
```

Both PostgreSQL tests create and remove isolated schemas inside the disposable database; they do not use the normal `public` schema. The real Navidrome suite creates users/playlists and triggers scans. Stop any normal dev Navidrome using port 4533, then use a separate Compose project so cleanup cannot remove normal development volumes:

```bash
./dev/create-audio-fixture.sh
docker compose --env-file deploy/.env.local -p flow-m0-contract -f deploy/docker-compose.dev.yml -f deploy/docker-compose.playback-test.yml up -d navidrome
until curl -fsS http://127.0.0.1:4533/ping >/dev/null; do sleep 1; done

cd backend
FLOW_REAL_NAVIDROME_URL='http://127.0.0.1:4533' \
FLOW_REAL_NAVIDROME_ADMIN_USERNAME='flow-m0-admin' \
FLOW_REAL_NAVIDROME_ADMIN_PASSWORD='flow-m0-admin-password' \
FLOW_REAL_NAVIDROME_CREATE_ADMIN='true' \
FLOW_REAL_NAVIDROME_ALLOW_MUTATION='true' \
FLOW_REAL_NAVIDROME_VERSION='0.64.2' \
  go test -tags=integration -run PinnedNavidrome -v ./integration
cd ..

docker image inspect deluan/navidrome:0.64.2 --format '{{json .RepoDigests}}'
docker compose --env-file deploy/.env.local -p flow-m0-contract -f deploy/docker-compose.dev.yml -f deploy/docker-compose.playback-test.yml down -v
```

The output must match the digest pinned in Compose. A tag-only run does not establish a new release pin.

### 6.2 M2 checks

From the repository root:

```bash
cd backend
GOCACHE=/tmp/flow-go-cache go test -race ./...
GOCACHE=/tmp/flow-go-cache go vet ./...
GOCACHE=/tmp/flow-go-cache go build ./cmd/flowd
GOCACHE=/tmp/flow-go-cache go test -tags=integration -run '^$' ./integration

cd ../web
npm run typecheck
npm run lint
npm run build
```

The fast backend suite includes a 32 MiB discard-writer stream test (bounded chunks, not whole-file buffering), client/upstream cancellation, active-stream suspension, the registration/suspension race, concurrent JWT refresh, eviction fencing, Range/`206`/`416`, `Accept-Ranges: none`, malformed upstream data, and credential/internal-address leakage assertions. It deliberately does not claim a concurrent-user capacity number; that measurement belongs to M6.

For a full local M2 smoke test, follow §2, sign in as both the administrator and one fully provisioned member, browse albums/artists/search/playlists, and stream the deterministic FLAC. In browser developer tools, the client request must target only `/audio/{mediaId}` or `/audio/coverart/{id}`; no request/response may contain the Navidrome host, username, password, or JWT.

### 6.3 M3 checks and live playback

Fast checks (repository root):

```bash
cd backend
GOCACHE=/tmp/flow-go-cache go test -race -count=1 ./...
GOCACHE=/tmp/flow-go-cache go vet ./...
GOCACHE=/tmp/flow-go-cache go build ./cmd/flowd
GOCACHE=/tmp/flow-go-cache go test -tags=integration -run '^$' ./integration
cd ../web
npm test
npm run typecheck
npm run lint
npm run build
```

`npm test` compiles the production controller/media/API modules into a temporary
directory and runs 33 Node tests. Cases include actual-position pause/seek, queue
controls, bounded checkpoint calls, loading/buffering/errors, autoplay rejection,
source retry/cancellation, natural end during a pending command, stale events,
409/two-tab reconciliation, uncertain responses, restore and teardown. The browser
gate separately tests rendered React controls, real decoding/progress and stable
element identity during navigation.

The backend service/API suite covers ownership, account gates, defaults, all queue
and transport controls, validation, shuffle, stale writes, competing commands and
checkpoints racing track/queue changes. `FLOW_TEST_DATABASE_URL` also enables the
real pgx suite during `go test -race ./...`; without it, PostgreSQL tests skip.

For a live gate, use §6.1's disposable database and a **separate disposable**
Navidrome Compose project. Create both fixtures before starting Navidrome:

```bash
./dev/create-audio-fixture.sh
sh dev/create-playback-fixture.sh
docker compose -p flow-m3-live -f deploy/docker-compose.dev.yml -f deploy/docker-compose.playback-test.yml up -d navidrome
# Wait for /ping. Build the web bundle with npm run build before the command below.
cd backend
FLOW_TEST_DATABASE_URL='postgres://flow:flow-dev-only@127.0.0.1:55432/flow_migration_test?sslmode=disable' \
FLOW_REAL_NAVIDROME_URL='http://127.0.0.1:4533' \
FLOW_REAL_NAVIDROME_ADMIN_USERNAME='flow-m3-navidrome-admin' \
FLOW_REAL_NAVIDROME_ADMIN_PASSWORD='flow-m3-navidrome-test-password' \
FLOW_REAL_NAVIDROME_CREATE_ADMIN='true' \
FLOW_REAL_NAVIDROME_ALLOW_MUTATION='true' \
FLOW_BROWSER_EXECUTABLE="$(command -v chromium)" \
GOCACHE=/tmp/flow-go-cache \
  go test -race -tags=integration -run '^TestM3LivePlayback$' -v ./integration
cd ..
docker compose -p flow-m3-live -f deploy/docker-compose.dev.yml -f deploy/docker-compose.playback-test.yml down -v
```

Use `google-chrome` for the executable where Chromium has that name. The live test
builds/starts exactly one temporary Flow child, provisions test members through
real auth/membership, tests library/range audio and CAS, runs the browser, stops
Flow, starts a fresh process against the same schema/keyring, and checks restore.
Its own static test server proxies API/audio to that child; the normal development
backend is not required. Member/schema cleanup is scoped to the test fixtures.
Omitting the browser executable explicitly skips that portion of the gate.

The optional `python3 dev/test-playback-schema.py` smoke uses a new disposable
single-user PostgreSQL process without listeners. It checks SQL upgrade/backfill,
trigger/constraints, conditional stale rejection, restart and down/reapply. It
does not exercise pgx, simultaneous transactions, Flow auth, Navidrome or audio.

Latest local evidence and unexecuted gates are recorded in M3_REPORT. The sandbox
denies Docker/TCP/Unix sockets and Chromium startup; live gates are not complete.

## 7. Migrations: Testing and Production Policy (task 4)

- **Layout:** embedded `backend/migrations/NNNN_name.up.sql` + `.down.sql`. M0 creates migration infrastructure and only schema needed by M0; later milestones add their own tables rather than creating future feature schema early.
- **Concurrency:** the runner holds a PostgreSQL advisory lock for the full migration operation. A second startup cannot migrate concurrently.
- **Production policy: forward-only.** Release `vN+1` migrates up. `down.sql` is **not** run in production (rollback of a release = deploy the fix forward, not downgrade). A downgrade is a data-recovery operation, done by an operator with `pg_dump` — not by the app.
- **Testing:** every PR runs migrations **up from empty** against disposable Postgres and runs `down.sql` in CI/dev to prove local reversibility. Once a prior release schema exists, CI also runs **up over the previous release's schema**; M0 has no prior Flow release schema to upgrade.
- **CI gate:** a migration that fails "up over previous" or that alters a column a live query reads is blocked. Destructive changes require explicit sign-off and are scheduled in the release notes.

## 8. CI/CD (GitHub Actions, `.github/workflows/`)

| Pipeline | Trigger | Jobs |
|---|---|---|
| `backend.yml` | PR, main | `go vet`, unit/fake-adapter tests, build, migration tests when PostgreSQL service is available |
| `navidrome-pinned.yml` | nightly, tag, manual | full real-contract suite against the pinned Navidrome release/image |
| `web.yml` | PR/main | `npm test`, typecheck, eslint, build |
| `playback-live.yml` | PR/main/manual | existing pinned-Navidrome contracts + PostgreSQL CAS + M3 Chromium/Flow process restart |
| `e2e.yml` | main (post-merge), nightly | Compose dev + Playwright: login → browse → stream (real audio bytes asserted) → sync session with 2 automated clients (real pinned Navidrome) |
| `release.yml` | tag `v*` | build multi-arch backend image (pinned Go) + web bundle, push to registry, `trivy` scan, update `deploy/` docs |

No secrets in CI for the fast suites. The pinned-Navidrome job creates a disposable administrator inside the Compose service using fixed test-only values; it uses no operator secrets.

## 9. Definition of Done (any V1 milestone task)

1. Code + fast tests pass on PR (unit + fake-Navidrome integration + migration up/down).
2. Feature works in the local direct-host stack (§2), not just unit-level.
3. Security-relevant paths: the corresponding SECURITY §11 acceptance test updated/passed.
4. Docs: if a contract changed (API event, schema, transition rule, ADR) → the doc changed in the same PR (docs are part of the product).
5. No new process-wide singleton / long-lived goroutine without a §5-style shutdown note; no new DB table without a migration pair; no new config knob without a declared home (env or DB, ARCHITECTURE §8).

## 10. Environment Matrix

| Env | Backend | Navidrome | Purpose |
|---|---|---|---|
| local-dev | direct host (`go run`), one instance | Compose, pinned tag, `dev/music-root/library` fixture | daily work |
| CI fast | GitHub Actions PostgreSQL service + in-process fake transport | **fake** | PR gates |
| CI pinned | GitHub Actions runner + Docker Compose | **real, pinned** tag + generated fixture library | nightly/tag verification |
| prod | container, one instance | pinned tag, real library | the actual thing; upgrades are deliberate |
