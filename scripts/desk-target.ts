// The Issue a request script works on, given by the desk number (--desk, the shared number the intake screen
// and the chat show) or by --project and the Gitea number (--number). Reading the desk number straight from
// the arguments keeps a chat AI from translating it into a Gitea number of the wrong repository (Issue #1545).
// Request scripts share this target validation. Like desk-client.ts, it
// uses only built-ins of Node.

const targetUsage = `対象の指定: --desk <共通番号>（受付画面・チャットに出る番号。こちらを使ってください）
または --project <owner/repo> --number <Gitea番号>（--number はGiteaのIssue番号で、共通番号ではありません）。
--desk と --number は同時に指定できません。--desk に --project を添えると、引いたIssueのプロジェクトと一致するかを確かめます。`;

const positive = (value) =>
  typeof value === 'string' &&
  /^[1-9]\d*$/.test(value) &&
  Number.isSafeInteger(Number(value));
const sameProject = (a, b) =>
  String(a).trim().toLowerCase() === String(b).trim().toLowerCase();

// Parses a comma-separated list of Gitea Issue numbers without accepting duplicates or unsafe integers.
export function positiveIssueNumbers(value) {
  if (typeof value !== 'string')
    throw Error('Issue番号はカンマ区切りの正の整数で指定してください。');
  const parts = value.split(',').map((part) => part.trim());
  if (parts.some((part) => !positive(part)))
    throw Error('Issue番号はカンマ区切りの正の整数で指定してください。');
  const numbers = parts.map(Number);
  if (new Set(numbers).size !== numbers.length)
    throw Error('Issue番号に重複があります。');
  return numbers;
}

// Checks the parsed { desk, project, number } (strings, as given) before anything is sent, and returns them.
function targetArgs(args, usage = targetUsage) {
  const { desk, project, number } = args;
  if (desk !== undefined && number !== undefined)
    throw Error(
      `--desk と --number は同時に指定できません（--number はGitea番号です）。\n${usage}`,
    );
  if (desk === undefined && number === undefined)
    throw Error(
      `--desk <共通番号> か --project・--number のどちらかを指定してください。\n${usage}`,
    );
  if (desk !== undefined && !positive(desk))
    throw Error(
      `--desk には1以上の整数（共通番号）を指定してください。\n${usage}`,
    );
  if (number !== undefined) {
    if (!positive(number))
      throw Error(
        `--number には1以上の整数（Gitea番号）を指定してください。\n${usage}`,
      );
    if (project === undefined || !String(project).trim())
      throw Error(
        `--number（Gitea番号）には --project <owner/repo> が必要です。\n${usage}`,
      );
  }
  return args;
}

// The detail of the intake (GET /api/issue): by desk number, or by project and Gitea number as before. A desk
// number with --project is refused when the Issue found belongs to another project, so a script that goes on
// to change the Issue never sends anything for it.
export async function resolveTarget(call, args) {
  const { desk, project, number } = targetArgs(args);
  if (desk === undefined) {
    const query = new URLSearchParams({ project, number });
    return call('GET', `/api/issue?${query}`);
  }
  const detail = await call(
    'GET',
    `/api/issue?${new URLSearchParams({ desk })}`,
  );
  if (Number(detail?.desk) !== Number(desk))
    throw Error(
      `共通番号${desk} を引きましたが、受付が別の共通番号（${detail?.desk ?? 'なし'}）のIssueを返しました。中止します。`,
    );
  if (project !== undefined && !sameProject(detail.project, project))
    throw Error(
      `共通番号${desk} は ${detail.project} の Gitea番号 ${detail.number} です（指定の --project ${project} と一致しません）。中止します。`,
    );
  return detail;
}
