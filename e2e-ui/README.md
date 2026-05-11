# Browser E2E (Playwright)

Exercises **`/ui/`** Quick infer against a **real** `openclaw-go` gateway with a **local mock** OpenAI HTTP server (so you get a non-`Echo:` reply without calling `api.openai.com`).

## Prerequisites

- **Node.js** 18+ (uses built-in `fetch` in `start-stack.mjs`)
- **Go** toolchain (same as repo)
- From this directory once:

```bash
npm install
npx playwright install
```

On Linux CI you may need `npx playwright install-deps` (or `--with-deps` in CI).

## Run

From **`openclaw-go/e2e-ui`**:

```bash
npm test
```

Default gateway URL: **`http://127.0.0.1:18889`** (avoids clashing with a dev server on **18789**).

Override:

```bash
E2E_GATEWAY_PORT=18910 npm test
```

Reuse an already-running stack (not recommended for CI):

```bash
E2E_REUSE_STACK=1 npm test
```

## What this proves

If this test passes, the **browser → `/rpc` → `message.send` → OpenAI runner** path returns a **real completion body** from the mock. If your manual setup still shows **`Echo:`** while this passes, the problem is **your live config / key / network** (fallback), not the UI wiring.
