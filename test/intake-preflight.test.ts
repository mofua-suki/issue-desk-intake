import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  consultationPreflight,
  preflightLines,
  readPreflightCandidates,
} from '../scripts/lib/intake-preflight.ts';

test('起票前照合は相談の全ページを読み失敗を報告する', async () => {
  const call = async (_method, route) => {
    if (route.startsWith('/api/issues/search?'))
      return { total: 0, issues: [], hasMore: false };
    if (route.startsWith('/api/todos?')) {
      const query = new URL(route, 'http://local').searchParams;
      assert.equal(query.get('project'), 'owner/repo');
      const page = Number(query.get('page'));
      return {
        items:
          page === 1
            ? Array.from({ length: 200 }, (_, i) => ({
                id: `todo-${i}`,
                project: 'owner/repo',
                removedAt: 'removed',
              }))
            : [{ id: 'todo-last', project: 'owner/repo' }],
        total: 201,
        page,
        limit: 200,
        hasMore: page === 1,
      };
    }
    if (!route.startsWith('/api/drafts?')) return [];
    const query = new URL(route, 'http://local').searchParams;
    assert.equal(query.get('project'), 'owner/repo');
    assert.equal(
      query.get('excludeStatus'),
      'submitted,closed,project_handoff',
    );
    const page = Number(query.get('page'));
    return {
      items:
        page === 1
          ? Array.from({ length: 200 }, () => ({
              project: 'owner/repo',
              status: 'closed',
            }))
          : [{ project: 'owner/repo', status: 'draft', id: 'last' }],
      total: 201,
      page,
      limit: 200,
      hasMore: page === 1,
    };
  };
  const result = await readPreflightCandidates(call, 'owner/repo');
  assert.equal(result.rows.length, 2);
  assert.equal(result.rows[1].id, 'todo-last');
  assert.equal(result.rows[0].id, 'last');
  assert.deepEqual(result.unavailable, []);
  const failed = await readPreflightCandidates(
    async (method, route) =>
      route.startsWith('/api/drafts?') && route.includes('page=2')
        ? Promise.reject(Error('失敗'))
        : call(method, route),
    'owner/repo',
  );
  assert.deepEqual(failed.unavailable, ['相談']);
});

const emptyOtherLists = (route) => {
  if (route.startsWith('/api/drafts?') || route.startsWith('/api/todos?'))
    return { items: [], total: 0, page: 1, limit: 200, hasMore: false };
  throw Error(`予期しない経路: ${route}`);
};
const issue = (number) => ({
  project: 'owner/repo',
  state: 'open',
  number,
  title: `依頼 ${number}`,
  body: '本文',
});

test('依頼を対象プロジェクト・開いた状態で絞って読む', async () => {
  const urls = [];
  const result = await readPreflightCandidates(async (method, route) => {
    assert.equal(method, 'GET');
    urls.push(route);
    if (route.startsWith('/api/issues/search?'))
      return { total: 0, issues: [], hasMore: false };
    return emptyOtherLists(route);
  }, 'owner/repo');
  assert.deepEqual(result.unavailable, []);
  assert.deepEqual(
    urls.filter((url) => url.startsWith('/api/issues')),
    ['/api/issues/search?project=owner%2Frepo&state=open&limit=100&page=1'],
  );
  assert.equal(urls.filter((url) => url === '/api/issues').length, 0);
});

test('依頼の一覧が複数ページでも全ページを読み、途中失敗は未確認にする', async () => {
  const pages = [];
  let fail = false;
  const call = async (_method, route) => {
    const url = new URL(route, 'http://local');
    if (url.pathname === '/api/issues/search') {
      const page = Number(url.searchParams.get('page'));
      pages.push(page);
      if (page === 2 && fail) throw Error('読取失敗');
      return { total: 2, issues: [issue(page)], hasMore: page === 1 };
    }
    if (url.pathname === '/api/issue')
      return issue(Number(url.searchParams.get('number')));
    return emptyOtherLists(route);
  };
  const result = await readPreflightCandidates(call, 'owner/repo');
  assert.deepEqual(pages, [1, 2]);
  assert.deepEqual(
    result.rows.map((row) => row.number),
    [1, 2],
  );
  assert.deepEqual(result.unavailable, []);
  pages.length = 0;
  fail = true;
  const failed = await readPreflightCandidates(call, 'owner/repo');
  assert.deepEqual(pages, [1, 2]);
  assert.deepEqual(failed.unavailable, ['依頼']);
  assert.equal(failed.rows.filter((row) => row.kind === '依頼').length, 0);
});

test('依頼の空の継続ページや不正な応答は候補なしと判定しない', async () => {
  for (const response of [
    { total: 1, issues: [], hasMore: true },
    { total: 0, issues: null, hasMore: false },
    { total: 0, issues: [], hasMore: 'false' },
    { issues: [], hasMore: false },
  ]) {
    const warnings = [];
    let observed;
    await consultationPreflight({
      text: '依頼本文',
      project: 'owner/repo',
      call: async (_method, route) =>
        route.startsWith('/api/issues/search?')
          ? response
          : emptyOtherLists(route),
      warn: (line) => warnings.push(line),
      onCandidates: (value) => {
        observed = value;
      },
    });
    assert.deepEqual(observed.unavailable, ['依頼']);
    assert.equal(observed.rows.filter((row) => row.kind === '依頼').length, 0);
    assert.ok(warnings.some((line) => line.includes('候補照合不能')));
    assert.ok(
      !warnings.some((line) => line.includes('重複候補は見つかりませんでした')),
    );
  }
});

test('送信前点検はフェンス内の見出しや否定を根拠の節として扱わない', () => {
  const text =
    '## assumptions\n確認されていない\n~~~md\n## 別の節\n未実装\n~~~\n未対応\n## 補足\n```md\n## assumptions\n```\n未検査';
  const rows = preflightLines(text);
  assert.deepEqual(
    rows.filter((row) => row.claim).map((row) => row.line),
    ['確認されていない', '未対応'],
  );
});

test('Go と共有するフェンス入力表で各行を判定する', async () => {
  const { readFile } = await import('node:fs/promises');
  const { markdownLines } = await import(
    '../shared/src/intake-request-size.ts'
  );
  const cases = JSON.parse(
    await readFile(
      new URL('./fixtures/markdown-fences.json', import.meta.url),
      'utf8',
    ),
  );
  for (const item of cases) {
    for (const text of [item.text, item.text.replaceAll('\r\n', '\n')]) {
      const rows = markdownLines(text);
      assert.deepEqual(
        rows.map((row) => row.fenced),
        item.fenced,
        item.name,
      );
      assert.deepEqual(
        rows.map((row) => row.line),
        item.lines,
        item.name,
      );
    }
  }
});

test('起票前照合は行がそろっていない依頼の一覧を読取失敗にする', async () => {
  const call = async (_method, route) =>
    route.startsWith('/api/issues/search?')
      ? {
          total: 1,
          issues: [{ project: 'owner/repo', number: 1, state: 'open' }],
          hasMore: false,
          incomplete: true,
          unreadProjects: [
            { project: 'owner/gone', reason: 'GiteaがHTTP 404を返しました。' },
          ],
        }
      : emptyOtherLists(route);
  const result = await readPreflightCandidates(call, 'owner/repo');
  assert.deepEqual(result.unavailable, ['依頼']);
  assert.equal(result.rows.filter((row) => row.kind === '依頼').length, 0);
});
