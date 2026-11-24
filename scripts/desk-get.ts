import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import {
  connectionArgs,
  connection,
  openDesk,
  configuredRemote,
} from './desk-client.ts';
import { forbiddenHint } from './desk-alerts.ts';

const DEFAULT_MAX_BYTES = 1024 * 1024;
const MAX_BYTES = 16 * 1024 * 1024;
const usage = '使い方: desk-get.ts GET /api/経路 [--max-bytes 1〜16777216]';

export function parseArgs(argv) {
  if (argv[0] !== 'GET' || !argv[1])
    throw Error(`GET と経路を指定してください。${usage}`);
  const route = argv[1];
  let decoded = route;
  try {
    for (let i = 0; i < 4; i++) {
      const next = decodeURIComponent(decoded);
      if (next === decoded) break;
      decoded = next;
    }
  } catch {
    throw Error('経路のエンコードが不正です。');
  }
  if (
    !route.startsWith('/api/') ||
    !decoded.startsWith('/api/') ||
    // eslint-disable-next-line no-control-regex -- 入力や表示から制御文字を除外する安全性検証。
    /\.\.|\/\/|\\|[\x00-\x20\x7f#]/.test(decoded) ||
    /%[0-9a-f]{2}/i.test(decoded) ||
    /(?:authorization|bearer|idk_|idh_)/i.test(decoded)
  )
    throw Error('経路は /api/ で始まる安全な相対パスを指定してください。');
  let maxBytes = DEFAULT_MAX_BYTES;
  if (argv.length > 2) {
    if (
      argv.length !== 4 ||
      argv[2] !== '--max-bytes' ||
      !/^[1-9]\d*$/.test(argv[3])
    )
      throw Error(usage);
    maxBytes = Number(argv[3]);
    if (!Number.isSafeInteger(maxBytes) || maxBytes > MAX_BYTES)
      throw Error(usage);
  }
  return { route, maxBytes };
}

// 認証情報を返すAPIを誤って指定した場合も資格情報を表示しない。
function safeText(value) {
  return value
    .replace(/authorization\s*[:=]\s*(?:bearer\s+)?[^\s,;]+/gi, '[非表示]')
    .replace(
      /\b(?:token|secret|password|csrf(?:[-_]?token)?|x-desk-key)\s*[:=]\s*[^\s,;]+/gi,
      '[非表示]',
    )
    .replace(/(?:Bearer\s+\S+|id[hk]_[A-Za-z0-9_-]+)/gi, '[非表示]');
}

function safeValue(value) {
  if (Array.isArray(value)) return value.map(safeValue);
  if (value && typeof value === 'object')
    return Object.fromEntries(
      Object.entries(value)
        .filter(
          ([key]) =>
            !/^(?:authorization|token|secret|password|csrf(?:[-_]?token)?|x[-_]?csrf[-_]?token|(?:x[-_]?)?desk[-_]?key)$/i.test(
              key,
            ),
        )
        .map(([key, item]) => [key, safeValue(item)]),
    );
  if (typeof value === 'string') return safeText(value);
  return value;
}

export async function deskGet(
  call,
  args,
  log = console.log,
  { remote = false } = {},
) {
  // 呼び出し側から直接渡される場合も送信前に検証する。
  const { route, maxBytes } = parseArgs([
    'GET',
    args.route,
    '--max-bytes',
    String(args.maxBytes ?? DEFAULT_MAX_BYTES),
  ]);
  try {
    const value = await call('GET', route);
    const original = JSON.stringify(value, null, 2);
    if (original === undefined) throw Error('invalid response');
    const size = Buffer.byteLength(original, 'utf8');
    const output = JSON.stringify(safeValue(value), null, 2);
    if (size > maxBytes) {
      log(
        `${route.split('?')[0]}: 応答は ${size} bytes で上限 ${maxBytes} bytes を超えました。出力していません。経路に limit・page・絞り込みの query があれば指定するか、--max-bytes を ${MAX_BYTES} 以下で上げてください。`,
      );
      return 1;
    }
    log(output);
    return 0;
  } catch (error) {
    const status = Number.isInteger(error?.status) ? error.status : null;
    const reason =
      status === 401
        ? '認証に失敗しました。'
        : status === 403
          ? '認証区分または権限が不足しています。'
          : status
            ? '受付が要求を拒否しました。'
            : '接続または応答を確認できません。';
    const message =
      typeof error?.message === 'string' ? safeText(error.message) : '';
    const guidance =
      typeof error?.recovery?.guidance === 'string'
        ? safeText(error.recovery.guidance)
        : '';
    log(
      `${route.split('?')[0]}: 状態コード ${status ?? '不明'}。${reason}${message ? ` ${message}` : ''}${guidance && !message.includes(guidance) ? ` ${guidance}` : ''}${status === 403 ? ` ${forbiddenHint}` : ''}${!remote && (status === 401 || status === 403) ? ' 一時キーで読めない経路があります。仕様の security を確認し、pcToken 専用ならPCトークン接続を使ってください。' : ''}`,
    );
    return error?.nasHandoff ? 75 : 1;
  }
}

export async function runCommand(
  argv,
  {
    env = process.env,
    configured = configuredRemote,
    open = openDesk,
    log = console.log,
  } = {},
) {
  // GET の明示を接続引数より先に検査する。
  if (argv[0] !== 'GET') throw Error(`GET を先頭に指定してください。${usage}`);
  const parsed = connectionArgs(argv, usage);
  const args = parseArgs(parsed.argv);
  let conn;
  let desk;
  try {
    conn = connection(parsed, env, configured);
    desk = await open(conn);
  } catch {
    log(
      `${args.route.split('?')[0]}: 状態コード 不明。接続設定または資格情報を確認できません。`,
    );
    return 1;
  }
  try {
    return await deskGet(desk.call, args, log, { remote: !!conn });
  } finally {
    desk.close();
  }
}

function invoked() {
  try {
    return (
      fs.realpathSync(path.resolve(process.argv[1])) ===
      fs.realpathSync(fileURLToPath(import.meta.url))
    );
  } catch {
    return false;
  }
}
if (invoked())
  runCommand(process.argv.slice(2)).then(
    (code) => {
      process.exitCode = code;
    },
    () => {
      console.error(
        '引数または接続を確認できません。GET /api/経路 を指定し、接続設定を確認してください。',
      );
      process.exitCode = 1;
    },
  );
