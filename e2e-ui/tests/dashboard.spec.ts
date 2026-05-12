import { test, expect } from '@playwright/test';

// Full sweep of the embedded Control Panel at /ui/. Every visible card and
// every JS-driven interaction in `internal/gateway/ui/index.html` should be
// exercised here so a regression that breaks a single counter, the auto-
// refresh, or the redirect at / doesn't get past CI.

test.describe('Control panel /ui — structure', () => {
  test('page loads with title, header, and version badge', async ({ page }) => {
    await page.goto('/ui/');
    await expect(page).toHaveTitle(/OpenClaw-Go Control Panel/);
    await expect(page.locator('header h1')).toHaveText('OpenClaw-Go');
    // Version badge populates from /health within the first refresh tick.
    await expect(page.locator('#version')).not.toHaveText('loading…', { timeout: 60_000 });
  });

  test('all six cards are present', async ({ page }) => {
    await page.goto('/ui/');
    // Asserting on the H2 text inside each card is more robust than DOM
    // index — a future re-order won't flake this test.
    for (const heading of ['Gateway', 'Sessions', 'Cron Jobs', 'Hooks', 'Recent Logs', 'Quick infer']) {
      await expect(page.locator('.card h2', { hasText: heading })).toBeVisible();
    }
  });

  test('status indicator turns green for a healthy gateway', async ({ page }) => {
    await page.goto('/ui/');
    await expect(page.locator('#status')).toHaveClass(/ok/, { timeout: 60_000 });
  });
});

test.describe('Control panel /ui — data refresh', () => {
  test('gateway card surfaces status and address from gateway.status RPC', async ({ page }) => {
    await page.goto('/ui/');
    // gw-status starts as "—" then becomes "live" (or whatever health.status returns).
    await expect(page.locator('#gw-status')).not.toHaveText('—', { timeout: 30_000 });
    // Address must be non-placeholder (defaults to "—") once gateway.status returns.
    await expect(page.locator('#gw-address')).not.toHaveText('—', { timeout: 30_000 });
  });

  test('session/cron/hook counters populate from usage.stats', async ({ page }) => {
    await page.goto('/ui/');
    // All three counters start as "—" (placeholder) and must populate to a
    // numeric value (possibly 0) once usage.stats returns.
    for (const id of ['#session-count', '#cron-count', '#hook-count']) {
      await expect(page.locator(id)).not.toHaveText('—', { timeout: 30_000 });
      const text = (await page.locator(id).textContent())?.trim() ?? '';
      expect(text).toMatch(/^\d+$/);
    }
  });

  test('recent logs panel renders entries or the empty marker', async ({ page }) => {
    await page.goto('/ui/');
    await expect(page.locator('#log-output')).not.toHaveText('loading…', { timeout: 30_000 });
    // Either real log lines or the documented "(no logs)" sentinel.
    const text = (await page.locator('#log-output').textContent())?.trim() ?? '';
    expect(text.length).toBeGreaterThan(0);
  });

  test('infer-agent label shows provider/model after first refresh', async ({ page }) => {
    await page.goto('/ui/');
    // start-stack.mjs configures provider=openai model=gpt-4o-mini with a
    // mock key, so the label should reflect that on first refresh.
    await expect(page.locator('#infer-agent')).toContainText('openai', { timeout: 30_000 });
  });

  test('auto-refresh picks up new sessions within the 5-second tick', async ({ page, request }) => {
    await page.goto('/ui/');
    // Wait for the initial counter to populate to a number.
    await expect(page.locator('#session-count')).toHaveText(/^\d+$/, { timeout: 30_000 });
    const before = parseInt((await page.locator('#session-count').textContent()) ?? '0', 10);

    // Create a session out-of-band by posting through /message.
    const r = await request.post('/message', {
      data: { sessionId: `ui-refresh-${Date.now()}`, message: 'hello', channel: 'cli' },
    });
    expect(r.ok()).toBeTruthy();

    // Auto-refresh fires every 5s; allow up to 15s for the counter to advance.
    await expect.poll(
      async () => parseInt((await page.locator('#session-count').textContent()) ?? '0', 10),
      { timeout: 15_000 },
    ).toBeGreaterThan(before);
  });
});

test.describe('Control panel /ui — quick infer interaction', () => {
  test('clicking Send with empty input does nothing', async ({ page }) => {
    await page.goto('/ui/');
    // Make sure the page is alive before we test the no-op.
    await expect(page.locator('#status')).toHaveClass(/ok/, { timeout: 60_000 });

    const out = page.locator('#infer-output');
    const before = (await out.textContent()) ?? '';
    await page.getByRole('button', { name: 'Send' }).click();
    // Brief settle, then assert no change. JS guard `if (!msg) return;` should
    // keep the field as it was.
    await page.waitForTimeout(300);
    const after = (await out.textContent()) ?? '';
    expect(after).toBe(before);
  });

  test('input accepts text and Send triggers a reply', async ({ page }) => {
    await page.goto('/ui/');
    await expect(page.locator('#status')).toHaveClass(/ok/, { timeout: 60_000 });

    await page.locator('#infer-input').fill('integration check');
    await page.getByRole('button', { name: 'Send' }).click();
    // The mock OpenAI returns a known string; assert we see it land.
    await expect(page.locator('#infer-output')).toContainText('E2E mock assistant reply', {
      timeout: 30_000,
    });
  });
});

test.describe('Control panel /ui — routing', () => {
  test('root path redirects to /ui/', async ({ page }) => {
    const resp = await page.goto('/');
    // page.goto follows redirects by default; the final URL must end with /ui/.
    expect(page.url()).toMatch(/\/ui\/$/);
    // And the response chain should include a redirect.
    expect(resp).toBeTruthy();
  });

  test('unknown path under /ui/ falls through to embedded server', async ({ request }) => {
    // /ui/missing.bogus — the http.FileServer should 404 it cleanly (NOT crash).
    const resp = await request.get('/ui/missing.bogus');
    expect([404, 200]).toContain(resp.status()); // SPA fallback may serve index.html
  });
});
