// 稼働実績の「優先度の見直し」（Issue #744。集計は server/src/intake-priority-review-store.mjs の collectPriorityReviewStats、定義は
// docs/priority-review.md『Operating Stats and Request List Display』）。client/src/ui/app.js の一部で、単独のモジュールではない
// （client/scripts/build.ts の順に app.js とつなぐ）。priorityReview の無い応答（更新前の受付）では出さない。記録の無い値は
// ゼロとは出さず「記録なし」「不明（理由）」にする。
const priorityReviewStatsNotes = [
  [
    '優先度の見直しの日次の集計',
    '監視（usageReviewHost のPC）の日次記録の優先度の段のうち、直近のものを出します。「優先」の割合は実行待ちの母集団（割り込みと利用者の指定の通常を除く）のうち「優先」の件数、利用者の変更は利用者が優先度を上げた・下げた件数、結果は直近にマージした依頼を着手時の段で分けたマージまでの時間・差し戻し・利用者の追加指示です。分からない値は「不明」と理由で示します。受付が usageReviewHost と別のPCにあるときは、そのPCの記録を読めないため「<PC> の記録を読めないため対象外」と示します（共有DBまで集めません）。',
  ],
  [
    '優先度の見直しの回',
    '条件（「優先」の割合が目安の外・利用者の変更が多い・理由の分類の偏り）のどれかに当たった日に、受付が「優先度の見直し 第N回」を始めます。開いている回がある間と、前の回の終了から7日たつまでは始めません。利用量の見直しとは別の回です。基準の調整は起票せず、受付からの質問として要対応に出ます。',
  ],
];
const priorityReviewOutcomeHeaders = [
  '着手時の段',
  '件数',
  '着手→マージ（中央値）',
  '差し戻し（1件あたり）',
  '利用者の追加指示（1件あたり）',
];
const priorityReviewRoundHeaders = [
  '回',
  '日次記録の日付',
  '状態',
  '開始の理由',
  '調査Issue',
  '自動のターン',
  '終了',
];
const priorityReviewConditionNames = {
  highRatio: '条件: 「優先」の割合が目安の外',
  userChanges: '条件: 利用者の優先度の変更',
  categoryBias: '条件: 理由の分類の偏り',
};
const priorityReviewLevels = [
  ['high', '優先'],
  ['normal', '通常'],
  ['interrupt', '割り込み'],
];
const priorityReviewNum = (v: unknown) =>
  typeof v === 'number' && Number.isFinite(v) ? v : null;
const priorityReviewCount = (v: unknown) =>
  priorityReviewNum(v) === null ? '記録なし' : `${v}件`;
const priorityReviewPercent = (r: number | undefined) =>
  priorityReviewNum(r) === null ? '—' : `${((r ?? 0) * 100).toFixed(1)}%`;
const priorityReviewTime = (iso: string | undefined) =>
  iso && Number.isFinite(Date.parse(iso))
    ? new Date(Date.parse(iso) + 9 * 3600000)
        .toISOString()
        .slice(0, 16)
        .replace('T', ' ')
    : '—';
const priorityReviewIssue = (i: DraftIssue | undefined) =>
  i
    ? `${i.desk ? `#${i.desk}（` : ''}${shownProject(i.project)} の Gitea番号 ${i.number}${i.desk ? '）' : ''}`
    : '未確定';
function priorityReviewConditionText(c?: PriorityCondition) {
  if (!c) return '記録なし';
  const result =
    c.result === true ? '該当' : c.result === false ? '非該当' : '欠測';
  return `${result}（${c.reason || '理由の記録なし'}）`;
}
function priorityReviewPopulationText(e?: PriorityPopulation) {
  if (!e) return '記録なし';
  return `${e.high ?? '?'}/${e.population ?? '?'}件（${priorityReviewPercent(e.ratio)}）。未判定・古い版の判定 ${priorityReviewCount(e.unjudged)}、母集団の外: 割り込み ${priorityReviewCount(e.interrupt)}・利用者の指定の通常 ${priorityReviewCount(e.userNormal)}`;
}
function priorityReviewChangeText(c?: PriorityChanges) {
  if (!c) return '記録なし';
  return `${priorityReviewCount(c.count)}（上げた ${priorityReviewCount(c.up)}・下げた ${priorityReviewCount(c.down)}・同じ段 ${priorityReviewCount(c.same)}）`;
}
// 直近の日次記録の優先の割合・利用者の変更・理由の偏り・条件（項目と値の2列）。
function priorityReviewDailyRows(
  d: PriorityDaily,
  names: Record<string, string>,
) {
  const pop = d.population ?? {};
  const repos = pop.repositories ? Object.entries(pop.repositories) : [];
  const ratio = d.highRatio
    ? `${priorityReviewPercent(d.highRatio.min)}〜${priorityReviewPercent(d.highRatio.max)}`
    : '記録なし';
  const changes = d.changes ?? {};
  const reasons = d.reasons ?? null;
  const categories = Object.entries(
    reasons?.categories ? reasons.categories : {},
  )
    .filter(([, n]) => (priorityReviewNum(n) ?? 0) > 0)
    .map(([k, n]) => `${names[k] || k} ${n}件`);
  const top = reasons?.top ? reasons.top : null;
  const conditions = d.conditions ?? {};
  return [
    ['「優先」の割合（全体）', priorityReviewPopulationText(pop.total)],
    ...repos.map(([repo, e]) => [
      `「優先」の割合（${shownProject(repo)}）`,
      priorityReviewPopulationText(e),
    ]),
    ['「優先」の割合の目安（priority.highRatio）', ratio],
    [
      '利用者の優先度の変更（前日）',
      priorityReviewChangeText(changes.yesterday),
    ],
    [
      `利用者の優先度の変更（直近${changes.days ?? '?'}日）`,
      priorityReviewChangeText(changes.recent),
    ],
    [
      '「優先」の理由の分類',
      reasons
        ? `${categories.length ? categories.join('、') : 'なし'}（分類の分からない「優先」${priorityReviewCount(reasons.unknown)}）`
        : '記録なし',
    ],
    [
      '理由の偏り（最多の分類）',
      top
        ? `${top.name || names[top.category] || top.category} ${top.count ?? '?'}/${reasons?.count ?? '?'}件（${priorityReviewPercent(top.ratio)}）`
        : reasons
          ? 'なし'
          : '記録なし',
    ],
    ...Object.entries(priorityReviewConditionNames).map(([key, label]) => [
      label,
      priorityReviewConditionText(conditions[key]),
    ]),
  ];
}
function priorityReviewOutcomeValue(
  value: unknown,
  reason: string | undefined,
  format: (v: number) => string,
) {
  return priorityReviewNum(value) === null
    ? `不明（${reason || '記録なし'}）`
    : format(Number(value));
}
// 優先と通常（と割り込み）の結果。着手時の段で分けた直近にマージした依頼。
function priorityReviewOutcomeRows(outcomes?: PriorityDaily['outcomes']) {
  const groups = outcomes?.groups ?? null;
  if (!groups) return [['記録なし', '—', '—', '—', '—']];
  return priorityReviewLevels.map(([level, label]) => {
    const g = groups[level];
    if (!g) return [label, '記録なし', '—', '—', '—'];
    const reasons = g.reasons ?? {};
    return [
      label,
      `${priorityReviewCount(g.count)}（利用者の指定 ${priorityReviewCount(g.userSpecified)}・今の版でない判定 ${priorityReviewCount(g.otherVersion)}）`,
      priorityReviewOutcomeValue(
        g.toMerge?.medianMs,
        reasons.toMergeMs,
        (ms: number) => `${(ms / 3600000).toFixed(1)}時間`,
      ),
      priorityReviewOutcomeValue(
        g.returns?.perJob,
        reasons.returns,
        (v: number) => v.toFixed(2),
      ),
      priorityReviewOutcomeValue(
        g.instructions?.perJob,
        reasons.instructions,
        (v: number) => v.toFixed(2),
      ),
    ];
  });
}
function priorityReviewRoundStatus(x: PriorityReviewRound) {
  if (x.status === 'closed') return x.noAction ? '終了（対策なし）' : '終了';
  if (x.autoTurnAt) return '進行中（受付AIのターン後）';
  return x.investigationIssue ? '進行中（報告待ち）' : '進行中（起票待ち）';
}
function priorityReviewRoundCells(x: PriorityReviewRound) {
  const filed = (x.filedIssues || []).map(priorityReviewIssue).join('、');
  return [
    `第${x.round}回`,
    x.date || '?',
    `${priorityReviewRoundStatus(x)}${x.held ? `。見送り ${x.held}日` : ''}`,
    (x.startReasons || []).join('、') || '記録なし',
    priorityReviewIssue(x.investigationIssue),
    priorityReviewTime(x.autoTurnAt),
    x.status === 'closed'
      ? `${priorityReviewTime(x.closedAt)}: ${x.closeReason || '理由の記録なし'}${filed ? `（起票: ${filed}）` : ''}`
      : '—',
  ];
}
function priorityReviewStatsResult(r: StatsReport) {
  if (!r.priorityReview) return [];
  const v = r.priorityReview;
  const d = v.daily ?? null;
  const rounds = Array.isArray(v.rounds) ? v.rounds : [];
  return [
    el('h2', { text: '優先度の見直し' }),
    ...(v.reason ? [el('div', { class: 'error-box', text: v.reason })] : []),
    ...(d
      ? [
          el('p', {
            class: 'stats-coverage',
            text: `直近の日次記録: ${d.date}（基準の版 ${d.version ?? '記録なし'}）${d.error ? `。${d.error}` : ''}`,
          }),
          statsTable(
            ['項目', '値'],
            priorityReviewDailyRows(d, v.categoryNames || {}),
            'priority-review-daily',
          ),
          statsTable(
            priorityReviewOutcomeHeaders,
            priorityReviewOutcomeRows(d.outcomes),
            'priority-review-outcomes',
          ),
        ]
      : v.dailyNote
        ? [el('p', { class: 'stats-coverage', text: v.dailyNote })]
        : []),
    ...(rounds.length
      ? [
          statsTable(
            priorityReviewRoundHeaders,
            [...rounds].reverse().map(priorityReviewRoundCells),
            'priority-review-rounds',
          ),
        ]
      : [
          el('p', {
            class: 'stats-coverage',
            text: '優先度の見直しの回はまだありません。',
          }),
        ]),
  ];
}
