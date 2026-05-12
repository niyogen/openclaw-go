# Plugin architecture — design note

Status: **design only, not yet implemented.** This document captures the
contract the plugin system should expose and the failure modes it must
handle. Implementation will land in small, separately-reviewable slices —
not as a single framework PR.

## What exists today

`internal/plugins/` already supports JSON-manifest-based plugins discovered
under `~/.openclaw-go/plugins/<name>/plugin.json`. Each manifest can declare:

- `routes` — HTTP routes the gateway reverse-proxies to a forward URL.
- `tools` — named tools the gateway exposes via `tools.invoke`, forwarded
  to a per-tool POST endpoint.
- `env` — config-resolved environment variables for the plugin process.

This is sufficient for "thin proxy" plugins that run as separate HTTP
servers. It is **not** sufficient for plugins that want to:

- Implement a new channel (e.g., a third-party messenger we don't ship).
- Subscribe to lifecycle hooks (`gateway.started`, `agent.run.complete`, …)
  and react.
- Define new agent runners or model providers.
- Participate in approval workflows (today only the built-in queue exists).

The gaps below address exactly those needs — no more.

## Three contracts, all HTTP-only

A plugin is an out-of-process program reachable over HTTP. We do NOT load
in-process Go plugins (`plugin.Open`) — their failure modes (panics, ABI
mismatches, leaked goroutines) are unacceptable for a gateway that has to
stay up for everyone else's traffic. The cost is a per-call HTTP roundtrip;
the win is hard process isolation.

### 1. Channel contract

```
POST {plugin.baseUrl}/channel/send
Body: { sessionId, channel, target, message, threadId, replyToMessageId, mediaUrl, buttons, reactions, ephemeral }
Response 2xx: empty body
Response 4xx/5xx: { error: string }
```

The gateway registers a channel-plugin in `channels.Router` under
`Name()` returning the plugin's declared channel name (e.g. `"matrix"`).
When `Router.Dispatch` selects it, the channel-plugin's `Send()` POSTs to
`/channel/send`. Standard router retries (`maxRetries`, exponential
backoff) apply.

Inbound from the plugin's side travels the reverse direction:

```
POST {gateway}/plugins/{name}/inbound
Body: InboundMessage { sessionId, channel, target, message }
```

This route is `auth-gated` with the same bearer/basic/token rules as `/rpc`.
The plugin must include an `Authorization: Bearer <token>` header — we'll
issue a per-plugin token at registration time (see Approval below).

**Failure modes:**

- Plugin process down → `Send` returns connection-refused → router treats
  as transient, retries with backoff, surfaces `channel_dispatch_errors_total`.
- Plugin times out (>20s) → same as down.
- Plugin returns 4xx (auth, malformed input) → router treats as permanent,
  does NOT retry, surfaces dispatch error to caller.
- Plugin sends garbage to `/plugins/{name}/inbound` → request fails 400,
  no session created, metric increments.

### 2. Tool contract

Already in the manifest as `tools[]`. No protocol change needed. A future
slice may add streaming responses (SSE) for long-running tools, but the
current request/response shape is sufficient for the use cases we have.

### 3. Hook contract

```
POST {plugin.baseUrl}/hook/{event-name}
Body: { event: string, payload: map[string]any, timestamp: RFC3339 }
Response 2xx: empty body  (response body is ignored — hooks are fire-and-forget)
```

The plugin's manifest gains a `hooks[]` array:

```json
"hooks": [
  { "event": "agent.run.complete", "endpoint": "http://plugin:9090/hook/agent" }
]
```

Hooks fire through the existing `hookstore` semaphore (max 32 concurrent
dispatches), so a slow plugin can't starve other hooks.

**Failure modes:**

- Plugin endpoint 5xx → log + drop. We do NOT retry hooks — at-most-once
  is the right semantic for "user pressed approve in Telegram" style
  events, since duplicate delivery is worse than missed delivery.
- Plugin endpoint slow → 10-second client timeout, then log + drop.
- Plugin endpoint missing → log + drop. The same fire-and-forget posture
  the built-in webhook hook already takes.

## Plugin lifecycle & approval

A plugin manifest is **inert until approved**. On gateway startup:

1. Loader scans `pluginsDir` for `plugin.json` files.
2. Each plugin is added to the registry in `state: pending`.
3. An operator runs `openclaw plugins approve <name>` to flip it to
   `state: approved`.
4. Only approved plugins receive routes, tool registrations, hook
   subscriptions, and inbound auth tokens.

The approval gate exists because the manifest can declare arbitrary forward
URLs and tool endpoints. An unapproved plugin shouldn't be able to
exfiltrate traffic just by being dropped in `pluginsDir`. This is the
same posture the upstream OpenClaw `plugin.approval.*` RPCs implement.

**Failure modes:**

- Plugin manifest is malformed → loader logs + skips, no state change.
- Plugin's declared forward URL points at a private/loopback address →
  rejected at approval time (mirrors the `validateManifestURLs` SSRF
  check that already exists for `routes[]`).

## Test strategy

Each slice ships with:

1. A unit test against a fake-plugin `httptest.Server` covering happy
   path + the four documented failure modes (down / timeout / 4xx / 5xx).
2. An integration test that runs the slice through `channels.Router` or
   `hookstore.Emit`, proving the new code composes correctly with the
   existing dispatch machinery.

No "framework" tests — every test exercises real behavior end-to-end via
HTTP, since that's how plugins actually run.

## Out of scope for the first implementation pass

- In-process Go plugins (`plugin.Open`).
- Plugin sandboxing beyond process isolation (no seccomp / containers).
- Plugin auto-restart or supervision — operators run their plugin
  processes under systemd / launchd themselves.
- Plugin marketplace, signing, or version pinning. (`version` in the
  manifest is informational only.)
- Streaming hook responses.

Adding any of those should be a separate design note + slice; do not
extend this file with speculative features.

## Forcing functions to start coding

This stays a design note until **one of**:

1. A user/operator asks for a channel plugin that isn't in the built-in
   set (today: Telegram/Slack/Discord/Teams/WhatsApp/Line/Nostr/Email/
   Signal/Matrix/Mattermost + generic webhook).
2. A user/operator asks for a hook plugin (e.g., "fire my custom HTTP
   endpoint on every approval.requested event") that can't be expressed
   with the current `hookstore.HookTypeWebhook` schema.
3. A real plugin shows up in `pluginsDir` exercising the existing
   `routes[]`/`tools[]` manifest fields, and the operator hits a missing
   capability.

Until then, the existing manifest covers ~80% of plugin use cases by
serving reverse-proxy routes + tool endpoints. Extending the contract
without a concrete consumer would be the "speculative framework" anti-
pattern this doc is supposed to avoid.
