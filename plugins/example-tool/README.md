# example-tool — reference tool plugin

A minimal openclaw-go tool plugin. Use it as a template for your own
plugins or as a "is the wiring alive?" probe in a fresh environment.

## What it does

Three tools, all served at `:9201/tool/{name}`:

| Tool           | Args                            | Returns                           |
|----------------|---------------------------------|-----------------------------------|
| `example_echo` | any JSON object                 | `{echoed: <args>, count: <n>}`    |
| `example_now`  | none                            | `{time: "<RFC3339Nano>"}`         |
| `example_add`  | `{a: number, b: number}`        | `{sum: a+b}` or 500 + error JSON  |

`example_add` is the easiest one to break — call it with a missing arg
to see how plugin errors surface as gateway RPC errors.

## Build

From the repo root:

```bash
go build -o /tmp/example-tool.exe ./plugins/example-tool      # Linux/macOS or Git Bash on Windows
```

On Windows under NTFS-locked dirs, build into a writable location like
`/tmp` (which Git Bash maps to `%TEMP%`) — the binary won't run if it
lands somewhere the user lacks read+exec ACL.

## Manual test recipe

Five steps. Total time ~1 minute.

```bash
# 1. Set a temp data dir so this doesn't touch your real ~/.openclaw-go
export OPENCLAW_DATA_DIR="$(cygpath -w /tmp/openclaw-example-smoke 2>/dev/null || echo /tmp/openclaw-example-smoke)"
export OPENCLAW_CONFIG_PATH="$OPENCLAW_DATA_DIR/openclaw.json"
mkdir -p "$OPENCLAW_DATA_DIR/plugins/example-tool"
cp plugins/example-tool/plugin.json "$OPENCLAW_DATA_DIR/plugins/example-tool/"

# 2. Onboard + start gateway
./openclaw.exe onboard --provider echo --gateway-port 18790
./openclaw.exe gateway run > /tmp/gw.log 2>&1 &

# 3. Approve the plugin (returns a token — copy it)
./openclaw.exe plugins tool approve example-tool

# 4. Restart gateway so the approved manifest is wired
./openclaw.exe stop && ./openclaw.exe gateway run > /tmp/gw.log 2>&1 &

# 5. Start the plugin (paste the token from step 3)
OPENCLAW_PLUGIN_NAME=example-tool \
OPENCLAW_GATEWAY_URL=http://127.0.0.1:18790 \
OPENCLAW_PLUGIN_TOKEN=<paste-token> \
/tmp/example-tool.exe &
```

Then exercise the tools:

```bash
./openclaw.exe tools list                                       # sees example_*
./openclaw.exe tools invoke example_now
./openclaw.exe tools invoke example_echo '{"hello":"world"}'
./openclaw.exe tools invoke example_add  '{"a":40,"b":2}'      # -> {"sum":42}
./openclaw.exe tools invoke example_add  '{"a":1}'             # -> RPC error
```

## How registration works

1. Gateway scans `$OPENCLAW_DATA_DIR/plugins/*/plugin.json` at startup.
2. Manifests with a non-empty `tools[]` get catalogued as **pending**.
3. `openclaw plugins tool approve <name>` issues a bearer token and
   flips the plugin to **approved** (token persisted at
   `$OPENCLAW_DATA_DIR/tool-plugin-tokens.json`, mode 0o600).
4. On the **next gateway startup**, approved tools are registered with
   the in-process `ToolRegistry`. Calls to `tools.invoke <name>` POST
   to the manifest endpoint.

Step 4 is the catch: today, approval requires a gateway restart to take
effect. Hot-registration is on the roadmap (see workflow_state in repo
memory).

## Authoring your own tool plugin

The plugin code in `main.go` is ~80 lines of comments + ~30 lines of
Go. Copy it, change the tool names, and update `plugin.json` so each
tool's `endpoint` is `http://127.0.0.1:<your-port>/tool/<your-tool>`.

The `pkg/toolplugin` SDK handles all the HTTP boilerplate. Your handler
signature is just:

```go
func(ctx context.Context, args map[string]any) (any, error)
```

Return any JSON-marshallable value (the SDK wraps it in `{"result": ...}`)
or an `error` (the SDK returns 500 + `{"error": "..."}`).
