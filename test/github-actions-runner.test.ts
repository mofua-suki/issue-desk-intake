import { strict as assert } from 'node:assert';
import { spawnSync } from 'node:child_process';
import {
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';

const operation = 'install-github-actions-runner';
const policy = JSON.parse(
  readFileSync(new URL('../host-ssh.json', import.meta.url), 'utf8'),
);

test('固定SSH操作は3台への書き込み操作であり、差し替えた入力を実行しない', () => {
  assert.deepEqual(policy.operationHosts[operation], [
    'gt12-pro',
    'nab-9s',
    'macbook-pro',
  ]);
  assert.ok(!policy.readOnlyOperations.includes(operation));
  const result = spawnSync('sh', ['-c', policy.operations[operation]], {
    input: Buffer.from('printf changed-installer').toString('base64') + '\n',
    encoding: 'utf8',
  });
  assert.equal(result.status, 1);
  assert.equal(result.stdout, '');
  assert.match(result.stderr, /SHA-256が一致しません/);
});

test('Linuxで対象外のPC・リポジトリを拒否し、有効な登録を再実行しても重複しない', (t) => {
  const installer = readFileSync(
    new URL(
      '../deploy/nas-host/install-github-actions-runner.sh',
      import.meta.url,
    ),
  );
  const home = mkdtempSync(join(tmpdir(), 'gha-runner-test.'));
  t.after(() => rmSync(home, { recursive: true, force: true }));
  const bin = join(home, 'bin');
  mkdirSync(bin);

  function tool(name: string, body: string) {
    writeFileSync(join(bin, name), '#!/bin/sh\nset -eu\n' + body, {
      mode: 0o755,
    });
  }

  tool('uname', 'case "$1" in -s) echo Linux;; -m) echo x86_64;; esac');
  tool('hostname', 'printf "%s\\n" "$FAKE_MACHINE"');
  tool(
    'loginctl',
    `
    if [ "$1" = show-user ]; then
      if [ -f "$HOME/linger" ]; then echo yes; else echo no; fi
    else
      [ "$1" = --no-ask-password ] && [ "$2" = enable-linger ]
      echo enabled >> "$HOME/linger"
    fi
  `,
  );
  tool(
    'systemctl',
    `
    test "$1" = --user
    if [ "$2" = show ]; then
      printf '%s/.config/systemd/user/%s\\n' "$HOME" "$3"
    fi
  `,
  );
  tool(
    'curl',
    `
    while [ "$1" != --output ]; do shift; done
    printf 'fake archive' > "$2"
  `,
  );
  tool(
    'sha256sum',
    `
    case "$1" in
      *.tar.gz)
        printf '%s %s\\n' "$FAKE_ARCHIVE_SHA" "$1" ;;
      *) exec /usr/bin/sha256sum "$@" ;;
    esac
  `,
  );
  tool(
    'tar',
    `
    mkdir -p "$4/bin"
    cat > "$4/config.sh" <<'SCRIPT'
#!/bin/sh
[ "$3" = https://github.com/mofua-suki/big-decimal ] || exit 1
printf '%s\\n' configure >> "$HOME/registrations"
printf '%s\\n' '{"gitHubUrl":"https://github.com/mofua-suki/explicit-decimal","agentName":"gt12-pro"}' > .runner
SCRIPT
    printf '#!/bin/sh\\nexit 0\\n' > "$4/bin/runsvc.sh"
    chmod +x "$4/config.sh" "$4/bin/runsvc.sh"
  `,
  );

  const correctDigest =
    'baebf204307bac6193480852732f0aba2ae97b6c5597c244f4d5558390147c40';
  function run(machine: string, repository: string, digest = correctDigest) {
    return spawnSync('sh', ['-c', policy.operations[operation]], {
      input: [installer.toString('base64'), repository, 'test-value', ''].join(
        '\n',
      ),
      encoding: 'utf8',
      env: {
        ...process.env,
        HOME: home,
        PATH: bin + ':' + process.env.PATH,
        FAKE_MACHINE: machine,
        FAKE_ARCHIVE_SHA: digest,
      },
    });
  }

  assert.match(run('a9-max', 'explicit-decimal').stderr, /導入対象外/);
  assert.match(run('gt12-pro', 'outside-scope').stderr, /導入対象外/);
  assert.match(
    run('gt12-pro', 'explicit-decimal', 'mismatched-digest').stderr,
    /SHA-256が一致しません/,
  );

  for (let attempt = 0; attempt < 2; attempt++) {
    const result = run('gt12-pro', 'explicit-decimal');
    assert.equal(result.status, 0, result.stderr);
    assert.match(result.stdout, /gt12-pro \/ explicit-decimal/);
  }
  assert.equal(
    readFileSync(join(home, 'registrations'), 'utf8'),
    'configure\n',
  );
  assert.equal(readFileSync(join(home, 'linger'), 'utf8'), 'enabled\n');
});

test('macOSではplistのパスからサービス名を求め、既存登録のサービスを起動する', (t) => {
  const home = mkdtempSync(join(tmpdir(), 'gha-runner-mac-test.'));
  t.after(() => rmSync(home, { recursive: true, force: true }));
  const bin = join(home, 'bin');
  mkdirSync(bin);
  const label = 'actions.runner.mofua-suki-issue-desk-intake.macbook-pro';
  const domain = 'gui/501';
  const target = domain + '/' + label;
  const plist = join(home, 'Library', 'LaunchAgents', label + '.plist');
  const root = join(
    home,
    '.local/share/github-actions-runners/issue-desk-intake',
  );
  mkdirSync(root, { recursive: true });
  writeFileSync(
    join(root, '.runner'),
    JSON.stringify({
      gitHubUrl: 'https://github.com/mofua-suki/issue-desk-intake',
      agentName: 'macbook-pro',
    }),
  );
  writeFileSync(join(root, '.service'), plist + '\n');

  const commands = {
    uname: 'case "$1" in -s) echo Darwin;; -m) echo arm64;; esac',
    hostname: 'echo usernoMacBook-Pro',
    id: 'echo 501',
    launchctl: `
      case "$1" in
        print)
          if [ "$2" = "$FAKE_DOMAIN" ]; then exit 0; fi
          [ "$2" = "$FAKE_TARGET" ] && [ -f "$HOME/loaded" ] || exit 1
          if [ "$FAKE_STOPPED" = yes ] && [ ! -f "$HOME/restarted" ]; then exit 0; fi
          printf '\\tpid = 123\\n' ;;
        bootstrap)
          [ "$2" = "$FAKE_DOMAIN" ] && [ "$3" = "$FAKE_PLIST" ]
          touch "$HOME/loaded"
          echo bootstrap >> "$HOME/launch-operations" ;;
        kickstart)
          [ "$2" = "$FAKE_TARGET" ]
          touch "$HOME/restarted"
          echo kickstart >> "$HOME/launch-operations" ;;
        *) exit 1 ;;
      esac
    `,
  };
  for (const [name, body] of Object.entries(commands)) {
    writeFileSync(join(bin, name), '#!/bin/sh\nset -eu\n' + body, {
      mode: 0o755,
    });
  }
  const script = new URL(
    '../deploy/nas-host/install-github-actions-runner.sh',
    import.meta.url,
  );
  const run = (repository: string, stopped: string) =>
    spawnSync('sh', [script.pathname], {
      input: repository + '\ntest-value\n',
      encoding: 'utf8',
      env: {
        ...process.env,
        HOME: home,
        PATH: bin + ':' + process.env.PATH,
        FAKE_DOMAIN: domain,
        FAKE_TARGET: target,
        FAKE_PLIST: plist,
        FAKE_STOPPED: stopped,
      },
    });
  assert.match(run('flutter-architecture', 'no').stderr, /Linuxランナーの対象/);
  for (const stopped of ['no', 'no', 'yes']) {
    const result = run('issue-desk-intake', stopped);
    assert.equal(result.status, 0, result.stderr);
  }
  assert.equal(
    readFileSync(join(home, 'launch-operations'), 'utf8'),
    'bootstrap\nkickstart\n',
  );
});
