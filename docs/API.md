# API Reference

Lantern discovers local Docker containers natively, and additionally accepts push-based
heartbeats and runs optional active monitors. All three write to the same
`status_events` table, so history, uptime, and incidents behave identically regardless
of where a check came from. Below is a comprehensive list of endpoints. See also
`GET /api/docs`, a live reference page generated from the actual route table.

> **Note**: Once `LANTERN_AUTH_TOKEN` or `LANTERN_AUTH_USER`/`LANTERN_AUTH_PASS` is set,
> mutating and administrative routes require auth — not just `POST`. That includes three
> `GET` routes whose responses are themselves sensitive: `GET /api/backup` (the whole
> database), `GET /api/webhooks` (webhook URLs, which *are* credentials), and both
> endpoints under `/api/services/{name}/docker/*` (container internals and log content).
> Pass a token as `Authorization: Bearer <TOKEN>`, or sign in for a session cookie.
> See [Public endpoints](#public-endpoints) below for what stays open, and the
> [Configuration Guide](CONFIG.md#security--authentication) for the exact route list.

---

## Authentication

Lantern supports three credentials, checked in this order: a session cookie, a bearer
token, then HTTP Basic Auth. Sessions exist because a browser's `WebSocket` constructor
cannot send an `Authorization` header, so header-only auth would leave the dashboard
falling back to polling.

### `GET /api/auth/session`
Reports whether sign-in is required and who you currently are. Always open — the SPA calls
it before deciding whether to render the login wall.

```bash
curl -s http://localhost:7654/api/auth/session
```

```json
{
  "auth_required": true,
  "authenticated": false,
  "token_mode": false,
  "can_setup": false
}
```

- `auth_required` — admin credentials exist, so the login gate is active.
- `token_mode` — only `LANTERN_AUTH_TOKEN` is configured; there is no password to type and
  no login wall is shown.
- `can_setup` — no credentials exist yet, so first-time setup is offered.

### `POST /api/auth/login`
Exchanges a username and password for an `HttpOnly`, `SameSite=Strict` session cookie.
Always open. Rate limited per client address: five failures triggers a 15-minute lockout,
answered with `429` and a `Retry-After` header.

```bash
curl -sc cookies.txt -X POST http://localhost:7654/api/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username": "admin", "password": "your-password"}'
```

### `POST /api/auth/logout`
Revokes the current session and clears the cookie.

### `PUT /api/auth/credentials`
Changes the admin username and/or password. Requires auth.

`current_password` is required and verified whenever credentials already exist, so a
borrowed session cannot silently change the password. A successful change revokes **every**
session and issues a fresh one to the calling browser — other devices are signed out, yours
is not. New passwords must be at least 8 characters.

```bash
curl -b cookies.txt -X PUT http://localhost:7654/api/auth/credentials \
  -H "Content-Type: application/json" \
  -d '{"current_password": "old-password", "new_password": "new-password"}'
```

When no credentials exist at all, this performs first-time setup without a current
password — it is how you turn sign-in on from a dashboard that is currently open. If
`LANTERN_AUTH_TOKEN` is set, that setup path additionally requires the token.

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
- `source` reports where the status came from: `monitor` when Lantern actively probes the
  service, `docker` when container discovery saw it on the most recent pass, or `host` for
  anything pushing to `POST /api/status`. It is derived per request, not stored, and
  `monitor` outranks `docker`.

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

## `DELETE /api/services/{name}`
Removes Lantern's record of a service: status history, diagnostics, maintenance state and
windows, group assignment, active monitor, and any scoped API tokens — all in one
transaction. Returns `404` for an unknown service.

This deletes Lantern's *record*, not the service itself. A container that Docker discovery
can still see re-registers on the next poll.

```bash
curl -X DELETE http://localhost:7654/api/services/old-service \
  -H "Authorization: Bearer your_token_here"
```

---

## `POST /api/services/{name}/check`
Runs one active-monitor probe immediately instead of waiting for the next tick. Returns
`202 Accepted`; the result arrives over the normal WebSocket broadcast once the worker runs
it. Returns `409` if the service has no active monitor, or its monitor is disabled.

```json
{ "status": "queued", "service_name": "nginx", "monitor_type": "http" }
```

---

## `GET /api/services/{name}/metadata`
Container and host metadata plus Lantern's own telemetry for one service: image, state,
health, container IP, network, published ports, mounts, restart count, total events
recorded, and last-seen time.

**Requires auth once configured.** There is no public equivalent — the anonymous
`/api/public/services/{name}/metadata` route was removed in v0.60.0 because it exposed
container IPs, published ports and host filesystem mount paths.

---

## `GET /api/services/{name}/uptime?range=1h|24h|7d|30d`
Uptime percentage, total downtime in minutes, incident count, and graph datapoints for the
requested window. Time spent under a maintenance window is excluded from downtime.

---

## `GET /api/services/{name}/strip?hours=`
Bucketed status history for the trend bar. `hours` defaults to `24`, capped at `720`; the
response holds at most 96 buckets, each reporting its dominant status.

---

## `GET /api/services/{name}/incidents?range=`
Detected `down`/`degraded` incidents with start, end, and duration in minutes, plus whether
each fell inside a maintenance window.

---

## `GET|PUT /api/services/{name}/maintenance`
Reads or sets a service's maintenance flag. Enabling it suppresses webhook alerts and opens
a maintenance window; disabling it closes the open window.

```bash
curl -X PUT http://localhost:7654/api/services/db/maintenance \
  -H "Authorization: Bearer your_token_here" \
  -H "Content-Type: application/json" \
  -d '{"enabled": true, "note": "schema upgrade"}'
```

---

## `GET /api/groups`
Every group name and the number of services in it.

## `PUT|POST /api/services/{name}/group`
Assigns a service to a group. An empty value clears the assignment.

```bash
curl -X PUT http://localhost:7654/api/services/db/group \
  -H "Authorization: Bearer your_token_here" \
  -H "Content-Type: application/json" \
  -d '{"group": "data"}'
```

---

## `GET /api/activity?limit=`
A merged, timestamp-sorted feed of every status change and webhook delivery attempt across
all services. `limit` defaults to `50`, capped at `200`.

---

## `GET /api/diagnostics/{id}`
The full stored content of one diagnostic run.

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
Returns the configured notification integrations and their source (`db`, `env`, or `none`).

**Requires auth.** The response contains each channel's URL in full, and a Discord webhook
URL or Telegram bot URL is itself the credential.

**Request:**
```bash
curl -s http://localhost:7654/api/webhooks \
  -H "Authorization: Bearer your_token_here"
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

## `GET /api/banner`
Returns the active announcement, or `{"active": false}` when there is none.
Also available anonymously at `GET /api/public/banner`, which is what the public
status page reads.

```json
{
  "active": true,
  "banner": {
    "id": 4,
    "level": "warning",
    "title": "Storage migration",
    "body": "02:00-04:00 UTC.",
    "created_at": "2026-08-27T12:51:53Z"
  }
}
```

## `POST /api/banner`
Publishes an announcement, dismissing whatever was active. Requires auth.
`level` is `info`, `warning` or `critical` and defaults to `info`; `title` is
required. Returns `201` with the created banner.

## `DELETE /api/banner`
Dismisses the active announcement. Requires auth. Dismissal is a timestamp, not
a delete, so the record survives.

---

## `GET|PUT /api/branding`
Reads or sets installation-wide branding for the header, on both the dashboard and
the public status page. Also available anonymously at `GET /api/public/branding`,
which is what the public status page reads. Unset fields are omitted.

```json
{ "title": "Acme Status", "logo_url": "https://acme.example/logo.svg", "accent_color": "#7c5cff" }
```

`PUT` is **admin-only** — a per-service scoped API token gets `403`, since branding
is not scoped to any one service. Validation: `title` at most 60 characters;
`logo_url` must be an `http`/`https` URL with a host (a `javascript:` or `data:`
URI is rejected, because this value is written into an `<img src>`);
`accent_color` must be a `#rgb` or `#rrggbb` hex color. Send a field as `""` to
clear it. The accent is only a default — a visitor who has picked their own accent
keeps it. Changes are recorded in the audit log.

Custom domains are a reverse-proxy concern, not application state: point a hostname
at Lantern and serve `/status` from it — see [Custom Domains](CUSTOM_DOMAIN.md).

The CSP is `img-src 'self' data:` plus the origin of the configured `logo_url`,
and nothing else, so an externally-hosted logo loads without opening the page to
every image host.

---

## `GET|PUT /api/notifications/schedule`
Reads or sets the global quiet-hours window for notifications.

```json
{ "enabled": true, "start_minute": 1320, "end_minute": 480, "mode": "digest" }
```

Minutes are 0-1439, UTC, from midnight — so `1320`/`480` is 22:00-08:00, wrapping
past midnight. `mode` is `mute` (alerts inside the window are dropped) or `digest`
(alerts are queued and sent as one combined message per channel when the window
closes). `start_minute == end_minute` or `enabled: false` means never active.
Admin-only; scoped tokens get `403`.

---

## `GET /api/admin/audit-log`
Returns admin actions, newest first. `?limit=` accepts 1-500 and defaults to 100.
Admin-only; scoped tokens get `403`.

```json
[
  {
    "id": 42,
    "actor": "admin",
    "action": "branding_change",
    "target": "",
    "detail": "title=Acme Status logo_url_set=true accent=#7c5cff",
    "success": true,
    "ip": "127.0.0.1",
    "timestamp": "2026-09-03T18:19:47Z"
  }
]
```

`actor` is `admin` for a session, Basic Auth or the admin bearer token, and
`token:<service>` for a per-service scoped token. Entries survive the deletion of
whatever they name — removing a service does not erase the record that it happened.

---

## `GET|PUT /api/services/{name}/alerts`
Reads or sets which webhook channels a service's alerts go to.

```json
{ "service_name": "db", "channels": ["discord", "gotify"], "routes_all": false }
```

`routes_all: true` means no route is configured, so the service alerts on every
configured channel. `PUT` with an empty `channels` array clears the route and
restores that default. Valid channels: `discord`, `telegram`, `gotify`,
`generic`.

---

## `GET /api/config/export`
Serialises services, groups, monitors, alert routes, maintenance flags and
webhook channels as portable JSON. Requires auth.

**Webhook URLs are redacted by default**, since they are credentials. Pass
`?include_secrets=true` to include them, which makes the file as sensitive as a
database backup.

```bash
curl -o lantern-config.json http://localhost:7654/api/config/export \
  -H "Authorization: Bearer your_token_here"
```

## `POST /api/config/import`
Applies an exported configuration. Requires auth. Additive and idempotent: it
upserts what the file describes and never deletes what the file omits. Fields
still holding `__REDACTED__` are skipped, so a redacted export can be re-imported
without clobbering live credentials. Invalid entries come back in `skipped`
rather than failing the whole import.

```json
{ "status": "ok", "applied": { "groups": 3, "monitors": 2, "alert_routes": 1, "webhooks": 0, "maintenance": 0 } }
```

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
Downloads a consistent database snapshot (`VACUUM INTO`), safe to fetch even under
concurrent writes.

**Requires auth.** The snapshot contains the bcrypt admin credential hash, session token
hashes, per-service API tokens, and saved webhook URLs — treat the downloaded file as a
secret. See [Backup & Restore](BACKUP.md).

```bash
curl -o lantern-backup.db http://localhost:7654/api/backup \
  -H "Authorization: Bearer your_token_here"
```

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

### `GET /api/public/ws`

The unauthenticated socket that drives the public `/status` page. As of v0.60.0 it runs on a
**separate hub** from `/ws` rather than sharing one, and carries strictly less:

- only `status_update` frames — no `heartbeat` deltas,
- and no `history` array.

Each frame's `service` object holds `service_name`, `status`, `message`, `timestamp`,
`last_seen`, `stale`, `maintenance`, `group_name`, `uptime_7d`, `uptime_30d`,
`uptime_percent`, `monitor_type`, and `source`.

Before v0.60.0 both paths were registered to the same hub, so the session gate on `/ws`
achieved nothing — an anonymous client could open `/api/public/ws` and receive byte-identical
broadcasts.

---

## Public endpoints

Reachable with no credential, whatever else is configured:

| Endpoint | Notes |
|---|---|
| `/status` and the static shell | Carries no service data itself; everything arrives over the API |
| `GET /api/public/services` | Same shape as `/api/services` |
| `GET /api/public/groups` | Same shape as `/api/groups` |
| `GET /api/public/services/{name}/uptime` | Same shape as the gated uptime route |
| `GET /api/public/ws` | Reduced live feed, described above |
| `GET\|HEAD /api/badge/{service}.svg` | So badges render when embedded in a README |
| `GET /metrics` | Prometheus scrapers do not usually send app-level auth |
| `GET /api/health` | What the container `HEALTHCHECK` polls, with no credentials |
| `GET /api/docs` | A static route reference |
| `GET /api/auth/session`, `POST /api/auth/login` | Or there would be no way to sign in |

`GET /api/public/services/{name}/metadata` was **removed in v0.60.0**. Use the gated
`GET /api/services/{name}/metadata` instead.
