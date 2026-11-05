# Flow — Navidrome Integration

Version: 1.1.0-draft (supersedes 1.0.0-draft)
Target: Navidrome release `v0.64.2`, source commit `101145742f4762164202cb4858c9126258bdc463` (Go 1.27, SQLite), image `deluan/navidrome:0.64.2@sha256:38dc2727bfcfd5ede290f8ada114fc90368146f265ae4701ddddbcbe2a44ee52`. A table status of **verified** means source-verified at that tag. The M0 real-contract suite passed against this immutable image; the M2 suite has been extended for the new adapter/gateway contracts and must pass in the Docker-capable pinned job before the M2 completion gate is signed off.

Revisions: streaming goes through the **Flow gateway** (ADR-001) — per-user credentials are used server-side, never delivered to clients; admin bootstrap is **operator-assisted** (ADR-003); provisioning is a **durable state machine** (ADR-004); uploads use a **staged pipeline** (task G).

---

## 1. What Navidrome Gives Flow

| Capability | Navidrome surface | Status | Flow uses it for |
|---|---|---|---|
| Subsonic browse (`getAlbum`, `getAlbumList2`, `getArtists`, `getArtist`, `getMusicDirectory`) | `GET /rest/{command}` | **verified** `server/subsonic/browsing.go` | Library proxy (server-side) |
| Subsonic search (`search2`, `search3`) | `GET /rest/search3` | **verified** `server/subsonic/searching.go` | Search |
| Stars | `star`, `unstar`, `getStarred2` | **verified** `server/subsonic/album_lists.go` | Personal favorites (per-user, as that user) |
| Streaming | `GET /rest/stream?id=&u=&jwt=…&format=&maxBitRate=&timeOffset=` | **verified** `server/subsonic/stream.go` — `format=raw` selects original-file passthrough; `timeOffset`, `format`, `maxBitRate`; transcoding (incl. Opus) via `core/stream/decider.go`; Range via `http.ServeContent` for seekable streams | Upstream of the Flow **gateway** (ADR-001) |
| Cover art | `getCoverArt` | **verified** route in `server/subsonic/api.go` | Artwork via the gateway (`/audio/coverart/{id}`) |
| Download | `GET /rest/download` — gated by `conf.Server.EnableDownloads` (default **true**) | **verified** `server/subsonic/stream.go` | V1.x only; deploy invariant `ND_ENABLEDOWNLOADS=false` |
| Playlists | `getPlaylists`, `getPlaylist`, `createPlaylist`, `updatePlaylist`, `deletePlaylist` | **verified** `server/subsonic/playlists.go` — no per-user write restriction in the API; admin-gating is Flow's job | Admin playlist CRUD; member read |
| Scan control | `getScanStatus` (all users), `startScan` (**admin-only**, via `adminOnly` middleware) | **verified** `server/subsonic/library_scanning.go` | Admin re-scan trigger |
| User read | `getUser` (self), `getUsers` (admin-only) | **verified** `server/subsonic/users.go` | Bootstrap verification (§5) |
| **User create/update/delete** | **501 in Subsonic** (`createUser`, `updateUser`, `deleteUser`, `changePassword` declared "Not Implemented") | **verified** `server/subsonic/api.go` `h501(…)` | → use the Native API instead (§4) |
| Native user management | `GET/POST/PUT/DELETE /api/user` (persistable), under `Authenticator` | **verified** `server/nativeapi/native_api.go` (`rx(r, "/user", …, persistable=true)`) | Provisioning members (admin credential) |
| Native user-library assignment | `PUT /api/user/{id}/library` (admin-only) | **verified** `server/nativeapi/library.go` | Library assignment for members (default libs are auto-assigned to new non-admins — **verified** `persistence/user_repository.go` `Put`: `INSERT … WHERE default_new_users = true`) |
| Native media/metadata | `/api/song`, `/api/album`, `/api/artist`; admin routes including `/api/library` and source-verified config route `/api/config/config` | **verified** `server/nativeapi/*` | Config read-back; metadata mutation remains V1.x |
| Native maintenance | `DELETE /api/missing` (`DeleteMissingFiles`), `POST /api/missing/{id}/remap` | **source-verified** `server/nativeapi/missing.go` + `core/maintenance.go` | Not used in V1; V1.x destructive operations require separate verification |
| Auth: login | `POST /auth/login` (body `{username,password}`) → `{token, subsonicSalt, subsonicToken}` | **verified** `server/auth.go` — the returned `token` is a **JWT** (signed with Navidrome's auto-generated secret, refreshed on use by `JWTRefresher`) | Server-side credential cache (§3) |
| Auth: first admin | `POST /auth/createAdmin` — **only allowed while zero users exist** (`CountAll > 0 ⇒ 403 "Cannot create another first admin"`) | **verified** `server/auth.go` `createAdmin` + `server/server.go` route | Operator bootstrap path, one of two (§5) |
| Auth: auto first admin | env `devautocreateadminpassword` ⇒ on initial setup creates user `admin`/`Dev Admin` (consts `DevInitialUserName = "admin"`) | **verified** `conf/configuration.go` (default `""`), `server/initial_setup.go` | Operator bootstrap path, the other one (§5) |
| External auth (reverse-proxy) | `ExtAuth.UserHeader` (default `Remote-User`), `ExtAuth.TrustedSources` IP allowlist | **verified** `server/auth.go` `UsernameFromExtAuthHeader` | Not used in V1 (see §7) |
| Events (SSE) | `GET /api/events` (auth + `DevActivityPanel` must be on) | **verified** `server/events/sse.go`, `cmd/root.go` | Not used; Flow owns its own WS |
| Jukebox | `jukeboxControl` → `core/playback` — drives **hardware audio devices**, per user | **verified** `server/subsonic/jukebox.go`, `core/playback/*` | **Not reusable** for peer sync (it is a device controller, not listener coordination) |

### Capabilities Navidrome does NOT provide (Flow builds these)

- Registration requests / approval / suspension — Flow Postgres + Navidrome user CRUD.
- Synchronized listening across networked clients — Flow's sync engine (PLAYBACK_SYSTEM.md).
- Per-user playback session persistence across devices — Flow `session_state` (Navidrome has per-user `playQueue` via OpenSubsonic `savePlayQueue` — **usable in V1.x** as a secondary resume source; V1 uses Flow's own persistence).
- Streaming gateway with per-request authorization — Flow `streaming` package (ADR-001).

## 2. Which API does Flow call, and with which credential?

All calls are made **server-side by the Flow backend**. No client ever sees the credential used (ADR-001).

| Flow operation | Navidrome endpoint | Credential (server-side) |
|---|---|---|
| Browse/search/star/playlist-read (library proxy) | Subsonic `/rest` | **the requesting member's own** cached Navidrome JWT (member-scoped) |
| Audio proxy (gateway) | Subsonic `stream` (+ `getCoverArt`) | member-scoped (admin's streams use the admin credential) |
| Playlist create/update/delete | Subsonic `createPlaylist`, `updatePlaylist`, `deletePlaylist` | **admin** cached JWT |
| `startScan` | Subsonic `startScan` (admin-gated) | **admin** cached JWT |
| Create/update/delete member users; library assignment | Native `/api/user` POST/PUT/DELETE, `PUT /api/user/{id}/library` | **admin** cached JWT |
| Config read-back (bootstrap verification) | Native `GET /api/config/config` in `v0.64.2` | **admin** cached JWT |
| Metadata refresh / delete-missing | Native admin routes | **admin** cached JWT (V1.x metadata; M5 maintenance) |

Rule: **member-scoped credentials are used only for that member's own read/stream calls. The admin credential is held only inside the Flow backend process.** This is the credential-isolation contract (SECURITY §4).

## 3. Credential Model (server-side, ADR-001)

Navidrome authenticates Subsonic calls as (verified in `server/subsonic/middlewares.go` `authenticate`):

1. `u` + `p` (password), or `u` + `s` + `t` (salt+token), or
2. `u` + `jwt` (JWT from `/auth/login`), or
3. `apiKey` (player API keys — **not used**).

Flow's strategy — **no credentials on the wire to clients, ever**:

1. **Admin credential (ADR-003):** operator-supplied at bootstrap. M0 uses it only from runtime configuration to verify the adapter. M1 persists it as versioned AEAD ciphertext; PostgreSQL stores no master key. Flow caches the returned Navidrome JWT in memory and reauthenticates when required.
2. **Member credentials (ADR-004):** provisioning generates `P_m`, encrypts and commits it with the active external key version **before** calling Navidrome, then reuses the same credential for every create/reconcile retry. Member JWTs are cached in memory only.
3. **Gateway usage:** every `/audio` request authorizes the Flow session first, then attaches the right **server-side** credential to the upstream call. The response is piped to the client with **no Navidrome address, credential, or JWT** in any header, URL, or body.
4. **Termination:** each stream is registered by Flow session + user. Logout aborts streams opened by that session but leaves the per-user Navidrome JWT cache available to other valid sessions. Suspension/delete aborts all streams for the user and evicts the cached Navidrome JWT.

M2 cache rules:

- The JWT `exp` claim is parsed; a token within 30 seconds of expiry is not reused for a new request.
- At most one Navidrome login per Flow user is in flight. Concurrent callers share it; a live waiter retries if the refresh owner alone is canceled.
- Eviction always increments a per-user generation. A login started before suspension cannot publish or return its result after eviction (`ErrCredentialInvalidated`).
- An upstream authentication rejection invalidates only the matching token and retries once; there is no administrator-credential fallback for a member operation.

### 3.1 M2 Subsonic mapping and binary behavior

- Albums: `getAlbumList2(type=alphabeticalByName, offset, size)`. Flow uses a one-item lookahead for cursor pagination; it does **not** invent or depend on an `X-Total-Count` header.
- Album/artist/song detail: `getAlbum`, `getArtists`, `getArtist`, `getTopSongs(artist=<name>)`, and `getSong`.
- Flat songs/search: `search3`; stars: `star`, `unstar`, `getStarred2`; scan status: `getScanStatus`.
- Playlists: `getPlaylists`, `getPlaylist`, `createPlaylist`, `updatePlaylist`, `deletePlaylist`. Flow explicitly marks admin-created playlists public, then rejects member mutations at the Flow HTTP boundary.
- Binary responses: `stream` and `getCoverArt` remain streaming bodies. Flow forwards only representation headers (`Content-Type`, `Content-Length`, `Content-Range`, `Accept-Ranges`, validators), never cookies, redirects, authorization headers, `Content-Disposition`, or raw upstream error bodies.
- `416` preserves the status and `Content-Range` but discards Navidrome's body. Artwork is `Cache-Control: private, max-age=3600`; audio is `private, no-store`.
- Ordinary Navidrome calls use `FLOW_NAVIDROME_TIMEOUT`; streaming has no whole-body deadline and instead uses `FLOW_NAVIDROME_STREAM_HEADER_TIMEOUT`, request cancellation, lifecycle cancellation, and shutdown cancellation.

Consequences:

- A member (or anyone who captures any response) holds **zero** Navidrome material: no username-in-URL, no salt, no password, no JWT, no internal address. Blast radius of a fully compromised client = *that client's own, currently-authorized, read/stream level* while its Flow session lives.
- `startScan`, user CRUD, and admin metadata are unreachable for members: they are admin-credential calls, and member credentials are never admin-scoped.
- No member can enumerate the admin credential: it appears in no member-facing response, URL, or log.

## 4. Membership Provisioning (durable state machine — ADR-004, FR-MEM-004)

Postgres and Navidrome **cannot share a transaction**. Consistency is a per-user state machine with idempotent steps, persisted in `users.provisioning_state`:

```
                approve(request) / directCreate(member)
                               │
                               ▼
                    not_started ──► credential_ready
                                        │  generate P_m; encrypt + commit ciphertext/key-version first
                                        ▼
                                  creating_nd_user
                                        │  POST /api/user with durable P_m
                                        │  timeout/exists: find username + authenticate with P_m
                                        ▼
                                      linking
                                        │  capture navidrome_user_id; verify/assign intended libraries
                                        ▼
                                     complete   (status may now flip to active)
                                        │
   any step failing:                    │  step failure → provisioning_state = failed, audit entry (correlation id),
   (Navidrome 5xx, timeout, 4xx)       │  admin-visible in /admin/users, retryable via /retry-provisioning
                                        ▼
                                     failed ──► retry from the failed step (idempotent)
```

Rules:

- **A user is `active`-capable only when `provisioning_state = complete`.** Login and the gateway both check it (`account_not_active` otherwise). Partially provisioned users can never stream or browse.
- **Crash-safe credential:** `credential_ready` is committed before any remote I/O. A crash after remote user creation cannot lose the password needed to recover that user.
- **Idempotent reconciliation:** username equality alone is not ownership proof. An existing user is linked only after authentication with the durable `P_m` succeeds; otherwise the flow stops as a visible collision.
- **Compensation:** if a step fails *after* the Navidrome user exists but before `linking` completes, admin retry reconciles it with the durable credential. If the admin deletes the Flow user, `navidrome_cleanup_state` persists `pending/failed/complete` independently of the audit viewer. When the ID was not captured, cleanup lists by username and authenticates with the durable credential before deletion; username equality alone is never authority. A repeated hard delete retries safely, and media files are never touched.
- **Audit:** every transition writes `audit_log` (action `provisioning.<step>`, outcome ok/error, correlation id).
- **Suspend:** `status=suspended` ⇒ Flow sessions deleted + active streams aborted + cached member JWT evicted. Navidrome-side, the member's account *can still authenticate if reached* — but members have no path to Navidrome (NFR-SEC-001), so suspension is complete at the Flow boundary. (Optionally, V1.x disables the Navidrome user too.)
- **Reactivation:** `status=active` (requires `provisioning_state = complete`, else 409).
- **Deletion:** soft = `status=deleted` + session/stream teardown (Navidrome user retained); hard (`?hard=true`) adds best-effort Navidrome user delete. Navidrome's delete path also unloads that user's plugins (verified `core/user.go` — harmless side effect).

## 5. Navidrome Administrator Bootstrap (operator-assisted — ADR-003)

**The problem the decision solves:** Flow cannot create Navidrome's *first* admin through the API. The two first-admin mechanisms are both **pre-authenticated or condition-gated**:

- `POST /auth/createAdmin` works **only while zero users exist** (verified: `CountAll > 0 ⇒ 403`) and is rate-limited — it is Navidrome's own first-run affordance, not an authenticated admin API.
- `devautocreateadminpassword` auto-creates the `admin` user **at initial setup** (verified `server/initial_setup.go`) — a deploy-time env flag, not runtime.

There is **no authenticated endpoint that creates an admin** in Navidrome; admin user management is via the native API *as an existing admin*. Therefore the bootstrap is operator-assisted:

1. **Operator initializes Navidrome's first admin** using one of its supported first-run mechanisms:
   - (a) start the fresh Navidrome and use the UI's first-admin creation (backed by `createAdmin`), **or**
   - (b) set `ND_DEVAUTOCREATEADMINPASSWORD` (env for `devautocreateadminpassword`) in the Navidrome deploy so initial setup creates user `admin`.
   The canonical production procedure is the first-run UI/createAdmin path. The auto-admin setting is retained only for disposable development/integration environments.
2. **Operator supplies the resulting admin credentials to Flow** securely: `FLOW_NAVIDROME_ADMIN_USERNAME` / `FLOW_NAVIDROME_ADMIN_PASSWORD` in `.env` (chmod 600, not in the image, not in git).
3. **Flow authenticates and verifies the integration at boot:** `POST /auth/login`, `GET /api/user/`, and `GET /api/config/config` for the deploy gates. These exact paths are release-specific adapter contracts and are covered by M0 tests.
4. **Credential custody:** M0 verifies the supplied credential without persistence. M1 stores ciphertext + key version in PostgreSQL and loads key material only from the root-readable external keyring. The in-memory JWT cache is always transient.
5. **Flow establishes the admin mapping:** upserts the Flow admin row (`users` with `role=admin`, `is_navidrome_admin=true`, `provisioning_state=complete`, `status=active`) and links it to the Navidrome admin user id. **Idempotent** — restarts just re-verify. The Flow admin's *login* password is `FLOW_ADMIN_PASSWORD` (a distinct Flow-side credential, argon2id) — the Navidrome admin password is never reused for Flow login and never exposed to clients.

Note: with the **gateway** model, members' streaming does **not** require the admin credential at all — it uses each member's own scoped credential (§3). The admin credential is used for: provisioning (user CRUD), scans, admin's own audio, config read-back, and (V1.x) metadata/maintenance.

## 6. Music File Ingestion — Staged Upload Pipeline (task G)

Navidrome's scanner indexes its watched music folder. Flow **never writes partial files into that folder**; uploads go through a staging area:

1. **Authenticate + authorize:** `POST /admin/uploads` is admin-gated (403 before the body is read).
2. **Stream into same-filesystem staging:** multipart body streams to `<music-root>/.flow-staging/<batch>/<album>/*.part`. Navidrome mounts only `<music-root>/library`, so staging is not visible to its scanner while remaining on the same filesystem.
3. **Validate:** per-file size cap (default 500 MB, admin-set), extension allowlist (FLAC/MP3/M4A/AAC/OGG + Navidrome-indexable set), **path sanitization** (no `..`, no absolute paths, no symlinks — `albumPath` segments match `^[A-Za-z0-9 _\-()\.\[\]]+$`, depth ≤ 3), **collision check** (target path exists ⇒ reject that file, never overwrite an original).
4. **Completion validation:** byte count matches the declared size; file is closed and `fsync`-ed before leaving `staging`.
5. **Atomic promote:** V1 accepts one new album directory per batch. After all files and the staging directory are synced, `rename(2)` moves the completed album directory into `<music-root>/library/<albumPath>`. Existing destination directories are rejected; V1 does not merge. This single directory rename is the batch visibility boundary.
6. **Trigger scan:** `startScan` (fast) with the admin credential; 202 + job semantics.
7. **Record status/errors:** `upload_operations` row per batch (state `staging → validating → promoted → scanning → done|failed`), `scan_status` captured from the scan result; admin UI shows per-file outcomes.
8. **Crash recovery:** a durable manifest records expected paths, sizes, and hashes. Startup removes `.part` files, resumes fully staged validation, retries `ready_to_promote`, recognizes a destination matching the manifest after a crash between rename and DB update, and triggers a missed `scan_pending` scan. Cleanup is restricted to `.flow-staging`; library files are never removed by recovery.

**Guarantee: incomplete uploads are never visible in Navidrome's scanned music directory** — the scanner only ever sees fully promoted files.

Index-row removal and physical file deletion are both V1.x. They will use separate explicit operations after behavior is verified; V1 exposes neither an ambiguous `deleteFile` switch nor a destructive maintenance shortcut.

## 7. Navidrome Deployment Settings Flow Requires (invariants, verified at bootstrap)

| Setting | Value in `deploy/` | Why |
|---|---|---|
| `musicfolder` | `/music` | shared volume |
| `enabledownloads` | `false` (**must be explicit — Navidrome defaults it `true`**, verified `conf/configuration.go`) | downloads are a Flow-gated V1.x capability |
| `enableartworkupload` | `false` (**must be explicit — defaults `true`**) | admin edits artwork via Flow later |
| `enablesharing` | `false` | no anonymous public `/share/*` surface; Flow is the only entry. At `v0.64.2`, disabled Subsonic sharing commands are registered through `h501` and return HTTP 501 rather than an HTTP-200 Subsonic failure envelope. |
| `authrequestlimit` / `authwindowlength` | `60` / `5m` | brute-force backstop on `/auth/login` (Flow rate-limits too) |
| `devactivitypanel` | `false` | SSE broker not needed |
| `devuishowconfig` | `true` | exposes the authenticated admin-only `/api/config/config` read-back used to verify security gates |
| `enableexternalservices` | `false` | no LastFM/Deezer/ListenBrainz for a private platform |
| `datafolder` | `/data` | Navidrome SQLite + cache |
| TLS on the Navidrome service | not required | internal network only; Caddy terminates TLS at the edge |
| Navidrome **ports published** | **none** | the security boundary of the whole design |

**Source-verified at `v0.64.2`:** Viper uses prefix `ND` and replaces dots with underscores, so the flat keys map to `ND_ENABLEDOWNLOADS`, `ND_ENABLEARTWORKUPLOAD`, `ND_ENABLESHARING`, and `ND_DEVAUTOCREATEADMINPASSWORD`. The M0 adapter gate read-back and real contract suite passed against the immutable image pin above.

## 8. Alternatives Considered

| Alternative | Rejected because |
|---|---|
| Give members signed Navidrome stream URLs (per-user JWT in the URL) | Puts a Navidrome credential into the client's possession; a captured URL = a reusable Navidrome session for that user. The **gateway** (ADR-001) removes credentials from the client entirely. |
| Point Caddy at Navidrome with ext-auth passthrough | Grants every member a full Navidrome session (including `getUsers`-adjacent surface and, on any misroute, admin endpoints). The gateway model is strictly stronger and keeps Navidrome invisible. |
| Share Navidrome's SQLite between Flow and Navidrome | Two writers to one DB, no transactional isolation between apps, fights Navidrome's goose migrations. Separate DBs + durable state machines (ADR-004). |
| Modify Navidrome to add a sync/membership API | Violates the unmodified-Navidrome principle; sync and membership are Flow's domain (the plugin system `plugins/pdk` is a V2 option, not V1). |
| `apiKey` (player) credentials for members | Player keys have no user principal (no `u=`), which breaks per-user stars/queues and makes revocation coarser. Rejected. |
| Flow creates Navidrome's first admin at boot via `POST /api/user` | Impossible: the native user API requires an *existing* admin to authenticate (verified: `rx(r,"/user")` is inside the `Authenticator` group). Hence operator-assisted bootstrap (ADR-003). |

## 9. Failure Handling

| Navidrome state | Flow behavior |
|---|---|
| Unreachable (health probe `GET /rest/ping` every 10 s, 1 s timeout) | Library/browse + gateway → `503 navidrome_unavailable` (clients show retry); sync **state** unaffected (audio down per-client, coordinator keeps running); admin UI shows degraded integration. |
| Back (health restored) | cached-JWT health re-primes lazily (login on next use); scan-status polling resumes; gateway reopens upstream. |
| Scan running | `GET /admin/library/scan/status` reflects it; new promoted uploads are picked up by the same or next scan pass (uploading during a scan is safe — the scanner's phases handle new files). |
| Admin credential wrong/expired at bootstrap | integration state stays `degraded`; M0 reports a sanitized reason in `/healthz` and logs. The later admin UI exposes the same status. Operator fixes the secret and restarts; no product data is mutated. |

## 10. M2 Verification Status

- Deterministic fake-transport tests executed for browse/detail/search, malformed JSON, one-time authentication retry, raw Range/`206`, sanitized `416`, `Accept-Ranges: none`, cancellation, rate-limit headers, and internal-address redaction.
- The real pinned suite now exercises the M2 adapter with member-scoped album/artist/song lists and details, search/scan status, `format=raw` Range, MP3 and Opus `timeOffset` transcodes, artwork, star read/write, and an admin-created/updated public playlist list/detail read by a member.
- In the current implementation environment Docker access and a listening Navidrome instance were unavailable, so that extended live suite was compiled but not executed here. Its result remains an explicit M2 completion-gate item; no runtime-verified claim is made for the newly added cases until the pinned job passes.
