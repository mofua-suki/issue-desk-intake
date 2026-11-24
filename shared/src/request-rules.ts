// 監視への依頼（PRの作成 pr_create・手動PRのマージ merge_manual。大整理 2-2 PR-3。docs/shared-db-requests.md『Shared DB: requests to the watcher (PR creation and manual PR merge)』）の
// 項目の決まり。受付の受け口（server/src/api-v1-requests.mjs）、監視の処理（client/src/pr-requests.mjs・shared/src/merge-requests.mjs）、
// 受付APIとクライアントが共有するPR依頼の入力規約。
import { createHash } from 'node:crypto';

const requestKinds = ['pr_create', 'merge_manual', 'full_test', 'pr_close'];
const titleLimit = 255;
const bodyLimit = 60000;
// 依頼ID（送り手が作る。同じIDで送り直しても1件）。<ミリ秒>-<16進8桁>。
export const requestIdPattern = /^\d{1,16}-[a-f0-9]{8}$/;
const repoName = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;
const branchName = /^(?!-)[A-Za-z0-9._/-]+$/;
export const sha40 = /^[a-f0-9]{40}$/;

export const validRepoName = (repo: string) =>
  typeof repo === 'string' && repoName.test(repo);

// ブランチを断る理由（無ければ null）。
function branchRefusal(branch: any): string | null {
  if (typeof branch !== 'string' || !branch)
    return 'ブランチを指定してください';
  if (branch.includes(':'))
    return `別のリポジトリのブランチ（${branch}）は扱いません。同じリポジトリへ push したブランチを指定してください`;
  if (
    !branchName.test(branch) ||
    branch.includes('..') ||
    branch.includes('//') ||
    branch.endsWith('/') ||
    branch.endsWith('.lock')
  )
    return `ブランチ名が不正です: ${branch}`;
  if (/^codex\/issue-/.test(branch))
    return 'IssueのブランチPR（codex/issue-*）は監視が独立レビューの経路で作ります';
  return null;
}

const patchLimit = 200000;
export const patchHash = (patch) =>
  createHash('sha256').update(patch, 'utf8').digest('hex');

function patchProblem(p: any): string | null {
  if (p.branch !== undefined) return 'パッチとブランチは同時に指定できません';
  if (typeof p.patch !== 'string' || !p.patch.trim())
    return 'パッチは空でない文字列にしてください';
  if (Buffer.byteLength(p.patch, 'utf8') > patchLimit)
    return 'パッチはUTF-8で200,000 bytes以内にしてください';
  if (p.patch.includes('\0') || p.patch.includes('GIT binary patch'))
    return 'バイナリのパッチは扱えません';
  if (typeof p.base !== 'string' || !sha40.test(p.base))
    return 'base は40桁のSHAで指定してください';
  if (p.patchSha256 !== patchHash(p.patch))
    return 'パッチのSHA256が一致しません';
  return null;
}

// PRの作成の依頼の項目の不備（無ければ null）。
function requestProblem({
  repo,
  branch,
  title,
  body,
  emergency,
  ...patch
}: {
  repo: string;
  branch?: any;
  title: any;
  body: string;
  emergency?: any;
  patch?: any;
  base?: any;
  patchSha256?: any;
}): string | null {
  if (emergency === true) return '緊急PRは廃止したため、受け付けません';
  if (!validRepoName(repo))
    return 'リポジトリ名を owner/name で指定してください';
  const branchProblem = ['patch', 'base', 'patchSha256'].some(
    (key) => key in patch,
  )
    ? patchProblem({ branch, ...patch })
    : branchRefusal(branch);
  if (branchProblem) return branchProblem;
  if (
    typeof title !== 'string' ||
    !title.trim() ||
    /[\r\n]/.test(title) ||
    title.length > titleLimit
  )
    return `題は1行・${titleLimit}文字以内で指定してください`;
  if (typeof body !== 'string' || !body.trim() || body.length > bodyLimit)
    return `本文は空でない ${bodyLimit}文字以内で指定してください`;
  return null;
}

// 手動PRのマージの依頼の項目の不備（無ければ null）。
function mergeProblem({
  repo,
  number,
  head,
}: {
  repo: string;
  number: number;
  head: string;
}): string | null {
  if (!validRepoName(repo))
    return 'リポジトリ名を owner/name で指定してください';
  if (!Number.isInteger(number) || number < 1) return 'PR番号が不正です';
  if (!sha40.test(head || ''))
    return '先頭コミットを40桁のSHAで指定してください';
  return null;
}

const instructionAtFormat =
  /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})$/;

// 利用者の承認発言（500文字以内）と、利用者がチャットで指示した日時（UTCオフセット付きISO 8601、承認発言があるとき必須）。
// 発言の無い日時は断る。
function approvalFields(p: any): {
  problem?: string;
  fields?: Record<string, string>;
} {
  if (p.approval !== undefined) {
    if (typeof p.approval !== 'string' || p.approval.trim().length > 500)
      return { problem: '承認発言は500文字以内の文字列です' };
    if (p.approval.trim()) {
      if (
        typeof p.instructionAt !== 'string' ||
        !instructionAtFormat.test(p.instructionAt)
      )
        return {
          problem:
            '承認発言には利用者がチャットで指示した日時（instructionAt: UTCオフセット付きISO 8601）が必要です',
        };
      return {
        fields: { approval: p.approval.trim(), instructionAt: p.instructionAt },
      };
    }
  }
  if (p.instructionAt !== undefined)
    return {
      problem: 'instructionAt は承認発言（approval）と一緒に指定してください',
    };
  return { fields: {} };
}

// 依頼の中身（payload）を種類ごとに決まった項目だけにする。不備は { problem }。
export function requestPayload(
  kind: any,
  repo: string,
  payload: any,
): { problem?: string; payload?: Record<string, any> } {
  const p =
    payload && typeof payload === 'object' && !Array.isArray(payload)
      ? payload
      : {};
  if (kind === 'pr_close') {
    if (!validRepoName(repo))
      return { problem: 'リポジトリ名を owner/name で指定してください' };
    if (![p.number, p.issue].every((n) => Number.isSafeInteger(n) && n > 0))
      return { problem: 'PR番号とIssue番号は正の整数で指定してください' };
    return { payload: { number: p.number, issue: p.issue } };
  }
  if (kind === 'full_test') {
    if (!validRepoName(repo))
      return { problem: 'リポジトリ名を owner/name で指定してください' };
    return Object.keys(p).length === 0
      ? { payload: {} }
      : { problem: 'main の全部のテストの payload は空だけです' };
  }
  if (kind === 'pr_create') {
    const value: Record<string, any> = ['patch', 'base', 'patchSha256'].some(
      (key) => key in p,
    )
      ? {
          ...(p.branch !== undefined ? { branch: p.branch } : {}),
          title: p.title,
          body: p.body,
          patch: p.patch,
          base: p.base,
          patchSha256: p.patchSha256,
        }
      : { branch: p.branch, title: p.title, body: p.body };
    const problem = requestProblem({
      repo,
      ...(value as any),
      emergency: p.emergency,
    });
    if (problem) return { problem };
    // 事前承認。PR番号が決まる前の利用者の承認を作成依頼に残し、マージ依頼の approvalRequest から使う。
    const approval = approvalFields(p);
    return approval.problem
      ? { problem: approval.problem }
      : { payload: { ...value, ...approval.fields } };
  }
  if (kind === 'merge_manual') {
    const value = { number: p.number, head: p.head };
    const problem = mergeProblem({ repo, ...value });
    if (problem) return { problem };
    // 利用者の承認発言。IssueのPR(codex/issue-*)・権限保護対象・データ削除SQLをマージするときだけ、ワーカーが要求する。
    const approval = approvalFields(p);
    if (approval.problem) return { problem: approval.problem };
    // approvalRequest は事前承認を付けた作成依頼（pr_create）のID。受付が照合して承認を写す。
    if (p.approvalRequest !== undefined) {
      if (approval.fields?.approval)
        return {
          problem:
            'approvalRequest は承認発言（approval）と同時に指定できません',
        };
      if (
        typeof p.approvalRequest !== 'string' ||
        !requestIdPattern.test(p.approvalRequest)
      )
        return {
          problem:
            'approvalRequest は作成依頼のID（<数字>-<16進8桁>）で指定してください',
        };
      return { payload: { ...value, approvalRequest: p.approvalRequest } };
    }
    return { payload: { ...value, ...approval.fields } };
  }
  return {
    problem: `依頼の種類は ${requestKinds.join('・')} のどれかにしてください`,
  };
}

// 送り手が作る依頼ID。
export const newRequestId = (
  now: number = Date.now(),
  random: any = () => crypto.randomUUID(),
) => `${now}-${random().replaceAll('-', '').slice(0, 8)}`;
