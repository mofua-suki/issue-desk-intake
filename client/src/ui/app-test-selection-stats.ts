// 稼働実績の「テストの選び方の前後比較」（Issue #791。集計は server/src/test-selection-stats.mjs、定義は docs/test-selection.md の
// 「稼働実績での前後比較（3/3）」）。client/src/ui/app.js の一部で、単独のモジュールではない（client/scripts/build.ts の順に app.js とつなぐ）。
// testSelection の無い応答（更新前の受付）では出さない。記録の無い欄は、ゼロとせず「記録なし（理由）」。
const selectionStatsNone = (x: { reason?: string } | undefined) =>
  `記録なし（${x?.reason || '理由不明'}）`;
const selectionStatsSince = (x: { since?: string }) =>
  x?.since ? `（記録の始まり ${x.since} 以後）` : '';
const selectionStatsMinutes = (ms: number) =>
  typeof ms === 'number' ? `${(ms / 60000).toFixed(1)}分` : '—';
const selectionStatsPercent = (v: number) =>
  typeof v === 'number'
    ? `${(v * 100).toFixed(1)}%`
    : '記録なし（選んで流した回がありません）';
// 件数・平均・中央値。件数が0なら平均と中央値は出さない（0分とは出さない）。
const selectionStatsTook = (d: SelectionDuration) =>
  d?.count
    ? `${d.count}件・平均 ${selectionStatsMinutes(d.averageMs)}・中央値 ${selectionStatsMinutes(d.medianMs)}`
    : '0件';
// section が記録なしなら理由、あれば value(section)。
const selectionStatsCell = <T extends { reason?: string }>(
  x: T | undefined,
  value: (x: T) => string,
) => (!x || x.reason ? selectionStatsNone(x) : value(x));
function selectionStatsFixed(f: SelectionFixed) {
  const missing = [
    ...(f.missing ? [`欠測 ${f.missing}件`] : []),
    ...(f.running ? [`実行中 ${f.running}件`] : []),
  ];
  return `${selectionStatsTook(f)}${missing.length ? `（${missing.join('・')}）` : ''}${selectionStatsSince(f)}`;
}
function selectionStatsLeaks(l: SelectionLeaks) {
  const missing = [
    ...(l.unknownSelections
      ? [`選び方の記録の無いマージ ${l.unknownSelections}件`]
      : []),
    ...(l.unknownFiles
      ? [`失敗したファイルの分からない回 ${l.unknownFiles}回`]
      : []),
    ...(l.cutRuns ? [`失敗したファイルが上限で切れた回 ${l.cutRuns}回`] : []),
    ...(l.noPrevious ? [`前の全部の実行が記録に無い回 ${l.noPrevious}回`] : []),
  ];
  return `${l.count}件（失敗した全部の実行 ${l.failedRuns}回・その間のマージ ${l.merged}件）${missing.length ? `。欠測: ${missing.join('・')}` : ''}${selectionStatsSince(l)}`;
}
// 理由の行（全部を流した理由・取り込み前の合格を使えなかった理由）。前後の期間の理由を合わせ、片方に無い理由は0回
// （その期間が記録なしなら記録なし）。
function selectionStatsReasonRows(
  periods: [string, SelectionPeriod][],
  section: 'selection' | 'followReuse' = 'selection',
  label = '全部を流した理由',
) {
  const reasons: string[] = [];
  for (const [, p] of periods)
    for (const r of p[section]?.reasons || [])
      if (!reasons.includes(r.reason)) reasons.push(r.reason);
  return reasons.map((reason) => [
    `${label}: ${reason}`,
    ...periods.map(([, p]) =>
      selectionStatsCell(
        p[section],
        (s) => `${s.reasons.find((r) => r.reason === reason)?.count ?? 0}回`,
      ),
    ),
  ]);
}
function testSelectionStatsResult(r: StatsReport) {
  if (!r.testSelection) return [];
  const s = r.testSelection;
  const title = el('h2', { text: 'テストの選び方の前後比較' });
  if (!s.before || !s.after)
    return [
      title,
      el('div', {
        class: 'error-box',
        text: s.reason || '集計できませんでした（理由不明）',
      }),
    ];
  const periods: [string, SelectionPeriod][] = [
    ['前', s.before],
    ['後', s.after],
  ];
  const headers = [
    '項目',
    ...periods.map(([label, p]) => `${label}（${p.fromDate}〜${p.toDate}）`),
  ];
  const row = <
    K extends 'fixed' | 'selection' | 'followReuse' | 'full' | 'leaks',
  >(
    label: string,
    section: K,
    value: (x: NonNullable<SelectionPeriod[K]>) => string,
  ) => [
    label,
    ...periods.map(([, p]) =>
      selectionStatsCell(
        p[section] as NonNullable<SelectionPeriod[K]> | undefined,
        value,
      ),
    ),
  ];
  const rows = [
    row('固定テストの工程（件数・平均・中央値）', 'fixed', selectionStatsFixed),
    row('うち選んだテストだけを流した回', 'fixed', (f) =>
      selectionStatsTook(f.chosen),
    ),
    row('うち全部を流した回', 'fixed', (f) => selectionStatsTook(f.whole)),
    row('うち選び方の記録が無い回', 'fixed', (f) => `${f.unlabeled}件`),
    row(
      '選んで流した回数',
      'selection',
      (x) => `${x.chosen}回${selectionStatsSince(x)}`,
    ),
    row('全部を流した回数', 'selection', (x) => `${x.whole}回`),
    row(
      '選び方を記録したが流さなかった回数',
      'selection',
      (x) => `${x.notRun ?? 0}回`,
    ),
    ...selectionStatsReasonRows(periods),
    row(
      '選んだファイルの割合の平均（選んで流した回）',
      'selection',
      (x: { selectedShare: number }) => selectionStatsPercent(x.selectedShare),
    ),
    row(
      '流したファイルの割合の平均（全部は100%）',
      'selection',
      (x: { runShare: number }) => selectionStatsPercent(x.runShare),
    ),
    row(
      '追従後に取り込み前の合格を使った回数',
      'followReuse',
      (x) =>
        `${x.reused + x.rechecked}回（全部を省いた回 ${x.reused}回・一覧で読むテストだけ流した回 ${x.rechecked}回）${selectionStatsSince(x)}`,
    ),
    row(
      '追従後に取り込み前の合格を使えなかった回数',
      'followReuse',
      (x) => `${x.notReused}回`,
    ),
    ...selectionStatsReasonRows(
      periods,
      'followReuse',
      '取り込み前の合格を使えなかった理由',
    ),
    row(
      '取り込み前の合格で省いた時間の見込み',
      'followReuse',
      (x: { savedMs: number }) => selectionStatsMinutes(x.savedMs),
    ),
    row(
      'main の全部のテストの実行',
      'full',
      (f) =>
        `${f.runs}回（合格 ${f.passed}回・失敗 ${f.failed}回）${f.interrupted || f.unrunnable ? `。ほかに中断 ${f.interrupted}回・実行できず ${f.unrunnable}回` : ''}${selectionStatsSince(f)}`,
    ),
    row(
      '全部のテストで失敗したファイル',
      'full',
      (f) =>
        `${f.failedFiles}件${f.cutFiles ? `（ほかに上限で切れた ${f.cutFiles}件）` : ''}${f.unknownFiles ? `（ほかに失敗したファイルの分からない回 ${f.unknownFiles}回）` : ''}`,
    ),
    row('漏れの件数', 'leaks', selectionStatsLeaks),
  ];
  const items = periods.flatMap(([label, p]) =>
    (p.leaks?.items || []).map(
      (item): [string, SelectionLeaks | undefined, SelectionLeakItem] => [
        label,
        p.leaks,
        item,
      ],
    ),
  );
  return [
    title,
    statsTable(headers, rows, 'test-selection-stats'),
    el('p', {
      class: 'stats-coverage',
      text: '漏れは、main の全部のテストで失敗したテストファイルのうち、前の全部の実行からその実行までにマージしたIssueの固定テストで選び方が省いていたものの件数です（定義は下の注記と docs/test-selection.md）。このPCの runner.log だけを読みます。',
    }),
    ...(items.length
      ? [
          el(
            'ul',
            { class: 'stats-coverage test-selection-leaks' },
            ...items.map(([label, , item]) =>
              el('li', {
                text: `${label}: ${item.version} の ${item.file}（省いたIssue: ${item.issues.map(reviewIssue).join('、')}）`,
              }),
            ),
            ...periods
              .filter(([, p]) => p.leaks?.omitted)
              .map(([label, p]) =>
                el('li', { text: `${label}: ほかに ${p.leaks?.omitted}件` }),
              ),
          ),
        ]
      : []),
    selectionStatsNotesList(),
  ];
}
// 定義の注記。testSelection のある応答の節の中だけに出す（画面末尾の「集計の定義」には入れない。項目の無い応答では出さない）。
const selectionStatsNotesList = () =>
  el(
    'dl',
    { class: 'stats-coverage test-selection-notes' },
    ...testSelectionStatsNotes.flatMap(([term, text]) => [
      el('dt', { text: term }),
      el('dd', { text }),
    ]),
  );
const testSelectionStatsNotes = [
  [
    'テストの選び方の前後比較（固定テストの工程）',
    'このPCの runner.log の「固定テスト」の工程（開始と終了・中断の組）を開始の時刻で期間へ振り分け、件数・平均・中央値を出します。直前の「テスト選択」の行で、選んだテストだけを流した回と全部を流した回に分けます。終わりの分からない工程は欠測、選び方の行の無い工程（導入前・ほかのリポジトリ）は「選び方の記録が無い回」として件数で示します。',
  ],
  [
    'テストの選び方の前後比較（選んだ割合と全部を流した理由）',
    '「テスト選択」の行を行の時刻で数えます。選んだファイルの割合は選んだファイル数÷テストファイル数の平均で、全部を流した回を100%として含めた平均も出します。全部を流した理由はファイル名などの細目を除いた種類で数えます。行の無い期間（選び方の導入前など）は「記録なし」です。追従後の「固定テスト省略」の行は、取り込み前の合格を使った回（全部を省いた回・一覧で読むテストだけ流した回）と使えなかった回（理由は細目を除いた種類）を数え、省いた時間の見込みは使った回の行の見込みの秒数の合計です（一覧で読むテストだけ流した回は、流した時間を引いた見込み）。',
  ],
  [
    'テストの選び方の前後比較（main の全部のテストと漏れ）',
    '「全部のテスト」の結果の行（合格・失敗・中断・実行できず）を行の時刻で数えます。漏れは、失敗した回の失敗したテストファイルのうち、前の全部の実行（合格・失敗で終わった回）の開始からこの回の開始までにマージ完了の行のあるIssueの、マージより前の最後の「テスト選択」の行で省いていたファイルの件数です。選び方の行の無いマージ・失敗したファイルの分からない回・前の全部の実行が記録に無い回は欠測として件数で示します。',
  ],
];
