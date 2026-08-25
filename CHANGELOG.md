# Changelog

All notable changes to Lantern are documented here.

Lantern is **beta** software: the API and database schema may still change between
releases. Pin a version tag rather than `latest`, and take [backups](docs/BACKUP.md).

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
