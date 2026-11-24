import { awaitDraft, unconfirmed } from '../desk-client.ts';

// Retry has no message or revision; an unconfirmed POST must be checked before retrying again.
export async function retryDraft(
  call,
  { draft, timeout }: { draft?: string; timeout?: number | string },
  script,
  hint = '',
  interval = 3000,
  linkContext = {},
) {
  let started;
  try {
    started = await call('POST', `/api/drafts/${draft}/chat`, { retry: true });
  } catch (e) {
    if (e.status >= 400 && e.status < 500) {
      console.error(e.message);
      return 1;
    }
    return unconfirmed(draft, e, false, false, script, hint);
  }
  console.log(`下書き: ${started.id}`);
  return awaitDraft(
    call,
    started.id,
    timeout,
    true,
    script,
    hint,
    interval,
    linkContext,
  );
}
