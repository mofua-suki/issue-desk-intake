import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { execFileSync } from 'node:child_process';

test('hostdの更新は既存資格情報を使い、配置済みなら重複導入しない', () => {
  const work = fs.mkdtempSync(path.join(os.tmpdir(), 'desk-hostd-install-'));
  try {
    const bin = path.join(work, 'bin');
    const source = path.join(work, 'worker/deploy');
    const hostd = path.join(work, 'hostd');
    fs.mkdirSync(path.join(source, 'client/linux-hostd'), { recursive: true });
    fs.mkdirSync(bin);
    fs.mkdirSync(hostd);
    const executable = (file, text) =>
      fs.writeFileSync(file, '#!/bin/sh\nset -eu\n' + text, { mode: 0o700 });
    executable(path.join(bin, 'node'), 'exec "$TEST_NODE" "$@"\n');
    executable(path.join(bin, 'crontab'), 'cat "$TEST_WORK/cron"\n');
    executable(path.join(bin, 'ssh'), 'echo unexpected_ssh >&2; exit 99\n');
    fs.writeFileSync(
      path.join(work, 'cron'),
      '*/5 * * * * old-hostd # issue-desk-linux-hostd\n',
    );
    fs.writeFileSync(
      path.join(hostd, 'config.json'),
      JSON.stringify({ host: 'fixture' }),
    );
    const token = 'idh_' + 'x'.repeat(43);
    const password = 'private-registry-fixture';
    fs.writeFileSync(path.join(hostd, 'token'), token + '\n');
    fs.writeFileSync(path.join(hostd, 'registry-password'), password + '\n');
    fs.writeFileSync(
      path.join(work, '.env'),
      'REGISTRY=registry.invalid\nISSUE_DESK_CLIENT_TAG=123456789abc\n',
    );
    fs.writeFileSync(path.join(source, 'update-linux.sh'), 'fixture');
    fs.writeFileSync(path.join(work, 'update-linux.sh'), 'fixture');
    executable(
      path.join(source, 'client/linux-hostd/install.sh'),
      `
[ "$1" = "$TEST_WORK" ] && [ "$2" = fixture ]
[ "$3" = https://desk.invalid/ ] && [ "$4" = registry.invalid/issue-desk-client:123456789abc ]
IFS= read -r token
IFS= read -r password
[ "$token" = "$TEST_TOKEN" ] && [ "$password" = "$TEST_PASSWORD" ]
printf 'installed\\n' >>"$TEST_WORK/count"
printf '#!/bin/sh\\nexit 0\\n' >"$TEST_WORK/hostd/hostd"
chmod 700 "$TEST_WORK/hostd/hostd"
printf "*/5 * * * * '%s/hostd/hostd' --config '%s/hostd/config.json' # issue-desk-linux-hostd\\n" "$TEST_WORK" "$TEST_WORK" >"$TEST_WORK/cron"
echo linux_hostd_installed
`,
    );
    const script = path.resolve(
      import.meta.dirname,
      '../deploy/clients/bootstrap-ubuntu-hostd.sh',
    );
    const env = {
      ...process.env,
      PATH: bin + ':' + process.env.PATH,
      TEST_WORK: work,
      TEST_NODE: process.execPath,
      TEST_TOKEN: token,
      TEST_PASSWORD: password,
      BOOTSTRAP_HOST: 'fixture',
      BOOTSTRAP_SERVER_URL: 'https://desk.invalid/',
    };
    const first = execFileSync('bash', [script, work], {
      env,
      encoding: 'utf8',
    });
    const second = execFileSync('bash', [script, work], {
      env,
      encoding: 'utf8',
    });
    assert.equal(
      fs.readFileSync(path.join(work, 'count'), 'utf8'),
      'installed\n',
    );
    assert.match(first, /linux_hostd_installed/);
    assert.match(second, /飛ばした/);
    for (const secret of [token, password])
      assert.equal((first + second).includes(secret), false);
  } finally {
    fs.rmSync(work, { recursive: true, force: true });
  }
});
