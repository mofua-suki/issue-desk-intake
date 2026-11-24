import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import { compileSource } from '../scripts/source.ts';

function page(storage: Map<string, string>, ui = 'version-1') {
  const context = vm.createContext({
    document: { documentElement: { dataset: { ui } } },
    sessionStorage: {
      getItem: (key: string) => storage.get(key) ?? null,
      setItem: (key: string, value: string) => storage.set(key, value),
      removeItem: (key: string) => storage.delete(key),
    },
    listCache: null,
    cancelTimer: () => {},
    Date,
  });
  const source = ['app-list-sync.ts', 'app-updates.ts']
    .map((name) =>
      fs.readFileSync(new URL('../src/ui/' + name, import.meta.url), 'utf8'),
    )
    .join('\n');
  vm.runInContext(compileSource(source, 'page.ts'), context);
  const run = (code: string, values: Record<string, unknown> = {}) => {
    Object.assign(context, values);
    return vm.runInContext(code, context);
  };
  return { run, context };
}

const data = () => ({
  at: 123,
  issues: [],
  drafts: [],
  todos: [],
  closes: [],
  scope: 'active',
  cursor: 'c1',
  stalls: [{ id: 'stall-1' }],
});

test('保存から再読込し、空差分で停止通知を保持して明示的な解除を反映する', () => {
  const storage = new Map<string, string>();
  page(storage).run('keepList(data)', { data: data() });
  const reloaded = page(storage);
  reloaded.run('listCache = readList()');
  assert.equal(reloaded.context.listCache.stalls.length, 1);
  assert.equal(reloaded.context.listCache.stalls[0].id, 'stall-1');
  const apply = (cursor: string, aux?: object) => {
    const response = {
      full: false,
      scope: 'active',
      cursor,
      added: [],
      updated: [],
      removed: [],
      ...(aux ? { aux } : {}),
    };
    reloaded.run(
      '{ const next = listIntegrate(response, 1, listCache.cursor); if (!next) throw Error("差分を適用できません"); listCommit(1, response); keepList(next); }',
      { response },
    );
  };
  apply('c2');
  assert.equal(reloaded.context.listCache.stalls.length, 1);
  apply('c3', { stalls: [] });
  assert.equal(reloaded.context.listCache.stalls.length, 0);
});

test('旧形式と不正な停止通知を空にし、版違いと必須項目不正を拒否する', () => {
  const storage = new Map<string, string>();
  const initial = page(storage);
  initial.run('keepList(data)', { data: data() });
  const key = 'issue-desk:list';
  const saved = JSON.parse(storage.get(key)!);
  for (const stalls of [undefined, null, {}, 'invalid']) {
    storage.set(key, JSON.stringify({ ...saved, stalls }));
    assert.equal(page(storage).run('readList().stalls.length'), 0);
  }
  storage.set(key, JSON.stringify(saved));
  assert.equal(page(storage, 'version-2').run('readList()'), null);
  for (const field of ['issues', 'drafts', 'todos', 'closes', 'at']) {
    storage.set(key, JSON.stringify({ ...saved, [field]: null }));
    assert.equal(page(storage).run('readList()'), null, field);
  }
});

// 文字列化された回数を数える相談の行。
function countedRows() {
  const counts = new Map<string, number>();
  const row = (id: string, title = '相談 ' + id) => ({
    id,
    status: 'draft',
    createdAt: '2026-11-20T00:00:00.000Z',
    title,
    toJSON() {
      counts.set(id, (counts.get(id) ?? 0) + 1);
      return { id, status: this.status, createdAt: this.createdAt, title };
    },
  });
  return { counts, row };
}
const savedCursor = (storage: Map<string, string>) =>
  JSON.parse(storage.get('issue-desk:list')!).cursor;

test('空差分では基準と同じ行を保存の1回だけ文字列化し、差分のカーソルを保存する', () => {
  const storage = new Map<string, string>();
  const { counts, row } = countedRows();
  const list = page(storage);
  list.run('keepList(data)', {
    data: { ...data(), drafts: [row('d1'), row('d2')] },
  });
  counts.clear();
  list.run(
    '{ const next = listIntegrate(response, 1, listCache.cursor); if (!next) throw Error("差分を適用できません"); listCommit(1, response); keepList(next); }',
    {
      response: {
        full: false,
        scope: 'active',
        cursor: 'c2',
        added: [],
        updated: [],
        removed: [],
      },
    },
  );
  assert.deepEqual(
    [...counts],
    [
      ['d1', 1],
      ['d2', 1],
    ],
  );
  assert.equal(savedCursor(storage), 'c2');
  assert.deepEqual(
    JSON.parse(storage.get('issue-desk:list')!).drafts.map(
      (d: { id: string }) => d.id,
    ),
    ['d1', 'd2'],
  );
});

test('行の参照が基準と違うときは中身を比べ、同じならカーソルを保存し、違えば保存しない', () => {
  const { row } = countedRows();
  const keep = (baseline: object[], drafts: object[]) => {
    const storage = new Map<string, string>();
    page(storage).run('listBaseline = baseline; keepList(next)', {
      baseline: { ...data(), drafts: baseline },
      next: { ...data(), cursor: 'c2', drafts },
    });
    return savedCursor(storage);
  };
  // 別の取得で読んだ同じ内容の行。
  assert.equal(keep([row('d1'), row('d2')], [row('d1'), row('d2')]), 'c2');
  // 個別取得などで基準から変わった行・増減した行があれば、差分の起点にできない。
  const shared = row('d1');
  assert.equal(keep([shared, row('d2')], [shared, row('d2', '変更後')]), null);
  assert.equal(keep([shared, row('d2')], [shared]), null);
  assert.equal(keep([shared], [shared, row('d2')]), null);
});
