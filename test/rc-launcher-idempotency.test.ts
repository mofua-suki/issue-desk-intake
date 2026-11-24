import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { execFileSync } from 'node:child_process';

// 正規のPC配布全体を走らせる。HTTP報告/systemdだけをfakeにし、Skillとlauncherは実物を配る。
test('PC配布の2回目はNode入口を含め既存fileへ書き込まない', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'desk-rc-launcher-'));
  const home = path.join(root, 'home');
  const bin = path.join(root, 'bin');
  fs.mkdirSync(home);
  fs.mkdirSync(bin);
  for (const command of ['systemctl', 'timeout']) {
    const file = path.join(bin, command);
    fs.writeFileSync(file, '#!/bin/sh\nexit 0\n', { mode: 0o755 });
  }
  const environment = {
    ...process.env,
    HOME: home,
    PATH: `${bin}:${path.dirname(process.execPath)}:/usr/local/bin:/usr/bin:/bin`,
    ISSUE_DESK_RC_EXISTING_ONLY: '1',
  };
  const script = path.resolve(
    import.meta.dirname,
    '../deploy/clients/remote-control/rc-update.sh',
  );
  const run = () =>
    execFileSync('/bin/sh', [script], {
      env: environment,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'pipe'],
    });
  const snapshot = () => {
    const result = {};
    const walk = (directory) => {
      for (const item of fs.readdirSync(directory, { withFileTypes: true })) {
        const file = path.join(directory, item.name);
        if (item.isDirectory()) walk(file);
        else if (item.isFile()) {
          const stat = fs.statSync(file, { bigint: true });
          result[path.relative(home, file)] = [
            fs.readFileSync(file).toString('base64'),
            String(stat.ino),
            String(stat.mtimeNs),
          ];
        }
      }
    };
    walk(home);
    return result;
  };
  try {
    run();
    const before = snapshot();
    const output = run();
    assert.deepEqual(
      snapshot(),
      before,
      '内容一致でもinodeやmtimeが変わる二重置換を許さない',
    );
    assert.doesNotMatch(
      output,
      /(?:更新しました|作成しました|を置きました|を配りました|failed)/,
    );
    assert.match(
      fs.readFileSync(path.join(home, '.issue-desk/node'), 'utf8'),
      /ISSUE_DESK_NODE_DIRS/,
    );
    for (const provider of ['.claude', '.codex'])
      assert.ok(
        fs.existsSync(
          path.join(home, provider, 'skills/task-completion/SKILL.md'),
        ),
      );
    assert.equal(
      fs.existsSync(path.join(home, '.claude.json')),
      false,
      '既存限定modeでonboarding/trustを作らない',
    );
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});
