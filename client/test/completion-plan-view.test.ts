import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import ts from 'typescript';

test('完了計画の保存提示と了承は下書きの読込と更新で保持される', async () => {
  const source = readFileSync(
    new URL('../src/ui/app-draft.ts', import.meta.url),
    'utf8',
  ).split('\n// TODOに戻された相談')[0];
  const plan = {
    version: 1,
    basis: '既存の認証条件を維持する',
    approvals: [],
    steps: [],
  };
  const messages = [
    {
      id: 'proposal',
      role: 'assistant',
      text: '提示済み計画',
      completionProposal: 'original',
      completionPlan: plan,
    },
    {
      id: 'response',
      role: 'user',
      text: 'A',
      at: '2026-11-07T10:00:00Z',
      source: 'cli-key',
    },
  ];
  let payload: any = {
    id: 'draft',
    status: 'draft',
    revision: 1,
    ready: true,
    messages,
    questions: [],
    completionPlan: plan,
    completionApproval: {
      fingerprint: 'original',
      messageId: 'response',
      quote: 'A',
      at: messages[1].at,
      source: 'cli-key',
    },
    completionComparison: {
      rule: 'completion-plan-presented-reply-v1',
      presented: 'original',
      presentedPlan: plan,
      candidate: 'rewritten',
      candidatePlan: { ...plan, basis: '候補が書き直した根拠' },
      equivalent: false,
      presentedReplyCompatible: true,
      changes: ['basis'],
    },
    actions: ['chat'],
  };
  const rendered: any[] = [];
  let refresh: (() => Promise<void>) | undefined;
  const context: any = {
    current: null,
    reset: () => 1,
    isCurrent: () => true,
    api: async () => structuredClone(payload),
    renderDraft: (d: any) => rendered.push(structuredClone(d)),
    pollDraft: (fn: () => Promise<void>) => {
      refresh = fn;
    },
    draftPriorityHeld: () => false,
    draftStatus: (d: any) => d.status,
    activeComposer: undefined,
  };
  vm.runInNewContext(
    ts.transpile(`${source}\nglobalThis.view = draftView;`),
    context,
  );
  await context.view('draft', null, false);
  assert.deepEqual(rendered.at(-1).messages, messages);
  assert.equal(rendered.at(-1).completionApproval.fingerprint, 'original');
  assert.deepEqual(
    rendered.at(-1).completionComparison,
    payload.completionComparison,
  );
  assert.deepEqual(rendered.at(-1).completionPlan, plan);
  payload = {
    ...payload,
    revision: 2,
    completionReconfirmation: 'changed',
    completionReconfirmationAfter: 2,
    ready: false,
    completionApproval: null,
    questions: ['接続経路が変わった計画を確認してください'],
  };
  await refresh!();
  assert.equal(rendered.at(-1).ready, false);
  assert.deepEqual(rendered.at(-1).questions, payload.questions);
  assert.equal(rendered.at(-1).completionReconfirmationAfter, 2);
  assert.deepEqual(rendered.at(-1).messages[0].completionPlan, plan);
});
