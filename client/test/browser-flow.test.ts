import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import http from 'node:http';
import { execFileSync } from 'node:child_process';
import { sharedClientBuild } from './shared-client-build.ts';
import { cleanupStack, startChromium } from './chromium.ts';

const chromium = process.env.ISSUE_DESK_TEST_CHROMIUM;

test(
  'ブラウザで詳細を閲覧し、相談とTODOを利用する',
  {
    skip:
      !chromium && !(process.platform === 'linux' && process.arch === 'x64'),
    timeout: 180000,
  },
  async (t) => {
    const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'intake-browser-'));
    const cleanup = cleanupStack(t);
    cleanup(() => fs.rmSync(directory, { recursive: true, force: true }));
    const answerFixture = path.join(directory, 'answer.json');
    execFileSync(
      'go',
      [
        'test',
        '-count=1',
        '-run',
        '^TestEndAnswerNewProjectNoticeOrder$',
        './internal/consultationai',
      ],
      {
        cwd: path.resolve(import.meta.dirname, '../../server'),
        env: {
          ...process.env,
          ISSUE_DESK_BROWSER_ANSWER_FIXTURE: answerFixture,
        },
        stdio: 'inherit',
        timeout: 90000,
      },
    );
    const assistantAnswer: string = JSON.parse(
      fs.readFileSync(answerFixture, 'utf8'),
    );
    const { destination: dist, ...built } = sharedClientBuild();
    const mutations: unknown[] = [];
    const reads = new Set<string>();
    let emptyList = false,
      listReads = 0;
    const listAuxReads: string[] = [];
    let drained = false;
    const drainRequests: string[] = [];
    const hosts = () => ({
      homeMonitor: { stopped: true, host: 'pc-a' },
      hosts: [
        {
          name: 'pc-a',
          repositories: [
            'mofua-suki/issue-desk-intake',
            'mofua-suki/issue-desk-worker',
            'owner/another-very-long-repository-name-for-mobile',
          ],
          repositoryLoads: [
            {
              repository: 'mofua-suki/issue-desk-intake',
              max: 2,
              implementing: 1,
              publishing: 1,
            },
            {
              repository: 'mofua-suki/issue-desk-worker',
              max: 2,
              implementing: 1,
              publishing: 1,
            },
            {
              repository: 'owner/another-very-long-repository-name-for-mobile',
              max: 1,
              implementing: null,
              publishing: null,
            },
          ],
          maxImplementations: 5,
          capacity: 4,
          hostLoad: { implementing: 2, publishing: 1 },
          implementing: listReads > 1 ? 2 : 1,
          publishing: 1,
          claimedTotal: 9,
          updatedAt: '2026-11-01T00:00:00Z',
          alive: true,
          load: {
            cpuPercent: listReads > 1 ? 42 : 21,
            freeMemoryMB: 2048,
            freeDiskMB: 4096,
            gpuPercent: 5,
          },
          activity: {
            phase: { name: '実装' },
            skip: { reason: '待機' },
            claims: [{ project: 'owner/repo', issue: 1 }],
          },
          environmentHold: { reason: '修復方法' },
          ...(drained ? { drain: { indefinite: true } } : {}),
        },
        {
          name: 'pc-b',
          repositories: [],
          maxImplementations: 4,
          capacity: 4,
          hostLoad: { implementing: 3, publishing: 1 },
          load: {},
        },
        {
          name: 'pc-zero',
          maxImplementations: 4,
          capacity: 0,
          hostLoad: { implementing: 0, publishing: 0 },
        },
        {
          name: 'pc-limited',
          maxImplementations: 3,
          capacity: 6,
          hostLoad: { implementing: 0, publishing: 0 },
        },
        {
          name: 'pc-max-zero',
          maxImplementations: 0,
          capacity: 4,
          hostLoad: { implementing: 0, publishing: 0 },
        },
        {
          name: 'pc-null-capacity',
          maxImplementations: 4,
          capacity: null,
          hostLoad: { implementing: 0, publishing: 0 },
        },
        {
          name: 'pc-test',
          role: 'test-only',
          testRepositories: [],
          maxImplementations: 4,
          capacity: 4,
          hostLoad: { implementing: 0, publishing: 0 },
        },
        { name: 'pc-c', repositories: null },
        { name: 'pc-old' },
        {
          name: 'pc-one',
          repositories: ['mofua-suki/issue-desk-worker'],
          repositoryLoads: [
            {
              repository: 'mofua-suki/issue-desk-worker',
              max: 2,
              implementing: 1,
              publishing: 1,
            },
          ],
          maxImplementations: 3,
          drain: { until: '2026-12-01T00:00:00Z' },
        },
        { name: 'pc-legacy', repositories: ['owner/legacy'] },
        { name: 'pc-empty-name', repositories: ['owner/valid', ''] },
        { name: 'pc-invalid-name', repositories: ['owner/valid', 42] },
      ],
    });
    const fixtureID = '11111111-1111-4111-8111-111111111111';
    const workPeriod = {
      fromDate: '2026-10-01',
      toDate: '2026-10-07',
      date: '2026-10-01',
      measured: null,
      days: [],
    };
    // 旧受付が省略する追加分類と、元から存在する分類の欠測を区別する。
    const reasonCounts = {
      success: 1,
      'fixed-test': 0,
      checks: 0,
      timeout: 0,
      question: 0,
      prerequisite: 0,
      guard: 0,
      'usage-limit': 0,
      'review-hold': 0,
      stopped: 2,
      'lease-expired': 3,
      unknown: 0,
    };
    const legacyReasons = Object.fromEntries(
      Object.entries(reasonCounts).filter(
        ([key]) => !['stopped', 'lease-expired', 'checks'].includes(key),
      ),
    );
    const implementationPeriod = (reasons: Record<string, number>) => ({
      runs: Object.values(reasons).reduce((total, count) => total + count, 0),
      noImplementationRecord: 0,
      reasons,
      unknownGenerations: 0,
      days: [{ day: '2026-10-01', reasons }],
      distribution: [{ implementations: 1, issues: 1 }],
      unknownIssues: 0,
      skipped: 0,
    });
    const implementation = {
      before: implementationPeriod(reasonCounts),
      after: implementationPeriod(legacyReasons),
    };
    const draft = {
      id: fixtureID,
      project: 'owner/repo',
      status: 'draft',
      body: '相談本文',
      title: '型付き画面の相談',
      messages: [
        { id: 'user-one', role: 'user', text: '相談本文' },
        { id: 'assistant-one', role: 'assistant', text: assistantAnswer },
      ],
      actions: ['chat'],
      provider: 'codex',
    };
    const handoffs = [
      {
        id: '22222222-2222-4222-8222-222222222222',
        target: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
        filing: 'pending',
        label: '未起票',
      },
      {
        id: '33333333-3333-4333-8333-333333333333',
        target: 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb',
        filing: 'filed',
        label: '起票済み',
      },
      {
        id: '44444444-4444-4444-8444-444444444444',
        target: 'cccccccc-cccc-4ccc-8ccc-cccccccccccc',
        filing: undefined,
        label: '状態不明',
      },
      {
        id: '55555555-5555-4555-8555-555555555555',
        target: '',
        filing: undefined,
        label: '引き継ぎ先不明',
      },
    ].map(({ id, target, filing, label }) => ({
      id,
      project: 'owner/repo',
      status: 'project_handoff',
      title: `引き継ぎ相談 ${label}`,
      body: `元の本文 ${label}`,
      messages: [
        { id: `message-${label}`, role: 'user', text: `元の会話 ${label}` },
      ],
      actions: [],
      provider: 'codex',
      projectHandoff: {
        id: target,
        ...(filing ? { status: 'registered', filing } : {}),
      },
    }));
    const handoffMutations: string[] = [];
    const grantID = '11111111111141118111111111111111';
    const grant = {
      id: grantID,
      kind: 'host-ssh',
      target: 'test-host',
      status: 'active',
      createdAt: '2026-11-01T00:00:00Z',
      createdBy: 'test',
      evidence: { quote: 'テストでの許可', statedAt: '2026-11-01T00:00:00Z' },
    };
    const ticket = {
      id: fixtureID,
      host: 'test-host',
      operation: 'inspect',
      reason: '接続を確認',
      requester: 'test',
      status: 'requested',
      requested_at: '2026-11-01T00:00:00Z',
    };
    const todoWrites: { id?: string; text?: string; revision?: string }[] = [];
    let todo = {
      id: '',
      project: 'owner/repo',
      body: '',
      images: [],
      revision: 'first-todo',
      updatedAt: '2026-11-01T00:00:00Z',
    };
    let resolveBrowser: (result: {
      success: boolean;
      message?: string;
    }) => void = () => {};
    const browserResult = new Promise<{ success: boolean; message?: string }>(
      (resolve) => {
        resolveBrowser = resolve;
      },
    );
    const parentConsultation = {
      id: 'parent-consultation',
      project: 'owner/repo',
      status: 'submitted',
      title: '表示しない親相談',
      hasChildren: true,
      familyProgress: {
        total: 2,
        remaining: 2,
        completed: 0,
        closed: 0,
        stopped: [],
        unknown: 0,
      },
    };
    const issue = {
      project: 'owner/repo',
      number: 1,
      desk: 101,
      title: 'ブラウザ確認用の依頼',
      body: '依頼の本文',
      status: 'waiting',
      requiresUser: true,
      investigation: false,
      dependencyReason: '',
      actions: [
        'reply',
        'retry',
        'resume',
        'stop',
        'review',
        'priority',
        'handover',
        'reason',
        'approve_deletions',
        'approve_permissions',
      ],
      comments: [
        { kind: 'result', body: '完了結果', createdAt: '2026-11-01T00:00:00Z' },
      ],
      images: [],
      commentCount: 1,
      contentRevision: 'first',
      transferPending: true,
      overLimitApprovals: [
        { grantId: 'secret-grant', id: 'secret-send', sourcePc: 'secret-pc' },
      ],
      url: 'https://example.invalid/issue/1',
      updatedAt: '2026-11-01T00:00:00Z',
      createdAt: '2026-11-01T00:00:00Z',
      host: 'nas',
    };
    let issueReads = 0;
    const liveResponses = new Set<http.ServerResponse>();
    let resizeSocket: WebSocket | undefined;
    let resizeSequence = 0;
    cleanup(() => resizeSocket?.close());
    const server = http.createServer(async (request, response) => {
      const target = new URL(request.url ?? '/', 'http://localhost');
      if (target.pathname === '/__resize') {
        try {
          const pages = (await (
            await fetch(new URL('/json/list', devTools))
          ).json()) as { type: string; webSocketDebuggerUrl: string }[];
          const page = pages.find((item) => item.type === 'page');
          assert.ok(page);
          if (!resizeSocket) {
            resizeSocket = new WebSocket(page.webSocketDebuggerUrl);
            await new Promise<void>((resolve, reject) => {
              resizeSocket!.onerror = () =>
                reject(Error('DevTools接続に失敗しました'));
              resizeSocket!.onopen = () => resolve();
            });
          }
          const socket = resizeSocket;
          const id = ++resizeSequence;
          await new Promise<void>((resolve, reject) => {
            socket.onerror = () => reject(Error('DevTools接続に失敗しました'));
            socket.onmessage = (event) => {
              const result = JSON.parse(String(event.data));
              if (result.id === id) {
                if (result.error) reject(Error(result.error.message));
                else resolve();
              }
            };
            socket.send(
              JSON.stringify({
                id,
                method: 'Emulation.setDeviceMetricsOverride',
                params: {
                  width: Number(target.searchParams.get('width')),
                  height: 800,
                  deviceScaleFactor: 1,
                  mobile: false,
                },
              }),
            );
          });
          // Emulation is tied to the DevTools session; keep it alive until cleanup.
          response.end('{}');
        } catch (error) {
          response.writeHead(500);
          response.end(String(error));
        }
        return;
      }
      if (target.pathname === '/__issue_refresh') {
        issue.comments.push({
          kind: 'result',
          body: '更新結果' + issue.comments.length,
          createdAt: '2026-11-01T01:00:00Z',
        });
        issue.commentCount = issue.comments.length;
        issue.contentRevision = 'revision-' + issue.commentCount;
        if (target.searchParams.get('notice') === 'true')
          for (const stream of liveResponses)
            stream.write(
              'data: ' +
                JSON.stringify({
                  type: 'issue',
                  project: issue.project,
                  number: issue.number,
                }) +
                '\n\n',
            );
        response.end(
          JSON.stringify({ issueReads, commentCount: issue.commentCount }),
        );
        return;
      }
      if (target.pathname === '/__issue_state') {
        issue.status = target.searchParams.get('status') || 'waiting';
        issue.investigation = target.searchParams.get('reported') === 'true';
        issue.dependencyReason = target.searchParams.get('reason') || '';
        response.end('{}');
        return;
      }
      if (target.pathname === '/__list_check') {
        if (request.method === 'POST')
          emptyList = target.searchParams.get('empty') !== 'false';
        response.setHeader('Content-Type', 'application/json');
        response.end(
          JSON.stringify({ listReads, listAuxReads, drainRequests }),
        );
        return;
      }
      if (target.pathname === '/__browser_result') {
        const chunks: Buffer[] = [];
        for await (const chunk of request) chunks.push(Buffer.from(chunk));
        resolveBrowser(JSON.parse(Buffer.concat(chunks).toString('utf8')));
        response.end('{}');
        return;
      }
      if (target.pathname.startsWith('/api/')) {
        reads.add(target.pathname);
        if (target.pathname === '/api/list') listReads++;
        if (
          target.pathname === '/api/ssh-tickets' ||
          target.pathname === '/api/permission-grants'
        )
          listAuxReads.push(target.pathname);
        const handoff = handoffs.find(
          (item) =>
            target.pathname === `/api/drafts/${item.id}` ||
            target.pathname.startsWith(`/api/drafts/${item.id}/`),
        );
        if (handoff && request.method !== 'GET' && request.method !== 'HEAD') {
          handoffMutations.push(`${request.method} ${target.pathname}`);
          response.writeHead(405);
          response.end('{}');
          return;
        }
        if (handoff && target.pathname === `/api/drafts/${handoff.id}`) {
          response.setHeader('Content-Type', 'application/json');
          response.end(JSON.stringify(handoff));
          return;
        }
        response.setHeader('Content-Type', 'application/json');
        let value: unknown;
        switch (target.pathname) {
          case '/api/session':
            value = { authenticated: true, csrf: 'browser-csrf', local: false };
            break;
          case '/api/meta':
            value = {
              providers: [{ id: 'codex', name: 'Codex', ready: true }],
              projects: [{ id: 'owner/repo', name: 'repo' }],
              worker: true,
              projectCreation: { owner: 'owner' },
            };
            break;
          case '/api/health':
            value = { ready: true, ui: built.ui };
            break;
          case '/api/issue':
            issueReads++;
            value = issue;
            break;
          case '/api/list':
            value = {
              full: true,
              cursor: 'browser-list',
              rows: emptyList
                ? []
                : [
                    {
                      key: 'draft:parent-consultation',
                      type: 'draft',
                      item: parentConsultation,
                    },
                    { key: 'issue:owner/repo#1', type: 'issue', item: issue },
                    ...handoffs.map((item) => ({
                      key: `draft:${item.id}`,
                      type: 'draft',
                      item,
                    })),
                  ],
              aux: {
                reviews: [],
                fullTests: [],
                stalls: [],
                attentionAlerts: [],
              },
              unavailable: [],
              hosts: hosts(),
            };
            break;
          case '/api/hosts':
            value = hosts();
            break;
          case '/api/hosts/pc-a/drain': {
            assert.ok(request.method === 'POST' || request.method === 'DELETE');
            assert.equal(request.headers['x-csrf-token'], 'browser-csrf');
            if (request.method === 'POST') {
              const chunks: Buffer[] = [];
              for await (const chunk of request)
                chunks.push(Buffer.from(chunk));
              const input = JSON.parse(Buffer.concat(chunks).toString('utf8'));
              assert.deepEqual(input, {
                indefinite: true,
                reason: '受付画面から無期限停止',
              });
            }
            drainRequests.push(request.method);
            drained = request.method === 'POST';
            value = {
              host: 'pc-a',
              drain: drained ? { indefinite: true } : null,
            };
            break;
          }
          case '/api/ssh-tickets':
            value = { tickets: [ticket] };
            break;
          case '/api/ssh-tickets/' + fixtureID:
            value = ticket;
            break;
          case '/api/permission-grants':
            value = { grants: [grant] };
            break;
          case '/api/permission-grants/' + grantID:
            value = grant;
            break;
          case '/api/model-overrides':
            value = {
              locked: true,
              overrides: [],
              defaults: {},
              steps: [],
              providers: ['codex'],
              efforts: [],
            };
            break;
          case '/api/tokens':
            value = [
              {
                id: fixtureID,
                name: 'test-host',
                delegate: false,
                issuedAt: '2026-11-01T00:00:00Z',
              },
            ];
            break;
          case '/api/projects':
            value = [];
            break;
          case '/api/projects/' + fixtureID:
            value = {
              id: fixtureID,
              status: 'draft',
              title: '新しいシステム',
              messages: [
                { id: 'project-user', role: 'user', text: '新しいシステム' },
              ],
              actions: ['chat'],
            };
            break;
          case '/api/drafts/' + fixtureID:
            value = draft;
            break;
          case '/api/todos/' + fixtureID:
            value = {
              id: fixtureID,
              project: 'owner/repo',
              body: '保存済みTODO',
              images: [],
              revision: 'fixture-todo',
              actions: ['save'],
              provider: 'codex',
            };
            break;
          case '/api/stats/overview':
            value = {
              unavailable: [],
              implementations: {
                byHost: [{ host: 'test-host', completed: 2 }],
                byProject: [{ project: 'owner/repo', completed: 2 }],
              },
              implementationTiers: [{ tier: 'standard', count: 2 }],
              consultTiers: [],
              tokens: {
                steps: [
                  {
                    step: 'implementation',
                    weighted: 100,
                    percent: 100,
                    unknown: 0,
                  },
                ],
                weighted: 100,
                unknown: 0,
              },
              models: [
                {
                  step: 'implementation',
                  provider: 'codex',
                  model: 'model',
                  effort: 'medium',
                  count: 1,
                  percent: 100,
                },
              ],
              roundTrips: {
                zero: 0,
                distribution: [{ rounds: 1, count: 1 }],
                average: 1,
                threshold: 10,
                many: 0,
                missing: 0,
              },
              durations: {
                review: {
                  averageMs: 1000,
                  medianMs: 1000,
                  count: 1,
                  missing: 0,
                },
              },
              daily: [
                { date: '2026-11-01', completed: 2, failed: 0, tokens: 100 },
              ],
            };
            break;
          case '/api/stats':
            assert.equal(target.searchParams.get('details'), 'true');
            value = {
              project: '',
              before: workPeriod,
              after: workPeriod,
              unavailable: [],
              implementation,
            };
            break;
          case '/api/provider-disables':
            value = { disables: [] };
            break;
          case '/api/events':
            response.writeHead(200, { 'Content-Type': 'text/event-stream' });
            response.write(': connected\n\n');
            liveResponses.add(response);
            response.on('close', () => liveResponses.delete(response));
            return;
          case '/api/todos': {
            if (request.method === 'GET') {
              value = {
                items: [],
                total: 0,
                page: 1,
                limit: 100,
                hasMore: false,
              };
              break;
            }
            assert.equal(request.headers['x-csrf-token'], 'browser-csrf');
            const chunks: Buffer[] = [];
            for await (const chunk of request) chunks.push(Buffer.from(chunk));
            const input = JSON.parse(Buffer.concat(chunks).toString('utf8'));
            todo.id = input.id;
            value = todo;
            break;
          }
          case '/api/comment': {
            assert.equal(request.headers['x-csrf-token'], 'browser-csrf');
            const chunks: Buffer[] = [];
            for await (const chunk of request) chunks.push(Buffer.from(chunk));
            mutations.push(JSON.parse(Buffer.concat(chunks).toString('utf8')));
            value = {};
            break;
          }
          default: {
            if (/^\/api\/todos\/[^/]+\/save$/.test(target.pathname)) {
              assert.equal(request.headers['x-csrf-token'], 'browser-csrf');
              const chunks: Buffer[] = [];
              for await (const chunk of request)
                chunks.push(Buffer.from(chunk));
              const input = JSON.parse(Buffer.concat(chunks).toString('utf8'));
              todoWrites.push(input);
              todo = { ...todo, body: input.text, revision: 'saved-todo' };
              value = todo;
              break;
            }
            response.writeHead(404);
            response.end(
              JSON.stringify({ error: 'unexpected API: ' + target.pathname }),
            );
            return;
          }
        }
        response.end(JSON.stringify(value));
        return;
      }
      if (target.pathname === '/browser-check.js') {
        response.setHeader('Content-Type', 'text/javascript');
        response.end(`
const pause = () => new Promise(resolve => setTimeout(resolve, 10));
const wait = async fn => { for (let i=0;i<300;i++) { if(fn()) return; await pause(); } throw Error('画面の待機が時間切れです: '+document.body.innerText); };
const check = (value, message) => { if (!value) throw Error(message); };
// 接続中の一覧は、最後の取得時刻から30秒になるように残り時間を予約する。
// 未接続時の15秒以下のタイマーは実時間のまま。周期の境界は list-timing.test.ts で検証する。
const nativeSetTimeout = window.setTimeout.bind(window);
const nativeClearTimeout = window.clearTimeout.bind(window);
const listRefreshTimers = new Map();
window.setTimeout = (callback, delay, ...args) => {
  if (!(delay > 15000 && delay <= 30000)) return nativeSetTimeout(callback, delay, ...args);
  const run = () => {
    listRefreshTimers.delete(id);
    return callback(...args);
  };
  const id = nativeSetTimeout(run, delay);
  listRefreshTimers.set(id, run);
  return id;
};
window.clearTimeout = id => {
  listRefreshTimers.delete(id);
  nativeClearTimeout(id);
};
const runListRefresh = async () => {
  const deadline = performance.now() + 35000;
  while (listRefreshTimers.size === 0 && performance.now() < deadline) await pause();
  check(listRefreshTimers.size === 1, '接続中の一覧の定期更新を1件取得できません');
  const [id, run] = listRefreshTimers.entries().next().value;
  nativeClearTimeout(id);
  await run();
};
const readList = async () => (await fetch('/__list_check')).json();
try {
  await import('/v/${built.ui}/app.js');
  await wait(() => document.querySelector('h1')?.textContent === 'ブラウザ確認用の依頼');
  check(location.hash === '#/issues/101', '共通番号のURLを保持していません');
  document.querySelector('.back').click();
  await wait(() => document.querySelector('.issue-row'));
  check(document.querySelector('h1')?.textContent === '依頼一覧', '一覧へ戻れません');
  const checkList = () => {
    check(!document.querySelector('#families') && !document.querySelector('.family-heading') && !document.body.innerText.includes('表示しない親相談'), '親相談が一覧に表示されました');
    check(![...document.querySelectorAll('#app button, #app a')].some(node => /^(新しいプロジェクト|＋ 新しい依頼|アイディアを書く →|AIと相談する →|ホスト作業用SSHの申請（承認待ちを確認）|許可の付与（有効な件数を確認）)$/.test(node.textContent.trim()) || ['#/new', '#/projects', '#/ssh-tickets', '#/permission-grants'].includes(node.getAttribute('href'))), '一覧に削除対象の入口があります');
    check(!document.body.innerText.includes('新しい依頼の画面'), 'TODOの古い案内があります');
    check(document.querySelector('.stats-link')?.getClientRects().length > 0, '稼働実績が表示されません');
  };
  checkList();
  const visit = async (route, ready) => { location.hash = route; await wait(ready).catch(error => { throw Error(route + ': ' + error.message); }); check(document.querySelector('#notice')?.hidden !== false, '画面でエラーになりました: '+document.querySelector('#notice')?.textContent); };
  check(document.querySelectorAll('.hosts-table').length === 2, 'PCの表が2つありません');
  const assignment = document.querySelector('.hosts-assignment');
  const load = document.querySelector('.hosts-load');
  const assignmentHeaders = [...assignment.querySelectorAll('th')].map(cell => cell.textContent);
  const repositoriesColumn = assignmentHeaders.indexOf('担当リポジトリ');
  check(repositoriesColumn >= 0 && assignmentHeaders.lastIndexOf('担当リポジトリ') === repositoriesColumn, '担当列を一意に取得できません headers=' + JSON.stringify(assignmentHeaders));
  check(assignment.innerText.includes('担当リポジトリ') && !assignment.innerText.includes('空きメモリ'), '担当の分類が違います');
  check(load.innerText.includes('空きメモリ') && !load.innerText.includes('担当リポジトリ'), '負荷の分類が違います');
  check(assignment.innerText.includes('担当なし') && [...assignment.querySelectorAll('tbody tr')].filter(row => /pc-c|pc-old/.test(row.innerText)).every(row => row.innerText.includes('不明')), '担当の空と不明を区別できません');
  for (const name of ['pc-empty-name', 'pc-invalid-name']) {
    const row = [...assignment.querySelectorAll('tbody tr')].find(row => row.cells[0]?.textContent === name);
    check(row?.cells[repositoriesColumn]?.textContent === '不明', name + 'の不正な担当が不明になりません headers=' + JSON.stringify([...assignment.querySelectorAll('th')].map(cell => cell.textContent)) + ' cells=' + JSON.stringify([...(row?.cells || [])].map(cell => cell.textContent)));
  }
  check(assignment.innerText.includes('修復方法') && assignment.innerText.includes('占有') && document.querySelector('.hosts-monitor'), '詳細または監視停止がありません');
  check((await fetch('/__resize?width=375')).ok, 'スマホ幅に変更できません');
  await wait(() => innerWidth === 375);
  for (const table of document.querySelectorAll('.hosts-table')) {
    const cell = table.querySelector('tbody tr td');
    const initial = cell.getBoundingClientRect();
    table.scrollLeft = table.scrollWidth;
    await pause();
    const moved = cell.getBoundingClientRect();
    check(table.scrollLeft > 0 && Math.abs(initial.left - moved.left) < 2 && moved.right <= innerWidth, 'スクロール後にPC名が見えません: '+JSON.stringify({table:table.className,scroll:table.scrollLeft,width:table.clientWidth,scrollWidth:table.scrollWidth,viewport:innerWidth,initial:{left:initial.left,right:initial.right},moved:{left:moved.left,right:moved.right}}));
  }
  check(document.documentElement.scrollWidth <= innerWidth, 'ページが横にはみ出しています');
  check((await fetch('/__resize?width=1200')).ok, '画面幅を戻せません');
  await wait(() => innerWidth === 1200);
  check([...document.querySelectorAll('.hosts-assignment th')].map(x => x.textContent).join(',') === 'PC,役割,状態,担当リポジトリ,総枠,使用中,空き,停止', '担当表の列が違います headers=' + JSON.stringify([...document.querySelectorAll('.hosts-assignment th')].map(cell => cell.textContent)));
  check(!document.querySelector('.hosts-assignment select'), '期限選択が残っています');
  const rowA = [...document.querySelectorAll('.hosts-assignment tbody tr')].find(row => row.cells[0]?.textContent === 'pc-a');
  const slots = row => ['総枠', '使用中', '空き'].map(name => row.cells[assignmentHeaders.indexOf(name)].textContent).join(',');
  check((rowA.cells[repositoriesColumn].innerText.match(/issue-desk/g) || []).length === 1 && !/上限|実装中|公開中/.test(rowA.cells[repositoriesColumn].innerText), '担当名の表示が違います');
  check(slots(rowA) === '4枠,3枠,1枠', 'PC全体の枠またはcapacityによる上限が違います: ' + slots(rowA));
  const full = [...assignment.querySelectorAll('tbody tr')].find(row => row.cells[0]?.textContent === 'pc-b');
  check(slots(full) === '4枠,4枠,0枠', '満枠の表示が違います');
  for (const [name, expected] of [['pc-zero', '0枠,0枠,0枠'], ['pc-limited', '3枠,0枠,3枠'], ['pc-max-zero', '0枠,0枠,0枠'], ['pc-null-capacity', '不明,0枠,不明']]) {
    const row = [...assignment.querySelectorAll('tbody tr')].find(row => row.cells[0]?.textContent === name);
    check(row && slots(row) === expected, name + 'の枠表示が違います: ' + (row ? slots(row) : '行なし'));
  }
  const testOnly = [...assignment.querySelectorAll('tbody tr')].find(row => row.cells[0]?.textContent === 'pc-test');
  check(slots(testOnly) === '−,−,−', 'テスト専用PCに枠が表示されました');
  const legacy = [...assignment.querySelectorAll('tbody tr')].find(row => row.cells[0]?.textContent === 'pc-legacy');
  check(slots(legacy) === '不明,不明,不明', '旧応答の欠測が不明になりません');
  const one = [...document.querySelectorAll('.hosts-assignment tbody tr')].find(row => row.cells[0]?.textContent === 'pc-one');
  check(one.innerText.includes(new Date('2026-12-01T00:00:00Z').toLocaleString('ja-JP', {month:'numeric',day:'numeric',hour:'2-digit',minute:'2-digit'})), '期限付き停止の日時が違います');
  const before = await (await fetch('/__list_check', {method:'POST'})).json();
  check(before.listReads > 0 && before.listAuxReads.length === 0, '一覧初回に件数APIを呼びました');
  await runListRefresh();
  await wait(() => document.querySelector('#items .empty'));
  let after = await readList();
  check(after.listReads > before.listReads, '一覧の定期更新が一覧APIを取得しません: ' + JSON.stringify({before, after}));
  check(document.querySelector('.hosts-assignment').innerText.includes('issue-desk') && document.querySelector('.hosts-assignment').innerText.includes('3枠') && document.querySelector('.hosts-load').innerText.includes('42%'), '担当・件数・負荷が更新されません');
  [...document.querySelectorAll('.hosts-assignment tbody tr')].find(row => row.cells[0]?.textContent === 'pc-a').querySelector('button').click();
  await wait(() => document.querySelector('.hosts-assignment').innerText.includes('停止中（期限なし）'));
  [...document.querySelectorAll('.hosts-assignment tbody tr')].find(row => row.cells[0]?.textContent === 'pc-a').querySelector('button').click();
  await wait(() => !document.querySelector('.hosts-assignment').innerText.includes('停止中（期限なし）'));
  check((await (await fetch('/__list_check')).json()).drainRequests.join(',') === 'POST,DELETE', '停止要求の回数が違います');

  // 描画後に非同期で開始される要求も、各画面へ移動する前に観測する。
  await new Promise(resolve => setTimeout(resolve, 1000));
  after = await (await fetch('/__list_check')).json();
  check(after.listAuxReads.length === 0, '一覧更新時に件数APIを呼びました');
  for (const tab of ['attention', 'issues', 'waiting', 'stopped', 'completed', 'closed', 'todos', 'followups']) {
    await visit('#/list/' + tab, () => location.hash === '#/list/' + tab && document.querySelector('#items.list-' + tab + ' .empty'));
    checkList();
  }
  await visit('#/list/attention', () => document.querySelector('#items.list-attention .empty'));
  const restored = await (await fetch('/__list_check?empty=false', {method:'POST'})).json();
  await runListRefresh();
  await wait(() => document.querySelector('.issue-row'));
  after = await readList();
  check(after.listReads > restored.listReads, '一覧の復元が一覧APIを取得しません');
  const resumed = [...document.querySelectorAll('.hosts-assignment tbody tr')].find(row => row.cells[0]?.textContent === 'pc-a');
  check(!resumed.innerText.includes('停止中（期限なし）') && [...resumed.querySelectorAll('button')].filter(button => button.textContent === '無期限で止める').length === 1, '定期更新後の停止解除が保持されません');
  document.querySelector('.issue-row').click();
  await wait(() => document.querySelector('.issue-content'));
  for (const width of [1200, 390]) {
    check((await fetch('/__resize?width=' + width)).ok, '画面幅を変更できません');
    await wait(() => innerWidth === width);
    check(!document.querySelector('textarea, input, select, .composer, .docked'), '詳細に操作UIがあります');
    check(!/必要な対応|ほかの操作|相談・未送信入力を確認|secret-grant|secret-send|secret-pc/.test(document.body.innerText), '詳細に不要な操作や承認情報があります');
    check(document.body.innerText.includes('依頼の本文') && document.body.innerText.includes('質問への回答を待っています'), '内容または状況がありません');
  }
  [...document.querySelectorAll('button')].find(b => b.textContent.includes('全文を見る')).click();
  await wait(() => document.querySelector('.request-body'));
  [...document.querySelectorAll('button')].find(b => b.textContent.includes('会話履歴を見る')).click();
  await wait(() => document.body.innerText.includes('完了結果'));
  for (const notice of [true, false]) {
    const refresh = await (await fetch('/__issue_refresh?notice=' + notice)).json();
    const expected = '更新結果' + (refresh.commentCount - 1);
    for (let i=0; i<350 && !document.body.innerText.includes(expected); i++) await new Promise(resolve => setTimeout(resolve, 100));
    check(document.body.innerText.includes(expected), (notice ? '通知' : '定期') + '更新で開いた履歴が再取得されません');
    check(document.querySelector('.request-body') && document.body.innerText.includes('依頼の本文'), '更新で全文の折り畳みが閉じました');
  }
  for (const [status, expected, reported, reason] of [
    ['review_held', 'レビューが保留されています。', false, ''],
    ['stopped', '作業は停止しています。', false, ''],
    ['failed', '作業が失敗しました。', false, ''],
    ['closed', '依頼は閉じられています。', false, ''],
    ['prerequisite_waiting', '必要な前提成果物を待っています。', false, ''],
    ['dependency_waiting', '確認用の先行成果物を待っています。', false, '確認用の先行成果物を待っています。'],
    ['completed', '調査の報告コメントを投稿し、PRを作らずにIssueを閉じました。', true, ''],
  ]) {
    await fetch('/__issue_state?' + new URLSearchParams({status, reported:String(reported), reason}));
    await visit('#/list/issues', () => document.querySelector('h1')?.textContent === '依頼一覧');
    await visit('#/issues/101', () => document.querySelector('.issue-content') && document.body.innerText.includes(expected));
    for (const width of [1200, 390]) {
      check((await fetch('/__resize?width=' + width)).ok, '画面幅を変更できません');
      await wait(() => innerWidth === width);
      check(!document.querySelector('textarea, input, select, .composer, .docked'), '状態別詳細に操作UIがあります: ' + status);
      check(document.body.innerText.includes(expected), '状態の案内が異なります: ' + status);
    }
  }
  await fetch('/__issue_state?status=waiting');
  document.querySelector('.back').click();
  await wait(() => document.querySelector('h1')?.textContent === '依頼一覧');
  location.hash = '#/new';
  await wait(() => document.querySelector('textarea[aria-label="変更したいこと"]')?.disabled === false);
  const project = document.querySelector('select[aria-label="プロジェクト"]');
  project.value = 'owner/repo'; project.dispatchEvent(new Event('change'));
  const body = document.querySelector('textarea[aria-label="変更したいこと"]');
  body.value = 'あとで相談する内容'; body.dispatchEvent(new Event('input', {bubbles:true}));
  await pause(); await pause();
  [...document.querySelectorAll('button')].find(button => button.textContent === 'TODOとして保存').click();
  await wait(() => document.querySelector('h1')?.textContent === '依頼一覧' && document.querySelector('#toast')?.textContent.includes('TODOを保存'));
  await visit('#/todos/${fixtureID}', () => document.querySelector('textarea[aria-label="TODOの内容"]')?.value === '保存済みTODO');
  await visit('#/drafts/${fixtureID}', () => document.querySelector('textarea[aria-label="AIへのメッセージ"]')?.disabled === false);
  check(document.body.innerText.includes('相談本文'), '相談の本文が表示されません');
  const answer = document.querySelector('.conversation .message.assistant .md')?.innerText;
  const expectedAnswer = ${JSON.stringify(assistantAnswer)};
  check(answer && answer.replace(/\\s+/g, '') === expectedAnswer.replace(/\\s+/g, ''), 'サーバーの回答が順番どおり表示されません: ' + answer);
  await visit('#/list/issues', () => document.querySelector('h1')?.textContent === '依頼一覧');
  await visit('#/drafts/${fixtureID}', () => document.querySelector('.conversation .message.assistant .md'));
  const reopenedAnswer = document.querySelector('.conversation .message.assistant .md')?.innerText;
  check(reopenedAnswer && reopenedAnswer.replace(/\\s+/g, '') === expectedAnswer.replace(/\\s+/g, ''), '相談を開き直すと回答が変わります: ' + reopenedAnswer);
  const handoffs = ${JSON.stringify(handoffs)};
  const checkHandoff = async item => {
    await wait(() => document.querySelector('.conversation')?.innerText.includes(item.messages[0].text));
    check(location.hash === '#/drafts/' + item.id, '元の相談URLではありません: ' + location.hash);
    [...document.querySelectorAll('button')].find(button => button.textContent === '依頼内容').click();
    check(document.body.innerText.includes('引き継いだ記録') && document.body.innerText.includes(item.projectHandoff.id || '不明'), '引き継ぎの記録またはIDがありません: ' + item.id);
    [...document.querySelectorAll('button')].find(button => button.textContent === '相談').click();
    check(document.querySelector('textarea[aria-label="AIへのメッセージ"]')?.disabled, '編集できます: ' + item.id);
    check(document.querySelector('.composer button.primary')?.disabled, '送信できます: ' + item.id);
    const workspace = document.querySelector('#app .workspace');
    check(workspace, '相談画面がありません: ' + item.id);
    check(![...workspace.querySelectorAll('button, a')].some(node => /開く|相談を再試行|相談を再開|起票を再試行|送信結果を照合/.test(node.textContent) || (item.projectHandoff.id && node.getAttribute('href')?.includes(item.projectHandoff.id))), '引き継ぎ先を開く、または再送する操作があります: ' + item.id);
  };
  for (const item of handoffs) {
    await visit('#/list/completed', () => [...document.querySelectorAll('.issue-row')].some(row => row.innerText.includes(item.id)));
    for (const expected of handoffs) check([...document.querySelectorAll('.issue-row')].some(row => row.innerText.includes(expected.id)), '完了一覧にありません: ' + expected.id);
    [...document.querySelectorAll('.issue-row')].find(row => row.innerText.includes(item.id)).click();
    await checkHandoff(item);
    await visit('#/list/completed', () => [...document.querySelectorAll('.issue-row')].some(row => row.innerText.includes(item.id)));
    await visit('#/drafts/' + item.id, () => document.querySelector('.conversation')?.innerText.includes(item.messages[0].text));
    await checkHandoff(item);
  }
  await visit('#/list/issues', () => document.querySelector('#items.list-issues'));
  for (const width of [1200, 375]) {
    check((await fetch('/__resize?width=' + width)).ok, '画面幅を変更できません');
    await wait(() => innerWidth === width);
    checkList();
    for (const selector of ['.models-link', '.tokens-link']) {
      check((document.querySelector(selector)?.getClientRects().length > 0) === (width > 760), selector + ' の表示幅が不正です');
    }
  }
  check((await fetch('/__resize?width=1200')).ok, '画面幅を戻せません');
  await wait(() => innerWidth === 1200);
  await visit('#/models', () => document.querySelector('#model-overrides')?.innerText.includes('固定規則'));
  await visit('#/tokens', () => document.querySelector('.token-name')?.textContent === 'test-host');
  await visit('#/ssh-tickets', () => document.querySelector('a[href="#/ssh-tickets/${fixtureID}"]'));
  await visit('#/ssh-tickets/${fixtureID}', () => document.body.innerText.includes('接続を確認'));
  await visit('#/permission-grants', () => document.querySelector('a[href="#/permission-grants/${grantID}"]'));
  await visit('#/permission-grants/${grantID}', () => document.body.innerText.includes('テストでの許可'));
  await visit('#/projects', () => document.body.innerText.includes('新しいプロジェクトの相談はまだありません'));
  await visit('#/projects/${fixtureID}', () => document.querySelector('textarea[aria-label="返信"]'));
  await visit('#/list/issues', () => document.querySelector('h1')?.textContent === '依頼一覧');
  await wait(() => document.querySelector('.stats-link')?.getClientRects().length > 0);
  check(![...document.querySelectorAll('button')].some(button => button.textContent === '後続案の整理記録'), '削除したボタンが表示されました');
  await visit('#/proposal-dismissals', () => document.querySelector('h1')?.textContent === '依頼一覧' && location.hash !== '#/proposal-dismissals');
  check(location.hash === '#/list/issues', '旧URLから一覧タブを維持できません');
  const wentBack = new Promise(resolve => window.addEventListener('popstate', resolve, { once: true }));
  history.back();
  await wentBack;
  await wait(() => location.hash === '#/list/issues' && document.querySelector('h1')?.textContent === '依頼一覧');
  await pause();
  check(location.hash === '#/list/issues', '戻る操作で旧URLに戻りました');
  await visit('#/stats', () => document.querySelector('#stats-result')?.innerText.includes('前後の比較') && document.querySelector('.ops-results svg'));
  check(document.querySelectorAll('.ops-results svg').length >= 8, '概要のグラフが不足しています');
  const reasonTables = [...document.querySelectorAll('.implementation-days table')];
  const reasonCell = (table, label) => {
    const index = [...table.querySelectorAll('th')].findIndex((th) => th.textContent === label);
    return index < 0 ? null : table.querySelector('tbody tr').children[index].textContent;
  };
  check(reasonTables.length === 2, '実装の終了理由の表がありません');
  check(reasonCell(reasonTables[0], '停止') === '2件', '新しい応答の停止の件数を表示しません');
  check(reasonCell(reasonTables[1], '停止') === '0件', '停止を欠く旧応答を0件と表示しません');
  check(reasonCell(reasonTables[0], '実装リース失効') === '3件', '新しい応答の失効件数を表示しません');
  check(reasonCell(reasonTables[1], '実装リース失効') === '0件', '失効を欠く旧応答を0件と表示しません');
  check(reasonCell(reasonTables[1], '検査失敗') !== null && reasonCell(reasonTables[1], '検査失敗') !== '0件' && reasonCell(reasonTables[1], '成功') === '1件', '既存の検査失敗の欠測を0件で埋めました');
  const tierTabs = [...document.querySelectorAll('.ops-tabs [role=tab]')];
  const tierPanels = [...document.querySelectorAll('.ops-tabs [role=tabpanel]')];
  check(tierTabs.map((tab) => tab.textContent).join('|') === '実装の難易度|相談の難易度', '難易度のタブがありません');
  check(tierPanels[0].getClientRects().length > 0 && tierPanels[1].getClientRects().length === 0, '最初は実装の難易度だけを表示していません');
  tierTabs[1].click();
  check(tierPanels[0].getClientRects().length === 0 && tierPanels[1].getClientRects().length > 0 && tierTabs[1].getAttribute('aria-selected') === 'true', 'タブで相談の難易度へ切り替わりません');
  await fetch('/__browser_result', {method:'POST', body:JSON.stringify({success:true})});
} catch(error) {
  await fetch('/__browser_result', {method:'POST',body:JSON.stringify({success:false,message:error.message})});
}
`);
        return;
      }
      const file =
        target.pathname === '/' ? 'index.html' : target.pathname.slice(1);
      const resolved = path.resolve(dist, file);
      if (!resolved.startsWith(dist + path.sep) || !fs.existsSync(resolved)) {
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
      const source = fs.readFileSync(resolved);
      response.end(
        file === 'index.html'
          ? source
              .toString('utf8')
              .replace(
                /<script type="module"[^>]*><\/script>/,
                '<script type="module" src="/browser-check.js"></script>',
              )
          : source,
      );
    });
    await new Promise<void>((resolve, reject) => {
      server.once('error', reject);
      server.listen(0, '127.0.0.1', resolve);
    });
    cleanup(async () => {
      try {
        server.closeAllConnections();
      } finally {
        await new Promise<void>((resolve, reject) =>
          server.close((error) => (error ? reject(error) : resolve())),
        );
      }
    });
    const address = server.address();
    assert.ok(address && typeof address === 'object');
    const browser = await startChromium(
      t,
      chromium!,
      directory,
      `http://127.0.0.1:${address.port}/#/issues/101`,
      undefined,
      cleanup,
    );
    const devTools = browser.endpoint;
    let timeout: ReturnType<typeof setTimeout> | undefined;
    const outcome = await Promise.race([
      browserResult,
      new Promise<never>((_resolve, reject) => {
        timeout = setTimeout(
          () =>
            reject(Error('ブラウザが結果を返しません: ' + browser.stderr())),
          80000,
        );
      }),
      browser.exited.then((error) => {
        throw error;
      }),
    ]).finally(() => clearTimeout(timeout));
    assert.equal(outcome.success, true, outcome.message);
    browser.child.kill('SIGTERM');
    assert.deepEqual(
      handoffMutations,
      [],
      '引き継ぎ相談への変更要求が発生しました',
    );
    for (const item of handoffs)
      assert.ok(reads.has('/api/drafts/' + item.id), item.id);
    assert.deepEqual(mutations, [], '詳細から変更要求が発生しました');
    assert.equal(todoWrites.length, 1);
    assert.equal(todoWrites[0].text, 'あとで相談する内容');
    assert.equal(todoWrites[0].revision, 'first-todo');
    assert.ok(todoWrites[0].id);
    for (const endpoint of [
      '/api/model-overrides',
      '/api/tokens',
      '/api/stats',
      '/api/stats/overview',
      '/api/projects',
      '/api/permission-grants/' + grantID,
      '/api/ssh-tickets/' + fixtureID,
      '/api/drafts/' + fixtureID,
      '/api/todos/' + fixtureID,
    ])
      assert.ok(reads.has(endpoint), endpoint);
  },
);
