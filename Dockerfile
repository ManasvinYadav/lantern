# Stage 1: Build
# golang:1.25-alpine matches the go directive in go.mod (raised when
# golang.org/x/crypto was added for bcrypt password hashing).
# It is sufficient — no C toolchain needed.
# modernc.org/sqlite is a pure-Go SQLite implementation (no CGO).
FROM golang:1.25-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o lantern .

# Stage 2: Final
#
# No USER directive: the container runs as root, deliberately.
#
# Lantern's two optional integrations both need it. Native Docker discovery
# talks to /var/run/docker.sock, whose owning group ID differs from host to
# host, so a fixed non-root UID/GID in the image would fail on most machines.
# ICMP monitors need CAP_NET_RAW, which Docker grants to root by default and
# not otherwise. Shipping non-root would quietly break both for most users.
#
# If you do not use Docker discovery or ping monitors, running non-root costs
# nothing — add to your compose service:
#
#   user: "1000:1000"
#
# and if you want ping monitors back, add:
#
#   cap_add: ["NET_RAW"]
#
# Note that mounting the Docker socket is already equivalent to granting root
# on the host, so a non-root USER buys little while discovery is enabled.
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /app/lantern .
COPY static/ ./static/

# Create data directory
RUN mkdir -p /data

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD wget -qO- http://localhost:7654/api/health || exit 1

EXPOSE 7654

ENTRYPOINT ["/app/lantern"]
