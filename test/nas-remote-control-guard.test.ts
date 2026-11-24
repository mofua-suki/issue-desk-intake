import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';

// 条件を変える全工程を1回の sh にまとめ、実際の配置・復元・呼び出し数を記録する。
test('remote-control guard places, refuses, rolls back, suppresses and retries in one sh', () => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'rc-guard-'));
  try {
    const home = path.join(tmp, 'home');
    const repo = path.join(tmp, 'repo');
    const bin = path.join(tmp, 'bin');
    const put = (file: string, text: string) => {
      fs.mkdirSync(path.dirname(file), { recursive: true });
      fs.writeFileSync(file, text, { mode: 0o755 });
    };
    put(
      path.join(home, '.local/bin/claude'),
      '#!/bin/sh\n[ ! -f "$HOME/version-error" ] || exit 1\ncat "$HOME/version"\n',
    );
    put(path.join(home, 'version'), 'v1\nignored\n');
    put(path.join(home, '.config/systemd/user/claude-rc@.service'), 'old-unit');
    put(
      path.join(home, '.issue-desk/remote-control/settings.json'),
      'old-settings',
    );
    put(path.join(repo, 'deploy/nas/claude-rc@.service'), 'new-unit');
    put(
      path.join(repo, 'deploy/nas/remote-control-settings.json'),
      'new-settings',
    );
    for (const name of [
      'fresh',
      'old',
      'same',
      'custom',
      'tracked',
      'linked',
      'plain',
      '.hidden',
    ]) {
      fs.mkdirSync(path.join(home, 'rc', name, '.claude'), { recursive: true });
    }
    put(path.join(home, 'rc/old/.claude/settings.json'), 'old-settings');
    put(path.join(home, 'rc/same/.claude/settings.json'), 'new-settings');
    put(path.join(home, 'rc/custom/.claude/settings.json'), 'my-settings');
    put(path.join(home, 'rc/tracked/.claude/settings.json'), 'old-settings');
    put(path.join(tmp, 'outside'), 'outside');
    fs.symlinkSync(
      path.join(tmp, 'outside'),
      path.join(home, 'rc/linked/.claude/settings.json'),
    );
    put(
      path.join(bin, 'git'),
      `#!/bin/sh
folder=$2
shift 2
case "$1" in
rev-parse)
  case "$folder" in */plain) exit 128 ;; esac
  case "$2" in
  --show-toplevel) printf '%s\\n' "$folder" ;;
  --show-prefix) printf '\\n.git/info/exclude\\n' ;;
  --git-path) printf '.git/info/exclude\\n' ;;
  esac ;;
ls-files)
  [ ! -f "$HOME/git-error" ] || exit 2
  case "$folder" in */tracked) exit 0 ;; *) exit 1 ;; esac ;;
*) exit 2 ;;
esac
`,
    );
    put(
      path.join(bin, 'cp'),
      `#!/bin/sh
if [ -f "$HOME/copy-error" ]; then
  case "$2" in */.claude/settings.json.*.tmp) exit 1 ;; esac
fi
exec /bin/cp "$@"
`,
    );
    put(
      path.join(bin, 'systemctl'),
      `#!/bin/sh
printf '%s\\n' "$*" >> "$HOME/calls"
case "$2" in
list-units) echo 'claude-rc@fresh.service loaded active running' ;;
is-active)
  [ "$(cat "$HOME/mode")" != both-fail ] || exit 1
  if [ "$(cat "$HOME/mode")" = fail ] && [ "$(cat "$HOME/.config/systemd/user/claude-rc@.service")" != old-unit ]; then exit 1; fi ;;
esac
`,
    );
    put(
      path.join(home, '.issue-desk/desk'),
      `#!/bin/sh
printf '%s\\n' "$*" >> "$HOME/notices"
echo 'notice output must stay hidden'
echo 'notice error must stay hidden' >&2
[ ! -f "$HOME/notice-error" ]
`,
    );
    const driver = `set -eu
DIR=$TEST_REPO
DRY_RUN=0
RC_WAIT_SECONDS=0
RC_SETTLE_SECONDS=0
say() { printf '%s\\n' "$*"; }
plan() { say "$*"; }
. "$GUARD"
. "$UPDATE"
unit=$HOME/.config/systemd/user/claude-rc@.service
state=$HOME/.issue-desk/remote-control
run() {
  printf '==%s\\n' "$1"
  : > "$HOME/calls"
  remote_control
  printf 'calls=%s\\n' "$(wc -l < "$HOME/calls")"
  printf 'restarts=%s\\n' "$(grep ' restart ' "$HOME/calls" | tr '\\n' ' ')"
  for name in fresh old same custom tracked linked plain .hidden; do
    dest=$HOME/rc/$name/.claude/settings.json
    if [ -f "$dest" ]; then printf '%s=%s\\n' "$name" "$(cat "$dest")"; else printf '%s=absent\\n' "$name"; fi
  done
  if [ -f "$state/failed-definition" ]; then echo failed-record; else echo no-record; fi
}
echo success > "$HOME/mode"
run install
run unchanged
rm "$HOME/rc/fresh/.claude/settings.json"
run project-only
# 窓口の無い新しい作業ディレクトリへ配るだけなら、どの窓口も再起動しない。
mkdir -p "$HOME/rc/newdir"
run new-dir
printf 'newdir=%s\\n' "$(cat "$HOME/rc/newdir/.claude/settings.json")"
run new-dir-again
# プロジェクト設定だけの回が途中で止まったら、復旧でも記録した名前の窓口だけを再起動する。
interrupt() {
  cp "$unit" "$unit.prev.77"
  cp "$state/settings.json" "$state/settings.json.prev.77"
  printf 'stage=placing\\nprev_unit=%s\\nprev_set=%s\\nhad_set=1\\nunits=\\nscope=project\\nprojects=%s\\n' \\
    "$unit.prev.77" "$state/settings.json.prev.77" "$1" > "$unit.update-pending"
}
interrupt newdir
run recover-new-dir
interrupt 'newdir fresh'
run recover-fresh
: > "$HOME/version-error"
run version-error
rm "$HOME/version-error"
: > "$HOME/git-error"
run git-error
rm "$HOME/git-error"
printf 'new-unit' > "$DIR/deploy/nas/claude-rc@.service"
printf 'new-settings2' > "$DIR/deploy/nas/remote-control-settings.json"
printf 'old-unit' > "$unit"
echo fail > "$HOME/mode"
: > "$HOME/copy-error"
run copy-error
rm "$HOME/copy-error"
: > "$HOME/notice-error"
run failure
rm "$HOME/notice-error"
expected="$(cksum < "$DIR/deploy/nas/claude-rc@.service")
$(cksum < "$DIR/deploy/nas/remote-control-settings.json")
v1"
[ "$(cat "$state/failed-definition")" = "$expected" ]
printf 'v1\\ndifferent-second-line\\n' > "$HOME/version"
run suppressed
run notice-sent
# A different failed definition that was already sent remains suppressed when revisited.
printf 'v2\\nignored\\n' > "$HOME/version"
run version-retry
run version-suppressed
printf 'v1' > "$HOME/version"
run previous-sent
printf 'v2' > "$HOME/version"
run previous-sent-again
printf 'new-unit2' > "$DIR/deploy/nas/claude-rc@.service"
run unit-retry
run unit-suppressed
printf 'new-settings3' > "$DIR/deploy/nas/remote-control-settings.json"
printf 'old-unit' > "$unit"
run settings-retry
run settings-suppressed
echo success > "$HOME/mode"
printf 'v3' > "$HOME/version"
run success
run final
printf 'new-unit4' > "$DIR/deploy/nas/claude-rc@.service"
echo both-fail > "$HOME/mode"
run rollback-failure
run rollback-suppressed
printf 'v4' > "$HOME/version"
echo success > "$HOME/mode"
run key-change-recovery
run key-change-success
printf 'exclude-lines=%s\\n' "$(grep -c '^/.claude/settings.json$' "$HOME/rc/fresh/.git/info/exclude")"
# 配置後の中断は、次回にプロジェクト退避から戻す。
mkdir -p "$state/project-pending/old"
printf 'old-settings' > "$state/project-pending/old/settings.json"
cp "$HOME/rc/old/.claude/settings.json" "$state/project-pending/old/applied.json"
: > "$state/project-pending/old/ready"
remote_control_guard restore
# 復旧先がリンクへ変わったときは退避を消さない。
mkdir -p "$state/project-pending/old"
printf 'old-settings' > "$state/project-pending/old/settings.json"
cp "$HOME/rc/old/.claude/settings.json" "$state/project-pending/old/applied.json"
: > "$state/project-pending/old/ready"
rm "$HOME/rc/old/.claude/settings.json"
ln -s "$TEST_OUTSIDE" "$HOME/rc/old/.claude/settings.json"
rc_prev_unit=$unit.prev.99
rc_prev_set=$state/settings.json.prev.99
if remote_control_guard restore; then exit 2; fi
[ -f "$state/project-pending/old/settings.json" ]
printf 'kept=%s\\n' "$RC_KEPT"
rm "$HOME/rc/old/.claude/settings.json"
printf 'user-change' > "$HOME/rc/old/.claude/settings.json"
if remote_control_guard restore; then exit 3; fi
[ "$(cat "$HOME/rc/old/.claude/settings.json")" = user-change ]
[ -f "$state/project-pending/old/settings.json" ]
cp "$state/project-pending/old/applied.json" "$HOME/rc/old/.claude/settings.json"
remote_control_guard restore
printf 'recovered=%s\\n' "$(cat "$HOME/rc/old/.claude/settings.json")"
`;
    const result = spawnSync('sh', ['-c', driver], {
      encoding: 'utf8',
      // 全工程をまとめた子shが固定テストの並列負荷で20秒を超えても、末尾の復旧検証まで待つ。
      timeout: 120000,
      env: {
        ...process.env,
        HOME: home,
        PATH: `${bin}:${process.env.PATH}`,
        TEST_REPO: repo,
        TEST_OUTSIDE: path.join(tmp, 'outside'),
        GUARD: path.resolve('deploy/nas/update-remote-control-guard.sh'),
        UPDATE: path.resolve('deploy/nas/update-remote-control.sh'),
      },
    });
    assert.ifError(result.error);
    assert.equal(result.status, 0, result.stderr + result.stdout);
    assert.doesNotMatch(
      result.stdout + result.stderr,
      /notice (?:output|error) must stay hidden/,
    );
    const notices = fs
      .readFileSync(path.join(home, 'notices'), 'utf8')
      .trim()
      .split('\n');
    assert.equal(notices.length, 6, notices.join('\n'));
    assert.equal(
      notices[0],
      notices[1],
      'failed send is retried with identical payload',
    );
    assert.equal(
      new Set(notices.slice(1)).size,
      5,
      'each definition is sent only once after success',
    );
    for (const notice of notices)
      assert.match(
        notice,
        /^desk-ops\.ts notice remote-control --signature [a-f0-9]{64} --instances fresh$/,
      );
    const phases: Record<string, string> = {};
    for (const part of result.stdout.split('==').slice(1)) {
      const split = part.indexOf('\n');
      phases[part.slice(0, split)] = part.slice(split + 1);
    }
    assert.match(phases.install, /\.hidden=new-settings/);
    assert.match(
      phases.install,
      /fresh=new-settings\nold=new-settings\nsame=new-settings/,
    );
    assert.match(
      phases.install,
      /custom=my-settings\ntracked=old-settings\nlinked=outside\nplain=new-settings/,
    );
    assert.equal((phases.install.match(/置き換えません/g) || []).length, 3);
    assert.match(phases.unchanged, /^calls=0\n/);
    assert.doesNotMatch(phases.unchanged, /update:/);
    assert.match(phases['project-only'], /calls=[1-9]/);
    assert.match(
      phases['project-only'],
      /restarts=--user restart claude-rc@fresh\.service \n/,
    );
    assert.match(
      phases['project-only'],
      /プロジェクト設定が変わったため、claude-rc@fresh\.service を再起動しました/,
    );
    assert.match(phases['new-dir'], /restarts=\n/);
    assert.match(phases['new-dir'], /再起動していません）: newdir\n/);
    assert.match(phases['new-dir'], /newdir=new-settings\n/);
    assert.match(phases['new-dir-again'], /^calls=0\n/);
    assert.match(phases['recover-new-dir'], /restarts=\n/);
    assert.match(phases['recover-new-dir'], /前回の更新が途中で止まっていた/);
    assert.match(
      phases['recover-fresh'],
      /restarts=--user restart claude-rc@fresh\.service \n/,
    );
    assert.doesNotMatch(phases['new-dir-again'], /update:/);
    for (const phase of ['version-error', 'git-error', 'copy-error']) {
      assert.match(phases[phase], /^calls=0\n/);
      assert.match(phases[phase], /fresh=new-settings\nold=new-settings/);
    }
    assert.match(phases.failure, /前の定義へ戻して再起動しました/);
    assert.match(phases.failure, /fresh=new-settings\nold=new-settings/);
    assert.match(phases.failure, /failed-record/);
    for (const phase of [
      'suppressed',
      'notice-sent',
      'version-suppressed',
      'unit-suppressed',
      'settings-suppressed',
    ]) {
      assert.match(phases[phase], /^calls=0\n/);
      assert.doesNotMatch(phases[phase], /update:/);
    }
    assert.match(phases['version-retry'], /calls=[1-9]/);
    assert.match(phases['previous-sent'], /calls=[1-9]/);
    assert.match(phases['previous-sent-again'], /calls=[1-9]/);
    assert.match(phases['unit-retry'], /calls=[1-9]/);
    assert.match(phases['unit-retry'], /failed-record/);
    assert.match(phases['settings-retry'], /calls=[1-9]/);
    assert.match(phases['settings-retry'], /failed-record/);
    assert.match(phases.success, /no-record/);
    assert.match(phases.final, /^calls=0\n/);
    assert.match(phases['rollback-failure'], /戻した後も起動しなかった/);
    assert.match(phases['rollback-suppressed'], /^calls=0\n/);
    assert.doesNotMatch(phases['rollback-suppressed'], /update:/);
    assert.match(
      phases['key-change-recovery'],
      /前回の更新が途中で止まっていた/,
    );
    assert.match(phases['key-change-success'], /no-record/);
    assert.match(phases['key-change-success'], /exclude-lines=1/);
    assert.match(phases['key-change-success'], /recovered=old-settings/);
    assert.match(phases['key-change-success'], /退避と未完了記録を残しました/);
    assert.match(phases['key-change-success'], /kept=.*prev.99/);
    assert.equal(
      fs.existsSync(path.join(home, 'rc/tracked/.git/info/exclude')),
      false,
    );
    assert.equal(fs.existsSync(path.join(home, 'rc/plain/.git')), false);
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true });
  }
});

test('remote-control notice keeps pending records across persistence errors and interrupted cleanup', () => {
  const home = fs.mkdtempSync(path.join(os.tmpdir(), 'rc-notice-errors-'));
  try {
    const driver = `set -eu
. "$GUARD"
rc_guard_root=$HOME/notices
rc_guard_failed=$HOME/failed-definition
rc_guard_key=definition
rc_bad_new='first, second'
signature=$(printf '%s\\n' "$rc_guard_key" | sha256sum)
signature=\${signature%% *}
pending=$rc_guard_root/notice-pending/$signature
sent=$rc_guard_root/notice-sent/$signature
# Failure to hash, create the directory, write or rename never creates a ready payload.
( sha256sum() { return 1; }; if remote_control_notice_queue; then exit 1; fi )
printf blocked > "$rc_guard_root"
if remote_control_notice_queue 2>/dev/null; then exit 2; fi
rm "$rc_guard_root"
( tr() { return 1; }; if remote_control_notice_queue; then exit 3; fi )
[ ! -f "$pending" ] && [ ! -f "$pending.$$.tmp" ]
( mv() { return 1; }; if remote_control_notice_queue; then exit 4; fi )
[ ! -f "$pending" ] && [ ! -f "$pending.$$.tmp" ]
remote_control_notice_queue
[ "$(cat "$pending")" = first,second ]
# Invalid filenames and non-files are ignored; failures keep the valid pending payload.
printf ignored > "$rc_guard_root/notice-pending/invalid.tmp"
printf ignored > "$rc_guard_root/notice-pending/abcd"
mkdir "$rc_guard_root/notice-pending/directory"
timeout() { printf '%s\\n' "$*" >> "$HOME/send-calls"; return 0; }
( cat() { return 1; }; remote_control_notice_send )
[ ! -f "$HOME/send-calls" ]
( timeout() { return 1; }; remote_control_notice_send )
[ -f "$pending" ] && [ ! -f "$sent" ]
( mkdir() { return 1; }; remote_control_notice_send )
[ -f "$pending" ] && [ ! -f "$sent" ]
mkdir -p "$sent"
remote_control_notice_send 2>/dev/null || :
[ -f "$pending" ]
rmdir "$sent"
# A crash after sending and marking, before cleanup, must not send again.
( rm() { return 1; }; remote_control_notice_send )
[ -f "$sent" ] && [ -f "$pending" ]
before=$(wc -l < "$HOME/send-calls")
remote_control_notice_send
[ "$before" = "$(wc -l < "$HOME/send-calls")" ]
[ ! -f "$pending" ]
remote_control_notice_queue
[ ! -f "$pending" ]
[ -f "$rc_guard_root/notice-pending/invalid.tmp" ]
[ -f "$rc_guard_root/notice-pending/abcd" ]
# Notification persistence/send errors do not change the existing failed action's result or output.
( remote_control_notice_queue() { return 1; }; remote_control_notice_send() { return 1; }; remote_control_guard failed )
[ "$(cat "$rc_guard_failed")" = definition ]
`;
    const result = spawnSync('sh', ['-c', driver], {
      encoding: 'utf8',
      env: {
        ...process.env,
        HOME: home,
        GUARD: path.resolve('deploy/nas/update-remote-control-guard.sh'),
      },
    });
    assert.equal(result.status, 0, result.stderr + result.stdout);
    assert.equal(result.stdout, '');
    assert.equal(result.stderr, '');
    const calls = fs
      .readFileSync(path.join(home, 'send-calls'), 'utf8')
      .trim()
      .split('\n');
    assert.equal(calls.length, 3);
    for (const call of calls)
      assert.match(
        call,
        /^30 .*desk desk-ops\.ts notice remote-control --signature [a-f0-9]{64} --instances first,second$/,
      );
  } finally {
    fs.rmSync(home, { recursive: true, force: true });
  }
});
