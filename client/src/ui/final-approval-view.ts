import { el } from './dom.js';
import { shownProject } from './repository-display.js';
import type { Issue } from './issue-types.js';

// 表示した対象の複製だけを送る。現在のSHA・指紋・認証は既存APIが操作時に再検証する。
export function finalApprovalView(
  issue: Issue,
  send: (payload: Record<string, unknown>) => Promise<unknown>,
  failed: (error: unknown) => void,
  projects: readonly unknown[] = [],
) {
  const { project, number } = issue;
  const box = el('section', {
    class: 'final-approvals',
    'aria-label': '未了の承認',
  });
  for (const transfer of issue.finalApprovalTransfers ?? []) {
    box.append(
      el('p', {
        text: `承認 ${transfer.id} の引継ぎ: 元head ${transfer.fromHead} → 新head ${transfer.toHead}／差分指紋 ${transfer.diffFingerprint}／日時 ${transfer.at}`,
      }),
    );
  }
  if (issue.finalApprovalsUnavailable)
    box.append(el('p', { text: issue.finalApprovalsUnavailable }));
  for (const target of issue.pendingFinalApprovals ?? []) {
    const approval = { ...target };
    const question = `${project}#${number} の承認 ${approval.id}（head SHA: ${approval.headSha}）を了承しますか？`;
    const shownQuestion = `${shownProject(project, projects)}#${number} の承認 ${approval.id}（head SHA: ${approval.headSha}）を了承しますか？`;
    const terms = [
      approval.target && `対象: ${approval.target}`,
      approval.operation && `操作: ${approval.operation}`,
      approval.condition && `条件: ${approval.condition}`,
    ]
      .filter(Boolean)
      .join('／');
    const status = el('p');
    const approve = el('button', { type: 'button', text: 'A: 了承する' });
    const hold = el('button', { type: 'button', text: 'B: 保留する' });
    let pending = false;
    const id = crypto.randomUUID();
    approve.onclick = async () => {
      if (pending) return;
      pending = true;
      approve.disabled = hold.disabled = true;
      try {
        await send({
          id,
          project,
          number,
          action: 'approve-final',
          approvalId: approval.id,
          head: approval.headSha,
          approvalFingerprint: approval.fingerprint,
          statement: `質問「${question}${terms ? ` ${terms}` : ''}」への利用者の回答: A`,
        });
        status.textContent = '了承を記録しました。';
      } catch (error) {
        pending = false;
        approve.disabled = hold.disabled = false;
        failed(error);
      }
    };
    hold.onclick = () => {
      status.textContent = '保留しています。承認は送信していません。';
    };
    box.append(
      el('p', { text: approval.reason }),
      el('p', { text: terms }),
      el('p', { text: shownQuestion }),
    );
    if (issue.actions) box.append(approve, hold);
    box.append(status);
  }
  return box;
}
