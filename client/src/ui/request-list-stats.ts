import type { WorkPeriod, WorkDay } from './stats-types.js';
// 稼働実績の表（並列・日別・工程別）の文言と行。Issue #1489 で client/src/ui/request-list.js から分けた（既存の import は
// client/src/ui/request-list.js の再エクスポートで保つ）。このファイルは他のモジュールを import しない。

// --- 稼働実績（Issue #246。集計は shared/src/work-stats.mjs） -----------------------------------------------
// 画面と docs/parallel-implementation.md の「3で入れたもの」で同じ定義を示す。
export const workStatsNotes = [
  [
    '対象',
    'このPC（受付と同じPC）の監視の記録（runner.log と runner.log.previous）。他のPCが担当したIssueは含みません。',
  ],
  [
    '対象工程',
    '各Issueの外側の「実装」工程（工程開始 … 実装 から 工程終了／工程中断 まで）。PR作成・レビュー・マージは含みません。',
  ],
  [
    '並列割合',
    '分子は2件以上のIssueが同時に実装中だった経過時間、分母は1件以上が実装中だった経過時間です。延べ実装時間（各実装の時間の合計）は参考値で、割合には使いません。同じIssueの記録が重なっても1件と数えます。',
  ],
  [
    '完了',
    'そのIssueのPRの作成が済んだ時刻（Issueごとに最初の1回）。自動マージの無いリポジトリは「完了」の行、自動マージのあるリポジトリはレビュー・マージの工程が始まる直前の「PR作成済み」の行（それが無い古い記録は、公開処理に失敗していない「PR作成」工程の終了）です。マージ・デプロイの完了ではありません。PRの作成が記録に無いものは欠測です。',
  ],
  [
    '実行待ち時間',
    '起票（GiteaのIssueの作成日時）から最初の実装開始まで。先行PR待ち・優先度判定・順番待ちを含みます。',
  ],
  [
    '再試行・中断',
    '再試行・修正指示による再実行もそれぞれ1回の実行として時間に含め、中断した実行も中断までの時間を含めます。完了件数・所要時間・実行待ちはIssueごとに最初の完了・最初の実装開始だけを使い、重ねて数えません。',
  ],
  [
    '期間の境界',
    '日本時間の0時区切りで、終了日を含みます。境界をまたぐ実装は期間内の部分だけを数えます。完了件数・所要時間は完了の時刻、実行待ちは実装開始の時刻の属する期間に入れます。前後の期間は同じ定義で集計します。',
  ],
  [
    '欠測',
    '終了も中断も記録されないまま監視が再起動した（または止まった）実装は、開始から次の起動（または監視が最後に動いていた時刻）までをそのリポジトリの欠測の区間として時間の集計から除きます。記録の始まりより前に起票したIssueと起票日時を取得できないIssueは、初回か確定できないため件数・平均に入れず欠測として示します。',
  ],
  [
    '監視の停止',
    '監視が今止まっているときは、最後に動いていた時刻（記録の最後の行と heartbeat の新しい方）から今までを「監視の停止」として集計対象から除きます。過去に止まっていた時間は、記録が出来事のときにしか書かれないため実装していない時間と区別できず、集計対象の長さに含まれます（割合・件数には影響しません。実装の無い日は「—（実装中の時間がありません）」と出ます）。',
  ],
];
// 経過時間の表示。数値でなければ欠測。
export function durationText(ms: unknown) {
  if (typeof ms !== 'number' || !Number.isFinite(ms) || ms < 0) return '欠測';
  const minutes = Math.round(ms / 60000);
  if (!minutes) return ms ? '1分未満' : '0分';
  if (minutes < 60) return `${minutes}分`;
  const hours = Math.floor(minutes / 60);
  if (hours < 48)
    return minutes % 60 ? `${hours}時間${minutes % 60}分` : `${hours}時間`;
  return hours % 24
    ? `${Math.floor(hours / 24)}日${hours % 24}時間`
    : `${Math.floor(hours / 24)}日`;
}
// 日本時間の表示。相談の難易度と結末・フォローアップの実績・モデルの切り替え（client/src/ui/request-list-outcomes.js）も使う。
export const jstTime = (t: string | number | Date) =>
  new Date(t).toLocaleString('ja-JP', {
    timeZone: 'Asia/Tokyo',
    year: 'numeric',
    month: 'numeric',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  });
// 分母が無い期間は割合を出さない。集計できる時間そのものが無い（全部が欠測）場合は欠測と示す。
export function ratioText(p: Pick<WorkDay, 'ratio' | 'measuredMs'>) {
  if (typeof p.ratio === 'number') return `${(p.ratio * 100).toFixed(1)}%`;
  return p.measuredMs > 0
    ? '—（実装中の時間がありません）'
    : '欠測（集計できる時間がありません）';
}
function averageText(
  averageMs: unknown,
  count: number,
  unconfirmed: number,
  none: string,
) {
  if (count)
    return `${durationText(averageMs)}（${count}件${unconfirmed ? `。ほかに欠測 ${unconfirmed}件` : ''}）`;
  return unconfirmed ? `欠測（確定できない ${unconfirmed}件）` : none;
}
// 前後の比較表の行（項目名と値）。記録の無い期間は「記録なし」の1行だけ。
export function workStatsRows(p: WorkPeriod): [string, string][] {
  if (!p.measured)
    return [
      ['集計対象', '記録なし（この期間に runner.log の記録がありません）'],
    ];
  return [
    ['集計対象', `${jstTime(p.measured.start)} 〜 ${jstTime(p.measured.end)}`],
    [
      '欠測の区間',
      p.gapMs
        ? `${durationText(p.gapMs)}（終了の記録が無い実装 ${p.gaps}件。集計から除外）`
        : 'なし',
    ],
    [
      '監視の停止',
      p.stoppedMs
        ? `${durationText(p.stoppedMs)}（監視が止まっている時間。集計から除外）`
        : 'なし',
    ],
    ['2件以上の同時実装時間（割合の分子）', durationText(p.parallelMs)],
    ['1件以上が実装中の経過時間（割合の分母）', durationText(p.activeMs)],
    ['並列割合', ratioText(p)],
    ['延べ実装時間（参考。割合には使いません）', durationText(p.totalMs)],
    ['最大同時実装数', `${p.maxConcurrent}件`],
    [
      '実装の実行回数（再試行を含む）',
      `${p.runs}回（うち中断 ${p.interrupted}回、終了の記録なし ${p.unended}回）`,
    ],
    [
      '完了件数',
      `${p.completed}件${p.unconfirmed ? `（ほかに初回か確定できない ${p.unconfirmed}件）` : ''}`,
    ],
    [
      '起票から完了までの平均',
      averageText(
        p.leadMs,
        p.completed,
        p.unconfirmed,
        '—（完了がありません）',
      ),
    ],
    [
      '実行待ち時間の平均（起票から最初の実装開始まで）',
      averageText(
        p.waitMs,
        p.waitCount,
        p.waitUnconfirmed,
        '—（実装の開始がありません）',
      ),
    ],
  ];
}
export const workDayHeaders = [
  '日付',
  '完了件数',
  '2件以上の同時実装',
  '並列割合',
  '起票から完了の平均',
  '実行待ちの平均',
];
// 日別の表の1行。
export function workDayCells(d: WorkDay) {
  if (!d.measured) return [d.date, '記録なし', '—', '—', '—', '—'];
  const average = (ms: unknown, count: number, unconfirmed: number) =>
    count ? durationText(ms) : unconfirmed ? '欠測' : '—';
  return [
    d.date,
    `${d.completed}件${d.unconfirmed ? `（欠測 ${d.unconfirmed}件）` : ''}`,
    `${durationText(d.parallelMs)}${d.gapMs ? '（欠測あり）' : d.stoppedMs ? '（監視の停止あり）' : ''}`,
    ratioText(d),
    average(d.leadMs, d.completed, d.unconfirmed),
    average(d.waitMs, d.waitCount, d.waitUnconfirmed),
  ];
}
