import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { pathToFileURL } from 'node:url';
import { performance } from 'node:perf_hooks';
import { sharedClientBuild } from './shared-client-build.ts';
import { compileSource } from '../scripts/source.ts';

test(
  '一覧を開くまでの所要を最初の取得だけ受付へ知らせる',
  { timeout: 60000 },
  async (t) => {
    const { destination: directory, ...built } = sharedClientBuild();
    const model = await import(
      pathToFileURL(path.join(directory, 'v', built.ui, 'request-list.js')).href
    );
    const rows: object[] = [];
    for (let i = 1; i <= 4436; i++)
      rows.push({
        key: `issue:owner/repo#${i}`,
        type: 'issue',
        item: {
          project: 'owner/repo',
          number: i,
          title: '固定のIssue',
          status: 'open',
          updatedAt: '2026-11-01T00:00:00Z',
        },
      });
    for (let i = 0; i < 6702; i++) {
      const id = `${i.toString(16).padStart(8, '0')}-1111-4111-8111-111111111111`;
      rows.push({
        key: `draft:${id}`,
        type: 'draft',
        item: {
          id,
          project: 'owner/repo',
          title: '固定の相談',
          status: 'draft',
          messages: [{ role: 'user', text: '固定の依頼文' }],
        },
      });
    }
    const body = JSON.stringify({
      scope: 'active',
      cursor: 'l1.fixture',
      full: true,
      reset: false,
      rows,
      aux: { reviews: [], fullTests: [], stalls: [], attentionAlerts: [] },
      unavailable: ['hosts'],
    });
    const elements = new Map<string, any>();
    const element = () => ({
      hidden: false,
      textContent: '',
      className: '',
      children: [] as any[],
      setAttribute() {},
      replaceChildren(...items: any[]) {
        this.children = items;
      },
      insertBefore(item: any, before: any) {
        if (before)
          this.children.splice(this.children.indexOf(before), 0, item);
        else this.children.push(item);
      },
      removeChild(item: any) {
        this.children.splice(this.children.indexOf(item), 1);
      },
      scrollIntoView() {},
    });
    const $ = (selector: string) => {
      if (!elements.has(selector)) elements.set(selector, element());
      return elements.get(selector);
    };
    let requests = 0;
    const reports: Record<string, number>[] = [];
    let fail = false;
    let retry: (() => Promise<void>) | undefined;
    let saved: any;
    const context = vm.createContext({
      ...model,
      performance,
      URLSearchParams,
      location: { hash: '' },
      document: { hidden: true, createTextNode: (s: string) => s },
      $,
      el: element,
      console,
      setTimeout,
      clearTimeout,
      window: {},
      listCache: null,
      listScreen: null,
      routeId: 1,
      routeHash: '',
      tab: null,
      meta: {},
      hostsState: null,
      issueSort: { field: 'updated', direction: 'desc' },
      kindFilter: 'all',
      listGen: 0,
      listDrops: 0,
      listFetches: 0,
      polling: null,
      listRefreshMs: 15000,
      remember() {},
      reset: () => 1,
      head: element,
      isCurrent: () => true,
      isListTab: (s: string) =>
        [
          'attention',
          'issues',
          'waiting',
          'stopped',
          'followups',
          'todos',
          'completed',
          'closed',
        ].includes(s),
      listReserve: () => 0,
      listBegin: () => 0,
      hasBootRead: () => false,
      cancelTimer() {},
      api: async (url: string, data?: Record<string, number>) => {
        if (url === '/api/list/timing') {
          reports.push(data!);
          return { ok: true };
        }
        assert.equal(url, '/api/list?tabs=active');
        requests++;
        if (fail) throw new Error('取得失敗');
        return JSON.parse(body);
      },
      keepList(data: any) {
        saved = data;
      },
      uiError: (e: any) => e,
      hostsValid: () => false,
      hostsDraw() {},
      shownProject: (p: string) => p,
      deskLabel: () => '',
      giteaNumber: () => '',
      badge: element,
      rowTime: element,
      fullTestAlertRows: model.fullTestRows,
      attentionAlertRows: model.attentionRows,
      poll(fn: () => Promise<void>) {
        retry = fn;
      },
      pollList(fn: () => Promise<void>) {
        retry = fn;
      },
      listPollMs: () => 15000,
      error() {},
    });
    const source = ['app-list-sync.ts', 'app-list-rows.ts', 'app-home.ts']
      .map((name) =>
        fs.readFileSync(new URL('../src/ui/' + name, import.meta.url), 'utf8'),
      )
      .join('\n');
    performance.clearMeasures();
    await vm.runInContext(
      compileSource(source + '\nhome(false);', 'timing.ts'),
      context,
    );
    assert.equal(requests, 1);
    assert.ok($('#items').children.length > 0);
    assert.equal(reports.length, 1);
    assert.equal(reports[0].rows, rows.length);
    for (const name of ['fetchMs', 'integrateMs', 'drawMs', 'firstDisplayMs']) {
      assert.ok(
        Number.isInteger(reports[0][name]) && reports[0][name] >= 0,
        name,
      );
      t.diagnostic(
        `${name}: ${reports[0][name]} ms (テスト環境、描画先は代役、${rows.length} rows)`,
      );
    }
    context.listCache = { ...saved, at: 0 };
    context.listScreen = null;
    await vm.runInContext('home(false)', context);
    assert.equal(reports.length, 1, '保持済み一覧');

    context.listCache = null;
    context.listScreen = null;
    fail = true;
    await vm.runInContext('home(false)', context);
    fail = false;
    assert.ok(retry);
    await retry();
    assert.equal(reports.length, 1, '失敗後の再取得');
  },
);

// 配布する画面断片・取得周期・保持処理をそのまま動かし、時計とブラウザーの通信境界だけを代役にする。
async function listBrowser() {
  const { destination, ui } = sharedClientBuild();
  const model = await import(
    pathToFileURL(path.join(destination, 'v', ui, 'request-list.js')).href
  );
  const source = [
    'app-api.ts',
    'app-live.ts',
    'app-list-sync.ts',
    'app-list-rows.ts',
    'app-home.ts',
    'app-updates.ts',
  ]
    .map((name) =>
      fs.readFileSync(new URL('../src/ui/' + name, import.meta.url), 'utf8'),
    )
    .join('\n');
  const code = compileSource(source, 'list-browser.ts');
  let now = 1800000000000,
    nextTimer = 0,
    cursor = 0;
  const origin = now;
  type Scheduled = { at: number; tab: object; fn: () => unknown };
  const timers = new Map<number, Scheduled>();
  const channels = new Set<any>();
  const failures: unknown[] = [];
  const requests: { tab: object; at: number; url: string }[] = [];
  const rows = Array.from({ length: 60 }, (_, index) => ({
    key: `issue:owner/repo#${index + 1}`,
    type: 'issue',
    item: {
      project: 'owner/repo',
      number: index + 1,
      title: `Issue ${index + 1}`,
      status: index === 1 ? 'completed' : 'open',
      updatedAt: '2026-11-01T00:00:00Z',
    },
  }));
  let owner: object | null = null;
  const queue: (() => void)[] = [];
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
  function tab(shared = false, available = true) {
    const token = {};
    const schedule = (fn: () => unknown, ms = 0) => {
      const id = ++nextTimer;
      timers.set(id, { at: now + Math.max(0, ms), tab: token, fn });
      return id;
    };
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
      fail(state = 0) {
        this.readyState = state;
        this.onerror?.();
      }
      notice(number: number) {
        this.onmessage?.({
          data: JSON.stringify({
            type: 'issue',
            project: 'owner/repo',
            number,
          }),
        });
      }
    }
    const sources: Source[] = [];
    class Channel {
      name: string;
      onmessage?: (e: { data: unknown }) => void;
      constructor(name: string) {
        this.name = name;
        channels.add(this);
      }
      postMessage(data: unknown) {
        for (const channel of channels)
          if (channel !== this && channel.name === this.name)
            schedule(() => channel.onmessage?.({ data }));
      }
    }
    const lock = (
      _name: string,
      options: any,
      callback?: (lock: object | null) => unknown,
    ) => {
      const fn = callback ?? options;
      return new Promise<void>((resolve, reject) => {
        const take = () => {
          owner = token;
          Promise.resolve(fn({})).then(() => {
            owner = null;
            queue.shift()?.();
            resolve();
          }, reject);
        };
        if (!owner) take();
        else if (options.ifAvailable) {
          Promise.resolve(fn(null)).then(() => resolve(), reject);
        } else queue.push(take);
      });
    };
    const elements = new Map<string, any>();
    const element = () => ({
      hidden: false,
      textContent: '',
      className: '',
      children: [] as any[],
      setAttribute() {},
      replaceChildren(...items: any[]) {
        this.children = items;
      },
      insertBefore(item: any, before: any) {
        if (before)
          this.children.splice(this.children.indexOf(before), 0, item);
        else this.children.push(item);
      },
      removeChild(item: any) {
        this.children.splice(this.children.indexOf(item), 1);
      },
      scrollIntoView() {},
    });
    const $ = (selector: string) => {
      if (!elements.has(selector)) elements.set(selector, element());
      return elements.get(selector);
    };
    const listeners = new Map<string, () => void>();
    const document = {
      visibilityState: 'visible',
      documentElement: { dataset: { ui } },
      createTextNode: (s: string) => s,
      addEventListener(name: string, fn: () => void) {
        listeners.set(name, fn);
      },
    };
    const storage = new Map<string, string>();
    const context = vm.createContext({
      ...model,
      performance,
      URL,
      URLSearchParams,
      Date: Clock,
      console,
      location: { hash: '', origin: 'http://localhost' },
      document,
      $,
      el: element,
      head: element,
      hasBootRead: () => false,
      takeBootRead: () => null,
      setTimeout: schedule,
      clearTimeout: (id: number) => timers.delete(id),
      cancelTimer: (id: number) => timers.delete(id),
      window: { scrollTo() {} },
      sessionStorage: {
        setItem: (key: string, value: string) => storage.set(key, value),
        getItem: (key: string) => storage.get(key) ?? null,
        removeItem: (key: string) => storage.delete(key),
      },
      listCache: null,
      listScreen: null,
      routeId: 0,
      routeHash: '',
      tab: 'issues',
      meta: {},
      hostsState: null,
      current: null,
      activeComposer: null,
      holdReason: null,
      timer: null,
      polling: null,
      session: { csrf: '' },
      issueSort: { field: 'updated', direction: 'desc' },
      kindFilter: 'all',
      remember() {},
      providerDisableRoute() {},
      isListTab: (s: string) =>
        [
          'attention',
          'issues',
          'waiting',
          'stopped',
          'followups',
          'todos',
          'completed',
          'closed',
        ].includes(s),
      uiError: (e: any) => e,
      hostsValid: () => false,
      hostsDraw() {},
      shownProject: (p: string) => p,
      deskLabel: () => '',
      giteaNumber: () => '',
      badge: element,
      rowTime: element,
      fullTestAlertRows: model.fullTestRows,
      attentionAlertRows: model.attentionRows,
      ...(available ? { EventSource: Source } : {}),
      ...(shared
        ? { BroadcastChannel: Channel, navigator: { locks: { request: lock } } }
        : {}),
      requestJson: async (url: string) => {
        requests.push({ tab: token, at: now - origin, url });
        const parsed = new URL(url, 'http://localhost');
        let value: unknown;
        if (parsed.pathname === '/api/list/timing') value = { ok: true };
        else if (parsed.pathname === '/api/list/entry')
          value = rows.find(
            (row) =>
              row.item.number === Number(parsed.searchParams.get('number')),
          );
        else {
          assert.equal(parsed.pathname, '/api/list');
          const scope = parsed.searchParams.has('tabs') ? 'active' : 'full';
          const selected = structuredClone(
            rows.filter(
              (row) => scope === 'full' || row.item.status !== 'completed',
            ),
          );
          value = {
            scope,
            cursor: `cursor${++cursor}`,
            full: !parsed.searchParams.has('since'),
            rows: selected,
            added: [],
            updated: selected,
            removed: [],
            aux: {
              reviews: [],
              fullTests: [],
              stalls: [],
              attentionAlerts: [],
            },
          };
        }
        return { response: { ok: true, status: 200 }, value };
      },
    });
    vm.runInContext(code, context);
    const run = (s: string) => vm.runInContext(s, context);
    return {
      context,
      run,
      sources,
      async start() {
        await run('watchLive(); startLive(); home(false);');
        await advance(0);
      },
      async visible(visible: boolean) {
        document.visibilityState = visible ? 'visible' : 'hidden';
        listeners.get('visibilitychange')?.();
        await advance(0);
      },
      crash() {
        for (const [id, task] of timers)
          if (task.tab === token) timers.delete(id);
      },
      lists: () =>
        requests.filter(
          (r) =>
            r.tab === token &&
            new URL(r.url, 'http://localhost').pathname === '/api/list',
        ),
      entries: () =>
        requests.filter(
          (r) => r.tab === token && r.url.startsWith('/api/list/entry?'),
        ),
    };
  }
  return { tab, advance, rows };
}

test('SSE未対応・接続待ちは15秒、接続完了後は30秒で差分取得する', async () => {
  const browser = await listBrowser();
  const unsupported = browser.tab(false, false),
    connecting = browser.tab();
  await unsupported.start();
  await connecting.start();
  await browser.advance(15000);
  assert.deepEqual(
    unsupported.lists().map((r) => r.at),
    [0, 15000],
  );
  assert.deepEqual(
    connecting.lists().map((r) => r.at),
    [0, 15000],
  );
  connecting.sources[0].open();
  await browser.advance(15000);
  assert.equal(connecting.lists().length, 2);
  await browser.advance(15000);
  assert.deepEqual(
    connecting.lists().map((r) => r.at),
    [0, 15000, 45000],
  );
  assert.match(connecting.lists().at(-1)!.url, /since=cursor/);
});

test('SSE切断時は予約済みの取得も15秒周期へ戻し、再接続待ちも延長しない', async () => {
  const browser = await listBrowser(),
    tab = browser.tab();
  await tab.start();
  tab.sources[0].open();
  await browser.advance(5000);
  tab.sources[0].fail();
  await browser.advance(9999);
  assert.equal(tab.lists().length, 1);
  await browser.advance(1);
  assert.deepEqual(
    tab.lists().map((r) => r.at),
    [0, 15000],
  );
  tab.sources[0].fail(2);
  await browser.advance(15000);
  assert.equal(tab.sources.length, 2);
  assert.equal(tab.sources[1].readyState, 0);
  assert.deepEqual(
    tab.lists().map((r) => r.at),
    [0, 15000, 30000],
  );
});

test('他タブの接続が未確認のロック待機中は15秒、共有確認中は30秒で取得する', async () => {
  const browser = await listBrowser(),
    leader = browser.tab(true),
    follower = browser.tab(true);
  await leader.start();
  await follower.start();
  assert.equal(follower.sources.length, 0);
  await browser.advance(15000);
  assert.deepEqual(
    follower.lists().map((r) => r.at),
    [0, 15000],
  );
  leader.sources[0].open();
  await browser.advance(0);
  browser.rows[0].item.title = '共有通知で変わったIssue';
  leader.sources[0].notice(1);
  await browser.advance(15000);
  assert.equal(follower.lists().length, 2);
  assert.equal(follower.entries().length, 1);
  assert.equal(
    follower.context.listCache.issues.find((row: any) => row.number === 1)
      .title,
    '共有通知で変わったIssue',
  );
  await browser.advance(15000);
  assert.deepEqual(
    follower.lists().map((r) => r.at),
    [0, 15000, 45000],
  );
  leader.sources[0].fail();
  await browser.advance(0);
  await browser.advance(15000);
  assert.deepEqual(
    follower.lists().map((r) => r.at),
    [0, 15000, 45000, 60000],
  );
});

test('接続確認の共有が途絶えたタブは失効時に15秒周期へ戻す', async () => {
  const browser = await listBrowser(),
    leader = browser.tab(true),
    follower = browser.tab(true);
  await leader.start();
  leader.sources[0].open();
  await browser.advance(0);
  await follower.start(); // 通知の無い既存接続にも状態を問い合わせる。
  await browser.advance(5000);
  leader.crash();
  await browser.advance(14999);
  assert.equal(follower.lists().length, 1);
  await browser.advance(1);
  assert.deepEqual(
    follower.lists().map((r) => r.at),
    [0, 20000],
  );
  await browser.advance(15000);
  assert.deepEqual(
    follower.lists().map((r) => r.at),
    [0, 20000, 35000],
  );
});

test('非表示タブは共有通知で延長せず、表示復帰と接続引継ぎで全体照合する', async () => {
  const browser = await listBrowser(),
    leader = browser.tab(true),
    follower = browser.tab(true);
  await leader.start();
  leader.sources[0].open();
  await follower.start();
  await browser.advance(5000);
  await follower.visible(false);
  leader.sources[0].notice(1);
  await browser.advance(10000);
  assert.deepEqual(
    follower.lists().map((r) => r.at),
    [0],
  );
  assert.equal(follower.entries().length, 0);
  await follower.visible(true);
  await browser.advance(3000);
  assert.equal(
    new URL(follower.lists().at(-1)!.url, 'http://localhost').searchParams.has(
      'since',
    ),
    false,
  );
  await leader.visible(false);
  assert.equal(follower.sources.length, 1);
  follower.sources[0].open();
  await browser.advance(3000);
  assert.equal(
    new URL(follower.lists().at(-1)!.url, 'http://localhost').searchParams.has(
      'since',
    ),
    false,
  );
});

test('接続復帰の全体照合は30秒の定期確認を動かさない', async () => {
  const browser = await listBrowser(),
    tab = browser.tab();
  await tab.start();
  tab.sources[0].open();
  await browser.advance(1000);
  tab.sources[0].fail();
  tab.sources[0].open();
  await browser.advance(2000);
  assert.deepEqual(
    tab.lists().map((r) => r.at),
    [0, 3000],
  );
  assert.equal(
    new URL(tab.lists()[1].url, 'http://localhost').searchParams.has('since'),
    false,
  );
  await browser.advance(27000);
  assert.deepEqual(
    tab.lists().map((r) => r.at),
    [0, 3000, 30000],
  );
  assert.match(tab.lists()[2].url, /since=/);
});

test('接続中も保持した他の一覧タブを個別更新し、50件超の全体照合と最短3秒を保つ', async () => {
  const browser = await listBrowser(),
    tab = browser.tab();
  await tab.start();
  tab.sources[0].open();
  tab.run("tab = 'completed'; home(false);");
  await browser.advance(3000);
  assert.equal(tab.context.listCache.scope, 'full');
  const before = tab.lists().length;
  tab.run("tab = 'issues'; home(false);");
  assert.equal(tab.lists().length, before, '取得済みタブは保持から表示');
  browser.rows[1].item.title = '通知で変わった完了Issue';
  tab.sources[0].notice(2);
  await browser.advance(3000);
  assert.equal(tab.entries().length, 1);
  assert.equal(tab.entries()[0].at, 6000);
  assert.equal(
    tab.context.listCache.issues.find((row: any) => row.number === 2).title,
    '通知で変わった完了Issue',
  );
  for (let number = 1; number <= 50; number++) tab.sources[0].notice(number);
  await browser.advance(3000);
  assert.equal(tab.entries().length, 51, '50件までは個別取得');
  assert.ok(
    tab
      .entries()
      .slice(1)
      .every((r) => r.at === 9000),
  );
  for (let number = 1; number <= 51; number++) tab.sources[0].notice(number);
  await browser.advance(3000);
  assert.equal(tab.entries().length, 51);
  assert.equal(tab.lists().at(-1)!.at, 12000);
  assert.equal(
    new URL(tab.lists().at(-1)!.url, 'http://localhost').searchParams.has(
      'since',
    ),
    false,
  );
  await browser.advance(21000);
  assert.equal(
    tab.lists().at(-1)!.at,
    33000,
    '個別更新・全体照合で定期確認を先送りしない',
  );
});

test('保持した一覧への復帰も接続確認に応じた残り時間で取得する', async () => {
  const browser = await listBrowser(),
    tab = browser.tab();
  await tab.start();
  tab.sources[0].open();
  await browser.advance(10000);
  await tab.run('reset(); listScreen = null; home(false);');
  await browser.advance(19999);
  assert.equal(tab.lists().length, 1);
  await browser.advance(1);
  assert.deepEqual(
    tab.lists().map((r) => r.at),
    [0, 30000],
  );
  await browser.advance(20000);
  tab.sources[0].fail();
  await browser.advance(0);
  assert.deepEqual(
    tab.lists().map((r) => r.at),
    [0, 30000, 50000],
    '15秒を過ぎて切断した予約は直ちに回収',
  );
});

test('HiddenListResumeOnce', async () => {
  const b = await listBrowser(),
    t = b.tab();
  await t.start();
  t.sources[0].open();
  await t.visible(false);
  await b.advance(60000);
  assert.equal(t.lists().length, 1);
  await t.visible(true);
  await b.advance(3000);
  assert.equal(t.lists().length, 2);
  assert.equal(
    new URL(t.lists()[1].url, 'http://localhost').searchParams.has('since'),
    false,
  );
});

test('ResumeConnectedPeriod', async () => {
  const b = await listBrowser(),
    t = b.tab();
  await t.start();
  t.sources[0].open();
  await t.visible(false);
  await b.advance(60000);
  await t.visible(true);
  t.sources.at(-1)!.open();
  await b.advance(300);
  const count = t.lists().length;
  await b.advance(29999);
  assert.equal(t.lists().length, count);
  await b.advance(1);
  assert.equal(t.lists().length, count + 1);
  assert.match(t.lists().at(-1)!.url, /since=/);
});

test('ResumeWithoutSSE', async () => {
  for (const available of [false, true]) {
    const b = await listBrowser(),
      t = b.tab(false, available);
    await t.start();
    await t.visible(false);
    await b.advance(60000);
    assert.equal(t.lists().length, 1);
    await t.visible(true);
    await b.advance(3000);
    assert.equal(t.lists().length, 2);
    assert.equal(
      new URL(t.lists()[1].url, 'http://localhost').searchParams.has('since'),
      false,
    );
    await b.advance(15000);
    assert.equal(t.lists().length, 3);
    assert.match(t.lists()[2].url, /since=/);
  }
});

test('PollVisibilityPeriods', async () => {
  for (const ms of [5000, 10000, 15000]) {
    const b = await listBrowser(),
      t = b.tab();
    t.run(
      `watchLive(); globalThis.calls = 0; globalThis.tick = () => { calls++; poll(tick, ${ms}); }; poll(tick, ${ms});`,
    );
    await t.visible(false);
    await b.advance(ms * 2);
    assert.equal(t.context.calls, 0);
    await t.visible(true);
    await b.advance(0);
    assert.equal(t.context.calls, 1);
    await b.advance(ms);
    assert.equal(t.context.calls, 2);
    delete t.context.document;
    await b.advance(ms);
    assert.equal(t.context.calls, 3);
  }
});

test('ResumeRaceAndRouteReset', async () => {
  const b = await listBrowser(),
    leader = b.tab(true),
    t = b.tab(true);
  await leader.start();
  leader.sources[0].open();
  await t.start();
  await t.visible(false);
  await b.advance(60000);
  await t.visible(true);
  await leader.visible(false);
  await b.advance(3000);
  t.sources.at(-1)?.open();
  await b.advance(3000);
  assert.equal(t.lists().length, 2);
  assert.ok(t.lists()[1].at - t.lists()[0].at >= 3000);
  await t.visible(false);
  t.run('reset(); listScreen = null;');
  await t.visible(true);
  await b.advance(30000);
  assert.equal(t.lists().length, 2);
});

test('HiddenInflightAndRetry', async () => {
  const b = await listBrowser(),
    t = b.tab();
  t.run(`watchLive(); globalThis.calls = 0; globalThis.release = null;
    globalThis.tick = async () => { calls++; if (calls === 1) await new Promise(resolve => { release = resolve; });
      if (calls === 2) throw Error('retry'); poll(tick, 5000); }; poll(tick, 5000);`);
  await b.advance(5000);
  await t.visible(false);
  t.run('release()');
  await b.advance(20000);
  assert.equal(t.context.calls, 1);
  await t.visible(true);
  await b.advance(0);
  assert.equal(t.context.calls, 2);
  await t.visible(false);
  await b.advance(20000);
  assert.equal(t.context.calls, 2);
  await t.visible(true);
  await b.advance(0);
  assert.equal(t.context.calls, 3);
  await b.advance(5000);
  assert.equal(t.context.calls, 4);
});
