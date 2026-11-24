import { spawn, type SpawnOptions } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { createInterface } from 'node:readline';

const packages = {
  'linux-x64': [
    'x64',
    '9jImXRTybgh+12zLpE4Jg5Y6TNBomKxiWTxCTCMRxy6qI9gBqw3Vm8SZmnHgakxF+rK0INPJm293iqOvafHeKw==',
  ],
  'linux-arm64': [
    'arm64',
    '7pH5LwZtSygwryB+FCp4UW3Lj/cluEWZKhUoGIIjTlPzJ56ROTuOFPMi0dsnyyspWUIebLAD2/9B/NWGG1OUsA==',
  ],
};
const version = '0.14.0-dev.2456';
const controller = new AbortController();
for (const signal of ['SIGINT', 'SIGTERM'])
  process.on(signal, () => controller.abort());

function run(
  command: string,
  args: string[],
  options: SpawnOptions = {},
  onLine?: (line: string) => void,
) {
  return new Promise<void>((resolve, reject) => {
    const child = spawn(command, args, { stdio: 'inherit', ...options });
    if (onLine && child.stdout)
      createInterface({ input: child.stdout, crlfDelay: Infinity }).on(
        'line',
        onLine,
      );
    const abort = () => child.kill('SIGTERM');
    controller.signal.addEventListener('abort', abort, { once: true });
    if (controller.signal.aborted) abort();
    child.once('error', reject);
    child.once('close', (code, signal) => {
      controller.signal.removeEventListener('abort', abort);
      if (code === 0 && !controller.signal.aborted) resolve();
      else reject(new Error(`${command} failed (${signal ?? code})`));
    });
  });
}

let directory;
try {
  const env: NodeJS.ProcessEnv = { ...process.env, CGO_ENABLED: '1' };
  let hasCompiler = Boolean(env.CC);
  for (const compiler of ['gcc', 'clang']) {
    if (hasCompiler) break;
    try {
      await run(compiler, ['--version'], { stdio: 'ignore' });
      if (compiler === 'clang') env.CC = compiler;
      hasCompiler = true;
    } catch (error) {
      if (controller.signal.aborted) throw error;
    }
  }
  if (!hasCompiler) {
    const entry = packages[`${process.platform}-${process.arch}`];
    if (!entry)
      throw new Error(
        'Cコンパイラがありません。CCにGoのrace検出に対応するコンパイラを指定してください。',
      );
    directory = await mkdtemp(path.join(os.tmpdir(), 'issue-desk-race-'));
    const [arch, integrity] = entry;
    const url = `https://registry.npmjs.org/@ryoppippi/zig-linux-${arch}/-/zig-linux-${arch}-${version}.tgz`;
    console.log('race検証用のCコンパイラを準備します。');
    const response = await fetch(url, {
      redirect: 'error',
      signal: controller.signal,
    });
    if (!response.ok)
      throw new Error(
        `Cコンパイラの取得に失敗しました (HTTP ${response.status})。`,
      );
    const chunks = [];
    let size = 0;
    for await (const chunk of response.body) {
      size += chunk.length;
      if (size > 256 * 1024 * 1024)
        throw new Error('Cコンパイラの配布物が上限を超えています。');
      chunks.push(chunk);
    }
    const bytes = Buffer.concat(chunks);
    if (createHash('sha512').update(bytes).digest('base64') !== integrity)
      throw new Error('Cコンパイラの整合性が一致しません。');
    const archive = path.join(directory, 'zig.tgz');
    await writeFile(archive, bytes, { mode: 0o600 });
    await run('tar', ['-xzf', archive, '-C', directory, '--no-same-owner']);
    const wrapper = path.join(directory, 'cc');
    const zig = path.join(directory, 'package', 'zig').replaceAll("'", "'\\''");
    await writeFile(wrapper, `#!/bin/sh\nexec '${zig}' cc "$@"\n`, {
      mode: 0o700,
    });
    env.CC = `"${wrapper.replaceAll('\\', '\\\\').replaceAll('"', '\\"')}"`;
    env.ZIG_GLOBAL_CACHE_DIR = path.join(directory, 'cache');
    env.ZIG_LOCAL_CACHE_DIR = path.join(directory, 'local-cache');
  }
  const missing = new Set([
    'TestOpenAPISpecConcurrentRequests',
    'TestOpenAPISpecConcurrentAuthorization',
  ]);
  let invalidOutput = false;
  await run(
    env.GO || 'go',
    [
      'test',
      '-race',
      '-count=1',
      '-json',
      '-run',
      '^TestOpenAPISpecConcurrent',
      './internal/httpapi',
    ],
    { env, stdio: ['inherit', 'pipe', 'inherit'] },
    (line) => {
      try {
        const event = JSON.parse(line);
        if (typeof event.Output === 'string')
          process.stdout.write(event.Output);
        if (
          event.Package === 'issue-desk-intake/server/internal/httpapi' &&
          event.Action === 'pass'
        )
          missing.delete(event.Test);
      } catch {
        console.error(line);
        invalidOutput = true;
      }
    },
  );
  if (invalidOutput) throw new Error('GoテストのJSON出力を読み取れません。');
  if (missing.size)
    throw new Error(
      `OpenAPI race検証でPASSを確認できません: ${[...missing].join(', ')}`,
    );
} catch (error) {
  console.error(error.message);
  process.exitCode = 1;
} finally {
  if (directory) await rm(directory, { recursive: true, force: true });
}
