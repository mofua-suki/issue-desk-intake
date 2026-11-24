import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import http from 'node:http';
import { spawn } from 'node:child_process';

async function fixture(handler) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'nas-handoff-'));
  const events = [];
  const server = http.createServer((req, res) => {
    events.push({
      method: req.method,
      url: req.url,
      key: req.headers['x-desk-key'],
    });
    handler(req, res);
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const config = path.join(dir, 'config.json');
  fs.writeFileSync(
    config,
    JSON.stringify({
      workerId: 'nas',
      intake: {
        port: (server.address() as import('node:net').AddressInfo).port,
      },
    }),
  );
  const binary = path.join(dir, 'intake');
  const log = path.join(dir, 'keys');
  fs.writeFileSync(
    binary,
    `#!/bin/sh\ncase "$*" in *'cli-key issue') echo issue >>"$KEY_LOG"; printf '%043d\\n' 0;; *'cli-key revoke') cat >/dev/null; echo revoke >>"$KEY_LOG";; *) exit 1;; esac\n`,
    { mode: 0o700 },
  );
  const env = {
    ...process.env,
    ISSUE_DESK_CONFIG: config,
    ISSUE_DESK_INTAKE_BINARY: binary,
    ISSUE_DESK_NAS_HANDOFF: 'read',
    KEY_LOG: log,
  };
  const run = (args, extra = {}) =>
    new Promise<any>((resolve) => {
      const child = spawn(
        process.execPath,
        ['--import', './deploy/nas/chat-local.ts', ...args],
        { env: { ...env, ...extra } },
      );
      let stdout = '',
        stderr = '';
      child.stdout.on('data', (b) => {
        stdout += b;
      });
      child.stderr.on('data', (b) => {
        stderr += b;
      });
      child.on('close', (code) => resolve({ code, stdout, stderr }));
    });
  return {
    events,
    run,
    log,
    config,
    binary,
    close: async () => {
      await new Promise<any>((resolve) => server.close(resolve));
      fs.rmSync(dir, { recursive: true, force: true });
    },
  };
}

test('実通信とdesk-getを通した一時503は再試行後に専用終了し、毎回キーを回収する', async () => {
  const f = await fixture((_req, res) => {
    res.writeHead(503, { 'content-type': 'application/json' });
    res.end('{"code":"TEMPORARY_UNAVAILABLE"}');
  });
  try {
    const r = await f.run(['scripts/desk-get.ts', 'GET', '/api/issues']);
    assert.equal(r.code, 75, r.stderr);
    assert.equal(f.events.length, 4);
    assert.ok(
      f.events.every((e) => e.method === 'GET' && e.key === '0'.repeat(43)),
    );
    assert.deepEqual(
      fs.readFileSync(f.log, 'utf8').trim().split('\n'),
      Array(4).fill(['issue', 'revoke']).flat(),
    );
    assert.ok(!r.stdout.includes('0'.repeat(43)));
  } finally {
    await f.close();
  }
});

test('再試行対象外のGETとPOSTは再送せず専用終了にしない', async () => {
  const f = await fixture((_req, res) => {
    res.writeHead(503, { 'content-type': 'application/json' });
    res.end('{"code":"TEMPORARY_UNAVAILABLE"}');
  });
  try {
    const get = await f.run(['scripts/desk-get.ts', 'GET', '/api/next']);
    assert.equal(get.code, 1);
    assert.equal(f.events.length, 1);
    const post = await f.run([
      '--input-type=module',
      '-e',
      `try { await globalThis.issueDeskLocal().call('POST', '/api/issues', {text:'hello'}); } catch (e) { process.exitCode = e.nasHandoff ? 75 : 1; }`,
    ]);
    assert.equal(post.code, 1);
    assert.equal(f.events.length, 2);
    assert.deepEqual(fs.readFileSync(f.log, 'utf8').trim().split('\n'), [
      'issue',
      'revoke',
      'issue',
      'revoke',
    ]);
  } finally {
    await f.close();
  }
});

test('503の再試行禁止codeと認証失敗は専用終了にしない', async () => {
  for (const [status, body] of [
    [503, '{"code":"FORBIDDEN"}'],
    [403, '{"code":"FORBIDDEN"}'],
  ]) {
    const f = await fixture((_req, res) => {
      res.writeHead(status, { 'content-type': 'application/json' });
      res.end(body);
    });
    try {
      const r = await f.run(['scripts/desk-get.ts', 'GET', '/api/issues']);
      assert.equal(r.code, 1);
      assert.equal(f.events.length, 1);
      assert.deepEqual(fs.readFileSync(f.log, 'utf8').trim().split('\n'), [
        'issue',
        'revoke',
      ]);
    } finally {
      await f.close();
    }
  }
});

test('送信後の接続喪失は下書きIDとcheckを案内しPOSTを繰り返さない', async () => {
  const { consult } = await import('../scripts/desk-client.ts');
  const calls = [],
    lines = [];
  const oldLog = console.log,
    oldMode = process.env.ISSUE_DESK_NAS_HANDOFF;
  process.env.ISSUE_DESK_NAS_HANDOFF = 'notice';
  console.log = (line) => lines.push(line);
  try {
    const code = await consult(
      async (method, route, payload) => {
        calls.push({ method, route, payload });
        if (method === 'POST') return { id: 'draft-123' };
        throw Object.assign(Error('接続喪失'), {
          nasHandoff: true,
          unreachable: true,
        });
      },
      { project: 'owner/repo', provider: 'codex', text: '依頼' },
      'scripts/create-issue.ts',
      '',
      0,
    );
    assert.equal(code, 2);
    assert.equal(calls.filter((c) => c.method === 'POST').length, 1);
    assert.match(lines.join('\n'), /受付済み/);
    assert.match(lines.join('\n'), /--check draft-123/);
  } finally {
    console.log = oldLog;
    if (oldMode === undefined) delete process.env.ISSUE_DESK_NAS_HANDOFF;
    else process.env.ISSUE_DESK_NAS_HANDOFF = oldMode;
  }
});

test('checkの再開時に元の期限が過ぎていれば読取を開始しない', async () => {
  const { checkDraft } = await import('../scripts/desk-client.ts');
  const oldLog = console.log,
    oldMode = process.env.ISSUE_DESK_NAS_HANDOFF,
    oldStart = process.env.ISSUE_DESK_NAS_STARTED,
    oldRestarts = process.env.ISSUE_DESK_NAS_RESTARTS;
  process.env.ISSUE_DESK_NAS_HANDOFF = 'read';
  process.env.ISSUE_DESK_NAS_STARTED = '1';
  process.env.ISSUE_DESK_NAS_RESTARTS = '1';
  console.log = () => {};
  try {
    let count = 0;
    assert.equal(
      await checkDraft(
        async () => {
          count++;
        },
        { check: 'abc', timeout: '1' },
        'scripts/create-issue.ts',
        '',
        0,
      ),
      2,
    );
    assert.equal(count, 0);
  } finally {
    console.log = oldLog;
    if (oldMode === undefined) delete process.env.ISSUE_DESK_NAS_HANDOFF;
    else process.env.ISSUE_DESK_NAS_HANDOFF = oldMode;
    if (oldStart === undefined) delete process.env.ISSUE_DESK_NAS_STARTED;
    else process.env.ISSUE_DESK_NAS_STARTED = oldStart;
    if (oldRestarts === undefined) delete process.env.ISSUE_DESK_NAS_RESTARTS;
    else process.env.ISSUE_DESK_NAS_RESTARTS = oldRestarts;
  }
});

async function fullHandoff(failure) {
  let blueReads = 0;
  const blue = await fixture((_req, res) => {
    blueReads++;
    res.setHeader('content-type', 'application/json');
    if (blueReads === 1)
      res.end(
        JSON.stringify({
          desk: 7,
          title: '作業',
          state: 'open',
          status: 'review_held',
          updatedAt: 'one',
        }),
      );
    else {
      res.statusCode = 503;
      res.end('{"code":"TEMPORARY_UNAVAILABLE"}');
    }
  });
  const green = await fixture((_req, res) => {
    res.setHeader('content-type', 'application/json');
    res.end(
      JSON.stringify({
        desk: 7,
        title: '作業',
        state: 'closed',
        status: 'completed',
        updatedAt: 'two',
      }),
    );
  });
  if (failure !== '503')
    fs.writeFileSync(
      blue.binary,
      `#!/bin/sh\ncase "$*" in *'cli-key issue') echo issue >>"$KEY_LOG"; if [ -f "$KEY_LOG.once" ]; then echo '受付のPostgreSQLを開けません' >&2; exit 1; fi; touch "$KEY_LOG.once"; printf '%043d\\n' 0;; *'cli-key revoke') cat >/dev/null; echo revoke >>"$KEY_LOG";; esac\n`,
      { mode: 0o700 },
    );
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'nas-full-handoff-'));
  const home = path.join(dir, 'home'),
    bin = path.join(dir, 'bin'),
    nas = path.join(dir, 'nas');
  fs.mkdirSync(home);
  fs.mkdirSync(bin);
  fs.mkdirSync(path.join(nas, 'deploy/nas'), { recursive: true });
  fs.copyFileSync(
    'deploy/nas/intake.sh',
    path.join(nas, 'deploy/nas/intake.sh'),
  );
  const { spawnSync } = await import('node:child_process');
  const install = spawnSync(
    'sh',
    [
      '-c',
      '. "$1"; say() { :; }; plan() { :; }; desk_access',
      'sh',
      path.resolve('deploy/nas/update-desk-access.sh'),
    ],
    {
      env: { ...process.env, HOME: home, DIR: path.resolve('.'), DRY_RUN: '0' },
      encoding: 'utf8',
    },
  );
  assert.equal(install.status, 0, install.stderr);
  fs.writeFileSync(
    path.join(bin, 'docker'),
    `#!${process.execPath}
const fs = require('node:fs'); const { spawnSync } = require('node:child_process');
const a = process.argv.slice(2), state = process.env.COLOR_LOG;
if (a[0] === 'compose') { const color = fs.existsSync(state) ? 'green' : 'blue'; if (a.at(-1) === 'issue-desk-intake-' + color) console.log('intake-' + color); }
else if (a[0] === 'exec') process.exit(0);
else if (a[0] === 'inspect') { if (a[2].includes('.Config.Env')) console.log('ISSUE_DESK_APP_STORE=postgres\\nISSUE_DESK_DATABASE_URL=fixture\\n'); else console.log('image'); }
else if (a[0] === 'run') {
 const network = a[a.indexOf('--network') + 1]; const green = network.endsWith('green');
 fs.appendFileSync(state, network + '\\n');
 const env = {...process.env};
 for (let i=0; i<a.length; i++) if (a[i] === '-e') { const j=a[i+1].indexOf('='); if (j > 0) env[a[i+1].slice(0,j)] = a[i+1].slice(j+1); }
 env.ISSUE_DESK_CONFIG = green ? process.env.GREEN_CONFIG : process.env.BLUE_CONFIG;
 env.ISSUE_DESK_INTAKE_BINARY = green ? process.env.GREEN_BINARY : process.env.BLUE_BINARY;
 env.KEY_LOG = green ? process.env.GREEN_KEYS : process.env.BLUE_KEYS;
 const c = a.findIndex((x, k) => x === '-c' && String(a[k + 1]).includes('/app/deploy/nas/chat-local.ts')); const i = c + 2;
 const result = spawnSync(process.execPath, ['--import', process.env.ROOT + '/deploy/nas/chat-local.ts', process.env.ROOT + a[i+1].slice(4), ...a.slice(i+2)], { env, stdio: 'inherit' });
 process.exit(result.status ?? 1);
} else process.exit(1);
`,
    { mode: 0o700 },
  );
  const env: NodeJS.ProcessEnv = {
    ...process.env,
    HOME: home,
    PATH: bin + ':' + process.env.PATH,
    ISSUE_DESK_NAS_DIR: nas,
    ROOT: path.resolve('.'),
    COLOR_LOG: path.join(dir, 'colors'),
  };
  // フィクスチャの一時設定を窓口の子から使う。
  Object.assign(env, {
    BLUE_CONFIG: blue.config,
    GREEN_CONFIG: green.config,
    BLUE_BINARY: blue.binary,
    GREEN_BINARY: green.binary,
    BLUE_KEYS: blue.log,
    GREEN_KEYS: green.log,
  });
  if (failure.includes('期限超過')) {
    fs.writeFileSync(
      path.join(bin, 'date'),
      '#!/bin/sh\nn=0; [ ! -f "$CLOCK_FILE" ] || n=$(cat "$CLOCK_FILE"); n=$((n+1)); echo "$n" >"$CLOCK_FILE"; now=$(/bin/date +%s); if [ "$n" -le 5 ]; then echo "$now"; else echo $((now+181)); fi\n',
      { mode: 0o700 },
    );
    env.CLOCK_FILE = path.join(dir, 'clock');
  }
  try {
    const state = path.join(home, 'state.json');
    const child = spawn(
      'sh',
      [
        path.join(home, '.issue-desk/desk'),
        'follow-issues.ts',
        '--desk',
        '7',
        '--state',
        state,
        '--interval',
        '0.001',
        '--run',
        '1',
      ],
      { cwd: home, env },
    );
    let stdout = '',
      stderr = '';
    child.stdout.on('data', (b) => (stdout += b));
    child.stderr.on('data', (b) => (stderr += b));
    const status = await new Promise<any>((resolve) =>
      child.on('close', resolve),
    );
    if (failure.includes('期限超過')) {
      assert.notEqual(status, 0);
      assert.match(stderr, /待機期限/);
      assert.equal((stdout.match(/【要対処】共通番号7/g) || []).length, 1);
      assert.deepEqual(
        fs.readFileSync(env.COLOR_LOG, 'utf8').trim().split('\n'),
        ['container:intake-blue'],
      );
      assert.equal(blueReads, 1);
      assert.equal(green.events.length, 0);
      assert.ok(fs.existsSync(state));
      assert.deepEqual(
        fs
          .readdirSync(path.join(home, '.issue-desk'))
          .filter((x) => x.startsWith('.desk-env.')),
        [],
      );
      return;
    }
    assert.equal(status, 0, stderr);
    assert.equal((stdout.match(/【要対処】共通番号7/g) || []).length, 1);
    assert.match(stdout, /【完了】共通番号7/);
    assert.deepEqual(
      fs.readFileSync(env.COLOR_LOG, 'utf8').trim().split('\n'),
      ['container:intake-blue', 'container:intake-green'],
    );
    assert.equal(blueReads, failure === '503' ? 5 : 1);
    assert.equal(green.events.length, 1);
    for (const f of [blue, green]) {
      const keys = fs.readFileSync(f.log, 'utf8').trim().split('\n');
      assert.equal(
        keys.filter((x) => x === 'issue').length,
        keys.filter((x) => x === 'revoke').length +
          (f === blue && failure === 'キー発行失敗' ? 1 : 0),
      );
    }
    assert.equal(
      JSON.parse(fs.readFileSync(state, 'utf8')).issues['7'].done,
      true,
    );
    assert.deepEqual(
      fs
        .readdirSync(path.join(home, '.issue-desk'))
        .filter((x) => x.startsWith('.desk-env.')),
      [],
    );
  } finally {
    await blue.close();
    await green.close();
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

test('窓口から503を経て新色で通知を再開する', () => fullHandoff('503'));
test('窓口からキー発行失敗を経て新色で通知を再開する', () =>
  fullHandoff('キー発行失敗'));
test('窓口はキー発行失敗後の期限超過で終了する', () =>
  fullHandoff('キー発行失敗後の期限超過'));

async function restartWindow(durations) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'nas-restart-window-'));
  const home = path.join(dir, 'home'),
    bin = path.join(dir, 'bin'),
    nas = path.join(dir, 'nas');
  fs.mkdirSync(home);
  fs.mkdirSync(bin);
  fs.mkdirSync(path.join(nas, 'deploy/nas'), { recursive: true });
  fs.copyFileSync(
    'deploy/nas/intake.sh',
    path.join(nas, 'deploy/nas/intake.sh'),
  );
  const { spawnSync } = await import('node:child_process');
  const install = spawnSync(
    'sh',
    [
      '-c',
      '. "$1"; say() { :; }; plan() { :; }; desk_access',
      'sh',
      path.resolve('deploy/nas/update-desk-access.sh'),
    ],
    {
      env: { ...process.env, HOME: home, DIR: path.resolve('.'), DRY_RUN: '0' },
      encoding: 'utf8',
    },
  );
  assert.equal(install.status, 0, install.stderr);
  fs.writeFileSync(path.join(bin, 'date'), '#!/bin/sh\ncat "$CLOCK_FILE"\n', {
    mode: 0o700,
  });
  fs.writeFileSync(
    path.join(bin, 'docker'),
    `#!${process.execPath}
const fs = require('node:fs'); const a = process.argv.slice(2);
if (a[0] === 'compose') { if (a.at(-1).endsWith('-blue')) console.log('intake-blue'); }
else if (a[0] === 'exec') process.exit(0);
else if (a[0] === 'inspect') { if (a[2].includes('.Config.Env')) console.log('ISSUE_DESK_APP_STORE=postgres\\nISSUE_DESK_DATABASE_URL=fixture\\n'); else console.log('image'); }
else if (a[0] === 'run') {
 const env = {}; for (let i=0; i<a.length; i++) if (a[i] === '-e') { const j=a[i+1].indexOf('='); if (j>0) env[a[i+1].slice(0,j)] = a[i+1].slice(j+1); }
 const count = fs.existsSync(process.env.RUN_LOG) ? fs.readFileSync(process.env.RUN_LOG, 'utf8').trim().split('\\n').length : 0;
 fs.appendFileSync(process.env.RUN_LOG, JSON.stringify(env) + '\\n');
 const durations = JSON.parse(process.env.DURATIONS);
 fs.writeFileSync(process.env.CLOCK_FILE, String(Number(fs.readFileSync(process.env.CLOCK_FILE, 'utf8')) + (durations[count] || 0)));
 process.exit(count < durations.length ? 75 : 0);
} else process.exit(1);
`,
    { mode: 0o700 },
  );
  const env = {
    ...process.env,
    HOME: home,
    PATH: bin + ':' + process.env.PATH,
    ISSUE_DESK_NAS_DIR: nas,
    CLOCK_FILE: path.join(dir, 'clock'),
    RUN_LOG: path.join(dir, 'runs'),
    DURATIONS: JSON.stringify(durations),
  };
  fs.writeFileSync(env.CLOCK_FILE, '1000');
  try {
    const child = spawn(
      'sh',
      [
        path.join(home, '.issue-desk/desk'),
        'follow-issues.ts',
        '--desk',
        '7',
        '--state',
        path.join(home, 'state.json'),
      ],
      { env, cwd: home },
    );
    let stderr = '';
    child.stderr.on('data', (b) => {
      stderr += b;
    });
    const code = await new Promise<any>((resolve) =>
      child.on('close', resolve),
    );
    assert.ok(fs.existsSync(env.RUN_LOG), `子が起動しません: ${stderr}`);
    const runs = fs
      .readFileSync(env.RUN_LOG, 'utf8')
      .trim()
      .split('\n')
      .map((line) => JSON.parse(line));
    return { code, stderr, runs };
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

test('2回の切り替えを180秒超の間隔でまたぎ、期限と回数を数え直す', async () => {
  const r = await restartWindow([0, 181]);
  assert.equal(r.code, 0, r.stderr);
  assert.equal(r.runs.length, 3);
  assert.deepEqual(
    r.runs.map((x) => x.ISSUE_DESK_NAS_RESTARTS),
    ['0', '1', '2'],
  );
  assert.deepEqual(
    r.runs.map((x) => x.ISSUE_DESK_NAS_STARTED),
    ['1000', '1000', '1000'],
  );
  const many = await restartWindow([0, 0, 0, 181, 0, 0]);
  assert.equal(many.code, 0, many.stderr);
  assert.equal(many.runs.length, 7);
  assert.ok(
    many.runs.slice(1).every((x) => Number(x.ISSUE_DESK_NAS_RESTARTS) > 0),
  );
  assert.ok(many.runs.every((x) => x.ISSUE_DESK_NAS_STARTED === '1000'));
});

test('再起動回数は1回の切り替えで3回を超えず、子には再起動を伝える', async () => {
  const r = await restartWindow([0, 0, 0, 0]);
  assert.equal(r.code, 1);
  assert.match(r.stderr, /読取の再起動回数の上限に達しました。/);
  assert.deepEqual(
    r.runs.map((x) => x.ISSUE_DESK_NAS_RESTARTS),
    ['0', '1', '2', '3'],
  );
  assert.ok(r.runs.every((x) => x.ISSUE_DESK_NAS_STARTED === '1000'));
});

test('キー発行失敗は読取CLIだけを再開しPOSTと対象外GETを再送しない', async () => {
  const f = await fixture((_req, res) => res.end('{}'));
  fs.writeFileSync(
    f.binary,
    `#!/bin/sh\necho issue >>"$KEY_LOG"\necho '受付のPostgreSQLを開けません' >&2\nexit 1\n`,
    { mode: 0o700 },
  );
  try {
    for (const args of [
      ['scripts/desk-get.ts', 'GET', '/api/issue?desk=7'],
      ['scripts/desk-get.ts', 'GET', '/api/issues/search?limit=1'],
      [
        'scripts/list-issues.ts',
        '--duplicates',
        '--project',
        'owner/repo',
        '--query',
        'test',
      ],
      ['scripts/desk-get.ts', 'GET', '/api/issues'],
      ['scripts/create-issue.ts', '--check', 'draft-123', '--timeout', '10'],
    ])
      assert.equal((await f.run(args)).code, 75, args.join(' '));
    for (const [method, route] of [
      ['GET', '/api/next'],
      ['POST', '/api/issues'],
    ]) {
      const r = await f.run([
        '--input-type=module',
        '-e',
        `try { await issueDeskLocal().call('${method}', '${route}', {text:'request'}); } catch(e) { process.exitCode=e.nasHandoff?75:1; }`,
      ]);
      assert.equal(r.code, 1);
    }
    assert.equal(f.events.length, 0);
    assert.equal(fs.readFileSync(f.log, 'utf8').trim().split('\n').length, 7);
  } finally {
    await f.close();
  }
});

test('キー回収失敗でも受信済みの応答を返す', async () => {
  const f = await fixture((_req, res) => {
    res.setHeader('content-type', 'application/json');
    res.end('{"ok":true}');
  });
  fs.writeFileSync(
    f.binary,
    `#!/bin/sh\ncase "$*" in *'cli-key issue') printf '%043d\\n' 0;; *'cli-key revoke') cat >/dev/null; exit 1;; esac\n`,
    { mode: 0o700 },
  );
  try {
    const r = await f.run(['scripts/desk-get.ts', 'GET', '/api/issues']);
    assert.equal(r.code, 0, r.stderr);
    assert.match(r.stdout, /"ok": true/);
    assert.equal(f.events.length, 1);
  } finally {
    await f.close();
  }
});

test('送信受領後のキー発行失敗はIDを案内しPOSTを繰り返さない', async () => {
  const f = await fixture((_req, res) => {
    res.setHeader('content-type', 'application/json');
    res.end('{"id":"draft-123"}');
  });
  fs.writeFileSync(
    f.binary,
    `#!/bin/sh\ncase "$*" in *'cli-key issue') echo issue >>"$KEY_LOG"; if [ -f "$KEY_LOG.once" ]; then exit 1; fi; touch "$KEY_LOG.once"; printf '%043d\\n' 0;; *'cli-key revoke') cat >/dev/null; echo revoke >>"$KEY_LOG";; esac\n`,
    { mode: 0o700 },
  );
  try {
    const r = await f.run([
      '--input-type=module',
      '-e',
      `process.env.ISSUE_DESK_NAS_HANDOFF='notice'; const {consult}=await import('./scripts/desk-client.ts'); process.exitCode=await consult(issueDeskLocal().call,{project:'owner/repo',provider:'codex',text:'依頼'},'scripts/create-issue.ts','',0);`,
    ]);
    assert.equal(r.code, 2, r.stderr);
    assert.match(r.stdout, /--check draft-123/);
    assert.deepEqual(
      f.events.map((e) => e.method),
      ['POST'],
    );
    assert.deepEqual(fs.readFileSync(f.log, 'utf8').trim().split('\n'), [
      'issue',
      'revoke',
      'issue',
    ]);
  } finally {
    await f.close();
  }
});

test('短いcheck期限でも最初のGETで結果を表示する', async () => {
  const { checkDraft } = await import('../scripts/desk-client.ts');
  const oldLog = console.log,
    lines = [];
  let count = 0;
  console.log = (line) => lines.push(line);
  try {
    const result = await checkDraft(
      async () => {
        count++;
        return {
          id: 'draft-123',
          status: 'draft',
          messages: [{ role: 'assistant', text: '回答' }],
        };
      },
      { check: 'draft-123', timeout: '1' },
      'scripts/create-issue.ts',
      '',
      3000,
    );
    assert.equal(result, 0);
    assert.equal(count, 1);
    assert.match(lines.join('\n'), /回答/);
  } finally {
    console.log = oldLog;
  }
});

test('窓口の開始時刻で期限が過ぎていても、最初の子は実際のCLIで一度読み、再起動した子は読まない', async () => {
  const draft = {
    id: 'draft-123',
    status: 'draft',
    messages: [{ role: 'assistant', text: '回答' }],
  };
  const f = await fixture((_req, res) => {
    res.setHeader('content-type', 'application/json');
    res.end(JSON.stringify(draft));
  });
  try {
    const args = [
      'scripts/create-issue.ts',
      '--check',
      'draft-123',
      '--timeout',
      '1',
    ];
    const past = String(Math.floor(Date.now() / 1000) - 600);
    const first = await f.run(args, {
      ISSUE_DESK_NAS_STARTED: past,
      ISSUE_DESK_NAS_RESTARTS: '0',
    });
    assert.equal(first.code, 0, first.stderr);
    assert.match(first.stdout, /回答/);
    assert.equal(f.events.filter((e) => e.method === 'GET').length, 1);
    f.events.length = 0;
    const restarted = await f.run(args, {
      ISSUE_DESK_NAS_STARTED: past,
      ISSUE_DESK_NAS_RESTARTS: '1',
    });
    assert.equal(restarted.code, 2, restarted.stderr);
    assert.deepEqual(f.events, []);
  } finally {
    await f.close();
  }
});

test('一時503が同じ子で回復した場合は古い失敗で正常終了を上書きしない', async () => {
  let requests = 0;
  const f = await fixture((_req, res) => {
    res.setHeader('content-type', 'application/json');
    if (++requests === 1) {
      res.statusCode = 503;
      res.end('{"code":"TEMPORARY_UNAVAILABLE"}');
    } else res.end('{"ok":true}');
  });
  try {
    const result = await f.run(['scripts/desk-get.ts', 'GET', '/api/issues']);
    assert.equal(result.code, 0, result.stderr);
    assert.match(result.stdout, /"ok": true/);
    assert.equal(requests, 2);
    assert.deepEqual(fs.readFileSync(f.log, 'utf8').trim().split('\n'), [
      'issue',
      'revoke',
      'issue',
      'revoke',
    ]);
  } finally {
    await f.close();
  }
});

test('接続切断も上位CLIを経て専用終了し、キーを回収する', async () => {
  const f = await fixture((req) => req.socket.destroy());
  try {
    const result = await f.run(['scripts/desk-get.ts', 'GET', '/api/issues']);
    assert.equal(result.code, 75, result.stderr);
    assert.equal(f.events.length, 4);
    assert.deepEqual(
      fs.readFileSync(f.log, 'utf8').trim().split('\n'),
      Array(4).fill(['issue', 'revoke']).flat(),
    );
  } finally {
    await f.close();
  }
});

test('create-issueのcheck入口は実通信エラーを専用終了に伝える', async () => {
  const f = await fixture((_req, res) => {
    res.writeHead(503, { 'content-type': 'application/json' });
    res.end('{"code":"TEMPORARY_UNAVAILABLE"}');
  });
  try {
    const r = await f.run([
      'scripts/create-issue.ts',
      '--check',
      'draft-123',
      '--timeout',
      '30',
    ]);
    assert.equal(r.code, 75, r.stderr);
    assert.equal(f.events.length, 4);
    assert.ok(
      f.events.every(
        (e) =>
          e.method === 'GET' && e.url === '/api/drafts/draft-123?view=summary',
      ),
    );
    assert.match(r.stdout, /下書き: draft-123/);
    assert.deepEqual(
      fs.readFileSync(f.log, 'utf8').trim().split('\n'),
      Array(4).fill(['issue', 'revoke']).flat(),
    );
  } finally {
    await f.close();
  }
});

test('送信ID付きPOSTは既存の再試行で本文とIDを保ち、応答喪失でも保存は1件', async () => {
  const received = [];
  const saved = new Map();
  const f = await fixture(async (req, res) => {
    let body = '';
    for await (const chunk of req) body += chunk;
    const value = JSON.parse(body);
    received.push(value);
    if (!saved.has(value.id)) saved.set(value.id, value);
    if (received.length < 4) return req.socket.destroy();
    res.setHeader('content-type', 'application/json');
    res.end('{"ok":true}');
  });
  try {
    const result = await f.run([
      '--input-type=module',
      '-e',
      `const d=globalThis.issueDeskLocal(); const r=await d.call('POST','/api/issues',{id:'same-id',text:'同じ本文',project:'owner/repo'}); console.log(JSON.stringify(r));`,
    ]);
    assert.equal(result.code, 0, result.stderr);
    assert.equal(received.length, 4);
    assert.equal(saved.size, 1);
    assert.ok(
      received.every(
        (value) => JSON.stringify(value) === JSON.stringify(received[0]),
      ),
    );
    assert.deepEqual(
      fs.readFileSync(f.log, 'utf8').trim().split('\n'),
      Array(4).fill(['issue', 'revoke']).flat(),
    );
  } finally {
    await f.close();
  }
});

test('送信IDのないPOSTの応答喪失は再送せず報告する', async () => {
  const f = await fixture((req) => req.socket.destroy());
  try {
    const r = await f.run([
      '--input-type=module',
      '-e',
      `try { await globalThis.issueDeskLocal().call('POST','/api/issues',{text:'依頼'}); } catch(e) { console.log(e.message); process.exitCode=e.nasHandoff?75:1; }`,
    ]);
    assert.equal(r.code, 1);
    assert.equal(f.events.length, 1);
    assert.match(r.stdout, /接続できません/);
    assert.deepEqual(fs.readFileSync(f.log, 'utf8').trim().split('\n'), [
      'issue',
      'revoke',
    ]);
  } finally {
    await f.close();
  }
});

test('リダイレクト先に一時キーを送らない', async () => {
  let received = 0;
  const target = http.createServer((_req, res) => {
    received++;
    res.end('{}');
  });
  await new Promise<void>((resolve) => target.listen(0, '127.0.0.1', resolve));
  const f = await fixture((_req, res) => {
    res.writeHead(302, {
      location: `http://127.0.0.1:${(target.address() as import('node:net').AddressInfo).port}/api/issues`,
    });
    res.end();
  });
  try {
    const result = await f.run(['scripts/desk-get.ts', 'GET', '/api/issues']);
    assert.equal(result.code, 1);
    assert.equal(received, 0);
    assert.equal(f.events.length, 1);
    assert.deepEqual(fs.readFileSync(f.log, 'utf8').trim().split('\n'), [
      'issue',
      'revoke',
    ]);
  } finally {
    await f.close();
    await new Promise<any>((resolve) => target.close(resolve));
  }
});

test('配備されるprobeのヘルス応答をactive・standby・不正・接続失敗で区別する', async () => {
  const { spawnSync } = await import('node:child_process');
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'nas-probe-'));
  const bin = path.join(dir, 'bin');
  fs.mkdirSync(bin);
  fs.writeFileSync(
    path.join(dir, 'fetch.cjs'),
    `global.fetch = async () => { if (process.env.HEALTH === 'failed') throw Error('offline'); return {ok: true, json: async () => process.env.HEALTH === 'malformed' ? {role:'active'} : {ok:true, role:process.env.HEALTH, ready:process.env.HEALTH === 'active'}}; };`,
  );
  fs.writeFileSync(
    path.join(bin, 'docker'),
    `#!${process.execPath}
const { spawnSync } = require('node:child_process'); const a=process.argv.slice(2);
if (a[0] === 'compose') { if (a.at(-1).endsWith('-blue')) console.log('blue'); }
else if (a[0] === 'exec') { const i=a.indexOf('-e'); const r=spawnSync(process.execPath, ['-r', process.env.FETCH_STUB, '-e', a[i+1]], {env:process.env}); process.exit(r.status ?? 1); }
else process.exit(1);
`,
    { mode: 0o700 },
  );
  try {
    for (const [health, code, output] of [
      ['active', 0, 'blue'],
      ['standby', 3, ''],
      ['malformed', 1, ''],
      ['failed', 4, ''],
    ] as [string, number, string][]) {
      const r = spawnSync('sh', ['deploy/nas/intake.sh', 'probe'], {
        env: {
          ...process.env,
          PATH: bin + ':' + process.env.PATH,
          FETCH_STUB: path.join(dir, 'fetch.cjs'),
          HEALTH: health,
        },
        encoding: 'utf8',
      });
      assert.equal(r.status, code, health);
      assert.equal(r.stdout.trim(), output);
    }
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test('checkは503の再試行対象外codeを受けたら再読取しない', async () => {
  const f = await fixture((_req, res) => {
    res.writeHead(503, { 'content-type': 'application/json' });
    res.end('{"code":"FORBIDDEN"}');
  });
  try {
    const result = await f.run([
      'scripts/create-issue.ts',
      '--check',
      'draft-123',
      '--timeout',
      '30',
    ]);
    assert.equal(result.code, 2, result.stderr);
    assert.equal(f.events.length, 1);
    assert.match(result.stdout, /--check draft-123/);
  } finally {
    await f.close();
  }
});

test('期限まで正常に下書きを読めた場合は従来の応答待ち終了を維持する', async (t) => {
  const { checkDraft } = await import('../scripts/desk-client.ts');
  t.mock.timers.enable({
    apis: ['Date', 'setTimeout'],
    now: Date.UTC(2026, 0, 1),
  });
  const oldLog = console.log,
    lines = [];
  console.log = (line) => lines.push(line);
  try {
    let reads = 0;
    const result = checkDraft(
      async () => {
        reads++;
        return { id: 'draft-123', status: 'thinking', messages: [] };
      },
      { check: 'draft-123', timeout: '0.02' },
      'scripts/create-issue.ts',
      '',
      2,
    );
    t.mock.timers.tick(2);
    await Promise.resolve();
    await Promise.resolve();
    assert.equal(reads, 1);
    t.mock.timers.tick(20);
    const code = await result;
    assert.equal(code, 0);
    assert.match(lines.join('\n'), /応答待ちのまま/);
  } finally {
    console.log = oldLog;
  }
});

test('単件と一覧の汎用GETも実通信失敗を後片付け後に専用終了へ伝える', async () => {
  const make = () =>
    fixture((_req, res) => {
      res.writeHead(503, { 'content-type': 'application/json' });
      res.end('{"code":"TEMPORARY_UNAVAILABLE"}');
    });
  const fixtures = await Promise.all([make(), make()]);
  try {
    const results = await Promise.all([
      fixtures[0].run(['scripts/desk-get.ts', 'GET', '/api/issue?desk=7']),
      fixtures[1].run([
        'scripts/desk-get.ts',
        'GET',
        '/api/issues/search?limit=1',
      ]),
    ]);
    for (let i = 0; i < fixtures.length; i++) {
      assert.equal(results[i].code, 75, results[i].stderr);
      assert.equal(fixtures[i].events.length, 4);
      assert.deepEqual(
        fs.readFileSync(fixtures[i].log, 'utf8').trim().split('\n'),
        Array(4).fill(['issue', 'revoke']).flat(),
      );
    }
  } finally {
    await Promise.all(fixtures.map((f) => f.close()));
  }
});

test('checkの起票結果と分割子の追加読取が失敗しても再開へ伝える', async () => {
  const make = (draft) =>
    fixture((req, res) => {
      res.setHeader('content-type', 'application/json');
      // 待ちの要約と、待ちが解けた後の全文の読取。
      if (req.url === '/api/drafts/draft-123?view=summary')
        res.end(
          JSON.stringify({ view: 'summary', ...draft, messages: undefined }),
        );
      else if (req.url === '/api/drafts/draft-123')
        res.end(JSON.stringify(draft));
      else {
        res.statusCode = 503;
        res.end('{"code":"TEMPORARY_UNAVAILABLE"}');
      }
    });
  const fixtures = await Promise.all([
    make({
      id: 'draft-123',
      status: 'filed',
      messages: [],
      project: 'owner/repo',
      issue: { project: 'owner/repo', number: 7 },
    }),
    make({
      id: 'draft-123',
      status: 'draft',
      messages: [],
      consultationChildren: ['child-1'],
    }),
  ]);
  try {
    const results = await Promise.all(
      fixtures.map((f) =>
        f.run([
          'scripts/create-issue.ts',
          '--check',
          'draft-123',
          '--timeout',
          '30',
        ]),
      ),
    );
    for (let i = 0; i < fixtures.length; i++) {
      assert.equal(results[i].code, 75, results[i].stderr);
      // 要約1回・全文1回の後、追加読取を4回試す。
      assert.equal(fixtures[i].events.length, 6);
      assert.deepEqual(
        fs.readFileSync(fixtures[i].log, 'utf8').trim().split('\n'),
        Array(6).fill(['issue', 'revoke']).flat(),
      );
    }
  } finally {
    await Promise.all(fixtures.map((f) => f.close()));
  }
});

test('NAS窓口は送信前に回復記録を確定し、中断と再送でもIDを保護する', async () => {
  const { spawnSync } = await import('node:child_process');
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'nas-recovery-'));
  const home = path.join(dir, 'home'),
    bin = path.join(dir, 'bin'),
    nas = path.join(dir, 'nas');
  fs.mkdirSync(home);
  fs.mkdirSync(bin);
  fs.mkdirSync(path.join(nas, 'deploy/nas'), { recursive: true });
  fs.copyFileSync(
    'deploy/nas/intake.sh',
    path.join(nas, 'deploy/nas/intake.sh'),
  );
  const install = spawnSync(
    'sh',
    [
      '-c',
      '. "$1"; say() { :; }; plan() { :; }; desk_access',
      'sh',
      path.resolve('deploy/nas/update-desk-access.sh'),
    ],
    {
      env: { ...process.env, HOME: home, DIR: path.resolve('.'), DRY_RUN: '0' },
      encoding: 'utf8',
    },
  );
  assert.equal(install.status, 0, install.stderr);
  const accepted = new Map();
  const server = http.createServer(async (req, res) => {
    if (req.method === 'POST') {
      let body = '';
      for await (const chunk of req) body += chunk;
      const value = JSON.parse(body);
      accepted.set(value.id, 'draft-123');
      res.end(JSON.stringify({ id: 'draft-123' }));
    } else {
      const id = req.url!.slice('/api/drafts/'.length);
      res.end(
        JSON.stringify({
          accepted: accepted.has(id),
          draftId: accepted.get(id),
        }),
      );
    }
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  fs.writeFileSync(
    path.join(bin, 'docker'),
    `#!${process.execPath}
const fs=require('node:fs'), path=require('node:path'); const a=process.argv.slice(2);
if(a[0]==='compose') { if(a.at(-1).endsWith('-blue')) console.log('blue'); }
else if(a[0]==='exec') process.exit(0);
else if(a[0]==='inspect') console.log(a[2].includes('.Config.Env')?'ISSUE_DESK_APP_STORE=postgres\\nISSUE_DESK_DATABASE_URL=fixture\\n':'image');
else if(a[0]==='run') { (async()=>{ fs.appendFileSync(process.env.RUN_LOG,'run\\n'); const args=a.slice(a.indexOf('/app/scripts/create-issue.ts')+1); const i=args.indexOf('--send-id'); if(i<0) process.exit(0); const id=args[i+1].toLowerCase(); const file=path.join(process.env.HOME,'.issue-desk/recovery',id+'.json'); if(!fs.existsSync(file)) process.exit(9); await fetch(process.env.MOCK_URL+'/api/issues',{method:'POST',body:JSON.stringify({id})}); process.exit(process.env.INTERRUPT==='1'?42:0); })().catch(()=>process.exit(8)); }
else process.exit(1);
`,
    { mode: 0o700 },
  );
  const thread = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
    id = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
  const env = {
    ...process.env,
    HOME: home,
    PATH: bin + ':' + process.env.PATH,
    ISSUE_DESK_NAS_DIR: nas,
    CODEX_THREAD_ID: thread,
    RUN_LOG: path.join(dir, 'runs'),
    MOCK_URL: `http://127.0.0.1:${(server.address() as import('node:net').AddressInfo).port}`,
  };
  const run = (args, extra = {}) =>
    new Promise<any>((resolve) => {
      const child = spawn(
        'sh',
        [path.join(home, '.issue-desk/desk'), ...args],
        { cwd: home, env: { ...env, ...extra } },
      );
      let stderr = '';
      child.stderr.on('data', (b) => (stderr += b));
      child.on('close', (code) => resolve({ code, stderr }));
    });
  const args = [
    'create-issue.ts',
    '--project',
    'owner/repo',
    '--tier',
    'light',
    '--text',
    'secret body',
  ];
  const recordDir = path.join(home, '.issue-desk/recovery');
  try {
    const first = await run([...args, '--send-id', id], { INTERRUPT: '1' });
    assert.equal(first.code, 42, first.stderr);
    const record = JSON.parse(
      fs.readFileSync(path.join(recordDir, id + '.json'), 'utf8'),
    );
    assert.deepEqual(record, {
      sendId: id,
      threadId: thread,
      host: os.hostname(),
      receiptState: 'unreconciled',
    });
    assert.equal(fs.statSync(recordDir).mode & 0o077, 0);
    assert.equal(
      fs.statSync(path.join(recordDir, id + '.json')).mode & 0o077,
      0,
    );
    assert.deepEqual(
      await (await fetch(env.MOCK_URL + '/api/drafts/' + id)).json(),
      { accepted: true, draftId: 'draft-123' },
    );
    assert.equal((await run([...args, '--send-id', id])).code, 0);
    assert.notEqual(
      (
        await run([...args, '--send-id', id], {
          CODEX_THREAD_ID: 'cccccccc-cccc-4ccc-8ccc-cccccccccccc',
        })
      ).code,
      0,
    );
    assert.deepEqual(fs.readdirSync(recordDir), [id + '.json']);
    assert.equal((await run(args)).code, 0);
    const generated = fs
      .readdirSync(recordDir)
      .find((x) => x !== id + '.json')!
      .slice(0, -5);
    assert.match(
      generated,
      /^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$/,
    );
    assert.deepEqual(
      await (await fetch(env.MOCK_URL + '/api/drafts/' + generated)).json(),
      { accepted: true, draftId: 'draft-123' },
    );
    const before = fs.readdirSync(recordDir).sort();
    for (const [call, extra] of [
      [['create-issue.ts', '--spec'], {}],
      [['create-issue.ts', '--check', 'draft-123'], {}],
      [[...args, '--check-only'], {}],
      [[...args, '--dry-run'], {}],
      [[...args, '--consultation-provider', 'claude'], {}],
      [args, { CODEX_THREAD_ID: '' }],
      [args, { CODEX_THREAD_ID: 'invalid' }],
      [
        [
          'create-issue.ts',
          '--project',
          'owner/repo',
          '--tier',
          'light',
          '--text',
          '',
        ],
        {},
      ],
    ] as [string[], Record<string, string>][])
      await run(call, extra);
    assert.deepEqual(fs.readdirSync(recordDir).sort(), before);
    const noNodePath = path.join(dir, 'no-node');
    fs.mkdirSync(noNodePath);
    for (const name of [
      'sh',
      'date',
      'timeout',
      'dirname',
      'awk',
      'mktemp',
      'rm',
      'readlink',
      'tr',
      'sleep',
    ]) {
      const command = spawnSync('which', [name], { encoding: 'utf8' });
      assert.equal(command.status, 0, name);
      fs.symlinkSync(command.stdout.trim(), path.join(noNodePath, name));
    }
    fs.symlinkSync(path.join(bin, 'docker'), path.join(noNodePath, 'docker'));
    for (const [call, extra] of [
      [args, { CODEX_THREAD_ID: '' }],
      [args, { CODEX_THREAD_ID: 'invalid' }],
      [args, { CODEX_THREAD_ID: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa-' }],
      [['create-issue.ts', '--spec'], {}],
      [['create-issue.ts', '--check', 'draft-123'], {}],
      [[...args, '--dry-run'], {}],
      [[...args, '--consultation-provider', 'claude'], {}],
    ] as [string[], Record<string, string>][]) {
      const count = fs
        .readFileSync(env.RUN_LOG, 'utf8')
        .trim()
        .split('\n').length;
      const result = await run(call, { ...extra, PATH: noNodePath });
      assert.equal(result.code, 0, result.stderr);
      assert.equal(
        fs.readFileSync(env.RUN_LOG, 'utf8').trim().split('\n').length,
        count + 1,
      );
    }
    assert.deepEqual(fs.readdirSync(recordDir).sort(), before);
    const runsBeforeFailure = fs.readFileSync(env.RUN_LOG, 'utf8');
    const missingNode = await run(args, { PATH: noNodePath });
    assert.notEqual(missingNode.code, 0);
    assert.equal(fs.readFileSync(env.RUN_LOG, 'utf8'), runsBeforeFailure);
    assert.deepEqual(fs.readdirSync(recordDir).sort(), before);
    assert.ok(
      !fs
        .readFileSync(path.join(recordDir, id + '.json'), 'utf8')
        .includes('secret body'),
    );
    fs.renameSync(recordDir, recordDir + '-saved');
    fs.writeFileSync(recordDir, 'blocked');
    assert.notEqual((await run(args)).code, 0);
    assert.equal(accepted.size, 2);
  } finally {
    await new Promise<void>((resolve) => server.close(() => resolve()));
    fs.rmSync(dir, { recursive: true, force: true });
  }
});
