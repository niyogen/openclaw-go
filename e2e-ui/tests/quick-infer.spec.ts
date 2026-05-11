import { test, expect } from '@playwright/test';

test.describe('Control panel /ui', () => {
  test('Quick infer uses OpenAI mock — reply is not Echo fallback', async ({ page }) => {
    await page.goto('/ui/');

    await expect(page.locator('#status')).toHaveClass(/ok/, { timeout: 60_000 });
    await expect(page.locator('#version')).not.toHaveText('loading…', { timeout: 60_000 });

    await page.locator('#infer-input').fill('hi');
    await page.getByRole('button', { name: 'Send' }).click();

    const out = page.locator('#infer-output');
    await expect(out).not.toHaveText('…', { timeout: 30_000 });
    await expect(out).not.toContainText('Echo:', { timeout: 5_000 });
    await expect(out).toContainText('E2E mock assistant reply', { timeout: 5_000 });
  });
});
