import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { test } from 'node:test';
import {
  runDevelopment,
  validateRequest,
  marker,
} from '../scripts/ssh-development-runner.ts';
import { readResult } from '../scripts/ssh-development.ts';
import {
  cleanOutput,
  reportSummary,
} from '../deploy/nas-host/host-ssh-output.ts';

const request = () => ({
  version: 1,
  id: 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee',
  repository: 'mofua-suki/issue-desk-worker',
  base: 'a'.repeat(40),
  prompt: '試験の不具合を直してください',
  bundle: 'YWJj',
});

test('対象外リポジトリや基点・不正なbundleは実行しない', () => {
  for (const change of [
    { repository: 'other/repo' },
    { base: '--upload-pack=bad' },
    { bundle: '../file' },
    { prompt: '' },
  ])
    assert.throws(() => validateRequest({ ...request(), ...change }));
});

test('実装・新規ファイル回収・固定検証を実行し、失敗を合格にせず専用cloneを片付ける', () => {
  for (const fail of ['', 'implementation', 'test', 'format', 'mutate']) {
    const home = fs.mkdtempSync(
      path.join(os.tmpdir(), 'ssh-development-test-'),
    );
    const calls = [];
    let differences = 0;
    try {
      const result = runDevelopment(request(), {
        home,
        exec(command, args, opts) {
          assert.equal(command, 'timeout');
          const [tool, ...parameters] = args.slice(3);
          calls.push([tool, ...parameters]);
          if (tool === 'git' && parameters[0] === 'clone')
            fs.mkdirSync(path.join(opts.cwd, 'repo/.git/info'), {
              recursive: true,
            });
          if (tool === 'git' && parameters[0] === 'rev-parse')
            return { status: 0, stdout: request().base };
          if (tool === 'git' && parameters[0] === 'diff')
            return {
              status: 0,
              stdout:
                'diff --git a/new.go b/new.go\n' +
                (fail === 'mutate' && differences++
                  ? '+changed during test\n'
                  : '+package test\n'),
            };
          if (tool === 'codex' && parameters[0] === 'exec') {
            assert.ok(parameters.includes('workspace-write'));
            assert.ok(opts.input.includes('AGENTS.md'));
            assert.ok(!parameters.some((arg) => arg.includes('bypass')));
            if (fail === 'implementation')
              return { status: 1, stderr: '実装失敗' };
          }
          if (tool === 'go' && parameters[0] === 'test' && fail === 'test')
            return { status: 1, stderr: '固定検証失敗' };
          if (tool === 'gofmt' && fail === 'format')
            return { status: 0, stdout: 'new.go\n' };
          return { status: 0, stdout: '' };
        },
      });
      assert.equal(result.status, fail ? 'failed' : 'verified');
      assert.deepEqual(
        fs.readdirSync(path.join(home, '.issue-desk/ssh-development')),
        [],
      );
      if (!fail) {
        assert.deepEqual(
          result.stages.map((s) => s.name),
          ['implementation', 'go-test', 'go-vet', 'gofmt'],
        );
        assert.ok(calls.some((c) => c.join(' ') === 'git add -N -- .'));
        assert.match(Buffer.from(result.patch, 'base64').toString(), /new.go/);
      }
    } finally {
      fs.rmSync(home, { recursive: true, force: true });
    }
  }
});

test('実Git bundleから別cloneを作りAIの変更を固定検証後にパッチで回収できる', () => {
  const home = fs.mkdtempSync(path.join(os.tmpdir(), 'ssh-development-git-'));
  const source = path.join(home, 'source');
  fs.mkdirSync(source);
  const git = (...args) => {
    const r = spawnSync('git', args, { cwd: source, encoding: 'utf8' });
    assert.equal(r.status, 0, r.stderr);
    return r.stdout.trim();
  };
  try {
    git('init');
    git('config', 'user.name', 'test');
    git('config', 'user.email', 'test@example.invalid');
    fs.writeFileSync(path.join(source, 'AGENTS.md'), 'ここで実装する');
    git('add', '.');
    git('commit', '-m', 'base');
    const base = git('rev-parse', 'HEAD');
    git('bundle', 'create', path.join(home, 'input.bundle'), 'HEAD');
    const result = runDevelopment(
      {
        ...request(),
        base,
        bundle: fs
          .readFileSync(path.join(home, 'input.bundle'))
          .toString('base64'),
      },
      {
        home,
        exec(command, args, opts) {
          const tool = args[3];
          if (tool === 'codex') {
            if (args.includes('exec'))
              fs.writeFileSync(path.join(opts.cwd, 'new.go'), 'package demo\n');
            return { status: 0, stdout: '' };
          }
          if (tool === 'go' || tool === 'gofmt')
            return { status: 0, stdout: '' };
          return spawnSync(command, args, opts);
        },
      },
    );
    assert.equal(result.status, 'verified', result.error);
    const patch = Buffer.from(result.patch, 'base64').toString();
    assert.match(patch, /new file mode/);
    const applied = spawnSync('git', ['apply', '--check', '-'], {
      cwd: source,
      input: patch,
      encoding: 'utf8',
    });
    assert.equal(applied.status, 0, applied.stderr);
  } finally {
    fs.rmSync(home, { recursive: true, force: true });
  }
});

test('取得した結果のID・基点・固定検証の欠落を拒否する', () => {
  const r = request();
  const result = {
    ...r,
    status: 'verified',
    patch: '',
    stages: ['implementation', 'go-test', 'go-vet', 'gofmt'].map((name) => ({
      name,
      exitCode: 0,
    })),
  };
  const output = (obj) =>
    marker + Buffer.from(JSON.stringify(obj)).toString('base64');
  assert.equal(readResult(output(result), r).status, 'verified');
  for (const change of [
    { id: 'other' },
    { base: 'b'.repeat(40) },
    { stages: [] },
  ])
    assert.throws(() => readResult(output({ ...result, ...change }), r));
  assert.throws(() => readResult('', r));
  assert.throws(() => readResult(output(result) + '\n' + output(result), r));
});

test('構造化成果物は行の秘密語フィルターで壊さず受付の要約からは除く', () => {
  const artifact = marker + 'token';
  assert.equal(cleanOutput(artifact), artifact);
  assert.equal(
    reportSummary('完了', artifact + '\nverify=0'),
    '完了\nverify=0',
  );
  assert.equal(cleanOutput('token=資格情報\n普通のログ'), '普通のログ');
});

test('実装の権限境界を変えるスクリプトは権限承認を必要とする', async () => {
  const { permissionReason } = await import(
    '../shared/src/permission-guard.ts'
  );
  const reasons = permissionReason([
    ['M', 'scripts/ssh-development-host.sh'],
    ['M', 'scripts/ssh-development-runner.ts'],
  ]);
  assert.equal(reasons.length, 2);
});

test('送った実装コードは登録済みハッシュと一致しなければ実行しない', async () => {
  const { developmentCommand } = await import(
    '../scripts/ssh-development-policy.ts'
  );
  const policy = JSON.parse(
    fs.readFileSync(new URL('../host-ssh.json', import.meta.url), 'utf8'),
  );
  assert.equal(policy.operations.develop, developmentCommand());
  assert.ok(!policy.readOnlyOperations.includes('develop'));
  const tampered = spawnSync('sh', ['-c', policy.operations.develop], {
    input:
      Buffer.from('echo 不正なコードが実行された').toString('base64') +
      '\n' +
      Buffer.from('invalid').toString('base64') +
      '\n{}',
    encoding: 'utf8',
  });
  assert.notEqual(tampered.status, 0);
  assert.doesNotMatch(tampered.stdout, /不正なコードが実行された/);
  assert.match(tampered.stderr, /FAILED/);
});

test('呼出元は依頼と照合済み成果物を保存し、一時bundleと送信本文を片付ける', async () => {
  const { main } = await import('../scripts/ssh-development.ts');
  const root = fs.mkdtempSync(
    path.join(os.tmpdir(), 'ssh-development-client-'),
  );
  const source = path.join(root, 'source');
  const bin = path.join(root, 'bin');
  const oldPath = process.env.PATH;
  fs.mkdirSync(source);
  fs.mkdirSync(bin);
  const git = (...args) => {
    const r = spawnSync('git', args, { cwd: source, encoding: 'utf8' });
    assert.equal(r.status, 0, r.stderr);
  };
  try {
    git('init');
    git('config', 'user.name', 'test');
    git('config', 'user.email', 'test@example.invalid');
    fs.writeFileSync(path.join(source, 'README.md'), 'fixture');
    git('add', '.');
    git('commit', '-m', 'base');
    git(
      'remote',
      'add',
      'origin',
      'gitea-nas:mofua-suki/issue-desk-worker.git',
    );
    fs.writeFileSync(
      path.join(bin, 'sh'),
      `#!${process.execPath}
const fs=require('node:fs');
const sent=fs.readFileSync(process.argv.at(-1),'utf8').split('\\n');
if(sent.length!==3)process.exit(2);
const req=JSON.parse(sent[2]);
const result={version:1,id:req.id,base:req.base,repository:req.repository,status:'verified',patch:Buffer.from('diff fixture').toString('base64'),stages:['implementation','go-test','go-vet','gofmt'].map(name=>({name,exitCode:0}))};
console.log('ISSUE_DESK_DEVELOPMENT_RESULT='+Buffer.from(JSON.stringify(result)).toString('base64'));
`,
      { mode: 0o755 },
    );
    const prompt = path.join(root, 'prompt.txt');
    const out = path.join(root, 'result');
    fs.writeFileSync(prompt, '依頼');
    process.env.PATH = bin + ':' + oldPath;
    assert.equal(
      main([
        '--host',
        'gt12-pro',
        '--cwd',
        source,
        '--repo',
        'mofua-suki/issue-desk-worker',
        '--file',
        prompt,
        '--out',
        out,
      ]),
      0,
    );
    assert.deepEqual(fs.readdirSync(out).sort(), [
      'change.patch',
      'request-info.json',
      'result.json',
    ]);
    assert.equal(
      fs.readFileSync(path.join(out, 'change.patch'), 'utf8'),
      'diff fixture',
    );
    assert.equal(
      JSON.parse(fs.readFileSync(path.join(out, 'request-info.json'), 'utf8'))
        .host,
      'gt12-pro',
    );
  } finally {
    process.env.PATH = oldPath;
    fs.rmSync(root, { recursive: true, force: true });
  }
});
