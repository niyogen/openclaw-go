# syntax=docker/dockerfile:1
# ──────────────────────────────────────────────────────────────────────────────
# Stage 1 – build
# ──────────────────────────────────────────────────────────────────────────────
FROM golang:1.24-alpine AS builder

# Buildx injects TARGETOS / TARGETARCH automatically.
ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /src

# Cache dependency downloads separately from the main build.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN VERSION=$(cat VERSION 2>/dev/null || echo dev) && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -ldflags="-s -w -X openclaw-go/internal/gateway.Version=${VERSION}" \
      -o /out/openclaw \
      ./cmd/openclaw

# ──────────────────────────────────────────────────────────────────────────────
# Stage 2 – minimal runtime image
# ──────────────────────────────────────────────────────────────────────────────
FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /out/openclaw /openclaw

VOLUME ["/data"]
EXPOSE 18789
ENV OPENCLAW_DATA_DIR=/data

ENTRYPOINT ["/openclaw"]
CMD ["gateway"]
