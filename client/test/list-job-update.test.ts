import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { pathToFileURL } from 'node:url';
import { sharedClientBuild } from './shared-client-build.ts';
import { compileSource } from '../scripts/source.ts';

test('仕事の更新日時で一覧を並べ替え行に同じ日時と状態を表示する', async () => {
  const { destination, ui } = sharedClientBuild();
  const model = await import(
    pathToFileURL(path.join(destination, 'v', ui, 'request-list.js')).href
  );
  const rows = [
    {
      kind: 'issue',
      project: 'owner/repo',
      number: 1,
      status: 'queued',
      updatedAt: '2026-11-03T00:00:00.000000000Z',
    },
    {
      kind: 'issue',
      project: 'owner/repo',
      number: 2,
      status: 'running',
      updatedAt: '2026-11-02T00:00:00.000000000Z',
    },
  ];
  assert.deepEqual(
    model
      .sortRows(rows, { field: 'updated', direction: 'desc' })
      .map((r: any) => r.number),
    [1, 2],
  );
  assert.deepEqual(
    model
      .sortRows(rows, { field: 'updated', direction: 'asc' })
      .map((r: any) => r.number),
    [2, 1],
  );
  const context = vm.createContext({
    ...model,
    shortTime: () => '短い日時',
    repositoryDisplay: (p: string) => p,
    meta: { projects: [] },
    el: (tag: string, attrs: object, ...children: unknown[]) => ({
      tag,
      attrs,
      children,
    }),
  });
  const source = ['app-parts.ts', 'app-list-rows.ts']
    .map((name) =>
      fs.readFileSync(new URL('../src/ui/' + name, import.meta.url), 'utf8'),
    )
    .join('\n');
  vm.runInContext(compileSource(source, 'rows.ts'), context);
  for (const row of rows) {
    context.row = row;
    const rendered = vm.runInContext(
      'listRowChildren(row, "issues", false)',
      context,
    );
    assert.equal(rendered.at(-1).tag, 'time');
    assert.equal(
      rendered.at(-1).attrs.datetime,
      new Date(row.updatedAt).toISOString(),
    );
    assert.equal(
      rendered[0].attrs.text,
      row.status === 'queued' ? '受付済み' : '作業中',
    );
  }
  rows[0].updatedAt = '2026-11-01T12:00:00.000000000Z';
  assert.deepEqual(
    model
      .sortRows(rows, { field: 'updated', direction: 'desc' })
      .map((r: any) => r.number),
    [2, 1],
  );
});
