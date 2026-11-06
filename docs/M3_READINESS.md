# M3 readiness audit — 2026-11-06

Inspected the reconciled specifications, ADR-001…006, migrations, application
composition, auth/session and membership gates, library/media adapters, gateway
and registry, API errors, frontend, CI, and existing tests before implementation.

| Area | Finding |
|---|---|
| Database | M0 migration ledger/advisory locking and M1 account/session schema exist. No playback table or media index copy exists. M3 can add its own reversible migration. |
| Authentication | Opaque cookie sessions resolve the current PostgreSQL account, status, provisioning, and mandatory-password gate on protected requests. Ownership is derived from the principal. |
| Library | Member-scoped Subsonic reads, string media IDs, stable Flow DTOs, and private artwork URLs are available. No admin fallback for member credentials. |
| Streaming | Flow auth → registration → authoritative recheck → member-scoped getSong → incremental stream. Existing tests cover cancellation, revocation, suspension races, Range, transcodes, bounded copying, and leakage. |
| Frontend | Library, search, album/artist views, playlists and an ephemeral M2 player exist. The audio path is Flow-only. M3 must replace local queue state and the track-keyed audio element. |
| Blocking defect | Vite uses 5174 while the documented public origin/CSRF configuration uses 5173. Align Vite to 5173 with strict port selection. |
| Live test fixture defect | The existing dev Compose mounts `/home/ruhin/Music`, while the contract tests generate fixtures under `dev/music-root/library`. A test-only Compose override mounts fixtures in isolated test projects; retain the operator's normal library mount. |
| Documentation | M2's extended live contract run is explicitly pending in ROADMAP/CHANGELOG, despite the supplied milestone completion label. ADR-007 resolves individual PUT/PATCH, units, transport intent, caching, and unspecified queue/checkpoint behavior before code changes. |

Baseline executed successfully: `go test ./...`, web `typecheck`, `lint`, and
`build`. Those tests do not prove live authentication/library/audio integration.
This environment denies Docker access, TCP socket creation and Unix socket binding, so a local
PostgreSQL 18.6 cluster initializes but cannot start. The exact pinned Navidrome
0.64.2 binary exists; live services are nevertheless unavailable. npm registry
resolution also fails. Live PostgreSQL and full browser/gateway/Navidrome gates
must be reported separately from deterministic tests; neither earlier milestone
labels nor integration test compilation count as a live pass.

The package and schema boundaries are ready for M3. Fix the port mismatch and
test fixture mount; retain existing security, library contracts and dev media.
