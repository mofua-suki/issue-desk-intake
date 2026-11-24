import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path, { join } from 'node:path';
import vm from 'node:vm';
import { pathToFileURL } from 'node:url';
import { sharedClientBuild } from './shared-client-build.ts';
import { compileSource } from '../scripts/source.ts';

// 候補のリポジトリを読めなかった応答（GET /api/list の unreadProjects）を一覧の保持へ反映する処理を、
// 画面と同じソースで動かす。
function syncContext() {
  const notices: string[] = [];
  const context = vm.createContext({
    listCache: null,
    cancelTimer: () => {},
    isCurrent: () => true,
    error: (e: Error) => notices.push(e.message),
    Date,
    URLSearchParams,
    uiError: (e: Error) => e,
  });
  const source = fs.readFileSync(
    new URL('../src/ui/app-list-sync.ts', import.meta.url),
    'utf8',
  );
  vm.runInContext(compileSource(source, 'app-list-sync.ts'), context);
  const run = (code: string, values: Record<string, unknown> = {}) => {
    Object.assign(context, values);
    return vm.runInContext(code, context);
  };
  // 全体・差分の応答を置き、置いた一覧を返す（画面の refresh と同じ順: listIntegrate → listCommit → 保持）。
  let seq = 0;
  const apply = (response: Record<string, unknown>) => {
    seq++;
    const data = run(
      'listIntegrate(response, seq, listCache?.cursor ?? null)',
      { response, seq },
    );
    assert.ok(data, '応答を置けませんでした');
    run('listCommit(seq, response); listCache = data;', { data });
    run('listFetches = seq', { seq });
    return data;
  };
  return { run, apply, notices };
}
const issue = (project: string, number: number, title: string) => ({
  type: 'issue',
  key: `issue:${project}#${number}`,
  item: {
    kind: 'issue',
    project,
    number,
    title,
    status: 'queued',
    updatedAt: '2026-11-01T00:00:00Z',
  },
});

test('個別追加と削除を次の空差分で基準に戻す', async () => {
  const { run, apply } = syncContext();
  apply({
    full: true,
    scope: 'active',
    cursor: 'c1',
    rows: [issue('o/r', 1, '基準')],
  });
  run(
    `listFetches = 1; listDrops = 0; routeId = 1; listScreen = {id: 1, apply() {}};
    listStartWait = () => null; listFlushSchedule = () => {}; keepList = (data) => { listCache = data; }`,
    {
      listFetches: 1,
      listDrops: 0,
      routeId: 1,
      listScreen: null,
      keepList: () => {},
    },
  );
  const target = { type: 'issue', project: 'o/r', number: 2 };
  run('api = async () => value; listNote(target)', {
    api: async () => {},
    target,
    value: issue('o/r', 2, '先行'),
  });
  await run('listFlush(1)');
  assert.equal(run('listCache.issues.length'), 2);
  let data = apply({
    full: false,
    scope: 'active',
    cursor: 'c2',
    added: [],
    updated: [],
    removed: [],
  });
  assert.deepEqual(
    Array.from(data.issues, (r: any) => r.number),
    [1],
  );
  run(
    'api = async () => { throw Object.assign(new Error(), {status: 404}) }; listNote(target)',
    {
      target: { type: 'issue', project: 'o/r', number: 1 },
      uiError: (e: Error) => e,
    },
  );
  await run('listFlush(1)');
  assert.equal(run('listCache.issues.length'), 0);
  data = apply({
    full: false,
    scope: 'active',
    cursor: 'c3',
    added: [],
    updated: [],
    removed: [],
  });
  assert.equal(data.issues[0].title, '基準');
});

test('差分中の新しい個別取得と読取失敗を保持し、無効な差分は基準にしない', async () => {
  const { run, apply } = syncContext();
  apply({
    full: true,
    scope: 'active',
    cursor: 'c1',
    rows: [issue('owner/gone', 2, '前回')],
  });
  run(
    `listFetches = 2; listDrops = 0; routeId = 1; listScreen = {id: 1, apply() {}};
    listStartWait = () => null; listFlushSchedule = () => {}; keepList = (data) => { listCache = data; }`,
    {
      listFetches: 2,
      listDrops: 0,
      routeId: 1,
      listScreen: null,
      keepList: () => {},
      api: async () => issue('o/r', 1, '新しい'),
    },
  );
  run('listNote(target)', {
    target: { type: 'issue', project: 'o/r', number: 1 },
  });
  await run('listFlush(1)');
  const response = {
    full: false,
    scope: 'active',
    cursor: 'c2',
    added: [issue('o/r', 1, '古い')],
    updated: [],
    removed: [],
    unreadProjects: unread,
  };
  const data = run('listIntegrate(response, 2, "c1")', { response });
  assert.equal(
    data.issues.find((r: any) => r.project === 'o/r').title,
    '新しい',
  );
  assert.equal(
    data.issues.find((r: any) => r.project === 'owner/gone').readFailed,
    true,
  );
  run('listCommit(2, response); listCache = data', { data });
  assert.equal(run('listIntegrate(response, 4, "wrong")', { response }), null);
  assert.equal(
    run('listIntegrate(response, 4, "c2")', {
      response: { ...response, scope: 'full' },
    }),
    null,
  );
  const next = run('listIntegrate(response, 5, "c2")', {
    response: {
      full: false,
      scope: 'active',
      cursor: 'c3',
      added: [],
      updated: [],
      removed: [],
      unreadProjects: unread,
    },
  });
  assert.equal(next.issues.find((r: any) => r.project === 'o/r').title, '古い');
});

const unread = [
  { project: 'owner/gone', reason: 'GiteaがHTTP 404を返しました。' },
];

test('読めなかったリポジトリの行は全体・差分のどちらでも消さず読取失敗の目印を付けて残す', () => {
  const { run, apply, notices } = syncContext();
  apply({
    full: true,
    cursor: 'c1',
    rows: [
      issue('owner/repo', 1, '読める'),
      issue('owner/gone', 2, '前回の内容'),
    ],
  });
  // 差分: サーバーが行を返さなくても削除しない。
  let data = apply({
    full: false,
    cursor: 'c2',
    added: [],
    updated: [],
    removed: ['issue:owner/gone#2'],
    unreadProjects: unread,
  });
  let gone = data.issues.find((r: any) => r.project === 'owner/gone');
  assert.equal(gone?.title, '前回の内容');
  assert.equal(gone?.readFailed, true);
  assert.equal(
    data.issues.find((r: any) => r.project === 'owner/repo')?.readFailed,
    undefined,
  );
  // 全体: 応答に無い読めなかったリポジトリの行は保持から残す。
  data = apply({
    full: true,
    cursor: 'c3',
    rows: [issue('owner/repo', 1, '読める')],
    unreadProjects: unread,
  });
  gone = data.issues.find((r: any) => r.project === 'owner/gone');
  assert.equal(gone?.readFailed, true);
  assert.deepEqual(data.unreadProjects, unread);
  assert.equal(run('listNeedFull'), false);
  // 表示中の画面へ理由を知らせる。
  run('listUnavailable(response, 1)', { response: { unreadProjects: unread } });
  assert.match(
    notices.at(-1) ?? '',
    /owner\/gone（GiteaがHTTP 404を返しました。）/,
  );
  assert.match(notices.at(-1) ?? '', /そろっていません/);
});

test('読めるようになったら目印を外し、読めない間に消えた行を全体照合で除く', () => {
  const { run, apply } = syncContext();
  apply({
    full: true,
    cursor: 'c1',
    rows: [issue('owner/repo', 1, '読める')],
    unreadProjects: [],
  });
  // 差分で届いた読めないリポジトリの行にも目印を付ける。
  let data = apply({
    full: false,
    cursor: 'c2',
    added: [issue('owner/gone', 2, '前回の内容')],
    updated: [],
    removed: [],
    unreadProjects: unread,
  });
  assert.equal(
    data.issues.find((r: any) => r.project === 'owner/gone')?.readFailed,
    true,
  );
  // 読めるようになった差分: 目印を外し、全体照合を求める。
  data = apply({
    full: false,
    cursor: 'c3',
    added: [],
    updated: [],
    removed: [],
  });
  assert.equal(
    data.issues.find((r: any) => r.project === 'owner/gone')?.readFailed,
    undefined,
  );
  assert.equal(run('listNeedFull'), true);
  run('listTake()');
  // 全体照合で応答に無い行は消える。
  data = apply({
    full: true,
    cursor: 'c4',
    rows: [issue('owner/repo', 1, '読める')],
  });
  assert.deepEqual(
    Array.from(data.issues, (r: any) => r.project),
    ['owner/repo'],
  );
  assert.equal(run('listNeedFull'), false);
});

test('読取失敗の行は目印を表示し、目印の有無で描き直す', async () => {
  const { destination, ui } = sharedClientBuild();
  const model = await import(
    pathToFileURL(path.join(destination, 'v', ui, 'request-list.js')).href
  );
  const context = vm.createContext({
    ...model,
    shortTime: () => '短い日時',
    repositoryDisplay: (p: string) => p,
    meta: { projects: [] },
    el: (tag: string, attrs: object, ...children: unknown[]) => ({
      tag,
      attrs,
      children,
    }),
  });
  const source = ['app-parts.ts', 'app-list-rows.ts']
    .map((name) =>
      fs.readFileSync(new URL('../src/ui/' + name, import.meta.url), 'utf8'),
    )
    .join('\n');
  vm.runInContext(compileSource(source, 'rows.ts'), context);
  const row = {
    kind: 'issue',
    project: 'owner/gone',
    number: 2,
    status: 'queued',
    updatedAt: '2026-11-01T00:00:00Z',
  };
  context.row = row;
  const plain = vm.runInContext(
    'listRowSignature(row, "issues", false)',
    context,
  );
  context.row = { ...row, readFailed: true };
  assert.notEqual(
    vm.runInContext('listRowSignature(row, "issues", false)', context),
    plain,
  );
  const text = JSON.stringify(
    vm.runInContext('listRowChildren(row, "issues", false)', context),
  );
  assert.match(text, /読取失敗（前回の内容）/);
});

// 読めなかった候補のIssueは一覧に無くても起票待ちとして補わない（開き直し直後・全範囲で前回の行が無いとき）。
test('読めなかった候補のIssueは起票待ちの行として補わない', async () => {
  const { destination, ui } = sharedClientBuild();
  const { requestGroupsWithAux } = await import(
    pathToFileURL(join(destination, 'v', ui, 'request-list.js')).href
  );
  const data = () => ({
    issues: [],
    todos: [],
    closes: [],
    reviews: [],
    fullTests: [],
    stalls: [],
    attentionAlerts: [],
    drafts: [
      {
        id: 'a',
        project: 'o/ok',
        status: 'submitted',
        issues: [{ project: 'o/gone', number: 5 }, { number: 7 }],
      },
      {
        id: 'b',
        project: 'o/ok',
        status: 'investigation_waiting',
        investigations: [{ project: 'o/gone', number: 9 }],
      },
    ],
  });
  const pending = (groups: any) =>
    Object.values(groups)
      .flat()
      .filter((r: any) => r.kind === 'issue' && r.status === 'pending')
      .map((r: any) => `${r.project}#${r.number}`)
      .sort();
  assert.deepEqual(pending(requestGroupsWithAux(data())), [
    'o/gone#5',
    'o/gone#9',
    'o/ok#7',
  ]);
  assert.deepEqual(
    pending(
      requestGroupsWithAux({
        ...data(),
        unreadProjects: [{ project: 'o/gone', reason: '読めません' }],
      }),
    ),
    ['o/ok#7'],
  );
});

// 画面の home、通知、保存・復元を配信元と同じ断片で動かす。通信の完了順はテストが制御する。
async function listPage(storage = new Map<string, string>()) {
  const { destination, ui } = sharedClientBuild();
  const model = await import(
    pathToFileURL(path.join(destination, 'v', ui, 'request-list.js')).href
  );
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
  const requests: {
    url: string;
    resolve: (value: any) => void;
    reject: (error: Error) => void;
  }[] = [];
  const errors: string[] = [];
  let periodic: (() => Promise<void>) | undefined;
  const context = vm.createContext({
    ...model,
    performance,
    URLSearchParams,
    Date,
    location: { hash: '' },
    document: {
      hidden: true,
      documentElement: { dataset: { ui } },
      createTextNode: (s: string) => s,
    },
    sessionStorage: {
      setItem: (key: string, value: string) => storage.set(key, value),
      getItem: (key: string) => storage.get(key) ?? null,
      removeItem: (key: string) => storage.delete(key),
    },
    $: (selector: string) => {
      if (!elements.has(selector)) elements.set(selector, element());
      return elements.get(selector);
    },
    el: element,
    head: element,
    window: { scrollTo() {} },
    setTimeout: () => 1,
    clearTimeout() {},
    cancelTimer() {},
    listCache: null,
    listScreen: null,
    routeId: 1,
    routeHash: '',
    tab: 'issues',
    meta: {},
    hostsState: null,
    polling: null,
    issueSort: { field: 'updated', direction: 'desc' },
    kindFilter: 'all',
    remember() {},
    reset: () => 1,
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
    listPollMs: () => 15000,
    hasBootRead: () => false,
    session: { csrf: '' },
    takeBootRead: () => null,
    requestJson: (url: string) => {
      if (url === '/api/list/timing')
        return Promise.resolve({
          response: { ok: true, status: 200 },
          value: {},
        });
      return new Promise<any>((resolve, reject) =>
        requests.push({ url, resolve, reject }),
      ).then((value) =>
        value?.testUnauthorized
          ? {
              response: { ok: false, status: 401 },
              value: { error: '認証切れ' },
            }
          : { response: { ok: true, status: 200 }, value },
      );
    },
    uiError: (e: Error) => e,
    error: (e: Error) => errors.push(e.message),
    hostsValid: () => false,
    hostsDraw() {},
    shownProject: (p: string) => p,
    deskLabel: () => '',
    giteaNumber: () => '',
    badge: element,
    rowTime: element,
    fullTestAlertRows: model.fullTestRows,
    attentionAlertRows: model.attentionRows,
    poll: (fn: () => Promise<void>) => {
      periodic = fn;
    },
    pollList: (fn: () => Promise<void>) => {
      periodic = fn;
    },
  });
  const source = [
    'app-list-sync.ts',
    'app-list-rows.ts',
    'app-home.ts',
    'app-updates.ts',
    'app-api.ts',
  ]
    .map((name) =>
      fs.readFileSync(new URL('../src/ui/' + name, import.meta.url), 'utf8'),
    )
    .join('\n');
  vm.runInContext(compileSource(source, 'page.ts'), context);
  Object.assign(context, {
    isCurrent: () => true,
    reset: () => 1,
    error: (e: Error) => errors.push(e.message),
  });
  const run = (code: string, values: Record<string, unknown> = {}) => {
    Object.assign(context, values);
    return vm.runInContext(code, context);
  };
  return {
    run,
    context,
    requests,
    errors,
    storage,
    start: () => run('listCache = readList(); home(false)'),
    periodic: () => {
      assert.ok(periodic);
      return periodic();
    },
    notice: async (project: string, number: number) => {
      run('listNote(target)', { target: { type: 'issue', project, number } });
      return run('listFlush(routeId)');
    },
  };
}
const whole = (cursor: string, rows: unknown[] = [], scope = 'active') => ({
  full: true,
  scope,
  cursor,
  rows,
});
const delta = (cursor: string, fields: Record<string, unknown> = {}) => ({
  full: false,
  scope: 'active',
  cursor,
  added: [],
  updated: [],
  removed: [],
  ...fields,
});

test('通知後の定期差分と同版再読込で基準と先行表示を混ぜない', async () => {
  const page = await listPage();
  const start = page.start();
  page.requests.shift()!.resolve(whole('c1', [issue('o/r', 1, '基準')]));
  await start;
  const notification = page.notice('o/r', 2);
  const entry = page.requests.shift()!;
  assert.match(entry.url, /^\/api\/list\/entry\?/);
  entry.resolve(issue('o/r', 2, '先行'));
  await notification;
  assert.equal(page.context.listCache.issues.length, 2);
  const reloaded = await listPage(page.storage);
  const reloadStart = reloaded.start();
  assert.equal(reloaded.context.listCache.issues.length, 2);
  // 復元表示は残るが、基準のないカーソルを用いた差分は送らない。
  assert.equal(reloaded.context.listCache.cursor, null);
  await reloadStart;
  const reloadRefresh = reloaded.periodic();
  const reloadRequest = reloaded.requests.shift()!;
  assert.equal(reloadRequest.url, '/api/list?tabs=active');
  reloadRequest.resolve(whole('reload', [issue('o/r', 1, '基準')]));
  await reloadRefresh;
  const periodic = page.periodic();
  const request = page.requests.shift()!;
  assert.equal(request.url, '/api/list?since=c1&tabs=active');
  request.resolve(delta('c2'));
  await periodic;
  assert.deepEqual(
    Array.from(page.context.listCache.issues, (r: any) => r.number),
    [1],
  );
});

test('読取失敗中は個別削除した行を戻さず、回復後の差分で基準に戻す', async () => {
  const page = await listPage();
  const start = page.start();
  page.requests.shift()!.resolve(whole('c1', [issue('owner/gone', 2, '基準')]));
  await start;
  const entry = page.notice('owner/gone', 2);
  page.requests
    .shift()!
    .reject(Object.assign(new Error('削除済み'), { status: 404 }));
  await entry;
  assert.equal(page.context.listCache.issues.length, 0);
  for (const cursor of ['c2', 'c3']) {
    const refresh = page.periodic();
    page.requests.shift()!.resolve(delta(cursor, { unreadProjects: unread }));
    await refresh;
    assert.equal(page.context.listCache.issues.length, 0);
  }
  const recovered = page.periodic();
  page.requests.shift()!.resolve(delta('c4'));
  await recovered;
  assert.equal(page.context.listCache.issues[0].title, '基準');
});

test('画面の差分中の通知と読取失敗を保持し、失敗や破棄した要求を基準にしない', async () => {
  const page = await listPage();
  const start = page.start();
  page.requests.shift()!.resolve(whole('c1', [issue('owner/gone', 2, '基準')]));
  await start;
  const old = page.periodic();
  const oldRequest = page.requests.shift()!;
  const notification = page.notice('owner/gone', 2);
  page.requests.shift()!.resolve(issue('owner/gone', 2, '新しい個別'));
  await notification;
  oldRequest.resolve(
    delta('c2', {
      updated: [issue('owner/gone', 2, '古い差分')],
      unreadProjects: unread,
    }),
  );
  await old;
  assert.equal(page.context.listCache.issues[0].title, '新しい個別');
  assert.equal(page.context.listCache.issues[0].readFailed, true);
  assert.match(page.errors.at(-1) ?? '', /owner\/gone/);
  const failed = page.periodic();
  const failedRequest = page.requests.shift()!;
  assert.match(failedRequest.url, /since=c2/);
  failedRequest.reject(new Error('接続失敗'));
  await assert.rejects(failed, /接続失敗/);
  const next = page.periodic();
  assert.match(page.requests[0].url, /since=c2/);
  page.requests.shift()!.resolve(delta('c3', { unreadProjects: unread }));
  await next;
  assert.equal(page.context.listCache.issues[0].title, '新しい個別');
  assert.equal(page.context.listCache.issues[0].readFailed, true);
  const wrongScope = page.periodic();
  page.requests.shift()!.resolve(delta('wrong-scope', { scope: 'full' }));
  await wrongScope;
  assert.equal(page.context.listCache.cursor, 'c3');
  const earlier = page.periodic();
  const earlierRequest = page.requests.shift()!;
  assert.match(earlierRequest.url, /since=c3/);
  const later = page.periodic();
  page.requests.shift()!.resolve(delta('c4', { unreadProjects: unread }));
  await later;
  earlierRequest.resolve(delta('wrong-cursor'));
  await earlier;
  assert.equal(page.context.listCache.cursor, 'c4');
  const discarded = page.periodic();
  const stale = page.requests.shift()!;
  assert.match(stale.url, /since=c4/);
  // 別の全体応答が先に到着し、カーソルとスコープを更新する。
  const full = page.run('tab = "todos"; listScreen.refresh(true, true)');
  assert.equal(page.requests[0].url, '/api/list');
  page.requests
    .shift()!
    .resolve(whole('full', [issue('o/r', 3, '全範囲')], 'full'));
  await full;
  stale.resolve(delta('invalid', { removed: ['issue:o/r#3'] }));
  await discarded;
  assert.equal(page.context.listCache.cursor, 'full');
  assert.equal(page.context.listCache.scope, 'full');
  const after = page.periodic();
  assert.equal(page.requests[0].url, '/api/list?since=full');
  page.requests.shift()!.resolve(delta('full2', { scope: 'full' }));
  await after;
  assert.equal(page.context.listCache.issues[0].title, '全範囲');
  const expired = page.periodic();
  const expiredRequest = page.requests.shift()!;
  const auth = page.run('api("/api/session")');
  page.requests.shift()!.resolve({ testUnauthorized: true });
  await assert.rejects(auth, /認証切れ/);
  expiredRequest.resolve(
    delta('expired', { added: [issue('o/r', 4, '捨てる')] }),
  );
  await expired;
  assert.equal(page.context.listCache, null);
  const restart = page.run('tab = "issues"; listScreen = null; home(false)');
  assert.equal(page.requests[0].url, '/api/list?tabs=active');
  page.requests.shift()!.resolve(whole('fresh', [issue('o/r', 5, '復帰')]));
  await restart;
  assert.equal(page.context.listCache.cursor, 'fresh');
  assert.equal(page.context.listCache.issues[0].title, '復帰');
});
