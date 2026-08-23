# Lantern API Reference

Lantern features a clean JSON REST API. By default, all endpoints are public unless `LANTERN_AUTH_TOKEN` (Bearer) or `LANTERN_AUTH_USER`/`LANTERN_AUTH_PASS` (Basic) are set in the environment.

## Endpoints

### `GET /api/health`
Healthcheck endpoint.
**Response**: `{"status": "ok", "version": "0.3.0"}`

### `GET /api/services`
Retrieves the consolidated list of all known services, including 7-day and 30-day uptime calculations, and 30-day sparkline bucket history.
**Response**:
```json
[
  {
    "service_name": "api-gateway",
    "status": "up",
    "message": "Healthy",
    "timestamp": "2026-08-23T11:45:00Z",
    "last_seen": "2026-08-23T11:45:00Z",
    "stale": false,
    "maintenance": false,
    "uptime_7d": 99.9,
    "uptime_30d": 99.5,
    "uptime_percent": 99.5,
    "history": [
      { "start": "2026-07-24T00:00:00Z", "status": "up" }
    ]
  }
]
```

### `POST /api/status`
Pushes a heartbeat or status update.
**Payload**:
```json
{
  "service_name": "api-gateway",
  "status": "up",
  "message": "Operating normally",
  "maintenance": false
}
```

### `POST /api/diagnostics`
Pushes a diagnostic log or arbitrary JSON/text payload tied to a service.
**Payload**:
```json
{
  "service_name": "api-gateway",
  "title": "Daily DB Backup Log",
  "content": "Backup successful, 4050 rows processed."
}
```

### `GET /api/diagnostics`
Retrieves stored diagnostic runs, optionally filtered by `?service_name=...` and `?limit=50`.

### `GET /api/webhooks`
Retrieves the enablement status of the four supported notification channels (read from environment variables).
**Response**:
```json
{
  "discord": true,
  "telegram": false,
  "gotify": false,
  "generic": false
}
```

### `POST /api/webhooks/test`
Triggers a live test payload to one or all configured webhooks.
**Payload**:
```json
{
  "channel": "all" 
}
```
*Valid channels: `all`, `discord`, `telegram`, `gotify`, `generic`*
