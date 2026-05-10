# Parity checklist (reference OpenClaw)

Use this file to track **feature parity** against a **pinned** upstream OpenClaw revision.

## How to use

1. Record the reference repo URL and **exact git tag or commit SHA** you are comparing to.
2. Walk RPC methods, CLI commands, and channel behaviors; mark each row **done**, **partial**, or **missing**.
3. Update the pin when you intentionally target a newer upstream release.

## Reference pin

| Field | Value |
|--------|--------|
| Upstream repository | https://github.com/openclaw/openclaw |
| Pin (commit SHA) | `95a1c915312a520c3c33d2b96943aa3e3b48a10e` |
| Last reviewed | 2026-05-10 |

*Pin captured from the sibling checkout under SurfClaw; re-run `git rev-parse HEAD` in upstream when refreshing.*

## Status summary

| Area | openclaw-go | Notes |
|------|-------------|--------|
| Gateway control plane | **partial** | JSON-RPC, sessions, tools, agent run, metrics, tracing — narrower surface than upstream TS gateway |
| Workspace / onboarding | **missing** | No `openclaw onboard` equivalent; CLI `configure` + `config init` only |
| Clients (mobile / Canvas / voice) | **missing** | Out of scope for Go MVP |
| Channel count | **partial** | Subset; HTTP bridge + Telegram/WhatsApp/Slack/Discord/Teams/Line/Nostr |

## Detailed rows

| Area | Item | Status |
|------|------|--------|
| Gateway | HTTP `/health`, `/metrics`, `/logs`, `/logs/stream` | done |
| Gateway | HTTP `/rpc` JSON-RPC 2.0 core methods | partial |
| Gateway | OpenAI-compatible `/v1/chat/completions` (if present in tree) | partial |
| Gateway | WebSocket `/ws` | partial |
| Gateway | Auth (Bearer, token query, Basic, trusted proxies) | partial |
| Gateway | Request body size limits (JSON routes + webhooks) | done |
| Sessions | File-backed store, list/get/delete, patch, model override | partial |
| Agent | `/agent/run`, `/agent/run/stream`, tool loop + approvals | partial |
| Runtime | Tool registry, MCP HTTP, config skills | partial |
| Cron / hooks / secrets / topology | HTTP + persistence | partial |
| CLI | `gateway`, `configure`, `doctor`, `rpc`, sessions CRUD | partial |
| CLI | Full upstream command parity | missing |
| Channels | Telegram polling + webhook + secret | partial |
| Channels | WhatsApp Cloud API webhook + verify token | partial |
| Channels | Slack / Discord / Teams / LINE webhooks | partial |
| Channels | Upstream “all channels” list | missing |
| Ops | Prometheus metrics, request IDs | partial |
| Ops | Upstream-style daemon installer | missing |
| Product | Personal assistant UX (wizard, apps) | missing |

**Note:** `openclaw-go` is an independent implementation; parity follows product priorities, not automatic 1:1 copying.
