# Flow — Roadmap

Version: 1.1.0-draft (supersedes 1.0.0-draft)
Method: milestones M0–M6 (M4 split into M4A/M4B) for V1.0.0, each independently shippable/testable. Requirements IDs reference REQUIREMENTS.md; ADRs reference `docs/decisions/`.

Revisions: reorganized around the decided architecture — **M4 is split** (state machine/executor/persistence/event protocol first, then WebSockets + drift + DJ lifecycle + UI); **advanced metadata editing and destructive library operations are moved to M14 / V1.x** so they do not block V1 delivery; the bootstrap, gateway, and session models from the ADRs are named explicitly. Android, desktop, downloads, advanced recovery, multi-instance, social, and custom themes remain outside V1.

---

## M0 — Foundation and Verified Navidrome Connection

| Task | IDs / refs | Done when |
|---|---|---|
| Go module, HTTP server skeleton, validated config, slog JSON, `/livez` + `/readyz` + `/healthz`, graceful SIGTERM | — | `go run ./cmd/flowd` boots and drains cleanly |
| PostgreSQL pool + embedded migration runner with advisory-lock concurrency protection; M0 migration only | NFR-MAINT-004 | empty/up/down and concurrent-runner tests pass |
| Docker compose dev stack (**postgres + navidrome only**; backend runs direct-host — DEVELOPMENT_GUIDE §2) | A-002 | generate the fixture, then `compose up postgres navidrome` → Navidrome indexes `dev/music-root/library` |
| **Navidrome release `v0.64.2` selected; source commit recorded; image digest and runtime evidence captured** | NAVIDROME_INTEGRATION §5/§7 | immutable pin `sha256:38dc2727…ee52`; real contract suite green |
| Narrow Navidrome adapter: ping, native login, current/admin user read, config read-back, explicit timeouts and redacted errors | ADR-003 | deterministic fake tests pass; credentials/tokens never logged |
| Executable real-contract suite for required future surfaces (users/libraries, browse/search, member scope, playlists, original/transcoded stream behavior, scan, gates) | NAVIDROME_INTEGRATION | each behavior is verified or explicitly unresolved against the pinned release |
| Operator-assisted Navidrome first-admin runbook and degraded integration status | FR-AUTH-001, ADR-003 | uninitialized Navidrome reports degraded without mutation; supplied admin credentials verify successfully |
| CI for format, vet, unit/fake tests, build; real Navidrome job separate/manual-nightly | DEVELOPMENT_GUIDE §8 | fast checks are reproducible without external secrets |

Exit: **the backend foundation, PostgreSQL/migrations, and a real verified connection to pinned Navidrome work reproducibly. M0 contains no Flow browser login, users, membership, streaming gateway, or product UI.**

## M1 — Authentication and Membership

| Task | IDs / refs |
|---|---|
| Flow admin bootstrap mapping + external-key encrypted Navidrome admin credential | FR-AUTH-001, NFR-SEC-007/008 |
| Opaque browser sessions (argon2id login, random token hash, logout/me, rate limits) | FR-AUTH-002…007, ADR-002 |
| Admin-issued temporary member passwords + restricted first-login change flow | FR-AUTH-009, ADR-002 |
| Register-request endpoint + request model + rate limit | FR-MEM-001, FR-MEM-002 |
| Admin request list/approve/reject UI + routes | FR-MEM-003 |
| **Durable provisioning state machine** (`not_started→credential_ready→creating_nd_user→linking→complete/failed`; credential committed before remote create; retry/collision reconciliation; audit) | FR-MEM-004, ADR-004 |
| Direct account create (same machine) | FR-MEM-005 |
| Suspend / reactivate / delete (sessions revoked; durable Navidrome cleanup state on hard delete). M1 has no long-lived member JWT cache or streams; M2 connects these lifecycle actions to credential-cache eviction and the active-stream registry when those components first exist. | FR-MEM-006…008 |
| Member list view with last-active + provisioning state | FR-MEM-009 |
| Security acceptance tests available at this milestone: opaque-session revocation, CSRF/Origin checks, provisioning gate, admin/member route authorization | SECURITY §11 |

Exit: **full request → approve → member-logs-in → suspend → re-login-blocked loop works; a member's Navidrome user is provisioned durably with no partially-active users. Live-stream teardown is verified in M2, where the gateway first exists.**

## M2 — Library and Secure Streaming Gateway

| Task | IDs / refs |
|---|---|
| **Streaming gateway** (ADR-001): per-request authn/z + active-stream registry indexed by session/user; logout flushes one session, suspension/deletion flushes the user; Range/partial/seeking passthrough; transcoding params; cancellation/upstream abort; cover-art via gateway | FR-STRM-001, FR-STRM-004, FR-STRM-005, NFR-SEC-002 |
| Library proxy: albums, album detail, artists, artist detail, search, scan status (server-side, member-scoped) | FR-LIB-001, FR-LIB-002, FR-LIB-004 |
| Stars (favorites) | FR-LIB-005 |
| Read-only playlist list/detail for members | FR-PL-002 |
| Admin playlist CRUD | FR-PL-001, FR-PL-003 |
| Web: library screens, search, album/artist views, player skeleton (play/pause/next/prev/seek/volume) streaming through `/audio`, with FLAC/Opus negotiation | FR-STRM-002, NFR-COMPAT-002 |

Exit: **admin and a member both browse and stream FLAC in the browser through the gateway; a member's responses contain zero Navidrome credential material; members cannot mutate playlists (403 verified).**

Implementation status (2026-11-05): M1 readiness was audited and the M2 backend/web implementation plus deterministic race, cancellation, security, and bounded-stream tests are present. Fast Go and web gates pass. The extended `v0.64.2` real-contract suite compiles, but this environment could not execute it because Docker/Navidrome were unavailable; therefore the M2 exit gate is **pending that live run**, not marked fully complete.

## M3 — Individual Playback

| Task | IDs / refs |
|---|---|
| `session_state` persistence, throttled writes, restore | FR-IND-001…002 |
| **Multi-device concurrency: one session per user, revision CAS, 409 + reconcile** | FR-IND-005 (task F) |
| Queue management UI (add, remove, reorder, shuffle) | FR-IND-003 |
| Resume-on-login from the last durably checkpointed queue, track, and position | FR-IND-002 |
| Command persistence, 10-second active checkpoints, guarded natural end, and source retry | FR-IND-006, ADR-007 |
| Global browser player, optional Media Session, library/listing integration | FR-IND-003, ADR-007 |
| REST refresh, 409 reconciliation, actual browser decoding and restart gate | FR-IND-004…005, NFR-REL-001 |

Exit: **close browser, reopen next day on another device — queue and position restored; two devices on one account reconcile without losing state (409 path verified).**

Implementation is present, with deterministic service/API/frontend coverage and
an executed SQL smoke. The real pgx/PostgreSQL 16 and browser/Navidrome/Flow process
restart completion gates remain pending because the implementation environment
denies Docker/socket access. CI now has an executable `playback-live.yml` gate;
no CI pass is claimed before it runs. See M3_READINESS and M3_REPORT. M3 is not
marked complete while those gates are outstanding. No M4 implementation started.

## M4A — Synchronization State Machine and Backend Executor

| Task | IDs / refs |
|---|---|
| Sync executor (single writer; checkpoint-before-broadcast) + `shared_sessions` + `sync_events` persistence | FR-SYN-001…002, ADR-005/006 |
| **Version vs. event-seq distinction; exactly-one-authoritative-event-per-seq invariant** | ADR-006 |
| **DJ lease (server-issued generation + connection-bound rotated token) + persisted 90 s grace deadline** | FR-SYN-008, ADR-005 |
| **Restart policy: safe-end of active sessions on boot (T6)** | FR-SYN-011, NFR-REL-004, ADR-005 |
| Admin sync routes (start/play/pause/seek/next/prev/queue/end/lease) — state only, no WS yet | FR-SYN-001, FR-SYN-002 |
| Drift-decision pure function (position + buffering + seeking + rate + monotonic clock) | FR-SYN-005, FR-SYN-006, task E |
| Transition table T1–T6 as table-driven tests (fake clock) | FR-MODE-001…003 |
| Integration: checkpoint ordering, zombie-DJ rejection, grace-deadline persistence + restart safe-end | ADR-005/006 |

Exit: **the sync engine is correct and persisted: mutations are serialized, checkpointed authoritatively, DJ lifecycle is lease-gated, and a restart ends sessions safely — all proven by fake-Navidrome integration tests, before any WebSocket code exists.**

## M4B — WebSockets, Multi-Client Playback, Drift Correction, DJ Lifecycle, Sync UI

| Task | IDs / refs |
|---|---|
| **WS hub: session-cookie + Origin auth (no query-string tokens), per-user channels, envelope (ADR-002), ping/pong** | FR-SYN-003, FR-SYN-007 |
| **Atomic subscribe/snapshot/drain + stale-event / sequence-gap rules (ADR-006)** | FR-SYN-005, FR-SYN-007, FR-SYN-010 |
| Drift coordinator live: heartbeat in-band, seek-correction fan-out | FR-SYN-006 |
| **DJ disconnect → freeze + grace → fallback; reconnection lease verification** | FR-SYN-008 |
| Mode switching + client-side transition effects | FR-MODE-001…003 |
| **Web sync UI: shared queue, DJ status, drift readout, "DJ disconnected" countdown, session-ended handling** | FR-SYN-001…010 |
| **Audio for sync via the gateway** (representation-appropriate seek; integer `offsetSeconds` only for an initial transcode load) | ADR-001 |
| E2E: two Playwright clients, full DJ session incl. staggered joins + a forced-restart safe-end | PLAYBACK_SYSTEM §11 |

Exit: **admin runs a listening session; two members stay within 2 s; DJ leaves and the 90 s rule fires (or a valid-lease reconnect resumes); a backend restart ends the session safely; everyone lands back in Individual Mode with their old queues.**

## M5 — Uploads, Library Administration, Settings, Audit, and Themes

| Task | IDs / refs |
|---|---|
| **Staged upload pipeline** (same-filesystem sibling staging → durable manifest → validate/fsync → atomic new-album directory rename → scan/recovery; `upload_operations`) | FR-ADM-005, FR-ADM-006 |
| Re-scan trigger + status | FR-ADM-001, FR-ADM-002 |
| Platform settings CRUD + **audit logging (correlation id + outcome) for admin actions from M1/M4/M5** | FR-ADM-004, FR-ADM-007 (baseline) |
| **Theme system: 5 token themes, per-user choice, platform default, live switching** | FR-THM-001…003 |
| No metadata mutation or destructive media endpoint in V1; no placeholder route is registered | FR-ADM-003, FR-ADM-009 → V1.x |

Exit: **admin's daily loop works: drop a new album via the web UI → staged + promoted + scanned → it's playable; tweak settings (audited); pick a theme. Destructive/advanced library operations remain absent until M14.**

## M6 — Production Hardening and Release

| Task | IDs / refs |
|---|---|
| Production compose + Caddyfile final (**no published Navidrome port; `/audio` gateway route; same-filesystem `.flow-staging` outside Navidrome's mounted library**) | DEPLOYMENT, ADR-001 |
| Backup/restore scripts + docs (database and external credential keyring backed up separately) | NFR-REL-001…004, SECURITY §10 |
| Performance pass: gateway throughput + p95, WS latency, drift, and measured stream ceiling | NFR-PERF-001…004 |
| Full **real-pinned-Navidrome** e2e suite green on the pinned tag (DEVELOPMENT_GUIDE §8) | |
| Release: tag `v1.0.0`, images, changelog | |

Exit: **v1.0.0.**

---

## V1.x improvements (sequenced, each a mini-milestone)

| Item | Notes |
|---|---|
| M7: **Sync crash-recovery (auto-resume shared session after backend restart)** — upgrades the V1 safe-end | FR-SYN-011; NFR-REL-004 |
| M8: Android app (Kotlin/Compose/Media3): background playback, notifications, lock-screen art, Bluetooth/media keys, audio focus, individual + sync mode; **native bearer-session auth (ADR-002)** | new `android/` module; same API |
| M9: Desktop (Tauri 2, Linux+Windows): reuse web bundle, system media keys, tray, persistent session | new `desktop/` module |
| M10: Downloads (flag on: platform + per-user) — **delivered through the gateway** (no client credentials; the download variant of `/audio`) | FR-STRM-003 flag |
| M11: Random albums/songs, genres, duplicate detection on upload (content-hash pre-check) | FR-LIB-003, FR-LIB-006 |
| M12: Audit log viewer + pruning; `library-changed` WS events closed-loop via scan-completion polling | FR-ADM-007 full |
| M13: Per-user stream format preference UI; Opus-default for mobile | NFR-COMPAT-002 |
| M14: **Advanced metadata editing** (tag write + affected-album re-scan) and **destructive library ops** (index-row + file delete) — now delivered for real | FR-ADM-003, FR-ADM-009 |
| M15: Custom themes (admin-uploaded token CSS, hot-apply), community theme submission flow | FR-THM-004 |

## V2.0.0

| Item |
|---|
| Offline download queue (Android): background download of album/playlist + offline play; `session_state` gains an `offline` marker |
| Multi-instance Flow (Postgres NOTIFY or Redis bus; WS statelessness) — the NFR-SCALE-002 path |
| Roles beyond admin/member (e.g., `curator` with playlist rights); per-user permission grants (schema already `role` + flags; API shape reserved) |
| Social layer: live presence, listening reactions, private chat inside sync sessions (new WS message families, `v:2` protocol) |
| Smart playlists (Navidrome `nsp` files via Flow admin UI) |
| Navidrome plugin: Flow-as-scrobbler (plugin PDK), listening history sharing |
| Multi-library curation (per-member library subsets via `PUT /api/user/{id}/library`) |
| API v2 namespace if any breaking change lands |

## Long-term possibilities (watchlist, no design yet)

- Web-based music *upload from any client* (admin mobile) — same upload endpoint, mobile UX.
- Lossless-to-Opus adaptive: per-member bandwidth profiling auto-selecting transcode target (Navidrome's `getTranscodeDecision` computes it; Flow would surface it).
- Hardware sync for a shared room (Navidrome jukebox for a physical speaker, Flow sync session for people) — "listen together in the living room" mode.
- Read-only guest access (time-boxed Navidrome shares surfaced through Flow, no account).
- iOS app.

---

## Biggest risks and their planned countermeasures

| Risk | Likelihood | Mitigation |
|---|---|---|
| Synchronized playback feels laggy/unsatisfying on the internet | Medium | 2 s threshold + monotonic clocks + rate-catch-up (PLAYBACK_SYSTEM §3.3); NFR-PERF-003 measured before V1 sign-off; a "relaxed sync" toggle (10 s tolerance) ships in M4B as a runtime setting |
| Navidrome API/behavior drift vs. the pinned tag | Medium | **Real-pinned-Navidrome CI job** + fake fixture kept in lockstep; runtime evidence is recorded per release |
| Gateway becomes a bandwidth/buffering bottleneck | Medium | M2/M6 perf pass; unbuffered `io.Copy` proxy; hard per-user admission cap; document the measured deployment ceiling |
| Bootstrap dead-end (operator confused by two credential sets) | Medium | ADR-003 + DEPLOYMENT §4 step-by-step; `/admin/integration/health` surfaces the exact stuck state |
| Scope creep into V1 (themes go wide, metadata goes deep) | Medium | this roadmap is the gate: advanced metadata + destructive operations live in M14; anything not in M0–M6 is V1.x by definition |
| Two-database + two-credential-set mental model confuses operators | Low | NAVIDROME_INTEGRATION §5/§6 + DEPLOYMENT backup/restore docs |

## First milestone to start

**M0**, in this order:
1. repo scaffold (`backend/`, `deploy/`, `.github/`); web waits for the first web milestone
2. pinned Go toolchain, config, structured logging, health/readiness, graceful lifecycle
3. PostgreSQL connectivity + advisory-locked M0 migrations
4. dev compose for PostgreSQL + Navidrome `0.64.2` with same-filesystem music-root topology
5. narrow Navidrome adapter, fake tests, and the executable real-contract suite
6. verify operator-assisted first admin and all required Navidrome contracts; record and use the immutable image digest

M1 starts only after review of the M0 verification report.

## Documentation change report (this revision)

- **Modified:** all 12 primary docs + 6 ADRs (`docs/decisions/ADR-001…006.md`) + root `CHANGELOG.md`, `ARCHITECTURE_STATUS.md`, and `CONTRACT_MATRIX.md`.
- **Decisions made:** gateway streaming (ADR-001), opaque browser sessions + bearer reserved (ADR-002), operator-assisted admin bootstrap (ADR-003), durable provisioning state machine (ADR-004), V1 safe-end restart policy (ADR-005), version + event-seq ordering (ADR-006), individual-session revision CAS (task F), staged upload pipeline (task G), settings precedence + schema additions (task H).
- **M0 verification completed:** PostgreSQL migration concurrency/up/down and the real image contract suite are green; the immutable Navidrome digest is pinned. Coverage includes first-admin, login, native/Subsonic schemas, user-library assignment, member authorization boundaries, playlists, Range, MP3/Opus transcodes, scan, and configuration gates.
- **Assumptions now flagged as source-verified vs. runtime-verified:** NAVIDROME_INTEGRATION and ARCHITECTURE_STATUS keep the distinction explicit.
