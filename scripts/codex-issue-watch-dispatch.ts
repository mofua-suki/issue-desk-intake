import {
  classifyWatchTargets,
  type WatchTarget,
} from './codex-issue-watch-classification.ts';
import type { WatchState } from './codex-issue-watch-state.ts';

type Identity = Pick<
  WatchState,
  'sendId' | 'conversationId' | 'targetId' | 'notificationId'
>;
type Response = Parameters<typeof classifyWatchTargets>[1];
type Message = {
  notificationId: string;
  conversationId: string;
  target: string;
  state: 'decision' | 'held';
};

type Dependencies = {
  readClassification: () => Promise<Response>;
  // The caller holds the watch lock and assigns a state file to this identity.
  resume: (
    identity: Identity,
  ) => WatchState | null | Promise<WatchState | null>;
  save: (state: WatchState) => void | Promise<void>;
  deliver: (message: Message) => Promise<string>;
};

/** Dispatch one received target; delivery does not change the response to it. */
export async function dispatchWatch(
  identity: Identity,
  target: WatchTarget,
  dependencies: Dependencies,
): Promise<string> {
  const expectedTarget =
    target.type === 'draft'
      ? `draft:${target.id}`
      : target.desk === undefined
        ? ''
        : `desk:${target.desk}`;
  const id = /^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$/;
  if (
    !target.received ||
    identity.targetId !== expectedTarget ||
    !/^(?:draft:[A-Za-z0-9][A-Za-z0-9_-]{0,127}|desk:[1-9][0-9]*)$/.test(
      identity.targetId,
    ) ||
    !id.test(identity.conversationId) ||
    !id.test(identity.notificationId) ||
    !/^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,255}$/.test(identity.sendId)
  )
    return 'invalid';

  const previous = await dependencies.resume(identity);
  if (
    previous &&
    (['sendId', 'conversationId', 'targetId', 'notificationId'] as const).some(
      (key) => previous[key] !== identity[key],
    )
  )
    return 'invalid';
  if (previous?.notification === 'delivered') return 'delivered';

  let classification: ReturnType<typeof classifyWatchTargets>[number];
  try {
    classification = classifyWatchTargets(
      [target],
      await dependencies.readClassification(),
    )[0];
  } catch {
    return 'unconfirmed';
  }
  if (
    classification.status !== 'confirmed' ||
    (classification.group !== 'attention' && classification.group !== 'stopped')
  )
    return 'unconfirmed';

  const pending: WatchState = {
    sendId: identity.sendId,
    conversationId: identity.conversationId,
    targetId: identity.targetId,
    notificationId: identity.notificationId,
    notification: 'pending',
    response: previous?.response ?? { kind: 'pending' },
  };
  await dependencies.save(pending);
  let result: string;
  try {
    result = await dependencies.deliver({
      notificationId: identity.notificationId,
      conversationId: identity.conversationId,
      target: identity.targetId,
      state: classification.group === 'attention' ? 'decision' : 'held',
    });
  } catch {
    return 'unknown';
  }
  if (result === 'delivered')
    await dependencies.save({ ...pending, notification: 'delivered' });
  return result;
}
