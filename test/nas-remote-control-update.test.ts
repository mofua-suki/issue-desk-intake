// deploy/nas/update-remote-control.sh を、一時のホームと偽の systemctl で1回の sh だけで走らせる。
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
const sh = '/bin/sh';
const withoutShellWarnings = (text) => String(text);

test('shipped remote-control unit leaves the auto-compact window to the Claude Code default', () => {
  const unit = fs.readFileSync(
    path.resolve('deploy/nas/claude-rc@.service'),
    'utf8',
  );
  const assignments =
    unit.match(/^\s*Environment=.*CLAUDE_CODE_AUTO_COMPACT_WINDOW.*$/gm) ?? [];
  assert.deepEqual(assignments, []);
  assert.match(unit, /^ExecStart=.*\bclaude remote-control\b/m);
});

const windows = process.platform === 'win32';
const shPath = (p) =>
  windows
    ? p
        .replace(/^([A-Za-z]):[\\/]/, (_, d) => `/${d.toLowerCase()}/`)
        .replaceAll('\\', '/')
    : p;

const driver = `set -eu
HOME=$TEST_HOME
DIR=$TEST_REPO
DRY_RUN=0
export HOME SC_MODE
SC_MODE=good
RC_WAIT_SECONDS=1
RC_SETTLE_SECONDS=0
say() { printf '%s\\n' "$*"; }
plan() { say "[update:dry-run] $*"; }
. "$RC_SCRIPT"
unit=$HOME/.config/systemd/user/claude-rc@.service
set_file=$HOME/.issue-desk/remote-control/settings.json
src=$DIR/deploy/nas
phase() { printf '== %s\\n' "$1"; }
run() {
  status=0
  out=$(remote_control) || status=$?
  printf 'status=%s\\n' "$status"
  if [ -n "$out" ]; then printf '%s\\n' "$out"; fi
  if [ -f "$SC_LOG" ]; then cat "$SC_LOG"; rm -f "$SC_LOG"; fi
}
dump() {
  for f in "$unit" "$set_file" "$unit.before-issue-desk"; do
    if [ -f "$f" ]; then printf '%s=%s\\n' "\${f##*/}" "$(cat "$f")"; else printf '%s=-\\n' "\${f##*/}"; fi
  done
  ls "$HOME/.config/systemd/user" "$HOME/.issue-desk/remote-control" 2>/dev/null | grep -c '\\.prev\\.\\|\\.tmp$' || :
}

phase not-target
run
phase write-failure
mkdir -p "$HOME/.config/systemd/user"
printf 'old-unit' > "$unit"
mkdir -p "$HOME/.issue-desk"
printf 'blocker' > "$HOME/.issue-desk/remote-control"
run
dump
rm "$HOME/.issue-desk/remote-control"
phase missing-source
DIR=$TEST_REPO/none
run
DIR=$TEST_REPO
phase create
run
dump
phase unchanged
run
phase comment-only
printf 'unit-v1\\n# note\\n\\n; other\\n' > "$src/claude-rc@.service"
run
dump
phase comment-only-back
printf 'unit-v1' > "$src/claude-rc@.service"
run
dump
phase settings-only
printf 'settings-v2' > "$src/remote-control-settings.json"
run
dump
phase crash-rollback
printf 'unit-v3' > "$src/claude-rc@.service"
printf 'settings-new' > "$src/remote-control-settings.json"
SC_MODE=crash-b
run
dump
phase start-limit
SC_MODE=limit-b
run
dump
ls "$SC_STATE"
phase restart-failure
SC_MODE=restart-fail
run
dump
pending=$unit.update-pending
pend() { if [ -f "$pending" ]; then echo pending; else echo no-pending; fi; }
pend
phase recover-after-restart-failure
SC_MODE=good
run
pend
phase systemctl-failure
SC_MODE=reload-fail
run
dump
pend
phase recover-after-reload-failure
SC_MODE=good
run
pend
phase dry-run
SC_MODE=good
DRY_RUN=1
run
dump
DRY_RUN=0
phase restore-failure
printf 'unit-v4' > "$src/claude-rc@.service"
printf 'settings-new2' > "$src/remote-control-settings.json"
SC_MODE=crash-b
MV_FAIL=1
export MV_FAIL
run
dump
for f in "$unit".prev.* "$set_file".prev.*; do printf 'kept=%s\\n' "$(cat "$f")"; rm -f "$f"; done
MV_FAIL=0
interrupt() {
  printf 'unit-v1' > "$unit.prev.99"
  printf 'settings-v2' > "$set_file.prev.99"
  printf 'stage=restarting\\nprev_unit=%s\\nprev_set=%s\\nhad_set=1\\nunits=claude-rc@a.service claude-rc@b.service\\n' "$unit.prev.99" "$set_file.prev.99" > "$pending"
}
phase interrupted-fail
interrupt
SC_MODE=restart-fail
run
dump
pend
phase interrupted-retry
SC_MODE=good
run
dump
pend
phase interrupted-bad-record
printf 'stage=restarting\\nprev_unit=/etc/passwd\\nprev_set=x\\nhad_set=1\\nunits=\\n' > "$pending"
run
pend
rm -f "$pending"
phase placing-fail
printf 'unit-v1' > "$unit.prev.98"
printf 'settings-v2' > "$set_file.prev.98"
printf 'stage=placing\\nprev_unit=%s\\nprev_set=%s\\nhad_set=1\\nunits=\\n' "$unit.prev.98" "$set_file.prev.98" > "$pending"
SC_MODE=restart-fail
run
grep '^units=' "$pending"
pend
phase placing-retry
SC_MODE=good-lista
run
pend
phase journal-fail
printf 'unit-v5' > "$src/claude-rc@.service"
printf 'settings-new3' > "$src/remote-control-settings.json"
SC_MODE=good
MV_FAIL=2
run
dump
pend
MV_FAIL=0
rm -f "$pending"
phase relative-home
HOME=relative
run
`;

test(
  'NAS remote-control update places files, restarts only on change and rolls back in one sh',
  { skip: !fs.existsSync(sh) && `${sh} がない` },
  (t) => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'nas-remote-control-'));
    t.after(() => fs.rmSync(tmp, { recursive: true, force: true }));
    const home = path.join(tmp, 'home');
    const repo = path.join(home, 'repo');
    const bin = path.join(tmp, 'bin');
    const log = path.join(tmp, 'systemctl.log');
    const state = path.join(tmp, 'state');
    for (const dir of [path.join(repo, 'deploy/nas'), bin])
      fs.mkdirSync(dir, { recursive: true });
    fs.writeFileSync(
      path.join(repo, 'deploy/nas/claude-rc@.service'),
      'unit-v1',
    );
    fs.writeFileSync(
      path.join(repo, 'deploy/nas/remote-control-settings.json'),
      'settings-v1',
    );
    const put = (file, code) => fs.writeFileSync(file, code, { mode: 0o755 });
    // MV_FAIL=1 のとき、.prev の退避から配置先への復元（mv -f <退避> <配置先>）だけを失敗させる。
    put(
      path.join(bin, 'mv'),
      `#!/bin/sh
if [ "\${MV_FAIL:-0}" = 1 ]; then
  case "$2" in *.prev.*) exit 1 ;; esac
fi
# MV_FAIL=2 のとき、許可一覧が新しい内容に置き換わった後の未完了記録の書き込みだけを失敗させる。
if [ "\${MV_FAIL:-0}" = 2 ]; then
  case "$2" in *update-pending.*.tmp)
    case "$(cat "$TEST_HOME/.issue-desk/remote-control/settings.json" 2>/dev/null)" in *new3*) exit 1 ;; esac ;;
  esac
fi
exec /bin/mv "$@"
`,
    );
    put(
      path.join(bin, 'systemctl'),
      `#!/bin/sh
printf '%s %s %s\\n' "$XDG_RUNTIME_DIR" "$DBUS_SESSION_BUS_ADDRESS" "$*" >> "$SC_LOG"
new=0
case "$(cat "$TEST_HOME/.issue-desk/remote-control/settings.json" 2>/dev/null)" in *new*) new=1 ;; esac
case "$*" in
*daemon-reload) [ "$SC_MODE" != reload-fail ] || exit 1 ;;
*list-units*)
  printf 'claude-rc@a.service loaded active running x\\n'
  # good-lista: b が active 一覧から外れている（前回の途中で止まった後の状態）。
  [ "$SC_MODE" = good-lista ] || printf 'claude-rc@b.service loaded active running x\\n'
  ;;
*reset-failed*claude-rc@b.service) rm -f "$SC_STATE/b.limit" ;;
*restart*claude-rc@b.service)
  if [ -f "$SC_STATE/b.limit" ] || [ "$SC_MODE" = restart-fail ]; then printf 'rejected %s\\n' "$*" >> "$SC_LOG"; exit 1; fi
  ;;
*is-active*claude-rc@b.service)
  case "$SC_MODE:$new" in
  crash-b:1 | restart-fail:*) exit 3 ;;
  limit-b:1)
    # 最初は active、落ち続けた後は failed で起動回数の制限にかかる（start-limit-hit）。
    if [ -f "$SC_STATE/b.seen" ]; then : > "$SC_STATE/b.limit"; exit 3; fi
    : > "$SC_STATE/b.seen"
    ;;
  esac
  ;;
esac
exit 0
`,
    );
    fs.mkdirSync(state);
    const file = path.join(tmp, 'driver.sh');
    fs.writeFileSync(file, driver);
    const env = Object.fromEntries(
      Object.entries(process.env).filter(
        ([key]) =>
          !['PATH', 'XDG_RUNTIME_DIR', 'DBUS_SESSION_BUS_ADDRESS'].includes(
            key.toUpperCase(),
          ),
      ),
    );
    const realPath = process.env.PATH ?? process.env.Path ?? '';
    const r = spawnSync(sh, [shPath(file)], {
      encoding: 'utf8',
      env: {
        ...env,
        PATH: [bin, ...(windows ? [path.dirname(sh)] : []), realPath].join(
          path.delimiter,
        ),
        RC_SCRIPT: shPath(path.resolve('deploy/nas/update-remote-control.sh')),
        TEST_HOME: shPath(home),
        TEST_REPO: shPath(repo),
        SC_LOG: shPath(log),
        SC_STATE: shPath(state),
      },
    });
    assert.equal(r.error, undefined);
    assert.equal(r.status, 0, r.stdout + r.stderr);
    assert.equal(withoutShellWarnings(r.stderr), '');
    const g = Object.fromEntries(
      r.stdout
        .split(/^== /m)
        .slice(1)
        .map((s) => [s.split('\n', 1)[0], s.slice(s.indexOf('\n') + 1)]),
    );
    const sc = /^\/run\/user\/\d+ unix:path=\/run\/user\/\d+\/bus /;
    const calls = (name) => g[name].split('\n').filter((l) => sc.test(l));
    const noCalls = (name) => assert.equal(calls(name).length, 0, name);

    // 対象外: 何も出さず systemctl も呼ばない。
    assert.equal(g['not-target'], 'status=0\n');
    // 書けない・正本が無い: 前の内容のまま、失敗を1行出して終了コードは変えない。
    assert.match(
      g['write-failure'],
      /^status=0\n\[update:remote-control-failed\] 書けなかった/,
    );
    assert.match(g['write-failure'], /claude-rc@\.service=old-unit\n/);
    assert.match(g['write-failure'], /\n0\n$/);
    noCalls('write-failure');
    assert.match(
      g['missing-source'],
      /^status=0\n\[update:remote-control-failed\] .*読めない/,
    );
    // 作成: 元を1回だけ退避し、daemon-reload と動いている全インスタンスの再起動を行う。
    assert.match(
      g.create,
      /^status=0\n\[update:remote-control\] .*claude-rc@a\.service claude-rc@b\.service を再起動しました\n/,
    );
    assert.match(
      g.create,
      /claude-rc@\.service=unit-v1\nsettings\.json=settings-v1\nclaude-rc@\.service\.before-issue-desk=old-unit\n0\n$/,
    );
    assert.deepEqual(
      calls('create').map((l) =>
        l.replace(sc, '').split(' ').slice(0, 2).join(' '),
      ),
      [
        '--user daemon-reload',
        '--user list-units',
        '--user reset-failed',
        '--user restart',
        '--user reset-failed',
        '--user restart',
        '--user is-active',
        '--user is-active',
        '--user is-active',
        '--user is-active',
      ],
    );
    // 変更なし: 何も出さず systemctl を呼ばない。
    assert.equal(g.unchanged, 'status=0\n');
    // ユニットの差がコメント行と空行だけ: 配るが、窓口は再起動しない（チャットを切らない）。
    for (const name of ['comment-only', 'comment-only-back']) {
      assert.match(
        g[name],
        /^status=0\n\[update:remote-control\] 起動定義はコメントだけが変わったため、配り直しましたが再起動していません\n/,
        name,
      );
      assert.equal(
        calls(name).filter((l) => /restart/.test(l)).length,
        0,
        name,
      );
    }
    assert.match(
      g['comment-only'],
      /claude-rc@\.service=unit-v1\n# note\n\n; other\nsettings\.json=settings-v1\n/,
    );
    assert.match(
      g['comment-only-back'],
      /claude-rc@\.service=unit-v1\nsettings\.json=settings-v1\n.*\n0\n$/s,
    );
    // 許可一覧だけの変更でも再起動する。ユニットは変わらず、退避は上書きしない。
    assert.match(g['settings-only'], /^status=0\n\[update:remote-control\] /);
    assert.match(
      g['settings-only'],
      /claude-rc@\.service=unit-v1\nsettings\.json=settings-v2\nclaude-rc@\.service\.before-issue-desk=old-unit\n/,
    );
    assert.equal(
      calls('settings-only').filter((l) => /restart/.test(l)).length,
      2,
    );
    // 起動しない: 前の内容へ戻して再起動し、起動しなかったインスタンスを出す。退避は上書きしない。
    assert.match(
      g['crash-rollback'],
      /^status=0\n\[update:remote-control-failed\] 起動しなかったインスタンス: b。前の定義へ戻して再起動しました\n/,
    );
    assert.match(
      g['crash-rollback'],
      /claude-rc@\.service=unit-v1\nsettings\.json=settings-v2\nclaude-rc@\.service\.before-issue-desk=old-unit\n0\n$/,
    );
    assert.equal(
      calls('crash-rollback').filter((l) => /daemon-reload/.test(l)).length,
      2,
    );
    assert.equal(
      calls('crash-rollback').filter((l) => /restart/.test(l)).length,
      4,
    );
    assert.equal(
      calls('crash-rollback').filter((l) => /reset-failed/.test(l)).length,
      4,
    );
    // 最初は active、確かめ直すと落ちて起動回数の制限にかかる: 戻すときに制限を解いてから restart し、起動を確かめる。
    assert.match(
      g['start-limit'],
      /^status=0\n\[update:remote-control-failed\] 起動しなかったインスタンス: b。前の定義へ戻して再起動しました\n/,
    );
    assert.doesNotMatch(g['start-limit'], /^rejected /m);
    assert.match(
      g['start-limit'],
      /claude-rc@\.service=unit-v1\nsettings\.json=settings-v2\nclaude-rc@\.service\.before-issue-desk=old-unit\n0\nb\.seen\n$/,
    );
    const limitCalls = calls('start-limit').map((l) => l.replace(sc, ''));
    const lastResetB = limitCalls.lastIndexOf(
      '--user reset-failed claude-rc@b.service',
    );
    const lastRestartB = limitCalls.lastIndexOf(
      '--user restart claude-rc@b.service',
    );
    assert.ok(
      lastResetB !== -1 && lastResetB < lastRestartB,
      limitCalls.join('\n'),
    );
    assert.ok(
      limitCalls
        .slice(lastRestartB)
        .includes('--user is-active --quiet claude-rc@b.service'),
    );
    // restart が拒まれる: 最初の restart の失敗で戻し、戻した後も起動しなければ「再起動しました」とは言わない。
    assert.match(
      g['restart-failure'],
      /^status=0\n\[update:remote-control-failed\] 起動しなかったインスタンス: b。前の定義へ戻しましたが、戻した後も起動しなかったインスタンス: b\n/,
    );
    assert.doesNotMatch(g['restart-failure'], /再起動しました/);
    assert.equal(
      g['restart-failure'].split('\n').filter((l) => l.startsWith('rejected '))
        .length,
      2,
    );
    assert.match(
      g['restart-failure'],
      /claude-rc@\.service=unit-v1\nsettings\.json=settings-v2\nclaude-rc@\.service\.before-issue-desk=old-unit\n0\npending\n$/,
    );
    // 戻した後も起動しなかった回は記録を残し、次の呼び出しが再起動を完了させて記録を消す。
    assert.match(
      g['recover-after-restart-failure'],
      /^status=0\n\[update:remote-control\] 前回の更新が途中で止まっていたため.*\nno-pending\n$/s,
    );
    // systemctl の失敗: 戻して失敗を1行出し、終了コードは変えない。
    assert.match(
      g['systemctl-failure'],
      /^status=0\n\[update:remote-control-failed\] systemctl --user daemon-reload に失敗/,
    );
    assert.match(
      g['systemctl-failure'],
      /claude-rc@\.service=unit-v1\nsettings\.json=settings-v2\n/,
    );
    assert.equal(
      calls('systemctl-failure').filter((l) => /restart/.test(l)).length,
      0,
    );
    assert.match(g['systemctl-failure'], /\npending\n$/);
    assert.match(
      g['recover-after-reload-failure'],
      /^status=0\n\[update:remote-control\] 前回の更新が途中で止まっていたため.*\nno-pending\n$/s,
    );
    // dry-run: 書かず、systemctl も呼ばず、予定を1行。
    assert.match(
      g['dry-run'],
      /^status=0\n\[update:dry-run\] 予定（最後に）: /,
    );
    assert.equal(
      g['dry-run'].split('\n').filter((l) => l.startsWith('[update:')).length,
      1,
    );
    assert.match(g['dry-run'], /settings\.json=settings-v2\n/);
    noCalls('dry-run');
    // 復元の mv だけが失敗: 退避を残し、戻したとは言わず、退避先を出す。終了コードは変えない。
    assert.match(
      g['restore-failure'],
      /^status=0\n\[update:remote-control-failed\] 起動しなかったインスタンス: b。前の定義へ戻せませんでした（再起動していません）。退避を残しました: \S+claude-rc@\.service\.prev\.\d+ \S+settings\.json\.prev\.\d+\n/,
    );
    assert.doesNotMatch(
      g['restore-failure'],
      /戻して再起動しました|戻しました/,
    );
    assert.match(
      g['restore-failure'],
      /claude-rc@\.service=unit-v4\nsettings\.json=settings-new2\nclaude-rc@\.service\.before-issue-desk=old-unit\n2\nkept=unit-v1\nkept=settings-v2\n$/,
    );
    // 配置後に止まった記録がある: 内容比較より先に旧内容へ戻し、daemon-reload と記録された対象を再起動する。
    // 再起動の確認に失敗したら記録を残し、次の呼び出しで起動を確かめられたときだけ消す。
    assert.match(
      g['interrupted-fail'],
      /^status=0\n\[update:remote-control-failed\] 前回の更新を戻しましたが、起動しなかったインスタンス: b。未完了記録を残しました\n/,
    );
    assert.match(
      g['interrupted-fail'],
      /claude-rc@\.service=unit-v1\nsettings\.json=settings-v2\nclaude-rc@\.service\.before-issue-desk=old-unit\n0\npending\n$/,
    );
    assert.deepEqual(
      calls('interrupted-fail').map((l) =>
        l.replace(sc, '').split(' ').slice(0, 3).join(' '),
      ),
      [
        '--user daemon-reload',
        '--user reset-failed claude-rc@a.service',
        '--user restart claude-rc@a.service',
        '--user reset-failed claude-rc@b.service',
        '--user restart claude-rc@b.service',
        '--user is-active --quiet',
        '--user is-active --quiet',
      ],
    );
    assert.match(
      g['interrupted-retry'],
      /^status=0\n\[update:remote-control\] 前回の更新が途中で止まっていたため、前の定義へ戻して claude-rc@a\.service claude-rc@b\.service を再起動しました\n/,
    );
    assert.match(
      g['interrupted-retry'],
      /claude-rc@\.service=unit-v1\nsettings\.json=settings-v2\n.*\n0\nno-pending\n$/s,
    );
    assert.equal(
      calls('interrupted-retry').filter((l) => /restart/.test(l)).length,
      2,
    );
    // 記録が壊れている・範囲外のパスを指す: 何も動かさず、記録を残す。
    assert.match(
      g['interrupted-bad-record'],
      /^status=0\n\[update:remote-control-failed\] 前回の更新の未完了記録を読めない/,
    );
    assert.match(g['interrupted-bad-record'], /pending\n$/);
    noCalls('interrupted-bad-record');
    // 配置直後（対象が空）の記録から復旧して b の再起動が失敗: 対象を記録へ残し、次回は b が active 一覧に無くても
    // a・b とも再起動し、起動を確かめられたときだけ記録を消す。
    assert.match(
      g['placing-fail'],
      /^status=0\n\[update:remote-control-failed\] 前回の更新を戻しましたが、起動しなかったインスタンス: b。未完了記録を残しました\n/,
    );
    assert.match(
      g['placing-fail'],
      /\nunits=claude-rc@a\.service claude-rc@b\.service\npending\n$/,
    );
    assert.match(
      g['placing-retry'],
      /^status=0\n\[update:remote-control\] 前回の更新が途中で止まっていたため、前の定義へ戻して claude-rc@a\.service claude-rc@b\.service を再起動しました\n/,
    );
    assert.match(g['placing-retry'], /no-pending\n$/);
    assert.equal(
      calls('placing-retry').filter((l) => /--user restart/.test(l)).length,
      2,
    );
    assert.ok(
      calls('placing-retry').some((l) =>
        /restart claude-rc@b\.service/.test(l),
      ),
    );
    // 再起動する対象を記録へ書けない: 再起動せず前の内容へ戻し、記録（配置の段階）を残す。
    assert.match(
      g['journal-fail'],
      /^status=0\n\[update:remote-control-failed\] 再起動する対象を未完了記録へ書けないため、再起動せず前の定義へ戻しました\n/,
    );
    assert.match(
      g['journal-fail'],
      /claude-rc@\.service=unit-v1\nsettings\.json=settings-v2\n/,
    );
    assert.equal(
      calls('journal-fail').filter((l) => /--user restart/.test(l)).length,
      0,
    );
    assert.match(g['journal-fail'], /\npending\n$/);
    // HOME が相対パス。
    assert.equal(
      g['relative-home'],
      'status=0\n[update:remote-control-failed] HOME が絶対パスではないため、remote-control の起動定義と許可一覧を配りません\n',
    );
  },
);
