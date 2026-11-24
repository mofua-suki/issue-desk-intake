type StopCheckRow = {
  id: string;
  kind: string;
  purpose: string;
  introducedAt: string;
  nextReviewAt: string;
  overdue: boolean;
  total: number | null;
  last30Days: number | null;
  blocked: number | null;
  warned: number | null;
  lastFiredAt: string | null;
  reviewResult: string | null;
};

function isStopCheckRow(value: unknown): value is StopCheckRow {
  if (value === null || typeof value !== 'object') return false;
  const row = value as Record<string, unknown>;
  return (
    ['id', 'kind', 'purpose', 'introducedAt', 'nextReviewAt'].every(
      (key) => typeof row[key] === 'string',
    ) &&
    typeof row.overdue === 'boolean' &&
    ['total', 'last30Days', 'blocked', 'warned'].every(
      (key) => row[key] === null || typeof row[key] === 'number',
    ) &&
    ['lastFiredAt', 'reviewResult'].every(
      (key) => row[key] === null || typeof row[key] === 'string',
    )
  );
}

const stopCheckStatsNotes = [
  [
    'Stop検査の台帳',
    '見直し日の日本時間の当日は超過にせず、翌日から超過とします。件数の「記録なし」は集計対象外、最後の発動の「記録なし」は発動時刻の記録がないことを示します。「未見直し」は見直し結果が記録されていないことを示します。',
  ],
];

// Stop検査の台帳。APIの null は集計対象外、見直し結果の null は未見直し。
function stopCheckStatsResult(report: StatsReport) {
  const data = (report as StatsReport & { stopChecks?: unknown }).stopChecks;
  const heading = el('h2', { text: 'Stop検査の台帳' });
  if (
    !data ||
    typeof data !== 'object' ||
    !('rows' in data) ||
    !Array.isArray(data.rows) ||
    !data.rows.every(isStopCheckRow)
  ) {
    const reported = report.unavailable?.includes(
      'stopChecksの集計を読み取れませんでした。',
    );
    return [
      heading,
      ...(reported
        ? []
        : [
            el('div', {
              class: 'error-box',
              text: 'Stop検査の台帳を読めませんでした。',
            }),
          ]),
    ];
  }
  const rows = (data.rows as StopCheckRow[])
    .slice()
    .sort((a, b) => Number(b.overdue) - Number(a.overdue));
  const headers = [
    '状態',
    '検査ID',
    '狙い',
    '種類',
    '発動数（全期間）',
    '発動数（直近30日）',
    '止めた数',
    '警告の数',
    '最後の発動',
    '導入日',
    '次の見直し日',
    '見直し結果',
  ];
  const count = (value: number | null) =>
    value === null ? '記録なし' : `${value}件`;
  const table = el(
    'div',
    { class: 'stats-table stop-check-stats' },
    el(
      'table',
      {},
      el(
        'thead',
        {},
        el('tr', {}, ...headers.map((text) => el('th', { text }))),
      ),
      el(
        'tbody',
        {},
        ...rows.map((row) =>
          el(
            'tr',
            { class: row.overdue ? 'overdue' : '' },
            ...[
              row.overdue ? '見直し日超過' : '—',
              row.id,
              row.purpose,
              row.kind,
              count(row.total),
              count(row.last30Days),
              count(row.blocked),
              count(row.warned),
              row.lastFiredAt ? reviewTime(row.lastFiredAt) : '記録なし',
              row.introducedAt,
              row.nextReviewAt,
              row.reviewResult || '未見直し',
            ].map((text) => el('td', { text })),
          ),
        ),
      ),
    ),
  );
  const unknown =
    'unknownCheckIds' in data && Array.isArray(data.unknownCheckIds)
      ? data.unknownCheckIds.filter((id: unknown) => typeof id === 'string')
      : [];
  return [
    heading,
    ...(unknown.length
      ? [
          el('p', {
            class: 'error-box',
            text: `台帳にない検査IDの記録: ${unknown.join('、')}`,
          }),
        ]
      : []),
    table,
  ];
}
