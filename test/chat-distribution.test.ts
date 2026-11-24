import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { execFileSync } from 'node:child_process';

test('チャット用配布はServerとClientを展開せず、HTTPとSkillを更新する', () => {
  const root = fs.mkdtempSync(
    path.join(os.tmpdir(), 'desk-chat-distribution-'),
  );
  try {
    const origin = path.join(root, 'origin');
    const checkout = path.join(root, 'checkout');
    fs.mkdirSync(origin);
    const git = (args, cwd = origin) =>
      execFileSync('git', args, { cwd, stdio: 'pipe' });
    git(['init', '--initial-branch=trunk']);
    const files = [
      'server/main.go',
      'client/src/main.ts',
      'README.md',
      'scripts/verify.sh',
      'scripts/desk-client.ts',
      'shared/src/desk.mjs',
      'skills/task-completion/SKILL.md',
      'skills/usage-per-issue/SKILL.md',
      'chat-deploy.json',
      'chat-instructions.md',
      'deploy/nas/intake.sh',
      'deploy/nas/update-skills.sh',
      'deploy/clients/remote-control/rc-update.sh',
    ];
    for (const file of files) {
      const target = path.join(origin, file);
      fs.mkdirSync(path.dirname(target), { recursive: true });
      fs.writeFileSync(target, 'fixture');
    }
    for (const file of [
      'skills/usage-per-issue/SKILL.md',
      'chat-deploy.json',
    ]) {
      fs.copyFileSync(
        path.resolve(import.meta.dirname, '..', file),
        path.join(origin, file),
      );
    }
    git(['add', '.']);
    git([
      '-c',
      'user.name=Fixture',
      '-c',
      'user.email=fixture@example.invalid',
      'commit',
      '-m',
      '配布資産',
    ]);
    git(['clone', '--no-checkout', origin, checkout]);
    const source = path.resolve(
      import.meta.dirname,
      '../deploy/clients/remote-control/chat-source.sh',
    );
    execFileSync('sh', [
      '-c',
      '. "$1"; chat_sources "$2"',
      'sh',
      source,
      checkout,
    ]);
    for (const file of [
      'server/main.go',
      'client/src/main.ts',
      'README.md',
      'scripts/verify.sh',
      'deploy/nas/intake.sh',
    ])
      assert.equal(fs.existsSync(path.join(checkout, file)), false, file);
    for (const file of [
      'scripts/desk-client.ts',
      'shared/src/desk.mjs',
      'skills/task-completion/SKILL.md',
      'skills/usage-per-issue/SKILL.md',
      'chat-deploy.json',
      'deploy/clients/remote-control/rc-update.sh',
    ])
      assert.equal(fs.existsSync(path.join(checkout, file)), true, file);
    assert.ok(
      JSON.parse(
        fs.readFileSync(path.join(checkout, 'chat-deploy.json'), 'utf8'),
      ).targets.some(
        (target) =>
          target.kind === 'skill' && target.name === 'usage-per-issue',
      ),
    );
    assert.equal(
      fs.readFileSync(
        path.join(checkout, 'skills/usage-per-issue/SKILL.md'),
        'utf8',
      ),
      fs.readFileSync(
        path.resolve(import.meta.dirname, '../skills/usage-per-issue/SKILL.md'),
        'utf8',
      ),
    );
    fs.writeFileSync(path.join(origin, 'scripts/desk-client.ts'), 'updated');
    git(['add', '.']);
    git([
      '-c',
      'user.name=Fixture',
      '-c',
      'user.email=fixture@example.invalid',
      'commit',
      '-m',
      '窓口の更新',
    ]);
    git(['fetch', 'origin', 'trunk'], checkout);
    git(['merge', '--ff-only', 'origin/trunk'], checkout);
    assert.equal(
      fs.readFileSync(path.join(checkout, 'scripts/desk-client.ts'), 'utf8'),
      'updated',
    );
    assert.equal(fs.existsSync(path.join(checkout, 'server/main.go')), false);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});
