// アーカイブ済みの旧リポジトリの名前が実装・設定・Skill に戻るのを止める固定テスト。
// 旧リポジトリは読み取り専用なので、書き込み先に旧名が戻ると Gitea に断られる不具合になる。
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root = fileURLToPath(new URL('..', import.meta.url));
const legacyName = /mofua-suki\/issue-desk(?![-\w])/;
// Git 管理外の生成物と依存。管理下でない物は固定テストの環境に無いこともあるので見ない。
// .tmp はGit対象外の作業場所で、並行するテストが作っては消すので読まない。
const skippedEntries = new Set([
  '.git',
  '.tmp',
  'node_modules',
  'dist',
  'bin',
  'runtime',
  'private',
]);

// 旧名を残してよいファイルと理由。書き込み先に使う箇所はここへ足さず、現在の project へ直す。
const allowed = new Map([
  [
    'deploy/nas/deploy-server.sh',
    '過去の Issue を指す temporary-code の記録で、書き込み先ではない',
  ],
  [
    'server/internal/fleet/system_project.go',
    '旧名で保存された記録を読み替える互換の定数',
  ],
  [
    'server/internal/issueapi/coordination.go',
    '旧リポジトリに残るお知らせ Issue を判定する互換の定数',
  ],
  [
    'server/internal/startup/background.go',
    '旧リポジトリに残るお知らせ Issue を判定する互換の定数',
  ],
  [
    'server/internal/httpapi/hostd_routes_test.go',
    '旧名の設定を無視することを確かめる試験データ',
  ],
  [
    'server/internal/issueapi/coordination_test.go',
    '互換の判定を確かめる試験データ',
  ],
  [
    'server/internal/consultations/family-progress_test.go',
    '旧 Issue の進捗を確認する読取専用フィクスチャ',
  ],
  [
    'server/internal/startup/family_progress_test.go',
    '旧 Issue 履歴の読取経路を確認するフィクスチャ',
  ],
  [
    'server/internal/operations/ai_failure_legacy_time_test.go',
    '旧名で保存された要対応を読み替える試験データ',
  ],
  [
    'server/internal/startup/administrative_issue_test.go',
    '互換の判定を確かめる試験データ',
  ],
  ['test/legacy-repo-name.test.ts', 'この検査自体'],
]);

function findLegacyNames(directory) {
  const found = [];
  const walk = (current) => {
    for (const entry of fs.readdirSync(current, { withFileTypes: true })) {
      // 監視の作業場所では .git がファイルで読めないので、種類を問わず名前で飛ばす。
      if (skippedEntries.has(entry.name)) continue;
      const full = path.join(current, entry.name);
      if (entry.isDirectory()) {
        walk(full);
      } else if (entry.isFile()) {
        const data = fs.readFileSync(full);
        if (data.includes(0)) continue;
        data
          .toString('utf8')
          .split('\n')
          .forEach((line, index) => {
            if (legacyName.test(line)) {
              found.push({
                file: path.relative(directory, full).split(path.sep).join('/'),
                line: index + 1,
              });
            }
          });
      }
    }
  };
  walk(directory);
  return found;
}

function report(found, allowedFiles) {
  return found
    .filter(({ file }) => !allowedFiles.has(file))
    .map(
      ({ file, line }) =>
        `${file}:${line} アーカイブ済みの旧リポジトリ名です。現在の project（mofua-suki/issue-desk-intake・mofua-suki/issue-desk-worker）に直してください`,
    );
}

test('実装・設定・Skill に旧リポジトリ名が戻っていない', () => {
  const problems = report(findLegacyNames(root), allowed);
  assert.deepEqual(problems, [], problems.join('\n'));
});

test('許可リストは今も旧名を含むファイルだけを挙げ、理由を持つ', () => {
  const files = new Set(findLegacyNames(root).map(({ file }) => file));
  const stale = [...allowed.keys()].filter((file) => !files.has(file));
  assert.deepEqual(
    stale,
    [],
    `旧名が無くなったので許可リストから外してください: ${stale.join(', ')}`,
  );
  for (const [file, reason] of allowed)
    assert.ok(reason.trim(), `${file} の理由がありません`);
});

test('旧名の1行を見つけ、後ろに続く名前と生成物は見逃す', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'legacy-repo-name-'));
  try {
    fs.mkdirSync(path.join(dir, 'skills'));
    fs.mkdirSync(path.join(dir, 'node_modules'));
    fs.writeFileSync(
      path.join(dir, 'skills', 'a.md'),
      '# 題\n--repo mofua-suki/issue-desk\n',
    );
    fs.writeFileSync(
      path.join(dir, 'b.go'),
      'const r = "mofua-suki/issue-desk"\nconst n = "mofua-suki/issue-desk#1"\n',
    );
    fs.writeFileSync(
      path.join(dir, 'ok.json'),
      '"mofua-suki/issue-desk-intake" "mofua-suki/issue-desk-worker"\n',
    );
    fs.writeFileSync(
      path.join(dir, 'node_modules', 'x.js'),
      'mofua-suki/issue-desk\n',
    );
    fs.writeFileSync(path.join(dir, '.git'), 'mofua-suki/issue-desk\n', {
      mode: 0o000,
    });
    const found = findLegacyNames(dir);
    assert.deepEqual(found.map(({ file, line }) => `${file}:${line}`).sort(), [
      'b.go:1',
      'b.go:2',
      'skills/a.md:2',
    ]);
    const problems = report(found, new Set(['b.go']));
    assert.equal(problems.length, 1);
    assert.match(
      problems[0],
      /^skills\/a\.md:2 アーカイブ済みの旧リポジトリ名です。/,
    );
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});
