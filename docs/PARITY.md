# Parity checklist (reference OpenClaw)

Use this file to track **feature parity** against a **pinned** upstream OpenClaw revision.

## How to use

1. Record the reference repo URL and **exact git tag or commit SHA** you are comparing to.
2. Walk RPC methods, CLI commands, and channel behaviors; mark each row **done**, **partial**, or **missing**.
3. Update the pin when you intentionally target a newer upstream release.

## Reference pin (fill in)

| Field | Value |
|--------|--------|
| Upstream repository | _e.g. https://github.com/…/openclaw_ |
| Pin (tag or SHA) | _e.g. v0.x.y or abc1234_ |
| Last reviewed | _date_ |

## Suggested rows (extend as needed)

| Area | Item | Status |
|------|------|--------|
| Gateway | HTTP `/rpc` method surface | |
| Gateway | OpenAI-compatible `/v1/*` | |
| CLI | `configure` subcommands | |
| Channels | Telegram (inbound/outbound) | |
| Channels | Slack / Discord / … | |
| Runtime | Tool policy / approvals | |
| Ops | Metrics / tracing | |

**Note:** `openclaw-go` is an independent implementation; parity is optional and should follow product priorities, not automatic 1:1 copying.
