import assert from 'node:assert/strict';
import test from 'node:test';
import fs from 'node:fs';
import path from 'node:path';
import { readGrantRules } from '../deploy/nas/permission-grants-apply.ts';
import { isUsageReport } from '../scripts/chat-usage-report.ts';

const root = path.resolve(import.meta.dirname, '..');
const { rules, invalid } = readGrantRules(
  path.join(root, 'deploy/nas/permission-grant-rules.json'),
);
const read = (file: string) => fs.readFileSync(path.join(root, file), 'utf8');

test('規則ファイルのすべての規則を反映係が使える', () => {
  assert.deepEqual(invalid, []);
  assert.deepEqual([...rules.keys()].sort(), [
    'docker-compose-ps',
    'docker-inspect',
    'docker-stats',
    'usage-per-issue',
  ]);
});

test('規則が呼ぶコンテナ内のファイルは配置物に実在する', () => {
  const dockerfile = read('deploy/nas/Dockerfile');
  for (const [id, rule] of rules) {
    for (const [, file] of rule.matchAll(/(?:^|\s)\/app\/(\S+)/g))
      assert.ok(fs.existsSync(path.join(root, file)), `${id}: /app/${file}`);
    for (const [binary] of rule.matchAll(/\/usr\/local\/bin\/\S+/g))
      assert.ok(dockerfile.includes(` ${binary}\n`), `${id}: ${binary}`);
  }
});

test('usage-per-issue の規則は依頼別使用量レポートの引数だけを任意にする', () => {
  const rule = rules.get('usage-per-issue');
  assert.ok(rule);
  const body = rule.slice('Bash('.length, -1);
  const prefix =
    'sh ~/Workspace/issue-desk-worker/deploy/nas/intake.sh exec -T ';
  assert.ok(body.startsWith(prefix), body);
  assert.equal(body.indexOf('*'), body.length - 1, '* は末尾の1か所だけ');
  assert.ok(body.endsWith(' usage-per-issue report --from *'), body);
  const args = body.slice(prefix.length, -1).split(' ').filter(Boolean);
  assert.equal(
    isUsageReport([
      ...args,
      '2026-11-01',
      '--to',
      '2026-11-30',
      '--format',
      'json',
    ]),
    true,
  );
});
