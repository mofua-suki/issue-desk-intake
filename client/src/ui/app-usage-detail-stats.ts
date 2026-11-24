// 稼働実績の「AIの使用量」の呼び出し・文脈・道具・ファイル・プロンプトの構成・利用上限の表（Issue #571。集計は
// shared/src/usage-detail-stats.mjs、定義は docs/ai-usage-detail.md『Detail statistics』と「画面（4/4）」）。client/src/ui/app.js の一部で、単独の
// モジュールではない（client/scripts/build.ts の順に app.js とつなぐ）。使用量の表（client/src/ui/app-stats.js の usageResult）の後に
// 期間ごとに出す。tools の無い応答（更新前の受付）では出さない。値の無い項目は「不明（N件）」で、ゼロと出さない。
const usageDetailResetRows = 20;
const usageDetailNumber = (v: number) => Math.round(v).toLocaleString('ja-JP');
// 値と不明の件数。値が無ければ「不明（N件）」（行が無ければ「—」）、一部だけ不明なら値に「（ほかに不明 N件）」を添える。
function usageDetailValue(
  value: number | null | undefined,
  unknown: number | undefined,
  text = usageDetailNumber,
) {
  if (typeof value !== 'number') return unknown ? `不明（${unknown}件）` : '—';
  return `${text(value)}${unknown ? `（ほかに不明 ${unknown}件）` : ''}`;
}
const usageDetailTime = (iso: string | undefined) =>
  iso && Number.isFinite(Date.parse(iso))
    ? new Date(Date.parse(iso) + 9 * 3600000)
        .toISOString()
        .slice(0, 16)
        .replace('T', ' ')
    : '不明';
const usageDetailPercent = (v: number | undefined) =>
  typeof v === 'number' ? `${v}%` : '不明';
// 合計・工程別・実行先×モデル別の行（名前と軸の値）。
function usageDetailAxes(p: UsagePeriod): [string, UsageTotal][] {
  return [
    ['合計', p.total],
    ...(p.steps || []).map((s): [string, UsageTotal] => [
      `工程: ${s.step ?? '不明'}`,
      s,
    ]),
    ...(p.models || []).map((m): [string, UsageTotal] => [
      `${m.provider ?? '不明'} / ${m.model ?? '不明'}`,
      m,
    ]),
  ];
}
const usageDetailCallHeaders = [
  '区分',
  '件数',
  '呼び出し回数',
  '問いかけのバイト数（合計）',
  '問いかけのバイト数（1件あたり）',
];
function usageDetailCallCells([label, t]: [string, UsageTotal]) {
  const u = t.detailUnknown || {};
  return [
    label,
    `${t.count}件`,
    usageDetailValue(t.calls, u.calls),
    usageDetailValue(t.promptTotalBytes, u.promptTotalBytes),
    usageDetailValue(t.promptAverageBytes, u.promptTotalBytes),
  ];
}
const usageDetailContextHeaders = ['区分', '件数', '文脈の最大', '文脈の合計'];
function usageDetailContextCells([label, t]: [string, UsageTotal]) {
  const u = t.detailUnknown || {};
  return [
    label,
    `${t.count}件`,
    usageDetailValue(t.contextMaxTokens, u.contextMaxTokens),
    usageDetailValue(t.contextTotalTokens, u.contextTotalTokens),
  ];
}
const usageDetailCoverage = (what: string, x: DetailCoverage) =>
  el('p', {
    class: 'stats-coverage usage-detail-note',
    text: `${what}の記録のある行 ${x.count}件${x.unknown ? `、記録の無い行（旧形式を含む。不明） ${x.unknown}件` : ''}。`,
  });
function usageDetailFileCells(f: UsageFile) {
  return [
    shownProject(f.repo) ?? '不明',
    f.path,
    `${f.count}回`,
    usageDetailValue(f.bytes, f.unknown),
  ];
}
function usageDetailPromptRows(pp: PromptParts) {
  const row = (label: string, s: PromptPart) => [
    label,
    usageDetailValue(s.total, s.unknown),
    usageDetailValue(s.average, s.unknown),
  ];
  return [
    ...pp.parts.map((x) => row(x.name, x)),
    row('合計（promptTotalBytes）', pp.promptTotalBytes),
    row('同じ行のキャッシュ読み（トークン）', pp.cacheReadTokens),
    row('同じ行のキャッシュ書き（トークン）', pp.cacheWriteTokens),
  ];
}
const usageDetailLimitHeaders = [
  '窓',
  'リセット時刻',
  '記録',
  '最初（時刻・利用率）',
  '最後（時刻・利用率）',
  'Claude の行',
  '入力',
  '出力',
  'キャッシュ読み',
  'キャッシュ書き',
];
function usageDetailLimitCells(name: string, g: LimitReset) {
  const c = g.claude;
  return [
    name,
    usageDetailTime(g.resetsAt),
    `${g.records}件`,
    `${usageDetailTime(g.first.at)} ${usageDetailPercent(g.first.percent)}`,
    `${usageDetailTime(g.last.at)} ${usageDetailPercent(g.last.percent)}`,
    `${c.count}件`,
    ...(
      [
        'inputTokens',
        'outputTokens',
        'cacheReadTokens',
        'cacheWriteTokens',
      ] as const
    ).map((k) => usageDetailValue(c[k], c.unknown?.[k])),
  ];
}
function usageDetailLimitResult(r: RateLimits) {
  if (!r.windows.length)
    return [
      el('p', {
        class: 'stats-coverage usage-detail-note',
        text: `利用上限の記録なし${r.unknown ? `（記録の無い Claude の行 ${r.unknown}件は不明）` : ''}。`,
      }),
    ];
  const latest = r.windows
    .map((w) => {
      const l = w.latest;
      return `${w.name} ${usageDetailPercent(l.percent)}（リセット ${usageDetailTime(l.resetsAt)}、記録 ${usageDetailTime(l.at)}）`;
    })
    .join('、');
  const omitted = r.windows.reduce(
    (n: number, w) => n + Math.max(0, w.resets.length - usageDetailResetRows),
    0,
  );
  return [
    el('p', {
      class: 'stats-coverage usage-detail-note',
      text: `最新の記録: ${latest}。利用上限の記録のある Claude の行 ${r.count}件${r.unknown ? `、記録の無い行（不明） ${r.unknown}件` : ''}${omitted ? `。窓ごとに新しい ${usageDetailResetRows}件のリセット時刻を表示（ほかに ${omitted}件は API だけ）` : ''}。`,
    }),
    statsTable(
      usageDetailLimitHeaders,
      r.windows.flatMap((w) =>
        w.resets
          .slice(-usageDetailResetRows)
          .map((g) => usageDetailLimitCells(w.name, g)),
      ),
      'usage-detail-table',
    ),
  ];
}
function usageDetailResult(label: string, p: UsagePeriod) {
  if (!p?.tools) return [];
  const title = (what: string) =>
    el('h3', { text: `${label}の期間の${what}（${p.fromDate}〜${p.toDate}）` });
  const axes = usageDetailAxes(p);
  const files = p.files.items;
  return [
    title('呼び出し'),
    statsTable(
      usageDetailCallHeaders,
      axes.map(usageDetailCallCells),
      'usage-detail-table',
    ),
    title('文脈の大きさ（トークン）'),
    statsTable(
      usageDetailContextHeaders,
      axes.map(usageDetailContextCells),
      'usage-detail-table',
    ),
    title('道具（Codex は item 種別）'),
    usageDetailCoverage('道具', p.tools),
    statsTable(
      ['道具', '回数'],
      p.tools.items.length
        ? p.tools.items.map((x) => [x.name, `${x.count}回`])
        : [['記録なし', '—']],
      'usage-detail-table',
    ),
    title('読んだファイル（バイト数の多い順の上位）'),
    usageDetailCoverage('ファイル', p.files),
    statsTable(
      ['リポジトリ', 'パス', '回数', 'バイト数'],
      files.length
        ? files.map(usageDetailFileCells)
        : [['記録なし', '—', '—', '—']],
      'usage-detail-table',
    ),
    ...(p.files.omitted
      ? [
          el('p', {
            class: 'stats-coverage usage-detail-note',
            text: `ほかに ${p.files.omitted}件のパス。`,
          }),
        ]
      : []),
    title('プロンプトの構成（受付の相談。バイト数）'),
    el('p', {
      class: 'stats-coverage usage-detail-note',
      text: `構成の記録のある受付の相談 ${p.promptParts.count}件${p.promptParts.unknown ? `、記録の無い受付の相談（旧形式を含む。不明） ${p.promptParts.unknown}件` : ''}。`,
    }),
    statsTable(
      ['構成要素', '合計', '1件あたり'],
      usageDetailPromptRows(p.promptParts),
      'usage-detail-table',
    ),
    title('利用上限（Claude。全リポジトリ）'),
    ...usageDetailLimitResult(p.rateLimits),
  ];
}

// 到着状況は全記録元の観測。既存のトークン項目の欠測と分ける。
function usageArrivalResult(c: UsageArrival | undefined) {
  if (!c) return [];
  const unknown = (v: unknown) => v ?? '不明';
  return [
    el('h3', { text: 'PC／記録元の使用量の到着状況' }),
    el('p', { text: `${c.from}〜${c.to}。${c.scope} ${c.caution}` }),
    el('p', {
      text: '受付の取り込み位置は各PCの送信位置ではありません。各PCの送信位置・hostとsenderとファイルの対応は不明。',
    }),
    el('p', {
      text: c.failures.length
        ? `取得失敗: ${c.failures.join('; ')}`
        : '取得成功（正常0件を含む）',
    }),
    statsTable(
      [
        'host',
        'sender',
        '記録元',
        '期間内行数',
        '最新at',
        '状態',
        '登録host',
        '登録sender',
      ],
      c.rows.map((g) => [
        g.host,
        g.sender,
        unknown(g.source),
        g.count,
        usageDetailTime(g.latest),
        g.status,
        unknown(g.registeredHost),
        unknown(g.registeredSender),
      ]),
      'usage-arrival-table',
    ),
    statsTable(
      ['登録種別', '名前', '期間内行数', '最新at', '状態'],
      c.registrations.map((g) => [
        g.kind,
        g.name,
        unknown(g.count),
        usageDetailTime(g.latest),
        g.status,
      ]),
      'usage-arrival-table',
    ),
    statsTable(
      [
        '受付ファイル（絶対パスは非表示）',
        '取り込み位置（bytes）',
        '更新日時',
        'PCとの対応',
      ],
      (c.imports || []).map((p) => [
        p.file,
        p.position,
        usageDetailTime(p.updatedAt),
        p.mapping,
      ]),
      'usage-arrival-table',
    ),
    el('p', {
      text: `呼び出し件数: 母数不明。記録あり ${unknown(c.reconciliation.recorded)}行、記録なし ${unknown(c.reconciliation.missing)}件、照合不能 ${unknown(c.reconciliation.unmatchable)}行。${c.reconciliation.reason}`,
    }),
  ];
}
