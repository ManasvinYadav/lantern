# API Reference

Lantern operates primarily via a push-based heartbeat system. Below is a comprehensive list of all endpoints.

> **Note**: All POST endpoints are secured if `LANTERN_AUTH_TOKEN` is set. Pass the token as `Authorization: Bearer <TOKEN>`.

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
