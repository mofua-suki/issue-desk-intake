// 稼働実績の「起票本文の参照の有無による前後の比較」（Issue #978。集計は server/src/issue-body-stats.mjs、定義は
// docs/intake-issue-body.md『Issue body before/after stats』）。client/src/ui/app.js の一部で、単独のモジュールではない
// （client/scripts/build.ts の順に app.js とつなぐ）。issueBody の無い応答（更新前の受付）では出さない。
const issueBodyStatsHeaders = [
  '項目',
  '導入前（参照の折りたたみなし）',
  '導入後（参照の折りたたみあり）',
];
const issueBodyStatsNotes = [
  [
    '起票本文の参照の有無による前後の比較',
    'このPCの監視の状態にある受付専用アカウントの起票のうち、直近30日に実装・競合解消の使用量の行があるものを、起票本文に参照の折りたたみ（受付の経緯・根拠）が無い「導入前」とある「導入後」に分けます。文字数は起票本文の中央値・上位1割（90パーセンタイル）・最大で、導入後は実装AIへ渡す部分（参照を外した本文）と参照の部分も示します。実装1件あたりの使用量は直近30日の実装・競合解消の行（費用の補正を通した値）を実行先ごとに数え、Claude は費用の目安とキャッシュ読み、Codex は入力＋出力トークンとキャッシュ読み、Antigravity は入力＋出力トークンで、実行先をまたいで合算しません。差し戻しの割合は手戻りの表の差し戻しの割合と同じ定義（直近30日に独立レビューを始めたか自動差し戻しのあったIssueのうち、自動差し戻しのあった割合）です。受付の出力トークンは、起票した相談の受付の行の出力トークンを、その相談から起票したIssueの数で等分した起票1件あたりです。値の無い行を含む件は合計にも1件あたりの分母にも入れず「不明」の件数で示します。集計はこのPCの記録だけで、仕事の内容の違いも値に表れるので、差だけで効果とは断定できません。',
  ],
];
const ibCount = (v: number) => Math.round(v).toLocaleString('ja-JP');
const ibCost = (v: number) => `$${v.toFixed(v >= 1 ? 2 : 4)}`;
function issueBodyCharsText(s: { median: number; p90: number; max: number }) {
  if (!s) return '記録なし';
  return `中央値 ${ibCount(s.median)}・上位1割 ${ibCount(s.p90)}・最大 ${ibCount(s.max)}文字`;
}
// 1件あたりの値。値のある件が無ければ「記録なし」か「不明（N件）」。
function issueBodyMetricText(
  m: PerIssueMetric | undefined,
  format: (v: number) => string,
) {
  if (!m) return '記録なし';
  if (typeof m.perIssue !== 'number')
    return m.unknown ? `不明（${m.unknown}件）` : '記録なし';
  return `${format(m.perIssue)}（${m.known}件${m.unknown ? `、ほかに不明 ${m.unknown}件` : ''}）`;
}
function issueBodyReturnsText(r: ReturnRate | undefined) {
  if (!r) return '記録なし（runner.log を読めません）';
  if (r.reason) return `記録なし（${r.reason}）`;
  if (!r.reviewed) return '記録なし（独立レビューの記録がありません）';
  return `${(r.rate * 100).toFixed(1)}%（差し戻し ${r.returned}件／レビュー ${r.reviewed}件）${r.since ? `（${r.since} からの記録）` : ''}`;
}
// 表の行（項目、導入前、導入後）。記録の無い群は件数の行に理由、他の行は「記録なし」。
function issueBodyStatsRows(s: IssueBodyReport) {
  const groups = [s.groups!.before, s.groups!.after];
  const row = (label: string, cell: (g: IssueBodyGroup) => string) => [
    label,
    ...groups.map((g) => (g.count ? cell(g) : '記録なし')),
  ];
  const usage =
    (
      provider: string,
      key: string,
      format: {
        (v: number): string;
        (v: number): string;
        (v: number): string;
        (v: number): string;
        (v: number): string;
      },
    ) =>
    (g: IssueBodyGroup) =>
      issueBodyMetricText(g.usage?.[provider]?.[key], format);
  return [
    [
      `件数（直近${s.days}日に実装の使用量の行がある受付の起票）`,
      ...groups.map((g) => (g.count ? `${g.count}件` : g.reason || '記録なし')),
    ],
    row('起票本文の文字数', (g) => issueBodyCharsText(g.bodyChars)),
    row('うち実装AIへ渡す部分', (g) =>
      g.passedChars
        ? issueBodyCharsText(g.passedChars)
        : '—（参照なし。本文全体を渡す）',
    ),
    row('うち参照の部分', (g) =>
      g.referenceChars ? issueBodyCharsText(g.referenceChars) : '—（参照なし）',
    ),
    row(
      'Claude: 費用の目安（実装1件あたり）',
      usage('claude', 'costUsd', ibCost),
    ),
    row(
      'Claude: キャッシュ読み（実装1件あたり）',
      usage('claude', 'cacheReadTokens', ibCount),
    ),
    row(
      'Codex: 入力＋出力トークン（実装1件あたり）',
      usage('codex', 'tokens', ibCount),
    ),
    row(
      'Codex: キャッシュ読み（実装1件あたり）',
      usage('codex', 'cacheReadTokens', ibCount),
    ),
    row(
      'Antigravity: 入力＋出力トークン（実装1件あたり）',
      usage('antigravity', 'tokens', ibCount),
    ),
    row('差し戻しの割合', (g) => issueBodyReturnsText(g.returns)),
    row('受付の出力トークン（起票1件あたり）', (g) =>
      g.intakeOutput
        ? issueBodyMetricText(g.intakeOutput, ibCount)
        : '記録なし（受付の記録を読めません）',
    ),
  ];
}
function issueBodyStatsResult(r: StatsReport) {
  if (!r.issueBody) return [];
  const s = r.issueBody;
  const title = el('h2', { text: '起票本文の参照の有無による前後の比較' });
  if (s.reason || !s.groups)
    return [
      title,
      el('div', { class: 'error-box', text: s.reason || '記録なし' }),
    ];
  return [
    title,
    ...(s.unavailable || []).map((text) =>
      el('div', { class: 'error-box', text }),
    ),
    el('p', {
      class: 'stats-coverage',
      text: `直近${s.days}日（${reviewTime(s.from)}〜${reviewTime(s.to)}）に実装の使用量の行がある、受付専用アカウントの起票。${(s.notes || []).join('')}`,
    }),
    statsTable(
      issueBodyStatsHeaders,
      issueBodyStatsRows(s),
      'issue-body-stats',
    ),
  ];
}
