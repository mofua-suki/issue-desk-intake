import test from 'node:test';
import assert from 'node:assert/strict';
import { registration } from '../scripts/mac-registration.ts';

const repo = 'mofua-suki/sample-expense-client';
function fixture(): [any, any] {
  return [
    {
      version: 1,
      defaults: {},
      hosts: {
        first: {
          giteaUrl: 'https://gitea.test',
          environment: { platform: 'linux' },
          repositories: [
            { name: repo, testCommand: ['flutter', 'test'] },
            { name: 'o/other', testCommand: ['go', 'test'] },
          ],
        },
        second: {
          repositories: [{ name: repo, testCommand: ['flutter', 'test'] }],
        },
      },
    },
    {
      hosts: {
        first: { maxImplementations: 2, repositories: { [repo]: { rank: 1 } } },
        second: {
          maxImplementations: 1,
          repositories: { [repo]: { rank: 2 } },
        },
      },
    },
  ];
}

test('テスト専用の登録は実装担当を増やさず全実装PCに検証先を付ける', () => {
  const [fleet, assignments] = fixture();
  const result = registration(fleet, assignments, {
    name: 'macbook',
    role: 'test-only',
    home: '/Users/tester',
  });
  assert.equal(result.worker, null);
  assert.equal(
    result.fleet.hosts['macbook-test'].repositories[0].nativeTestHost,
    'macbook-test',
  );
  assert.equal(
    result.assignments.workerAccounts['macbook-test'],
    'desk-macbook-test',
  );
  assert.deepEqual(result.assignments.hosts['macbook-test'].repositories, {});
  assert.deepEqual(result.assignments.hosts['macbook-test'].testRepositories, [
    repo,
  ]);
  for (const host of ['first', 'second'])
    assert.equal(
      result.fleet.hosts[host].repositories[0].nativeTestHost,
      'macbook-test',
    );
  assert.equal(
    result.fleet.hosts.first.repositories[1].nativeTestHost,
    undefined,
  );
  assert.equal(fleet.hosts.first.repositories[0].nativeTestHost, undefined);
  assert.equal(
    result.fleet.hosts['macbook-test'].runtimeDir,
    '/Users/tester/.issue-desk/apple/runtime',
  );
  // リポジトリ単位の期限は見直し期限と組でしか書けない（受付の一時設定の検査）ので、PC単位の期限だけを使う。
  assert.equal(
    result.fleet.hosts['macbook-test'].repositories[0].testTimeoutSeconds,
    undefined,
  );
});

test('実装兼用は別IDのLinux Workerを末尾の順位に追加する', () => {
  const result = registration(...fixture(), {
    name: 'macbook',
    role: 'implementation',
    home: '/Users/tester',
    releases: { hosts: { first: 'registry.test:5443' } },
  });
  assert.equal(result.worker, 'macbook-worker');
  assert.equal(result.releases.hosts['macbook-worker'], 'registry.test:5443');
  assert.equal(
    result.assignments.hosts['macbook-worker'].repositories[repo].rank,
    3,
  );
  assert.equal(
    result.fleet.hosts['macbook-worker'].environment.platform,
    'linux',
  );
  assert.equal(
    result.fleet.hosts['macbook-worker'].repositories[0].nativeTestHost,
    'macbook-test',
  );
  assert.equal(
    result.fleet.hosts['macbook-test'].environment.platform,
    'darwin',
  );
});

test('衝突するID・不正な役割・既存Mac担当の無断切替は拒否する', () => {
  const options = { name: 'macbook', role: 'test-only', home: '/Users/tester' };
  for (const changed of [
    { name: '../mac' },
    { role: 'both' },
    { home: '/tmp/tester' },
  ])
    assert.throws(() => registration(...fixture(), { ...options, ...changed }));
  const first = registration(...fixture(), options);
  assert.throws(
    () => registration(first.fleet, first.assignments, options),
    /登録済み/,
  );
  assert.throws(
    () =>
      registration(first.fleet, first.assignments, {
        ...options,
        name: 'another',
      }),
    /検証先が登録済み/,
  );
});

test('実装兼用はServerとClientを担当し、ClientだけをMac検証へ送る', () => {
  const [fleet, assignments] = fixture();
  assignments.repositoryGroups = [[repo, 'o/other']];
  assignments.hosts.first.repositories['o/other'] = { rank: 1 };
  const result = registration(fleet, assignments, {
    name: 'macbook',
    role: 'implementation',
    home: '/Users/tester',
    releases: { hosts: { first: 'registry.test:5443' } },
  });
  const worker = result.assignments.hosts['macbook-worker'];
  assert.equal(worker.repositories[repo].rank, 3);
  assert.equal(worker.repositories['o/other'].rank, 2);
  const settings = result.fleet.hosts['macbook-worker'].repositories;
  assert.equal(
    settings.find((entry) => entry.name === repo).nativeTestHost,
    'macbook-test',
  );
  assert.equal(
    settings.find((entry) => entry.name === 'o/other').nativeTestHost,
    undefined,
  );
});

test('gate=falseは登録だけ行い、実装担当にMac検証を必須にしない（2台目も登録できる）', () => {
  const [fleet, assignments] = fixture();
  const options = {
    name: 'macbook-pro',
    role: 'test-only',
    home: '/Users/tester',
    gate: false,
  };
  const first = registration(fleet, assignments, options);
  assert.deepEqual(
    first.assignments.hosts['macbook-pro-test'].testRepositories,
    [repo],
  );
  assert.equal(first.assignments.workerAccounts, undefined);
  assert.deepEqual(first.fleet.hosts.first, fleet.hosts.first);
  assert.deepEqual(first.fleet.hosts.second, fleet.hosts.second);
  const second = registration(first.fleet, first.assignments, {
    ...options,
    name: 'macbook-air',
  });
  assert.equal(
    second.fleet.hosts['macbook-air-test'].environment.platform,
    'darwin',
  );
  for (const host of ['first', 'second'])
    assert.equal(
      second.fleet.hosts[host].repositories[0].nativeTestHost,
      undefined,
    );
  const worker = registration(...fixture(), {
    ...options,
    role: 'implementation',
    releases: { hosts: { first: 'registry.test:5443' } },
  });
  assert.equal(
    worker.fleet.hosts['macbook-pro-worker'].repositories[0].nativeTestHost,
    undefined,
  );
  assert.deepEqual(worker.assignments.workerAccounts, {
    'macbook-pro-worker': 'desk-macbook-pro-worker',
  });
});

test('登録済みMacはテスト専用で、対象の実装担当はMacBook Proの検証を必須にする', async () => {
  const fs = await import('node:fs');
  const read = (file) =>
    JSON.parse(fs.readFileSync(new URL(`../${file}`, import.meta.url), 'utf8'));
  const fleet = read('fleet.json'),
    assignments = read('assignments.json');
  const macBookProRepositories = [repo, 'mofua-suki/alarm-checklist'];
  for (const host of ['macbook-pro-test', 'macbook-air-test']) {
    assert.equal(assignments.hosts[host].role, 'test-only');
    assert.deepEqual(assignments.hosts[host].repositories, {});
    assert.deepEqual(
      assignments.hosts[host].testRepositories,
      host === 'macbook-pro-test' ? macBookProRepositories : [repo],
    );
    assert.equal(fleet.hosts[host].environment.platform, 'darwin');
    // Giteaのアカウントは稼働の準備を始めたMac（MacBook Pro）だけに書く。
    assert.equal(
      assignments.workerAccounts[host],
      host === 'macbook-pro-test'
        ? 'issue-desk-worker-mac-book-pro'
        : undefined,
    );
    assert.deepEqual(
      fleet.hosts[host].repositories.map((entry) => entry.name),
      assignments.hosts[host].testRepositories,
    );
    for (const entry of fleet.hosts[host].repositories) {
      assert.deepEqual(entry.testCommand, [
        'sh',
        'scripts/verify-apple.sh',
        'ios',
      ]);
      assert.equal('testTimeoutSeconds' in entry, 'testTimeoutReview' in entry);
    }
  }
  for (const [host, config] of Object.entries<any>(fleet.hosts)) {
    if (assignments.hosts[host]?.role === 'test-only') continue;
    // 稼働しているのはMacBook Proだけ。MacBook Airは停止中で、どの実装担当の検証先にもしない。
    for (const entry of config.repositories ?? [])
      assert.equal(
        entry.nativeTestHost,
        macBookProRepositories.includes(entry.name) &&
          assignments.hosts[host]?.repositories?.[entry.name]
          ? 'macbook-pro-test'
          : undefined,
        `${host} ${entry.name}`,
      );
  }
});
