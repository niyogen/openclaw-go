# Operator quickstart (HTTP + Telegram + WhatsApp)

Use this path when you want the **“power loop”** running locally or on a small VPS: gateway → session → model → reply → outbound channel.

## 1. Build and config

```bash
go build -o openclaw ./cmd/openclaw
./openclaw config init
```

Set provider keys via env or `openclaw.json` (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, …). Use **`OPENCLAW_CONFIG_PATH`** for a non-default config file.

## 2. HTTP control plane (always available)

Start the gateway:

```bash
./openclaw gateway
```

- **Simple reply (single turn, no tool executor):** `POST /message` with JSON `{ "sessionId", "message", "channel", "target" }`.
- **Agent with tools / policy:** `POST /agent/run` or `POST /agent/run/stream` (same JSON shape as documented in the root README).
- **Observe:** `GET /health`, `GET /metrics`, `GET /logs`, `GET /logs/stream` — use `X-Request-ID` for correlation.

Protect untrusted networks: set **`gateway.authToken`** (or password) in config.

## 3. Telegram

1. Set `TELEGRAM_BOT_TOKEN` (and optional `TELEGRAM_CHAT_ID` for default outbound).
2. `openclaw configure telegram enable true`
3. **Webhook:** `openclaw configure telegram inbound-mode webhook`, set public URL with `openclaw configure telegram webhook set https://host/webhooks/telegram`, optional `TELEGRAM_WEBHOOK_SECRET`.
4. **Long polling:** leave inbound mode as `polling` (default) — no public URL required.

Webhook POST bodies are capped at **4 MiB** (same order as gateway JSON routes).

## 4. WhatsApp (Cloud API)

1. Set `WHATSAPP_ACCESS_TOKEN`, `WHATSAPP_PHONE_NUMBER_ID`, `WHATSAPP_TO_NUMBER` as needed.
2. **`WHATSAPP_VERIFY_TOKEN`** (or `channels.whatsapp.verifyToken`) is **required** when WhatsApp is enabled — the gateway **refuses to start** without it.
3. Optional: `WHATSAPP_APP_SECRET` for signature verification on inbound POSTs.
4. `openclaw configure whatsapp enable true` and point Meta’s webhook to your `channels.whatsapp.webhookPath` (default `/webhooks/whatsapp`).

## 5. Sanity checks

```bash
./openclaw doctor
go test ./...
```

For a running instance, use **`make smoke`** or **`scripts/smoke.sh`** (curl checks) if configured.

## 6. Hitting ~80% readiness

With HTTP + Telegram + WhatsApp webhooks hardened (body limits, verify token, inbound error metrics), focus **product** time on: real traffic on one channel, `/agent/run` with your tools/MCP, and monitoring **`openclaw_gateway_channel_inbound_errors_total`** plus **`GET /logs/stream`**.

See **[PARITY.md](./PARITY.md)** for a pinned comparison to upstream OpenClaw.
