import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { startChromium } from './chromium.ts';

test(
  'ブラウザの親が終了してもpipeを継承した子孫を後片付けする',
  { timeout: 10000 },
  async (t) => {
    const directory = fs.mkdtempSync(
      path.join(os.tmpdir(), 'chromium-cleanup-'),
    );
    t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
    const executable = path.join(directory, 'browser');
    fs.writeFileSync(
      executable,
      '#!/bin/sh\nsleep 60 &\nprintf "DevTools listening on ws://127.0.0.1:12345/devtools/browser/test\\n" >&2\nexit 0\n',
      { mode: 0o700 },
    );
    let browser: Awaited<ReturnType<typeof startChromium>> | undefined;
    await t.test('子孫のpipeが残る親を起動する', async (sub) => {
      browser = await startChromium(sub, executable, directory, 'about:blank');
      assert.equal(browser.endpoint.port, '12345');
      await new Promise<void>((resolve) => {
        if (browser!.child.exitCode !== null) resolve();
        else browser!.child.once('exit', () => resolve());
      });
    });
    assert.ok(browser);
    const result = await browser.exited;
    assert.match(result.message, /ブラウザが終了しました/);
  },
);

test('後片付けの失敗後もブラウザとHTTPを停止し元の失敗を残す', async (t) => {
  const { spawn } = await import('node:child_process');
  const { connect } = await import('node:net');
  const { pathToFileURL } = await import('node:url');
  const directory = fs.mkdtempSync(
    path.join(os.tmpdir(), 'cleanup-regression-'),
  );
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
  const info = path.join(directory, 'info.json');
  const executable = path.join(directory, 'browser');
  fs.writeFileSync(
    executable,
    '#!/bin/sh\nprintf "DevTools listening on ws://127.0.0.1:12345/devtools/browser/test\\n" >&2\nexec sleep 60\n',
    { mode: 0o700 },
  );
  const fixture = path.join(directory, 'fixture.mjs');
  fs.writeFileSync(
    fixture,
    `import test from 'node:test';
import http from 'node:http';
import fs from 'node:fs';
import { cleanupStack, startChromium } from ${JSON.stringify(pathToFileURL(path.resolve(import.meta.dirname, 'chromium.ts')).href)};
test('injected failure', async (t) => {
  const cleanup = cleanupStack(t);
  const server = http.createServer((_req, res) => res.end('ok'));
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  cleanup(async () => {
    server.closeAllConnections();
    await new Promise((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
  });
  cleanup(() => { throw new Error('injected cleanup error'); });
  const browser = await startChromium(t, ${JSON.stringify(executable)}, ${JSON.stringify(directory)}, 'about:blank', undefined, cleanup);
  fs.writeFileSync(${JSON.stringify(info)}, JSON.stringify({pid: browser.child.pid, port: server.address().port}));
  throw new Error('original test error');
});
`,
  );
  const started = performance.now();
  const env = { ...process.env };
  delete env.NODE_TEST_CONTEXT;
  const child = spawn(process.execPath, ['--test', fixture], {
    env,
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  let output = '';
  child.stdout.on('data', (chunk: Buffer) => (output += chunk.toString()));
  child.stderr.on('data', (chunk: Buffer) => (output += chunk.toString()));
  const status = await new Promise<number | null>((resolve, reject) => {
    child.once('error', reject);
    child.once('close', resolve);
  });
  assert.equal(status, 1, output);
  assert.ok(performance.now() - started < 1800000);
  assert.match(output, /original test error/);
  assert.match(output, /injected cleanup error/);
  const { pid, port } = JSON.parse(fs.readFileSync(info, 'utf8')) as {
    pid: number;
    port: number;
  };
  assert.throws(() => process.kill(pid, 0), { code: 'ESRCH' });
  await new Promise<void>((resolve, reject) => {
    const socket = connect(port, '127.0.0.1');
    socket.once('connect', () => {
      socket.destroy();
      reject(Error('HTTPサーバーが停止していません'));
    });
    socket.once('error', (error: NodeJS.ErrnoException) => {
      if (error.code === 'ECONNREFUSED') resolve();
      else reject(error);
    });
  });
});
