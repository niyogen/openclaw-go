# Security policy

## Reporting a vulnerability

**Do NOT open a public issue for security problems.** Email the maintainer
directly at <prageeth.mahendra@gmail.com> with:

- A clear description of the issue, including the affected version
  (run `openclaw version` or check `VERSION`).
- Step-by-step reproduction. A failing test or minimal `curl` invocation
  helps a lot.
- Your assessment of impact (e.g., "lets an unauthenticated network
  attacker bypass gateway auth" vs. "leaks a metric value to a
  trusted-network reader").
- Whether you want public credit and under what name.

We aim to acknowledge within 72 hours and ship a fix or a documented
mitigation within 14 days for `Critical` / `High` severity issues. Lower
severity items roll up into the next release.

## Scope

In scope:

- Authentication and authorization on `/rpc`, `/ws`, `/agent/run`,
  `/agent/run/stream`, `/sessions/*`, `/message`, `/cron`, `/hooks`,
  `/secrets`, `/v1/*`, `/tools/*`, `/web/login/*`.
- Webhook signature verification for any built-in channel
  (Slack/Discord/Teams/Telegram/WhatsApp/Line and the four new
  outbound-only channels' inbound bridges if/when added).
- Timing attacks against shared-secret comparisons.
- SSRF via plugin manifests, agent runner BaseURL fields, or
  channel-config URLs.
- Persisted-state file permissions (config, sessions, secrets,
  hookstore, cronstore, topology, agents/workspace, logstore).
- Sandbox isolation (`internal/sandbox`): payload exfiltration via
  argv, container escape via mis-built `docker run` args.
- Approval queue: bypass, replay, race.
- Web login flow: nonce reuse, CSRF on the confirm endpoint,
  token-rotation hijack.

Out of scope:

- Issues that require physical access to the machine running the
  gateway, the operator's home directory, or the operator's terminal
  session.
- Issues that require the operator to install an unsanctioned plugin
  (the manifest approval gate is the security boundary — bypassing it
  is in scope, but a plugin behaving badly *after* operator approval
  is the plugin's problem, not the gateway's).
- Denial-of-service via `agent.run` on a runner the operator
  configured to call paid third-party APIs. Configure rate limits.
- Vulnerabilities in third-party services we proxy to (Slack, Matrix,
  signal-cli-rest-api, etc.). Report those upstream.
- Test code, example configs, and anything under `e2e/`, `e2e-ui/`,
  or `.gocache/`.

## Supported versions

While openclaw-go is pre-1.0, only the latest tagged release on `main`
receives security patches. Run the latest binary and re-pull periodically.

## Known security postures

These are documented design choices, not bugs:

- **Plugin DNS fail-open**: when DNS resolution fails for a hostname
  in a plugin manifest forward URL, the gateway treats the host as
  external (allows the manifest). This avoids breaking legitimate
  manifests in offline / test environments at the cost of letting an
  attacker who controls DNS suppress resolution and slip an internal
  hostname past validation. The risk is acknowledged in
  `internal/plugins/loader.go:159`.
- **Plain SMTP allowed**: the email channel will send over an
  unencrypted connection if STARTTLS isn't advertised by the server
  and port 465 isn't used. This exists so local test MTAs work without
  TLS gymnastics. For production, force port 465 or run against a
  server that advertises STARTTLS.
- **No content-level encryption on persisted state**: `~/.openclaw-go/`
  files are mode 0o600 (owner read/write only) but their contents
  (auth tokens, API keys, secret values) are stored in plaintext.
  Protect the parent directory with OS-level access controls. Disk
  encryption is the operator's responsibility.

## What we will and won't do

We will:

- Apply security fixes ahead of feature work when a `Critical` or
  `High` is open.
- Credit reporters in [CHANGELOG.md](CHANGELOG.md) under a `Security`
  heading when the fix lands.
- Backport a fix to the previous tagged release for any
  network-exposed `Critical` (gateway auth bypass, RCE, secret
  exfiltration).

We will not:

- Pay bug bounties. This is a personal-license MIT project.
- Negotiate embargo periods longer than 90 days from acknowledgment.
- Treat as security issues any behavior we've documented as a known
  posture in this file.
