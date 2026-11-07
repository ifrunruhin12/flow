# Flow — Architecture Decision and Reconciliation Status

Status: V1 architecture baseline after reconciliation
Date: 2026-11-07

## Accepted baseline

- Go modular monolith; one backend instance in V1.
- Go 1.27 standard `net/http` routing; no web framework dependency is required for M0.
- PostgreSQL owns Flow state. Navidrome owns its separate SQLite database; Flow never writes it.
- Navidrome provides indexing, metadata reads, streaming/transcoding, playlists, and scanning behind Flow's private network boundary.
- Flow is the only client-facing gateway. Audio bytes are incrementally proxied through Flow; control events use WebSocket.
- Browser authentication uses opaque server-side sessions (M1).
- Navidrome first-admin creation is operator-assisted.
- Member provisioning is durable and retry-safe; the encrypted reusable member credential commits before any remote create call.
- Individual playback uses one row per user and revision CAS. A 409 requires explicit client reconciliation; V1 has no automatic merge algorithm.
- M3 follows ADR-007: direct PostgreSQL reads/CAS, saved transport intent, one shared React controller/audio element, 10-second checkpoints, 15-second REST refresh and no autoplay on restore. No backend playback hot cache or additional state dependency.
- Synchronized playback uses one executor, `version` for content, `event_seq` for every authoritative event, one composite event per transaction, and checkpoint-before-broadcast.
- DJ authority is a server-issued generation + rotated opaque token bound to one authenticated connection. Generation alone is not authority.
- Reconnect consistency is subscribe-before-snapshot, buffered drain, then live delivery.
- A backend restart ends shared sessions safely; individual sessions survive.
- Upload staging and the watched library are siblings on one filesystem. V1 promotes one fully validated new album directory with one atomic rename.
- No Redis, Kafka, microservices, Kubernetes, or separate sync service in V1.
- M0 uses standard `net/http`, an embedded SQL migration ledger, and a PostgreSQL advisory lock; no router or migration framework is present.
- M0 ships development Compose only. Production Compose/Caddy and the web bundle are M6 deliverables.
- M1 stores the Navidrome credential username separately from the Flow login username. This is required for the operator-linked admin, whose two configured usernames may differ; AEAD associated data binds to the Navidrome credential identity.
- The single V1 Flow administrator password is operator-managed by `FLOW_ADMIN_PASSWORD`; changing the deployment secret and restarting rotates the hash and revokes admin sessions. Member passwords use the normal self-change/reset flow.
- Hard deletion persists `navidrome_cleanup_state` on the user row so compensation is visible and retryable in M1 without depending on the deferred audit viewer. Cleanup reconciles uncertain remote creation with the durable credential before deleting.
- M4A uses `POST /api/platform/mode` as the sole public mode-transition API. Lease acquisition/disconnect are trusted service boundaries for the future M4B hub; no public route accepts a caller-chosen connection ID to establish DJ authority.
- M4A persists `settings`, `shared_sessions`, and `sync_events`; mode changes, checkpoints, authoritative events, and the existing append-only `mode.changed` audit record share the same transaction.
- `sync_participation`, heartbeats, WebSocket subscribe/snapshot delivery, and synchronized browser transport are M4B work and are not M4A placeholders.

## Contradictions resolved

| Conflict | Resolution and reason | Documents updated |
|---|---|---|
| Docs said audio used Flow but also said no audio transited Flow. | Audio bytes transit the incremental Flow proxy; only the sync bus carries no audio. This matches ADR-001 and credential isolation. | PRODUCT_SPEC, ARCHITECTURE, PLAYBACK_SYSTEM, ADR-001 |
| Database stored encryption keys beside ciphertext. | PostgreSQL stores ciphertext + version metadata only. Versioned master keys live in an external mounted keyring, backed up separately. | DATABASE_SCHEMA, SECURITY, DEPLOYMENT, ARCHITECTURE, ADR-003/004 |
| Provisioning created the remote user before durably storing its password. | `credential_ready` commits encrypted `P_m` before remote I/O; retries reuse it and prove ownership by authenticating, not username equality. | REQUIREMENTS, DATABASE_SCHEMA, NAVIDROME_INTEGRATION, ADR-004, ROADMAP |
| Separate staging volume was paired with an atomic-rename guarantee. | Canonical topology is a single filesystem root with sibling `library/` and `.flow-staging/`; V1 atomically renames one complete new album directory. | PRODUCT_SPEC, REQUIREMENTS, ARCHITECTURE, DATABASE_SCHEMA, SECURITY, DEPLOYMENT, NAVIDROME_INTEGRATION, ROADMAP |
| DJ generation could be presented by multiple sockets. | Server issues generation/token and binds both to one connection; reconnect/takeover rotates and fences; every command validates all lease fields. | REQUIREMENTS, DATABASE_SCHEMA, API_SPECIFICATION, PLAYBACK_SYSTEM, ADR-005 |
| Snapshot then subscribe could miss event N+1. | Register/buffer subscriber first, read/send committed snapshot, drain buffered events, then go live. | REQUIREMENTS, API_SPECIFICATION, PLAYBACK_SYSTEM, ADR-006 |
| Event docs alternated between one event per version and one per sequence. | Exactly one authoritative composite event per executor command/transaction and `event_seq`; every event carries current `version`. Transient corrections do not consume sequence values. | ARCHITECTURE, DATABASE_SCHEMA, API_SPECIFICATION, PLAYBACK_SYSTEM, ADR-006 |
| API docs exposed both `/sync/session` and `/platform/mode` for start/end, while lease acquisition depended on a WebSocket connection M4A does not create. | `/platform/mode` is canonical for start/end. DJ lease acquisition/disconnect remain authenticated, connection-bound service calls for M4B; M4A exposes only fenced playback commands and state reads. | API_SPECIFICATION, ARCHITECTURE, PLAYBACK_SYSTEM, M4A_READINESS |
| Individual playback prescribed undefined queue union/LWW behavior. | CAS mismatch returns 409 + current state. Client reloads or explicitly retries replacement; no automatic merge. Essential mutations commit immediately; playing position may be client-throttled. | REQUIREMENTS, DATABASE_SCHEMA, API_SPECIFICATION, PLAYBACK_SYSTEM |
| Gateway promised Range/206 for all streams. | Range is representation-dependent. Flow forwards upstream behavior and never synthesizes byte-range support for a non-seekable transcode. | REQUIREMENTS, API_SPECIFICATION, PLAYBACK_SYSTEM, ADR-001 |
| Product role table required metadata editing/deletion while roadmap deferred it. | V1 includes metadata reading, upload, favorites, five themes, member deletion, and admin playlists. Tag editing and both index/file deletion are V1.x. | PRODUCT_SPEC, REQUIREMENTS, API_SPECIFICATION, ROADMAP, README |
| M0 included Flow browser authentication despite the milestone boundary. | M0 is foundation + verified Navidrome adapter only. Browser auth/admin mapping move to M1. | ROADMAP, DEVELOPMENT_GUIDE, DEPLOYMENT, API traceability |
| Dev backend ran on host while infra published no ports. | Dev Compose binds PostgreSQL/Navidrome to loopback; production publishes neither. | DEVELOPMENT_GUIDE, DEPLOYMENT |
| Architecture named `chi` and `golang-migrate`, while the interrupted M0 implementation used standard `net/http` and a small embedded runner. | The implementation-aligned choices are normative for M0: standard routing plus an advisory-locked ledger/transaction runner. This avoids undocumented dependencies and still satisfies the contracts. | ARCHITECTURE, DATABASE_SCHEMA, DEVELOPMENT_GUIDE, ROADMAP |
| README and DEPLOYMENT said production Compose, Caddy, and a web app already existed. | M0 documents and ships only `docker-compose.dev.yml`; production files and the web project remain milestone deliverables. | README, DEPLOYMENT, DEVELOPMENT_GUIDE, ROADMAP |
| Sync start/restart wording implied multiple authoritative events or a row-only restart update. | Session start is one composite `session-started` event at v0/seq1. Restart safe-end commits checkpoint + terminal event + mode change transactionally. | ARCHITECTURE, API_SPECIFICATION, PLAYBACK_SYSTEM, ADR-005/006 |
| Playback docs mixed `offsetMs` and `offsetSeconds` and invented an age-based resume reset. | The public gateway uses integer `offsetSeconds` only for initial transcoded seeking; normal seeking follows verified representation behavior. Individual resume restores the last durable checkpoint without an undocumented age reset. | API_SPECIFICATION, PLAYBACK_SYSTEM, ROADMAP |
| M1 required live-stream teardown and upload authorization tests before either feature existed. | M1 owns account/session lifecycle; M2 integrates the session/user stream registry; M5 owns upload-route authorization. | ROADMAP, SECURITY, ADR-001/002 |
| Session revocation semantics named only a per-user stream registry. | Stream handles are indexed by session + user: logout flushes one session, suspension/deletion flushes the user. | ARCHITECTURE, NAVIDROME_INTEGRATION, SECURITY, ADR-001/002 |
| The streaming sequence diagram opened Navidrome before registering the stream, leaving a suspension race. | M2 registers under the session/user first, rechecks authoritative account state, then performs media lookup and opens upstream. A concurrent user block either rejects or cancels the registration. | ARCHITECTURE §5.2, SECURITY §4, M2 `streaming.Registry` tests |
| API text said original quality should omit `format`, while pinned source exposes an explicit raw/original selector. | M2 sends `format=raw`; omission could select a player preference. The deviation is documented and added to the real pinned suite. | API_SPECIFICATION §2.1, NAVIDROME_INTEGRATION §3.1 |
| The schema said migration `0001` seeded five themes, while M0's accepted boundary makes `0001_m0_foundation` infrastructure-only. | Theme tables and seed data are owned by M5; M0 creates no future-feature schema. This preserves incremental milestone ownership. | DATABASE_SCHEMA, DEVELOPMENT_GUIDE, ROADMAP |
| One settings paragraph said Flow refused to start on a Navidrome gate failure, while bootstrap/runbook contracts require a reachable degraded process for diagnosis. | The backend remains ready for health/bootstrap diagnostics, marks the integration degraded, and blocks product/Navidrome operations until configuration is repaired. | DATABASE_SCHEMA, DEPLOYMENT, NAVIDROME_INTEGRATION, ADR-003 |
| README referred developers to migration/live-contract commands that were not present in the development guide. | Added explicit disposable-database and isolated Compose-project commands, including destructive-test warnings and digest capture. | README, DEVELOPMENT_GUIDE |
| Session expiry still referenced removed draft identifier `U-004`. | The stable owner is `FR-AUTH-004`; all session schema and ADR references now use that requirement ID. | DATABASE_SCHEMA, ADR-002, REQUIREMENTS |
| `FR-LIB-001` was labeled V1 must-have while also embedding genre browsing as V1.x. | `FR-LIB-001` now owns V1 album/artist browsing; new stable `FR-LIB-006` owns V1.x genre browsing and M11/API references it explicitly. | REQUIREMENTS, API_SPECIFICATION, ROADMAP |
| Theme scope wording grouped no-reload switching with V1.x community uploads, while `FR-THM-003` and M5 require live switching among the five V1 themes. | V1 switches among the five built-in token themes without reload; uploading and hot-applying new community themes remains V1.x. | PRODUCT_SPEC, REQUIREMENTS, ROADMAP |
| The live contract test assumed disabled `getShares` returned an HTTP-200 Subsonic failure envelope. | `v0.64.2` registers disabled sharing commands with `h501`, so the verified contract is HTTP 501. The test now checks that exact gate behavior. | NAVIDROME_INTEGRATION, real Navidrome contract suite |
| M1 required approved members to log in but registration, approval, and direct-create contracts supplied no Flow password or invitation mechanism. | The approved V1 design uses an admin-supplied temporary Flow password communicated out-of-band, a restricted first-login session, and mandatory password replacement. | REQUIREMENTS, API_SPECIFICATION, DATABASE_SCHEMA, SECURITY, ADR-002, ROADMAP |
| The Flow administrator username was configurable separately from the Navidrome administrator username, but the credential row stored only the Flow username while AEAD requires stable credential identity. | Store `navidrome_username` beside ciphertext/key-version metadata and bind AEAD to it. Member values equal their Flow username; admin values may differ. | DATABASE_SCHEMA, SECURITY, M1 migration/bootstrap implementation |
| Individual summaries named PATCH, a backend hot cache and Zustand, despite the accepted PUT API and existing React context architecture. Play/pause intent and queue/checkpoint edges were unspecified. | ADR-007 preserves GET/PUT/DELETE, uses direct PostgreSQL CAS, adds saved intent and additive control fields, and defines the shared controller, deterministic queue and guarded checkpoint/restore behavior. | API_SPECIFICATION, PLAYBACK_SYSTEM, DATABASE_SCHEMA, ARCHITECTURE, DEVELOPMENT_GUIDE, REQUIREMENTS |
| Historical unavailable queue entries could prevent current-item removal or an intentional pause; natural end could self-conflict with an earlier local write. | Retained removal is network-independent; paused checkpoints remain bounded when duration lookup is unavailable. Natural end verifies the captured occurrence after earlier local acknowledgements before sampling dispatch guards; no stale request is replayed. | ADR-007, API_SPECIFICATION, PLAYBACK_SYSTEM, M3 tests |

## Navidrome evidence status

- Selected release: `v0.64.2`, source commit `101145742f4762164202cb4858c9126258bdc463`. The local Navidrome checkout is newer, so tag-specific claims were inspected from the exact tag object rather than from checkout HEAD.
- Source-verified at that tag: first-admin route and zero-user guard; native login payload; native user CRUD authorization; user-library route; browse/search/stars/playlists/scan routes; disabled sharing commands returning HTTP 501; explicit `format=raw`; integer `timeOffset`; seekable `http.ServeContent`; non-seekable `Accept-Ranges: none`; Opus/MP3 transcode paths; Viper `ND_` environment mapping and gate defaults.
- Executable real-contract coverage exists for first-admin/login, admin verification, native user CRUD and library assignment, member authorization boundaries, browse/search, playlist behavior, scan authorization/lifecycle, original Range/206/content type, MP3 + Opus transcode behavior, and download/sharing/artwork gates. The suite now also exercises the M2 Flow adapter path; those new cases compiled but were not executed in the latest Docker-restricted environment, so they remain runtime-unverified until CI/local pinned execution succeeds.
- Runtime-verified through the operator-accessible development stack: PostgreSQL migration concurrency/up/down and the full real contract suite passed against Navidrome `0.64.2`. The image is pinned as `deluan/navidrome:0.64.2@sha256:38dc2727bfcfd5ede290f8ada114fc90368146f265ae4701ddddbcbe2a44ee52`.

## Remaining decisions

No unresolved architecture or V1 scope decision blocks M3 implementation. M3 code
and deterministic evidence are present; live PostgreSQL and browser/gateway/
Navidrome/process-restart completion gates remain pending in the restricted
implementation environment. See M3_REPORT for actual results. No M4 code started.
The exact next milestone is M4A — Synchronization State Machine and Backend
Executor, after the M3 completion gates pass and further work is authorized.
