# Flow — Roadmap

Version: 1.0.0-draft
Method: milestones M0–M6 for V1.0.0, each independently shippable/testable. Requirements IDs reference REQUIREMENTS.md.

---

## M0 — Foundations (no product value yet, no rework later)

| Task | IDs | Done when |
|---|---|---|
| Go module, chi server skeleton, config/env loading, slog JSON, `/healthz` | — | `go run` boots, health green |
| Postgres schema + migration runner (all tables in DATABASE_SCHEMA) | NFR-MAINT-004 | migrations up on empty DB |
| Docker compose dev stack (navidrome + postgres + music volume) | A-002 | `compose up` → Navidrome indexes `dev/music` |
| Navidrome client package: admin bootstrap login, health probe, JWT cache (admin) | NAVIDROME-INT §3 | backend logs into pinned Navidrome |
| `users` + `auth`: argon2, JWT issue/verify/revocation, login/logout/me, rate limit | FR-AUTH-002…007 | admin can log in via curl |

Exit: **a logged-in admin hits a healthy backend in front of a working Navidrome.**

## M1 — Membership lifecycle

| Task | IDs |
|---|---|
| Register-request endpoint + request model + rate limit | FR-MEM-001, 002 |
| Admin request list/approve/reject UI + routes | FR-MEM-003 |
| Provisioning orchestration (PG row + Navidrome user + link, retry-safe, tx) | FR-MEM-004 |
| Direct account create | FR-MEM-005 |
| Suspend / reactivate / delete (sessions revoked, Navidrome user removed) | FR-MEM-006…008 |
| Member list view with last-active | FR-MEM-009 |
| Security acceptance tests 2, 3, 6 | SECURITY §11 |

Exit: **full request → approve → member-logs-in → suspend → re-login-blocked loop works; member has a working Navidrome user with isolated credentials.**

## M2 — Library & streaming

| Task | IDs |
|---|---|
| Per-member Navidrome JWT minting + stream URL issuance (15-min, scoped) | FR-STRM-001, 004 |
| Library proxy: albums, album detail, artists, artist detail, search, scan status | FR-LIB-001, 002, 004 |
| Stars (favorites) | FR-LIB-005 |
| Read-only playlist list/detail for members | FR-PL-002 |
| Admin playlist CRUD | FR-PL-001, 003 |
| Web: library screens, search, album/artist views, player skeleton (play/pause/next/prev/seek/volume) with FLAC/Opus negotiation | FR-STRM-002, NFR-COMPAT-002 |

Exit: **admin and a member both browse and stream FLAC in the browser; members cannot mutate playlists (403 verified).**

## M3 — Individual playback, persisted

| Task | IDs |
|---|---|
| `session_state` persistence, throttled writes, restore | FR-IND-001…002 |
| Queue management UI (add, remove, reorder, shuffle) | FR-IND-003 |
| Resume-on-login (queue + position ±5 s rule) | FR-IND-002 |

Exit: **close browser, reopen next day on another device — queue and position restored.**

## M4 — Synchronized engine (the hard one)

| Task | IDs |
|---|---|
| WS hub: auth on upgrade, per-user channels, event envelope, snapshot-request, ping/pong | FR-SYN-003, 007 |
| Sync executor + shared session state + Postgres checkpoints + sync_events log | FR-SYN-001…002 |
| Admin sync routes (play/pause/seek/next/prev/queue/end/start) | FR-SYN-001, 002 |
| Drift coordinator: heartbeats, decisions, seek-correction fan-out | FR-SYN-005, 006 |
| Late join/rejoin + stale-event rule | FR-SYN-005, 007 |
| DJ disconnect + 90 s grace + fallback | FR-SYN-008 |
| Mode switching + transition table T1–T6 + client-side transition effects | FR-MODE-001…003 |
| Web sync UI: shared queue, DJ status, drift readout, "DJ disconnected" state | FR-SYN |
| E2E: two Playwright clients, full DJ session incl. staggered joins | PLAYBACK_SYSTEM §10 |

Exit: **admin runs a listening session; two members stay within 2 s; DJ leaves and the 90 s rule fires; everyone lands back in Individual Mode with their old queues.**

## M5 — Admin: uploads, library care, settings, themes

| Task | IDs |
|---|---|
| Upload endpoint (streaming, validated) + music-volume write + auto scan | FR-ADM-005, 006 |
| Re-scan trigger + status | FR-ADM-001, 002 |
| Delete track/album (+ optional file removal), metadata edit | FR-ADM-003, 004 |
| Platform settings CRUD + audit logging (admin actions from M1/M4/M5) | FR-ADM-004, FR-ADM-007-lite |
| Theme system: 5 token themes, per-user choice, platform default, live switching | FR-THM-001…003 |

Exit: **admin's full daily loop: drop a new album in the web UI → scan → it's playable; remove an album; tweak settings; pick a theme.**

## M6 — V1.0.0 hardening and ship

| Task | IDs |
|---|---|
| Production compose + Caddyfile final (no published Navidrome port — deployment test from SECURITY §11.4) | DEPLOYMENT |
| Backup/restore scripts + docs | NFR-REL, SECURITY §10 |
| Performance pass: library proxy p95, WS latency (NFR-PERF-001/002), drift target NFR-PERF-003 | |
| Full e2e suite green on the pinned Navidrome tag | |
| Release: tag `v1.0.0`, images, changelog | |

Exit: **v1.0.0.**

---

## V1.x improvements (sequenced, each a mini-milestone)

| Item | Notes |
|---|---|
| M7: Sync crash-recovery (auto-resume shared session after backend restart) | FR-SYN-011; NFR-REL-004 upgrade |
| M8: Android app (Kotlin/Compose/Media3): background playback, notifications, lock-screen art, Bluetooth/media keys, audio focus, individual + sync mode | new `android/` module; same API; WS auto-resume; offline = V2 |
| M9: Desktop (Tauri 2, Linux+Windows): reuse web bundle, system media keys, tray, persistent session | new `desktop/` module |
| M10: Downloads (flag on: platform + per-user), stream-proxy short tokens (close the 15-min capture window) | FR-STRM-003 flag |
| M11: Random albums/songs (FR-LIB-003), genres (FR-LIB), duplicate detection on upload (content-hash pre-check) | |
| M12: Audit log viewer + pruning; library-changed WS events driven by scan-completion polling; `library-changed` event loop closed | FR-ADM-007 full |
| M13: Per-user stream format preference UI; Opus-default for mobile | NFR-COMPAT |
| M14: Custom themes (admin-uploaded token CSS, hot-apply), community theme submission flow | FR-THM-004 |

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
- Lossless-to-Opus adaptive: per-member bandwidth profiling auto-selecting transcode target (Navidrome's `getTranscodeDecision` already computes it; Flow would surface it).
- Hardware sync for a shared room (Navidrome jukebox for a physical speaker, Flow sync session for people) — "listen together in the living room" mode.
- Read-only guest access (time-boxed Navidrome shares surfaced through Flow, no account).
- iOS app.

---

## Biggest risks and their planned countermeasures

| Risk | Likelihood | Mitigation |
|---|---|---|
| Synchronized playback feels laggy/unsatisfying on the internet | Medium | 2 s threshold + monotonic clocks + rate-catch-up (PLAYBACK_SYSTEM §3.3); NFR-PERF-003 measured before V1 sign-off; a "relaxed sync" toggle (10 s tolerance) ships in M4 as config |
| Navidrome API drift between pinned tag and master | Low (we pin) | Integration CI against the pinned image; one-release-behind upgrade policy |
| Stream-URL capture (15-min window) | Low, A-004 | M10 stream-proxy short tokens; document the residual risk |
| Upload volume exhaustion / bad files | Medium | size caps, scan-after-upload, delete-admin route |
| Scope creep into V1 (themes go wide, favorites multiply) | Medium | this roadmap is the gate: anything not in M0–M6 is V1.x by definition |
| Two-database mental model (Flow PG + Navidrome SQLite) confuses operators | Low | DEPLOYMENT backup/restore docs + "index is rebuildable" note |

## First milestone to start

**M0**, specifically in this order:
1. repo scaffold (`backend/`, `web/`, `deploy/`, `.github/`), Go module, chi + config + slog + healthz
2. compose dev stack with pinned Navidrome + Postgres + `dev/music`
3. migrations for the full schema (write it all now, use it incrementally — avoids mid-milestone schema churn)
4. auth (argon2 + JWT + revocation) — admin login via curl
5. Navidrome admin-bootstrap client

Estimated: 1–2 weeks to M0 exit. M1 immediately after.
