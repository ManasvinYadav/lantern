# Webhook Integrations

Lantern natively supports external notifications when a service transitions between `up`, `down`, or `degraded`.

To enable a provider, set its corresponding Environment Variable in your `.env` or `docker-compose.yml`.
You can also save URLs from **Settings → Webhooks**, which stores them in the database and
takes precedence over the environment.

> **These URLs are credentials.** A Discord webhook URL lets anyone who has it post to your
> channel, and a Telegram URL embeds your bot token outright. `GET /api/webhooks` returns
> them in full, so that route requires authentication as of v0.60.0 — it was reachable
> anonymously when only `LANTERN_AUTH_TOKEN` was configured. They are also included in any
> database snapshot from `GET /api/backup`. Do not commit them, and rotate any that have
> been exposed.

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
