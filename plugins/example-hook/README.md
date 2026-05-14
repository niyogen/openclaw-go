# example-hook — reference hook plugin

A minimal openclaw-go hook plugin. Subscribes to nine gateway lifecycle
events and logs each envelope to stderr. Useful as a probe ("did my
event fire?") and as a template for real audit / notification plugins.

## Events subscribed

| Event                | When it fires                                         |
|----------------------|-------------------------------------------------------|
| `gateway.started`    | Once at gateway boot (right after listener is up)     |
| `gateway.stopping`   | Once on shutdown (before HTTP listener closes)        |
| `session.created`    | A new session-id was seen for the first time          |
| `message.received`   | Inbound message arrived (via `/message`, channel, …)  |
| `message.sent`       | Outbound reply written                                |
| `agent.run.started`  | Agent run started (sync or streaming endpoint)        |
| `agent.run.complete` | Agent run finished                                    |
| `tool.invoked`       | A registered tool returned                            |
| `approval.requested` | A tool/action queued for human approval               |

The plugin listens on `:9301`. Each event has its own URL path under
`/hook/...` so log lines clearly identify which event fired.

## Build

```bash
go build -o /tmp/example-hook.exe ./plugins/example-hook
```

(On Windows under NTFS-locked dirs, `/tmp` maps to `%TEMP%` via Git
Bash and is writable + executable.)

## Manual test recipe

```bash
# 1. Temp data dir (same one as example-tool, if you're running both)
export OPENCLAW_DATA_DIR="$(cygpath -w /tmp/openclaw-example-smoke 2>/dev/null || echo /tmp/openclaw-example-smoke)"
export OPENCLAW_CONFIG_PATH="$OPENCLAW_DATA_DIR/openclaw.json"
mkdir -p "$OPENCLAW_DATA_DIR/plugins/example-hook"
cp plugins/example-hook/plugin.json "$OPENCLAW_DATA_DIR/plugins/example-hook/"

# 2. Onboard + start gateway (skip if already done for example-tool)
./openclaw.exe onboard --provider echo --gateway-port 18790
./openclaw.exe gateway run > /tmp/gw.log 2>&1 &

# 3. Approve the plugin (copy the token)
./openclaw.exe plugins hook approve example-hook

# 4. Restart gateway so the listener is installed
./openclaw.exe stop && ./openclaw.exe gateway run > /tmp/gw.log 2>&1 &

# 5. Start the plugin
OPENCLAW_PLUGIN_NAME=example-hook \
OPENCLAW_GATEWAY_URL=http://127.0.0.1:18790 \
OPENCLAW_PLUGIN_TOKEN=<paste-token> \
/tmp/example-hook.exe > /tmp/example-hook.log 2>&1 &

# 6. Trigger events
curl -s -X POST -H "Content-Type: application/json" \
  -d '{"sessionId":"smoke-1","message":"hello"}' \
  http://127.0.0.1:18790/message
./openclaw.exe stop                                       # fires gateway.stopping

# 7. Watch the events arrive
tail -20 /tmp/example-hook.log
```

Expected output:

```
event=session.created   payload={"channel":"cli","sessionId":"smoke-1"}
event=message.received  payload={"channel":"cli","message":"hello","sessionId":"smoke-1"}
event=message.sent      payload={"channel":"cli","reply":"Echo: hello","sessionId":"smoke-1"}
event=gateway.stopping  payload={"address":"127.0.0.1:18790","time":"..."}
```

## How registration works

Same shape as the tool plugin:

1. Gateway scans `$OPENCLAW_DATA_DIR/plugins/*/plugin.json`.
2. Manifests with non-empty `hooks[]` get catalogued as **pending**.
3. `openclaw plugins hook approve <name>` issues a bearer token and
   flips the plugin to **approved** (persisted at
   `$OPENCLAW_DATA_DIR/hook-plugin-tokens.json`, mode 0o600).
4. On **next gateway startup**, the gateway installs an EventListener
   that POSTs `{event, payload, timestamp}` to each declared endpoint
   when the matching event fires.

## Semantics worth knowing

- **Fire-and-forget.** The gateway does NOT retry on 5xx, NOT wait for
  a response body, NOT block other listeners on a slow plugin. At-most-
  once delivery is intentional (duplicate "approval requested" pings
  are worse than a missed one).
- **10s client timeout.** Slow handler → logged + dropped.
- **Snapshot at gateway start.** Re-approving a plugin while the
  gateway is running requires a restart to pick it up (matches the
  tool-plugin posture).
- **Per-iteration env at boot.** The very first event most plugins see
  is `gateway.started` — and it can fire before the plugin's HTTP
  server is listening. Don't rely on it for first-time-init; use a
  `setup` step in your plugin's `main()` instead.

## Authoring your own hook plugin

The plugin code is ~30 lines. The `pkg/hookplugin` SDK handles all the
HTTP boilerplate. Your handler signature is just:

```go
func(ctx context.Context, env hookplugin.Envelope)
```

`env.Event`, `env.Payload`, and `env.Timestamp` carry everything the
gateway sent. Handler panics are recovered by the SDK; the plugin
process survives a bad event.
