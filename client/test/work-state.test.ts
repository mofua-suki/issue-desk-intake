import test from 'node:test';
import assert from 'node:assert/strict';
import { workState, rowGroup } from '../../shared/src/desk-attention.mts';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { sharedClientBuild } from './shared-client-build.ts';

test('work states require concrete decisions, live execution, or an automatic resume path', () => {
  const stopped = [
    'running',
    'waiting',
    'queued',
    'failed',
    'stopped',
    'untracked',
    'review_held',
  ];
  for (const status of stopped)
    assert.equal(workState({ kind: 'issue', status }).state, 'stopped', status);
  assert.equal(
    workState({ kind: 'issue', status: 'running', executionActive: true })
      .state,
    'running',
  );
  assert.equal(
    workState({ kind: 'issue', status: 'failed', executionActive: true }).state,
    'stopped',
  );
  assert.equal(
    workState({ kind: 'issue', status: 'failed', requiresUser: true }).state,
    'attention',
  );
  assert.equal(
    workState({
      kind: 'issue',
      status: 'failed',
      permissionApproval: { head: 'a' },
    }).state,
    'attention',
  );
  assert.equal(
    workState({
      kind: 'issue',
      status: 'failed',
      finalApprovalsUnavailable: '読取失敗',
    }).state,
    'stopped',
  );
  assert.equal(
    workState({ kind: 'attention', status: 'waiting', requiresUser: true })
      .state,
    'stopped',
    '質問が不明な通知を利用者の判断待ちにしない',
  );
  assert.equal(
    workState({
      kind: 'attention',
      status: 'waiting',
      requiresUser: true,
      nextAction: '使用量上限の引上げを判断する',
    }).state,
    'attention',
  );
  for (const row of [
    {
      kind: 'issue',
      status: 'dependency_waiting',
      executionScheduled: true,
      dependencyReason: '先行PRのマージ',
    },
    {
      kind: 'draft',
      status: 'ai_recovering',
      executionScheduled: true,
      retryAt: '2026-11-13T00:00:00Z',
    },
  ]) {
    const work = workState(row);
    assert.equal(work.state, 'waiting');
    assert.ok(work.waitCondition && work.resumeBy && work.nextAction);
  }
  const gist = {
    kind: 'attention',
    caseKind: 'job_recovery',
    status: 'waiting',
    requiresUser: true,
    gist: '対象データの削除を判断してください。',
  };
  assert.equal(workState(gist).state, 'attention');
  assert.equal(workState(gist).nextAction, gist.gist);
  assert.equal(workState({ ...gist, requiresUser: false }).state, 'stopped');
  assert.equal(rowGroup({ kind: 'issue', status: 'completed' }), 'completed');
  assert.equal(rowGroup({ kind: 'issue', status: 'closed' }), 'closed');
});

test('parent consultations stay out of lists while notifications follow independent work', async () => {
  const { destination, ui } = sharedClientBuild();
  const { requestGroupsWithAux } = await import(
    pathToFileURL(join(destination, 'v', ui, 'request-list.js')).href
  );
  const data = {
    issues: [
      {
        project: 'o/r',
        number: 1,
        intakeId: 'parent',
        status: 'running',
        executionActive: true,
      },
      {
        project: 'o/r',
        number: 2,
        intakeId: 'parent',
        status: 'queued',
        executionScheduled: true,
      },
    ],
    drafts: [
      {
        id: 'parent',
        status: 'submitted',
        project: 'o/r',
        issues: [{ number: 1 }, { number: 2 }],
        familyProgress: {
          total: 2,
          completed: 0,
          closed: 0,
          remaining: 2,
          stopped: [],
          unknown: 0,
        },
      },
    ],
    todos: [],
    closes: [],
    reviews: [],
    stalls: [],
    fullTests: [],
    attentionAlerts: [] as any[],
  };
  const independent = requestGroupsWithAux({
    ...data,
    issues: [1, 2].map((number) => ({
      project: 'o/r',
      number,
      status: 'waiting',
      requiresUser: true,
      causeKey: 'same',
    })),
    drafts: [],
  });
  assert.equal(
    independent.attention.length,
    2,
    '独立した仕事は同原因でも各1件',
  );
  let groups = requestGroupsWithAux(data);
  assert.equal(groups.issues.length, 1);
  assert.equal(groups.waiting.length, 1);
  assert.ok(
    Object.values<any>(groups)
      .flat()
      .every((row) => row.id !== 'parent'),
  );
  assert.equal(groups.issues[0].families, undefined);
  data.attentionAlerts.push({
    key: 'notify',
    kind: 'job_recovery',
    project: 'o/r',
    number: 1,
    handoff: true,
    reason: '復旧できず',
    nextActor: '担当AI',
    nextAction: '復旧条件を調べる',
  });
  groups = requestGroupsWithAux(data);
  assert.equal(groups.issues.length, 0);
  assert.equal(groups.stopped.length, 1);
  assert.equal(groups.stopped[0].relatedAlerts.length, 1);
  data.attentionAlerts.push({
    key: 'ask',
    kind: 'job_recovery',
    project: 'o/r',
    number: 1,
    requiresUser: true,
    nextAction: '上限を引き上げるか判断する',
  });
  groups = requestGroupsWithAux(data);
  assert.equal(groups.attention.length, 1);
  assert.equal(groups.stopped.length, 0);
  assert.equal(groups.waiting.length, 1);
  assert.equal(groups.attention[0].relatedAlerts.length, 2);
});

test('saved child links hide parents in every list without hiding the children or standalone proposals', async () => {
  const { destination, ui } = sharedClientBuild();
  const { requestGroupsWithAux } = await import(
    pathToFileURL(join(destination, 'v', ui, 'request-list.js')).href
  );
  const statuses = [
    'draft',
    'intake_waiting',
    'thinking',
    'submit_error',
    'submit_unknown',
    'submitted',
    'error',
    'closed',
    'project_handoff',
    'investigation_reported',
  ];
  for (const status of statuses)
    for (const link of [
      { parent: { hasChildren: true }, child: {} },
      { parent: { splitChildren: ['child'] }, child: {} },
      { parent: { consultationChildren: ['child'] }, child: {} },
      { parent: {}, child: { parentSplit: 'parent' } },
      { parent: {}, child: { parentConsultation: 'parent' } },
    ]) {
      const data = {
        issues: [
          {
            project: 'o/r',
            number: 1,
            intakeId: 'parent',
            status: 'running',
            executionActive: true,
          },
        ],
        drafts: [
          {
            id: 'parent',
            project: 'o/r',
            status,
            questions: ['親の古い質問'],
            splitIssues: [{}],
            messages: [{ images: [{}] }],
            ...link.parent,
          },
          {
            id: 'child',
            project: 'o/r',
            status: 'draft',
            questions: ['子の判断'],
            ...link.child,
          },
        ],
        todos: [],
        closes: [],
        reviews: [],
        stalls: [
          {
            id: 'parent-stall',
            draftId: 'parent',
            title: '親の止まり',
            detail: '親の詳細',
          },
        ],
        fullTests: [],
        attentionAlerts: [
          {
            key: 'parent-budget',
            kind: 'consultation_budget',
            draftId: 'parent',
            requiresUser: true,
            nextAction: '親の質問',
          },
        ],
      };
      const groups = requestGroupsWithAux(data);
      assert.deepEqual(
        groups.issues.map((row) => row.number),
        [1],
        status,
      );
      assert.deepEqual(
        groups.attention.map((row) => row.id),
        ['child'],
        status,
      );
      assert.ok(
        Object.values<any>(groups)
          .flat()
          .every((row) => row.id !== 'parent' && row.draftId !== 'parent'),
        status,
      );
      for (const tab of [
        'waiting',
        'stopped',
        'completed',
        'closed',
        'followups',
      ])
        assert.equal(groups[tab].length, 0, status + tab);
    }
  for (const extra of [
    {},
    { hasChildren: false },
    { splitChildren: [], consultationChildren: [] },
    { splitChildren: [''] },
  ]) {
    const proposal = {
      id: 'proposal',
      project: 'o/r',
      status: 'draft',
      splitIssues: [{}, {}],
      questions: ['未作成の案を確認する'],
      ...extra,
    };
    const groups = requestGroupsWithAux({
      issues: [],
      drafts: [proposal],
      todos: [],
      closes: [],
      reviews: [],
      stalls: [],
      fullTests: [],
      attentionAlerts: [],
    });
    assert.deepEqual(
      groups.attention.map((row) => row.id),
      ['proposal'],
    );
  }
});

test('closed consultations retain history without reviving missing Issues or hiding live work', async () => {
  const { destination, ui } = sharedClientBuild();
  const { requestGroupsWithAux } = await import(
    pathToFileURL(join(destination, 'v', ui, 'request-list.js')).href
  );
  const references = [
    {},
    { issue: { number: 1 } },
    { issues: [{ number: 1 }, { number: 2 }] },
    { issue: { number: 1 }, proposal: { kind: 'followup' } },
    {
      issue: { number: 1 },
      splitIssues: [{ title: 'saved proposal' }],
      messages: [{ images: [{ name: 'saved image' }] }],
    },
  ];
  const liveCases = [
    { tab: 'issues', status: 'running', executionActive: true },
    { tab: 'waiting', status: 'queued', executionScheduled: true },
    { tab: 'stopped', status: 'failed' },
    {
      tab: 'attention',
      status: 'waiting',
      requiresUser: true,
      nextAction: '対象の承認を判断する',
    },
  ];
  for (const parentSplit of [undefined, 'parent'])
    for (const cleanup of [undefined, { reason: 'finished' }])
      for (const reference of references) {
        const draft = {
          id: 'closed',
          project: 'o/r',
          status: 'closed',
          parentSplit,
          cleanup,
          questions: ['古い質問'],
          executionActive: true,
          ...reference,
        };
        const data = {
          issues: [],
          drafts: [draft],
          todos: [],
          closes: [],
          reviews: [],
          stalls: [],
          fullTests: [],
          attentionAlerts: [],
        };
        const groups = requestGroupsWithAux(data);
        for (const tab of [
          'attention',
          'issues',
          'waiting',
          'stopped',
          'followups',
        ])
          assert.equal(groups[tab].length, 0, tab);
        assert.deepEqual(
          groups.closed.map((row) => [row.kind, row.id, row.status]),
          [['draft', 'closed', 'closed']],
        );
        for (const { tab, ...state } of liveCases) {
          const issue = {
            project: 'o/r',
            number: 1,
            intakeId: 'closed',
            ...state,
          };
          const withIssue = requestGroupsWithAux({ ...data, issues: [issue] });
          assert.equal(
            withIssue[tab].length,
            1,
            `closed draft must not hide ${tab}`,
          );
          assert.equal(withIssue[tab][0].kind, 'issue');
          assert.equal(withIssue[tab][0].number, 1);
          assert.equal(withIssue.closed.length, 1);
          assert.equal(withIssue.closed[0].id, 'closed');
        }
      }
});

test('each related approval and the original issue keep separate entry buttons', async () => {
  const { readFileSync } = await import('node:fs');
  const { default: vm } = await import('node:vm');
  const { compileSource } = await import('../scripts/source.ts');
  const { destination, ui } = sharedClientBuild();
  const model = await import(
    pathToFileURL(join(destination, 'v', ui, 'request-list.js')).href
  );
  const opened: string[] = [];
  const container: any = {
    children: [],
    insertBefore(item: any, before: any) {
      const old = this.children.indexOf(item);
      if (old >= 0) this.children.splice(old, 1);
      const index = this.children.indexOf(before);
      if (index >= 0) this.children.splice(index, 0, item);
      else this.children.push(item);
    },
    removeChild(item: any) {
      this.children.splice(this.children.indexOf(item), 1);
    },
  };
  const el = (tag: string, attrs: any, ...children: any[]): any => ({
    tag,
    attrs,
    children,
    replaceChildren(...items: any[]) {
      this.children = items;
    },
  });
  const context = vm.createContext({
    ...model,
    document: { activeElement: null },
    window: {},
    $: () => container,
    el,
    shownProject: (p: string) => p,
    deskLabel: () => '共通番号1',
    badge: () => null,
    rowTime: () => null,
    issueView: () => opened.push('issue'),
    attentionAlertView: (a: any) => opened.push(a.key),
  });
  vm.runInContext(
    compileSource(
      readFileSync(
        new URL('../src/ui/app-list-rows.ts', import.meta.url),
        'utf8',
      ),
      'rows.ts',
    ),
    context,
  );
  context.screen = {};
  context.rows = [
    {
      kind: 'issue',
      status: 'waiting',
      project: 'o/r',
      number: 1,
      families: [{ id: 'parent', title: '親相談は非表示' }],
      permissionApproval: { head: 'a' },
      relatedAlerts: ['one', 'two'].map((key) => ({
        kind: 'attention',
        key,
        status: 'waiting',
        work: { state: 'attention', nextAction: key },
      })),
    },
  ];
  vm.runInContext(
    'listRowsRender(screen, rows, {tabName:"attention",filtered:false});',
    context,
  );
  assert.equal(container.children.length, 3);
  assert.ok(
    !JSON.stringify(container.children.map((node) => node.children)).includes(
      '親相談は非表示',
    ),
  );
  for (const button of container.children) button.attrs.onClick();
  assert.deepEqual(opened, ['issue', 'one', 'two']);
  opened.length = 0;
  const members = ['one', 'two'].map((key) => ({
    kind: 'attention',
    key,
    id: key,
    status: 'waiting',
  }));
  context.rows = [{ ...members[0], attentionMembers: members }];
  vm.runInContext(
    'listRowsRender(screen, rows, {tabName:"attention",filtered:false});',
    context,
  );
  assert.equal(
    container.children.length,
    2,
    '単独の同原因集約でも各判断への入口を残す',
  );
  for (const button of container.children) button.attrs.onClick();
  assert.deepEqual(opened, ['one', 'two']);
});

test('all active work tabs can be opened directly', async () => {
  const { readFileSync } = await import('node:fs');
  const { default: vm } = await import('node:vm');
  const { compileSource } = await import('../scripts/source.ts');
  const source = readFileSync(
    new URL('../src/ui/app-routing.ts', import.meta.url),
    'utf8',
  );
  const value = vm.runInNewContext(
    compileSource(
      source +
        '\nJSON.stringify(["attention","issues","waiting","stopped"].map(t=>parseRoute("#/list/"+t)));',
      'routing.ts',
    ),
    { window: { addEventListener() {} } },
  );
  assert.deepEqual(
    JSON.parse(value),
    ['attention', 'issues', 'waiting', 'stopped'].map((tab) => ({
      type: 'home',
      tab,
    })),
  );
});

test('same-cause notification priority is independent of arrival order', async () => {
  const { destination, ui } = sharedClientBuild();
  const { requestGroupsWithAux } = await import(
    pathToFileURL(join(destination, 'v', ui, 'request-list.js')).href
  );
  const stop = {
    key: 'stop',
    kind: 'job_recovery',
    causeKey: 'same',
    project: 'o/r',
    number: 1,
    handoff: true,
    reason: '復旧できず',
  };
  const decision = {
    key: 'decision',
    kind: 'job_recovery',
    causeKey: 'same',
    project: 'o/r',
    number: 1,
    requiresUser: true,
    nextAction: '新しい対象を承認するか判断する',
  };
  for (const attentionAlerts of [
    [stop, decision],
    [decision, stop],
  ]) {
    for (const issues of [
      [],
      [{ project: 'o/r', number: 1, status: 'running', executionActive: true }],
    ]) {
      const g = requestGroupsWithAux({
        issues,
        drafts: [],
        todos: [],
        closes: [],
        reviews: [],
        stalls: [],
        fullTests: [],
        attentionAlerts,
      });
      assert.equal(g.attention.length, 1);
      assert.equal(g.stopped.length, 0);
      assert.equal(g.issues.length, 0);
      assert.equal(g.attention[0].work.nextAction, decision.nextAction);
      const members =
        g.attention[0].relatedAlerts || g.attention[0].attentionMembers;
      assert.equal(members.length, 2);
      assert.equal(
        members.find((a: any) => a.key === 'stop').work.state,
        'stopped',
      );
      assert.equal(
        members.find((a: any) => a.key === 'decision').work.state,
        'attention',
      );
    }
  }
});
