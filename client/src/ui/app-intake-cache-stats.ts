// 稼働実績の「受付AIのキャッシュ（Claude）」（Issue #880。集計は shared/src/intake-cache-stats.mjs、定義は docs/ai-usage.md の
// 「集計」）。client/src/ui/app.js の一部で、単独のモジュールではない（client/scripts/build.ts の順に app.js とつなぐ）。
// 使用量の表（client/src/ui/app-stats.js の usageResult）の後に期間ごとの工程別の表を出す。intakeCache の無い応答（更新前の受付）では
// 出さない。日別（intakeCache.days）は API にだけ載せる。不明はゼロと出さない。
const intakeCacheHeaders = [
  '工程',
  '件数',
  'キャッシュ読み',
  'キャッシュ書き',
  '読みの割合',
];
const intakeCacheNote =
  '定義: 受付の相談・起票時の変更範囲の見積もりの Claude の呼び出しだけを数え、読みの割合はキャッシュ読み÷（キャッシュ読み＋キャッシュ書き）。どちらかの値が無い行は合計・割合に入れず「不明（N件）」に数えます（Codex の行は入れません）。';
// 値の無い項目は「不明（N件）」、行の無い工程は「—」、一部だけ不明なら値に「（ほかに不明 N件）」を添える。
function intakeCacheValue(
  value: number | null | undefined,
  unknown: number,
  text: (value: number) => string,
) {
  if (typeof value !== 'number') return unknown ? `不明（${unknown}件）` : '—';
  return `${text(value)}${unknown ? `（ほかに不明 ${unknown}件）` : ''}`;
}
function intakeCacheCells(s: CacheAggregate) {
  const tokens = (v: number) => Math.round(v).toLocaleString('ja-JP');
  return [
    s.step,
    `${s.count}件`,
    intakeCacheValue(s.cacheReadTokens, s.unknown, tokens),
    intakeCacheValue(s.cacheWriteTokens, s.unknown, tokens),
    intakeCacheValue(
      s.readRatio,
      s.unknown,
      (v: number) => `${(v * 100).toFixed(1)}%`,
    ),
  ];
}
function intakeCacheResult(label: string, p: UsagePeriod) {
  if (!p?.intakeCache) return [];
  return [
    el('h2', {
      text: `${label}の期間の受付AIのキャッシュ（Claude）（${p.fromDate}〜${p.toDate}）`,
    }),
    el('p', {
      class: 'stats-coverage intake-cache-note',
      text: intakeCacheNote,
    }),
    statsTable(
      intakeCacheHeaders,
      p.intakeCache.steps.map(intakeCacheCells),
      'intake-cache-table',
    ),
  ];
}
