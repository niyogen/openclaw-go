/**
 * Playwright webServer entry: mock OpenAI + real openclaw-go gateway on E2E_GATEWAY_PORT (default 18889).
 * Stays alive until SIGINT/SIGTERM (Playwright tears down webServer when tests finish).
 */
import http from 'node:http';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(__dirname, '..');
const gatewayPort = Number(process.env.E2E_GATEWAY_PORT || 18889);

const dataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'openclaw-playwright-'));
const configPath = path.join(dataDir, 'openclaw.json');

const mockAssistantText = 'E2E mock assistant reply (not echo).';

const mock = http.createServer((req, res) => {
  if (req.method === 'POST' && req.url === '/v1/chat/completions') {
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(
      JSON.stringify({
        choices: [
          {
            message: { role: 'assistant', content: mockAssistantText },
          },
        ],
      }),
    );
    return;
  }
  res.writeHead(404);
  res.end();
});

await new Promise((resolve, reject) => {
  mock.listen(0, '127.0.0.1', (err) => (err ? reject(err) : resolve()));
});
const mockPort = /** @type {import('node:net').AddressInfo} */ (mock.address()).port;

const cfg = {
  gateway: { host: '127.0.0.1', port: gatewayPort },
  agent: { provider: 'openai', model: 'gpt-4o-mini' },
  providers: {
    openai: {
      apiKey: 'sk-e2e-test-not-real',
      baseUrl: `http://127.0.0.1:${mockPort}/v1`,
      model: 'gpt-4o-mini',
    },
    anthropic: { apiKey: '', baseUrl: '', model: 'claude-3-5-haiku-20241022' },
  },
};
fs.writeFileSync(configPath, JSON.stringify(cfg, null, 2), 'utf8');

const goArgs = ['run', './cmd/openclaw', 'gateway'];
const gw = spawn('go', goArgs, {
  cwd: repoRoot,
  env: {
    ...process.env,
    OPENCLAW_CONFIG_PATH: configPath,
    OPENCLAW_DATA_DIR: dataDir,
    OPENCLAW_GATEWAY_HOST: '127.0.0.1',
  },
  stdio: 'inherit',
  windowsHide: true,
});

function shutdown() {
  try {
    gw.kill('SIGTERM');
  } catch {
    /* ignore */
  }
  try {
    mock.close();
  } catch {
    /* ignore */
  }
  try {
    fs.rmSync(dataDir, { recursive: true, force: true });
  } catch {
    /* ignore */
  }
  process.exit(0);
}

process.on('SIGINT', shutdown);
process.on('SIGTERM', shutdown);

const healthURL = `http://127.0.0.1:${gatewayPort}/health`;
const deadline = Date.now() + 90_000;
// eslint-disable-next-line no-constant-condition
while (true) {
  if (Date.now() > deadline) {
    console.error('timeout waiting for gateway /health');
    shutdown();
    process.exit(1);
  }
  try {
    const r = await fetch(healthURL);
    if (r.ok) break;
  } catch {
    /* retry */
  }
  await new Promise((r) => setTimeout(r, 250));
}

console.log(`[e2e-ui] mock OpenAI 127.0.0.1:${mockPort}`);
console.log(`[e2e-ui] gateway ${healthURL}`);

await new Promise(() => {
  /* block until signal */
});
