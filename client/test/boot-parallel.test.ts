import test, { type TestContext } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import http from 'node:http';
import { sharedClientBuild } from './shared-client-build.ts';
import { cleanupStack, startChromium } from './chromium.ts';
const id = '11111111-1111-4111-8111-111111111111';
const issue = {
  project: 'first/shared',
  number: 1,
  desk: 101,
  title: '起動時の依頼',
  body: '本文',
  status: 'running',
  executionActive: true,
  actions: [],
  comments: [],
  images: [],
  createdAt: '2026-11-01T00:00:00Z',
  updatedAt: '2026-11-01T00:00:00Z',
};
const meta = {
  providers: [{ id: 'codex', name: 'Codex', ready: true }],
  projects: [
    { id: 'first/shared', name: 'shared' },
    { id: 'second/shared', name: 'shared' },
  ],
  worker: true,
  projectCreation: { owner: 'first' },
};
const draft = {
  id,
  project: 'first/shared',
  status: 'draft',
  body: '相談本文',
  title: '起動時の相談',
  messages: [{ id: 'u', role: 'user', text: '相談本文' }],
  actions: [],
  provider: 'codex',
};
const project = {
  id,
  status: 'draft',
  title: '起動時のプロジェクト',
  messages: [],
  actions: [],
};
async function setup(
  t: TestContext,
  hash: string,
  opts: { authenticated?: boolean; holdData?: boolean } = {},
) {
  const built = sharedClientBuild();
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'boot-parallel-'));
  const cleanup = cleanupStack(t);
  cleanup(() => fs.rmSync(dir, { recursive: true, force: true }));
  const requests: string[] = [];
  const listUrls: string[] = [];
  let metaResponse: http.ServerResponse | undefined,
    dataResponse: http.ServerResponse | undefined;
  const waiters = new Map<string, Array<() => void>>();
  function waitRequest(name: string) {
    if (requests.includes(name)) return Promise.resolve();
    return new Promise<void>((resolve) =>
      waiters.set(name, [...(waiters.get(name) ?? []), resolve]),
    );
  }
  const list = {
    full: true,
    scope: 'active',
    cursor: 'boot',
    rows: [{ key: 'issue:first/shared#1', type: 'issue', item: issue }],
    aux: { reviews: [], fullTests: [], stalls: [], attentionAlerts: [] },
    unavailable: [],
  };
  function json(response: http.ServerResponse, value: unknown, status = 200) {
    response.writeHead(status, { 'Content-Type': 'application/json' });
    response.end(JSON.stringify(value));
  }
  const server = http.createServer((request, response) => {
    const url = new URL(request.url!, 'http://localhost');
    if (url.pathname.startsWith('/api/')) {
      requests.push(url.pathname);
      for (const fn of waiters.get(url.pathname) ?? []) fn();
      waiters.delete(url.pathname);
      if (url.pathname === '/api/list')
        listUrls.push(url.pathname + url.search);
      if (url.pathname === '/api/session')
        return json(response, {
          authenticated: opts.authenticated !== false,
          csrf: 'fixture',
          local: false,
        });
      if (url.pathname === '/api/meta') {
        metaResponse = response;
        return;
      }
      if (url.pathname === '/api/events') {
        response.writeHead(200, { 'Content-Type': 'text/event-stream' });
        response.write(': connected\n\n');
        return;
      }
      if (url.pathname === '/api/list' && opts.holdData) {
        dataResponse = response;
        return;
      }
      if (url.pathname === '/api/list')
        return json(
          response,
          url.searchParams.has('since')
            ? {
                full: false,
                scope: 'active',
                cursor: 'next',
                added: [],
                updated: [
                  {
                    key: 'issue:first/shared#1',
                    type: 'issue',
                    item: { ...issue, title: '差分で更新' },
                  },
                ],
                removed: [],
                aux: {},
              }
            : list,
        );
      if (url.pathname === `/api/todos/${id}`)
        return json(response, {
          id,
          project: 'first/shared',
          body: '保存済みTODO',
          images: [],
          revision: 'fixture',
          actions: ['save'],
          provider: 'codex',
        });
      if (url.pathname === `/api/close-requests/${id}`)
        return json(response, { error: '見つかりません' }, 404);
      if (url.pathname === '/api/ssh-tickets')
        return json(response, { tickets: [] });
      if (url.pathname === '/api/permission-grants')
        return json(response, { grants: [] });
      if (
        url.pathname.startsWith('/api/ssh-tickets/') ||
        url.pathname.startsWith('/api/permission-grants/')
      )
        return json(response, { error: '見つかりません' }, 404);
      if (
        url.pathname === '/api/stats' ||
        url.pathname === '/api/stats/overview'
      )
        return json(response, { error: '集計できません' }, 500);
      if (url.pathname === `/api/drafts/${id}`) return json(response, draft);
      if (url.pathname === '/api/issue') return json(response, issue);
      if (url.pathname === `/api/projects/${id}`)
        return json(response, project);
      if (url.pathname === '/api/projects') return json(response, [project]);
      if (url.pathname === '/api/provider-disables')
        return json(response, { disables: [] });
      if (url.pathname === '/api/health')
        return json(response, { ready: true, ui: built.ui });
      return json(response, {});
    }
    const file = url.pathname === '/' ? 'index.html' : url.pathname.slice(1);
    const resolved = path.resolve(built.destination, file);
    if (
      !resolved.startsWith(built.destination + path.sep) ||
      !fs.existsSync(resolved)
    ) {
      response.writeHead(404);
      response.end();
      return;
    }
    response.setHeader(
      'Content-Type',
      file.endsWith('.js')
        ? 'text/javascript'
        : file.endsWith('.css')
          ? 'text/css'
          : 'text/html',
    );
    response.end(fs.readFileSync(resolved));
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  cleanup(async () => {
    server.closeAllConnections();
    await new Promise<void>((resolve, reject) =>
      server.close((error) => (error ? reject(error) : resolve())),
    );
  });
  const addr = server.address();
  assert.ok(addr && typeof addr === 'object');
  const browser = await startChromium(
    t,
    process.env.ISSUE_DESK_TEST_CHROMIUM,
    dir,
    `http://127.0.0.1:${addr.port}/${hash}`,
    undefined,
    cleanup,
  );
  const pages = (await (
    await fetch(new URL('/json/list', browser.endpoint))
  ).json()) as { type: string; webSocketDebuggerUrl: string }[];
  const page = pages.find((p) => p.type === 'page');
  assert.ok(page);
  const ws = new WebSocket(page.webSocketDebuggerUrl);
  await new Promise<void>((resolve, reject) => {
    ws.onopen = () => resolve();
    ws.onerror = () => reject(Error('DevTools接続失敗'));
  });
  cleanup(() => ws.close());
  let seq = 0;
  async function evaluate(expression: string) {
    const messageId = ++seq;
    return await new Promise<any>((resolve, reject) => {
      const listener = (event: MessageEvent) => {
        const data = JSON.parse(String(event.data));
        if (data.id !== messageId) return;
        ws.removeEventListener('message', listener);
        if (data.error || data.result?.exceptionDetails)
          reject(
            Error(JSON.stringify(data.error || data.result.exceptionDetails)),
          );
        else resolve(data.result.result.value);
      };
      ws.addEventListener('message', listener);
      ws.send(
        JSON.stringify({
          id: messageId,
          method: 'Runtime.evaluate',
          params: { expression, awaitPromise: true, returnByValue: true },
        }),
      );
    });
  }
  async function waitText(text: string) {
    const ok = await evaluate(
      `new Promise(resolve => { const end=Date.now()+30000; const check=()=>{ if(document.body.innerText.includes(${JSON.stringify(text)})) resolve(true); else if(Date.now()>end) resolve(false); else setTimeout(check,20); }; check(); })`,
    );
    assert.ok(
      ok,
      `表示されない: ${text}; ${await evaluate('document.body.innerText')}`,
    );
  }
  async function waitListResponse() {
    const ok = await evaluate(
      `new Promise(resolve => { const end=Date.now()+30000; const check=()=>{ if(performance.getEntriesByType('resource').some(e=>new URL(e.name).pathname==='/api/list' && e.responseEnd>0)) setTimeout(()=>resolve(true),0); else if(Date.now()>end) resolve(false); else setTimeout(check,20); }; check(); })`,
    );
    assert.ok(ok, '一覧の応答が完了しません');
  }
  return {
    requests,
    listUrls,
    waitRequest,
    evaluate,
    waitText,
    waitListResponse,
    releaseMeta(value: unknown = meta, status = 200) {
      assert.ok(metaResponse);
      json(metaResponse, value, status);
    },
    releaseList() {
      assert.ok(dataResponse);
      json(dataResponse, list);
    },
  };
}
test('TestBootStartsListBeforeMetaResponse', { timeout: 120000 }, async (t) => {
  const ui = await setup(t, '#/list/issues');
  await ui.waitRequest('/api/meta');
  await ui.waitRequest('/api/list');
  ui.releaseMeta();
  await ui.waitText('起動時の依頼');
  await ui.waitText('first/shared');
  assert.equal(ui.requests.filter((x) => x === '/api/list').length, 1);
});
test('TestBootReusesStoredListDelta', { timeout: 120000 }, async (t) => {
  const ui = await setup(t, '#/list/issues');
  await ui.waitRequest('/api/meta');
  await ui.waitRequest('/api/list');
  ui.releaseMeta();
  await ui.waitText('起動時の依頼');
  ui.requests.length = 0;
  ui.listUrls.length = 0;
  await ui.evaluate('location.reload(); true');
  await ui.waitRequest('/api/meta');
  await ui.waitRequest('/api/list');
  assert.deepEqual(ui.listUrls, ['/api/list?since=boot&tabs=active']);
  ui.releaseMeta();
  await ui.waitText('差分で更新');
  assert.deepEqual(ui.listUrls, ['/api/list?since=boot&tabs=active']);
});
test(
  'TestBootAdditionalDirectRoutesWithPendingMeta',
  { timeout: 300000 },
  async (t) => {
    const grant = 'a'.repeat(32);
    for (const [hash, endpoints, displayed] of [
      [`#/todos/${id}`, [`/api/todos/${id}`], '保存済みTODO'],
      [
        `#/close-requests/${id}`,
        [`/api/close-requests/${id}`],
        'クローズ依頼を開けませんでした',
      ],
      [
        '#/ssh-tickets',
        ['/api/ssh-tickets'],
        'ホスト作業用SSHの申請はありません',
      ],
      [
        `#/ssh-tickets/${id}`,
        [`/api/ssh-tickets/${id}`],
        '申請を読み込めませんでした',
      ],
      [
        '#/permission-grants',
        ['/api/permission-grants'],
        '許可の付与はありません',
      ],
      [
        `#/permission-grants/${grant}`,
        [`/api/permission-grants/${grant}`],
        '許可の付与を読み込めませんでした',
      ],
      ['#/stats', ['/api/stats/overview', '/api/stats'], '稼働実績'],
      ['#/models', ['/api/model-overrides'], 'モデル'],
      ['#/tokens', ['/api/tokens'], 'トークン'],
    ] as const)
      await t.test(hash, async (st) => {
        const ui = await setup(st, hash);
        await ui.waitRequest('/api/meta');
        for (const endpoint of endpoints) await ui.waitRequest(endpoint);
        ui.releaseMeta();
        await ui.waitText(displayed);
        for (const endpoint of endpoints)
          assert.equal(
            ui.requests.filter((x) => x === endpoint).length,
            1,
            endpoint,
          );
      });
  },
);
test('TestBootDirectRoutesWithPendingMeta', { timeout: 300000 }, async (t) => {
  for (const [hash, endpoint, text] of [
    [`#/drafts/${id}`, `/api/drafts/${id}`, '相談本文'],
    ['#/issues/101', '/api/issue', '起動時の依頼'],
    [`#/projects/${id}`, `/api/projects/${id}`, '起動時のプロジェクト'],
    ['#/projects', '/api/projects', '起動時のプロジェクト'],
  ] as const)
    await t.test(hash, async (st) => {
      const ui = await setup(st, hash);
      await ui.waitRequest('/api/meta');
      await ui.waitRequest(endpoint);
      ui.releaseMeta();
      await ui.waitText(text);
    });
  await t.test('無効', async (st) => {
    const ui = await setup(st, `#/projects/${id}`);
    await ui.waitRequest('/api/meta');
    await ui.waitRequest(`/api/projects/${id}`);
    ui.releaseMeta({ ...meta, projectCreation: null });
    await ui.waitText('新しいプロジェクトの相談を開けませんでした');
    assert.equal(
      await ui.evaluate(
        `document.body.innerText.includes('起動時のプロジェクト')`,
      ),
      false,
    );
  });
  await t.test('取得のない新規依頼', async (st) => {
    const ui = await setup(st, '#/new');
    await ui.waitRequest('/api/meta');
    assert.equal(
      await ui.evaluate(
        `!!document.querySelector('select[aria-label="プロジェクト"]')`,
      ),
      false,
    );
    ui.releaseMeta();
    await ui.waitText('依頼するプロジェクトを選択');
    assert.deepEqual(
      await ui.evaluate(
        `Array.from(document.querySelector('select[aria-label="プロジェクト"]').options, o => o.value).filter(v => v.includes('/'))`,
      ),
      ['first/shared', 'second/shared'],
    );
    assert.equal(ui.requests.includes('/api/list'), false);
  });
});
test(
  'TestBootUnauthenticatedDoesNotFetchMetaOrList',
  { timeout: 120000 },
  async (t) => {
    const ui = await setup(t, '#/', { authenticated: false });
    await ui.waitRequest('/api/session');
    await ui.waitText('接続コードを入力');
    assert.equal(
      ui.requests.filter((x) => x === '/api/meta' || x === '/api/list').length,
      0,
    );
  },
);
test('TestBootMetaFailureIsNotOverwritten', { timeout: 120000 }, async (t) => {
  const ui = await setup(t, '#/', { holdData: true });
  await ui.waitRequest('/api/meta');
  await ui.waitRequest('/api/list');
  ui.releaseMeta({ error: '失敗' }, 500);
  await ui.waitText('受付に接続できませんでした。');
  ui.releaseList();
  await ui.waitListResponse();
  await ui.waitText('受付に接続できませんでした。');
  assert.equal(
    await ui.evaluate(`document.body.innerText.includes('起動時の依頼')`),
    false,
  );
});
test('TestBootDiscardsStaleRouteResponse', { timeout: 120000 }, async (t) => {
  await t.test('metaの後に移動', async (st) => {
    const ui = await setup(st, '#/', { holdData: true });
    await ui.waitRequest('/api/meta');
    await ui.waitRequest('/api/list');
    ui.releaseMeta();
    await ui.waitText('依頼一覧');
    await ui.evaluate(`location.hash='#/drafts/${id}'`);
    await ui.waitText('相談本文');
    ui.releaseList();
    await ui.waitListResponse();
    await ui.waitText('相談本文');
    assert.equal(
      await ui.evaluate(`document.body.innerText.includes('起動時の依頼')`),
      false,
    );
  });
  await t.test('metaの前に移動', async (st) => {
    const ui = await setup(st, '#/', { holdData: true });
    await ui.waitRequest('/api/meta');
    await ui.waitRequest('/api/list');
    await ui.evaluate(`location.hash='#/drafts/${id}'`);
    ui.releaseMeta();
    await ui.waitText('相談本文');
    ui.releaseList();
    await ui.waitListResponse();
    await ui.waitText('相談本文');
    assert.equal(
      await ui.evaluate(`document.body.innerText.includes('起動時の依頼')`),
      false,
    );
  });
});
