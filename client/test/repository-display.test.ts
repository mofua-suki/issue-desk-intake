import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
import vm from 'node:vm';
import { shownProject } from '../src/ui/repository-display.ts';
import { compileSource } from '../scripts/source.ts';
import { sharedClientBuild } from './shared-client-build.ts';
import { repositoryDisplayScreens } from './repository-display-fixture.ts';

test('異なるownerの表示を短縮しても完全名の入力を変更しない', () => {
  const projects = Object.freeze([
    'first/alpha',
    'second/beta',
    'third/alpha-extra',
  ]);
  const issue = Object.freeze({
    project: projects[0],
    url: `https://example.invalid/${projects[0]}/issues/1`,
  });
  assert.deepEqual(
    projects.map((project) => shownProject(project, projects)),
    ['alpha', 'beta', 'alpha-extra'],
  );
  assert.equal(shownProject('another/repository'), 'repository');
  assert.equal(issue.project, 'first/alpha');
  assert.equal(issue.url, 'https://example.invalid/first/alpha/issues/1');
  assert.deepEqual(projects, [
    'first/alpha',
    'second/beta',
    'third/alpha-extra',
  ]);
});

test('別ownerの同名は完全名を残し同じ完全名の重複は短縮する', () => {
  const projects = Object.freeze([
    'first/repo',
    'second/repo',
    'first/unique',
    'first/unique',
  ]);
  assert.deepEqual(
    projects.map((project) => shownProject(project, projects)),
    ['first/repo', 'second/repo', 'unique', 'unique'],
  );
  assert.equal(
    shownProject('unregistered/repo', projects),
    'unregistered/repo',
  );
});

test('文字列以外の登録名を無視して残りの登録名で衝突を判定する', () => {
  const projects = [null, 42, {}, 'first/repo', 'first/unique', 'second/repo'];
  assert.equal(shownProject('first/unique', projects), 'unique');
  assert.equal(shownProject('first/repo', projects), 'first/repo');
});

test('欠測と空とowner/repo形式以外はそのまま表示する', () => {
  for (const project of [
    null,
    undefined,
    '',
    'repo',
    '/repo',
    'owner/',
    'owner/group/repo',
  ]) {
    assert.equal(shownProject(project), project);
  }
});

test('配布モジュールを画面部品から利用して登録一覧の更新と内部値を保つ', async () => {
  const built = sharedClientBuild();
  const display = await import(
    pathToFileURL(
      path.join(built.destination, 'v', built.ui, 'repository-display.js'),
    ).href
  );
  const reference = Object.freeze({
    resolution: 'resolved',
    desk: 100,
    project: 'other/repo',
    number: 7,
    title: '依頼',
  });
  const meta = {
    projects: [
      { id: null as string | null, name: '' },
      { id: 'other/repo', name: 'repo' },
    ],
  };
  const context = vm.createContext({
    repositoryDisplay: display.shownProject,
    meta,
    reference,
  });
  const parts = fs.readFileSync(
    new URL('../src/ui/app-parts.ts', import.meta.url),
    'utf8',
  );
  vm.runInContext(compileSource(parts, 'app-parts.ts'), context);
  assert.equal(
    vm.runInContext('referenceNote(reference)', context),
    '#100 → repo（Gitea番号 7）「依頼」',
  );
  meta.projects.push({ id: 'another/repo', name: 'repo' });
  assert.equal(
    vm.runInContext('referenceNote(reference)', context),
    '#100 → other/repo（Gitea番号 7）「依頼」',
  );
  assert.equal(reference.project, 'other/repo');
  assert.equal(reference.number, 7);
});

// 配布用の結合コードと独立モジュールで対象画面を実ブラウザで通す。
// 検出はソースの書き方ではなく、画面の表示文字と利用者向け属性を対象にする。
test('配布画面のリポジトリ表示と内部完全名を検証し直接表示の漏れを検出する', async (t) => {
  await repositoryDisplayScreens(t);
});
