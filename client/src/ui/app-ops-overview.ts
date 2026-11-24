// 運用の概要。app.js と同じスコープで組み立てる数値表と画面。SVGグラフの部品は client/src/ui/app-ops-charts.js。
const opsNumber = (value: number | null | undefined, unit = '') =>
  value == null
    ? '欠測'
    : `${Number(value).toLocaleString('ja-JP', { maximumFractionDigits: 2 })}${unit}`;
const opsName = (value: unknown) =>
  value == null || value === '' || value === 'unknown'
    ? '記録なし'
    : String(value);
const opsProjectName = (value: unknown) =>
  shownProject(opsName(value)) ?? '記録なし';
const opsTier = (value: string | undefined) =>
  (
    ({ light: '低', standard: '中', heavy: '高', fixed: '固定値' }) as Record<
      string,
      string
    >
  )[value ?? ''] || opsName(value);
// 使用量の工程名を大まかな段階へまとめる。APIは工程名のまま返し、画面だけで束ねる。
type OpsStepRow = NonNullable<OpsReport['tokens']>['steps'][number];
type OpsModelRow = NonNullable<OpsReport['models']>[number];
const opsStageOrder = [
  '相談',
  '実装',
  'レビュー',
  'コンフリクト解消',
  'その他',
];
const opsStageOf: Record<string, string> = {
  受付の相談: '相談',
  相談の下書き: '相談',
  受付の相談の分割: '相談',
  起票直前の重複と依存の確認: '相談',
  filingCheck: '相談',
  代理の回答: '相談',
  利用者への質問の代理確認: '相談',
  受付の始めの判定: '相談',
  段の判定: '相談',
  起票時の変更範囲の見積もり: '相談',
  変更範囲の後付け: '相談',
  クローズ依頼の確認: '相談',
  優先度の判定: '相談',
  実装: '実装',
  実装報告補完: '実装',
  固定テストの修正: '実装',
  調査: '実装',
  独立レビュー: 'レビュー',
  注意点の更新: 'レビュー',
  競合解消: 'コンフリクト解消',
};
const opsStage = (step: unknown) => opsStageOf[String(step ?? '')] ?? 'その他';
const opsStageRank = (stage: string | undefined) =>
  opsStageOrder.indexOf(stage ?? 'その他');
const opsAdd = (a: number | null | undefined, b: number | null | undefined) =>
  a == null && b == null ? null : (a ?? 0) + (b ?? 0);
function opsTokenStages(steps: OpsStepRow[]): OpsStepRow[] {
  const stages = new Map<string, OpsStepRow>();
  for (const row of steps) {
    const stage = opsStage(row.step);
    const old = stages.get(stage);
    stages.set(
      stage,
      old
        ? {
            step: stage,
            weighted: opsAdd(old.weighted, row.weighted),
            percent: opsAdd(old.percent, row.percent),
            unknown: opsAdd(old.unknown, row.unknown),
          }
        : {
            step: stage,
            weighted: row.weighted,
            percent: row.percent,
            unknown: row.unknown,
          },
    );
  }
  return [...stages.values()].sort(
    (a, b) => opsStageRank(a.step) - opsStageRank(b.step),
  );
}
function opsModelStages(models: OpsModelRow[]): OpsModelRow[] {
  const groups = new Map<string, OpsModelRow>();
  const totals = new Map<string, number>();
  for (const row of models) {
    const stage = opsStage(row.step);
    const key = JSON.stringify([
      stage,
      row.provider ?? null,
      row.model ?? null,
      row.effort ?? null,
    ]);
    const old = groups.get(key);
    groups.set(key, {
      ...row,
      step: stage,
      count: opsAdd(old?.count, row.count),
    });
    totals.set(stage, (totals.get(stage) ?? 0) + (row.count ?? 0));
  }
  return [...groups.values()]
    .map((row) => {
      const total = totals.get(row.step ?? '') ?? 0;
      return {
        ...row,
        percent:
          row.count == null || total === 0 ? null : (row.count / total) * 100,
      };
    })
    .sort(
      (a, b) =>
        opsStageRank(a.step) - opsStageRank(b.step) ||
        (b.count ?? 0) - (a.count ?? 0),
    );
}
function opsOverviewResult(report: OpsReport) {
  const sections = [];
  if ('implementations' in report) {
    const data = report.implementations;
    const content =
      data == null
        ? [el('p', { text: '欠測' })]
        : (['byHost', 'byProject'] as const).flatMap((axis) => {
            if (!(axis in data)) return [];
            if (data[axis] == null) return [el('p', { text: '欠測' })];
            const rows = data[axis].map((row) => ({
              ...row,
              label:
                axis === 'byProject'
                  ? opsProjectName(row.project)
                  : opsName(row.host),
            }));
            return [
              el('h4', { text: axis === 'byHost' ? 'PC別' : 'プロジェクト別' }),
              ...opsBars('実装件数', rows, ['completed']),
              statsTable(
                ['PC / プロジェクト', '完了'],
                rows.map((row) => [row.label, opsNumber(row.completed)]),
              ),
            ];
          });
    sections.push(
      opsSection(
        'PC別・プロジェクト別の実装件数',
        '期間内に完了（マージ）へ達したジョブを、同じジョブにつき1回数えます。',
        opsLegend(['完了']),
        ...content,
      ),
    );
  }
  if ('implementationTiers' in report || 'consultTiers' in report) {
    const panels = (['implementationTiers', 'consultTiers'] as const).flatMap(
      (key) => {
        if (!(key in report)) return [];
        const title =
          key === 'implementationTiers' ? '実装の難易度' : '相談の難易度';
        if (report[key] == null)
          return [{ title, content: [el('p', { text: '欠測' })] }];
        const rows = report[key].map((row) => ({
          label: opsTier(row.tier),
          value: row.count,
        }));
        return [
          {
            title,
            content: [
              opsColumns(title, rows),
              opsLegend(rows.map((row) => row.label)),
              statsTable(
                ['難易度', '件数'],
                rows.map((row) => [row.label, opsNumber(row.value)]),
              ),
            ],
          },
        ];
      },
    );
    sections.push(
      opsSection(
        '実装の難易度・相談の難易度ごとの件数',
        '実装は期間内に最初に着手したジョブの難易度、相談は期間内に作成した相談で最後に実行した難易度を数えます。未実行や記録のない難易度は記録なしです。',
        opsTabs('難易度の種類', panels),
      ),
    );
  }
  if ('tokens' in report) {
    const data = report.tokens;
    const stages = data == null ? [] : opsTokenStages(data.steps);
    sections.push(
      opsSection(
        '段階ごとのトークンの割合',
        '全PCの使用量の既知の重み付きトークン量に対する段階別の割合です。工程は相談・実装・レビュー・コンフリクト解消にまとめ、運用の見張りなど当てはまらない工程はその他にします。不明な量は除外します。',
        ...(data == null
          ? [el('p', { text: '欠測' })]
          : [
              opsDonut(stages),
              opsLegend(
                stages.map(
                  (row) =>
                    `${opsName(row.step)}: ${opsNumber(row.percent, '%')}`,
                ),
              ),
              statsTable(
                ['段階', '重み付きトークン', '割合', '量が不明の行'],
                stages.map((row) => [
                  opsName(row.step),
                  opsNumber(row.weighted),
                  opsNumber(row.percent, '%'),
                  opsNumber(row.unknown),
                ]),
              ),
              el('p', {
                text: `合計: ${opsNumber(data.weighted)} / 量が不明の行: ${opsNumber(data.unknown)}`,
              }),
            ]),
      ),
    );
  }
  if ('models' in report) {
    const data = report.models == null ? null : opsModelStages(report.models);
    const content =
      data == null
        ? [el('p', { text: '欠測' })]
        : [...new Set(data.map((row) => row.step))].flatMap((step) => {
            const rows = data.filter((row) => row.step === step);
            const keys = rows.map((_, index) => String(index));
            const bar: { label: string } & Record<
              string,
              string | number | null
            > = {
              label: opsName(step),
              ...Object.fromEntries(
                rows.map((row, index) => [String(index), row.percent]),
              ),
            };
            return [
              ...opsBars('実行先・モデル・推論レベル', [bar], keys, true),
              opsLegend(
                rows.map(
                  (row) =>
                    `${opsName(row.provider)} / ${opsName(row.model)} / ${opsName(row.effort)}: ${opsNumber(row.percent, '%')}`,
                ),
              ),
              statsTable(
                [
                  '段階',
                  '実行先',
                  'モデル',
                  '推論レベル',
                  '呼び出し件数',
                  '割合',
                ],
                rows.map((row) => [
                  opsName(step),
                  opsName(row.provider),
                  opsName(row.model),
                  opsName(row.effort),
                  opsNumber(row.count),
                  opsNumber(row.percent, '%'),
                ]),
              ),
            ];
          });
    sections.push(
      opsSection(
        '段階ごとの実行先・モデル・推論レベル',
        '使用量の1行を1呼び出しとし、段階内の全呼び出し件数を100%とします。トークン量が不明の行も含みます。',
        ...content,
      ),
    );
  }
  if ('roundTrips' in report) {
    const data = report.roundTrips;
    const rows =
      data == null
        ? []
        : [
            { label: '0', value: data.zero },
            ...data.distribution.map((row) => ({
              label: row.rounds,
              value: row.count,
            })),
          ];
    sections.push(
      opsSection(
        '相談の往復',
        '期間内に作成した相談のassistant発言数を往復として数えます。未起票も含み、分割の子と後続案を除きます。',
        ...(data == null
          ? [el('p', { text: '欠測' })]
          : [
              opsColumns('相談の往復の分布', rows),
              opsLegend(rows.map((row) => String(row.label))),
              statsTable(
                ['往復', '相談件数'],
                rows.map((row) => [String(row.label), opsNumber(row.value)]),
              ),
              el('p', {
                text: `平均: ${opsNumber(data.average, '往復')} / 往復が多い相談（${data.threshold}回以上）: ${opsNumber(data.many, '件')} / 欠測: ${opsNumber(data.missing, '件')}`,
              }),
            ]),
      ),
    );
  }
  if ('durations' in report) {
    const names: Record<string, string> = {
      consultToFiled: '相談→起票',
      filedToLeased: '起票→着手',
      leasedToPr: '着手→PR作成',
      review: '独立レビュー',
      prToMerged: 'PR作成→マージ',
      mergedToDeployed: 'マージ→配備',
    };
    const rows =
      report.durations == null
        ? []
        : Object.entries(report.durations).map(([key, row]) => ({
            label: names[key] || key,
            averageMs: row?.averageMs,
            medianMs: row?.medianMs,
            count: row?.count,
            missing: row?.missing,
            excluded: row?.excluded,
          }));
    const longest = rows
      .filter((row) => row.averageMs != null)
      .sort((a, b) => (b.averageMs ?? 0) - (a.averageMs ?? 0))[0];
    sections.push(
      opsSection(
        '区間の所要時間',
        '区間の終端が期間内にある記録の平均・中央値です。端のない記録は欠測、配備履歴がないプロジェクトは対象外です。独立レビューは使用量の1行を1回とした実行時間です。マージ→配備は、同じマージかその後のマージの版が初めて配備（Workerは最初のPCへの反映、受付は切り戻されなかった切り替え）された時刻までです。Workerと受付は配備の記録が始まる前のマージを対象外にし、依頼の記録が無い版（直接PRなど）が間に配備されたマージは欠測にします。単位は秒です。',
        ...(report.durations == null
          ? [el('p', { text: '欠測' })]
          : [
              ...opsBars('区間の平均所要時間', rows, ['averageMs']),
              statsTable(
                [
                  '区間',
                  '平均（秒）',
                  '中央値（秒）',
                  '測定件数',
                  '欠測件数',
                  '対象外',
                ],
                rows.map((row) => [
                  row.label,
                  opsNumber(
                    row.averageMs == null ? null : row.averageMs / 1000,
                  ),
                  opsNumber(row.medianMs == null ? null : row.medianMs / 1000),
                  opsNumber(row.count),
                  opsNumber(row.missing),
                  row.excluded === undefined ? '—' : opsNumber(row.excluded),
                ]),
              ),
              el('p', {
                text: longest
                  ? `最も長い区間（平均）: ${longest.label}（${opsNumber((longest.averageMs ?? 0) / 1000, '秒')}）`
                  : '最も長い区間: 欠測',
              }),
            ]),
      ),
    );
  }
  if ('daily' in report) {
    const rows = report.daily;
    const content =
      rows == null
        ? [el('p', { text: '欠測' })]
        : [
            opsLines('日ごとの完了・失敗', rows, ['completed', 'failed']),
            opsLegend(['完了', '失敗']),
            statsTable(
              ['日付', '完了', '失敗'],
              rows.map((row) => [
                row.date,
                opsNumber(row.completed),
                opsNumber(row.failed),
              ]),
            ),
          ];
    if (rows?.some((row) => 'tokens' in row))
      content.push(
        opsLines('日ごとのトークン量', rows, ['tokens']),
        opsLegend(['重み付きトークン']),
        statsTable(
          ['日付', '重み付きトークン'],
          rows.map((row) => [row.date, opsNumber(row.tokens)]),
        ),
      );
    sections.push(
      opsSection(
        '日ごとの推移',
        '日本時間の日付ごとの完了・失敗（実装件数と同じ定義）と既知の重み付きトークン量です。欠測の日で線を切ります。',
        ...content,
      ),
    );
  }
  return [
    ...(report.unavailable || []).map((reason) =>
      el('p', { class: 'error-box', text: reason }),
    ),
    ...sections,
  ];
}
function opsOverview(id: number) {
  const range = el(
    'select',
    { 'aria-label': '概要の期間' },
    ...[
      ['today', '今日'],
      ['7d', '7日'],
      ['30d', '30日'],
    ].map(([value, text]) =>
      el('option', { value, text, selected: value === '7d' }),
    ),
  );
  const project = el(
    'select',
    { 'aria-label': '概要のプロジェクト' },
    el('option', { value: '', text: '全リポジトリ' }),
    ...(meta.projects || []).map((item) =>
      el('option', { value: item.id, text: projectOptionName(item) }),
    ),
  );
  const result = el('div', { class: 'ops-results' });
  let generation = 0;
  async function load() {
    const request = ++generation;
    result.replaceChildren(el('p', { text: '概要を集計しています…' }));
    try {
      const report = await api<OpsReport>(
        `/api/stats/overview?${new URLSearchParams({ range: range.value, project: project.value })}`,
      );
      if (!isCurrent(id) || request !== generation) return;
      result.replaceChildren(...opsOverviewResult(report));
    } catch (caughtValue) {
      const e = uiError(caughtValue);
      if (!isCurrent(id) || request !== generation) return;
      result.replaceChildren(
        el('p', {
          class: 'error-box',
          text: `概要を集計できませんでした: ${e.message}`,
        }),
        el('button', {
          type: 'button',
          text: '概要を再読み込み',
          onClick: load,
        }),
      );
    }
  }
  range.onchange = load;
  project.onchange = load;
  void load();
  return el(
    'section',
    { class: 'ops-overview' },
    el('h2', { text: '運用の概要' }),
    el(
      'div',
      { class: 'ops-filters' },
      field('期間', range),
      field('プロジェクト', project),
    ),
    result,
    el('h2', { text: '詳しい比較（前後の期間）' }),
  );
}
