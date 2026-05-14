# Changelog

All notable changes to openclaw-go are recorded here.

This file follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project aims to follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Upstream-compatible WebSocket endpoint** at `/control/ws`
  (`internal/gateway/control_ws.go`). Speaks the upstream openclaw
  protocol shape (`connect.challenge` → `req method=connect` → `res
  ok=true`, then `{type:"req"|"res"|"event", id, method, params,
  payload, ok, error}` framing) so frontends written against upstream
  openclaw — openclaw-studio, openclaw-nerve, native apps — can
  connect to openclaw-go unmodified. Phase 1 of the
  upstream-protocol-compat work: connect handshake + `wake` heartbeat
  wired; every other method returns `METHOD_NOT_FOUND` until later
  phases register handlers. Grants all 5 operator scopes
  unconditionally (scope-enforced authz is a separate parity item).
  Coexists with the existing `/ws` endpoint; existing clients
  unaffected.
- **Reference example plugins**: `plugins/example-tool/` and
  `plugins/example-hook/`. Tiny runnable plugins (~30 lines of Go each,
  plus manifest + README) demonstrating the `pkg/toolplugin` and
  `pkg/hookplugin` SDKs end-to-end. Smoke-testable against a real
  gateway in ~1 minute following the recipe in each plugin's README.
  Closes one of the v0.5.x follow-ups deferred at v0.5.0 cut.

### Fixed

- **`statusRecorder` in `internal/gateway/trace.go` now forwards
  `http.Hijacker`** and exposes `Unwrap()`. Previously the trace
  middleware wrapped the response writer in a type that did not
  implement `http.Hijacker`, which silently broke WebSocket upgrades
  through the production handler chain — gorilla's upgrader returns
  HTTP 500 when the writer is not hijackable. Existing unit tests
  served from `s.mux` directly so the bug never surfaced in CI; live
  smoke testing of the new `/control/ws` endpoint exposed it.

## [0.5.0] - 2026-05-12

"Plugin architecture." Lifts the previously-deferred P3 work to a
first-class extension surface: channel, tool, and hook plugins all
share the same scan → approve → dispatch shape with per-plugin tokens
persisted at 0o600. Built-in Telegram and WhatsApp channels now run
as plugins behind a config flag (built-in path retained until plugin
reaches parity — no big-bang switch). Commits on the branch:
`a3aa8c5`, `ffa86fb`, `b5f5ae4`, `efdab34`, `d977a2b`, `ffffe06`,
`72c2d7a`.

### Added

- **Channel-plugin runtime** (`a3aa8c5`, iter 1/4). New
  `internal/plugins/runtime.go` introduces `ChannelPluginRegistry`
  scanning `$dataDir/plugins/*/plugin.json` and a gateway-side
  `pluginChannel` adapter that forwards outbound messages over HTTP
  to the plugin process. Inbound handler at
  `/plugins/{name}/inbound` validates the per-plugin token. New RPCs
  `plugin.approval.list/approve/revoke` (mirrors the approval-queue
  shape). New `pkg/channelplugin/` SDK: plugin authors implement
  `Send(ctx, OutboundMessage)` and the SDK handles HTTP, token
  loading, graceful shutdown. Tokens persisted to
  `$dataDir/channel-plugin-tokens.json` at 0o600.
- **Telegram migrated to channel plugin** (`ffa86fb`, iter 2/4).
  Polling-mode only in v1; webhook-mode plugin deferred. Plugin
  binary at `plugins/telegram/`, listens on :9101 by convention.
  Reuses `internal/channels.TelegramChannel` + `TelegramPoller` as
  a library (Path α from PLUGIN-ARCHITECTURE.md). Config flag
  `channels.telegram.usePlugin` (default false) toggles in-process vs
  out-of-process; gateway-side register block is skipped when true.
  11 tests in `plugins/telegram/runner_test.go`.
- **WhatsApp migrated to channel plugin — outbound only** (`efdab34`,
  iter 3/4). Inbound stays at the gateway because WhatsApp inbound is
  webhook-only (Meta-driven public URL, no polling equivalent).
  `UsePlugin` gates only the outbound register block; the inbound
  webhook handler at `cfg.Channels.WhatsApp.WebhookPath` is mounted
  unconditionally. Plugin listens on :9102. Config flag
  `channels.whatsapp.usePlugin` + new CLI `openclaw configure
  whatsapp use-plugin <true|false>`. 10 tests.
- **Tool-plugin contract** (`d977a2b`, iter 4a/4). The existing
  `Manifest.Tools[]` was data-only (logged at startup, never
  registered). Iter 4a wires it. `ToolPluginRegistry` mirrors the
  channel-plugin pattern; tokens at
  `$dataDir/tool-plugin-tokens.json`. `pkg/toolplugin/` SDK exposes
  `Plugin.RegisterTool(name, handler)` with `/tool/{name}` routing.
  `Server.Tools()` accessor exposed so cmd/openclaw can register
  approved tools with the gateway `ToolRegistry`. 3 RPCs
  (`plugins.tool.list/approve/revoke`). CLI `openclaw plugins tool
  list|approve|revoke <name>`. 29 tests across runtime + SDK + RPC.
- **Hook-plugin contract** (`ffffe06`, iter 4b/4). Plugins subscribe
  to hookstore events via `hooks[]` in plugin.json; gateway POSTs
  `{event, payload, timestamp RFC3339}` to each declared endpoint on
  matching events. Fire-and-forget, 10s client timeout, no retries.
  Key infra: new `hookstore.EventListener` type + `Store.AddListener`
  + `Emit` fan-out (listeners are in-memory only, not persisted to
  the hook JSON file, so plugin-hook subscriptions reload from
  manifest each restart). `NewPluginHookDispatcher(approved)`
  builds an event → []endpoint index at construction for O(matching)
  dispatch. `pkg/hookplugin/` SDK with `Plugin.HandlePath`; SDK
  recovers from handler panics so one bad event doesn't kill the
  plugin process. 3 RPCs (`plugins.hook.list/approve/revoke`). CLI
  `openclaw plugins hook ...`. 29 tests.

### Changed

- `internal/hookstore` gained the `EventListener` extension surface
  alongside the existing persisted-Hook model. Operator-visible
  hooks and plugin-derived hooks stay cleanly separated.
- `internal/gateway/server.go` exposes `Server.Tools()`,
  `Server.HookStore()`, and `SetHookPluginRegistry` accessors so
  `cmd/openclaw` can wire plugin registries from outside the package
  without violating layering.

### Fixed

- **Race on `tp.server` in Telegram plugin test** (`b5f5ae4`). CI
  race detector flagged `TestListenShutsDownOnCtxCancel` reading
  `tp.server` from the test goroutine while `listen()` wrote it
  concurrently. Dropped the field peek; shutdown semantic is the
  contract, the field layout is implementation detail. Lesson
  recorded inline.
- **Lint S1016 on plugin List() methods** (`72c2d7a`). `hook_runtime.go`
  and `tool_runtime.go` built plugin-side structs field-by-field from
  manifest structs when type conversion was valid (tag-only
  differences are conversion-legal in Go since 1.8). Replaced with
  `HookPluginHook(h)` and `ToolPluginTool(t)`.

### Dependencies

- `go mod tidy` (`72c2d7a`): promoted `webpush-go` and `go-imap/v2`
  from indirect to direct (they're imported by `internal/push` and
  `internal/channels` respectively). No version bumps; cleanup only.

### Tests

- Plugin runtime, SDKs, RPCs, and CLIs collectively gained ~80 new
  test functions across `internal/plugins`, `pkg/channelplugin`,
  `pkg/toolplugin`, `pkg/hookplugin`, `plugins/telegram`,
  `plugins/whatsapp`, and gateway integration suites.
- Full CI matrix (Linux/macOS/Windows × Go 1.22/1.24) plus `-race`
  on Linux/macOS green on tip `72c2d7a` (run 25753234773).

### Deferred to v0.5.x / v0.6.0

- Reference example plugins under `plugins/example-tool/` and
  `plugins/example-hook/`. SDK contracts well-tested; examples
  optional.
- Hot-registration of plugin tools/hooks on runtime approval —
  operator restarts to pick them up (matches channel-plugin posture).
- Token verification on hook delivery direction (gateway → plugin).
  Reserved field in the envelope; current contract doesn't send it.
- Webhook-mode plugin migration for Telegram (Iter 2 only handled
  polling).
- WhatsApp plugin inbound (Meta-driven webhook; would require
  exposing plugin endpoint publicly).
- Voice scope B (~5-6 days) targeted for v0.6.0.

## [0.4.0] - 2026-05-12

"Bidirectional + browser delivery + UI." Closes two of the explicit
PARITY-PLAN.md deferrals (P0.3 web-push, P1.1 IMAP inbound). Commits on
the branch: `fc37bde`, `410274c`, `f60225b`, `7d0aa9d`, `1a4f6a3`, `1eb283a`.

### Added

- **IMAP inbound for the email channel** (`fc37bde`). New
  `EmailFetcher` interface + `EmailInboundPoller` in
  `internal/channels/email_inbound.go`; real `IMAPFetcher` wraps
  emersion's `go-imap/v2 imapclient`. Polls unseen messages, marks
  them Seen, converts to InboundMessage with
  `SessionID="email:<from>"`. Six new config fields on
  `EmailChannelConfig` (InboundEnabled / IMAPHost / IMAPPort /
  IMAPUseTLS / IMAPMailbox / IMAPPollSeconds). Validation refuses to
  start with missing creds. New CLI subcommands: `configure email
  inbound-enable | imap-host | imap-port | imap-tls | imap-mailbox
  | imap-poll`. Tests cover poller against an in-memory fake AND
  integration against `imapmemserver`. **Closes the P1.1 follow-up
  deferral** for IMAP inbound.
- **Web Push (VAPID) delivery** (`410274c`). New `internal/push/`
  package with `Service` owning the VAPID keypair (auto-generated and
  persisted at 0o600 on first use) and subscription store. New
  `Sender` interface; production uses `webpush-go`'s
  `SendNotificationWithContext`. Gateway exposes 5 new RPCs:
  `push.publicKey`, `push.web.subscribe`, `push.web.unsubscribe`,
  `push.web.list`, `push.test`. `ApprovalQueue.SetOnEnqueue` now
  fan-outs an `approval.requested` push to every registered
  subscription alongside the existing hook. New config field
  `gateway.pushContact` (a missing value disables push). New CLI:
  `configure gateway push-contact <mailto:...>`. **Closes the P0.3
  deferral** for VAPID web-push.
- **UI management panels** (`f60225b`). Three new cards in the
  embedded Control Panel: pending-approvals list with inline Approve/
  Reject buttons (drives `approvals.list/decide`); session-compaction
  panel with Restore + Branch buttons (drives
  `sessions.compaction.list/restore/branch`); push-subscriptions list
  with per-row Remove and a "Trigger test push to all" button. Each
  panel renders an honest "not configured" / "no entries" state
  rather than crashing or hiding. New `esc()` helper prevents HTML
  injection via RPC-returned content. Browser-side push subscribe
  flow (service worker + PushManager.subscribe) deferred — operators
  register subscriptions via the RPC directly today.

### Tests

- IMAP inbound: 9 unit tests + 2 integration against `imapmemserver`
  (proves full dial→login→select→search→fetch→store-seen).
- Push: 9 unit tests on the `push.Service` (subscribe roundtrip,
  reload persistence, fan-out error aggregation, public-key stability)
  + 5 gateway integration tests (each RPC end-to-end +
  approval-Enqueue-fires-push).
- UI: 5 new Playwright tests for the management panels + the dashboard
  + web-login Playwright suites still green at 23 total tests, 13.2s
  wall-clock.

### Dependencies

- `github.com/emersion/go-imap/v2 v2.0.0-beta.8` (+ transitive
  go-message, go-sasl).
- `github.com/SherClockHolmes/webpush-go v1.4.0` (+ transitive
  golang-jwt/jwt/v5).
- `golang.org/x/crypto` upgraded to current.

All MIT/BSD-3. Single-dep policy was relaxed by maintainer earlier in
this work cycle — see `memory/project_single_dep_policy.md`.

### Fixed (post-v0.3.1)

- `runDashboard` was claiming `/ui/` returns 404; reality is the
  embedded Control Panel works. Message corrected to print an
  auth-required hint only when a bearer token is configured.

## [0.3.1] - 2026-05-12

## [0.3.1] - 2026-05-12

Substantial parity-and-hardening pass. Phases 1-6 of the stabilize-then-parity
plan are complete; 11 of 12 Phase-7 items shipped. Three remaining items
(web-push, IMAP inbound, plugin implementation) are deferred with explicit
pickup triggers documented in [docs/PARITY-PLAN.md](docs/PARITY-PLAN.md).

### Security

- **Constant-time webhook secret comparison.** discord/teams/telegram
  webhook handlers now use `hmac.Equal` instead of `==`, matching the
  slack/whatsapp pattern. Prevents byte-level timing attacks on the
  shared secret.
- **Gateway auth: timing-safe compares + XFF trust boundary.** Bearer /
  X-OpenClaw-Token / query / Basic compares moved to
  `crypto/subtle.ConstantTimeCompare`. `X-Forwarded-For` is now honored
  only when the immediate peer (`directRemoteIP`) is in the trusted-proxy
  list, fixing an auth-bypass where a spoofed XFF could masquerade as a
  trusted-proxy address.
- **Auth state race.** `authToken` / `password` / `trustedProxies` /
  `allowedOrigins` are now guarded by `authMu sync.RWMutex`; mutators
  (`SetAuth`, `SetAuthToken`, `SetAllowedOrigins`, `gateway.config` RPC)
  write under it, and `isAuthorized` / `isAllowedOrigin` snapshot under
  RLock. Catchable on Linux CI via `-race`.
- **Persisted state file mode tightened to 0o600.** sessions, hookstore,
  cronstore, topology, agents/workspace, logstore, config all now write
  with `0o600` (was `0o644`). Matches `secretstore`. Linux-tagged test
  in `fileutil` asserts the mode end-to-end.
- **Sandbox payload off argv onto stdin.** `sandbox.InvokeToolJSON` no
  longer appends JSON payloads to `docker run` argv (where they would
  leak to `ps` and `docker inspect`). New `Stdin io.Reader` field on
  `Options` pipes payloads through `cmd.Stdin`; `docker run -i` added
  when `Stdin` is set.
- **SSE marshal hardening.** `writeSSE` in `agent_run.go` now logs +
  skips on marshal failure instead of emitting an empty `data: \n\n`
  frame that clients could misparse as a run state.
- **Approval queue: expired-pending prune.** `pruneLocked` now removes
  expired-pending entries (previously only decided entries), so
  `Enqueue`-without-matching-`Wait` callers can't leak memory.

### Added

#### Session compaction subsystem (P0.1)

- New `sessions.CompactionRecord` capturing the pre-image, removed
  count, `KeepN`, and timestamp for every explicit `Compact()` event.
  Persisted to a sidecar `${path}.compactions.json` at 0o600.
- New RPCs: `sessions.compaction.list`, `sessions.compaction.get`,
  `sessions.compaction.restore`, `sessions.compaction.branch`.
- New CLI: `openclaw compaction list|get|restore|branch` (restore
  requires `--yes`).

#### OpenAI compatibility surface (P0.2)

- Default model bumped from `text-embedding-ada-002` to
  `text-embedding-3-small`.
- New `agents.Embedder` interface + `OpenAIRunner.Embed` proxying real
  embeddings to the configured provider. Gateway's `/v1/embeddings`
  type-asserts the active runner and uses real vectors when available,
  falling back to the deterministic 256-dim pseudo embedding otherwise.

#### Web login flow (P0.4)

- New `web.login.start` / `web.login.wait` RPCs implementing a
  device-code-style browser approval handshake.
- New HTTP routes: `GET /web/login/{nonce}` (renders inline HTML
  confirm page), `POST /web/login/{nonce}/confirm` (records the
  decision and issues a fresh bearer token). Confirm POST is
  auth-gated when auth is already configured (token rotation);
  open during initial setup.
- New CLI: `openclaw web-login`.

#### Onboard CLI (P0.5)

- `openclaw onboard` now accepts `--provider`, `--openai-key`,
  `--anthropic-key`, `--gateway-token`, `--gateway-port` flags.
  Non-destructive merge over existing config. Bare form (no flags)
  preserves the historical default-config-write behavior.

#### Hook system depth (P0.6)

- New event types: `gateway.started`, `gateway.stopping`,
  `agent.run.started`, `approval.requested`.
- New `ApprovalQueue.SetOnEnqueue` callback so gateway can fire hooks
  for new approval requests without `runtime` depending on
  `hookstore`.

#### Outbound channels (P1.1–P1.4)

- **Email** — `internal/channels/email.go`. SMTP via stdlib `net/smtp`.
  Implicit TLS on port 465; opportunistic STARTTLS on 587. PLAIN auth.
  Plaintext UTF-8 body with auto-derived `[sessionId] firstLine` subject.
  `EmailDialer` interface for testability. **Outbound only** (IMAP
  inbound deferred).
- **Signal** — `internal/channels/signal.go`. POSTs `/v2/send` to a
  configurable signal-cli-rest-api sidecar. Inbound deferred.
- **Matrix** — `internal/channels/matrix.go`. PUTs
  `/_matrix/client/v3/rooms/{roomId}/send/m.room.message/{txnId}` with
  bearer auth; per-process unique txn ids; rejects aliases. Inbound
  `/sync` long-poll deferred.
- **Mattermost** — `internal/channels/mattermost.go`. POSTs
  `/api/v4/posts` with bearer; `ThreadID → root_id` for threaded
  replies. Inbound deferred.

All four channels have config schema entries
(`EmailChannelConfig`/`SignalChannelConfig`/`MatrixChannelConfig`/`MattermostChannelConfig`),
gateway wiring that registers them when `Enabled=true`, `printConfig`
redaction of sensitive fields, `validateGatewayChannelConfig`
fail-fast on missing required fields, `runDoctor` status surfacing,
and `openclaw configure email|signal|matrix|mattermost <subcmd>`
helpers.

#### Operational CLI

- **`openclaw dashboard`** — print gateway URL, best-effort open in
  browser (rundll32 / open / xdg-open).
- **`openclaw daemon install|uninstall|path`** — write a user-level
  systemd unit (Linux) or launchd plist (macOS); prints the
  user-runnable activation command rather than auto-executing it.
  Windows rejected with a clear pointer to NSSM / Task Scheduler.
- **`openclaw backup [list]`** / **`openclaw restore <path> --yes`**
  — explicit `--yes` guard for the destructive restore path.
- **`openclaw message history|dispatch`** — fetch a session
  transcript or push outbound to a specific channel by name.

### Changed

- File modes for persisted state files tightened to 0o600
  (sessions/hookstore/cronstore/topology/agents/logstore/config). See
  Security section.
- `/v1/embeddings` default model: `text-embedding-ada-002` →
  `text-embedding-3-small`.
- Approval queue `pruneLocked` extended to drop expired-pending
  entries (memory-leak fix).

### Fixed

- SSE writer in `agent_run.go` previously emitted an empty `data: \n\n`
  frame on `json.Marshal` failure. Now logs the marshal error to
  stderr and skips the event.
- Lint cleanups in production code: `cronstore.go` uses
  `strings.CutPrefix`; `hookstore.go` uses `for range n` (Go 1.22)
  and drops the obsolete `h := h` loop-variable shadow;
  `cmd/openclaw/main.go` uses `any` over `interface{}`.

### Tests

- Full `go test ./...` and `go vet ./...` green at every checkpoint.
- New end-to-end pipeline test in
  `internal/gateway/integration_test.go` exercises POST `/message` →
  store → EchoRunner → router → registered channel; dispatch-failure
  resilience; hook fanout; and the `message.send` RPC path.
- Context-cancellation tests added to signal/matrix/mattermost; dial-
  error propagation test added to email.
- Total new test functions: ~90 across the session's work.

### Deferred (with explicit pickup triggers)

See [docs/PARITY-PLAN.md](docs/PARITY-PLAN.md) for the canonical
trigger list. Summary:

- **VAPID web-push** — operator reports approval-flow delivery
  failures in production OR the Control UI ships.
- **IMAP inbound** — user explicitly asks for email replies OR a
  deployment scenario emerges where email is the only viable inbound.
- **commitments/tasks** — user requests "remind me / show background
  jobs" semantics; requires state-model design first.
- **Config schema versioning + real `migrate`** — first
  schema-breaking change actually lands.
- **Plugin architecture implementation** — a real plugin needs a
  contract the existing manifest doesn't cover. Design captured in
  [docs/PLUGIN-ARCHITECTURE.md](docs/PLUGIN-ARCHITECTURE.md).
