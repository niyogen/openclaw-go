# Studio capability matrix vs openclaw-go

Source of truth for what openclaw-studio can do against openclaw-go.
Built by enumerating every `/api/*` route in studio's source plus
every `callGateway()` call site. Each capability row links its UI
affordance to the gateway methods it exercises and our current
support status.

Last audit: 2026-05-16.

## Coverage summary

- **Capabilities catalogued: 24**
- **Fully supported (UI + tests pass): TBD until Playwright sweep**
- **Partially supported (works but with caveat): listed inline**
- **Out of scope for now: media (audio/video transcripts)**

## Gateway methods studio actually calls

21 distinct methods (extracted from studio's callGateway sites):

| Method | Adapter | Adapter file:line | Tested |
|---|---|---|---|
| `wake` | handleWake | control_methods.go | ✅ |
| `status` | handleStatus | control_methods.go | ✅ |
| `agents.list` | handleAgentsList | control_methods.go | ✅ (2 tests) |
| `agents.create` | handleAgentsCreate | control_methods.go | ✅ |
| `agents.update` | handleAgentsUpdate | control_methods.go | ✅ (partial-merge) |
| `agents.delete` | handleAgentsDelete | control_methods.go | ✅ |
| `agents.files.get` | handleAgentsFilesGet | control_methods.go | needs test |
| `agents.files.set` | handleAgentsFilesSet | control_methods.go | needs test |
| `sessions.list` | handleSessionsList | control_methods.go | needs test (sort) |
| `sessions.preview` | handleSessionsPreview | control_methods.go | ✅ (shape) |
| `sessions.reset` | handleSessionsReset | control_methods.go | needs test |
| `sessions.patch` | handleSessionsPatch | control_methods.go | ✅ (upsert) |
| `cron.list` | handleCronList | control_methods.go | needs test |
| `cron.add` | handleCronAdd | control_methods.go | needs test |
| `cron.run` | handleCronRun | control_methods.go | needs test |
| `cron.remove` | handleCronRemove (→cron.delete) | control_methods.go | ✅ (alias) |
| `config.get` | handleConfigGet | control_methods.go | ✅ (2 tests) |
| `config.set` | handleConfigSet | control_methods.go | needs test |
| `config.patch` | handleConfigPatch | control_methods.go | ✅ (2 tests) |
| `chat.send` | handleChatSend | control_methods.go | ✅ (guard) |
| `chat.history` | handleChatHistory | control_methods.go | ✅ (rename) |
| `chat.abort` | handleChatAbort | control_methods.go | needs test |
| `agent.wait` | handleAgentWait | control_methods.go | needs test |
| `models.list` | handleModelsList | control_methods.go | needs test |
| `exec.approvals.get` | handleExecApprovalsGet | control_methods.go | ✅ (stub) |
| `exec.approvals.set` | handleExecApprovalsSet | control_methods.go | ✅ (stub) |
| `exec.approval.resolve` | handleExecApprovalResolve | control_methods.go | needs test |

## Studio's API routes

Studio's frontend hits 24 distinct API paths under `/api/`. Each
maps to either intent routes (writes via gateway) or runtime routes
(reads, some via gateway, some via projection store).

### Intent routes (writes — POST)

| Studio route | Gateway method(s) | Test target |
|---|---|---|
| `/api/intents/agent-create` | config.get + agents.create | Playwright: create-agent flow |
| `/api/intents/agent-delete` | agents.delete | Playwright: delete-agent flow |
| `/api/intents/agent-file-set` | agents.files.set | Playwright: write agent file |
| `/api/intents/agent-permissions-update` | config.set + config.get | Playwright: change permissions |
| `/api/intents/agent-rename` | agents.update | Playwright: rename agent |
| `/api/intents/agent-wait` | agent.wait | Playwright: wait-for-run |
| `/api/intents/chat-abort` | chat.abort | Playwright: abort run |
| `/api/intents/chat-send` | chat.send | Playwright: send message ✅ |
| `/api/intents/cron-add` | cron.add | Playwright: add cron |
| `/api/intents/cron-remove` | cron.remove | Playwright: remove cron |
| `/api/intents/cron-remove-agent` | cron.list + cron.remove | Playwright: agent cleanup |
| `/api/intents/cron-restore` | cron.add | Playwright: undo agent delete |
| `/api/intents/cron-run` | cron.run | Playwright: manual cron trigger |
| `/api/intents/exec-approval-resolve` | exec.approval.resolve | Playwright: approve/deny |
| `/api/intents/session-settings-sync` | sessions.patch | Playwright: per-session settings |
| `/api/intents/sessions-reset` | sessions.reset | Playwright: clear session |

### Runtime routes (reads — GET / occasionally POST)

| Studio route | Gateway method(s) | Test target |
|---|---|---|
| `/api/runtime/agent-state` | projection only | (projection coverage) |
| `/api/runtime/agents/[id]/history` | chat.history | Playwright: history view |
| `/api/runtime/agents/[id]/preview` | sessions.preview | Playwright: preview pane |
| `/api/runtime/config` | config.get + status + models.list | Playwright: config card |
| `/api/runtime/cron` | cron.list | Playwright: cron panel |
| `/api/runtime/disconnect` | adapter.disconnect | Playwright: settings disconnect |
| `/api/runtime/fleet` | agents.list + config.get + exec.approvals.get + status + sessions.preview | Playwright: bootstrap ✅ |
| `/api/runtime/media` | (unknown — TBD) | TBD |
| `/api/runtime/models` | models.list | Playwright: model picker |
| `/api/runtime/stream` | SSE (events from /control/ws) | Playwright: live updates |
| `/api/runtime/summary` | projection only | (projection coverage) |

### Studio settings routes (no gateway involvement)

| Studio route | What it does |
|---|---|
| `/api/studio` | GET/PUT studio's own settings.json |
| `/api/studio/test-connection` | Probes a candidate gateway URL |

## High-level UI capabilities

Each capability is something a user does in the studio UI. Tests
should verify the end-to-end flow, not just the API call.

| # | Capability | UI affordance | Gateway methods | Test status |
|---|---|---|---|---|
| 1 | Connect to gateway | "Connected" pill in header | connect handshake | ✅ |
| 2 | View agent fleet | Left panel "AGENTS (N)" with cards | agents.list (via fleet) | ✅ |
| 3 | Create new agent | "+ New agent" button | agents.create | needs test |
| 4 | Rename agent | Pencil icon next to agent name | agents.update | needs test |
| 5 | Delete agent | Settings → delete | agents.delete (+ cron cleanup) | needs test |
| 6 | Send chat message | "type a message" + Send button | chat.send → reply via events/history | ✅ (round-trip) |
| 7 | View chat history | Center transcript panel | chat.history | ✅ (via round-trip) |
| 8 | Abort agent run | Stop button (when running) | chat.abort | needs test |
| 9 | Reset session | "New session" button | sessions.reset | needs test |
| 10 | Configure session model | Model dropdown bottom-left | sessions.patch | needs test |
| 11 | Configure thinking level | Thinking dropdown bottom-left | sessions.patch | needs test (silent accept) |
| 12 | View / edit agent files | (Workspace UI — not in mainline studio yet) | agents.files.get/set | needs test |
| 13 | List cron jobs | Cron panel | cron.list | needs test |
| 14 | Add cron job | Cron panel → new | cron.add | needs test |
| 15 | Trigger cron manually | Cron panel → run | cron.run | needs test |
| 16 | Remove cron job | Cron panel → delete | cron.remove | needs test |
| 17 | View exec approvals | Approvals panel | exec.approvals.get | needs test (stub) |
| 18 | Resolve exec approval | Approve/deny buttons | exec.approval.resolve | needs test (stub) |
| 19 | Configure agent permissions | Settings → permissions | config.set + config.get | needs test |
| 20 | Change agent permissions | Per-agent settings dialog | config.patch | needs test |
| 21 | Browse / change provider+model | Model selector dropdown | models.list | needs test |
| 22 | View gateway config card | Settings panel | config.get | needs test |
| 23 | Receive live event updates | UI auto-refresh on agent activity | SSE stream + /control/ws events | partial (presence) |
| 24 | Disconnect from gateway | Settings → disconnect | adapter.disconnect | needs test |

## Known limitations of openclaw-go's adapter layer

These are intentional gaps, documented for honesty:

1. **Live event lifecycle is approximated as `presence`** — studio's
   transcript panel may need a page reload to surface the very
   latest assistant reply on tight timing windows. Run-lifecycle
   events (`agent.run.started` → chat deltas → chat final) require
   a real run tracker in openclaw-go.

2. **`exec.approvals.get/set` are stubs** — return empty `{file:
   {agents:{}}}`. Per-agent permission policies aren't persisted
   yet; the UI shows an "empty" state but doesn't error.

3. **`config.get` returns secrets unredacted** — fine for local-
   loopback WS, but needs a server-side redaction layer before
   any non-loopback deployment.

4. **`sessions.patch` accepts but silently ignores `thinkingLevel`,
   `execHost`, `execSecurity`, `execAsk`** — openclaw-go doesn't
   model these per-session yet. Forward-compatible with studio's
   UI which sets them optimistically.

5. **`agents.files.get` with empty path returns first artifact in
   map-iteration order** — unreachable in studio's flow (always
   passes a path), but the fallback is non-deterministic.

## Out of scope for openclaw-go right now

- **Voice / TTS / STT** — separate parity item, planned for v0.6.0.
- **3D office view** (only in tenacitOS, not in studio).
- **Plugin marketplace** (gateway plugin contract exists; UI doesn't).
