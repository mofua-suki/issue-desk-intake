import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { execFileSync } from 'node:child_process';

test('NASとUbuntuの更新は正本を両常設指示へ配り管理範囲外を保持する', () => {
  const root = fs.mkdtempSync(
    path.join(os.tmpdir(), 'desk-instructions-distribution-'),
  );
  const repo = path.resolve(import.meta.dirname, '..');
  const begin = '<!-- issue-desk:chat-instructions:begin -->';
  const end = '<!-- issue-desk:chat-instructions:end -->';
  const note =
    'この範囲は issue-desk の監視が chat-instructions.md から書き換える（編集は正本で）';
  try {
    const bin = path.join(root, 'bin');
    fs.mkdirSync(bin);
    for (const name of ['systemctl', 'timeout'])
      fs.writeFileSync(path.join(bin, name), '#!/bin/sh\nexit 0\n', {
        mode: 0o755,
      });
    const source = fs
      .readFileSync(path.join(repo, 'chat-instructions.md'), 'utf8')
      .replace(/\n+$/, '');
    for (const kind of ['nas', 'pc']) {
      const home = path.join(root, kind);
      const files = ['.claude/CLAUDE.md', '.codex/AGENTS.md'];
      for (const file of files) {
        const destination = path.join(home, file);
        fs.mkdirSync(path.dirname(destination), { recursive: true });
        fs.writeFileSync(
          destination,
          `前の利用者設定\n${begin}\n古い管理内容\n${end}\n後の利用者設定\n`,
        );
      }
      const env = {
        ...process.env,
        HOME: home,
        PATH: `${bin}:${path.dirname(process.execPath)}:/usr/bin:/bin`,
        ISSUE_DESK_RC_EXISTING_ONLY: '1',
        REPO: repo,
      };
      const args =
        kind === 'nas'
          ? [
              '-c',
              'DIR=$REPO; DRY_RUN=0; NL="\n"; say() { :; }; plan() { :; }; . "$DIR/deploy/nas/update-skills.sh"; skills_listed',
            ]
          : [path.join(repo, 'deploy/clients/remote-control/rc-update.sh')];
      execFileSync('/bin/sh', args, { env, encoding: 'utf8' });
      for (const file of files) {
        assert.equal(
          fs.readFileSync(path.join(home, file), 'utf8'),
          `前の利用者設定\n${begin}\n${note}\n\n${source}\n${end}\n後の利用者設定\n`,
          `${kind}: ${file}`,
        );
      }
    }
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

// 実際の配布入口を使い、parser のコピー先だけ書込み失敗させる。
test('parser 配布失敗時は NAS と PC の既存フック部品を維持する', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'desk-hook-failure-'));
  const repo = path.resolve(import.meta.dirname, '..');
  try {
    const bin = path.join(root, 'bin');
    fs.mkdirSync(bin);
    fs.writeFileSync(
      path.join(bin, 'cp'),
      '#!/bin/sh\ncase "$2" in */.claude/hooks/chat-shell-parser.ts.*.tmp) exit 1;; esac\nexec /bin/cp "$@"\n',
      { mode: 0o755 },
    );
    for (const name of ['systemctl', 'timeout'])
      fs.writeFileSync(path.join(bin, name), '#!/bin/sh\nexit 0\n', {
        mode: 0o755,
      });
    for (const kind of ['nas', 'pc']) {
      const home = path.join(root, kind);
      const hooks = path.join(home, '.claude/hooks');
      fs.mkdirSync(hooks, { recursive: true });
      for (const file of ['chat-turn-checks.ts', 'chat-stop-hook.ts'])
        fs.writeFileSync(path.join(hooks, file), `old ${file}\n`);
      const env = {
        ...process.env,
        HOME: home,
        PATH: `${bin}:${path.dirname(process.execPath)}:/usr/bin:/bin`,
        ISSUE_DESK_RC_EXISTING_ONLY: '1',
        REPO: repo,
      };
      const args =
        kind === 'nas'
          ? [
              '-c',
              'DIR=$REPO; DRY_RUN=0; NL="\n"; say() { printf "%s\\n" "$*"; }; plan() { :; }; . "$DIR/deploy/nas/update-skills.sh"; skills_listed',
            ]
          : [path.join(repo, 'deploy/clients/remote-control/rc-update.sh')];
      const output = execFileSync('/bin/sh', args, { env, encoding: 'utf8' });
      assert.match(output, /chat-shell-parser\.ts を書けません/, kind);
      for (const file of ['chat-turn-checks.ts', 'chat-stop-hook.ts'])
        assert.equal(
          fs.readFileSync(path.join(hooks, file), 'utf8'),
          `old ${file}\n`,
          `${kind}: ${file}`,
        );
      assert.equal(
        fs.existsSync(path.join(hooks, 'chat-shell-parser.ts')),
        false,
      );
    }
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test('控え部品の配布失敗は各更新経路の既存タグとパスで知らせる', () => {
  const root = fs.mkdtempSync(
    path.join(os.tmpdir(), 'desk-hook-record-failure-'),
  );
  const repo = path.resolve(import.meta.dirname, '..');
  try {
    const bin = path.join(root, 'bin');
    fs.mkdirSync(bin);
    fs.writeFileSync(
      path.join(bin, 'cp'),
      '#!/bin/sh\ncase "$2" in */.claude/hooks/stop-check-record.ts.*.tmp) exit 1;; esac\nexec /bin/cp "$@"\n',
      { mode: 0o755 },
    );
    for (const name of ['systemctl', 'timeout'])
      fs.writeFileSync(path.join(bin, name), '#!/bin/sh\nexit 0\n', {
        mode: 0o755,
      });
    for (const kind of ['nas', 'pc']) {
      const home = path.join(root, kind);
      fs.mkdirSync(home);
      const env = {
        ...process.env,
        HOME: home,
        PATH: `${bin}:${path.dirname(process.execPath)}:/usr/bin:/bin`,
        ISSUE_DESK_RC_EXISTING_ONLY: '1',
        REPO: repo,
      };
      const args =
        kind === 'nas'
          ? [
              '-c',
              'DIR=$REPO; DRY_RUN=0; NL="\n"; say() { printf "%s\\n" "$*"; }; plan() { :; }; . "$DIR/deploy/nas/update-skills.sh"; skills_listed',
            ]
          : [path.join(repo, 'deploy/clients/remote-control/rc-update.sh')];
      const output = execFileSync('/bin/sh', args, { env, encoding: 'utf8' });
      assert.ok(
        output.includes(
          `${kind === 'nas' ? '[update:skills-failed]' : '[rc-update-failed]'} ${home}/.claude/hooks/stop-check-record.ts を書けません`,
        ),
        output,
      );
    }
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test('両更新ユニットの経路は遅い送信の完了を待って控えを消す', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'desk-periodic-send-'));
  const repo = path.resolve(import.meta.dirname, '..');
  try {
    for (const kind of ['nas', 'pc']) {
      const unit = fs.readFileSync(
        path.join(
          repo,
          kind === 'nas'
            ? 'deploy/nas/issue-desk-intake-update.service'
            : 'deploy/clients/remote-control/issue-desk-rc-update.service',
        ),
        'utf8',
      );
      assert.match(unit, /Type=oneshot/);
      assert.match(
        unit,
        kind === 'nas'
          ? /^TimeoutStartSec=infinity$/m
          : /^TimeoutStartSec=600$/m,
      );
      const home = path.join(root, kind);
      const deskDir = path.join(home, '.issue-desk');
      fs.mkdirSync(deskDir, { recursive: true });
      fs.writeFileSync(
        path.join(deskDir, 'stop-check-fires.jsonl'),
        '{"id":"existing"}\n',
      );
      fs.writeFileSync(
        path.join(deskDir, 'desk'),
        '#!/bin/sh\nsleep 0.2\n[ "$1" = stop-check-send.ts ] || exit 1\n: > "$HOME/.issue-desk/stop-check-fires.jsonl"\n',
        { mode: 0o755 },
      );
      const bin = path.join(root, 'bin');
      fs.mkdirSync(bin, { recursive: true });
      fs.writeFileSync(path.join(bin, 'systemctl'), '#!/bin/sh\nexit 0\n', {
        mode: 0o755,
      });
      const env = {
        ...process.env,
        HOME: home,
        REPO: repo,
        PATH: `${bin}:${process.env.PATH}`,
        ISSUE_DESK_RC_EXISTING_ONLY: '1',
      };
      const args =
        kind === 'nas'
          ? [
              '-c',
              'DIR=$REPO; DRY_RUN=0; NL="\n"; say() { :; }; plan() { :; }; . "$DIR/deploy/nas/update-skills.sh"; skills_listed',
            ]
          : [path.join(repo, 'deploy/clients/remote-control/rc-update.sh')];
      execFileSync('/bin/sh', args, { env });
      assert.equal(
        fs.readFileSync(path.join(deskDir, 'stop-check-fires.jsonl'), 'utf8'),
        '',
      );
    }
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test('実際の配布登録から優先度SkillをNASとPCへ配置する', () => {
  const root = fs.mkdtempSync(
    path.join(os.tmpdir(), 'desk-priority-distribution-'),
  );
  const repo = path.resolve(import.meta.dirname, '..');
  try {
    const bin = path.join(root, 'bin');
    fs.mkdirSync(bin);
    for (const name of ['systemctl', 'timeout'])
      fs.writeFileSync(path.join(bin, name), '#!/bin/sh\nexit 0\n', {
        mode: 0o755,
      });
    const source = fs.readFileSync(
      path.join(repo, 'skills/priority-issue/SKILL.md'),
      'utf8',
    );
    for (const kind of ['nas', 'pc']) {
      const home = path.join(root, kind);
      fs.mkdirSync(home);
      const env = {
        ...process.env,
        HOME: home,
        PATH: `${bin}:${path.dirname(process.execPath)}:/usr/bin:/bin`,
        ISSUE_DESK_RC_EXISTING_ONLY: '1',
        REPO: repo,
      };
      const args =
        kind === 'nas'
          ? [
              '-c',
              'DIR=$REPO; DRY_RUN=0; NL="\n"; say() { :; }; plan() { :; }; . "$DIR/deploy/nas/update-skills.sh"; skills_listed',
            ]
          : [path.join(repo, 'deploy/clients/remote-control/rc-update.sh')];
      execFileSync('/bin/sh', args, { env });
      for (const target of ['.claude', '.codex'])
        assert.equal(
          fs.readFileSync(
            path.join(home, target, 'skills/priority-issue/SKILL.md'),
            'utf8',
          ),
          source,
          `${kind}: ${target}`,
        );
    }
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test('実際の配布登録から相談確定SkillをNASとPCへ配置する', () => {
  const root = fs.mkdtempSync(
    path.join(os.tmpdir(), 'desk-finalization-distribution-'),
  );
  const repo = path.resolve(import.meta.dirname, '..');
  try {
    const bin = path.join(root, 'bin');
    fs.mkdirSync(bin);
    for (const name of ['systemctl', 'timeout'])
      fs.writeFileSync(path.join(bin, name), '#!/bin/sh\nexit 0\n', {
        mode: 0o755,
      });
    const source = fs.readFileSync(
      path.join(repo, 'skills/issue-desk-finalization/SKILL.md'),
      'utf8',
    );
    for (const kind of ['nas', 'pc']) {
      const home = path.join(root, kind);
      fs.mkdirSync(home);
      const env = {
        ...process.env,
        HOME: home,
        PATH: `${bin}:${path.dirname(process.execPath)}:/usr/bin:/bin`,
        ISSUE_DESK_RC_EXISTING_ONLY: '1',
        REPO: repo,
      };
      const args =
        kind === 'nas'
          ? [
              '-c',
              'DIR=$REPO; DRY_RUN=0; NL="\n"; say() { :; }; plan() { :; }; . "$DIR/deploy/nas/update-skills.sh"; skills_listed',
            ]
          : [path.join(repo, 'deploy/clients/remote-control/rc-update.sh')];
      execFileSync('/bin/sh', args, { env });
      for (const target of ['.claude', '.codex'])
        assert.equal(
          fs.readFileSync(
            path.join(home, target, 'skills/issue-desk-finalization/SKILL.md'),
            'utf8',
          ),
          source,
          `${kind}: ${target}`,
        );
    }
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});
