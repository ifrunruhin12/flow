# Flow — Deployment

Version: 1.0.0-draft
Target: single host (personal server, NAS, home lab). Docker Compose + Caddy. No k8s, no service mesh, no queues.

---

## 1. Topology Recap

```
host
├── caddy          (publishes 80, 443)
├── flow-web       (internal only; static assets — OR built artifacts served by caddy; see §3)
├── flow-backend   (internal only :8080)
├── navidrome      (internal only :4533)
└── postgres       (internal only :5432)

volumes:
  music/    rw→flow-backend, ro→navidrome     (audio files — THE asset)
  nd_data/  rw→navidrome
  flow_pg/  rw→postgres
```

Two deployment shapes for the web app, pick one per environment:

- **A (default, V1): Caddy serves the built web bundle.** `npm run build` → static files copied into the Caddy container (or a `flow-web` static container). One public surface: Caddy.
- **B: Caddy serves `flow-web` container directly.** Same behavior, one more container.

Both keep the API/WS origin identical (`/api`, `/ws`), so the SPA needs zero per-environment config for origins.

## 2. Docker Compose (production)

`deploy/docker-compose.yml` (canonical content lives in the repo; summarized here):

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
      FLOW_DSN: "postgres://flow:${FLOW_DB_PASSWORD}@postgres:5432/flow?sslmode=disable"
      FLOW_NAVIDROME_URL: "http://navidrome:4533"
      FLOW_ADMIN_USERNAME: "${FLOW_ADMIN_USERNAME}"
      FLOW_ADMIN_PASSWORD: "${FLOW_ADMIN_PASSWORD}"
      FLOW_NAVIDROME_KEY: "${FLOW_NAVIDROME_KEY}"
      FLOW_MUSIC_FOLDER: "/music"
      FLOW_LOG_LEVEL: "info"
    volumes:
      - ./music:/music:rw
    depends_on:
      postgres:
        condition: service_healthy
      navidrome:
        condition: service_healthy
    # no ports published. none.

  navidrome:
    image: deluan/navidrome:<pinned-tag>   # pin to a tested tag, not :latest (U-001)
    restart: unless-stopped
    environment:
      ND_MUSICFOLDER: "/music"
      ND_DATAFOLDER: "/data"
      ND_ENABLESHARING: "false"
      ND_ENABLEDOWNLOADS: "false"
      ND_ENABLEARTWORKUPLOAD: "false"
      ND_ENABLEEXTERNALSERVICES: "false"
      ND_DEVACTIVITYPANEL: "false"
      ND_AUTHREQUESTLIMIT: "60"
      ND_AUTHWINDOWLENGTH: "5m"
    volumes:
      - ./music:/music:ro
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
FLOW_ADMIN_PASSWORD=<change-me>
FLOW_DB_PASSWORD=<change-me>
FLOW_NAVIDROME_KEY=<openssl rand -hex 32>
```

## 3. Caddyfile

```
:80 {
  redir https://{host}{uri}
}

{domain} {
  encode zstd gzip

  handle /api/* {
    reverse_proxy flow-backend:8080
  }
  handle /ws* {
    reverse_proxy flow-backend:8080 {
      flush_interval -1
      transport http  {
        keep_alive 30s
        pooling { max_idle_conns 100 }
      }
    }
  }
  handle {
    root * /srv/flow-web
    file_server
    @assets { path * }
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

Caddy auto-provisions ACME for `{domain}`. Self-hosted behind a VPS with a domain: works out of the box. Intranet-only: use `{$internal_domain}` with `tls internal` (CA in browser trust store).

## 4. Startup Order and Bootstrap

1. `postgres` up → `flow-backend` runs embedded migrations (idempotent, `migrations` ledger).
2. Backend bootstrap (once, guarded by DB state): no `users` row for `FLOW_ADMIN_USERNAME` → create admin row + `POST /api/user` to Navidrome (admin, `isAdmin:true`) + store encrypted `P_admin`. Navidrome must be up (health-probed with backoff; compose `depends_on: condition: service_healthy` where Navidrome exposes a healthcheck via `GET /rest/ping` — add the healthcheck to the navidrome service).
3. Caddy up last (or with a retrying `reverse_proxy` — Caddy's default is retry-on-error).

## 5. Backups (from SECURITY.md §10, operational form)

```bash
# nightly (cron/host):
pg_dump -h postgres flow > backup/flow_pg_$(date +%F).sql.gz
ndbackup:  docker exec navidrome nd backup --data /data  # navidrome ships a backup command
music:     operator's existing snapshot scheme (btrfs/zfs/rsync)
key:       FLOW_NAVIDROME_KEY kept with the pg backup (encrypted at rest)
```

Restore order: music volume (never lost) → postgres (restore dump, run migrations) → key → boot. Navidrome: restore `nd_data` or re-scan from zero (index is derivable).

## 6. Observability

- Logs: JSON to stdout; Caddy's log block to file; `docker logs` + a log driver of choice (loki = out of scope V1).
- Health: `GET /healthz` on flow-backend (DB + Navidrome probe, 200/503) — used by compose healthchecks and by a simple status page route if desired (V1.x).
- No Prometheus in V1 (Navidrome's `ND_PROMETETHEUS` stays off).

## 7. Updating

- **App update**: pull new tag, `docker compose up -d` (migrations run automatically, backward-compatible within a release — see DEVELOPMENT_GUIDE versioning).
- **Navidrome update**: pin the tag, upgrade one at a time: update Navidrome → verify `GET /api/config` reachable from backend → update Flow if API shapes changed (they are pinned per release; CI runs integration tests against the pinned Navidrome tag).

## 8. Multi-Host (out of scope, documented so it's not accidental)

Single instance assumed (NFR-SCALE-001). If multi-instance ever needed: Postgres shared, sync engine's in-memory event bus → Postgres LISTEN/NOTIFY or Redis, WS hub made stateless (session affinity by `shared_session_id` not required if events flow via DB). That is a V2 project, deliberately not designed against now — only the *absence of a blocking decision* (no single-process assumptions baked into API contracts).
