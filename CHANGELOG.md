# Changelog

All notable changes to openclaw-go are recorded here.

This file follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project aims to follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

Nothing yet.

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
