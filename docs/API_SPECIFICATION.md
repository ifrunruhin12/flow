# Flow — API Specification

Version: 1.1.0-draft (supersedes 1.0.0-draft)
Base path: `https://<domain>/api` (REST) and `wss://<domain>/ws` (WebSocket); audio at `https://<domain>/audio/…` (ADR-001).

**Authentication (ADR-002, browser V1):** an opaque server-side session in an HttpOnly, Secure, `SameSite=Lax` cookie (`flow_session`). The cookie holds a random token whose SHA-256 hash is stored in Postgres. **No signed JWTs, no tokens in URLs, no WebSocket query-param tokens.** WebSocket upgrades authenticate with the same session cookie, validated server-side, plus an **Origin header check** (must match the configured domain). CSRF: state-changing REST calls require `SameSite=Lax` cookie + `Origin`/`Referer` validation for cookie-authenticated mutations (V1 same-origin SPA ⇒ cross-site POSTs are blocked by SameSite; the Origin check is defense-in-depth). A separately defined **bearer-session mechanism for native clients** (Android/desktop) is reserved, not implemented in V1.

Production requires an HTTPS `FLOW_PUBLIC_ORIGIN`, so `Secure` is always set there. The loopback-only HTTP development origin is the explicit exception: the development cookie omits `Secure` because browsers otherwise refuse to send it over `http://127.0.0.1`.

All responses are JSON unless noted. Auth: the `flow_session` cookie (browser). Native clients: bearer session token (V1.x).

---

## 0. Conventions

- Errors: `{"error": {"code": "…", "message": "…", "detail": "…"}}` with standard HTTP status.
- Pagination: `?cursor=<opaque>&limit=50`; responses include `nextCursor` (null when exhausted). Cursor is opaque; do not parse.
- Media IDs: Navidrome `media_file.id` (string). Pass through, never re-encode.
- All timestamps: RFC 3339 UTC.
- Every authenticated endpoint checks Flow session + current user status + role server-side; `401` = no valid session, `403` = an otherwise-valid session resolved to the wrong role/status. Suspension/deletion also revokes session rows, so replaying a previously issued cookie after that commit normally returns `401`; a request that races the lifecycle transition may instead be stopped as `403 account_not_active`.
- `X-Correlation-Id` header is returned on every response and stored in `audit_log.correlation_id` for admin actions.

### Error codes (stable, used by clients)

| code | HTTP | meaning |
|---|---|---|
| `unauthenticated` | 401 | missing/invalid/expired Flow session |
| `forbidden` | 403 | wrong role (admin-only route hit by a member) |
| `account_not_active` | 403 | session valid but user status ≠ active (suspended/deleted) or `provisioning_state ≠ complete` |
| `password_change_required` | 403 | restricted first-login session attempted a route other than me/change-password/logout |
| `conflict` | 409 | username/email taken, request already decided, **session revision mismatch** (playback concurrency, task F) |
| `rate_limited` | 429 | |
| `navidrome_unavailable` | 503 | Navidrome health probe failing; library/audio endpoints retry-safe |
| `provisioning_failed` | 503 | Flow account exists but durable Navidrome provisioning is in `failed` and requires admin retry |
| `bad_gateway` | 502 | Navidrome returned a malformed or otherwise unusable response; the upstream body is never relayed |

## 1. Auth (Flow)

| Method | Path | Body | Response | Notes |
|---|---|---|---|---|
| POST | `/auth/login` | `{username, password}` | `200 {user:{id, username, role, status, provisioningState}}`; sets `flow_session` cookie (HttpOnly, Secure, SameSite=Lax, Path=/) | Rate-limited (per IP + per username). Rejects `account_not_active`. |
| POST | `/auth/logout` | – | `204` | M1 deletes the session row. M2, when streams first exist, also synchronously flushes handles registered to that session before returning. |
| GET | `/auth/me` | – | M1: `{user}`. Later owning milestones add `settings:{themeId, preferredStreamFormat}`, `mode`, and `activeSharedSessionId?` additively. | The single boot-call the SPA makes; no future-feature placeholder values are emitted. |
| POST | `/auth/change-password` | `{currentPassword, newPassword}` | `204`; clears `mustChangePassword`, retains the current session, and revokes the member's other sessions. | Member route, including a restricted first-login session. New password minimum 12 characters, maximum 128. The single Flow admin password is operator-managed through `FLOW_ADMIN_PASSWORD`; changing that deployment secret and restarting rotates it and revokes admin sessions. |
| POST | `/auth/register-request` | `{displayName, email, desiredUsername}` | `202 {requestId}` | Anonymous. One open pending request per email (DB partial unique index). |
| POST | `/auth/native/login` | (V1.x) | bearer token | Not routed in V1. Native-client sessions (Android/desktop) are introduced with FR-AUTH-008. |

M1 identity validation: usernames are 3–32 characters and match `^[A-Za-z0-9][A-Za-z0-9._-]{2,31}$`; display names are 1–100 Unicode characters; request emails must be syntactically valid and at most 254 characters. Direct member creation may omit email. Passwords are 12–128 Unicode characters.

### CSRF model (browser)

- The API is cookie-authenticated. `SameSite=Lax` already blocks cross-site cookie submission for all methods except top-level GET navigation (which is safe/read-only by design).
- Defense-in-depth: the backend validates the `Origin` (or `Referer`) header on cookie-authenticated `POST/PUT/PATCH/DELETE`; mismatch ⇒ 403. The SPA always sends `Origin`; native (future) bearer flows are not subject to this (bearer + explicit headers, no cookie).
- There is no double-submit token, no `X-CSRF-Token` header in V1 — SameSite + Origin check is the documented, testable control (SECURITY §5).

## 2. Library (member+admin read; proxied to Navidrome Subsonic)

| Method | Path | Query | Response shape |
|---|---|---|---|
| GET | `/library/albums` | `cursor, limit` | `{items:[{id, title, artist, albumArtist, coverUrl, songCount, durationMs, year}], nextCursor}` |
| GET | `/library/albums/{albumId}` | – | album detail + ordered track listing |
| GET | `/library/artists` | `cursor, limit, query?` | `{items:[{id, name, albumCount, coverUrl}]}` |
| GET | `/library/artists/{artistId}` | – | discography + top tracks |
| GET | `/library/songs` | `cursor, limit, albumId?` | flat song list |
| GET | `/library/songs/{songId}` | – | `{id,title,artist,artistId?,album,albumId?,durationMs,trackNumber?,discNumber?,year?,coverUrl?,contentType?,suffix?,starred}` |
| GET | `/library/search` | `q, limit, albumCount?, artistCount?, songCount?` | `{albums:[], artists:[], songs:[]}` |
| GET | `/library/genres` | – | V1.x (`FR-LIB-006`); no V1 route |
| POST | `/library/stars` | – | `{mediaId, kind:"song"\|"album"}` → star (Navidrome `Star`, as this user) |
| DELETE | `/library/stars/{kind}/{mediaId}` | – | unstar |
| GET | `/library/stars` | `kind` | starred items (per user) |
| GET | `/library/scanner/status` | – | scan progress |

Artwork: served **through Flow** at `/audio/coverart/{id}` (proxied from Navidrome `getCoverArt`) — consistent with ADR-001 (no Navidrome address or credential on the wire). The path accepts only validated Navidrome cover-art identifiers, never a URL or filesystem path. Responses set `Cache-Control: private, max-age=3600`.

M2 response ownership: Flow maps every Subsonic payload into the models above. Missing descriptive metadata becomes an empty string/omitted optional field and negative numeric metadata is clamped to zero; an invalid envelope or media object without its stable ID is `502 bad_gateway`. Pagination and search routes reject unknown or duplicate query keys rather than forwarding them. Flow stores no album, artist, song, artwork, star, or playlist index rows.

### 2.1 Streaming Gateway (ADR-001 — audio)

| Method | Path | Query | Notes |
|---|---|---|---|
| GET | `/audio/{mediaId}` | `format? (original\|opus\|mp3\|auto)`, `maxBitRate?`, `offsetSeconds?`, plus standard `Range` | Audio proxy |

- **All audio and cover-art in V1 flows through this endpoint.** Clients never see Navidrome URLs, credentials, JWTs, or internal addresses.
- Flow authorizes (session + status + role + per-user stream allowance), then opens an upstream `Range`-aware connection to Navidrome `/rest/stream` with the appropriate **server-side** credential (member-scoped for member streams, admin-scoped only for the admin's own streams), attaches it after authn/z, and pipes bytes both ways with `io.Copy` (no whole-file buffering).
- **Range/seeking is representation-dependent:** `Range` is forwarded. Flow passes through the upstream status and safe representation headers; it does not synthesize `206` or `Accept-Ranges: bytes`. Seekable originals may support Range; a non-seekable transcode may advertise `Accept-Ranges: none`. `offsetSeconds` maps to Navidrome's integer `timeOffset` for an initial transcoded seek.
- **Cancellation:** client abort closes the upstream via context cancel; Navidrome's side sees the connection drop (its `Serve` aborts via `http.ErrAbortHandler` for partial writes — verified behavior in `core/stream/media_streamer.go`).
- **Transcoding params:** `original` explicitly sends Navidrome `format=raw`, which the pinned `v0.64.2` source defines as original-file passthrough; omitting `format` is not used because it may select a player preference. `opus`/`mp3` request the verified transcoders. In M2, `format=auto` is the safe MP3 fallback; the web client normally sends `original`, `opus`, or `mp3` after `HTMLMediaElement.canPlayType` capability checks. Persisted user format preference remains M13. The gateway never treats `flac` as a synonym for "original."
- **Validation:** Flow checks the session and current account state before returning media-parameter validation errors. `maxBitRate` is an integer from 32 through 320 and applies only to a transcode; `offsetSeconds` is an integer from 0 through 86400 and applies only to a transcode. Empty values, unknown/duplicate query keys, and unsupported formats are rejected. `Range` is limited to one valid byte range (bounded header length, positive suffix, and non-decreasing explicit bounds). A browser may send a Range header with a transcoded offset request, but the two controls are not interchangeable: Flow forwards both and preserves Navidrome's actual `200`/`Accept-Ranges: none` response. Unknown query keys include `download`.
- **Original seek behavior:** original/raw requests may return `200` or `206`; Flow preserves `Content-Range` and the upstream `Accept-Ranges`. Transcoded requests return the pinned upstream behavior (`200`, `Accept-Ranges: none`) and an initial seek uses integer `timeOffset=offsetSeconds`.
- **Suspended members:** on suspension, the gateway's active-stream registry aborts all in-flight streams for that user *immediately* (in addition to rejecting new requests). See ADR-001 §2.4.
- **No `download` variant exists in V1.0.0.** (Navidrome's `enableDownloads` defaults to **true** — the compose file sets `ND_ENABLEDOWNLOADS=false` explicitly; deployment invariant, SECURITY §4. Download capability is a Flow-gated admin flag, off by default, implemented in V1.x behind this same gateway.)

## 3. Playlists

| Method | Path | Body | Notes |
|---|---|---|---|
| GET | `/playlists` | – | All members. `{items:[{id, name, songCount}]}` |
| GET | `/playlists/{id}` | – | Track listing (members too) |
| POST | `/playlists` (admin) | `{name, comment?, songIds?:[]}` | |
| PUT | `/playlists/{id}` (admin) | `{name?, comment?, songIds?, removeIndices?}` | |
| DELETE | `/playlists/{id}` (admin) | – | |

Streaming a playlist = streaming its tracks via `/audio/{mediaId}` in V1 (V1.x: playlist-native streams).

Playlist detail is `{id,name,comment?,songCount,durationMs,coverUrl?,tracks:[song…]}`. Flow creates administrator playlists as public in Navidrome so members can read them, while Flow rejects member `POST`/`PUT`/`DELETE` requests before reading or forwarding a mutation.

## 4. Playback — Individual (task F: one session, revision CAS)

| Method | Path | Body | Notes |
|---|---|---|---|
| GET | `/playback/session` | – | `{queue, queueIndex, currentMediaId, positionMs, volume, shuffle, revision}` |
| PUT | `/playback/session` | `{queue?, queueIndex?, currentMediaId?, positionMs?, volume?, shuffle?, status?, queueEdit?, command?, ifRevision:<n>}` | Atomic CAS; returns the complete session at the new revision. |
| DELETE | `/playback/session` | `{ifRevision:<n>}` | CAS clears queue/current/position and pauses; retains volume/shuffle, row, and monotonic revision. Returns the complete session. |

### 4.1 Session and mutations (M3, ADR-007)

All paths above are relative to `/api`. GET also retrieves the queue; separate
control routes are unnecessary. Responses add `status:"paused"|"playing"` and
`updatedAt` (RFC3339 UTC) to the baseline shape. `status` is saved transport
intent, not proof that a browser is producing audio. Default: `queue:[]`,
`queueIndex:0`, `currentMediaId:null`, `positionMs:0`, `volume:1`, `shuffle:false`,
`status:"paused"`, `revision:0`. The session is created with the Flow account.

`queue` is a canonical ordered array of `{mediaId, addedAt}`. Replacement requests
may omit `addedAt` for new occurrences; the server stamps new entries and
preserves timestamps only for matching existing occurrences. At most 200 queue
entries. Duplicate media IDs are allowed as distinct occurrences. Unknown body
fields, owner IDs, query keys, missing/null/negative revisions, nonintegral
indices/positions, and invalid values return `400 invalid_request`. `ifRevision`
is a JSON safe integer. No route accepts a target user ID.

Examples using the **same PUT endpoint**, all with `ifRevision`:

| Interaction | Additional fields |
|---|---|
| Start album/listing | `queue:[{mediaId},…], queueIndex:<selected>, positionMs:0, status:"playing"` |
| Select queued track | `queueIndex:<index>, positionMs:0, status:"playing"` |
| Play / pause | `status:"playing"` / `status:"paused"`, plus sampled `positionMs` |
| Seek | `positionMs:<target>, currentMediaId:<sampled>, queueIndex:<sampled>` |
| Volume / shuffle | `volume:<0…1>` / `shuffle:<boolean>` |
| Add items | `queueEdit:{add:[mediaId,…]}` |
| Remove occurrences | `queueEdit:{remove:[indices…]}` |
| Reorder | `queueEdit:{move:{from,to}}` (destination is the final canonical index) |
| Next / previous | `command:"next"` / `command:"previous"`, optional sampled `positionMs` |
| Natural end | `command:"ended", currentMediaId:<sampled>, queueIndex:<sampled>, positionMs:<sampled>` |
| Periodic checkpoint | `command:"checkpoint", currentMediaId:<sampled>, queueIndex:<sampled>, positionMs:<sampled>` |

`queueEdit` contains exactly one nonempty operation and cannot accompany full
queue/selection fields or `command`. `command` cannot accompany queue edits,
replacement, status, shuffle, or volume. End/checkpoint require all three sampled
identity/position fields. Next/previous may include position but cannot select a
different track. Position is bounded to 0…86,400,000 ms and the server's known
Navidrome duration (1-second tolerance for rounded metadata); unknown duration
uses the absolute bound. Volume is rounded to the schema's 0.001 precision.
New/replacement items and selections use this user's library access; missing
media ⇒ `404 not_found`, denied media ⇒ `403 forbidden`, upstream outage ⇒ 503.
Retained missing items are not silently dropped and can still be removed/moved.
Removal can select a retained unavailable successor/predecessor; that does not
block deleting the requested occurrence. The browser surfaces the unavailable
destination and persists paused intent. Explicit selection remains validated.
Pause of the unchanged retained occurrence accepts a bounded checkpoint when
metadata is missing/denied/temporarily unavailable; when available, authoritative
duration validation still applies. Library failure cannot prevent stopping audio.

### 4.2 Concurrency and persistence

Every accepted write commits immediately and advances revision, including clear
and harmless boundary commands. Stale revision or mismatched sampled checkpoint/
end identity ⇒ HTTP 409:

```json
{"error":{"code":"conflict","message":"Playback changed. Reload the current session."},"currentRevision":8,"currentSession":{"queue":[],"queueIndex":0,"currentMediaId":null,"positionMs":0,"volume":1,"shuffle":false,"status":"paused","revision":8,"updatedAt":"2026-11-06T00:00:00Z"}}
```

The client adopts `currentSession`, pauses local transport, and shows the
conflict. It never merges or retries a stale checkpoint/action automatically.
An explicit subsequent user action uses the reconciled revision. Account/session
authorization is checked again in the commit transaction. All reads/writes use
the authenticated principal, including for the administrator's own session.
Natural-end dispatch waits for earlier local commands and verifies the captured
occurrence before sampling its current revision/index. This does not retry a
rejected write. Periodic checkpoints always retain their original sampled guards.

Ordinary playing-position checkpoints occur every 10 seconds; pause, seek,
transition/end, and other commands persist immediately. Browser unload is not
used. GET refresh occurs every 15 seconds and on focus/visibility/online. Queue
boundaries, stable shuffle traversal, unavailable tracks, and resume behavior are
defined in PLAYBACK_SYSTEM §2. Multiple devices may produce independent audio;
M3 coordinates durable state only, with no remote playback/event protocol.

M3 service/REST/controller coverage and the unexecuted live completion gates are
recorded in M3_REPORT. `TestM3LivePlayback` is the executable real auth/PostgreSQL/
Navidrome/gateway/Chromium/process-restart check; compilation or skipping does not
establish that contract at runtime.

## 5. Synchronized Mode (shared session)

Read endpoints (authenticated, both roles):

| Method | Path | Response |
|---|---|---|
| GET | `/sync/state` | `{mode, session:{id, version, eventSeq, state, currentMediaId, positionMs, refTs, queue, queueIndex, djLeaseGeneration, djGraceDeadline?}, you:{joined, driftMs?, lastCorrection?}}` (503 if mode is `individual` with no active session) |
| GET | `/sync/participants` (admin only) | `[{userId, username, joinedAt, lastHeartbeat, estimatedDriftMs}]` |

Write endpoints (**admin only**; members get 403; in an active session, only the **DJ** (the admin who started it — V1 is single-admin anyway) may mutate):

Except for initial session creation and lease acquisition, every DJ mutation must include `X-Flow-DJ-Generation`, `X-Flow-DJ-Connection`, and `X-Flow-DJ-Lease`, plus the current content `ifVersion` in its body. The executor validates authenticated user, session, server-issued generation, bound connection, constant-time token hash, and content version immediately before applying the command.

| Method | Path | Body | Effect (server-side, single-writer executor) |
|---|---|---|---|
| POST | `/sync/session` | `{seedMediaId?, seedPositionMs?}` | Create shared session (seeds from admin's individual state or explicit args) at `version=0,event_seq=1`; commit one composite `session-started` checkpoint/event, then broadcast. |
| POST | `/sync/session/play` / `pause` | `{ifVersion}` | State mutation; `version++`, `event_seq++`; checkpoint; broadcast |
| POST | `/sync/session/seek` | `{positionMs, ifVersion}` | Same; also emits immediate per-client seek corrections |
| POST | `/sync/session/next` / `prev` | `{ifVersion}` | Advance shared queue; position=0, new `refTs`; same |
| POST | `/sync/session/queue` | `{add?:[mediaId], remove?:[indices], move?:{from,to}, ifVersion}` | Queue mutation; same |
| POST | `/sync/session/end` | `{ifVersion}` | state=ended, `event_seq++`; broadcast one composite `session-ended` event that also sets mode=`individual`; individual sessions restored |
| POST | `/sync/session/lease` | `{connectionId, takeover?:boolean}` | From an authenticated admin WebSocket connection. Server issues `{generation, leaseToken}` and binds it to that connection. Reconnect/takeover rotates both and fences the old socket. DJ control requests carry `X-Flow-DJ-Connection` + `X-Flow-DJ-Lease`; any mismatch is `409 stale-lease`. |

Member heartbeats:

| Method | Path | Body | Notes |
|---|---|---|---|
| POST | `/sync/heartbeat` | `{sharedSessionId, positionMs, bufferMs}` | 204 on in-tolerance; `409 {targetPositionMs, reason:"behind"|"ahead"}` when `|drift| > threshold` (client seeks); 410 if session ended. V1 ships the **WS `heartbeat` message as primary** (no HTTP keepalive chatter); this endpoint exists for native clients and diagnostics. |

### 5.1 Event ordering and consistency (ADR-006)

- **Two counters, deliberately distinct:**
  - `version` — order of shared **content** mutations (play/pause/seek/next/prev/queue). Bumped on content changes only.
  - `event_seq` — order of **all** authoritative state events, including lifecycle events that don't change content (`dj-disconnected`, `session-ended`). Always monotonically increasing per session; equals the WS envelope `seq`.
- **Invariant: exactly one authoritative state event is emitted per `event_seq`, and the Postgres checkpoint of the shared-session row and the insert of that `sync_events` row commit in the *same* transaction, before the WS broadcast.** Clients therefore never see a broadcast whose checkpoint has not committed (no phantom events on crash).
- **Snapshot consistency:** reconnect is performed on `/ws`: register the subscriber and buffer events first, then read/send the committed snapshot, drain buffered events above the snapshot sequence, then go live. This prevents the snapshot-N/missed-N+1 race. `/sync/state` alone is diagnostic and not a complete reconnect protocol.
- **Sequence-gap detection:** if a client observes `event_seq` jumping (skipped numbers) on a live connection, it triggers an immediate re-snapshot (the gap means a frame was lost on that connection; the snapshot is the resync source).
- Stale-event rejection is by `event_seq`: drop `<= snapshot.eventSeq`; a lifecycle event can carry the same current `version` as the prior content event.
- Exactly one executor command/transaction produces one authoritative event. Multi-field changes use one composite payload. Per-client corrections and acknowledgements are transient and do not allocate an `event_seq`.

## 6. Mode Control

| Method | Path | Body | Role |
|---|---|---|---|
| GET | `/platform/mode` | – | `{mode, activeSharedSessionId?}` (all roles) |
| POST | `/platform/mode` | `{mode: "individual"\|"synchronized", seed?, ifVersion?}` | admin only. Implements PRODUCT_SPEC §3.3 transitions: on `synchronized` creates the session (seed defaults to the admin's individual current track/position); on `individual`, `ifVersion` is required and the same lease/version fencing as `/sync/session/end` applies. Session start/end events are the mode notifications; no duplicate authoritative mode event is emitted. |

## 7. Admin — Membership (durable provisioning, ADR-004)

| Method | Path | Body | Notes |
|---|---|---|---|
| GET | `/admin/requests` | `?status=pending` | list |
| POST | `/admin/requests/{id}/approve` | `{note?, temporaryPassword}` | Creates the Flow member with `mustChangePassword=true`, then runs `not_started → credential_ready → creating_nd_user → linking → complete`. The admin communicates the temporary Flow password out-of-band; it is never returned or audited. The encrypted reusable Navidrome credential commits before remote creation; retry reuses it. |
| POST | `/admin/requests/{id}/reject` | `{note?}` | Terminal; audited |
| GET | `/admin/users` | `?status=`, `?role=`, `?provisioningState=` | account list including in-flight/failed provisioning and durable `navidromeCleanupState` (admin can see and repair) |
| POST | `/admin/users` | `{username, displayName, email?, temporaryPassword}` | Direct account creation (no request); same provisioning machine; first-login password change required. |
| POST | `/admin/users/{id}/suspend` | `{reason?}` | `status=suspended` + session row delete + audit. M2 attaches credential-cache eviction and active-stream flush when those components first exist (ADR-001 §2.4). |
| POST | `/admin/users/{id}/reactivate` | – | `status=active` (provisioning must be `complete`; else 409) |
| POST | `/admin/users/{id}/retry-provisioning` | – | Re-run the state machine from the current step (V1 repair affordance for `failed`) |
| POST | `/admin/users/{id}/reset-password` | `{temporaryPassword}` | Replaces the Flow password hash, sets `mustChangePassword=true`, and revokes all sessions. The password is communicated out-of-band and never returned. |
| DELETE | `/admin/users/{id}` | `?hard=true` | Soft: `status=deleted` + session revoke + stream flush. Hard: additionally best-effort Navidrome user delete (compensation step; may leave the Navidrome row if Navidrome is down — recorded as a compensation-pending audit entry, retried on demand). |

## 8. Admin — Library & Platform

| Method | Path | Body | Notes |
|---|---|---|---|
| POST | `/admin/library/scan` | `{full?:boolean}` | Navidrome `startScan` (admin); 202 + job semantics |
| GET | `/admin/library/scan/status` | – | |
| DELETE | `/admin/media/{mediaId}` | – | V1.x only; no V1 route. V1.x defines separate, explicit index-removal and physical-file-deletion operations after their Navidrome behavior is verified. |
| PUT | `/admin/media/{mediaId}` | `{title?, album?, artist?, …}` | Advanced metadata editing is V1.x only; no V1 route. |
| POST | `/admin/uploads` | multipart (`files[]`, `albumPath` e.g. `Artist/Album`) | One new album directory per batch. Stage under the same filesystem root, validate + fsync + manifest, atomically rename the directory into the watched library, then trigger scan. Existing destination = conflict; no merge/overwrite in V1. |
| GET | `/admin/uploads` | `?state=` | upload_operations list (state, scan status, errors) |
| GET | `/admin/settings` | – | runtime settings |
| PUT | `/admin/settings` | `{mode?, defaultTheme?, allowDownloads?, syncDriftThresholdMs?, syncHeartbeatIntervalMs?, djGracePeriodS?, maxConcurrentStreamsPerUser?}` | Partial updates; every change audit-logged (correlation id) |
| GET | `/admin/integration/health` | – | Bootstrap/health: Navidrome reachable? admin credential verified? gate invariants (downloads/artwork/sharing off) hold? `200 {ok, details}` / `503` |

## 9. WebSocket — `/ws`

### 9.1 Handshake and auth (ADR-002)

- `GET /ws` with `Upgrade`; authentication is the **`flow_session` cookie** (validated server-side, exactly like REST) **plus an `Origin` header check** (must equal the configured origin; any mismatch ⇒ `403`).
- **No token in the query string. No `Authorization`-header JWT. No `?token=`.** (Task B: "Do not put credentials or session tokens in WebSocket query parameters.")
- Server replies `101` (authenticated + origin ok) or `401`/`403`.
- One connection per browser tab/device. Each receives a server-generated `connectionId`. Reconnect uses the atomic subscribe/snapshot/drain protocol (§5.1).
- Native clients (V1.x) authenticate the upgrade with the bearer-session mechanism; the Origin check is relaxed for them (no cookie), same token validation.

### 9.2 Envelope

Every message is a JSON frame:

```json
{ "type": "<event>", "v": 1, "seq": 42, "version": 7, "ts": "<rfc3339>", "payload": { … } }
```

- `seq` = **event_seq** of the shared session this event belongs to (lifecycle + content events alike; ADR-006). For non-sync events (mode, library) `seq`/`version` are absent.
- `v` = protocol version. Clients drop frames with unknown `v`.

### 9.3 Server → client events

| type | fields | when |
|---|---|---|
| `hello` | `{connectionId, mode, session?, user:{role,status,provisioningState}}` | transient snapshot after subscription is registered |
| `session-started` | `{version, seq, fullState}` | initial authoritative session event |
| `state-changed` | `{version, seq, changes, fullState?}` | one composite authoritative event per committed command |
| `session-ended` | `{version, seq, reason}` | `reason` ∈ `admin` \| `dj-disconnect` \| `platform-restart` \| `grace-expiry` |
| `dj-disconnected` / `dj-reconnected` | `{version, seq, graceUntil?}` | lifecycle events carry the current content version |
| `seek-correction` (per-client) | `{targetPositionMs, refTs, version, basedOnEventSeq}` | transient; does not increment `event_seq` |
| `library-changed` | `{resource, ids:[…]}` | after a scan completes (poll-driven in V1) |
| `error` | `{code, message}` | |

Client → server:

| type | payload |
|---|---|
| `heartbeat` | `{sharedSessionId?, positionMs, bufferMs}` — every `settings.sync_heartbeat_interval_ms` during sync |
| `ping` | `{}` → `pong` (keeps NAT/Proxy alive; Caddy idle timeout 60 s) |
| `subscribe` / `snapshot-request` | `{lastSeenEventSeq?}` → server performs a fresh subscribe/snapshot/drain cycle (§5.1) |

### 9.4 Ordering, staleness, gaps

Per §5.1: snapshot is the resync source; `event_seq` orders; a gap ⇒ re-snapshot. Events are **not** retransmitted on a single connection (loss = reconnect + snapshot); the drift-correction loop self-heals content drift within one heartbeat cycle.

## 10. Feature Flags (V1 surface, admin-gated)

| Flag | Default | Effect when on |
|---|---|---|
| `settings.allow_downloads` (platform) + `user_settings.allow_downloads` (per-user) | off | Reserved for V1.x. No download route exists in V1; `ND_ENABLEDOWNLOADS=false` remains a deployment invariant. |

Rule: a capability is *visible* to a member only if the platform flag **and** the per-user flag are both true.

## 11. Versioning and compatibility

- No `/v1` prefix in V1.0.0; a breaking change later goes under `/v2` with the old paths running one release.
- WS protocol `v:1` in every frame; bump on any payload-shape change, run both for one release.
- Clients send `X-Flow-Client: web|android|desktop` + `X-Flow-Version` for observability.

## 12. Route → requirement traceability (kept in sync with REQUIREMENTS/ROADMAP)

| Area | Routes | Requirement IDs | Milestone |
|---|---|---|---|
| Auth (sessions) | §1 | FR-AUTH-002…007, NFR-SEC-004/006 | M1 |
| Library + gateway | §2 | FR-LIB-001/002/004/005, FR-STRM-001…005 | M2 |
| Playlists | §3 | FR-PL-001…003 | M2 |
| Individual playback | §4 | FR-IND-001…006 | M3 |
| Sync | §5 | FR-SYN-001…011, FR-MODE-001…003 | M4A/M4B |
| Membership | §7 | FR-MEM-001…009 | M1 |
| Admin library/uploads/settings | §8 | FR-ADM-001…006 | M5 |
| WS | §9 | FR-SYN-003/007 | M4B |
