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
make docker-run     # runs gateway, mounts ~/.openclaw-go as /data
```

Or directly:

```bash
docker build -t openclaw-go .
docker run --rm -p 18789:18789 \
  -v "$HOME/.openclaw-go:/data" \
  -e OPENAI_API_KEY \
  openclaw-go
```

### Run tests

```bash
make test           # go test -race ./...
```

Integration (optional; uses a temp config file and live `api.github.com`):

```bash
go test -tags=integration -count=1 -timeout 120s -p 1 ./...
```

Quick smoke (expects gateway already listening on `OPENCLAW_BASE_URL`, default `http://127.0.0.1:18789`):

```bash
bash scripts/smoke.sh   # Linux/macOS/Git Bash — set OPENCLAW_TOKEN if auth is enabled
```

In another terminal:

```bash
go run ./cmd/openclaw onboard
go run ./cmd/openclaw doctor
go run ./cmd/openclaw status
go run ./cmd/openclaw rpc health
go run ./cmd/openclaw agent "hello there"
go run ./cmd/openclaw sessions
```

## Next parity milestones

1. **CLI** — extend `configure` for more gateway fields (shutdown timeout, trusted proxies, plugins path); richer `channels` / `plugins` subcommands.
2. **Multi-node** — `node.invoke` forwards JSON-RPC with **retries** on transport errors and HTTP 408/429/5xx (exponential backoff, up to 4 attempts). A **per-peer circuit breaker** opens after **5** consecutive failed invokes (excluding param/marshal errors like bad URL), stays open for **30s**, then allows a **half-open** trial; success closes the circuit. Prometheus **`/metrics`** exposes `openclaw_node_invoke_*` (success/failure/circuit_open counts, duration sum/count) and `openclaw_node_circuit_open` per peer. Declare peers in **`openclaw.json` `nodes`** (`enabled`, `id`, `name`, `url`, `apiKey`): on gateway startup and on **`SIGHUP`** reload, entries with `enabled: true` and a non-empty `url` are upserted into **topology** (stable `cfg-…` id when `id` is omitted); `enabled: false` removes the same logical peer. Still on the roadmap: streaming proxy and optional config knobs for circuit thresholds.
3. **Channels** — deeper parity (attachments, edits, reactions, threads) per provider; outbound `replyToMessageId` for Telegram is supported on `OutboundMessage`.
4. **Observability** — optional OpenTelemetry exporter; histogram latencies on `/metrics` if you add `prometheus/client_golang` or similar.
5. **Persistence** — optional Postgres/Redis backends for sessions and HA deployments.
6. **Updates** — `update.status` / `update.run` and `openclaw doctor` query GitHub for the latest release tag; fully automated binary install remains out of scope (package manager / CI deploy).
