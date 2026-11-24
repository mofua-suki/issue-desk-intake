// 稼働実績の「止まりと全PCの同時実装」（Issue #849。集計は server/src/stall-stats.mjs、定義は docs/parallel-implementation.md の
// 「止まりと全PCの同時実装の実績（Issue #849）」）。client/src/ui/app.js の一部で、単独のモジュールではない（client/scripts/build.ts の
// 順に app.js とつなぐ）。stalls の無い応答（更新前の受付）では出さない。記録の無い期間は「記録なし（理由）」で、ゼロとは
// 出さない。
const stallStatsKindNames = [
  ['idle_host', '実装しない実装PC'],
  ['stuck_claim', '進まない占有'],
  ['repeated_failure', '同じ失敗の繰り返し'],
  ['scope_blocked', '範囲の重なりで取れない実行待ち'],
];
const stallStatsNotes = [
  [
    '止まりの実績の対象',
    'このPC（受付と同じPC）の監視の runner.log の、ホームの監視が残す止まりの検知・解消と外す印の行です。このPCの監視がホームのリポジトリの止まりだけを数え、ほかのPCがホームのリポジトリ（NAS がホームのリポジトリなど）の止まりは含みません。',
  ],
  [
    '止まりの件数・時間',
    '件数は検知の時刻が期間内の止まりで、種類別と、そのうち不具合の疑いの件数を出します。止まっていた時間は、止まりごとの検知から解消までのうち期間内の記録のある区間の部分の合計で、未解消の止まりは今まで（監視が止まっていれば最後に動いていた時刻まで）数えます。同時に複数の止まりがあれば重ねて数えます。',
  ],
  [
    '自動で外した占有',
    'ホームが外す印を付けた占有（進まない占有・同じ失敗を繰り返す占有）のうち、持ち主が外した、または応えずに期限切れと同じ扱いで外れたものの件数です（ホームが次に確かめた記録から消えていたときの「外す印の付いた占有が外れました」の行。実装済みへ移ったものは数えません）。「外す印を付けた占有」は印を付けた件数で、まだ外れていないものも含みます。',
  ],
  [
    '出した依頼',
    '受付が不具合と見られる止まりについて起票した直す依頼の件数です（受付の記録。起票した時刻で数えます）。claim.coordination が未設定の受付では記録なしです。',
  ],
  [
    '全PCの同時実装の割合',
    '受付の実行記録（job_events）から、有効なleaseを持ち、running状態かつ実装段階にある依頼をPC別に数えます。PR作成中・公開中などは含みません。分子は2台以上のPCが同時に実装していた時間、分母は1台以上のPCが実装していた時間です。件数を復元できない時間は記録の無い時間として除きます。プロジェクトを選んでも全リポジトリの値です。',
  ],
  [
    '1日のマージ完了件数',
    'このPCの監視が残したマージ完了の行（このPCがマージした依頼）のうち記録のある区間の件数を、記録のある時間の日数で割った値です。ほかのPCがホームのリポジトリのマージは含みません。',
  ],
  [
    '止まりの実績の記録なし',
    '記録の有無は区間ごとに決めます。記録のある区間は、受付の実行記録から件数を復元できる区間と、このPCの監視のログがある稼働（起動からその稼働の最後の行まで）の重なりだけです。どちらかの記録が無い間は、必要な記録が入る前の古い期間も含め「記録の無い時間」として値に入れません。期間の全体に記録が無ければ、値をゼロにせず「記録なし」と理由を出します。',
  ],
];
function stallStatsDuration(ms: unknown) {
  if (typeof ms !== 'number' || !Number.isFinite(ms) || ms < 0)
    return '記録なし';
  const minutes = Math.round(ms / 60000);
  if (!minutes) return ms ? '1分未満' : '0分';
  if (minutes < 60) return `${minutes}分`;
  const hours = Math.floor(minutes / 60);
  return minutes % 60 ? `${hours}時間${minutes % 60}分` : `${hours}時間`;
}
const stallStatsTime = (iso: string) =>
  new Date(Date.parse(iso) + 9 * 3600000)
    .toISOString()
    .slice(0, 16)
    .replace('T', ' ');
const stallStatsRatio = (p: {
  parallel: { ratio: number | null; measuredMs: number };
}) =>
  typeof p.parallel.ratio === 'number'
    ? `${(p.parallel.ratio * 100).toFixed(1)}%`
    : p.parallel.measuredMs > 0
      ? '—（実装していた時間がありません）'
      : '記録なし（件数の分かる時間がありません）';
// 前後の比較の表の行（項目、前、後）。記録の無い期間は集計対象の行に理由、ほかの行は「記録なし」。
function stallStatsRows(periods: StallPeriod[]) {
  const cell = (p: StallPeriod, value: (p: StallPeriod) => string) =>
    p.measured ? value(p) : '記録なし';
  const row = (label: string, value: (p: StallPeriod) => string) => [
    label,
    ...periods.map((p) => cell(p, value)),
  ];
  return [
    [
      '集計対象',
      ...periods.map((p) =>
        p.measured
          ? `${stallStatsTime(p.measured.start)} 〜 ${stallStatsTime(p.measured.end)}（日本時間）`
          : `記録なし（${p.reason || '理由不明'}）`,
      ),
    ],
    row(
      '止まりの件数',
      (p) => `${p.stalls.total}件（うち不具合の疑い ${p.stalls.suspected}件）`,
    ),
    ...stallStatsKindNames.map(([kind, name]) =>
      row(`　${name}`, (p) => `${p.stalls.kinds[kind] ?? 0}件`),
    ),
    row(
      '止まっていた時間の合計',
      (p) =>
        `${stallStatsDuration(p.stalls.stalledMs)}${p.stalls.unresolved ? `（期間の終わりに未解消 ${p.stalls.unresolved}件）` : ''}`,
    ),
    row('記録の無い時間（集計から除外）', (p) =>
      p.unrecordedMs ? stallStatsDuration(p.unrecordedMs) : 'なし',
    ),
    row('外す印を付けた占有', (p) => `${p.marked}件`),
    row('自動で外した占有', (p) => `${p.released}件`),
    row('出した直す依頼（受付の記録）', (p) =>
      typeof p.fixes === 'number'
        ? `${p.fixes}件`
        : `記録なし（${p.fixesReason || '理由不明'}）`,
    ),
    row(
      '2台以上のPCが同時に実装していた時間（割合の分子）',
      (p: { parallel: { parallelMs: unknown } }) =>
        stallStatsDuration(p.parallel.parallelMs),
    ),
    row(
      '1台以上のPCが実装していた時間（割合の分母）',
      (p: { parallel: { activeMs: unknown } }) =>
        stallStatsDuration(p.parallel.activeMs),
    ),
    row('全PCの同時実装の割合', stallStatsRatio),
    row('最大同時実装台数', (p) => `${p.parallel.maxHosts}台`),
    row('PCごとの実装していた時間', (p) => {
      const hosts = Object.entries(p.parallel.hosts || {});
      return hosts.length
        ? hosts
            .map(([name, ms]) => `${name} ${stallStatsDuration(ms)}`)
            .join('、')
        : 'なし';
    }),
    row(
      '1日のマージ完了件数',
      (p) =>
        `${p.mergedPerDay.toFixed(1)}件（${p.merged}件／${p.days.toFixed(1)}日）`,
    ),
  ];
}
// 日別の表の1行。
function stallStatsDayCells(d: StallDay) {
  if (!d.measured) return [d.date, '記録なし', '記録なし', '記録なし'];
  return [
    d.date,
    `${d.stalls}件`,
    `${d.merged}件`,
    typeof d.ratio === 'number' ? `${(d.ratio * 100).toFixed(1)}%` : '—',
  ];
}
function stallStatsResult(r: StatsReport) {
  if (!r.stalls) return [];
  const s = r.stalls;
  const title = el('h2', { text: '止まりと全PCの同時実装（前後の比較）' });
  const errors = (s.unavailable || []).map((text) =>
    el('div', { class: 'error-box', text }),
  );
  if (!s.before || !s.after) return [title, ...errors];
  const periods: [string, StallPeriod][] = [
    ['前', s.before],
    ['後', s.after],
  ];
  return [
    title,
    ...errors,
    el('p', {
      class: 'stats-coverage',
      text: s.coverage
        ? `記録のある期間: ${listDate(s.coverage.start)} 〜 ${listDate(s.coverage.end)}。このPCの監視がホームのリポジトリの止まりだけを数えます（ほかのPCがホームのリポジトリの止まりは含みません）。全PCの同時実装はリポジトリを分けません。`
        : `記録なし（${s.reason || '理由不明'}）。すべての期間を「記録なし」として表示します。`,
    }),
    statsTable(
      [
        '項目',
        ...periods.map(
          ([label, p]) => `${label}（${p.fromDate}〜${p.toDate}）`,
        ),
      ],
      stallStatsRows(periods.map(([, p]) => p)),
      'stall-stats',
    ),
    ...periods.flatMap(([label, p]) =>
      p.daily
        ? [
            el('h3', {
              text: `${label}の期間の日別（${p.fromDate}〜${p.toDate}）`,
            }),
            statsTable(
              ['日付', '止まりの検知', 'マージ完了', '全PCの同時実装の割合'],
              p.daily.map(stallStatsDayCells),
              'stall-stats-days',
            ),
          ]
        : [],
    ),
  ];
}
