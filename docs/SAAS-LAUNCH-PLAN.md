# openclaw-go SaaS launch plan

Last updated: 2026-05-16. Written for tomorrow's work session.

## Where we are today

- **Product:** openclaw-go gateway + openclaw-studio frontend
  working end-to-end. 63 unit tests + 8 Playwright tests passing.
- **Deployment model today:** single-tenant, local-loopback,
  user runs both binaries on their own machine.
- **Gap to SaaS:** large but tractable. Detailed below.

## What "SaaS" means for openclaw-go

The plausible offering: hosted multi-tenant gateway + studio UI
where users sign up, get a workspace, configure their agents
(BYO API keys or use platform-managed providers), and chat with
them from anywhere.

## Critical-path items (must ship for launch)

Ordered by dependency, not by ease. Each is a real engineering
investment.

### 1. Multi-tenancy / data isolation (~1 week)

Today: every subsystem writes to a single `$dataDir`. SaaS needs
per-account scoping.

- [ ] Introduce an `AccountID` concept. Propagate through:
  - sessions store
  - workspace (agents)
  - cron store
  - hookstore
  - logstore
  - artifact store
  - any persisted state
- [ ] Decide isolation strategy: per-account directories vs.
  single SQLite with account_id column. Files match current
  code; SQLite scales better. **Recommend SQLite migration**
  as a separate prior step.
- [ ] Token-to-account mapping. Today the gateway token is a
  single string; SaaS needs to look up which account a token
  belongs to.
- [ ] Auth gate: every RPC + /control/ws call asserts the
  authenticated account matches the data being touched.
- [ ] Tests: per-account isolation invariants (account A's
  agents are invisible to account B).

### 2. config.get secret redaction (~0.5 day)

Today: `config.get` returns the openclaw.json file contents
unredacted. SaaS exposes `/control/ws` over the network — must
redact API keys, gateway tokens, channel secrets, etc.

- [ ] Define a redaction policy (which fields are sensitive).
- [ ] Server-side redaction in `loadConfigForControl`.
- [ ] `config.patch` round-trip: when client writes back a
  redacted value, server preserves the original (don't blank
  out the real secret).
- [ ] Tests: redacted-round-trip preserves secrets.

### 3. Auth + identity (~1 week)

- [ ] OAuth provider integration (recommend GitHub + Google to
  start; email/password is more compliance-heavy).
- [ ] Account creation flow.
- [ ] Web session management (cookies, CSRF tokens).
- [ ] API keys (per account, multiple, scopable, revokable).
- [ ] Existing `web.login.start/wait` flow can stay for CLI
  device-code login — keep it.

### 4. Billing (~1-2 weeks)

- [ ] Stripe integration: customer/subscription/usage events.
- [ ] Pricing tiers — decide before coding. Suggested:
  - Free: 1 agent, 100 messages/month, BYO API keys
  - Pro ($X/mo): 10 agents, 10k messages/month, BYO keys or
    platform-managed
  - Enterprise: contact-us
- [ ] Metering: which dimensions? Likely: messages, agents,
  storage. Track in projection store + reconciliation cron.
- [ ] Usage caps + soft-block / hard-block enforcement.
- [ ] Stripe webhook handler.
- [ ] Subscription state propagated to gateway auth (downgrade
  on payment failure, etc.).

### 5. TLS + production hosting (~3 days)

- [ ] Production deployment target chosen (Fly.io, Railway,
  AWS ECS, GCP Cloud Run, or self-hosted on a VPS via Caddy
  + systemd).
- [ ] TLS via Caddy / Cloudflare / hosting platform.
- [ ] DNS configured.
- [ ] WebSocket-friendly load balancing (sticky sessions if
  needed for SSE).
- [ ] Process supervisor (systemd / supervisord / platform-
  managed).
- [ ] Logs aggregated to a sink (Loki, Datadog, hosted).
- [ ] Metrics scrape endpoint reachable from a Prometheus
  instance (we have `/metrics`).

### 6. Security hardening (~1 week)

- [ ] Per-account rate limiting (we have `withRateLimit` but
  it's IP-scoped — needs account-scoping).
- [ ] CORS: tighten allowed origins to known frontends.
- [ ] Secret encryption at rest. Today secrets live plaintext
  in `openclaw.json` and `secretstore`. Wrap with platform
  KMS or libsodium boxed secrets.
- [ ] Audit log: who did what, when. Separate from app logs.
- [ ] DDoS protection at the edge (CDN + WAF).
- [ ] `/control/ws` over TLS (`wss://`).
- [ ] Token rotation flow.

## Important but not launch-blocking

These can ship in the first month post-launch.

### Onboarding UX (~3 days)

- [ ] Marketing landing page (static site or Next.js).
- [ ] Pricing page.
- [ ] Sign-up → first-agent flow target: <5 min.
- [ ] Studio's settings UI needs to handle hosted vs. local
  configurations gracefully.
- [ ] Sample agents / templates to seed new accounts.

### Documentation (~1 week, parallelizable)

- [ ] docs.openclaw-go.com (or whatever the SaaS domain is).
- [ ] Quickstart (under 5 min).
- [ ] Agent configuration deep dive.
- [ ] Channel integration guides per channel.
- [ ] Plugin authoring guide (link to pkg/{toolplugin,channelplugin,hookplugin}).
- [ ] API reference.

### Operational maturity

- [ ] Backup / disaster recovery automated.
- [ ] Status page (statuspage.io or self-hosted).
- [ ] Incident response runbook.
- [ ] On-call rotation (or just you, if solo).
- [ ] Error tracking (Sentry or similar) wired up.

### Compliance / legal

- [ ] Privacy policy.
- [ ] Terms of service.
- [ ] Cookie consent for EU traffic.
- [ ] GDPR data deletion flow (account → delete-my-data).
- [ ] DMCA process.

## Tomorrow's work session — concrete starting tasks

Suggested order for picking back up:

1. **Config redaction** (#2 above) — small, well-defined,
   immediately unblocks any non-loopback deploy.
2. **Decide isolation strategy** — files vs. SQLite. Decide
   THIS, then schedule the refactor. The decision shouldn't take
   more than an hour with notes.
3. **Pricing model decision** — needed before building the
   billing integration. Worth a coffee + paper session.
4. **Hosting platform decision** — file-based persistence
   constrains options (Fly volumes vs. AWS EBS vs. local
   disk). Decide before deploying anything.

After those four decisions, the engineering work above is
ranked, scoped, and ready to execute.

## Estimated total launch effort

- Critical path: ~3-4 weeks of focused work
- Important-but-not-blocking: another ~2 weeks parallel
- Marketing + docs + GTM: separate track, ~1 week

Realistic launch timeline from "decisions made" to "first paying
customer": **6-8 weeks** of solo work, faster with a small team.

## What openclaw-go already has that helps

Worth crediting before listing what's missing:

- Gateway with metrics, request IDs, rate limiting (IP-scoped),
  auth (bearer + basic + trusted proxies + constant-time
  compares), CORS allow-list, body-size limits, atomic file
  writes at 0o600.
- 12 channel integrations (most outbound complete).
- Plugin architecture for channels, tools, hooks.
- Web push (VAPID) for browser notifications.
- Web login flow for CLI device-code auth.
- Hooks subsystem with lifecycle events.
- Backup / restore CLI.
- Daemon installer (systemd / launchd).
- 100% of openclaw-studio's gateway-method dependencies
  implemented (21/21).
- 63 Go unit tests + 8 Playwright integration tests across
  the studio path.
- Build/CI pipeline (Linux/macOS/Windows × Go 1.22/1.24,
  -race on Linux/macOS).

This is a strong foundation. The launch work is real but
finite.
