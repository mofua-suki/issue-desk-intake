// 実装の難易度の表示（Issue #489。Issue画面の実行先の欄は Issue #2419 で削った）。相談画面の起票時の判定、稼働実績の「相談の難易度と結末」の列。
// client/src/ui/app.js の一部で、単独のモジュールではない。受付は client/scripts/build.ts の順に app.js とつないだ
// 1つのモジュールとして /app.js を配信する。難易度は server/internal/filing/tier.go（Issue #487）、引き上げは
// shared/src/tier-escalation.mjs（Issue #488）、稼働実績の値は server/src/implementation-tier-report.mjs。
// 表示名は監視（issue-desk-worker）の shared/src/implementation-tiers.mjs の implementationTierNames・implementationTierSourceNames と同じ。
const implementationTierLabels: Record<string, string> = {
  light: '低',
  standard: '中',
  heavy: '高',
  fixed: '固定値',
};
const implementationTierSourceLabels: Record<string, string> = {
  user: '依頼者の指定',
  desk: '受付の判定',
  fixed: '固定値',
};
const implementationTierLabel = (level?: string) =>
  implementationTierLabels[level ?? ''] || level || '不明';
const implementationTierSourceLabel = (source?: string) =>
  implementationTierSourceLabels[source ?? ''] || source || '不明';
const implementationTierTime = (t?: string) =>
  typeof t === 'string' && Number.isFinite(Date.parse(t))
    ? listDate(t)
    : '時刻不明';
// 引き上げの記録（時刻・難易度・理由）の1件。
const implementationTierStep = (h?: TierChange) =>
  `${implementationTierTime(h?.at)} ${implementationTierLabel(h?.from)}→${implementationTierLabel(h?.to)}：${h?.reason || '理由なし'}`;
const implementationTierHistory = (history?: TierChange[]) =>
  (Array.isArray(history) ? history : []).filter(
    (h) => h && typeof h === 'object',
  );

// 判定の元と理由。decision は起票時の判定（下書きの implementationTier。受付の判定の理由を持つ）。監視の記録の
// tier.reason は「起票本文の目印」を読んだ旨や再試行の理由に置き換わるので、判定の理由には使わない（引き上げは
// 別に出す）。固定値は監視の記録の理由（調査Issue・目印なしなど）がその理由。記録が無ければその旨を出す。
function implementationTierJudgement(
  t: TierRecord | null,
  decision?: TierRecord,
) {
  if (decision && typeof decision === 'object' && decision.level)
    return `起票時の判定: ${implementationTierLabel(decision.level)}（${implementationTierSourceLabel(decision.source)}：${decision.reason || '理由の記録なし'}）`;
  if (t?.level === 'fixed')
    return `判定の元: 固定値（${t.reason || '理由の記録なし'}）`;
  return `判定の元: ${implementationTierSourceLabel(t?.source)}（起票時の判定の理由は受付に記録がありません）`;
}

// 相談画面の起票時の判定（下書きの implementationTier。起票したときに受付が決めた難易度）。見出しと本文の組。
// 記録の無い下書き（起票前・調査Issue・implementation.tiers の無い設定）は null（何も出さない）。
function implementationTierRows(tier?: TierRecord) {
  if (!tier || typeof tier !== 'object' || !tier.level) return null;
  return [
    ['難易度', implementationTierLabel(tier.level)],
    ['判定の元', implementationTierSourceLabel(tier.source)],
    ['理由', tier.reason || '記録なし'],
    ['判定の時刻', implementationTierTime(tier.decidedAt)],
  ];
}
function implementationTierRecord(tier?: TierRecord) {
  const rows = implementationTierRows(tier);
  if (!rows) return null;
  return fieldBlock(
    '実装の難易度',
    el(
      'dl',
      { class: 'tier-record implementation-tier-record' },
      rows.flatMap(([label, value]) => [
        el('dt', { text: label }),
        el('dd', { text: value }),
      ]),
    ),
  );
}

// 稼働実績の「相談の難易度と結末」の列（tierReport の行の implementation と implementationDecision）。難易度・モデル・
// 推論レベル、起票時の判定（判定の元と理由）、引き上げ。監視の記録が無くても起票時の判定があれば出す。
const implementationTierHeader = '実装の難易度';
function implementationTierCell(x?: TierRecord, decision?: TierRecord) {
  const run = x && typeof x === 'object' ? x : null;
  const hasDecision = !!(
    decision &&
    typeof decision === 'object' &&
    decision.level
  );
  const history = implementationTierHistory(run?.history);
  return [
    run
      ? `難易度「${implementationTierLabel(run.level)}」：${run.model || '不明'} ／ ${run.effort || '不明'}`
      : '記録なし（このPCの監視に難易度の記録がありません）',
    run || hasDecision ? implementationTierJudgement(run, decision) : null,
    run
      ? history.length
        ? history.map((h) => `引き上げ ${implementationTierStep(h)}`).join('\n')
        : '引き上げなし'
      : null,
  ]
    .filter(Boolean)
    .join('\n');
}
// 行に実装の難易度（implementation）がある応答か。無い応答（更新前の受付）では列を足さない。
const implementationTierColumn = (rows: object[]) =>
  rows.some((r: object) => r && Object.hasOwn(r, 'implementation'));

const implementationTierNotes = [
  [
    '実装の難易度（相談の難易度と結末の列）',
    '起票したIssueを実装したときの難易度（低・中・高・固定値）・モデル・推論レベルと、起票時の判定（受付が起票の直前に決めた難易度と、判定の元（依頼者の指定／受付の判定）・理由）、再試行での引き上げ（時刻・難易度・理由）です。難易度・モデル・推論レベルと引き上げはこのPCの監視の記録（実行のたびに保存する実行設定）から読むため、他のPCが担当したIssue・まだ実行していないIssue・難易度の導入前に実行したIssueは「記録なし」です（起票時の判定は受付の記録から出します）。難易度は最後に実行したときの難易度で、引き上げた後はその難易度です。',
  ],
];
