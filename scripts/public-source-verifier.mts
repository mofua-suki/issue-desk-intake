import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import process from 'node:process';
import { Buffer } from 'node:buffer';
import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';

function treeId(root) {
  const hash = createHash('sha256');
  function visit(dir, prefix) {
    for (const name of fs.readdirSync(dir).sort()) {
      const rel = prefix ? prefix + '/' + name : name;
      const full = path.join(dir, name);
      const stat = fs.lstatSync(full);
      if (!stat.isFile() && !stat.isDirectory())
        throw Error('生成木が通常ファイルではありません');
      const data = stat.isFile() ? fs.readFileSync(full) : Buffer.alloc(0);
      hash.update(
        JSON.stringify([
          rel,
          stat.mode & 0o777,
          stat.isDirectory() ? 'd' : 'f',
          data.length,
        ]),
      );
      hash.update('\0');
      hash.update(data);
      if (stat.isDirectory()) visit(full, rel);
    }
  }
  visit(root, '');
  return hash.digest('hex');
}

const stages = {
  intake: { directory: 'server', main: './cmd/intake', client: true },
  worker: { directory: '.', main: './cmd/worker', client: false },
};

function verify(root, method, image) {
  const stage = stages[method];
  if (!stage || root.includes(',')) throw Error('検査方式またはパスが不正です');
  const before = treeId(root);
  const dockerConfig = fs.mkdtempSync(path.join(os.tmpdir(), 'public-docker-'));
  // Only fixed commands are selected by method; registry data is never shell code.
  const commands = [
    'set -eu',
    'mkdir -p /tmp/src /tmp/home /tmp/cache',
    'cp -a /source/. /tmp/src/',
    'diff -qr /source /tmp/src >/dev/null',
    ...(stage.client
      ? [
          'cp -a /opt/public-deps/pnpm /tmp/pnpm',
          'cp -a /opt/public-deps/cache /tmp/pnpm-cache',
          'chmod -R u+rwX /tmp/pnpm /tmp/pnpm-cache',
          'export XDG_CACHE_HOME=/tmp/pnpm-cache',
          'cd /tmp/src/client',
          'pnpm install --offline --frozen-lockfile --ignore-scripts --store-dir /tmp/pnpm >/dev/null || exit 21',
          'pnpm build >/dev/null || exit 21',
        ]
      : []),
    `cd /tmp/src/${stage.directory}`,
    `go build -mod=readonly -o /tmp/application ${stage.main} || exit 21`,
    ...(method === 'intake'
      ? [
          'set +e; /tmp/application --help >/tmp/help 2>&1; result=$?; set -e',
          'test "$result" -le 1 && grep -qi usage /tmp/help || exit 22',
        ]
      : [
          '/tmp/application version >/tmp/version 2>&1 || exit 22',
          "head -n 1 /tmp/version | grep -q '^issue-desk-worker ' || exit 22",
        ]),
    'set +e; /tmp/application --public-source-unknown-option >/dev/null 2>&1; result=$?; set -e',
    'test "$result" -ne 0 || exit 23',
    `go test -mod=readonly ${stage.main} || exit 23`,
  ].join('\n');
  try {
    const dockerEnv = {
      PATH: process.env.PATH || '/usr/bin:/bin',
      HOME: dockerConfig,
    };
    const volumes = JSON.parse(
      execFileSync(
        'docker',
        [
          '--config',
          dockerConfig,
          'image',
          'inspect',
          '--format',
          '{{json .Config.Volumes}}',
          image,
        ],
        {
          encoding: 'utf8',
          stdio: ['ignore', 'pipe', 'ignore'],
          env: dockerEnv,
        },
      ),
    );
    if (
      volumes !== null &&
      (typeof volumes !== 'object' ||
        Array.isArray(volumes) ||
        Object.keys(volumes).length)
    )
      throw Error('イメージに外部ボリュームがあります');
    execFileSync(
      'docker',
      [
        '--config',
        dockerConfig,
        'run',
        '--rm',
        '--pull',
        'never',
        '--network',
        'none',
        '--read-only',
        '--cap-drop',
        'ALL',
        '--security-opt',
        'no-new-privileges',
        '--user',
        '1000:1000',
        '--pids-limit',
        '256',
        '--log-driver',
        'none',
        '--tmpfs',
        '/tmp:rw,exec,nosuid,nodev,uid=1000,gid=1000',
        '--mount',
        `type=bind,source=${root},destination=/source,readonly`,
        '--entrypoint',
        '/usr/bin/env',
        image,
        '-i',
        'PATH=/usr/local/go/bin:/opt/agent-tools/bin:/usr/local/bin:/usr/bin:/bin',
        'HOME=/tmp/home',
        'GOCACHE=/tmp/cache',
        'GOMODCACHE=/opt/public-deps/go',
        'GOPROXY=off',
        'GOTOOLCHAIN=local',
        'CGO_ENABLED=0',
        '/bin/sh',
        '-c',
        commands,
      ],
      { stdio: 'ignore', env: dockerEnv },
    );
  } catch {
    throw Error('隔離またはビルド・起動・動作確認に失敗しました');
  } finally {
    fs.rmSync(dockerConfig, { recursive: true, force: true });
  }
  if (treeId(root) !== before) throw Error('検査中に生成木が変化しました');
  return before;
}

process.stdout.write(verify(process.argv[2], process.argv[3], process.argv[4]));
