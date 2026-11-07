# Flow — Playback System

Version: 1.1.0-draft (supersedes 1.0.0-draft)
Covers: individual sessions, the synchronized engine, drift model, reconnection, mode transitions, failure modes.
Individual playback is implemented by `playback` and the shared browser player
(ADR-007). Synchronized sections describe later M4 contracts with the future
`sync` package/WS layer; M3 introduces none of that implementation.
Key revisions: restart policy is safe-end (ADR-005); version vs. event sequence distinction (ADR-006); DJ lease generation; individual-session revision control (task F); audio always via the streaming gateway (ADR-001).

---

## 1. Ground Rules

1. **No audio relay through the sync bus.** The Flow server never carries audio in the WebSocket. Audio for every client (including the DJ's own listening) flows through the **streaming gateway** (`/audio/…`, ADR-001) directly to Navidrome server-side; the WS carries control + state only.
2. **Server authority.** The shared session object lives in the Flow backend (in-memory, checkpointed to Postgres). Clients propose nothing; they report.
3. **Single writer.** One executor goroutine is the only mutator of shared session state (task E). All mutations are serialized through it.
4. **Postgres checkpoints are authoritative** for what has happened. The in-memory copy is the live working state; the checkpoint is the crash-boundary (ADR-005).

## 2. Individual Mode (baseline, task F)

State per user (Postgres `session_state`, read directly) — **one persisted session per account, shared across all that account's devices** (ADR-007):

```go
type IndividualSession struct {
    Queue        []QueueItem // {MediaID, AddedAt}
    QueueIndex   int
    CurrentMedia string
    PositionMs   int64
    Volume       float32
    Shuffle      bool
    Status       string // paused | playing: saved intent, not actual browser transport
    Revision     int64   // optimistic concurrency counter
    UpdatedAt    time.Time
}
```

- Client owns transport locally (play/pause/seek are client-side `<audio>` controls via the `/audio` gateway; the server only persists state for resume).
- **Concurrency (multi-device):** every write carries `ifRevision`. The server applies it with a single `UPDATE … WHERE user_id=? AND revision=?`, then `revision++`. On mismatch it returns `409 {currentRevision, currentSession}`. V1 performs no automatic queue union or last-writer-wins merge: the client reloads, shows the conflict where appropriate, and may deliberately replace state only with a new CAS request against `currentRevision`.
- Persistence cadence: intentional play/pause, queue/current-track/index, seek, track-end, shuffle, and volume changes commit immediately. While continuously playing, actual audio position is checkpointed every 10 seconds. There is no unload persistence handler.
- Resume: on login, `GET /playback/session` → client restores the last durably checkpointed queue, track, and `PositionMs`. V1 does not invent an age-based reset rule; the user can explicitly restart the track.

### 2.1 Canonical queue and controls

- Empty: index 0, null current media, position 0, paused. Default volume 1,
  shuffle false. Clear retains personal volume/shuffle and advances revision.
- Queue entries contain only media IDs and server-added timestamps. Duplicates
  are allowed; occurrence identity survives reorder/removal. Maximum 200.
- Current media always equals `queue[queueIndex].mediaId` for a nonempty queue.
  A selected index must be in bounds; contradictory ID/index is invalid.
- Add appends; first addition to an empty queue selects index 0 and stays paused.
  Remove noncurrent entries preserves the current occurrence and its position.
  Remove current selects the first surviving successor, otherwise the nearest
  predecessor; resets position, retains intent while nonempty. Removing all
  entries gives the empty state. Move uses the final destination index and
  preserves current occurrence/position even for duplicate media IDs.
- Full replacement validates every supplied media ID. Without explicit selection
  it retains the current occurrence if present, otherwise uses the same removal
  rule. New track/occurrence selection resets position unless explicitly supplied.
- Next moves forward and resets position. At the last item it keeps current
  track/position and pauses. Previous restarts the current track when sampled
  position is greater than 3,000 ms; otherwise moves backward, or restarts the
  first item at zero. Empty next/previous are safe accepted no-ops.
- Shuffle never rewrites the canonical queue: traversal sorts occurrences by
  SHA-256 of `mediaId + NUL + addedAt` (canonical index breaks ties). It is stable
  across devices/restarts, has no repeat/wrap, and retains the selected occurrence
  on toggle. Next/previous use this order. Natural end follows next semantics.
- New items and selected destinations are checked through member-scoped getSong.
  Removal is an exception for its retained destination: a missing successor or
  predecessor cannot prevent removing historical queue occurrences.
  Historical unavailable entries remain visible/removable; playback stops with
  an unavailable message rather than silently deleting or skipping them. A
  Navidrome outage preserves the queue and is retryable by an explicit gesture.
- Album/playlist playback replaces the queue with the ordered listing and starts
  the selected item. Flat song/search playback replaces it with the chosen song.
  Artist top-track listings work like a listing; discography albums can be opened
  and played. Add-to-queue always appends to the one shared player.

### 2.2 Checkpoints, restore, and browser lifecycle

Sample checkpoint revision, current media ID, canonical index, and actual media
position together. A checkpoint only updates position of that still-playing
occurrence. CAS or identity mismatch returns the full 409 snapshot, which the
browser adopts while pausing; the losing checkpoint is discarded. It is never
rebased onto a newer revision. Natural-end identity guards prevent a delayed
audio event from advancing a newly selected track. Natural end waits for this
browser's earlier pending commands and verifies the captured occurrence before
sampling the dispatch revision/index; a volume/checkpoint acknowledgement cannot
lose the end event. It is never retried after an external conflict.

Before intentional transition/pause, capture actual position in the same command.
Seek commits before moving transport. A failed/uncertain command pauses locally,
shows that persistence failed, and refreshes state; it is not blindly retried.
Only successfully committed state is durable. The usual foreground loss window
is up to 10 seconds plus request latency; background throttling/offline periods
can extend it. Position progress events only render UI and never write the DB.

One provider per authenticated account owns one audio element; navigation changes
neither its identity nor source. Replacement explicitly pauses, removes the old
source, and calls `load()` to cancel it before opening `/audio/{mediaId}`. Cleanup
removes listeners/timers/Media Session handlers and cancels the stream. Original
seeking uses the element's supported Range behavior. Non-seekable transcodes are
reloaded with integer `offsetSeconds`; the displayed position includes that base
offset. Seeking while paused stays paused. Buffering, load errors, autoplay
rejection, and explicit retry are visible. Media Session is optional and uses the
same persisted commands, never an independent player.
Stream failures persist actual stopped position and paused intent. A rejected or
discarded failure checkpoint cannot restore an old error over reconciliation or
a replacement source. Retry cancels
and reloads the failed source. A retained unavailable destination from queue
removal pauses and saves paused intent without deleting any other occurrences.

Restore uses the durable position at any session age. There is **no age-based
resume threshold** (zero minimum); when it is at/beyond a known duration, local
resume starts at zero. Unknown duration uses the validated saved position.
Saved `playing` intent does not auto-play on login/refresh or external state
adoption: actual audio remains paused until a gesture. Metadata is fetched from
the library, never copied into `session_state`; unavailable metadata cannot crash
the player.

### 2.3 Multi-tab/device behavior

Serialize this browser's mutations; refresh every 15 seconds and on visible
focus/online. Ignore older in-flight GET results. Adopt a newer REST snapshot by
pausing local audio, restoring the state, and showing that another client changed
playback. On 409, cancel pending stale actions, adopt the server snapshot, and
allow a fresh user gesture against its revision. No queue union, no silent
overwrite, no automatic retry. Two devices can listen at once; there is no
exclusive device lease, immediate remote transport control, or synchronization.

## 3. The Synchronized Engine

### 3.1 State

```go
type SharedSession struct {
    ID             int64
    Version        int64 // content version: bumped on play/pause/seek/next/prev/queue
    EventSeq       int64 // event sequence: bumped on EVERY authoritative state event
                        // (content + lifecycle: session-started, dj-disconnected,
                        // dj-reconnected, session-ended)
    State          SessionState // active | paused | ended
    DJUserID       int64
    DJLeaseGeneration int64    // issued and incremented only by the server
    DJConnectionID string      // server-issued active WebSocket connection id
    DJLeaseTokenHash [32]byte  // raw opaque token is never persisted
    DJConnected    bool
    DJDisconnectedAt time.Time // persisted freeze instant while in grace
    DJGraceDeadline time.Time  // persisted (timestamptz): DJDisconnect + settings.dj_grace_period_s
    CurrentMedia   string
    PositionMs     int64 // position measured at RefTs
    RefTs          time.Time // server wall-clock at last content mutation
    Queue          []string
    QueueIndex     int
}
```

**Version vs. event sequence (task E / ADR-006) — the two counters, precisely:**

- `Version` orders **content** of the shared state. Only content mutations bump it. Clients use it to decide "is this event's payload newer than what I have?"
- `EventSeq` orders **all** authoritative events, including lifecycle events that do *not* change content (a `dj-disconnected` freezes the state; its content version is unchanged but it still needs a place in the total order, otherwise two different clients can't agree on "what happened between my last event and this one").
- **Invariant: exactly one authoritative state event is emitted per `EventSeq` value**, and it is checkpointed to Postgres *before* the WS broadcast (same transaction: `shared_sessions` row + `sync_events` row). Every event carries the current `Version`. One executor command produces one composite event even when it changes several fields. Per-client corrections/acks are transient, have `basedOnEventSeq`, and do not allocate a sequence.
- Consequence: clients can reject stale frames and detect gaps by `EventSeq` alone; `Version` disambiguates "same content, new lifecycle fact".

Time model: "now" for the shared timeline = `PositionMs + (nowServer - RefTs)` while `State == active`; frozen while `paused`. All clients compute their target from **server-sent** timestamps (`RefTs`, event `ts`), never from their own clock.

### 3.2 Admin Mutation Protocol (single-writer executor)

Every admin control action follows one path:

```
    admin client ──HTTP POST /api/sync/session/{action}──► Flow
    Flow (sync engine, single-goroutine executor — the ONLY writer):
      1. validate state (409 on version mismatch from the client's `ifVersion`, 410 if ended)
      2. mutate state; Version++ only if content changed; EventSeq++ once
      3. Postgres checkpoint (UPDATE shared_sessions + INSERT sync_events — same transaction; this is the authority)
      4. WS broadcast event {type, version, seq: EventSeq, payload, ts} to all participating clients
```

The single-goroutine executor (a channel-fed loop) is why there is no lock around shared state: one event at a time, total order guaranteed, and `EventSeq` is that total order made visible to clients. Step 3-before-4 is the crash invariant: **a crash after step 3 and before step 4 means clients catch up via snapshot on reconnect; a crash between 2 and 3 means the mutation never happened (no phantom events).**

Lifecycle events (`dj-disconnected`, `dj-reconnected`, `session-ended`) follow the same path through the executor: `EventSeq++`, content `Version` unchanged, checkpoint + broadcast. They still carry that unchanged current version.

### 3.3 Drift and Correction

Drift correction uses **actual media playback position** as reported by the client — not wall-clock math alone — and accounts for buffering, seeking, playback rate, and the client's monotonic clock (task E).

Definitions (per member `m`, at server time `t`):

- Expected target: `E(t) = PositionMs + (t - RefTs)` while playing; `E = PositionMs` while paused. (Content-based.)
- Reported position: `R(t)` — the client's **actual media playback position** at the time it sends the heartbeat.
- Client position accounting (client-side, between heartbeats): the client measures its own position with a **monotonic clock** (Web: `performance.now()` deltas over the media element; Android: `SystemClock`-based). Position = `positionAtLastSeek + monotonicDeltaAdjustedForRate`. **Playback rate** is factored in (if the client is catching up at 1.25×, its monotonic advance is 1.25× wall time). **Seeking** resets the anchor (new `positionAtLastSeek`). **Buffering** is reported separately as `bufferMs` and must not be conflated with "drifting ahead": a buffered client's *playback head* is still the position anchor.

```
D = R(t) - E(t)        // negative: behind the shared timeline; positive: ahead
```

Corrections:

| Condition | Action |
|---|---|
| `\|D\| ≤ threshold` (default 2000 ms) | nothing |
| `D < -threshold` (behind) | send `seek-correction {target: E(t_now)}`. Client: if gap > 2 s, seek immediately; else catch up via rate 1.05–1.25× (client decision, no extra event). Respect current buffering: do not seek while a seek would interrupt an active buffer underrun. |
| `D > +threshold` (ahead) | send `seek-correction {target: E(t_now)}` immediately (seek backward). |
| track mismatch (`m.currentMedia != session.CurrentMedia`) | client loads the correct track at `E(t)` (fresh `/audio` request with integer `offsetSeconds=floor(E(t)/1000)` only for an initial transcode seek — see §6); the drift loop keeps it on target after load. |
| client paused while session active | client resumes on the next `play`/`seek-correction` (state is implied by the latest event). |

Rules:

- Heartbeat interval: `settings.sync_heartbeat_interval_ms` (default 5 s). Each heartbeat gets an immediate decision in-band (WS ack or `409` with target on the HTTP variant); worst-case correction latency is one interval.
- **One correction per client per heartbeat cycle.** No correction storms. Corrections are debounced against active seeks: if the client is mid-seek (it told us via a "seeking" heartbeat flag), the correction is deferred up to 500 ms.
- While `state == paused`, no corrections are emitted (target is frozen `PositionMs`).
- **Clock discipline:** client position is monotonic-derived; wall-clock jumps (NTP) never perturb position. Only server-sent `RefTs`/`ts` enter the target math. This is the standard media-clock pattern.
- **Fresh joiner grace:** the first two heartbeats of a client's participation get no correction (it is still loading/seeking to target); from the third, normal rules apply.

### 3.4 Late Join and Rejoin

1. Client authenticates the WebSocket and sends `subscribe`.
2. Under the hub subscription lock, the server registers that connection and starts buffering events, reads the committed snapshot `(version V, eventSeq S)`, sends `hello`, drains buffered events with `seq > S`, then switches to live delivery. This closes the snapshot-N/missed-N+1 race.
3. Client opens `/audio/{CurrentMedia}?offsetSeconds=<floor(T/1000)>` only when an initial transcoded offset is required; otherwise it loads the representation and seeks with the media element/Range behavior the upstream actually supports.
4. Client drops `seq <= S`, applies `seq > S` in strict order, and starts a fresh subscription cycle on any gap or buffer-overflow signal.
5. Normal heartbeat loop begins after the drain completes.

### 3.5 DJ Lifecycle (lease + grace, task E / ADR-005)

The DJ is identified by **identity + one server-bound connection + a rotated lease token**:

- **Acquire:** every WS connection has a server-issued `connectionId`. The authenticated admin asks to acquire; the executor increments the generation, creates a random opaque token, stores its hash with that connection ID, and returns the raw token once. While another connection is active, acquisition requires explicit takeover and fences the old connection.
- **Command validation:** every DJ command is checked against authenticated admin ID, session ID, current server-issued generation, active `connectionId`, and the lease-token hash. Old sockets and copied generations are insufficient. A mismatch returns `409 stale-lease` without mutation.
- **Disconnect:** DJ WS closes (or the DJ misses 3 lease heartbeats) → `DJConnected=false`; **freeze**: session → `paused`; compute `DJGraceDeadline = now + settings.dj_grace_period_s` and **persist it** (checkpoint transaction). WS broadcast `dj-disconnected {graceUntil}`.
- **Reconnect within grace:** the new authenticated connection acquires a newly issued lease, which immediately fences the old socket. Grace clears, but the shared session stays paused until `play` or `end` from the new owner.
- **Grace expiry:** the executor's deadline timer fires → `EndSession(reason: "grace-expiry")` → `session-ended {reason: grace-expiry}` broadcast → platform mode returns to `individual`, individual sessions restored.
- **Restart inside grace (V1, ADR-005):** the session ends immediately as `platform-restart`; no lease or connection survives process restart. The persisted deadline is retained only for diagnosis/audit.

### 3.6 Backend Restart — V1 policy: safe-end (ADR-005)

- **V1: active synchronized sessions end safely on backend restart.** On boot, the executor scans `shared_sessions` for `state IN ('active','paused')` rows; each is marked `ended` with `end_reason='platform-restart'` (if a `DJGraceDeadline` is still in the future, the end is recorded but the session was already ending anyway). `settings.mode` is set to `individual`. Clients, on reconnect, receive `session-ended {reason: platform-restart}` and fall back to their individual sessions.
- **Individual sessions are NOT affected** — they remain persisted in `session_state` and are restorable on any device (NFR-REL-001).
- The Postgres checkpoint is authoritative for *what had happened up to the crash*; boot recovery commits the terminal checkpoint and `session-ended` event transactionally. It is **not** a mechanism to resume the shared session in V1.
- No corrupt state is possible: the executor is the only writer, and its mutations are checkpoint-then-broadcast; a crash between them is indistinguishable from "the event never happened", and the snapshot heals clients.

## 4. Mode Transition State Machine (authoritative, testable)

```
                  ┌──────────────┐
    admin switches │              │  admin switches
──────────────────►│ individual   │──────────────────┐
                  │              │                  │
                  └──────┬───────┘                  │
                         │ admin starts sync        │
                         │ (POST /api/platform/mode)│
                         ▼                          ▼
                  ┌──────────────┐   admin ends   ┌──────────────┐
                  │ synchronized │───────────────►│ individual   │
                  │  (active)    │                │ (restored)   │
                  └──────┬───────┘                └──────────────┘
                         │ DJ disconnects
                         ▼
                  ┌──────────────┐  grace expires ┌──────────────┐
                  │ synchronized │───────────────►│ individual   │
                  │ (paused,     │  or DJ resumes │ (restored)  │
                  │ DJ-grace)    │                └──────────────┘
                  └──────────────┘
```

Transition table (single source of truth — implement as a table-driven test in `sync`, fake clock):

| # | From | Trigger | To | Side effects |
|---|---|---|---|---|
| T1 | individual | admin `POST /api/platform/mode {mode:"synchronized"}` | synchronized (active) | session created (seed = arg or admin's individual current); one composite `session-started` event commits checkpoint v0/seq1 with mode + full state; members pause individual playback in M4B |
| T2 | synchronized | admin `POST /api/platform/mode {mode:"individual"}` with active lease/version | individual | `session-ended {reason:admin}` (seq++), individual sessions remain preserved in PG, session row `ended` |
| T3 | synchronized | DJ WS close (or 3 missed lease heartbeats) | synchronized (DJ-grace, paused) | freeze; `dj-disconnected {graceUntil}` (seq++, version unchanged); `DJGraceDeadline` persisted; deadline timer started |
| T4 | T3 | DJ reconnects ≤ grace and receives a new connection-bound lease | synchronized (paused until command) | old socket fenced; explicit `play` re-baselines (`RefTs=now`, `PositionMs`=member position) |
| T5 | T3 | grace deadline passes | individual | forced `session-ended {reason:grace-expiry}`, same restoration behavior as T2 |
| T6 | any | platform/backend restart | individual | **V1: shared sessions end safely** (§3.6); individual sessions persist |

M4A implements T1–T6 persistence and trusted lease lifecycle calls. It does
not register a public lease-acquisition endpoint: M4B must pass the
authenticated server-issued WebSocket connection ID to `AcquireLease`, and
must call `DisconnectDJ` for that exact fenced lease when the connection is
lost. The persisted-deadline timer rechecks session ID and deadline before T5;
startup recovery applies T6 instead of resuming a pre-restart lease.

Client-side transition effects (all roles):

- T1: stop local transport, persist individual state, switch to shared (initial transcode seek may use integer `offsetSeconds`; otherwise use the media element's verified seek path).
- T2/T5/T6: restore individual state (queue + position); do **not** auto-play — the user starts.

## 5. Concurrency and Correctness Invariants

| Invariant | Enforced by |
|---|---|
| One writer for shared state | single executor goroutine (task E) |
| `Version` strictly increases on content mutations; `EventSeq` strictly increases on every authoritative event | executor loop (ADR-006) |
| Checkpoint commits **before** broadcast (crash ⇒ no phantom event) | executor order (step 3 before step 4), same transaction |
| Exactly one authoritative state event per `EventSeq` | ADR-006 invariant; `sync_events` UNIQUE(session_id, event_seq) |
| A client never applies an event older than its last snapshot | client-side `seq` compare (§3.4, ADR-006) |
| A gap in `seq` on a live connection triggers a re-snapshot | client rule (§9.4 API) |
| DJ identity + active connection + rotated token; no zombie takeover | executor fencing (§3.5) |
| Restart cannot resurrect a shared session (V1) | boot-time safe-end scan (§3.6, ADR-005) |
| Audio is per-user and credential-isolated | streaming gateway (ADR-001); clients never hold Navidrome credentials |
| Individual session writes are CAS-revisioned | `session_state.revision` (task F) |

## 6. Per-Client Audio (via the gateway, ADR-001)

When a client needs the shared current track:

```
GET /audio/{mediaId}?offsetSeconds=<target>   (Flow session cookie; initial transcode offset only)
  → Flow gateway: authn/z (session + status + provisioning) → register cancel handle
     → authoritative account recheck → resolve media → open Navidrome /rest/stream
     with the member-scoped server-side credential
  → pipe bytes client↔Flow↔Navidrome
```

Registration happens before any Navidrome lookup or stream open, so suspension either rejects the registration or cancels the registered context. No Navidrome address, credential, or JWT is ever visible to the client. Original requests explicitly use upstream `format=raw` and preserve Range behavior when available. Non-seekable transcodes may use the verified integer `timeOffset` only for initial load; Flow never translates a time offset into a fabricated byte range.

## 7. Failure Modes and Behaviors (client UX contract)

| Failure | Client behavior |
|---|---|
| WS dropped, < 3 s | silent reconnect (exponential backoff 500 ms → 8 s cap) |
| WS dropped, > 3 s, individual mode | banner "connection lost" — local audio continues (audio is direct from the gateway, not the WS; only control is down) |
| WS dropped, sync mode | freeze + banner; on reconnect: snapshot → representation-appropriate seek or integer `offsetSeconds` for a fresh transcode, then seek-correct |
| Navidrome/gateway stream error (410/404/503) | toast "track unavailable"; in sync mode the client waits for the DJ's next event (no member-side auto-skip in sync) |
| `seek-correction` arrives during a buffer underrun or active seek | defer correction ≤ 500 ms (debounce), then apply |
| `session-ended` (any reason) | return to individual UI; restore individual session; show session recap (V1.x) |
| `dj-disconnected` | pause UI, "DJ disconnected — resuming shortly" + countdown to `graceUntil` |
| Drift indicator | thin "±ms" readout near the play button, updated per heartbeat; amber > 1 s, red > 2 s |
| `409 stale-lease` to a DJ client | the DJ seat was taken by a newer connection; this client is demoted to a member listener |

## 8. Tuning Parameters (target runtime settings, ARCHITECTURE §8)

| Key (settings) | Default | Notes |
|---|---|---|
| `sync_drift_threshold_ms` | 2000 | correction trigger |
| `sync_heartbeat_interval_ms` | 5000 | client interval (server advertises it in `hello`) |
| `dj_grace_period_s` | 90 | T3 → T5 window |
| `max_concurrent_streams_per_user` | 100 | fixed in M2; over-cap stream starts return 429 before upstream I/O; runtime management is introduced in M5 |
| `catchUpRateMin/Max` | 1.0 / 1.25 | client-side only |

## 9. What is deliberately NOT in the engine

- No shared chat/reactions (V2 social).
- No per-member volume in the shared session (volume stays personal — "listen together" ≠ "same volume").
- No skip-queue voting (DJ-only by design).
- No HLS/segmented sync (HTTP streaming + seek is sufficient at ±2 s; HLS adds a second timing domain for no V1 benefit).
- No shared-session auto-resume across backend restarts in V1 (ADR-005; it's M7 in V1.x).

## 10. Navidrome-side facts the engine depends on (verified in source)

- At source tag `v0.64.2`, `timeOffset`/`format`/`maxBitRate` are handled by `server/subsonic/stream.go`; seekable streams use `http.ServeContent`, while non-seekable transcodes set `Accept-Ranges: none` (`core/stream/media_streamer.go`). The gateway therefore forwards actual upstream Range behavior and uses integer `timeOffset` only for an initial transcoded offset.
- A partial upstream write that fails aborts via `http.ErrAbortHandler` (verified) — the gateway's context-cancel on client abort matches this.
- Navidrome's `jukebox`/`core/playback` is a **hardware device controller** (mpv-backed) — confirmed no peer-listener concept. Not reused; the sync engine above is Flow's own.
- Navidrome's event broker (`/api/events`, SSE, per-user, requires `DevActivityPanel`) is **not** used; Flow owns its own WS hub.

## 11. Testing Plan (engine)

M3 executable evidence is in `internal/playback/*_test.go`,
`internal/api/playback_test.go`, and `web/tests/player.test.mjs` /
`api.test.mjs`. The 33 frontend tests cover restore/transport/queue, seek/pause,
checkpoint capture and conflicts, pending-command end races, stale events,
buffering/errors/autoplay, retry, account invalidation and cleanup.

`TestIndividualPlaybackPostgres` exercises real conditional writes, account/
session rechecks, migration/default-row behavior, a checkpoint/track race and
new-pool restore. `TestM3LivePlayback` combines real auth, PostgreSQL, pinned
Navidrome, the Flow gateway, compiled React UI/Chromium and a real Flow process
restart. These live gates are conditional on disposable services and remain
unexecuted locally in the restricted environment. The single-user SQL smoke is
separate evidence, with no concurrency or browser claim. M3_REPORT records results.

The following synchronized-engine plan belongs to M4 and is not M3 coverage:

Unit (pure Go, fake clock):
- Transition table (§4) as table-driven tests, including T3/T4/T5 timing and the restart safe-end (T6).
- Drift decision as pure functions `(report{positionMs, bufferMs, seeking}, session, now) → decision` (all branches of §3.3).
- Version/event-seq logic: exactly-one-event-per-seq, gap detection, stale rejection.
- Individual-session revision CAS and explicit 409 reconciliation (no automatic merge).

Integration (disposable PostgreSQL supplied by CI or `FLOW_TEST_DATABASE_URL`):
- Checkpoint-then-broadcast ordering (crash between 3 and 4 ⇒ snapshot heal; no phantom).
- DJ lease generation (zombie rejection), grace-deadline persistence + restart behavior (safe-end).
- Concurrent heartbeats from N fake clients; assert no event emitted for a wrong/out-of-order seq.

End-to-end (Playwright, two+ clients, real backend, pinned Navidrome):
- Full DJ session: start → 5 members join staggered → seek → next → DJ disconnect → grace → fallback. Assert drift < threshold post-correction; assert safe-end after a forced backend restart.
