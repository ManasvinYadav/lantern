# Custom domains for the status page

Lantern has no domain setting. Serving the public status page at
`status.example.com` is a reverse-proxy job, and treating it as application
state would only duplicate — badly — what the proxy already does well.

What Lantern *does* own is the branding on that page: the name, logo, and
default accent colour, set under **Settings → General → Branding** or via
[`PUT /api/branding`](API.md). Those follow whatever hostname you point at it.

## The proxy

Point the hostname at Lantern's port (`7654` by default) and forward the
original `Host` and protocol. Caddy:

```caddyfile
status.example.com {
    reverse_proxy localhost:7654 {
        header_up X-Forwarded-Proto {scheme}
    }
}
```

nginx:

```nginx
server {
    listen 443 ssl;
    server_name status.example.com;

    location / {
        proxy_pass http://127.0.0.1:7654;
        proxy_set_header Host              $host;
        proxy_set_header X-Forwarded-Proto $scheme;

        # WebSocket upgrade — without these the live feed silently falls back
        # to polling.
        proxy_http_version 1.1;
        proxy_set_header Upgrade    $http_upgrade;
        proxy_set_header Connection "upgrade";
    }
}
```

Traefik needs no special configuration beyond a router rule; it forwards
`Host` and sets `X-Forwarded-Proto` by default.

## Serving only the public page on that hostname

`/status` is the public status page and `/api/public/*` is the data behind it.
Everything else is the dashboard. To expose *only* the status page on a public
hostname, restrict the proxy to those paths and the static assets:

```caddyfile
status.example.com {
    @public path /status /api/public/* /favicon.svg /icon.svg /manifest.json
    reverse_proxy @public localhost:7654
    redir / /status
}
```

This is defence in depth, not the primary control: `/api/public/*` is
unauthenticated by design, and every other route is gated once you have set
credentials under **Settings → Account & Security**. Set credentials either
way — an unauthenticated Lantern reachable from the internet is an open
dashboard, proxy rules or not.

## Things that change behind a proxy

- **`X-Forwarded-Proto` is required for HTTPS sessions.** Lantern marks the
  session cookie `Secure` only when the original request was HTTPS, which it
  learns from its own TLS connection or from this header. Omit it and sign-in
  works but the cookie is sent over plain HTTP too.

- **The login throttle sees the proxy, not the visitor.** Failed logins are
  rate-limited per source address, keyed on the actual TCP peer rather than
  `X-Forwarded-For` — a header any caller can set freely, which would make the
  limit trivial to sidestep. Behind a proxy that means all sign-in attempts
  share one bucket. That errs on the safe side, but if the proxy itself is
  public, put its own rate limiting in front of `/api/auth/login`.

- **Audit log IPs are the proxy's**, for the same reason. If you need the
  original client address in the log, log it at the proxy.

- **WebSockets need a matching `Host`.** The handshake is accepted when the
  `Origin` host matches the request `Host`. Forward `Host` as above and this
  just works. If your proxy rewrites `Host`, list the browser-visible origins
  explicitly:

  ```
  LANTERN_WS_ALLOWED_ORIGINS=https://status.example.com,https://dash.example.com
  ```

- **Embedding in another page** is blocked cross-origin by
  `frame-ancestors 'self'`. To embed the status page in a homepage app on a
  different origin, set `LANTERN_FRAME_ANCESTORS` to a space-separated CSP
  source list, e.g. `LANTERN_FRAME_ANCESTORS="'self' https://home.example.com"`.

- **A branding logo on another host is allowed automatically.** The CSP is
  `img-src 'self' data:` plus the origin of whatever `logo_url` you configure,
  and nothing else — so the logo loads without opening the page up to every
  image host on the internet.

## Multiple hostnames

One Lantern can serve any number of hostnames; nothing in it is bound to a
domain. The branding is global, though, so every hostname shows the same name
and logo. Two differently-branded status pages means two Lantern instances.
