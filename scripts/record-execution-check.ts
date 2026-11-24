// コントローラーが実際に得た確認結果を、実装の再開と分離して記録する。
import fs from 'node:fs';
import { randomUUID } from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { openDesk, connectionArgs, connection } from './desk-client.ts';

/** テストで差し替える依存。 */
interface Deps {
  log?: (line: string) => void;
  error?: (line: string) => void;
  read?: (file: string, encoding: 'utf8') => string;
  open?: typeof openDesk;
  connection?: (
    parsed: ReturnType<typeof connectionArgs>,
  ) => Parameters<typeof openDesk>[0];
}
export async function runCommand(
  argv = process.argv.slice(2),
  deps: Deps = {},
) {
  const log = deps.log ?? console.log;
  try {
    const parsed = connectionArgs(
      argv,
      '--desk <共通番号> --file <確認結果.json> [--id <送信ID>]',
    );
    const args: { desk?: string; file?: string; id?: string } = {};
    for (let i = 0; i < parsed.argv.length; i += 2) {
      const key = parsed.argv[i]?.replace(/^--/, '');
      if (
        !['desk', 'file', 'id'].includes(key) ||
        args[key] ||
        !parsed.argv[i + 1]
      )
        throw Error('共通番号・確認結果ファイル・送信IDを確認してください。');
      args[key] = parsed.argv[i + 1];
    }
    if (!/^[1-9]\d*$/.test(args.desk ?? '') || !args.file)
      throw Error('--desk と --file が必要です。');
    const check = JSON.parse((deps.read ?? fs.readFileSync)(args.file, 'utf8'));
    const id = args.id ?? check.id ?? randomUUID();
    check.id = id;
    if (
      !/^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$/.test(id)
    )
      throw Error('送信IDが不正です。');
    const { call, close } = await (deps.open ?? openDesk)(
      (deps.connection ?? connection)(parsed),
    );
    try {
      const row = await call(
        'GET',
        `/api/issue?desk=${encodeURIComponent(args.desk)}`,
      );
      if (
        !row.executionProgress?.checks?.some((c) => c.id === check.requirement)
      )
        throw Error('対象の共通番号に確認条件がありません。');
      log(`確認記録の送信ID: ${id}`);
      const result = await call('POST', '/api/comment', {
        desk: Number(args.desk),
        id,
        action: 'record-check',
        executionCheck: check,
      });
      log(JSON.stringify({ desk: Number(args.desk), ...result }));
    } finally {
      await close();
    }
    return 0;
  } catch (e) {
    (deps.error ?? console.error)(e.message);
    return e?.nasHandoff ? 75 : 1;
  }
}
if (
  process.argv[1] &&
  fs.realpathSync(process.argv[1]) === fileURLToPath(import.meta.url)
)
  process.exitCode = await runCommand();
