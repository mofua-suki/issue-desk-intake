import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { pathToFileURL } from 'node:url';
import { performance } from 'node:perf_hooks';
import { sharedClientBuild } from './shared-client-build.ts';
import { compileSource } from '../scripts/source.ts';

test('直リンクの先読みと画面は必要な範囲を一度だけ取得する', async (t) => {
  for (const [hash, selectedTab, expectedUrl, rowKey] of [
    ['#/list/completed', 'completed', '/api/list', 'issue:owner/repo#1'],
    ['#/list/closed', 'closed', '/api/list', 'issue:owner/repo#2'],
    [
      '#/list/todos',
      'todos',
      '/api/list',
      'todo:11111111-1111-4111-8111-111111111111',
    ],
    ['#/list/followups', 'followups', '/api/list', 'issue:owner/repo#3'],
    ['#/', null, '/api/list?tabs=active', null],
    ['#/list/attention', 'attention', '/api/list?tabs=active', null],
    ['#/list/issues', 'issues', '/api/list?tabs=active', null],
    ['#/list/waiting', 'waiting', '/api/list?tabs=active', null],
    ['#/list/stopped', 'stopped', '/api/list?tabs=active', null],
  ] as const)
    await t.test(hash, async () => {
      const { destination: directory, ...built } = sharedClientBuild();
      const model = await import(
        pathToFileURL(path.join(directory, 'v', built.ui, 'request-list.js'))
          .href
      );
      const rows = [
        {
          key: 'issue:owner/repo#1',
          type: 'issue',
          item: {
            project: 'owner/repo',
            number: 1,
            title: '完了の行',
            status: 'completed',
          },
        },
        {
          key: 'issue:owner/repo#2',
          type: 'issue',
          item: {
            project: 'owner/repo',
            number: 2,
            title: 'クローズの行',
            status: 'closed',
          },
        },
        {
          key: 'issue:owner/repo#3',
          type: 'issue',
          item: {
            project: 'owner/repo',
            number: 3,
            title: 'フォローの行',
            status: 'queued',
            followup: true,
          },
        },
        {
          key: 'todo:11111111-1111-4111-8111-111111111111',
          type: 'todo',
          item: {
            id: '11111111-1111-4111-8111-111111111111',
            project: 'owner/repo',
            body: 'TODOの行',
          },
        },
      ];
      const body = {
        scope: expectedUrl === '/api/list' ? 'full' : 'active',
        cursor: 'cursor',
        full: true,
        rows,
        aux: { reviews: [], fullTests: [], stalls: [], attentionAlerts: [] },
        unavailable: [],
      };
      const elements = new Map<string, any>();
      const element = (
        _tag?: string,
        attrs?: { text?: string },
        ...children: any[]
      ) => ({
        hidden: false,
        textContent: attrs?.text || '',
        className: '',
        children,
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
      const urls: string[] = [];
      const context = vm.createContext({
        ...model,
        performance,
        URLSearchParams,
        location: { hash },
        history: { replaceState() {}, pushState() {} },
        resumed: null,
        providerDisableRoute() {},
        session: { csrf: '' },
        document: { hidden: true, createTextNode: (s: string) => s },
        $,
        el: element,
        console,
        setTimeout,
        clearTimeout,
        window: { scrollTo() {}, addEventListener() {} },
        listCache: null,
        listScreen: null,
        routeId: 1,
        activeComposer: null,
        timer: null,
        pollResume: false,
        liveProjects: null,
        liveEarly: null,
        current: null,
        holdReason: null,
        routeHash: '',
        tab: selectedTab,
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
        listReserve: () => 0,
        listBegin: () => 0,
        cancelTimer() {},
        listNeutral: ['/api/list/timing'],
        requestJson: async (url: string) => {
          if (url !== '/api/list/timing') urls.push(url);
          return { response: { ok: true }, value: body };
        },
        keepList(data: any) {
          context.listCache = data;
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
        poll() {},
        pollList() {},
        listPollMs: () => 15000,
      });
      const source = [
        'app-list-sync.ts',
        'app-list-rows.ts',
        'app-home.ts',
        'app-routing.ts',
        'app-api.ts',
      ]
        .map((name) =>
          fs.readFileSync(
            new URL('../src/ui/' + name, import.meta.url),
            'utf8',
          ),
        )
        .join('\n');
      performance.clearMeasures();
      await vm.runInContext(
        compileSource(source + '\nstartBootReads(); home(false);', 'direct.ts'),
        context,
      );
      assert.deepEqual(urls, [expectedUrl]);
      assert.equal(context.listScreen.failed, false);
      if (rowKey) {
        const node = context.listScreen.rowNodes.get(rowKey);
        assert.ok(node, `対象行が表示されない: ${rowKey}`);
        assert.ok(context.$('#items').children.includes(node.button));
        const texts = (item: any): string =>
          typeof item === 'string'
            ? item
            : [
                item?.textContent || '',
                ...(item?.children || []).map(texts),
              ].join(' ');
        const title = rows.find((row) => row.key === rowKey)!.item;
        assert.ok(
          texts(node.button).includes(
            'title' in title ? title.title! : title.body!,
          ),
        );
      }
    });
});

test('active の保持から全体を取得して差分に移行する', async () => {
  const { destination: directory, ...built } = sharedClientBuild();
  const model = await import(
    pathToFileURL(path.join(directory, 'v', built.ui, 'request-list.js')).href
  );
  const rows: object[] = [];
  for (let i = 1; i <= 1; i++)
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
  for (let i = 0; i < 0; i++) {
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
      if (before) this.children.splice(this.children.indexOf(before), 0, item);
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
  const reports: Record<string, number>[] = [];
  let fail = false;
  const urls: string[] = [];
  let scope = 'active';
  let retry: (() => Promise<void>) | undefined;
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
    window: { scrollTo() {} },
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
      urls.push(url);
      if (fail) throw new Error('取得失敗');
      if (url === '/api/list?since=full')
        return {
          scope: 'full',
          full: false,
          cursor: 'full2',
          added: [],
          updated: [],
          removed: ['issue:owner/repo#1'],
          aux: {},
        };
      const result = JSON.parse(body);
      result.scope = scope;
      result.cursor = scope;
      return result;
    },
    keepList(data: any) {
      context.listCache = data;
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
  assert.equal(urls[0], '/api/list?tabs=active');
  assert.equal(
    context.listScreen.tabNodes.get('todos').number.textContent,
    '—',
  );
  assert.ok(retry);
  await retry();
  assert.equal(urls[1], '/api/list?since=active&tabs=active');
  fail = true;
  context.tab = 'todos';
  context.listScreen.show();
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(urls[2], '/api/list');
  assert.equal(context.listCache.issues.length, 1);
  assert.equal(
    context.listScreen.tabNodes.get('todos').number.textContent,
    '—',
  );
  fail = false;
  scope = 'full';
  await retry();
  assert.deepEqual(urls, [
    '/api/list?tabs=active',
    '/api/list?since=active&tabs=active',
    '/api/list',
    '/api/list',
  ]);
  assert.equal(context.listCache.scope, 'full');
  assert.equal(context.listCache.issues.length, 1);
  assert.equal(
    context.listScreen.tabNodes.get('todos').number.textContent,
    '0',
  );
  const count = urls.length;
  context.listScreen.show();
  assert.equal(urls.length, count);
  await retry();
  assert.equal(urls.at(-1), '/api/list?since=full');
  assert.equal(urls.length, 5);
  assert.equal(context.listCache.issues.length, 0);
});

test('全体取得の待機と定期確認が重なっても読み込み表示が終了する', async () => {
  const { destination: directory, ...built } = sharedClientBuild();
  const model = await import(
    pathToFileURL(path.join(directory, 'v', built.ui, 'request-list.js')).href
  );
  const rows: object[] = [];
  for (let i = 1; i <= 1; i++)
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
  for (let i = 0; i < 0; i++) {
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
      if (before) this.children.splice(this.children.indexOf(before), 0, item);
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
  const reports: Record<string, number>[] = [];
  const fail = false;
  const urls: string[] = [];
  let scope = 'active';
  let retry: (() => Promise<void>) | undefined;
  let delay = 0;
  let loading: any;
  const createElement = (_tag: string, attrs?: any) => {
    const node = element();
    if (attrs?.id === 'list-scope-loading') loading = node;
    return node;
  };
  const context = vm.createContext({
    ...model,
    performance,
    URLSearchParams,
    location: { hash: '' },
    document: { hidden: true, createTextNode: (s: string) => s },
    $,
    el: createElement,
    console,
    setTimeout,
    clearTimeout,
    window: { scrollTo() {} },
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
    listReserve: () => delay,
    listBegin: () => 0,
    hasBootRead: () => false,
    cancelTimer: clearTimeout,
    api: async (url: string, data?: Record<string, number>) => {
      if (url === '/api/list/timing') {
        reports.push(data!);
        return { ok: true };
      }
      urls.push(url);
      if (fail) throw new Error('取得失敗');
      if (url === '/api/list?since=full')
        return {
          scope: 'full',
          full: false,
          cursor: 'full2',
          added: [],
          updated: [],
          removed: ['issue:owner/repo#1'],
          aux: {},
        };
      const result = JSON.parse(body);
      result.scope = scope;
      result.cursor = scope;
      return result;
    },
    keepList(data: any) {
      context.listCache = data;
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
  await vm.runInContext(
    compileSource(source + '\nhome(false);', 'scope-concurrency.ts'),
    context,
  );
  delay = 20;
  scope = 'full';
  context.tab = 'todos';
  context.listScreen.show();
  assert.equal(loading.hidden, false);
  assert.ok(retry);
  await retry();
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(context.listCache.scope, 'full');
  assert.equal(loading.hidden, true);
  assert.equal(urls.filter((url) => url === '/api/list').length, 1);
});

// 全範囲と有効範囲の切り替えを確かめる画面。時刻（clock）とタブを試験から動かし、要求したURLを記録する。
// 全範囲の応答は終わったIssueとTODOを含み、有効範囲の応答は対応中のIssueだけを返す。
async function scopeScreen(options: {
  tab: string | null;
  cache?: Record<string, unknown>;
}) {
  const { destination: directory, ...built } = sharedClientBuild();
  const model = await import(
    pathToFileURL(path.join(directory, 'v', built.ui, 'request-list.js')).href
  );
  const issue = (number: number, status: string) => ({
    key: `issue:owner/repo#${number}`,
    type: 'issue',
    item: {
      project: 'owner/repo',
      number,
      title: `固定のIssue ${number}`,
      status,
      updatedAt: '2026-11-01T00:00:00Z',
    },
  });
  const todoId = '11111111-1111-4111-8111-111111111111';
  const activeRows = [issue(1, 'open')];
  const fullRows = [
    ...activeRows,
    issue(2, 'closed'),
    {
      key: `todo:${todoId}`,
      type: 'todo',
      item: { id: todoId, project: 'owner/repo', body: '固定のTODO' },
    },
  ];
  const aux = { reviews: [], fullTests: [], stalls: [], attentionAlerts: [] };
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
      if (before) this.children.splice(this.children.indexOf(before), 0, item);
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
  const state = {
    clock: 1_800_000_000_000,
    urls: [] as string[],
    // 有効範囲の全体応答を、試験が解放するまで待たせる。
    holdActive: null as Promise<void> | null,
    retry: undefined as (() => Promise<void>) | undefined,
  };
  const context = vm.createContext({
    ...model,
    performance,
    URLSearchParams,
    Date: { now: () => state.clock },
    location: { hash: '' },
    document: { hidden: true, createTextNode: (s: string) => s },
    $,
    el: element,
    console,
    setTimeout,
    clearTimeout,
    window: { scrollTo() {} },
    listCache: options.cache ?? null,
    listScreen: null,
    routeId: 1,
    routeHash: '',
    tab: options.tab,
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
    listSoon: () => 0,
    hasBootRead: () => false,
    cancelTimer() {},
    api: async (url: string) => {
      if (url === '/api/list/timing') return { ok: true };
      state.urls.push(url);
      const query = new URLSearchParams(url.split('?')[1] ?? '');
      const scope = query.get('tabs') === 'active' ? 'active' : 'full';
      if (query.has('since'))
        return {
          scope,
          full: false,
          cursor: query.get('since'),
          added: [],
          updated: [],
          removed: [],
          aux: {},
        };
      if (scope === 'active' && state.holdActive) await state.holdActive;
      return {
        scope,
        cursor: `${scope}-cursor`,
        full: true,
        reset: false,
        rows: scope === 'full' ? fullRows : activeRows,
        aux,
        unavailable: [],
      };
    },
    keepList(data: any) {
      context.listCache = data;
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
      state.retry = fn;
    },
    pollList(fn: () => Promise<void>) {
      state.retry = fn;
    },
    listPollMs: () => 15000,
    error() {},
  });
  const source = ['app-list-sync.ts', 'app-list-rows.ts', 'app-home.ts']
    .map((name) =>
      fs.readFileSync(new URL('../src/ui/' + name, import.meta.url), 'utf8'),
    )
    .join('\n');
  await vm.runInContext(
    compileSource(source + '\nhome(false);', 'scope-keep.ts'),
    context,
  );
  const settle = () => new Promise((resolve) => setImmediate(resolve));
  return {
    context,
    state,
    settle,
    // タブを押したときと同じく、選んだタブで一覧を描き直す。
    async open(name: string) {
      context.tab = name;
      context.listScreen.show();
      await settle();
    },
    // 次の定期取得。
    async poll() {
      assert.ok(state.retry);
      await state.retry();
    },
    count: (name: string) =>
      context.listScreen.tabNodes.get(name).number.textContent,
  };
}

test('全範囲を要するタブを離れて60秒たつと有効範囲の取得へ戻り、開き直すと全範囲を読む', async () => {
  const screen = await scopeScreen({ tab: 'issues' });
  const { context, state } = screen;
  await screen.open('todos');
  assert.deepEqual(state.urls, ['/api/list?tabs=active', '/api/list']);
  assert.equal(context.listCache.scope, 'full');
  assert.equal(screen.count('todos'), '1');

  // 離れて60秒以内は全範囲の保持と差分取得を続け、戻っても読み直さない。
  await screen.open('issues');
  state.clock += 59_999;
  await screen.poll();
  assert.equal(state.urls.at(-1), '/api/list?since=full-cursor');
  assert.equal(context.listCache.scope, 'full');
  assert.equal(screen.count('todos'), '1');
  assert.equal(screen.count('closed'), '1');

  // 60秒たった後の定期取得は、有効範囲の全体応答で保持を置き換える。終わった依頼とTODOは保持から外れ、件数は分からない表示になる。
  state.clock += 1;
  await screen.poll();
  assert.equal(state.urls.at(-1), '/api/list?tabs=active');
  assert.equal(context.listCache.scope, 'active');
  assert.deepEqual(
    Array.from(context.listCache.issues, (i: { number: number }) => i.number),
    [1],
  );
  assert.equal(context.listCache.todos.length, 0);
  assert.equal(screen.count('todos'), '—');
  assert.equal(screen.count('closed'), '—');
  assert.equal(screen.count('stopped'), '1');
  await screen.poll();
  assert.equal(state.urls.at(-1), '/api/list?since=active-cursor&tabs=active');

  // 戻った後に全範囲を要するタブを開けば、全範囲を読み直して件数を出す。
  const before = state.urls.length;
  await screen.open('closed');
  assert.deepEqual(state.urls.slice(before), ['/api/list']);
  assert.equal(context.listCache.scope, 'full');
  assert.equal(screen.count('closed'), '1');
  assert.equal(screen.count('todos'), '1');
  // 表示している間は、何秒たっても全範囲の差分取得を続ける。
  state.clock += 600_000;
  await screen.poll();
  assert.equal(state.urls.at(-1), '/api/list?since=full-cursor');
});

test('全範囲を要するタブへ60秒以内に戻ると、離れた時間を数え直す', async () => {
  const screen = await scopeScreen({ tab: 'issues' });
  const { context, state } = screen;
  await screen.open('completed');
  await screen.open('issues');
  state.clock += 50_000;
  await screen.open('completed');
  await screen.open('waiting');
  state.clock += 50_000;
  await screen.poll();
  assert.equal(state.urls.at(-1), '/api/list?since=full-cursor');
  assert.equal(context.listCache.scope, 'full');
  assert.equal(state.urls.filter((url) => url === '/api/list').length, 1);
  state.clock += 10_000;
  await screen.poll();
  assert.equal(state.urls.at(-1), '/api/list?tabs=active');
  assert.equal(context.listCache.scope, 'active');
});

test('再読み込みで復元した全範囲の保持は、有効範囲のタブなら最初の取得で有効範囲へ戻す', async () => {
  const cache = () => ({
    at: 0,
    scope: 'full',
    cursor: 'full-cursor',
    issues: [
      {
        project: 'owner/repo',
        number: 2,
        title: '終わったIssue',
        status: 'closed',
      },
    ],
    drafts: [],
    todos: [],
    closes: [],
    reviews: [],
    fullTests: [],
    stalls: [],
    attentionAlerts: [],
  });
  const active = await scopeScreen({ tab: 'issues', cache: cache() });
  assert.deepEqual(active.state.urls, ['/api/list?tabs=active']);
  assert.equal(active.context.listCache.scope, 'active');
  assert.equal(active.count('closed'), '—');
  // 全範囲を要するタブで読み込み直したときは、保持のカーソルで全範囲の差分取得を続ける。
  const full = await scopeScreen({ tab: 'closed', cache: cache() });
  assert.deepEqual(full.state.urls, ['/api/list?since=full-cursor']);
  assert.equal(full.context.listCache.scope, 'full');
  assert.equal(full.count('closed'), '1');
});

test('有効範囲へ戻す取得の間に全範囲を要するタブへ戻ったら、全範囲の保持を残す', async () => {
  const screen = await scopeScreen({ tab: 'issues' });
  const { context, state } = screen;
  await screen.open('todos');
  await screen.open('issues');
  state.clock += 60_000;
  let release = () => {};
  state.holdActive = new Promise<void>((resolve) => (release = resolve));
  assert.ok(state.retry);
  const shrinking = state.retry();
  await screen.settle();
  assert.equal(state.urls.at(-1), '/api/list?tabs=active');
  await screen.open('todos');
  release();
  await shrinking;
  assert.equal(context.listCache.scope, 'full');
  assert.equal(screen.count('todos'), '1');
  assert.equal(state.urls.filter((url) => url === '/api/list').length, 1);
  await screen.poll();
  assert.equal(state.urls.at(-1), '/api/list?since=full-cursor');
});
