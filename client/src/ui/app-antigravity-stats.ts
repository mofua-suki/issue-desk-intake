// 稼働実績の「フォローアップの実行先別の比較（Antigravity と Claude）」（Issue #843。集計は shared/src/antigravity-stats.mjs、定義は
// docs/providers-antigravity.md『Comparing follow-ups by provider』）。client/src/ui/app.js の一部で、単独のモジュールではない
// （client/scripts/build.ts の順に app.js とつなぐ）。antigravity の無い応答（更新前の受付）では出さない。
const antigravityStatsHeaders = [
  '項目',
  'Antigravity 群（切り替えたものを含む）',
  'Claude 群（最初から Claude）',
];
const antigravityStatsNotes = [
  [
    'フォローアップの実行先別の比較',
    '直近30日にマージしたフォローアップ（コントローラー生成の後続Issue）を、このPCの監視の記録で分けます。Antigravity で実装したもの（Claude へ切り替えたものを含む）が Antigravity 群、最初から Claude で実装したものが Claude 群で、Codex で実装したもの・実行先の記録が無いものはどちらにも数えません。一発合格は、最初の固定テストが合格し、独立レビューの自動差し戻しが無かったものです。差し戻しの回数は自動差し戻しの回数（人の指示で数え直しません）、固定テストの不合格の回数は実装の実行ごとの記録の合計、Claude への切り替えは2回続けて不合格・差し戻しになって切り替えた件数です。トークンは使用量の記録（runtime/usage/）の実装の工程の入力と出力の合計の1件あたりで、Claude の入力にはキャッシュの読み書きを含みません。固定テストの記録が無い（この機能の前に始めた・別のPCで実装した）もの、行の無い・値の無い行を含むものは「記録なし」で、割合と1件あたりの分母に入れません。',
  ],
];
const agPercent = (r: number) =>
  typeof r === 'number' ? `${(r * 100).toFixed(1)}%` : '記録なし';
const agUnknown = (n: number | undefined) =>
  n ? `、ほかに記録なし ${n}件` : '';
function antigravityStatsTotal(
  x: PerIssueMetric | undefined,
  unit: string,
  digits = 2,
) {
  if (!x || x.total === null) return '記録なし';
  const per =
    typeof x.perIssue === 'number'
      ? `1件あたり ${x.perIssue.toFixed(digits)}${unit}`
      : '1件あたり 記録なし';
  return `${x.total}${unit}（${per}${agUnknown(x.unknown)}）`;
}
function antigravityStatsTokens(x: PerIssueMetric | undefined) {
  if (!x) return '記録なし（使用量を読めません）';
  if (typeof x.perIssue !== 'number') return '記録なし';
  return `${Math.round(x.perIssue).toLocaleString('ja-JP')}トークン（${x.known}件${agUnknown(x.unknown)}）`;
}
// 表の行（項目、Antigravity 群、Claude 群）。
function antigravityStatsRows(a: AntigravityReport) {
  const groups = [a.groups!.antigravity, a.groups!.claude];
  const row = (label: string, cell: (g: AntigravityGroup) => string) => [
    label,
    ...groups.map(cell),
  ];
  return [
    row(
      `件数（直近${a.days}日にマージしたフォローアップ）`,
      (g) => `${g.count}件`,
    ),
    row('一発合格の割合', (g) =>
      g.oneShot.known
        ? `${agPercent(g.oneShot.rate)}（${g.oneShot.passed}件／${g.oneShot.known}件${agUnknown(g.oneShot.unknown)}）`
        : '記録なし',
    ),
    row('差し戻しの回数（合計と1件あたり）', (g) =>
      antigravityStatsTotal(g.returns, '回'),
    ),
    row('Claude への切り替え', (g) => `${g.switches}件`),
    row('固定テストの不合格の回数（合計と1件あたり）', (g) =>
      antigravityStatsTotal(g.testFailures, '回'),
    ),
    row('トークン（実装の工程、1件あたり）', (g) =>
      antigravityStatsTokens(g.tokens),
    ),
  ];
}
function antigravityStatsResult(r: StatsReport) {
  if (!r.antigravity) return [];
  const a = r.antigravity;
  const title = el('h2', {
    text: 'フォローアップの実行先別の比較（Antigravity と Claude）',
  });
  if (a.reason || !a.groups)
    return [
      title,
      el('div', { class: 'error-box', text: a.reason || '記録なし' }),
    ];
  return [
    title,
    ...(a.unavailable || []).map((text) =>
      el('div', { class: 'error-box', text }),
    ),
    el('p', {
      class: 'stats-coverage',
      text: `直近${a.days}日（${reviewTime(a.from)}〜${reviewTime(a.to)}）にマージしたフォローアップ。Codex で実装したもの・実行先の記録が無いもの ${a.excluded?.other ?? 0}件は数えていません。`,
    }),
    statsTable(
      antigravityStatsHeaders,
      antigravityStatsRows(a),
      'antigravity-stats',
    ),
  ];
}
