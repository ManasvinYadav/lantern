# API Reference

Lantern discovers local Docker containers natively, and additionally accepts push-based
heartbeats and runs optional active monitors. All three write to the same
`status_events` table, so history, uptime, and incidents behave identically regardless
of where a check came from. Below is a comprehensive list of endpoints. See also
`GET /api/docs`, a live reference page generated from the actual route table.

> **Note**: Once `LANTERN_AUTH_TOKEN` or `LANTERN_AUTH_USER`/`LANTERN_AUTH_PASS` is set, mutating and administrative routes require auth — not just `POST` — including both `GET` endpoints under `/api/services/{name}/docker/*`, since they expose container internals and log content. Pass the token as `Authorization: Bearer <TOKEN>`. `/api/public/*`, `/api/health`, `/api/docs`, `/api/badge/*`, and `/metrics` are always open. See [Configuration Guide](CONFIG.md#security--authentication) for the exact route list.

---

## `POST /api/status`
Reports the health of a service.

**Request:**
```bash
curl -X POST http://localhost:7654/api/status \
  -H "Authorization: Bearer your_token_here" \
  -H "Content-Type: application/json" \
  -d '{
    "service_name": "nginx",
    "status": "up",
    "message": "Serving requests normally",
    "latency_ms": 42
  }'
```

**Parameters:**
- `service_name` (string, required): The unique identifier for your service.
- `status` (string, required): `up`, `down`, or `degraded`.
- `message` (string, optional): A short description of the current status.
- `latency_ms` (integer, optional): How long your own check took, in milliseconds. Omitted, `null`, or negative values are recorded as `0`.
- `timestamp` (string, optional): RFC 3339. Defaults to now.
- `group_name` (string, optional): Assigns the service to a dashboard group.
- `maintenance` (boolean, optional): If `true`, suppresses alerts and marks the service under maintenance.

**Response:**
```json
{ "id": 71106 }
```

---

## `GET /api/services`
Fetches the consolidated dashboard payload for all services.

**Request:**
```bash
curl -s http://localhost:7654/api/services
```

**Response:**
```json
[
  {
    "service_name": "nginx",
    "status": "up",
    "message": "Up 4 days (healthy)",
    "timestamp": "2026-08-25T18:23:49Z",
    "last_seen": "2026-08-25T18:23:49Z",
    "stale": false,
    "maintenance": false,
    "group_name": "",
    "monitor_type": "",
    "uptime_7d": 100.0,
    "uptime_30d": 99.8,
    "uptime_percent": 96.7,
    "history": [
      {
        "status": "up",
        "timestamp": "2026-08-25T18:22:49Z",
        "msg": "Up 4 days (healthy)",
        "latency_ms": 173
      }
    ]
  }
]
```

**Field notes:**
- `history` is the **live heartbeat window**: the last 30 individual checks, oldest
  first. A service with fewer than 30 recorded checks is left-padded with placeholder
  beats of `{"status": "empty"}` so the array is always exactly 30 entries long.
- `latency_ms` is how long that specific check took. Beats produced by the Docker
  poller share one measurement per polling cycle — it is the cost of the daemon query
  for that batch, not a per-container probe time.
- `uptime_percent` is the ratio across the heartbeat window: `up ÷ non-empty beats`.
  Padding beats are excluded from both sides, so a service with 3 recorded checks is
  scored out of 3, not out of 30. This is **not** a 30-day figure — use `uptime_30d`
  for that.
- `uptime_7d` and `uptime_30d` remain time-window averages over the retained history.
- `monitor_type` is `""` for discovered or pushed services, or `http`/`tcp`/`ping` when
  an active monitor is configured.

---

## `GET /api/badge/{service}.svg`
Renders an embeddable shields-style SVG badge for a service's current status. Always
unauthenticated, so it works when embedded in a README. Responds to `GET` and `HEAD`.

**Request:**
```bash
curl -s http://localhost:7654/api/badge/nginx.svg
```

**Response headers:**
```
Content-Type: image/svg+xml; charset=utf-8
Cache-Control: no-cache, no-store, must-revalidate
```

**Colors:**

| Status | Color |
|---|---|
| `up` | `#10B981` |
| `down` | `#F43F5E` |
| `degraded` | `#F59E0B` |
| `maintenance`, `unknown`, unrecognised | `#6B7280` |

A service under maintenance reports `maintenance` regardless of its last recorded
check. An unknown service name renders an `unknown` badge rather than a 404, so a
badge never appears broken.

**Embedding:**
```markdown
![status](http://localhost:7654/api/badge/nginx.svg)
```

---

## `GET /api/services/{name}/history`
Returns the retained status events for a service, newest first.

**Query Parameters:**
- `limit`: Maximum number of records. Defaults to `100`, capped at `500`.

---

## `POST /api/diagnostics`
Pushes diagnostic logs or text payloads for debugging.

**Request:**
```bash
curl -X POST http://localhost:7654/api/diagnostics \
  -H "Authorization: Bearer your_token_here" \
  -H "Content-Type: application/json" \
  -d '{
    "service_name": "nginx",
    "title": "Access Log Snapshot",
    "content": "..."
  }'
```

---

## `GET /api/diagnostics`
Retrieves recent diagnostic logs.

**Query Parameters:**
- `service_name`: Filter by service.
- `limit`: Maximum number of records (default: 50).

**Request:**
```bash
curl -s "http://localhost:7654/api/diagnostics?limit=5"
```

---

## `GET /api/monitors`
Lists configured active monitors.

## `PUT /api/services/{name}/monitor`
Creates or updates an active monitor for a service.

**Request:**
```bash
curl -X PUT http://localhost:7654/api/services/nginx/monitor \
  -H "Authorization: Bearer your_token_here" \
  -H "Content-Type: application/json" \
  -d '{
    "monitor_type": "http",
    "target": "https://example.com/healthz",
    "interval_seconds": 60,
    "enabled": true
  }'
```

*(Valid `monitor_type` values: `http`, `tcp`, `ping`)*

## `DELETE /api/services/{name}/monitor`
Removes a service's active monitor.

---

## `GET /api/webhooks`
Returns the status of configured notification integrations.

**Request:**
```bash
curl -s http://localhost:7654/api/webhooks
```

---

## `PUT /api/webhooks`
Saves a webhook URL for a channel. An empty `url` deletes the configuration.

**Request:**
```bash
curl -X PUT http://localhost:7654/api/webhooks \
  -H "Authorization: Bearer your_token_here" \
  -H "Content-Type: application/json" \
  -d '{"channel": "discord", "url": "https://discord.com/api/webhooks/..."}'
```

---

## `POST /api/webhooks/test`
Dispatches a test notification to configured webhooks.

**Request:**
```bash
curl -X POST http://localhost:7654/api/webhooks/test \
  -H "Authorization: Bearer your_token_here" \
  -H "Content-Type: application/json" \
  -d '{"channel": "all"}'
```
*(Valid channels: `all`, `discord`, `telegram`, `gotify`, `generic`)*

---

## `GET /api/webhooks/deliveries`
Returns the webhook delivery log — every attempt, successful or failed, with the HTTP
status and error text. Useful for confirming whether an alert actually went out.

---

## `GET /metrics`
Prometheus text-exposition-format metrics. Always unauthenticated (exposes the same data as `/api/public/services`).

**Request:**
```bash
curl -s http://localhost:7654/metrics
```

**Metrics exported:**
- `lantern_service_status{service,group}` — `1` if up, else `0`.
- `lantern_service_uptime_ratio{service,range="7d"|"30d"}` — 0–1.
- `lantern_incident_count{service}` — distinct down/degraded incidents in the last 30 days.

---

## `GET /api/backup`
Downloads a consistent database snapshot (`VACUUM INTO`), safe to fetch even under concurrent writes. See [Backup & Restore](BACKUP.md).

---

## `GET /api/services/{name}/export?format=csv|json`
Downloads a service's full retained status history.

---

## WebSocket: `GET /ws`

Streams live updates. Two message types are emitted per event, in this order:

**1. `heartbeat`** — a lightweight per-check delta, so the dashboard can slide one new
beat into the heartbeat bar without re-parsing a full payload:

```json
{
  "type": "heartbeat",
  "service_name": "nginx",
  "status": "up",
  "timestamp": "2026-08-25T18:27:39Z",
  "uptime_pct": 100,
  "new_beat": {
    "status": "up",
    "timestamp": "2026-08-25T18:27:39Z",
    "msg": "Up 4 days (healthy)",
    "latency_ms": 173
  }
}
```

**2. `status_update`** — the same shape as one `GET /api/services` entry, under a
`service` key.

The ordering is deliberate: `heartbeat` arrives first so the client can update the bar
incrementally before the fuller snapshot lands. Clients whose send buffer is full have
frames dropped rather than blocking ingestion, and recover on the next update.

`GET /api/public/ws` is the unauthenticated equivalent for public status pages.
