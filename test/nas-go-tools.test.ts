import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';

const source = path.resolve('deploy/nas/update-go-tools.sh');

const install = (home) =>
  spawnSync(
    'sh',
    [
      '-c',
      '. "$1"; say() { echo "$@"; }; plan() { echo "$@"; }; go_tools',
      'sh',
      source,
    ],
    {
      env: { PATH: process.env.PATH, HOME: home },
      encoding: 'utf8',
    },
  );

const withHome = (run) => {
  const home = fs.mkdtempSync(path.join(os.tmpdir(), 'desk-go-tools-'));
  try {
    return run(home);
  } finally {
    // 後始末の失敗（固定テストのサンドボックスで ENOTEMPTY が出ることがある）で、本来の失敗を隠さない。
    try {
      fs.rmSync(home, {
        recursive: true,
        force: true,
        maxRetries: 5,
        retryDelay: 100,
      });
    } catch {
      // 一時ディレクトリは作業領域ごと捨てられるので、残っても害がない。
    }
  }
};

const fakeTool = (home, name) => {
  const bin = path.join(home, '.local', 'bin');
  fs.mkdirSync(bin, { recursive: true });
  const file = path.join(bin, name);
  fs.writeFileSync(
    file,
    `#!/bin/sh\necho "${name}: GODEBUG=\${GODEBUG-unset} args=$*"\n`,
  );
  fs.chmodSync(file, 0o755);
};

test('Goツールの更新は ~/.issue-desk/go と gofmt を置き、2回目は更新しない', () => {
  withHome((home) => {
    const first = install(home);
    assert.equal(first.status, 0, first.stderr);
    for (const tool of ['go', 'gofmt']) {
      const file = path.join(home, '.issue-desk', tool);
      assert.ok(
        fs.statSync(file).mode & 0o100,
        `${tool} が実行できる形で置かれていません`,
      );
    }
    assert.match(first.stdout, /go ラッパー/);
    const second = install(home);
    assert.equal(second.status, 0, second.stderr);
    assert.doesNotMatch(second.stdout, /更新しました/);
  });
});

test('ラッパーは PATH に go が無くても ~/.local/bin/go を呼び、起動時に落ちる GODEBUG を外す', () => {
  withHome((home) => {
    assert.equal(install(home).status, 0);
    fakeTool(home, 'go');
    fakeTool(home, 'gofmt');
    const env = { PATH: '/nonexistent', HOME: home, GODEBUG: 'tlskyber=0' };
    const go = spawnSync(
      '/bin/sh',
      [path.join(home, '.issue-desk', 'go'), 'test', './...'],
      { env, encoding: 'utf8' },
    );
    assert.equal(go.status, 0, go.stderr);
    assert.equal(go.stdout.trim(), 'go: GODEBUG=unset args=test ./...');
    const gofmt = spawnSync(
      '/bin/sh',
      [path.join(home, '.issue-desk', 'gofmt'), '-l', 'internal'],
      { env, encoding: 'utf8' },
    );
    assert.equal(gofmt.status, 0, gofmt.stderr);
    assert.equal(gofmt.stdout.trim(), 'gofmt: GODEBUG=unset args=-l internal');
  });
});

test('Goが見つからないときは、探した場所を表示して127で止まる（無言で消えない）', () => {
  withHome((home) => {
    assert.equal(install(home).status, 0);
    // 固定テストの環境のように /usr/local/go/bin/go があるマシンでも成り立つよう、探す場所を空のホームに限る。
    const searched = path.join(home, '.local', 'bin');
    const result = spawnSync(
      '/bin/sh',
      [path.join(home, '.issue-desk', 'go'), 'version'],
      {
        env: { PATH: '/nonexistent', HOME: home, ISSUE_DESK_GO_DIRS: searched },
        encoding: 'utf8',
      },
    );
    assert.equal(result.status, 127);
    assert.match(result.stderr, /go が見つかりません/);
    assert.ok(result.stderr.includes(searched), result.stderr);
  });
});
