# M3 implementation and verification report

Verified: 2026-11-07. Scope: individual playback only.

**Implementation is present; M3 acceptance remains pending the live PostgreSQL
and browser/Navidrome gates.** No M4 work was started. Passing controller doubles
and SQL smoke are not evidence that real browser playback passed.

## 1. M0–M2 readiness

[M3_READINESS.md](M3_READINESS.md) records the repository and documentation audit.
Authentication resolves the Flow cookie session into the current account;
protected requests enforce account status, completed provisioning and mandatory
password change. Library lookups use that account's Navidrome identity. The
gateway authenticates/rechecks media access, streams incrementally and supports
Range/transcodes, cancellation and lifecycle revocation.

The existing deterministic auth, membership, library, API, credential and gateway
tests still pass. They cover stream cancellation/suspension and credential
suppression. Live authenticated library/audio browsing was not verified here.
The earlier roadmap already left the extended M2 real-Navidrome gate pending;
the supplied milestone label does not establish that runtime evidence.

Two blocking setup defects were corrected: Vite now uses the configured 5173
origin with strict port selection; live-test Compose projects use an override
mounting generated fixtures instead of the operator's `/home/ruhin/Music` library.
Normal development media and existing working domain behavior were preserved.

## 2. Backend functionality

`internal/playback` owns individual persistence, validation and deterministic
queue/transport transitions. State contains media-ID occurrences, canonical
index/current ID, position, volume, shuffle, saved playing/paused intent,
revision and timestamp. Ownership always comes from the authenticated Flow
principal, including administrator operations on the administrator's own state.

All controls are implemented: replacement/current selection, play/pause, seek,
append/remove/move, clear, next/previous, volume/shuffle, checkpoints and natural
end. Member-scoped metadata validates new items and explicit selections, without
duplicating Navidrome metadata in PostgreSQL. Queue limit is 200; volume is 0–1
at 0.001 precision; position is bounded to 24 hours and known server duration
with a one-second rounding tolerance. Unavailable retained items remain removable
and an unchanged retained track can be paused during a library outage.

Duplicates have server-owned occurrence timestamps. Removing current selects
the first surviving successor, otherwise nearest predecessor, and resets
position. Move preserves selected occurrence and position. Empty state is safe.
Next at the end pauses without wrapping; previous restarts above three seconds,
otherwise traverses backward or resets the first track. Stable SHA-256 shuffle
orders occurrences for traversal without changing the canonical queue. No repeat.

## 3. Frontend functionality

One account-keyed React provider owns one controller and one audio element.
Navigation preserves that element and source. The global bar displays metadata,
artwork, transport/buffering/errors, time/progress, volume, shuffle and queue.
Queue view supports select, remove, move and clear, including empty/unavailable
states. Library controls append to or start the shared queue: album/playlist and
artist listings replace with the ordered listing; flat song/search playback
replaces with one selected song. Discography albums are playable.

Audio uses only `/audio/{mediaId}`. Source replacement/retry/teardown explicitly
pauses, removes the source and calls `load()` to cancel the old stream. Native
original seeking uses browser Range behavior; transcodes reload with integer
`offsetSeconds`. Loading, buffering, autoplay rejection, failure and explicit
retry are handled. Optional Media Session controls use the same mutations.

Restore loads queue/metadata, volume, shuffle and the last saved position without
autoplay, even if saved intent is playing. There is no age cutoff; a position at
or beyond a known duration locally resets to zero. Actual browser transport is
distinct from saved intent. Natural end waits for earlier local acknowledgements
and verifies its captured occurrence; delayed errors/end cannot control a new
source. A failed stopped-intent write preserves the reconciled conflict display.

## 4. Database migration

`0003_m3_individual_playback.up.sql` creates `session_state`, with `user_id` as
primary key/foreign key, `ON DELETE CASCADE`, field and canonical-empty/current
constraints, backfill for existing accounts and an AFTER INSERT user trigger.
Account creation and default session creation are transactional. Exactly one row
is used across tabs/devices. Suspension/soft deletion retain state; clear updates
the existing row while preserving volume, shuffle and monotonic revision.

The down migration removes the trigger/function/table; it discards playback
state, so use forward migrations for production. The Go migration runner and
existing auth/membership tables remain in place. No shared-session table exists.

## 5. API contracts

The documented routes are implemented without separate invented control routes:

| Method/path | Behavior |
|---|---|
| GET `/api/playback/session` | Current user's entire session, including queue/defaults |
| PUT `/api/playback/session` | Partial state mutation, `queueEdit`, or `command`, with `ifRevision` |
| DELETE `/api/playback/session` | Clear with JSON `{ifRevision}`, return the updated session |

PUT additively supports `status`, `queueEdit:{add/remove/move}` and
`command:next/previous/checkpoint/ended`. Success returns the full snapshot.
409 contains `error.code:"conflict"`, `currentRevision` and `currentSession`.
Unknown owner fields/query keys and invalid values return the existing 400 error
shape. Account/session gates and member-scoped media errors retain API conventions.
Full contracts are in [API_SPECIFICATION.md §4](API_SPECIFICATION.md).

[ADR-007](decisions/ADR-007.md) resolves the old PUT/PATCH, hot-cache/Zustand,
missing transport intent, queue-edge and checkpoint ambiguities. Related product,
requirements, architecture, schema, API and playback documents were reconciled.

## 6. Atomic CAS

The proposal read grants no write authority. Every mutation commits with
`UPDATE session_state ... WHERE user_id=$owner AND revision=$expected RETURNING ...`,
incrementing revision and timestamp atomically. The commit transaction rechecks
and share-locks the current account and unexpired authenticated Flow session in
lifecycle lock order; no database lock spans a Navidrome request. A zero-row write
reads the current owned snapshot and returns 409. One successful CAS is
authoritative; stale writes cannot overwrite or merge it.

Executed service concurrency tests prove one winning write for a revision using
an atomic test store. Real simultaneous pgx/HTTP tests exist but have not executed
here. The SQL smoke proves sequential stale conditional-write rejection only.

## 7. Checkpointing and durability

Progress events render without writes. While actively playing, the provider
checkpoints every **10 seconds**, capturing revision, track, canonical index and
actual position together. Intentional controls commit immediately; pause and
seek include position, transitions commit the new selected state, and natural
end commits the ended position or next-track state. Failures checkpoint stopped
position and paused intent. No unload/page-close persistence handler exists.

A stale checkpoint adopts the 409 snapshot and is discarded, never rebased.
Natural end samples dispatch revision/index after earlier local commands only if
its captured occurrence is still selected. Failed/uncertain commands pause and
read back state instead of replaying. Durability is the last acknowledged CAS;
the usual foreground loss window is 10 seconds plus request latency, and timer
throttling or network failure can extend it.

## 8. Multiple devices/tabs

Clients refresh REST state every **15 seconds**, plus visible focus/online.
Newer state or 409 pauses local audio and restores the authoritative snapshot.
409 shows a notice and discards pending stale actions; a fresh user action uses
the reconciled revision. Older in-flight reads are ignored. No automatic queue
merge, silent overwrite, stale retry, exclusive device lease or remote transport
protocol exists. Separate devices can briefly play independent audio; durable
state is coordinated through CAS, without M4 synchronization.

## 9. Executed verification

Commands use `backend/` or `web/` as indicated. Final results:

| Check | Actual result |
|---|---|
| Backend `GOCACHE=/tmp/flow-go-cache go test -race -count=1 ./...` | PASS across all enabled packages; PostgreSQL-dependent tests skipped |
| Backend `go vet ./...` with the same GOCACHE | PASS |
| Backend `go build -o /tmp/flow-m3-flowd ./cmd/flowd` with the same GOCACHE | PASS |
| Backend `go test -tags=integration -count=1 -v ./...` with the same GOCACHE | PASS enabled tests and compilation; live Navidrome/M3 tests skipped |
| Web `npm test` | **33 passed, 0 failed, 0 skipped** |
| Web `npm run typecheck`, `npm run lint`, `npm run build` | PASS |
| Root `python3 dev/test-playback-schema.py` | PASS on PostgreSQL **18.6**, single-user SQL smoke |
| Fixture generation, FFprobe and shell syntax | PASS; 30-second Checkpoint and 5-second Transition FLACs |
| CI YAML/Compose configuration and `git diff --check` | PASS |
| Docker/TCP/Unix listener probes | Docker denied; TCP socket creation and Unix binding denied |
| Chromium DevTools startup probe | FAILED: Chromium closed its DevTools pipe before assertions |
| Real PostgreSQL 16/pgx/browser/Navidrome/process-restart gates | **NOT EXECUTED / NOT PASSED** |

The new stopped-intent reconciliation regression first failed against the old
callback, then passed after correction. Final frontend tests cover production
controller/API modules with transport/server doubles: restore, controls, queue,
progress/checkpoints, end races, errors/autoplay/retry, conflicts/two clients,
uncertain responses, auth invalidation and teardown. React DOM controls and real
decoding/navigation are covered by the unexecuted browser harness.

Skipped tests: `TestMigrationsAgainstPostgres`,
`TestRequestApprovalProvisioningLoginPasswordChangeAndSuspend`,
`TestIndividualPlaybackPostgres`, `TestPinnedNavidromeContract` and
`TestM3LivePlayback`. Disposable service variables were absent. The SQL smoke
checks upgrade/backfill/trigger, constraints, sequential conditional writes,
cross-user isolation, PostgreSQL backend-process restart and down/reapply; it
does not exercise pgx, simultaneous clients, Flow authentication or browser audio.

Local toolchains: Go **1.27.3**; Node **26.13.0**; PostgreSQL **18.6**.
CI/project pins Node **20.18.2** and PostgreSQL **16**. A local result under the
newer tools does not establish a pass under those pins. No production dependency
was added. `.github/workflows/playback-live.yml` runs pinned real gates; no CI
execution is claimed in this report.

## 10. Remaining acceptance gates and limitations

The implementation has passing deterministic checks; no known unresolved
architecture decision remains. M3 is **not marked complete** until the real pgx
CAS/lifecycle tests and actual Flow/Navidrome/browser/restart gate pass. The
extended M2 real contract also needs its live run. Docker/listener restrictions
prevent those checks here, and Chromium cannot start. Browser compatibility and
the new live harness's runtime behavior remain unverified.

V1 limitations are intentional: last-checkpoint progress durability, background
timer throttling, explicit retry for connectivity/media failures, no automatic
skipping of missing media, bounded queue, and independent audio on devices.
Navidrome validation and PostgreSQL persistence cannot form one transaction;
the existing gateway rechecks media at stream time. No M4/upload/native/offline
or multi-instance code or placeholders were introduced.

## 11. Run and test locally

Use [README.md — Run M3 Locally](../README.md#run-m3-locally) for first-run
Navidrome admin/keyring/env setup. With that existing setup, from the repo root:

```bash
docker compose --env-file deploy/.env.local -f deploy/docker-compose.dev.yml up -d postgres navidrome
# Backend terminal
cd backend
set -a
source ../deploy/.env.local
set +a
go run ./cmd/flowd
# Separate web terminal, from repository root
cd web
npm ci --ignore-scripts
npm run dev
```

Open `http://127.0.0.1:5173`. Backend startup applies the migration automatically.
For fast checks, in separate shells from repository root:

```bash
cd backend
GOCACHE=/tmp/flow-go-cache go test -race -count=1 ./...
GOCACHE=/tmp/flow-go-cache go vet ./...
GOCACHE=/tmp/flow-go-cache go build -o /tmp/flowd ./cmd/flowd
GOCACHE=/tmp/flow-go-cache go test -tags=integration ./...
```

```bash
cd web
npm test
npm run typecheck
npm run lint
npm run build
```

Use [DEVELOPMENT_GUIDE.md §6.3](DEVELOPMENT_GUIDE.md#63-m3-checks-and-live-playback)
for the disposable PostgreSQL, test-only Compose fixture mount and Chromium
command. `FLOW_TEST_DATABASE_URL` enables pgx tests; `FLOW_REAL_NAVIDROME_*` with
explicit disposable mutation authorization enables the real contracts;
`FLOW_BROWSER_EXECUTABLE` enables the real browser gate. Missing variables cause
skips, not acceptance. Test fixtures are generated by
`sh dev/create-playback-fixture.sh`; SQL smoke alone is
`python3 dev/test-playback-schema.py`.

## 12. Exact next milestone

**M4A — Synchronization State Machine and Backend Executor.**

Start only after the remaining M3 acceptance gates pass and further development
is requested. This implementation stops at M3.
