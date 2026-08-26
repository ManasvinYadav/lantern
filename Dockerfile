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
