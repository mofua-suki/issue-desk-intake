import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { stripTypeScriptTypes } from 'node:module';
import vm from 'node:vm';

test('定期更新は自動への変更を表示し、未送信の選択を保持する', async () => {
  const source = readFileSync(
    new URL('../src/ui/app-draft.ts', import.meta.url),
    'utf8',
  );
  const view = source.split('\n// TODOに戻された相談')[0];
  const priority = `const draftPriorityChoice${source.split('const draftPriorityChoice')[1]}`;
  const code = stripTypeScriptTypes(
    `${view}\n${priority}\nglobalThis.view = draftView; globalThis.box = draftPriorityBox;`,
  );
  let next: any = {
    id: 'one',
    status: 'thinking',
    revision: 1,
    actions: [],
    priorityChangeable: true,
    priority: 'high',
    prioritySource: { via: 'draft-screen' },
  };
  let refresh: () => Promise<void> = async () => {};
  let choice: any = null;
  const rendered: any[] = [];
  const context: any = {
    current: null,
    mobileView: '',
    activeComposer: null,
    remember: () => {},
    draftHash: (id: string) => id,
    reset: () => 1,
    isCurrent: () => true,
    api: async () => structuredClone(next),
    pollDraft: (fn: () => Promise<void>) => {
      refresh = fn;
    },
    draftStatus: (d: any) => d.status,
    $: () => choice,
    userPriorityNames: { high: '割り込み', normal: '通常' },
    el: (tag: string, attrs: any, ...children: any[]) => ({
      tag,
      ...attrs,
      children,
    }),
    field: (...args: any[]) => args,
  };
  context.renderDraft = (d: any) => {
    const box = context.box(d, {});
    rendered.push({
      text: box.children[0].text,
      priority: d.priority,
      source: d.prioritySource,
    });
    choice = { value: d.priority || 'auto' };
  };
  vm.runInNewContext(code, context);
  await context.view('one', null, false);
  assert.equal(rendered[0].text, '優先度: 割り込み（利用者の指定）');
  next = {
    id: 'one',
    status: 'thinking',
    revision: 1,
    actions: [],
    priorityChangeable: true,
  };
  await refresh();
  assert.equal(rendered.length, 2);
  assert.equal(rendered[1].text, '優先度: 自動');
  assert.equal(rendered[1].source, undefined);
  await refresh();
  assert.equal(rendered.length, 2);
  choice.value = 'normal';
  next = { ...next, priority: 'high', prioritySource: { via: 'draft-screen' } };
  await refresh();
  assert.equal(rendered.length, 2);
  assert.equal(choice.value, 'normal');
  choice.value = 'auto';
  await refresh();
  assert.equal(rendered.length, 3);
  assert.equal(rendered[2].text, '優先度: 割り込み（利用者の指定）');
});
