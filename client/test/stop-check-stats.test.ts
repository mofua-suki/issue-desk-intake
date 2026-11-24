import test from 'node:test';
import assert from 'node:assert/strict';
import vm from 'node:vm';
import fs from 'node:fs';
import path from 'node:path';
import { applicationSource, compileSource } from '../scripts/source.ts';
import { sharedClientBuild } from './shared-client-build.ts';

// 配信する結合ソースから画面描画関数を実行する。
const application = applicationSource();
const start = application.source.indexOf('type StopCheckRow =');
const end =
  application.source.indexOf(
    '\n}',
    application.source.indexOf('function stopCheckStatsResult(', start),
  ) + 2;
assert.ok(start >= 0 && end > start);
const source = compileSource(
  application.source.slice(start, end),
  'stop-check.ts',
);
type Node = { tag: string; props: Record<string, unknown>; children: Node[] };
const context = vm.createContext({
  el: (
    tag: string,
    props: Record<string, unknown>,
    ...children: Node[]
  ): Node => ({ tag, props, children }),
});
vm.runInContext(source, context);
const timeStart = application.source.indexOf('const reviewTime =');
const timeEnd = application.source.indexOf(
  '\nfunction usageReviewCostText',
  timeStart,
);
vm.runInContext(
  compileSource(application.source.slice(timeStart, timeEnd), 'review-time.ts'),
  context,
);
const render = (data: unknown): Node[] => context.stopCheckStatsResult(data);
const flatten = (nodes: Node[]): Node[] =>
  nodes.flatMap((node) => [node, ...flatten(node.children)]);
const row = {
  id: 'normal',
  kind: 'hook',
  purpose: '検査の狙い',
  introducedAt: '2026-11-05',
  nextReviewAt: '2026-12-05',
  overdue: false,
  total: 8,
  last30Days: 3,
  blocked: 5,
  warned: 3,
  lastFiredAt: '2026-11-06T00:00:00Z',
  reviewResult: '残す',
};

test('統計の台帳は超過行を先頭にし、件数と未見直し・集計対象外を表示する', () => {
  const nodes = flatten(
    render({
      stopChecks: {
        rows: [
          row,
          { ...row, id: 'late', overdue: true, reviewResult: null },
          {
            ...row,
            id: 'guard',
            kind: 'guard',
            total: null,
            last30Days: null,
            blocked: null,
            warned: null,
            lastFiredAt: null,
          },
        ],
        unknownCheckIds: ['unknown'],
      },
    }),
  );
  const rows = nodes.filter((node) => node.tag === 'tr').slice(1);
  assert.equal(rows[0].props.class, 'overdue');
  assert.deepEqual(
    Array.from(rows[0].children, (cell) => cell.props.text).slice(0, 2),
    ['見直し日超過', 'late'],
  );
  assert.ok(rows[0].children.some((cell) => cell.props.text === '未見直し'));
  assert.deepEqual(
    Array.from(rows[1].children, (cell) => cell.props.text).slice(1),
    [
      'normal',
      '検査の狙い',
      'hook',
      '8件',
      '3件',
      '5件',
      '3件',
      '2026-11-06 09:00',
      '2026-11-05',
      '2026-12-05',
      '残す',
    ],
  );
  assert.equal(
    rows[2].children.filter((cell) => cell.props.text === '記録なし').length,
    5,
  );
  assert.ok(nodes.some((node) => String(node.props.text).includes('unknown')));
  assert.ok(!nodes.some((node) => node.tag === 'button'));
});

test('台帳が欠落、null、または不正なら読み取れなかった旨を表示する', () => {
  for (const stopChecks of [
    undefined,
    null,
    {},
    { rows: [null] },
    { rows: [{ ...row, overdue: 'false' }] },
    { rows: [{ ...row, total: undefined }] },
    { rows: [{ ...row, lastFiredAt: 1 }] },
  ]) {
    const nodes = flatten(render({ stopChecks }));
    assert.ok(
      nodes.some(
        (node) => node.props.text === 'Stop検査の台帳を読めませんでした。',
      ),
    );
  }
  assert.ok(application.source.includes('...stopCheckStatsResult(r)'));
  assert.ok(application.parts.includes('app-stop-check-stats.ts'));
});

test('台帳を読めなくても統計の他の表を描画する', () => {
  const begin = application.source.indexOf('function statsResult(');
  const finish = application.source.indexOf('\n// フォローアップの実績', begin);
  assert.ok(begin >= 0 && finish > begin);
  const helpers = [
    'usageResult',
    'usageDbStatsResult',
    'reworkStatsResult',
    'reviewNotesStatsResult',
    'usageReviewResult',
    'priorityReviewStatsResult',
    'antigravityStatsResult',
    'issueBodyStatsResult',
    'tiersResult',
    'followupsResult',
    'scopeStatsResult',
    'workTypeStatsResult',
    'testSelectionStatsResult',
    'stallStatsResult',
    'inventoryStatsResult',
  ];
  for (const name of helpers) context[name] = () => [];
  context.workStatsRows = () => [['比較項目', '比較値']];
  context.statsTable = (_headers: string[], rows: unknown[][]) => ({
    tag: 'existing-table',
    props: { rows },
    children: [],
  });
  context.workDayHeaders = [];
  context.workDayCells = () => [];
  vm.runInContext(
    compileSource(application.source.slice(begin, finish), 'stats.ts'),
    context,
  );
  const period = { fromDate: '2026-11-01', toDate: '2026-11-07', days: [] };
  const nodes = flatten(
    context.statsResult({
      before: period,
      after: period,
      unavailable: [],
      stopChecks: null,
    }),
  );
  assert.ok(
    nodes.some(
      (node) =>
        node.tag === 'existing-table' &&
        JSON.stringify(node.props.rows).includes('比較値'),
    ),
  );
  assert.ok(
    nodes.some(
      (node) => node.props.text === 'Stop検査の台帳を読めませんでした。',
    ),
  );
});

test('サーバーの集計失敗は一度だけ表示し、他の統計は表示する', () => {
  const period = { fromDate: '2026-11-01', toDate: '2026-11-07', days: [] };
  const message = 'stopChecksの集計を読み取れませんでした。';
  const nodes = flatten(
    context.statsResult({
      before: period,
      after: period,
      unavailable: [message],
      stopChecks: null,
    }),
  );
  assert.equal(
    nodes.filter((node) => node.props.class === 'error-box').length,
    1,
  );
  assert.ok(nodes.some((node) => node.props.text === message));
  assert.ok(nodes.some((node) => node.tag === 'existing-table'));
});

function statsNotes(): Node[] {
  const begin = application.source.indexOf('async function statsView(');
  const finish = application.source.indexOf('\n}', begin) + 2;
  const names = [
    'workStatsNotes',
    'usageStatsNotes',
    'usageDbStatsNotes',
    'reworkStatsNotes',
    'usageReviewNotes',
    'usageReviewSpeedNotes',
    'antigravityStatsNotes',
    'issueBodyStatsNotes',
    'tierOutcomeNotes',
    'implementationTierNotes',
    'followupStatsNotes',
    'scopeStatsNotes',
    'workTypeStatsNotes',
    'stallStatsNotes',
    'inventoryStatsNotes',
  ];
  for (const name of names) context[name] = [];
  const notesStart = application.source.indexOf('const stallStatsNotes =');
  const notesEnd = application.source.indexOf(
    '\nfunction stallStatsDuration',
    notesStart,
  );
  assert.ok(notesStart >= 0 && notesEnd > notesStart);
  vm.runInContext(
    compileSource(
      '{\n' +
        application.source.slice(notesStart, notesEnd) +
        '\nthis.stallStatsNotes = stallStatsNotes;\n}',
      'stall-notes.ts',
    ),
    context,
  );
  let displayed: Node[] = [];
  Object.assign(context, {
    reset: () => 1,
    current: null,
    meta: {},
    statsSelection: null,
    $: () => ({
      replaceChildren: (...nodes: Node[]) => {
        displayed = nodes;
      },
    }),
    head: () => ({ tag: 'head', props: {}, children: [] }),
    opsOverview: () => ({ tag: 'overview', props: {}, children: [] }),
    field: () => ({ tag: 'field', props: {}, children: [] }),
    api: () => new Promise(() => {}),
    URLSearchParams,
  });
  vm.runInContext(
    compileSource(application.source.slice(begin, finish), 'stats-view.ts'),
    context,
  );
  void context.statsView(false);
  const section = flatten(displayed).find(
    (node) => node.props.class === 'stats-notes',
  );
  assert.ok(section);
  return flatten([section]);
}

test('稼働実績画面の集計の定義にStop検査の説明を表示する', () => {
  const nodes = statsNotes();
  assert.ok(
    nodes.some(
      (node) => node.tag === 'dt' && node.props.text === 'Stop検査の台帳',
    ),
  );
  const description = String(
    nodes.find(
      (node) =>
        node.tag === 'dd' && String(node.props.text).includes('見直し日'),
    )?.props.text,
  );
  for (const word of ['当日', '翌日', '集計対象外', '発動時刻', '未見直し'])
    assert.ok(description.includes(word));
});

function noteDescription(title: string): string {
  const nodes = statsNotes();
  const index = nodes.findIndex(
    (node) => node.tag === 'dt' && node.props.text === title,
  );
  assert.ok(index >= 0);
  assert.equal(nodes[index + 1]?.tag, 'dd');
  return String(nodes[index + 1].props.text);
}

test('全PCの同時実装の説明は実行記録と割合の対象を表示する', () => {
  const description = noteDescription('全PCの同時実装の割合');
  assert.doesNotMatch(description, /runner\.log/);
  assert.match(description, /実行記録|job_events/);
  for (const word of ['分子', '分母', '全リポジトリ'])
    assert.ok(description.includes(word));
});

test('記録なしの説明は実行記録と監視の欠測を表示する', () => {
  const description = noteDescription('止まりの実績の記録なし');
  assert.ok(!description.includes('件数の行'));
  assert.match(description, /実行記録|job_events/);
  for (const word of ['監視', '記録の無い時間'])
    assert.ok(description.includes(word));
});

test('配布される台帳の全行を表示し、配布CSSで超過行に色を付ける', () => {
  const ledger = JSON.parse(
    fs.readFileSync(
      new URL(
        '../../server/internal/statistics/stop-checks.json',
        import.meta.url,
      ),
      'utf8',
    ),
  );
  const rows = ledger.map((entry: any) => ({
    ...entry,
    overdue: true,
    total: entry.counted ? 0 : null,
    last30Days: entry.counted ? 0 : null,
    blocked: entry.counted ? 0 : null,
    warned: entry.counted ? 0 : null,
    lastFiredAt: null,
    reviewResult: entry.reviews.at(-1)?.result ?? null,
  }));
  const rendered = flatten(
    render({ stopChecks: { rows, unknownCheckIds: [] } }),
  )
    .filter((node) => node.tag === 'tr')
    .slice(1);
  assert.equal(rendered.length, ledger.length);
  for (const [index, entry] of ledger.entries()) {
    assert.equal(rendered[index].children[1].props.text, entry.id);
    assert.equal(rendered[index].children[2].props.text, entry.purpose);
    if (!entry.counted)
      assert.equal(rendered[index].children[4].props.text, '記録なし');
  }
  const { destination } = sharedClientBuild();
  const css = fs.readFileSync(path.join(destination, 'style.css'), 'utf8');
  assert.match(
    css,
    /\.stop-check-stats tr\.overdue td\s*\{[^}]*background\s*:/,
  );
  assert.match(
    fs.readFileSync(path.join(destination, 'app.js'), 'utf8'),
    /見直し日超過/,
  );
});
