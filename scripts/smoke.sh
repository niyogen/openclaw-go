#!/usr/bin/env bash
# Quick smoke against a gateway that is already running (default http://127.0.0.1:18789).
# For a full scripted run (start + teardown), use: make e2e-sh
#
# Usage:
#   OPENCLAW_BASE_URL=http://127.0.0.1:18789 OPENCLAW_TOKEN=secret ./scripts/smoke.sh
set -euo pipefail

BASE_URL="${OPENCLAW_BASE_URL:-http://127.0.0.1:18789}"
TOKEN="${OPENCLAW_TOKEN:-}"

hdr=()
if [[ -n "$TOKEN" ]]; then
  hdr=(-H "Authorization: Bearer ${TOKEN}")
fi

echo "Smoke: GET ${BASE_URL}/health"
curl -fsS "${hdr[@]}" "${BASE_URL}/health" | head -c 400
echo

echo "Smoke: POST ${BASE_URL}/rpc (method=health)"
curl -fsS "${hdr[@]}" -X POST -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"health","params":{}}' \
  "${BASE_URL}/rpc" | head -c 400
echo

echo "Smoke: OK"
