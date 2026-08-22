# Stage 1: Build
# golang:1.22-alpine is sufficient — no C toolchain needed.
# modernc.org/sqlite is a pure-Go SQLite implementation (no CGO).
FROM golang:1.22-alpine AS builder

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

EXPOSE 7654

ENTRYPOINT ["/app/lantern"]
