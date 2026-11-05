# Flow — Deployment

Version: 1.1.0-draft (supersedes 1.0.0-draft)
Target: single host (personal server, NAS, home lab). Docker Compose + Caddy. No k8s, no service mesh, no queues, no Redis.

Revisions: operator-assisted bootstrap, private gateway topology, canonical Caddy-served bundle, same-filesystem upload staging, explicit health/readiness, external key custody, and migration locking.

---

## 1. Topology Recap

```
host
├── caddy          (publishes 80, 443 — the ONLY public surface)
│   └── serves the built web bundle from its mounted `/srv/flow-web`
├── flow-backend   (internal only :8080)
├── navidrome      (internal only :4533 — NO published ports, NO Caddy route)
└── postgres       (internal only :5432)

host filesystem:
  music-root/library/       rw→flow-backend, ro→navidrome as /music
  music-root/.flow-staging/ rw→flow-backend only; same filesystem, outside watched mount
  nd_data/        rw→navidrome
  flow_pg/        rw→postgres
```

**Canonical V1 production shape:** Caddy serves the built web bundle directly and proxies `/api`, `/audio`, and `/ws` to the backend. There is no separate `flow-web` runtime service. Development uses Vite and a separate infra-only compose file (DEVELOPMENT_GUIDE §2).

## 2. Docker Compose (production target, delivered in M6)

M0 ships only `deploy/docker-compose.dev.yml` for PostgreSQL and Navidrome. Development publishes PostgreSQL on configurable loopback port `FLOW_DEV_POSTGRES_PORT` (default `55432`) so an operator's existing PostgreSQL on 5432 is not disturbed. The production Compose file and Caddyfile are M6 deliverables and must not be treated as runnable repository files before then. The following is the normative target shape for M6:

```yaml
services:
  caddy:
    image: caddy:2
    restart: unless-stopped
    ports: ["80:80", "443:443"]
    volumes:
      - caddy_data:/data
      - ./caddy/Caddyfile:/etc/caddy/Caddyfile:ro
      - ./web/dist:/srv/flow-web:ro        # shape A
    depends_on: [flow-backend, navidrome]

  flow-backend:
    image: registry.example/flow-backend:1.0.0
    restart: unless-stopped
    environment:
      FLOW_DATABASE_URL: "postgres://flow:${FLOW_DB_PASSWORD}@postgres:5432/flow?sslmode=disable"
      FLOW_NAVIDROME_URL: "http://navidrome:4533"
      FLOW_NAVIDROME_ADMIN_USERNAME: "${FLOW_NAVIDROME_ADMIN_USERNAME}"   # ADR-003
      FLOW_NAVIDROME_ADMIN_PASSWORD: "${FLOW_NAVIDROME_ADMIN_PASSWORD}"   # ADR-003
      FLOW_ADMIN_USERNAME: "${FLOW_ADMIN_USERNAME}"                       # Flow-side admin login
      FLOW_ADMIN_PASSWORD: "${FLOW_ADMIN_PASSWORD}"
      FLOW_CREDENTIAL_KEYRING_FILE: "/run/secrets/flow_credential_keyring"
      FLOW_PUBLIC_ORIGIN: "https://${FLOW_DOMAIN}"
      FLOW_SESSION_TTL: "8h"
      FLOW_PROVISION_TIMEOUT: "30s"
      FLOW_NAVIDROME_STREAM_HEADER_TIMEOUT: "10s" # bounds header wait, not the audio body
      FLOW_MUSIC_ROOT: "/music-root"
      FLOW_LOG_LEVEL: "info"
    volumes:
      - ./music-root:/music-root:rw
      - ./secrets/flow-credential-keyring.json:/run/secrets/flow_credential_keyring:ro
    depends_on:
      postgres:
        condition: service_healthy
      navidrome:
        condition: service_healthy
    # no ports published. none.

  navidrome:
    image: deluan/navidrome:0.64.2@sha256:38dc2727bfcfd5ede290f8ada114fc90368146f265ae4701ddddbcbe2a44ee52
    restart: unless-stopped
    environment:
      ND_MUSICFOLDER: "/music"
      ND_DATAFOLDER: "/data"
      ND_ENABLESHARING: "false"
      ND_ENABLEDOWNLOADS: "false"      # invariant — Navidrome default is ON
      ND_ENABLEARTWORKUPLOAD: "false"  # invariant — Navidrome default is ON
      ND_ENABLEEXTERNALSERVICES: "false"
      ND_DEVACTIVITYPANEL: "false"
      ND_DEVUISHOWCONFIG: "true"       # required for admin-only /api/config/config gate verification
      ND_AUTHREQUESTLIMIT: "60"
      ND_AUTHWINDOWLENGTH: "5m"
      # Operator-assisted first admin: expose Navidrome on loopback temporarily and use
      # its first-run UI/createAdmin flow. The dev-only auto-admin flag is not canonical production setup.
    volumes:
      - ./music-root/library:/music:ro
      - nd_data:/data
    healthcheck:
      test: ["CMD", "curl", "-fsS", "http://localhost:4533/ping"]
      interval: 10s
      timeout: 3s
      retries: 12
    # no ports published.

  postgres:
    image: postgres:16
    restart: unless-stopped
    environment:
      POSTGRES_DB: flow
      POSTGRES_USER: flow
      POSTGRES_PASSWORD: "${FLOW_DB_PASSWORD}"
    volumes: [flow_pg:/var/lib/postgresql/data]
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U flow -d flow"]
      interval: 5s
      timeout: 3s
      retries: 12
    # no ports published.

volumes: { caddy_data: {}, nd_data: {}, flow_pg: {} }
```

`.env` (host, `chmod 600`, **not committed**):
```
FLOW_ADMIN_USERNAME=admin
FLOW_ADMIN_PASSWORD=<change-me>          # Flow-side admin login
FLOW_NAVIDROME_ADMIN_USERNAME=admin       # Navidrome first admin (ADR-003)
FLOW_NAVIDROME_ADMIN_PASSWORD=<change-me>
FLOW_DB_PASSWORD=<change-me>
FLOW_DOMAIN=flow.example.com
```

`FLOW_CREDENTIAL_KEYRING_FILE` is the in-container path configured on `flow-backend`; it is not a host `.env` path to be dereferenced by Compose.

## 3. Caddyfile (M6 target)

```
:80 {
  redir https://{host}{uri}
}

{domain} {
  encode zstd gzip

  handle /api/* {
    reverse_proxy flow-backend:8080
  }
  handle /audio/* {
    # streaming gateway: unbuffered proxy, long-lived upstream, idle-keepalive
    reverse_proxy flow-backend:8080 {
      flush_interval -1
      transport http {
        keep_alive 30s
        pooling { max_idle_conns 100 }
      }
    }
  }
  handle /ws* {
    reverse_proxy flow-backend:8080 {
      flush_interval -1
      transport http {
        keep_alive 30s
        pooling { max_idle_conns 100 }
      }
    }
  }
  handle {
    root * /srv/flow-web
    file_server
  }
  header {
    Strict-Transport-Security "max-age=31536000; includeSubDomains; preload"
    Referrer-Policy "strict-origin-when-cross-origin"
    X-Content-Type-Options "nosniff"
    Content-Security-Policy "default-src 'self'; connect-src 'self' wss://@domain; img-src 'self' data:; style-src 'self' 'unsafe-inline'"
  }
  log {
    output file /data/access.log
    format json
  }
}
```

Caddy auto-provisions ACME for `{domain}`. Intranet-only: use `{$internal_domain}` with `tls internal` (CA in the browser trust store).

## 4. Startup Order and Bootstrap (ADR-003)

1. **Navidrome first-run (operator, once):** temporarily bind Navidrome to `127.0.0.1`, use its first-run UI (`POST /auth/createAdmin`, valid only while zero users exist), then remove the binding. `ND_DEVAUTOCREATEADMINPASSWORD` is source-verified at `v0.64.2` but is a development convenience, not the canonical production procedure.
2. **Operator supplies credentials to Flow:** set `FLOW_NAVIDROME_ADMIN_USERNAME` / `FLOW_NAVIDROME_ADMIN_PASSWORD` in `.env`.
3. **`postgres` up → `flow-backend` boots:** runs embedded migrations; then verifies the integration: login as the supplied Navidrome admin (JWT issued), admin user read-back, and config-gate read-back (`downloads/artwork/sharing` all off). M0 exposes this state inside `/healthz`; the authenticated `/api/admin/integration/health` view is added with the admin API.
4. **Bootstrap the Flow admin mapping (M1):** persist ciphertext + external key version, never key material. M0 stops after adapter verification and does not create browser auth/users.
5. If step 3 fails ⇒ the integration state is **degraded** and product mutations remain unavailable. In M0 the operator reads `/healthz` and logs; the later admin UI presents the same sanitized reason. Fix the secret/configuration and restart.

Caddy starts last (its `reverse_proxy` retries upstream errors by default).

## 5. Backups (from SECURITY.md §10, operational form)

```bash
# nightly (cron/host):
pg_dump -h postgres flow > backup/flow_pg_$(date +%F).sql
# navidrome (while stopped or via its backup command):
cp -a nd_data/ backup/nd_data_$(date +%F)/
music:     operator's existing snapshot scheme (btrfs/zfs/rsync)
keyring:   back up separately under independent access control
```

Restore order: music root → PostgreSQL → separately protected matching keyring → validate referenced key versions → boot. Navidrome: restore `nd_data` or re-scan from zero (index is derivable).

## 6. Observability

- Logs: JSON to stdout; Caddy's log block to file; `docker logs` + a log driver of choice (loki = out of scope V1).
- `GET /livez`: process event loop is alive; no dependency checks.
- `GET /readyz`: configuration loaded, migrations complete, and PostgreSQL reachable. Navidrome may be degraded while the operator performs first-run bootstrap.
- `GET /healthz`: detailed aggregate status; returns 503 when PostgreSQL or Navidrome is unavailable and reports the failing component without secret details.
- No Prometheus in V1 (Navidrome's `ND_PROMETHEUS` stays off).

## 7. Updating

- **App update**: pull new tag, `docker compose up -d` (migrations run automatically, forward-only, backward-compatible within a release — DEVELOPMENT_GUIDE §7).
- **Navidrome update**: pin the new tag, upgrade **one at a time**: update Navidrome → verify `/admin/integration/health` green from the backend → update Flow if API shapes changed (they are pinned per release; CI runs integration tests against the pinned Navidrome tag).
- **Credential rotation**: add the new version to the external keyring, activate its metadata, re-encrypt and verify all rows, then retire the old version subject to backup retention (DATABASE_SCHEMA §2.12).

## 8. Migration and process lifecycle

- Startup validates configuration, connects to PostgreSQL, acquires a dedicated PostgreSQL advisory lock, runs embedded migrations, releases the lock, then marks `/readyz` ready. A second process waits/fails on the lock rather than migrating concurrently.
- SIGTERM stops accepting new requests, marks readiness false, cancels background work, drains HTTP connections within the configured shutdown timeout, and closes database/Navidrome clients.
- A crash runs no graceful hooks. On the next boot, migrations remain ledgered/transactional and later sync milestones apply ADR-005 safe-end recovery.

## 9. Multi-Host (out of scope, documented so it's not accidental)

Single instance assumed (NFR-SCALE-001). If multi-instance ever needed: Postgres shared, sync engine's in-memory executor/event bus → Postgres LISTEN/NOTIFY (Redis is an alternative, not a V1 requirement), WS hub made stateless (no session affinity needed if events flow via DB). That is a V2 project, deliberately not designed against now — only the *absence of a blocking decision* (no single-process assumptions baked into API contracts).
