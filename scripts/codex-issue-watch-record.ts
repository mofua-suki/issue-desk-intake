import {
  resumeWatchState,
  saveWatchState,
  type WatchState,
} from './codex-issue-watch-state.ts';

type Identity = Pick<
  WatchState,
  'sendId' | 'conversationId' | 'targetId' | 'notificationId'
>;

// The caller holds the watch lock across this read, confirmation, and save.
export async function recordWatchResponse(
  file: string,
  identity: Identity,
  response: WatchState['response'],
  confirm: () => Promise<boolean>,
) {
  const current = resumeWatchState(file, identity);
  if (!current || (current.notification === 'pending' && !(await confirm())))
    throw Error('notification is not delivered');
  if (current.response.kind === 'operation') return;
  if (current.response.kind === 'waiting' && response.kind === 'investigated')
    return;
  saveWatchState(file, { ...current, notification: 'delivered', response });
}
