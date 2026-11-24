import test from 'node:test';
import assert from 'node:assert/strict';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import fs from 'node:fs';
import vm from 'node:vm';
import { sharedClientBuild } from './shared-client-build.ts';
import { compileSource } from '../scripts/source.ts';
import {
  attentionClassification,
  attentionRows,
  rowGroup,
  selectAttention,
} from '../../shared/src/desk-attention.mts';

test('自動で続かない停止中の相談は要対応の対象になる', () => {
  const rows = [
    'stalled',
    'ai_stopped',
    'submit_error',
    'submit_unknown',
    'error',
  ].map((status) => ({ kind: 'draft', id: status, status }));
  for (const row of rows) assert.equal(attentionClassification(row), 'handoff');
  const selected = selectAttention(rows);
  assert.deepEqual(
    selected.attention.map((row) => row.id),
    rows.map((row) => row.id),
  );
  assert.equal(selected.monitoring.length, 0);
});

test('自動で続く相談は監視のまま', () => {
  const rows = [
    { executionActive: true },
    { executionScheduled: true },
    { retryAt: '2026-11-23T12:00:00Z' },
    { hasChildren: true },
    { splitChildren: ['child'] },
  ].map((fields, index) => ({
    kind: 'draft',
    id: String(index),
    status: 'stalled',
    ...fields,
  }));
  for (const row of rows)
    assert.equal(attentionClassification(row), 'monitoring');
  const selected = selectAttention(rows);
  assert.equal(selected.attention.length, 0);
  assert.equal(selected.monitoring.length, rows.length);
});

test('attention classification seven routes', async () => {
  const { destination, ui } = sharedClientBuild();
  const { requestGroupsWithAux } = await import(
    pathToFileURL(join(destination, 'v', ui, 'request-list.js')).href
  );
  const data = {
    issues: [
      { project: 'o/r', number: 1, status: 'failed' },
      { project: 'o/r', number: 2, status: 'running' },
      { project: 'o/r', number: 3, status: 'review_held' },
      {
        project: 'o/r',
        number: 4,
        status: 'queued',
        interruptDelay: { minutes: 60 },
      },
    ],
    drafts: [
      'draft',
      'error',
      'stalled',
      'submit_error',
      'ai_stopped',
      'intake_waiting',
    ].map((status, i) => ({ id: `draft-${i}`, status, questions: [] })),
    closes: [{ id: 'close', project: 'o/r', number: 1, status: 'reviewing' }],
    todos: [],
    reviews: [{ round: 1 }],
    fullTests: [{ id: 'full-test', title: '失敗', excerpt: '保存した失敗' }],
    stalls: [{ id: 'stall', title: '止まり', detail: '保存した診断' }],
    attentionAlerts: [
      'failed',
      'job_recovery',
      'prerequisite_missing',
      'attention_pending',
      'main_broken',
    ].map((kind) => ({
      key: kind,
      kind,
      stateLabel: '調査中',
      state: kind === 'failed' ? 'notify' : 'investigating',
      reason: '受付側で調査する',
    })),
  };
  const groups = requestGroupsWithAux(data);
  assert.equal(groups.attention.length, 0);
  assert.equal(groups.monitoring.length, 12);
  assert.ok(
    groups.issues
      .filter((r: any) => r.status === 'failed' || r.kind === 'stall')
      .every(
        (r: any) =>
          r.guidance.startsWith('受付側の監視記録です。') &&
          !r.guidance.includes('進行中'),
      ),
  );
  assert.equal(
    groups.monitoring.find((r: any) => r.kind === 'full_test').excerpt,
    '保存した失敗',
  );
  assert.equal(
    groups.monitoring.find((r: any) => r.kind === 'stall').detail,
    '保存した診断',
  );
  const unknown = {
    key: 'future',
    kind: 'future_kind',
    state: 'notify',
    requiresUser: true,
    reason: '未知の記録',
    extra: { evidence: '監視へ残す' },
  };
  data.attentionAlerts.push(
    unknown as any,
    { kind: 'future_without_key', reason: '識別子欠測も残す' } as any,
  );
  const future = requestGroupsWithAux(data);
  assert.equal(future.attention.length, 0);
  assert.ok(
    future.monitoring.some((r: any) => r.reason === '識別子欠測も残す'),
  );
  assert.deepEqual(
    future.monitoring.find((r: any) => r.key === 'future').extra,
    unknown.extra,
  );
  const stopped = {
    key: 'consultation-stop:old',
    kind: 'consultation_stop',
    project: 'o/r',
    state: 'stopped',
    handoff: true,
    requiresUser: false,
    reason: '前提のマージ待ち。停止した操作者: token:nas。経過: 24時間。',
    draftId: 'old',
  };
  const stoppedData = {
    ...data,
    issues: [],
    drafts: [{ id: 'old', project: 'o/r', status: 'stalled' }],
    closes: [],
    reviews: [],
    stalls: [],
    fullTests: [],
    attentionAlerts: [stopped],
  };
  const stoppedGroups = requestGroupsWithAux(stoppedData);
  assert.equal(stoppedGroups.attention.length, 0);
  assert.equal(stoppedGroups.stopped.length, 1);
  const stoppedDraft = stoppedGroups.stopped[0];
  assert.equal(stoppedDraft.kind, 'draft');
  assert.equal(stoppedDraft.id, stopped.draftId);
  assert.equal(stoppedDraft.relatedAlerts[0].key, stopped.key);
  assert.equal(stoppedDraft.relatedAlerts[0].reason, stopped.reason);
  const cleared = requestGroupsWithAux({ ...stoppedData, attentionAlerts: [] });
  assert.equal(cleared.stopped.length, 1);
  assert.equal(cleared.stopped[0].relatedAlerts, undefined);
  const orphan = requestGroupsWithAux({ ...stoppedData, drafts: [] });
  assert.equal(orphan.stopped.length, 1);
  assert.equal(orphan.stopped[0].attentionClass, 'handoff');
  const otherProject = requestGroupsWithAux({
    ...stoppedData,
    drafts: [{ id: 'old', project: 'other/r', status: 'stalled' }],
  });
  assert.equal(
    otherProject.stopped.length,
    2,
    '別プロジェクトの相談へ通知を付けない',
  );
  const unassigned = requestGroupsWithAux({
    ...stoppedData,
    drafts: [{ id: 'old', project: null, unassigned: true, status: 'stalled' }],
    attentionAlerts: [{ ...stopped, project: null }],
  });
  assert.equal(
    unassigned.stopped.length,
    1,
    'プロジェクト未定の表示名でも同じ相談の通知を付加する',
  );
  assert.equal(unassigned.stopped[0].relatedAlerts[0].key, stopped.key);
  const escalation = {
    kind: 'job_recovery',
    state: 'send_rejected',
    stateLabel: '権限不足',
    project: 'o/r',
    number: 1,
    reason: '停止操作の権限がありません',
    causeKey: 'same-cause',
  };
  data.attentionAlerts.push(
    ...(['one', 'two'].map((key) => ({ ...escalation, key })) as any),
  );
  const blocked = requestGroupsWithAux(data);
  assert.equal(blocked.attention.length, 0);
  const task = blocked.stopped.find(
    (r: any) => r.kind === 'issue' && r.number === 1,
  );
  assert.equal(task.relatedAlerts.length, 2);
  data.attentionAlerts = data.attentionAlerts.filter(
    (r: any) => r.causeKey !== 'same-cause',
  );
  assert.equal(requestGroupsWithAux(data).attention.length, 0);
  // Shapes returned for held/question and provider usage
  // holds. No synthetic requiresUser flag is added to make these decisions pass.
  data.issues.push({ project: 'o/r', number: 10, status: 'waiting' });
  data.attentionAlerts.push({
    key: 'usage-limit-wait:codex',
    kind: 'usage_limit_wait',
    state: 'usage_wait',
  } as any);
  assert.equal(
    requestGroupsWithAux(data).attention.length,
    1,
    '手動再試行が必要な使用量上限は判断待ち、waitingという状態名だけのIssueは停止中',
  );
  (data.issues[data.issues.length - 1] as any).requiresUser = true;
  assert.equal(requestGroupsWithAux(data).attention.length, 2);
  const fallbackRows = attentionRows(
    ['one', 'two'].map((key) => ({
      ...escalation,
      key,
      causeKey: 'fallback-cause',
      state: 'notify',
      recoveryState: 'skipped',
      requiresUser: false,
      handoff: true,
    })) as any,
  );
  const fallback = selectAttention(fallbackRows);
  assert.equal(fallback.attention.length, 1);
  assert.equal(fallback.attention[0].attentionClass, 'handoff');
  assert.equal(fallback.attention[0].attentionMembers!.length, 2);
  assert.equal(
    selectAttention(
      attentionRows([
        { ...escalation, key: 'resolved', state: 'resolved' } as any,
      ]),
    ).attention.length,
    0,
  );
});

class Element {
  textContent = '';
  className = '';
  disabled = false;
  children: Element[] = [];
  attrs: Record<string, string> = {};
  onclick?: () => unknown;
  append(...children: Element[]) {
    this.children.push(...children);
  }
  setAttribute(key: string, value: string) {
    this.attrs[key] = value;
  }
  addEventListener(event: string, fn: () => unknown) {
    if (event === 'click') this.onclick = fn;
  }
}
const descendants = (e: Element): Element[] => [
  e,
  ...e.children.flatMap(descendants),
];

test('attention decision response authorization', async (t) => {
  const prior = Object.getOwnPropertyDescriptor(globalThis, 'document');
  Object.defineProperty(globalThis, 'document', {
    configurable: true,
    value: { createElement: () => new Element() },
  });
  t.after(() => {
    if (prior) Object.defineProperty(globalThis, 'document', prior);
    else Reflect.deleteProperty(globalThis, 'document');
  });
  const { destination, ui } = sharedClientBuild();
  const { finalApprovalView } = await import(
    pathToFileURL(join(destination, 'v', ui, 'final-approval-view.js')).href
  );
  const approval = {
    id: 'deploy',
    headSha: 'a'.repeat(40),
    fingerprint: 'original',
    reason: '差分の了承が必要',
    target: '配置先',
    operation: '配置',
    condition: '差分確認',
  };
  const issue = {
    project: 'o/r',
    number: 12,
    actions: [],
    pendingFinalApprovals: [approval],
  };
  const sent: any[] = [],
    failures: any[] = [];
  let reject = true;
  const send = async (payload: any) => {
    sent.push(payload);
    if (reject) throw Object.assign(Error('拒否'), { status: 403 });
  };
  const panel = finalApprovalView(issue, send, (e: any) => failures.push(e));
  const nodes = descendants(panel);
  assert.ok(
    nodes.some((n) => n.textContent.includes('head SHA: ' + 'a'.repeat(40))),
  );
  const a = nodes.find((n) => n.textContent === 'A: 了承する')!;
  const b = nodes.find((n) => n.textContent === 'B: 保留する')!;
  await b.onclick!();
  assert.equal(sent.length, 0);
  approval.headSha = 'b'.repeat(40);
  approval.fingerprint = 'changed';
  await a.onclick!();
  assert.equal(failures[0].status, 403);
  assert.ok(!nodes.some((n) => n.textContent === '了承を記録しました。'));
  assert.equal(sent[0].head, 'a'.repeat(40));
  assert.equal(sent[0].approvalFingerprint, 'original');
  assert.equal(sent[0].approvalId, 'deploy');
  assert.match(sent[0].statement, /回答: A$/);
  assert.match(sent[0].statement, /対象: 配置先／操作: 配置／条件: 差分確認/);
  reject = false;
  await Promise.all([a.onclick!(), a.onclick!()]);
  assert.equal(sent.length, 2, '送信中の二重回答を送らない');
  assert.ok(nodes.some((n) => n.textContent === '了承を記録しました。'));
  const readonly = finalApprovalView(
    { ...issue, actions: undefined },
    send,
    () => {},
  );
  assert.ok(
    !descendants(readonly).some((n) => n.textContent === 'A: 了承する'),
  );
  assert.equal(sent.length, 2);
});

test('final approval transfer history', async (t) => {
  const prior = Object.getOwnPropertyDescriptor(globalThis, 'document');
  Object.defineProperty(globalThis, 'document', {
    configurable: true,
    value: { createElement: () => new Element() },
  });
  t.after(() => {
    if (prior) Object.defineProperty(globalThis, 'document', prior);
    else Reflect.deleteProperty(globalThis, 'document');
  });
  const { destination, ui } = sharedClientBuild();
  const { finalApprovalView } = await import(
    pathToFileURL(join(destination, 'v', ui, 'final-approval-view.js')).href
  );
  const transfer = {
    id: 'deploy',
    fromHead: 'a'.repeat(40),
    toHead: 'b'.repeat(40),
    diffFingerprint: 'sha256:' + 'c'.repeat(64),
    at: '2026-11-01T00:00:00Z',
  };
  const sent: unknown[] = [];
  const panel = finalApprovalView(
    { project: 'o/r', number: 12, finalApprovalTransfers: [transfer] },
    async (payload: unknown) => {
      sent.push(payload);
    },
    () => {},
  );
  const text = descendants(panel)
    .map((n) => n.textContent)
    .join(' ');
  for (const value of Object.values(transfer)) assert.ok(text.includes(value));
  assert.equal(sent.length, 0);
});

test('完了案件に関連する判断待ち通知は独立した要対応行に残る', async () => {
  const { destination, ui } = sharedClientBuild();
  const { requestGroupsWithAux } = await import(
    pathToFileURL(join(destination, 'v', ui, 'request-list.js')).href
  );
  const groups = requestGroupsWithAux({
    issues: [
      {
        project: 'owner/repo',
        number: 7,
        status: 'completed',
        state: 'closed',
        executionProgress: { pending: 1 },
      },
    ],
    drafts: [],
    closes: [],
    todos: [],
    reviews: [],
    stalls: [],
    fullTests: [],
    attentionAlerts: [
      {
        key: 'job-recovery:owner/repo#7',
        kind: 'job_recovery',
        project: 'owner/repo',
        number: 7,
        requiresUser: true,
        nextActor: 'user',
        nextAction: '上限超えを判断する',
      },
    ],
  });
  assert.equal(groups.attention.length, 1);
  assert.equal(groups.attention[0].id, 'attention:job-recovery:owner/repo#7');
  assert.equal(groups.attention[0].work.nextAction, '上限超えを判断する');
  const issue = [...groups.stopped, ...groups.completed].find(
    (r: any) => r.kind === 'issue' && r.number === 7,
  );
  assert.ok(issue);
  assert.equal(issue.relatedAlerts, undefined);
});

test('finished followup consultations move to history while unresolved close confirmations remain', async () => {
  const { destination, ui } = sharedClientBuild();
  const { requestGroupsWithAux } = await import(
    pathToFileURL(join(destination, 'v', ui, 'request-list.js')).href
  );
  const groups = requestGroupsWithAux({
    issues: [
      {
        project: 'o/r',
        number: 1,
        status: 'closed',
        state: 'closed',
        followup: true,
      },
    ],
    drafts: [
      {
        id: 'closed-proposal',
        status: 'closed',
        proposal: { kind: 'followup' },
      },
      {
        id: 'completed-proposal',
        status: 'completed',
        proposal: { kind: 'followup' },
      },
      {
        id: 'waiting-proposal',
        status: 'draft',
        proposal: { kind: 'followup' },
      },
    ],
    closes: [
      { id: 'unresolved', project: 'o/r', number: 1, status: 'confirm' },
      { id: 'resolved', project: 'o/r', number: 1, status: 'closed' },
    ],
    todos: [],
    reviews: [],
    stalls: [],
    fullTests: [],
    attentionAlerts: [],
  });
  assert.deepEqual(
    groups.followups.map((r: any) => r.id),
    ['waiting-proposal', 'unresolved'],
  );
  assert.deepEqual(
    groups.closed.map((r: any) => r.id),
    [undefined, 'closed-proposal', 'resolved'],
  );
  assert.deepEqual(
    groups.completed.map((r: any) => r.id),
    ['completed-proposal'],
  );
  assert.equal(groups.attention.length, 0);
});

test('後続Issueの受入確認は残し、終了した相談自身は履歴へ移す', async () => {
  const { destination, ui } = sharedClientBuild();
  const { requestGroupsWithAux } = await import(
    pathToFileURL(join(destination, 'v', ui, 'request-list.js')).href
  );
  const groups = requestGroupsWithAux({
    issues: [
      {
        project: 'o/r',
        number: 1,
        status: 'completed',
        followup: true,
        executionProgress: { pending: 1 },
      },
      {
        project: 'o/r',
        number: 2,
        status: 'completed',
        followup: true,
        executionProgress: { unavailable: '受入確認を読めません' },
      },
      {
        project: 'o/r',
        number: 3,
        status: 'closed',
        followup: true,
        executionProgress: { pending: 1 },
      },
    ],
    drafts: [
      ...['completed', 'closed'].map((status) => ({
        id: status + '-proposal',
        status,
        proposal: { kind: 'followup' },
        executionProgress: { pending: 1 },
      })),
      ...['draft', 'closed'].map((status) => ({
        id: status + '-parent',
        status,
        hasChildren: true,
        proposal: { kind: 'followup' },
      })),
    ],
    closes: [],
    todos: [],
    reviews: [],
    fullTests: [],
    stalls: [],
    attentionAlerts: [],
  });
  assert.deepEqual(
    groups.followups.map((r: any) => r.number),
    [1, 2],
  );
  assert.deepEqual(
    groups.completed.map((r: any) => r.id),
    ['completed-proposal'],
  );
  assert.ok(
    groups.closed.some((r: any) => r.kind === 'issue' && r.number === 3),
  );
  assert.ok(groups.closed.some((r: any) => r.id === 'closed-proposal'));
  assert.ok(!groups.closed.some((r: any) => r.id === 'closed-parent'));
});

test('起票前確認の上限通知は止まりの一覧から要対応へ出る', async () => {
  const { destination, ui } = sharedClientBuild();
  const { requestGroupsWithAux } = await import(
    pathToFileURL(join(destination, 'v', ui, 'request-list.js')).href
  );
  const detail =
    '判定が上限5回に達しました（読み足し0回/上限8回・判定5回/上限5回）。相談を更新して再送してください。';
  const limit = {
    id: 'filing-check-limit-d1',
    kind: 'intake_error',
    status: 'filing_check_stopped',
    project: 'o/r',
    draftId: 'd1',
    title: '起票前の確認を止めました: 相談（読み足し0回・判定5回）',
    detail,
    reason: detail,
    requiresUser: true,
    nextAction: '相談を更新して再送する',
    detectedAt: '2026-10-01T00:05:00.000Z',
  };
  const groups = requestGroupsWithAux({
    issues: [],
    drafts: [],
    closes: [],
    todos: [],
    reviews: [],
    fullTests: [],
    attentionAlerts: [],
    stalls: [limit],
  });
  const row = groups.attention.find(
    (r: any) => r.id === 'stall-filing-check-limit-d1',
  );
  assert.ok(row, '上限通知が要対応に出ない');
  assert.equal(row.stallKind, 'intake_error');
  assert.equal(
    row.status,
    'filing_check_stopped',
    '専用の状態のまま残し、止まりの汎用の状態へ丸めない',
  );
  assert.equal(rowGroup(row), 'attention');
  assert.equal(row.work.nextActor, '利用者');
  assert.equal(row.work.nextAction, '相談を更新して再送する');
  assert.ok(
    row.guidance.includes('相談を更新して再送してください') &&
      !row.guidance.includes('通常のPRで直して'),
  );
  assert.equal(
    groups.monitoring.some((r: any) => r.id === row.id),
    false,
  );
});

test('相談に結び付く止まりの行を押すと、その相談の画面を開く', () => {
  const opened: unknown[][] = [];
  let scrolled = 0;
  const context = vm.createContext({
    draftView: (...args: unknown[]) => opened.push(['draft', ...args]),
    issueView: (...args: unknown[]) => opened.push(['issue', ...args]),
    $: () => ({
      scrollIntoView: () => {
        scrolled++;
      },
    }),
  });
  vm.runInContext(
    compileSource(
      fs.readFileSync(
        new URL('../src/ui/app-home.ts', import.meta.url),
        'utf8',
      ),
      'app-home.ts',
    ),
    context,
  );
  const open = (row: object) => {
    context.row = row;
    vm.runInContext('stallAlertOpen(row)', context);
  };
  open({ kind: 'stall', id: 'stall-filing-check-limit-d1', draftId: 'd1' });
  assert.deepEqual(opened, [['draft', 'd1']]);
  open({
    kind: 'stall',
    id: 'stall-x',
    draftId: 'd1',
    fix: { issue: { project: 'o/r', number: 3, desk: 9 } },
  });
  assert.deepEqual(
    opened[1],
    ['issue', 'o/r', 3, true, 9],
    '直す依頼があればそちらを先に開く',
  );
  open({ kind: 'stall', id: 'stall-host' });
  assert.equal(opened.length, 2);
  assert.equal(scrolled, 1, '相談も直す依頼も無ければ各PCへ移る');
});

test('マージ後の受入確認を一覧と詳細に表示し条件別情報を開く', async () => {
  const { destination, ui } = sharedClientBuild();
  const { requestGroups, statusLabel } = await import(
    pathToFileURL(join(destination, 'v', ui, 'request-list.js')).href
  );
  class Element {
    textContent = '';
    children: any[] = [];
    onclick: (() => void) | null = null;
    isConnected = true;
    constructor(_tag: string) {}
    append(...children: any[]) {
      this.children.push(...children);
    }
    replaceChildren(...children: any[]) {
      this.children = children;
    }
    setAttribute() {}
    addEventListener(_name: string, handler: () => void) {
      this.onclick = handler;
    }
  }
  const previousDocument = globalThis.document;
  (globalThis as any).document = {
    createElement: (tag: string) => new Element(tag),
    createTextNode: (text: string) => text,
  };
  try {
    const { el } = await import(
      pathToFileURL(join(destination, 'v', ui, 'dom.js')).href
    );
    const { executionProgressView } = await import(
      pathToFileURL(join(destination, 'v', ui, 'execution-progress.js')).href
    );
    const app = new Element('app');
    const badge = (status: string, label: string | null) =>
      el('span', {
        text:
          label ??
          (status === 'completed'
            ? '完了'
            : status === 'closed'
              ? '終了'
              : status),
      });
    let full: any;
    const context = vm.createContext({
      el,
      badge,
      statusLabel,
      executionProgressView,
      issueKey: '',
      issueOlder: new Set(),
      mobileView: '',
      issueFullLoader: async () => full,
      guidance: { reported: '報告済みです。' },
      $: () => app,
      head: (...nodes: any[]) =>
        el('header', {}, ...nodes.filter((n) => n instanceof Element)),
      shownProject: (p: string) => p,
      deskLabel: () => '共通番号',
      originLabel: () => null,
      finalApprovalView: () => null,
      repositoryProjects: () => [],
      error: () => {},
      rowTime: () => null,
      hint: (s: string) => el('p', { text: s }),
      closeReasons: {},
    });
    for (const name of ['app-issue-view.ts', 'app-list-rows.ts']) {
      vm.runInContext(
        compileSource(
          fs.readFileSync(
            new URL(`../src/ui/${name}`, import.meta.url),
            'utf8',
          ),
          name,
        ),
        context,
      );
    }
    const flatten = (node: any): string =>
      typeof node === 'string'
        ? node
        : node
          ? [node.textContent, ...node.children.map(flatten)].join(' ')
          : '';
    const find = (node: any, text: string): any =>
      node &&
      typeof node !== 'string' &&
      (node.textContent === text
        ? node
        : node.children.map((child: any) => find(child, text)).find(Boolean));
    let number = 0;
    for (const [progress, label, description] of [
      [{ pending: 1 }, 'マージ済み・受入確認 残り1件', '受入確認が残り1件'],
      [
        { unavailable: '受入確認を読めません' },
        'マージ済み・受入確認を読取不能',
        '受入確認を読み取れません',
      ],
      [{ pending: 0 }, '完了', '完了しました。'],
    ] as const) {
      const issue = {
        project: 'o/r',
        number: ++number,
        status: 'completed',
        executionProgress: progress,
      };
      const groups = requestGroups([issue], []);
      const row = (
        progress.pending === 0 ? groups.completed : groups.stopped
      )[0];
      context.row = row;
      const list = vm.runInContext(
        "listRowChildren(row, 'stopped', false)",
        context,
      );
      assert.match(list.map(flatten).join(' '), new RegExp(label));
      context.issue = issue;
      full = {
        ...issue,
        executionProgress: {
          pending: 1,
          checks: [
            {
              id: 'c',
              criterion: '実際の条件',
              owner: 'implementer',
              phase: 'verification',
              status: 'pending',
              reason: '',
              method: '確認方法',
              expected: '期待結果',
            },
          ],
        },
      };
      vm.runInContext('renderIssue(issue)', context);
      assert.match(flatten(app), new RegExp(label));
      assert.match(flatten(app), new RegExp(description));
      if (progress.pending === 1 || progress.unavailable) {
        const button = find(app, '受入条件の詳細を見る ▼');
        assert.ok(button);
        button.onclick();
        await new Promise((resolve) => setImmediate(resolve));
        assert.match(flatten(app), /未確認：実際の条件/);
        assert.match(flatten(app), /確認方法.*期待結果/);
      } else {
        assert.equal(find(app, '受入条件の詳細を見る ▼'), undefined);
      }
    }
    for (const progress of [
      {
        pending: 1,
        checks: [
          {
            id: 'c',
            criterion: '実際の条件',
            owner: 'implementer',
            phase: 'verification',
            status: 'pending',
            method: '確認方法',
            expected: '期待結果',
          },
        ],
      },
      {
        pending: 1,
        checks: [
          {
            id: 'c',
            criterion: '実際の条件',
            owner: 'implementer',
            phase: 'verification',
            status: 'failed',
            method: '確認方法',
            expected: '期待結果',
          },
        ],
      },
    ]) {
      const issue = {
        project: 'o/r',
        number: ++number,
        state: 'closed',
        status: 'completed',
        executionProgress: { pending: progress.pending },
      };
      const groups = requestGroups([issue], []);
      assert.equal(groups.completed.length, 1);
      assert.equal(groups.stopped.length, 0);
      context.row = groups.completed[0];
      assert.match(
        vm
          .runInContext("listRowChildren(row, 'completed', false)", context)
          .map(flatten)
          .join(' '),
        /マージ済み・受入確認 残り1件/,
      );
      context.issue = issue;
      full = { ...issue, executionProgress: progress };
      vm.runInContext('renderIssue(issue)', context);
      const button = find(app, '受入条件の詳細を見る ▼');
      assert.ok(button);
      button.onclick();
      await new Promise((resolve) => setImmediate(resolve));
      assert.match(flatten(app), /実際の条件/);
      assert.match(flatten(app), /実装担当/);
      assert.match(
        flatten(app),
        progress.checks[0].status === 'failed' ? /不合格/ : /未確認/,
      );
      assert.match(flatten(app), /確認方法.*期待結果/);
    }
    const base = {
      project: 'o/r',
      state: 'closed',
      status: 'completed',
      executionProgress: { pending: 1 },
    };
    for (const [extra, tab] of [
      [{ pendingFinalApprovals: [{ reason: '承認' }] }, 'attention'],
      [{ executionActive: true }, 'issues'],
      [{ executionScheduled: true }, 'waiting'],
      [{ recoveryState: 'stopped' }, 'stopped'],
      [{ handoff: true }, 'stopped'],
      [{ executionProgress: { unavailable: '読取失敗' } }, 'stopped'],
    ] as const) {
      const groups = requestGroups(
        [{ ...base, number: ++number, ...extra }],
        [],
      );
      assert.equal(groups[tab].length, 1);
      assert.equal(groups.completed.length, 0);
    }
    for (const status of ['running', 'stall_alert', 'family_in_progress']) {
      for (const executionProgress of [
        { pending: 1 },
        { unavailable: '受入確認を読めません' },
      ]) {
        context.issue = {
          project: 'o/r',
          number: ++number,
          status,
          executionProgress,
        };
        vm.runInContext('renderIssue(issue)', context);
        assert.equal(find(app, '受入条件の詳細を見る ▼'), undefined);
        if (status !== 'running') {
          assert.match(flatten(app), /現在の状態は上の帯に表示されています。/);
        }
      }
    }
    context.issue = {
      project: 'o/r',
      number: 2,
      status: 'completed',
      investigation: true,
      executionProgress: { pending: 1 },
    };
    vm.runInContext('renderIssue(issue)', context);
    assert.match(flatten(app), /報告済み/);
    context.issue = { project: 'o/r', number: 3, status: 'closed' };
    vm.runInContext('renderIssue(issue)', context);
    assert.match(flatten(app), /依頼は閉じられています/);
  } finally {
    (globalThis as any).document = previousDocument;
  }
});
