// ホスト作業用SSHの送り先を、担当表（assignments.json）の順位で選ぶ。
// host-ssh.json のPC名（a9-max）は assignments.json の machineName で担当名（a9max）へ対応付ける（無ければ同じ名前）。
// 使い方: node scripts/ssh-host-select.ts --repo <owner/repo>  → 順位の最も高い送り先のPC名を1行で出す
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const readJSON = (file: string) =>
  JSON.parse(fs.readFileSync(path.join(root, file), 'utf8'));

/** assignments.json のうち、送り先の選択で読む部分。 */
export interface Assignments {
  hosts?: Record<
    string,
    {
      machineName?: string;
      role?: string;
      repositories?: Record<string, { rank?: number }>;
    }
  >;
}
/** host-ssh.json のうち、登録されたPCの一覧。 */
export interface HostPolicy {
  hosts?: Record<string, unknown>;
}

// repository の実装担当のうち host-ssh.json に登録されたPCを、順位（rank）の小さい順に返す。
// 同じ順位は担当名の順。テスト専用（role test-only）は実装担当ではないので含めない。
export function rankedHosts(
  assignments: Assignments,
  policy: HostPolicy,
  repository: string,
): string[] {
  const rows = [];
  for (const [name, entry] of Object.entries(assignments.hosts ?? {})) {
    const rank = entry?.repositories?.[repository]?.rank;
    if (entry?.role === 'test-only' || !Number.isInteger(rank)) continue;
    const pc = entry.machineName ?? name;
    if (Object.hasOwn(policy.hosts ?? {}, pc)) rows.push({ pc, name, rank });
  }
  return rows
    .sort((a, b) => a.rank - b.rank || a.name.localeCompare(b.name))
    .map((row) => row.pc);
}

export function selectHost(
  assignments: Assignments,
  policy: HostPolicy,
  repository: string,
) {
  const [first] = rankedHosts(assignments, policy, repository);
  if (!first)
    throw Error(
      `${repository} の実装担当で host-ssh.json に登録されたPCがありません。`,
    );
  return first;
}

function main(
  argv: string[],
  options: { assignments?: Assignments; policy?: HostPolicy } = {},
) {
  if (
    argv.length !== 2 ||
    argv[0] !== '--repo' ||
    !/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(argv[1])
  )
    throw Error('使い方: node scripts/ssh-host-select.ts --repo <owner/repo>');
  return selectHost(
    options.assignments ?? readJSON('assignments.json'),
    options.policy ?? readJSON('host-ssh.json'),
    argv[1],
  );
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) {
  try {
    console.log(main(process.argv.slice(2)));
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
