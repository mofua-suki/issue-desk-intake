// 稼働実績の「変更範囲の宣言の実績」（Issue #771。集計は server/src/scope-stats.mjs、定義は docs/parallel-implementation.md の
// 「変更範囲の宣言の実績」）。client/src/ui/app.js の一部で、単独のモジュールではない（client/scripts/build.ts の順に app.js とつなぐ）。
// scope の無い応答（更新前の受付）では出さない。記録が無い・読めない・期間内に1件も無いときは、ゼロとせず「記録なし（理由）」。
const scopeStateOrder = ['file', 'directory', 'absent', 'unknown', 'none'];
const scopeNone = (x: { reason?: string } | undefined) =>
  `記録なし（${x?.reason || '理由不明'}）`;
const scopeSince = (x: { since?: string }) =>
  x.since ? `（記録の始まり ${x.since} 以後）` : '';
function scopeFiledCell(p: ScopePeriod, states: 'total' | string[]) {
  const f = p.filed;
  if (!f || f.reason) return scopeNone(f);
  if (states === 'total') return `${f.total}件${scopeSince(f)}`;
  if (states.length === 1) return `${f.states[states[0]] ?? 0}件`;
  const [a, u] = states.map((s: string | number) => f.states[s] ?? 0);
  return `${a + u}件（宣言なし ${a}件・不明 ${u}件）`;
}
function scopeCheckCell(x: ScopeCheck | undefined) {
  if (!x || x.reason) return scopeNone(x);
  return `照合 ${x.checks}件・宣言外の変更のあった照合 ${x.withOutside}件（宣言外のファイル 計${x.outside}件）${x.unreadable ? `・実変更を読めなかった照合 ${x.unreadable}件` : ''}${scopeSince(x)}`;
}
// 経路・理由ごとの内訳。前後の期間の行を合わせ、片方に無い行は0件（その期間が記録なしなら記録なし）。
function scopeGroupRows(periods: [string, ScopePeriod][]) {
  const groups = new Map<string, ScopeGroup & { counts: number[] }>();
  for (const [i, [, p]] of periods.entries())
    for (const g of p.filed?.groups || []) {
      const key = JSON.stringify([g.state, g.route, g.reason]);
      const row = groups.get(key) ?? { ...g, counts: [0, 0] };
      row.counts[i] = g.count;
      groups.set(key, row);
    }
  return [...groups.values()]
    .sort(
      (a, b) =>
        scopeStateOrder.indexOf(a.state) - scopeStateOrder.indexOf(b.state) ||
        b.counts[0] + b.counts[1] - (a.counts[0] + a.counts[1]),
    )
    .map((g) => [
      g.stateName || g.state,
      g.routeName || g.route,
      g.reason || '—',
      ...periods.map(([, p], i: number) =>
        !p.filed || p.filed.reason ? '記録なし' : `${g.counts[i]}件`,
      ),
    ]);
}
function scopeStatsResult(r: StatsReport) {
  if (!r.scope) return [];
  const s = r.scope;
  const periods: [string, ScopePeriod][] = [
    ['前', s.before],
    ['後', s.after],
  ];
  const headers = [
    '項目',
    ...periods.map(([label, p]) => `${label}（${p.fromDate}〜${p.toDate}）`),
  ];
  const filedDefs: [string, 'total' | string[]][] = [
    ['ファイル単位の宣言で起票', ['file']],
    ['ディレクトリを含む宣言で起票', ['directory']],
    ['宣言なし・不明で起票', ['absent', 'unknown']],
    ['変更なし（調査Issue）で起票', ['none']],
    ['起票の合計', 'total'],
  ];
  const filedRows = filedDefs.map(([label, states]) => [
    label,
    ...periods.map(([, p]) => scopeFiledCell(p, states)),
  ]);
  const groupRows = scopeGroupRows(periods);
  const checkRows = [
    ['通常の宣言', 'normal'],
    ['後から付けた宣言', 'backfilled'],
  ].map(([label, key]) => [
    label,
    ...periods.map(([, p]) => scopeCheckCell(p.checks?.[key])),
  ]);
  checkRows.push([
    '宣言なし・不明（比べる範囲が無く記録のみ）',
    ...periods.map(([, p]) => {
      const x = p.checks?.undeclared;
      return !x || x.reason
        ? scopeNone(x)
        : `照合 ${x.checks}件${scopeSince(x)}`;
    }),
  ]);
  const skipped = s.skipped || {};
  return [
    el('h2', { text: '変更範囲の宣言の実績' }),
    ...(skipped.filed
      ? [
          el('p', {
            class: 'stats-coverage',
            text: `読めなかった記録（起票 ${skipped.filed || 0}件）は数えていません。`,
          }),
        ]
      : []),
    el('h3', { text: '起票した依頼の宣言' }),
    statsTable(headers, filedRows, 'scope-filed'),
    el('h3', { text: '経路・理由ごとの内訳' }),
    ...(groupRows.length
      ? [
          statsTable(
            ['宣言', '経路', '理由', ...headers.slice(1)],
            groupRows,
            'scope-groups',
          ),
        ]
      : [
          el('p', {
            class: 'stats-coverage',
            text: '内訳に出す起票の記録はありません。',
          }),
        ]),
    el('h3', { text: '実装の終了時の照合（宣言外の変更）' }),
    statsTable(headers, checkRows, 'scope-checks'),
  ];
}
const scopeStatsNotes = [
  [
    '変更範囲の宣言で起票した件数',
    '受付が起票した依頼ごとに記録した変更範囲の宣言の種類（ファイル単位・ディレクトリを含む・宣言なし・不明・変更なし）を、起票の時刻で前後の期間へ振り分けて数えます。内訳は宣言の種類・経路（通常の相談・分割の案・導入前に作られた下書き・後続Issueの案など）・理由ごとの件数です。記録が無い・読めない・期間が記録の始まりより前・期間内に起票が無いときは「記録なし」と理由を出し、ゼロとは出しません。',
  ],
  [
    '実装の終了時の照合（宣言外の変更）',
    'このPCの runner.log の「範囲の照合」の行（実装の終了時。公開工程の追従での変更の後の行は除く）を、照合した宣言が後から付けたものか通常のものかで分けて数えます。宣言外の変更のあった照合の件数と、宣言外のファイルの合計、実変更を読めなかった照合の件数です。他のPCで実装した依頼は含みません。',
  ],
];
