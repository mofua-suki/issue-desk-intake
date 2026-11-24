import { spawn } from 'node:child_process';
import { createReadStream, createWriteStream, mkdirSync } from 'node:fs';
import { join } from 'node:path';
import { pipeline } from 'node:stream/promises';
import { createBrotliDecompress } from 'node:zlib';
import type { TestContext } from 'node:test';

const browserStartupTimeoutMs = 30000;

class BrowserStartupTimeout extends Error {}

export function cleanupStack(t: TestContext) {
  const steps: (() => void | Promise<void>)[] = [];
  t.after(async () => {
    const errors: unknown[] = [];
    for (const step of steps.reverse()) {
      try {
        await step();
      } catch (error) {
        errors.push(error);
        t.diagnostic(`後片付け: ${String(error)}`);
      }
    }
    if (errors.length)
      throw new AggregateError(errors, '後片付けに失敗しました');
  });
  return (step: () => void | Promise<void>) => steps.push(step);
}

function launch(
  executable: string,
  args: string[],
  timeoutMs: number,
  cleanup: (step: () => void | Promise<void>) => unknown,
) {
  const child = spawn(executable, args, {
    stdio: ['ignore', 'pipe', 'pipe'],
    detached: process.platform !== 'win32',
  });
  let stderr = '';
  let resolveReady: (endpoint: URL) => void;
  const ready = new Promise<URL>((resolve) => {
    resolveReady = resolve;
  });
  child.stdout.resume();
  child.stderr.on('data', (chunk: Buffer) => {
    stderr = (stderr + chunk.toString('utf8')).slice(-16000);
    const match = /DevTools listening on (ws:\/\/127\.0\.0\.1:\d+\/\S+)/.exec(
      stderr,
    );
    if (match) {
      const endpoint = new URL(match[1]);
      endpoint.protocol = 'http:';
      resolveReady(endpoint);
    }
  });
  // Resolve rather than reject so an exit during startup never produces an
  // unhandled rejection before the caller begins waiting for the UI result.
  const exited = new Promise<Error>((resolve) => {
    child.once('error', resolve);
    child.once('close', (status, signal) =>
      resolve(Error(`ブラウザが終了しました: ${status ?? signal} ${stderr}`)),
    );
  });
  cleanup(async () => {
    if (process.platform !== 'win32' && child.pid) {
      try {
        process.kill(-child.pid, 'SIGKILL');
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code !== 'ESRCH') throw error;
      }
    } else if (child.exitCode === null && child.signalCode === null)
      child.kill('SIGKILL');
    let timer: ReturnType<typeof setTimeout> | undefined;
    try {
      await Promise.race([
        exited,
        new Promise<never>((_resolve, reject) => {
          timer = setTimeout(() => {
            child.stdout.destroy();
            child.stderr.destroy();
            reject(Error('ブラウザの後片付けが5秒以内に終わりませんでした'));
          }, 5000);
        }),
      ]);
    } finally {
      clearTimeout(timer);
    }
  });
  let startupTimer: ReturnType<typeof setTimeout> | undefined;
  const startup = new Promise<never>((_resolve, reject) => {
    startupTimer = setTimeout(() => {
      reject(
        new BrowserStartupTimeout(
          `ブラウザの起動を ${timeoutMs}ms 以内に確認できませんでした: ${stderr}`,
        ),
      );
    }, timeoutMs);
  });
  return {
    child,
    exited,
    stderr: () => stderr,
    ready: Promise.race([
      ready,
      exited.then((error) => {
        throw error;
      }),
      startup,
    ]).finally(() => clearTimeout(startupTimer)),
  };
}

export async function startChromium(
  t: TestContext,
  executable: string | undefined,
  directory: string,
  url: string,
  startupTimeoutMs = browserStartupTimeoutMs,
  cleanup: (step: () => void | Promise<void>) => unknown = (step) =>
    t.after(step),
) {
  const args = [
    '--headless',
    '--no-sandbox',
    '--disable-gpu',
    '--disable-background-networking',
    '--disable-dev-shm-usage',
    '--disable-notifications',
    '--no-first-run',
    '--no-default-browser-check',
    '--remote-debugging-port=0',
    '--remote-allow-origins=*',
    '--user-data-dir=' + join(directory, 'profile'),
    url,
  ];
  const bundled = async () => {
    const fallback = join(directory, 'headless-shell');
    mkdirSync(fallback);
    const binary = join(fallback, 'chromium');
    // 展開は試験ごとの領域へ限定し、パッケージ付属の起動フラグは使わない。
    const compressed = new URL(
      '../bin/chromium.br',
      import.meta.resolve('@sparticuz/chromium'),
    );
    await pipeline(
      createReadStream(compressed),
      createBrotliDecompress(),
      createWriteStream(binary, { mode: 0o700 }),
    );
    const browser = launch(
      binary,
      [...args, '--no-zygote'],
      startupTimeoutMs,
      cleanup,
    );
    return { ...browser, endpoint: await browser.ready };
  };
  if (!executable && process.platform === 'linux' && process.arch === 'x64')
    return bundled();
  const browser = launch(
    executable ?? 'chromium',
    args,
    startupTimeoutMs,
    cleanup,
  );
  try {
    return { ...browser, endpoint: await browser.ready };
  } catch (error) {
    if (error instanceof BrowserStartupTimeout) throw error;
    // Full Chrome requires an AF_UNIX listener for its profile singleton.
    // Headless Shell runs the same rendering/JS engine without that desktop
    // profile service. Other startup errors remain failures of the chosen binary.
    const stderr = browser.stderr();
    if (
      process.platform !== 'linux' ||
      process.arch !== 'x64' ||
      !stderr.includes('process_singleton_posix.cc') ||
      !stderr.includes('socket() failed: Operation not permitted')
    )
      throw error;
    t.diagnostic(
      'Unixソケットが禁止されているため、固定版Chromium Headless Shellで同じ画面検証を実行します。',
    );
    return bundled();
  }
}
