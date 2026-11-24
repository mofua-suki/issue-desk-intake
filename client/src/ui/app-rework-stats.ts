// 稼働実績の「実装の手戻りの内訳」（Issue #786。集計は shared/src/rework-stats.mjs、定義は docs/ai-usage-aggregation.md『Aggregation definitions』）。
// client/src/ui/app.js の一部で、単独のモジュールではない（client/scripts/build.ts の順に app.js とつなぐ）。rework の無い応答
// （更新前の受付）では出さない。値の無い項目は「不明（N件）」で、ゼロとは出さない（client/src/ui/request-list.js の usageCells）。
const reworkPhaseLabels: Record<string, string> = {
  first: '最初の実装',
  fix: '修正ターン',
  return: '差し戻し後',
  conflict: '競合の解消',
  repair: '固定テストの修正',
  unknown: '不明',
};
const reworkTriggerLabels: Record<string, string> = {
  'review-return': '自動差し戻し',
  'hold-instruction': '保留後の修正指示',
  retry: '再試行',
  answer: '質問への回答',
  unknown: '不明',
};
const reworkKindLabels: Record<string, string> = {
  data: 'データ損失・破損',
  security: 'セキュリティ・認証',
  regression: '回帰',
  acceptance: '受入条件の未達',
  blocking: '操作不能',
  other: 'その他',
  unknown: '不明',
};
const reworkStatsNotes = [
  [
    '手戻りの内訳の区分',
    'このPCの runtime/usage/ の実装・固定テストの修正・競合解消の行を分けます。最初の実装は実装の generation 1・turn 1（turn の無い Codex の行は generation 1）、修正ターンは generation 1・turn 2以降（staged・依存・固定テストの修正の往復）、固定テストの修正は Codex の固定テストの修正の工程の行（世代・ターンを問わない）、差し戻し後は generation 2以降（自動差し戻しに限らず、再試行・質問への回答なども含む2回目以降の実行）、競合の解消は競合解消の工程です。generation の無い実装の行などは「不明」に数えます。差し引きの節約の見積もりは GET /api/stats の rework.before.fixedTestRepair・rework.after.fixedTestRepair で読めます。費用は使用量の表と同じ補正を通した値です。実装1件あたりは、期間内に実装・固定テストの修正・競合解消の行のあるIssueの数で割った値で、値の無い行は割らずに「ほかに不明 N件」と示します。',
  ],
  [
    '差し戻し後のきっかけ',
    '2回目以降の実行が始まったきっかけです。着手のときの追加の指示と前の実行の状態から決めて使用量の行に残します。自動差し戻し（独立レビューの指摘）・保留後の修正指示（レビュー保留の後の指示）・再試行（/codex retry・resume・run、失敗の後の指示、コントローラーの自動再試行）・質問への回答・不明（決められないもの、この項目を記録する前の行）に分けます。',
  ],
  [
    '自動差し戻しの指摘の種類',
    '自動差し戻しで始まった実行の行を、その差し戻しの独立レビューの指摘の種類（findingKinds）で数えます。1回の差し戻しに複数の種類があれば各種類に数えるので、種類別の合計は自動差し戻しの合計より大きくなることがあります（重複あり）。種類の記録が無い行は「不明」です。',
  ],
  [
    '差し戻しの割合',
    'このPCの runner.log で、期間内に独立レビューを始めた（または自動差し戻しのあった）Issueのうち、期間内に自動差し戻しが1回以上あったIssueの件数と割合です。記録の始まりより前の期間は「記録なし」です。',
  ],
];
const reworkNone = (label: string) => [label, '—', '—', '—', '—', '—', '—'];
// 区分・きっかけ・種類の行。行の無い区分は「0件」で、値の欄は「—」（不明（0件）とは出さない）。
const reworkCells = (label: string, t: ReworkTotal) =>
  t.count ? usageCells(label, t) : [label, '0件', '—', '—', '—', '—', '—'];
function reworkRateText(x: ReturnRate | undefined) {
  if (!x) return '記録なし';
  if (x.reason) return `記録なし（${x.reason}）`;
  const since = x.since ? `（記録の始まり ${x.since} 以後）` : '';
  if (!x.reviewed) return `独立レビューなし${since}`;
  return `${(x.rate * 100).toFixed(1)}%（${x.returned}件／${x.reviewed}件）${since}`;
}
// 実装1件あたりの行。件数は小数1桁、項目の値は usageCells の書き方（不明の件数は割らない）。
function reworkPerIssueCells(label: string, t: ReworkTotal) {
  if (!t.count) return reworkCells(label, t);
  if (!t.perIssue) return reworkNone(label);
  return [
    label,
    `${t.perIssue.count.toFixed(1)}件`,
    ...usageCells(label, t.perIssue).slice(2),
  ];
}
// 前後の比較の表の行（項目、前、後）。
function reworkCompareRows(periods: ReworkPeriod[]) {
  const cost = (p: ReworkPeriod, phase: string | null) => {
    if (!p.usage) return '記録なし';
    const t = phase
      ? p.usage.phases.find((x) => x.phase === phase)
      : p.usage.total;
    if (!t?.perIssue) return '—（実装の記録なし）';
    return t.count ? usageCells('', t.perIssue)[6] : '—（この区分の記録なし）';
  };
  return [
    [
      '実装したIssue',
      ...periods.map((p) => (p.usage ? `${p.usage.issues}件` : '記録なし')),
    ],
    ['差し戻しの割合', ...periods.map((p) => reworkRateText(p.returns))],
    ...['first', 'fix', 'return', 'conflict', 'repair'].map((phase) => [
      `${reworkPhaseLabels[phase]}（1件あたりの費用の目安）`,
      ...periods.map((p) => cost(p, phase)),
    ]),
    ['合計（1件あたりの費用の目安）', ...periods.map((p) => cost(p, null))],
  ];
}
function reworkPeriodView(label: string, p: ReworkPeriod) {
  const title = el('h2', {
    text: `${label}の期間の実装の手戻りの内訳（${p.fromDate}〜${p.toDate}）`,
  });
  const u = p.usage;
  if (!u)
    return [
      title,
      el('p', {
        class: 'stats-coverage',
        text: '記録なし（理由は上のとおり）',
      }),
    ];
  if (!u.total.count)
    return [
      title,
      el('p', {
        class: 'stats-coverage',
        text: 'この期間に実装・固定テストの修正・競合解消の使用量の記録がありません。',
      }),
    ];
  const phaseLabel = (x: ReworkTotal) =>
    reworkPhaseLabels[x.phase ?? ''] ?? '不明';
  return [
    title,
    el('p', {
      class: 'stats-coverage',
      text: `実装したIssue ${u.issues}件（実装・固定テストの修正・競合解消の行 ${u.total.count}件）。差し戻しの割合: ${reworkRateText(p.returns)}`,
    }),
    el('h3', { text: '区分別' }),
    statsTable(
      ['区分', ...usageHeaders],
      [
        ...u.phases.map((x) => reworkCells(phaseLabel(x), x)),
        usageCells('合計', u.total),
      ],
      'rework-phases',
    ),
    el('h3', { text: '区分別（実装1件あたり）' }),
    statsTable(
      ['区分', ...usageHeaders],
      [
        ...u.phases.map((x) => reworkPerIssueCells(phaseLabel(x), x)),
        reworkPerIssueCells('合計', u.total),
      ],
      'rework-per-issue',
    ),
    el('h3', { text: '差し戻し後のきっかけ別' }),
    statsTable(
      ['きっかけ', ...usageHeaders],
      u.triggers.length
        ? u.triggers.map((x) =>
            usageCells(reworkTriggerLabels[x.trigger ?? ''] ?? '不明', x),
          )
        : [reworkNone('記録なし（差し戻し後の実行がありません）')],
      'rework-triggers',
    ),
    el('h3', { text: '自動差し戻しの指摘の種類別（重複あり）' }),
    el('p', {
      class: 'stats-coverage',
      text: `自動差し戻しで始まった実行の行 ${u.autoReturns}件。1回の差し戻しに複数の種類があれば各種類に数えるので、種類別の合計はこの件数と一致しないことがあります。`,
    }),
    statsTable(
      ['指摘の種類', ...usageHeaders],
      u.kinds.length
        ? u.kinds.map((x) =>
            usageCells(reworkKindLabels[x.kind ?? ''] ?? '不明', x),
          )
        : [reworkNone('記録なし（自動差し戻しの実行がありません）')],
      'rework-kinds',
    ),
  ];
}
function reworkStatsResult(r: StatsReport) {
  if (!r.rework) return implementationStatsResult(r);
  const w = r.rework;
  const errors = (w.unavailable || []).map((text) =>
    el('div', { class: 'error-box', text }),
  );
  const title = el('h2', { text: '実装の手戻りの内訳（前後の比較）' });
  if (!w.before || !w.after)
    return [title, ...errors, ...implementationStatsResult(r)];
  const periods: [string, ReworkPeriod][] = [
    ['前', w.before],
    ['後', w.after],
  ];
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
      reworkCompareRows(periods.map(([, p]) => p)),
      'rework-compare',
    ),
    ...periods.flatMap(([label, p]) => reworkPeriodView(label, p)),
    ...implementationStatsResult(r),
  ];
}

// GET /api/stats の共有DB実装集計。手戻り集計とは独立して描画し、旧受付の応答では何も追加しない。
const implementationReasonLabels: Record<string, string> = {
  success: '成功',
  'fixed-test': '固定テスト失敗',
  checks: '検査失敗',
  timeout: '時間切れ',
  question: '質問',
  prerequisite: '前提不足',
  guard: 'ガード停止',
  'usage-limit': '利用上限',
  'review-hold': 'レビュー保留',
  stopped: '停止',
  'lease-expired': '実装リース失効',
  unknown: '分類不能',
};
// 追加した分類は旧受付の応答にない場合だけ0件とする。
function implementationDayCount(reasons: Record<string, number>, key: string) {
  const added = key === 'stopped' || key === 'lease-expired';
  return added && !Object.hasOwn(reasons, key) ? 0 : reasons[key];
}
function implementationStatsResult(r: StatsReport) {
  if (!Object.hasOwn(r, 'implementation')) return [];
  const value = r.implementation;
  const title = el('h2', {
    text: '実装の終了理由と実装回数の分布（前後の比較）',
  });
  if (!value)
    return [
      title,
      el('p', {
        class: 'stats-coverage',
        text: '実装集計を取得できません（理由は上のとおり）。',
      }),
    ];
  return [
    title,
    ...(
      [
        ['前', value.before],
        ['後', value.after],
      ] as const
    ).flatMap(([label, p], index) => {
      const range = index === 0 ? r.before : r.after;
      const content = [
        el('h3', {
          text: `${label}の期間の日別終了理由（${range.fromDate}〜${range.toDate}）`,
        }),
      ];
      if (p.runs === 0 && p.noImplementationRecord > 0)
        content.push(
          el('p', {
            class: 'stats-coverage',
            text: `実装記録なし（対象Issue ${p.noImplementationRecord}件）。`,
          }),
        );
      else
        content.push(
          el('p', {
            class: 'stats-coverage',
            text: `実装終了 ${p.runs}件。分類不能 ${p.reasons.unknown}件、対応不能な実行 ${p.unknownGenerations}件。`,
          }),
        );
      content.push(
        statsTable(
          ['日付', ...Object.values(implementationReasonLabels)],
          p.days.map((day) => [
            day.day,
            ...Object.keys(implementationReasonLabels).map(
              (key) => `${implementationDayCount(day.reasons, key)}件`,
            ),
          ]),
          'implementation-days',
        ),
      );
      content.push(
        el('h3', {
          text: `${label}の期間のIssueごとの lease ごとの実装の実行回数の分布`,
        }),
      );
      content.push(
        statsTable(
          ['lease ごとの実装の実行回数', 'Issue数'],
          [
            ...p.distribution.map((entry) => [
              `${entry.implementations}回`,
              `${entry.issues}件`,
            ]),
            ...(p.unknownIssues ? [['回数不明', `${p.unknownIssues}件`]] : []),
            ...(p.distribution.length === 0 && p.unknownIssues === 0
              ? [
                  [
                    p.skipped ? '読取不能' : '記録なし',
                    p.skipped ? `${p.skipped}件` : '—',
                  ],
                ]
              : []),
          ],
          'implementation-distribution',
        ),
      );
      if (Array.isArray(p.uc1Distribution)) {
        content.push(
          el('h3', {
            text: `${label}の期間のIssueごとの UC1 の実装回数（上限判定と同じ数え方）の分布`,
          }),
        );
        content.push(
          statsTable(
            ['実装回数（UC1）', 'Issue数'],
            [
              ...p.uc1Distribution.map((entry) => [
                `${entry.implementations}回`,
                `${entry.issues}件`,
              ]),
              ...(p.uc1UnknownIssues
                ? [['回数不明', `${p.uc1UnknownIssues}件`]]
                : []),
              ...(p.uc1Distribution.length === 0 && p.uc1UnknownIssues === 0
                ? [
                    [
                      p.skipped ? '読取不能' : '記録なし',
                      p.skipped ? `${p.skipped}件` : '—',
                    ],
                  ]
                : []),
            ],
            'implementation-uc1-distribution',
          ),
        );
        if (typeof p.implementationMismatchIssues === 'number')
          content.push(
            el('p', {
              class: 'stats-coverage',
              text: `両方の回数が食い違うIssue ${p.implementationMismatchIssues}件。`,
            }),
            el('p', {
              class: 'stats-coverage',
              text: '同じ世代での引き継ぎ・やり直し等による実行回数と現在のUC1の回数が異なる依頼の数なので、期間・プロジェクトを絞って確かめてください。',
            }),
          );
      }
      if (p.skipped)
        content.push(
          el('p', {
            class: 'stats-coverage',
            text: `読取不能な使用量記録 ${p.skipped}件。`,
          }),
        );
      return content;
    }),
  ];
}
