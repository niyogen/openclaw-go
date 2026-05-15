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
| Last reviewed | 2026-05-16 (studio integration shipped) |

*Pin captured from the sibling checkout under SurfClaw; re-run `git rev-parse HEAD` in upstream when refreshing.*

## 2026-05-16 update: studio-integration parity

openclaw-studio drives openclaw-go through the new `/control/ws`
upstream-compat endpoint. End-to-end verified: bootstrap, agent
fleet, chat round-trip, session settings, config get/set/patch,
models, cron CRUD. **21 of 21 gateway methods studio calls have
real adapters** (no METHOD_NOT_FOUND on any studio operation).
Coverage proof: 63 Go unit tests + 8 Playwright integration tests.

Capability catalog: `docs/STUDIO-CAPABILITY-MATRIX.md`.

## Status summary

| Area | openclaw-go | Notes |
|------|-------------|--------|
| Gateway control plane | **partial** (broad) | JSON-RPC + push + web.login + plugin.approval + plugins.tool/hook + sessions.compaction added across v0.3.x → v0.5.0; voicewake + native-hook-relay namespaces still missing |
| Workspace / onboarding | **partial** | `openclaw onboard` ships flag-driven (non-interactive); interactive wizard CLI still missing |
| Clients (mobile / Canvas / voice) | **missing** | Out of scope for Go MVP; voice scope B planned for v0.6.0 |
| Channel count | **partial** | 12 channels (Discord, Slack, Telegram, WhatsApp, Line, Teams, Nostr, Signal, Matrix, Mattermost, Email w/ IMAP, generic webhook); outbound complete, bidirectional gap on Signal/Matrix/Mattermost |
| Plugin architecture | **done** | v0.5.0 — channel + tool + hook plugin contracts shipped with HTTP-only out-of-process model; Telegram and WhatsApp migrated |

## Detailed rows

| Area | Item | Status |
|------|------|--------|
| Gateway | HTTP `/health`, `/metrics`, `/logs`, `/logs/stream` | done |
| Gateway | HTTP `/rpc` JSON-RPC 2.0 core methods | partial (broad — see PARITY-PLAN.md coverage snapshot) |
| Gateway | OpenAI-compatible `/v1/models`, `/v1/chat/completions`, `/v1/embeddings`, `/v1/responses` | done (real OpenAI embeddings via `agents.Embedder`) |
| Gateway | WebSocket `/ws` | partial |
| Gateway | Auth (Bearer, token query, Basic, trusted proxies, constant-time compare, XFF safety) | done |
| Gateway | Request body size limits (JSON routes + webhooks) | done |
| Gateway | Web-push (VAPID) + `push.*` namespace | done (v0.4.0) |
| Gateway | Web login (`web.login.start/wait`) + browser confirm page | done (v0.4.0) |
| Sessions | File-backed store, list/get/delete, patch, model override | done |
| Sessions | Compaction subsystem (list/get/restore/branch) | done (v0.3.x) |
| Agent | `/agent/run`, `/agent/run/stream`, tool loop + approvals | partial |
| Runtime | Tool registry, MCP HTTP, config skills | partial |
| Cron / hooks / secrets / topology | HTTP + persistence | partial |
| Hooks | Lifecycle events (gateway.started/stopping, agent.run.started, approval.requested) | done |
| Hooks | Plugin-driven hook delivery contract | done (v0.5.0) |
| Plugins | Channel / tool / hook plugin contracts (HTTP-only, scan→approve→dispatch) | done (v0.5.0) |
| Plugins | Telegram + WhatsApp run as out-of-process plugins behind config flag | done (v0.5.0) |
| CLI | `gateway`, `configure`, `doctor`, `rpc`, sessions CRUD, onboard (flag-form), dashboard, backup/restore, web-login, compaction, daemon install/path, plugins subcli, message subcli | done |
| CLI | Interactive wizard `onboard` (stub-driven) | missing |
| CLI | `commitments`, `tasks`, `migrate` | missing |
| Channels | Telegram polling + webhook + secret + plugin | done (plugin in v0.5.0) |
| Channels | WhatsApp Cloud API webhook + verify token + plugin (outbound) | done (plugin in v0.5.0) |
| Channels | Slack / Discord / Teams / LINE webhooks | done |
| Channels | Email SMTP-out + IMAP inbound | done (v0.4.0) |
| Channels | Signal / Matrix / Mattermost (outbound) | done (v0.4.0) |
| Channels | Signal / Matrix / Mattermost (inbound) | missing |
| Channels | Upstream long-tail (BlueBubbles, iMessage, NCT, Synology, IRC, Tlon, Twitch, Feishu, QQ, Zalo, Xiaomi, Zephyr) | missing (low value) |
| Ops | Prometheus metrics, request IDs | done |
| Ops | Upstream-style daemon installer (systemd / launchd; Windows rejected) | done |
| Product | Personal assistant UX (wizard, apps) | partial — minimal embedded UI panel; full SPA out of scope |

**Note:** `openclaw-go` is an independent implementation; parity follows product priorities, not automatic 1:1 copying.

## Remaining known gaps (as of 2026-05-16)

Prioritized by impact:

### High impact — should close before SaaS launch

1. **Lifecycle-correct chat events** (run tracker). The studio
   transcript currently relies on a `presence` → summary-refresh
   approximation for live updates. Page reloads always work, but
   in-page live updates can lag 2-20s. A real run tracker
   (`agent.run.started` → `chat` deltas → `chat` final with
   matching runId) would close this. ~1-2 days. **Tracked as
   primary v0.5.x follow-up.**

2. **`config.get` secret redaction.** Returns API keys + tokens
   unredacted to anything connected to `/control/ws`. Fine for
   local-loopback only; required before any non-loopback deploy
   (i.e., SaaS). ~0.5 days — need a redaction layer that
   round-trips safely with `config.patch`.

3. **Per-tenant data isolation.** Currently a single data dir.
   SaaS needs per-account directories (or schema). Significant
   refactor (~3-5 days) of the data plane. Touches sessions,
   workspace, cron, hookstore, every persistent subsystem.

### Medium impact — close opportunistically

4. **Interactive onboard wizard** — current `onboard` is
   flag-driven only. A TUI flow would help self-service install.
5. **Signal/Matrix/Mattermost inbound** — outbound complete;
   inbound is webhook/polling work per channel.
6. **`commitments`, `tasks` CLI surfaces** — upstream concepts
   we don't model yet. Need a data design pass first.
7. **`migrate` CLI** — upstream's data-migration tool. Probably
   nice-to-have rather than blocking.

### Low impact — explicit skips

8. **Upstream channel long-tail** (BlueBubbles, iMessage, NCT,
   Synology, IRC, Tlon, Twitch, Feishu, QQ, Zalo, Xiaomi,
   Zephyr) — low value, deferred indefinitely.
9. **Voice / TTS / STT** — Voice scope B planned for v0.6.0,
   separate release.
10. **Mobile apps + Canvas + 3D office** — explicitly out of
    scope for openclaw-go core. Would be separate consumer apps.

### Percentage estimate

Counting by user-facing feature surface (not method count or LoC):
- **~92% parity** with upstream openclaw as of 2026-05-16
- Gaps to 100% are items 1-7 above
- 100% is realistically achievable in ~2-3 weeks of focused work
  AFTER per-tenant isolation (which is the SaaS-blocking item)
