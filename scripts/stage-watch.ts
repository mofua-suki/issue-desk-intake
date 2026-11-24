// 段取り（長い処理）を包んで終わりを調べ、失敗・0枚・遅れを同じセッションと受付の要対応へ知らせる（docs/chat-instructions.md『Stage Watch (stage-watch)』）。
// 使い方: node scripts/stage-watch.ts --name <名前> [--expect-end <ISO8601>] [--grace-min <分>] [--outputs <フォルダー> --min-outputs <枚数>] -- <コマンド> [引数…]
// 接続は desk-ops.ts の notice と同じ desk-client.ts の connection・openDesk（環境変数 ISSUE_DESK_URL・ISSUE_DESK_TOKEN_FILE か intake.remoteUrl の設定）。トークン・URLは出さない。
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawn } from 'node:child_process';
import { openDesk, connection, configuredRemote } from './desk-client.ts';
import { invokedAs } from './lib/invoked.ts';

const usage = `使い方: node scripts/stage-watch.ts --name <名前> [--expect-end <ISO8601>] [--grace-min <分>] [--outputs <フォルダー> --min-outputs <枚数>] -- <コマンド> [引数…]
  --name         必須。英数字と . _ - の64文字以内
  --expect-end   終了の予定時刻（タイムゾーン付きのISO8601）
  --grace-min    予定時刻からの猶予（0以上の整数の分。既定10）
  --outputs      出力のフォルダー（--min-outputs と対）
  --min-outputs  出力の最小の枚数（0以上の整数。--outputs と対）
終了コード: 子の終了コード / 3 は出力が足りない / 2 は引数の誤り`;

type Anomaly = 'exit' | 'empty' | 'late' | 'overdue';
type Args = {
  name: string;
  expectEnd: string | null;
  graceMin: number;
  outputs: string | null;
  minOutputs: number | null;
  command: string[];
};
type Notice = {
  kind: 'stage-watch';
  name: string;
  anomaly: Anomaly;
  exitCode: number | null;
  outputs: number | null;
  lateMin: number | null;
  startedAt: string;
};
type Child = { done: Promise<number | null> };
export type Deps = {
  now: () => number;
  countFiles: (dir: string) => number;
  spawnChild: (command: string, args: string[]) => Child;
  setTimer: (fn: () => void, ms: number) => unknown;
  clearTimer: (handle: unknown) => void;
  send: (notice: Notice) => Promise<void>;
  writeResult: (name: string, result: object) => void;
  removeResult: (name: string) => void;
  log: (line: string) => void;
  warn: (line: string) => void;
};

const isoPattern =
  /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2}(\.\d+)?)?(Z|[+-]\d{2}:\d{2})$/;
const wholeNumber = (value: string) => /^\d{1,7}$/.test(value);
const valueFlags = [
  '--name',
  '--expect-end',
  '--grace-min',
  '--outputs',
  '--min-outputs',
];

// 引数の誤りは Error（メッセージを使い方と一緒に出して終了コード2）。
export function parseArgs(argv: string[]): Args {
  const sep = argv.indexOf('--');
  if (sep < 0 || sep === argv.length - 1)
    throw Error('-- の後に実行するコマンドを指定してください。');
  const given = new Map<string, string>();
  const head = argv.slice(0, sep);
  for (let i = 0; i < head.length; i++) {
    const flag = head[i];
    if (!valueFlags.includes(flag))
      throw Error(`引数を確認してください: ${flag}`);
    if (given.has(flag)) throw Error(`${flag} が重複しています。`);
    const value = head[++i];
    if (value === undefined || value === '')
      throw Error(`${flag} の値がありません。`);
    given.set(flag, value);
  }
  const name = given.get('--name');
  if (name === undefined) throw Error('--name は必須です。');
  if (!/^[A-Za-z0-9._-]{1,64}$/.test(name))
    throw Error('--name は英数字と . _ - の64文字以内にしてください。');
  const expectEnd = given.get('--expect-end') ?? null;
  if (
    expectEnd !== null &&
    (!isoPattern.test(expectEnd) || Number.isNaN(Date.parse(expectEnd)))
  )
    throw Error('--expect-end はタイムゾーン付きのISO8601にしてください。');
  const grace = given.get('--grace-min') ?? '10';
  if (!wholeNumber(grace))
    throw Error('--grace-min は0以上の整数にしてください。');
  const outputs = given.get('--outputs') ?? null;
  const min = given.get('--min-outputs') ?? null;
  if ((outputs === null) !== (min === null))
    throw Error('--outputs と --min-outputs は両方指定してください。');
  if (min !== null && !wholeNumber(min))
    throw Error('--min-outputs は0以上の整数にしてください。');
  return {
    name,
    expectEnd,
    graceMin: Number(grace),
    outputs,
    minOutputs: min === null ? null : Number(min),
    command: argv.slice(sep + 1),
  };
}

const reasons: Record<
  Anomaly,
  (
    name: string,
    n: {
      exitCode: number | null;
      outputs: number | null;
      lateMin: number | null;
    },
    min: number | null,
  ) => string
> = {
  exit: (name, n) =>
    `段取り ${name} が終了コード ${n.exitCode} で終わりました。`,
  empty: (name, n, min) =>
    `段取り ${name} の出力が ${n.outputs}枚で、必要な ${min}枚に足りません。`,
  late: (name, n) =>
    `段取り ${name} が予定時刻より ${n.lateMin}分遅れて終わりました。`,
  overdue: (name, n) =>
    `段取り ${name} が予定時刻を ${n.lateMin}分過ぎても実行中です（子は止めません）。`,
};

// 見張りの本体。戻り値は終了コード（子の終了コード、出力不足は3、引数の誤りは2）。
export async function runStageWatch(
  argv: string[],
  deps: Deps,
): Promise<number> {
  let args: Args;
  try {
    args = parseArgs(argv);
  } catch (e) {
    deps.warn(`${(e as Error).message}\n${usage}`);
    return 2;
  }
  const startedMs = deps.now();
  const startedAt = new Date(startedMs).toISOString();
  const expectMs = args.expectEnd === null ? null : Date.parse(args.expectEnd);
  const deadline = expectMs === null ? null : expectMs + args.graceMin * 60_000;
  const anomalies: { kind: Anomaly; lateMin: number | null; sent: boolean }[] =
    [];
  const sends: Promise<void>[] = [];
  const notify = (
    anomaly: Anomaly,
    exitCode: number | null,
    outputs: number | null,
    lateMin: number | null,
  ) => {
    const note = { exitCode, outputs, lateMin };
    deps.log(reasons[anomaly](args.name, note, args.minOutputs));
    const entry = { kind: anomaly, lateMin, sent: false };
    anomalies.push(entry);
    sends.push(
      deps
        .send({
          kind: 'stage-watch',
          name: args.name,
          anomaly,
          exitCode,
          outputs,
          lateMin,
          startedAt,
        })
        .then(
          () => {
            entry.sent = true;
          },
          () =>
            deps.warn(
              `警告: ${anomaly} の通知を受付へ送れませんでした。結果ファイルを確認してください。`,
            ),
        ),
    );
  };

  let finished = false;
  let overdueSent = false;
  let timer: unknown = null;
  const lateMinutes = (at: number) =>
    Math.max(0, Math.floor((at - (expectMs as number)) / 60_000));
  // 既定のタイマーは長い待ちを分割するため早く発火しうる。期限前なら残り時間で掛け直す。
  const arm = (ms: number) => {
    timer = deps.setTimer(() => {
      if (finished || overdueSent) return;
      const nowMs = deps.now();
      if (nowMs < (deadline as number)) {
        arm((deadline as number) - nowMs);
        return;
      }
      overdueSent = true;
      notify('overdue', null, null, lateMinutes(nowMs));
    }, ms);
  };
  if (deadline !== null) arm(Math.max(0, deadline - startedMs));

  let code: number | null;
  try {
    code = await deps.spawnChild(args.command[0], args.command.slice(1)).done;
  } catch {
    deps.warn('警告: コマンドを起動できませんでした。');
    code = 127;
  }
  finished = true;
  if (timer !== null) deps.clearTimer(timer);
  const endedMs = deps.now();
  const exitCode = code ?? 1;
  const outputs = args.outputs === null ? null : deps.countFiles(args.outputs);
  let result = exitCode;
  if (exitCode !== 0) notify('exit', exitCode, outputs, null);
  else if (outputs !== null && outputs < (args.minOutputs as number)) {
    notify('empty', 0, outputs, null);
    result = 3;
  } else if (deadline !== null && endedMs > deadline)
    notify('late', 0, outputs, lateMinutes(endedMs));
  await Promise.all(sends);
  if (anomalies.length > 0) {
    try {
      deps.writeResult(args.name, {
        name: args.name,
        kinds: anomalies.map((a) => a.kind),
        anomalies,
        exitCode: result,
        outputs,
        expectEnd: args.expectEnd,
        endedAt: new Date(endedMs).toISOString(),
        sent: anomalies.every((a) => a.sent),
      });
    } catch {
      deps.warn('警告: 結果ファイルを書けませんでした。');
    }
  } else {
    // 異常の無い終了では、同じ名前の前回の結果ファイルを残さない（異常と誤認させない）。
    try {
      deps.removeResult(args.name);
    } catch {
      deps.warn('警告: 前回の結果ファイルを消せませんでした。');
    }
  }
  return result;
}

// 結果ファイルの既定の書き込み・削除。rename に失敗したら一時ファイルを消してから例外を投げ直す。
export function resultFiles(
  dir: string,
): Pick<Deps, 'writeResult' | 'removeResult'> {
  return {
    writeResult: (name, result) => {
      fs.mkdirSync(dir, { recursive: true });
      const file = path.join(dir, `${name}.json`);
      const tmp = `${file}.${process.pid}.tmp`;
      try {
        fs.writeFileSync(tmp, JSON.stringify(result, null, 2) + '\n');
        fs.renameSync(tmp, file);
      } catch (e) {
        fs.rmSync(tmp, { force: true });
        throw e;
      }
    },
    removeResult: (name) => {
      try {
        fs.unlinkSync(path.join(dir, `${name}.json`));
      } catch (e) {
        if ((e as NodeJS.ErrnoException).code !== 'ENOENT') throw e;
      }
    },
  };
}

function defaultDeps(env: NodeJS.ProcessEnv = process.env): Deps {
  const dir =
    env.ISSUE_DESK_STAGE_RESULTS_DIR ||
    path.join(os.homedir(), '.issue-desk', 'stage-watch');
  if (!path.isAbsolute(dir))
    throw Error('結果フォルダーは絶対パスで指定してください。');
  return {
    ...resultFiles(dir),
    now: () => Date.now(),
    countFiles: (target) => {
      try {
        return fs
          .readdirSync(target, { withFileTypes: true })
          .filter((e) => e.isFile()).length;
      } catch {
        return 0;
      }
    },
    spawnChild: (command, args) => ({
      done: new Promise((resolve, reject) => {
        const child = spawn(command, args, { stdio: 'inherit', shell: false });
        child.on('error', reject);
        child.on('close', (c) => resolve(c));
      }),
    }),
    setTimer: (fn, ms) => {
      const handle = setTimeout(fn, Math.min(ms, 2_000_000_000));
      return handle;
    },
    clearTimer: (handle) => clearTimeout(handle as NodeJS.Timeout),
    send: async (notice) => {
      const conn = connection({} as any, env, configuredRemote);
      const { call, close } = await openDesk(conn);
      try {
        await call('POST', '/api/v1/notices', notice);
      } finally {
        close();
      }
    },
    log: (line) => console.log(line),
    warn: (line) => console.error(line),
  };
}

// コマンドとして実行したときだけ動く（実体パスを照合する）。
if (invokedAs(import.meta.url, process.argv[1], { ignoreCase: true }))
  runStageWatch(process.argv.slice(2), defaultDeps()).then(
    (code) => (process.exitCode = code),
    () => {
      console.error('段取りの見張りに失敗しました。');
      process.exitCode = 1;
    },
  );
