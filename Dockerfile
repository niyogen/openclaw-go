# syntax=docker/dockerfile:1
# ──────────────────────────────────────────────────────────────────────────────
# Stage 1 – build
# ──────────────────────────────────────────────────────────────────────────────
FROM golang:1.24-alpine AS builder

# Install git so `go mod download` can fetch VCS-backed modules if needed.
RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /src

# Cache dependency downloads separately from the main build.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build a fully static binary (CGO disabled for Alpine portability).
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
      -trimpath \
      -ldflags="-s -w -X openclaw-go/internal/gateway.Version=$(cat VERSION 2>/dev/null || echo dev)" \
      -o /out/openclaw \
      ./cmd/openclaw

# ──────────────────────────────────────────────────────────────────────────────
# Stage 2 – minimal runtime image
# ──────────────────────────────────────────────────────────────────────────────
FROM scratch

# Pull in TLS roots and timezone data from the builder.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

COPY --from=builder /out/openclaw /openclaw

# Data dir that will be mounted as a volume in production.
VOLUME ["/data"]

# Gateway HTTP port.
EXPOSE 18789

ENV OPENCLAW_DATA_DIR=/data

ENTRYPOINT ["/openclaw"]
CMD ["gateway"]
