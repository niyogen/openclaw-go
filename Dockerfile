# syntax=docker/dockerfile:1
# ──────────────────────────────────────────────────────────────────────────────
# Stage 1 – build
# ──────────────────────────────────────────────────────────────────────────────
FROM golang:1.24-alpine AS builder

# Buildx injects TARGETOS / TARGETARCH automatically for multi-arch builds.
ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /src

# ── Dependency layer (cached unless go.mod/go.sum change) ────────────────────
COPY go.mod go.sum ./
RUN go mod download

# ── Source layer ──────────────────────────────────────────────────────────────
COPY . .

# ── Build ─────────────────────────────────────────────────────────────────────
RUN mkdir -p /out && \
    VERSION=$(cat VERSION 2>/dev/null || echo dev) && \
    echo "Building openclaw ${VERSION} for ${TARGETOS}/${TARGETARCH}" && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
      go build \
        -v \
        -trimpath \
        -ldflags="-s -w -X openclaw-go/internal/gateway.Version=${VERSION}" \
        -o /out/openclaw \
        ./cmd/openclaw && \
    echo "Done: $(ls -lh /out/openclaw)"

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
