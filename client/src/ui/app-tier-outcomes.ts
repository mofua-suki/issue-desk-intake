// 稼働実績の「相談の難易度と結末」（Issue #433。Issue #570 で client/src/ui/app-stats.js から分けた）。相談＋実装の合計の列と、前後の
// 期間ごとの通った割合・安く済んで通った割合の表（Issue #570）。client/src/ui/app.js の一部で、単独のモジュールではない。受付は
// client/scripts/build.ts の順に app.js とつないだ1つのモジュールとして /app.js を配信する。値は server/src/usage-outcomes.mjs、
// 定義の注記は client/src/ui/request-list.js の tierOutcomeNotes。
// 難易度の表示名は server/src/intake-tiers.mjs の tierNames と同じ。
const tierRateLevelLabels: Record<string, string> = {
  light: '低',
  standard: '中',
  heavy: '高',
  fixed: '固定値',
};
// 相談の難易度と結末（Issue #433）。tiers の無い応答（更新前の受付）では出さない。実装の難易度（Issue #489）の列は、
// 行の implementation がある応答だけに足す。相談＋実装の合計の列（Issue #570）は行に combined の
// 項目がある応答だけ、割合の表は tiers.rates がある応答だけに足す。
function tiersResult(r: StatsReport) {
  if (!r.tiers) return [];
  const rows = r.tiers.rows || [];
  const implementation = implementationTierColumn(rows);
  const combined = rows.some(
    (row: object) => row && Object.hasOwn(row, 'combined'),
  );
  return [
    el('h2', { text: '相談の難易度と結末' }),
    ...(r.tiers.unavailable || []).map((text) =>
      el('div', { class: 'error-box', text }),
    ),
    el('p', {
      class: 'stats-coverage',
      text: rows.length
        ? `難易度の記録がある相談から起票したIssue ${r.tiers.total}件${r.tiers.omitted ? `（新しい順に ${rows.length}件を表示。ほかに ${r.tiers.omitted}件）` : ''}。結末は今の状態で分けます（過去の状態の履歴はありません）。`
        : '難易度の記録がある相談から起票したIssueはありません。',
    }),
    ...(rows.length
      ? [
          statsTable(
            [
              ...tierOutcomeHeaders,
              ...(combined ? [tierCombinedHeader] : []),
              ...(implementation ? [implementationTierHeader] : []),
            ],
            rows.map((row) => [
              ...tierOutcomeCells(row, repositoryProjects()),
              ...(combined ? [tierCombinedCell(row)] : []),
              ...(implementation
                ? [
                    implementationTierCell(
                      row.implementation,
                      row.implementationDecision,
                    ),
                  ]
                : []),
            ]),
            'tier-outcomes',
          ),
        ]
      : []),
    ...tierRatesResult(r.tiers),
  ];
}

const tierCombinedHeader = '相談＋実装の合計';
const tierRateNumber = (v: number) => Math.round(v).toLocaleString('ja-JP');
// 1つの項目の値。値の無い項目は「不明（N件）」、一部だけ不明なら合計に添える（ゼロとは表示しない）。
function tierCombinedValue(
  c: UsageTotal,
  key: MetricKey,
  show: (v: number) => string,
) {
  const unknown = c.unknown?.[key] ?? 0;
  if (typeof c[key] !== 'number') return `不明（${unknown}件）`;
  return `${show(c[key])}${unknown ? `（ほかに不明 ${unknown}件）` : ''}`;
}
// 相談＋実装の合計の欄。1行目は件数・費用の目安・入力・出力・キャッシュ読み・キャッシュ書き、2行目は監視側の工程別の件数。
// 記録が無ければ「記録なし」。相談の記録が無い行はその旨、同じ相談から複数のIssueを起票した行（分割起票・調査Issue）は
// 相談の分をそのIssueの数で共有していること（各行に同じ相談の分を含む）を添える。
function tierCombinedCell(row: TierOutcomeRow) {
  const c = row?.combined;
  if (!c || typeof c !== 'object')
    return '記録なし（このPCに使用量の記録がありません）';
  const usd = (v: number) => `$${v.toFixed(v >= 1 ? 2 : 4)}`;
  const head = [
    `${c.count}件`,
    `費用の目安 ${tierCombinedValue(c, 'costUsd', usd)}`,
    `入力 ${tierCombinedValue(c, 'inputTokens', tierRateNumber)}`,
    `出力 ${tierCombinedValue(c, 'outputTokens', tierRateNumber)}`,
    `キャッシュ読み ${tierCombinedValue(c, 'cacheReadTokens', tierRateNumber)}`,
    `キャッシュ書き ${tierCombinedValue(c, 'cacheWriteTokens', tierRateNumber)}`,
  ].join('、');
  const steps = Array.isArray(row.issueUsage?.steps)
    ? row.issueUsage.steps
        .map((s) => `${s.step || '工程不明'} ${s.count}件`)
        .join('・')
    : '';
  return [
    `${head}${
      !c.includesConsultation
        ? '（相談の記録なし）'
        : c.consultationShared > 1
          ? `（相談の分は同じ相談から起票した${c.consultationShared}件のIssueに共通）`
          : ''
    }`,
    steps ? `実装側: ${steps}` : '実装側: 記録なし',
  ].join('\n');
}

const tierRateHeaders = [
  '分け方',
  '区分',
  '件数',
  'そのまま通った',
  '安く済んで通った',
  '判定量なし',
];
const tierRatePercent = (v: number) =>
  typeof v === 'number' ? `${(v * 100).toFixed(1)}%` : '—';
function tierRateCells(kind: string, label: string, g: TierRateGroup) {
  return [
    kind,
    label,
    `${g.count}件`,
    g.count
      ? `${g.passed}件（${tierRatePercent(g.passedRate)}）${g.outcomeUnknown ? `（状態不明 ${g.outcomeUnknown}件を含む母数）` : ''}`
      : '—',
    g.judged
      ? `${g.cheapPassed}件 / ${g.judged}件（${tierRatePercent(g.cheapRate)}）`
      : '—（判定量のあるIssueなし）',
    `${g.unknown}件`,
  ];
}
// 期間の中央値の1行（判定量ごと。値の無い判定量は「記録なし」）。
function tierRateMedianText(p: TierRatePeriod) {
  const m = p.medians || {};
  return `中央値: 費用の目安（すべて Claude のIssue）${typeof m.cost === 'number' ? `$${m.cost.toFixed(m.cost >= 1 ? 2 : 4)}` : '記録なし'}、入力＋出力トークン（Codex を含むIssue）${typeof m.tokens === 'number' ? tierRateNumber(m.tokens) : '記録なし'}`;
}
// 前後の期間ごとの通った割合の表（Issue #570）。rates の無い応答（更新前の受付・難易度の記録を持つ相談が無い）では出さない。
function tierRatesResult(tiers: TierReport | undefined) {
  const rates = tiers?.rates;
  if (!rates) return [];
  const periods: [string, TierRatePeriod | undefined][] = [
    ['前', rates.before],
    ['後', rates.after],
  ];
  return periods.flatMap(([label, p]) =>
    p
      ? [
          el('h3', {
            text: `${label}の期間の通った割合（相談の日時 ${p.fromDate}〜${p.toDate}）`,
          }),
          el('p', { class: 'stats-coverage', text: tierRateMedianText(p) }),
          statsTable(
            tierRateHeaders,
            [
              tierRateCells('全体', '—', p.total),
              ...(p.levels || []).map((g) =>
                tierRateCells(
                  '相談の難易度（出発点）',
                  tierRateLevelLabels[g.level ?? ''] || g.level || '記録なし',
                  g,
                ),
              ),
              ...(p.consultModels || []).map((g) =>
                tierRateCells('相談のモデル', g.model || '記録なし', g),
              ),
              ...(p.implementations || []).map((g) =>
                tierRateCells(
                  '実装の実行先×モデル',
                  g.provider || g.model
                    ? `${g.provider || '不明'} / ${g.model || '不明'}`
                    : '記録なし',
                  g,
                ),
              ),
            ],
            'tier-rates',
          ),
        ]
      : [],
  );
}
