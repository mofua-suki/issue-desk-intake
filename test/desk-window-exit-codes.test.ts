import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import http from 'node:http';
import { spawn, spawnSync } from 'node:child_process';

const root = process.cwd(),
  key = '0'.repeat(43),
  token = 'idk_fixture_token';
const id = '12345678-1234-1234-1234-123456789abc';
const base = ['--project', 'owner/repo', '--number', '12'];
const reply = [...base, '--text', '確認した内容への返信です', '--id', id];

// 窓口がコンテナで動かすシェルを、イメージ（chat-local.ts）の写しで実際に動かす。
// /app を写しの場所へ置き換え、node の代役が受け取った引数と、終了コード（引き継ぎの75）がそのまま返るかを見る。
function inContainer(script: string, args: string[]) {
  const image = fs.mkdtempSync(path.join(os.tmpdir(), 'desk-image-'));
  try {
    fs.mkdirSync(path.join(image, 'app/deploy/nas'), { recursive: true });
    fs.mkdirSync(path.join(image, 'bin'));
    fs.writeFileSync(path.join(image, 'app/deploy/nas/chat-local.ts'), '');
    fs.writeFileSync(
      path.join(image, 'bin/node'),
      '#!/bin/sh\nprintf "%s\\n" "$@" > "$NODE_ARGS"\nexit 75\n',
      { mode: 0o700 },
    );
    const run = spawnSync(
      'sh',
      [
        '-c',
        script.replaceAll('/app/', image + '/app/'),
        'sh',
        image + '/app/scripts/reply-issue.ts',
        ...args,
      ],
      {
        env: {
          PATH: path.join(image, 'bin') + ':/usr/bin:/bin',
          NODE_ARGS: path.join(image, 'args'),
        },
        encoding: 'utf8',
      },
    );
    assert.equal(run.status, 75, run.stderr);
    assert.deepEqual(
      fs
        .readFileSync(path.join(image, 'args'), 'utf8')
        .split('\n')
        .slice(0, -1),
      [
        '--import',
        path.join(image, 'app/deploy/nas/chat-local.ts'),
        image + '/app/scripts/reply-issue.ts',
        ...args,
      ],
    );
  } finally {
    fs.rmSync(image, { recursive: true, force: true });
  }
}

async function fixture(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'desk-window-'));
  const home = path.join(dir, 'home'),
    bin = path.join(dir, 'bin'),
    nas = path.join(dir, 'nas');
  for (const p of [home, bin, path.join(nas, 'deploy/nas')])
    fs.mkdirSync(p, { recursive: true });
  fs.copyFileSync(
    'deploy/nas/intake.sh',
    path.join(nas, 'deploy/nas/intake.sh'),
  );
  const events = [];
  let handler: (req: any, res: any, event?: any) => void = (_req, res) =>
    res.end(
      JSON.stringify({
        project: 'owner/repo',
        number: 12,
        title: 'test',
        status: 'open',
      }),
    );
  const server = http.createServer(async (req, res) => {
    let body = '';
    for await (const chunk of req) body += chunk;
    const event = {
      method: req.method,
      url: req.url,
      key: req.headers['x-desk-key'],
      auth: req.headers.authorization,
      body: body ? JSON.parse(body) : undefined,
    };
    events.push(event);
    res.setHeader('content-type', 'application/json');
    handler(req, res, event);
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const config = path.join(dir, 'config'),
    log = path.join(dir, 'keys'),
    dockerLog = path.join(dir, 'docker-log');
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
  fs.writeFileSync(
    binary,
    `#!/bin/sh\ncase "$*" in *'cli-key issue') echo issue >>"$KEY_LOG"; printf '%043d\\n' 0;; *'cli-key revoke') cat >/dev/null; echo revoke >>"$KEY_LOG";; *) exit 1;; esac\n`,
    { mode: 0o700 },
  );
  fs.writeFileSync(
    path.join(bin, 'docker'),
    `#!${process.execPath}
const fs=require('fs'), {spawnSync}=require('child_process'), a=process.argv.slice(2);
if(a[0]==='compose') { if(a.at(-1).endsWith('blue')) console.log('intake-blue'); }
else if(a[0]==='exec') process.exit(0);
else if(a[0]==='ps') console.log(process.env.IDS || 'abc123');
else if(a[0]==='inspect') { if(a[2].includes('.Config.Env')) console.log('ISSUE_DESK_APP_STORE=postgres\\nISSUE_DESK_DATABASE_URL=fixture\\n'); else if(a[2].includes('.Config.Image')) console.log(process.env.META || 'image|volume:issue-desk-worker-state|true'); else console.log('image'); }
else if(a[0]==='run') { fs.appendFileSync(process.env.DOCKER_LOG, JSON.stringify(a)+'\\n'); const c=a.findIndex((x,k)=>x==='-c'&&String(a[k+1]).includes('/app/deploy/nas/chat-local.ts')); const i=c<0?-1:c+2; if(i<0) { if(process.env.FIX_K2 && a.includes('--entrypoint') && a[a.indexOf('--entrypoint')+1]==='sh') { const j=a.indexOf('-c'); const inner=a[j+1].replaceAll('/state/config.local.json', process.env.FIX_K2); const r=spawnSync('sh',['-c',inner,...a.slice(j+2)],{env:process.env,stdio:'inherit'}); process.exit(r.status ?? 1); } process.exit(Number(process.env.DOCKER_STATUS || 0)); } const env={...process.env}; for(let j=0;j<a.length;j++) if(a[j]==='-e' && a[j+1].includes('=')) { const k=a[j+1].indexOf('='); env[a[j+1].slice(0,k)]=a[j+1].slice(k+1); } env.ISSUE_DESK_CONFIG=process.env.FIX_CONFIG; env.ISSUE_DESK_INTAKE_BINARY=process.env.FIX_BINARY; const r=spawnSync(process.execPath,['--import',process.env.ROOT+'/deploy/nas/chat-local.ts',process.env.ROOT+a[i+1].slice(4),...a.slice(i+2)],{env,stdio:'inherit'}); process.exit(r.status ?? 1); } else process.exit(1);
`,
    { mode: 0o700 },
  );
  const env: NodeJS.ProcessEnv = {
    ...process.env,
    HOME: home,
    PATH: bin + ':' + process.env.PATH,
    DIR: root,
    DRY_RUN: '0',
    ISSUE_DESK_NAS_DIR: nas,
    ROOT: root,
    FIX_CONFIG: config,
    FIX_BINARY: binary,
    KEY_LOG: log,
    DOCKER_LOG: dockerLog,
  };
  delete env.ISSUE_DESK_URL;
  delete env.ISSUE_DESK_TOKEN_FILE;
  const install = spawnSync(
    'sh',
    [
      '-c',
      '. "$DIR/deploy/nas/update-desk-access.sh"; say() { :; }; plan() { :; }; desk_access',
    ],
    { env, encoding: 'utf8' },
  );
  assert.equal(install.status, 0, install.stderr);
  const nasDesk = path.join(home, '.issue-desk/desk'),
    ubuntuDesk = path.join(home, '.issue-desk/ubuntu-desk');
  const linux = spawnSync(
    'sh',
    [
      '-c',
      '. "$DIR/deploy/clients/remote-control/desk-access.sh"; linux_desk_access_body',
    ],
    { env, encoding: 'utf8' },
  );
  assert.equal(linux.status, 0);
  fs.writeFileSync(ubuntuDesk, linux.stdout);
  const run = async (command, args, extra = {}, cwd = home) => {
    const result = await new Promise<any>((resolve, reject) => {
      const child = spawn(command, args, { cwd, env: { ...env, ...extra } });
      let stdout = '',
        stderr = '';
      child.stdout.on('data', (b) => (stdout += b));
      child.stderr.on('data', (b) => (stderr += b));
      child.on('error', reject);
      child.on('close', (code) => resolve({ code, stdout, stderr }));
    });
    assert.ok(
      !result.stdout.includes(key) &&
        !result.stderr.includes(key) &&
        !result.stdout.includes(token) &&
        !result.stderr.includes(token),
    );
    return result;
  };
  const desk = (args, extra?, cwd?) =>
    run('sh', [nasDesk, ...args], extra, cwd);
  const ubuntu = (args, extra?, cwd?) =>
    run('sh', [ubuntuDesk, ...args], extra, cwd);
  const logs = (file) =>
    fs.existsSync(file) ? fs.readFileSync(file, 'utf8').trim().split('\n') : [];
  const checkLogs = () => {
    for (const file of [log, dockerLog]) {
      const contents = fs.existsSync(file) ? fs.readFileSync(file, 'utf8') : '';
      assert.ok(
        !contents.includes(key) && !contents.includes(token),
        'ログに資格情報が含まれています',
      );
    }
  };
  const reset = () => {
    checkLogs();
    events.length = 0;
    for (const f of [log, dockerLog]) fs.rmSync(f, { force: true });
  };
  t.after(async () => {
    await new Promise<any>((resolve) => server.close(resolve));
    try {
      checkLogs();
    } finally {
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });
  return {
    dir,
    home,
    events,
    desk,
    ubuntu,
    run,
    reset,
    logs,
    log,
    dockerLog,
    setHandler: (h) => (handler = h),
    url: `http://127.0.0.1:${(server.address() as import('node:net').AddressInfo).port}`,
  };
}

test('NAS窓口の名前拒否と読取認証', async (t) => {
  const f = await fixture(t);
  for (const name of [
    'get',
    'operation',
    'foo',
    '../bad.mjs',
    'desk-get.mjs',
    'reply-issue.mjs',
  ]) {
    assert.equal((await f.desk([name])).code, 2);
    assert.equal(f.events.length, 0);
    assert.equal(f.logs(f.dockerLog).length, 0);
  }
  for (const args of [
    ['desk-get.ts', 'GET', '/api/issue?desk=12'],
    ['desk-get.ts', 'GET', '/api/issue?project=owner%2Frepo&number=12'],
  ]) {
    f.reset();
    assert.equal((await f.desk(args)).code, 0);
    assert.equal(f.events.length, 1);
    assert.equal(f.events[0].key, key);
    assert.equal(f.events[0].auth, undefined);
    assert.deepEqual(f.logs(f.log), ['issue', 'revoke']);
  }
  for (const status of [401, 403]) {
    f.reset();
    f.setHandler((_req, res) => {
      res.statusCode = status;
      res.end('{}');
    });
    assert.equal(
      (
        await f.desk([
          'desk-get.ts',
          'GET',
          '/api/issue?project=owner%2Frepo&number=12',
        ])
      ).code,
      1,
    );
    assert.equal(f.events.length, 1);
    assert.deepEqual(f.logs(f.log), ['issue', 'revoke']);
  }
});

// 専用操作CLIではなく、残る返信経路を通して共通transportの秘密保護・キー回収を検査する。
test('NAS窓口の返信は成功・認証拒否・通信切断でも一時キーを回収する', async (t) => {
  const f = await fixture(t);
  let r = await f.desk(['reply-issue.ts', ...reply]);
  assert.equal(r.code, 0, r.stderr);
  const dockerRuns = f.logs(f.dockerLog).map((line) => JSON.parse(line));
  assert.equal(dockerRuns.length, 1);
  const entrypoint = dockerRuns[0].indexOf('--entrypoint');
  assert.deepEqual(dockerRuns[0].slice(entrypoint, entrypoint + 4), [
    '--entrypoint',
    'sh',
    'image',
    '-c',
  ]);
  assert.deepEqual(dockerRuns[0].slice(entrypoint + 5), [
    'sh',
    '/app/scripts/reply-issue.ts',
    ...reply,
  ]);
  inContainer(
    dockerRuns[0][entrypoint + 4],
    dockerRuns[0].slice(entrypoint + 6),
  );
  assert.deepEqual(
    f.events.map((e) => [e.method, e.url.split('?')[0]]),
    [
      ['GET', '/api/issue'],
      ['POST', '/api/comment'],
      ['GET', '/api/issue'],
    ],
  );
  assert.deepEqual(f.events[1].body, {
    async: true,
    id,
    project: 'owner/repo',
    number: 12,
    action: 'reply',
    text: '確認した内容への返信です',
  });
  assert.ok(f.events.every((e) => e.key === key && e.auth === undefined));
  assert.deepEqual(f.logs(f.log), [
    'issue',
    'revoke',
    'issue',
    'revoke',
    'issue',
    'revoke',
  ]);
  for (const status of [400, 401, 403, 409]) {
    f.reset();
    f.setHandler((req, res) => {
      if (req.method === 'POST') res.statusCode = status;
      res.end('{}');
    });
    r = await f.desk(['reply-issue.ts', ...reply]);
    assert.equal(r.code, 1);
    assert.equal(f.events.filter((e) => e.method === 'POST').length, 1);
    assert.deepEqual(f.logs(f.log), ['issue', 'revoke', 'issue', 'revoke']);
  }
  f.reset();
  f.setHandler((req, res) => {
    if (req.method === 'POST') return req.socket.destroy();
    res.end('{}');
  });
  r = await f.desk(['reply-issue.ts', ...reply]);
  assert.equal(r.code, 1);
  assert.equal(f.events.filter((e) => e.method === 'POST').length, 1);
  assert.deepEqual(f.logs(f.log), ['issue', 'revoke', 'issue', 'revoke']);
});

test('Ubuntu窓口のWorker引数、終了コードと境界', async (t) => {
  const f = await fixture(t);
  for (const [cmd, status] of [
    ['get', 0],
    ['operation', 7],
  ]) {
    f.reset();
    assert.equal(
      (
        await f.ubuntu([cmd, '--path', '/api/issue'], {
          DOCKER_STATUS: String(status),
        })
      ).code,
      status,
    );
    const args = JSON.parse(f.logs(f.dockerLog)[0]);
    assert.ok(args.includes('issue-desk-worker-state:/state:ro'));
    assert.deepEqual(args.slice(args.indexOf('--entrypoint')), [
      '--entrypoint',
      '/usr/local/bin/desk',
      'image',
      cmd,
      '--config',
      '/state/config.local.json',
      '--path',
      '/api/issue',
    ]);
  }
  for (const extra of [
    { IDS: 'abc\ndef' },
    { META: 'image|volume:wrong|true' },
  ]) {
    f.reset();
    assert.equal((await f.ubuntu(['get'], extra)).code, 1);
    assert.equal(f.logs(f.dockerLog).length, 0);
  }
  const outside = path.join(f.dir, 'outside');
  fs.writeFileSync(outside, 'x');
  for (const [args, extra, cwd] of [
    [['get', outside], {}, f.home],
    [['get'], {}, f.dir],
    [['get'], { ISSUE_DESK_TOKEN_FILE: outside }, f.home],
  ]) {
    f.reset();
    assert.equal((await f.ubuntu(args, extra, cwd)).code, 1);
    assert.equal(f.logs(f.dockerLog).length, 0);
  }
  fs.mkdirSync(path.join(f.home, '.issue-desk/remote-control/source/scripts'), {
    recursive: true,
  });
  fs.writeFileSync(
    path.join(
      f.home,
      '.issue-desk/remote-control/source/scripts/desk-client.ts',
    ),
    '',
  );
  assert.equal((await f.ubuntu(['absent.ts'])).code, 2);
  // 改名前の .mjs の名前は、正本に同じ名前の .mjs や .ts が有っても使い方の誤りとして断り、コンテナを起動しない。
  fs.writeFileSync(
    path.join(f.home, '.issue-desk/remote-control/source/scripts/desk-get.ts'),
    '',
  );
  fs.writeFileSync(
    path.join(f.home, '.issue-desk/remote-control/source/scripts/desk-get.mjs'),
    '',
  );
  for (const name of ['desk-get.mjs', 'absent.mjs']) {
    f.reset();
    const r = await f.ubuntu([name]);
    assert.equal(r.code, 2);
    assert.equal(f.logs(f.dockerLog).length, 0);
  }
  // 正本の desk-client が .mjs だけ（改名前の版）なら窓口は動かさない。
  fs.rmSync(
    path.join(
      f.home,
      '.issue-desk/remote-control/source/scripts/desk-client.ts',
    ),
  );
  fs.writeFileSync(
    path.join(
      f.home,
      '.issue-desk/remote-control/source/scripts/desk-client.mjs',
    ),
    '',
  );
  f.reset();
  assert.equal((await f.ubuntu(['desk-get.ts'])).code, 1);
  assert.equal(f.logs(f.dockerLog).length, 0);
});

test('PCトークンによる直接読取と送信の成功・認証拒否', async (t) => {
  const f = await fixture(t),
    tokenFile = path.join(f.home, 'token');
  fs.writeFileSync(tokenFile, token, { mode: 0o600 });
  for (const script of ['desk-get.ts', 'reply-issue.ts'])
    for (const status of [200, 401, 403]) {
      f.reset();
      f.setHandler((req, res) => {
        if (
          status !== 200 &&
          (script === 'desk-get.ts' || req.method === 'POST')
        )
          res.statusCode = status;
        res.end('{}');
      });
      const args =
        script === 'desk-get.ts'
          ? ['GET', '/api/issue?project=owner%2Frepo&number=12']
          : reply;
      const r = await f.run(process.execPath, [
        path.join(root, 'scripts', script),
        ...args,
        '--url',
        f.url,
        '--token-file',
        tokenFile,
      ]);
      assert.equal(r.code, status === 200 ? 0 : 1, r.stderr);
      assert.ok(f.events.length > 0);
      assert.ok(
        f.events.every(
          (e) => e.auth === `Bearer ${token}` && e.key === undefined,
        ),
      );
      if (script === 'reply-issue.ts' && status === 403)
        assert.match(r.stderr, /運用の代行/);
    }
});

test('Ubuntuスクリプト窓口はK2の必須項目が欠けると停止する', async (t) => {
  const f = await fixture(t);
  const source = path.join(f.home, '.issue-desk/scripts-root');
  fs.writeFileSync(source, root + '\n');
  const config = path.join(f.home, 'k2.json');
  const complete = {
    serverUrl: f.url,
    serverTokenFile: 'token',
    workerId: token,
  };
  for (const missing of Object.keys(complete)) {
    f.reset();
    const value = { ...complete };
    delete value[missing];
    fs.writeFileSync(config, JSON.stringify(value));
    const result = await f.ubuntu(
      ['desk-get.ts', 'GET', '/api/issue?project=owner%2Frepo&number=12'],
      { FIX_K2: config },
    );
    assert.equal(result.code, 1);
    assert.match(result.stderr, /3項目/);
    assert.equal(f.events.length, 0);
    assert.equal(f.logs(f.dockerLog).length, 1);
  }
});
