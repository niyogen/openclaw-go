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
- `GET /logs` — event log (filterable by `level`, `component`)
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
- `POST /rpc` JSON-RPC 2.0 endpoint (`health`, `gateway.status`, `sessions.list`, `sessions.get`, `sessions.delete`, `message.send`, `plugins.list`, `models.list`, `models.capability`, `tools.list`, `tools.invoke`, `agent.run`, `approvals.list`, `approvals.decide`)
- `GET /ws` WebSocket endpoint (heartbeat + echo)
- gateway auth token support for `/sessions`, `/message`, `/rpc`, `/ws`
- configurable WebSocket origin allowlist
- `GET /plugins` plugin registry introspection endpoint
- CLI: `status`, `sessions`, `session get|delete <id>`, `message send`, `agent`, `rpc <method> [args|json]`
- model runner chain:
  - OpenAI Chat Completions (`openai` provider — `OPENAI_API_KEY`)
  - Anthropic Claude Messages API (`anthropic` / `claude` provider — `ANTHROPIC_API_KEY`)
  - automatic fallback to local echo runner
- channel dispatchers:
  - webhook outbound adapter
  - telegram outbound adapter (`TELEGRAM_BOT_TOKEN` + `TELEGRAM_CHAT_ID` or per-message `target`)
  - telegram inbound long-polling (`getUpdates`) mode
  - telegram inbound webhook mode with optional secret-token verification
  - slack outbound adapter via `chat.postMessage`
  - slack inbound Events API webhook mode with optional signature verification
  - discord outbound adapter via channel messages API
  - discord inbound webhook bridge mode with optional token header
  - teams outbound adapter via incoming webhook URL
  - teams inbound webhook bridge mode with optional token header
  - whatsapp outbound adapter via Cloud API (`/{phone_number_id}/messages`)
  - whatsapp inbound webhook mode (GET verify + POST events, optional app-secret signature check)

State is persisted at:

- `~/.openclaw-go/sessions.json`

Config is loaded from:

- `~/.openclaw-go/openclaw.json` (optional, defaults are used if missing)

Environment helpers:

- `OPENAI_API_KEY`
- `OPENAI_BASE_URL`
- `ANTHROPIC_API_KEY`
- `ANTHROPIC_BASE_URL`
- `OPENCLAW_GATEWAY_AUTH_TOKEN`
- `OPENCLAW_GATEWAY_ALLOWED_ORIGINS` (comma-separated)
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

1. Rich command tree (onboard/configure/channels/plugins)
2. Real channel adapters (Telegram/Slack/Webhooks)
3. Model backends (OpenAI/Anthropic/etc) via `AgentRunner` implementations
4. Dynamic plugin loading and capability registration
5. Gateway RPC method surface compatible with existing clients
6. Security policies (tool allow/deny, auth scopes, sandbox modes)
