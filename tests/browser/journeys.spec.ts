import { test, expect, Page, BrowserContext, APIRequestContext } from '@playwright/test';

const base = process.env.BROWSER_BASE_URL!;
const secure = process.env.BROWSER_HTTPS_URL!;
const fixture = process.env.BROWSER_FIXTURE_URL!;
const token = process.env.BROWSER_FIXTURE_TOKEN!;
const key = process.env.BROWSER_GATEWAY_KEY!;
type Observation = { id: number; body: { model: string; messages: unknown[] }; cancelled: boolean; released: boolean };
type State = { requests: Observation[]; methods: string[]; signedIn: boolean };
async function control(api: APIRequestContext, path: string, data?: unknown) {
  const response = await api.post(fixture + '/control/' + path, { headers: { Authorization: `Bearer ${token}` }, data });
  expect(response.ok()).toBeTruthy();
  return response;
}
async function state(api: APIRequestContext): Promise<State> { return (await control(api, 'state')).json(); }
async function count(api: APIRequestContext) { return (await state(api)).requests.length; }
async function login(page: Page, origin = base) {
  await page.goto(origin + '/ui/');
  await page.getByLabel('Gateway key').fill(key);
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(page.getByRole('navigation', { name: 'Main navigation' })).toBeVisible();
}
async function playground(page: Page) { await page.getByRole('link', { name: 'Playground', exact: true }).click(); await expect(page.getByRole('heading', { name: 'Playground', exact: true })).toBeVisible(); }
async function send(page: Page, alias: string, prompt: string) {
  const select = page.getByLabel('Route alias');
  if (await select.evaluate(el => el.tagName === 'SELECT')) await select.selectOption(alias);
  await page.getByLabel('Message', { exact: true }).fill(prompt);
  await page.getByRole('button', { name: 'Send message', exact: true }).click();
}
async function started(api: APIRequestContext, before: number) {
  await expect.poll(() => count(api)).toBe(before + 1);
  return (await state(api)).requests[before];
}
async function release(api: APIRequestContext, id: number) { await control(api, 'release', { ID: id }); }
async function csrf(page: Page) { return page.locator('form[action="/ui/logout"] input[name="csrf"]').inputValue(); }
async function conversation(page: Page) { return page.locator('form[action="/ui/playground/new"] input[name="conversation_id"]').inputValue(); }
async function allowNetwork(context: BrowserContext) {
  // Observe, never intercept: all browser requests must stay on owned loopback origins.
  const allowed = new Set([new URL(base).origin, new URL(secure).origin, new URL(fixture).origin, new URL(process.env.BROWSER_ATTACKER_URL!).origin]);
  context.on('request', request => {
    const url = new URL(request.url());
    expect(allowed.has(url.origin), `forbidden external browser request: ${url.origin}`).toBeTruthy();
  });
}
test.beforeEach(async ({ context }) => { await allowNetwork(context); });

test('B1 real forms, preauth, CSRF, origin, revocation and HTTP/HTTPS cookies', async ({ page, context, browser, request }) => {
  const missing = await request.post(base + '/ui/login', { form: { key, csrf: 'forged' }, headers: { Origin: base } });
  expect(missing.status()).toBe(403);
  await page.goto('/ui/login');
  await page.getByLabel('Gateway key').fill('wrong-browser-key');
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(page.getByRole('alert')).toContainText('Invalid gateway key');
  await login(page);
  const cookies = await context.cookies(base + '/ui/');
  const session = cookies.find(c => c.name.includes('session'));
  expect(session).toBeDefined();
  expect(session!.httpOnly).toBe(true); expect(session!.sameSite).toBe('Strict'); expect(session!.secure).toBe(false); expect(session!.path).toBe('/ui');
  const old = await context.storageState();
  expect((await context.request.post(base + '/ui/logout', { form: { csrf: 'forged' }, headers: { Origin: base } })).status()).toBe(403);
  expect((await context.request.post(base + '/ui/logout', { form: { csrf: await csrf(page) }, headers: { Origin: 'https://attacker.invalid' } })).status()).toBe(403);
  await page.goto(process.env.BROWSER_ATTACKER_URL! + '/attacker?target=' + encodeURIComponent(base));
  const rejectedForm = page.waitForResponse(r => r.url() === base + '/ui/logout' && r.request().method() === 'POST');
  await page.getByRole('button', { name: 'Attempt cross-origin logout' }).click();
  const rejected = await rejectedForm;
  expect(rejected.status()).toBe(403);
  expect((await rejected.request().allHeaders()).cookie ?? '').not.toContain('omp_ui_session=');
  await page.goto(base + '/ui/');
  await expect(page.getByRole('button', { name: 'Sign out' })).toBeVisible();
  await page.getByRole('button', { name: 'Sign out' }).click();
  await expect(page.getByLabel('Gateway key')).toBeVisible();
  const replay = await browser.newContext({ storageState: old });
  try { const p = await replay.newPage(); await p.goto(base + '/ui/playground'); await expect(p.getByLabel('Gateway key')).toBeVisible(); } finally { await replay.close(); }
  const tls = await browser.newContext({ ignoreHTTPSErrors: true });
  try {
    await allowNetwork(tls); const p = await tls.newPage(); await login(p, secure);
    const secureSession = (await tls.cookies(secure + '/ui/')).find(c => c.name.includes('session'));
    expect(secureSession).toMatchObject({ secure: true, httpOnly: true, sameSite: 'Strict', path: '/ui' });
    await p.getByRole('button', { name: 'Sign out' }).click(); await expect(p.getByLabel('Gateway key')).toBeVisible();
  } finally { await tls.close(); }
});

test('B2 routes and HTMX usage filters use real inference and distinguish unknown cost', async ({ page, request }) => {
  const before = await count(request);
  const reply = await request.post(base + '/v1/chat/completions', { headers: { Authorization: `Bearer ${key}` }, data: { model: 'chat', messages: [{ role: 'user', content: 'usage seed' }] } });
  expect(reply.status()).toBe(200); await started(request, before);
  await login(page);
  await page.getByRole('link', { name: 'Routes', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Routes', exact: true })).toBeVisible();
  await expect(page.locator('main')).toContainText('chat');
  await page.getByRole('link', { name: 'Usage', exact: true }).click();
  await expect(async () => {
    await page.reload(); await expect(page.getByRole('region', { name: 'Recent usage records' })).toContainText('chat');
  }).toPass({ timeout: 10000 });
  await expect(page.getByRole('region', { name: 'Recent usage records' })).toContainText('unreported');
  const unknownRow = page.getByRole('region', { name: 'Recent usage records' }).getByRole('row').filter({ hasText: 'chat' }).first();
  await expect(unknownRow.getByRole('cell').nth(6)).toHaveText('Unknown / Unknown');
  await expect(unknownRow.getByRole('cell').nth(8)).toContainText('Unknown');
  await page.getByLabel('Alias', { exact: true }).fill('no-such-browser-alias');
  const swapped = page.waitForResponse(r => r.url().includes('/ui/usage?') && r.request().headers()['hx-request'] === 'true');
  await page.getByRole('button', { name: 'Apply filters' }).click(); await swapped;
  await expect(page.getByText('No requests match these filters.')).toBeVisible();
  await page.getByLabel('Alias', { exact: true }).fill('chat');
  await page.getByLabel('Group by').selectOption('cost_source');
  await page.getByRole('button', { name: 'Apply filters' }).click();
  await expect(page.getByRole('region', { name: 'Usage group totals' })).toContainText('unreported');
  const unchanged = await count(request);
  await page.getByRole('link', { name: 'Overview', exact: true }).click();
  await expect(page.locator('main')).toContainText('fixture');
  expect(await count(request)).toBe(unchanged);
});

for (const alias of ['chat', 'messages']) {
  test(`B3 ${alias}: held deltas, reload without redispatch, inert text, two-turn history`, async ({ page, request }) => {
    await login(page); await playground(page);
    const before = await count(request);
    await send(page, alias, `opening-${alias}`); const first = await started(request, before);
    await expect(page.getByRole('region', { name: 'Conversation' })).toContainText(`partial-${first.id}`);
    expect(first.released).toBe(false);
    await page.reload();
    await expect(page.getByRole('region', { name: 'Conversation' })).toContainText(`partial-${first.id}`);
    expect(await count(request)).toBe(before + 1);
    expect(await page.evaluate(() => 'browserExecuted' in window)).toBe(false);
    await expect(page.locator('#conversation script')).toHaveCount(0);
    await release(request, first.id);
    await expect(page.getByRole('button', { name: 'Send message', exact: true })).toBeEnabled();
    await send(page, alias, `followup-${alias}`); const second = await started(request, before + 1);
    expect(JSON.stringify(second.body.messages)).toContain(`opening-${alias}`);
    expect(JSON.stringify(second.body.messages)).toContain(`partial-${first.id}`);
    expect(JSON.stringify(second.body.messages)).toContain(`followup-${alias}`);
    await release(request, second.id);
    await expect(page.getByRole('button', { name: 'Send message', exact: true })).toBeEnabled();
    await expect(page.locator('#conversation article pre').filter({ hasText: `opening-${alias}` })).toHaveCount(1);
    await expect(page.locator('#conversation article pre').filter({ hasText: `partial-${first.id}` })).toHaveCount(1);
    await page.reload(); expect(await count(request)).toBe(before + 2);
  });
}

test('B4 two independent operators cannot read or stop each other; Stop/New/logout cancel only owned work', async ({ browser, page, request }) => {
  const other = await browser.newContext();
  try {
    await allowNetwork(other); const second = await other.newPage();
    await login(page); await playground(page); await login(second); await playground(second);
    let n = await count(request); await send(page, 'chat', 'operator-one-secret'); const one = await started(request, n);
    n = await count(request); await send(second, 'chat', 'operator-two-secret'); const two = await started(request, n);
    await expect(page.locator('#conversation')).toContainText(`partial-${one.id}`);
    await expect(second.locator('#conversation')).toContainText(`partial-${two.id}`);
    const foreignID = await conversation(page);
    const denied = await other.request.post(base + '/ui/playground/stop', { headers: { Origin: base }, form: { csrf: await csrf(second), conversation_id: foreignID } });
    expect(denied.ok()).toBeTruthy();
    expect((await state(request)).requests[one.id].cancelled).toBe(false);
    expect((await state(request)).requests[two.id].cancelled).toBe(false);
    const stolen = await other.request.get(base + '/ui/playground/stream?conversation_id=' + foreignID);
    expect(stolen.status()).toBeGreaterThanOrEqual(400);
    await expect(second.locator('#conversation')).not.toContainText('operator-one-secret');
    await page.getByRole('button', { name: 'Stop generation' }).click();
    await expect.poll(async () => (await state(request)).requests[one.id].cancelled).toBe(true);
    expect((await state(request)).requests[two.id].cancelled).toBe(false);
    await expect(page.getByRole('button', { name: 'Send message', exact: true })).toBeDisabled();
    await page.getByRole('button', { name: 'New conversation' }).click();
    await expect(page.locator('#conversation')).not.toContainText('operator-one-secret');
    await expect(page.getByRole('button', { name: 'Send message', exact: true })).toBeEnabled();
    n = await count(request); await send(page, 'chat', 'fresh-owned-turn'); const fresh = await started(request, n);
    expect(JSON.stringify(fresh.body.messages)).not.toContain('operator-one-secret');
    await page.getByRole('button', { name: 'Sign out' }).click();
    await expect.poll(async () => (await state(request)).requests[fresh.id].cancelled).toBe(true);
    expect((await state(request)).requests[two.id].cancelled).toBe(false);
    await second.getByRole('button', { name: 'New conversation' }).click();
    await expect.poll(async () => (await state(request)).requests[two.id].cancelled).toBe(true);
    await expect(second.locator('#conversation')).not.toContainText('operator-two-secret');
  } finally { await other.close(); }
});

test('B5 explicit CLI consent, client tools are display-only, failures require New', async ({ page, request }) => {
  await login(page); await playground(page);
  await expect(page.getByLabel('Route alias').locator('option[value="responses"]')).toBeDisabled();
  const initial = (await state(request)).methods.filter(m => m === 'cli/run').length;
  await send(page, 'agent', 'client owned tool');
  await expect(page.getByRole('alert')).toBeVisible();
  expect((await state(request)).methods.filter(m => m === 'cli/run')).toHaveLength(initial);
  await page.getByLabel(/I understand CLI-backed routes/).check();
  await send(page, 'agent', 'client owned tool');
  await expect(page.locator('#conversation')).toContainText('never_execute_browser_tool');
  await expect(page.getByRole('status')).toContainText('Awaiting tools');
  await expect(page.getByRole('button', { name: 'Send message', exact: true })).toBeDisabled();
  expect((await state(request)).methods.filter(m => m === 'cli/run')).toHaveLength(initial + 1);
  expect((await state(request)).methods.some(m => m.startsWith('tool/'))).toBe(false);
  await page.getByRole('button', { name: 'New conversation' }).click();
  await expect(page.getByRole('combobox', { name: 'Route alias' })).toBeVisible();
  const n = await count(request); await send(page, 'chat', 'force stream failure'); const turn = await started(request, n);
  await expect(page.locator('#conversation')).toContainText(`partial-${turn.id}`);
  await control(request, 'fail', { ID: turn.id });
  await expect(page.getByRole('alert')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Send message', exact: true })).toBeDisabled();
  await page.getByRole('button', { name: 'New conversation' }).click();
  await expect(page.getByRole('button', { name: 'Send message', exact: true })).toBeEnabled();
  await expect(page.getByRole('combobox', { name: 'Route alias' })).toBeVisible();
  const beforeRecovery = await count(request);
  await send(page, 'chat', 'recovered after failed turn');
  const recovered = await started(request, beforeRecovery);
  await release(request, recovered.id);
  await expect(page.getByRole('button', { name: 'Send message', exact: true })).toBeEnabled();
  await expect(page.locator('#conversation')).toContainText(`partial-${recovered.id}`);
});

test('B6 Connections cached GET/poll is read-only, protocol actions and notification share account not browser auth', async ({ page, browser, request }) => {
  await login(page); await page.getByRole('link', { name: 'Connections', exact: true }).click();
  const panel = page.locator('#connections section.panel').filter({ has: page.getByRole('heading', { name: 'codex', exact: true }) });
  await expect(panel.getByRole('status')).toContainText('Account status not checked');
  const start = (await state(request)).methods.filter(m => m !== 'fixture/poll');
  await page.reload(); expect((await state(request)).methods.filter(m => m !== 'fixture/poll')).toEqual(start);
  await panel.getByRole('button', { name: 'Refresh account and catalog' }).click();
  await expect(panel.getByRole('status')).toContainText('Not signed in');
  await panel.getByRole('button', { name: 'Sign in with ChatGPT' }).click();
  const link = panel.getByRole('link', { name: 'Continue sign-in with OpenAI' });
  await expect(link).toHaveAttribute('href', 'https://auth.openai.com/browser-fixture');
  await expect(link).toHaveAttribute('target', '_blank'); await expect(link).toHaveAttribute('rel', /noopener/); await expect(link).toHaveAttribute('rel', /noreferrer/);
  const beforePoll = (await state(request)).methods.filter(m => m !== 'fixture/poll');
  const poll = page.waitForResponse(r => r.url() === base + '/ui/connections' && r.request().headers()['hx-request'] === 'true');
  await poll; expect((await state(request)).methods.filter(m => m !== 'fixture/poll')).toEqual(beforePoll);
  await panel.getByRole('button', { name: 'Cancel sign-in' }).click(); await expect(link).toHaveCount(0);
  await panel.getByRole('button', { name: 'Sign in with ChatGPT' }).click();
  await control(request, 'notify'); await expect(panel.getByRole('status')).toContainText('Signed in with ChatGPT');
  const other = await browser.newContext();
  try {
    await allowNetwork(other); const p = await other.newPage(); await p.goto(base + '/ui/connections'); await expect(p.getByLabel('Gateway key')).toBeVisible();
    await login(p); await p.getByRole('link', { name: 'Connections', exact: true }).click(); await expect(p.locator('#connections')).toContainText('Signed in with ChatGPT');
    await page.getByRole('button', { name: 'Sign out' }).click();
    await expect(p.getByRole('button', { name: 'Disconnect Codex account' })).toBeVisible();
    await p.getByRole('button', { name: 'Disconnect Codex account' }).click(); await expect(p.locator('#connections')).toContainText('Not signed in');
  } finally { await other.close(); }
  const methods = (await state(request)).methods;
  for (const method of ['account/read', 'model/list', 'account/login/start', 'account/login/cancel', 'account/logout']) expect(methods).toContain(method);
});
