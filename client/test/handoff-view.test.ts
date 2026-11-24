import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import ts from 'typescript';

test('引き継ぎ行と直接URLと更新中の引き継ぎは元の相談を表示する', async () => {
  const draft = readFileSync(
    new URL('../src/ui/app-draft.ts', import.meta.url),
    'utf8',
  ).split('\n// TODOに戻された相談')[0];
  const rows = readFileSync(
    new URL('../src/ui/app-list-rows.ts', import.meta.url),
    'utf8',
  ).split('const listRowTitle')[0];
  const code = ts.transpile(
    `${draft}\n${rows}\nglobalThis.open = listRowOpen; globalThis.view = draftView;`,
  );
  const rendered: any[] = [];
  const requests: string[] = [];
  let refresh: (() => Promise<void>) | undefined;
  let next: any = {
    id: 'source',
    status: 'project_handoff',
    messages: [{ text: '元の会話' }],
    actions: [],
  };
  let checkpoints = 0;
  const context: any = {
    current: null,
    mobileView: '',
    activeComposer: {
      checkpoint: async () => {
        checkpoints++;
      },
    },
    remember: () => {},
    draftHash: (id: string) => id,
    reset: () => 1,
    isCurrent: () => true,
    api: async (url: string) => {
      requests.push(url);
      return next;
    },
    renderDraft: (d: any) => rendered.push(d),
    pollDraft: (fn: () => Promise<void>) => {
      refresh = fn;
    },
    error: () => {},
    draftPriorityHeld: () => false,
  };
  vm.runInNewContext(code, context);
  await context.open({ kind: 'draft', id: 'source', handoffRow: true });
  assert.equal(requests[0], '/api/drafts/source');
  assert.equal(rendered.at(-1).messages[0].text, '元の会話');
  await context.view('source', null, false);
  assert.equal(rendered.length, 2);
  next = { id: 'source', status: 'thinking', messages: [], actions: ['chat'] };
  await context.view('source', null, false);
  next = {
    id: 'source',
    status: 'project_handoff',
    messages: [{ text: '更新後の会話' }],
    actions: [],
  };
  await refresh!();
  assert.equal(checkpoints, 1);
  assert.equal(rendered.at(-1).messages[0].text, '更新後の会話');
  assert.deepEqual(rendered.at(-1).actions, []);
  assert.ok(requests.every((url) => url === '/api/drafts/source'));
});
