# Changelog

All notable changes to Lantern are documented here.

As of v0.60.0 Lantern is **stable**: the REST API and database schema are settled, and
breaking changes will come with a major version bump and upgrade notes. Pinning a version
tag rather than `latest` is still recommended, and [backups](docs/BACKUP.md) are still a
good idea.

---

## v0.67.1 — Branding logo blocked by our own CSP

### Fixed

- **A branding `logo_url` on another host never loaded.** Settings accepted it
  and the API stored it, but the response CSP is `img-src 'self' data:`, so the
  browser refused the image — the feature was only ever going to work for a
  same-origin logo. `img-src` now also carries the single origin of the
  configured logo (scheme and host only, not its path), primed at startup and
  refreshed on every branding change. Nothing else is allowed: a blanket
  `https:` would have opened every install to every image host to support a
  setting most installs never touch.

### Added

- **[docs/CUSTOM_DOMAIN.md](docs/CUSTOM_DOMAIN.md)** — serving the status page
  on your own hostname, with Caddy/nginx/Traefik snippets, how to expose only
  `/status` on a public hostname, and the four things that change behind a
  proxy: `X-Forwarded-Proto` is required for `Secure` session cookies, the
  login throttle and the audit log both see the proxy's address rather than the
  visitor's, and a `Host`-rewriting proxy needs `LANTERN_WS_ALLOWED_ORIGINS`.

## v0.67.0 — New brand mark

### Changed

- **A real lantern for a logo.** The favicon and PWA icon were two unrelated
  marks: a red Lucide "flame-kindling" outline (`#e32400`) and an amber flame
  on a rounded square (`#f59e0b`). Both were painted in status colours — on a
  monitoring dashboard a red tab icon reads as "something is down" at a
  glance, and amber is `--degraded`. They are now one mark, drawn once and
  used everywhere: a lantern housing in slate with an emerald-lit glass panel
  and a white flame.
- **The header has a logo.** The pulsing emerald dot beside the wordmark is
  replaced by the lantern mark, whose flame flickers instead (and holds still
  under `prefers-reduced-motion`, via the existing global rule). Its housing
  follows the theme's text colour so it reads on light and dark; the glass
  keeps the logo's own fixed emerald rather than following the accent picker,
  since a light custom accent would swallow the white flame drawn on it. A
  custom `logo_url` from Settings → Branding replaces the mark rather than
  sitting beside it.
- The `🔆` in the browser tab title is gone — the favicon does that job now.

### Removed

- The `.loading-pulse` indicator and its `pulse` keyframes. Its `.loading`
  state was only ever removed, never set, so it was a permanently-pulsing dot
  with no meaning.

## v0.66.0 — Status page branding

### Added

- **`GET`/`PUT /api/branding`** (plus the unauthenticated public mirror
  `GET /api/public/branding`), and a new "Branding" section in
  Settings → General. Overrides three things in the header, on both the
  dashboard and the public status page:
  - **Name** — replaces "Lantern" in the header and the browser tab title.
  - **Logo URL** — an image shown beside the name. Restricted to `http(s)`
    URLs server-side, so a stored value can never become a `javascript:` or
    `data:` URI reflected into an `<img src>`.
  - **Default accent** — a hex color applied to visitors who have not
    picked their own accent. A personal choice always wins over the
    operator default.

  Writes are admin-only: a per-service scoped API token gets a 403, since
  branding is installation-wide. Every change is recorded in the audit log.
  Closes the "public status page cannot be branded" feature gap.

  Custom domains stay a reverse-proxy concern rather than application
  state — point a hostname at Lantern's `/status` and the branding follows.

### Fixed

- The browser tab title had a hardcoded `v0.62.2` in it, five releases
  stale. It now reads "Lantern — Service Dashboard" (or the configured
  name).

## v0.65.0 — Notification quiet hours (mute or digest)

### Added

- **A global, scheduled quiet-hours window for notifications** —
  `GET`/`PUT /api/notifications/schedule`, and a new "Quiet Hours" section
  in Settings → Alerts & Webhooks. Configure a daily UTC start/end time
  (wrapping past midnight is fine, e.g. 22:00-08:00) and a mode:
  - **Mute**: notifications during the window are dropped, the same way
    per-service maintenance mode already works, just time-scheduled and
    global instead of manual and per-service.
  - **Digest**: notifications during the window are queued instead of
    dropped, then sent as one combined message per channel — respecting
    each service's existing alert routing — as soon as the window closes.
    A background flusher checks every minute.

  This sits alongside, not instead of, per-service maintenance mode: both
  are independent suppression paths into the same notification dispatch.
  Closes the "only binary maintenance mode; no scheduled quiet hours or
  batched digest" feature-gap.

## v0.64.0 — Admin action audit log

### Added

- **`GET /api/admin/audit-log`**: a persisted record of admin actions —
  logins (success and failure, with source IP), credential changes, service
  deletion, group/alert-route/monitor edits, maintenance mode toggles,
  webhook channel changes (channel names only, never the URLs), config
  imports, and Docker container restarts. Each entry records who (the
  session/admin-token caller as `"admin"`, or a scoped token as
  `"token:<service>"`), what, the target, success/failure, source IP, and
  timestamp. Closes the "login throttling is in-memory only, no persisted
  record of login attempts or config changes" gap from the last audit.
  Admin-only — see `isAdminOnlyEndpoint` below. Viewable from the
  Diagnostics drawer's new "Audit Log" tab.

### Fixed

- **Scoped API tokens could disable another service's maintenance mode
  (High).** `PUT /api/services/{name}/maintenance` was the one mutating
  per-service handler without a scoped-token ownership check — the same
  class of gap Fix 1 (v0.62.4) closed for the monitor endpoints. A token
  issued to service A could silence service B's alerts by flipping B into
  maintenance mode, or falsely mark it under maintenance to mask a real
  outage. Found while wiring the audit log's own admin-only gating; fixed
  the same way as the earlier monitor-handler fix.

## v0.63.1 — Scoped API tokens could authenticate against admin-only routes

### Fixed

- **Privilege escalation via scoped API tokens (High).** A token issued to
  one service authenticated successfully as a Bearer credential on *every*
  route the auth middleware saw — including `GET /api/backup` (a full
  database snapshot containing the admin credential hash, every session
  hash, and every other service's API token), `/api/webhooks`
  (Discord/Telegram webhook URLs), `/api/config/export` and
  `/api/config/import` (the whole installation's configuration), and
  `PUT /api/auth/credentials`. Earlier scoped-token checks (v0.62.4) only
  stopped a token from acting on a service other than its own; nothing
  stopped it from acting as a full admin. `isAdminOnlyEndpoint` now
  enumerates these routes and `authMiddleware` rejects a scoped token
  against them with 403 before it ever reaches the handler. The admin-wide
  bearer token, session cookie, and Basic Auth paths are unaffected.

## v0.63.0 — Service-group status rollup

### Added

- **`GET /api/groups` (and its public counterpart) now returns an aggregate
  health rollup per group**, not just a member count. `GroupSummary` gained
  `status` (the worst status across the group's member services — down beats
  degraded beats up beats unknown, the same ranking the heartbeat strip uses
  to pick a bucket's dominant status) and `maintenance` (true if any member
  currently has maintenance mode enabled). The dashboard's collapsible group
  headers show a live status dot driven by this rollup, computed the same
  way client-side from `state.services` so it updates in real time over the
  WebSocket feed without an extra poll; a group with anything down or
  degraded always shows that, even if others in it are paused for
  maintenance.

## v0.62.4 — Monitor response validation, and a scoped-token gap closed

### Added

- **Regex and JSON-path validation for HTTP monitors.** An active HTTP
  monitor can now optionally require the response body to match a regex
  (`body_pattern`), and/or a JSON field to equal an expected value
  (`json_path`/`json_expect`), on top of the existing status-code check. All
  configured checks must pass for "up"; the first one that fails determines
  the "down" message. Configurable from the monitor's edit form in the
  dashboard. `body_pattern` is validated as a real regex when saved (400 on
  a bad pattern), and its compiled form is cached rather than recompiled on
  every check.

### Fixed

- **Scoped API tokens could hijack another service's monitor.** A token
  issued to one service could `PUT`/`DELETE` any other service's active
  monitor config — forging its status, redirecting its target, or deleting
  it outright. The monitor endpoints now enforce the same scoped-token check
  already used by the alert-routing and group endpoints.
- **Config export issued 4 queries per exported service** instead of
  batching them, and two of the four silently discarded database errors
  under lock contention (a real service could export with an empty
  group/maintenance state and no indication anything went wrong). Both the
  export and import paths now propagate query errors instead of swallowing
  them.

---

## v0.62.3 — DOCKER_HOST support for socket proxies (closes #2)

Users running a **socket proxy** (such as
[`docker-socket-proxy`](https://github.com/Tecnativa/docker-socket-proxy)) can
now point Lantern at the proxy's TCP endpoint instead of mounting the raw
`/var/run/docker.sock`. Set `DOCKER_HOST` and Lantern picks it up automatically
— no other configuration is needed, and existing deployments that mount the
socket are completely unaffected.

### Added

- **`DOCKER_HOST` environment variable support.** Lantern now resolves the
  Docker endpoint once at startup from the standard Docker environment
  variables rather than always dialing `/var/run/docker.sock` directly.
  Supported URL schemes:

  | Value | Effect |
  |---|---|
  | *(unset)* | Falls back to `/var/run/docker.sock` — existing behaviour unchanged |
  | `unix:///path/to/docker.sock` | Explicit Unix socket at an alternative path |
  | `tcp://host:port` or `http://host:port` | Plain HTTP to a TCP endpoint (e.g. a socket proxy) |
  | `https://host:port` | TLS — also activated by `DOCKER_TLS_VERIFY=1` |

- **`DOCKER_TLS_VERIFY` and `DOCKER_CERT_PATH` support.** When
  `DOCKER_TLS_VERIFY=1` (or the scheme is `https://`), Lantern loads
  `ca.pem`, `cert.pem`, and `key.pem` from `DOCKER_CERT_PATH` (or
  `~/.docker` when unset) for mutual-TLS authentication.

- **Startup log line** reports the resolved transport so it is easy to
  confirm which path is active:
  ```
  docker: transport = TCP (http://socket-proxy:2375)
  ```
  or, for a Unix socket:
  ```
  docker: transport = Unix socket (/var/run/docker.sock)
  ```

- **13 new unit tests** in `docker_test.go` covering all URL schemes,
  error paths, availability probing, and container lookup over a fake
  TCP server — no real Docker daemon required.

---

## v0.62.2 — Tabbed settings, and a credential for push scripts

Lantern's own behaviour is unchanged in this release. The fix is to the shipped
`docker-compose.yml`, and the note below is worth reading if you turned sign-in
on and something quietly stopped reporting.

### Fixed

- **`LANTERN_AUTH_TOKEN` is now wired through `docker-compose.yml`**, read from a
  gitignored `.env` and defaulting to empty. It was present only as a commented
  example, so the usual way to give a script a credential was to hand-edit the
  compose file. Setting it does not weaken the sign-in gate: `authMiddleware`
  evaluates `authRequired()` before the token in its fallthrough switch, so with
  admin credentials stored, every non-exempt route still demands a session and
  an unauthenticated `POST /api/status` is still a 401. The token only adds a
  second accepted credential; it opens nothing that was closed.

### Changed

- **The settings drawer is grouped into four tabs** — General, Account &
  Security, Alerts & Webhooks, Monitors — replacing a single column that had
  grown to eight unrelated sections. The tab bar sits between the drawer header
  and its scroll region, so it stays put while a panel scrolls, and it scrolls
  horizontally on a narrow viewport rather than handing the drawer body a
  sideways scrollbar. Credential fields in a tab that is not showing are
  disabled, reusing the switch that already keeps them inert in a closed
  overlay, so no password manager offers to fill a field you cannot see.
- The browser tab icon is now `static/favicon.svg` rather than an inline
  data URI. The installed PWA icon is unchanged.
- Refreshed the README screenshots to the current UI.

### Upgrade notes

Turning sign-in on gates `POST /api/status`. Any script that pushes heartbeats
without a credential starts receiving **401**, and because the common idiom

```bash
curl -sf -X POST "$LANTERN_URL/api/status" ... || true
```

discards both the response body and the exit code, it will keep reporting
success while Lantern receives nothing. The services it covers stay on their
last known status until the stale sweep marks them down `LANTERN_STALE_HOURS`
later — 24 hours by default — which reads as several unrelated services failing
at the same second, a day after the actual cause.

If you have such a script, give it the token and stop hiding the status code:

```bash
TOKEN=$(grep -m1 '^LANTERN_AUTH_TOKEN=' /path/to/.env | cut -d= -f2-)
code=$(curl -s -o /tmp/out -w '%{http_code}' -X POST "$LANTERN_URL/api/status" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"service_name":"my-service","status":"up"}')
case "$code" in 2*) ;; *) echo "push failed: $code $(cat /tmp/out)" >&2; exit 1 ;; esac
```

Pushing a heartbeat for the pusher itself makes the next such breakage visible
on the dashboard within a minute instead of a day.

---

## v0.62.1 — Announcement authoring UI

v0.62.0 shipped the announcement banner's API and its display, but no way to
write one from the dashboard — publishing meant a `curl` call, which is not a
feature anyone reaches for mid-incident.

### Added

- **Settings → Announcement.** Severity selector, title, optional detail, and
  Publish / Take down. The panel reads the same endpoint the banner does, so it
  always reflects what visitors are actually seeing rather than a local guess,
  and it prefills the form with the live announcement so amending the wording is
  an edit rather than a retype.

### Fixed

- Dismissing the banner with its own ✕ now also refreshes the settings panel, so
  the two cannot disagree about whether anything is showing.

---

## v0.62.0 — Announcements, alert routing, config portability & PWA

Feature release. Nothing here changes existing behaviour: every addition is
inert until you use it.

### Added

- **Announcement banner.** Publish a maintenance or incident notice in one of
  three severities; it pins to the top of the dashboard and the public `/status`
  page. Reading is anonymous — that is the point of a status banner — while
  publishing and dismissing require admin. One announcement is active at a time:
  posting a new one dismisses the old, in a single transaction so there is never
  a moment with two or with none. Dismissal is a timestamp rather than a delete,
  so what was announced and when survives.
  `GET|POST|DELETE /api/banner`, `GET /api/public/banner`.
- **Per-service alert routing.** Send one service's alerts to Discord and
  another's to Telegram. A service with no route alerts on every configured
  channel, which is exactly what happened before routing existed — so this is
  inert until you opt a service in, and clearing a route restores that default
  rather than silencing the service.
  `GET|PUT /api/services/{name}/alerts`.
- **Configurable TLS expiry thresholds.** `LANTERN_CERT_WARN_DAYS` (default 30)
  and `LANTERN_CERT_CRITICAL_DAYS` (default 7) replace a hardcoded 14-day
  warning. Monitors now report `cert_status` of `ok`, `warning`, `critical` or
  `expired`. **This changes monitor outcomes:** a certificate at or below the
  critical threshold degrades an otherwise-`up` service, and an expired one marks
  it `down` — verification is already failing for real clients even though
  Lantern's own probe still got an answer.
- **Configuration export and import.** `GET /api/config/export` serialises
  services, groups, monitors, alert routes, maintenance flags and webhook
  channels as portable JSON; `POST /api/config/import` restores it. Webhook URLs
  are redacted unless `?include_secrets=true`, and an import skips fields still
  holding the placeholder, so a redacted export round-trips without clobbering
  live credentials. Import is additive and idempotent.
- **Installable as a PWA.** A web app manifest, an inline SVG favicon, and the
  meta tags needed to open standalone from a phone home screen or a desktop
  dock. The favicon is a data URI, so it costs no request and cannot 404 behind
  the auth gate.

### Changed

- Every write route now answers a malformed or oversized body with the same
  `400` and the same `{"error": "payload too large or invalid json"}`. Each
  endpoint previously invented its own wording, which a client had to
  special-case per route to tell apart from a real validation failure.
- `Referrer-Policy` is now `strict-origin-when-cross-origin` (was
  `no-referrer`).

### Fixed

- The fuzzy-search scoring comment described the opposite of what the code does.
  It claimed `"nb"` ranks `netbird` above `n8n-backup`; the function returns 34.3
  and 49.0 respectively, because a character landing on a word boundary is worth
  +15 and `n8n-backup` collects that twice.

### Removed

- References to n8n throughout comments and documentation. It was the external
  signalling pipeline before native Docker discovery replaced it in v0.59.1; the
  workflow file went then, the references did not.

### Notes

- Export/import is JSON only. YAML would mean a third-party dependency, and
  Lantern's build has none outside the router, WebSocket, CORS and SQLite driver.
- Test coverage 20.8% → 29.2%, including the request-size ceilings on five
  routes and a forced mid-transaction failure proving `setMaintenanceState`
  rolls back rather than leaving the flag and the audit trail disagreeing.

### Upgrade notes

- No schema migration needed; the two new tables are created on first start.
- If you run HTTPS monitors against certificates inside 30 days of expiry, they
  will start reporting `warning`, and inside 7 days they will degrade the
  service. Raise or lower the thresholds with the two new variables.

---

## v0.61.0 — Resource bounds, correctness fixes & tighter defaults

The second half of the audit that produced v0.60.0. That release closed the
authentication holes; this one works through what was listed under Known
limitations — resource bounds, a handful of correctness bugs, and two defaults
that were more permissive than they needed to be.

### Changed — read before upgrading

Two changes tighten defaults and can affect an existing deployment:

- **WebSocket handshakes are now restricted by origin.** Accepted from the
  dashboard's own host, from anything in the new `LANTERN_WS_ALLOWED_ORIGINS`,
  and from clients sending no `Origin` header (scripts and CLIs). Previously
  every origin was accepted, so any website a user visited could open a socket
  to their Lantern and read the live feed.
- **The dashboard can no longer be framed from another origin.** A
  `Content-Security-Policy` with `frame-ancestors 'self'` and
  `X-Frame-Options: SAMEORIGIN` now ship on every response. If you embed Lantern
  in a homepage app served from a different port, set `LANTERN_FRAME_ANCESTORS`
  to that origin.

### Fixed

- **Concurrent monitor updates could orphan a goroutine.** `monitorScheduler.start`
  stopped the old ticker and stored the new one under two separate lock holds. Two
  concurrent `PUT /api/services/{name}/monitor` calls could both create a channel,
  one overwriting the other in the map — leaving a ticker that ran for the lifetime
  of the process, still writing `status_events`. The visible symptom was a deleted
  service coming back.
- **Concurrent ping checks could read each other's replies.** A raw ICMP socket
  receives every echo reply the host gets, and `checkPing` took the first one
  without checking source address, echo ID or sequence — while using a fixed
  sequence number for every probe. Four workers pinging four hosts could each
  report on whichever reply landed first, so a check against a dead host could
  report `up`. Replies are now matched to their probe.
- **The maintenance toggle could leave the flag and the audit trail disagreeing.**
  `setMaintenanceState` performed two writes with both errors discarded. It is now
  one transaction, and enabling maintenance twice no longer opens a second window
  that never closes — which had been permanently excluding real downtime from
  uptime.
- **Restarting a container recorded a status it had no basis for.** The Docker
  restart route inserted `status='up'` directly, bypassing `ingestStatusEvent` — so
  no cache invalidation, no broadcast, no flap dampening, and a green card for a
  container that had only just been asked to restart. It now goes through the
  normal write path and records `degraded`.
- **Deleting a service left its metrics cached** for up to 15 seconds, and a
  service recreated under the same name inherited the old uptime figures.
- **Schema migrations discarded every error**, making a real failure
  indistinguishable from the expected duplicate-column no-op.
- **The SPA fallback resolved `static/index.html` against the working directory**,
  so running the binary from anywhere but the repository root turned every
  client-side route into a 404 while the API kept working.

### Changed — resource bounds

- **The SQLite pool is bounded** (8 open, 4 idle, 1h lifetime). Unbounded, a single
  dashboard poll across N services could open N connections that then queued for
  the same write lock. `runStaleChecker` was also restructured to drain its result
  set before issuing per-row queries — it previously held one connection open while
  demanding a second, which is the shape that deadlocks against a bounded pool.
- **Metrics computation is capped at 8 concurrent services** rather than one
  goroutine per service.
- **Request bodies are capped**: 64 KiB for status pushes, 1 MiB for diagnostics,
  4 KiB for configuration writes. Only the auth endpoints had a limit before.
- **The webhook test call has a 10s timeout.** It used `http.DefaultClient`, which
  has none, so an endpoint that accepted the connection and never answered pinned
  the handler indefinitely.
- **The login throttle evicts stale entries.** Its map only shrank on a successful
  login or an expired lockout, so a rotating source address grew it without bound.
  Active lockouts are never evicted.
- **History export streams** instead of loading a service's entire retained history
  into memory first.
- **`computeServiceMetricsUnified` stopped building 30 status buckets that all
  three of its callers discarded** — each call was spending 30 extra maintenance
  queries producing a value nothing read, on every uncached poll, every broadcast
  and every Prometheus scrape.
- **Uptime and incident calculations load maintenance windows once** instead of
  querying per timeline segment. At `range=30d` with a 60s monitor that was on the
  order of tens of thousands of queries in one request, on a route reachable
  without credentials.

### Added

- `LANTERN_WS_ALLOWED_ORIGINS` and `LANTERN_FRAME_ANCESTORS`.
- Tests for maintenance-window equivalence, the maintenance transaction, the
  origin policy, throttle eviction, migration idempotence, the security headers,
  and body-size rejection. Coverage 17.1% → 20.8%.

### Known limitations

- The container still runs as root by default. Docker discovery needs a socket
  whose group ID varies per host, and ICMP monitors need `CAP_NET_RAW`; shipping
  non-root would break both for most users. The Dockerfile documents how to opt in.
  Note that mounting the Docker socket is already root-equivalent on the host.
- The CSP carries `'unsafe-inline'` for scripts and styles, because the dashboard
  is a single self-contained `index.html`.
- Test coverage is 20.8%. The concurrency paths above are still largely covered by
  reading rather than by tests.

### Upgrade notes

- No schema migration. No API changes.
- If your dashboard is embedded in an iframe on another origin, set
  `LANTERN_FRAME_ANCESTORS` or the frame will be blocked.
- If anything opens a WebSocket to Lantern from a different origin, add it to
  `LANTERN_WS_ALLOWED_ORIGINS`. Non-browser clients are unaffected.

---

## v0.60.0 — Sign-in, security hardening & a rebuilt dashboard

**First stable release.** Lantern gains a real sign-in gate, the dashboard has been
rebuilt on a design-token system, and a pre-release audit found and fixed several
authentication flaws. If you are running any earlier version, **upgrade promptly** — see
Security fixes below for what was reachable without credentials and
the Upgrade notes at the end of this section for what to rotate.

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
