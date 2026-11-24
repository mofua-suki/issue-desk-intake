import crypto from 'node:crypto';
import { execFile, spawn } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { promisify } from 'node:util';
import { pathToFileURL } from 'node:url';
import {
  classifyWatchTargets,
  type WatchTarget,
} from './codex-issue-watch-classification.ts';
import { classifyWatchOutcome } from './codex-issue-watch-outcome.ts';
import { deliver } from './codex-issue-watch-deliver.ts';
import { recordWatchResponse } from './codex-issue-watch-record.ts';
import {
  dispatchWatchLocal,
  withWatchLock,
} from './codex-issue-watch-local-dispatch.ts';
import {
  resumeWatchState,
  saveWatchState,
  type WatchState,
} from './codex-issue-watch-state.ts';

const uuid = /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/;
const projectName = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;
const object = (value: any) =>
  value !== null && typeof value === 'object' && !Array.isArray(value);
const positive = (value: any) => Number.isSafeInteger(value) && value > 0;
type Receipt = {
  sendId: string;
  threadId: string;
  host: string;
  receiptState: 'unreconciled';
};
type IssueRef = { project: string; number: number };
type Details = {
  target: WatchTarget;
  detail: any;
  trail: string[];
  issue?: IssueRef;
};
type WorkResponse = Parameters<typeof classifyWatchTargets>[1];
type Notice = {
  kind: 'ops-watch';
  id: string;
  text: string;
  reason: string;
  issues: { repo: string; number: number }[];
};
type Checkpoint = {
  event: string;
  id: string;
  startedAt: number;
  deliveredAt?: number;
  terminal?: 'merged' | 'withdrawn';
};
type ResponseEvidence = {
  action: 'read' | 'operation' | 'waiting';
  reference?: string;
};
type Runtime = {
  host: string;
  deadline?: number;
  now: () => number;
  begin?: (receipt: Receipt) => void;
  rotation?: (receipt: Receipt, draftId: string) => number;
  processed?: () => void;
  records: Receipt[];
  get: (route: string) => Promise<any>;
  load: (key: string) => Checkpoint | undefined;
  save: (key: string, state: Checkpoint) => void;
  response: (id: string) => ResponseEvidence | undefined;
  verified?: (identity: Identity) => WatchState['response'] | undefined;
  saveResponse: (
    identity: Identity,
    evidence: WatchState['response'],
  ) => Promise<void>;
  dispatch: typeof dispatchWatchLocal;
  deliver: Parameters<typeof dispatchWatchLocal>[3]['deliver'];
  notice: (notice: Notice) => Promise<void>;
  stateDirectory: string;
};
type Identity = Parameters<typeof dispatchWatchLocal>[1];

const draftRoute = (id: string, summary = true) =>
  '/api/drafts/' + encodeURIComponent(id) + (summary ? '?view=summary' : '');
const issueRoute = (issue: IssueRef) =>
  '/api/issue?' +
  new URLSearchParams({
    project: issue.project,
    number: String(issue.number),
    verifyMerge: 'true',
  });
const workRoute = '/api/list?view=work';

function validReceipt(record: any, host: string): record is Receipt {
  return (
    object(record) &&
    uuid.test(record.sendId) &&
    uuid.test(record.threadId) &&
    record.host === host &&
    record.receiptState === 'unreconciled'
  );
}

function issueRef(value: any, project: string): IssueRef {
  const ref = { project: value?.project ?? project, number: value?.number };
  if (!projectName.test(ref.project) || !positive(ref.number))
    throw Error('invalid issue link');
  return ref;
}

function rotated<T>(list: T[], offset: number): T[] {
  if (!list.length) return list;
  const start = offset % list.length;
  return [...list.slice(start), ...list.slice(0, start)];
}

async function readTargets(
  receipt: Receipt,
  get: Runtime['get'],
  options: {
    rotation?: (draftId: string) => number;
    selected?: Details;
    readIssue?: Runtime['get'];
  } = {},
) {
  const root = await get(draftRoute(receipt.sendId, false));
  if (
    root?.accepted !== true ||
    root.requestId !== receipt.sendId ||
    !uuid.test(root.draftId ?? '') ||
    root.id !== root.draftId ||
    !projectName.test(root.project ?? '') ||
    root.consultationProvider === 'claude'
  )
    return { items: [], unconfirmed: 1 };
  let unconfirmed = 0;
  const drafts = new Map<string, { detail: any; trail: string[] }>();
  const issues = new Map<string, { issue: IssueRef; trail: string[] }>();
  let rootOffset = 0;
  const visit = async (draft: any, trail: string[]) => {
    if (drafts.has(draft.id) || drafts.size >= 50)
      throw Error('ambiguous draft graph');
    drafts.set(draft.id, { detail: draft, trail });
    for (const raw of [
      ...(draft.issues ?? []),
      ...(draft.issue ? [draft.issue] : []),
    ]) {
      const ref = issueRef(raw, draft.project);
      if (
        options.selected &&
        (options.selected.issue?.project !== ref.project ||
          options.selected.issue?.number !== ref.number)
      )
        continue;
      const key = ref.project + '#' + ref.number;
      if (!issues.has(key)) issues.set(key, { issue: ref, trail });
    }
    const offset = options.rotation?.(draft.id) ?? 0;
    if (draft.id === root.id) rootOffset = offset;
    const children = rotated<string>(draft.splitChildren ?? [], offset);
    for (const id of children) {
      if (options.selected && options.selected.trail[trail.length] !== id)
        continue;
      try {
        if (!uuid.test(id)) throw Error('invalid child id');
        const child = await get(draftRoute(id));
        if (
          child?.id !== id ||
          child.parentSplit !== draft.id ||
          child.project !== draft.project
        )
          throw Error('unconfirmed child');
        await visit(child, [...trail, child.id]);
      } catch {
        unconfirmed++;
      }
    }
  };
  await visit(root, [root.id]);
  const result: Details[] = Array.from(drafts, ([id, value]) => ({
    target: { type: 'draft', id, received: true },
    ...value,
  }));
  for (const { issue, trail } of rotated([...issues.values()], rootOffset)) {
    try {
      const detail = await (options.readIssue ?? get)(issueRoute(issue));
      if (
        detail?.project !== issue.project ||
        detail.number !== issue.number ||
        !positive(detail.desk)
      )
        throw Error('unconfirmed issue link');
      result.push({
        target: { type: 'issue', received: true, desk: detail.desk, ...issue },
        detail,
        issue,
        trail,
      });
    } catch {
      unconfirmed++;
    }
  }
  return { items: result, unconfirmed };
}

const targetKey = (target: WatchTarget) =>
  target.type === 'draft' ? 'draft:' + target.id : 'desk:' + target.desk;

function outcome({ target, detail, issue }: Details) {
  if (target.type !== 'issue' || !issue || !target.desk) return undefined;
  const evidence = detail.mergeEvidence;
  const verified =
    evidence?.repo === issue.project &&
    positive(evidence.pr) &&
    /^[0-9a-f]{40}$/.test(evidence.mergeCommitSha ?? '')
      ? { project: evidence.repo, number: evidence.pr, merged: true }
      : undefined;
  const route = detail.closeReason?.route;
  const linked =
    verified &&
    route?.repo === verified.project &&
    route?.pr === verified.number &&
    !positive(detail.pr?.number)
      ? { ...detail, pr: { number: verified.number } }
      : detail;
  const result = classifyWatchOutcome(
    { ...issue, desk: target.desk },
    linked,
    verified,
  );
  if (result === 'withdrawn' || (result === 'merged' && verified))
    return result;
}

function decision(detail: any, type: WatchTarget['type']): string {
  const questions =
    type === 'draft' && detail?.status === 'draft'
      ? detail.questions
      : type === 'issue'
        ? detail.latestAsk
          ? [detail.latestAsk]
          : detail.result?.questions
        : [];
  const concrete = Array.isArray(questions)
    ? questions
        .map((item: any) =>
          typeof item === 'string'
            ? item.trim()
            : String(item?.text ?? item?.question ?? '').trim(),
        )
        .filter(Boolean)
    : [];
  if (concrete.length) return 'question:' + hash(JSON.stringify(concrete));
  const approval = detail?.pendingFinalApprovals?.find(
    (item) =>
      typeof item?.id === 'string' &&
      /^[0-9a-f]{40}$/.test(item?.headSha) &&
      typeof item.reason === 'string' &&
      item.reason.trim(),
  );
  if (approval) return approval.id + '@' + approval.headSha;
  return '';
}

async function checkResponse(
  runtime: Pick<Runtime, 'get' | 'response' | 'saveResponse' | 'verified'>,
  identity: Identity,
  item: Details,
  startedAt: number,
) {
  const saved = runtime.verified?.(identity);
  if (
    saved?.kind === 'operation' ||
    (saved?.kind === 'waiting' &&
      saved.decision === decision(item.detail, item.target.type)) ||
    (saved?.kind === 'investigated' && item.detail.status === 'stopped')
  )
    return true;
  const evidence = runtime.response(identity.notificationId);
  if (!evidence) return false;
  const { target, detail, issue } = item;
  if (evidence.action === 'read') {
    await runtime.saveResponse(identity, {
      kind: 'investigated',
      evidenceId: identity.notificationId,
    });
    return detail.status === 'stopped';
  }
  if (evidence.action === 'waiting') {
    const confirmed = decision(detail, target.type);
    if (confirmed && confirmed === evidence.reference) {
      await runtime.saveResponse(identity, {
        kind: 'waiting',
        decision: confirmed,
      });
      return true;
    }
  }
  if (
    issue &&
    evidence.action === 'operation' &&
    uuid.test(evidence.reference ?? '')
  ) {
    const receipt = await runtime.get(
      '/api/comment?' + new URLSearchParams({ id: evidence.reference! }),
    );
    if (
      receipt?.id === evidence.reference &&
      receipt.status === 'done' &&
      receipt.accepted === true &&
      !receipt.error &&
      receipt.project === issue.project &&
      receipt.number === issue.number &&
      receipt.action === 'reply' &&
      Date.parse(receipt.createdAt) >= startedAt &&
      positive(receipt.result?.job) &&
      positive(receipt.result?.id)
    ) {
      await runtime.saveResponse(identity, {
        kind: 'operation',
        evidenceId: evidence.reference!,
      });
      return true;
    }
  }
  return false;
}

function notifyFallback(
  runtime: Runtime,
  identity: Identity,
  item: Details,
  delivered: boolean,
) {
  const id = delivered
    ? hash(identity.notificationId + ':unanswered')
        .slice(0, 32)
        .replace(
          /^(........)(....)(....)(....)(............)$/,
          '$1-$2-$3-$4-$5',
        )
    : identity.notificationId;
  return runtime.notice({
    kind: 'ops-watch',
    id,
    text:
      (delivered
        ? 'Codexの元会話で対処を確認できません。'
        : 'Codexの起票元の会話へ追跡通知を配信できません。') +
      ' 対象 ' +
      identity.targetId,
    reason: '受付で原因と必要な対処を確認してください。',
    issues: item.issue
      ? [{ repo: item.issue.project, number: item.issue.number }]
      : [],
  });
}

function eventKey(
  group: string,
  work: { reason: string; nextActor: string; nextAction: string },
  item: Details,
) {
  return JSON.stringify([
    group,
    work.reason,
    work.nextActor,
    work.nextAction,
    decision(item.detail, item.target.type),
  ]);
}

export async function poll(runtime: Runtime) {
  const deadline = runtime.deadline ?? Infinity;
  const counts = {
    confirmed: 0,
    unconfirmed: 0,
    delivered: 0,
    merged: 0,
    withdrawn: 0,
  };
  if (!runtime.records.length) return counts;
  let work: WorkResponse;
  try {
    work = await runtime.get(workRoute);
  } catch {
    counts.unconfirmed += runtime.records.length;
    return counts;
  }
  for (const receipt of runtime.records) {
    // Reserve time for issue lookups and notification after exploring this receipt.
    const draftDeadline = deadline - 35_000;
    const issueDeadline = deadline - 15_000;
    if (runtime.now() >= draftDeadline) break;
    runtime.begin?.(receipt);
    const boundedRead = (latest = Infinity) => {
      let readDeadline: number | undefined;
      return (route: string) => {
        readDeadline ??= Math.min(runtime.now() + 45_000, latest);
        if (runtime.now() >= readDeadline)
          throw Error('受付読取の時間上限に達しました');
        return runtime.get(route);
      };
    };
    try {
      if (!validReceipt(receipt, runtime.host)) continue;
      const { items, unconfirmed } = await readTargets(
        receipt,
        boundedRead(draftDeadline),
        {
          rotation: (draftId) => runtime.rotation?.(receipt, draftId) ?? 0,
          readIssue: boundedRead(issueDeadline),
        },
      );
      counts.unconfirmed += unconfirmed;
      for (const item of items) {
        if (runtime.now() >= deadline) break;
        const key = receipt.sendId + '/' + targetKey(item.target);
        const previous = runtime.load(key);
        const terminal = outcome(item);
        if (terminal) {
          if (previous && !previous.terminal)
            await checkResponse(
              runtime,
              {
                sendId: receipt.sendId,
                conversationId: receipt.threadId,
                targetId: targetKey(item.target),
                notificationId: previous.id,
              },
              item,
              previous.startedAt,
            );
          runtime.save(key, {
            event: terminal,
            id: previous?.id ?? crypto.randomUUID(),
            startedAt: previous?.startedAt ?? runtime.now(),
            terminal,
          });
          counts[terminal]++;
          continue;
        }
        const classified = classifyWatchTargets([item.target], work)[0];
        if (classified.status !== 'confirmed') {
          if (previous && !previous.terminal)
            await checkResponse(
              runtime,
              {
                sendId: receipt.sendId,
                conversationId: receipt.threadId,
                targetId: targetKey(item.target),
                notificationId: previous.id,
              },
              item,
              previous.startedAt,
            );
          if (item.detail.status !== 'submitted') counts.unconfirmed++;
          continue;
        }
        counts.confirmed++;
        const { group, work: status } = classified;
        const event = eventKey(group, status, item);
        const active = group === 'attention' || group === 'stopped';
        if (previous && previous.event !== event) {
          await checkResponse(
            runtime,
            {
              sendId: receipt.sendId,
              conversationId: receipt.threadId,
              targetId: targetKey(item.target),
              notificationId: previous.id,
            },
            item,
            previous.startedAt,
          );
        }
        if (!previous && !active) continue;
        const checkpoint =
          previous?.event === event
            ? previous
            : { event, id: crypto.randomUUID(), startedAt: runtime.now() };
        const { id } = checkpoint;
        if (checkpoint !== previous) runtime.save(key, checkpoint);
        if (!active) continue;
        const identity = {
          sendId: receipt.sendId,
          conversationId: receipt.threadId,
          targetId: targetKey(item.target),
          notificationId: id,
        };
        // Finish the recheck for an admitted notification even at the end of the sweep.
        const get = boundedRead();
        const result = await runtime.dispatch(
          runtime.stateDirectory,
          identity,
          item.target,
          {
            readClassification: async () => {
              const latest = await readTargets(receipt, get, {
                selected: item,
              });
              const linked = latest.items.find(
                (row) =>
                  targetKey(row.target) === identity.targetId && !outcome(row),
              );
              if (!linked) throw Error('target no longer linked');
              const current = await get(workRoute);
              const actual = classifyWatchTargets([linked.target], current)[0];
              if (
                actual.status !== 'confirmed' ||
                eventKey(actual.group, actual.work, linked) !== event
              )
                throw Error('target changed');
              return current;
            },
            deliver: runtime.deliver,
          },
        );
        if (result === 'delivered') {
          counts.delivered++;
          const deliveredAt = checkpoint.deliveredAt ?? runtime.now();
          if (!checkpoint.deliveredAt)
            runtime.save(key, { ...checkpoint, deliveredAt });
          const handled = await checkResponse(
            runtime,
            identity,
            item,
            checkpoint.startedAt,
          );
          if (!handled && runtime.now() - deliveredAt >= 10 * 60 * 1000)
            await notifyFallback(runtime, identity, item, true);
        } else if (
          ['offline', 'unsupported', 'unknown', 'invalid'].includes(result) ||
          (result === 'busy' &&
            runtime.now() - checkpoint.startedAt >= 10 * 60 * 1000)
        ) {
          await notifyFallback(runtime, identity, item, false);
        }
      }
    } catch {
      counts.unconfirmed++;
    } finally {
      runtime.processed?.();
    }
  }
  return counts;
}

const runFile = promisify(execFile);
const hash = (value: string) =>
  crypto.createHash('sha256').update(value).digest('hex');
const privateMode = (stat: fs.Stats) =>
  stat.uid === process.getuid?.() && !(stat.mode & 0o077);

export function privateDirectory(directory: string, create = false) {
  const created =
    create && fs.mkdirSync(directory, { mode: 0o700, recursive: true });
  if (created) {
    const fd = fs.openSync(
      directory,
      fs.constants.O_RDONLY |
        fs.constants.O_DIRECTORY |
        fs.constants.O_NOFOLLOW,
    );
    try {
      const stat = fs.fstatSync(fd);
      if (!stat.isDirectory() || stat.uid !== process.getuid?.())
        throw Error('unsafe watch directory');
      // Some NAS filesystems apply inherited permissions despite the mkdir mode.
      fs.fchmodSync(fd, 0o700);
    } finally {
      fs.closeSync(fd);
    }
  }
  const stat = fs.lstatSync(directory);
  if (!stat.isDirectory() || !privateMode(stat))
    throw Error('unsafe watch directory');
}

function readPrivate(file: string) {
  let fd: number;
  try {
    fd = fs.openSync(
      file,
      fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW | fs.constants.O_NONBLOCK,
    );
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return undefined;
    throw error;
  }
  try {
    const stat = fs.fstatSync(fd);
    if (!stat.isFile() || !privateMode(stat) || stat.size > 16_384)
      throw Error('unsafe watch record');
    return JSON.parse(fs.readFileSync(fd, 'utf8'));
  } finally {
    fs.closeSync(fd);
  }
}

function writePrivate(file: string, value: object) {
  const directory = path.dirname(file);
  const temporary = path.join(directory, '.' + crypto.randomUUID());
  const fd = fs.openSync(temporary, 'wx', 0o600);
  try {
    try {
      fs.writeFileSync(fd, JSON.stringify(value) + '\n');
      fs.fsyncSync(fd);
    } finally {
      fs.closeSync(fd);
    }
    fs.renameSync(temporary, file);
    const directoryFd = fs.openSync(
      directory,
      fs.constants.O_RDONLY | fs.constants.O_DIRECTORY,
    );
    try {
      fs.fsyncSync(directoryFd);
    } finally {
      fs.closeSync(directoryFd);
    }
  } finally {
    if (fs.existsSync(temporary)) fs.unlinkSync(temporary);
  }
}

function loadRecords(directory: string, host: string) {
  if (!fs.existsSync(directory)) return [];
  privateDirectory(directory);
  const records: Receipt[] = [];
  for (const name of fs.readdirSync(directory).sort()) {
    const id = name.endsWith('.json') ? name.slice(0, -5) : '';
    if (!uuid.test(id)) continue;
    try {
      const record = readPrivate(path.join(directory, name));
      if (validReceipt(record, host) && record.sendId === id)
        records.push(record);
    } catch {
      console.error('Codex巡回の回復記録を読み取れません。');
    }
  }
  return records;
}

function runtimePaths() {
  const home = os.homedir();
  const base = path.join(home, '.issue-desk');
  const stateDirectory = path.join(base, 'codex-watch');
  return { base, stateDirectory, recovery: path.join(base, 'recovery') };
}

async function deskRead(base: string, route: string) {
  const { stdout } = await runFile(
    path.join(base, 'desk'),
    ['desk-get.ts', 'GET', route, '--max-bytes', '16777216'],
    { timeout: 12000, maxBuffer: 18 * 1024 * 1024 },
  );
  return JSON.parse(stdout);
}

async function sendNotice(base: string, notice: Notice) {
  const child = spawn(
    'sh',
    [
      path.join(base, 'desk-pc'),
      'operation',
      '--method',
      'POST',
      '--path',
      '/api/v1/notices',
      '--body-file',
      '-',
    ],
    { stdio: ['pipe', 'pipe', 'ignore'], timeout: 12000 },
  );
  const response = new Promise<any>((resolve, reject) => {
    let output = '';
    child.stdout.setEncoding('utf8').on('data', (chunk) => {
      output += chunk;
      if (output.length > 65536) child.kill();
    });
    child.once('error', reject);
    child.once('close', (code) => {
      if (code !== 0) reject(Error('notice failed'));
      else {
        try {
          resolve(JSON.parse(output));
        } catch (error) {
          reject(error);
        }
      }
    });
  });
  child.stdin.on('error', () => child.kill());
  child.stdin.end(JSON.stringify(notice));
  const result = await response;
  if (![200, 201].includes(result?.status) || result.body?.id !== notice.id)
    throw Error('notice not accepted');
}

async function deliverOnHost(
  stateDirectory: string,
  message: Parameters<Runtime['deliver']>[0],
  verifyOnly = false,
  sockets = path.join('/tmp', 'codex-daemon-' + process.getuid?.()),
  connection?: Parameters<typeof deliver>[2],
) {
  if (!fs.existsSync(sockets)) return 'offline';
  privateDirectory(sockets);
  const journal = path.join(stateDirectory, 'journal');
  privateDirectory(journal, true);
  const candidates = fs
    .readdirSync(sockets)
    .filter((name) => /^[0-9a-f]{64}$/.test(name));
  const seen = path.join(
    journal,
    hash(JSON.stringify([message.conversationId, message.notificationId])),
  );
  let status = 'offline';
  for (const name of candidates.slice(0, 8)) {
    const socket = path.join(sockets, name);
    const registration = path.join(
      stateDirectory,
      'socket-' + hash(message.conversationId + '/' + name) + '.json',
    );
    writePrivate(registration, {
      host: os.hostname(),
      conversationId: message.conversationId,
      socket,
      journal,
      codexVersion: '0.171.0',
    });
    const result = await deliver(
      { ...message, guidance: true },
      registration,
      connection,
      verifyOnly,
    );
    if (result === 'delivered' || result === 'busy') return result;
    if (fs.existsSync(seen)) status = 'unknown';
    else if (status === 'offline' && result !== 'offline') status = result;
  }
  return status;
}

function stateFile(directory: string, identity: Identity) {
  // Same key used by dispatchWatchLocal for a notification.
  return path.join(
    directory,
    hash(
      JSON.stringify([
        identity.sendId,
        identity.conversationId,
        identity.targetId,
        identity.notificationId,
      ]),
    ) + '.json',
  );
}

export async function main(
  args: string[],
  options: {
    socketDirectory?: string;
    connection?: Parameters<typeof deliver>[2];
  } = {},
) {
  const { base, stateDirectory, recovery } = runtimePaths();
  const host = os.hostname();
  const records = loadRecords(recovery, host);
  const get = (route: string) => deskRead(base, route);
  privateDirectory(stateDirectory, true);
  if (args[0] === 'record' && (args.length === 4 || args.length === 5)) {
    const [, notificationId, target, action, reference] = args;
    const conversationId = process.env.CODEX_THREAD_ID;
    if (
      !uuid.test(notificationId) ||
      !conversationId ||
      !uuid.test(conversationId) ||
      !['read', 'operation', 'waiting'].includes(action)
    )
      throw Error('invalid response parameters');
    for (const receipt of records.filter(
      (record) => record.threadId === conversationId,
    )) {
      const identity = {
        sendId: receipt.sendId,
        conversationId,
        targetId: target,
        notificationId,
      };
      const state = resumeWatchState(
        stateFile(stateDirectory, identity),
        identity,
      );
      if (!state) continue;
      const item = (await readTargets(receipt, get)).items.find(
        (row) => targetKey(row.target) === target,
      );
      if (!item) throw Error('unconfirmed response target');
      const checkpoint = readPrivate(
        path.join(stateDirectory, 'event-' + notificationId + '.json'),
      );
      if (!positive(checkpoint?.startedAt))
        throw Error('missing notification evidence');
      const currentDecision = decision(item.detail, item.target.type);
      const resolvedReference =
        action === 'waiting' && reference === 'question'
          ? currentDecision.startsWith('question:')
            ? currentDecision
            : ''
          : reference;
      const verified: { response?: WatchState['response'] } = {};
      await checkResponse(
        {
          get,
          response: () => ({
            action: action as ResponseEvidence['action'],
            reference: resolvedReference,
          }),
          saveResponse: async (_, response) => {
            verified.response = response;
          },
        },
        identity,
        item,
        checkpoint.startedAt,
      );
      const response = verified.response;
      if (!response) throw Error('unconfirmed response evidence');
      await withWatchLock(path.join(stateDirectory, 'poll.lock'), async () => {
        await recordWatchResponse(
          stateFile(stateDirectory, identity),
          identity,
          response,
          async () => {
            for (const state of ['held', 'decision'] as const) {
              if (
                (await deliverOnHost(
                  stateDirectory,
                  {
                    notificationId,
                    conversationId,
                    target,
                    state,
                  },
                  true,
                  options.socketDirectory,
                  options.connection,
                )) === 'delivered'
              ) {
                return true;
              }
            }
            return false;
          },
        );
      });
      console.log('Codex巡回の対処証拠を保存しました。');
      return;
    }
    throw Error('notification not delivered to this conversation');
  }
  if (args.length) throw Error('expected one sweep or a response record');
  await withWatchLock(path.join(stateDirectory, 'poll.lock'), async () => {
    const indexFile = path.join(stateDirectory, 'poll-position.json');
    const savedPosition = readPrivate(indexFile);
    const interrupted = records.findIndex(
      (record) => record.sendId === savedPosition?.inFlight,
    );
    const start =
      interrupted >= 0
        ? (interrupted + 1) % records.length
        : savedPosition?.offset;
    const offset = positive(start) && start < records.length ? start : 0;
    const batch = [...records.slice(offset), ...records.slice(0, offset)].slice(
      0,
      6,
    );
    let processed = offset;
    const result = await poll({
      host,
      deadline: Date.now() + 80_000,
      now: Date.now,
      rotation: (receipt, draftId) => {
        const key = path.join(
          stateDirectory,
          'rotation-' + hash(receipt.sendId + '/' + draftId) + '.json',
        );
        const saved = readPrivate(key)?.offset;
        const offset = positive(saved) ? saved : 0;
        writePrivate(key, { offset: offset + 1 });
        return offset;
      },
      begin: (receipt) =>
        writePrivate(indexFile, {
          offset: processed,
          inFlight: receipt.sendId,
        }),
      processed: () => {
        processed = (processed + 1) % records.length;
        writePrivate(indexFile, { offset: processed });
      },
      records: batch,
      get,
      stateDirectory,
      load: (key) =>
        readPrivate(path.join(stateDirectory, 'target-' + hash(key) + '.json')),
      save: (key, checkpoint) => {
        const event = path.join(
          stateDirectory,
          'event-' + checkpoint.id + '.json',
        );
        if (!readPrivate(event))
          writePrivate(event, { startedAt: checkpoint.startedAt });
        writePrivate(
          path.join(stateDirectory, 'target-' + hash(key) + '.json'),
          checkpoint,
        );
      },
      response: () => undefined,
      verified: (identity) =>
        resumeWatchState(stateFile(stateDirectory, identity), identity)
          ?.response,
      saveResponse: async (identity, response) => {
        const file = stateFile(stateDirectory, identity);
        const state = resumeWatchState(file, identity);
        if (
          state?.notification === 'delivered' &&
          state.response.kind !== 'operation' &&
          !(
            state.response.kind === 'waiting' &&
            response.kind === 'investigated'
          )
        )
          saveWatchState(file, { ...state, response });
      },
      notice: (notice) => sendNotice(base, notice),
      dispatch: dispatchWatchLocal,
      deliver: (message) => deliverOnHost(stateDirectory, message),
    });
    console.log(JSON.stringify(result));
  });
}

if (
  process.argv[1] &&
  pathToFileURL(process.argv[1]).href === import.meta.url
) {
  main(process.argv.slice(2)).catch((error) => {
    console.error('Codex巡回が処理を完了できません。', error.message);
    process.exitCode = 1;
  });
}
