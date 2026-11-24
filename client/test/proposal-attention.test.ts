import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import { compileSource } from '../scripts/source.ts';
import { attentionRows } from '../src/ui/request-classification.ts';

test('malformed proposal alert opens inspection with recorded and current reasons', async () => {
  const source = fs.readFileSync(
    new URL('../src/ui/app-attention-alerts.ts', import.meta.url),
    'utf8',
  );
  const alert = {
    key: 'unfiled-proposal:owner/repo#1:10',
    kind: 'unfiled_proposal',
    state: 'waiting',
    project: 'owner/repo',
    number: 1,
    proposalId: null,
    commentId: 10,
    proposalStatus: 'rejected',
    stateLabel: '1時間超・採用拒否',
    reason: '案の形式が不正です（当時の詳細未保存）。',
  };
  const candidate = {
    recordedReason: alert.reason,
    parseError: '案のJSONを読めません。',
    body: '<!-- issue-desk-proposal {bad} -->',
    fingerprint: 'abc',
  };
  let children: any[] = [];
  const calls: string[] = [];
  const context = {
    URLSearchParams,
    attentionRows,
    shownProject: (x: string) => x,
    el: (tag: string, attrs: any, ...children: any[]) => ({
      tag,
      ...attrs,
      children: children.flat(),
    }),
    head: (...args: string[]) => ({ tag: 'head', text: args.join(' ') }),
    $: () => ({
      replaceChildren: (...items: any[]) => {
        children = items;
      },
    }),
    reset: () => 1,
    current: null,
    crypto: { randomUUID: () => 'id' },
    api: async (url: string) => {
      calls.push(url);
      return url.startsWith('/api/proposals/dismissal?') ? candidate : [alert];
    },
    isCurrent: () => true,
  };
  const run = vm.runInNewContext(
    compileSource(source + '\nattentionRouteView;', 'attention.ts'),
    context,
  );
  await run(alert.key);
  const all = (nodes: any[]): any[] =>
    nodes.flatMap((node) => [node, ...all(node.children ?? [])]);
  assert.ok(all(children).some((x) => x.text === `理由: ${alert.reason}`));
  const button = all(children).find((x) => x.text === '対応不要として整理');
  assert.ok(button);
  await button.onClick();
  assert.equal(calls[0], '/api/attention/alerts');
  const query = new URLSearchParams(calls[1].split('?')[1]);
  assert.equal(query.get('proposalId'), '');
  assert.equal(query.get('commentId'), '10');
  assert.ok(
    all(children).some(
      (x) => x.text === `保存時の理由: ${candidate.recordedReason}`,
    ),
  );
  assert.ok(
    all(children).some(
      (x) => x.text === `現在の解析結果: ${candidate.parseError}`,
    ),
  );
  assert.ok(
    all(children).some((x) => x.tag === 'pre' && x.text === candidate.body),
  );
});
