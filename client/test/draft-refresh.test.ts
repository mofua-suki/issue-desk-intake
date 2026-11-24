import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import ts from 'typescript';

function setup() {
  const source = (name: string) =>
    readFileSync(new URL(`../src/ui/${name}`, import.meta.url), 'utf8');
  const routing = source('app-routing.ts')
    .split('function routeError(')[1]
    .split('// 「TODOに戻す」')[0];
  // 定期取得の予約（poll）と、相談の周期の決め方（pollPeriodic・pollDraft）。SSEは未接続のまま動かす。
  const liveSource = source('app-live.ts');
  const live =
    liveSource.split('// 受付からの即時通知')[0] +
    'let liveConnected = false, livePeriodic: any = null;\nconst listPollMs' +
    liveSource
      .split('const listPollMs')[1]
      .split('function liveConnection(')[0];
  const draft = source('app-draft.ts').split(
    'async function submittedDraft',
  )[0];
  const code = ts.transpile(
    `${live}\nfunction routeError(${routing}\n${draft}\nglobalThis.view = draftView;`,
  );
  const timers: { fn: () => Promise<void>; ms: number }[] = [];
  const requests: string[] = [];
  const errors: any[] = [];
  const todos: string[] = [];
  const app: any = { children: [] };
  let generation = 0;
  let response: () => Promise<any> = async () => ({
    id: 'a',
    status: 'thinking',
    revision: 1,
  });
  const context: any = {
    current: null,
    mobileView: '',
    activeComposer: null,
    routeId: 0,
    timer: null,
    polling: null,
    listScreen: null,
    listNeedFull: false,
    liveEarly: null,
    remember: () => {},
    draftHash: (id: string) => id,
    reset: () => {
      context.routeId = ++generation;
      context.current = null;
      return generation;
    },
    isCurrent: (id: number) => id === generation,
    api: async (url: string) => {
      requests.push(url);
      return response();
    },
    renderDraft: (d: any) => {
      app.children = [d.messages?.[0]?.text ?? '会話', '送信欄'];
    },
    error: (e: any) => errors.push(e),
    uiError: (e: any) => e,
    draftPriorityHeld: () => false,
    draftStatus: (d: any) => d.status,
    todoView: (id: string) => {
      todos.push(id);
      context.reset();
    },
    $: () => app,
    el: (tag: string, attrs: any) => ({ tag, ...attrs }),
    cancelTimer: () => {},
    liveHidden: () => false,
    // 周期の計算が実時計の進みで1msずれないよう、時刻を固定する。
    Date: { now: () => 0 },
    setTimeout: (fn: () => Promise<void>, ms: number) => {
      timers.push({ fn, ms });
      return timers.length;
    },
  };
  app.replaceChildren = (...children: any[]) => {
    app.children = children;
  };
  vm.runInNewContext(code, context);
  return {
    context,
    timers,
    requests,
    errors,
    todos,
    app,
    respond: (fn: () => Promise<any>) => {
      response = fn;
    },
  };
}

const failure = (status: number, code?: string) =>
  Object.assign(new Error('GET failed'), { status, code });

test('再取得404は既存案内に切り替え、再予約しない', async () => {
  const h = setup();
  await h.context.view('a');
  assert.deepEqual(h.app.children, ['会話', '送信欄']);
  h.respond(async () => {
    throw failure(404);
  });
  await h.timers.shift()!.fn();
  assert.equal(h.requests.length, 2);
  assert.equal(h.requests[1], '/api/drafts/a');
  assert.equal(h.errors[0].status, 404);
  assert.equal(h.app.children.length, 2);
  assert.equal(h.app.children[0].tag, 'button');
  assert.equal(h.app.children[1].class, 'empty');
  assert.ok(
    !h.app.children.includes('会話') && !h.app.children.includes('送信欄'),
  );
  assert.equal(h.timers.length, 0);
});

test('5xxは再試行され成功後に回復する', async () => {
  const h = setup();
  await h.context.view('a');
  h.respond(async () => {
    throw failure(503);
  });
  await h.timers.shift()!.fn();
  assert.equal(h.errors[0].status, 503);
  assert.equal(h.timers[0].ms, 10000);
  h.respond(async () => ({
    id: 'a',
    status: 'thinking',
    revision: 2,
    messages: [{ text: '回復した会話' }],
  }));
  await h.timers.shift()!.fn();
  assert.equal(h.app.children[0], '回復した会話');
  assert.equal(h.timers[0].ms, 5000);
});

test('識別子付き409はTODOへ遷移する', async () => {
  const h = setup();
  await h.context.view('a');
  h.respond(async () => {
    throw failure(409, 'draft_returned_to_todo');
  });
  await h.timers.shift()!.fn();
  assert.deepEqual(h.todos, ['a']);
  assert.equal(h.timers.length, 0);
});

test('画面切替後の古い成功と404は現在画面を変えない', async () => {
  for (const status of [200, 404]) {
    const h = setup();
    await h.context.view('a');
    let finish!: (value: any) => void;
    h.respond(
      () =>
        new Promise((resolve, reject) => {
          finish = status === 200 ? resolve : reject;
        }),
    );
    const pending = h.timers.shift()!.fn();
    h.context.reset();
    h.app.children = ['別の画面'];
    finish(
      status === 200
        ? { id: 'a', status: 'thinking', revision: 2 }
        : failure(404),
    );
    await pending;
    assert.deepEqual(h.app.children, ['別の画面']);
    assert.equal(h.errors.length, 0);
    assert.equal(h.timers.length, 0);
  }
});
