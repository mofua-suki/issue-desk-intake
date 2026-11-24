import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { spawnSync } from 'node:child_process';

// NAS の窓口は一覧に従い PCトークン窓口へ振り分け、それ以外は一時キーを使う。
function composeStoreEnvironment(color) {
  const compose = fs.readFileSync('deploy/nas/compose.yaml', 'utf8');
  const service = compose
    .split(`  issue-desk-intake-${color}:\n`)[1]
    ?.split(/^ {2}\S/m)[0];
  assert.ok(service, `${color}の配備構成が必要`);
  const entries = [
    ...service.matchAll(
      /^ {6}(ISSUE_DESK_APP_STORE|ISSUE_DESK_DATABASE_URL|PGHOST|PGUSER|PGDATABASE|PGPASSWORD): (.+)$/gm,
    ),
  ].map(
    ([, name, value]) =>
      `${name}=${value === '${ISSUE_DESK_DB_PASSWORD:-}' ? 'secret-pw = with spaces' : value.replace(/^'|'$/g, '')}`,
  );
  assert.equal(entries.length, 6, '配備構成の保存先の6変数を検査する');
  return entries;
}

function fixture({
  entries = composeStoreEnvironment('blue'),
  color = 'blue',
} = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'desk-nas-pr-'));
  const home = path.join(dir, 'home');
  const bin = path.join(dir, 'bin');
  const nas = path.join(dir, 'nas');
  fs.mkdirSync(home);
  fs.mkdirSync(bin);
  fs.mkdirSync(path.join(nas, 'deploy/nas'), { recursive: true });
  // activeの選択も配備される本体を通す（Dockerだけを偽にする）。
  fs.copyFileSync(
    'deploy/nas/intake.sh',
    path.join(nas, 'deploy/nas/intake.sh'),
  );
  const source = path.resolve('deploy/nas/update-desk-access.sh');
  const install = spawnSync(
    'sh',
    [
      '-c',
      '. "$1"; say() { echo "$@"; }; plan() { echo "$@"; }; desk_access',
      'sh',
      source,
    ],
    {
      env: { ...process.env, HOME: home, DIR: path.resolve('.'), DRY_RUN: '0' },
      encoding: 'utf8',
    },
  );
  assert.equal(install.status, 0, install.stderr);
  const docker = path.join(bin, 'docker');
  fs.writeFileSync(
    docker,
    `#!${process.execPath}
const fs = require('node:fs');
const args = process.argv.slice(2);
if (args[0] === 'cp' || args.some(a => a.includes('worker-colors'))) process.exit(9);
if (args[0] === 'ps') console.log('b'.repeat(12));
else if (args[0] === 'compose') {
  if (process.env.COMPOSE_FAIL) process.exit(7);
  const color = args.at(-1).replace('issue-desk-intake-', '');
  let n = Number(fs.existsSync(process.env.RUN_LOG + '.probe') ? fs.readFileSync(process.env.RUN_LOG + '.probe', 'utf8') : 0);
  if (color === 'blue') { n++; fs.writeFileSync(process.env.RUN_LOG + '.probe', String(n)); }
  const active = process.env.RUN_COLORS ? (n > 1 ? 'green' : 'blue') : process.env.ACTIVE_COLOR;
  if (process.env.TRANSITION || (!process.env.EMPTY_ALL && (!process.env.EMPTY_FIRST || n > 1) && (process.env.BOTH_COLORS || color === active))) console.log('intake-' + color);
} else if (args[0] === 'exec') {
  if (args.includes('admin')) fs.writeFileSync(process.env.RUN_LOG + '.exec', JSON.stringify(args));
  if (process.env.TRANSITION) {
    const n = Number(fs.readFileSync(process.env.RUN_LOG + '.probe', 'utf8'));
    const color = args[1].replace('intake-', '');
    if (process.env.TRANSITION === 'stuck') process.exit(11);
    if (n === 1) process.exit(color === 'blue' ? 0 : 11);
    if (n <= 3) process.exit(11);
    process.exit(color === 'green' ? 0 : 10);
  }
  process.exit(Number(process.env.HEALTH_STATUS || 0));
}
else if (args[0] === 'inspect') {
  const t = args[2];
  if (t.includes('.Config.Image')) console.log('worker-image|volume:issue-desk-worker-state|true');
  else if (t.includes('.Image')) console.log('intake-image');
  else if (t.includes('.Config.Env')) {
    if (process.env.INSPECT_FAIL) { console.error('secret-pw'); process.exit(7); }
    const entries = JSON.parse(process.env.ACTIVE_ENV);
    const selected = entries.filter(e => /^(ISSUE_DESK_APP_STORE|ISSUE_DESK_DATABASE_URL|PGHOST|PGUSER|PGDATABASE|PGPASSWORD)=/.test(e));
    for (const entry of selected) console.log(/[\\r\\n]/.test(entry) ? 'INVALID_STORE_ENV' : entry);
    // 実際の docker inspect --format と同じく、出力の最後に空行が1つ付く。
    if (!process.env.INSPECT_NO_TRAILING_BLANK) console.log('');
    for (const extra of JSON.parse(process.env.INSPECT_EXTRA_LINES || '[]')) console.log(extra);
  }
  else process.exit(9);
}else if (args[0] === 'run') {
  const countFile = process.env.RUN_LOG + '.count';
  const count = Number(fs.existsSync(countFile) ? fs.readFileSync(countFile, 'utf8') : 0) + 1;
  fs.writeFileSync(countFile, String(count));
  const i = args.indexOf('--env-file');
  const envFile = i < 0 ? null : args[i + 1];
  const log = { args, envFile, envMode: envFile ? fs.statSync(envFile).mode & 0o777 : null, envText: envFile ? fs.readFileSync(envFile, 'utf8') : null };
  fs.writeFileSync(process.env.RUN_LOG + '.env', JSON.stringify(log));
  fs.writeFileSync(process.env.RUN_LOG, JSON.stringify(args));
  fs.appendFileSync(process.env.RUN_LOG + '.history', JSON.stringify(args) + '\\n');
  if (process.env.RUN_SIGNAL) process.kill(process.ppid, process.env.RUN_SIGNAL);
  process.exit(Number(process.env.RUN_SEQUENCE ? process.env.RUN_SEQUENCE.split(',')[count - 1] || '0' : process.env.RUN_STATUS || 0));
}

else process.exit(9);
`,
  );
  fs.chmodSync(docker, 0o700);
  const log = path.join(dir, 'run.json');
  const env = {
    ...process.env,
    HOME: home,
    PATH: bin + ':' + process.env.PATH,
    RUN_LOG: log,
    ISSUE_DESK_NAS_DIR: nas,
    ACTIVE_ENV: JSON.stringify([...entries, 'PATH=/x']),
    ACTIVE_COLOR: color,
  };
  // チャット用の受付スクリプトの正本（NASでは ~/.issue-desk/scripts-root が指す）
  const source_root = path.join(dir, 'chat-source');
  fs.mkdirSync(path.join(source_root, 'scripts'), { recursive: true });
  fs.mkdirSync(path.join(source_root, 'shared'));
  fs.mkdirSync(path.join(source_root, 'deploy'));
  fs.writeFileSync(path.join(source_root, 'deploy/versions.json'), '{}');
  for (const name of ['desk-client.ts', 'stage-watch.ts'])
    fs.writeFileSync(path.join(source_root, 'scripts', name), '');
  fs.writeFileSync(
    path.join(home, '.issue-desk/scripts-root'),
    source_root + '\n',
  );
  const desk = path.join(home, '.issue-desk/desk');
  return { dir, home, desk, log, env };
}

test('書き手不在の後に出現するまで待ち、確認失敗と複数は待たずに拒否する', () => {
  const f = fixture();
  try {
    const invoke = (extra) =>
      spawnSync('sh', [f.desk, 'desk-get.ts', 'GET', '/api/issue?desk=1'], {
        cwd: f.home,
        env: { ...f.env, ...extra },
        encoding: 'utf8',
      });
    const waited = invoke({ EMPTY_FIRST: '1' });
    assert.equal(waited.status, 0, waited.stderr);
    assert.equal((waited.stderr.match(/交代を待っています/g) || []).length, 1);
    for (const extra of [
      { BOTH_COLORS: '1' },
      { COMPOSE_FAIL: '1' },
      { HEALTH_STATUS: '12' },
    ]) {
      const r = invoke(extra);
      assert.notEqual(r.status, 0);
      assert.doesNotMatch(r.stderr, /交代を待っています/);
    }
  } finally {
    fs.rmSync(f.dir, { recursive: true, force: true });
  }
});

test('待受前の新色を無視して旧色で起動し、接続できない交代中を経て新色で再開する', () => {
  const f = fixture();
  try {
    fs.writeFileSync(path.join(f.dir, 'bin/sleep'), '#!/bin/sh\nexit 0\n', {
      mode: 0o700,
    });
    const r = spawnSync(
      'sh',
      [f.desk, 'desk-get.ts', 'GET', '/api/issue?desk=1'],
      {
        cwd: f.home,
        env: { ...f.env, TRANSITION: '1', RUN_SEQUENCE: '75,0' },
        encoding: 'utf8',
      },
    );
    assert.equal(r.status, 0, r.stderr);
    assert.equal(Number(fs.readFileSync(f.log + '.count', 'utf8')), 2);
    const history = fs
      .readFileSync(f.log + '.history', 'utf8')
      .trim()
      .split('\n')
      .map((line) => JSON.parse(line));
    assert.ok(history[0].includes('container:intake-blue'));
    assert.ok(
      JSON.parse(fs.readFileSync(f.log, 'utf8')).includes(
        'container:intake-green',
      ),
    );
    assert.equal(Number(fs.readFileSync(f.log + '.probe', 'utf8')), 4);
  } finally {
    fs.rmSync(f.dir, { recursive: true, force: true });
  }
});

test('コンテナが存在しても接続できない状態は期限まで待つ', () => {
  const f = fixture();
  try {
    fs.writeFileSync(
      path.join(f.dir, 'bin/date'),
      '#!/bin/sh\nn=0; [ ! -f "$CLOCK_FILE" ] || n=$(cat "$CLOCK_FILE"); n=$((n+1)); echo "$n" >"$CLOCK_FILE"; if [ "$n" -le 2 ]; then echo 100; else echo 281; fi\n',
      { mode: 0o700 },
    );
    fs.writeFileSync(path.join(f.dir, 'bin/sleep'), '#!/bin/sh\nexit 0\n', {
      mode: 0o700,
    });
    const r = spawnSync(
      'sh',
      [f.desk, 'desk-get.ts', 'GET', '/api/issues/search'],
      {
        cwd: f.home,
        env: {
          ...f.env,
          TRANSITION: 'stuck',
          CLOCK_FILE: path.join(f.dir, 'clock'),
        },
        encoding: 'utf8',
      },
    );
    assert.notEqual(r.status, 0);
    assert.match(r.stderr, /待機期限/);
    assert.match(r.stderr, /接続を確認中/);
    assert.ok(!fs.existsSync(f.log));
  } finally {
    fs.rmSync(f.dir, { recursive: true, force: true });
  }
});

test('書き手不在の待機期限を時計を進めて検証する', () => {
  const f = fixture();
  try {
    const bin = path.join(f.dir, 'bin');
    fs.writeFileSync(
      path.join(bin, 'date'),
      '#!/bin/sh\nn=0; [ ! -f "$CLOCK_FILE" ] || n=$(cat "$CLOCK_FILE"); n=$((n+1)); echo "$n" >"$CLOCK_FILE"; if [ "$n" -le 2 ]; then echo 100; else echo 281; fi\n',
      { mode: 0o700 },
    );
    fs.writeFileSync(path.join(bin, 'sleep'), '#!/bin/sh\nexit 0\n', {
      mode: 0o700,
    });
    const r = spawnSync(
      'sh',
      [f.desk, 'desk-get.ts', 'GET', '/api/issue?desk=1'],
      {
        cwd: f.home,
        env: {
          ...f.env,
          EMPTY_ALL: '1',
          CLOCK_FILE: path.join(f.dir, 'clock'),
        },
        encoding: 'utf8',
      },
    );
    assert.notEqual(r.status, 0);
    assert.match(r.stderr, /待機期限/);
    assert.equal((r.stderr.match(/交代を待っています/g) || []).length, 1);
    assert.ok(!fs.existsSync(f.log));
  } finally {
    fs.rmSync(f.dir, { recursive: true, force: true });
  }
});

test('個々の確認と交代後の再起動も期限で打ち切る', () => {
  const a = fixture();
  try {
    fs.writeFileSync(path.join(a.dir, 'bin/timeout'), '#!/bin/sh\nexit 124\n', {
      mode: 0o700,
    });
    const r = spawnSync(
      'sh',
      [a.desk, 'desk-get.ts', 'GET', '/api/issues/search'],
      { cwd: a.home, env: a.env, encoding: 'utf8' },
    );
    assert.notEqual(r.status, 0);
    assert.match(r.stderr, /確認が待機期限/);
    assert.ok(!fs.existsSync(a.log));
  } finally {
    fs.rmSync(a.dir, { recursive: true, force: true });
  }
  const b = fixture();
  try {
    const clock = path.join(b.dir, 'clock');
    fs.writeFileSync(clock, '100');
    fs.writeFileSync(
      path.join(b.dir, 'bin/date'),
      '#!/bin/sh\ncat "$CLOCK_FILE"\n',
      { mode: 0o700 },
    );
    fs.writeFileSync(
      path.join(b.dir, 'bin/sleep'),
      '#!/bin/sh\necho 281 >"$CLOCK_FILE"\n',
      { mode: 0o700 },
    );
    const r = spawnSync(
      'sh',
      [b.desk, 'desk-get.ts', 'GET', '/api/issues/search'],
      {
        cwd: b.home,
        env: { ...b.env, TRANSITION: '1', RUN_STATUS: '75', CLOCK_FILE: clock },
        encoding: 'utf8',
      },
    );
    assert.notEqual(r.status, 0);
    assert.match(r.stderr, /待機期限/);
    assert.match(r.stderr, /交代を待っています/);
    assert.equal(Number(fs.readFileSync(b.log + '.count', 'utf8')), 1);
    assert.deepEqual(
      fs
        .readdirSync(path.join(b.home, '.issue-desk'))
        .filter((x) => x.startsWith('.desk-env.')),
      [],
    );
  } finally {
    fs.rmSync(b.dir, { recursive: true, force: true });
  }
});

test('許可された読取だけ専用終了後に新しい色で起動し、上限で止まる', () => {
  for (const [args, sequence, expected] of [
    [['desk-get.ts', 'GET', '/api/issue?desk=1'], '75,0', 2],
    [['follow-issues.ts', '--desk', '1', '--state', 'HOME_STATE'], '75,0', 2],
    [['desk-get.ts', 'GET', '/api/issues/search?limit=1'], '75,0', 2],
    [['desk-get.ts', 'GET', '/api/issues'], '75,0', 2],
    [['create-issue.ts', '--check', 'abc'], '75,0', 2],
    [['create-issue.ts', '--timeout', '5', '--check', 'abc'], '75,0', 2],
    [['create-issue.ts', '--check-only', '--project', 'x/y'], '75,0', 1],
    [['desk-get.ts', 'POST', '/api/issues'], '75,0', 1],
    [['desk-get.ts', 'GET', '/api/tasks/next?limit=1'], '75,0', 1],
    [
      ['desk-get.ts', 'GET', '/api/issue?desk=1', '--url', 'http://example'],
      '75,0',
      1,
    ],
    [['desk-get.ts', '--unknown', 'x'], '75,0', 1],
    [['desk-get.ts'], '75,0', 1],
    [['follow-issues.ts', '--desk', '1'], '75,0', 1],
    [['desk-get.ts', 'GET', '/api/../x'], '75,0', 1],
    [['desk-get.ts', 'GET', '/api/issues/search'], '75,75,75,75,0', 4],
    [['list-issues.ts', '--duplicates', '--project', 'x/y'], '75,0', 2],
    [['list-issues.ts', '--project', 'x/y'], '75,0', 1],
  ] as [string[], string, number][]) {
    const f = fixture();
    try {
      const r = spawnSync(
        'sh',
        [
          f.desk,
          ...args.map((arg) =>
            arg === 'HOME_STATE' ? path.join(f.home, 'state') : arg,
          ),
        ],
        {
          cwd: f.home,
          env: { ...f.env, RUN_SEQUENCE: sequence, RUN_COLORS: '1' },
          encoding: 'utf8',
        },
      );
      assert.equal(
        Number(fs.readFileSync(f.log + '.count', 'utf8')),
        expected,
        args.join(' '),
      );
      if (expected === 2) {
        assert.equal(r.status, 0, r.stderr);
        assert.ok(
          JSON.parse(fs.readFileSync(f.log, 'utf8')).includes(
            'container:intake-green',
          ),
        );
      } else assert.notEqual(r.status, 0);
      assert.deepEqual(
        fs
          .readdirSync(path.join(f.home, '.issue-desk'))
          .filter((x) => x.startsWith('.desk-env.')),
        [],
      );
    } finally {
      fs.rmSync(f.dir, { recursive: true, force: true });
    }
  }
});

test('通常の終了コードは窓口がそのまま返し、再起動しない', () => {
  for (const status of [0, 1, 2, 3]) {
    const f = fixture();
    try {
      const r = spawnSync(
        'sh',
        [f.desk, 'desk-get.ts', 'GET', '/api/issue?desk=1'],
        {
          cwd: f.home,
          env: { ...f.env, RUN_STATUS: String(status) },
          encoding: 'utf8',
        },
      );
      assert.equal(r.status, status);
      assert.equal(Number(fs.readFileSync(f.log + '.count', 'utf8')), 1);
    } finally {
      fs.rmSync(f.dir, { recursive: true, force: true });
    }
  }
});

test('PCトークン専用スクリプトは稼働WorkerのK2を使い一時キーへ切り替えない', () => {
  const f = fixture();
  try {
    assert.ok(
      fs.statSync(path.join(f.home, '.issue-desk/desk-pc')).mode & 0o100,
      'desk-pc が配置されている',
    );
    for (const script of ['stage-watch.ts']) {
      fs.rmSync(f.log, { force: true });
      const r = spawnSync('sh', [f.desk, script, '--number', '1'], {
        cwd: f.home,
        env: f.env,
        encoding: 'utf8',
      });
      assert.equal(r.status, 0, r.stderr);
      const args = JSON.parse(fs.readFileSync(f.log, 'utf8'));
      assert.ok(
        args.includes('issue-desk-worker-state:/state:ro'),
        'Workerのstateを読み取り専用で使う',
      );
      assert.ok(args.includes('worker-image'));
      assert.ok(
        args.some((a) =>
          String(a).endsWith(
            '/deploy/versions.json:/chat/deploy/versions.json:ro',
          ),
        ),
        'deploy/versions.json をコンテナへ渡す',
      );
      assert.ok(
        !args.includes('issue-desk-intake-state:/state'),
        '一時キー用のstateは使わない',
      );
      assert.ok(
        !args.some((a) => String(a).includes('chat-local.ts')),
        '一時キー接続を使わない',
      );
      assert.equal(args.at(-3), script);
    }
  } finally {
    fs.rmSync(f.dir, { recursive: true, force: true });
  }
});

test('PR以外のスクリプトは従来どおり一時キー窓口のまま', () => {
  const f = fixture();
  try {
    const r = spawnSync(
      'sh',
      [f.desk, 'desk-get.ts', 'GET', '/api/issues/search?limit=1'],
      { cwd: f.home, env: f.env, encoding: 'utf8' },
    );
    assert.equal(r.status, 0, r.stderr);
    const args = JSON.parse(fs.readFileSync(f.log, 'utf8'));
    assert.ok(args.includes('issue-desk-intake-state:/state'));
    assert.ok(
      args.some((a) => String(a).includes('/app/deploy/nas/chat-local.ts')),
    );
    assert.ok(!args.includes('issue-desk-worker-state:/state:ro'));
  } finally {
    fs.rmSync(f.dir, { recursive: true, force: true });
  }
});

test('一時キー窓口は稼働コンテナの保存先の設定を0600の一時ファイルで渡し、終了後に消す', () => {
  const f = fixture();
  try {
    const r = spawnSync(
      'sh',
      [f.desk, 'desk-get.ts', 'GET', '/api/issues/search?limit=1'],
      { cwd: f.home, env: f.env, encoding: 'utf8' },
    );
    assert.equal(r.status, 0, r.stderr);
    const log = JSON.parse(fs.readFileSync(f.log + '.env', 'utf8'));
    assert.ok(log.args.includes('--env-file'));
    assert.equal(log.envMode, 0o600);
    assert.match(log.envText, /^ISSUE_DESK_APP_STORE=postgres$/m);
    assert.match(log.envText, /^PGPASSWORD=secret-pw = with spaces$/m);
    assert.match(log.envText, /^ISSUE_DESK_EXPECTED_APP_STORE=postgres$/m);
    for (const entry of composeStoreEnvironment('blue'))
      assert.ok(log.envText.split('\n').includes(entry));
    assert.equal(path.dirname(log.envFile), path.join(f.home, '.issue-desk'));
    assert.ok(!/^PATH=/m.test(log.envText), '許可した名前だけを渡す');
    assert.ok(
      !log.args.some((a) => String(a).includes('secret-pw')),
      '値を引数に出さない',
    );
    assert.ok(
      !(r.stdout + r.stderr).includes('secret-pw'),
      '秘密をログに出さない',
    );
    assert.ok(!fs.existsSync(log.envFile), '終了後に一時ファイルを消す');
  } finally {
    fs.rmSync(f.dir, { recursive: true, force: true });
  }
});

test('greenの配備構成も稼働書き手から取得し、失敗・割込みでも一時ファイルを消す', () => {
  const f = fixture({
    entries: composeStoreEnvironment('green'),
    color: 'green',
  });
  try {
    for (const extra of [
      { RUN_STATUS: '7' },
      { RUN_SIGNAL: 'SIGTERM' },
      { RUN_SIGNAL: 'SIGINT' },
      { RUN_SIGNAL: 'SIGHUP' },
    ]) {
      const r = spawnSync('sh', [f.desk, 'desk-get.ts'], {
        cwd: f.home,
        env: { ...f.env, ...extra },
        encoding: 'utf8',
      });
      assert.notEqual(r.status, 0);
      const log = JSON.parse(fs.readFileSync(f.log + '.env', 'utf8'));
      assert.ok(log.args.includes('container:intake-green'));
      for (const entry of composeStoreEnvironment('green'))
        assert.ok(log.envText.split('\n').includes(entry));
      assert.ok(!fs.existsSync(log.envFile));
      assert.ok(
        !(r.stdout + r.stderr + JSON.stringify(log.args)).includes('secret-pw'),
      );
    }
  } finally {
    fs.rmSync(f.dir, { recursive: true, force: true });
  }
});

test('管理CLIの同種入口は稼働書き手のコンテナ内で環境を引き継ぐ', () => {
  for (const source of [
    'deploy/clients/bootstrap-ubuntu-client.sh',
    'deploy/clients/bootstrap-ubuntu-hostd.sh',
    'deploy/nas/update-worker-release.sh',
  ]) {
    assert.ok(
      /intake\.sh"? exec -T \/usr\/local\/bin\/intake admin/.test(
        fs.readFileSync(source, 'utf8'),
      ),
      source,
    );
  }
  const f = fixture({ color: 'green' });
  try {
    const args = [
      '/usr/local/bin/intake',
      'admin',
      '--config',
      '/state/config.local.json',
      '--application-dir',
      '/app',
      'pc-token',
      'list',
    ];
    const r = spawnSync(
      'sh',
      [
        path.join(f.env.ISSUE_DESK_NAS_DIR, 'deploy/nas/intake.sh'),
        'exec',
        '-T',
        ...args,
      ],
      { env: f.env, encoding: 'utf8' },
    );
    assert.equal(r.status, 0, r.stderr);
    assert.deepEqual(JSON.parse(fs.readFileSync(f.log + '.exec', 'utf8')), [
      'exec',
      '-i',
      'intake-green',
      ...args,
    ]);
    assert.ok(!fs.existsSync(f.log), '環境を転送する別コンテナを作らない');
  } finally {
    fs.rmSync(f.dir, { recursive: true, force: true });
  }
});

test('稼働受付がSQLiteまたは保存先未設定でも一時キー窓口の実行経路を保つ', () => {
  for (const entries of [
    [],
    ['ISSUE_DESK_APP_STORE=sqlite'],
    ['ISSUE_DESK_APP_STORE='],
  ]) {
    const f = fixture({ entries });
    try {
      const r = spawnSync(
        'sh',
        [f.desk, 'desk-get.ts', 'GET', '/api/issue?desk=1'],
        { cwd: f.home, env: f.env, encoding: 'utf8' },
      );
      assert.equal(r.status, 0, r.stderr);
      const log = JSON.parse(fs.readFileSync(f.log + '.env', 'utf8'));
      assert.match(log.envText, /^ISSUE_DESK_EXPECTED_APP_STORE=sqlite$/m);
      assert.ok(log.args.includes('container:intake-blue'));
      assert.ok(log.args.includes('issue-desk-intake-state:/state'));
      assert.ok(
        log.args.some((a) =>
          String(a).includes('/app/deploy/nas/chat-local.ts'),
        ),
      );
      assert.ok(!fs.existsSync(log.envFile));
    } finally {
      fs.rmSync(f.dir, { recursive: true, force: true });
    }
  }
});

test('保存先の環境の読取失敗・接続先欠落・不正な転送値ではDocker runを開始しない', () => {
  const cases = [
    {
      entries: ['ISSUE_DESK_APP_STORE=postgres'],
      message: /PostgreSQL.*ISSUE_DESK_DATABASE_URL/,
    },
    {
      entries: ['ISSUE_DESK_APP_STORE=postgres', 'ISSUE_DESK_DATABASE_URL=   '],
      message: /PostgreSQL.*ISSUE_DESK_DATABASE_URL/,
    },
    {
      entries: composeStoreEnvironment('blue'),
      extra: { INSPECT_FAIL: '1' },
      message: /保存先の環境を読めない/,
    },
    {
      entries: ['ISSUE_DESK_APP_STORE=unknown-secret-pw'],
      message: /環境が不正/,
    },
    {
      entries: ['ISSUE_DESK_APP_STORE=postgres', 'ISSUE_DESK_APP_STORE=sqlite'],
      message: /環境が不正/,
    },
    {
      entries: [
        ...composeStoreEnvironment('blue'),
        'PGPASSWORD=secret-pw\nPGHOST=other',
      ],
      message: /環境が不正/,
    },
    {
      entries: [...composeStoreEnvironment('blue'), 'PGPASSWORD=secret-pw\r'],
      message: /環境が不正/,
    },
  ];
  for (const { entries, extra, message } of cases) {
    const f = fixture({ entries });
    try {
      const r = spawnSync('sh', [f.desk, 'desk-get.ts'], {
        cwd: f.home,
        env: { ...f.env, ...extra },
        encoding: 'utf8',
      });
      assert.notEqual(r.status, 0);
      assert.match(r.stderr, message);
      assert.ok(!fs.existsSync(f.log), 'docker runを起動しない');
      assert.ok(
        !fs
          .readdirSync(path.join(f.home, '.issue-desk'))
          .some((n) => n.startsWith('.desk-env.')),
      );
      assert.ok(!(r.stdout + r.stderr).includes('secret-pw'));
    } finally {
      fs.rmSync(f.dir, { recursive: true, force: true });
    }
  }
});

test('inspectの末尾の空行1つは許し、途中の空行・=の無い行・重複名は拒否する', () => {
  const ok = fixture();
  try {
    const r = spawnSync('sh', [ok.desk, 'desk-get.ts'], {
      cwd: ok.home,
      env: ok.env,
      encoding: 'utf8',
    });
    assert.doesNotMatch(r.stderr, /環境が不正/);
    assert.ok(fs.existsSync(ok.log), '末尾の空行つきでもdocker runまで進む');
  } finally {
    fs.rmSync(ok.dir, { recursive: true, force: true });
  }
  const bad = [
    { name: '末尾の空行が2つ', extra: [''] },
    { name: '=の無い行', extra: ['NOEQUALS-secret-pw'] },
    { name: '空行の後に行が続く', extra: ['', 'PGHOST=x'] },
    { name: '重複名', extra: ['PGHOST=again'] },
  ];
  for (const { name, extra } of bad) {
    const f = fixture();
    try {
      const r = spawnSync('sh', [f.desk, 'desk-get.ts'], {
        cwd: f.home,
        env: { ...f.env, INSPECT_EXTRA_LINES: JSON.stringify(extra) },
        encoding: 'utf8',
      });
      assert.notEqual(r.status, 0, name);
      assert.match(r.stderr, /環境が不正/, name);
      assert.ok(!fs.existsSync(f.log), name + ': docker runを起動しない');
      assert.ok(!(r.stdout + r.stderr).includes('secret-pw'), name);
    } finally {
      fs.rmSync(f.dir, { recursive: true, force: true });
    }
  }
  // 途中の空行（最後に空行が付く出力でも、その前に空行がある）
  const mid = fixture({ entries: composeStoreEnvironment('blue') });
  try {
    const r = spawnSync('sh', [mid.desk, 'desk-get.ts'], {
      cwd: mid.home,
      env: { ...mid.env, INSPECT_EXTRA_LINES: JSON.stringify(['', 'x']) },
      encoding: 'utf8',
    });
    assert.notEqual(r.status, 0);
  } finally {
    fs.rmSync(mid.dir, { recursive: true, force: true });
  }
});

test('PCトークン窓口が無いときはPR作成・マージを実行しない', () => {
  const f = fixture();
  try {
    fs.rmSync(path.join(f.home, '.issue-desk/desk-pc'));
    const r = spawnSync('sh', [f.desk, 'stage-watch.ts'], {
      cwd: f.home,
      env: f.env,
      encoding: 'utf8',
    });
    assert.notEqual(r.status, 0);
    assert.ok(!fs.existsSync(f.log), 'dockerを起動していない');
  } finally {
    fs.rmSync(f.dir, { recursive: true, force: true });
  }
});

test('正本に deploy/versions.json が無いときはPR作成・マージを実行しない', () => {
  const f = fixture();
  try {
    const source = fs
      .readFileSync(path.join(f.home, '.issue-desk/scripts-root'), 'utf8')
      .trim();
    fs.rmSync(path.join(source, 'deploy/versions.json'));
    const r = spawnSync('sh', [f.desk, 'stage-watch.ts'], {
      cwd: f.home,
      env: f.env,
      encoding: 'utf8',
    });
    assert.notEqual(r.status, 0);
    assert.match(r.stderr, /deploy\/versions\.json/);
    assert.ok(!fs.existsSync(f.log), 'dockerを起動していない');
  } finally {
    fs.rmSync(f.dir, { recursive: true, force: true });
  }
});

test('窓口の配置は繰り返しても変わらない', () => {
  const f = fixture();
  try {
    const before = fs.readFileSync(
      path.join(f.home, '.issue-desk/desk-pc'),
      'utf8',
    );
    const source = path.resolve('deploy/nas/update-desk-access.sh');
    const again = spawnSync(
      'sh',
      [
        '-c',
        '. "$1"; say() { echo "$@"; }; plan() { echo "$@"; }; desk_access',
        'sh',
        source,
      ],
      {
        env: {
          ...process.env,
          HOME: f.home,
          DIR: path.resolve('.'),
          DRY_RUN: '0',
        },
        encoding: 'utf8',
      },
    );
    assert.equal(again.status, 0, again.stderr);
    assert.equal(again.stdout.trim(), '', '内容が同じなら書き直さない');
    assert.equal(
      fs.readFileSync(path.join(f.home, '.issue-desk/desk-pc'), 'utf8'),
      before,
    );
  } finally {
    fs.rmSync(f.dir, { recursive: true, force: true });
  }
});

const routes = JSON.parse(
  fs.readFileSync('deploy/nas/desk-pc-routes.json', 'utf8'),
);
test('一覧の各経路と条件付き経路を実際の窓口で振り分ける', () => {
  const f = fixture();
  try {
    const pc = path.join(f.home, '.issue-desk/desk-pc');
    fs.writeFileSync(pc, '#!/bin/sh\nprintf "%s\\n" "$@" > "$ROUTE_LOG"\n');
    fs.chmodSync(pc, 0o700);
    const log = path.join(f.dir, 'route.log');
    const env = { ...f.env, ROUTE_LOG: log };
    for (const entry of routes) {
      const [script, ...flags] = entry.split('|');
      for (const flag of flags.length ? flags : [null]) {
        fs.rmSync(log, { force: true });
        const args = flag ? [script, flag, 'value'] : [script, 'value'];
        const r = spawnSync('sh', [f.desk, ...args], {
          cwd: f.home,
          env,
          encoding: 'utf8',
        });
        assert.equal(r.status, 0, `${entry}: ${r.stderr}`);
        assert.deepEqual(fs.readFileSync(log, 'utf8').trim().split('\n'), args);
      }
    }
    for (const script of ['reply-issue.ts']) {
      const r = spawnSync('sh', [f.desk, script, '--number', '1'], {
        cwd: f.home,
        env,
        encoding: 'utf8',
      });
      assert.equal(r.status, 0, r.stderr);
      assert.ok(
        JSON.parse(fs.readFileSync(f.log, 'utf8')).some((a) =>
          String(a).includes('/app/deploy/nas/chat-local.ts'),
        ),
      );
      for (const flag of [
        '--over-limit-statement',
        '--implementation-limit-retry',
        '--approval-question',
        '--approval-answer',
      ]) {
        const args = [script, '--number', '1', `${flag}=value`];
        const matched = spawnSync('sh', [f.desk, ...args], {
          cwd: f.home,
          env,
          encoding: 'utf8',
        });
        assert.equal(matched.status, 0, matched.stderr);
        assert.deepEqual(fs.readFileSync(log, 'utf8').trim().split('\n'), args);
      }
    }
    fs.rmSync(log, { force: true });
    const nonCommand = spawnSync(
      'sh',
      [f.desk, 'desk-ops.ts', 'other', 'notice'],
      { cwd: f.home, env, encoding: 'utf8' },
    );
    assert.equal(nonCommand.status, 0, nonCommand.stderr);
    assert.ok(!fs.existsSync(log), '第1引数以外の notice は振り分けない');
    assert.ok(
      JSON.parse(fs.readFileSync(f.log, 'utf8')).some((a) =>
        String(a).includes('/app/deploy/nas/chat-local.ts'),
      ),
    );
    fs.rmSync(pc);
    for (const entry of routes) {
      fs.rmSync(f.log, { force: true });
      const [script, flag] = entry.split('|');
      const r = spawnSync('sh', [f.desk, script, ...(flag ? [flag] : [])], {
        cwd: f.home,
        env,
        encoding: 'utf8',
      });
      assert.notEqual(r.status, 0, entry);
      assert.ok(!fs.existsSync(f.log), entry);
    }
  } finally {
    fs.rmSync(f.dir, { recursive: true, force: true });
  }
});

test('窓口は .ts の名前の入口だけを受け付け、改名前の .mjs の名前はPC経路にもコンテナにも渡さず断る', () => {
  const f = fixture();
  try {
    const pc = path.join(f.home, '.issue-desk/desk-pc');
    fs.writeFileSync(pc, '#!/bin/sh\nprintf "%s\\n" "$@" > "$ROUTE_LOG"\n');
    fs.chmodSync(pc, 0o700);
    const log = path.join(f.dir, 'route.log');
    const env = { ...f.env, ROUTE_LOG: log };
    const old = (name: string) => name.replace(/\.ts$/, '.mjs');
    for (const args of [
      ...routes.map((entry) => {
        const [script, flag] = entry.split('|');
        return [old(script), ...(flag ? [flag] : []), 'value'];
      }),
      ['desk-get.mjs', 'GET', '/api/issues'],
      ['create-issue.mjs', '--check', 'abc'],
      [
        'follow-issues.mjs',
        '--desk',
        '1',
        '--state',
        path.join(f.home, 'state'),
      ],
    ]) {
      const r = spawnSync('sh', [f.desk, ...args], {
        cwd: f.home,
        env,
        encoding: 'utf8',
      });
      assert.equal(r.status, 2, `${args[0]}: ${r.stderr}`);
      assert.match(r.stderr, /受付スクリプト名\.tsを指定してください/);
      assert.ok(
        !fs.existsSync(log),
        `${args[0]} をPCトークン窓口へ振り分けない`,
      );
      assert.ok(!fs.existsSync(f.log), `${args[0]} でコンテナを起動しない`);
    }
    // 一時キー窓口のコンテナ内の sh は、指定の入口をそのまま、イメージの chat-local.ts を読み込み役にして動かす。
    const r = spawnSync('sh', [f.desk, 'desk-get.ts', 'GET', '/api/issues'], {
      cwd: f.home,
      env,
      encoding: 'utf8',
    });
    assert.equal(r.status, 0, r.stderr);
    const args = JSON.parse(fs.readFileSync(f.log, 'utf8'));
    const inner = args[args.indexOf('-c') + 1];
    assert.equal(args[args.indexOf('-c') + 3], '/app/scripts/desk-get.ts');
    const bin = path.join(f.dir, 'inner-bin');
    fs.mkdirSync(bin);
    fs.writeFileSync(
      path.join(bin, 'node'),
      '#!/bin/sh\nprintf "%s\\n" "$@"\n',
      { mode: 0o700 },
    );
    const ran = spawnSync(
      'sh',
      ['-c', inner, 'sh', '/app/scripts/desk-get.ts', 'GET', '/api/issues'],
      {
        env: { ...process.env, PATH: bin + ':' + process.env.PATH },
        encoding: 'utf8',
      },
    );
    assert.deepEqual(ran.stdout.trim().split('\n'), [
      '--import',
      '/app/deploy/nas/chat-local.ts',
      '/app/scripts/desk-get.ts',
      'GET',
      '/api/issues',
    ]);
  } finally {
    fs.rmSync(f.dir, { recursive: true, force: true });
  }
});
