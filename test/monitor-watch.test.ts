import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { spawnSync } from 'node:child_process';
import { monitorCli } from '../scripts/monitor-watch.ts';
import {
  startMonitor,
  nextMonitor,
  receiveMonitor,
  cancelMonitor,
} from '../scripts/lib/monitor-runtime.ts';
import { evaluate, monitorConfig } from '../scripts/lib/monitor-contract.ts';
import type {
  Capability,
  MonitorConfig,
  MonitorState,
  Observation,
  Receipt,
} from '../scripts/lib/monitor-contract.ts';

const childEnv = { ...process.env };
delete childEnv.NODE_TEST_CONTEXT;

const epoch = Date.parse('2026-10-30T07:00:00Z');
const capability = (kind = 'read'): Capability => ({
  tool: kind === 'read' ? 'read-issue' : 'conversation',
  specification: 'verified session tool schema',
  checkedAt: epoch,
  available: true,
  kind,
  destination: 'current-conversation',
});
const config = (change: Partial<MonitorConfig> = {}): MonitorConfig => ({
  targets: [{ id: 'parent', conditions: ['merged', 'healthy'] }],
  predicate: 'all',
  mode: 'conditional',
  observeMs: 120_000,
  reportMs: 600_000,
  observer: capability(),
  reporter: capability('user-conversation'),
  end: { description: 'report completion', onSatisfied: true },
  cancellation: 'cancel this session watch',
  continuity: {
    scope: 'session',
    constraint: 'AI must keep this conversation active; no wakeup registered',
  },
  ...change,
});
const observation = (
  merged: boolean | null = true,
  healthy: boolean | null = true,
  at = epoch,
): Observation => ({
  samples: [{ target: 'parent', observedAt: at, values: { merged, healthy } }],
});
function receipt(s: MonitorState, change: Partial<Receipt> = {}): Receipt {
  const p = s.pending!;
  return {
    requestId: p.id,
    tool: p.tool,
    destination: p.destination,
    callId: `call-${p.id}`,
    evidence: 'tool response / displayed message reference',
    outcome: 'confirmed',
    ...change,
  };
}
function observed(c = config(), o = observation(), at = epoch) {
  const s = nextMonitor(startMonitor(c, 'run', epoch), epoch);
  return receiveMonitor(s, receipt(s, { observation: o }), at);
}

test('CLI handoff: verified fake tools deliver to user sink, authorized action result, then cancel stops work', (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'monitor-watch-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const file = path.join(dir, 'session.json');
  let now = epoch;
  let ids = 0;
  const io = { now: () => now, id: () => `id-${++ids}` };
  const cli = (command: string, input = {}) =>
    monitorCli([command, '--file', file], input, io);
  const c = config({
    end: { description: 'explicit cancellation', onSatisfied: false },
    action: {
      description: 'collect already requested verification',
      authorization: 'user request reference 42',
      capability: capability('operation'),
    },
  });
  // One real CLI launch; the preload supplies only the fake clock, never a production escape hatch.
  const launched = spawnSync(
    process.execPath,
    [
      '--import',
      `data:text/javascript,Date.now=()=>${epoch}`,
      'scripts/monitor-watch.ts',
      'start',
      '--file',
      file,
    ],
    { input: JSON.stringify({ config: c }), encoding: 'utf8', env: childEnv },
  );
  assert.equal(launched.status, 0, launched.stderr);
  assert.equal(JSON.parse(launched.stdout).notification, 'unsent');
  assert.equal(cli('status').notification, 'unsent');
  assert.throws(() => cli('start', { config: c }), /already exists/);
  const userSink: unknown[] = [];
  let actions = 0;
  let reads = 0;
  const invoke = (s: MonitorState) => {
    const p = s.pending!;
    assert.ok(p.id);
    if (p.kind === 'observe') {
      reads++;
      return receipt(s, { observation: observation() });
    }
    if (p.kind === 'report') {
      userSink.push(p.payload);
      return receipt(s, { outcome: 'accepted' });
    }
    actions++;
    return receipt(s);
  };
  let s = cli('next');
  assert.equal(
    cli('next').pending!.id,
    s.pending!.id,
    'pending request is stable, not a second tool call',
  );
  cli('result', { receipt: invoke(s) });
  s = cli('next');
  s = cli('result', { receipt: invoke(s) });
  assert.equal(
    s.notification,
    'accepted',
    'tool acceptance does not claim display',
  );
  assert.equal(s.pending!.kind, 'action');
  cli('result', { receipt: invoke(s) });
  s = cli('next');
  s = cli('result', { receipt: invoke(s) });
  assert.equal(s.status, 'completed');
  assert.equal(userSink.length, 2);
  assert.equal((userSink[1] as { action: string }).action, 'confirmed');
  assert.equal(actions, 1);
  assert.equal(reads, 1);
  assert.equal(
    cli('cancel', { reason: 'release finished watch' }).status,
    'completed',
  );
  now += 1_000_000;
  assert.equal(cli('next').pending, undefined);
  assert.equal(cli('status').history.length, 4);
  assert.throws(
    () => cli('result', { receipt: { requestId: 'late' } }),
    /ended/,
  );
  // Cancellation of an active watch (including its in-flight tool request) is irreversible.
  const other = path.join(dir, 'cancel.json');
  const commands = (command: string, input = {}) =>
    monitorCli([command, '--file', other], input, { ...io, now: () => epoch });
  commands('start', { config: config() });
  s = commands('next');
  const late = receipt(s, { observation: observation() });
  commands('cancel', { reason: 'user cancelled' });
  assert.equal(commands('next').pending, undefined);
  assert.throws(() => commands('result', { receipt: late }), /ended/);
});

test('all predicate: partial, missing, uncreated children, stale and failed reads never become success', () => {
  const cases: [Observation, string][] = [
    [observation(true, false), 'pending'],
    [observation(), 'satisfied'],
    [observation(null), 'read-error'],
    [{ samples: [] }, 'read-error'],
    [{ samples: [], error: 'read denied' }, 'read-error'],
    [observation(true, true, epoch - 120_000), 'read-error'],
    [observation(true, true, epoch + 1), 'read-error'],
    [
      {
        samples: [
          {
            ...observation().samples[0],
            expectedTargets: ['not-created-child'],
          },
        ],
      },
      'read-error',
    ],
    [
      { samples: [...observation().samples, ...observation().samples] },
      'read-error',
    ],
    [{ samples: [{ ...observation().samples[0], values: {} }] }, 'read-error'],
    [
      {
        samples: [{ ...observation().samples[0], expectedTargets: [null] }],
      } as Observation,
      'read-error',
    ],
    [{ samples: [null] }, 'read-error'],
    [{} as Observation, 'read-error'],
  ];
  for (const [o, expected] of cases) {
    const s = observed(config(), o);
    assert.equal(s.evaluation, expected);
    nextMonitor(s, epoch);
    assert.equal(
      s.pending?.kind,
      expected === 'pending' ? undefined : 'report',
    );
  }
  const multi = config({
    targets: [...config().targets, { id: 'child', conditions: ['done'] }],
  });
  assert.equal(evaluate(multi, observation(), epoch), 'read-error');
  const o = {
    samples: [
      ...observation().samples,
      { target: 'child', observedAt: epoch, values: { done: false } },
    ],
  };
  assert.equal(evaluate(multi, o, epoch), 'pending');
  o.samples[1].values.done = true;
  assert.equal(evaluate(multi, o, epoch), 'satisfied');
  const invalidHistory = [
    { samples: null },
    { samples: [null, { expectedTargets: [null] }] },
  ] as Observation[];
  assert.equal(
    evaluate(config(), observation(), epoch, invalidHistory),
    'satisfied',
  );
  const knownChild = {
    samples: [
      { ...observation().samples[0], expectedTargets: ['known-child'] },
    ],
  };
  assert.equal(
    evaluate(config(), observation(), epoch, [...invalidHistory, knownChild]),
    'read-error',
  );
});

test('unchanged periodic observations report at default or explicit cadence; conditional pending stays quiet', () => {
  const defaults = config();
  delete defaults.observeMs;
  delete defaults.reportMs;
  assert.equal(monitorConfig(defaults).observeMs, 120_000);
  assert.equal(monitorConfig(defaults).reportMs, 600_000);
  const c = config({ mode: 'periodic', observeMs: 1000, reportMs: 500 });
  const s = observed(c, observation(false, false));
  assert.equal(nextMonitor(s, epoch + 499).pending, undefined);
  assert.equal(nextMonitor(s, epoch + 500).pending!.kind, 'report');
  receiveMonitor(s, receipt(s, { outcome: 'accepted' }), epoch + 500);
  assert.equal(s.notification, 'accepted');
  nextMonitor(s, epoch + 1000);
  receiveMonitor(
    s,
    receipt(s, { observation: observation(false, false, epoch + 1000) }),
    epoch + 1000,
  );
  assert.equal(
    nextMonitor(s, epoch + 1000).pending!.kind,
    'report',
    'no change still reports',
  );
  const conditional = observed(config(), observation(false));
  assert.equal(nextMonitor(conditional, epoch + 1).pending, undefined);
});

test('periodic CLI rechecks preserve refresh requests and report only at deadlines across two unchanged periods', (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'monitor-periodic-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const file = path.join(dir, 'session.json');
  let now = epoch;
  let ids = 0;
  const cli = (command: string, input = {}) =>
    monitorCli([command, '--file', file], input, {
      now: () => now,
      id: () => `periodic-${++ids}`,
    });
  const c = config({
    mode: 'periodic',
    end: { description: 'until cancelled', onSatisfied: false },
  });
  let s = cli('start', { config: c });
  let reports = 0;
  for (let elapsed = 0; elapsed <= 1_200_000; elapsed += c.observeMs) {
    now = epoch + elapsed;
    s = cli('next');
    assert.equal(s.pending!.kind, 'observe');
    const refresh = s.pending;
    s = cli('next');
    assert.deepEqual(
      s.pending,
      refresh,
      'execution-time recheck preserves the saved observation request',
    );
    s = cli('result', {
      receipt: receipt(s, { observation: observation(true, true, now) }),
    });
    assert.equal(s.evaluation, 'satisfied');
    s = cli('next');
    if (elapsed > 0 && elapsed % c.reportMs === 0) {
      assert.equal(s.pending!.kind, 'report');
      assert.equal(
        (s.pending!.payload as { evaluation: string }).evaluation,
        'satisfied',
      );
      s = cli('result', { receipt: receipt(s) });
      reports++;
    } else
      assert.equal(
        s.pending,
        undefined,
        `no report before deadline at ${elapsed}`,
      );
    now += c.observeMs - 1;
    assert.equal(cli('next').pending, undefined);
  }
  assert.equal(reports, 2);
  assert.equal(s.status, 'active');
  assert.equal(
    cli('status').history.filter((h) => h.request.kind === 'report').length,
    2,
  );
  const abnormal = observed(c, { samples: [], error: 'read failed' });
  assert.equal(
    nextMonitor(abnormal, epoch).pending!.kind,
    'report',
    'observation errors still report immediately',
  );
});

test('CLI retains discovered children after anomaly delivery and later manifest omission', (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'monitor-children-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const file = path.join(dir, 'session.json');
  let now = epoch;
  let ids = 0;
  const cli = (command: string, input = {}) =>
    monitorCli([command, '--file', file], input, {
      now: () => now,
      id: () => `children-${++ids}`,
    });
  cli('start', {
    config: config({
      action: {
        description: 'authorized follow-up',
        authorization: 'prior user request',
        capability: capability('operation'),
      },
    }),
  });
  const discovered = observation();
  discovered.samples[0].expectedTargets = ['child-not-created'];
  const later = observation(true, true, epoch + 120_000);
  const childAppears = observation(true, true, epoch + 240_000);
  childAppears.samples.push({
    target: 'child-not-created',
    observedAt: epoch + 240_000,
    values: { done: true },
  });
  for (const o of [discovered, later, childAppears]) {
    let s = cli('next');
    assert.equal(s.pending!.kind, 'observe');
    s = cli('result', { receipt: receipt(s, { observation: o }) });
    assert.equal(s.evaluation, 'read-error');
    s = cli('next');
    assert.equal(s.pending!.kind, 'report');
    assert.equal(
      (s.pending!.payload as { evaluation: string }).evaluation,
      'read-error',
    );
    s = cli('result', { receipt: receipt(s) });
    assert.equal(s.status, 'active');
    assert.equal(s.action, 'unstarted');
    assert.equal(s.pending, undefined);
    now += s.config.observeMs;
  }
  const saved = cli('status');
  assert.deepEqual(
    saved.history[0].receipt.observation!.samples[0].expectedTargets,
    ['child-not-created'],
  );
  assert.ok(saved.history.every((h) => h.request.kind !== 'action'));
  // next and report-result must also honor saved discoveries when rechecking an existing handoff.
  for (const kind of ['report', 'action'] as const) {
    const recheck = structuredClone(saved);
    recheck.evaluation = 'satisfied';
    recheck.pending = {
      ...saved.history.at(-1)!.request,
      id: `recheck-${kind}`,
      kind,
    };
    if (kind === 'report')
      receiveMonitor(recheck, receipt(recheck), epoch + 240_000);
    else nextMonitor(recheck, epoch + 240_000);
    assert.equal(recheck.evaluation, 'read-error');
    assert.equal(recheck.status, 'active');
    assert.notEqual(recheck.pending?.kind, 'action');
  }
  assert.equal(
    cli('cancel', { reason: 'switch to a new explicit child contract' }).status,
    'cancelled',
  );
  const explicit = config({
    targets: [
      ...config().targets,
      { id: 'child-not-created', conditions: ['done'] },
    ],
  });
  assert.equal(
    observed(explicit, childAppears, epoch + 240_000).evaluation,
    'satisfied',
  );
});

test('notification failure is reported and retried; acceptance and confirmation remain distinct', () => {
  const s = nextMonitor(observed(), epoch);
  receiveMonitor(s, receipt(s, { outcome: 'failed' }), epoch);
  assert.equal(s.notification, 'failed');
  assert.equal(s.status, 'active');
  nextMonitor(s, epoch);
  assert.equal(
    (s.pending!.payload as { notificationProblem: boolean })
      .notificationProblem,
    true,
  );
  receiveMonitor(s, receipt(s, { outcome: 'confirmed' }), epoch);
  assert.equal(s.notification, 'confirmed');
  assert.equal(s.status, 'completed');
  const accepted = nextMonitor(observed(), epoch);
  receiveMonitor(accepted, receipt(accepted, { outcome: 'accepted' }), epoch);
  assert.equal(accepted.notification, 'accepted');
  assert.equal(accepted.history[1].receipt.outcome, 'accepted');
});

test('preflight rejects unavailable/unchecked routes, internal messages, stdout and failed registrations', () => {
  for (const kind of ['stdout', 'file', 'attention', 'internal-message']) {
    const s = startMonitor(
      config({ reporter: capability(kind) }),
      'run',
      epoch,
    );
    assert.equal(s.status, 'unavailable');
    assert.match(s.limitations.join(), /no user-facing delivery/);
    assert.equal(nextMonitor(s, epoch).pending, undefined);
    assert.equal(cancelMonitor(s, 'cannot continue').status, 'cancelled');
  }
  for (const reporter of [
    { ...capability(), available: false },
    { ...capability(), specification: '' },
    { ...capability(), checkedAt: epoch - 600_001 },
    { ...capability(), checkedAt: epoch + 1 },
    undefined,
  ]) {
    assert.equal(
      startMonitor(config({ reporter }), 'run', epoch).status,
      'unavailable',
    );
  }
  for (const registration of [
    {
      id: 'external',
      tool: 'verified-tool',
      evidence: 'registration refused',
      accepted: false,
    },
    { id: 'run', tool: 'verified-tool', evidence: 'self id', accepted: true },
    { id: 'external', tool: '', evidence: '', accepted: true },
  ]) {
    const s = startMonitor(
      config({
        continuity: {
          scope: 'session',
          constraint: 'no continued wakeup',
          registration,
        },
      }),
      'run',
      epoch,
    );
    assert.equal(s.status, 'unavailable');
    assert.match(s.limitations.join(), /registration/);
  }
  const s = startMonitor(
    config({
      continuity: {
        scope: 'session',
        constraint: 'session only',
        registration: {
          id: 'external',
          tool: 'registered-tool',
          evidence: 'tool registration receipt 99',
          accepted: true,
        },
      },
    }),
    'run',
    epoch,
  );
  assert.equal(s.status, 'active');
  assert.notEqual(s.runId, s.config.continuity.registration!.id);
  assert.equal(s.config.continuity.scope, 'session');
});

test('authorized follow-up failures are reported; stale conditions never dispatch follow-up', () => {
  const c = config({
    action: {
      description: 'previously requested operation',
      authorization: 'user quote ref',
      capability: capability('operation'),
    },
  });
  const s = nextMonitor(observed(c), epoch);
  receiveMonitor(s, receipt(s), epoch);
  assert.equal(s.pending!.kind, 'action');
  assert.throws(
    () => receiveMonitor(s, receipt(s, { outcome: 'accepted' }), epoch),
    /Unmatched/,
  );
  receiveMonitor(s, receipt(s, { outcome: 'failed' }), epoch);
  nextMonitor(s, epoch);
  assert.equal((s.pending!.payload as { action: string }).action, 'failed');
  receiveMonitor(s, receipt(s), epoch);
  assert.equal(s.status, 'completed');
  const stale = nextMonitor(observed(c), epoch);
  receiveMonitor(stale, receipt(stale), epoch + c.observeMs);
  assert.equal(stale.pending, undefined);
  assert.equal(stale.evaluation, 'read-error');
  assert.equal(nextMonitor(stale, epoch + c.observeMs).pending!.kind, 'report');
  const late = observed(c);
  assert.equal(nextMonitor(late, epoch + c.observeMs).evaluation, 'read-error');
  const unexecuted = nextMonitor(observed(c), epoch);
  receiveMonitor(unexecuted, receipt(unexecuted), epoch);
  const actionId = unexecuted.pending!.id;
  nextMonitor(unexecuted, epoch + c.observeMs);
  assert.equal(
    unexecuted.pending!.kind,
    'report',
    'stale pending action is revoked before execution',
  );
  assert.notEqual(unexecuted.pending!.id, actionId);
  const noAction = nextMonitor(observed(), epoch);
  receiveMonitor(noAction, receipt(noAction), epoch + c.observeMs);
  assert.equal(
    noAction.status,
    'active',
    'stale satisfaction cannot finish even without follow-up',
  );
  assert.equal(noAction.evaluation, 'read-error');
  const failed = nextMonitor(startMonitor(config(), 'run', epoch), epoch);
  receiveMonitor(failed, receipt(failed, { outcome: 'failed' }), epoch);
  assert.equal(nextMonitor(failed, epoch).pending!.kind, 'report');
  const missing = nextMonitor(startMonitor(config(), 'run', epoch), epoch);
  receiveMonitor(missing, receipt(missing), epoch);
  assert.equal(missing.evaluation, 'read-error');
  for (const pending of [
    nextMonitor(observed(), epoch),
    nextMonitor(observed(c), epoch),
  ]) {
    if (pending.config.action) receiveMonitor(pending, receipt(pending), epoch);
    const lateReceipt = receipt(pending);
    cancelMonitor(pending, 'cancel report or action');
    assert.equal(nextMonitor(pending, epoch).pending, undefined);
    assert.throws(() => receiveMonitor(pending, lateReceipt, epoch), /ended/);
  }
});

test('end deadline, receipt correlation, duplicate calls and validation fail closed', () => {
  const c = config({
    end: { description: 'deadline', onSatisfied: false, at: epoch + 10 },
  });
  const s = nextMonitor(startMonitor(c, 'run', epoch), epoch);
  const r = receipt(s, { observation: observation() });
  for (const change of [
    { requestId: 'wrong' },
    { tool: 'other' },
    { destination: 'other' },
    { callId: '' },
    { evidence: '' },
    { outcome: 'unknown' },
  ])
    assert.throws(
      () => receiveMonitor(s, { ...r, ...change } as Receipt, epoch),
      /Unmatched/,
    );
  receiveMonitor(s, r, epoch);
  nextMonitor(s, epoch);
  assert.throws(
    () => receiveMonitor(s, receipt(s, { callId: r.callId }), epoch),
    /Unmatched/,
  );
  receiveMonitor(s, receipt(s), epoch + 10);
  assert.equal(s.status, 'completed');
  assert.equal(s.pending, undefined);
  assert.throws(() => cancelMonitor(s, ''), /reason/);
  for (const change of [
    { targets: [] },
    { predicate: 'any' },
    { mode: 'other' },
    { observeMs: 0 },
    { reportMs: -1 },
    { targets: [{ id: 'x', conditions: [] }] },
    { targets: [...c.targets, ...c.targets] },
    { continuity: { scope: 'background', constraint: 'unverified' } },
    { action: { description: 'execute' } },
    { end: { description: '', onSatisfied: true } },
  ])
    assert.throws(() => monitorConfig({ ...c, ...change } as MonitorConfig));
});

test('CLI rejects invalid arguments, overlapping writes and corrupt versions without replacing record', (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'monitor-cli-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const file = path.join(dir, 'state.json');
  const io = { now: () => epoch, id: () => 'test-id' };
  assert.throws(() => monitorCli([], {}, io), /Usage/);
  assert.throws(
    () => monitorCli(['status', '--file', 'relative.json'], {}, io),
    /absolute/,
  );
  monitorCli(['start', '--file', file], { config: config() }, io);
  const original = fs.readFileSync(file, 'utf8');
  fs.mkdirSync(`${file}.lock`);
  assert.throws(
    () => monitorCli(['cancel', '--file', file], { reason: 'race' }, io),
    /EEXIST/,
  );
  assert.equal(fs.readFileSync(file, 'utf8'), original);
  fs.rmdirSync(`${file}.lock`);
  fs.writeFileSync(file, '{"version":2}');
  assert.throws(
    () => monitorCli(['status', '--file', file], {}, io),
    /Unsupported/,
  );
  assert.equal(fs.readFileSync(file, 'utf8'), '{"version":2}');
  const child = spawnSync(
    process.execPath,
    ['scripts/monitor-watch.ts', 'invalid'],
    { encoding: 'utf8', env: childEnv },
  );
  assert.equal(child.status, 2);
  assert.match(child.stderr, /request rejected/);
  assert.equal(child.stdout, '');
});

test('CLI status enforces deadline; failed persistence exposes no new handoff or completion', (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'monitor-save-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const file = path.join(dir, 'state.json');
  let now = epoch;
  const io = { now: () => now, id: () => 'save-id' };
  monitorCli(
    ['start', '--file', file],
    {
      config: config({
        end: { description: 'deadline', onSatisfied: true, at: epoch + 10 },
      }),
    },
    io,
  );
  const original = fs.readFileSync(file, 'utf8');
  const write = t.mock.method(fs, 'writeFileSync', () => {
    throw Error('simulated disk full');
  });
  assert.throws(
    () => monitorCli(['next', '--file', file], {}, io),
    /disk full/,
  );
  assert.equal(fs.readFileSync(file, 'utf8'), original);
  write.mock.restore();
  assert.equal(
    monitorCli(['next', '--file', file], {}, io).pending!.kind,
    'observe',
  );
  now += 10;
  assert.equal(
    monitorCli(['status', '--file', file], {}, io).status,
    'completed',
  );
  assert.equal(JSON.parse(fs.readFileSync(file, 'utf8')).pending, undefined);
});

test('blocked observations require handling or reporting with cadence and preserve receipt correlation', () => {
  const blocked = (
    actor: 'ai' | 'user' | 'system',
    at = epoch,
  ): Observation => ({
    samples: [
      {
        ...observation(false, false, at).samples[0],
        blocked: { actor, reason: '停止理由', action: '記録を確認' },
      },
    ],
  });
  const c = config({ handler: capability('operation') });
  const s = nextMonitor(observed(c, blocked('ai')), epoch);
  assert.equal(s.pending!.kind, 'handle');
  assert.equal(s.pending!.tool, c.handler!.tool);
  assert.match(
    JSON.stringify(s.pending!.payload),
    /停止理由.*記録を確認.*tool response/,
  );
  const pending = s.pending;
  assert.deepEqual(nextMonitor(s, epoch + c.observeMs).pending, pending);
  assert.throws(
    () => receiveMonitor(s, receipt(s, { outcome: 'accepted' }), epoch),
    /Unmatched/,
  );
  assert.throws(
    () => receiveMonitor(s, receipt(s, { destination: 'wrong' }), epoch),
    /Unmatched/,
  );
  receiveMonitor(s, receipt(s), epoch);
  assert.equal(nextMonitor(s, epoch + c.observeMs).pending!.kind, 'observe');
  receiveMonitor(
    s,
    receipt(s, { observation: blocked('ai', epoch + c.observeMs) }),
    epoch + c.observeMs,
  );
  assert.equal(nextMonitor(s, epoch + c.observeMs).pending, undefined);
  nextMonitor(s, epoch + c.reportMs);
  receiveMonitor(
    s,
    receipt(s, { observation: blocked('ai', epoch + c.reportMs) }),
    epoch + c.reportMs,
  );
  assert.equal(nextMonitor(s, epoch + c.reportMs).pending!.kind, 'handle');
  receiveMonitor(
    s,
    receipt(s, { outcome: 'failed', evidence: '対処失敗の根拠' }),
    epoch + c.reportMs,
  );
  assert.match(
    (nextMonitor(s, epoch + c.reportMs).pending!.payload as { message: string })
      .message,
    /対処に失敗.*対処失敗の根拠/,
  );
  for (const actor of ['user', 'ai'] as const) {
    const report = nextMonitor(observed(config(), blocked(actor)), epoch);
    assert.equal(report.pending!.kind, 'report');
    assert.match(
      (report.pending!.payload as { message: string }).message,
      /停止理由.*記録を確認/,
    );
    receiveMonitor(report, receipt(report), epoch);
    nextMonitor(report, epoch + 120_000);
    receiveMonitor(
      report,
      receipt(report, { observation: blocked(actor, epoch + 120_000) }),
      epoch + 120_000,
    );
    assert.equal(nextMonitor(report, epoch + 120_000).pending, undefined);
    nextMonitor(report, epoch + 600_000);
    receiveMonitor(
      report,
      receipt(report, { observation: blocked(actor, epoch + 600_000) }),
      epoch + 600_000,
    );
    assert.equal(nextMonitor(report, epoch + 600_000).pending!.kind, 'report');
  }
  const system = observed(c, blocked('system'));
  assert.equal(nextMonitor(system, epoch).pending, undefined);
  assert.equal(
    nextMonitor(system, epoch + c.observeMs).pending!.kind,
    'observe',
  );
  assert.equal(observed(config(), observation(false)).evaluation, 'pending');
  assert.equal(
    startMonitor(
      config({ handler: { ...capability(), available: false } }),
      'bad',
      epoch,
    ).status,
    'unavailable',
  );
  for (const invalid of [
    null,
    {},
    { actor: 'worker', reason: 'x', action: 'y' },
    { actor: 'ai', reason: '', action: 'y' },
  ]) {
    const o = observation(false);
    (o.samples[0] as { blocked?: unknown }).blocked = invalid;
    assert.equal(evaluate(c, o, epoch), 'read-error');
  }
});

test('a resolved blockage can recur before the notification interval', () => {
  const c = config({ handler: capability('operation') });
  const stopped = (at: number): Observation => ({
    samples: [
      {
        ...observation(false, false, at).samples[0],
        blocked: { actor: 'ai', reason: '停止', action: '調査' },
      },
    ],
  });
  const s = nextMonitor(observed(c, stopped(epoch)), epoch);
  assert.equal(s.pending!.kind, 'handle');
  receiveMonitor(s, receipt(s), epoch);
  nextMonitor(s, epoch + c.observeMs);
  receiveMonitor(
    s,
    receipt(s, { observation: observation(false, false, epoch + c.observeMs) }),
    epoch + c.observeMs,
  );
  assert.deepEqual(s.blockedNotified, {});
  nextMonitor(s, epoch + 2 * c.observeMs);
  receiveMonitor(
    s,
    receipt(s, { observation: stopped(epoch + 2 * c.observeMs) }),
    epoch + 2 * c.observeMs,
  );
  assert.equal(nextMonitor(s, epoch + 2 * c.observeMs).pending!.kind, 'handle');
});

test('CLI persists multiple blocked handoffs and reloads version 1 records', (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'monitor-blocked-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const file = path.join(dir, 'state.json');
  let ids = 0;
  const cli = (command: string, input = {}) =>
    monitorCli([command, '--file', file], input, {
      now: () => epoch,
      id: () => `blocked-${++ids}`,
    });
  const c = config({
    handler: capability('operation'),
    targets: [
      { id: 'parent', conditions: ['merged'] },
      { id: 'child', conditions: ['done'] },
    ],
  });
  cli('start', { config: c });
  let s = cli('next');
  s = cli('result', {
    receipt: receipt(s, {
      observation: {
        samples: [
          {
            target: 'parent',
            observedAt: epoch,
            values: { merged: false },
            blocked: {
              actor: 'ai',
              reason: 'parent stopped',
              action: 'inspect parent',
            },
          },
          {
            target: 'child',
            observedAt: epoch,
            values: { done: false },
            blocked: {
              actor: 'user',
              reason: 'child stopped',
              action: 'ask user',
            },
          },
        ],
      },
    }),
  });
  assert.equal(s.version, 1);
  s = cli('next');
  assert.equal(s.pending!.kind, 'handle');
  assert.deepEqual(cli('next').pending, s.pending);
  cli('result', { receipt: receipt(s) });
  s = cli('next');
  assert.equal(s.pending!.kind, 'report');
  assert.match(
    (s.pending!.payload as { message: string }).message,
    /child stopped.*ask user/,
  );
  cli('result', { receipt: receipt(s) });
  assert.equal(cli('status').history.length, 3);
});
