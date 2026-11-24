import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';

test(
  'NAS prepares the same four consultation projects for Codex and routes image consultations in both clients',
  { skip: process.platform === 'win32' },
  (t) => {
    const home = fs.mkdtempSync(path.join(os.tmpdir(), 'nas-consultations-'));
    t.after(() => fs.rmSync(home, { recursive: true, force: true }));
    const bin = path.join(home, '.local/bin');
    fs.mkdirSync(bin, { recursive: true });
    const log = path.join(home, 'codex.log');
    fs.writeFileSync(
      path.join(bin, 'codex'),
      '#!/bin/sh\nprintf "%s\\n" "$*" >> "$HOME/codex.log"\n',
      { mode: 0o700 },
    );
    fs.writeFileSync(
      path.join(bin, 'systemd-run'),
      '#!/bin/sh\n[ "$1 $2 $3" = "--user --scope --quiet" ] || exit 2\nshift 3\nexec "$@"\n',
      { mode: 0o700 },
    );
    const deploy = () =>
      spawnSync(
        '/bin/sh',
        [
          '-ec',
          `. ./deploy/nas/update-remote-control.sh
          . ./deploy/nas/update-remote-control-single.sh
          . ./deploy/nas/update-consultations.sh
          say() { printf "%s\\n" "$*"; }
          claude_consultations
          codex_consultations`,
        ],
        {
          env: {
            ...process.env,
            HOME: home,
            PATH: bin + path.delimiter + process.env.PATH,
          },
          encoding: 'utf8',
        },
      );
    const result = deploy();
    assert.equal(result.status, 0, result.stderr);
    const root = path.join(home, 'Consultations');
    assert.deepEqual(fs.readdirSync(root).sort(), [
      'demo-image-generator',
      'image-review-client',
      'issue-desk-intake',
      'issue-desk-worker',
    ]);
    for (const name of fs.readdirSync(root)) {
      assert.deepEqual(fs.readdirSync(path.join(root, name)), ['AGENTS.md']);
    }
    const codex = path.join(root, 'image-review-client/AGENTS.md');
    const claude = path.join(home, 'rc/image-review-client/CLAUDE.md');
    const instructions = fs.readFileSync(codex, 'utf8');
    assert.equal(fs.readFileSync(claude, 'utf8'), instructions);
    assert.match(instructions, /mofua-suki\/demo-image-generator/);
    assert.match(instructions, /mofua-suki\/demo-image-rating/);
    assert.equal(fs.readFileSync(log, 'utf8'), 'remote-control start --json\n');

    fs.writeFileSync(codex, 'custom instructions\n');
    const worker = path.join(root, 'issue-desk-worker/AGENTS.md');
    fs.unlinkSync(worker);
    assert.equal(deploy().status, 1);
    assert.match(
      fs.readFileSync(worker, 'utf8'),
      /mofua-suki\/issue-desk-worker/,
    );
    assert.equal(fs.readFileSync(codex, 'utf8'), 'custom instructions\n');
    assert.equal(
      fs.readFileSync(log, 'utf8'),
      'remote-control start --json\n'.repeat(2),
    );
  },
);
