import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { execFileSync, spawnSync } from 'node:child_process';

const repo = path.resolve(import.meta.dirname, '..');
const paths = [
  'skills/issue-desk-operations/SKILL.md',
  'skills/issue-desk-operations/refs/tier-and-splitting.md',
  'skills/issue-intake/SKILL.md',
  'skills/issue-desk-finalization/SKILL.md',
  'api/schemas/tier.schema.json',
];
test('難易度の生成を照合し、各配布物のずれを検出して書き換えずに止める', () => {
  const root = fs.mkdtempSync(
    path.join(os.tmpdir(), 'tier-policy-generation-'),
  );
  try {
    const bin = path.join(root, 'generate-tier-policy');
    execFileSync(
      process.env.GO || 'go',
      ['build', '-o', bin, './cmd/generate-tier-policy'],
      { cwd: path.join(repo, 'server'), stdio: 'pipe' },
    );
    const env = { ...process.env };
    delete env.GODEBUG;
    const run = (where, check = true) =>
      spawnSync(bin, ['--root', where, ...(check ? ['--check'] : [])], {
        encoding: 'utf8',
        env,
      });
    const initial = run(repo);
    assert.equal(initial.status, 0, initial.stderr);
    const fixture = path.join(root, 'fixture');
    for (const file of paths) {
      fs.mkdirSync(path.dirname(path.join(fixture, file)), { recursive: true });
      fs.copyFileSync(path.join(repo, file), path.join(fixture, file));
    }
    for (const file of paths) {
      const target = path.join(fixture, file),
        original = fs.readFileSync(target, 'utf8');
      let changed;
      if (file.endsWith('.json')) {
        const value = JSON.parse(original);
        value.properties.level.description += '変更';
        changed = JSON.stringify(value);
      } else
        changed = original.replace(
          '<!-- issue-desk:tier-policy:begin -->',
          '<!-- issue-desk:tier-policy:begin -->\n独立した旧基準',
        );
      fs.writeFileSync(target, changed);
      const failed = run(fixture);
      assert.notEqual(failed.status, 0, file);
      assert.ok(failed.stderr.includes(file));
      assert.equal(
        fs.readFileSync(target, 'utf8'),
        changed,
        '検査は書き換えない',
      );
      assert.equal(run(fixture, false).status, 0);
      if (file.endsWith('.json'))
        assert.deepEqual(
          JSON.parse(fs.readFileSync(target, 'utf8')),
          JSON.parse(original),
        );
      else assert.equal(fs.readFileSync(target, 'utf8'), original);
      assert.equal(run(fixture).status, 0);
    }
    for (const [file, stale] of [
      [paths[0], '通常は `light`'],
      [paths[1], '通常は Sonnet'],
      [paths[2], 'Sonnetで始め相談ごとに見直す。'],
    ]) {
      const target = path.join(fixture, file),
        original = fs.readFileSync(target, 'utf8');
      fs.appendFileSync(target, `\n${stale}\n`);
      const changed = fs.readFileSync(target, 'utf8'),
        failed = run(fixture);
      assert.notEqual(
        failed.status,
        0,
        '生成部分の外に旧基準を戻す変更も拒否する',
      );
      assert.ok(failed.stderr.includes('独立した旧基準'));
      assert.equal(fs.readFileSync(target, 'utf8'), changed);
      fs.writeFileSync(target, original);
    }
    const skill = path.join(fixture, paths[0]);
    fs.appendFileSync(skill, '\n生成外の運用条件は保持する。\n');
    const schema = path.join(fixture, paths[4]),
      value = JSON.parse(fs.readFileSync(schema, 'utf8'));
    value.properties.reason.maxLength = 300;
    fs.writeFileSync(schema, JSON.stringify(value));
    assert.equal(run(fixture, false).status, 0);
    assert.ok(
      fs.readFileSync(skill, 'utf8').endsWith('生成外の運用条件は保持する。\n'),
    );
    assert.equal(
      JSON.parse(fs.readFileSync(schema, 'utf8')).properties.reason.maxLength,
      300,
    );
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});
