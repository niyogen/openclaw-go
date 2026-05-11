# OpenClaw-Go

> A from-scratch Go implementation of the OpenClaw agent platform.
> © 2026 Prageeth Mahendra Gunathilaka — MIT License

This repository is a from-scratch Go implementation inspired by OpenClaw's core architecture:

- gateway as the control plane
- CLI-first operator workflow
- file-backed session history
- agent runner abstraction
- plugin/channel contracts

## Current scope

Implemented MVP:

- `openclaw onboard` / `openclaw config init` to write starter config
- `openclaw config show` (redacts known secret fields)
- `openclaw configure gateway auth-token <token>`
- `openclaw configure gateway allowed-origins <csv>`
- `openclaw configure gateway metrics-require-auth <true|false>`
- `openclaw configure set-agent-provider <echo|openai|anthropic>`
- `openclaw models [<provider>]` — list known models (optionally filtered by provider)
- `openclaw capability [<provider>]` — show provider features and model ids
- `openclaw infer <message>` — single-turn inference via configured provider
- `openclaw configure telegram inbound-mode <polling|webhook>`
- `openclaw configure telegram enable <true|false>`
- `openclaw configure telegram webhook set <public-base-url>` (calls Telegram `setWebhook`)
- `openclaw configure slack enable <true|false>`
- `openclaw configure slack inbound-mode <webhook>`
- `openclaw configure slack webhook-path <path>`
- `openclaw configure discord enable <true|false>`
- `openclaw configure discord inbound-mode <webhook>`
- `openclaw configure discord webhook-path <path>`
- `openclaw configure teams enable <true|false>`
- `openclaw configure teams inbound-mode <webhook>`
- `openclaw configure teams webhook-path <path>`
- `openclaw configure whatsapp enable <true|false>`
- `openclaw configure whatsapp inbound-mode <webhook>`
- `openclaw configure whatsapp webhook-path <path>`
- `openclaw doctor` for quick local health/config checks
- `openclaw gateway` / `openclaw gateway run` starts an HTTP gateway server
- **`GET /ui/`** — embedded **control panel** (health, gateway status, session/cron/hook counts, log preview, quick infer via JSON-RPC `message.send`); **`GET /`** redirects to **`/ui/`** (UI routes use the same gateway auth as `/rpc` when a token or password is configured — the static page does not inject **`Authorization`** headers, so for locked-down gateways use the CLI or curl with a Bearer token)
- `GET /health` for status checks (includes `version`)
- `GET /metrics` — Prometheus text metrics (uptime, heap, goroutines, RPC / channel inbound / agent run / dispatch-error counters); **public by default** so scrapers do not need a token
- `GET /logs` — event log (filterable by `level`, `component`)
- `GET /logs/stream` — Server-Sent Events stream of recent log lines plus live appends (same `level` / `component` query filters as `GET /logs`)
- `GET|POST /cron` — list / add cron jobs
- `DELETE /cron/{id}` — remove a cron job
- `GET|POST /hooks` — list / add event hooks
- `DELETE /hooks/{id}` — remove a hook
- `GET|POST /secrets` — list secret metadata / set a secret
- `DELETE /secrets/{name}` — remove a secret
- `POST /agent/run` — run agent with exec policy + approval-queue support
- `GET /approvals` — list pending approval requests
- `POST /approvals/{id}/decide` — approve or reject a pending tool call
- `GET /tools` to list available gateway tools
- `POST /tools/invoke` to invoke a tool by name + arguments
- `GET /sessions` to list sessions
- `GET /sessions/{id}` to fetch one session
- `DELETE /sessions/{id}` to remove a session
- `POST /message` to append user/assistant turns
- `POST /rpc` JSON-RPC 2.0 endpoint (`health`, `gateway.status` — includes `metricsRequireAuth`, `update.status`, `update.run`, `tracing.status`, `node.invoke` to forward RPC to a registered peer, `sessions.list`, `sessions.get`, `sessions.delete`, `message.send`, `plugins.list`, `models.list`, `models.capability`, `tools.list`, `tools.invoke`, `agent.run`, `approvals.list`, `approvals.decide`)
- `GET /ws` WebSocket endpoint (heartbeat + echo)
- gateway auth token support for `/sessions`, `/message`, `/rpc`, `/ws` (and for `GET /metrics` when `gateway.metricsRequireAuth` is `true` in config — use Prometheus `authorization` / `bearer_token` on the scrape job)
- gateway request tracing: middleware sets `X-Request-ID` on every response; reuse a client-provided `X-Request-ID` (sanitized, max 128 chars) for log correlation
- `GET /plugins` plugin registry introspection endpoint
- CLI: `status`, `sessions`, `session get|delete <id>`, `message send`, `agent`, `rpc <method> [args|json]`
- model runner chain:
  - OpenAI Chat Completions (`openai` provider — `OPENAI_API_KEY`)
  - Anthropic Claude Messages API (`anthropic` / `claude` provider — `ANTHROPIC_API_KEY`)
  - automatic fallback to local echo runner
- **Config-driven tools (openclaw.json)**:
  - **`skills[]`** — enabled entries with `name` + `endpoint` register gateway tools as `skill.<name>` (HTTP POST JSON `{ "skill", "arguments" }` to the endpoint).
  - **`mcp[]`** — enabled entries with `url` register remote MCP tools over **HTTP JSON-RPC** as `mcp.<server>.<tool>` (`initialize` + `notifications/initialized` + `tools/list` at startup; optional `apiKey` as Bearer; `Mcp-Session-Id` echoed when present).
- **Memory (`memory` in config)** — `maxMessages` (if set) overrides `gateway.maxMessages` for the session store cap. `compactAfter` with `summarizeOnCompact: false` trims oldest persisted messages after each append past the threshold. With `summarizeOnCompact: true`, after successful **`/agent/run`**, **`/agent/run/stream`**, **`agent.run`**, and **`agents.run`** RPC, older turns are summarized via the session runner and replaced with a leading `[Memory summary] …` system message (fallback: hard trim).
- channel dispatchers:
  - webhook outbound adapter
  - telegram outbound adapter (`TELEGRAM_BOT_TOKEN` + `TELEGRAM_CHAT_ID` or per-message `target`)
  - telegram inbound long-polling (`getUpdates`) mode
  - telegram inbound webhook mode with optional secret-token verification (webhook POST bodies capped at 4 MiB)
  - slack outbound adapter via `chat.postMessage`
  - slack inbound Events API webhook mode with optional signature verification (webhook POST bodies capped at 4 MiB)
  - discord outbound adapter via channel messages API
  - discord inbound webhook bridge mode with optional token header (webhook POST bodies capped at 4 MiB)
  - teams outbound adapter via incoming webhook URL
  - teams inbound webhook bridge mode with optional token header (webhook POST bodies capped at 4 MiB)
  - whatsapp outbound adapter via Cloud API (`/{phone_number_id}/messages`)
  - whatsapp inbound webhook mode (GET verify + POST events, optional app-secret signature check); when **`channels.whatsapp.enabled`** is `true`, **`WHATSAPP_VERIFY_TOKEN`** / **`channels.whatsapp.verifyToken`** is **required** (gateway refuses to start otherwise). Webhook POST bodies are capped at 4 MiB (same order as other JSON routes).

State is persisted at:

- `~/.openclaw-go/sessions.json`

Config is loaded from:

- `~/.openclaw-go/openclaw.json` (optional, defaults are used if missing), unless **`OPENCLAW_CONFIG_PATH`** points at another file
- Optional gateway field `metricsRequireAuth` (`gateway.metricsRequireAuth` in JSON): when `true` and `gateway.authToken` or `gateway.password` is set, `GET /metrics` uses the same auth rules as other protected routes (Bearer, `X-OpenClaw-Token`, `?token=`, HTTP Basic, trusted proxies). If auth is not configured, the flag has no effect and a warning is logged. Applies on startup and on `SIGHUP` config reload.

Environment helpers:

- `OPENAI_API_KEY`
- `OPENAI_BASE_URL`
- `ANTHROPIC_API_KEY`
- `ANTHROPIC_BASE_URL`
- `OPENCLAW_GATEWAY_AUTH_TOKEN`
- `OPENCLAW_GATEWAY_ALLOWED_ORIGINS` (comma-separated)
- `OPENCLAW_CONFIG_PATH` (path to `openclaw.json`; overrides `~/.openclaw-go/openclaw.json` for tests and multiple profiles)
- `OPENCLAW_DATA_DIR` (directory for `sessions.json`, topology/logs/cron/hooks/secrets stores, and default `plugins/` when `gateway.pluginsDir` is unset; Docker image defaults to `/data`)
- `OPENCLAW_GATEWAY_HOST` (bind address — use **`0.0.0.0`** in containers so published ports work)
- `TELEGRAM_BOT_TOKEN`
- `TELEGRAM_CHAT_ID`
- `TELEGRAM_WEBHOOK_SECRET`
- `SLACK_BOT_TOKEN`
- `SLACK_CHANNEL_ID`
- `SLACK_SIGNING_SECRET`
- `DISCORD_BOT_TOKEN`
- `DISCORD_CHANNEL_ID`
- `DISCORD_WEBHOOK_TOKEN`
- `TEAMS_OUTBOUND_WEBHOOK_URL`
- `TEAMS_WEBHOOK_SECRET`
- `WHATSAPP_ACCESS_TOKEN`
- `WHATSAPP_PHONE_NUMBER_ID`
- `WHATSAPP_TO_NUMBER`
- `WHATSAPP_VERIFY_TOKEN`
- `WHATSAPP_APP_SECRET`

## Production checklist

Before exposing the gateway on a network:

- Set **`gateway.authToken`** and/or **`gateway.password`**; avoid running with no auth on untrusted networks.
- If Prometheus scrapes **`/metrics`**, set **`gateway.metricsRequireAuth`** and use scrape auth when the gateway is reachable beyond your mesh.
- Configure **`gateway.allowedOrigins`** for browser clients; restrict **`trustedProxies`** if you use `X-Forwarded-For` for auth decisions.
- Back up **`~/.openclaw-go/`** (sessions, secrets, cron, hooks, topology) or your **`OPENCLAW_CONFIG_PATH`** directory on a schedule.
- Run **`go test ./...`** (and optionally **`go test -tags=integration ./...`**) before releases; use **`make e2e`** or **`./scripts/smoke.sh`** against a running instance for a quick sanity check.

See [docs/PARITY.md](docs/PARITY.md) for a pinned parity checklist vs upstream OpenClaw, and **[docs/OPERATOR_QUICKSTART.md](docs/OPERATOR_QUICKSTART.md)** for HTTP + Telegram + WhatsApp setup.

## Run and test capabilities

Use this flow to **run the gateway locally** and **prove each layer** (health → model → sessions → tools/agent → observability → automated tests).

### Prerequisites

- **Go** toolchain matching [`go.mod`](go.mod) (currently **1.22+**).
- **First-time config** (writes `~/.openclaw-go/openclaw.json` unless `OPENCLAW_CONFIG_PATH` is set):

  ```bash
  go run ./cmd/openclaw config init
  ```

- **Provider** — for tests without API keys, use the built-in echo runner:

  ```bash
  go run ./cmd/openclaw configure set-agent-provider echo
  ```

  For real inference, switch to `openai` or `anthropic` / `claude` and set **`OPENAI_API_KEY`** / **`ANTHROPIC_API_KEY`** (or embed keys in config — prefer env vars).

### 1. Start the gateway

Terminal **A**:

```bash
go run ./cmd/openclaw gateway
```

Default URL is **`http://127.0.0.1:18789`** (from `gateway.host` / `gateway.port` in config). If you set **`gateway.authToken`**, every protected request below needs **`Authorization: Bearer <token>`**, **`X-OpenClaw-Token`**, **`?token=`**, or HTTP Basic as documented for your setup.

### 2. Sanity checks (terminal B)

```bash
curl -s http://127.0.0.1:18789/health
go run ./cmd/openclaw doctor
go run ./cmd/openclaw rpc health
go run ./cmd/openclaw status
```

### 3. Model inference (CLI)

Single-turn call through the configured runner:

```bash
go run ./cmd/openclaw infer "What is 2+2?"
```

Higher-level exercise that talks to the gateway’s agent path (still useful with **`echo`**):

```bash
go run ./cmd/openclaw agent "Reply with one short greeting."
```

### 4. Sessions and `/message`

```bash
go run ./cmd/openclaw message send demo-session "Hello from CLI"
go run ./cmd/openclaw sessions
go run ./cmd/openclaw session get demo-session
```

Raw HTTP equivalent:

```bash
curl -s -X POST http://127.0.0.1:18789/message \
  -H "Content-Type: application/json" \
  -d '{"sessionId":"demo-http","channel":"cli","message":"Hello via curl"}'
```

### 5. Tools and agent loop (`/agent/run`)

List tools:

```bash
curl -s http://127.0.0.1:18789/tools
go run ./cmd/openclaw rpc tools.list
```

Blocking agent run (tools + policy — behavior depends on provider and registered tools):

```bash
curl -s -X POST http://127.0.0.1:18789/agent/run \
  -H "Content-Type: application/json" \
  -d '{"sessionId":"agent-demo","message":"Say hello in one sentence."}'
```

Streaming variant: **`POST /agent/run/stream`** (SSE). With auth, add the same headers you use for `/message`.

### 6. Observability

```bash
curl -s http://127.0.0.1:18789/metrics | head -50
curl -s "http://127.0.0.1:18789/logs?level=info"
```

Live log tail (**Server-Sent Events**):

```bash
curl -N "http://127.0.0.1:18789/logs/stream?level=info"
```

Responses include **`X-Request-ID`** for correlation with log lines.

### 7. Automated tests (from clone root)

```bash
go test ./... -count=1 -timeout 120s
```

**Race detector** (matches Linux/macOS CI — requires cgo there):

```bash
go test -p 1 -count=1 -timeout 120s -race ./...
```

**Integration tag** (extra tests; CI runs this on **Linux + Go 1.24**):

```bash
go test -tags=integration -p 1 -count=1 -timeout 120s ./...
```

**End-to-end** package (starts gateway in-process):

```bash
go test ./e2e/... -count=1 -timeout 90s -v
# or: make e2e   # builds dist/openclaw then runs the same package (see Makefile)
```

### 8. Smoke script (gateway already running)

[`scripts/smoke.sh`](scripts/smoke.sh) hits **`/health`** and **`/rpc`** with `curl`:

```bash
bash scripts/smoke.sh
# optional:
OPENCLAW_BASE_URL=http://127.0.0.1:18789 OPENCLAW_TOKEN=your-token bash scripts/smoke.sh
```

On **Windows**, use **Git Bash**, **WSL**, or run the same `curl` commands manually. For an in-repo scripted check without Bash, **`make e2e-ps`** runs [`scripts/e2e.ps1`](scripts/e2e.ps1) (requires PowerShell).

### 9. Channels (Telegram, WhatsApp, HTTP bridging)

Step-by-step env vars, webhooks, and safety notes: **[docs/OPERATOR_QUICKSTART.md](docs/OPERATOR_QUICKSTART.md)**.

---

## Build & Run

### Quick start (local)

```bash
go run ./cmd/openclaw gateway
```

### Build binary

```bash
make build          # → dist/openclaw
make run-gateway    # hot-start with -race
```

### Cross-compile release artefacts

```bash
make release        # Linux/macOS/Windows × amd64/arm64 → dist/release/
```

### Docker

```bash
make docker-build   # builds openclaw-go:latest
make docker-run     # bind-mount ~/.openclaw-go → /data, OPENCLAW_DATA_DIR + listen 0.0.0.0
```

Or directly:

```bash
docker build -t openclaw-go .
docker run --rm -p 18789:18789 \
  -v "$HOME/.openclaw-go:/data" \
  -e OPENCLAW_DATA_DIR=/data \
  -e OPENCLAW_CONFIG_PATH=/data/openclaw.json \
  -e OPENCLAW_GATEWAY_HOST=0.0.0.0 \
  -e OPENAI_API_KEY \
  openclaw-go
```

### Docker Compose (gateway + smoke + tests)

[`compose.yaml`](compose.yaml) runs the gateway with a named volume (`openclaw_data` → `/data`), **`OPENCLAW_GATEWAY_HOST=0.0.0.0`**, and **`OPENCLAW_CONFIG_PATH=/data/openclaw.json`**. On first start there is no config file yet — defaults apply (**echo** provider). Drop a prepared `openclaw.json` into the volume or bind-mount a host file over `/data/openclaw.json` if you need custom settings.

```bash
# Foreground gateway only (after image exists):
docker compose up --build

# Or via Makefile (builds image first):
make compose-up

# One-shot capability check: starts gateway + curl container (health + JSON-RPC health):
docker compose --profile smoke up --build --abort-on-container-exit --exit-code-from smoke

# Same via Makefile:
make compose-smoke

# Full Go test suite in a Linux + Go 1.24 container (bind-mounts the repo; no race flag):
docker compose --profile test run --rm test
# or: make compose-test

# Optional integration tests (extra packages; hits live network where tests require it):
OPENCLAW_INTEGRATION_TESTS=1 docker compose --profile test run --rm test
# or: make compose-test-integration
```

Optional: create **`openclaw-go/.env`** with `OPENAI_API_KEY`, `OPENCLAW_GATEWAY_AUTH_TOKEN`, etc. Compose substitutes `${VAR:-}` from your environment or `.env`. With auth enabled, pass the same token to clients (see smoke container’s `OPENCLAW_TOKEN` wiring).

After **`docker compose up`**, from the host open **`http://127.0.0.1:18789/ui/`** for the control panel (or **`http://127.0.0.1:18789/`**, which redirects there). Use **`OPENCLAW_PORT`** if you remap the published port.

### Run tests

```bash
make test           # go test -race ./... with a short timeout (see Makefile)
```

Full **manual + CI-style** checks (race, integration, e2e, smoke) are documented in **[Run and test capabilities](#run-and-test-capabilities)** above.

## Next parity milestones

1. **CLI** — extend `configure` for more gateway fields (shutdown timeout, trusted proxies, plugins path); richer `channels` / `plugins` subcommands.
2. **Multi-node** — `node.invoke` forwards JSON-RPC with **retries** on transport errors and HTTP 408/429/5xx (exponential backoff, up to 4 attempts). A **per-peer circuit breaker** opens after **5** consecutive failed invokes (excluding param/marshal errors like bad URL), stays open for **30s**, then allows a **half-open** trial; success closes the circuit. Prometheus **`/metrics`** exposes `openclaw_node_invoke_*` (success/failure/circuit_open counts, duration sum/count) and `openclaw_node_circuit_open` per peer. Declare peers in **`openclaw.json` `nodes`** (`enabled`, `id`, `name`, `url`, `apiKey`): on gateway startup and on **`SIGHUP`** reload, entries with `enabled: true` and a non-empty `url` are upserted into **topology** (stable `cfg-…` id when `id` is omitted); `enabled: false` removes the same logical peer. Still on the roadmap: streaming proxy and optional config knobs for circuit thresholds.
3. **Channels** — deeper parity (attachments, edits, reactions, threads) per provider; outbound `replyToMessageId` for Telegram is supported on `OutboundMessage`.
4. **Observability** — optional OpenTelemetry exporter; histogram latencies on `/metrics` if you add `prometheus/client_golang` or similar.
5. **Persistence** — optional Postgres/Redis backends for sessions and HA deployments.
6. **Updates** — `update.status` / `update.run` and `openclaw doctor` query GitHub for the latest release tag; fully automated binary install remains out of scope (package manager / CI deploy).
