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
| `LANTERN_AUTH_USER` | Admin username. Seeds the credential store on first boot; also accepted as HTTP Basic Auth. |
| `LANTERN_AUTH_PASS` | Admin password. Used in conjunction with `LANTERN_AUTH_USER`. |

### Signing in

Admin credentials live in the database, not in the environment. Set
`LANTERN_AUTH_USER` and `LANTERN_AUTH_PASS` and Lantern hashes them with
bcrypt into `admin_credentials` **on first boot only** — after that the stored
row is authoritative, so a password you change in the dashboard is not
reverted by a stale environment variable on the next restart. You can also
skip the environment entirely and set credentials from **Settings → Account &
Security**, which is how you turn sign-in on for a dashboard that is currently
open.

With credentials set, signing in issues an `HttpOnly`, `SameSite=Strict`
session cookie. The cookie is what authenticates the `/ws` live feed: a
browser's `WebSocket` constructor cannot send an `Authorization` header, so
header-based auth alone leaves the dashboard falling back to polling. Failed
logins are rate limited per client address.

Change your username or password at **Settings → Account & Security**. The
current password is required and verified; a wrong one returns `401`. A
successful change revokes every session and issues a fresh one to the browser
that made the change, so other devices are signed out and yours is not. New
passwords must be at least 8 characters.

If `LANTERN_AUTH_TOKEN` is set, turning sign-in on for the first time requires
that token. Setup mode issues an admin session to a caller who has proved
nothing, which is only defensible when the dashboard was already wide open — on
a token-configured deployment it would be a privilege escalation.

### What each mode gates

With **admin credentials set**, everything requires a session except the
always-open surface below.

With **`LANTERN_AUTH_TOKEN` alone**, only mutating and administrative routes
require the token. Those are:

- `POST /api/status` and `POST /api/diagnostics`
- `GET`, `PUT` and `POST /api/webhooks`, and `POST /api/webhooks/test`
- `GET /api/backup`
- `PUT /api/auth/credentials`
- every `/api/services/{name}/docker/*` route, including its two `GET`
  endpoints, since they expose container internals and log content
- non-`GET` writes to any `/group`, `/maintenance` or `/monitor` path
- `POST` to any `/check` path
- `DELETE /api/services/{name}`

General dashboard reads stay open, and no login wall appears — there is no
password to type.

> Three of those are `GET` routes, which is easy to miss when reasoning about
> "reads are open". `GET /api/backup` returns the entire database, and
> `GET /api/webhooks` returns webhook URLs in full — a Discord webhook URL or a
> Telegram bot URL *is* the credential. Both were reachable anonymously in this
> mode before v0.60.0.

With **nothing set**, Lantern is fully open. That is the out-of-the-box
behavior and it is preserved deliberately: adding the login gate must not lock
an existing deployment out of itself.

Per-service scoped tokens (issued into the `api_tokens` table) can push status
and use Docker controls for their own service name only, and are rejected with
`403` if used against a different service. Both token types keep working under
the login gate, so scripts, n8n and CI need no changes.

### Always open

Reachable without any credential, regardless of configuration:

- `/status` and the static SPA shell
- `GET /api/public/services`
- `GET /api/public/groups`
- `GET /api/public/services/{name}/uptime`
- `GET /api/public/ws`
- `GET` and `HEAD /api/badge/{service}.svg`
- `GET /metrics`
- `GET /api/health`
- `GET /api/docs`
- `GET /api/auth/session` and `POST /api/auth/login`

The public API is enumerated route by route rather than as a blanket
`/api/public/*` because that prefix is exempt by pattern: anything registered
under it is anonymous by construction, so the list is the security boundary.
`GET /api/public/services/{name}/metadata` was removed in v0.60.0 — it returned
the container image, its IP, its published ports and its host filesystem mount
paths to anyone who asked. The gated `GET /api/services/{name}/metadata` still
serves that to authenticated callers.

The shell is served so the login form has somewhere to render; it carries no
service data of its own, which all arrives over the gated `/api` routes. This
is also what keeps `/status` public once you enable sign-in. `/api/health` is
what the container `HEALTHCHECK` polls, with no credentials — gating it would
mark the container unhealthy the moment auth was switched on. Status badges are
intentionally anonymous so they render when embedded in a README.

### The public live feed

`/ws` and `/api/public/ws` are backed by **separate hubs**. The gated socket
carries the full `status_update` payload plus per-check `heartbeat` deltas; the
public one carries a reduced `status_update` only, with no heartbeat frames and
no check history.

This matters because before v0.60.0 both paths were registered to the same hub.
`/ws` was gated and `/api/public/ws` was not, so the gate achieved nothing — an
anonymous client could open the public path and receive byte-identical
broadcasts. Enabling sign-in now genuinely restricts the live feed.

### Scoped API tokens at rest

Per-service tokens in `api_tokens` are matched by SHA-256 hash. A row still
holding a plaintext token is upgraded in place the first time that token is
used, so the existing "insert a row with `sqlite3`" workflow keeps working while
the plaintext drains out of the table.

The upgrade is lazy, which is the honest caveat: a token that is never used
again stays in plaintext. If you have issued tokens you no longer use, delete
the rows rather than leaving them.

### Origin and framing controls

| Variable | Default | Description |
|---|---|---|
| `LANTERN_WS_ALLOWED_ORIGINS` | *(empty)* | Comma-separated origins allowed to open a WebSocket, in addition to the dashboard's own host. Empty means same-host only. |
| `LANTERN_FRAME_ANCESTORS` | `'self'` | The CSP `frame-ancestors` source list. Set this to embed the dashboard in an iframe on another origin. |

WebSocket handshakes are accepted from the dashboard's own host, from anything
listed in `LANTERN_WS_ALLOWED_ORIGINS`, and from clients that send no `Origin`
header at all — browsers always send one, so an absent header means a script or
a CLI rather than a cross-site page. Before v0.61.0 every origin was accepted,
which let any website a user visited open a socket to their Lantern and read the
live service feed.

Framing follows the same shape. The dashboard sets `frame-ancestors 'self'` and
`X-Frame-Options: SAMEORIGIN`, so it can be embedded on its own origin but not
from another one. If you surface Lantern inside a homepage app served from a
different port, name that origin explicitly:

```yaml
- LANTERN_FRAME_ANCESTORS=http://homepage.local:3000
```

Setting it to anything other than the default drops the `X-Frame-Options`
header, which cannot express an allowlist.

### Hardening notes

The HTTP server sets `ReadHeaderTimeout` (10s), `ReadTimeout` (30s) and
`IdleTimeout` (120s), so a client cannot hold a connection open indefinitely
part-way through a request. `WriteTimeout` is deliberately unset: `GET
/api/backup` streams the whole database and can legitimately run long on a slow
link. `SIGTERM` and `SIGINT` drain in-flight requests for up to 15 seconds
before the process exits, so a `docker compose down` no longer cuts a SQLite
write mid-flight.

Every response also carries `Content-Security-Policy`, `X-Content-Type-Options:
nosniff` and `Referrer-Policy: no-referrer`. The CSP allows inline scripts and
styles, because the dashboard ships as one self-contained `index.html`; what it
does enforce is that nothing loads from, or connects to, another origin.

Request bodies are capped: 64 KiB for `POST /api/status`, 1 MiB for
`POST /api/diagnostics` (which carries log dumps), and 4 KiB for configuration
writes. Oversized bodies are rejected with `400`.

The SQLite pool is bounded at 8 connections. SQLite has a single writer, so an
unbounded pool spent connections queueing for the same lock rather than buying
concurrency.

These bound resource exhaustion from slow or abandoned connections. They are not
a rate limiter and not a WAF — Lantern still expects to sit on a trusted network
or behind a reverse proxy.

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
