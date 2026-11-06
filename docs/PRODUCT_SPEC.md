# Flow — Product Specification

Version: 1.0.0-draft
Status: Final V1.0.0 scope definition

---

## 1. Vision

Flow is a private, admin-curated music platform. It is a personal listening sanctuary, not a public service. One administrator owns the entire music library. Approved members listen, they do not contribute.

The platform supports two distinct listening modes that the administrator switches globally:

- **Individual Mode** — every listener controls their own playback independently
- **Synchronized Mode** — one shared playback session; the administrator is the DJ/host; all members listen to the same track at the same position

*Note:* audio (in both modes) flows through the **Flow streaming gateway** — each client streams through Flow's authorized `/audio` endpoint; no client holds Navidrome credentials. Audio bytes transit Flow's incremental proxy, while the synchronized-playback WebSocket carries control events only (ADR-001).

The visual identity is a core product feature. Flow ships with a token-based theme system and at least five visually distinct theme directions. A user's theme choice is personal and does not affect other members.

---

## 2. User Roles

### Administrator (single, initially)

| Capability | Details |
|---|---|
| Music library | Upload FLAC/MP3/AAC and read indexed metadata. Tag editing and index/file deletion are V1.x. |
| Playlists | Create, edit, delete admin playlists (visible to all members) |
| Membership | Review approval requests, approve/reject, create/suspend/remove member accounts |
| Platform | Switch between Individual and Synchronized modes, control synchronized playback |
| Settings | Configure platform-wide defaults, theme defaults, feature flags |
| Sync session | Select tracks, play/pause, seek, next/prev, manage shared queue, start/end session |

### Member (read-only)

| Capability | Details |
|---|---|
| Browse | Albums, artists, songs, search |
| Stream | Any track in the library at original or transcoded quality |
| Play (Individual) | Control own play/pause, next/prev, seek, queue, volume |
| Play (Sync) | Listen to shared session; follow admin's track selection; no independent control |
| Favorites | Personal star/unstar on songs and albums (V1) |
| Theme | Select personal theme; does not affect other members |
| Downloads | Explicitly disabled in V1.0.0; feature flag reserved for future |

Members **cannot**: upload, modify metadata, create/edit/delete playlists, manage other members, change platform settings, control synchronized playback, switch modes.

---

## 3. Listening Modes in Detail

### 3.1 Individual Mode

Each authenticated user has an independent playback session. Sessions are isolated:

- Playback position, queue, volume are per-user and persisted across sessions/devices
- One user's controls have zero effect on another user's session
- User can switch tracks freely within the library
- User can start or stop their session at any time

State to persist per user (Postgres):
- Current track ID, position (milliseconds), canonical queue (media IDs and added
  timestamps), current index, volume, shuffle, transport intent, revision, and
  updated timestamp. Resume has no age cutoff and never auto-plays without a
  gesture. Individual controls/checkpoints use the CAS contract in PLAYBACK_SYSTEM
  §2 and API §4; no automatic multi-device queue merge.

### 3.2 Synchronized Mode

A single shared session is created when the admin switches to Synchronized Mode. All members in the library are part of the same listening room.

**Admin (DJ) controls:**
- Start session, stop session
- Select/queue/swap tracks
- Play/pause/seek/next/prev on the shared queue
- Session is server-authoritative

**Member behavior:**
- All members stream the same track independently, **through the Flow streaming gateway** (ADR-001)
- No admin audio is relayed — each client has its own authorized stream; no client holds Navidrome credentials
- Members have no playback controls in sync mode
- Late-joining members enter the currently active shared session and sync to the current position
- Disconnected members can rejoin; their position is resynchronized on reconnect

**Synchronization model (high-level):**

The Flow backend maintains:
- A shared session object: current track, queue, playback state (playing/paused), reference timestamp
- A session **version** counter — incremented on every admin content action
- A monotonic **event sequence** — one authoritative state event per sequence value, including lifecycle events (ADR-006)
- A server timestamp for each state transition

Clients report their playback position periodically (heartbeat). The coordinator computes a drift offset and sends correction events (seek to target position, or pause) to clients that drift beyond a threshold (default: 2 seconds).

No audio is relayed through the WebSocket synchronization bus. All audio bytes flow through the Flow streaming gateway to Navidrome; the WebSocket channel carries control events only.

### 3.3 Mode Transition Rules

| Transition | Behavior |
|---|---|
| Individual → Synchronized | Admin initiates. Admin's current playback position and track are used to seed the shared session. Members are notified of the mode change and begin playing the seeded track at the seeded position. Members who are not currently playing also join at the seeded position. |
| Synchronized → Individual | Admin ends the session. All members receive a "session ended" event. Each member's individual session state is restored (or reset to empty if they had none). |
| Admin disconnects (sync active) | Shared session pauses. Members remain connected with a "DJ disconnected — resuming shortly" indicator. The admin has 90 seconds to reconnect, acquire a newly rotated connection-bound lease, and explicitly play or end. After 90 seconds the shared session ends and the platform falls back to Individual Mode automatically. |
| Member disconnects | Member can rejoin. On reconnect, the client requests the current session state and synchronizes. |
| New member joins while sync active | Member enters the current shared session and synchronizes to the current track/position. |
| Platform restart | V1: the shared session **ends safely** and the platform falls back to Individual Mode (ADR-005); individual sessions remain persisted and restorable. Auto-resume of a shared session is a V1.x improvement. |

These rules are documented and must be implemented exactly as specified. They are part of the V1.0.0 contract.

---

## 4. Membership Lifecycle

```
[Anonymous visitor]
        |
        v
   Submit request (name, email, desired username)
        |
        v
   PENDING_APPROVAL status
        |
        +---- Admin rejects --> REJECTED status (request closed)
        |
        v (Admin approves)
   APPROVED status
        |
        v
   Account provisioned in Navidrome (read-only user)
        |
        v
   ACTIVE status (member can log in and stream)
        |
        +---- Admin suspends --> SUSPENDED status (login blocked, data preserved)
        +---- Admin removes   --> DELETED status (Flow access revoked immediately;
                                  Navidrome account cleanup retries durably)
```

- Registration is the only anonymous action available on the platform
- No self-service account creation
- Admin can create member accounts directly (bypasses the request flow)
- Suspended accounts retain their data; reactivation restores access

---

## 5. Music Library

### 5.1 Source of Truth

All music files live in a shared volume. Navidrome indexes them. Flow does not maintain its own media index — it proxies library queries to Navidrome's Subsonic API and presents the results to its own clients.

### 5.2 Formats

- FLAC (primary, original quality preserved)
- MP3
- AAC / M4A
- Any format Navidrome's scanner supports, where appropriate

Transcoding is optional and controlled by Navidrome's transcoding pipeline. Original files are never modified or transcoded at rest.

### 5.3 Upload Flow (Admin, V1 — staged pipeline, task G)

1. Admin uses the web upload screen (drag-and-drop folder)
2. Flow streams one new album directory into `<music-root>/.flow-staging/<batch>` and validates size, type, paths, symlinks, declared byte counts, and collisions. The staging directory and `<music-root>/library` are sibling directories on the **same filesystem**; Navidrome is mounted only on `<music-root>/library`.
3. After every file is closed, synced, and validated, Flow atomically renames the completed album directory into the library. V1 rejects an existing destination directory rather than merging or overwriting it.
4. Flow backend triggers a Navidrome scan; scan status and per-file errors are recorded (`upload_operations`)
5. New tracks appear in the library after the scan completes

Original files are preserved. No re-encoding. No duplicate detection in V1 (documented as a V1.x improvement). A durable batch manifest lets startup recovery remove abandoned `.part` files, resume a pre-promotion batch, recognize a completed rename, and trigger a scan that was not yet started. Upload pipeline details: NAVIDROME_INTEGRATION §6, SECURITY §7.

### 5.4 Metadata

- Read from Navidrome's index (tags embedded in files)
- V1 metadata is read-only in Flow; advanced tag editing is V1.x
- No separate metadata database in Flow

---

## 6. Playlists

- Admin creates, edits, and deletes playlists (stored in Navidrome)
- Members see all admin-created playlists in the library
- Members can stream tracks from admin playlists but cannot modify them
- Members' personal favorites (stars) are stored in Navidrome's user-scoped star system (each user's stars are independent)

---

## 7. Themes

### 7.1 Architecture

Themes are defined as CSS custom property tokens. The web app resolves the active theme at runtime and applies tokens to the component layer. No theme ships hardcoded component styles; all visual properties flow through the token system.

```
theme-definitions/
├── pixel-art.css       ← CSS custom property definitions
├── cyberpunk.css
├── dark-gothic.css
├── ghibli-cozy.css
└── minimal-audiophile.css
```

Each theme file defines:
- Color tokens: `--flow-bg`, `--flow-surface`, `--flow-text-primary`, `--flow-accent`, etc.
- Typography: `--flow-font-heading`, `--flow-font-body`, font sizes, weights
- Component tokens: `--flow-player-bg`, `--flow-card-radius`, `--flow-card-shadow`
- Animation tokens: `--flow-transition-duration`, `--flow-card-hover-scale`

### 7.2 User Theme Selection

- Each member selects their personal theme in Settings
- Theme choice is stored per user in Postgres
- Changing a member's theme has zero effect on other members
- Admin can set the platform default theme (used by members who haven't chosen)

### 7.3 V1 Theme Scope

Five starter themes ship as static CSS files, and users switch among those built-in themes without a full reload in V1. Custom/community theme upload and hot-application of newly uploaded token CSS are V1.x.

---

## 8. Platform Clients

| Client | V1.0.0 | V1.x | V2.0.0 |
|---|---|---|---|
| Web (browser) | Required | Improved | Continuous |
| Android (Kotlin/Media3) | Not in scope | Background playback, offline queue | Full feature parity |
| Desktop (Tauri 2) | Not in scope | Linux + Windows, system media controls | Full feature parity |

All clients connect to the same Flow backend. The REST + WebSocket API contract is identical across all clients. No client-specific backends.

---

## 9. Non-Functional Product Goals

- **Privacy**: All data stays on the operator's infrastructure. No third-party tracking, no external analytics in V1.
- **Original quality**: FLAC streams at full quality by default. No forced transcoding.
- **Personal sanctuary**: Visual identity is distinctive and customizable. Not a generic streaming UI.
- **Admin authority**: Admin controls are server-authoritative. No client can escalate permissions.
- **Credential isolation**: No client ever holds Navidrome credentials; audio flows only through the Flow gateway (ADR-001).
- **Reliability**: Platform restarts do not lose individual playback state. In V1, a restart ends a shared session safely (ADR-005); individual sessions restore.
- **Low latency**: Synchronized mode should feel "together" — target ±2 seconds drift tolerance with correction.

---

## 10. Out of Scope for V1.0.0 (Documented Deliberately)

- Android app (all features)
- Desktop Tauri app
- Native-client bearer-session auth (V1 is browser cookie sessions; the bearer mechanism is reserved — ADR-002)
- Offline downloads for members
- Community/user-submitted themes
- Multiple admin accounts
- User-defined permission roles beyond admin/member
- Social features (chat, listening history sharing, reactions)
- Music streaming via external sources (YouTube, Spotify, etc.)
- Advanced audio analysis (sonic similarity search is Navidrome-native; exposed if available, not a V1 priority)
- Duplicate detection on upload
- Multi-library / multi-collection support
- **Advanced metadata editing** (tag write + affected-album re-scan) — V1.x (FR-ADM-009)
- **Destructive library operations** (delete index row or physical file) — V1.x (FR-ADM-003)
- **Auto-resume of a shared session across a backend restart** — V1 ends shared sessions safely (ADR-005); V1.x

These items are designed for but not implemented in V1.0.0. The architecture must not preclude them.
