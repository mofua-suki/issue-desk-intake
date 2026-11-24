import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import http from 'node:http';
import type { TestContext } from 'node:test';
import { sharedClientBuild } from './shared-client-build.ts';
import { cleanupStack, startChromium } from './chromium.ts';
import { compileSource } from '../scripts/source.ts';

export async function repositoryDisplayScreens(t: TestContext) {
  const built = sharedClientBuild();
  const directory = fs.mkdtempSync(
    path.join(os.tmpdir(), 'intake-repository-display-'),
  );
  const cleanup = cleanupStack(t);
  cleanup(() => fs.rmSync(directory, { recursive: true, force: true }));
  const id = '11111111-1111-4111-8111-111111111111';
  const at = '2026-11-01T00:00:00Z';
  const names = [
    'first/alpha',
    'second/dependency',
    'third/successor',
    'fourth/holder',
    'fifth/usage',
    'sixth/inventory',
    'seventh/priority',
    'eighth/followup',
    'ninth/detail',
    'first/shared',
    'second/shared',
  ];
  const issue = {
    project: names[0],
    number: 1,
    desk: 101,
    title: '排他待ちの依頼',
    status: 'project_waiting',
    body: '依頼本文',
    comments: [],
    images: [],
    actions: [],
    createdAt: at,
    updatedAt: at,
    projectWait: {
      reason: '占有中',
      condition: '先行処理の終了',
      holders: [{ project: names[3], issue: 4, host: 'pc-a' }],
    },
    pendingFinalApprovals: [
      {
        id: 'approval',
        headSha: 'abc',
        fingerprint: 'fingerprint',
        reason: '許可の確認',
      },
    ],
    url: `https://example.invalid/${names[0]}/issues/1`,
  };
  const draft = {
    id,
    project: names[0],
    status: 'draft',
    body: '相談本文',
    title: '相談の表示',
    ready: false,
    messages: [
      { id: 'message', role: 'assistant', text: '引用 first/alpha を保持' },
      {
        id: 'report',
        role: 'report',
        source: 'second/dependency#2',
        text: '調査した結果',
      },
    ],
    actions: ['chat'],
    provider: 'codex',
    splitIssues: [
      {
        project: names[1],
        title: '分割案',
        purpose: '依存先の修正',
        acceptance: ['動作確認'],
        dependsOn: [],
      },
    ],
    crossProposal: {
      reason: '連携を直す',
      scopes: [{ project: names[1], scope: 'src/' }],
    },
  };
  const close = {
    id,
    project: names[0],
    number: 1,
    desk: 101,
    pc: 'pc-a',
    status: 'confirm',
    reason: 'resolved',
    actions: ['keep'],
    fingerprint: 'full-fingerprint',
    url: issue.url,
    material: {
      references: [
        {
          ref: 'second/dependency#2',
          kind: 'issue',
          found: true,
          title: '参照する依頼',
        },
        {
          ref: 'third/successor@abcdef',
          kind: 'commit',
          found: true,
          message: '参照するコミット',
        },
      ],
      successors: {
        found: [{ project: names[2], number: 3, title: '後続' }],
        broken: [{ project: names[1], number: 2 }],
      },
    },
  };
  const alerts = [
    {
      key: 'missing:1',
      caseKind: 'prerequisite_missing',
      kind: 'prerequisite_missing',
      project: names[0],
      number: 1,
      title: '前提不足',
      stateLabel: '停止',
      artifacts: [{ repo: names[1], issue: 2, artifact: '共通関数' }],
    },
    {
      key: 'stage:1',
      caseKind: 'stage_watch',
      kind: 'stage_watch',
      stageWatch: true,
      project: names[0],
      number: 1,
      title: '処理の監視',
      stateLabel: '停止',
    },
    {
      key: 'proposal:1',
      caseKind: 'unfiled_proposal',
      kind: 'unfiled_proposal',
      project: names[0],
      number: 1,
      title: '後続案',
      sourceKind: 'pulls',
      url: `https://example.invalid/${names[0]}/pulls/1`,
    },
  ];
  const hosts = {
    hosts: [
      {
        name: 'pc-a',
        repositories: [
          'mofua-suki/issue-desk-intake',
          'mofua-suki/issue-desk-worker',
          'first/alpha',
          'other/issue-desk-intake',
        ],
        maxImplementations: 4,
        capacity: 4,
        hostLoad: { implementing: 2, publishing: 1 },
        repositoryLoads: [
          'mofua-suki/issue-desk-intake',
          'mofua-suki/issue-desk-worker',
          'first/alpha',
          'other/issue-desk-intake',
        ].map((repository) => ({
          repository,
          max: 2,
          implementing: 1,
          publishing: 1,
        })),
      },
    ],
  };
  const work = {
    fromDate: '2026-10-01',
    toDate: '2026-10-07',
    date: '2026-10-01',
    measured: null,
    days: [],
  };
  const usage = {
    ...work,
    total: { count: 1 },
    steps: [],
    models: [],
    kinds: [],
    issues: [{ repo: names[4], number: 5, count: 1 }],
    repos: [{ repo: names[4], count: 1 }],
    rateLimits: { windows: [], count: 0, unknown: 1 },
    tools: { items: [], count: 1, unknown: 0 },
    files: {
      items: [{ repo: names[8], path: 'src/index.ts', count: 1, bytes: 100 }],
      count: 1,
      unknown: 0,
    },
    promptParts: {
      count: 1,
      unknown: 0,
      parts: [],
      promptTotalBytes: {},
      cacheReadTokens: {},
      cacheWriteTokens: {},
    },
  };
  const followup = {
    ...work,
    counts: { total: 1 },
    pending: {
      items: 1,
      records: 1,
      projects: [{ project: names[7], items: 1, records: 1 }],
    },
  };
  const fixed = {
    ...work,
    rows: [
      {
        repo: names[4],
        host: 'pc-a',
        scope: 'full',
        unknownImplementing: 1,
        zeroImplementing: 0,
        unknownLoad: 0,
        groups: [{ group: '1', count: 1 }],
      },
    ],
  };
  const stats = {
    project: '',
    before: work,
    after: work,
    unavailable: [],
    usage: { before: usage, after: usage },
    fixedTestUsage: { before: fixed, after: fixed },
    inventory: {
      rounds: [
        {
          round: 1,
          projects: [
            {
              project: names[5],
              investigationIssue: { project: names[5], number: 6 },
              status: 'done',
            },
          ],
        },
      ],
    },
    priorityReview: {
      daily: {
        population: {
          repositories: { [names[6]]: { high: 1, population: 2, ratio: 0.5 } },
        },
      },
      rounds: [
        { round: 1, investigationIssue: { project: names[6], number: 7 } },
      ],
    },
    usageReview: {
      rounds: [
        {
          round: 1,
          investigationIssue: { project: names[4], number: 5 },
          treatmentIssues: [{ project: names[4], number: 6 }],
          verdict: { result: 'waiting' },
        },
      ],
    },
    tiers: {
      total: 3,
      rows: [names[0], 'first/shared', 'second/shared'].map((project) => ({
        project,
        issue: { project, number: 1 },
        outcome: 'passed',
      })),
    },
    followups: {
      before: followup,
      after: followup,
      total: 1,
      rows: [
        {
          project: names[7],
          number: 8,
          source: 'second/dependency#2',
          kind: 'followup',
          outcome: 'passed',
        },
      ],
    },
  };
  const project = {
    id,
    status: 'draft',
    title: 'プロジェクト作成',
    messages: [],
    actions: ['approve'],
    definition: {
      name: 'created',
      description: '新しいプロジェクト',
      technology: 'node',
      placement: 'nas',
      splitIssues: [],
      testCommand: ['node', '--test'],
    },
  };
  const requests: { method: string; url: string; body: any }[] = [];
  let resolveResult: (value: { success: boolean; message?: string }) => void;
  const result = new Promise<{ success: boolean; message?: string }>(
    (resolve) => {
      resolveResult = resolve;
    },
  );
  const server = http.createServer(async (request, response) => {
    const url = new URL(request.url!, 'http://localhost');
    const chunks: Buffer[] = [];
    for await (const chunk of request) chunks.push(Buffer.from(chunk));
    const body = chunks.length
      ? JSON.parse(Buffer.concat(chunks).toString())
      : null;
    response.setHeader('Content-Type', 'application/json');
    if (url.pathname === '/__result') {
      resolveResult(body);
      response.end('{}');
      return;
    }
    if (url.pathname.startsWith('/api/')) {
      requests.push({ method: request.method!, url: request.url!, body });
      let value: unknown;
      switch (url.pathname) {
        case '/api/session':
          value = { authenticated: true, csrf: 'fixture-csrf', local: false };
          break;
        case '/api/meta':
          value = {
            providers: [{ id: 'codex', name: 'Codex', ready: true }],
            projects: names.map((id) => ({
              id,
              name: id.endsWith('/shared')
                ? 'shared'
                : id === 'ninth/detail'
                  ? '読取資料'
                  : id,
            })),
            projectCreation: { owner: 'creator' },
          };
          break;
        case '/api/provider-disables':
          value = { disables: [] };
          break;
        case '/api/health':
          value = { ready: true, ui: built.ui };
          break;
        case '/api/attention/alerts':
          value = alerts;
          break;
        case '/api/close-requests/' + id:
          value = close;
          break;
        case '/api/close-requests/' + id + '/keep':
          value = { ...close, status: 'declined', actions: [] };
          break;
        case '/api/drafts':
          value = draft;
          break;
        case '/api/drafts/' + id:
          value = draft;
          break;
        case '/api/drafts/' + id + '/chat':
          value = draft;
          break;
        case '/api/issue': {
          const project = url.searchParams.get('project');
          value =
            project === 'first/shared' || project === 'second/shared'
              ? {
                  ...issue,
                  project,
                  number: 9,
                  desk: project === 'first/shared' ? 102 : 103,
                  title: project,
                  status: 'queued',
                  pendingFinalApprovals: [],
                }
              : issue;
          break;
        }
        case '/api/comment':
          value = {};
          break;
        case '/api/stats':
          value = stats;
          break;
        case '/api/stats/overview':
          value = {
            implementations: {
              byProject: [
                { project: names[4], completed: 1 },
                { project: 'first/shared', completed: 1 },
              ],
              byHost: [],
            },
          };
          break;
        case '/api/projects/' + id:
          value = project;
          break;
        case '/api/projects/22222222-2222-4222-8222-222222222222':
          value = {
            ...project,
            status: 'registered',
            definition: { ...project.definition, placement: 'a9max' },
          };
          break;
        case '/api/projects/33333333-3333-4333-8333-333333333333':
          value = { ...project, status: 'registered' };
          break;
        case '/api/list':
          value = {
            full: true,
            cursor: 'display-list',
            rows: [
              {
                key: 'issue:first/alpha#11',
                type: 'issue',
                item: {
                  ...issue,
                  number: 11,
                  desk: 111,
                  title: '割り込みの待機',
                  status: 'queued',
                  executionScheduled: true,
                  pendingFinalApprovals: undefined,
                  interruptDelay: { minutes: 60, behind: ['fourth/holder#4'] },
                },
              },
              {
                key: 'issue:first/alpha#1',
                type: 'issue',
                item: {
                  ...issue,
                  status: 'queued',
                  executionScheduled: true,
                  pendingFinalApprovals: undefined,
                },
              },
              ...['first/shared', 'second/shared'].map((project, index) => ({
                key: `issue:${project}#9`,
                type: 'issue',
                item: {
                  ...issue,
                  project,
                  number: 9,
                  desk: 102 + index,
                  title: project,
                  status: 'queued',
                  executionScheduled: true,
                  pendingFinalApprovals: undefined,
                },
              })),
            ],
            aux: {
              reviews: [],
              fullTests: [],
              stalls: [],
              attentionAlerts: [],
            },
            unavailable: [],
            hosts,
          };
          break;
        case '/api/hosts':
          value = hosts;
          break;
        case '/api/events':
          response.writeHead(200, { 'Content-Type': 'text/event-stream' });
          response.write(': connected\n\n');
          return;
        default:
          response.writeHead(404);
          response.end(
            JSON.stringify({ error: 'unexpected API: ' + request.url }),
          );
          return;
      }
      response.end(JSON.stringify(value));
      return;
    }
    if (url.pathname === '/repository-check.js') {
      response.setHeader('Content-Type', 'text/javascript');
      response.end(
        compileSource(
          fs.readFileSync(
            new URL('./repository-display-browser.ts', import.meta.url),
            'utf8',
          ),
          'repository-display-browser.ts',
        ).replaceAll('__UI__', built.ui),
      );
      return;
    }
    const file = url.pathname === '/' ? 'index.html' : url.pathname.slice(1);
    const resolved = path.resolve(built.destination, file);
    if (
      !resolved.startsWith(built.destination + path.sep) ||
      !fs.existsSync(resolved)
    ) {
      response.writeHead(404);
      response.end();
      return;
    }
    response.setHeader(
      'Content-Type',
      file.endsWith('.js')
        ? 'text/javascript'
        : file.endsWith('.css')
          ? 'text/css'
          : 'text/html',
    );
    const bytes = fs.readFileSync(resolved);
    response.end(
      file === 'index.html'
        ? bytes
            .toString()
            .replace(
              /<script type="module"[^>]*><\/script>/,
              '<script type="module" src="/repository-check.js"></script>',
            )
        : bytes,
    );
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  cleanup(async () => {
    server.closeAllConnections();
    await new Promise<void>((resolve, reject) =>
      server.close((error) => (error ? reject(error) : resolve())),
    );
  });
  const address = server.address();
  assert.ok(address && typeof address === 'object');
  const browser = await startChromium(
    t,
    process.env.ISSUE_DESK_TEST_CHROMIUM,
    directory,
    `http://127.0.0.1:${address.port}/#/attention/missing%3A1`,
    undefined,
    cleanup,
  );
  let resultTimer: ReturnType<typeof setTimeout> | undefined;
  const outcome = await Promise.race([
    result,
    new Promise<never>((_resolve, reject) => {
      resultTimer = setTimeout(
        () =>
          reject(
            Error(`${t.name}: ブラウザの結果POST待機が期限切れになりました`),
          ),
        180000,
      );
    }),
    browser.exited.then((error) => {
      throw error;
    }),
  ]).finally(() => clearTimeout(resultTimer));
  assert.equal(outcome.success, true, outcome.message);
  const created = requests.find(
    (r) => r.method === 'POST' && r.url === '/api/drafts',
  );
  assert.equal(created?.body.project, 'first/alpha');
  assert.equal(created?.body.tier, 'standard', '相談の難易度を送らない');
  const kept = requests.find((r) => r.url.endsWith('/keep'));
  assert.deepEqual(kept?.body, { fingerprint: 'full-fingerprint' });
  const approved = requests.find((r) => r.url === '/api/comment');
  assert.equal(approved?.body.project, 'first/alpha');
  assert.match(approved?.body.statement, /first\/alpha#1/);
  assert.ok(requests.some((r) => r.url.includes('project=first%2Falpha')));
  for (const owner of ['first', 'second'])
    assert.ok(
      requests.some(
        (r) =>
          r.url.includes(`project=${owner}%2Fshared`) &&
          r.url.includes('number=9'),
      ),
    );
}
