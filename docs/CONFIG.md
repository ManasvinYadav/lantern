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

To secure the write endpoints (`POST /api/status`, etc.), set one of the following:

| Variable | Description |
|---|---|
| `LANTERN_AUTH_TOKEN` | Bearer token authentication. Passed via `Authorization: Bearer <TOKEN>`. Highly recommended for API clients. |
| `LANTERN_AUTH_USER` | Basic auth username. Passed via standard HTTP Basic Auth. |
| `LANTERN_AUTH_PASS` | Basic auth password. Used in conjunction with `LANTERN_AUTH_USER`. |

*Note: The main dashboard `/` and `GET` requests remain completely public even if auth is enabled.*

## Webhooks
See the [Webhooks Setup Guide](WEBHOOKS.md) for details on `LANTERN_WEBHOOK_*` environment variables.
