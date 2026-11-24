import {
  capabilityProblems,
  evaluate,
  monitorConfig,
} from './monitor-contract.ts';
import type {
  MonitorConfig,
  MonitorState,
  Receipt,
  Request,
} from './monitor-contract.ts';

export function startMonitor(
  input: MonitorConfig,
  runId: string,
  now: number,
): MonitorState {
  const config = monitorConfig(input);
  const limitations = [
    ...capabilityProblems(config.observer, now),
    ...capabilityProblems(config.reporter, now, true),
  ];
  if (config.action)
    limitations.push(...capabilityProblems(config.action.capability, now));
  if (config.handler)
    limitations.push(...capabilityProblems(config.handler, now));
  const registration = config.continuity.registration;
  if (
    registration &&
    (!registration.accepted ||
      !registration.id ||
      !registration.tool ||
      !registration.evidence ||
      registration.id === runId)
  )
    limitations.push(
      'External registration failed or lacks independent registration evidence; runId is not a wakeup registration',
    );
  return {
    version: 1,
    runId,
    config,
    status: limitations.length ? 'unavailable' : 'active',
    limitations,
    sequence: 0,
    nextCheckAt: now,
    nextReportAt: now + config.reportMs,
    evaluation: 'unobserved',
    reportDue: false,
    notification: 'unsent',
    action: 'unstarted',
    history: [],
  };
}

function request(
  s: MonitorState,
  kind: Request['kind'],
  now: number,
  payload: unknown,
) {
  const c =
    kind === 'observe'
      ? s.config.observer
      : kind === 'report'
        ? s.config.reporter
        : kind === 'handle'
          ? s.config.handler!
          : s.config.action!.capability;
  s.pending = {
    id: `${s.runId}:${++s.sequence}`,
    kind,
    tool: c.tool,
    destination: c.destination,
    issuedAt: now,
    payload,
  };
  if (kind === 'report') s.notification = 'unsent';
}

function evaluateMonitor(s: MonitorState, now: number) {
  const previous = s.history
    .filter((h) => h.request.kind === 'observe' && h.receipt.observation)
    .map((h) => h.receipt.observation!);
  return evaluate(s.config, s.observation!, now, previous);
}

// next/result are a tool handoff, not an arbitrary command or URL executor.
export function nextMonitor(s: MonitorState, now: number): MonitorState {
  if (s.status !== 'active') return s;
  if (s.config.end.at !== undefined && now >= s.config.end.at) {
    s.status = 'completed';
    s.stoppedReason = 'deadline';
    delete s.pending;
    return s;
  }
  if (!s.pending && s.blockedQueue?.length) {
    const item = s.blockedQueue.shift()!;
    request(s, 'handle', now, item);
    return s;
  }
  if (!s.pending && !s.reportDue && now >= s.nextCheckAt) {
    request(s, 'observe', now, {
      targets: s.config.targets,
      predicate: s.config.predicate,
    });
    return s;
  }
  if (
    s.pending?.kind !== 'observe' &&
    s.observation &&
    s.evaluation === 'satisfied' &&
    evaluateMonitor(s, now) !== 'satisfied'
  ) {
    s.evaluation = 'read-error';
    s.reportDue = true;
    // Recheck satisfied reports/actions, but keep observations that refresh the expired evidence.
    delete s.pending;
  }
  if (s.pending) return s;
  if (
    s.reportDue ||
    (s.config.mode === 'periodic' && now >= s.nextReportAt && s.observation)
  ) {
    const evaluation = evaluateMonitor(s, now);
    s.evaluation = evaluation;
    request(s, 'report', now, {
      evaluation,
      observation: s.observation,
      action: s.action,
      notificationProblem: s.notification === 'failed',
      continuation: s.config.continuity,
      message: `監視対象 ${s.config.targets.map((t) => t.id).join('、')}: ${
        {
          satisfied: '全条件が成立しました',
          pending: '条件はまだ成立していません',
          'read-error': '観測に異常があり成立を確認できません',
          unobserved: '未観測です',
        }[evaluation]
      }。後続操作: ${{ unstarted: '未開始', accepted: '開始受理', confirmed: '結果確認済み', failed: '失敗' }[s.action]}。${(s.blockedReports ?? []).join(' ')}${s.notification === 'failed' ? '前回の通知に失敗しています。' : ''}`,
    });
  }
  return s;
}

export function receiveMonitor(
  s: MonitorState,
  receipt: Receipt,
  now: number,
): MonitorState {
  if (s.status !== 'active')
    throw Error(
      'Monitor ended; no new observation/report/action results accepted',
    );
  const p = s.pending;
  if (
    !p ||
    receipt?.requestId !== p.id ||
    receipt.tool !== p.tool ||
    receipt.destination !== p.destination ||
    !receipt.callId?.trim() ||
    !receipt.evidence?.trim() ||
    !['failed', 'accepted', 'confirmed'].includes(receipt.outcome) ||
    (['action', 'handle'].includes(p.kind) && receipt.outcome === 'accepted') ||
    s.history.some((h) => h.receipt.callId === receipt.callId)
  )
    throw Error('Unmatched or unproven tool receipt');
  // At the deadline even an in-flight result cannot initiate a new operation.
  if (s.config.end.at !== undefined && now >= s.config.end.at)
    return nextMonitor(s, now);
  s.history.push({ request: p, receipt: structuredClone(receipt) });
  delete s.pending;
  if (p.kind === 'observe') {
    s.observation =
      receipt.outcome === 'failed'
        ? {
            samples: [],
            error: 'Observation tool failed; inspect call evidence',
          }
        : (receipt.observation ?? {
            samples: [],
            error: 'Missing observation',
          });
    s.evaluation = evaluateMonitor(s, now);
    s.nextCheckAt = now + s.config.observeMs;
    s.reportDue =
      s.evaluation === 'read-error' ||
      (s.config.mode === 'conditional' && s.evaluation === 'satisfied') ||
      (s.config.mode === 'periodic' && now >= s.nextReportAt);
    if (s.evaluation !== 'read-error') {
      s.blockedQueue = [];
      s.blockedNotified ??= {};
      const currentKeys = new Set(
        s.observation.samples
          .filter(
            (sample) =>
              s.config.targets.some((target) => target.id === sample.target) &&
              sample.blocked &&
              sample.blocked.actor !== 'system',
          )
          .map((sample) =>
            JSON.stringify([
              sample.target,
              sample.blocked!.actor,
              sample.blocked!.reason,
              sample.blocked!.action,
            ]),
          ),
      );
      for (const key of Object.keys(s.blockedNotified)) {
        if (!currentKeys.has(key)) delete s.blockedNotified[key];
      }
      for (const sample of s.observation.samples) {
        if (!s.config.targets.some((target) => target.id === sample.target))
          continue;
        const blocked = sample.blocked;
        if (!blocked || blocked.actor === 'system') continue;
        const key = JSON.stringify([
          sample.target,
          blocked.actor,
          blocked.reason,
          blocked.action,
        ]);
        if (
          s.blockedNotified[key] !== undefined &&
          now - s.blockedNotified[key] < s.config.reportMs
        )
          continue;
        s.blockedNotified[key] = now;
        if (blocked.actor === 'ai' && s.config.handler) {
          s.blockedQueue.push({
            target: sample.target,
            blocked,
            evidence: receipt.evidence,
            key,
          });
        } else {
          (s.blockedReports ??= []).push(
            `対象 ${sample.target}: ${blocked.reason}。対処: ${blocked.action}。${blocked.actor === 'ai' ? '対処の道具がありません。' : ''}`,
          );
          s.reportDue = true;
        }
      }
    }
  } else if (p.kind === 'handle') {
    if (receipt.outcome === 'failed') {
      const item = p.payload as {
        target: string;
        blocked: { reason: string; action: string };
      };
      (s.blockedReports ??= []).push(
        `対象 ${item.target}: ${item.blocked.reason}。対処: ${item.blocked.action}。対処に失敗しました。根拠: ${receipt.evidence}`,
      );
      s.reportDue = true;
    }
  } else if (p.kind === 'action') {
    s.action = receipt.outcome;
    s.reportDue = true;
  } else {
    s.notification = receipt.outcome;
    s.reportDue = receipt.outcome === 'failed';
    if (s.reportDue) return s;
    s.blockedReports = [];
    s.nextReportAt = now + s.config.reportMs;
    if (
      s.action === 'unstarted' &&
      s.evaluation === 'satisfied' &&
      evaluateMonitor(s, now) !== 'satisfied'
    ) {
      s.evaluation = 'read-error';
      s.reportDue = true;
      return s;
    }
    if (
      s.action !== 'unstarted' ||
      (s.evaluation === 'satisfied' &&
        s.config.end.onSatisfied &&
        !s.config.action)
    ) {
      s.status = 'completed';
      s.stoppedReason = 'reported result';
    } else if (s.evaluation === 'satisfied' && s.config.action) {
      request(s, 'action', now, s.config.action);
    }
  }
  return s;
}

export function cancelMonitor(s: MonitorState, reason: string): MonitorState {
  if (!reason?.trim()) throw Error('Cancellation reason required');
  if (s.status === 'active' || s.status === 'unavailable') {
    s.status = 'cancelled';
    s.stoppedReason = reason;
    delete s.pending;
  }
  return s;
}
