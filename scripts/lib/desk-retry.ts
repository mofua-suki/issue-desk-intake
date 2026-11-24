import { deskContract, deskContractHeader } from './desk-contract.generated.ts';
// トークン接続だけの短い再試行。送信済みか分からない POST は送信IDで照合する経路だけ。
export function retryableDeskCall(method: string, route: string, payload: any) {
  if (method === 'GET') return !/\/next(?:\?|$)/.test(route);
  if (method !== 'POST' || typeof payload?.id !== 'string' || !payload.id)
    return false;
  return (
    ['/api/v1/requests', '/api/issues', '/api/todos'].includes(route) ||
    (payload.retry !== true &&
      /^\/api\/drafts\/[a-f0-9-]{36}\/chat$/.test(route)) ||
    /^\/api\/todos\/[a-f0-9-]{36}\/save$/.test(route)
  );
}

export function connectionFailed(error: unknown) {
  const e = error as any;
  return [
    'ECONNREFUSED',
    'ECONNRESET',
    'EPIPE',
    'ENOTFOUND',
    'EAI_AGAIN',
    'ETIMEDOUT',
    'EHOSTUNREACH',
    'ENETUNREACH',
    'UND_ERR_CONNECT_TIMEOUT',
    'UND_ERR_SOCKET',
  ].includes(e?.cause?.code);
}

export async function fetchDesk(
  url: string,
  init: RequestInit,
  safe: boolean,
  {
    send = fetch,
    sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms)),
    warn = console.error,
    now = Date.now,
  }: {
    send?: typeof fetch;
    sleep?: (ms: number) => Promise<void>;
    warn?: (line: string) => void;
    now?: () => number;
  } = {},
) {
  const delays = deskContract.retryDelays;
  // NAS更新係の15分の印がある409だけ。通常の接続失敗の予算とは共有しない。
  let mismatches = 0;
  // 受付の入れ替え待ち。最初の未受理から5分まで（1回の試行は受付が最大120秒保留してから返すことがある）。
  // 接続失敗の予算とは共有しない。
  let handoffs = 0;
  let handoffSince = 0;
  for (let attempt = 0; ; attempt++) {
    try {
      const res = await send(url, init);
      const value =
        !res.ok && typeof res.clone === 'function'
          ? await res
              .clone()
              .json()
              .catch(() => ({}))
          : {};
      const retry = value.code
        ? value.code === 'TEMPORARY_UNAVAILABLE'
        : [502, 503].includes(res.status);
      const server = res.headers.get(deskContractHeader);
      const incoming = new Headers(init.headers).get(deskContractHeader);
      if (
        safe &&
        res.status === 409 &&
        value.code === 'CONTRACT_MISMATCH' &&
        res.headers.get('x-issue-desk-updating') === '1' &&
        server &&
        /^[a-f0-9]{64}$/.test(server) &&
        incoming &&
        incoming !== server &&
        mismatches < 180
      ) {
        mismatches++;
        warn(
          `受付の更新中です。契約版を再照合します（${mismatches}/180、5秒後）。送信IDと本文は保持します。`,
        );
        await res.body?.cancel();
        await sleep(5000);
        // 不一致の再照合で、接続失敗の既存の予算を消費しない。
        attempt--;
        continue;
      }
      // 未受理の印が付いた503は業務処理に届いていないので、POSTも同じ本文で送り直せる。
      // 起票・作成の停止（SERVER_DRAINING）は、送信IDで照合できる経路だけ待って送り直す。
      const notAccepted =
        res.status === 503 &&
        res.headers.get('x-issue-desk-not-accepted') === '1';
      if (
        notAccepted ||
        (safe && res.status === 503 && value.code === 'SERVER_DRAINING')
      ) {
        // 予算切れはここで返す（下の接続失敗の再送へ進んで待ちを延ばさない）。
        handoffSince ||= now();
        if (handoffs === 60 || now() - handoffSince >= 300000) return res;
        handoffs++;
        warn(
          `受付の入れ替え中です。同じ要求を送り直します（${handoffs}回目、5秒後）。送信IDと本文は保持します。`,
        );
        await res.body?.cancel();
        await sleep(5000);
        attempt--;
        continue;
      }
      if (!safe || !retry || attempt === delays.length) return res;
      await res.body?.cancel();
    } catch (e) {
      if (!safe || !connectionFailed(e) || attempt === delays.length) throw e;
    }
    await sleep(delays[attempt]);
  }
}
