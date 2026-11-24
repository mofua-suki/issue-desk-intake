import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import { compileSource } from '../scripts/source.ts';

// Issueの詳細を開く処理と、定期取得・即時通知の断片をそのまま動かし、時計・通信・描画だけを代役にする。
function issueBrowser(available = true) {
  const source = (name: string) =>
    fs.readFileSync(new URL('../src/ui/' + name, import.meta.url), 'utf8');
  const code = compileSource(
    source('app-live.ts') +
      '\n' +
      source('app-issue.ts').split('// app-updates の共通更新判定')[0],
    'issue-browser.ts',
  );
  let now = 1800000000000,
    nextTimer = 0;
  const origin = now;
  const timers = new Map<number, { at: number; fn: () => unknown }>();
  const failures: unknown[] = [];
  const requests: { at: number; url: string }[] = [];
  const settle = () => new Promise<void>((resolve) => setImmediate(resolve));
  async function advance(ms: number) {
    const end = now + ms;
    for (;;) {
      const next = [...timers.entries()]
        .filter(([, task]) => task.at <= end)
        .sort((a, b) => a[1].at - b[1].at || a[0] - b[0])[0];
      if (!next) break;
      now = next[1].at;
      timers.delete(next[0]);
      try {
        Promise.resolve(next[1].fn()).catch((error) => failures.push(error));
      } catch (error) {
        failures.push(error);
      }
      await settle();
    }
    now = end;
    await settle();
    assert.deepEqual(failures, []);
  }
  class Clock extends Date {
    static now() {
      return now;
    }
  }
  class Source {
    readyState = 0;
    onopen?: () => void;
    onerror?: () => void;
    onmessage?: (e: { data: string }) => void;
    constructor(url: string) {
      assert.equal(url, '/api/events');
      sources.push(this);
    }
    close() {
      this.readyState = 2;
    }
    open() {
      this.readyState = 1;
      this.onopen?.();
    }
    fail() {
      this.readyState = 0;
      this.onerror?.();
    }
    notice(number: number) {
      this.onmessage?.({
        data: JSON.stringify({ type: 'issue', project: 'owner/repo', number }),
      });
    }
  }
  const sources: Source[] = [];
  const context: any = vm.createContext({
    Date: Clock,
    console,
    setTimeout: (fn: () => unknown, ms = 0) => {
      const id = ++nextTimer;
      timers.set(id, { at: now + Math.max(0, ms), fn });
      return id;
    },
    cancelTimer: (id: number) => timers.delete(id),
    ...(available ? { EventSource: Source } : {}),
    document: { visibilityState: 'visible', addEventListener() {} },
    location: { hash: '' },
    timer: null,
    polling: null,
    routeId: 0,
    current: null,
    listScreen: null,
    listNeedFull: false,
    listEntryTimer: null,
    listRefreshMs: 15000,
    listNote() {},
    issueFull: null,
    issueFullLoader: null,
    uiError: (e: unknown) => e,
    error: (e: unknown) => failures.push(e),
    routeError: (e: unknown) => failures.push(e),
    remember: (hash: string) => {
      context.location.hash = hash;
    },
    reset: () => ++context.routeId,
    isCurrent: (id: number) => id === context.routeId,
    $: () => ({ replaceChildren() {} }),
    el: () => ({}),
    home() {},
    renderIssue() {},
    api: async (url: string) => {
      requests.push({ at: now - origin, url });
      return { project: 'owner/repo', number: 7, desk: 12, title: '依頼' };
    },
  });
  vm.runInContext(code, context);
  return {
    sources,
    advance,
    async open() {
      await vm.runInContext(
        'watchLive(); startLive(); issueView("owner/repo", 7, false)',
        context,
      );
      await advance(0);
    },
    // 詳細の取得を始めた時刻（開いてからの ms）。すべて軽量応答（view=summary）で読む。
    reads() {
      for (const r of requests)
        assert.equal(
          r.url,
          '/api/issue?project=owner%2Frepo&number=7&view=summary',
        );
      return requests.map((r) => r.at);
    },
  };
}

test('Issueの詳細はSSE未対応・接続待ちの間は10秒ごとに読み直す', async () => {
  for (const available of [false, true]) {
    const browser = issueBrowser(available);
    await browser.open();
    await browser.advance(30000);
    assert.deepEqual(browser.reads(), [0, 10000, 20000, 30000]);
  }
});

test('Issueの詳細はSSE接続中は30秒ごとに読み直し、そのIssueの通知では前倒しする', async () => {
  const browser = issueBrowser();
  await browser.open();
  browser.sources[0].open();
  await browser.advance(29999);
  assert.deepEqual(browser.reads(), [0]);
  await browser.advance(1);
  assert.deepEqual(browser.reads(), [0, 30000]);
  // 別のIssueの通知では読み直さない。
  await browser.advance(5000);
  browser.sources[0].notice(8);
  await browser.advance(1000);
  assert.deepEqual(browser.reads(), [0, 30000]);
  browser.sources[0].notice(7);
  await browser.advance(300);
  assert.deepEqual(browser.reads(), [0, 30000, 36300]);
  // 通知で読み直した後も、次の定期取得は30秒後。
  await browser.advance(29999);
  assert.equal(browser.reads().length, 3);
  await browser.advance(1);
  assert.deepEqual(browser.reads(), [0, 30000, 36300, 66300]);
});

test('Issueの詳細はSSE切断で予約済みの取得も10秒周期へ戻し、再接続で読み直して30秒へ戻す', async () => {
  const browser = issueBrowser();
  await browser.open();
  browser.sources[0].open();
  await browser.advance(5000);
  browser.sources[0].fail();
  await browser.advance(4999);
  assert.deepEqual(browser.reads(), [0]);
  await browser.advance(1);
  assert.deepEqual(browser.reads(), [0, 10000]);
  await browser.advance(10000);
  assert.deepEqual(browser.reads(), [0, 10000, 20000]);
  // 切れていた間の通知は届いていないので、つながり直したら一度読み直す。
  await browser.advance(2000);
  browser.sources[0].open();
  await browser.advance(300);
  assert.deepEqual(browser.reads(), [0, 10000, 20000, 22300]);
  await browser.advance(30000);
  assert.deepEqual(browser.reads(), [0, 10000, 20000, 22300, 52300]);
});
