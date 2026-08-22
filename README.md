# 🔆 Lantern

> A lightweight, self-hostable service health dashboard for your homelab. Push status events from any service, view them in a clean dark UI, and store diagnostic runs for post-mortem review.

![Screenshot placeholder — run Lantern and visit http://localhost:7654](docs/screenshot.png)

---

## Features

- **Push-based status ingestion** — services POST their own health events; no polling agents required
- **Per-service event history** — full chronological timeline of every status change
- **Diagnostic runs** — store structured output (logs, test results, diffs) alongside status events
- **Auto-retention cleanup** — old records are pruned automatically (configurable window)
- **Pure Go + SQLite** — single binary, no external runtime dependencies, no CGO required
- **Polished dark UI** — responsive card grid, slide-in detail drawer, auto-refreshes every 30 s
- **Optional Basic Auth** — lock it down with a username and password
- **Docker-first** — multi-stage build produces a minimal Alpine image under 20 MB

---

## Quickstart

```bash
# Clone and start with Docker Compose
git clone https://github.com/yourusername/lantern.git
cd lantern
docker compose up -d

# Visit the dashboard
open http://localhost:7654
```

That's it. Data is stored in a named Docker volume (`lantern_data`) and persists across restarts.

---

## Sending Status Updates

Push status events to Lantern from any service, script, or monitoring tool using plain `curl`.

```bash
# Service is healthy
curl -s -X POST http://localhost:7654/api/status \
  -H 'Content-Type: application/json' \
  -d '{"service_name":"nginx","status":"up","message":"All workers healthy"}'

# Service is down
curl -s -X POST http://localhost:7654/api/status \
  -H 'Content-Type: application/json' \
  -d '{"service_name":"postgres","status":"down","message":"Connection refused on port 5432"}'

# Service is degraded (partial outage)
curl -s -X POST http://localhost:7654/api/status \
  -H 'Content-Type: application/json' \
  -d '{"service_name":"redis","status":"degraded","message":"High memory usage: 94%"}'

# With an explicit timestamp (RFC 3339)
curl -s -X POST http://localhost:7654/api/status \
  -H 'Content-Type: application/json' \
  -d '{"service_name":"nginx","status":"up","timestamp":"2025-01-15T08:30:00Z"}'
```

Valid `status` values: `up` · `down` · `degraded` · `unknown`

---

## Logging a Diagnostic Run

Attach arbitrary long-form text (logs, test output, changelogs) to a service:

```bash
curl -s -X POST http://localhost:7654/api/diagnostics \
  -H 'Content-Type: application/json' \
  -d '{
    "service_name": "postgres",
    "title":        "pg_dump integrity check — 2025-01-15",
    "content":      "Running pg_dump...\nRows: 1,042,891\nDuration: 4m 12s\nChecksum OK",
    "timestamp":    "2025-01-15T09:00:00Z"
  }'
```

Diagnostic runs appear in the **Diagnostic Runs** section of the dashboard. Click any row to expand and read the full content inline.

---

## Configuration

All configuration is done via environment variables. All settings have sensible defaults.

| Variable | Default | Description |
|---|---|---|
| `LANTERN_PORT` | `7654` | TCP port the HTTP server listens on |
| `LANTERN_DB_PATH` | `/data/lantern.db` | Path to the SQLite database file |
| `LANTERN_RETENTION_DAYS` | `30` | Number of days to retain events and diagnostics |
| `LANTERN_AUTH_USER` | _(unset)_ | Basic Auth username — leave unset to disable auth |
| `LANTERN_AUTH_PASS` | _(unset)_ | Basic Auth password |

Set variables in `docker-compose.yml`, in a `.env` file, or pass them directly to `docker run`.

---

## API Reference

All endpoints return JSON. All timestamps are RFC 3339 (UTC).

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/health` | Liveness check — returns `{"status":"ok","version":"0.1.0"}` |
| `POST` | `/api/status` | Ingest a status event for a service |
| `GET` | `/api/services` | List all services with their current (latest) status |
| `GET` | `/api/services/{name}/history` | Paginated event history for a service (`?limit=&offset=`) |
| `POST` | `/api/diagnostics` | Store a diagnostic run |
| `GET` | `/api/diagnostics` | List diagnostic runs — summary only (`?service_name=&limit=&offset=`) |
| `GET` | `/api/diagnostics/{id}` | Fetch a single diagnostic run including full content |

### POST /api/status — Request Body

```json
{
  "service_name": "string (required)",
  "status":       "up | down | degraded | unknown (required)",
  "message":      "string (optional)",
  "timestamp":    "RFC 3339 (optional, defaults to server time)"
}
```

### POST /api/diagnostics — Request Body

```json
{
  "service_name": "string (required)",
  "title":        "string (required)",
  "content":      "string (required)",
  "timestamp":    "RFC 3339 (optional, defaults to server time)"
}
```

---

## Security

> [!WARNING]
> **Do not expose Lantern directly to the public internet without authentication.** The API has no rate-limiting and accepts writes from any client.

For homelab use behind a firewall or VPN, no auth is necessary. To enable HTTP Basic Auth:

```yaml
# docker-compose.yml
environment:
  - LANTERN_AUTH_USER=admin
  - LANTERN_AUTH_PASS=a-very-strong-password
```

For public exposure, it is strongly recommended to put Lantern behind a reverse proxy (e.g. Caddy, Nginx, Traefik) with TLS termination.

---

## Development

### Prerequisites

- Go 1.22+
- No C compiler required (`modernc.org/sqlite` is pure Go)

### Run locally

```bash
git clone https://github.com/yourusername/lantern.git
cd lantern

# Use a local DB path for development
export LANTERN_DB_PATH=./lantern-dev.db
export LANTERN_PORT=7654

go run .
# → Lantern v0.1.0 listening on :7654
```

Open `http://localhost:7654` in your browser.

### Build binary

```bash
go build -o lantern .
./lantern
```

### Build Docker image

```bash
docker build -t lantern:latest .
docker run -p 7654:7654 -v lantern_data:/data lantern:latest
```

### Project structure

```
lantern/
├── main.go              # HTTP server, API handlers, DB logic
├── static/
│   └── index.html       # Self-contained SPA (no external deps)
├── Dockerfile           # Multi-stage build
├── docker-compose.yml
├── go.mod
├── go.sum
└── README.md
```

---

## Contributing

Contributions are welcome! Please open an issue first to discuss significant changes.

1. Fork the repository
2. Create a feature branch (`git checkout -b feat/my-feature`)
3. Commit your changes (`git commit -m 'feat: add my feature'`)
4. Push to the branch and open a Pull Request

---

## License

MIT — see [LICENSE](LICENSE) for details.

Copyright © 2025 Lantern Contributors
