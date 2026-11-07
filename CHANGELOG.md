# Changelog

## Unreleased — V1 architecture reconciliation and M0–M4A implementation

### Architecture and documentation

- Reconciled all primary documents and ADR-001 through ADR-006 around the Flow-only streaming gateway, opaque browser sessions, operator-assisted Navidrome bootstrap, durable provisioning, safe-end sync restart policy, and `version`/`event_seq` ordering.
- Moved encryption master keys outside PostgreSQL and documented backup, restore, rotation, and credential re-encryption custody.
- Made member provisioning crash-safe by committing the encrypted reusable Navidrome credential before remote creation.
- Defined same-filesystem upload staging, whole-directory atomic promotion, manifests, and crash recovery.
- Defined connection-bound DJ leases, subscribe-before-snapshot delivery, revision-CAS individual playback, and representation-truthful Range behavior.
- Reconciled V1 scope: uploads, favorites, five themes, member deletion, and admin playlists are V1; metadata editing and destructive media operations are V1.x and have no V1 placeholder routes.
- Added `docs/ARCHITECTURE_STATUS.md` and `docs/CONTRACT_MATRIX.md` as the contradiction/evidence report and normative contract ownership map.
- Removed the stale `U-004` session reference in favor of the existing stable requirement `FR-AUTH-004`.
- Split V1.x genre browsing into `FR-LIB-006`, leaving `FR-LIB-001` as an unambiguous V1 album/artist browsing requirement.
- Clarified that no-reload switching among the five built-in themes is V1, while custom/community theme upload and hot-application are V1.x.

### M0 implementation

- Added the Go 1.27.3 backend module, validated environment configuration, JSON logging, health/readiness endpoints, and graceful HTTP shutdown.
- Added PostgreSQL connectivity plus an embedded migration ledger with per-migration transactions and PostgreSQL advisory-lock concurrency protection.
- Added a narrow Navidrome adapter for ping, login, native user read-back, token refresh, and security-gate verification. Adapter errors redact internal URLs and never include credentials or response bodies.
- Added deterministic fake-adapter tests, PostgreSQL migration integration tests, and an opt-in disposable-only real Navidrome contract suite.
- Added development Compose for PostgreSQL 16 and Navidrome 0.64.2, a deterministic FLAC fixture generator, and separate fast and real-contract CI workflows.
- Added copy-pasteable, isolated migration and real-Navidrome test commands; clarified that M0's first migration is infrastructure-only and that failed Navidrome gate verification leaves a diagnosable degraded process rather than aborting startup.
- Moved development PostgreSQL's default host binding to configurable port `55432`, avoiding collisions with existing local PostgreSQL services on 5432.
- Corrected the pinned Navidrome contract for disabled sharing: `getShares` returns HTTP 501 at `v0.64.2`, not an HTTP-200 Subsonic failure envelope.
- Resolved M1 initial-password delivery: admins set temporary Flow passwords out-of-band; first login is restricted until password replacement.

### M1 implementation — authentication and membership

- Added the M1 PostgreSQL schema for users, opaque sessions, membership requests, credential-key metadata, and append-only audit records.
- Added an external versioned AES-256-GCM credential keyring, strict file-permission checks, startup metadata synchronization, bounded credential re-encryption, and separate storage of Flow versus Navidrome administrator usernames.
- Added idempotent Flow administrator bootstrap with Argon2id password hashing, encrypted Navidrome administrator credentials, password-rotation session revocation, and audit output without secret material.
- Added opaque browser sessions, sliding eight-hour expiry, opportunistic expired-session cleanup, immediate revocation, forced first-login password replacement, and other-session revocation after password change.
- Added the durable member provisioning state machine with a transactionally committed encrypted credential before remote I/O, advisory-lock concurrency fencing, uncertain-create credential reconciliation, retry from stable failed steps, library-link verification, lifecycle actions, and persisted/retryable hard-delete compensation state.
- Added M1 REST routes with JSON errors, correlation IDs, login/register rate limits, same-origin mutation checks, admin role gates, and restricted-session enforcement.
- Added deterministic adapter/API tests and a disposable-PostgreSQL lifecycle test covering request → approval → uncertain remote result → retry → login → forced password change → suspension.
- Added the React 18/TypeScript/Vite/TanStack Query/Tailwind v4 M1 client for login, access requests, forced password replacement, request decisions, direct member creation, lifecycle management, and provisioning retry. It deliberately contains no M2 library/player surface.
- Added a non-overwriting development keyring generator and documented the three-process local workflow (Compose infrastructure, direct-host backend, Vite client).

### M2 implementation — library and secure streaming gateway

- Added a member-scoped Navidrome JWT cache with expiry parsing, per-user single-flight refresh, safe one-time authentication retry, and generation fencing so suspension eviction cannot be undone by an in-flight login.
- Connected suspension/deletion/reactivation, password reset/change, logout, and shutdown to a concurrency-safe active-stream registry indexed by Flow session and user. Registration now happens before any Navidrome request, followed by an authoritative session/account recheck.
- Added Flow-owned album, artist, song, search, star, scan-status, artwork, and playlist models/routes without a duplicate media index. Member playlist writes are denied at Flow; administrator-created playlists are made public for member reads.
- Added the `/audio/{mediaId}` and `/audio/coverart/{id}` gateways with validated identifiers/parameters, explicit `format=raw` original passthrough, MP3/Opus transcodes, `offsetSeconds`→`timeOffset`, incremental copy, request cancellation, Range/`206`/`416` truthfulness, safe-header allowlisting, and upstream body/address/token suppression.
- Enforced authentication and current account-state checks before audio parameter errors, while keeping request validation before registry admission or Navidrome I/O; tightened single-Range semantics and empty/duplicate parameter rejection.
- Tightened library pagination/search validation so unknown or duplicate query keys are rejected rather than silently accepted or forwarded.
- Added the M2 web library, search, album/artist/playlist detail views, favorites, administrator playlist editor, and one non-persistent browser player with play/pause/previous/next/seek/volume plus FLAC/Opus/MP3 capability selection. Persistent queue/session state remains M3.
- Added no database migration: Navidrome remains the media-index source of truth and all M2 process state is intentionally transient.
- Corrected the pre-M2 architecture diagram that registered a stream after opening upstream; the implemented and documented order is register → authoritative recheck → media lookup → upstream open.

### M3 implementation — individual playback

- Added ADR-007 and an M0–M2 readiness report. Fixed the blocking Vite 5174/5173 origin mismatch with strict port selection and the live-contract fixture mount through an isolated test-only Compose override.
- Added reversible migration `0003_m3_individual_playback`: one `session_state` row per user, existing-user backfill, transactional user-insert trigger, canonical empty/queue/index/current/position/volume/status constraints, and cascading physical-account cleanup.
- Added the individual playback package and GET/PUT/DELETE `/api/playback/session`. All ownership comes from the authenticated principal; each write rechecks account/session authority and commits a conditional revision UPDATE. Stale writes return 409 with the latest own session.
- Implemented validated current selection, play/pause, seek, queue append/remove/move/replacement, deterministic next/previous/boundaries, duplicate occurrence identity, volume, stable shuffle and guarded checkpoint/end commands.
- Replaced the M2 ephemeral player with one shared account-keyed React provider/controller/audio element. Added global controls, queue editing/select/clear, album/listing/search/song/artist integration, saved-state restore, local loading/buffering/error states, optional Media Session and source teardown/retry.
- Added 10-second active checkpoints plus immediate intentional commands, REST refresh every 15 seconds/on visible focus or online, visible 409 reconciliation, no automatic stale retries/merges and no unload durability dependency.
- Fixed natural end racing an earlier local command, stale audio-error events, failed-source retry, and actual-position failure checkpoints. A rejected stopped-intent callback preserves conflict reconciliation. Retained unavailable media remains removable and can be durably paused.
- Added Node frontend/controller/API tests, real pgx CAS/lifecycle/restart tests, a listener-free SQL smoke, and an opt-in live test combining the real Flow executable, PostgreSQL, pinned Navidrome, gateway and Chromium with an actual Flow process restart.
- Added `playback-live.yml` and frontend test CI. Updated the product/API/playback/schema/architecture/development/requirements/roadmap documents.

### M4A implementation — synchronization state machine and backend executor

- Added reversible migration `0004_m4a_synchronization` with the platform-mode singleton, one-open-shared-session constraints, connection-bound DJ lease fields, authoritative timing checkpoint, and bounded per-session durable event log.
- Added the channel-fed single-writer synchronization service, legal transition validation, server-authoritative active/paused position calculation, queue commands, content-version CAS, and a separate monotonically increasing lifecycle event sequence.
- Added rotated random DJ lease tokens stored only as SHA-256 hashes, connection/generation/token fencing, explicit takeover, idempotent disconnect freeze, persisted 90-second grace deadline, reconnect, retrying deadline enforcement, and stale-owner rejection.
- Added atomic PostgreSQL checkpoint + event + mode transition + `mode.changed` audit transactions, commit-time opaque-session/account rechecks, and startup/graceful-shutdown safe-end recovery that preserves every individual `session_state` row.
- Added canonical mode/state/control REST routes. `/api/platform/mode` is the only public start/end API; lease acquisition/disconnect stay trusted internal boundaries for M4B and no public caller-chosen connection lease route exists.
- Gated individual playback writes during synchronized mode without deleting or modifying individual state.
- Added deterministic state-machine, timing, authorization, lease, queue, recovery, executor-concurrency, API, origin, and mode-gate tests plus an opt-in real-PostgreSQL suite for cross-executor locking, rollback, event order, suspension races, restart recovery, and migration reapplication.
- Fixed Chromium test-profile cleanup to retry transient `ENOTEMPTY` failures instead of masking the M3 live-gate result.

### Verification status

- Passed locally: formatting check, `go test -race ./...`, `go vet ./...`, backend build, integration-suite compilation, deterministic fixture validation, Compose configuration rendering, and documentation consistency checks.
- Passed through the operator-accessible development stack: PostgreSQL migration concurrency/up/down; operator-assisted Navidrome bootstrap and backend verification; the full real Navidrome contract suite; and immutable image digest capture.
- Navidrome is pinned as `deluan/navidrome:0.64.2@sha256:38dc2727bfcfd5ede290f8ada114fc90368146f265ae4701ddddbcbe2a44ee52`.
- M0 completion gate passed; M1 is authorized as the next implementation scope.
- M1 readiness audit: protected requests resolve current PostgreSQL session/user state; inactive or incompletely provisioned users are denied; credentials remain encrypted/server-side and member-scoped; session revocation is immediate. M2 added and tested the previously absent credential-cache and active-stream lifecycle controls.
- M2 fast gates executed successfully: `go test -race ./...`, web typecheck, eslint, and production build. The tagged real-integration suite also compiled successfully.
- The deterministic M2 suite covers a 32 MiB incremental stream plus eight concurrent 4 MiB bounded-copy streams, original/transcoded responses, Range/`206`/sanitized `416`, `Accept-Ranges: none`, cancellation, active suspension, registration races, concurrent refresh, refresh eviction fencing, malformed responses, member playlist-write denial, and credential/internal-address leakage.
- Added explicit regression coverage for Navidrome login `401`/`403` mapping, upstream deadline mapping, and duplicate/unknown library query rejection.
- The extended real Navidrome `v0.64.2` M2 contract (including album/artist/song lists and details, raw Range, both transcodes, artwork, stars, and playlist member reads) was not executed in this environment: Docker socket access was denied and no Navidrome listener was available on `127.0.0.1:4533`. M2 remains pending that live completion-gate run; no pass is claimed.
- M3 deterministic frontend tests, backend tests, integration compilation and SQL smoke have been executed. Current results and commands are in `docs/M3_REPORT.md`.
- M3's live PostgreSQL/Chromium/Navidrome/process-restart gates remain outstanding because Docker/socket access and Chromium startup are restricted. SQL smoke and transport doubles are not claimed as a browser playback pass. M3 is not marked fully complete until the live gates pass.
- M4A deterministic Go tests pass. The PostgreSQL synchronization suite compiles but was skipped because `FLOW_TEST_DATABASE_URL` was unset; Docker access was denied and no local PostgreSQL listener was available. M4B has not started.
