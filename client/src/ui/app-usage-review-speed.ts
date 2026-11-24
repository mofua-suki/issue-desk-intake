// 稼働実績の「利用量の見直し」の速さ・難易度の前後比較（Issue #614。集計は server/src/usage-review-speed-tiers.mjs、定義は
// docs/usage-review.md『Speed and tier before and after』）。client/src/ui/app.js の一部で、単独のモジュールではない（受付は
// client/scripts/build.ts の順に app.js とつないだ1つのモジュールとして /app.js を配信する）。client/src/ui/app-stats.js の
// usageReviewResult から呼ぶ。speed・tiers の無い応答（更新前の受付）では出さない。値の無い回・項目は「欠測」。
const usageReviewExtraNames: Record<string, string> = {
  compared: '比較',
  waiting: '比較待ち',
  cost_only: '比較なし',
  undeterminable: '判定不能',
};
const usageReviewSpeedHeaders = [
  '回',
  '状態',
  '完了（PR作成）／日',
  'マージ完了／日',
  '実装の所要の中央値',
  '空き時間／日',
  '起票→着手の中央値',
  '高の割合',
  '低・中からの引き上げ',
  '高が不要だった可能性',
];
const usageReviewSpeedNotes = [
  [
    '利用量の見直しの速さ・難易度の前後比較',
    '費用対効果と同じ前後の期間（最初の対策PRのマージの前7日と、最後の対策PRのマージから最長7日）で、このPCの runner.log から処理の速さ（1日あたりの完了（PR作成）件数とマージ完了の件数、外側の「実装」の工程の所要の中央値、実行待ちがあるのに実装の枠が空いていた時間の日平均、起票→着手の中央値）を、受付の相談の難易度の記録から相談の難易度（出発点が高の相談の割合、低・中からの引き上げの件数、「高が不要だった可能性」の候補の割合）を「前 → 後」で並べます。参考として並べるだけで、「元が取れていない」の判定と要対応には使いません。記録の無い値と、比べる期間の無い回は「欠測」です。起票日時を確定できない完了・着手は件数を「（欠測 前N・後M件）」と添え、確定できた完了が無ければ0件とは出しません。空き時間の実装の枠の数は監視の日次記録の値を使い、期間のすべての日の記録がそろわない・期間の中で変わった回は「欠測」です。後の期間が3日に満たない回は「比較待ち」、前の期間の全体が runner.log に無い回の速さは「判定不能」です。候補は兆候で、判定の誤りとは断定しません。',
  ],
];
const extraNumber = (v: unknown, f: (v: number) => string) =>
  typeof v === 'number' && Number.isFinite(v) ? f(v) : '欠測';
const extraFormats = {
  perDay: (v: unknown) => extraNumber(v, (x: number) => `${x.toFixed(1)}件`),
  minutes: (v: unknown) =>
    extraNumber(v, (x: number) => `${(x / 60000).toFixed(1)}分`),
  hours: (v: unknown) =>
    extraNumber(v, (x: number) => `${(x / 3600000).toFixed(2)}時間`),
  pct: (v: unknown) => extraNumber(v, (x: number) => `${Math.round(x * 100)}%`),
  count: (v: unknown) => extraNumber(v, (x) => `${x}件`),
};
// 前 → 後。前後の値の無い回は「欠測」。missingKey は起票日時を確定できない件数（あれば「（欠測 前N・後M件）」を添える）。
function extraPair(
  x: ReviewExtra | undefined,
  key: string,
  f: (v: unknown) => string,
  missingKey: string | null = null,
) {
  if (!x?.before || !x?.after) return '欠測';
  const count = (v: unknown) =>
    typeof v === 'number' && Number.isFinite(v) ? v : 0;
  const [b, a] = missingKey
    ? [count(x.before[missingKey]), count(x.after[missingKey])]
    : [0, 0];
  return `${f(x.before[key])} → ${f(x.after[key])}${b || a ? `（欠測 前${b}・後${a}件）` : ''}`;
}
function extraState(label: string, x: ReviewExtra | undefined) {
  if (!x) return `${label}: 欠測（記録なし）`;
  const name = usageReviewExtraNames[x.result] || x.result;
  const missing = (x.unavailable || []).length
    ? `。${x.unavailable?.join('、')}`
    : '';
  return `${label}: ${x.before ? name : `欠測・${name}`}${x.reason ? `（${x.reason}）` : ''}${missing}`;
}
function usageReviewSpeedCells(r: UsageReviewRound) {
  const s = r.speed;
  const t = r.tiers;
  return [
    `第${r.round}回`,
    `${extraState('速さ', s)}／${extraState('難易度', t)}`,
    extraPair(s, 'completedPerDay', extraFormats.perDay, 'completedMissing'),
    extraPair(s, 'mergedPerDay', extraFormats.perDay),
    extraPair(s, 'implementationMedianMs', extraFormats.minutes),
    extraPair(s, 'idlePerDayMs', extraFormats.hours),
    extraPair(s, 'leadMedianMs', extraFormats.hours, 'leadMissing'),
    extraPair(t, 'heavyRatio', extraFormats.pct),
    extraPair(t, 'raised', extraFormats.count),
    extraPair(t, 'unneededRatio', extraFormats.pct),
  ];
}
function usageReviewSpeedResult(rounds: UsageReviewRound[]) {
  if (!rounds.some((x) => 'speed' in x || 'tiers' in x)) return [];
  return [
    el('p', {
      class: 'stats-coverage',
      text: '速さ・難易度の前後比較（参考。「元が取れていない」の判定には使いません）',
    }),
    statsTable(
      usageReviewSpeedHeaders,
      rounds.map(usageReviewSpeedCells),
      'usage-review-speed',
    ),
  ];
}
