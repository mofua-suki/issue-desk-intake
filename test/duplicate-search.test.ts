import test from 'node:test';
import assert from 'node:assert/strict';
import { runCommand } from '../scripts/list-issues.ts';
import { searchDuplicates } from '../scripts/lib/duplicate-search.ts';

const project = 'owner/repo';
const page = (items, p = 1, total = items.length, hasMore = false) => ({
  items,
  page: p,
  total,
  limit: 200,
  hasMore,
});
async function execute(
  handler,
  args = ['--duplicates', '--project', project, '--query', '検索'],
) {
  let output, failure;
  const code = await runCommand(args, {
    connection: () => ({}),
    open: async () => ({ call: handler, close: async () => {} }),
    log: (s) => {
      output = JSON.parse(s);
    },
    error: (s) => {
      failure = s;
    },
  });
  return { code, output, failure };
}
test('重複照合は全ページの相談とTODOを同じ題名で照合する', async () => {
  const requests = [];
  const result = await execute(async (_, route) => {
    const u = new URL(route, 'http://localhost');
    requests.push(u);
    if (u.pathname.endsWith('/search'))
      return { issues: [], total: 0, hasMore: false };
    if (u.pathname.endsWith('/drafts'))
      return u.searchParams.get('page') === '1'
        ? page(
            [
              {
                id: 'other',
                project: 'other/repo',
                title: '検索',
                status: 'draft',
              },
            ],
            1,
            2,
            true,
          )
        : page(
            [{ id: 'd', project, title: '検索の相談', status: 'draft' }],
            2,
            2,
          );
    return page([{ id: 't', project, body: '検索TODO' }]);
  });
  assert.equal(result.code, 0);
  assert.deepEqual(result.output.issues.candidates, []);
  assert.deepEqual(result.output.drafts.candidates, [
    { id: 'd', title: '検索の相談', state: 'draft' },
  ]);
  assert.deepEqual(result.output.todos.candidates, [
    { id: 't', title: '検索TODO', state: 'todo' },
  ]);
  const drafts = requests.filter((u) => u.pathname === '/api/drafts');
  assert.equal(drafts.length, 2);
  for (const [i, u] of drafts.entries())
    assert.deepEqual(Object.fromEntries(u.searchParams), {
      project,
      page: String(i + 1),
      limit: '200',
      excludeStatus: 'submitted',
      view: 'summary',
    });
  for (const u of requests.filter((u) => u.pathname !== '/api/drafts'))
    assert.equal(u.searchParams.has('view'), false);
});
test('相談だけが一致すると相談候補を表示する', async () => {
  const result = await execute(async (_, route) => {
    if (route.includes('/search?'))
      return { issues: [], total: 0, hasMore: false };
    if (route.includes('/drafts?'))
      return page([
        { id: 'draft', project, title: '検索相談', status: 'thinking' },
      ]);
    return page([]);
  });
  assert.deepEqual(result.output.drafts.candidates, [
    { id: 'draft', title: '検索相談', state: 'thinking' },
  ]);
  assert.deepEqual(result.output.todos.candidates, []);
});
test('相談の読取失敗やページ欠落は0件と区別する', async () => {
  for (const broken of [true, false]) {
    const result = await execute(async (_, route) => {
      if (route.includes('/search?'))
        return { issues: [], total: 0, hasMore: false };
      if (route.includes('/drafts?')) {
        if (broken) throw Error('failed');
        return page([], 1, 1);
      }
      return page([]);
    });
    assert.equal(result.output.drafts.confirmed, false);
    assert.equal(result.output.todos.confirmed, true);
  }
});
test('依頼の複数ページを照合し通常一覧の入口を受け付けない', async () => {
  const call = async (_, route) => {
    const u = new URL(route, 'http://localhost');
    if (!u.pathname.endsWith('/search')) return page([]);
    if (!u.searchParams.has('state'))
      return { total: 0, issues: [], hasMore: false };
    const n = Number(u.searchParams.get('page'));
    return {
      total: 2,
      hasMore: n === 1,
      issues: [{ number: n, project, state: 'open', title: `検索${n}` }],
    };
  };
  const result = await execute(call);
  assert.equal(result.output.issues.candidates.length, 2);
  for (const args of [
    [],
    ['--count-only'],
    ['--project', project, '--query', '検索'],
    ['--duplicates', '--project', project, '--query', '検索', '--limit', '1'],
  ]) {
    let opened = false;
    assert.equal(
      await runCommand(args, {
        open: async () => {
          opened = true;
          throw Error('unexpected');
        },
        error() {},
      }),
      1,
    );
    assert.equal(opened, false);
  }
  assert.equal(
    (await execute(call, ['--duplicates', '--query', '検索'])).code,
    1,
  );
});

const draft = (overrides = {}) => ({
  id: 'd',
  project,
  title: '検索の相談',
  status: 'draft',
  ...overrides,
});
const todo = (overrides = {}) => ({
  id: 't',
  project,
  body: '検索TODO',
  ...overrides,
});
const fixture =
  (
    drafts = page([]),
    todos = page([]),
    issues = { issues: [], total: 0, hasMore: false },
  ) =>
  async (_, route) => {
    if (route.includes('/search?')) return issues;
    return route.includes('/drafts?') ? drafts : todos;
  };

test('summary行は同じ保存内容の全文行と同じ候補とskippedを返す', async () => {
  const summary = [
    draft({
      id: 'shown',
      title: '別の題名',
      shownTitle: '検索の表示名',
      unassigned: false,
      createdAt: '2026-01-01',
      updatedAt: '2026-01-02',
    }),
    draft({ id: 'empty', title: '' }),
    draft({ id: 'other', project: 'other/repo' }),
    draft({ id: 'unassigned', project: null, unassigned: true }),
    draft({ id: 'submitted', status: 'submitted' }),
  ];
  const full = summary.map((row) => ({
    ...row,
    body: '本文',
    messages: [{ text: '会話' }],
  }));
  const a = await execute(fixture(page(summary)));
  const b = await execute(fixture(page(full)));
  assert.equal(a.code, 0);
  assert.equal(b.code, 0);
  assert.deepEqual(a.output.drafts, b.output.drafts);
  assert.deepEqual(a.output.drafts, {
    confirmed: true,
    candidates: [{ id: 'shown', title: '検索の表示名', state: 'draft' }],
    skipped: 1,
  });
});

test('summary非対応のHTTP 400は相談だけ未確認にする', async () => {
  let draftCalls = 0;
  const result = await execute(async (method, route) => {
    if (route.includes('/drafts?')) {
      draftCalls++;
      throw Object.assign(new Error('unsupported'), { status: 400 });
    }
    return fixture(page([]), page([todo()]), {
      issues: [{ number: 1, project, state: 'open', title: '検索依頼' }],
      total: 1,
      hasMore: false,
    })(method, route);
  });
  assert.equal(result.code, 0);
  assert.equal(draftCalls, 1);
  assert.deepEqual(result.output.drafts, {
    confirmed: false,
    candidates: [],
    reason: 'drafts 一覧読取: 読取の失敗 (HTTP 400)',
  });
  assert.deepEqual(result.output.issues.candidates, [
    { id: 1, title: '検索依頼', state: 'open' },
  ]);
  assert.deepEqual(result.output.todos.candidates, [
    { id: 't', title: '検索TODO', state: 'todo' },
  ]);
});

test('空題名を除外して正常な相談を照合する', async () => {
  const { output } = await execute(
    fixture(page([draft({ id: 'empty', title: '' }), draft()])),
  );
  assert.deepEqual(output.drafts, {
    confirmed: true,
    skipped: 1,
    candidates: [{ id: 'd', title: '検索の相談', state: 'draft' }],
  });
  assert.equal(output.issues.skipped, 0);
  const normal = await execute(fixture(page([draft()])));
  assert.equal(normal.output.drafts.skipped, 0);
});
test('空本文と空白題名を照合から除外する', async () => {
  const empty = await execute(fixture(page([]), page([todo({ body: '' })])));
  assert.deepEqual(empty.output.todos, {
    confirmed: true,
    skipped: 1,
    candidates: [],
  });
  const whitespace = await execute(
    fixture(page([draft({ title: ' \t ' })]), page([todo({ body: ' \n\t' })])),
  );
  for (const kind of ['drafts', 'todos'])
    assert.deepEqual(whitespace.output[kind], {
      confirmed: true,
      skipped: 1,
      candidates: [],
    });
});
test('不正項目は空題名でも失敗しフィールドと安全な識別子を示す', async () => {
  for (const [kind, row, field, idText] of [
    ['drafts', draft({ id: undefined, title: '' }), 'id', 'id欠落'],
    ['drafts', draft({ project: null, title: '' }), 'project', 'id=d'],
    ['drafts', draft({ status: null, title: '' }), 'status', 'id=d'],
    ['drafts', draft({ title: 3 }), 'title', 'id=d'],
    ['todos', todo({ id: undefined, body: '' }), 'id', 'id欠落'],
    ['todos', todo({ project: 1, body: '' }), 'project', 'id=t'],
    ['todos', todo({ status: null, body: '' }), 'status', 'id=t'],
    ['todos', todo({ body: 3 }), 'body', 'id=t'],
  ] as [string, any, string, string][]) {
    const result = await execute(
      fixture(
        kind === 'drafts' ? page([row]) : page([]),
        kind === 'todos' ? page([row]) : page([]),
      ),
    );
    const value = result.output[kind];
    assert.equal(value.confirmed, false);
    assert.deepEqual(value.candidates, []);
    for (const text of ['項目の不正', kind, field, idText])
      assert.ok(value.reason.includes(text), value.reason);
  }
  const escaped = await execute(
    fixture(page([draft({ id: 'a\nb', status: null })])),
  );
  assert.ok(escaped.output.drafts.reason.includes('id=a\\nb'));
});
test('未割当相談を飛ばし正常な相談を照合する', async () => {
  const { output } = await execute(
    fixture(
      page([
        draft({ id: 'unassigned', project: null, unassigned: true }),
        draft(),
      ]),
    ),
  );
  assert.deepEqual(output.drafts, {
    confirmed: true,
    skipped: 0,
    candidates: [{ id: 'd', title: '検索の相談', state: 'draft' }],
  });
});
test('読取例外は安全に分類し引継ぎ例外は再送出する', async () => {
  const secrets = [
    '本文秘密',
    '題名秘密',
    '例外秘密',
    'Bearer token-secret',
    'https://user:password@example.com',
  ];
  for (const [error, detail] of [
    [Object.assign(Error(secrets.join(' ')), { status: 503 }), '503'],
    [
      Object.assign(Error(secrets.join(' ')), { unreachable: true }),
      '接続不能',
    ],
    [
      Object.assign(Error(secrets.join(' ')), {
        code: 'ERR_CHILD_PROCESS_STDIO_MAXBUFFER',
      }),
      'サイズ上限',
    ],
    [Error(secrets.join(' ')), '読取の失敗'],
  ]) {
    const result = await execute(async (_, route) => {
      if (route.includes('/drafts?')) throw error;
      return fixture()(_, route);
    });
    assert.equal(result.output.drafts.confirmed, false);
    assert.deepEqual(result.output.drafts.candidates, []);
    assert.match(result.output.drafts.reason, /読取の失敗/);
    assert.ok(result.output.drafts.reason.includes(detail));
    for (const secret of secrets)
      assert.ok(!result.output.drafts.reason.includes(secret));
  }
  const handoff = Object.assign(Error('handoff'), { nasHandoff: true });
  await assert.rejects(
    () =>
      searchDuplicates(
        async (_, route) => {
          if (route.includes('/drafts?')) throw handoff;
          return fixture()(_, route);
        },
        project,
        '検索',
      ),
    (e) => e === handoff,
  );
});
test('応答の形と一覧の途中欠落を区別する', async () => {
  const shape = await execute(fixture({ ...page([]), items: null }));
  assert.match(shape.output.drafts.reason, /応答の形の不正/);
  for (const value of [page([draft()], 1, 2), page([], 1, 1, true)]) {
    const result = await execute(fixture(value));
    assert.equal(result.output.drafts.confirmed, false);
    assert.deepEqual(result.output.drafts.candidates, []);
    assert.match(result.output.drafts.reason, /途中欠落/);
  }
  let calls = 0;
  const duplicate = await execute(async (_, route) => {
    if (!route.includes('/drafts?')) return fixture()(_, route);
    calls++;
    const n = Number(
      new URL(route, 'http://localhost').searchParams.get('page'),
    );
    return page([draft()], n, 2, n === 1);
  });
  assert.equal(calls, 6);
  assert.equal(duplicate.output.drafts.confirmed, false);
  assert.deepEqual(duplicate.output.drafts.candidates, []);
  assert.match(duplicate.output.drafts.reason, /途中欠落/);
  const invalid = await execute(
    fixture(
      page([draft({ status: null, title: '題名秘密', body: '本文秘密' })]),
    ),
  );
  assert.match(invalid.output.drafts.reason, /項目の不正/);
  assert.doesNotMatch(invalid.output.drafts.reason, /題名秘密|本文秘密/);
});
test('読めなかった候補のリポジトリがある依頼の一覧は照合済みにしない', async () => {
  const result = await execute(async (_, route) => {
    if (route.includes('/search?'))
      return {
        issues: [],
        total: 0,
        hasMore: false,
        incomplete: true,
        unreadProjects: [
          { project: 'owner/gone', reason: 'GiteaがHTTP 404を返しました。' },
        ],
      };
    return page([]);
  });
  assert.equal(result.output.issues.confirmed, false);
  assert.deepEqual(result.output.issues.candidates, []);
  assert.match(result.output.issues.reason, /途中欠落/);
  // 対象のプロジェクトを読めていれば、ほかの候補が読めなくても照合済み（unreadProjects だけ・incomplete なし）。
  const filtered = await execute(async (_, route) => {
    if (route.includes('/search?'))
      return {
        issues: [],
        total: 0,
        hasMore: false,
        unreadProjects: [
          { project: 'owner/gone', reason: 'GiteaがHTTP 404を返しました。' },
        ],
      };
    return page([]);
  });
  assert.equal(filtered.output.issues.confirmed, true);
});
