// 受付画面の稼働実績（statsView と集計結果の表）。client/src/ui/app.js の一部（Issue #457）で、単独のモジュールではない。受付は
// client/scripts/build.ts の順に app.js とつないだ1つのモジュールとして /app.js を配信する。
// 稼働実績（Issue #246）。このPCの監視の記録から、選んだ前後の期間を同じ定義で集計して並べる。
// 選んだ条件は画面を離れても保ち、再読み込みで既定（今日までの7日間とその前の7日間）に戻る。
let statsSelection: Record<string, string> | null = null;
const statsTable = (headers: string[], rows: unknown[][], className = '') =>
  el(
    'div',
    { class: `stats-table${className ? ` ${className}` : ''}` },
    el(
      'table',
      {},
      el(
        'thead',
        {},
        el('tr', {}, ...headers.map((h) => el('th', { text: h }))),
      ),
      el(
        'tbody',
        {},
        ...rows.map((cells) =>
          el('tr', {}, ...cells.map((c) => el('td', { text: c }))),
        ),
      ),
    ),
  );
function statsResult(r: StatsReport) {
  const periods: [string, WorkPeriod][] = [
    ['前', r.before],
    ['後', r.after],
  ];
  const rows = periods.map(([, p]) => new Map(workStatsRows(p)));
  const labels = [
    ...new Set(
      periods.flatMap(([, p]) => workStatsRows(p).map(([label]) => label)),
    ),
  ];
  return [
    el('p', {
      class: 'stats-coverage',
      text: r.coverage
        ? `記録のある期間: ${listDate(r.coverage.start)} 〜 ${listDate(r.coverage.end)}（${r.worker ? '監視は稼働中' : r.stoppedSince ? `監視の稼働を確認できません。${listDate(r.stoppedSince)} 以後は監視の停止として集計から除きます` : '監視の稼働を確認できません'}）`
        : r.unavailable.some((text: string) =>
              text.startsWith('runner.log を読めませんでした'),
            )
          ? '記録を読めませんでした（理由は下のとおり）。すべての期間を「記録なし」として表示します。'
          : '記録がありません（このPCに runner.log が見つかりません）。すべての期間を「記録なし」として表示します。',
    }),
    ...r.unavailable.map((text) => el('div', { class: 'error-box', text })),
    el('h2', { text: '前後の比較' }),
    statsTable(
      [
        '項目',
        ...periods.map(
          ([label, p]) => `${label}（${p.fromDate}〜${p.toDate}）`,
        ),
      ],
      labels.map((label) => [
        label,
        ...rows.map((m) => m.get(label) ?? '記録なし'),
      ]),
    ),
    ...periods.flatMap(([label, p]) => [
      el('h2', { text: `${label}の期間の日別（${p.fromDate}〜${p.toDate}）` }),
      statsTable(workDayHeaders, p.days.map(workDayCells)),
    ]),
    ...usageResult(r),
    // リポジトリ別（工程ごと）と上限の使用率（Issue #1155。client/src/ui/app-usage-db-stats.js）。
    ...usageDbStatsResult(r),
    // 実装の手戻りの内訳と差し戻しの割合（Issue #786。client/src/ui/app-rework-stats.js）。
    ...reworkStatsResult(r),
    // 実装の注意点の効き目（Issue #650。client/src/ui/app-review-notes-stats.js）。
    ...reviewNotesStatsResult(r),
    ...usageReviewResult(r),
    ...priorityReviewStatsResult(r),
    // フォローアップの実行先別の比較（Issue #843。client/src/ui/app-antigravity-stats.js）。
    ...antigravityStatsResult(r),
    ...issueBodyStatsResult(r),
    ...tiersResult(r),
    ...followupsResult(r),
    // 変更範囲の宣言の実績（Issue #771。client/src/ui/app-scope-stats.js）。
    ...scopeStatsResult(r),
    ...workTypeStatsResult(r),
    // テストの選び方の前後比較（Issue #791。client/src/ui/app-test-selection-stats.js）。
    ...testSelectionStatsResult(r),
    // 止まりと全PCの同時実装（Issue #849。client/src/ui/app-stall-stats.js）。
    ...stallStatsResult(r),
    ...inventoryStatsResult(r),
    ...stopCheckStatsResult(r),
  ];
}
// フォローアップの実績（Issue #486）。followups の無い応答（更新前の受付）では出さない。集計できなかった期間は記録なし。
function followupsResult(r: StatsReport) {
  if (!r.followups) return [];
  const f = r.followups;
  const periods: [string, FollowupPeriod | null | undefined, WorkPeriod][] = [
    ['前', f.before, r.before],
    ['後', f.after, r.after],
  ];
  const rows = periods.map(
    ([, p]) =>
      new Map(
        followupStatsRows(p, repositoryProjects()).map(
          ([k, v]): [string, string] => [k, v],
        ),
      ),
  );
  const labels = followupStatsRows(null).map(([label]) => label);
  const list = f.rows || [];
  return [
    el('h2', { text: 'フォローアップの実績' }),
    ...(f.unavailable || []).map((text) =>
      el('div', { class: 'error-box', text }),
    ),
    statsTable(
      [
        '項目',
        ...periods.map(
          ([label, p, w]) =>
            `${label}（${(p || w)?.fromDate ?? '?'}〜${(p || w)?.toDate ?? '?'}）`,
        ),
      ],
      labels.map((label) => [
        label,
        ...rows.map((m) => m.get(label) ?? '記録なし'),
      ]),
      'followup-stats',
    ),
    el('p', {
      class: 'stats-coverage',
      text: list.length
        ? `前後の期間に起票したフォローアップ ${f.total}件${f.omitted ? `（新しい順に ${list.length}件を表示。ほかに ${f.omitted}件）` : ''}。結末は今の状態で分けます（過去の状態の履歴はありません）。`
        : '前後の期間に起票したフォローアップはありません。',
    }),
    ...(list.length
      ? [
          statsTable(
            followupRowHeaders,
            list.map((row) => followupRowCells(row, repositoryProjects())),
            'followup-rows',
          ),
        ]
      : []),
  ];
}
// AIの使用量（Issue #480）。usage の無い応答（更新前の受付）では出さない。記録が無い・読めないときは理由だけ。
function usageResult(r: StatsReport) {
  if (!r.usage) return [];
  const u = r.usage;
  if (!u.before || !u.after)
    return [
      el('h2', { text: 'AIの使用量' }),
      ...usageArrivalResult(u.coverage),
      el('p', {
        class: 'stats-coverage',
        text: `使用量の記録なし（${u.reason || '理由不明'}）`,
      }),
    ];
  const periods: [string, UsagePeriod][] = [
    ['前', u.before],
    ['後', u.after],
  ];
  const axes: [UsageAxis, string, string][] = [
    ['steps', '工程別', '工程'],
    ['models', '実行先×モデル別', '実行先 / モデル'],
    ['kinds', '区分別', '区分'],
    ['issues', 'Issue上位（10件）', 'Issue'],
    // PC別（Issue #876）。ほかのPCの行は引き渡しで取り込んだもの。
    ['hosts', 'PC別', 'PC'],
  ];
  return [
    ...(u.skipped
      ? [
          el('p', {
            class: 'stats-coverage',
            text: `使用量の記録のうち読めなかった行 ${u.skipped}件は数えていません。`,
          }),
        ]
      : []),
    ...periods.flatMap(([label, p]) => {
      const tables = weightedUsageTables(p);
      const correction = usageCorrectionText(p);
      return [
        el('h2', {
          text: `${label}の期間のAIの使用量（${p.fromDate}〜${p.toDate}）`,
        }),
        el('p', { class: 'stats-coverage', text: weightedUsageTotalText(p) }),
        ...(correction
          ? [
              el('p', {
                class: 'stats-coverage usage-correction',
                text: correction,
              }),
            ]
          : []),
        ...axes.flatMap(([key, title, head]) => [
          el('h3', { text: title }),
          statsTable(
            [head, ...weightedUsageHeaders],
            tables[key],
            'usage-table',
          ),
        ]),
      ];
    }),
    // 受付AIのキャッシュ（Issue #880。client/src/ui/app-intake-cache-stats.js）。
    ...periods.flatMap(([label, p]) => intakeCacheResult(label, p)),
    // 呼び出し・文脈・道具・ファイル・プロンプトの構成・利用上限（Issue #571。client/src/ui/app-usage-detail-stats.js）。
    ...usageArrivalResult(u.coverage),
    ...periods.flatMap(([label, p]) => usageDetailResult(label, p)),
  ];
}
// AIの使用量の表の列（Issue #745。集計は shared/src/usage-weights.mjs、定義は docs/ai-usage.md『Aggregation』）。
// 記録の入力をキャッシュでない入力へ揃えた4つの内訳と、重み付きの量・費用の目安。Issue単位の時間の流れ・手戻りの内訳の表は
// 記録の値のまま（client/src/ui/request-list.js の usageHeaders・usageCells）。
const weightedUsageHeaders = [
  '件数',
  'キャッシュでない入力',
  'キャッシュ読み',
  'キャッシュ書き',
  '出力',
  '重み付きの量',
  '費用の目安',
];
const weightedUsageKeys: MetricKey[] = [
  'uncachedInputTokens',
  'cacheReadTokens',
  'normalizedCacheWriteTokens',
  'outputTokens',
  'weighted',
  'costUsd',
];
// 1つの項目の値。値の無い項目は「不明（N件）」、一部だけ不明なら合計に添える（ゼロとは表示しない）。Codex だけの行の
// キャッシュ書きは非該当。
function weightedUsageValue(t: UsageTotal, key: MetricKey) {
  if (
    key === 'normalizedCacheWriteTokens' &&
    t.count > 0 &&
    t.cacheWriteNotApplicable === t.count
  )
    return '非該当';
  const value = t?.[key];
  const unknown = t?.unknown?.[key] ?? 0;
  if (typeof value !== 'number') return `不明（${unknown}件）`;
  const shown =
    key === 'costUsd'
      ? `$${value.toFixed(value >= 1 ? 2 : 4)}`
      : Math.round(value).toLocaleString('ja-JP');
  return `${shown}${unknown ? `（ほかに不明 ${unknown}件）` : ''}`;
}
const weightedUsageCells = (label: string, t: UsageTotal) => [
  label,
  `${t.count}件`,
  ...weightedUsageKeys.map((k) => weightedUsageValue(t, k)),
];
const weightedUsageNone = (label: string) => [
  [label, ...weightedUsageHeaders.map(() => '—')],
];
// 1つの期間の各軸の表の行。記録の無い期間は「記録なし」の1行だけ。行の名前（工程・実行先×モデル・区分・Issue・PC）と
// 並びは記録の値の表（usageTables）と同じものを使う。
function weightedUsageTables(p: UsagePeriod) {
  const axes: UsageAxis[] = ['steps', 'models', 'kinds', 'issues', 'hosts'];
  if (!p?.total?.count)
    return Object.fromEntries(
      axes.map((k) => [k, weightedUsageNone('記録なし')]),
    );
  const named = usageTables(p, repositoryProjects());
  return Object.fromEntries(
    axes.map((k) => [
      k,
      // PC別（Issue #876）。hosts の無い旧い応答では「記録なし」。
      Array.isArray(p[k])
        ? p[k].map((t, i) => weightedUsageCells(named[k][i][0], t))
        : weightedUsageNone('記録なし'),
    ]),
  );
}
// 期間の合計の1行（表と同じ並び）。
function weightedUsageTotalText(p: UsagePeriod) {
  if (!p?.total?.count)
    return '合計: 記録なし（この期間に使用量の記録がありません）';
  return `合計: ${weightedUsageCells('', p.total)
    .slice(1)
    .map((v, i) => (i ? `${weightedUsageHeaders[i]} ${v}` : v))
    .join(
      '、',
    )}${p.omittedIssues ? `（Issue上位の表の外に ${p.omittedIssues}件）` : ''}`;
}
// 利用量の見直し（Issue #591。集計は server/src/usage-review-stats.mjs、定義は docs/usage-review.md）。usageReview の無い応答
// （更新前の受付）では出さない。回ごとに実行先（claude・codex）の2行。Claude はドル、Codex は重み付きの量（Issue #745）で、
// 合算しない。
const usageReviewVerdictNames: Record<string, string> = {
  recouped: '元が取れた',
  not_recouped: '元が取れていない',
  undeterminable: '判定不能',
  waiting: '比較待ち',
  cost_only: '費用だけ',
};
const usageReviewHeaders = [
  '回',
  '開始日',
  '調査Issue',
  '対策Issue',
  '実行先',
  '見直しの費用（Claude は米ドル、Codex は重み付きの量）',
  '前後の期間',
  '日平均（前 → 後）と差',
  '減った量（差×後の日数）',
  '判定',
];
const usageReviewNotes = [
  [
    '利用量の見直しの費用',
    '見直しの回ごとに、このPCの runtime/usage/ の記録のうち、見直し用の相談（下書きID）の受付の行（全ターン・難易度の判定）と、調査Issueの行（調査の実行）を合計します。Claude は費用の目安（米ドル）、Codex は重み付きの量（キャッシュでない入力トークン換算。「（重み付き）」と示します）で、合算しません。値の無い行は「不明（N件）」です。費用対効果は表示のたびに今の数え方で計算し直すため、過去の回の Codex も重み付きの量で出ます。',
  ],
  [
    '利用量の見直しの前後比較',
    '前は最初の対策PRのマージの前7日、後は最後の対策PRのマージから最長7日（今まで）。見直しの費用の行は除き、実行先ごとの日平均と差、減った量（前の日平均−後の日平均）×後の日数を示します。後が3日に満たない回とマージを確かめていない対策Issueのある回は「比較待ち」、対策Issueの無い回は費用だけです。',
  ],
  [
    '利用量の見直しの判定',
    '実行先ごとに、減った量が見直しの費用に届かなければ「元が取れていない」（ちょうど同じなら元が取れた。費用の記録が無い実行先は費用0として、マージ後に増えていれば元が取れていない）。1つでもあれば回の判定も同じで、要対応に出します。費用・前・後のどれかで不明が件数の半分を超える実行先、前の期間の全体に記録が無い回は「判定不能」です。仕事量（Issue件数）の違いも使用量に表れるため、総量の減少だけで効果とは断定しません。判定しても頻度・しきい値・設定は自動では変えません。',
  ],
];
// Codex の値は重み付きの量で、server/src/usage-review-stats.mjs の判定の理由と同じ「（重み付き）」を添える（unit=false は単位を省く）。
const reviewAmount = (p: string, v: number, unit = true) =>
  typeof v !== 'number'
    ? '不明'
    : p === 'claude'
      ? `$${v.toFixed(Math.abs(v) >= 1 ? 2 : 4)}`
      : `${Math.round(v).toLocaleString('ja-JP')}${unit ? '（重み付き）' : ''}`;
const reviewIssue = (i: DraftIssue | null | undefined) =>
  i
    ? `${i.desk ? `#${i.desk}（` : ''}${shownProject(i.project)} の Gitea番号 ${i.number}${i.desk ? '）' : ''}`
    : '未確定';
const reviewTime = (iso: string | undefined) =>
  iso
    ? new Date(Date.parse(iso) + 9 * 3600000)
        .toISOString()
        .slice(0, 16)
        .replace('T', ' ')
    : '?';
function usageReviewCostText(p: string, t: ReviewMeasure | undefined) {
  if (!t || !t.count) return '記録なし';
  if (typeof t.value !== 'number') return `不明（${t.unknown}件）`;
  return `${reviewAmount(p, t.value)}（${t.count}件${t.unknown ? `、ほかに不明 ${t.unknown}件` : ''}）`;
}
function usageReviewCells(r: UsageReviewRound, p: string) {
  const c = r.comparison;
  const cmp = c?.providers?.[p];
  const treatments = r.treatmentIssues.length
    ? r.treatmentIssues
        .map(
          (t) =>
            `${reviewIssue(t)}${t.mergedAt ? `（マージ ${reviewTime(t.mergedAt)}）` : t.closedAt ? '（マージせずに終了）' : '（マージ未確認）'}`,
        )
        .join('、')
    : 'なし';
  // 実行先ごとの判定（比較待ち・費用だけの回は回の判定）。
  const verdict = r.verdict.providers?.[p] || r.verdict;
  return [
    `第${r.round}回`,
    r.startDate || '?',
    reviewIssue(r.investigationIssue),
    treatments,
    p,
    r.cost ? usageReviewCostText(p, r.cost[p]) : '記録なし',
    c
      ? `前 ${reviewTime(c.before.from)}〜${reviewTime(c.before.to)}（${c.before.days}日）／後 ${reviewTime(c.after.from)}〜${reviewTime(c.after.to)}（${c.after.days.toFixed(1)}日）`
      : '—',
    cmp
      ? `${reviewAmount(p, cmp.before.perDay, false)} → ${reviewAmount(p, cmp.after.perDay, false)}（差 ${reviewAmount(p, cmp.diffPerDay, false)}）${p === 'codex' ? '（重み付き）' : ''}${r.verdict.result === 'waiting' ? `。取得済み: 前 ${cmp.before.count}件／後 ${cmp.after.count}件` : ''}`
      : '—',
    cmp && r.verdict.result !== 'waiting' ? reviewAmount(p, cmp.saved) : '—',
    `${usageReviewVerdictNames[verdict.result] || verdict.result}（${verdict.reason}）`,
  ];
}
function usageReviewResult(r: StatsReport) {
  if (!r.usageReview) return [];
  const u = r.usageReview;
  const rounds = u.rounds || [];
  return [
    el('h2', { text: '利用量の見直し' }),
    ...(u.reason ? [el('div', { class: 'error-box', text: u.reason })] : []),
    ...(rounds.length
      ? [
          statsTable(
            usageReviewHeaders,
            rounds.flatMap((x) =>
              ['claude', 'codex'].map((p) => usageReviewCells(x, p)),
            ),
            'usage-review',
          ),
          el(
            'ul',
            { class: 'stats-coverage' },
            ...rounds.map((x) =>
              el('li', {
                text: `第${x.round}回: ${usageReviewVerdictNames[x.verdict.result] || x.verdict.result}（${x.verdict.reason}）${x.acknowledgedAt ? `。確認済み（${reviewTime(x.acknowledgedAt)}）` : ''}${x.note ? `。${x.note}` : ''}`,
              }),
            ),
          ),
          // 速さ・難易度の前後比較（Issue #614。client/src/ui/app-usage-review-speed.js）。
          ...usageReviewSpeedResult(rounds),
        ]
      : [
          el('p', {
            class: 'stats-coverage',
            text: '利用量の見直しの回はまだありません。',
          }),
        ]),
  ];
}
// 要対応の「利用量の見直し 第N回: 見直しの費用が減った量を上回っています」を開いた画面。稼働実績へのリンクと
// 「確認済みにする」（受付画面のセッションだけ。PC用の一時キー・トークン接続は受付が断る）。
function usageReviewAlertView(alert: RequestRow) {
  reset();
  current = { type: 'usage-review' };
  const done = el('button', {
    type: 'button',
    class: 'primary',
    text: '確認済みにする',
  });
  done.onclick = () =>
    action(done, async () => {
      await api(`/api/usage-review/rounds/${alert.round}/acknowledge`, {});
      toast(`第${alert.round}回を確認済みにしました。`);
      // 保持した一覧からも外し、次に一覧を開いたときに取り直させる（確認済みの回は再び出さない）。
      if (listCache)
        keepList({
          ...listCache,
          at: 0,
          reviews: (listCache.reviews || []).filter(
            (x) => x.round !== alert.round,
          ),
        });
      home();
    });
  $('#app').replaceChildren(
    el('button', { class: 'back', onClick: () => home(), text: '← 依頼一覧' }),
    head('USAGE REVIEW', alert.title ?? '', guidance.usage_review_alert),
    el('p', { text: `判定: ${alert.reason || '記録なし'}` }),
    el('p', {
      text: `調査Issue: ${reviewIssue(alert.investigationIssue)}（開始 ${alert.startDate || '?'}）`,
    }),
    el(
      'div',
      { class: 'actions' },
      el('button', {
        type: 'button',
        onClick: () => statsView(),
        text: '稼働実績で確かめる',
      }),
      done,
    ),
  );
}
// 相談の難易度と結末（tiersResult）は client/src/ui/app-tier-outcomes.js（Issue #570 で分けた）。
async function statsView(navigate: Navigation = true) {
  if (navigate !== false) remember('#/stats', navigate === 'replace');
  const id = reset();
  current = { type: 'stats' };
  const project = el(
    'select',
    { 'aria-label': 'プロジェクト' },
    el('option', { value: '', text: '全リポジトリ' }),
    ...(meta.projects || []).map((p) =>
      el('option', { value: p.id, text: projectOptionName(p) }),
    ),
  );
  const dates = Object.fromEntries(
    ['beforeFrom', 'beforeTo', 'afterFrom', 'afterTo'].map((k) => [
      k,
      el('input', { type: 'date' }),
    ]),
  );
  const run = el('button', {
    type: 'button',
    class: 'primary',
    text: '集計する',
  });
  const result = el(
    'div',
    { id: 'stats-result' },
    el('div', { class: 'loading', text: '記録を集計しています…' }),
  );
  const notes = el(
    'dl',
    {},
    ...[
      ...workStatsNotes,
      ...usageStatsNotes,
      ...usageDbStatsNotes,
      ...reworkStatsNotes,
      ...usageReviewNotes,
      ...usageReviewSpeedNotes,
      ...antigravityStatsNotes,
      ...issueBodyStatsNotes,
      ...tierOutcomeNotes,
      ...implementationTierNotes,
      ...followupStatsNotes,
      ...scopeStatsNotes,
      ...workTypeStatsNotes,
      ...stallStatsNotes,
      ...inventoryStatsNotes,
      ...stopCheckStatsNotes,
    ].flatMap(([term, text]) => [el('dt', { text: term }), el('dd', { text })]),
  );
  let priorityNotesAdded = false;
  const period = (label: string, from: HTMLElement, to: HTMLElement) =>
    el(
      'fieldset',
      {},
      el('legend', { text: label }),
      field('開始日', from),
      field('終了日（この日を含む）', to),
    );
  $('#app').replaceChildren(
    el('button', { class: 'back', onClick: () => home(), text: '← 依頼一覧' }),
    head(
      'STATS',
      '稼働実績',
      '先頭の運用の概要で、全PCの実装件数・工程の使用量・相談と所要時間を確認できます。詳しい比較では、このPCの監視の記録とAIの使用量を前後の期間で比べられます。',
      // スマホ幅では依頼一覧の見出しに「モデルの切り替え」を出さないので、ここを入口にする（Issue #469）。
      el(
        'div',
        { class: 'heading-actions' },
        el('button', {
          class: 'quiet models-link',
          onClick: () => modelsView(),
          text: 'モデルの切り替え',
        }),
        el('button', {
          class: 'quiet tokens-link',
          onClick: () => tokensView(),
          text: 'PC別トークン',
        }),
      ),
    ),
    opsOverview(id),
    el(
      'div',
      { class: 'stats-form' },
      field('プロジェクト', project),
      period('前の期間', dates.beforeFrom, dates.beforeTo),
      period('後の期間', dates.afterFrom, dates.afterTo),
      run,
    ),
    result,
    el(
      'section',
      { class: 'stats-notes' },
      el('h2', { text: '集計の定義' }),
      notes,
    ),
  );
  function show(selection: Record<string, string>) {
    selectOption(project, selection.project);
    for (const [k, node] of Object.entries(dates)) node.value = selection[k];
  }
  async function load(selection: Record<string, string> | null) {
    const query = `?${new URLSearchParams({ ...(selection || {}), details: 'true' })}`;
    const report = await api<StatsReport>(`/api/stats${query}`);
    if (!isCurrent(id)) return;
    if (report.priorityReview && !priorityNotesAdded) {
      for (const [term, text] of priorityReviewStatsNotes)
        notes.append(el('dt', { text: term }), el('dd', { text }));
      priorityNotesAdded = true;
    }
    statsSelection = {
      project: report.project,
      beforeFrom: report.before.fromDate,
      beforeTo: report.before.toDate,
      afterFrom: report.after.fromDate,
      afterTo: report.after.toDate,
    };
    show(statsSelection);
    result.replaceChildren(...statsResult(report));
  }
  run.onclick = () =>
    action(run, () =>
      load({
        project: project.value,
        ...Object.fromEntries(
          Object.entries(dates).map(([k, node]) => [k, node.value]),
        ),
      }),
    );
  if (statsSelection) show(statsSelection);
  try {
    await load(statsSelection);
  } catch (caughtValue) {
    const e = uiError(caughtValue);
    if (!isCurrent(id)) return;
    error(e);
    result.replaceChildren(
      el('div', {
        class: 'empty',
        text: '稼働実績を集計できませんでした。条件を確かめて「集計する」を押し直してください。',
      }),
    );
  }
}
