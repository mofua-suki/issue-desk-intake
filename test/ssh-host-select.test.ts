import test from 'node:test';
import assert from 'node:assert/strict';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { rankedHosts, selectHost } from '../scripts/ssh-host-select.ts';

const root = fileURLToPath(new URL('..', import.meta.url));
const policy = { hosts: { 'pc-a': {}, 'pc-b': {}, 'pc-c': {}, mac: {} } };
const assignments = {
  hosts: {
    third: { machineName: 'pc-c', repositories: { 'o/app': { rank: 3 } } },
    first: {
      machineName: 'pc-a',
      repositories: { 'o/app': { rank: 1 }, 'o/other': { rank: 2 } },
    },
    'pc-b': { repositories: { 'o/app': { rank: 2 } } },
    unregistered: { repositories: { 'o/app': { rank: 0 } } },
    tester: {
      machineName: 'mac',
      role: 'test-only',
      repositories: {},
      testRepositories: ['o/app'],
    },
  },
};

test('担当表の順位の高い順に、host-ssh.jsonに登録された実装担当のPC名を返す', () => {
  assert.deepEqual(rankedHosts(assignments, policy, 'o/app'), [
    'pc-a',
    'pc-b',
    'pc-c',
  ]);
  assert.equal(selectHost(assignments, policy, 'o/other'), 'pc-a');
  assert.throws(
    () => selectHost(assignments, policy, 'o/none'),
    /host-ssh.json に登録されたPCがありません/,
  );
});

test('同じ順位は担当名の順に並べ、毎回同じPCを選ぶ', () => {
  const tie = {
    hosts: {
      zeta: { machineName: 'pc-a', repositories: { 'o/app': { rank: 1 } } },
      alpha: { machineName: 'pc-b', repositories: { 'o/app': { rank: 1 } } },
    },
  };
  assert.deepEqual(rankedHosts(tie, policy, 'o/app'), ['pc-b', 'pc-a']);
});

test('コマンドは選んだPC名を1行で出し、引数の誤りは使い方を示して終了コード1にする', () => {
  const ok = spawnSync(
    process.execPath,
    [
      path.join(root, 'scripts/ssh-host-select.ts'),
      '--repo',
      'mofua-suki/issue-desk-intake',
    ],
    { encoding: 'utf8' },
  );
  assert.equal(ok.status, 0, ok.stderr);
  assert.match(ok.stdout, /^[a-z0-9][a-z0-9-]*\n$/);
  const bad = spawnSync(
    process.execPath,
    [path.join(root, 'scripts/ssh-host-select.ts'), '--host', 'x'],
    { encoding: 'utf8' },
  );
  assert.equal(bad.status, 1);
  assert.match(bad.stderr, /使い方/);
});
