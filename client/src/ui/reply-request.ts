export type ReplyReceipt = {
  accepted?: boolean;
  id: string;
  project: string;
  number: number;
  status: string;
  result?: unknown;
  error?: string;
  errorCode?: string;
};

// Keep the composer request and image IDs until this exact send has completed.
export async function waitReplyRequest(
  receipt: ReplyReceipt,
  expected: { id: string; project: string; number: number },
  read: (path: string) => Promise<unknown>,
  {
    sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms)),
    now = Date.now,
    timeout = 900000,
  } = {},
): Promise<unknown> {
  const deadline = now() + timeout;
  let state = receipt;
  for (;;) {
    if (
      state?.id !== expected.id ||
      state.project !== expected.project ||
      state.number !== expected.number
    )
      throw Error(`返信の照会対象が一致しません。送信ID: ${expected.id}`);
    if (state.status === 'done' && state.result != null) return state.result;
    if (state.status === 'failed')
      throw Object.assign(
        Error(
          `${state.error || '返信を拒否されました。'}\n送信ID: ${expected.id}`,
        ),
        { status: 409, retryable: 'new-request', code: state.errorCode },
      );
    if (state.status !== 'queued' || now() >= deadline)
      throw Error(
        `${state.error || '返信結果は未確認です。'}\n送信ID: ${expected.id}`,
      );
    await sleep(Math.min(3000, Math.max(0, deadline - now())));
    state = (await read(
      `/api/comment?${new URLSearchParams({ id: expected.id })}`,
    )) as ReplyReceipt;
  }
}
