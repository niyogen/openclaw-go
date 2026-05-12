import { test, expect } from '@playwright/test';

// Exercises the device-code-style web-login HTML surface added in v0.3.1.
// The flow:  POST /rpc {method:web.login.start} → navigate to /web/login/{nonce}
// in the browser → click Approve → confirm page returns approval JSON with a
// fresh token → POST /rpc {method:web.login.wait} returns the matching token.
//
// The stack starts with no auth configured (start-stack.mjs), so the confirm
// POST is open per the documented "initial setup" posture in SECURITY.md.
test.describe('Web login (/web/login/{nonce})', () => {
  test('confirm page approves and issues a token', async ({ page, request, baseURL }) => {
    // Start an attempt via JSON-RPC.
    const startResp = await request.post('/rpc', {
      data: { jsonrpc: '2.0', id: 1, method: 'web.login.start', params: {} },
    });
    expect(startResp.ok()).toBeTruthy();
    const startBody = await startResp.json();
    expect(startBody.result).toBeTruthy();
    const { nonce, url: approvalUrl } = startBody.result;
    expect(nonce).toMatch(/^[0-9a-f]{64}$/); // 32 random bytes hex-encoded
    expect(approvalUrl).toMatch(/^\/web\/login\/[0-9a-f]{64}$/);

    // Render the confirm page in the browser.
    await page.goto(approvalUrl);
    await expect(page).toHaveTitle(/openclaw login/i);
    await expect(page.locator('text=Approve gateway login')).toBeVisible();
    // Both Approve and Reject buttons should be present.
    await expect(page.getByRole('button', { name: 'Approve' })).toBeVisible();
    await expect(page.getByRole('button', { name: 'Reject' })).toBeVisible();

    // Approve via the form submit. The confirm endpoint responds with JSON,
    // so we navigate via request rather than .click() to inspect the body
    // directly (clicking the button would replace the page with JSON text,
    // which is awkward to assert against).
    const confirmResp = await request.post(`${approvalUrl}/confirm`);
    expect(confirmResp.ok()).toBeTruthy();
    const confirmBody = await confirmResp.json();
    expect(confirmBody.ok).toBe(true);
    expect(confirmBody.status).toBe('approved');
    expect(typeof confirmBody.token).toBe('string');
    expect(confirmBody.token.length).toBeGreaterThan(0);

    // web.login.wait should return immediately with the approved snapshot.
    const waitResp = await request.post('/rpc', {
      data: {
        jsonrpc: '2.0', id: 2,
        method: 'web.login.wait',
        params: { nonce },
      },
    });
    expect(waitResp.ok()).toBeTruthy();
    const waitBody = await waitResp.json();
    expect(waitBody.result.status).toBe('approved');
    expect(waitBody.result.issuedToken).toBe(confirmBody.token);
  });

  test('confirm page handles reject path', async ({ request }) => {
    const startResp = await request.post('/rpc', {
      data: { jsonrpc: '2.0', id: 1, method: 'web.login.start', params: {} },
    });
    const { nonce, url: approvalUrl } = (await startResp.json()).result;

    const rejectResp = await request.post(`${approvalUrl}/confirm?approve=false`);
    expect(rejectResp.ok()).toBeTruthy();
    const body = await rejectResp.json();
    expect(body.status).toBe('rejected');
    expect(body.token).toBe('');

    const waitResp = await request.post('/rpc', {
      data: { jsonrpc: '2.0', id: 2, method: 'web.login.wait', params: { nonce } },
    });
    const waitResult = (await waitResp.json()).result;
    expect(waitResult.status).toBe('rejected');
    // issuedToken should be absent or empty after rejection.
    expect(waitResult.issuedToken || '').toBe('');
  });

  test('unknown nonce returns 404 from the GET page', async ({ request }) => {
    const resp = await request.get('/web/login/bogus-nonce-not-in-registry');
    expect(resp.status()).toBe(404);
  });

  test('confirming twice returns "already decided"', async ({ request }) => {
    const startResp = await request.post('/rpc', {
      data: { jsonrpc: '2.0', id: 1, method: 'web.login.start', params: {} },
    });
    const { url: approvalUrl } = (await startResp.json()).result;

    const first = await request.post(`${approvalUrl}/confirm`);
    expect((await first.json()).status).toBe('approved');

    // Second confirm must fail with a 4xx — the server tracks decided state.
    const second = await request.post(`${approvalUrl}/confirm`);
    expect(second.status()).toBe(400);
    const errBody = await second.json();
    expect(errBody.error || '').toMatch(/already decided|not found|expired/i);
  });

  test('start accepts a custom ttlSeconds parameter', async ({ request }) => {
    const startResp = await request.post('/rpc', {
      data: {
        jsonrpc: '2.0', id: 1,
        method: 'web.login.start',
        params: { ttlSeconds: 30 },
      },
    });
    const { nonce, expiresAt } = (await startResp.json()).result;
    expect(nonce).toMatch(/^[0-9a-f]{64}$/);
    // expiresAt should be within ~30 seconds of now (allow generous slack).
    const expiry = new Date(expiresAt).getTime();
    const now = Date.now();
    expect(expiry - now).toBeGreaterThan(20_000);
    expect(expiry - now).toBeLessThan(60_000);
  });
});
