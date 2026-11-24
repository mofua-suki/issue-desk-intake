// 稼働実績の作業の種類×宣言したパス数。app.jsと連結して配信する。
const workTypeStatsNotes = [
  [
    '作業の種類×変更範囲の大きさ',
    '全PCのjobsの起票日時で前後の期間に振り分けます。種類は起票時の記録（無ければ不明）、大きさは宣言したパス数（1〜2・3〜4・5〜7・8以上・全体（*）・変更なし・宣言なしの不明）です。ディレクトリも1パスとして数えます。種類別・大きさ別の小計は同じ依頼の別の集計なので、区分別の表へ足し直しません。',
  ],
  [
    '種類×大きさの率と使用量の分母',
    '使用量DBの監視の行がある依頼だけを分母にし、行のない依頼数を別に示します。差し戻し・再試行・再検証はgenerationTriggerのreview-return・retry・retestが一度以上ある依頼の割合です。起票後から集計時点までの記録を使い、初回のnull以外の不明なきっかけで有無を判定できない依頼は各率の分母から除きます。使用量は全工程の監視の行を費用補正して合計し、記録のある依頼数で割ります。不明の値は合計に入れず、項目ごとの不明な行数を割らずに添えます。記録なしはゼロと表示しません。',
  ],
];
function workTypeRateText(value: WorkTypeRate | undefined) {
  if (!value) return '記録なし';
  if (value.rate === null)
    return `不明（判定可能な記録なし、不明 ${value.unknown}件）`;
  return `${(value.rate * 100).toFixed(1)}%（${value.count}/${value.denominator}件${value.unknown ? `、ほかに不明 ${value.unknown}件` : ''}）`;
}
function workTypeStatsRows(groups: WorkTypeGroup[]) {
  const count = (n: number | null) => (n === null ? '記録なし' : `${n}件`);
  return groups.map((g) => [
    g.typeName ?? '全種類',
    g.sizeName ?? '全サイズ',
    count(g.count),
    count(g.usageIssues),
    count(g.missingUsage),
    count(g.unknownTriggers),
    ...[g.reviewReturns, g.retries, g.retests].map(workTypeRateText),
    ...(g.perIssue
      ? usageCells('', g.perIssue).slice(2)
      : Array(5).fill('記録なし')),
  ]);
}
function workTypeStatsResult(report: StatsReport) {
  if (!report.workTypes) return [];
  const stats = report.workTypes;
  const headers = [
    '種類',
    '大きさ（パス数）',
    '依頼数',
    '使用量のある依頼',
    '使用量の行なし',
    'きっかけ不明',
    '差し戻し率',
    '再試行率',
    '再検証の率',
    '入力/件',
    '出力/件',
    'キャッシュ読取/件',
    'キャッシュ書込/件',
    '費用/件',
  ];
  return [
    el('h2', { text: '作業の種類×変更範囲の大きさ' }),
    ...(stats.unavailable ?? []).map((text) =>
      el('div', { class: 'error-box', text }),
    ),
    ...(stats.skipped?.jobs || stats.skipped?.usage
      ? [
          el('p', {
            class: 'stats-coverage',
            text: `読めずに除いた記録: ジョブ ${stats.skipped.jobs}件、使用量 ${stats.skipped.usage}行`,
          }),
        ]
      : []),
    ...(
      [
        ['前', stats.before],
        ['後', stats.after],
      ] as [string, WorkTypePeriod | undefined][]
    ).flatMap(([label, period]) => [
      el('h3', {
        text: `${label}の期間${period ? `（${period.fromDate}〜${period.toDate}）` : ''}`,
      }),
      ...(period?.reason || !period
        ? [
            el('p', {
              class: 'stats-coverage',
              text: `記録なし（${period?.reason ?? 'ジョブの表を読めませんでした。'}）`,
            }),
          ]
        : []),
      ...(period?.groups.length
        ? [
            statsTable(
              headers,
              workTypeStatsRows(period.groups),
              'work-type-groups',
            ),
            el('h4', { text: '種類別の小計' }),
            statsTable(
              headers,
              workTypeStatsRows(period.types),
              'work-type-totals',
            ),
            el('h4', { text: '大きさ別の小計' }),
            statsTable(
              headers,
              workTypeStatsRows(period.sizes),
              'work-size-totals',
            ),
          ]
        : []),
    ]),
  ];
}
