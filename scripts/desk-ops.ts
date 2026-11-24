// 配布処理が呼ぶ機械通知だけを扱う。手動操作は各Skillの汎用 get/operation 手順へ移行した。
import {
  openDesk,
  connectionArgs,
  connection,
  connectionUsage,
  configuredRemote,
} from './desk-client.ts';
import { invokedAs } from './lib/invoked.ts';
import { parseRcUpdate, rcUpdateNotice } from './lib/desk-ops-rc-update.ts';
import { parseNotice, sendNotice } from './lib/desk-ops-notices.ts';

const usage = `使い方: node scripts/desk-ops.ts notice <種類> [引数]
  notice remote-control --signature <16進8〜64桁> --instances <名前,…>
  notice rc-update --timer enabled|disabled
  notice boot-check --signature <16進8〜64桁> --items <名前,…>
既存の配布・起動後点検が送る機械通知。PC別トークンを使い、運用代行の許可は不要です。
手動操作は issue-desk-admin / issue-desk-issue-control Skillの汎用 get/operation を使います。
${connectionUsage}`;

export function parseArgs(argv) {
  if (argv[0] !== 'notice') throw Error(usage);
  if (argv[1] === 'rc-update') return parseRcUpdate(argv);
  const flags = {};
  for (let i = 2; i < argv.length; i += 2) {
    const key = argv[i].match(/^--(signature|instances|items)$/)?.[1];
    if (
      !key ||
      Object.hasOwn(flags, key) ||
      argv[i + 1] === undefined ||
      argv[i + 1].startsWith('--')
    )
      throw Error(`通知の引数を確認してください。\n${usage}`);
    flags[key] = argv[i + 1];
  }
  return parseNotice([argv[1]], flags, (message) =>
    Error(`${message}\n${usage}`),
  );
}

async function deskOps(call, args, log = console.log) {
  if (args.command !== 'notice') throw Error(usage);
  if (args.kind === 'rc-update') return rcUpdateNotice(call, args, log);
  return sendNotice(call, args, log);
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
  const parsed = connectionArgs(argv, usage);
  const args = parseArgs(parsed.argv);
  const conn = connection(parsed, env, configured);
  const { call, close } = await open(conn);
  try {
    return await deskOps(call, args, log);
  } finally {
    await close();
  }
}

if (invokedAs(import.meta.url, process.argv[1], { ignoreCase: true }))
  runCommand(process.argv.slice(2)).then(
    (code) => (process.exitCode = code),
    (error) => {
      console.error(error.message);
      process.exitCode = 1;
    },
  );
