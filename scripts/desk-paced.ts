import fs from 'node:fs/promises';
import path from 'node:path';
import os from 'node:os';
import { createHash, randomUUID } from 'node:crypto';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { setTimeout as delay } from 'node:timers/promises';
import { protectDeskCheckout } from './lib/desk-checkout-use.ts';

const scripts = path.dirname(fileURLToPath(import.meta.url));
const blocked =
  'desk-paced: 状態を確認できません。送信IDと子CLIの終了を照合してください。記録・排他を自動解除しません。';
const maxNs = BigInt(Number.MAX_SAFE_INTEGER) * 1_000_000n;

function parseInterval(text: string) {
  const m = /^\+?(?:(\d+)(?:\.(\d*))?|\.(\d+))(?:e([+-]?\d+))?$/i.exec(text);
  if (
    !m ||
    text.length > 100 ||
    !Number.isFinite(Number(text)) ||
    Number(text) <= 0
  )
    throw Error('interval は安全に表現できる有限の正数（秒）にしてください。');
  const fraction = m[2] ?? m[3] ?? '';
  const exponent = Number(m[4] || 0) - fraction.length + 9;
  if (Math.abs(exponent) > 100) throw Error('interval の桁数が大きすぎます。');
  const digits = BigInt((m[1] || '0') + fraction);
  const ns =
    exponent >= 0
      ? digits * 10n ** BigInt(exponent)
      : (digits + 10n ** BigInt(-exponent) - 1n) / 10n ** BigInt(-exponent);
  if (ns <= 0n || ns > maxNs)
    throw Error('interval が安全な範囲を超えています。');
  const decimal = digits.toString();
  const significant = decimal.replace(/0+$/, '');
  return {
    ns: ns.toString(),
    interval: `${significant}e${exponent - 9 + decimal.length - significant.length}`,
  };
}

export function parsePaced(argv: string[]) {
  const split = argv.indexOf('--');
  const options: Record<string, string> = {};
  if (split < 0 || split % 2)
    throw Error(
      '--sequence <識別子> --interval <秒> -- <CLI名> <引数…> が必要です。',
    );
  for (let i = 0; i < split; i += 2) {
    const key = argv[i];
    if (
      !['--sequence', '--interval'].includes(key) ||
      options[key] !== undefined ||
      !argv[i + 1]
    )
      throw Error('待機オプションが不正です。');
    options[key] = argv[i + 1];
  }
  const sequence = options['--sequence'];
  if (!sequence || !/^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/.test(sequence))
    throw Error(
      'sequence は英数字から始まる128文字以内の英数字・点・ハイフン・下線にしてください。',
    );
  const interval = parseInterval(options['--interval'] || '');
  const [command, ...args] = argv.slice(split + 1);
  if (!['operation', 'reply-issue.ts', 'create-issue.ts'].includes(command))
    throw Error(
      '対象は operation（POST /api/comment）、reply-issue.ts、create-issue.ts --draft のみです。',
    );
  if (command === 'operation') {
    const flags = new Map<string, string>();
    if (args.length !== 6)
      throw Error(
        'operation は --method POST --path /api/comment --body-file <絶対パス> を指定してください。',
      );
    for (let i = 0; i < args.length; i += 2) {
      if (
        !['--method', '--path', '--body-file'].includes(args[i]) ||
        flags.has(args[i])
      )
        throw Error('operation の引数が不正です。');
      flags.set(args[i], args[i + 1]);
    }
    if (
      flags.get('--method') !== 'POST' ||
      flags.get('--path') !== '/api/comment' ||
      !path.isAbsolute(flags.get('--body-file') || '')
    )
      throw Error(
        'operation は POST /api/comment と本文の絶対パスだけを指定できます。',
      );
  }
  // Inspect option positions, never mistake a body containing "--draft" for an option.
  if (command === 'create-issue.ts') {
    let draft = false;
    for (
      let i = 0;
      i < args.length;
      i += ['--hold', '--submit'].includes(args[i]) ? 1 : 2
    ) {
      if (
        args[i] === '--draft' &&
        /^[A-Za-z0-9][A-Za-z0-9-]*$/.test(args[i + 1] || '')
      )
        draft = true;
      if (args[i] === '--project') throw Error('新規相談は対象外です。');
    }
    if (!draft) throw Error('既存相談への回答には --draft <ID> が必要です。');
  }
  return {
    sequence,
    intervalNs: interval.ns,
    interval: interval.interval,
    command,
    args,
  };
}

type Options = ReturnType<typeof parsePaced>;
type Result = { code: number | null; signal: string | null };
type RecordState = {
  version: 1;
  sequence: string;
  intervalNs: string;
  interval: string;
  phase: 'ready' | 'running' | 'finished';
  previousEnd: string | null;
  waitStarted: string | null;
  started: string | null;
  ended: string | null;
  waitedNs: string;
  result: Result | null;
};
type Store = {
  acquire(): Promise<void>;
  read(): Promise<unknown>;
  write(value: RecordState): Promise<void>;
  release(): Promise<void>;
};
type Clock = {
  mono(): bigint;
  wall(): string;
  sleep(ms: number, signal?: AbortSignal): Promise<void>;
};
const clock: Clock = {
  mono: () => process.hrtime.bigint(),
  wall: () => new Date().toISOString(),
  sleep: async (ms, signal) => {
    await delay(ms, undefined, { signal });
  },
};

function validate(value: unknown, options: Options): RecordState {
  const s = value as RecordState;
  const time = (t: unknown) =>
    typeof t === 'string' && Number.isFinite(Date.parse(t));
  if (
    !s ||
    s.version !== 1 ||
    s.sequence !== options.sequence ||
    typeof s.intervalNs !== 'string' ||
    typeof s.interval !== 'string' ||
    !/^[1-9]\d*e-?\d+$/.test(s.interval) ||
    !/^\d+$/.test(s.intervalNs) ||
    BigInt(s.intervalNs) <= 0n ||
    BigInt(s.intervalNs) > maxNs ||
    !['ready', 'finished'].includes(s.phase) ||
    !/^\d+$/.test(s.waitedNs) ||
    (s.phase === 'ready' &&
      (s.previousEnd !== null ||
        s.waitStarted !== null ||
        s.started !== null ||
        s.ended !== null ||
        s.result !== null ||
        s.waitedNs !== '0')) ||
    (s.phase === 'finished' &&
      (!time(s.started) ||
        !time(s.ended) ||
        !s.result ||
        !time(s.waitStarted) ||
        (s.previousEnd !== null && !time(s.previousEnd)) ||
        (s.previousEnd !== null && BigInt(s.waitedNs) < BigInt(s.intervalNs)) ||
        !(
          (Number.isInteger(s.result.code) &&
            s.result.code >= 0 &&
            s.result.code <= 255 &&
            s.result.signal === null) ||
          (s.result.code === null &&
            typeof s.result.signal === 'string' &&
            s.result.signal.length > 0)
        )))
  )
    throw Error(`${blocked} 実行中・破損記録を確認してください。`);
  return s;
}

// The exclusive directory is never reclaimed by PID or wall-clock age. A crash leaves evidence.
function filePacedStore(
  sequence: string,
  root = path.join(os.homedir(), '.issue-desk', 'paced'),
): Store {
  root = path.resolve(root);
  const dir = path.join(
    root,
    createHash('sha256').update(sequence).digest('hex'),
  );
  const lock = dir + '.lock';
  const state = path.join(dir, 'state.json');
  let fresh = false;
  async function syncDir(directory = dir) {
    const handle = await fs.open(directory, 'r');
    try {
      await handle.sync();
    } finally {
      await handle.close();
    }
  }
  async function write(value: RecordState) {
    const temp = path.join(dir, `${randomUUID()}.tmp`);
    const handle = await fs.open(temp, 'wx', 0o600);
    try {
      try {
        await handle.writeFile(JSON.stringify(value) + '\n');
        await handle.sync();
      } catch (error) {
        await handle.close().catch(() => {});
        throw error;
      }
      await handle.close();
      await fs.rename(temp, state);
      await syncDir();
    } finally {
      // Only reclaim the file this call created; keep the committed state and lock.
      // Cleanup is best effort and must never replace the persistence error.
      await fs.unlink(temp).catch(() => {});
    }
  }
  return {
    async acquire() {
      const created = await fs.mkdir(root, { recursive: true, mode: 0o700 });
      if (created) {
        for (let directory = root; ; directory = path.dirname(directory)) {
          await syncDir(directory);
          if (directory === path.dirname(created)) break;
        }
      }
      try {
        await fs.mkdir(lock, { mode: 0o700 });
      } catch {
        throw Error(`${blocked} 排他が使用中、または所有者不明です。`);
      }
      try {
        await fs.mkdir(dir, { mode: 0o700 });
        fresh = true;
      } catch (error) {
        if (error.code !== 'EEXIST') throw error;
      }
      await syncDir(root);
      if (
        !(await fs.lstat(dir)).isDirectory() ||
        (await fs.lstat(dir)).isSymbolicLink()
      )
        throw Error(blocked);
      // Failure here intentionally keeps the lock; never turn failed reads into first use.
      await syncDir();
    },
    async read() {
      try {
        if (
          !(await fs.lstat(state)).isFile() ||
          (await fs.lstat(state)).isSymbolicLink()
        )
          throw Error(blocked);
        const value = JSON.parse(await fs.readFile(state, 'utf8'));
        if (value === null) throw Error(blocked);
        fresh = false;
        return value;
      } catch (error) {
        if (error.code === 'ENOENT' && fresh) {
          fresh = false;
          return null;
        }
        throw Error(`${blocked} 永続記録の読取に失敗しました。`);
      }
    },
    write,
    async release() {
      await fs.rmdir(lock);
      await syncDir(root);
    },
  };
}

export function executePaced(
  command: string,
  args: string[],
  signal?: AbortSignal,
  spawnChild: typeof spawn = spawn,
): Promise<Result> {
  return new Promise((resolve, reject) => {
    signal?.throwIfAborted();
    const binary =
      command === 'operation' ? '/usr/local/bin/desk' : process.execPath;
    const childArgs =
      command === 'operation'
        ? ['operation', '--config', '/state/config.local.json', ...args]
        : [path.join(scripts, command), ...args];
    const child = spawnChild(binary, childArgs, {
      shell: false,
      stdio: 'inherit',
    });
    const stop = () => {
      child.kill('SIGTERM');
    };
    signal?.addEventListener('abort', stop, { once: true });
    if (signal?.aborted) stop();
    child.once('error', reject);
    child.once('close', (code, childSignal) => {
      signal?.removeEventListener('abort', stop);
      resolve({ code, signal: childSignal });
    });
  });
}

export async function runPaced(
  options: Options,
  {
    store = filePacedStore(options.sequence),
    time = clock,
    execute = executePaced,
    signal,
    protect = () => protectDeskCheckout(path.dirname(scripts)),
  }: {
    store?: Store;
    time?: Clock;
    execute?: typeof executePaced;
    signal?: AbortSignal;
    protect?: () => ReturnType<typeof protectDeskCheckout>;
  } = {},
): Promise<Result> {
  signal?.throwIfAborted();
  const checkoutUse = await protect();
  let acquired = false;
  let safe = false;
  try {
    await store.acquire();
    acquired = true;
    const raw = await store.read();
    let state: RecordState;
    if (raw === null) {
      state = {
        version: 1,
        sequence: options.sequence,
        intervalNs: options.intervalNs,
        interval: options.interval,
        phase: 'ready',
        previousEnd: null,
        waitStarted: null,
        started: null,
        ended: null,
        waitedNs: '0',
        result: null,
      };
      await store.write(state);
    } else state = validate(raw, options);
    safe = true;
    if (
      state.intervalNs !== options.intervalNs ||
      state.interval !== options.interval
    )
      throw Error('同じ系列の interval は変更できません。');
    const waitStarted = time.wall();
    const begin = time.mono();
    const required =
      state.phase === 'finished' ? BigInt(options.intervalNs) : 0n;
    let elapsed = 0n;
    // Restart always waits the entire interval. Wall-clock changes cannot grant credit.
    while (elapsed < required) {
      signal?.throwIfAborted();
      const remaining = required - elapsed;
      await time.sleep(
        Number(
          (remaining + 999_999n) / 1_000_000n > 1000n
            ? 1000n
            : (remaining + 999_999n) / 1_000_000n,
        ),
        signal,
      );
      elapsed = time.mono() - begin;
      if (elapsed < 0n) throw Error('単調時計が巻き戻りました。起動しません。');
    }
    signal?.throwIfAborted();
    state = {
      ...state,
      previousEnd: state.ended,
      waitStarted,
      started: time.wall(),
      ended: null,
      waitedNs: elapsed.toString(),
      phase: 'running',
      result: null,
    };
    safe = false;
    await store.write(state);
    signal?.throwIfAborted();
    const result = await execute(options.command, options.args, signal);
    if (
      !result ||
      !(
        (Number.isInteger(result.code) &&
          result.code >= 0 &&
          result.code <= 255 &&
          result.signal === null) ||
        (result.code === null &&
          typeof result.signal === 'string' &&
          result.signal.length > 0)
      )
    )
      throw Error(blocked);
    await store.write({
      ...state,
      phase: 'finished',
      ended: time.wall(),
      result,
    });
    safe = true;
    return result;
  } finally {
    if (safe) await store.release();
    if (safe || !acquired) await checkoutUse.release();
  }
}

async function pacedMain(argv: string[], run: typeof runPaced = runPaced) {
  const controller = new AbortController();
  let stopped: string | null = null;
  const stop = (name: string) => {
    stopped = name;
    controller.abort();
  };
  const interrupt = () => stop('SIGINT');
  const terminate = () => stop('SIGTERM');
  process.on('SIGINT', interrupt);
  process.on('SIGTERM', terminate);
  try {
    const result = await run(parsePaced(argv), { signal: controller.signal });
    if (stopped || result.signal) {
      console.error(
        `desk-paced: シグナル終了 ${stopped || result.signal}。自動再送しません。`,
      );
      return stopped === 'SIGINT' || result.signal === 'SIGINT' ? 130 : 143;
    }
    return result.code;
  } catch (error) {
    console.error(
      `desk-paced: ガード失敗または停止。${error.message} ${blocked}`,
    );
    return stopped === 'SIGINT' ? 130 : stopped === 'SIGTERM' ? 143 : 1;
  } finally {
    process.off('SIGINT', interrupt);
    process.off('SIGTERM', terminate);
  }
}

if (
  process.argv[1] &&
  path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  process.exitCode = await pacedMain(process.argv.slice(2));
}
