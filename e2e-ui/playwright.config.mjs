// @ts-check
import { defineConfig } from '@playwright/test';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const port = process.env.E2E_GATEWAY_PORT || '18889';

export default defineConfig({
  testDir: 'tests',
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: 1,
  timeout: 90_000,
  use: {
    baseURL: `http://127.0.0.1:${port}`,
    trace: 'on-first-retry',
  },
  webServer: {
    command: `node "${path.join(__dirname, 'start-stack.mjs')}"`,
    url: `http://127.0.0.1:${port}/health`,
    timeout: 120_000,
    reuseExistingServer: !!process.env.E2E_REUSE_STACK,
    stdout: 'pipe',
    stderr: 'pipe',
  },
});
