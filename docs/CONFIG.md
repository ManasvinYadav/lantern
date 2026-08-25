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

## Native Docker Discovery

When the Docker socket is reachable, Lantern polls the daemon itself and records a
heartbeat for every container it finds. Containers are registered automatically the
first time they are seen — there is nothing to configure per service.

| Variable | Default | Description |
|---|---|---|
| `LANTERN_DOCKER_DISCOVERY` | `true` | Enables the container poller. Disabled only by the literal value `false`, so a typo leaves discovery working rather than silently off. |
| `LANTERN_DOCKER_POLL_SECONDS` | `60` | How often to poll the Docker daemon. Floored at `10` so a mistake cannot hammer the socket. |

Mount the socket to enable it:

```yaml
volumes:
  - /var/run/docker.sock:/var/run/docker.sock:ro
```

If the socket is absent or unreachable, Lantern logs one line at startup and discovery
stays inactive — it does not retry or error on every tick. Push-based heartbeats and
active monitors are unaffected.

> **Security note**: `:ro` applies to the *mount point*, not to the Docker API. The
> daemon still accepts write operations over that socket, so anything that can reach it
> can start, stop, and create containers. Treat socket access as equivalent to root on
> the host, and keep Lantern's admin routes authenticated.

### Opting a container out

Add a label to any container that should not appear on the dashboard:

```yaml
labels:
  lantern.ignore: "true"
```

The value is compared case-insensitively and surrounding whitespace is ignored.

### Status mapping

One `GET /containers/json` call covers every container, so discovery costs one request
per interval regardless of how many containers the host runs. Each container's Docker
state and healthcheck are mapped as follows:

| Docker state | Healthcheck | Lantern status |
|---|---|---|
| `running` | `healthy`, or no healthcheck declared | `up` |
| `running` | `health: starting` | `degraded` |
| `running` | `unhealthy` | `degraded` |
| `restarting`, `paused` | — | `degraded` |
| `exited`, `dead`, `created`, `removing` | — | `down` |
| anything else | — | `unknown` |

A container that is running but whose healthcheck has not yet passed reports `degraded`
rather than `up`, so a service that is still booting never shows a green card. A
container that declares no healthcheck at all is taken at its word.

The Docker status line is carried through verbatim as the beat message, so cards read
`Up 4 days (healthy)` or `Exited (0) 4 hours ago`.

### Monitoring things Docker cannot see

Lantern runs in a container and cannot inspect the host's systemd units, remote
machines, or scheduled jobs. Push those with `POST /api/status` (optionally including a
measured `latency_ms`), or configure an active HTTP/TCP/ping monitor.

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
`/api/docs`, `/api/badge/*`, and `/metrics`, which are always open. The admin
dashboard's Settings drawer has an "Admin API Token" field that stores your token in
that browser and attaches it automatically to admin actions.

Per-service scoped tokens (issued into the `api_tokens` table) can push
status and use Docker controls for their own service name only, and are
rejected with `403` if used against a different service.

*Note: `/api/public/*`, `/api/health`, `/api/docs`, `/api/badge/*`, and `/metrics` are
always open, regardless of auth configuration. Status badges are intentionally
anonymous so they render when embedded in a README.*

## Observability

| Endpoint | Description |
|---|---|
| `GET /metrics` | Prometheus text-format metrics: `lantern_service_status`, `lantern_service_uptime_ratio{range="7d"\|"30d"}`, `lantern_incident_count`. Always unauthenticated (same data `/api/public/services` already exposes). |
| `GET /api/docs` | A reference page listing every route, method, and example payload. |
| `GET /api/badge/{service}.svg` | Embeddable SVG status badge. Always unauthenticated. |

## Webhooks

See the [Webhooks Setup Guide](WEBHOOKS.md) for details on `LANTERN_WEBHOOK_*` environment variables.

### Alert flap dampening

Outage notifications are dampened so a single bad check does not page anyone:

- A `down` notification fires only once **two consecutive** checks report `down`.
- It fires **once per outage**, not once per failing check.
- A recovery notification fires only if the outage that preceded it was actually
  announced. A single-beat flap therefore produces no traffic at all, rather than a
  down alert followed immediately by a recovery.
- Transitions that do not involve `down` (for example `up` → `degraded`) still notify
  immediately.

Dampening counts *checks*, not time, so the delay before an alert depends on your
polling interval — with the default 60s Docker poll, a confirmed outage notifies about
a minute after the first failure.
