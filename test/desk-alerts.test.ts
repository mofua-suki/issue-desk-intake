import { test } from 'node:test';
import assert from 'node:assert/strict';
import { deskAlerts, parseArgs, runCommand } from '../scripts/desk-alerts.ts';
import { cpSync, mkdirSync, mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { buildClient } from '../client/scripts/build.ts';

test('停止中の相談を items に出す', async () => {
  const call = async (_method, route) => {
    const path = new URL(route, 'http://local').pathname;
    if (path === '/api/drafts')
      return {
        items: [{ id: 'stopped', status: 'stalled' }],
        total: 1,
        page: 1,
        limit: 200,
        hasMore: false,
      };
    if (path === '/api/close-requests')
      return { items: [], total: 0, page: 1, limit: 200, hasMore: false };
    return [];
  };
  let output;
  assert.equal(
    await deskAlerts(call, { json: true }, (s) => {
      output = JSON.parse(s);
    }),
    0,
  );
  assert.equal(output.items.length, 1);
  assert.equal(output.items[0].id, 'stopped');
  assert.equal(output.monitoring.length, 0);
  assert.equal(output.summary.screenTotal, 1);
  assert.equal(output.summary.rawTotal, 1);
});

test('監視行に未確定と起票失敗の理由を付け既存分類を保つ', async () => {
  const now = Date.parse('2026-11-11T12:00:00Z');
  const old = '2026-11-11T11:00:00Z';
  const drafts = [
    {
      id: 'boundary',
      status: 'draft',
      ready: false,
      questions: [],
      updatedAt: old,
    },
    {
      id: 'older',
      status: 'draft',
      questions: [],
      updatedAt: '2026-11-11T10:00:00Z',
    },
    {
      id: 'recent',
      status: 'draft',
      questions: [],
      updatedAt: '2026-11-11T11:00:01Z',
    },
    { id: 'held', status: 'draft', hold: true, questions: [], updatedAt: old },
    {
      id: 'ready',
      status: 'draft',
      ready: true,
      questions: [],
      updatedAt: old,
    },
    { id: 'missing-time', status: 'draft', questions: [] },
    { id: 'question', status: 'draft', questions: ['質問'], updatedAt: old },
    {
      id: 'proposal',
      status: 'submit_error',
      error: '送信失敗\n詳細',
      splitIssues: [{ project: 'o/r' }],
    },
    {
      id: 'parent',
      status: 'submit_error',
      error: '親の送信失敗',
      splitChildren: ['child', 'child-failure', 'child-filed'],
      splitIssues: [{ project: 'o/r' }],
      issue: { number: 5 },
    },
    {
      id: 'replacement',
      status: 'submit_error',
      error: '置換失敗',
      replaceIssues: [{ number: 1 }],
    },
    { id: 'plain', status: 'submit_error', error: '通常失敗' },
    {
      id: 'child',
      parentSplit: 'parent',
      status: 'draft',
      questions: [],
      updatedAt: old,
    },
    {
      id: 'child-failure',
      parentSplit: 'parent',
      status: 'submit_error',
      error: '子の失敗',
    },
    {
      id: 'child-filed',
      parentSplit: 'parent',
      project: 'o/r',
      status: 'submitted',
      issue: { number: 5 },
    },
    { id: 'error', status: 'error', error: '既存エラー' },
    { id: 'stalled', status: 'stalled' },
  ];
  const issues = [
    { project: 'o/r', number: 5, status: 'failed', intakeId: 'child-filed' },
  ];
  const call = async (_method, route) => {
    const pathname = new URL(route, 'http://local').pathname;
    if (pathname === '/api/issues') return issues;
    if (pathname === '/api/drafts')
      return {
        items: drafts,
        total: drafts.length,
        page: 1,
        limit: 200,
        hasMore: false,
      };
    if (pathname === '/api/close-requests')
      return { items: [], total: 0, page: 1, limit: 200, hasMore: false };
    return [];
  };
  let output;
  assert.equal(
    await deskAlerts(
      call,
      { json: true },
      (s) => {
        output = JSON.parse(s);
      },
      { now },
    ),
    0,
  );
  const monitored = (id) => output.monitoring.find((row) => row.id === id);
  for (const id of ['boundary', 'older']) {
    assert.equal(monitored(id).reason, '質問も確定も無いまま止まっている');
    assert.ok(!output.items.some((row) => row.id === id));
  }
  for (const [id, error] of [
    ['proposal', '送信失敗 詳細'],
    ['replacement', '置換失敗'],
    ['plain', '通常失敗'],
  ]) {
    assert.equal(
      output.items.find((row) => row.id === id).error.replace(/\s+/g, ' '),
      error,
    );
    assert.ok(!monitored(id));
  }
  for (const id of ['recent', 'held', 'ready', 'missing-time'])
    assert.ok(monitored(id) && !monitored(id).reason, id);
  assert.equal(
    monitored('parent').reason,
    '起票失敗: 親の送信失敗',
    '親自身の失敗理由を診断に残す',
  );
  assert.ok(!output.items.some((row) => row.id === 'parent'));
  assert.equal(
    monitored('child').parentSplit,
    'parent',
    '独立した子相談を診断から落とさない',
  );
  assert.ok(
    output.monitoring.some((row) => row.kind === 'issue' && row.number === 5),
    '親の診断で子Issueを置き換えない',
  );
  const dir = mkdtempSync(join(tmpdir(), 'desk-parent-diagnostic-'));
  try {
    buildClient(dir);
    const { requestGroupsWithAux } = await import(
      pathToFileURL(join(dir, 'request-list.js')).href
    );
    const groups: Record<string, any[]> = requestGroupsWithAux({
      drafts,
      issues,
    });
    for (const rows of Object.values(groups))
      assert.ok(
        !rows.some((row) => row.id === 'parent'),
        '親は画面の全タブから除外する',
      );
    assert.ok(
      Object.values(groups).some((rows) =>
        rows.some((row) => row.id === 'child'),
      ),
    );
    assert.ok(
      groups.stopped.some((row) => row.kind === 'issue' && row.number === 5),
    );
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
  assert.ok(output.items.some((row) => row.id === 'question'));
  assert.equal(
    output.items.find((row) => row.id === 'error').error,
    '既存エラー',
  );
  for (const id of ['child-failure', 'stalled'])
    assert.ok(output.items.some((row) => row.id === id));
});

test('attention classification client cli parity', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'desk-attention-ui-'));
  try {
    buildClient(dir);
    const { requestGroupsWithAux } = await import(
      pathToFileURL(join(dir, 'request-list.js')).href
    );
    // PC配布と同じsharedだけの配置でも、clientソースなしでCLI分類を読める。
    const cli = join(dir, 'cli');
    mkdirSync(cli);
    cpSync(new URL('../shared', import.meta.url), join(cli, 'shared'), {
      recursive: true,
    });
    const shipped = await import(
      pathToFileURL(join(cli, 'shared/src/desk-attention.mts')).href
    );
    const data = {
      issues: [
        { project: 'o/r', number: 1, status: 'failed' },
        { project: 'o/r', number: 2, status: 'scope_waiting' },
        { project: 'o/r', number: 3, status: 'stopped', requiresUser: true },
        { project: 'o/r', number: 4, status: 'failed', followup: true },
        {
          project: 'o/r',
          number: 5,
          status: 'queued',
          followup: true,
          interruptDelay: { minutes: 40 },
        },
        { project: 'o/r', number: 6, status: 'completed', state: 'closed' },
        { project: 'o/r', number: 7, status: 'failed', state: 'closed' },
        {
          project: 'o/r',
          number: 8,
          status: 'waiting',
          requiresUser: true,
          body: 'must not leak',
        },
        { project: 'o/r', number: 10, status: 'running', body: 'normal work' },
      ],
      drafts: [
        {
          id: 'draft',
          status: 'draft',
          questions: [],
          body: 'private draft',
          messages: [{ text: 'private conversation' }],
        },
        { id: 'child-error', parentSplit: 'parent', status: 'error' },
        {
          id: 'child-question',
          parentSplit: 'parent',
          status: 'draft',
          questions: ['q'],
        },
        {
          id: 'child-empty',
          parentSplit: 'parent',
          status: 'draft',
          questions: [],
        },
        { id: 'child-running', parentSplit: 'parent', status: 'thinking' },
        {
          id: 'followup-stopped',
          status: 'thinking',
          turnStopped: true,
          proposal: { kind: 'followup' },
        },
      ],
      todos: [{ id: 'todo', body: 'todo' }],
      closes: [{ id: 'close', project: 'o/r', number: 1, status: 'confirm' }],
      reviews: [{ round: 1 }],
      stalls: [
        {
          id: 'stall',
          project: 'o/r',
          issue: 1,
          title: 'stall',
          detail:
            '無出力時間切れ 4件（直近1h0m0s）\nモデル: fixture / 推論の強さ: high: 4件\n直近の診断ID: abc123',
        },
      ],
      fullTests: [{ id: 'full-test', repo: 'o/r', title: 'test' }],
      attentionAlerts: [
        {
          key: 'recovery',
          kind: 'job_recovery',
          project: 'o/r',
          number: 1,
          stateLabel: '復旧',
          state: 'pending',
        },
        {
          key: 'usage-limit-wait:codex',
          kind: 'usage_limit_wait',
          requiresUser: true,
          nextAction: '再開見込みを確認して再試行するか判断する',
          state: 'usage_wait',
        },
        {
          key: 'future',
          kind: 'future_kind',
          state: 'open',
          reason: '未知の監視記録',
        },
      ],
    };
    const routes = {
      '/api/issues': 'issues',
      '/api/drafts': 'drafts',
      '/api/close-requests': 'closes',
      '/api/stalls/alerts': 'stalls',
      '/api/full-test/alerts': 'fullTests',
      '/api/usage-review/alerts': 'reviews',
      '/api/attention/alerts': 'attentionAlerts',
    };
    const calls = [];
    const call = async (method, route) => {
      const url = new URL(route, 'http://local');
      calls.push([method, url.pathname]);
      if (url.pathname === '/api/issue')
        return {
          ...data.issues.find(
            (i) => i.number === Number(url.searchParams.get('number')),
          ),
          retryable: false,
          generation: 1,
        };
      assert.ok(routes[url.pathname], route);
      const rows = data[routes[url.pathname]];
      if (['drafts', 'closes'].includes(routes[url.pathname]))
        return {
          items: rows,
          total: rows.length,
          page: 1,
          limit: 200,
          hasMore: false,
        };
      return rows;
    };
    const identity = (r) =>
      r.kind === 'issue'
        ? `issue:${r.project}#${r.number}`
        : `${r.kind}:${r.id}`;
    const uiGroups = requestGroupsWithAux(structuredClone(data));
    const expected = shipped
      .selectAttention([...uiGroups.attention, ...uiGroups.stopped])
      .attention.map(identity)
      .sort();
    const classified = shipped.selectAttention([
      ...shipped
        .requestRows(
          data.issues,
          data.drafts,
          data.closes,
          data.reviews,
          data.stalls,
        )
        .filter((r) =>
          ['attention', 'issues', 'waiting', 'stopped'].includes(
            shipped.rowGroup(r),
          ),
        ),
      ...shipped.fullTestRows(data.fullTests),
      ...shipped.attentionRows(data.attentionAlerts),
    ]);
    assert.deepEqual(classified.attention.map(identity).sort(), expected);
    let output;
    assert.equal(
      await deskAlerts(call, { json: true }, (s) => {
        output = JSON.parse(s);
      }),
      0,
    );
    assert.deepEqual(output.items.map(identity).sort(), expected);
    assert.equal(output.summary.screenTotal, expected.length);
    assert.equal(output.summary.rawTotal, expected.length);
    assert.equal(output.summary.total, expected.length);
    assert.equal(output.summary.cases, output.cases.length);
    assert.ok(output.monitoring.some((r) => r.caseKind === 'job_recovery'));
    assert.ok(!output.items.some((r) => r.caseKind === 'job_recovery'));
    assert.ok(
      !output.items.some((row) => row.kind === 'issue' && row.number === 1),
    );
    assert.equal(
      output.items.find((row) => row.id === 'child-error').nextActor,
      'ai',
    );
    for (const id of [
      'issue:o/r#3',
      'issue:o/r#8',
      'draft:child-question',
      'draft:child-error',
      'draft:followup-stopped',
      'attention:attention:usage-limit-wait:codex',
    ])
      assert.ok(expected.includes(id), id);
    for (const id of [
      'issue:o/r#4',
      'issue:o/r#6',
      'issue:o/r#7',
      'draft:child-empty',
      'draft:child-running',
      'todo:todo',
      'issue:o/r#2',
      'draft:draft',
    ])
      assert.ok(!expected.includes(id), id);
    assert.ok(!expected.includes('issue:o/r#5'));
    assert.deepEqual(
      output.monitoring.map(identity).sort(),
      requestGroupsWithAux(structuredClone(data))
        .monitoring.map(identity)
        .sort(),
    );
    const detail = data.stalls[0].detail;
    assert.equal(
      output.monitoring.find((row) => row.kind === 'stall').detail,
      detail,
    );
    assert.ok(
      output.monitoring.some(
        (row) =>
          row.caseKind === 'future_kind' && row.reason === '未知の監視記録',
      ),
    );
    assert.ok(
      !output.monitoring.some(
        (row) =>
          row.number === 10 ||
          row.status === 'thinking' ||
          row.kind === 'close',
      ),
    );
    assert.ok(
      [...output.items, ...output.monitoring].every(
        (row) =>
          !('body' in row) &&
          !('messages' in row) &&
          !('questions' in row) &&
          !('extra' in row),
      ),
    );
    const lines = [];
    assert.equal(await deskAlerts(call, {}, (s) => lines.push(s)), 0);
    assert.ok(lines.some((line) => line.includes('受付側の監視:')));
    assert.match(lines[0], new RegExp(`画面相当.*: ${expected.length}件`));
    assert.ok(
      lines.includes(
        `対処集計行数: ${output.summary.total}件、対象別の集約件数: ${output.summary.cases}件`,
      ),
    );
    assert.ok(
      lines.includes(
        `対処集計 ${output.summary.total}件（依頼・相談 ${output.summary.cases}件、止まっている ${output.summary.stale}件。基準 30分）`,
      ),
    );
    const all = lines.slice(
      lines.indexOf('画面相当の全行:') + 1,
      lines.indexOf('画面相当の全行:') + 1 + expected.length,
    );
    for (const row of output.items)
      assert.ok(
        all.some((line) =>
          row.kind === 'issue'
            ? line.includes(`${row.project}#${row.number}`)
            : line.includes(`ID:${row.id}`),
        ),
        identity(row),
      );
    assert.ok(
      calls.every(
        ([method, pathname]) =>
          method === 'GET' && (pathname in routes || pathname === '/api/issue'),
      ),
    );
    assert.deepEqual(
      new Set(
        calls.map(([, pathname]) => pathname).filter((p) => p !== '/api/issue'),
      ),
      new Set(Object.keys(routes)),
    );
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('対処集計に調査の起票待ちの見込みを表示する', async () => {
  for (const notClearing of [false, true]) {
    const call = async (_method, route) => {
      const pathname = new URL(route, 'http://local').pathname;
      if (pathname === '/api/attention/alerts')
        return [
          {
            key: 'pending',
            kind: 'attention_pending',
            stateLabel: '待ち',
            waitingCount: 7,
            estimatedDays: 4,
            inPerDay: 1.236,
            outPerDay: 3,
            notClearing,
          },
        ];
      if (['/api/drafts', '/api/close-requests'].includes(pathname))
        return { items: [], total: 0, page: 1, limit: 200, hasMore: false };
      return [];
    };
    const lines = [];
    assert.equal(await deskAlerts(call, {}, (line) => lines.push(line)), 0);
    const estimate = notClearing ? '解消しない見込み' : 'さばききるまで約4日';
    assert.ok(
      lines.includes(
        `対処集計 0件（依頼・相談 0件、止まっている 0件。基準 30分） 調査の起票待ち 7件・${estimate}（入り 1.24件/日・出 3件/日）`,
      ),
    );
  }
});

test('各経路・途中ページ・詳細の失敗を欠測として示し取得済みの別経路を残す', async () => {
  const paths = [
    '/api/issues',
    '/api/drafts',
    '/api/close-requests',
    '/api/stalls/alerts',
    '/api/full-test/alerts',
    '/api/usage-review/alerts',
    '/api/attention/alerts',
  ];
  for (const failed of [
    ...paths,
    '/api/drafts?page=2',
    '/api/close-requests?page=2',
    '/api/issue',
  ]) {
    const call = async (method, route) => {
      assert.equal(method, 'GET');
      const url = new URL(route, 'http://local');
      const page = Number(url.searchParams.get('page') || 1);
      if (failed === url.pathname || failed === `${url.pathname}?page=${page}`)
        throw Error('読取失敗');
      if (url.pathname === '/api/issues')
        return [
          {
            project: 'o/r',
            number: 1,
            status: 'review_held',
            requiresUser: true,
          },
        ];
      if (url.pathname === '/api/issue')
        return { project: 'o/r', number: 1, status: 'review_held' };
      if (url.pathname === '/api/stalls/alerts')
        return [{ id: 'keep', title: 'keep', detail: 'detail' }];
      if (['/api/drafts', '/api/close-requests'].includes(url.pathname)) {
        if (failed === `${url.pathname}?page=2`)
          return {
            items: [{ id: 'first', status: 'error' }],
            total: 2,
            page,
            limit: 200,
            hasMore: true,
          };
        return { items: [], total: 0, page, limit: 200, hasMore: false };
      }
      assert.ok(paths.includes(url.pathname));
      return [];
    };
    let output;
    assert.equal(
      await deskAlerts(call, { json: true }, (s) => {
        output = JSON.parse(s);
      }),
      1,
      failed,
    );
    assert.equal(output.screenScope.partial, true);
    assert.equal(output.summary.partial, true);
    assert.equal(output.summary.screenTotal, output.items.length);
    assert.ok(
      output.failures.some((f) => f.error.includes('読取失敗')),
      failed,
    );
    if (failed === '/api/drafts?page=2')
      assert.ok(
        output.items.some((r) => r.kind === 'draft' && r.id === 'first'),
      );
    if (failed === '/api/close-requests?page=2')
      assert.ok(
        output.monitoring.some((r) => r.kind === 'close' && r.id === 'first'),
      );
    assert.ok(
      [...output.items, ...output.monitoring].some(
        (r) => r.kind === (failed === '/api/stalls/alerts' ? 'issue' : 'stall'),
      ),
      failed,
    );
    const lines = [];
    assert.equal(await deskAlerts(call, {}, (s) => lines.push(s)), 1);
    assert.match(lines[0], /欠測あり暫定値/);
    assert.ok(
      lines.includes(
        `対処集計 ${output.summary.total}件（依頼・相談 ${output.summary.cases}件、止まっている ${output.summary.stale}件。基準 30分） 読めない経路あり・取得範囲の暫定値`,
      ),
      failed,
    );
    assert.ok(lines.some((line) => line.includes('読取失敗')));
  }
});

test('7経路の表示行を project で絞ってから集計し条件を出力する', async () => {
  const pair = (f) => ['igs', 'other'].map(f);
  const data: Record<string, any[]> = {
    '/api/issues': pair((project, i) => ({
      project,
      number: i + 1,
      status: 'waiting',
      requiresUser: true,
      title: 'issue',
    })),
    '/api/drafts': pair((project) => ({
      id: `draft-${project}`,
      project,
      status: 'draft',
      questions: ['A: 了承 / B: 中止'],
    })),
    '/api/close-requests': pair((project, i) => ({
      id: `close-${project}`,
      project,
      number: i + 11,
      status: 'confirm',
    })),
    '/api/stalls/alerts': pair((project) => ({
      id: project,
      project,
      title: 'stall',
      detail: 'detail',
    })),
    '/api/full-test/alerts': pair((repo) => ({
      id: 'full-test',
      repo,
      title: 'full',
    })),
    '/api/usage-review/alerts': [{ round: 1 }],
    '/api/attention/alerts': pair((project) => ({
      project,
      key: project,
      kind: 'ops_watch',
      stateLabel: 'waiting',
    })),
  };
  const call = async (_method, route) => {
    const url = new URL(route, 'http://local');
    if (url.pathname === '/api/issue')
      return {
        ...data['/api/issues'].find(
          (r) => r.number === Number(url.searchParams.get('number')),
        ),
        generation: 0,
        retryable: false,
      };
    let rows = data[url.pathname];
    if (url.pathname === '/api/attention/alerts')
      rows = rows.filter(
        (r) =>
          !url.searchParams.getAll('excludeProject').includes(r.project) &&
          (!url.searchParams.has('kind') ||
            url.searchParams.getAll('kind').includes(r.kind)),
      );
    if (['/api/drafts', '/api/close-requests'].includes(url.pathname))
      return {
        items: rows,
        total: rows.length,
        page: 1,
        limit: 200,
        hasMore: false,
      };
    return rows;
  };
  let output;
  assert.equal(
    await deskAlerts(
      call,
      parseArgs(['--json', '--exclude-project', 'igs']),
      (s) => {
        output = JSON.parse(s);
      },
    ),
    0,
  );
  assert.deepEqual(output.filters.excludeProject, ['igs']);
  assert.equal(output.screenScope.filtered, true);
  assert.equal(output.summary.screenTotal, 3);
  assert.equal(output.items.length, 3);
  assert.ok(output.items.every((r) => r.project !== 'igs' && r.repo !== 'igs'));
  assert.equal(output.summary.total, 3);
  assert.equal(output.monitoring.length, 4);
  assert.ok(
    output.cases.every((c) =>
      c.rows.every((r) => r.project !== 'igs' && r.repo !== 'igs'),
    ),
  );
  await deskAlerts(call, parseArgs(['--json', '--kind', 'ops_watch']), (s) => {
    output = JSON.parse(s);
  });
  assert.equal(output.items.length, 0);
  assert.ok(
    output.monitoring.length > 0 &&
      output.monitoring.every((r) => r.caseKind === 'ops_watch'),
  );
  assert.equal(output.summary.total, output.items.length);
  await deskAlerts(call, parseArgs(['--json', '--kind', 'stall']), (s) => {
    output = JSON.parse(s);
  });
  assert.equal(output.items.length, 0);
  assert.ok(
    output.monitoring.length > 0 &&
      output.monitoring.every((r) => r.kind === 'stall'),
  );
  await deskAlerts(
    call,
    parseArgs([
      '--json',
      '--exclude-kind',
      'stall',
      '--exclude-kind',
      'ops_watch',
    ]),
    (s) => {
      output = JSON.parse(s);
    },
  );
  assert.ok(
    output.items.length > 0 &&
      output.items.every(
        (r) => r.kind !== 'stall' && r.caseKind !== 'ops_watch',
      ),
  );
  const lines = [];
  await deskAlerts(call, parseArgs(['--project', 'other']), (s) =>
    lines.push(s),
  );
  assert.match(lines[0], /絞り込み後/);
  assert.ok(
    lines.some(
      (line) => line.includes('project=other') && line.includes('7経路'),
    ),
  );
});

test('絞り込み引数は値の欠落と不正値を拒む', () => {
  for (const value of [[], [' a'], ['a '], ['--json'], [''], ['a\nb']])
    assert.throws(() => parseArgs(['--project', ...value]));
  assert.equal(
    parseArgs([
      '--kind',
      'ops_watch',
      '--kind',
      'main_broken',
    ]).filters.toString(),
    'kind=ops_watch&kind=main_broken',
  );
});

test('絞り込み後に詳細を取得し、権限判断と失敗を表示する', async () => {
  const calls = [];
  const issues = [1, 2, 3, 4].map((number) => ({
    project: 'o/r',
    number,
    desk: 100 + number,
    status: 'review_held',
    title: '保留',
    requiresUser: true,
  }));
  const call = async (_method, route) => {
    const url = new URL(route, 'http://local');
    calls.push(route);
    if (url.pathname === '/api/issues') return issues;
    if (url.pathname === '/api/attention/alerts')
      return [
        {
          key: 'permission',
          kind: 'review_held',
          requiresUser: true,
          nextAction: '権限変更を承認するか判断する',
          state: 'open',
          stateLabel: '保留',
          project: 'o/r',
          number: 1,
          reason: ['a', 'b']
            .map(
              (file) =>
                `権限の範囲を定めるファイルの変更があります（利用者の承認が必要）: ${file}`,
            )
            .join('\n'),
        },
        {
          key: 'other',
          kind: 'review_held',
          requiresUser: true,
          nextAction: '別の判断に回答する',
          state: 'open',
          stateLabel: '保留',
          project: 'o/r',
          number: 1,
          reason: '別の判断',
        },
      ];
    if (url.pathname === '/api/issue') {
      const number = Number(url.searchParams.get('number'));
      if (number === 4) throw Object.assign(Error('拒否'), { status: 403 });
      return {
        ...issues[number - 1],
        generation: 1,
        retryable: true,
        permissionApproval: {
          project: 'o/r',
          head: String(number).repeat(40),
          pr: 1,
          generation: 1,
          epoch: 1,
          files: number === 2 ? ['b', 'a'] : ['a', 'b'],
        },
      };
    }
    if (
      url.pathname === '/api/drafts' ||
      url.pathname === '/api/close-requests'
    )
      return { items: [], total: 0, page: 1, limit: 200, hasMore: false };
    return [];
  };
  let output;
  assert.equal(
    await deskAlerts(call, { json: true }, (s) => {
      output = JSON.parse(s);
    }),
    1,
  );
  const group = output.cases.find((c) => c.operation === 'permission');
  assert.deepEqual(
    group.members.map((m) => m.desk),
    [101, 102, 103],
  );
  assert.equal(output.summary.rawTotal, 6);
  assert.equal(group.rows.length, 3);
  assert.ok(
    output.cases.some((c) =>
      c.rows.some((r) => r.reason?.includes('権限の範囲を定める')),
    ),
  );
  assert.ok(
    output.cases.some((c) => c.rows.some((r) => r.reason === '別の判断')),
  );
  assert.equal(output.summary.decisionMembers, 3);
  assert.equal(output.failures[0].status, 403);
  assert.ok(
    output.cases.some(
      (c) => c.number === 4 && c.rows.some((r) => /拒否/.test(r.reason)),
    ),
  );
  const lines = [];
  assert.equal(await deskAlerts(call, {}, (s) => lines.push(s)), 1);
  assert.equal(lines.at(-1), '利用者の操作が要る共通番号: 104,101,102,103');
  assert.equal(calls.filter((r) => r.startsWith('/api/issue?')).length, 8);
});

test('停止解除と範囲上限は別に表示し番号不明を補わない', async () => {
  const issues = [1, 2].map((number) => ({
    project: 'o/r',
    number,
    status: 'stopped',
    requiresUser: true,
    nextAction: '停止解除または範囲分割を判断する',
    title: '停止',
  }));
  const call = async (_method, route) => {
    const url = new URL(route, 'http://local');
    if (url.pathname === '/api/issues') return issues;
    if (url.pathname === '/api/issue') {
      const number = Number(url.searchParams.get('number'));
      return {
        ...issues[number - 1],
        desk: null,
        generation: 2,
        retryable: false,
        ...(number === 2
          ? { result: { summary: '変更範囲が上限を超えました' } }
          : {}),
      };
    }
    if (
      url.pathname === '/api/drafts' ||
      url.pathname === '/api/close-requests'
    )
      return { items: [], total: 0, page: 1, limit: 200, hasMore: false };
    return [];
  };
  const lines = [];
  assert.equal(await deskAlerts(call, {}, (s) => lines.push(s)), 0);
  assert.ok(
    lines.some(
      (s) => s.includes('issue-desk-issue-control') && s.includes('停止解除'),
    ),
  );
  assert.ok(lines.some((s) => s.includes('通常の停止解除と確定できません')));
  assert.equal(lines.at(-1), '利用者の操作が要る共通番号: なし');
});

test('判断の集約後も対象の活動時刻と止まりと自動処理段階を表示する', async () => {
  const issues = [1, 2, 3].map((number) => ({
    project: 'o/r',
    number,
    status: 'review_held',
    requiresUser: true,
    updatedAt: '2026-01-01T00:00:00Z',
  }));
  const call = async (_method, route) => {
    const url = new URL(route, 'http://local');
    if (url.pathname === '/api/issues') return issues;
    if (url.pathname === '/api/attention/alerts')
      return [
        {
          key: 'old',
          kind: 'review_held',
          state: 'stopped',
          stateLabel: '停止',
          project: 'o/r',
          number: 1,
          updatedAt: '2026-01-01T00:05:00Z',
          reason: '以前の通知',
        },
      ];
    if (url.pathname === '/api/issue') {
      const number = Number(url.searchParams.get('number'));
      return {
        ...issues[number - 1],
        generation: 1,
        retryable: true,
        permissionApproval: {
          project: 'o/r',
          head: String(number).repeat(40),
          pr: 1,
          generation: 1,
          epoch: 1,
          files: ['a'],
        },
      };
    }
    if (
      url.pathname === '/api/drafts' ||
      url.pathname === '/api/close-requests'
    )
      return { items: [], total: 0, page: 1, limit: 200, hasMore: false };
    return [];
  };
  let output;
  assert.equal(
    await deskAlerts(
      call,
      { json: true },
      (s) => {
        output = JSON.parse(s);
      },
      { now: Date.parse('2026-01-01T01:00:00Z') },
    ),
    0,
  );
  const group = output.cases.find((c) => c.operation === 'permission');
  assert.equal(group.stale, true);
  assert.equal(group.lastActivityAt, '2026-01-01T00:05:00.000Z');
  assert.equal(group.elapsedMinutes, 55);
  assert.equal(group.automatic[0].state, 'stopped');
  assert.ok(output.summary.stale >= 1);
  assert.ok(
    output.cases.some((c) => c.rows.some((r) => r.reason === '以前の通知')),
  );
});

test('user-only は判断待ちを集め対象外を除きフィルターとJSONを維持する', async () => {
  const head = 'a'.repeat(40);
  const issues = [1, 2, 3, 4, 5, 6, 7, 8, 9].map((number) => ({
    project: number === 6 ? 'other' : 'o/r',
    number,
    desk: number === 2 ? null : 100 + number,
    status: number >= 7 ? 'waiting' : 'review_held',
    title: `題名${number}`,
    requiresUser: true,
  }));
  const details: any[] = issues.map((row) => ({
    ...row,
    generation: 1,
    retryable: false,
  }));
  for (const i of [0, 1, 5])
    details[i].permissionApproval = {
      project: issues[i].project,
      head,
      pr: 1,
      generation: 1,
      epoch: 1,
      files: ['policy'],
    };
  details[2].pendingFinalApprovals = ['first', 'second'].map((id) => ({
    id,
    headSha: head,
    fingerprint: 'b'.repeat(64),
  }));
  details[3].deletionApproval = {
    head,
    items: [
      { kind: 'delete', path: 'old' },
      { kind: 'changed', path: 'src/store.py' },
    ],
  };
  details[4].finalApprovalsUnavailable = '計画を読めません';
  for (const i of [6, 7, 8]) {
    details[i].retryable = true;
    details[i].result = {
      summary: '実装回数の上限を超え、使用量の上限を超えた',
    };
  }
  const call = async (_method, route) => {
    const url = new URL(route, 'http://local');
    if (url.pathname === '/api/issues') return issues;
    if (url.pathname === '/api/issue')
      return details.find(
        (d) => d.number === Number(url.searchParams.get('number')),
      );
    if (url.pathname === '/api/attention/alerts')
      return [
        {
          key: 'budget',
          kind: 'consultation_budget',
          project: 'o/r',
          title: '予算相談',
          state: 'open',
          requiresUser: true,
          nextAction: '上限を足して続行するか判断する',
        },
      ];
    if (url.pathname === '/api/drafts')
      return {
        items: [
          {
            id: 'question',
            project: 'o/r',
            status: 'draft',
            title: '通常質問',
            questions: ['質問'],
          },
        ],
        total: 1,
        page: 1,
        limit: 200,
        hasMore: false,
      };
    if (url.pathname === '/api/close-requests')
      return { items: [], total: 0, page: 1, limit: 200, hasMore: false };
    return [];
  };
  const run = async (args) => {
    const lines = [];
    const code = await runCommand(args, {
      env: {},
      configured: () => null,
      workerId: () => null,
      open: async () => ({ call, close() {} }),
      log: (line) => lines.push(line),
    });
    return { code, lines };
  };
  const { code, lines } = await run(['--user-only', '--project', 'o/r']);
  assert.equal(code, 0);
  assert.ok(
    lines.some(
      (l) =>
        l.includes('共通番号:101') &&
        l.includes('共通番号:不明') &&
        l.includes('題名1') &&
        l.includes('題名2') &&
        l.includes('権限'),
    ),
  );
  assert.equal(lines.filter((l) => l.includes('最終承認を判断')).length, 2);
  assert.ok(
    lines.some(
      (l) =>
        l.includes('上限超過の再試行') &&
        ['107', '108', '109'].every((n) => l.includes(`共通番号:${n}`)),
    ),
  );
  assert.ok(
    lines.some((l) => l.includes('削除承認を判断') && l.includes('題名4')),
  );
  assert.ok(lines.some((l) => l.includes('計画を読めません')));
  assert.ok(lines.some((l) => l.includes('予算相談') && l.includes('続行')));
  assert.ok(!lines.some((l) => l.includes('通常質問') || l.includes('題名6')));
  assert.match(lines.at(-1), /^利用者の操作が要る共通番号: /);
  assert.deepEqual(
    JSON.parse((await run(['--json', '--project', 'o/r'])).lines[0]),
    JSON.parse(
      (await run(['--user-only', '--json', '--project', 'o/r'])).lines[0],
    ),
  );
  const excluded = await run(['--user-only', '--exclude-kind', 'issue']);
  assert.ok(!excluded.lines.some((l) => l.includes('題名1')));
});

test('user-only は空と欠測を区別し取得済み判断を残す', async () => {
  const run = async (mode) => {
    const lines = [];
    const call = async (_method, route) => {
      const url = new URL(route, 'http://local');
      if (url.pathname === '/api/issues' && mode !== 'empty')
        return [
          {
            project: 'o/r',
            number: 1,
            desk: 42,
            title: '対象',
            status: 'review_held',
            requiresUser: true,
          },
        ];
      if (url.pathname === '/api/issue') {
        if (mode === 'detail') throw Error('詳細読取失敗');
        return {
          project: 'o/r',
          number: 1,
          desk: 42,
          title: '対象',
          status: 'review_held',
          generation: 1,
          retryable: false,
          deletionApproval: {
            head: 'a'.repeat(40),
            items: [{ kind: 'delete', path: 'x' }],
          },
        };
      }
      if (url.pathname === '/api/stalls/alerts' && mode === 'route')
        throw Error('経路読取失敗');
      if (['/api/drafts', '/api/close-requests'].includes(url.pathname))
        return { items: [], total: 0, page: 1, limit: 200, hasMore: false };
      return [];
    };
    const code = await runCommand(['--user-only'], {
      env: {},
      configured: () => null,
      workerId: () => null,
      open: async () => ({ call, close() {} }),
      log: (line) => lines.push(line),
    });
    return { code, lines };
  };
  assert.deepEqual(await run('empty'), {
    code: 0,
    lines: [
      '該当する承認・上限判断待ちはありません。',
      '利用者の操作が要る共通番号: なし',
    ],
  });
  for (const mode of ['route', 'detail']) {
    const { code, lines } = await run(mode);
    assert.equal(code, 1);
    assert.ok(lines.some((l) => l.includes('読取失敗')));
    assert.ok(!lines.some((l) => l.includes('待ちはありません')));
    if (mode === 'route')
      assert.ok(lines.some((l) => l.includes('削除承認を判断')));
  }
});
