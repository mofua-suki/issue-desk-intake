import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { pathToFileURL } from 'node:url';
import { sharedClientBuild } from './shared-client-build.ts';
import { compileSource } from '../scripts/source.ts';

test('範囲超過の共通番号の行にファイルと操作案内を表示し更新する', async () => {
  const { destination, ui } = sharedClientBuild();
  const model = await import(
    pathToFileURL(path.join(destination, 'v', ui, 'request-list.js')).href
  );
  const message =
    '変更範囲が目安を超えました（5件、上限4件）。理由を付けて再開するか、閉じて分けてください。追加しようとしたファイル: src/長い名前.go。';
  const row = model.requestGroups(
    [
      {
        project: 'owner/repo',
        number: 1,
        desk: 42,
        status: 'stopped',
        error: message,
      },
    ],
    [],
    [],
  ).stopped[0];
  assert.ok(row);
  const context = vm.createContext({
    ...model,
    shownProject: (p: string) => p,
    deskLabel: (i: any) => `#${i.desk}`,
    badge: () => null,
    rowTime: () => null,
    el: (tag: string, attrs: object, ...children: unknown[]) => ({
      tag,
      attrs,
      children,
    }),
  });
  const source = fs.readFileSync(
    new URL('../src/ui/app-list-rows.ts', import.meta.url),
    'utf8',
  );
  vm.runInContext(compileSource(source, 'app-list-rows.ts'), context);
  context.row = row;
  const children = vm.runInContext(
    'listRowChildren(row, "attention", false)',
    context,
  );
  assert.match(JSON.stringify(children), /#42/);
  assert.match(JSON.stringify(children), /src\/長い名前\.go/);
  assert.match(JSON.stringify(children), /理由を付けて再開/);
  assert.match(JSON.stringify(children), /閉じて分けて/);
  const before = vm.runInContext(
    'listRowSignature(row, "attention", false)',
    context,
  );
  context.row = { ...row, error: message + '更新' };
  assert.notEqual(
    vm.runInContext('listRowSignature(row, "attention", false)', context),
    before,
  );
});
