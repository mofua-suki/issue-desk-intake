import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import { compileSource } from '../scripts/source.ts';
import { draftStatus } from '../../shared/src/desk-attention.mts';

type Fields = Record<string, unknown>;
const idle = { id: 'a', status: 'intake_waiting', revision: 1, messages: [] };

// 相談を開く処理と、定期取得・即時通知の断片をそのまま動かし、時計・通信・描画だけを代役にする。
function draftBrowser(first: Fields = idle) {
  const source = (name: string) =>
    fs.readFileSync(new URL('../src/ui/' + name, import.meta.url), 'utf8');
  const code = compileSource(
    source('app-live.ts') +
      '\n' +
      source('app-draft.ts').split('async function submittedDraft')[0],
    'draft-browser.ts',
  );
  let now = 1800000000000,
    nextTimer = 0,
    response = first;
  const origin = now;
  const timers = new Map<number, { at: number; fn: () => unknown }>();
  const failures: unknown[] = [];
  const requests: { at: number; url: string }[] = [];
  const rendered: Fields[] = [];
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
    notice(id: string) {
      this.onmessage?.({ data: JSON.stringify({ type: 'draft', id }) });
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
    EventSource: Source,
    document: { visibilityState: 'visible', addEventListener() {} },
    timer: null,
    polling: null,
    routeId: 0,
    current: null,
    listScreen: null,
    listNeedFull: false,
    listEntryTimer: null,
    listRefreshMs: 15000,
    listNote() {},
    mobileView: 'chat',
    activeComposer: null,
    draftMissing: null,
    draftStatus,
    draftHash: (id: string) => '#/drafts/' + id,
    draftPriorityHeld: () => false,
    uiError: (e: unknown) => e,
    error: (e: unknown) => failures.push(e),
    routeError: (e: unknown) => failures.push(e),
    remember() {},
    reset: () => ++context.routeId,
    isCurrent: (id: number) => id === context.routeId,
    renderDraft: (d: Fields) => rendered.push(structuredClone(d)),
    api: async (url: string) => {
      requests.push({ at: now - origin, url });
      return structuredClone(response);
    },
  });
  vm.runInContext(code, context);
  return {
    sources,
    advance,
    rendered,
    respond(next: Fields) {
      response = next;
    },
    async open() {
      await vm.runInContext(
        'watchLive(); startLive(); draftView("a", null, false)',
        context,
      );
      await advance(0);
    },
    // 相談の取得を始めた時刻（開いてからの ms）。
    reads() {
      for (const r of requests) assert.equal(r.url, '/api/drafts/a');
      return requests.map((r) => r.at);
    },
  };
}

test('相談はSSE接続待ちの間は、動いていなくても5秒ごとに読み直す', async () => {
  const browser = draftBrowser();
  await browser.open();
  await browser.advance(15000);
  assert.deepEqual(browser.reads(), [0, 5000, 10000, 15000]);
});

test('動いていない相談はSSE接続中は30秒ごとに読み直し、その相談の通知では前倒しする', async () => {
  const browser = draftBrowser();
  await browser.open();
  browser.sources[0].open();
  await browser.advance(29999);
  assert.deepEqual(browser.reads(), [0]);
  await browser.advance(1);
  assert.deepEqual(browser.reads(), [0, 30000]);
  // 別の相談の通知では読み直さない。
  await browser.advance(5000);
  browser.sources[0].notice('b');
  await browser.advance(1000);
  assert.deepEqual(browser.reads(), [0, 30000]);
  // 受付AIの返答が保存された通知で読み直し、会話を描き直す。
  browser.respond({ ...idle, revision: 2, messages: [{ text: '返答' }] });
  browser.sources[0].notice('a');
  await browser.advance(300);
  assert.deepEqual(browser.reads(), [0, 30000, 36300]);
  assert.equal(browser.rendered.at(-1)?.revision, 2);
  // 通知で読み直した後も、次の定期取得は30秒後。
  await browser.advance(29999);
  assert.equal(browser.reads().length, 3);
  await browser.advance(1);
  assert.deepEqual(browser.reads(), [0, 30000, 36300, 66300]);
});

test('受付の処理が動いている相談はSSE接続中も5秒ごとに読み直す', async () => {
  const moving: Fields[] = [
    { status: 'thinking', executionActive: true },
    { status: 'thinking', executionScheduled: true },
    { status: 'thinking', queued: true },
    { status: 'thinking', retryAt: '2027-01-15T00:00:00.000Z' },
    { finishingTurn: true },
    { status: 'auto_ready' },
    { status: 'submitting' },
    {
      status: 'auto_ready',
      filingQueue: { stuck: false, inactiveMinutes: 0, ahead: 2 },
    },
    {
      status: 'submitted',
      splitChildren: ['b'],
      familyProgress: {
        total: 2,
        completed: 1,
        closed: 0,
        remaining: 1,
        stopped: [],
      },
    },
    {
      status: 'submitted',
      splitChildren: ['b'],
      familyProgress: {
        total: 2,
        completed: 1,
        closed: 0,
        remaining: 0,
        unknown: 1,
        stopped: [],
      },
    },
  ];
  for (const fields of moving) {
    const browser = draftBrowser({ ...idle, ...fields });
    await browser.open();
    browser.sources[0].open();
    await browser.advance(15000);
    assert.deepEqual(
      browser.reads(),
      [0, 5000, 10000, 15000],
      JSON.stringify(fields),
    );
  }
});

test('子がすべて終わった分割の親はSSE接続中は30秒ごとに読み直す', async () => {
  const browser = draftBrowser({
    ...idle,
    status: 'submitted',
    splitChildren: ['b'],
    familyProgress: {
      total: 2,
      completed: 2,
      closed: 0,
      remaining: 0,
      stopped: [],
    },
  });
  await browser.open();
  browser.sources[0].open();
  await browser.advance(30000);
  assert.deepEqual(browser.reads(), [0, 30000]);
});

test('相談の周期は描き直さない回も最後の応答で決め、処理が止まれば30秒、動き出せば5秒にする', async () => {
  const browser = draftBrowser({
    ...idle,
    status: 'thinking',
    executionActive: true,
  });
  await browser.open();
  browser.sources[0].open();
  await browser.advance(5000);
  assert.deepEqual(browser.reads(), [0, 5000]);
  // 表示に使う欄は同じまま、受付の実行だけが終わった（描き直しは起きない）。
  browser.respond({ ...idle, status: 'thinking', executionActive: false });
  await browser.advance(5000);
  assert.deepEqual(browser.reads(), [0, 5000, 10000]);
  assert.equal(browser.rendered.length, 1);
  await browser.advance(29999);
  assert.equal(browser.reads().length, 3);
  browser.respond({ ...idle, status: 'thinking', executionActive: true });
  await browser.advance(1);
  assert.deepEqual(browser.reads(), [0, 5000, 10000, 40000]);
  await browser.advance(5000);
  assert.deepEqual(browser.reads(), [0, 5000, 10000, 40000, 45000]);
});

test('動いていない相談はSSE切断で予約済みの取得も5秒周期へ戻し、再接続で読み直して30秒へ戻す', async () => {
  const browser = draftBrowser();
  await browser.open();
  browser.sources[0].open();
  await browser.advance(2000);
  browser.sources[0].fail();
  await browser.advance(2999);
  assert.deepEqual(browser.reads(), [0]);
  await browser.advance(1);
  assert.deepEqual(browser.reads(), [0, 5000]);
  await browser.advance(5000);
  assert.deepEqual(browser.reads(), [0, 5000, 10000]);
  // 切れていた間の通知は届いていないので、つながり直したら一度読み直す。
  await browser.advance(2000);
  browser.sources[0].open();
  await browser.advance(300);
  assert.deepEqual(browser.reads(), [0, 5000, 10000, 12300]);
  await browser.advance(30000);
  assert.deepEqual(browser.reads(), [0, 5000, 10000, 12300, 42300]);
});
