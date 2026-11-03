# Flow — Development Guide

Version: 1.0.0-draft

---

## 1. Prerequisites

| Tool | Version | Notes |
|---|---|---|
| Go | 1.22+ | backend |
| Docker + Compose | current | local env |
| Node.js | 20 LTS | web |
| Postgres | 16 | via compose (not installed locally) |
| Navidrome | pinned tag from `deploy/` | via compose |

## 2. Local Development Workflow

### 2.1 Layout

```
flow/
├── backend/   Go module: github.com/<you>/flow/backend
├── web/       npm package
├── deploy/    compose + caddy
└── docs/      these documents
```

### 2.2 Start the full local stack

```bash
# from flow/ root
cp deploy/.env.example deploy/.env.local  # fill in admin creds + key
docker compose -f deploy/docker-compose.dev.yml up -d   # navidrome + postgres + music volume
```

`docker-compose.dev.yml` = production compose with:
- `flow-backend` run from source: `command: go run ./cmd/server` with a bind-mount on `backend/`
- Vite dev server on `:5173`, `proxy: /api → localhost:8080`, `/ws → ws://localhost:8080/ws`
- Navidrome music volume pointing at a local `dev/music` folder (put a few FLAC files there)

Then:

```bash
cd backend && go run ./cmd/server   # another terminal; migrations + bootstrap run here
cd web    && npm run dev
```

First start bootstraps the admin (env vars from `.env.local`, loaded into the backend process).

### 2.3 Navidrome pinning

`deploy/.env.local` sets `NAVIDROME_IMAGE_TAG`. Integration tests in CI run against the **same** pinned image as compose (single source of truth in one file).

### 2.4 Configuration

All config = env vars (NFR-MAINT-003). `backend/internal/config` defines the struct + `envFile` loading for tests. Naming: `FLOW_<DOMAIN>_<KEY>` (e.g., `FLOW_SYNC_DRIFT_THRESHOLD_MS` overrides the DB default — env wins only for the operator-level keys; per-instance settings live in the DB `settings` table and are admin-editable).

Decision: **runtime settings** (mode, themes, flags, sync tuning) live in Postgres `settings`; **deployment settings** (URLs, DSN, keys, ports) live in env. Admin UI edits only the former.

## 3. Code Conventions

### 3.1 Backend (Go)

- Module: single `go.mod` at `backend/`.
- Packages under `internal/` (no external import).
- Package names: singular (`sync`, `playback`), directories match.
- Errors: sentinel errors in the owning package (`sync.ErrSessionEnded`), wrapped with context; HTTP layer maps via one `mapError` function (no error-type sprawl in `api`).
- Context first arg, always; `context.WithoutCancel` only at process boundaries.
- No global state except: the sync executor singleton and the Navidrome JWT cache (both deliberately process-wide, documented in the package, both behind small interfaces for tests).
- Tests: `X_test.go` beside code; integration tests in `internal/sync/` and `internal/membership/` use `testcontainers-go` (Postgres) and a `httptest`-fake Navidrome (shared fixture in `tests/fake_navidrome`).
- Lint: `golangci-lint` (`.golangci.yml` at repo root: `errcheck`, `gosimple`, `revive`, `goconst`, `unparam` enabled; `staticcheck` full).
- Concurrency: the sync executor is the only long-lived non-HTTP goroutine; everything else is request-scoped. New long-lived goroutines require a doc comment explaining lifetime + cancellation.

### 3.2 Web (TypeScript)

- `strict: true`, no `any` (eslint rule; use `unknown` + narrowing).
- Feature-folder layout (ARCHITECTURE §6). One barrel per feature; no cross-feature imports except via `app/` providers.
- Server state: TanStack Query only; no hand-rolled fetch wrappers except the one in `lib/api.ts`.
- Theming: tokens in `themes/*.css` (custom properties); components reference tokens **only** (`var(--flow-accent)`), never raw hex in components (enforced by a custom ESLint rule in V1.x; manual review until then).
- Player: single `AudioElement` instance in `player/context.tsx`; sync mode drives it via the same context. No second audio element, ever.

## 4. Branching and Commits

- **Trunk-based.** `main` is always deployable (tagged releases). No long-lived feature branches (> 2 days).
- Feature work: short-lived `topic/xxx` branches → PR → squash-merge to `main`.
- Release: tag `v1.0.0` from `main` → CI builds + publishes images → `deploy/` pins the tag.
- Commit messages: [Conventional Commits](https://www.conventionalcommits.org/): `feat(sync): …`, `fix(auth): …`, `docs: …`, `test: …`. One logical change per commit.
- Versioning: semver on the product. `v0.x` during development; **`v1.0.0` is the first stable release** (no API back-compat promise before then; after v1.0.0: major for breaking API, minor for features, patch for fixes).

## 5. CI/CD (GitHub Actions, `.github/workflows/`)

| Pipeline | Trigger | Jobs |
|---|---|---|
| `backend.yml` | PR, main | `go vet`, `golangci-lint`, unit tests (no containers), coverage gate ≥ 70% |
| `integration.yml` | PR, main | testcontainers Postgres: membership flow, sync executor restart/recovery, migration up/down |
| `web.yml` | PR, main | `tsc --noEmit`, eslint, build |
| `e2e.yml` | main (post-merge), nightly | full compose (dev file) + Playwright: login → browse → stream (real audio bytes asserted) → sync session with 2 automated clients; fake-navidrome fixture for speed, real pinned Navidrome image nightly |
| `release.yml` | tag `v*` | build multi-arch backend image + web bundle, push to registry, `trivy` scan, update `deploy/` docs |

No secrets in CI (SECURITY §9).

## 6. Definition of Done (any V1 milestone task)

1. Code + tests pass on PR.
2. Feature works in the local compose stack (not just unit-level).
3. Security-relevant paths: the corresponding SECURITY §11 acceptance test updated/passed.
4. Docs: if a contract changed (API event, schema, transition rule) → the doc changed in the same PR (docs are part of the product).
5. No new global state, no new long-lived goroutine, no new DB table without a migration pair.

## 7. Testing Strategy Summary

| Layer | What | Tools |
|---|---|---|
| Unit | pure logic: drift decision, transition table, path sanitization, JWT wrap | go test (fake clock), vitest |
| Integration | membership provisioning, sync checkpoint/restart, upload+scan | testcontainers Postgres + fake-navidrome httptest |
| E2E | real backend + real Navidrome image + Playwright web client; WS client simulated with a second Playwright page | compose dev |
| Manual checklist | FLAC passthrough quality spot-check, theme switching, mode transitions, suspend/reactivate | docs `DEPLOYMENT §manual-checklist` (V1.x) |

## 8. Environment Matrix

| Env | Navidrome | Purpose |
|---|---|---|
| local-dev | pinned tag, `dev/music` sample library | daily work |
| CI | pinned tag, fixture library | gates |
| prod | pinned tag, real library | the actual thing; upgrades are deliberate |
