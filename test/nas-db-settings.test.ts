import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

// この検査は対象サービスの command を限定形式で読み、解釈できない変更は失敗させる。
function checkSettings(compose) {
  const lines = compose.split(/\r?\n/);
  const services = lines.findIndex((line) =>
    /^services:\s*(?:#.*)?$/.test(line),
  );
  assert.notEqual(services, -1, 'services が必要');
  let inService = false;
  let found = 0;
  let command;
  for (const line of lines.slice(services + 1)) {
    if (!line.trim() || /^\s*#/.test(line)) continue;
    if (/^\S/.test(line)) break;
    const service = /^ {2}([\w-]+):\s*(?:#.*)?$/.exec(line);
    if (service) {
      inService = service[1] === 'issue-desk-db';
      if (inService) found++;
      continue;
    }
    if (!inService) continue;
    assert(!/^ {2}\S/.test(line), 'サービスの形式が不正');
    if (/^ {4}(entrypoint|configs):/.test(line))
      assert.fail('起動方法の上書きは禁止');
    if (/^ {4}command\s*:/.test(line)) {
      assert.equal(command, undefined, 'command が重複');
      const match = /^ {4}command: (\[.*\])\s*(?:#.*)?$/.exec(line);
      assert(match, 'command はインライン配列で指定');
      command = JSON.parse(match[1]);
    }
  }
  assert.equal(found, 1, '対象サービスは1つ必要');
  assert.deepEqual(command, [
    'postgres',
    '-c',
    'wal_compression=on',
    '-c',
    'max_wal_size=4GB',
    '-c',
    'checkpoint_timeout=15min',
  ]);
}

const compose = readFileSync(
  new URL('../deploy/nas/compose.yaml', import.meta.url),
  'utf8',
);

test('配備する DB の起動設定を検査する', () => {
  checkSettings(compose);
});

test('設定の削除・誤値・耐久性上書きを拒否する', () => {
  for (const setting of [
    'wal_compression=on',
    'max_wal_size=4GB',
    'checkpoint_timeout=15min',
  ]) {
    assert.throws(() =>
      checkSettings(compose.replace(`, "-c", "${setting}"`, '')),
    );
    assert.throws(() =>
      checkSettings(compose.replace(setting, `${setting}_wrong`)),
    );
  }
  for (const setting of ['synchronous_commit', 'fsync', 'full_page_writes']) {
    assert.throws(() =>
      checkSettings(
        compose.replace(
          '"postgres", "-c"',
          `"postgres", "-c", "${setting}=off", "-c"`,
        ),
      ),
    );
  }
  const misleading =
    compose.replace(/ {4}command: \[.*\]\n/, '') +
    '\n# wal_compression=on max_wal_size=4GB checkpoint_timeout=15min\n';
  assert.throws(() => checkSettings(misleading));
  const command = compose.match(/^ {4}command: \[.*\]$/m)[0];
  const otherService = compose
    .replace(`${command}\n`, '')
    .replace(
      '  issue-desk-intake-blue:\n',
      `  issue-desk-intake-blue:\n${command}\n`,
    );
  assert.throws(() => checkSettings(otherService));
});
