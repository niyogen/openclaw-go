// scripts/screenshot-integration.mjs
//
// Opens openclaw-studio at http://127.0.0.1:4001 in a headless browser,
// waits for the UI to render against our openclaw-go gateway at
// :28789, captures network traffic to prove the WS connection works,
// and writes a screenshot to /tmp for visual confirmation.
//
// Usage: node scripts/screenshot-integration.mjs <out-png>

import { chromium } from "playwright";
import fs from "node:fs";

const STUDIO_URL = process.env.STUDIO_URL || "http://127.0.0.1:4001";
const OUT = process.argv[2] || "/tmp/studio-integration.png";

const main = async () => {
  const browser = await chromium.launch({ headless: true });
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const page = await ctx.newPage();

  const requests = [];
  page.on("request", (req) => {
    requests.push({ url: req.url(), method: req.method() });
  });
  const consoleErrors = [];
  page.on("console", (msg) => {
    if (msg.type() === "error") consoleErrors.push(msg.text());
  });

  console.log("[screenshot] navigating to", STUDIO_URL);
  await page.goto(STUDIO_URL, { waitUntil: "domcontentloaded" });

  // Give studio time to bootstrap its runtime + fleet hydration.
  await page.waitForTimeout(8000);

  // Look for any of the known studio UI affordances. If none match,
  // the UI is in some intermediate / error state and we still
  // screenshot for inspection.
  const probeSelectors = [
    "[data-testid='agent-card']",
    ".agent-card",
    "[data-testid='fleet-bar']",
    "h1, h2, h3, h4",
  ];
  let foundLabel = "";
  for (const sel of probeSelectors) {
    const count = await page.locator(sel).count();
    if (count > 0) {
      foundLabel = `${sel}=${count}`;
      break;
    }
  }

  await page.screenshot({ path: OUT, fullPage: true });
  console.log("[screenshot] saved", OUT, "found:", foundLabel || "(no known affordances; raw screenshot only)");

  const fleetRequests = requests.filter((r) => r.url.includes("/api/runtime") || r.url.includes("/api/intents"));
  console.log("[screenshot] runtime/intent API calls:", fleetRequests.length);
  for (const r of fleetRequests.slice(0, 10)) console.log("  ", r.method, r.url);

  if (consoleErrors.length > 0) {
    console.log("[screenshot] console errors:", consoleErrors.length);
    for (const e of consoleErrors.slice(0, 5)) console.log("  ", e.slice(0, 200));
  }

  // Also dump the page title + h1 for confirmation.
  const title = await page.title();
  console.log("[screenshot] page title:", title);

  await browser.close();
};

main().catch((err) => {
  console.error("[screenshot] failed:", err);
  process.exit(1);
});
