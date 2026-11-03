# Flow — Playback System

Version: 1.0.0-draft
Covers: individual sessions, the synchronized engine, drift model, reconnection, mode transitions, failure modes.
This document is the contract between the `sync` package, the WS layer, and the clients.

---

## 1. Ground Rules

1. **No audio relay.** The Flow server never carries audio. Every client opens its own stream to Navidrome for the current shared track. The WebSocket carries control + state only.
2. **Server authority.** The shared session object lives in the Flow backend (in-memory, checkpointed to Postgres). Clients propose nothing; they report.
3. **Per-user credentials.** Each client's stream URL is minted by Flow for *that user's* Navidrome identity (see NAVIDROME_INTEGRATION §3).

## 2. Individual Mode (baseline)

State per user (Postgres `session_state`, hot-cached in memory):

```go
type IndividualSession struct {
    Queue        []QueueItem // {MediaID, AddedAt}
    QueueIndex   int
    CurrentMedia string
    PositionMs   int64
    Volume       float32
    Shuffle      bool
}
```

- Client owns transport locally (play/pause/seek are client-side `<audio>` controls; server only persists state for resume).
- Persistence cadence: on every meaningful change, capped at one write per 10 s (write-behind); flush on page hide (`visibilitychange`) and unload.
- Resume: on login, `GET /playback/session` → client restores queue + seeks to `PositionMs` (±5 s grace: if more than 5 s old, start at track beginning — configured).

## 3. The Synchronized Engine

### 3.1 State

```go
type SharedSession struct {
    ID          int64
    Version     int64     // every admin mutation bumps it
    State       SessionState // active | paused | ended
    DJUserID    int64
    CurrentMedia string
    PositionMs  int64     // position measured at RefTs
    RefTs       time.Time // server wall-clock
    Queue       []string  // media ids, CurrentMedia excluded (it's queue[QueueIndex])
    QueueIndex  int
    DJConnected bool      // WS state of the DJ
    DJGraceEnd  time.Time // set when DJ disconnects
}
```

Time model: "now" for the shared timeline = `PositionMs + (nowServer - RefTs)` while `State == active`; frozen while `paused`. All clients compute their target from **server-sent** timestamps (`RefTs`, `sync-*` event `ts`), never from their own clock.

### 3.2 Admin Mutation Protocol

Every admin control action follows one path:

```
admin client ──HTTP POST /sync/session/{action}──► Flow
    Flow (sync engine, single-goroutine executor):
      1. validate state (409 if version mismatch, 410 if ended)
      2. mutate state; Version++
      3. Postgres checkpoint (INSERT/UPDATE shared_sessions + sync_events row)
      4. WS broadcast event to all participating members (and DJ's other devices)
```

The single-goroutine executor (a channel-fed loop) is the reason no locking gymnastics are needed: one event at a time, total order guaranteed, and `Version` is that total order made visible to clients.

### 3.3 Drift and Correction

Definitions (per member `m` at server time `t`):

- Expected: `E_m(t) = PositionMs + (t - RefTs)` (if playing; else PositionMs)
- Reported: `R_m(t) = positionMs from last heartbeat, adjusted by client's monotonic timer`
- Drift: `D_m = R_m - E_m(t)` (negative = behind, positive = ahead)

Corrections:

| Condition | Action |
|---|---|
| `\|D_m\| ≤ threshold` (default 2000 ms) | nothing |
| `D_m < -threshold` (behind) | send `seek-correction {target: E(t)}`; client seeks if the gap is > 2 s, otherwise catches up with 1.05–1.25× rate (client-side decision, no extra event) |
| `D_m > +threshold` (ahead) | send `seek-correction {target: E(t)}` immediately (seek backward) |
| track mismatch (`m.currentMedia != session.CurrentMedia`) | send `sync-queue`/`sync-next`-equivalent full-state frame; client loads the correct track at `E(t)` |
| client was paused when session is playing | send `seek-correction` + `play` implied by state (client resumes on correction) |

Rules:

- Heartbeat interval: 5 s (configurable). Each heartbeat gets an immediate 204/WS-ack **with the current decision** (no correction needed → empty ack), so the worst-case correction latency is one interval.
- A single correction per client per heartbeat cycle. No correction storms.
- While `state == paused`, no corrections are emitted (target is frozen `PositionMs`).
- Clock handling: clients must use their **monotonic** clock (performance.now / SystemClock.uptimeMs) for position deltas; wall-clock jumps (NTP) must not perturb position. Only the *server* clock is used for `RefTs` arithmetic. This is the standard "media clock" pattern.
- Buffering: `bufferMs` in the heartbeat lets the coordinator distinguish "client is buffered ahead by design" from "drifting ahead" — a fresh joiner's first heartbeat gets a lenient window (no correction on the first two heartbeats).

### 3.4 Late Join and Rejoin

1. Client (re)connects → `hello` → client GETs `/sync/state` (or WS `snapshot-request`).
2. Response: full `SharedSession` at version V, `refTs`, queue, **target position computed now**: `T = E(now)`.
3. Client: load `CurrentMedia`, seek to `T` (±3 s: fine; >3 s: seek + optionally rate-catch-up), start playing if `state == active`.
4. Client discards all locally queued WS events with `version ≤ V` (stale-event rule, API_SPEC §8.4).
5. From there, normal heartbeat loop.

### 3.5 DJ Disconnect (admin leaves)

- DJ's WS closes (or 3 missed heartbeats if DJ's client stopped heartbeating).
- `DJConnected = false`; WS broadcast `dj-disconnected {graceUntil: now + 90s}`.
- Session state becomes `paused` (members freeze at current position).
- At `graceUntil`: auto `EndSession(reason: "dj-disconnect")` → platform mode returns to `individual` (the fallback rule) → WS `session-ended`.
- DJ reconnects within grace: `DJConnected = true`; DJ client chooses: resume (re-sends `play` → normal mutation) or end. Grace cleared.

### 3.6 Backend Restart

- Postgres checkpoint of the last admin mutation is the recovery point.
- On boot: if a `shared_sessions` row is `active`/`paused` and `refTs` is older than 10 min → mark `ended (reason platform-restart)` and set mode `individual`. Otherwise offer resume: since `refTs` is wall-clock, the shared timeline *kept moving* during the outage; members who reconnect see the session at `E(now)` and catch up by seeking — the protocol already handles this. (If the DJ was also offline, the 90 s rule has already fired server-side before restart; in practice restart → ended.)
- V1.x upgrade: auto-resume with an admin prompt; V1 default: safe end.

## 4. Mode Transition State Machine (authoritative, testable)

```
                 ┌──────────────┐
   admin switches │              │  admin switches
─────────────────►│ individual   │──────────────────┐
                 │              │                  │
                 └──────┬───────┘                  │
                        │ admin starts sync        │
                        │ (POST /sync/session)      │
                        ▼                          ▼
                 ┌──────────────┐   admin ends   ┌──────────────┐
                 │ synchronized │───────────────►│ individual   │
                 │  (active)    │                │ (restored)   │
                 └──────┬───────┘                └──────────────┘
                        │ DJ disconnects
                        ▼
                 ┌──────────────┐  grace expires ┌──────────────┐
                 │ synchronized │───────────────►│ individual   │
                 │  (paused,    │  or DJ resumes │  (restored) │
                 │  DJ-grace)   │                └──────────────┘
                 └──────────────┘
```

Transition table (single source of truth — implement as a table-driven test in `sync`):

| # | From | Trigger | To | Side effects |
|---|---|---|---|---|
| T1 | individual | admin `POST /sync/session` | synchronized (active) | session created (seed = arg or admin's individual current), checkpoint v0, broadcast `mode-changed` + `sync-*` initial, members pause individual playback |
| T2 | synchronized | admin `POST /sync/session/end` | individual | `session-ended` broadcast, individual sessions restored from PG, session row `ended` |
| T3 | synchronized | DJ WS close | synchronized (DJ-grace, paused) | `dj-disconnected` broadcast; timer started |
| T4 | T3 | DJ reconnect ≤ grace | synchronized (active) | DJ resumes: explicit `play` (re-baseline `RefTs` = now, `PositionMs` = where members sit) |
| T5 | T3 | grace expires | individual | forced `session-ended (reason dj-disconnect)`, same as T2 |
| T6 | any | platform restart | per §3.6 | |

Client-side transition effects (all roles):

- T1: stop local transport, remember individual state (auto-saved), switch to shared transport.
- T2/T5: restore individual state (queue + position) and resume at 0 (do not auto-play; user starts).

## 5. Concurrency and Correctness Invariants

| Invariant | Enforced by |
|---|---|
| One writer for shared state | single executor goroutine |
| `Version` strictly increases per mutation | executor loop |
| Checkpoint before broadcast (crash ⇒ no phantom event) | executor order (3 before 4) |
| A client never applies an event older than its last snapshot | client-side version compare (documented, spec'd) |
| Two members never receive *different* queues for the same version | broadcast is atomic per event (fan-out channel); events are total-ordered |
| Stream URLs are per-user | minted in the auth middleware layer, see §6 |

## 6. Per-Client Stream Minting

When a client requests the shared current track:

```
GET /streams/{mediaId}   (member's Flow JWT)
  → Flow uses cached Navidrome JWT for that member
  → returns https://navidrome-internal/stream?u=mem&jwt=…&format=<negotiated>&c=flow&i=<mediaId>
```

The Navidrome JWT is **short-lived** (Navidrome's own JWT TTL; refreshed by Flow transparently when it expires — the subsonic `jwt` credential is supported, verified in `server/subsonic/middlewares.go`). Clients never see usernames, passwords, salts.

## 7. Failure Modes and Behaviors (client UX contract)

| Failure | Client behavior |
|---|---|
| WS dropped, < 3 s | silent reconnect (exponential backoff 500 ms → 8 s cap) |
| WS dropped, > 3 s, individual mode | banner "connection lost" — local playback continues (audio is direct from Navidrome; only control is down) |
| WS dropped, sync mode | freeze + banner; on reconnect: snapshot-request → seek-correct |
| Navidrome stream error (410/404) | toast "track unavailable", auto-advance in sync mode only if DJ plays next (no member-side auto-skip in sync) |
| `seek-correction` arrives while buffer underrun | defer correction until playable (debounce 500 ms) |
| `session-ended` | return to individual UI; show session recap (V1.x) |
| Drift indicator | thin "±ms" readout near the play button, updated per heartbeat; amber when > 1 s, red when > 2 s |

## 8. Tuning Parameters (settings, live-updatable)

| Key | Default | Notes |
|---|---|---|
| `syncDriftThresholdMs` | 2000 | correction trigger |
| `syncHeartbeatIntervalMs` | 5000 | client interval (server sends in `hello`) |
| `djGracePeriodS` | 90 | T3 → T5 window |
| `maxSyncMembers` | 50 | 429/`sync-full` beyond (V1: soft cap with warning, not hard reject — document as soft) |
| `catchUpRateMin/Max` | 1.0 / 1.25 | client-side only |

## 9. What is deliberately NOT in the engine

- No shared chat/reactions (V2 social).
- No per-member volume in the shared session (volume stays personal — "listen together" ≠ "same volume").
- No skip-queue voting (admin DJ only, by design).
- No HLS/segmented sync (HTTP streaming + seek is sufficient at ±2 s; HLS adds a second timing domain for no V1 benefit).

## 10. Navidrome-side facts the engine depends on (verified in source)

- Stream URL `timeOffset` is honored by the Subsonic `stream` handler (`server/subsonic/stream.go` reads `timeOffset` into the stream request) — so **late joins can also start with `?timeOffset=` on the stream URL itself**, not only client-side seek. The engine prefers client-side seek (smoother, no reload) and uses `timeOffset` in the stream URL as the *initial* load position for fresh joins.
- Navidrome's own `jukebox`/`core/playback` is a **hardware device controller** (mpv-backed audio devices) — confirmed it has no peer-listener concept. It is not reused; the sync engine above is Flow's own.
- Navidrome's event broker (`/api/events`, SSE, per-user) is **not** used; Flow owns its own WS hub and needs no SSE fan-in.

## 11. Testing Plan (engine)

Unit (pure Go, no I/O):
- Transition table (§4) as table-driven tests, including T3/T4/T5 timing with a fake clock.
- Drift decisions (§3.3) as pure functions `(report, session, now) → decision`.
- Version/snapshot staleness logic.

Integration (testcontainers Postgres):
- Checkpoint/restart recovery.
- Concurrent heartbeats from N fake clients; assert no event is emitted for the wrong version.

End-to-end (scripts, two fake "clients" + real backend + fake Navidrome):
- Full DJ session: start → 5 members join at staggered times → seek → next → DJ disconnect → grace → fallback. Assert drift < threshold post-correction.
