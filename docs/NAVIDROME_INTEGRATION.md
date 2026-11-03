# Flow — Navidrome Integration

Version: 1.0.0-draft
Target: Navidrome as checked out in `../navidrome` (master, Go 1.27). All endpoint claims below were verified against that source; nothing here is assumed from documentation alone.

---

## 1. What Navidrome Gives Flow (verified in source)

| Capability | Navidrome surface | Verified location | Flow uses it for |
|---|---|---|---|
| Subsonic browse (`getAlbum`, `getAlbumList2`, `getArtists`, `getArtist`, `getMusicDirectory`) | `GET /rest/{command}?u=&p=&c=&v=` | `server/subsonic/browsing.go` | Library proxy (member-scoped) |
| Subsonic search (`search2`, `search3`) | `GET /rest/search3` | `server/subsonic/searching.go` | Search |
| Stars | `star`, `unstar`, `getStarred2` | `server/subsonic/album_lists.go` | Personal favorites |
| Streaming | `GET /rest/stream?id=&u=&p=…&jwt=…&format=&maxBitRate=` | `server/subsonic/stream.go` — supports `timeOffset`, `format`, `maxBitRate`; FLAC passes through lossless; transcoding (incl. Opus) via `core/stream/decider.go` | Stream URL issuance |
| Download | `GET /rest/download` — gated by `conf.Server.EnableDownloads` | `server/subsonic/stream.go` (`Download`) | V1.x downloads (flag off in V1) |
| Playlists | `getPlaylists`, `getPlaylist`, `createPlaylist`, `updatePlaylist`, `deletePlaylist` | `server/subsonic/playlists.go` — note: Subsonic playlist CRUD **has no per-user write restriction in the API itself**; write restriction is a Flow concern (admin-only routes) | Admin playlist CRUD; member read |
| Scan control | `getScanStatus` (all users), `startScan` (admin-only, via `adminOnly` middleware) | `server/subsonic/library_scanning.go` | Admin re-scan trigger |
| User read | `getUser` (self), `getUsers` (admin-only) | `server/subsonic/users.go` | Member introspection (rarely needed) |
| **User create/update/delete** | **501 in Subsonic** (`createUser`, `updateUser`, `deleteUser`, `changePassword` are declared "Not Implemented") | `server/subsonic/api.go` — `h501(r, "createUser", "updateUser", "deleteUser", "changePassword")` | → use Native API instead (§4) |
| Native user management | `GET/POST/PUT/DELETE /api/user` under `Authenticator` + admin-only for user routes | `server/nativeapi/native_api.go` (`rx(r, "/user", …, persistable=true)` inside the protected group) | Provisioning members (admin token) |
| Native media/metadata | `/api/song`, `/api/album`, `/api/artist` (read, non-persistable); `/api/song/{id}/playlists`; admin-only `POST /api/metadata/{kind}/{id}/refresh`, admin-only `/api/library` repo (persistable), admin-only `/api/inspect`, `/api/config` | `server/nativeapi/*` | Admin metadata edits, delete-missing, config reads |
| Native maintenance | `DELETE /api/missing` (`DeleteMissingFiles`), `POST /api/missing/{id}/remap` | `server/nativeapi/missing.go` + `core/maintenance.go` | Admin "remove songs" |
| Per-user JWT | `POST /auth/login` (body `{username,password}`) → `{token, subsonicSalt, subsonicToken}`; JWT accepted on Subsonic calls via `jwt` param (`validateCredentials(usr, pass, token, salt, jwt)` in middlewares.go) and on Native API via `Authorization: Bearer` (chi `JWTVerifier` in `server/auth.go`) | `server/auth.go` | Per-user credential isolation (§3) |
| External auth (reverse-proxy) | `ExtAuth.UserHeader` (default `Remote-User`), `ExtAuth.TrustedSources` IP allowlist | `server/auth.go` `UsernameFromExtAuthHeader` | Not used in V1 (see alternatives) |
| Events (SSE) | `GET /api/events` (auth + `DevActivityPanel` must be on) | `server/events/sse.go`, `cmd/root.go` | Not used; Flow owns its own WS |
| Jukebox | `jukeboxControl` → `core/playback` — drives **hardware audio devices**, per user | `server/subsonic/jukebox.go`, `core/playback/*` | **Not reusable** for peer sync (confirmed: it's a device controller, not a listener-coordination service) |

### Capabilities Navidrome does NOT provide (Flow builds these)

- Registration requests / approval workflow / suspension — Flow Postgres + Navidrome user CRUD.
- Synchronized listening across networked clients — Flow's sync engine (PLAYBACK_SYSTEM.md).
- Per-user playback session persistence across devices — Flow `session_state` (Navidrome has per-user `playQueue` via OpenSubsonic `savePlayQueue` — **usable in V1.x** as a fallback/resume source; V1 uses Flow's own persistence).
- Theming of *our* UI — irrelevant to Navidrome.

## 2. Which API does Flow call, and with which credential?

| Flow operation | Navidrome endpoint | Credential |
|---|---|---|
| Browse/search/star/stream/playlist-read | Subsonic `/rest` | **the member's own** JWT (cached server-side, §3) |
| Playlist create/update/delete | Subsonic `/rest` (`createPlaylist`…) or Native `/api/playlist` | **admin** JWT |
| Metadata edit, delete missing files, library scan config | Native `/api` | **admin** JWT (admin-only routes enforce it) |
| `startScan` | Subsonic `startScan` (admin-gated) | **admin** JWT |
| Create/delete member users | Native `/api/user` POST/DELETE | **admin** JWT |
| Assign a user's libraries | Native `PUT /api/user/{id}/library` (admin-only) | **admin** JWT |

Rule: **members' credentials are used only for read/stream operations as themselves. Admin credentials are only held by the Flow backend process.** This is the credential-isolation contract (SECURITY.md §4).

## 3. Per-User Credential Isolation (the core mechanism)

Navidrome authenticates Subsonic calls in one of three ways (verified in `server/subsonic/middlewares.go` `authenticate`):

1. `u` + `p` (password), or `u` + `s` + `t` (salt+token), or
2. `u` + `jwt` (JWT from `/auth/login`), or
3. `apiKey` (player API keys — **do not use**).

Flow's strategy — **no credentials on the wire to clients, ever**:

1. On member provisioning, Flow generates a random password `P_m` (32 bytes, `crypto/rand`), creates the Navidrome user with `P_m` via the admin native call, and stores `AES-GCM(P_m)` under the key in `FLOW_NAVIDROME_KEY` in `users.navidrome_password_enc`.
2. On first authenticated use, Flow calls `POST /auth/login` as the member (username + decrypted `P_m`), receives the Navidrome JWT, and caches it in memory: `map[navidromeUserID]jwtCacheEntry{token, expiresAt}`.
3. Every proxied read/stream call from that member is made with `?u=<member>&jwt=<cached>`.
4. On JWT expiry (~Navidrome's TTL; the token is refreshed automatically by Navidrome on use via `JWTRefresher`), Flow re-logins transparently.
5. **Stream URLs returned to members are Subsonic stream URLs with the member's cached JWT embedded** — scoped to that member, 15-minute Flow-level lifetime, no admin material, no password, no salt.

Consequences:

- A member who captures their stream URL can only stream, as themselves — exactly their permission level. They cannot call `startScan`, user CRUD, or admin metadata (those routes 403 for non-admin JWTs — verified in `native_api.go` `adminOnlyMiddleware`).
- Member suspension in Flow: stop issuing URLs + (V1.x) delete the Navidrome user. Cached JWTs are also evicted on suspend.
- No member can enumerate the admin's credential because it never appears in any member-facing response.

**Known limitation (documented, accepted):** a captured stream URL is valid until its Navidrome JWT dies (hours, refreshable). The 15-minute Flow-level cap + no password exposure bounds the blast radius. V1.x: issue *per-request* short tokens via a dedicated streaming proxy if the capture window matters.

## 4. User Provisioning Flow (admin, server-side)

```
approve(request)                          directCreate(member)
   │                                             │
   ▼                                             ▼
1. Flow: INSERT users (role=member, status=active)   [tx begins]
2. Flow: navidrome POST /api/user {userName, password:P_m, email, isAdmin:false}
   ────────────────────────────────────────────── [if 2 fails: tx rolls back, status stays pending]
3. Flow: navidrome PUT /api/user/{id}/library {libraryIds:[default…]}   (auto-done in step 2 for new non-admins: 
   they get `default_new_users` libraries automatically — verified in persistence/user_repository.go Put)
4. Flow: UPDATE users SET navidrome_user_id=…, navidrome_password_enc=AES(P_m)  [tx commits]
5. audit_log row
```

Removal / suspend:

- Suspend: Flow `status=suspended` + evict cached JWTs + (V1.x) disable the Navidrome user via `POST /api/user/{id}` `isEnabled`-style update — V1: Flow simply refuses all Flow API calls; Navidrome access was never public anyway, so there is nothing else to lock.
- Delete: `DELETE /api/user/{id}` (admin) + Flow `status=deleted`. Navidrome's `core/user.go` also unloads that user's plugins on delete (observed side-effect, harmless).

Admin bootstrap (FR-AUTH-001):

- On first boot, `FLOW_ADMIN_USERNAME`/`FLOW_ADMIN_PASSWORD` exist in env but no `users` row does → Flow creates the admin user row, then `POST /api/user` to Navidrome **as admin** (the admin's Navidrome user is created with `P_admin`, `P_admin` encrypted and stored the same way; the admin's Navidrome account is an admin in Navidrome (`isAdmin:true`)).
- Idempotent: if the Navidrome admin user already exists (operator pre-created it), Flow detects it via `GET /api/user?query=…` and just records the link — no duplicate.

## 5. Music File Ingestion (uploads)

Navidrome's scanner watches configured folders (`musicfolder` config) and imports new files; Flow leverages that:

1. Upload endpoint streams the multipart body straight to `<musicVolume>/<albumPath>` (validated path: `Artist/Album/NN – Title.ext`, sanitized; no `..`, no absolute paths). Extensions against Navidrome's known MIME table (FLAC/MP3/AAC + others it indexes).
2. After the batch writes successfully, Flow calls `startScan` (fast scan) with the admin JWT.
3. Web UI polls `/admin/library/scan/status` until the scan reports idle, then refreshes library views.

Duplicates (same file re-uploaded): V1 behavior = the scanner's own move/match logic (Navidrome `matcher`) detects renames/moves; identical-content duplicates are not deduped in V1 (documented; V1.x: hash-based pre-upload check in Flow).

Deletion: `DELETE /admin/media/{id}?deleteFile=true` → Flow native call semantics: mark missing / delete row; if `deleteFile`, also `os.Remove` on the music volume path (Flow has the path from the indexed file) — done *after* the Navidrome-side delete succeeds.

## 6. Navidrome Deployment Settings Flow Requires

| Setting | Value in `deploy/` | Why |
|---|---|---|
| `musicfolder` | `/music` | shared volume |
| `enablesharing` | `false` | Navidrome's public `/share` endpoints stay off; Flow is the only entry |
| `enabledownloads` | `false` (**must be explicit — Navidrome defaults it to `true`**) | downloads are a Flow-gated capability |
| `enableartworkupload` | `false` (**must be explicit — Navidrome defaults it to `true`**) | admin edits artwork via Flow later |
| `authrequestlimit` | `60` / `authwindowlength` `5m` | brute-force backstop on `/auth/login` (Flow also rate-limits) |
| `devactivitypanel` | `false` | SSE broker not needed |
| `enableexternalservices` | `false` | no LastFM/Deezer/ListenBrainz for a private platform |
| `scannervalues` | `musicfolder:/music/…` | |
| `datafolder` | `/data` | Navidrome SQLite + cache |
| TLS on the Navidrome service | not required | internal network only; Caddy terminates TLS at the edge |
| Navidrome **ports published** | none | the security boundary of the whole design |

## 7. Alternatives Considered

| Alternative | Rejected because |
|---|---|
| Point Caddy at Navidrome with basic-auth passthrough (ext-auth mode) | Gives every member a full Navidrome session (including access to `getUser` and, on any misroute, admin endpoints). Credential isolation by JWT-caching is strictly stronger and keeps Navidrome invisible. |
| Share Navidrome's SQLite between Flow and Navidrome | Two writers to one DB, no transactional isolation between apps, fights Navidrome's goose migrations. Separate DBs, soft references. |
| Modify Navidrome to add a sync service / membership API | Violates the unmodified-Navidrome principle; sync is Flow's domain; the plugin system (`plugins/pdk`) is interesting for V2 (e.g., a scrobbler into Flow history) but not needed in V1. |
| Run Navidrome's Subsonic UI as the "member app" and Flow only as admin | No: the product *is* the custom UI; the spec forbids leaning on Navidrome's UI as the experience. |
| `apiKey` (player) credentials for members | Player keys have no user principal (no `u=`), which breaks per-user stars/queues and makes revocation coarser. Rejected. |

## 8. Failure Handling

| Navidrome state | Flow behavior |
|---|---|
| Unreachable (health probe `GET /rest/ping` every 10 s, 1 s timeout) | Library/browse endpoints → `503 navidrome_unavailable` (clients show retry); stream URLs not issued; sync session unaffected (audio comes from Navidrome, but *state* is Flow's — a stuck shared session with audio down shows "stream error" per member, coordinator keeps running). |
| Back (health restored) | Flow re-primes cached-JWT health (login once per member lazily on next use); scan-status polling resumes. |
| Scan running | `GET /admin/library/scan/status` reflects it; uploads are still accepted (scanner handles new files appearing mid-scan per its phases — document as: upload during scan = safe, new files picked up in the same or next pass). |
