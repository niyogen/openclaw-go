// scripts/chat-roundtrip-integration.mjs
//
// Exercises the full end-to-end chat path: load studio, type a
// message into the chat input, click Send, wait for the agent
// reply, screenshot the result. Proves /control/ws + chat.send
// + chat.history paths are wired.
//
// Usage: node scripts/chat-roundtrip-integration.mjs <out-png>

import { chromium } from "playwright";

const STUDIO_URL = process.env.STUDIO_URL || "http://127.0.0.1:4001";
const OUT = process.argv[2] || "/tmp/studio-chat.png";
const MESSAGE = "ping from playwright " + Date.now();

const main = async () => {
  const browser = await chromium.launch({ headless: true });
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const page = await ctx.newPage();

  const errors = [];
  page.on("pageerror", (e) => errors.push("page: " + e.message));
  page.on("console", (m) => {
    if (m.type() === "error") errors.push("console: " + m.text());
  });

  await page.goto(STUDIO_URL, { waitUntil: "domcontentloaded" });
  await page.waitForTimeout(12_000); // let bootstrap settle (cold compile is slow)

  // Find the chat input by its placeholder.
  const input = page.locator('textarea[placeholder*="type a message" i], input[placeholder*="type a message" i]').first();
  await input.waitFor({ timeout: 30_000 });
  await input.click();
  await input.fill(MESSAGE);

  // Click the Send button.
  const send = page.locator('button:has-text("Send")').first();
  await send.click();

  // Wait for the message to appear in the transcript.
  console.log("[chat] sent:", MESSAGE);
  // Poll for the echoed reply; give up after 25s total.
  const echoText = "Echo: " + MESSAGE;
  const start = Date.now();
  let seenEcho = false;
  while (Date.now() - start < 25_000) {
    const text = await page.content();
    if (text.includes(echoText)) {
      seenEcho = true;
      break;
    }
    await page.waitForTimeout(500);
  }
  console.log("[chat] echo seen (live) in", Math.round((Date.now() - start) / 1000) + "s:", seenEcho);

  // If live updates didn't surface it, reload and re-check —
  // confirms the chat was stored and the integration end-to-end
  // works even if live events lag.
  if (!seenEcho) {
    console.log("[chat] reloading page to force history refetch...");
    await page.reload({ waitUntil: "domcontentloaded" });
    await page.waitForTimeout(10_000);
    const text2 = await page.content();
    seenEcho = text2.includes(echoText);
    console.log("[chat] echo seen (after reload):", seenEcho);
  }

  await page.screenshot({ path: OUT, fullPage: true });
  console.log("[chat] screenshot saved", OUT);

  // Find every visible block of text on the page; if our message
  // text is there, the studio at least rendered our outbound.
  const html = await page.content();
  const sent = html.includes(MESSAGE);
  const echoed = html.includes("Echo: " + MESSAGE);
  console.log("[chat] outbound rendered:", sent);
  console.log("[chat] echo reply rendered:", echoed);

  if (errors.length) {
    console.log("[chat] errors during test:", errors.length);
    for (const e of errors.slice(0, 6)) console.log("  ", e.slice(0, 240));
  }

  await browser.close();
  if (!sent) process.exit(2);
  if (!echoed) process.exit(3);
  console.log("CHAT ROUND-TRIP PASSED");
};

main().catch((err) => {
  console.error("[chat] failed:", err);
  process.exit(1);
});
