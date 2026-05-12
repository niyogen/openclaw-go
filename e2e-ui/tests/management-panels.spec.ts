import { test, expect } from '@playwright/test';

// UI management surfaces added in iter 3/3 of v0.4.0. Each panel drives
// real RPCs through the embedded Control Panel JS; these tests stage
// gateway state via /rpc + /message and then assert what the UI shows.

test.describe('Management panel: pending approvals', () => {
  test('shows empty marker when there are no approvals', async ({ page }) => {
    await page.goto('/ui/');
    await expect(page.locator('#approvals-list')).toHaveText(/no pending approvals|loading…/, {
      timeout: 30_000,
    });
    // After the first refresh tick the empty marker should land.
    await expect(page.locator('#approvals-list')).toContainText('no pending approvals', {
      timeout: 15_000,
    });
  });
});

test.describe('Management panel: compactions', () => {
  test('blank session id shows guidance', async ({ page }) => {
    await page.goto('/ui/');
    // Status indicator must be green before we interact.
    await expect(page.locator('#status')).toHaveClass(/ok/, { timeout: 60_000 });
    await page.getByRole('button', { name: 'Load' }).click();
    await expect(page.locator('#compaction-list')).toContainText('enter a session id');
  });

  test('unknown session returns empty marker', async ({ page }) => {
    await page.goto('/ui/');
    await expect(page.locator('#status')).toHaveClass(/ok/, { timeout: 60_000 });
    await page.locator('#compaction-session').fill('sess-does-not-exist-xyz');
    await page.getByRole('button', { name: 'Load' }).click();
    await expect(page.locator('#compaction-list')).toContainText(/no compactions/, {
      timeout: 5_000,
    });
  });

  test('lists records after a real compaction was made', async ({ page, request }) => {
    // Stage: create a session by sending messages, then compact it. The
    // mock OpenAI in start-stack.mjs returns one canned reply, so each
    // /message lands a user + assistant pair.
    const sessId = `ui-compact-${Date.now()}`;
    for (let i = 0; i < 4; i++) {
      const resp = await request.post('/message', {
        data: { sessionId: sessId, channel: 'cli', message: 'seed ' + i },
      });
      expect(resp.ok()).toBeTruthy();
    }
    // POST /sessions/{id}/compact with keepN=2.
    const compactResp = await request.post(`/sessions/${sessId}/compact`, {
      data: { keepN: 2 },
    });
    expect(compactResp.ok()).toBeTruthy();

    await page.goto('/ui/');
    await expect(page.locator('#status')).toHaveClass(/ok/, { timeout: 60_000 });
    await page.locator('#compaction-session').fill(sessId);
    await page.getByRole('button', { name: 'Load' }).click();
    // The compaction id is prefixed `cmp_` per Store.recordCompactionLocked.
    await expect(page.locator('#compaction-list')).toContainText(/cmp_/, { timeout: 5_000 });
    // Restore + Branch buttons must be rendered.
    await expect(page.locator('#compaction-list').getByRole('button', { name: 'Restore' }).first()).toBeVisible();
    await expect(
      page.locator('#compaction-list').getByRole('button', { name: 'Branch into new session' }).first(),
    ).toBeVisible();
  });
});

test.describe('Management panel: push subscriptions', () => {
  // The start-stack.mjs stack does NOT set gateway.pushContact, so push
  // is disabled by design. The UI must surface that clearly and disable
  // the test button — never crash, never lie about state.
  test('renders "not configured" when push contact is unset', async ({ page }) => {
    await page.goto('/ui/');
    await expect(page.locator('#push-status')).toContainText(/not configured/, { timeout: 60_000 });
    await expect(page.locator('#push-test-button')).toBeDisabled();
  });
});
