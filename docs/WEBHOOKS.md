# Webhook Integrations

Lantern notifies Discord, Telegram, Gotify, and any generic JSON endpoint when a service
transitions between `up`, `down` and `degraded`.

Set a provider's environment variable, or save its URL from **Settings → Alerts &
Webhooks**, which stores it in the database and takes precedence over the environment.
The Settings drawer also sends a test message and shows the delivery log.

> **These URLs are credentials.** A Discord webhook URL lets anyone who has it post to your
> channel, and a Telegram URL embeds your bot token outright. `GET /api/webhooks` returns
> them in full, so that route requires authentication as of v0.60.0 — it was reachable
> anonymously when only `LANTERN_AUTH_TOKEN` was configured. It is refused to
> [viewers](CONFIG.md#users--roles) and to per-service scoped tokens, and every change to
> it is recorded in the [audit log](CONFIG.md#audit-log) by channel *name* only, never the
> URL itself. They are also included in any database snapshot from `GET /api/backup`. Do
> not commit them, and rotate any that have been exposed.

## Providers

### 1. Discord
Create a Webhook in your Discord Server Settings -> Integrations -> Webhooks.
```env
LANTERN_WEBHOOK_DISCORD=https://discord.com/api/webhooks/YOUR_ID/YOUR_TOKEN
```

### 2. Telegram
Create a bot via `@BotFather` and retrieve your chat ID.
```env
LANTERN_WEBHOOK_TELEGRAM=https://api.telegram.org/bot<YOUR_BOT_TOKEN>/sendMessage?chat_id=<YOUR_CHAT_ID>
```

### 3. Gotify
Create a new Application in Gotify and copy the App Token.
```env
LANTERN_WEBHOOK_GOTIFY=https://gotify.yourdomain.com/message?token=<YOUR_APP_TOKEN>
```

### 4. Generic Webhook
For a custom backend, or any automation platform that accepts an incoming webhook,
Lantern can send standard JSON `POST` requests.
```env
LANTERN_WEBHOOK_GENERIC=https://your-domain.com/webhook/lantern
```

**Generic Payload Format:**
```json
{
  "service": "api-gateway",
  "old": "up",
  "new": "down",
  "message": "Connection refused"
}
```

## When Lantern actually sends

Four things sit between a status change and a notification. In order:

1. **Maintenance mode.** A service in maintenance never notifies, whether the toggle was
   flipped by hand or the service is inside a scheduled maintenance window.

2. **Flap dampening.** A `down` alert requires **two consecutive** `down` checks, and
   fires once per outage rather than once per check. A single bad check produces no
   traffic at all — no down alert, and no recovery alert immediately after it. A recovery
   only fires if the outage it ends was itself announced.

3. **[Quiet hours](CONFIG.md#notification-quiet-hours).** Inside the configured window,
   alerts are either dropped (`mute`) or queued and sent as one combined message per
   channel when the window closes (`digest`).

4. **[Per-service alert routing](CONFIG.md#per-service-alert-routing).** A service can
   send its alerts to a subset of the configured channels. A service with no route
   configured alerts on every channel, so existing installs behave exactly as before.
   Digests respect this too — each channel's digest contains only the events routed to it.

Every attempt, successful or not, is written to the delivery log with its HTTP status and
error text, readable at **Settings → Alerts & Webhooks** or
`GET /api/webhooks/deliveries`. Dispatch runs on a bounded worker pool with per-request
timeouts, so a slow or unreachable endpoint never delays status ingestion.

## Digest messages

A digest is one message per channel summarising everything that happened while quiet hours
were open, in the order it happened. Discord digests are embeds; because Discord caps an
embed at 25 fields, a longer digest is truncated with a trailing "…N more event(s) not
shown" field rather than being rejected outright.

The queue is drained transactionally — a digest that fails to send is not silently lost,
and a digest that succeeds is not sent twice. Turning quiet hours off, or switching from
`digest` to `mute`, still flushes whatever is already queued rather than stranding it.
