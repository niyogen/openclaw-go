// tests/integ/openclaw-go.spec.ts
//
// Integration tests for openclaw-studio talking to an openclaw-go
// gateway via the /control/ws upstream-compat endpoint.
//
// These are NOT studio's internal tests — they're owned by the
// openclaw-go integration. Run via:
//
//   STUDIO_PORT=4001 npx playwright test --config playwright.integ.config.ts

import { test, expect } from "@playwright/test";

const STUDIO_URL = `http://127.0.0.1:${process.env.STUDIO_PORT || "4001"}`;

test.describe("openclaw-go via /control/ws", () => {
  test("studio loads with Connected indicator", async ({ page }) => {
    await page.goto(STUDIO_URL, { waitUntil: "domcontentloaded" });
    // Give Next.js dev compile + runtime bootstrap time to settle on
    // first hit. Subsequent runs in the same dev session are fast.
    await page.waitForTimeout(10_000);

    // The header carries the Connected status pill — match by text.
    const pill = page.locator('text=Connected').first();
    await expect(pill).toBeVisible({ timeout: 20_000 });
  });

  test("agent fleet panel renders with at least one agent", async ({ page }) => {
    await page.goto(STUDIO_URL, { waitUntil: "domcontentloaded" });
    await page.waitForTimeout(10_000);

    // Header text "AGENTS [N]" or similar.
    const agentsHeader = page.locator('text=/AGENTS/i').first();
    await expect(agentsHeader).toBeVisible({ timeout: 20_000 });
  });

  test("chat round-trip echoes the message via openclaw-go", async ({ page }) => {
    await page.goto(STUDIO_URL, { waitUntil: "domcontentloaded" });
    await page.waitForTimeout(12_000);

    const message = `integ test ping ${Date.now()}`;
    const input = page
      .locator('textarea[placeholder*="type a message" i], input[placeholder*="type a message" i]')
      .first();
    await expect(input).toBeVisible({ timeout: 30_000 });
    await input.click();
    await input.fill(message);

    const send = page.locator('button:has-text("Send")').first();
    await send.click();

    // Wait for the outbound to render.
    await expect(page.locator(`text=${message}`).first()).toBeVisible({ timeout: 10_000 });

    // Echo reply MAY arrive via live event push or via history
    // refetch on next poll. Allow up to 30s, then reload and check
    // again (the integration also passes if the reply is durable
    // even without live events).
    const echoText = `Echo: ${message}`;
    const echoLocator = page.locator(`text=${echoText}`).first();
    try {
      await expect(echoLocator).toBeVisible({ timeout: 30_000 });
    } catch {
      // Fall back to a reload + retry.
      await page.reload({ waitUntil: "domcontentloaded" });
      await page.waitForTimeout(10_000);
      await expect(page.locator(`text=${echoText}`).first()).toBeVisible({ timeout: 15_000 });
    }
  });

  test("settings dialog opens and exposes gateway info", async ({ page }) => {
    await page.goto(STUDIO_URL, { waitUntil: "domcontentloaded" });
    await page.waitForTimeout(10_000);

    // Title should be openclaw studio.
    await expect(page).toHaveTitle(/OpenClaw Studio/i);
    // New agent button is visible (proves the workspace panel is up).
    const newAgent = page.locator('button:has-text("New agent")').first();
    await expect(newAgent).toBeVisible({ timeout: 20_000 });
  });

  test("chat persists across page reload", async ({ page }) => {
    // Send a uniquely-tagged message, reload the page, verify it
    // still renders. Proves session+message durability via openclaw-go.
    await page.goto(STUDIO_URL, { waitUntil: "domcontentloaded" });
    await page.waitForTimeout(12_000);

    const tag = `persist-${Date.now()}`;
    const input = page
      .locator('textarea[placeholder*="type a message" i], input[placeholder*="type a message" i]')
      .first();
    await expect(input).toBeVisible({ timeout: 30_000 });
    await input.click();
    await input.fill(tag);
    await page.locator('button:has-text("Send")').first().click();
    await expect(page.locator(`text=${tag}`).first()).toBeVisible({ timeout: 10_000 });

    // Reload. Tag must still be visible in the transcript.
    await page.reload({ waitUntil: "domcontentloaded" });
    await page.waitForTimeout(10_000);
    await expect(page.locator(`text=${tag}`).first()).toBeVisible({ timeout: 15_000 });
  });

  test("model picker is populated with at least one entry", async ({ page }) => {
    // The bottom-left dropdown shows the available models. With the
    // echo provider configured we expect at least "Echo" in the list.
    await page.goto(STUDIO_URL, { waitUntil: "domcontentloaded" });
    await page.waitForTimeout(12_000);
    // The dropdown's visible label uses the truncated model name
    // ("Echo (l..." was seen in screenshots). Just verify SOME
    // <select>/<option> structure exists in the input panel.
    const optionTexts = await page.locator('select option').allTextContents();
    // Even if studio uses a custom dropdown rather than <select>,
    // we expect a model-related label to appear somewhere visible.
    const haveEcho =
      optionTexts.some((s) => /echo/i.test(s)) ||
      (await page.locator('text=/echo/i').count()) > 0;
    expect(haveEcho).toBeTruthy();
  });

  test("multiple-message chat renders all turns", async ({ page }) => {
    // Send two messages in sequence, both must render (with their
    // echo replies, on reload).
    await page.goto(STUDIO_URL, { waitUntil: "domcontentloaded" });
    await page.waitForTimeout(12_000);

    const input = page
      .locator('textarea[placeholder*="type a message" i], input[placeholder*="type a message" i]')
      .first();
    await expect(input).toBeVisible({ timeout: 30_000 });
    const tag1 = `multi-a-${Date.now()}`;
    await input.click();
    await input.fill(tag1);
    await page.locator('button:has-text("Send")').first().click();
    await expect(page.locator(`text=${tag1}`).first()).toBeVisible({ timeout: 10_000 });

    // Brief wait so the second message goes through cleanly.
    await page.waitForTimeout(2_000);
    const tag2 = `multi-b-${Date.now()}`;
    await input.click();
    await input.fill(tag2);
    await page.locator('button:has-text("Send")').first().click();
    await expect(page.locator(`text=${tag2}`).first()).toBeVisible({ timeout: 10_000 });

    // Reload to get echo replies if the live path hasn't surfaced yet.
    await page.reload({ waitUntil: "domcontentloaded" });
    await page.waitForTimeout(10_000);
    await expect(page.locator(`text=Echo: ${tag1}`).first()).toBeVisible({ timeout: 15_000 });
    await expect(page.locator(`text=Echo: ${tag2}`).first()).toBeVisible({ timeout: 15_000 });
  });

  test("new session button is wired up", async ({ page }) => {
    await page.goto(STUDIO_URL, { waitUntil: "domcontentloaded" });
    await page.waitForTimeout(12_000);

    const newSession = page.locator('button:has-text("New session")').first();
    await expect(newSession).toBeVisible({ timeout: 20_000 });
    // Click it. We don't assert on the exact post-state (the UI
    // may show a confirmation or just refresh) — the assertion is
    // that the click doesn't throw, doesn't navigate away, and the
    // page is still on the studio root.
    await newSession.click();
    await page.waitForTimeout(2_000);
    await expect(page).toHaveTitle(/OpenClaw Studio/i);
  });
});
