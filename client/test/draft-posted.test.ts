import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { stripTypeScriptTypes } from 'node:module';
import vm from 'node:vm';

// 相談を開く処理と、優先度の変更・代理の回答の取り消しのボタンをそのまま動かし、通信と描画だけを代役にする。
function setup(stored: Record<string, unknown>) {
  const source = readFileSync(
    new URL('../src/ui/app-draft.ts', import.meta.url),
    'utf8',
  );
  const code = stripTypeScriptTypes(
    `${source}\nglobalThis.view = draftView; globalThis.priorityBox = draftPriorityBox; globalThis.undoButton = proxyUndoButton;`,
  );
  const requests: string[] = [];
  const rendered: any[] = [];
  const notices: unknown[] = [];
  const toasts: string[] = [];
  const inputImages = { live: (p: Promise<unknown>) => p, valid: () => true };
  let refresh: () => Promise<void> = async () => {};
  let post: (url: string, data: any) => unknown = () => {
    throw Error('POSTの応答が未設定です。');
  };
  const state = { stored, choice: null as any, change: null as any };
  const context: any = {
    current: null,
    mobileView: '',
    activeComposer: null,
    remember: () => {},
    draftHash: (id: string) => id,
    reset: () => 1,
    isCurrent: () => true,
    uiError: (e: unknown) => e,
    error: (e: unknown) => notices.push(e),
    routeError: (e: unknown) => notices.push(e),
    toast: (text: string) => toasts.push(text),
    action: async (_button: unknown, fn: () => Promise<unknown>) => fn(),
    api: async (url: string, data?: unknown) => {
      requests.push(`${data === undefined ? 'GET' : 'POST'} ${url}`);
      return structuredClone(
        data === undefined ? state.stored : post(url, data),
      );
    },
    pollDraft: (fn: () => Promise<void>) => {
      refresh = fn;
    },
    draftStatus: (d: any) => d.status,
    $: () => state.choice,
    userPriorityNames: { high: '割り込み', normal: '通常' },
    el: (tag: string, attrs: any, ...children: any[]) => ({
      tag,
      ...attrs,
      children,
    }),
    field: (...args: any[]) => args,
  };
  context.renderDraft = (d: any) => {
    rendered.push(structuredClone(d));
    const box = context.priorityBox(d, inputImages);
    state.choice = box.children[1][1];
    state.choice.value = d.priority || 'auto';
    state.change = box.children[2];
  };
  vm.runInNewContext(code, context);
  return {
    context,
    inputImages,
    requests,
    rendered,
    notices,
    toasts,
    state,
    refresh: () => refresh(),
    respond(fn: (url: string, data: any) => unknown) {
      post = fn;
    },
    async changePriority(value: string) {
      state.choice.value = value;
      await state.change.onclick();
    },
  };
}

const draft = (extra: Record<string, unknown> = {}) => ({
  id: 'one',
  status: 'intake_waiting',
  revision: 1,
  actions: ['chat'],
  messages: [],
  ...extra,
});

test('優先度の変更は応答の相談で描き直し、開き直しの取得をしない', async () => {
  const h = setup(draft());
  await h.context.view('one', null, false);
  h.respond((url, data) => {
    assert.equal(url, '/api/drafts/one/priority');
    h.state.stored = draft({
      priority: data.priority,
      prioritySource: { via: 'draft-screen' },
    });
    return h.state.stored;
  });
  await h.changePriority('high');
  assert.deepEqual(h.requests, [
    'GET /api/drafts/one',
    'POST /api/drafts/one/priority',
  ]);
  assert.equal(h.rendered.length, 2);
  assert.equal(h.rendered[1].priority, 'high');
  assert.deepEqual(h.notices, []);
  // 応答で開き直した後も定期取得は続き、別の画面での変更を反映する。
  h.state.stored = draft({ revision: 2, priority: 'high' });
  await h.refresh();
  assert.equal(h.requests.at(-1), 'GET /api/drafts/one');
  assert.equal(h.rendered.at(-1).revision, 2);
});

test('分割の親は操作の後に読み直し、子の進み具合を表示する', async () => {
  const progress = { total: 2, completed: 1, closed: 0, remaining: 1 };
  const parent = draft({ splitChildren: ['c1', 'c2'] });
  const h = setup({ ...parent, familyProgress: progress });
  await h.context.view('one', null, false);
  // 操作の応答には、取得だけが足す子の進み具合が無い。
  h.respond(() => ({ ...parent, priority: 'high' }));
  h.state.stored = { ...parent, priority: 'high', familyProgress: progress };
  await h.changePriority('high');
  assert.deepEqual(h.requests, [
    'GET /api/drafts/one',
    'POST /api/drafts/one/priority',
    'GET /api/drafts/one',
  ]);
  assert.deepEqual(h.rendered.at(-1).familyProgress, progress);
  assert.equal(h.rendered.at(-1).priority, 'high');
});

test('受付票の状態を表示中の相談は操作の後に読み直す', async () => {
  const receipt = { receiptId: 'one', receiptState: 'waiting' };
  const h = setup(draft(receipt));
  await h.context.view('one', null, false);
  h.respond(() => draft({ priority: 'normal' }));
  h.state.stored = draft({ ...receipt, priority: 'normal' });
  await h.changePriority('normal');
  assert.deepEqual(h.requests, [
    'GET /api/drafts/one',
    'POST /api/drafts/one/priority',
    'GET /api/drafts/one',
  ]);
  assert.equal(h.rendered.at(-1).receiptState, 'waiting');
});

test('代理の回答の取り消しは応答の相談で描き直し、失敗時は理由を出して読み直す', async () => {
  const proxy = { id: 'p1', role: 'proxy', text: '代理の回答' };
  const h = setup(draft({ messages: [proxy] }));
  await h.context.view('one', null, false);
  const press = async () => {
    const button = h.context.undoButton(h.rendered[0], proxy, h.inputImages);
    const event = { currentTarget: { dataset: {} as any, textContent: '' } };
    // 1回目は確認に変わるだけで送らない。
    await button.onClick(event);
    assert.equal(h.requests.length, 1);
    return button.onClick(event);
  };
  h.respond((url, data) => {
    assert.equal(url, '/api/drafts/one/proxy-undo');
    assert.equal(data.messageId, 'p1');
    return draft({ revision: 2 });
  });
  await press();
  assert.deepEqual(h.requests, [
    'GET /api/drafts/one',
    'POST /api/drafts/one/proxy-undo',
  ]);
  assert.deepEqual(h.rendered.at(-1).messages, []);
  assert.equal(h.rendered.at(-1).revision, 2);
  assert.equal(h.toasts.length, 1);

  const refused = Object.assign(Error('取り消し済みです。'), { status: 409 });
  h.requests.length = 1;
  h.respond(() => {
    throw refused;
  });
  await assert.rejects(press(), refused);
  assert.deepEqual(h.requests, [
    'GET /api/drafts/one',
    'POST /api/drafts/one/proxy-undo',
    'GET /api/drafts/one',
  ]);
  assert.deepEqual(h.notices, [refused]);
  assert.equal(h.toasts.length, 1);
});
