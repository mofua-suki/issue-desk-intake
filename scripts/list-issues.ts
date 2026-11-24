// 依頼・相談・TODOを全ページ読み、題名の重複候補と照合の完全性を返す。
// 単件読取と通常一覧は issue-desk-issue-control Skill の汎用GET手順を使う。
import { searchDuplicates } from './lib/duplicate-search.ts';
import {
  openDesk,
  connectionArgs,
  connection as defaultConnection,
  connectionUsage,
} from './desk-client.ts';
import { invokedAs } from './lib/invoked.ts';

const usage = `使い方: node scripts/list-issues.ts --duplicates --project <owner/repo> --query <題名の語> [--pretty]

依頼・相談・TODOの全ページを読み、題名をNFKC正規化と英字大小文字を無視した部分一致で照合します。
各種別の confirmed=false は読取失敗・応答不正・途中欠落等による未確認であり、候補なしではありません。
成功結果の skipped は空題名で除外した未照合の件数です。confirmed=true でも重複不存在を保証しません。
通常の単件・一覧・件数取得は issue-desk-issue-control Skill の汎用GET手順を使ってください。
${connectionUsage}`;

function parseArgs(argv: string[]) {
  const out: {
    duplicates?: boolean;
    pretty?: boolean;
    project?: string;
    query?: string;
  } = {};
  for (let i = 0; i < argv.length; i++) {
    const key = argv[i];
    if (key === '--duplicates' && !out.duplicates) {
      out.duplicates = true;
      continue;
    }
    if (key === '--pretty' && !out.pretty) {
      out.pretty = true;
      continue;
    }
    const name = /^--(project|query)$/.exec(key)?.[1];
    if (!name || out[name] !== undefined)
      throw Error(`引数を確認してください: ${key}\n${usage}`);
    const value = argv[++i];
    if (value === undefined || value.startsWith('--'))
      throw Error(`--${name} の値がありません。\n${usage}`);
    out[name] = value;
  }
  if (!out.duplicates || !out.project?.trim() || !out.query?.trim())
    throw Error(`--duplicates、--project、--query が必要です。\n${usage}`);
  return out;
}

export async function runCommand(
  argv = process.argv.slice(2),
  {
    open = openDesk,
    connection = defaultConnection,
    log = console.log,
    error = console.error,
  }: {
    open?: typeof openDesk;
    connection?: (
      parsed: ReturnType<typeof connectionArgs>,
    ) => Parameters<typeof openDesk>[0];
    log?: (line: string) => void;
    error?: (line: string) => void;
  } = {},
) {
  try {
    const parsed = connectionArgs(argv, usage);
    const args = parseArgs(parsed.argv);
    const { call, close } = await open(connection(parsed));
    try {
      log(
        JSON.stringify(
          await searchDuplicates(call, args.project, args.query),
          null,
          args.pretty ? 2 : undefined,
        ),
      );
    } finally {
      await close();
    }
    return 0;
  } catch (e) {
    error(e.message);
    return e?.nasHandoff ? 75 : 1;
  }
}

if (invokedAs(import.meta.url)) process.exitCode = await runCommand();
