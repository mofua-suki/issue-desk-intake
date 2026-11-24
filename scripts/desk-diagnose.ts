// 読取専用の診断。NAS: ~/.issue-desk/desk desk-diagnose.ts --samples 5 --interval 2
import {
  connectionArgs,
  connection,
  connectionUsage,
  configuredRemote,
  openDesk,
} from './desk-client.ts';
import { invokedAs } from './lib/invoked.ts';

const usage = `使い方: desk-diagnose.ts [--samples 回数] [--interval 秒] [--cpu 秒] [--goroutines] [--activity]
既定は5回、間隔2秒。CPUは1〜30秒。結果はJSONで表示します。\n${connectionUsage}`;

/** parseArgs が返す引数。--help のときは help だけ。 */
interface DiagnoseArgs {
  samples?: number;
  interval?: number;
  cpu?: number | null;
  goroutines?: boolean;
  activity?: boolean;
  help?: boolean;
}
export function parseArgs(argv: string[]): DiagnoseArgs {
  const args: DiagnoseArgs = {
    samples: 5,
    interval: 2,
    cpu: null,
    goroutines: false,
    activity: false,
  };
  const seen = new Set();
  for (let i = 0; i < argv.length; i++) {
    const flag = argv[i];
    if (flag === '--help') return { help: true };
    if (seen.has(flag)) throw Error(`重複した引数: ${flag}\n${usage}`);
    seen.add(flag);
    if (flag === '--activity') {
      args.activity = true;
      continue;
    }
    if (flag === '--goroutines') {
      args.goroutines = true;
      continue;
    }
    if (!['--samples', '--interval', '--cpu'].includes(flag))
      throw Error(`引数が不正です。\n${usage}`);
    const value = argv[++i];
    if (value === undefined || !/^(?:\d+)(?:\.\d+)?$/.test(value))
      throw Error(`${flag} の値が不正です。\n${usage}`);
    const number = Number(value);
    if (
      !Number.isFinite(number) ||
      (flag === '--interval'
        ? number < 0 || number > 86400
        : !Number.isSafeInteger(number) ||
          number < 1 ||
          (flag === '--cpu' && number > 30))
    )
      throw Error(`${flag} の値が範囲外です。\n${usage}`);
    args[flag.slice(2)] = number;
  }
  return args;
}

function validSample(s) {
  return (
    s &&
    typeof s.color === 'string' &&
    typeof s.instanceId === 'string' &&
    s.instanceId.length > 0 &&
    Array.isArray(s.queries) &&
    Array.isArray(s.connections) &&
    Number.isFinite(s.maxConnections) &&
    s.queries.every(
      (q) =>
        q &&
        typeof q.state === 'string' &&
        Number.isFinite(q.seconds) &&
        q.seconds >= 0 &&
        (q.waitType === null || typeof q.waitType === 'string') &&
        (q.query === undefined || typeof q.query === 'string'),
    ) &&
    s.connections.every(
      (c) =>
        c &&
        typeof c.state === 'string' &&
        Number.isFinite(c.count) &&
        c.count >= 0,
    )
  );
}

function validActivity(value) {
  return (
    ['blue', 'green', 'unknown'].includes(value.color) &&
    typeof value.startedAt === 'string' &&
    !Number.isNaN(Date.parse(value.startedAt)) &&
    Array.isArray(value.hours) &&
    value.hours.length <= 24 &&
    value.hours.every(
      (h) =>
        h &&
        typeof h.start === 'string' &&
        !Number.isNaN(Date.parse(h.start)) &&
        Array.isArray(h.rows) &&
        h.rows.length <= 1000 &&
        h.rows.every(
          (r) =>
            r &&
            ['periodic', 'http'].includes(r.kind) &&
            typeof r.name === 'string' &&
            ['count', 'failures'].every(
              (k) => Number.isSafeInteger(r[k]) && r[k] >= 0,
            ) &&
            r.failures <= r.count &&
            ['totalMilliseconds', 'maxMilliseconds'].every(
              (k) => Number.isFinite(r[k]) && r[k] >= 0,
            ) &&
            (r.kind !== 'http' ||
              (typeof r.method === 'string' &&
                typeof r.status === 'string' &&
                ['pc', 'cli', 'session', 'none'].includes(r.credential))),
        ),
    )
  );
}

// 機密が入り得る例外本文は出力しない。本文のないクエリは形が不明なものとして分ける。
export async function diagnose(
  call,
  args: DiagnoseArgs,
  {
    sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms)),
    log = console.log,
  } = {},
) {
  const groups = new Map(),
    samples = [],
    failures = [],
    profiles = [];
  let previous = null,
    changes = 0;
  for (let i = 1; i <= args.samples; i++) {
    if (i > 1) await sleep(args.interval * 1000);
    try {
      const s = await call('GET', '/api/diagnostics/db');
      if (!validSample(s)) throw Error('invalid response');
      const key = JSON.stringify([s.color, s.instanceId]);
      if (previous !== null && previous !== key) changes++;
      previous = key;
      if (!groups.has(key))
        groups.set(key, {
          color: s.color,
          instanceId: s.instanceId,
          queries: new Map(),
        });
      const group = groups.get(key);
      samples.push({
        sample: i,
        color: s.color,
        instanceId: s.instanceId,
        connections: s.connections,
        maxConnections: s.maxConnections,
      });
      for (const q of s.queries) {
        const query = q.query || null;
        const queryKey = JSON.stringify(query);
        if (!group.queries.has(queryKey))
          group.queries.set(queryKey, {
            query,
            count: 0,
            totalSeconds: 0,
            maxSeconds: 0,
            states: {},
            waitTypes: {},
          });
        const row = group.queries.get(queryKey);
        row.count++;
        row.totalSeconds += q.seconds;
        row.maxSeconds = Math.max(row.maxSeconds, q.seconds);
        // Maps avoid special object keys from the response.
        for (const [field, value] of [
          ['states', q.state],
          ['waitTypes', q.waitType ?? 'なし'],
        ]) {
          if (!Object.hasOwn(row[field], value))
            Object.defineProperty(row[field], value, {
              value: 0,
              writable: true,
              enumerable: true,
            });
          row[field][value]++;
        }
      }
    } catch (e) {
      failures.push({
        sample: i,
        status: Number.isInteger(e?.status) ? e.status : null,
        error: '標本を読み取れませんでした',
      });
    }
  }
  for (const [enabled, route, name] of [
    [
      args.cpu !== null && args.cpu !== undefined,
      `/api/diagnostics/cpu?seconds=${args.cpu}`,
      'cpu',
    ],
    [args.goroutines, '/api/diagnostics/goroutines', 'goroutines'],
    [args.activity, '/api/diagnostics/activity', 'activity'],
  ]) {
    if (!enabled) continue;
    try {
      const value = await call('GET', route);
      if (
        !value ||
        typeof value !== 'object' ||
        Array.isArray(value) ||
        typeof value.instanceId !== 'string' ||
        !value.instanceId ||
        typeof value.color !== 'string'
      )
        throw Error('invalid response');
      if (name === 'activity' && !validActivity(value))
        throw Error('invalid response');
      profiles.push({ type: name, result: value });
    } catch (e) {
      failures.push({
        type: name,
        status: Number.isInteger(e?.status) ? e.status : null,
        error: '診断を読み取れませんでした',
      });
    }
  }
  const instances = [...groups.values()].map((g) => ({
    ...g,
    queries: [...g.queries.values()].map((q) => ({
      ...q,
      averageSeconds: q.totalSeconds / q.count,
    })),
  }));
  const failedSamples = failures.filter((f) => f.sample !== undefined).length;
  log(
    JSON.stringify(
      {
        requestedSamples: args.samples,
        failedSamples,
        instanceChanges: changes,
        samples,
        instances,
        profiles,
        failures,
      },
      null,
      2,
    ),
  );
  return failedSamples === args.samples || failures.length > failedSamples
    ? 1
    : 0;
}

export async function runCommand(
  argv: string[],
  {
    env = process.env,
    configured = configuredRemote,
    open = openDesk,
    log = console.log,
    sleep,
  }: {
    env?: NodeJS.ProcessEnv;
    configured?: () => any;
    open?: typeof openDesk;
    log?: (line: string) => void;
    sleep?: (ms: number) => Promise<unknown>;
  } = {},
) {
  const parsed = connectionArgs(argv, usage);
  const args = parseArgs(parsed.argv);
  if (args.help) {
    log(usage);
    return 0;
  }
  const conn = connection(parsed, env, configured);
  const { call, close } = await open(conn);
  try {
    return await diagnose(call, args, { log, ...(sleep ? { sleep } : {}) });
  } finally {
    await close();
  }
}

if (invokedAs(import.meta.url))
  runCommand(process.argv.slice(2)).then(
    (code) => {
      process.exitCode = code;
    },
    () => {
      console.error(
        '診断を開始できませんでした。接続設定と引数を確認してください。',
      );
      process.exitCode = 1;
    },
  );
