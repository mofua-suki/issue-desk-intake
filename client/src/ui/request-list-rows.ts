import { shownProject } from './repository-display.js';
import type { ProjectDefinition, ProjectCreation } from './project-types.js';
import type { DraftIssue } from './draft-types.js';
import type { TierRecord, ModelOverride } from './tier-types.js';
// 相談画面の難易度の行と、新しいプロジェクトの相談（状態・作成の進み具合・定義）の行、TODOの見出し。Issue #1489 で
// client/src/ui/request-list.js から分けた（既存の import は client/src/ui/request-list.js の再エクスポートで保つ）。難易度の表示名と
// モデルの切り替えの記録の文は client/src/ui/request-list-outcomes.js から読む（client/src/ui/request-list.js は import しない）。
import {
  tierName,
  tierSourceNames,
  pendingTierLevelText,
  pendingTierSettingText,
  pendingTierReasonText,
  overrideUseList,
  overrideUseText,
} from './request-list-outcomes.js';

// 始めの判定の廃止前に保存した判定待ちの記録の判定の開始回数（attempts。Issue #467）。
const pendingAttemptsText = (tier: TierRecord) =>
  Number.isSafeInteger(tier?.attempts) && (tier.attempts ?? 0) > 0
    ? `（判定の開始 ${tier.attempts}回）`
    : '';
// 相談画面に出す難易度の記録（見出しと本文の組）。記録の無い導入前の下書きは null（何も出さない）。
// overrides は下書きの「どの切り替えで動いたか」の記録（modelOverrides。Issue #442）。あれば末尾に併記する。
export function tierRows(tier?: TierRecord, overrides?: ModelOverride[]) {
  const used = overrideUseList(overrides);
  const extra = used.length
    ? [['モデルの切り替え', used.map(overrideUseText).join('\n')]]
    : [];
  if (!tier || typeof tier !== 'object') return extra.length ? extra : null;
  const history = Array.isArray(tier.history) ? tier.history : [];
  const pending = tier.pending === true;
  const at = tier.decidedAt ? `（${tier.decidedAt}）` : '';
  return [
    ['難易度', pending ? pendingTierLevelText : tierName(tier.level)],
    ['モデル', tier.model || (pending ? pendingTierSettingText : '不明')],
    ['推論レベル', tier.effort || (pending ? pendingTierSettingText : '不明')],
    [
      '理由',
      pending
        ? `${pendingTierReasonText}${pendingAttemptsText(tier)}${at}`
        : `${tierSourceNames[tier.source ?? ''] || tier.source || '不明'}：${tier.reason || '記録なし'}${at}`,
    ],
    [
      '引き上げ',
      history.length
        ? history
            .map(
              (h) =>
                `${h.at || '時刻不明'} ${tierName(h.from)}→${tierName(h.to)}：${h.reason || '理由なし'}`,
            )
            .join('\n')
        : 'なし',
    ],
    ...extra,
  ];
}
// --- 新しいプロジェクトの相談（server/src/project-intake.mjs） ---------------------------------------------------
// 状態の表示名。依頼の相談の状態（states）とは別の一覧で、同じ名前（waiting など）でも意味が違う。
export const projectStatusNames: Record<string, string> = {
  draft: '相談中',
  thinking: 'AIが整理中',
  waiting: '受付の回答待ち',
  ready: '定義の確定（同意待ち）',
  error: '相談を再試行',
  create_approved: '作成の同意済み',
  creating: 'リポジトリを作成中',
  creation_failed: '作成を再試行',
  creation_blocked: '要対応',
  registered: '登録済み',
};
export const projectGuidance: Record<string, string> = {
  draft: '相談の途中です。続けるときは返信してください。',
  thinking: '受付AIが定義を整理しています。',
  waiting: '受付AIの質問に回答してください。',
  ready:
    '定義を確認して「リポジトリを作成する」を押してください。押すまで作りません。',
  error: '相談が止まりました。理由を確認して再試行するか、返信してください。',
  create_approved: '作成に同意済みです。',
  creating:
    '受付がリポジトリの作成・初期コミット・権限の付与・配置先への clone・登録を順に進めています。',
  creation_failed:
    '作成が途中で止まりました。理由を確認して「作成を再試行」を押すと、続きから再開します。',
  creation_blocked:
    'このままでは作成できません。理由を確認して、定義を見直してください。',
  registered:
    'リポジトリを作成し、受付の対象に登録しました。最初の分割案を起票します（起票したIssueは定義の画面から開けます）。',
};
// 登録の後の分割案の起票（server/src/project-creation.mjs の filingStatuses）の表示名。
// host_waiting は NAS 配置の担当待ち（server/src/project-creation-nas.mjs）。
export const projectFilingNames: Record<string, string> = {
  filing: '起票中',
  filed: '起票済み',
  failed: '起票を再試行',
  host_waiting: 'NAS の監視の担当待ち',
};
// 新しいプロジェクトの定義から起票したIssueの番号。定義の画面と依頼一覧で、共通番号とGitea番号を並べる。
export const projectIssueLabel = (
  i: Partial<Pick<DraftIssue, 'desk' | 'number'>>,
) => `${i.desk ? `#${i.desk}` : '共通番号未確定'}（Gitea番号 ${i.number}）`;
// リポジトリ作成の段階（server/src/project-creation.mjs の creationSteps と同じ順）と表示名。
export const projectCreationSteps = [
  ['created', 'リポジトリの作成'],
  ['committed', '初期コミット'],
  ['collaborators', '権限の付与'],
  ['registered', '受付への登録'],
];
// 作成の進み具合（段階ごとに「済み」「未」）。権限の付与は、Giteaに無くて付けられなかったアカウントも添える。
export function projectCreationRows(creation: ProjectCreation | undefined) {
  const steps = creation?.steps || {};
  return projectCreationSteps.map(([key, label]) => {
    const done = steps[key];
    const missing =
      key === 'collaborators'
        ? (done?.accounts || []).filter(
            (a: { result: string }) => a.result === 'missing',
          )
        : [];
    return [
      label,
      done
        ? `済み${missing.length ? `（Giteaに無いため未付与: ${missing.map((a) => a.login).join('、')}）` : ''}`
        : '未',
    ];
  });
}
export const projectTechnologyNames: Record<string, string> = {
  node: 'Node.js 24',
  python: 'Python 3.12',
};
export const projectPlacementNames: Record<string, string> = {
  a9max: 'A9 MAX',
  nas: 'NAS',
};
// 同意の前に画面へ出す定義の全文（見出しと本文の組）。技術・配置先・テスト・自動化・分割案の実施順を省かずに並べる。
export function projectDefinitionRows(
  d: ProjectDefinition,
  owner: string,
  projects: readonly unknown[] = [],
) {
  const split = d.splitIssues.map((p, i: number) =>
    [
      `${i + 1}. ${p.title}（${p.dependsOn.length ? `依存: ${p.dependsOn.map((n) => `案${n}`).join('、')}` : '依存なし'}）`,
      `目的: ${p.purpose}`,
      '受入条件:',
      ...p.acceptance.map((a) => `- ${a}`),
      ...(p.changeScope
        ? [
            `変更範囲: ${p.changeScope.known ? p.changeScope.paths.join(', ') : `不明（${p.changeScope.reason}）`}`,
          ]
        : []),
    ].join('\n'),
  );
  return [
    ['リポジトリ', shownProject(`${owner}/${d.name}`, projects) ?? ''],
    ['説明', d.description],
    ['技術', projectTechnologyNames[d.technology] || d.technology],
    ['配置先', projectPlacementNames[d.placement] || d.placement],
    ['固定テスト', d.testCommand.join(' ')],
    ['テスト環境の準備', d.setupCommand ? d.setupCommand.join(' ') : 'なし'],
    ['自動マージ', d.autoMerge ? '有効' : '無効'],
    ['自動デプロイ', d.autoDeploy ? '有効' : '無効'],
    ['最初の分割案（実施順）', split.join('\n\n')],
  ];
}
// 一覧の見出しは本文の先頭からコードで作る。タイトル入力を求めず、AIも使わない。
export function todoHeadline(body: unknown) {
  const line =
    String(body || '')
      .split('\n')
      .map((x) => x.trim())
      .find(Boolean) || '';
  if (!line) return '内容未入力のTODO';
  return line.length > 60 ? `${line.slice(0, 60)}…` : line;
}
