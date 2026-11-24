import test from 'node:test';
import assert from 'node:assert/strict';
import vm from 'node:vm';
import { applicationSource, compileSource } from '../scripts/source.ts';

// 配信する結合ソースから段階の束ね方を実行する。
const application = applicationSource();
const start = application.source.indexOf('type OpsStepRow =');
const end = application.source.indexOf('\nfunction opsOverviewResult(', start);
assert.ok(start >= 0 && end > start);
const context = vm.createContext({});
vm.runInContext(
  `${compileSource(application.source.slice(start, end), 'ops-stages.ts')}\nthis.opsTokenStages = opsTokenStages; this.opsModelStages = opsModelStages;`,
  context,
);
const plain = (value: unknown) => JSON.parse(JSON.stringify(value));

test('トークンの割合は工程を相談・実装・レビュー・コンフリクト解消・その他へ束ね、順に並べる', () => {
  const steps = [
    { step: '代理の回答', weighted: 10, percent: 1, unknown: 0 },
    { step: '実装', weighted: 400, percent: 40, unknown: 2 },
    { step: '運用の見張り', weighted: 5, percent: 0.5, unknown: null },
    { step: '受付の相談', weighted: 300, percent: 30, unknown: 1 },
    { step: '競合解消', weighted: 30, percent: 3, unknown: 0 },
    { step: '独立レビュー', weighted: 50, percent: 5, unknown: 0 },
    {
      step: '起票直前の重複と依存の確認',
      weighted: 40,
      percent: 4,
      unknown: null,
    },
    { step: '実装報告補完', weighted: 20, percent: 2, unknown: 0 },
    { step: '注意点の更新', weighted: 1, percent: 0.1, unknown: 0 },
    { step: '未登録の工程', weighted: 2, percent: 0.2, unknown: null },
  ];
  assert.deepEqual(plain(context.opsTokenStages(steps)), [
    { step: '相談', weighted: 350, percent: 35, unknown: 1 },
    { step: '実装', weighted: 420, percent: 42, unknown: 2 },
    { step: 'レビュー', weighted: 51, percent: 5.1, unknown: 0 },
    { step: 'コンフリクト解消', weighted: 30, percent: 3, unknown: 0 },
    { step: 'その他', weighted: 7, percent: 0.7, unknown: null },
  ]);
});

test('割合や量の欠測は束ねても0にしない', () => {
  assert.deepEqual(
    plain(
      context.opsTokenStages([
        { step: '受付の相談', weighted: null, percent: null, unknown: 3 },
        { step: '代理の回答', weighted: null, percent: null, unknown: 1 },
      ]),
    ),
    [{ step: '相談', weighted: null, percent: null, unknown: 4 }],
  );
});

test('実行先・モデルの割合は同じ段階・実行先・モデル・推論レベルの件数を足し、段階内で百分率を出し直す', () => {
  const row = { provider: 'codex', model: 'm1', effort: 'medium' };
  assert.deepEqual(
    plain(
      context.opsModelStages([
        { ...row, step: '受付の相談', count: 6, percent: 100 },
        { ...row, step: '代理の回答', count: 2, percent: 50 },
        { ...row, step: '代理の回答', effort: 'high', count: 2, percent: 50 },
        { ...row, step: '実装', count: 3, percent: 100 },
      ]),
    ),
    [
      { ...row, step: '相談', count: 8, percent: 80 },
      { ...row, step: '相談', effort: 'high', count: 2, percent: 20 },
      { ...row, step: '実装', count: 3, percent: 100 },
    ],
  );
});
