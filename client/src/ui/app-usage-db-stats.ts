// 稼働実績の AI の使用量の「リポジトリ別（工程ごと）」と「上限の使用率（Claude・Codex）」の表（Issue #1155。集計は
// server/src/usage-db-stats.mjs、定義は docs/ai-usage-db.md『Stats from the database』）。client/src/ui/app.js の一部で、単独のモジュールではない
// （client/scripts/build.ts の順に app-stats.js の後へつなぐ）。使用量の表の列と値（weightedUsageHeaders・weightedUsageCells・
// weightedUsageNone）は client/src/ui/app-stats.js のものを使う。値の無い項目は「不明」、記録が無ければ「記録なし」で、ゼロとは出さない。
const usageDbProviderNames: Record<string, string> = {
  claude: 'Claude',
  codex: 'Codex',
};
const usageLimitHeaders = [
  '実行先',
  '上限の種類',
  '使用率',
  '窓の長さ',
  'リセット時刻',
  '記録日時',
  'PC',
];
const usageDbStatsNotes = [
  [
    'AIの使用量の読み元',
    '前後の期間ごとのAIの使用量の表（工程別・実行先×モデル別・区分別・Issue上位・PC別と受付AIのキャッシュ）と下の2つの表は、受付の DB に集めた全PCの行（各PCの監視と受付の runtime/usage/・runtime/usage-limits/ から届いた行。同じ行は1件）から作ります。手戻りの内訳・注意点の効き目・相談ごとの使用量など、ほかの表は今までどおりこのPCの runtime/usage/ から作ります。',
  ],
  [
    'リポジトリ別（工程ごと）',
    '受付の DB に集めた全PCの使用量の行を、記録のリポジトリごとに合計し、その下に工程ごとの値を並べます。列と値の扱いはほかの使用量の表と同じです。リポジトリの無い行は「リポジトリの記録なし」にまとめます。',
  ],
  [
    '上限の使用率（Claude・Codex）',
    '受付の DB に集めた全PCの上限の行（直近30日）から、実行先×上限の種類ごとに最新の行を出します。値の無い項目は「不明」です。',
  ],
];

// 1つの期間のリポジトリ別の表の行。リポジトリの合計の行の後に、その工程の行（字下げ）を並べる。行が無ければ「記録なし」。
function usageReposRows(p: UsagePeriod) {
  if (!p?.repos?.length) return weightedUsageNone('記録なし');
  return p.repos.flatMap((r) => [
    weightedUsageCells(shownProject(r.repo) ?? 'リポジトリの記録なし', r),
    ...(r.steps || []).map((s) =>
      weightedUsageCells(`　${s.step ?? '工程の記録なし'}`, s),
    ),
  ]);
}

// 上限の使用率の1行の値。
const usageLimitTime = (t: string | undefined) =>
  typeof t === 'string' && Number.isFinite(Date.parse(t))
    ? listDate(t)
    : '不明';
function usageLimitCells(row: UsageLimit) {
  return [
    usageDbProviderNames[row.provider] || row.provider || '不明',
    row.limit || '不明',
    typeof row.percent === 'number' ? `${row.percent}%` : '不明',
    Number.isSafeInteger(row.windowMinutes) ? `${row.windowMinutes}分` : '不明',
    usageLimitTime(row.resetsAt),
    usageLimitTime(row.at),
    row.host || '不明',
  ];
}
// 実行先ごとの行。Claude・Codex に行が無ければ「記録なし」の1行（ゼロとは出さない）。
function usageLimitRows(limits: UsageLimits) {
  const rows = Array.isArray(limits?.rows) ? limits.rows : [];
  const providers = [
    ...new Set([
      ...Object.keys(usageDbProviderNames),
      ...rows.map((r) => r.provider),
    ]),
  ];
  return providers.flatMap((p) => {
    const mine = rows.filter((r) => r.provider === p);
    return mine.length
      ? mine.map(usageLimitCells)
      : [
          [
            usageDbProviderNames[p] || p || '不明',
            '記録なし',
            ...usageLimitHeaders.slice(2).map(() => '—'),
          ],
        ];
  });
}

// 使用量の表の後に置く2つの表。repos の無い応答・usageLimits の無い応答（更新前の受付）では、その表を出さない。
// 使用量を集計できなかったとき（before・after が null）は、リポジトリ別の表を出さない（理由は使用量の節に出る）。
function usageDbStatsResult(r: StatsReport) {
  const u = r.usage;
  const periods = (
    [
      ['前', u?.before],
      ['後', u?.after],
    ] as [string, UsagePeriod | undefined][]
  ).filter(
    (entry): entry is [string, UsagePeriod] =>
      !!entry[1] && Array.isArray(entry[1].repos),
  );
  const repos = periods.length
    ? [
        el('h2', { text: 'リポジトリ別（工程ごと）' }),
        ...periods.flatMap(([label, p]) => [
          el('h3', { text: `${label}の期間（${p.fromDate}〜${p.toDate}）` }),
          statsTable(
            ['リポジトリ / 工程', ...weightedUsageHeaders],
            usageReposRows(p),
            'usage-table usage-repos',
          ),
        ]),
      ]
    : [];
  const l = r.usageLimits;
  const limits = l
    ? [
        el('h2', { text: '上限の使用率（Claude・Codex）' }),
        ...(l.reason
          ? [
              el('p', {
                class: 'stats-coverage',
                text: `上限の使用率の記録なし（${l.reason}）`,
              }),
            ]
          : []),
        statsTable(usageLimitHeaders, usageLimitRows(l), 'usage-limits'),
      ]
    : [];
  return [...repos, ...limits, ...fixedTestStatsResult(r.fixedTestUsage)];
}

function fixedTestStatsResult(report: FixedTestReport | undefined) {
  if (!report) return [];
  const metric = (value: number | undefined, unit = '') =>
    typeof value === 'number' ? `${value}${unit}` : '不明';
  const groups: Record<string, string> = {
    1: '1件',
    '2-3': '2〜3件',
    '4+': '4件以上',
  };
  const headers = [
    'PC',
    'リポジトリ',
    '範囲',
    '開始時の実装中件数',
    '実行件数',
    '経過時間 中央値(ms)',
    '経過時間 p90(ms)',
    '期限使用率 中央値',
    '打ち切り件数',
    '開始時1分平均負荷 中央値',
  ];
  return [
    el('h2', { text: '固定テストの期限使用率（全PC）' }),
    el('p', {
      text: '指定した前後の期間に開始した実測をPC・リポジトリ・範囲（full・selected・early）別に比較します。docsと合格の再利用は含めません。実装中件数は開始時の同じ監視の実行中ジョブ数です。不明・0件は3群に含めず、過去分を推定しません。負荷不明は負荷の中央値から除外します。中央値は中央の値（偶数件は中央2値の平均）、p90は昇順のceil(0.9×件数)番目です。空の群は「記録なし」、値がない項目は「不明」です。',
    }),
    ...(report.reason ? [el('p', { text: report.reason })] : []),
    ...(
      [
        ['前', report.before],
        ['後', report.after],
      ] as [string, FixedTestPeriod | undefined][]
    ).flatMap(([label, period]) => {
      if (!period) return [];
      const rows = period.rows.flatMap((r) =>
        r.groups.map((g) => [
          r.host,
          shownProject(r.repo),
          r.scope,
          groups[g.group],
          g.count === null ? '記録なし' : metric(g.count),
          metric(g.elapsedMsMedian),
          metric(g.elapsedMsP90),
          metric(g.percentMedian, '%'),
          metric(g.cutCount),
          metric(g.loadavg1Median),
        ]),
      );
      return [
        el('h3', {
          text: `${label}の期間（${period.fromDate}〜${period.toDate}）`,
        }),
        ...period.rows.map((r) =>
          el('p', {
            text: `${r.host} / ${shownProject(r.repo)} / ${r.scope}: 実装中件数不明 ${r.unknownImplementing}件、0件 ${r.zeroImplementing}件、負荷不明 ${r.unknownLoad}件`,
          }),
        ),
        statsTable(
          headers,
          rows.length
            ? rows
            : [['記録なし', ...headers.slice(1).map(() => '—')]],
          'fixed-test-usage',
        ),
      ];
    }),
  ];
}
