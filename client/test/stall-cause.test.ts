import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import http from 'node:http';
import { sharedClientBuild } from './shared-client-build.ts';
import { cleanupStack, startChromium } from './chromium.ts';

test('配布画面は停止原因と理由を表示し原因のない一覧を断定しない', async (t) => {
  const built = sharedClientBuild();
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'stall-cause-'));
  const cleanup = cleanupStack(t);
  cleanup(() => fs.rmSync(dir, { recursive: true, force: true }));
  const ids = Array.from(
    { length: 6 },
    (_, i) => `11111111-1111-4111-8111-${String(i + 1).padStart(12, '0')}`,
  );
  const base = {
    project: 'owner/repo',
    status: 'stalled',
    body: '相談本文',
    title: '停止した相談',
    messages: [],
    actions: ['retry'],
    error: '停止理由の記録',
  };
  // 旧形式の判定はサーバーが行い、詳細APIは新形式と同じ explicit を返す。
  const drafts = [
    {
      ...base,
      id: ids[0],
      stallCause: 'explicit',
      error: '相談を停止しました: 新形式の理由',
      stalled: {
        since: '2026-11-01T00:00:00Z',
        stoppedAt: '2026-11-02T00:00:00Z',
      },
    },
    {
      ...base,
      id: ids[1],
      stallCause: 'explicit',
      error: '相談を停止しました: 旧形式の理由',
    },
    { ...base, id: ids[2], stallCause: 'timeout' },
    { ...base, id: ids[3] },
    { ...base, id: ids[4], stallCause: 'future' },
    { ...base, id: ids[5], status: 'draft', stallCause: 'explicit', error: '' },
  ];
  let removeCause = false;
  // 受付は相談の保存を通知する。通知が無いと、受付の処理が動いていない相談の画面は30秒ごとにしか読み直さない。
  const liveResponses = new Set<http.ServerResponse>();
  let resolveResult!: (value: { success: boolean; message?: string }) => void;
  const result = new Promise<{ success: boolean; message?: string }>(
    (resolve) => {
      resolveResult = resolve;
    },
  );
  const browserScript = `
    const ids = ${JSON.stringify(ids)};
    const pause = () => new Promise(r => setTimeout(r, 20));
    const wait = async f => { const end = Date.now() + 30000; while (!f()) { if (Date.now() > end) throw Error('表示待ち: ' + location.hash + ' ' + document.body.innerText); await pause(); } };
    const check = (ok, message) => { if (!ok) throw Error(message); };
    const BrowserEventSource = window.EventSource;
    let source;
    window.EventSource = class extends BrowserEventSource {
      constructor(...args) {
        super(...args);
        source = this;
      }
    };
    const notify = async path => {
      // 接続前の通知を落とすと、表示の変化を30秒の定期取得と競う。
      await wait(() => source?.readyState === BrowserEventSource.OPEN);
      const response = await fetch(path);
      check(response.ok, 'テスト用の通知を送れませんでした: ' + path);
    };
    try {
      await import('/v/${built.ui}/app.js');
      await wait(() => document.body.innerText.includes('一覧用の停止相談'));
      const list = document.querySelector('#app').innerText;
      check(list.includes('停止中') && !list.includes('24時間応答なしで停止') && !list.includes('24時間応答を返せず'), '一覧が原因を断定: ' + list);
      const expected = ['停止中（明示の停止）', '停止中（明示の停止）', '24時間応答なしで停止', '停止中（原因不明）', '停止中（原因不明）'];
      for (let i = 0; i < ids.length; i++) {
        location.hash = '#/drafts/' + ids[i];
        await wait(() => document.querySelector('#app').innerText.includes(ids[i]) && document.querySelector('.conversation') && document.querySelector('.panel-head .badge') && (i === 5 ? document.querySelector('.panel-head .badge').textContent === '起票前の相談' : document.querySelector('.panel-head .badge').textContent === expected[i]));
        if (i === 0) {
          await notify('/__remove-cause');
          await wait(() => document.querySelector('.panel-head .badge')?.textContent === '停止中（原因不明）');
          check(document.querySelector('#app').innerText.includes('停止原因を特定できませんでした'), '更新後の案内が違う');
          await notify('/__restore-cause');
          await wait(() => document.querySelector('.panel-head .badge')?.textContent === expected[0]);
        }
        const text = document.querySelector('#app').innerText;
        if (i < 5) {
          check(text.includes(i < 2 ? '明示の停止により' : i === 2 ? '24時間応答を返せず' : '停止原因を特定できませんでした'), '案内が違う: ' + text);
          check(text.includes(i === 0 ? '新形式の理由' : i === 1 ? '旧形式の理由' : '停止理由の記録'), '理由が消えた: ' + text);
          if (i !== 2) check(!text.includes('24時間応答なしで停止') && !text.includes('24時間応答を返せず') && !text.includes('期限に達しました'), '時間切れの誤表示: ' + text);
        } else {
          check(!text.includes('明示の停止') && !text.includes('停止原因を特定') && !text.includes('未送信'), '起票前の相談の表示が違う: ' + text);
          check(text.includes('相談を開いて次の進行条件を確認'), '起票前の案内が違う: ' + text);
        }
      }
      await fetch('/__result', { method: 'POST', body: JSON.stringify({ success: true }) });
    } catch (e) { await fetch('/__result', { method: 'POST', body: JSON.stringify({ success: false, message: String(e.stack || e) }) }); }
  `;
  const server = http.createServer(async (req, res) => {
    const url = new URL(req.url!, 'http://localhost');
    if (
      url.pathname === '/__remove-cause' ||
      url.pathname === '/__restore-cause'
    ) {
      if (liveResponses.size === 0) {
        res.writeHead(503).end('{}');
        return;
      }
      removeCause = url.pathname === '/__remove-cause';
      for (const stream of liveResponses)
        stream.write(
          'data: ' + JSON.stringify({ type: 'draft', id: ids[0] }) + '\n\n',
        );
      res.end('{}');
      return;
    }
    if (url.pathname === '/__result') {
      let body = '';
      for await (const part of req) body += part;
      resolveResult(JSON.parse(body));
      res.end('{}');
      return;
    }
    if (url.pathname === '/stall-test.js') {
      res.setHeader('Content-Type', 'text/javascript');
      res.end(browserScript);
      return;
    }
    if (url.pathname.startsWith('/api/')) {
      res.setHeader('Content-Type', 'application/json');
      if (url.pathname === '/api/events') {
        res.writeHead(200, { 'Content-Type': 'text/event-stream' });
        res.write(': connected\n\n');
        liveResponses.add(res);
        res.on('close', () => liveResponses.delete(res));
        return;
      }
      let value: unknown = {};
      if (url.pathname === '/api/session')
        value = { authenticated: true, csrf: 'fixture', local: false };
      else if (url.pathname === '/api/meta')
        value = {
          providers: [],
          projects: [{ id: 'owner/repo', name: 'repo' }],
        };
      else if (url.pathname === '/api/health')
        value = { ready: true, ui: built.ui };
      else if (url.pathname === '/api/list')
        value = {
          full: true,
          cursor: 'fixture',
          rows: [
            {
              key: 'draft:' + ids[0],
              type: 'draft',
              item: {
                id: ids[0],
                project: 'owner/repo',
                status: 'stalled',
                title: '一覧用の停止相談',
              },
            },
          ],
          aux: { reviews: [], fullTests: [], stalls: [], attentionAlerts: [] },
          unavailable: [],
          hosts: { hosts: [] },
        };
      else if (url.pathname.startsWith('/api/drafts/')) {
        const found = drafts.find(
          (d) => url.pathname === '/api/drafts/' + d.id,
        );
        value =
          found && removeCause && found.id === ids[0]
            ? { ...found, stallCause: undefined }
            : (found ?? {});
      } else if (url.pathname === '/api/hosts') value = { hosts: [] };
      else if (url.pathname === '/api/provider-disables')
        value = { disables: [] };
      res.end(JSON.stringify(value));
      return;
    }
    const file = url.pathname === '/' ? 'index.html' : url.pathname.slice(1);
    const resolved = path.resolve(built.destination, file);
    if (
      !resolved.startsWith(built.destination + path.sep) ||
      !fs.existsSync(resolved)
    ) {
      res.writeHead(404);
      res.end();
      return;
    }
    res.setHeader(
      'Content-Type',
      file.endsWith('.js')
        ? 'text/javascript'
        : file.endsWith('.css')
          ? 'text/css'
          : 'text/html',
    );
    const content = fs.readFileSync(resolved);
    res.end(
      file === 'index.html'
        ? content
            .toString()
            .replace(
              /<script type="module"[^>]*><\/script>/,
              '<script type="module" src="/stall-test.js"></script>',
            )
        : content,
    );
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  cleanup(async () => {
    server.closeAllConnections();
    await new Promise<void>((resolve, reject) =>
      server.close((error) => (error ? reject(error) : resolve())),
    );
  });
  const address = server.address();
  assert.ok(address && typeof address === 'object');
  const browser = await startChromium(
    t,
    process.env.ISSUE_DESK_TEST_CHROMIUM,
    dir,
    `http://127.0.0.1:${address.port}/`,
    undefined,
    cleanup,
  );
  let resultTimer: ReturnType<typeof setTimeout> | undefined;
  const outcome = await Promise.race([
    result,
    new Promise<never>((_resolve, reject) => {
      resultTimer = setTimeout(
        () =>
          reject(
            Error(`${t.name}: ブラウザの結果POST待機が期限切れになりました`),
          ),
        180000,
      );
    }),
    browser.exited.then((error) => {
      throw error;
    }),
  ]).finally(() => clearTimeout(resultTimer));
  assert.equal(outcome.success, true, outcome.message);
});
