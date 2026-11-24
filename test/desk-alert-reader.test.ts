import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  readAllDraftPages,
  readDeskAttention,
} from '../shared/src/desk-alert-reader.ts';

// ページを返すモック。pages[試行][ページ番号 - 1] を順に返す。
const pagedReader = (attempts) => {
  const calls = [];
  let attempt = -1;
  const readPage = async (page) => {
    if (page === 1) attempt += 1;
    calls.push([attempt, page]);
    const pages = attempts[Math.min(attempt, attempts.length - 1)];
    return pages[page - 1];
  };
  return { readPage, calls };
};
const page = (n, ids, total, hasMore) => ({
  items: ids.map((id) => ({ id })),
  total,
  page: n,
  limit: 200,
  hasMore,
});

test('読んでいる途中で total が増えたら、最初から読み直して安定した結果を返す', async () => {
  const { readPage, calls } = pagedReader([
    [page(1, ['a', 'b'], 4, true), page(2, ['c', 'd', 'e'], 5, false)],
    [page(1, ['a', 'b'], 5, true), page(2, ['c', 'd', 'e'], 5, false)],
  ]);
  const items = await readAllDraftPages(readPage, '形が違います');
  assert.deepEqual(
    items.map((x) => x.id),
    ['a', 'b', 'c', 'd', 'e'],
  );
  assert.deepEqual(
    calls.map(([attempt]) => attempt),
    [0, 0, 1, 1],
  );
});

test('同じ行が2ページに出たら、最初から読み直す', async () => {
  const { readPage, calls } = pagedReader([
    [page(1, ['a', 'b'], 4, true), page(2, ['b', 'c'], 4, false)],
    [page(1, ['a', 'b'], 4, true), page(2, ['c', 'd'], 4, false)],
  ]);
  const items = await readAllDraftPages(readPage, '形が違います');
  assert.deepEqual(
    items.map((x) => x.id),
    ['a', 'b', 'c', 'd'],
  );
  assert.equal(calls.filter(([, p]) => p === 1).length, 2);
});

test('集めた件数が total と合わないときも、最初から読み直す', async () => {
  const { readPage, calls } = pagedReader([
    // total は3のまま変わらないが、集まったのは2件だけ（途中で1件が抜けた）。
    [page(1, ['a', 'b'], 3, true), page(2, [], 3, false)],
    [page(1, ['a', 'b'], 3, true), page(2, ['c'], 3, false)],
  ]);
  assert.deepEqual(
    (await readAllDraftPages(readPage, '形が違います')).map((x) => x.id),
    ['a', 'b', 'c'],
  );
  assert.equal(calls.filter(([, p]) => p === 1).length, 2);
});

test('読み直しても安定しなければ、部分的な成功にせず失敗にする', async () => {
  const unstable = (n) => [
    page(1, ['a', 'b'], 4 + n, true),
    page(2, ['c', 'd'], 4, false),
  ];
  const { readPage, calls } = pagedReader([
    unstable(1),
    unstable(2),
    unstable(3),
  ]);
  await assert.rejects(
    readAllDraftPages(readPage, '形が違います'),
    /データが変わり続け/,
  );
  assert.equal(
    calls.filter(([, p]) => p === 1).length,
    3,
    '最初の1回と読み直し2回で止まる',
  );
});

test('形が崩れた応答は読み直さず、渡した文言で失敗にする', async () => {
  const { readPage, calls } = pagedReader([
    [{ items: [], total: 1, page: 1, limit: 200, hasMore: true }],
  ]);
  await assert.rejects(
    readAllDraftPages(readPage, '形が違います'),
    /形が違います/,
  );
  assert.equal(calls.length, 1);
});

test('要対応の読み手は上限を越える相談を最後まで読み、途中の失敗を報告する', async () => {
  const call = async (_method, route) => {
    if (route.startsWith('/api/close-requests?'))
      return { items: [], total: 0, page: 1, limit: 200, hasMore: false };
    if (!route.startsWith('/api/drafts?')) return [];
    const page = Number(
      new URL(route, 'http://local').searchParams.get('page'),
    );
    return {
      items:
        page === 1
          ? Array.from({ length: 200 }, () => ({ status: 'finished' }))
          : [{ status: 'draft', questions: ['質問'] }],
      total: 201,
      page,
      limit: 200,
      hasMore: page === 1,
    };
  };
  const attention = await readDeskAttention(call);
  assert.equal(attention.inputs.drafts.length, 201);
  assert.deepEqual(attention.failures, []);
  const broken = async (method, route) =>
    route.includes('page=2')
      ? Promise.reject(Error('失敗'))
      : call(method, route);
  const failed = await readDeskAttention(broken);
  assert.equal(failed.inputs.drafts.length, 200);
  assert.equal(failed.failures.length, 1);
});

test('要対応の読み手はクローズ依頼の全ページを読み、途中の失敗を伝える', async () => {
  const calls = [];
  const rows = Array.from({ length: 201 }, (_, i) => ({
    id: `close-${i}`,
    project: 'owner/repo',
    number: i,
    status: 'confirm',
    pc: 'nas',
    reason: 'resolved',
    actions: ['close', 'keep'],
  }));
  let failSecond = false;
  const call = async (method, route) => {
    assert.equal(method, 'GET');
    const url = new URL(route, 'http://local');
    if (url.pathname === '/api/drafts')
      return { items: [], total: 0, page: 1, limit: 200, hasMore: false };
    if (url.pathname !== '/api/close-requests') return [];
    const n = Number(url.searchParams.get('page'));
    calls.push(n);
    assert.equal(url.searchParams.get('limit'), '200');
    if (n === 2 && failSecond) throw Error('2ページ目の読取失敗');
    return {
      items: rows.slice((n - 1) * 200, n * 200),
      total: rows.length,
      page: n,
      limit: 200,
      hasMore: n === 1,
    };
  };
  const attention = await readDeskAttention(call);
  assert.deepEqual(attention.inputs.closes, rows);
  assert.deepEqual(attention.failures, []);
  assert.deepEqual(calls.splice(0), [1, 2]);
  failSecond = true;
  const failed = await readDeskAttention(call);
  assert.deepEqual(failed.inputs.closes, rows.slice(0, 200));
  assert.equal(
    failed.failures.find((failure) => failure.kind === 'closes').error,
    '2ページ目の読取失敗',
  );
});

test('要対応の読み手は指定した query を自動処理だけに渡しページの URL を変えない', async () => {
  for (const query of [
    new URLSearchParams('project=a&excludeKind=b'),
    new URLSearchParams(),
  ]) {
    const routes = [];
    const call = async (_method, route) => {
      routes.push(route);
      if (
        route.startsWith('/api/drafts?') ||
        route.startsWith('/api/close-requests?')
      )
        return { items: [], total: 0, page: 1, limit: 200, hasMore: false };
      return [];
    };
    assert.deepEqual((await readDeskAttention(call, query)).failures, []);
    assert.ok(
      routes.includes(`/api/attention/alerts${query.size ? `?${query}` : ''}`),
    );
    assert.ok(
      routes.includes(
        '/api/drafts?page=1&limit=200&excludeStatus=submitted,closed',
      ),
    );
    assert.ok(routes.includes('/api/close-requests?page=1&limit=200'));
    // Issueは対応中の範囲だけを読み、全件の一覧は読まない。
    assert.deepEqual(
      routes.filter((r) => r.split('?')[0] === '/api/issues'),
      ['/api/issues?scope=active'],
    );
    assert.ok(
      routes
        .filter(
          (r) =>
            r.includes('?') &&
            !r.startsWith('/api/attention/alerts') &&
            r !== '/api/issues?scope=active',
        )
        .every((r) =>
          /^(\/api\/drafts|\/api\/close-requests)\?page=1&limit=200(&excludeStatus=submitted,closed)?$/.test(
            r,
          ),
        ),
    );
  }
});

test('起票済みの相談は要対応の行にならないので読み手が読まなくても結果は同じ', async () => {
  const { requestRows, rowGroup } = await import(
    '../shared/src/desk-attention.mts'
  );
  const issue = {
    project: 'o/a',
    number: 1,
    status: 'failed',
    intakeId: 'd1',
    state: 'open',
    title: 't',
  };
  const open = { id: 'd2', project: 'o/a', status: 'error', title: 'x' };
  const submitted = [
    { id: 'd1', project: 'o/a', status: 'submitted', issue: { number: 1 } },
    { id: 'd3', project: 'o/a', status: 'submitted', issue: { number: 9 } },
    {
      id: 'd4',
      project: 'o/a',
      status: 'submitted',
      parentSplit: 'p',
      issue: { number: 8 },
    },
    {
      id: 'd5',
      project: 'o/a',
      status: 'submitted',
      splitIssues: [{ project: 'o/a', number: 7 }],
      issues: [{ number: 7 }],
    },
  ];
  const attention = (drafts) =>
    requestRows([issue], drafts)
      .filter((r) => rowGroup(r) === 'attention')
      .map((r) => `${r.kind}:${r.id ?? r.number}:${r.status}`)
      .sort();
  assert.deepEqual(attention([open, ...submitted]), attention([open]));
});
