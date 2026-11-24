// 読取と分類を分離し、表示更新ごとに詳細要求を共有する。承認は生成しない。
const numberOf = (row) =>
  Number.isSafeInteger(row.number) && row.number > 0
    ? row.number
    : Number.isSafeInteger(row.issue) && row.issue > 0
      ? row.issue
      : null;
const candidate = (row) =>
  (row.kind === 'issue' &&
    ['review_held', 'waiting', 'stopped'].includes(row.status)) ||
  (row.kind === 'attention' &&
    ['review_held', 'usage_limit_wait'].includes(row.caseKind) &&
    typeof row.project === 'string' &&
    !!row.project &&
    numberOf(row) !== null);
const identity = (row) => JSON.stringify([row.project, numberOf(row)]);
const cleanFiles = (files) =>
  Array.isArray(files) &&
  files.length &&
  files.every(
    (x) =>
      typeof x === 'string' &&
      x.trim() === x &&
      x !== '' &&
      !/[\\\0\r\n]/.test(x) &&
      x.split('/').every((part) => part && part !== '.' && part !== '..'),
  ) &&
  new Set(files).size === files.length
    ? [...files].sort()
    : null;

// resultProjection が返す summary と incomplete.reason だけを根拠にする。
export function classifyDecision(detail) {
  const p = detail.permissionApproval;
  if (detail.pendingFinalApprovals?.length || detail.finalApprovalsUnavailable)
    return {
      reason:
        '最終承認の対象と条件を確認し、利用者の了承を得てください（チャットからは issue-desk-issue-control Skillの最終承認手順）',
    };
  if (
    detail.deletionApproval ||
    detail.status === 'scope_waiting' ||
    detail.transferPending
  )
    return { reason: '別の判断が必要です' };
  if (detail.status === 'review_held' && p) {
    const files = cleanFiles(p.files);
    if (
      p.unavailable ||
      !files ||
      typeof p.head !== 'string' ||
      !/^[a-f0-9]{40}$/i.test(p.head) ||
      p.project !== detail.project ||
      !Number.isSafeInteger(p.pr) ||
      p.pr < 1 ||
      !Number.isSafeInteger(p.generation) ||
      p.generation !== detail.generation ||
      !Number.isSafeInteger(p.epoch) ||
      p.epoch < 0
    )
      return { reason: '権限承認の対象・head・filesを照合できません' };
    return {
      operation: 'permission',
      files,
      head: p.head,
      evidence: 'permissionApproval',
    };
  }
  const result = detail.result;
  const text = [result?.summary, result?.incomplete?.reason]
    .filter((x) => typeof x === 'string')
    .join(' ');
  if (detail.status === 'stopped') {
    if (
      detail.scopeWait ||
      /範囲|移管|使用量|上限|回数/.test(`${text} ${detail.error || ''}`)
    )
      return { reason: '停止理由を通常の停止解除と確定できません' };
    return {
      operation: 'resume',
      head: null,
      files: null,
      evidence: text || 'status: stopped',
    };
  }
  if (
    detail.retryable === true &&
    text &&
    !/使用量.{0,10}(?:未知|不明)/.test(text)
  ) {
    const run =
      /実装(?:の)?回数[^。]{0,40}(?:上限[^。]{0,20}(?:超え(?!ていない|ません)|達し(?!ていない|ていません))|超過(?!していない))/.test(
        text,
      );
    const usage =
      /使用量[^。]{0,50}上限[^。]{0,30}超え(?!ていない|ません)|使用量[^。]{0,30}超過(?!していない)/.test(
        text,
      );
    const runUnder =
      /実装(?:の)?回数[^。]{0,40}(?:上限[^。]{0,20}(?:超えていない|超過していない|達していない)|上限未満)/.test(
        text,
      );
    const usageUnder =
      /使用量[^。]{0,40}(?:上限[^。]{0,20}(?:超えていない|超過していない|達していない)|上限未満)/.test(
        text,
      );
    if (
      (run && !runUnder && usage && !usageUnder) ||
      (run && !runUnder && !usage && usageUnder) ||
      (usage && !usageUnder && !run && runUnder)
    )
      return {
        operation:
          run && usage ? 'run_and_usage' : run ? 'run_limit' : 'usage_limit',
        head: null,
        files: null,
        evidence: text,
      };
  }
  return { reason: '現在の判断と上限超過の根拠を確定できません' };
}

export async function readDecisionDetails(rows, read) {
  const pending = new Map();
  for (const row of rows.filter(candidate)) {
    const number = numberOf(row);
    if (typeof row.project !== 'string' || !row.project || number === null)
      continue;
    const key = identity(row);
    if (!pending.has(key))
      pending.set(
        key,
        Promise.resolve()
          .then(() => read(row.project, number))
          .then(
            (value) => ({ value }),
            (error) => ({ error }),
          ),
      );
  }
  const results = new Map<string, any>(
    await Promise.all(
      [...pending].map(
        async ([key, promise]) => [key, await promise] as [string, any],
      ),
    ),
  );
  return rows.map((row) => {
    if (!candidate(row)) return { row };
    const entry = results.get(identity(row));
    if (!entry) return { row, failure: 'project・番号を照合できません' };
    if (entry.error)
      return {
        row,
        failure: `詳細取得に失敗: ${entry.error.message || String(entry.error)}`,
        status: entry.error.status ?? null,
      };
    const d = entry.value;
    if (!d || d.project !== row.project || d.number !== numberOf(row))
      return { row, failure: '詳細応答の対象が一致しません' };
    if (
      row.kind === 'issue' &&
      (row.status !== d.status ||
        (row.state && d.state && row.state !== d.state) ||
        (row.desk != null && d.desk != null && row.desk !== d.desk) ||
        (row.head && d.permissionApproval?.head !== row.head) ||
        (row.permissionApproval?.head &&
          d.permissionApproval?.head !== row.permissionApproval.head) ||
        (row.permissionApproval?.files &&
          JSON.stringify(cleanFiles(row.permissionApproval.files)) !==
            JSON.stringify(cleanFiles(d.permissionApproval?.files))) ||
        (row.updatedAt && d.updatedAt && row.updatedAt !== d.updatedAt))
    )
      return { row, failure: '詳細取得中に状態または対象が変わりました' };
    if (
      typeof d.retryable !== 'boolean' ||
      !Object.hasOwn(d, 'generation') ||
      d.generation === undefined
    )
      return { row, failure: '通常の詳細応答に必要な情報がありません' };
    const decision = classifyDecision(d);
    if (
      d.permissionApproval?.unavailable ||
      (d.permissionApproval && decision.reason?.includes('照合できません'))
    )
      return { row, failure: decision.reason, status: null };
    return { row, detail: d, decision };
  });
}

// 同じ判断だと確認できた補助通知だけを Issue と統合する。
export function aggregateDecisions(entries) {
  const issues = new Map<string, any>(
    entries
      .filter((e) => e.row.kind === 'issue' && e.decision?.operation)
      .map((e) => [identity(e.row), e] as [string, any]),
  );
  const buckets = new Map(),
    consumed = new Set();
  for (const e of issues.values()) {
    const d = e.decision;
    const member = {
      project: e.row.project,
      number: numberOf(e.row),
      desk:
        Number.isSafeInteger(e.detail.desk) && e.detail.desk > 0
          ? e.detail.desk
          : Number.isSafeInteger(e.row.desk) && e.row.desk > 0
            ? e.row.desk
            : null,
      head: d.head ?? null,
      operation: d.operation,
      files: d.files ? [...d.files] : null,
    };
    const key = JSON.stringify([
      member.project,
      member.operation,
      member.files,
    ]);
    const group = buckets.get(key) || {
      key,
      operation: d.operation,
      files: d.files,
      members: [],
      entries: [],
      evidence: [],
    };
    group.members.push(member);
    group.entries.push(e);
    group.evidence.push(d.evidence);
    buckets.set(key, group);
    consumed.add(e);
    // 補助通知には対象headや世代がない。filesが同じでも以前の判断かもしれない。
  }
  return {
    groups: [...buckets.values()],
    remaining: entries.filter((e) => !consumed.has(e)),
  };
}

// 提示後に元配列を変更しても対象は増えない。回答は呼出側が実際の値を保存する。
export function decisionQuestions(group, prefix = '') {
  if (group.operation === 'resume') return [];
  const members = structuredClone(group.members);
  const action = {
    permission: '権限ファイルの変更を承認',
    run_limit: '実装回数上限を超えて再試行',
    usage_limit: '使用量上限を超えて再試行',
    run_and_usage: '実装回数と使用量の上限を超えて再試行',
  }[group.operation];
  if (!action) throw Error('承認質問に対応しない操作です');
  const render = (list) =>
    `${prefix}${group.members[0].project} の ${list.map((m) => `#${m.number}`).join('、')} について${action}しますか？`;
  const questions = [];
  let batch = [];
  for (const m of members) {
    if (render([...batch, m]).length > 400) {
      if (!batch.length) throw Error('対象1件の質問が400文字を超えます');
      questions.push({ question: render(batch), members: batch });
      batch = [];
      if (render([m]).length > 400)
        throw Error('対象1件の質問が400文字を超えます');
    }
    batch.push(m);
  }
  if (batch.length) questions.push({ question: render(batch), members: batch });
  return questions.map((record) =>
    Object.freeze({
      question: record.question,
      members: Object.freeze(
        record.members.map((member) =>
          Object.freeze({
            ...member,
            files: member.files ? Object.freeze([...member.files]) : null,
          }),
        ),
      ),
    }),
  );
}

// 呼び出し側が提示した文と実際の回答を渡す。対象は質問作成時の複製を使う。
export function recordDecisionAnswer(record, shownQuestion, answer) {
  if (
    !record ||
    shownQuestion !== record.question ||
    typeof answer !== 'string' ||
    !answer.trim()
  )
    throw Error('提示した質問と実際の回答が必要です');
  const members = structuredClone(record.members).map((member) =>
    Object.freeze({
      ...member,
      files: member.files ? Object.freeze(member.files) : null,
    }),
  );
  return Object.freeze({
    question: shownQuestion,
    answer,
    members: Object.freeze(members),
  });
}
