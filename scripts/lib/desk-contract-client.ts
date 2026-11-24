import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {
  deskContractVersion,
  deskContractHeader,
  contractError,
} from './desk-contract.generated.ts';

function deployedContractVersion(home: string = os.homedir()) {
  try {
    return JSON.parse(
      fs.readFileSync(
        path.join(home, '.issue-desk', 'desk-contract.json'),
        'utf8',
      ),
    ).version;
  } catch {
    return null;
  }
}

function contractMismatch(
  server: string | null,
  skill: string | null,
  script: string = deskContractVersion,
) {
  if (server === script && (!skill || skill === script)) return null;
  if (server === script && skill && skill !== script)
    return '取り決め: このPCのSkill・常設指示が古いか未反映です。このPCの作業ツリーの main 更新と、NASの更新（deploy/nas/update.sh）の配布を確認してください。';
  if (server && skill === server && server !== script)
    return '取り決め: このPCのscriptsが古いか未反映です。このPCのissue-deskを最新mainへ更新してください。';
  if (skill === script && server !== script)
    return `取り決め: 受付とPCの版が不一致です（受付 ${server || '版なし'} / PC ${script}）。NASの受付更新とPCのmain更新を確認してください。`;
  return `取り決め: scripts・Skill・受付の版を照合できません（scripts ${script} / Skill ${skill || '未配布'} / 受付 ${server || '版なし'}）。PCのmain更新・hostdの配布・NASの受付更新を確認してください。`;
}

export function checkContractResponse(
  response: any,
  value: any,
  warn: (line: string) => void = console.error,
) {
  const server =
    response.headers?.get(deskContractHeader) || value?.contractVersion || null;
  // 旧受付も版なしとして表示する。未導入PCのSkillは版不明なので推測しない。
  const message = contractMismatch(server, deployedContractVersion());
  if (message) warn(message);
  if (!response.ok) {
    const recovery = value?.recovery || contractError(value?.code).recovery;
    throw Object.assign(
      Error(
        [
          value?.error || `受付サービスが ${response.status} を返しました。`,
          recovery.action === 'report' ? null : recovery.guidance,
        ]
          .filter(Boolean)
          .join(' '),
      ),
      {
        status: response.status,
        code: value?.code,
        recovery,
      },
    );
  }
}

// 受付が作成済みの子だけを読み、起票結果と操作可能な項目を表示する。
export function failureCause(d) {
  return d.lastFailure?.cause || d.error || d.lastFailure?.message || '未記録';
}
export function actionsText(d) {
  return Array.isArray(d.actions)
    ? d.actions.length
      ? d.actions.join('、')
      : 'なし'
    : '未取得';
}
export async function showSplitChildren(
  call,
  draft,
  log = console.log,
  filedText = null,
) {
  const children = [
    ...new Set(
      [
        ...(draft.consultationChildren || []),
        ...(draft.splitChildren || []),
        ...(draft.splitConsultations || []).map((child) => child.id),
      ].filter(Boolean),
    ),
  ];
  for (const id of children) {
    try {
      const child = await call('GET', `/api/drafts/${id}`);
      log(
        `分割の子: ${id} 状態: ${child.status}${child.title ? ` ${child.title}` : ''}`,
      );
      if (child.issue || child.issues?.length) {
        if (filedText) log(await filedText(call, child));
      }
      if (child.status === 'submit_error') {
        log(`起票失敗: ${failureCause(child)}`);
      } else if (
        !child.issue &&
        !child.issues?.length &&
        (child.status === 'submit_unknown' ||
          child.status === 'submitted' ||
          child.status === 'error' ||
          child.attempted)
      ) {
        log(
          `起票結果は未確認です。送り直さず --check ${id} で確認してください。`,
        );
      } else if (!child.issue && !child.issues?.length) {
        log('未試行または待機中です。');
      }
      log(`操作: ${actionsText(child)}`);
    } catch (error) {
      if (error?.nasHandoff && process.env.ISSUE_DESK_NAS_HANDOFF === 'read')
        throw error;
      log(
        `分割の子: ${id} 状態は未確認です。送り直さず --check ${id} で確認してください。`,
      );
    }
  }
  return children.length;
}
