# Flow — API Specification

Version: 1.0.0-draft
Base path: `https://<domain>/api` (REST) and `wss://<domain>/ws` (WebSocket)
All responses are JSON unless noted. Auth: `Authorization: Bearer <flow-jwt>` (httpOnly cookie `flow_session` for web).

---

## 0. Conventions

- Errors: `{"error": {"code": "stream_denied", "message": "…", "detail": "…"}}` with standard HTTP status.
- Pagination: `?cursor=<opaque>&limit=50`; responses include `nextCursor` (null when exhausted). Cursor is opaque (base64 of offset+key); do not parse.
- Media IDs: Navidrome `media_file.id` (string). Pass through, never re-encode.
- All timestamps: RFC 3339 UTC.
- Every authenticated endpoint checks Flow role/status server-side; a 403 means "authenticated but not allowed", a 401 means "no valid session".

### Error codes (stable, used by clients)

| code | HTTP | meaning |
|---|---|---|
| `unauthenticated` | 401 | missing/invalid/expired Flow session |
| `forbidden` | 403 | authenticated, wrong role or status (suspended) |
| `not_found` | 404 | |
| `conflict` | 409 | username/email taken, request already decided |
| `rate_limited` | 429 | |
| `navidrome_unavailable` | 503 | Navidrome health probe failing; library endpoints retry-safe |

## 1. Auth (Flow)

| Method | Path | Body | Response | Notes |
|---|---|---|---|---|
| POST | `/auth/login` | `{username, password}` | `{token, user:{id, username, role, status}}` | Rate-limited; sets httpOnly cookie on web |
| POST | `/auth/logout` | – | `204` | Revokes session |
| GET | `/auth/me` | – | `{user, settings:{themeId, preferredStreamFormat}, mode}` | Also returns platform mode so the client can render the correct player |
| POST | `/auth/register-request` | `{displayName, email, desiredUsername}` | `202 {requestId}` | Anonymous. One open pending request per email. |

## 2. Library (member+admin read; proxied to Navidrome Subsonic)

| Method | Path | Query | Response shape |
|---|---|---|---|
| GET | `/library/albums` | `cursor, limit` | `{items:[{id, title, artist, albumArtist, coverUrl, songCount, durationMs, year}], nextCursor}` |
| GET | `/library/albums/{albumId}` | – | album detail + track listing (ordered) |
| GET | `/library/artists` | `cursor, limit, query?` | `{items:[{id, name, albumCount, coverUrl}]}` |
| GET | `/library/artists/{artistId}` | – | discography + top tracks |
| GET | `/library/songs` | `cursor, limit, albumId?` | flat song list |
| GET | `/library/search` | `q, limit, albumCount?, artistCount?, songCount?` | `{albums:[], artists:[], songs:[]}` |
| GET | `/library/genres` | – | V1.x stub: 501 with `retryInV1x` note |
| POST | `/library/stars` | – | `{mediaId, kind:"song"\|"album"}` → star (Navidrome `Star`) |
| DELETE | `/library/stars/{kind}/{mediaId}` | – | unstar |
| GET | `/library/stars` | `kind` | starred items (per user) |
| GET | `/library/scanner/status` | – | scan progress |

### Stream URL issuance

| Method | Path | Query | Response |
|---|---|---|---|
| GET | `/streams/{mediaId}` | `format? (flac\|ogg\|mp3\|auto), maxBitrate?` | `200 {url, expiresAt, format, mimeType}` |

- `url` is a **Navidrome Subsonic stream URL pre-signed for this user's credentials** with a 15-minute expiry. It is the only Navidrome URL a client ever sees.
- `format=auto` → Flow resolves per `user_settings.preferred_stream_format` + client negotiation hint (header `X-Flow-Client: web` in the request; V1.x: `Audio.isTypeSupported`-driven).
- 403 for suspended users; 404 if media no longer exists in the index.
- **No `download` variant exists in V1.0.0.** (Navidrome's `enableDownloads` defaults to **true** — the compose file must set `ND_ENABLEDOWNLOADS=false` explicitly; this is a deployment invariant, SECURITY §4. Download capability is a Flow-gated admin flag, off by default, implemented in V1.x.)

### Playlists

| Method | Path | Body | Notes |
|---|---|---|---|
| GET | `/playlists` | – | All members. `{items:[{id, name, songCount, public}]}` |
| GET | `/playlists/{id}` | – | Track listing (members too) |
| POST | `/playlists` (admin) | `{name, comment?, songIds?:[]}` | |
| PUT | `/playlists/{id}` (admin) | `{name?, comment?, songIds?, removeIndices?}` | |
| DELETE | `/playlists/{id}` (admin) | – | |
| POST | `/playlists/{id}/queue` | – | Returns Navidrome playlist id; stream via `/streams/` per-track in V1 (V1.x: playlist-native stream) |

## 3. Playback — Individual

| Method | Path | Body | Notes |
|---|---|---|---|
| GET | `/playback/session` | – | `{queue, queueIndex, currentMediaId, positionMs, volume, shuffle}` |
| PUT | `/playback/session` | `{queue?, queueIndex?, positionMs?, volume?, shuffle?}` | Throttled writes (10 s max); client may send more often, server drops |
| DELETE | `/playback/session` | – | Clears queue (e.g., "close player") |

All user-scoped. No cross-user reads are possible (routes resolve the session from the authenticated user only).

## 4. Synchronized Mode (shared session)

Read endpoints (authenticated, both roles):

| Method | Path | Response |
|---|---|---|
| GET | `/sync/state` | `{mode, session:{id, version, state, currentMediaId, positionMs, refTs, queue}, you:{joined, driftMs?}}` (503 if no active session and mode≠synchronized) |
| GET | `/sync/participants` (admin only) | `[{userId, username, joinedAt, lastHeartbeat, estimatedDriftMs}]` |

Write endpoints (**admin only**; members get 403):

| Method | Path | Body | Effect (server-side) |
|---|---|---|---|
| POST | `/sync/session` | `{seedMediaId?, seedPositionMs?}` | Create shared session (mode must be `synchronized`; auto-switches mode per admin intent, see §6) |
| POST | `/sync/session/play` / `pause` | – | State mutation, version++, checkpoint, WS broadcast |
| POST | `/sync/session/seek` | `{positionMs}` | Same; also emits per-client seek corrections computed immediately |
| POST | `/sync/session/next` / `prev` | – | Advance shared queue; position=0, new refTs |
| POST | `/sync/session/queue` | `{add?:[mediaId], remove?:[indices], move?:{from,to}}` | Queue mutation |
| POST | `/sync/session/end` | – | state=ended, broadcast `session-ended` |

Member heartbeats:

| Method | Path | Body | Notes |
|---|---|---|---|
| POST | `/sync/heartbeat` | `{sharedSessionId, version, positionMs, bufferMs, clientTs}` | 204 on success; 409 with `{state:"behind", targetPositionMs}` if |drift|>threshold → client seeks; 410 if session ended. Also available as WS `heartbeat` message to avoid HTTP keepalive chatter (V1 ships both; web uses WS). |

## 5. Mode Control

| Method | Path | Body | Role |
|---|---|---|---|
| GET | `/platform/mode` | – | `{mode, activeSharedSessionId?}` (all roles) |
| POST | `/platform/mode` | `{mode: "individual"\|"synchronized", seed?}` | admin only. Implements PRODUCT_SPEC §3.3 transitions: on `synchronized` creates session (seed defaults to admin's individual current track/position); on `individual` ends shared session and restores individual sessions. |

## 6. Admin — Membership

| Method | Path | Body | Notes |
|---|---|---|---|
| GET | `/admin/requests` | `?status=pending` | list |
| POST | `/admin/requests/{id}/approve` | `{note?}` | Provisioning flow (see NAVIDROME_INTEGRATION §5): user row → Navidrome user → link. 409 if username taken. |
| POST | `/admin/requests/{id}/reject` | `{note?}` | |
| GET | `/admin/users` | `?status=`, `?role=` | member list |
| POST | `/admin/users` | `{username, displayName, email?}` | Direct account creation (no request) |
| POST | `/admin/users/{id}/suspend` | `{reason?}` | Revokes Flow sessions + (best effort) deletes Navidrome user's active access by deactivating |
| POST | `/admin/users/{id}/reactivate` | – | |
| DELETE | `/admin/users/{id}` | – | Deactivate + delete Navidrome user (admin-confirmed flag `?hard=true` removes the Navidrome user) |

## 7. Admin — Library & Platform

| Method | Path | Body | Notes |
|---|---|---|---|
| POST | `/admin/library/scan` | `{full?:boolean}` | Navidrome `startScan`; 202 + job semantics |
| GET | `/admin/library/scan/status` | – | |
| DELETE | `/admin/media/{mediaId}` | `?deleteFile=true` | Delete track (DB row + optional file removal) — Navidrome admin maintenance |
| PUT | `/admin/media/{mediaId}` | `{title?, album?, artist?, …}` | Metadata edit (Navidrome native `metadata` refresh semantics: tags written, then re-scan affected album) |
| POST | `/admin/uploads` | multipart (`files[]`, `albumPath` e.g. `Artist/Album/01 – Track.flac`) | Streams into the music volume; responds `{accepted:[], rejected:[{name, reason}]}`; auto-triggers fast scan |
| GET | `/admin/settings` | – | |
| PUT | `/admin/settings` | `{mode?, defaultTheme?, allowDownloads?, syncDriftThresholdMs?, …}` | Partial updates; every change audit-logged |

## 8. WebSocket — `/ws`

### 8.1 Handshake and auth

- `GET /ws` with `Upgrade` + `Authorization: Bearer <flow-jwt>` (query param `token` accepted as fallback for non-browser clients; web uses the header via `Cookie`-carried JWT automatically).
- Server replies `101` or `401`.
- One connection per browser tab/device. Reconnects re-authenticate and fetch a snapshot (§4 GET /sync/state) — the protocol is resync-safe.

### 8.2 Envelope

Every message is a JSON frame:

```json
{ "type": "<event>", "v": 1, "ts": "<rfc3339>", "seq": 42, "payload": { … } }
```

`seq` is a per-connection monotonic counter. `v` = protocol version. Clients drop frames with unknown `v`.

### 8.3 Server → client events

| type | payload | when |
|---|---|---|
| `hello` | `{mode, session?, user:{role, status}}` | on connect |
| `mode-changed` | `{mode}` | admin switches platform mode |
| `sync-play` / `sync-pause` | `{version, refTs}` | state mutation |
| `sync-seek` | `{version, positionMs, refTs}` | |
| `sync-queue` | `{version, queue, currentIndex}` | |
| `sync-next` | `{version, currentMediaId, refTs, queueIndex}` | |
| `session-ended` | `{reason: "admin"\|"dj-disconnect"\|"platform-restart"}` | |
| `dj-disconnected` | `{graceUntil: <ts>}` | admin WS closed mid-session |
| `seek-correction` (uniquely per client) | `{targetPositionMs, refTs, version}` | coordinator judged |drift|>threshold |
| `library-changed` | `{resource:"album"\|"artist"\|"song", ids:[…]}` | after scan completes (Subsonic/Native event → Flow re-broadcast; V1.x: triggered by scan-status polling) |
| `error` | `{code, message}` | |

Client → server:

| type | payload |
|---|---|
| `heartbeat` | `{sharedSessionId?, version, positionMs, bufferMs}` — every 5 s during sync |
| `ping` | `{}` → `pong` (keeps NAT/Proxy alive; Caddy idle timeout 60 s) |
| `snapshot-request` | `{}` → server re-sends `hello` + current `sync-*` state for the active session |

### 8.4 Ordering and staleness

- Events carry `version` (shared session) and `ts` (server clock).
- A client that reconnects mid-session: sends `snapshot-request`, server responds with the full current state at version V; the client then applies the **stale-event rule**: drop every queued event whose `version ≤ V`; apply events with `version > V` strictly in `version` order (if two are out of order on arrival, hold the higher one until the lower applies).
- Events are not retransmitted on a single connection; loss = reconnect + snapshot. The coordinator's drift correction loop self-heals the rest (a missed `sync-next` surfaces as drift and is corrected within one heartbeat cycle).

## 9. Feature Flags (V1 surface, admin-gated)

| Flag | Default | Effect when on |
|---|---|---|
| `allowDownloads` (platform) + `allow_downloads` (per-user) | off | Enables `GET /downloads/{mediaId}` (API shape reserved in V1; 501 until implemented in V1.x; deploy invariant: Navidrome `ND_ENABLEDOWNLOADS=false` until the flag ships). Clients hide the button unless both flags are on |

Rule: a capability is *visible* to a member only if the platform flag AND the per-user flag are both true.

## 10. Versioning and compatibility

- No `/v1` prefix in V1.0.0; if a breaking change ever lands, new routes go under `/v2` and old paths keep running one release.
- WS protocol version `v:1` in every frame; bump on any payload shape change, run both for one release.
- Clients send `X-Flow-Client: web|android|desktop` + `X-Flow-Version: 1.0.0` for observability.
