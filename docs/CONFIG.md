# Configuration Guide

Lantern relies entirely on Environment Variables.

## Core Settings

| Variable | Default | Description |
|---|---|---|
| `LANTERN_PORT` | `7654` | The port the HTTP server binds to. |
| `LANTERN_DB_PATH` | `/data/lantern.db` | Absolute or relative path to the SQLite DB file. |
| `LANTERN_RETENTION_DAYS` | `30` | The number of days to keep status records before pruning them. |

## Automation & Health

| Variable | Default | Description |
|---|---|---|
| `LANTERN_STALE_HOURS` | `24` | If a service does not send a heartbeat within this window, Lantern marks it as `stale` (Down). |

## Security / Authentication

To secure the write and administrative endpoints, set one of the following:

| Variable | Description |
|---|---|
| `LANTERN_AUTH_TOKEN` | Admin bearer token. Passed via `Authorization: Bearer <TOKEN>`. Highly recommended for API clients pushing status. |
| `LANTERN_AUTH_USER` | Basic auth username. Passed via standard HTTP Basic Auth. |
| `LANTERN_AUTH_PASS` | Basic auth password. Used in conjunction with `LANTERN_AUTH_USER`. |

Setting `LANTERN_AUTH_TOKEN` alone requires authentication on mutating and
administrative routes specifically — `POST /api/status`, diagnostics,
webhook config/test, group/maintenance/monitor writes, and every
`/api/services/{name}/docker/*` route (including its two `GET` endpoints,
since they expose container internals and log content). General dashboard
reads (`/api/services`, `/api/groups`, uptime/strip/incidents, webhook
config `GET`) stay open. Setting `LANTERN_AUTH_USER`/`LANTERN_AUTH_PASS`
(Basic Auth) instead gates the entire app except `/api/public/*`, `/api/health`,
`/api/docs`, and `/metrics`, which are always open. The admin dashboard's
Settings drawer has an "Admin API Token" field that stores your token in
that browser and attaches it automatically to admin actions.

Per-service scoped tokens (issued into the `api_tokens` table) can push
status and use Docker controls for their own service name only, and are
rejected with `403` if used against a different service.

*Note: `/api/public/*`, `/api/health`, `/api/docs`, and `/metrics` are
always open, regardless of auth configuration.*

## Observability

| Endpoint | Description |
|---|---|
| `GET /metrics` | Prometheus text-format metrics: `lantern_service_status`, `lantern_service_uptime_ratio{range="7d"\|"30d"}`, `lantern_incident_count`. Always unauthenticated (same data `/api/public/services` already exposes). |
| `GET /api/docs` | A reference page listing every route, method, and example payload. |

## Webhooks
See the [Webhooks Setup Guide](WEBHOOKS.md) for details on `LANTERN_WEBHOOK_*` environment variables.
