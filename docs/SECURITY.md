# Flow — Security

Version: 1.1.0-draft (supersedes 1.0.0-draft)
Threat posture: A-003/A-004 from REQUIREMENTS — trusted LAN operator, invited members, internet edge at Caddy. Design goal: **no client, including an adversarial member, can exceed the permissions of their Flow role, even if they reach the stack with custom tools.**

Revisions: browser auth is **opaque server-side sessions** (ADR-002) — no signed JWTs, no WS query-param tokens; streaming is the **gateway** (ADR-001) — no Navidrome credentials reach any client; bootstrap is operator-assisted (ADR-003); provisioning is a durable state machine (ADR-004).

---

## 1. The Threat Model (explicit, so decisions are defensible)

| Threat | Actor | Mitigation (section) |
|---|---|---|
| Member guesses/reaches Navidrome's API | any authenticated member | Navidrome has **no public ingress** and no client ever holds a Navidrome credential (§2, ADR-001) |
| Member crafts raw HTTP to Flow's admin endpoints | member with valid Flow session | Role-gated routes + per-route role middleware; admin endpoints 403 for members even with a valid *member* session (§3) |
| Member captures/forwards an audio request | member | Audio is the Flow gateway (`/audio/…`); a replayed request is re-authorized per request against the *member's own* Flow session + status. No Navidrome credential is transferable because there is none in the client's possession (§4, ADR-001) |
| Session-token theft (browser) | external | Token is opaque, SHA-256-hashed at rest, HttpOnly + Secure + `SameSite=Lax`, path-scoped; revocation is immediate (row delete). CSRF: `SameSite=Lax` + server-side Origin check on cookie-authenticated mutations (§5, ADR-002) |
| WebSocket hijack / cross-site WS | external | WS upgrade authenticates the **session cookie** + strict **Origin** validation; **no token in the query string** (ADR-002) |
| Request forgery / XSS → account takeover | external | `SameSite=Lax` cookie blocks cross-site cookie submission; CSP; user data rendered as text (§5) |
| Brute-force on Flow login or register-request spam | external | Rate limits (§6) |
| Path traversal / symlink / collision in upload | admin only (uploads are admin-only) | Same-filesystem sibling staging outside the watched mount; one validated new album directory is atomically renamed; traversal/symlink/absolute-path/collision attempts are rejected (NAVIDROME_INTEGRATION §6) |
| Credential leakage via logs | ops | No Navidrome credentials/JWTs/DB-DSN in logs; redaction middleware; request logging on `/audio` without upstream-internal details (§8) |
| DB dump | host-level, out of scope but noted | PostgreSQL contains credential ciphertext + key-version metadata only. Master keys live in a separate mounted keyring; a DB dump alone cannot decrypt Navidrome credentials (§4/§10). |
| Zombie DJ takeover in sync | a reconnected stale DJ client | Server-issued generation + rotated opaque token bound to one authenticated connection; every command is fenced (PLAYBACK_SYSTEM §3.5, ADR-005). |

## 2. Network Topology = Primary Control

```
Internet ──► Caddy:443 (TLS/ACME)
                ├── /        → Caddy-served static web bundle
                ├── /api     → flow-backend:8080
                ├── /audio   → flow-backend:8080 (streaming gateway)
                ├── /ws      → flow-backend:8080/ws
                └── (navidrome: no route. period.)
Docker internal network (no host-published ports):
                flow-backend:8080 ──► navidrome:4533
                flow-backend ──► postgres:5432
                flow-backend ──► /music-root (rw)
                                 ├── library/ (promotion target)
                                 └── .flow-staging/ (Flow-only)
                navidrome ──► /music-root/library mounted as /music (ro)
```

- **Navidrome publishes zero ports** and has no route in the Caddyfile. The only path to Navidrome HTTP is through the Flow backend, which is the only process holding Navidrome admin credentials.
- Consequence: a member can only ever talk to Flow. "Bypass Flow and hit Navidrome directly" is not possible from the network, and is also not possible by *credential* because **no client holds a Navidrome credential** (ADR-001).

## 3. Authorization Architecture (Flow backend)

Three-layer enforcement (defense in depth; every layer is cheap):

```
L1  Middleware auth:  resolve the Flow session (cookie → SHA-256 → sessions row)
                       → attach User{role, status, provisioningState} to context
                       (status=suspended or provisioning≠complete ⇒ 403 account_not_active, every route;
                        session row deleted ⇒ 401)
L2  Route group gates:
      /api/auth/*             — anonymous (login, register-request); logout is session-scoped
      /api/library|playlists|playback|sync|audio|themes — any ACTIVE, COMPLETE user
      /api/admin/*            — admin only  (route middleware + RoleAdmin)
      /api/platform/mode      — admin write, all-user read
L3  Domain checks: services re-assert invariants (sync mutations require role==admin *and*
      DJ-lease ownership when a session is active; membership provisioning is the state machine)
```

- No endpoint trusts a client-supplied `role`. The role comes from the DB row bound to the verified session.
- **WebSocket:** the upgrade authenticates with the **same session cookie**, validated server-side exactly like REST, **plus an `Origin` header check** (must equal the configured domain). **No token in the query string, no `Authorization`-header JWT, no separate WS token** (ADR-002, NFR-SEC-004). The WS hub checks L1 on connect and L2 on every control-message type (`sync-*` control messages: admin-only — enforced in the message dispatch table, not just the HTTP route).
- **Members cannot escalate**: the only writes that change roles are `/admin/users` (admin-gated) and the bootstrap mapping (operator-supplied credential, verified at boot, no public API surface).

## 4. Credential Isolation (Navidrome side, ADR-001)

Verified facts driving this (see NAVIDROME_INTEGRATION.md §2–3):

1. **Members' Navidrome accounts**: server-generated passwords (`crypto/rand`, 32 B). Ciphertext + key version commits before remote creation (ADR-004). Master keys are loaded from an external keyring and never stored in PostgreSQL.
2. **The Navidrome admin credential**: operator-supplied (ADR-003). M0 uses it from runtime secrets only; M1 persists only its ciphertext + key version. Plaintext exists only in bounded process memory when authenticating.
3. **Navidrome JWTs (admin + member)**: in-memory cache, keyed by Flow user, evicted on suspend/delete; never logged, never serialized to disk, **never returned to any client**. The cache parses `exp`, refreshes before expiry, permits one login per user at a time, and uses generation fencing so an in-flight pre-suspension refresh cannot repopulate or return an evicted credential.
4. **Audio and artwork flow through the gateway**: `/audio/…` authorizes the Flow session, then attaches the appropriate **server-side** credential to the upstream Navidrome call and pipes bytes. No Navidrome address, credential, salt, or JWT appears in any client-facing response, URL, or log.
5. **Termination on revocation**: stream handles are indexed by Flow session + user. A new request registers before any Navidrome lookup/open, then rechecks the authoritative Flow session/account. Registration and user blocking share one lock: a concurrent suspension either rejects registration or cancels the new handle. Logout flushes the session's handles; suspension/deletion deletes all user sessions, blocks new registration, flushes all user handles, and generation-evicts the cached member JWT. Bytes already written to the client cannot be recalled, but the upstream is canceled immediately when the registry flush runs.
6. **Navidrome config invariants**: `enableSharing=false`, `enableDownloads=false`, `enableArtworkUpload=false` (**all three default to `true`/are on by default in Navidrome — the compose file sets each explicitly**). No anonymous public surface; and with downloads off, the gateway's read-only model cannot be widened by Navidrome configuration.
7. **Response minimization**: audio/artwork forward only safe representation headers. Navidrome cookies, redirects, authorization headers, `Content-Disposition`, internal URLs, and error bodies are dropped. A `416` retains only the safe status/range metadata. There is no V1 download route, and unknown `/audio` query keys such as `download` are rejected.

The invariant, testable end-to-end: **an attacker who fully compromises a member's device and captures every request/response the client ever saw can at worst replay the Flow gateway as that member, as that member, read-only, while that member's Flow session is alive — and nothing Navidrome-specific, because the client never possessed it.**

## 5. Web Client & CSRF Hardening (ADR-002)

- Cookie: `flow_session` = `HttpOnly`, `Secure`, `SameSite=Lax`, `Path=/`, value = 32-byte random token (64 hex). Only its SHA-256 hash is stored (Postgres `sessions.token_hash`).
- **Logout**: row delete ⇒ the next REST/WS validation fails; the logout service also synchronously flushes active-stream handles for that session before returning. There is no grace period and no in-memory authentication cache.
- **Suspension**: row delete for all that user's sessions (same mechanism) + stream abort + JWT eviction.
- **CSRF**: `SameSite=Lax` already prevents a cross-site page from submitting the cookie on `POST/PUT/PATCH/DELETE` (it is attached only on top-level GET navigation, which in this API is read-only). Defense-in-depth: the backend additionally validates `Origin` (or `Referer`) on cookie-authenticated mutations; mismatch ⇒ 403. There is no double-submit token in V1 — SameSite + Origin check is the documented, tested control (acceptance test 3).
- **WebSocket**: upgrade authenticated by the session cookie + `Origin` equality check. **No credentials or session tokens in the WS query string** (task B / ADR-002 explicit).
- CSP: `default-src 'self'; connect-src 'self' wss://<domain>; img-src 'self' data:; style-src 'self' 'unsafe-inline'` (Tailwind output is self-contained; revisit if a CDN ever enters). No `dangerouslySetInnerHTML` for user data.
- Register-request is the only fully anonymous mutation: body-validated, one open pending request per email, no PII beyond name/email/username; no account exists until approval.
- **Initial member password:** approval/direct-create/reset accepts an admin-chosen temporary Flow password (12–128 characters) that the admin communicates out-of-band. Only its argon2id hash is stored. First login is restricted to `/auth/me`, `/auth/change-password`, and logout until the member replaces it; the temporary password never appears in responses, audit details, or logs.
- **Flow administrator password:** `FLOW_ADMIN_PASSWORD` is operator-managed and authoritative for the single V1 administrator. Changing it and restarting performs an audited Argon2id hash rotation and revokes existing admin sessions; the member password-change route does not override it.
- **Cookie transport:** production configuration requires HTTPS and sets `Secure`. The documented loopback-only HTTP development origin omits `Secure` solely so local browsers can send the cookie; it is not a production option.
- **Native clients (V1.x+)**: a separately defined bearer-session mechanism (random token issued on `/auth/native/login`, presented via `Authorization: Bearer`; the Origin check is relaxed, token validation is identical). Not a JWT — same opaque, revocable model, different transport.

## 6. Rate Limiting

| Surface | Limit | Where |
|---|---|---|
| `/auth/login` | 10 / 15 min / IP **and** 20 / 15 min / username | Flow middleware (in-memory token bucket; single instance ⇒ fine) + Navidrome's `authrequestlimit` backstop |
| `/auth/register-request` | 3 / 24 h / IP, 1 open pending per email | Flow |
| `/audio/*` (stream start) | hard fixed M2 cap of 100 concurrent streams per user; over-cap starts return 429 before opening Navidrome. M5 moves the value into runtime settings. | Flow gateway |
| Uploads | size cap per file (default 500 MB, admin-set), batch cap 100 files | Flow |
| Sync heartbeats | per-connection min interval 2 s (drops+warns beyond) | WS hub |
| Library proxy | authenticated per request; a dedicated library burst limiter is deferred until measured need/M6 hardening | Flow |

Client-IP derivation trusts the first valid `X-Forwarded-For` hop only when the direct peer is loopback/private (the canonical Caddy-to-backend path). A public direct peer cannot choose the rate-limit key. Production does not publish the backend port.

## 7. Upload Validation (admin route, task G)

- Route: admin-only. Member attempts are 403 before any body is read.
- Staging: `<music-root>/.flow-staging` and `<music-root>/library` are siblings on the same filesystem. Navidrome mounts only `library`. Separate Docker volumes are explicitly forbidden for this guarantee because cross-filesystem rename is not atomic.
- Path: `albumPath` sanitized — each segment matches `^[A-Za-z0-9 _\-()\.\[\]]+$`, no `..`, no leading `/`, no symlinks (the staged path is created with `O_NOFOLLOW`-equivalent checks on Unix), total depth ≤ 3, final component is a file with an allowed extension (FLAC/MP3/M4A/AAC/OGG/MP4/WAV + Navidrome's indexable set; generated from Navidrome's `mime_types.yaml` at build time).
- Collision: the target album directory must not exist. V1 rejects rather than merges or overwrites.
- Body: streaming write (never buffer the whole file); `Content-Length` enforced ≤ cap; abort + partial-file cleanup on any error.
- Malicious-content note: a corrupted FLAC in the volume makes *that* track unplayable, not the platform. No execution path from media content into the backend (Navidrome streams it; Flow never execs media).

## 8. Logging and Secrets

- `slog` JSON; levels via `FLOW_LOG_LEVEL`.
- Redaction middleware: on `/audio/*`, `/auth/*` (password), `/admin/uploads` — log method+path+status+duration only; **never** upstream Navidrome URLs/credentials, never query strings containing credentials, never request bodies.
- `FLOW_NAVIDROME_ADMIN_PASSWORD` and DB credentials come from environment/secret mounts. Credential master keys come from the root-readable file named by `FLOW_CREDENTIAL_KEYRING_FILE`; the file is outside the repository and image. Only empty examples are committed.
- The external JSON keyring contains an active integer version and base64-encoded 32-byte AES keys. PostgreSQL contains only version metadata and AES-GCM envelopes; the Navidrome credential username/purpose is authenticated as AEAD associated data so ciphertext cannot be silently swapped between identities. This username is stored separately because the Flow administrator login may differ from the linked Navidrome administrator username.
- Audit log (`audit_log`, correlation id + outcome, DATABASE_SCHEMA §2.10) is the compliance trail; it never contains credentials.

## 9. CI / Image Hygiene

- Backend: pinned `golang:<version>` (DEVELOPMENT_GUIDE §1) multi-stage build → `distroless/static` (or `alpine` with non-root user); no shell in the final image.
- `trivy` scan on both images in CI; Dependabot on Go modules + npm.
- Migrations: `up` in CI against a disposable Postgres; forward-only in production (DEVELOPMENT_GUIDE §7).
- Secrets in CI: none for the fast suite. The pinned-Navidrome job uses a disposable Compose volume and fixed test-only bootstrap values, never operator credentials.

## 10. Backup and Recovery

| Asset | What | How |
|---|---|---|
| `music/` volume | **the irreplaceable asset** | Operator's existing backup (NAS snapshot / rsync to cold storage). Flow never rewrites originals. |
| Flow Postgres | users, sessions, provisioning, sync state, audit, ciphertext + key-version metadata | `pg_dump` nightly; restore = new cluster + run migrations. Contains no master key. |
| Navidrome data | SQLite index + cache | `nd_data` volume copy with Navidrome stopped; index is rebuildable by re-scan (worst case: drop the volume, re-scan). |
| External credential keyring | versioned master keys required to decrypt stored credentials | Back up separately from PostgreSQL under independent access controls. Restore requires a keyring containing every version referenced by the restored dump. |

Recovery drill (DEPLOYMENT.md): restore Flow Postgres, restore the matching external keyring separately, validate that every referenced key version exists, then boot. Rotation may retire a key only after no retained backup still references it.

Key rotation procedure:

1. Add a new version to the external keyring while retaining the old decrypt key; deploy and validate both load.
2. Mark the new version active in `credential_key_versions`; all new ciphertext uses it.
3. Re-encrypt credential rows in bounded transactions using CAS on their current version; audit counts and failures without logging plaintext.
4. Verify no live row references the old version, mark it `decrypt_only`, and complete a restore drill with the new keyring.
5. Remove the old key only after the retention window for every backup that references it has expired or those backups have been re-encrypted/replaced.

## 11. Security Acceptance Tests (implemented by the owning milestones)

1. Member session → every `/admin/*` and `POST /platform/mode`: **403**. (Route-table assertion test over all admin routes.)
2. Member session → WS `sync-play` control message: **rejected by dispatch** (`error: forbidden`), no state change.
3. **CSRF**: cross-site `POST /api/auth/logout` with the victim's `flow_session` cookie but foreign `Origin` ⇒ **403** (Origin check); same-site top-level navigation is read-only by design. **Session revocation**: a valid logout request deletes the row, flushes that session's in-flight `/audio` proxy, and makes the next request 401.
4. `curl` any Navidrome port on the host: **connection refused** (compose publishes none) — a deployment test.
5. **Credential isolation**: capture a full `/audio` response (headers + body) for a member ⇒ assert it contains no Navidrome host, no `u=`/`jwt=`/`s=`/`t=` params, no admin credential material. Replay the request after the member is suspended ⇒ **403/401** and the in-flight stream is aborted.
6. Compose config check: assert the three gates are false using the pinned release's source-verified native route `GET /api/config/config`; fail verification if the route is disabled or any gate is open.
7. Upload route with a member session: **403**, body not read; a staging artifact is never present in the music folder mid-upload.
8. **Bootstrap**: start the backend against a Navidrome whose first admin is *not* yet created ⇒ `degraded` state, no data mutated; after the operator creates the admin and configures the secret ⇒ M0 `/healthz` reports integration verified. The admin UI is an M1 deliverable (ADR-003).
9. **Zombie DJ**: after a reconnect/takeover rotates the connection-bound lease, the old socket repeats a command with its old token and generation ⇒ **409 stale-lease**, no state change (ADR-005).
10. **Provisioning invariant**: a user with `provisioning_state ≠ complete` is denied login and every gateway request (`account_not_active`), regardless of `status` (ADR-004).
11. **M4A authority boundary**: anonymous sync reads are 401; member mode/control writes are 403; foreign-Origin mode/control writes are 403; no public lease-acquisition route exists; stale connection/generation/token combinations are 409 and leave checkpoint/event sequence unchanged.

M4A rechecks the persisted user role/status/provisioning state and opaque
session row inside the same transaction that locks the platform/shared-session
rows. Suspension or revocation that wins before this recheck prevents the
command. Lease tokens are returned only by the trusted future M4B connection
boundary; PostgreSQL stores only their SHA-256 hashes.

M2 fast-suite coverage executed locally: member/admin library authorization, member playlist-mutation denial, safe-header allowlisting, unsupported download-query rejection, original/transcoded Range truthfulness, client cancellation, active-stream suspension, registration/suspension races, concurrent credential refresh, refresh eviction fencing, malformed upstream responses, and bounded-copy behavior with a 32 MiB fixture. The extended real pinned-Navidrome suite compiled but was not executed in this environment because Docker/Navidrome were unavailable; acceptance items depending on it remain pending that job.
