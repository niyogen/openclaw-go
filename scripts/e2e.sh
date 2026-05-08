#!/usr/bin/env bash
# ── openclaw-go  end-to-end smoke test  ──────────────────────────────────────
# Starts the gateway, hits every major endpoint, then tears down.
#
# Usage:
#   ./scripts/e2e.sh                  # auto-picks a free port
#   OPENCLAW_PORT=18789 ./scripts/e2e.sh
#   OPENCLAW_TOKEN=secret ./scripts/e2e.sh
#
# Requirements: bash, curl, jq (optional – degrades gracefully without it)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PORT="${OPENCLAW_PORT:-0}"
TOKEN="${OPENCLAW_TOKEN:-}"
BINARY="${ROOT}/dist/openclaw"
PASS=0; FAIL=0

# ── helpers ──────────────────────────────────────────────────────────────────

red='\033[0;31m'; green='\033[0;32m'; cyan='\033[0;36m'; reset='\033[0m'

pass() { echo -e "  ${green}PASS${reset}  $1"; ((PASS++)); }
fail() { echo -e "  ${red}FAIL${reset}  $1 — $2"; ((FAIL++)); }

check_status() {
  local label="$1" got="$2" want="$3"
  if [[ "$got" == "$want" ]]; then pass "$label"; else fail "$label" "got $got want $want"; fi
}

get() {
  local path="$1"
  curl -s -o /dev/null -w "%{http_code}" \
    ${TOKEN:+-H "Authorization: Bearer $TOKEN"} \
    "http://127.0.0.1:${PORT}${path}"
}

post() {
  local path="$1" body="$2"
  curl -s -o /dev/null -w "%{http_code}" \
    -X POST -H "Content-Type: application/json" \
    ${TOKEN:+-H "Authorization: Bearer $TOKEN"} \
    -d "$body" \
    "http://127.0.0.1:${PORT}${path}"
}

post_body() {
  local path="$1" body="$2"
  curl -s \
    -X POST -H "Content-Type: application/json" \
    ${TOKEN:+-H "Authorization: Bearer $TOKEN"} \
    -d "$body" \
    "http://127.0.0.1:${PORT}${path}"
}

get_body() {
  local path="$1"
  curl -s \
    ${TOKEN:+-H "Authorization: Bearer $TOKEN"} \
    "http://127.0.0.1:${PORT}${path}"
}

del() {
  local path="$1"
  curl -s -o /dev/null -w "%{http_code}" \
    -X DELETE \
    ${TOKEN:+-H "Authorization: Bearer $TOKEN"} \
    "http://127.0.0.1:${PORT}${path}"
}

rpc_call() {
  local method="$1" params="${2:-{}}"
  post_body "/rpc" "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"${method}\",\"params\":${params}}"
}

# ── build (if needed) ────────────────────────────────────────────────────────

if [[ ! -x "$BINARY" ]]; then
  echo "Building binary…"
  (cd "$ROOT" && CGO_ENABLED=0 go build -o "$BINARY" ./cmd/openclaw)
fi

# ── pick free port ────────────────────────────────────────────────────────────

if [[ "$PORT" == "0" ]]; then
  PORT=$(python3 -c "import socket; s=socket.socket(); s.bind(('',0)); print(s.getsockname()[1]); s.close()" 2>/dev/null \
      || ruby -e "require 'socket'; s=TCPServer.new(0); puts s.addr[1]; s.close" 2>/dev/null \
      || echo 19999)
fi
export OPENCLAW_GATEWAY_AUTH_TOKEN="$TOKEN"

# ── start gateway ─────────────────────────────────────────────────────────────

TMP_CFG=$(mktemp -d)
# The binary reads config from $HOME/.openclaw-go/openclaw.json
mkdir -p "${TMP_CFG}/.openclaw-go"
cat > "${TMP_CFG}/.openclaw-go/openclaw.json" <<EOF
{"gateway":{"host":"127.0.0.1","port":${PORT},"authToken":"${TOKEN}"},"agent":{"provider":"echo"}}
EOF

GW_PID=""
cleanup() {
  [[ -n "$GW_PID" ]] && kill "$GW_PID" 2>/dev/null || true
  rm -rf "$TMP_CFG"
}
trap cleanup EXIT

HOME="$TMP_CFG" "$BINARY" gateway run &
GW_PID=$!

# Wait for gateway to bind (up to 10s).
for i in $(seq 1 20); do
  if curl -s -o /dev/null -f "http://127.0.0.1:${PORT}/health" 2>/dev/null; then break; fi
  sleep 0.5
done

echo ""
echo -e "${cyan}=== openclaw-go E2E smoke test  (port ${PORT}) ===${reset}"

# ── 1. Health & readiness ────────────────────────────────────────────────────

echo -e "\n${cyan}1. Health & readiness${reset}"
for path in /health /healthz /ready /readyz /v1/health /v1/healthz; do
  check_status "$path" "$(get "$path")" 200
done

# ── 2. Auth ──────────────────────────────────────────────────────────────────

echo -e "\n${cyan}2. Auth${reset}"
if [[ -n "$TOKEN" ]]; then
  unauth=$(curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:${PORT}/sessions")
  check_status "/sessions no-token → 401" "$unauth" 401
  auth=$(curl -s -o /dev/null -w "%{http_code}" \
    -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:${PORT}/sessions")
  check_status "/sessions with token → 200" "$auth" 200
else
  pass "auth skipped (no token configured)"
fi

# ── 3. Sessions ──────────────────────────────────────────────────────────────

echo -e "\n${cyan}3. Sessions${reset}"
check_status "POST /message" "$(post "/message" '{"sessionId":"sh-s","message":"hello","channel":"cli"}')" 200
check_status "GET /sessions" "$(get "/sessions")" 200
check_status "GET /sessions/sh-s" "$(get "/sessions/sh-s")" 200
check_status "GET /sessions/sh-s/history" "$(get "/sessions/sh-s/history")" 200
check_status "POST /sessions/sh-s/patch" "$(post "/sessions/sh-s/patch" '[{"index":0,"content":"patched"}]')" 200
check_status "POST /sessions/sh-s/kill" "$(post "/sessions/sh-s/kill" '{}')" 200
check_status "DELETE /sessions/sh-s" "$(del "/sessions/sh-s")" 200
check_status "GET deleted session → 404" "$(get "/sessions/sh-s")" 404

# ── 4. RPC methods ───────────────────────────────────────────────────────────

echo -e "\n${cyan}4. RPC methods${reset}"
rpc_methods=(
  "health {}"
  "gateway.status {}"
  "sessions.list {}"
  "plugins.list {}"
  "models.list {}"
  "models.capability {\"provider\":\"openai\"}"
  "tools.list {}"
  "tools.invoke {\"name\":\"echo\",\"arguments\":{\"text\":\"hi\"}}"
  "tools.invoke {\"name\":\"time.now\",\"arguments\":{}}"
  "logs.list {}"
  "cron.list {}"
  "hooks.list {}"
  "secrets.list {}"
  "approvals.list {}"
  "agent.run {\"sessionId\":\"rpc-sh\",\"message\":\"ping\"}"
  "message.send {\"sessionId\":\"rpc-msg\",\"message\":\"hi\",\"channel\":\"cli\"}"
)
for entry in "${rpc_methods[@]}"; do
  method="${entry%% *}"
  params="${entry#* }"
  result=$(rpc_call "$method" "$params")
  if echo "$result" | grep -q '"result"'; then
    pass "rpc $method"
  else
    fail "rpc $method" "$result"
  fi
done

# ── 5. Tools REST ─────────────────────────────────────────────────────────────

echo -e "\n${cyan}5. Tools REST${reset}"
check_status "GET /tools" "$(get "/tools")" 200
check_status "POST /tools/invoke echo" "$(post "/tools/invoke" '{"name":"echo","arguments":{"text":"e2e"}}')" 200
check_status "POST /tools/invoke time.now" "$(post "/tools/invoke" '{"name":"time.now","arguments":{}}')" 200
check_status "POST /tools/invoke unknown → 400" "$(post "/tools/invoke" '{"name":"no.tool","arguments":{}}')" 400

# ── 6. OpenAI-compat ─────────────────────────────────────────────────────────

echo -e "\n${cyan}6. OpenAI-compat${reset}"
check_status "GET /v1/models" "$(get "/v1/models")" 200
check_status "POST /v1/chat/completions" "$(post "/v1/chat/completions" '{"model":"echo","messages":[{"role":"user","content":"hi"}]}')" 200

# ── 7. Cron, hooks, secrets ───────────────────────────────────────────────────

echo -e "\n${cyan}7. Cron${reset}"
check_status "POST /cron" "$(post "/cron" '{"id":"sh-job","name":"j","schedule":"@every 1h","command":"echo x","enabled":true}')" 200
check_status "GET /cron" "$(get "/cron")" 200
check_status "DELETE /cron/sh-job" "$(del "/cron/sh-job")" 200

echo -e "\n${cyan}8. Hooks${reset}"
check_status "POST /hooks" "$(post "/hooks" '{"id":"sh-hook","name":"h","event":"message.received","type":"log","enabled":true}')" 200
check_status "GET /hooks" "$(get "/hooks")" 200
check_status "DELETE /hooks/sh-hook" "$(del "/hooks/sh-hook")" 200

echo -e "\n${cyan}9. Secrets${reset}"
check_status "POST /secrets" "$(post "/secrets" '{"name":"SH_KEY","value":"secret"}')" 200
check_status "GET /secrets" "$(get "/secrets")" 200
check_status "DELETE /secrets/SH_KEY" "$(del "/secrets/SH_KEY")" 200

# ── 8. Channel webhooks ──────────────────────────────────────────────────────

echo -e "\n${cyan}10. Channel webhooks${reset}"
check_status "Telegram" "$(post "/webhooks/telegram" '{"update_id":1,"message":{"text":"hi","from":{"is_bot":false},"chat":{"id":1}}}')" 200
check_status "Slack verify" "$(post "/webhooks/slack" '{"type":"url_verification","challenge":"c"}')" 200
check_status "Slack event" "$(post "/webhooks/slack" '{"type":"event_callback","event":{"type":"message","text":"hi","channel":"C1","user":"U1"}}')" 200
check_status "Discord" "$(post "/webhooks/discord" '{"content":"hi","channel_id":"D1","author":{"bot":false,"id":"U1"}}')" 200
check_status "Teams" "$(post "/webhooks/teams" '{"type":"message","text":"hi","conversation":{"id":"T1"},"from":{"id":"U1"}}')" 200
check_status "WhatsApp verify" "$(curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:${PORT}/webhooks/whatsapp?hub.mode=subscribe&hub.verify_token=&hub.challenge=123")" 200
check_status "WhatsApp message" "$(post "/webhooks/whatsapp" '{"entry":[{"changes":[{"value":{"messages":[{"type":"text","from":"1234","text":{"body":"hi"}}]}}]}]}')" 200

# ── summary ───────────────────────────────────────────────────────────────────

echo ""
echo -e "${cyan}════════════════════════════════${reset}"
echo -e "  PASS: ${green}${PASS}${reset}   FAIL: $([ $FAIL -eq 0 ] && echo -e "${green}${FAIL}${reset}" || echo -e "${red}${FAIL}${reset}")"
echo -e "${cyan}════════════════════════════════${reset}"
[[ $FAIL -eq 0 ]] && exit 0 || exit 1
