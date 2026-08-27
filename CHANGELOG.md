# Changelog

All notable changes to Lantern are documented here.

As of v0.60.0 Lantern is **stable**: the REST API and database schema are settled, and
breaking changes will come with a major version bump and upgrade notes. Pinning a version
tag rather than `latest` is still recommended, and [backups](docs/BACKUP.md) are still a
good idea.

---

## v0.60.0 — Sign-in, security hardening & a rebuilt dashboard

**First stable release.** Lantern gains a real sign-in gate, the dashboard has been
rebuilt on a design-token system, and a pre-release audit found and fixed several
authentication flaws. If you are running any earlier version, **upgrade promptly** — see
[Security fixes](#security-fixes) below for what was reachable without credentials and
[Upgrade notes](#upgrade-notes) for what to rotate.

### Security fixes

Every issue below was reproduced against a running server before and after the fix.

- **Unauthenticated admin takeover in token mode.** `PUT /api/auth/credentials` was not in
  the protected-route list. With only `LANTERN_AUTH_TOKEN` configured, an anonymous caller
  could invoke it, perform "first-time setup", and be handed a valid admin session cookie.
  The original token kept working afterwards, so the takeover left no obvious trace. The
  route is now gated, and setup mode additionally refuses to run when a token is configured
  unless the caller presents it.
- **Stored cross-site scripting in the dashboard.** A service name was interpolated into an
  inline `onclick` handler. `escapeHtml()` had already turned apostrophes into `&#39;`, so
  the guard that followed it matched nothing, while the HTML parser decoded the entity back
  before the JavaScript was compiled. Anyone able to `POST /api/status` — which is
  unauthenticated in the default open configuration — could run script in an administrator's
  browser. Both inline handlers were replaced with data attributes and delegated listeners.
- **The `/ws` session gate did nothing.** `/ws` was gated and `/api/public/ws` was not, but
  both were registered to the *same* hub, so an anonymous client could open the public path
  and receive byte-identical broadcasts. They are now separate hubs with different payloads.
- **`GET /api/backup` was anonymous in token mode.** It returns the entire SQLite file: the
  bcrypt credential hash, session token hashes, per-service API tokens, and saved webhook
  URLs. Now gated.
- **`GET /api/webhooks` was anonymous in token mode.** It returns webhook URLs in full, and
  a Discord webhook URL or Telegram bot URL is itself the credential. Now gated.
- **`GET /api/public/services/{name}/metadata` disclosed container internals.** The
  anonymous route returned the container image, its IP address, its published ports and its
  host filesystem mount paths. It has been removed; the gated
  `GET /api/services/{name}/metadata` is unchanged.
- **Per-service API tokens were stored in plaintext**, while session tokens had always been
  hashed. Lookups now match a SHA-256 hash and upgrade a plaintext row in place the first
  time it is used. The upgrade is lazy, so a token never used again stays in plaintext —
  delete rows for tokens you no longer use.

### Added

- **Sign-in.** Username and password credentials, hashed with bcrypt into SQLite. Seeded
  from `LANTERN_AUTH_USER`/`LANTERN_AUTH_PASS` on first boot only, after which the stored
  row is authoritative — so a password changed in the dashboard is not reverted by a stale
  environment variable on the next restart. You can also skip the environment entirely and
  turn sign-in on from **Settings → Account & Security**.
- **Cookie sessions.** `HttpOnly`, `SameSite=Strict`, `Secure` when the request is actually
  HTTPS, 30-day TTL, stored as a SHA-256 hash so a copied database yields no usable session.
  The cookie is what authenticates the live feed: a browser's `WebSocket` constructor cannot
  send an `Authorization` header, so without it the dashboard falls back to polling.
- **Login throttling.** Five failures from one client address triggers a 15-minute lockout,
  answered with `429` and a `Retry-After` header.
- **Auth endpoints:** `GET /api/auth/session`, `POST /api/auth/login`,
  `POST /api/auth/logout`, `PUT /api/auth/credentials`. Changing credentials revokes every
  session and re-issues one to the calling browser, so other devices sign out and yours does
  not.
- **`DELETE /api/services/{name}`** — removes Lantern's record of a service across all seven
  service-scoped tables in one transaction. A container still visible to Docker discovery
  re-registers on the next poll.
- **`POST /api/services/{name}/check`** — runs one active-monitor probe immediately instead
  of waiting for the next tick. Returns `202`; the result arrives over the normal broadcast.
- **`source` on every service** — `monitor`, `docker`, or `host`, derived per request, with
  one-click filtering on the dashboard.
- **Command palette** (`Cmd`/`Ctrl`+`K`, or `/`) with fuzzy search across every service.
- **Collapsible service groups** that remember their state.
- **Design-token system**: surface and semantic color tokens, spacing, type, shadow and
  motion scales, all keyed off the accent color.
- **Rebuilt service cards and heartbeat pills**, a structured heartbeat popover, a global
  status banner and metric ticker, a per-card quick-action menu, a badge embed dialog, and a
  90-beat telemetry view with a latency breakdown.
- **HTTP server timeouts and graceful shutdown.** `ReadHeaderTimeout` 10s, `ReadTimeout`
  30s, `IdleTimeout` 120s; `SIGTERM`/`SIGINT` drain in-flight requests for up to 15 seconds.
  `WriteTimeout` is deliberately unset because `GET /api/backup` streams the whole database.
- **CI now runs the tests.** `gofmt`, `go vet` and `go test -race` gate the image build; the
  workflow previously built and pushed without running anything.
- Regression tests pinning every route that was reachable without credentials, in both token
  mode and login-gate mode.

### Changed

- **`/api/public/ws` carries less than `/ws`.** The public socket now emits only a reduced
  `status_update` envelope — no `heartbeat` deltas and no check history.
- **Accessibility:** every status badge now meets a 4.5:1 contrast floor, modals trap focus
  correctly, and `Escape` pops one layer at a time.
- **Password managers no longer offer to autofill on the dashboard.** Closed modals and
  drawers were `opacity: 0` but still laid out, so the hidden login form's password field
  kept a real bounding box in the middle of the viewport and extensions anchored their
  autofill prompt to it. Closed overlays are now `visibility: hidden` and their credential
  inputs are disabled.
- The WebSocket error handler closed the current socket rather than the one that errored,
  which could kill a freshly opened connection during a reconnect.
- Client-side monitor-target validation no longer rejects targets the server accepts.
- Skeleton cards are purged on first real render, and every card is ranked on render so
  search results order correctly.
- `docker-compose.yml` documents the sign-in variables alongside the API token.

### Removed

- `GET /api/public/services/{name}/metadata`. See [Security fixes](#security-fixes).

### Known limitations

Carried into this release deliberately, and tracked for a follow-up:

- No connection-pool limits on SQLite, and `GET /api/services` fans out one goroutine per
  service. Fine at homelab scale; not tuned for hundreds of services.
- Request bodies are unbounded on most write routes; only the auth endpoints cap their size.
- No Content-Security-Policy or clickjacking headers yet.
- `GET /api/services/{name}/uptime` issues a maintenance-window query per timeline segment,
  so a wide range over a dense history is expensive. It is a public route.
- Concurrent writes to the same service's monitor config can orphan a scheduler goroutine.
- The race detector cannot run on a 64-bit Raspberry Pi (`CONFIG_ARM64_VA_BITS=39` versus
  ThreadSanitizer's required 48). CI covers it on amd64.

### Upgrade notes

- **No schema migration is required.** The `admin_credentials` and `sessions` tables are
  created on first start if absent.
- **If you ran an earlier version with `LANTERN_AUTH_TOKEN` set and the dashboard reachable
  by anyone you do not trust**, assume the database could have been downloaded via
  `GET /api/backup`. Rotate `LANTERN_AUTH_TOKEN`, re-issue any per-service tokens, rotate
  every webhook URL, and change your admin password if one was set.
- **Check for unexpected admin credentials.** If `GET /api/auth/session` reports
  `auth_required: true` and you never set a username, someone else did. Sign-in cannot be
  turned off from the UI — delete the row directly:
  `sqlite3 /data/lantern.db 'DELETE FROM admin_credentials; DELETE FROM sessions;'`
- **If you consumed `/api/public/ws`**, note it no longer carries `heartbeat` frames or the
  `history` array. Use the gated `/ws` for those.
- **If you consumed `/api/public/services/{name}/metadata`**, switch to the gated
  `/api/services/{name}/metadata` and supply a credential.
- Existing `api_tokens` rows keep working and are hashed in place on first use.

---

## v0.59.1 — Native Docker Discovery, Latency Tracking & SVG Badges

*Beta.* The headline change: Lantern now generates its own heartbeats. Mount the Docker
socket and every container on the host appears by itself — no push script, no agent, no
per-service configuration.

### Added

- **Native Docker container discovery.** A background worker polls the Docker daemon on
  an interval and records a status event per container. New containers auto-register
  simply by existing; the service list is derived from `status_events`.
  - One `GET /containers/json?all=1` call per tick covers every container, so the cost
    is one request regardless of how many containers the host runs.
  - Opt a container out with the label `lantern.ignore=true`
    (case-insensitive, whitespace-tolerant).
  - Active whenever the socket is reachable; disabled only by the literal
    `LANTERN_DOCKER_DISCOVERY=false`.
  - `LANTERN_DOCKER_POLL_SECONDS` (default `60`) is floored at `10` so a typo cannot
    hammer the daemon.
  - A socket-less deployment logs one line at startup and stays inactive rather than
    erroring on every tick.
- **Per-check latency.** `status_events` gains a `latency_ms` column, populated from all
  three sources: the Docker poller, active monitors (timed around the probe itself), and
  passive `POST /api/status` calls supplying an optional `latency_ms`.
- **Public SVG status badges** at `GET /api/badge/{service}.svg` — shields-style,
  embeddable in a README, served anonymously. `up` `#10B981`, `down` `#F43F5E`,
  `degraded` `#F59E0B`, everything else neutral grey. Maintenance outranks the last
  recorded check; an unknown service renders an `unknown` badge rather than a 404.
- **Webhook flap dampening.** An outage must be confirmed by two consecutive `down`
  checks before it alerts, and it alerts once per episode rather than once per check. A
  recovery is announced only if the outage that preceded it was announced — so a
  single-beat flap now produces no traffic at all, instead of a down alert followed
  immediately by a recovery.
- **Live heartbeat bar** replacing the 30-day bucketed trend bar. Each card shows the
  last 30 individual checks, with new beats sliding in over the WebSocket as they
  arrive. Header reads *Check History (Last 30 Beats)*.
- **Heartbeat tooltips** reporting relative time, absolute time, check latency, and the
  reported message — composed on hover rather than at render time, so the humanized
  delta is accurate whenever you actually point at a block.
- **Test suite** — the first in the repo. 55 assertions covering beat padding and
  ordering, the window uptime ratio, the dampening truth table, badge colours, and every
  row of the Docker state mapping. `go test ./...` previously reported `[no test files]`.
- `GET /api/webhooks/deliveries` documented — every delivery attempt with its HTTP
  status and error text.

### Changed

- **`uptime_percent` now means the heartbeat window**, not a 30-day average: it is
  `up ÷ non-empty beats` across the last 30 checks. Padding beats are excluded from
  both sides of the ratio, so a service with 3 recorded checks is scored out of 3
  rather than out of 30. `uptime_7d` and `uptime_30d` are unchanged.
- `history` in `GET /api/services` changed shape from daily buckets
  (`{start, status}`) to individual beats
  (`{status, timestamp, msg, latency_ms}`), always exactly 30 entries, oldest first,
  left-padded with `{"status": "empty"}` placeholders.
- A running container whose healthcheck has not yet passed reports `degraded` rather
  than `up`, so a service that is still booting never shows a green card. A container
  declaring no healthcheck is taken at its word.
- `docker-compose.yml` mounts the Docker socket `:ro` and documents the new env vars.
- Beat pulse animation is now a scale-and-brightness pop over `0.45s`, replacing the
  vertical wipe.
- `/api/badge/*` added to the always-open route allowlist so badges render anonymously.

### Fixed

- `HEAD` requests to the badge route returned `404`. gorilla/mux matches on method, so
  registering only `GET` meant link checkers and some markdown renderers saw a broken
  badge while `GET` worked fine.

### Removed

- `n8n_lantern_workflow.json`. Native discovery replaces the external n8n + shell script
  pipeline it described.

### Upgrade notes

- The `latency_ms` migration applies automatically on first start; existing rows default
  to `0`.
- If you relied on `uptime_percent` being a 30-day figure, switch to `uptime_30d`.
- If you consumed `history` from `GET /api/services`, note the shape change above.
- To enable discovery, mount `/var/run/docker.sock:/var/run/docker.sock:ro`. **Note that
  `:ro` applies to the mount point, not to the Docker API** — the daemon still accepts
  writes over that socket, so treat access to it as equivalent to root on the host.
- Discovery is on by default when the socket is present. If you would rather keep your
  existing push-only setup, set `LANTERN_DOCKER_DISCOVERY=false`.

Also shipping in this release (previously untagged): admin API token UI, Prometheus
`/metrics`, a real `/api/docs` route, `LANTERN_AUTH_TOKEN` enforcement with secured
Docker endpoints and hardened gzip middleware, richer Discord embeds, a capped Docker
log frame allocation, and a consolidated service query with corrected metric baselines
and retention pruning.

---

## v0.5.0

### Added
- WebSocket push with a non-blocking webhook worker pool.
- Optional active monitoring: HTTP(S), TCP port, and real ICMP ping.
- SSL certificate expiry tracking for HTTPS monitors.
- Cross-service activity log for status changes and webhook deliveries.
- CSV/JSON status history export.
- Database backup download via `VACUUM INTO`, safe under concurrent writes.
- Design-system tokens, motion scale, responsive layout, and error states.

### Changed
- DOM patching, gzip compression, and parallel cached metrics computation optimized.

---

## v0.4.0

### Added
- Service grouping with dashboard filtering.
- Docker container management: restart and live log tails.
- Container metadata inspection (ports, image tags, IPs, network topology).
- Instant search and toast notifications.
- Settings drawer with theme selector, accent color picker, and webhook manager.
- Web-based webhook persistence in the database with environment-variable fallback.
- 1h/24h/7d/30d time-range selector for the uptime chart.

### Fixed
- Light theme WCAG AA contrast and component styling.
- Settings converted to a full slide-over drawer.
- `updated_at` schema migration and webhook channel persistence.

---

## v0.3.0

### Added
- UI overhaul and redesign.
- Webhooks panel and completed webhooks API.
- GHCR Docker publishing workflow.
- Documentation suite.

### Fixed
- 30-day sparkline rendering.
- Cache-control headers to prevent a stale UI.

---

## v0.2.0

Initial public release: uptime percentages, incidents, maintenance mode, stale
detection, webhooks, demo mode, and a Docker `HEALTHCHECK`.
