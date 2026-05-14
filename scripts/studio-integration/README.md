# Studio integration scripts

These scripts validate that openclaw-studio (or any upstream openclaw
frontend) can drive openclaw-go through the `/control/ws` endpoint.
They're intentionally outside `internal/gateway/...` because they
require a real running gateway and a Node WebSocket client (or a
Playwright browser) — they're not unit tests.

## Prerequisites

- `node` 22.x available on `$PATH`
- An openclaw-go gateway running on a known port (default `:18789`)
- A clone of `openclaw-studio` at a sibling path with its
  `node_modules` installed (for the chat round-trip script, which
  uses `playwright` from studio's deps)

## Scripts

### `screenshot-integration.mjs`

Headless-Chromium navigates studio, waits for bootstrap, captures
a screenshot + lists the runtime/intent API calls fired. Good for
"did studio's UI render against my gateway?" smoke checks.

```bash
# From the openclaw-go-studio clone:
node /path/to/openclaw-go/scripts/studio-integration/screenshot-integration.mjs /tmp/studio.png
```

Env:
- `STUDIO_URL` — defaults to `http://127.0.0.1:4001`. Adjust if you
  run studio on a different port.

### `chat-roundtrip-integration.mjs`

Drives studio's chat UI: navigates, types a message, clicks Send,
waits for the echo reply to render. Asserts the full
`chat.send → processMessage → chat.history → UI` loop works.

```bash
node /path/to/openclaw-go/scripts/studio-integration/chat-roundtrip-integration.mjs /tmp/chat.png
```

Exits 0 on success. The script also reloads the page once if the
echo reply didn't surface live — confirms the message was stored
even when the live-event path is still warming up.

## Manual end-to-end setup

A full integration run (gateway + studio + smoke):

```bash
# 1. Build openclaw-go.
cd /path/to/openclaw-go && go build -o ./openclaw.exe ./cmd/openclaw

# 2. Set up a temp data dir for the smoke (don't pollute ~/.openclaw-go).
SMOKE=$(mktemp -d -t openclaw-integ-XXXX)
export OPENCLAW_DATA_DIR="$SMOKE"
export OPENCLAW_CONFIG_PATH="$SMOKE/openclaw.json"

# 3. Onboard the gateway on a non-default port.
./openclaw.exe onboard --provider echo --gateway-port 28789 --gateway-token integtok

# 4. Start the gateway in the background.
./openclaw.exe gateway run > /tmp/gw.log 2>&1 &

# 5. Configure studio to point at the gateway.
mkdir -p "$SMOKE/studio-state/openclaw-studio"
cat > "$SMOKE/studio-state/openclaw-studio/settings.json" <<EOF
{
  "version": 1,
  "gateway": {"url": "ws://localhost:28789/control/ws", "token": "integtok"},
  "gatewayAutoStart": true,
  "focused": {},
  "avatars": {}
}
EOF

# 6. Start studio on a non-default port.
cd /path/to/openclaw-go-studio
PORT=4001 OPENCLAW_STATE_DIR="$SMOKE/studio-state" HOSTNAME=127.0.0.1 npm run dev > /tmp/studio.log 2>&1 &

# 7. (Optional) Create a default agent so the UI has something to render.
cd /path/to/openclaw-go
./openclaw.exe rpc agents.create '{"id":"main","name":"Main agent","provider":"echo","model":"echo"}'

# 8. Once studio is responding (give it ~20s), point a browser at it.
open http://127.0.0.1:4001
```

The browser should show a "Connected" status pill, the agent panel
populated, and a working chat input. Sending a message returns the
echo reply (on first send, page may need to be reloaded to surface
the reply until the live-event path is finalized — see PARITY.md).
