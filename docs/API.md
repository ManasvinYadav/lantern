# API Reference

Lantern operates primarily via a push-based heartbeat system, with optional active monitoring as an alternative. Below is a comprehensive list of endpoints. See also `GET /api/docs`, a live reference page generated from the actual route table.

> **Note**: Once `LANTERN_AUTH_TOKEN` or `LANTERN_AUTH_USER`/`LANTERN_AUTH_PASS` is set, mutating and administrative routes require auth — not just `POST` — including both `GET` endpoints under `/api/services/{name}/docker/*`, since they expose container internals and log content. Pass the token as `Authorization: Bearer <TOKEN>`. See [Configuration Guide](CONFIG.md#security--authentication) for the exact route list.

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
    "message": "Serving requests normally"
  }'
```

**Parameters:**
- `service_name` (string, required): The unique identifier for your service.
- `status` (string, required): `up`, `down`, or `degraded`.
- `message` (string, optional): A short description of the current status.
- `maintenance` (boolean, optional): If `true`, suppresses alerts and marks the service under maintenance.

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
    "message": "Serving requests normally",
    "timestamp": "2026-08-23T10:00:00Z",
    "last_seen": "2026-08-23T10:00:00Z",
    "stale": false,
    "maintenance": false,
    "uptime_7d": 100.0,
    "uptime_30d": 99.8,
    "uptime_percent": 99.8,
    "history": [
      { "start": "2026-08-22T00:00:00Z", "status": "up" }
    ]
  }
]
```

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

## `GET /api/webhooks`
Returns the status of configured notification integrations.

**Request:**
```bash
curl -s http://localhost:7654/api/webhooks
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
