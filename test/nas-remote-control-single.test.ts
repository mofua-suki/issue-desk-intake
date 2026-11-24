// deploy/nas/update-remote-control-single.sh を、一時のホームと偽の systemctl で走らせる。
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';

const windows = process.platform === 'win32';
const KEEP = [
  'issue-desk-worker',
  'demo-image-generator',
  'image-review-client',
];

// 状態は $SC_STATE/<unit>.{active,enabled,failed} の有無。呼び出しは $SC_LOG へ1行ずつ。
const fakeSystemctl = `#!/bin/sh
st=$SC_STATE
printf '%s\\n' "$*" >> "$SC_LOG"
[ "$1" = --user ] && shift
cmd=$1; shift
case "$cmd" in
list-units)
  [ "\${SC_LIST:-ok}" = ok ] || exit 1
  for f in "$st"/*; do
    [ -e "$f" ] || continue
    n=\${f##*/}; n=\${n%.*}
    printf '%s\\n' "$n"
  done | sort -u | while read -r n; do
    if [ -e "$st/$n.failed" ]; then printf '● %s loaded failed failed x\\n' "$n";
    elif [ -e "$st/$n.active" ]; then printf '%s loaded active running x\\n' "$n";
    else printf '%s loaded inactive dead x\\n' "$n"; fi
  done
  ;;
is-enabled) [ -e "$st/$2.enabled" ]; exit $? ;;
is-active) [ -e "$st/$2.active" ]; exit $? ;;
is-failed) [ -e "$st/$2.failed" ]; exit $? ;;
reset-failed) rm -f "$st/$1.failed" ;;
enable)
  u=$2
  : > "$st/$u.enabled"
  mkdir -p "$HOME/.config/systemd/user/default.target.wants"
  ln -sf ../claude-rc@.service "$HOME/.config/systemd/user/default.target.wants/$u"
  if [ "\${SC_PRIMARY:-good}" = good ]; then : > "$st/$u.active"; else : > "$st/$u.failed"; fi
  ;;
disable)
  u=$2
  [ "$u" != "\${SC_DISABLE_FAIL:-}" ] || exit 1
  rm -f "$st/$u.enabled" "$st/$u.active" "$HOME/.config/systemd/user/default.target.wants/$u"
  : > "$st/$u.loaded"
  ;;
*) ;;
esac
exit 0
`;

const driver = `set -eu
DRY_RUN=\${DRY_RUN:-0}
RC_WAIT_SECONDS=1
RC_SETTLE_SECONDS=0
say() { printf '%s\\n' "$*"; }
plan() { say "[update:dry-run] $*"; }
. "$REPO/deploy/nas/update-remote-control.sh"
. "$REPO/deploy/nas/update-remote-control-single.sh"
if [ "\${MODE:-nas}" = pc ]; then
  remote_control_stop_others ''
  printf 'stopped=%s failed=%s\\n' "$RS_STOPPED" "$RS_FAILED"
else
  remote_control_single "\${CLAUDE_READY:-1}"
fi
`;

function setup() {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'rc-single-'));
  const home = path.join(tmp, 'home');
  const bin = path.join(tmp, 'bin');
  const state = path.join(tmp, 'state');
  for (const d of [home, bin, state]) fs.mkdirSync(d, { recursive: true });
  fs.writeFileSync(path.join(bin, 'systemctl'), fakeSystemctl, { mode: 0o755 });
  fs.writeFileSync(path.join(tmp, 'driver.sh'), driver);
  const unitDir = path.join(home, '.config/systemd/user');
  const wants = path.join(unitDir, 'default.target.wants');
  const env = {
    HOME: home,
    SC_STATE: state,
    SC_LOG: path.join(tmp, 'log'),
    REPO: path.resolve('.'),
  };
  const place = () => {
    fs.mkdirSync(wants, { recursive: true });
    fs.writeFileSync(path.join(unitDir, 'claude-rc@.service'), 'unit');
    fs.mkdirSync(path.join(home, 'rc/issue-desk-intake'), { recursive: true });
    for (const n of KEEP)
      fs.mkdirSync(path.join(home, 'rc', n), { recursive: true });
  };
  const unit = (name, ...flags) => {
    for (const f of flags)
      fs.writeFileSync(path.join(state, `claude-rc@${name}.service.${f}`), '');
    if (flags.includes('enabled'))
      fs.symlinkSync(
        '../claude-rc@.service',
        path.join(wants, `claude-rc@${name}.service`),
      );
  };
  const run = (extra = {}) => {
    fs.rmSync(env.SC_LOG, { force: true });
    const r = spawnSync('/bin/sh', [path.join(tmp, 'driver.sh')], {
      encoding: 'utf8',
      env: {
        ...process.env,
        ...env,
        ...extra,
        PATH: [bin, process.env.PATH].join(path.delimiter),
      },
    });
    const log = fs.existsSync(env.SC_LOG)
      ? fs.readFileSync(env.SC_LOG, 'utf8')
      : '';
    return { status: r.status, out: r.stdout, err: r.stderr, log };
  };
  const has = (name, flag) =>
    fs.existsSync(path.join(state, `claude-rc@${name}.service.${flag}`));
  return { tmp, home, place, unit, run, has };
}

test(
  'remote-control single: does nothing on hosts without the managed unit',
  { skip: windows },
  () => {
    const t = setup();
    const r = t.run();
    assert.equal(r.status, 0, r.err);
    assert.equal(r.out, '');
    assert.equal(r.log, '');
  },
);

test(
  'remote-control single: keeps the others when the primary folder is missing',
  { skip: windows },
  () => {
    const t = setup();
    t.place();
    fs.rmSync(path.join(t.home, 'rc/issue-desk-intake'), { recursive: true });
    t.unit('old-a', 'enabled', 'active');
    const r = t.run();
    assert.equal(r.status, 0, r.err);
    assert.match(
      r.out,
      /^\[update:remote-control-failed\] 代表の作業フォルダー .*issue-desk-intake が無い/,
    );
    assert.equal(r.log, '');
    assert.ok(t.has('old-a', 'active'));
  },
);

test(
  'remote-control single: trusts and starts the primary, then stops every other instance once',
  { skip: windows },
  () => {
    const t = setup();
    t.place();
    fs.writeFileSync(
      path.join(t.home, '.claude.json'),
      JSON.stringify({ keep: 1, projects: { '/x': { a: 1 } } }),
    );
    t.unit('issue-desk-intake', 'failed');
    t.unit('old-a', 'enabled', 'active');
    t.unit('old-b', 'enabled');
    t.unit('old-c', 'failed');
    const r = t.run();
    assert.equal(r.status, 0, r.err);
    assert.equal(
      r.out,
      KEEP.map(
        (n) =>
          `[update:remote-control] システムの窓口 claude-rc@${n}.service を有効にして起動しました\n`,
      ).join('') +
        '[update:remote-control] システムごとに1つの窓口だけにするため、claude-rc@old-a.service claude-rc@old-b.service claude-rc@old-c.service を止めました\n',
    );
    const cj = JSON.parse(
      fs.readFileSync(path.join(t.home, '.claude.json'), 'utf8'),
    );
    assert.equal(cj.keep, 1);
    assert.deepEqual(cj.projects['/x'], { a: 1 });
    assert.equal(
      cj.projects[path.join(t.home, 'rc/issue-desk-intake')]
        .hasTrustDialogAccepted,
      true,
    );
    assert.equal(
      fs.statSync(path.join(t.home, '.claude.json')).mode & 0o777,
      0o600,
    );
    assert.ok(t.has('issue-desk-intake', 'active'));
    assert.ok(t.has('issue-desk-intake', 'enabled'));
    for (const n of ['old-a', 'old-b', 'old-c']) {
      assert.ok(
        !t.has(n, 'active') && !t.has(n, 'enabled') && !t.has(n, 'failed'),
        n,
      );
    }
    assert.ok(!r.log.includes('disable --now claude-rc@issue-desk-intake'));
    assert.ok(
      t.has('issue-desk-worker', 'active') &&
        t.has('issue-desk-worker', 'enabled'),
    );
    assert.equal(
      cj.projects[path.join(t.home, 'rc/issue-desk-worker')]
        .hasTrustDialogAccepted,
      true,
    );

    // 2回目: そろっていれば何も出さず、止め直さない。
    const again = t.run();
    assert.equal(again.out, '');
    assert.ok(!/disable|enable /.test(again.log), again.log);
  },
);

test(
  'remote-control single: keeps every system window running and never disables them',
  { skip: windows },
  () => {
    const t = setup();
    t.place();
    t.unit('issue-desk-intake', 'enabled', 'active');
    for (const n of KEEP) t.unit(n, 'enabled', 'active');
    t.unit('old-a', 'enabled', 'active');
    const r = t.run();
    assert.equal(r.status, 0, r.err);
    assert.equal(
      r.out,
      '[update:remote-control] システムごとに1つの窓口だけにするため、claude-rc@old-a.service を止めました\n',
    );
    for (const n of KEEP) {
      assert.ok(t.has(n, 'active') && t.has(n, 'enabled'), n);
      assert.ok(!r.log.includes(`able --now claude-rc@${n}.service`), r.log);
    }
  },
);

test(
  'remote-control single: keeps existing windows until every replacement is available',
  { skip: windows },
  () => {
    const t = setup();
    t.place();
    fs.rmSync(path.join(t.home, 'rc/issue-desk-worker'), { recursive: true });
    t.unit('issue-desk-intake', 'enabled', 'active');
    t.unit('demo-image-culling', 'enabled', 'active');
    const r = t.run();
    assert.equal(r.status, 0, r.err);
    assert.match(
      r.out,
      /^\[update:remote-control-failed\] システムの窓口 claude-rc@issue-desk-worker\.service の作業フォルダー .*issue-desk-worker が無い/,
    );
    assert.ok(!/claude-rc@issue-desk-worker|disable --now/.test(r.log), r.log);
    assert.ok(t.has('demo-image-culling', 'active'));
  },
);

test(
  'remote-control single: an unavailable image review setup leaves other windows serviceable',
  { skip: windows },
  () => {
    const t = setup();
    t.place();
    t.unit('issue-desk-intake', 'enabled', 'active');
    t.unit('demo-image-culling', 'enabled', 'active');
    const r = t.run({ CLAUDE_READY: '0' });
    assert.equal(r.status, 0, r.err);
    assert.ok(t.has('issue-desk-worker', 'active'));
    assert.ok(t.has('demo-image-generator', 'active'));
    assert.ok(!t.has('image-review-client', 'active'));
    assert.ok(t.has('demo-image-culling', 'active'));
  },
);

test(
  'remote-control single: never stops the others when the primary does not start',
  { skip: windows },
  () => {
    const t = setup();
    t.place();
    t.unit('old-a', 'enabled', 'active');
    const r = t.run({ SC_PRIMARY: 'crash' });
    assert.equal(r.status, 0, r.err);
    assert.match(
      r.out,
      /^\[update:remote-control-failed\] 代表の claude-rc@issue-desk-intake\.service が起動しないため/,
    );
    assert.ok(!/disable/.test(r.log));
    assert.ok(t.has('old-a', 'active'));
  },
);

test(
  'remote-control single: leaves .claude.json and the units alone when it is not JSON',
  { skip: windows },
  () => {
    const t = setup();
    t.place();
    fs.writeFileSync(path.join(t.home, '.claude.json'), '{broken');
    t.unit('old-a', 'enabled', 'active');
    const r = t.run();
    assert.match(
      r.out,
      /^\[update:remote-control-failed\] ~\/\.claude\.json に .* の信頼を書けない/,
    );
    assert.equal(
      fs.readFileSync(path.join(t.home, '.claude.json'), 'utf8'),
      '{broken',
    );
    assert.ok(!/enable|disable/.test(r.log));
  },
);

test(
  'remote-control single: reports instances it could not stop and list failures',
  { skip: windows },
  () => {
    const t = setup();
    t.place();
    t.unit('old-a', 'enabled', 'active');
    t.unit('old-b', 'enabled', 'active');
    const r = t.run({ SC_DISABLE_FAIL: 'claude-rc@old-a.service' });
    assert.match(
      r.out,
      /止めました\n\[update:remote-control-failed\] 止められなかった remote-control: claude-rc@old-a\.service\n$/,
    );
    const list = t.run({ SC_LIST: 'fail' });
    assert.match(
      list.out,
      /list-units に失敗したため、ほかの remote-control は止めていません/,
    );
  },
);

test(
  'remote-control single: dry-run only prints the plan',
  { skip: windows },
  () => {
    const t = setup();
    t.place();
    t.unit('old-a', 'enabled', 'active');
    const r = t.run({ DRY_RUN: '1' });
    assert.match(
      r.out,
      /^\[update:dry-run\] 予定（最後に）: claude-rc@issue-desk-intake\.service とシステムの窓口 issue-desk-worker demo-image-generator/,
    );
    assert.equal(r.log, '');
  },
);

test(
  'remote-control single: the Ubuntu PC stops every instance',
  { skip: windows },
  () => {
    const t = setup();
    t.place();
    t.unit('demo-image-rating', 'enabled', 'active');
    t.unit('issue-desk-intake', 'enabled', 'active');
    const r = t.run({ MODE: 'pc' });
    assert.equal(r.status, 0, r.err);
    assert.equal(
      r.out,
      'stopped=claude-rc@demo-image-rating.service claude-rc@issue-desk-intake.service failed=\n',
    );
    assert.ok(
      !t.has('demo-image-rating', 'active') &&
        !t.has('issue-desk-intake', 'active'),
    );
  },
);
