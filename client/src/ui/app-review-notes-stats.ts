// 稼働実績の「実装の注意点の効き目」（Issue #650。集計は server/src/review-notes-stats.mjs、定義は docs/review-notes.md の
// 「効き目の比較（稼働実績）」）。client/src/ui/app.js の一部で、単独のモジュールではない（client/scripts/build.ts の順に app.js と
// つなぐ）。reviewNotes の無い応答（更新前の受付）では、表も定義の注記も出さない（定義は節の中に置き、画面末尾の
// 「集計の定義」には入れない）。値の無い項目はゼロと出さず、件数で示す。
const reviewNotesStatsNotes = [
  [
    '注意点の効き目: 差し戻しの割合',
    'このPCの runner.log で、期間内にレビューの結末の行（「レビュー保留 … 自動差し戻し N/2」・人の確認待ち・削除の承認待ち・マージ完了）があるPRをレビューしたPRとします。ほかのPCで実装したPR（「引き渡しを受け取りました」の行があるPR）は、そのPCの使用量・注意点の添付・自動差し戻しがこのPCの記録に無いので、件数だけを示して以下の値に含めません。このPCで実装したPRのうち、期間内に自動差し戻しの行が1回以上あるPRの件数と割合、差し戻しの行の数（回数）を示します。記録の始まりより前の期間は「記録なし」です。',
  ],
  [
    '注意点の効き目: 1件あたりの使用量と時間',
    '使用量は、このPCで実装したPRごとに、期間内の実装・競合解消・独立レビューの使用量の行（runtime/usage/。使用量の表と同じ費用の補正を通したもの）を合計した平均です。1行でも値の無いPR（補正できない行、公開単価で換算できない Codex の行など）は「不明」、実装の行の無いPR（実装が期間の前など）は「実装の記録なし」、行の無いPRは「記録なし」に数え、どれも平均に入れません。トークンは入力の総量（Claude はキャッシュの読み書きを含む）と出力の合計です。時間は、runner.log の実装・追従・レビューの工程の所要の合計の平均で、終わりの分からない工程のあるPRは「欠測」です。',
  ],
  [
    '注意点の効き目: 入力の増加と差し引き',
    '入力の増加は、期間内の「注意点を添付」の行の bytes の合計と、それを3 bytes ≒ 1トークンとした概算です（実装の実行ごとに1回数えます）。差し引きは、このPCで実装したPRで、減った差し戻しの回数（前の期間の1件あたりの差し戻しの回数×後の期間のPR−後の期間の回数）×差し戻し1回あたりの平均トークン（前後の期間の差し戻しの実行の平均）から、後の期間の入力の増加を引いた値です。正なら減った分が入力の増加を上回ります。読めない添付の行・トークン数の不明な差し戻しの実行・読めなかった使用量の行があるときは計算せず、理由を示します。注意点を削る・やめる判断は自動では行いません。',
  ],
];
const reviewNotesCost = (v: number) =>
  `$${v.toFixed(Math.abs(v) >= 1 ? 2 : 4)}`;
const reviewNotesCount = (v: number) => Math.round(v).toLocaleString('ja-JP');
// 平均に入れなかった件数の書き方（[名前, 件数] の並び。0件は書かない）。
const reviewNotesRest = (pairs: [string, number | undefined][]) =>
  pairs.filter(([, n]) => n).map(([label, n]) => `${label} ${n}件`);
// 平均の値（unitAverage の結果）。値が無ければ「不明」で、ゼロとは出さない。u は PRの使用量（記録なしの件数）。
function reviewNotesAverageText(
  a: AverageMetric,
  format: (v: number) => string,
  u: Pick<ReviewNotesUsage, 'noImplementation' | 'noRecord'> = {},
) {
  const rest = reviewNotesRest([
    ['不明', a.unknown],
    ['実装の記録なし', u.noImplementation],
    ['記録なし', u.noRecord],
  ]);
  if (a.average === null)
    return rest.length ? `不明（${rest.join('・')}）` : '—（対象なし）';
  return `${format(a.average)}（${a.count}件の平均${rest.length ? `。ほかに${rest.join('・')}` : ''}）`;
}
function reviewNotesTimeText(t: NonNullable<ReviewNotesPeriod['time']>) {
  const rest = reviewNotesRest([
    ['欠測', t.missing],
    ['実装の記録なし', t.noImplementation],
    ['記録なし', t.noRecord],
  ]);
  if (t.averageMs === null)
    return rest.length ? `不明（${rest.join('・')}）` : '—（対象なし）';
  return `${(t.averageMs / 60000).toFixed(1)}分（${t.count}件の平均${rest.length ? `。ほかに${rest.join('・')}` : ''}）`;
}
// 前後の比較の表の行（項目、前、後）。p が null・reason のある期間は記録なし。
function reviewNotesStatsRows(periods: ReviewNotesPeriod[]) {
  const cell = (
    p: ReviewNotesPeriod,
    value: (p: ReviewNotesPeriod) => string,
  ) => {
    if (!p) return '記録なし';
    if (p.reason) return `記録なし（${p.reason}）`;
    return value(p);
  };
  const usage =
    (value: (u: ReviewNotesUsage, p: ReviewNotesPeriod) => string) =>
    (p: ReviewNotesPeriod) =>
      p.usage ? value(p.usage, p) : '記録なし（使用量を読めませんでした）';
  const row = (label: string, value: (p: ReviewNotesPeriod) => string) => [
    label,
    ...periods.map((p) => cell(p, value)),
  ];
  return [
    row(
      'レビューしたPR',
      (p) =>
        `${p.reviewed}件${p.otherHost ? `（うち他のPCで実装 ${p.otherHost}件は以下の値に含めません）` : ''}${p.since ? `（記録の始まり ${p.since} 以後）` : ''}`,
    ),
    row('差し戻したPR（割合）', (p) =>
      p.local
        ? `${p.returnedPrs}件（${(p.rate * 100).toFixed(1)}%）`
        : '—（このPCで実装したPRなし）',
    ),
    row('差し戻しの回数', (p) => `${p.returns}回`),
    row(
      '1件あたりの費用の目安',
      usage((u) => reviewNotesAverageText(u.costUsd, reviewNotesCost, u)),
    ),
    row(
      '1件あたりのトークン',
      usage((u) => reviewNotesAverageText(u.tokens, reviewNotesCount, u)),
    ),
    row(
      '補正できない費用の行',
      usage((u) => `${u.uncorrectable}件`),
    ),
    row('1件あたりの時間', (p) =>
      p.time ? reviewNotesTimeText(p.time) : '記録なし',
    ),
    row(
      '注意点の入力の増加',
      (p) =>
        `${reviewNotesCount(p.notes.bytes)} bytes（約 ${reviewNotesCount(p.notes.tokens)} トークン、${p.notes.lines}回）${p.notes.unreadable ? `。ほかに読めない行 ${p.notes.unreadable}件` : ''}`,
    ),
  ];
}
function reviewNotesNetText(n: ReviewNotesReport['net']) {
  if (!n) return '差し引き: 計算できません（集計できませんでした）';
  if (n.tokens === null) return `差し引き: 計算できません（${n.reason}）`;
  const per =
    n.perReturnTokens === null ? '—' : reviewNotesCount(n.perReturnTokens);
  return `差し引き: 約 ${reviewNotesCount(n.tokens)} トークン（減った差し戻し ${n.reducedReturns.toFixed(1)}回 × 1回あたり ${per} = ${reviewNotesCount(n.savedTokens)}、注意点の入力の増加 ${reviewNotesCount(n.noteTokens)}）。${n.tokens >= 0 ? '減った分が入力の増加を上回っています。' : '入力の増加が減った分を上回っています（元が取れていません）。'}`;
}
function reviewNotesStatsResult(r: StatsReport) {
  if (!r.reviewNotes) return [];
  const x = r.reviewNotes;
  const title = el('h2', { text: '実装の注意点の効き目（前後の比較）' });
  const errors = (x.unavailable || []).map((text) =>
    el('div', { class: 'error-box', text }),
  );
  const notes = el(
    'dl',
    { class: 'stats-coverage review-notes-notes' },
    ...reviewNotesStatsNotes.flatMap(([term, text]) => [
      el('dt', { text: term }),
      el('dd', { text }),
    ]),
  );
  if (!x.before || !x.after) return [title, ...errors, notes];
  const periods: [string, ReviewNotesPeriod][] = [
    ['前', x.before],
    ['後', x.after],
  ];
  const back = x.perReturn;
  return [
    title,
    ...errors,
    statsTable(
      [
        '項目',
        ...periods.map(
          ([label, p]) => `${label}（${p.fromDate}〜${p.toDate}）`,
        ),
      ],
      reviewNotesStatsRows(periods.map(([, p]) => p)),
      'review-notes-stats',
    ),
    el('p', {
      class: 'stats-coverage',
      text: back
        ? `差し戻し1回あたり（前後の期間の差し戻しの実行 ${back.returns}件）: トークン ${reviewNotesAverageText(back.tokens, reviewNotesCount)}、費用の目安 ${reviewNotesAverageText(back.costUsd, reviewNotesCost)}`
        : '差し戻し1回あたり: 記録なし（使用量を読めませんでした）',
    }),
    el('p', {
      class: 'stats-coverage review-notes-net',
      text: reviewNotesNetText(x.net),
    }),
    ...(x.skipped
      ? [
          el('p', {
            class: 'stats-coverage',
            text: `使用量の記録のうち読めなかった行 ${x.skipped}件は数えていません。`,
          }),
        ]
      : []),
    notes,
  ];
}
