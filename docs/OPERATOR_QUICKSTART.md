# Operator quickstart (HTTP + Telegram + WhatsApp + new channels)

Use this path when you want the **“power loop”** running locally or on a small VPS: gateway → session → model → reply → outbound channel.

## 1. Build and config

```bash
go build -o openclaw ./cmd/openclaw

# Scriptable form — sets provider, keys, gateway token in one shot:
./openclaw onboard --provider openai --openai-key sk-... --gateway-token mysecret

# Or the bare form, just writes a default config you can edit:
./openclaw onboard
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

## 4a. Email (SMTP outbound)

Outbound-only channel. Use it for daily summaries, alerts, async notifications.
Inbound (IMAP) is not implemented — pair with another channel for replies.

```bash
./openclaw configure email enable true
./openclaw configure email host smtp.gmail.com
./openclaw configure email port 587            # or 465 for implicit TLS
./openclaw configure email user bot@you.com
./openclaw configure email password app-pw     # app-passwords preferred
./openclaw configure email from bot@you.com    # defaults to user when empty
```

The gateway negotiates STARTTLS on 587 when the server advertises it; port 465
uses implicit TLS. PLAIN auth. Body is plaintext UTF-8 with auto-derived
`[sessionId] firstLine` subject.

## 4b. Signal (via signal-cli-rest-api sidecar)

Run the [signal-cli-rest-api](https://github.com/bbernhard/signal-cli-rest-api)
container alongside your gateway, register the bot's Signal number, then:

```bash
./openclaw configure signal enable true
./openclaw configure signal baseurl http://127.0.0.1:8080  # the sidecar URL
./openclaw configure signal number +15551234567            # bot's own number
```

The gateway POSTs to `{baseurl}/v2/send` with `{recipients, number, message}`.
Inbound (receive) deferred — wire signal-cli's own webhook into the generic
webhook channel for now, or pair Signal-out with another inbound channel.

## 4c. Matrix

```bash
./openclaw configure matrix enable true
./openclaw configure matrix baseurl https://matrix.example.com
./openclaw configure matrix token syt_...        # bot's Matrix access token
```

Outbound only. Target must be a **room id** (`!opaque:server`) — aliases
(`#general:server`) are rejected. Resolve aliases via the homeserver's
`/_matrix/client/v3/directory/room/{alias}` endpoint first.

## 4d. Mattermost

```bash
./openclaw configure mattermost enable true
./openclaw configure mattermost baseurl https://mm.example.com
./openclaw configure mattermost token tok-...    # personal access token / bot token
```

Outbound only. The gateway POSTs to `/api/v4/posts` with `{channel_id, message, root_id?}`.
Threading: set `OutboundMessage.ThreadID` to the root post id and it lands as `root_id`.
For inbound from Mattermost, wire its outgoing-webhook feature into the
generic webhook channel today.

## 5. Operational CLI

```bash
./openclaw dashboard            # print gateway URL + open in browser
./openclaw web-login            # device-code-style approval flow → bearer token
./openclaw daemon install       # writes systemd unit (Linux) or launchd plist (macOS)
./openclaw daemon path          # show where the unit file goes
./openclaw backup               # tar-style copy of ~/.openclaw-go
./openclaw backup list          # list existing backups
./openclaw restore ~/.openclaw-go.backup-... --yes
./openclaw compaction list sess-123
./openclaw compaction restore <id> --yes
./openclaw message history sess-123
./openclaw message dispatch matrix '!room:server' 'hello'
```

## 6. Sanity checks

```bash
./openclaw doctor
go test ./...
```

For a running instance, use **`make smoke`** or **`scripts/smoke.sh`** (curl checks) if configured.

## 7. Hitting ~80% readiness

With HTTP + Telegram + WhatsApp webhooks hardened (body limits, verify token, inbound error metrics), focus **product** time on: real traffic on one channel, `/agent/run` with your tools/MCP, and monitoring **`openclaw_gateway_channel_inbound_errors_total`** plus **`GET /logs/stream`**.

See **[PARITY.md](./PARITY.md)** for a pinned comparison to upstream OpenClaw,
**[PARITY-PLAN.md](./PARITY-PLAN.md)** for the active plan with explicit
pickup triggers for deferred items (web push, IMAP inbound, plugin
architecture, commitments/tasks, config migrations), and
**[../SECURITY.md](../SECURITY.md)** for the disclosure policy.
