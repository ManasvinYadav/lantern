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

Lantern polls the Docker daemon and records a heartbeat for every container it
finds. Containers are registered automatically the first time they are seen —
there is nothing to configure per service.

The daemon can be reached two ways:

- **Unix socket** (default): mount `/var/run/docker.sock` into the container.
- **TCP / socket proxy** (recommended): set `DOCKER_HOST` to point at a proxy
  such as [`docker-socket-proxy`](https://github.com/Tecnativa/docker-socket-proxy).
  No socket mount needed; the proxy grants only the API surface Lantern uses.

| Variable | Default | Description |
|---|---|---|
| `LANTERN_DOCKER_DISCOVERY` | `true` | Enables the container poller. Disabled only by the literal value `false`, so a typo leaves discovery working rather than silently off. |
| `LANTERN_DOCKER_POLL_SECONDS` | `60` | How often to poll the Docker daemon. Floored at `10` so a mistake cannot hammer the socket. |

Lantern also reads the standard Docker environment variables to determine *how* to
reach the daemon:

| Variable | Default | Description |
|---|---|---|
| `DOCKER_HOST` | *(unset — uses `/var/run/docker.sock`)* | Docker endpoint URL. Supports `unix://`, `tcp://`, `http://`, and `https://` schemes. |
| `DOCKER_TLS_VERIFY` | `0` | Set to `1` to enable mutual TLS. Requires `DOCKER_CERT_PATH`. |
| `DOCKER_CERT_PATH` | `~/.docker` | Directory containing `ca.pem`, `cert.pem`, and `key.pem` for TLS connections. |

Mount the socket to enable it (classic path):

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

### Socket Proxy

Mounting the raw Docker socket gives Lantern (and anything that can reach it)
unrestricted daemon access — effectively root on the host. A socket proxy such
as [`docker-socket-proxy`](https://github.com/Tecnativa/docker-socket-proxy)
exposes only the specific API endpoints Lantern needs. Set `DOCKER_HOST` to
point Lantern at the proxy over TCP:

```yaml
services:
  socket-proxy:
    image: tecnativa/docker-socket-proxy
    container_name: socket-proxy
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    environment:
      CONTAINERS: 1   # list containers + inspect (discovery, metadata, logs)
      INFO: 1         # GET /info — used by the TCP availability probe
      POST: 1         # POST /containers/{id}/restart — container restart action
    networks:
      - socket-proxy

  lantern:
    image: ghcr.io/manasvinyadav/lantern:v0.70.0
    container_name: lantern
    restart: unless-stopped
    ports:
      - "7654:7654"
    volumes:
      - lantern_data:/data
      # No socket mount — Lantern connects via DOCKER_HOST instead.
    environment:
      - DOCKER_HOST=tcp://socket-proxy:2375
      - LANTERN_AUTH_TOKEN=${LANTERN_AUTH_TOKEN:-}
    networks:
      - socket-proxy
    depends_on:
      - socket-proxy

volumes:
  lantern_data:

networks:
  socket-proxy:
```

`DOCKER_HOST` is parsed at startup and reported in the log:

```
docker: transport = TCP (http://socket-proxy:2375)
```

For a Unix socket the log reads:

```
docker: transport = Unix socket (/var/run/docker.sock)
```

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
| `LANTERN_AUTH_TOKEN` | Installation-wide bearer token. Passed via `Authorization: Bearer <TOKEN>`. Highly recommended for API clients pushing status. Authenticates as **admin**, never owner — see [Users & roles](#users--roles). |
| `LANTERN_AUTH_USER` | Username for the first account. Seeds the store on first boot; also accepted as HTTP Basic Auth. |
| `LANTERN_AUTH_PASS` | Password for it. Used in conjunction with `LANTERN_AUTH_USER`. |

### Users & roles

Every account has one of three roles:

| | viewer | admin | owner |
|---|---|---|---|
| Read the dashboard, history, incidents, badges | ✅ | ✅ | ✅ |
| Push status, trigger checks, toggle maintenance | | ✅ | ✅ |
| Services, monitors, groups, alert routes, branding, quiet hours, announcements | | ✅ | ✅ |
| Backup, webhook URLs, config export/import, audit log | | ✅ | ✅ |
| Create, disable, re-role and remove accounts | | | ✅ |

Roles are coarse deliberately. Per-service access for a *person* is not something Lantern
needs; per-service access for a *machine* already exists as
[scoped API tokens](#scoped-api-tokens-at-rest), which are a separate axis and are
unaffected by roles.

A **viewer** is read-only, and is additionally refused on the routes that carry
installation-wide secrets: `GET /api/backup` would hand them every password hash,
`GET /api/webhooks` every webhook URL, `GET /api/config/export` the whole configuration,
and `GET /api/admin/audit-log` every action anyone has taken. Those are refused with `403`
rather than served.

The gate lives in the auth middleware and runs before any handler. The dashboard hides
controls a role cannot use, but that is a courtesy — an API call bypassing the UI is
refused just the same. Roles are read from the database on every request rather than
stamped onto the session, so **demoting or disabling someone takes effect on their very
next request**, not at their next sign-in.

Two things Lantern will not let you do, because both strand the install with no way back
short of editing the database by hand:

- delete, disable, or demote the **last enabled owner** (`409`)
- delete the account **you are signed in as** (`409`)

Manage accounts at **Settings → Users**, which appears only for an owner, or through
[`/api/admin/users`](API.md). Account management is refused outright on an install with no
credentials at all: such an install is open by design, so without that guard anyone
reaching the port could POST themselves an owner.

**Upgrading from before v0.70:** nothing to do. The single operator in the old
`admin_credentials` row becomes the owner automatically on first boot, password digest
carried across rather than re-hashed, so the same credentials keep working. The legacy
table is left in place so a downgrade still finds what it expects.

### Signing in

Credentials live in the database, not in the environment. Set
`LANTERN_AUTH_USER` and `LANTERN_AUTH_PASS` and Lantern hashes them with
bcrypt into the `users` table as the owner **on first boot only** — after that the stored
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

Change **your own** username or password at **Settings → Account & Security**. The
current password is required and verified; a wrong one returns `401`. A
successful change revokes your other sessions and issues a fresh one to the browser
that made the change, so your other devices are signed out and this one is not — nobody
else's session is affected. New passwords must be at least 8 characters. An owner resets
*someone else's* password from **Settings → Users** instead, which also ends that
account's sessions.

If `LANTERN_AUTH_TOKEN` is set, turning sign-in on for the first time requires
that token. Setup mode issues an admin session to a caller who has proved
nothing, which is only defensible when the dashboard was already wide open — on
a token-configured deployment it would be a privilege escalation.

### What each mode gates

With **accounts set up**, everything requires a session except the always-open
surface below, and what a session may then do depends on its
[role](#users--roles).

With **`LANTERN_AUTH_TOKEN` alone**, only mutating and administrative routes
require the token. Those are:

- `POST /api/status` and `POST /api/diagnostics`
- `GET`, `PUT` and `POST /api/webhooks`, and `POST /api/webhooks/test`
- `GET /api/backup`
- `PUT /api/auth/credentials`
- `GET` and `PUT /api/branding` and `/api/notifications/schedule`
- every `/api/admin/*` route (the audit log; account management is refused to
  this credential outright)
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
`403` if used against a different service. They are also refused outright on the
routes that reach past a single service: the backup, webhook URLs, config
export/import, the audit log, account management, and the installation-wide
branding and quiet-hours settings. Both token types keep working under the login
gate, so existing scripts, automations and CI need no changes.

> Before v0.63.1 that second restriction did not exist. A token issued for one
> service could authenticate against `GET /api/backup` and download a full
> database snapshot — the credential hash, every session hash, and every other
> service's token. If you ran an earlier version and have issued scoped tokens,
> rotate them.

### Always open

Reachable without any credential, regardless of configuration:

- `/status` and the static SPA shell
- `GET /api/public/services`
- `GET /api/public/groups`
- `GET /api/public/services/{name}/uptime`
- `GET /api/public/ws`
- `GET /api/public/banner` and `GET /api/public/branding`
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

## TLS certificate expiry

HTTPS active monitors read the peer certificate on every check and record its
expiry date, then classify how much validity is left.

| Variable | Default | Description |
|---|---|---|
| `LANTERN_CERT_WARN_DAYS` | `30` | Below this, the check message carries an expiry notice. |
| `LANTERN_CERT_CRITICAL_DAYS` | `7` | Below this, the service is additionally marked `degraded`. |

| Days remaining | `cert_status` | Effect on the service |
|---|---|---|
| more than the warning threshold | `ok` | none |
| at or below the warning threshold | `warning` | expiry noted in the check message |
| at or below the critical threshold | `critical` | message, and an `up` service becomes `degraded` |
| negative | `expired` | service is marked `down` |

Degrading on `critical` is deliberate: a certificate days from expiry is an
outage with a start date already scheduled, and degrading is what turns it into
one alert now rather than a surprise later. `expired` marks the service `down`
because verification is already failing for real clients even though Lantern's
own probe still got an answer.

A critical threshold above the warning one would mean a certificate went
critical before it ever warned; that pair is clamped at startup with a log line
rather than refused.

## Per-service alert routing

By default every status change notifies every configured webhook channel. A
service can instead name the channels it should use:

```bash
curl -X PUT http://localhost:7654/api/services/db/alerts \
  -H "Authorization: Bearer your_token_here" \
  -H "Content-Type: application/json" \
  -d '{"channels": ["discord", "gotify"]}'
```

Valid channels are `discord`, `telegram`, `gotify` and `generic`. A service with
no route, or one whose route is cleared with an empty list, alerts on every
configured channel — which is what Lantern did before routing existed, so
upgrading changes nothing until you opt a service in.

Clearing a route restores alerting everywhere rather than silencing the service.
Silencing is what maintenance mode is for, and conflating the two would be a
trap.

## Announcements

An announcement banner pins to the top of the dashboard and the public `/status`
page. Reading it is always anonymous; publishing and dismissing require admin.

Publish it from **Settings → Announcement**: pick a severity, write a title and an
optional detail line, and press Publish. The panel shows what is currently live
and prefills the form with it, so amending the wording is an edit rather than a
retype. **Take down** removes it, as does the ✕ on the banner itself.

From the API:

```bash
curl -X POST http://localhost:7654/api/banner \
  -H "Authorization: Bearer your_token_here" \
  -H "Content-Type: application/json" \
  -d '{"level": "warning", "title": "Storage migration", "body": "02:00-04:00 UTC."}'
```

Levels are `info`, `warning` and `critical`. Only one announcement is active at
a time: posting a new one dismisses whatever was showing, which matches how this
is actually used — the current situation replaces the previous one rather than
stacking on it. Dismissal records a timestamp rather than deleting the row, so
what was announced and when survives for reconstructing an incident afterwards.

## Notification quiet hours

A single installation-wide window during which alerts either go quiet or get
batched. Configure it at **Settings → Alerts & Webhooks → Quiet Hours**, or
through [`GET|PUT /api/notifications/schedule`](API.md).

- **Mute** — notifications inside the window are dropped, the same way
  per-service maintenance mode already works, but time-scheduled and global
  rather than manual and per-service.
- **Digest** — notifications inside the window are queued instead of dropped,
  then sent as one combined message per channel as soon as the window closes,
  respecting each service's existing [alert routing](#per-service-alert-routing).
  A background flusher checks every minute.

Times are minutes past midnight **UTC**, 0–1439. A window may wrap past
midnight (22:00–08:00 is `start_minute: 1320`, `end_minute: 480`). A window
whose start equals its end is never active, as is a disabled one.

This sits alongside per-service maintenance mode, not instead of it: both are
independent suppression paths into the same dispatch. A service in maintenance
stays silent whatever the schedule says.

Switching mode, or turning quiet hours off, never strands queued events — the
flusher drains the queue when the window closes regardless of what the mode is
by then.

## Status page branding

Override the name, logo and default accent shown in the header of both the
dashboard and the public status page — they are one HTML page, so both follow.
**Settings → General → Branding**, or
[`GET|PUT /api/branding`](API.md#getput-apibranding).

- **Name** replaces "Lantern" in the header and the browser tab title.
- **Logo URL** must be an `http(s)` URL with a host. A `javascript:` or `data:`
  URI is refused: the value ends up in an `<img src>`. The Content-Security-Policy
  is widened by exactly that one origin and nothing else, so an
  externally-hosted logo loads without opening the page to every image host.
- **Default accent** applies only to visitors who have not picked their own
  accent. A personal choice always wins.

Writes are admin-level; a scoped API token is refused, since branding is
installation-wide. Reads are anonymous on `/api/public/branding`, which is what
the public status page uses.

Custom domains are deliberately *not* application state — see
[Custom Domains](CUSTOM_DOMAIN.md).

## Audit log

A persisted record of administrative actions, at **Diagnostics → Audit Log** or
[`GET /api/admin/audit-log`](API.md). Each entry records who, what, the target,
success or failure, the source IP, and when.

Covered: logins (success **and** failure), credential changes, account create /
update / delete, service deletion, group, alert-route and monitor edits,
maintenance toggles, webhook channel changes (channel *names* only, never the
URLs), branding and quiet-hours changes, config imports, and Docker container
restarts.

The actor is the signed-in username, `token:<service>` for a scoped API token,
or `admin` for the `LANTERN_AUTH_TOKEN` bearer credential, which names no
account. Behind a reverse proxy the IP is the proxy's — see
[Custom Domains](CUSTOM_DOMAIN.md).

Entries outlive whatever they name: deleting a service does not erase the record
that it was deleted. The log is readable by admins and owners, and refused to
viewers.

## Service groups

Assign a service to a group from its detail drawer, or with
`PUT /api/services/{name}/group`. Groups drive the dashboard's grouped view and
its filters.

Each group header carries a rollup of what is underneath it, on the same
worst-status-wins ranking the rest of the dashboard uses
(`down` > `degraded` > `up` > `unknown`), so a collapsed group still shows that
something inside it is wrong. Maintenance is surfaced on the header only when
nothing under it is down or degraded — a paused check must never mask a real
outage.

## Configuration export and import

`GET /api/config/export` serialises what Lantern watches and how it alerts —
services, groups, active monitors, alert routes, maintenance flags and webhook
channels — as portable JSON. Status history is deliberately excluded; that is an
observation rather than configuration, and `GET /api/backup` already produces a
full snapshot.

**Secrets are redacted by default.** Webhook URLs come back as `__REDACTED__`
unless you pass `?include_secrets=true`, because a Discord webhook URL or a
Telegram bot URL is itself the credential, and an export that carries them is as
sensitive as a database backup while being far easier to paste into an issue.

`POST /api/config/import` applies an export. It is additive and idempotent: it
upserts what the file describes and never deletes anything the file omits, so it
is safe to run against a populated instance and running it twice changes nothing
the second time. A field still holding `__REDACTED__` is skipped rather than
written, so re-importing a redacted export does not overwrite a working webhook
with the placeholder. Entries that fail validation are reported in a `skipped`
array rather than aborting the whole import.

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
