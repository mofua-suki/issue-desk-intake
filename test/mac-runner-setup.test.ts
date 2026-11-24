import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { spawnSync } from 'node:child_process';

const root = path.resolve(import.meta.dirname, '..');
const script = path.join(root, 'deploy/clients/macbook-pro-runner-setup.sh');

// Macのコマンドを記録する偽物に差し替え、runnerの準備を流す（実際のビルド・launchctlは行わない）。
function fakeMac(
  t,
  { token = true, registered = false, checkExit = 0, prepared = false } = {},
) {
  fs.mkdirSync(path.join(root, '.tmp'), { recursive: true });
  const dir = fs.mkdtempSync(path.join(root, '.tmp', 'mac-runner-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const bin = path.join(dir, 'bin'),
    home = path.join(dir, 'home'),
    log = path.join(dir, 'calls.log');
  fs.mkdirSync(bin);
  fs.mkdirSync(path.join(home, '.issue-desk/tools/go/bin'), {
    recursive: true,
  });
  fs.mkdirSync(path.join(home, '.issue-desk/apple/private'), {
    recursive: true,
  });
  if (token)
    fs.writeFileSync(
      path.join(home, '.issue-desk/apple/private/pc-token'),
      'placeholder\n',
      { mode: 0o600 },
    );
  const shim = (file, body) =>
    fs.writeFileSync(file, `#!/bin/sh\n${body}\n`, { mode: 0o755 });
  shim(path.join(bin, 'uname'), '[ "$1" = -m ] && echo arm64 || echo Darwin');
  shim(path.join(bin, 'python3'), 'exit 0');
  // prepare.sh と同じく、準備済みの定義が既にあれば断り、無ければ定義を作る。
  const plist = path.join(
    home,
    'Library/Application Support/issue-desk/apple-prepared/macbook-pro-test/com.issue-desk.apple.macbook-pro-test.plist',
  );
  if (prepared || registered) {
    fs.mkdirSync(path.dirname(plist), { recursive: true });
    fs.writeFileSync(plist, '<plist>prepared</plist>\n');
  }
  shim(
    path.join(bin, 'bash'),
    `echo "bash $*" >> "${log}"; [ -e "${plist}" ] && { echo 既存の準備済みWorkerを上書きしません >&2; exit 1; }; mkdir -p "$(dirname "${plist}")"; echo '<plist>prepared</plist>' > "${plist}"`,
  );
  shim(
    path.join(bin, 'launchctl'),
    `echo "launchctl $*" >> "${log}"; [ "$1" = print ] && exit ${registered ? 0 : 1}; exit 0`,
  );
  shim(
    path.join(bin, 'git'),
    `echo "git $*" >> "${log}"
case "$1" in
--version) echo "git version 2.50";;
clone) for last; do :; done; mkdir -p "$last/.git" "$last/cmd/worker";;
-C) [ "$3" = rev-parse ] && echo abc1234;;
esac
exit 0`,
  );
  // go build -o <出力> は、検査で呼ばれる worker の偽物を作る。
  shim(
    path.join(home, '.issue-desk/tools/go/bin/go'),
    `echo "go $* GOOS=$GOOS GOARCH=$GOARCH" >> "${log}"
out=''; while [ $# -gt 0 ]; do [ "$1" = -o ] && out=$2; shift; done
printf '#!/bin/sh\\necho "worker $*" >> "${log}"; exit ${checkExit}\\n' > "$out"; chmod +x "$out"`,
  );
  const run = (...args) =>
    spawnSync('/bin/sh', [script, ...args], {
      encoding: 'utf8',
      env: { PATH: `${bin}:/usr/bin:/bin`, HOME: home },
    });
  const calls = () => fs.readFileSync(log, 'utf8').trim().split('\n');
  return { home, run, calls };
}

test('runnerの準備は接続設定を書き、Workerをビルドして検査し、通ったときだけ常駐させる', (t) => {
  const mac = fakeMac(t);
  const result = mac.run();
  assert.equal(result.status, 0, result.stdout + result.stderr);
  const k2 = path.join(mac.home, '.issue-desk/apple/apple-k2.json');
  assert.deepEqual(JSON.parse(fs.readFileSync(k2, 'utf8')), {
    serverUrl: 'https://issue-desk.example.ts.net',
    serverTokenFile: path.join(mac.home, '.issue-desk/apple/private/pc-token'),
    workerId: 'macbook-pro-test',
  });
  const calls = mac.calls();
  const worker = path.join(mac.home, '.issue-desk/apple/bin/worker');
  assert.ok(
    calls.some((line) =>
      /^go build -o .*worker\.new \.\/cmd\/worker GOOS=darwin GOARCH=arm64$/.test(
        line,
      ),
    ),
  );
  const check = calls.indexOf(`worker apple check --config ${k2}`);
  const prepare = calls.findIndex(
    (line) =>
      line.startsWith('bash ') &&
      line.endsWith(
        `prepare.sh --start --role test-only --config ${k2} --worker ${worker}`,
      ),
  );
  assert.ok(check >= 0 && prepare > check, calls.join('\n'));
  assert.match(result.stdout, /完了: macbook-pro-test/);
});

test('検査に失敗したら常駐させず、理由を示して止まる', (t) => {
  const mac = fakeMac(t, { checkExit: 3 });
  const result = mac.run();
  assert.equal(result.status, 1);
  assert.match(result.stderr, /runnerの検査に失敗しました/);
  assert.ok(!mac.calls().some((line) => line.includes('prepare.sh')));
});

test('常駐済みのrunnerは登録し直さず、新しいWorkerで再起動する', (t) => {
  const mac = fakeMac(t, { registered: true });
  const result = mac.run();
  assert.equal(result.status, 0, result.stdout + result.stderr);
  const calls = mac.calls();
  assert.ok(
    calls.some((line) =>
      /^launchctl kickstart -k gui\/\d+\/com\.issue-desk\.apple\.macbook-pro-test$/.test(
        line,
      ),
    ),
    calls.join('\n'),
  );
  assert.ok(!calls.some((line) => line.includes('prepare.sh')));
});

test('再起動などで登録だけが消えていれば、準備済みの定義を読み込み直し、ログイン時の起動の定義も置く', (t) => {
  const mac = fakeMac(t, { prepared: true });
  const result = mac.run();
  assert.equal(result.status, 0, result.stdout + result.stderr);
  const calls = mac.calls();
  assert.ok(
    calls.some((line) =>
      /^launchctl bootstrap gui\/\d+ .*apple-prepared\/macbook-pro-test\/com\.issue-desk\.apple\.macbook-pro-test\.plist$/.test(
        line,
      ),
    ),
    calls.join('\n'),
  );
  assert.ok(
    calls.some((line) =>
      /^launchctl kickstart gui\/\d+\/com\.issue-desk\.apple\.macbook-pro-test$/.test(
        line,
      ),
    ),
  );
  assert.ok(!calls.some((line) => line.includes('prepare.sh')));
  assert.equal(
    fs.readFileSync(
      path.join(
        mac.home,
        'Library/LaunchAgents/com.issue-desk.apple.macbook-pro-test.plist',
      ),
      'utf8',
    ),
    '<plist>prepared</plist>\n',
  );
});

test('PC別トークンが無ければ、何も書かずに置き場所を示して止まる', (t) => {
  const mac = fakeMac(t, { token: false });
  const result = mac.run();
  assert.equal(result.status, 1);
  assert.match(result.stderr, /pc-token がありません/);
  assert.equal(
    fs.existsSync(path.join(mac.home, '.issue-desk/apple/apple-k2.json')),
    false,
  );
});

test('--source を付けると、GiteaからWorkerを取らず、渡されたソースでビルドして常駐させる', (t) => {
  const mac = fakeMac(t);
  const source = path.join(mac.home, 'sent-worker');
  fs.mkdirSync(path.join(source, 'cmd/worker'), { recursive: true });
  fs.writeFileSync(path.join(source, 'go.mod'), 'module issue-desk-worker\n');
  const result = mac.run('--source', source);
  assert.equal(result.status, 0, result.stdout + result.stderr);
  const calls = mac.calls();
  assert.ok(
    !calls.some((line) => /^git (clone|-C)/.test(line)),
    calls.join('\n'),
  );
  assert.ok(
    calls.some(
      (line) =>
        line.startsWith('bash ') &&
        line.includes(`${source}/deploy/client/macos-apple/prepare.sh --start`),
    ),
    calls.join('\n'),
  );
  assert.match(result.stdout, /送られたソースで作りました/);
  const missing = fakeMac(t).run('--source', path.join(mac.home, 'nothing'));
  assert.equal(missing.status, 1);
  assert.match(missing.stderr, /Workerのソースがありません/);
});
