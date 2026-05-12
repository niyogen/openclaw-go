# Parity execution plan — target 80%

This plan derives from the side-by-side inventory of upstream `openclaw/` (TS,
pin `95a1c915312a520c3c33d2b96943aa3e3b48a10e`) and `openclaw-go/` taken on
2026-05-11.

## Definition of "80% parity"

We measure parity by **user-facing feature value**, not by raw method count.
A user running openclaw-go to assist with messaging, tooling, and automation
should find that the platform handles the same workflows as upstream, even
if the long-tail integrations (Xiaomi, Tlon, Zalo, etc.) remain absent.

Today's coverage estimate: **~55-60%**. Closing to 80% means filling the
gaps below, in order.

## Coverage snapshot

| Surface | Upstream | openclaw-go | Gap |
|---|---|---|---|
| RPC namespaces | ~30 (cron, sessions, agent, tools, channels, secrets, config, cron, hooks, doctor, models, push, node, device, talk, voicewake, tts, models, skills, update, usage, wizard, artifacts, web, connect, system, native-hook, plugin-host-hooks, exec.approval, plugin.approval, …) | ~25 (most of the above except push, web, voicewake, plugin-host-hooks, native-hook-relay) | 5 namespaces missing |
| RPC methods | ~120 named methods | ~80 named methods | ~40 methods (mostly within partial namespaces) |
| Channels | ~25 messaging channels (Slack, Discord, Telegram, WhatsApp, Line, Teams, Signal, Mattermost, Matrix, BlueBubbles, iMessage, Nextcloud Talk, Synology Chat, IRC, Tlon, Nostr, Voice Call, Google Meet, Twitch, Email/Gmail, Feishu, QQ Bot, Zalo, Xiaomi, Zephyr, generic webhook) | 8 (Discord, Slack, Telegram, WhatsApp, Line, Teams, Nostr, generic webhook) | ~17 channels missing; only need ~4 high-value to reach 80% by user value |
| CLI commands | ~40 top-level commands incl. setup/onboard/dashboard/backup/migrate/message/commitments/tasks/security/plugins subcli/hooks subcli/etc. | ~30 top-level commands | ~10 commands missing |
| Built-in tools | Plugin-driven (dozens via bundled extensions) | 5 (`time.now`, `echo`, `sessions.count`, `sandbox.run`, `sandbox.available`) | Tool surface depends on plugin system, not core code |
| Agent runners | Per-extension model catalog | Echo / OpenAI / Anthropic / Multi | Good coverage; consider Gemini |
| Control UI | Bundled SPA served at `/ui/*` | None | Defer — frontend out of scope for Go MVP |

## P0 — Close existing partial surfaces (must-have)

These are surfaces the parity tracker already lists as "partial". Each one
has the scaffolding in place; finishing them moves the needle most.

### P0.1 — Session compaction subsystem
Upstream: `sessions.compaction.get/list/restore/branch`. We have only
`sessions.compact`. Compaction history is a core memory feature.

- Add `compaction` table/store to `internal/sessions/` (list of compaction
  events: when, before-len, after-len, snapshot path).
- New RPCs: `sessions.compaction.list`, `sessions.compaction.get`,
  `sessions.compaction.restore`, `sessions.compaction.branch`.
- Tests: history persists across restart; restore returns pre-compaction
  message log; branch forks a new session.

**Size:** ~1 day. Files: `internal/sessions/compaction.go` (new),
`internal/gateway/server.go` (dispatcher entries).

### P0.2 — OpenAI compatibility completeness
Upstream serves `/v1/models`, `/v1/chat/completions`, `/v1/embeddings`,
`/v1/responses`. We have `/v1/chat/completions` in `openai_compat.go`
(needs review of unused-params lint). Missing the model list, embeddings,
and responses endpoints.

- `GET /v1/models` — return our `KnownModels()` shaped as OpenAI model objects.
- `POST /v1/embeddings` — proxy to provider-specific embedding API
  (OpenAI direct; for Anthropic, return 501 with a clear "use OpenAI"
  hint until Anthropic ships their own embeddings).
- `POST /v1/responses` — structured-output passthrough; mirror the chat
  completions handler but emit the responses-shape JSON.
- Tests: each route returns valid OpenAI-shaped JSON for at least one
  happy-path request.

**Size:** ~1.5 days. Files: `internal/gateway/openai_compat.go`
(extend), `internal/gateway/server.go` (route registration).

### P0.3 — Push notification namespace
Upstream: `push.test`, `push.web.*` for web-push delivery to approval
flows and async runs. Missing in Go.

- `internal/push/` new package with VAPID key handling and a `Send(sub,
  payload) error` function.
- RPCs: `push.test`, `push.web.subscribe`, `push.web.unsubscribe`,
  `push.web.list`. Subscriptions persisted via `fileutil.WriteFile`
  with `0o600`.
- Wire to the approval queue so an approval request fires a web-push to
  every registered endpoint.

**Size:** ~2 days. Files: `internal/push/push.go` (new), `internal/gateway/server.go`.

### P0.4 — Web login flow
Upstream: `web.login.start`, `web.login.wait` for browser-based gateway
auth handshake. Currently CLI is bearer-token only.

- `web.login.start` returns a short URL + nonce; the user opens it; the
  server records "approved" when GET /web/login/{nonce}?ok=1 is hit.
- `web.login.wait` long-polls (or SSE) until approved or timeout.
- Issues a session token on success.

**Size:** ~1.5 days. Files: `internal/gateway/web_login.go` (new),
route registration in `server.go`.

### P0.5 — Onboard CLI flow
Upstream: `openclaw onboard` and `openclaw setup` run the wizard
RPCs interactively. We have `wizard.start/next/cancel/status` RPCs
but no CLI command driving them.

- New CLI subcommand `openclaw onboard` in `cmd/openclaw/main.go` that
  steps through `wizard.start` → loop on `wizard.next` until status==done.
- Reuse the configure flow for prompts.

**Size:** ~0.5 day. Files: `cmd/openclaw/main.go`.

### P0.6 — Hook system depth
Upstream has lifecycle/session/bootstrap/compaction/native hooks; we have
only basic `hooks.list/add/delete` (HTTP webhooks). Add at least:

- Lifecycle hooks (gateway startup/shutdown — fire registered URLs/scripts).
- Session hooks (pre-reply / post-reply firing per-session).
- Persist hook registrations under existing `hookstore` schema with a
  new `kind` field.

**Size:** ~1.5 days. Files: `internal/hookstore/hookstore.go` (extend
schema), `internal/gateway/server.go` (dispatch on lifecycle events).

## P1 — High-value missing channels

Picking the channels with biggest user impact, not the long tail.

### P1.1 — Email channel (SMTP-out)
Personal-assistant platforms live or die by email. Upstream has Gmail
hooks (`src/hooks/gmail*.ts`). Implement:

- Outbound: SMTP via `net/smtp` with TLS, configurable host/port/auth — **scoped to this only for 80% parity**.
- Inbound (IMAP IDLE / Gmail Pub/Sub) **deferred**: stdlib has no IMAP
  client; adding one (~`github.com/emersion/go-imap`) violates the
  single-dep policy, and a from-scratch RFC 3501 implementation is large
  and high-risk. Users wanting reply-via-email today should pair email
  outbound with another inbound channel (Telegram/Slack) until IMAP
  inbound is unlocked.

**Size:** ~1 day (outbound only). Files: `internal/channels/email.go` (new).

### P1.2 — Signal channel
Privacy-focused users heavily prefer Signal. Upstream uses a
JSON-RPC daemon over HTTP (`signal-cli-rest-api` style).

- Outbound: POST to local signal-cli daemon.
- Inbound: SSE subscription on `/api/v1/events`.
- Document the signal-cli sidecar requirement in README.

**Size:** ~2 days. Files: `internal/channels/signal.go` (new).

### P1.3 — Matrix channel
Federated/self-hosted chat. Use the homeserver client-server API.

- Outbound: `PUT /_matrix/client/v3/rooms/{roomId}/send/m.room.message`.
- Inbound: `/sync` long-poll or webhook bridge.

**Size:** ~2 days. Files: `internal/channels/matrix.go` (new).

### P1.4 — Mattermost channel
Enterprise self-hosted Slack alternative.

- Outbound: `POST /api/v4/posts` with bearer token.
- Inbound: outgoing-webhook HTTP POST handler.

**Size:** ~1.5 days. Files: `internal/channels/mattermost.go` (new).

## P2 — CLI command coverage

Close the most-used CLI gaps. Upstream commands missing in Go:

- `dashboard` — open Control UI in browser (no-op until UI exists; print URL).
- `backup` / `restore` — tar of config dir + sessions dir.
- `migrate` — import from upstream openclaw JSON state (best-effort).
- `message` subcommands (`send`, `read`, `reactions`, `pins`) — thin
  CLI wrappers over `message.send` + sessions RPCs.
- `commitments` / `tasks` — list pending follow-ups; reuses sessions store.
- `daemon install` / `uninstall` — systemd/launchd/Windows-service installer.

**Size:** ~3 days across the bundle. Files: `cmd/openclaw/main.go`.

## P3 — Plugin / extension architecture polish

Upstream's plugin system is rich: bundled channel entries, MCP servers,
tools, lifecycle hooks. We have a basic JSON-manifest loader. To make
the platform truly extensible:

- Channel-plugin contract: a plugin can declare a channel implementation
  via manifest + binary, loaded at runtime.
- Tool-plugin contract: a plugin can declare new tools.
- Plugin approval RPCs: `plugin.approval.list`, `plugin.approval.decide`.

Already aliased to `exec.approvals.*` in dispatch — needs the dedicated
plugin approval semantics + separate queue.

**Size:** ~3 days. Files: `internal/plugins/` (extend),
`internal/gateway/server.go`.

## Deferred items with explicit pickup triggers

These items are not in P0-P3 because their value is gated on a real-world
forcing function. Each lists what would prompt picking it up:

| Item | Pickup trigger |
|---|---|
| **VAPID web-push** | Operator reports approval-flow delivery failures in production OR the Control UI ships (see P0.3 row above). |
| **IMAP inbound for the email channel** | A user explicitly asks for email replies, OR a deployment scenario emerges where email is the only viable inbound (firewalled networks blocking bot APIs). See P1.1 row. |
| **commitments / tasks** | Upstream parity tracker calls these out, but openclaw-go users have not requested them. Pickup when a user asks for "remind me to follow up on X" or "show me what background jobs are running" semantics. Requires designing the underlying state model first. |
| **Config schema versioning + real `migrate`** | A schema-breaking change actually lands. Speculative versioning today would just add an unused field. Pickup when the first incompatible config change is proposed. |
| **Plugin architecture implementation** | A real plugin needs a contract the existing manifest doesn't cover (channel / hook / approval). The design note `docs/PLUGIN-ARCHITECTURE.md` documents the specific triggers; implementation is small slices, not a framework PR. |

## P4 — Deferred (out of 80% scope)

- Control UI (frontend SPA) — large, not blocking core functionality.
- Voice call / Google Meet / Twitch / Tlon / Xiaomi / Zalo channels — long tail.
- TTS personas/providers depth — basic `tts.convert` already there.
- iMessage / BlueBubbles — macOS-only, narrow audience.
- Talk client management — niche.

## Execution sequencing

Recommended order — each block ends in `go test ./...` green before the next:

1. **P0.1** session compaction subsystem (foundation for memory features).
2. **P0.2** OpenAI compat completeness (table-stakes for SDK clients).
3. ~~**P0.3** push notifications~~ — deferred to P4 (coupled to Control UI, blocked by single-dep policy).
4. **P0.4** web login (unblocks browser flow).
5. **P0.5** onboard CLI (UX polish).
6. **P0.6** hook system depth.
7. **P1.1** email channel.
8. **P1.2** signal channel.
9. **P1.3** matrix channel.
10. **P1.4** mattermost channel.
11. **P2** CLI command coverage.
12. **P3** plugin architecture polish.

Estimated time to 80%: ~18-22 working days at the current pace, assuming
single-developer cadence with test-after-every-change.

## Status updates

Mark items here as **done / in-progress / blocked** as work proceeds. The
canonical parity table stays in `PARITY.md`; this file is the active
execution log.

| Item | Status | Notes |
|---|---|---|
| P0.1 session compaction | **done** | landed 2026-05-11; `internal/sessions/compaction.go`, RPCs `sessions.compaction.list/get/restore/branch`, 9 tests; persists to sidecar `${path}.compactions.json` at mode 0o600 |
| P0.2 OpenAI compat completeness | **done** + real-embeddings landed 2026-05-12 | initial pass cleaned up unused `prompt` param + 5 new endpoint tests. **Follow-up landed same day**: new `agents.Embedder` interface + `OpenAIRunner.Embed()` proxying to the configured BaseURL with bearer auth; gateway's `/v1/embeddings` type-asserts the active runner to `Embedder` and uses real vectors when available, falling back to the deterministic pseudo 256-dim placeholder otherwise. 7 new agents-package tests + 2 gateway integration tests covering both branches. Endpoint default model bumped from `text-embedding-ada-002` to `text-embedding-3-small` to match current OpenAI defaults |
| P0.3 push notifications | **deferred — explicit trigger** | 2026-05-12 update: maintainer relaxed the single-dep rule, so a maintained webpush library is now allowed. Still deferred because no near-term operator forcing function: approval delivery already works through existing channels (Telegram/Slack/etc.) and web push primarily serves the deferred Control UI. **Pickup trigger:** an operator reports approval-flow delivery failures in production OR the Control UI ships. Implementation cost when picked up: add `github.com/SherClockHolmes/webpush-go` (or equivalent), add `push.*` namespace RPCs, wire to approval queue's `SetOnEnqueue`. |
| P0.4 web login | **done** | landed 2026-05-12; `internal/gateway/web_login.go` with `webLoginRegistry`; RPCs `web.login.start/wait`; HTTP routes `GET /web/login/{nonce}` (renders inline HTML confirm page) and `POST /web/login/{nonce}/confirm`; confirm requires existing auth when auth is enabled (token rotation), open during initial setup; 10 tests in `web_login_test.go` |
| P0.5 onboard CLI | **done** | landed 2026-05-12; reworked `runOnboard` in `cmd/openclaw/main.go` to accept `--provider`, `--openai-key`, `--anthropic-key`, `--gateway-token`, `--gateway-port` flags; non-destructive merge over existing config; falls through to default-write when no flags; clear next-steps message; 9 tests in `main_test.go`. Stub wizard RPCs left as-is (parity gap noted; their replacement is a deeper redesign) |
| P0.6 hook system depth | **done** | landed 2026-05-12; added 4 new event types (`gateway.started`, `gateway.stopping`, `agent.run.started`, `approval.requested`) to `hookstore`; emits wired in `server.go:Run` (started+stopping with address/version/time payload) and `agent_run.go` (started for both blocking and streaming paths); added `ApprovalQueue.SetOnEnqueue` callback so runtime fires hooks without depending on hookstore; gateway wires the callback at init; 4 new tests across hookstore + runtime |
| P1.1 email channel | **done** (SMTP-out only) | landed 2026-05-12; `internal/channels/email.go` with `EmailChannel` (host/port/user/pass/from) + `EmailDialer` interface for testability; supports implicit TLS on 465 and opportunistic STARTTLS on 587; PLAIN auth; plaintext UTF-8 body with auto-derived subject from session id + first line. 8 tests in `email_test.go` using fake dialer + dial-error propagation test. **IMAP inbound — explicit defer trigger:** 2026-05-12 update: maintainer relaxed the single-dep rule, so `github.com/emersion/go-imap` is now an option. Still deferred because no operator has reported "I need to reply via email" — they can pair with another inbound channel today. **Pickup trigger:** a user explicitly requests email replies OR a deployment scenario emerges where email is the only viable inbound (e.g., behind a firewall that blocks bot APIs). Implementation cost when picked up: add `go-imap` v2, implement an IMAP IDLE poller, parse `INBOX` messages into `InboundMessage`. |
| P1.2 signal channel | **done** (outbound) | landed 2026-05-12; `signal.go` POSTs to `/v2/send` on a configurable signal-cli-rest-api sidecar; 6 tests. Inbound deferred (poll /v1/receive or SSE — added scope, pair with another inbound channel for now) |
| P1.3 matrix channel | **done** (outbound) | landed 2026-05-12; `matrix.go` PUTs `/_matrix/client/v3/rooms/{roomId}/send/m.room.message/{txnId}` with bearer token; per-process unique txn ids; rejects room aliases (resolve first); 6 tests. Inbound (`/sync` long-poll) deferred |
| P1.4 mattermost channel | **done** (outbound) | landed 2026-05-12; `mattermost.go` POSTs `/api/v4/posts` with bearer; threading via `ThreadID → root_id`; 6 tests. Inbound (outgoing webhook from MM) deferred — users can wire MM's outgoing webhook into the generic webhook channel today |
| P2 CLI coverage | **partial+** | landed 2026-05-12: real `runBackup` (with `list` subcommand and missing-dir guard), new `runRestore <path> --yes` that merges a backup into the live data dir, AND new `dashboard` command that derives the gateway URL from config and best-effort opens it in the user's browser (Windows: rundll32, macOS: open, Linux: xdg-open). 9 tests total — 5 for backup/restore, 4 for dashboard (URL defaults, URL config, runDashboard survives launcher failure, openBrowser dispatches by GOOS). **Remaining P2 deferred**: `message`/`commitments`/`tasks` are wrappers over existing RPCs; `daemon install/uninstall` is platform-specific service-manager code, large |
| P3 plugin architecture | **design note landed, implementation pending** | `docs/PLUGIN-ARCHITECTURE.md` written 2026-05-12. Covers three contracts (channel / tool / hook), HTTP-only (no in-process Go plugins — failure modes unacceptable), pending→approved lifecycle, fire-and-forget hook delivery. **Implementation deferred** until a real plugin needs a contract the existing manifest doesn't cover. Forcing functions documented in the design note's "Pickup trigger" section |
