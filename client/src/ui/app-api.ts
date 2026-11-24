// 受付画面の API 呼び出し・通知（#notice・#toast）・操作中の数・画面の世代（reset・isCurrent）。定期取得と即時通知（SSE）は
// client/src/ui/app-live.js。client/src/ui/app.js の一部（Issue #456）で、単独のモジュールではない。受付は client/scripts/build.ts の
// 順に app.js とつないだ1つのモジュールとして /app.js を配信する。
function api(
  url: '/api/session',
  data?: unknown,
  method?: string,
): Promise<SessionResponse>;
function api(
  url: '/api/meta',
  data?: unknown,
  method?: string,
): Promise<MetaResponse>;
function api<Result = unknown>(
  url: string,
  data?: unknown,
  method?: string,
): Promise<Result>;
async function api<Result = unknown>(
  url: string,
  data?: unknown,
  method = data === undefined ? 'GET' : 'POST',
): Promise<Result> {
  if (
    url === '/api/comment' &&
    method === 'POST' &&
    data &&
    typeof data === 'object' &&
    'action' in data &&
    data.action === 'reply'
  )
    data = { ...data, async: true };
  const prefetched =
    method === 'GET' && data === undefined ? takeBootRead(url) : null;
  // 保存・送信・削除などは、受け付けられたら一覧を開き直したときにすぐ裏で取り直す（保持した一覧は先に描く）。
  // 通信が切れた・応答を読めなかったときも受付では処理済みかもしれないので同じにする。一覧を変えない要求は除く。
  const changes = method !== 'GET' && !listNeutral.includes(url);
  let r, v;
  try {
    const result = await (prefetched ??
      requestJson(url, { method, data, csrf: session?.csrf || '' }));
    r = result.response;
    v = result.value;
  } catch (caughtValue) {
    const e = uiError(caughtValue);
    if (changes) staleList();
    throw e;
  }
  if (!r.ok) {
    if (r.status === 401) {
      // 認証が切れた。保持した一覧は再読み込みの後にも出さない。
      dropList();
      setTimeout(() => location.reload(), 1500);
    }
    const failure = v as APIProblem;
    throw Object.assign(
      Error(failure.error + (failure.detail ? `\n${failure.detail}` : '')),
      {
        status: r.status,
        retryable: failure.retryable,
        // 理由の識別子（Issue #514）。同じ状態番号の別の理由を文言に頼らず見分ける。
        code: failure.code,
      },
    );
  }
  if (changes) staleList();
  const receipt = v as ReplyReceipt | null;
  if (
    url === '/api/comment' &&
    method === 'POST' &&
    receipt?.accepted === true
  ) {
    const input = data as { id: string; project: string; number: number };
    return (await waitReplyRequest(receipt, input, (path) =>
      api(path),
    )) as Result;
  }
  return v as Result;
}
function error(value: unknown) {
  const e = uiError(value);
  $('#notice').textContent = e.message || String(e);
  $('#notice').hidden = false;
}
$('#notice').onclick = () => ($('#notice').hidden = true);
// 成功の知らせ。エラー（#notice）とは別の要素と専用タイマーで持ち、画面切替の reset() では消さない。
let toastTimer: number | undefined;
function toast(message: string, ms = 4000) {
  const node = $('#toast');
  cancelTimer(toastTimer);
  node.textContent = message;
  node.hidden = false;
  toastTimer = setTimeout(() => (node.hidden = true), ms);
}
async function action(
  button: { disabled: boolean },
  fn: () => void | Promise<unknown>,
) {
  const id = routeId,
    owner = activeComposer;
  button.disabled = true;
  operations++;
  try {
    await fn();
  } catch (value) {
    const e = uiError(value);
    if (isCurrent(id) && owner === activeComposer && !e.composerReported)
      error(e);
  } finally {
    operations--;
    button.disabled = false;
  }
}
// The pairing form stays on screen when only its destination hash changes.
function isCurrent(id: number) {
  return (
    id === routeId &&
    (current?.type === 'pairing' || routeHash === location.hash)
  );
}
function reset() {
  activeComposer?.dispose();
  activeComposer = null;
  routeHash = location.hash;
  cancelTimer(timer);
  polling = null;
  pollResume = false;
  cancelTimer(liveProjects);
  liveProjects = null;
  liveEarly = null;
  // 一覧の更新待ちの予約は画面ごと。更新待ちそのものは残し、一覧に戻ったときに回収する（client/src/ui/app-list-sync.js）。
  cancelTimer(listEntryTimer);
  listEntryTimer = null;
  $('#notice').hidden = true;
  current = null;
  holdReason = null;
  providerDisableRoute({ clearForm: true });
  return ++routeId;
}
