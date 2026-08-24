# Lantern

Lantern is a zero-dependency, ultra-lightweight status dashboard and monitoring server built in Go. It provides a sleek, single-page application (SPA) UI with dark/light themes and a completely push-based "heartbeat" API for tracking service uptimes, downtimes, and maintenance windows.

It uses a `modernc.org/sqlite` backend (no CGO required, cross-compiles easily) and compiles down to a single tiny binary or a lightweight Docker container.

![Lantern Grid View](docs/screenshots/grid-view.png)

## Features (v0.4.0)

- **Push-based Heartbeats**: Services report their own status. No active scraping means Lantern can run securely behind NATs or on the edge.
- **Service Grouping**: Organize services into custom groups (e.g. `media`, `networking`, `monitoring`) with dedicated filtering on the dashboard.
- **Docker Management**: Integrated Docker controls for container restart and live log tails from the service detail drawer (guarded behind admin auth).
- **Service Detail & Metadata Inspector**: Real-time inspection of container ports, image tags, IP addresses, network topology, and health check history.
- **Background Stale Detection**: Automatically marks services as "Down" (Stale) if they miss their heartbeat window (configurable via `LANTERN_STALE_HOURS`).
- **Web-based Webhook Configuration**: Dedicated Settings drawer to configure, save, and test Discord, Telegram, Gotify, and generic webhooks with real-time test output.
- **Theming & Dynamic Accent Picker**: Dark, Midnight, and Light themes with customizable accent colors that reactively update across the entire dashboard and service drawers.
- **Instant Search & Real-time Filters**: Instant substring search across service names, groups, and messages with empty-state and toast notifications.
- **Maintenance Mode**: One-click UI toggles to silence alerts and ignore downtime during planned maintenance.
- **Scoped API Tokens**: Secure ingest and admin management APIs using Bearer tokens while keeping public status routes unauthenticated.

## Screenshots

<table>
  <tr>
    <td><img src="docs/screenshots/grid-view.png" alt="Grid View" width="400"/></td>
    <td><img src="docs/screenshots/list-view.png" alt="List View" width="400"/></td>
  </tr>
  <tr>
    <td><img src="docs/screenshots/webhooks-modal.png" alt="Webhooks Modal" width="400"/></td>
    <td><img src="docs/screenshots/diagnostics-drawer.png" alt="Diagnostics Drawer" width="400"/></td>
  </tr>
</table>

## Quick Start (Docker)

The easiest way to get started is with Docker Compose.

```yaml
services:
  lantern:
    image: ghcr.io/manasvinyadav/lantern:latest
    container_name: lantern
    restart: unless-stopped
    ports:
      - "7654:7654"
    volumes:
      - lantern_data:/data
    environment:
      # Secure your API
      - LANTERN_AUTH_TOKEN=your_secret_token
      # Configure Notifications
      - LANTERN_WEBHOOK_DISCORD=https://discord.com/api/webhooks/...

volumes:
  lantern_data:
```

```bash
docker compose up -d
```

Access the dashboard at `http://localhost:7654/`.

## Documentation Suite

Lantern is highly configurable. Dive into the detailed guides below:

- 📖 **[API Reference](docs/API.md)**: Full list of endpoints, payloads, and examples.
- ⚙️ **[Configuration Guide](docs/CONFIG.md)**: Detailed breakdown of all environment variables, auth, and database settings.
- 🔔 **[Webhooks & Notifications](docs/WEBHOOKS.md)**: Setup guides for Discord, Telegram, Gotify, and generic integrations.

## Quick API Example

Pushing a heartbeat is as simple as a single POST request:

```bash
curl -X POST http://localhost:7654/api/status \
  -H "Authorization: Bearer your_secret_token" \
  -H "Content-Type: application/json" \
  -d '{
    "service_name": "database",
    "status": "up",
    "message": "Latency normal"
  }'
```

## Building from Source

Lantern can be natively compiled into a standalone binary:

```bash
go mod download
go build -ldflags="-s -w" -o lantern .
./lantern
```

## License
MIT License
