// 受付画面の定期取得（poll・pollList・pollIssue）と即時通知（SSE）の接続管理（タブ間の共有・再接続）。client/src/ui/app.js の一部で、
// 単独のモジュールではない。client/scripts/build.ts の順で app-api.js の直後につなぎ、同じスコープを共有する。
let pollResume = false;
function poll(fn: () => void | Promise<void>, ms = 5000) {
  cancelTimer(timer);
  const id = routeId;
  const resuming = pollResume && !liveHidden();
  if (resuming) {
    if (listScreen?.id === id) listNeedFull = true;
    ms = listScreen?.id === id ? listSoon() : 0;
  }
  // waiting: 予約中（まだ取得を始めていない） / again: 取得中に即時通知が届いた / hurried: 即時通知で前倒し済み。
  const entry: Polling = (polling = {
    fn,
    id,
    waiting: true,
    again: false,
    hurried: resuming,
  });
  timer = setTimeout(async () => {
    if (!isCurrent(id)) return;
    if (liveHidden()) {
      pollResume = true;
      return;
    }
    entry.waiting = false;
    pollResume = false;
    try {
      await fn();
    } catch (caughtValue) {
      const e = uiError(caughtValue);
      if (isCurrent(id)) {
        error(e);
        poll(fn, 10000);
      }
    }
    // 取得中に届いた通知は、この取得に間に合わなかったかもしれないので、次の取得を前倒しして読み直す。
    if (entry.again) hurry();
  }, ms);
  // 画面の最初の取得（定期取得の予約の前）の間に届いた通知は、予約ができたこの時点で前倒しする。
  if (liveEarly === id) {
    liveEarly = null;
    hurry();
  }
}
// 受付からの即時通知（SSE。GET /api/events。docs/live-updates.md）。通知は読み直す対象の種類と識別子だけ。
// 表示中の画面に関係するときは、その画面の次の定期取得を前倒しする。取得・描き直し・入力中の保護（sync の判定・
// holdReason）は定期取得そのものなので変わらない。続けて届いた通知は1回の取得にまとめる。
// 一覧の定期取得は接続確認中だけ30秒、それ以外は15秒。Issueの詳細は接続確認中だけ30秒、それ以外は10秒（issuePollMs）。
// 相談は接続確認中で受付の処理が動いていない間だけ30秒、それ以外は5秒（pollDraft）。
// ほかの定期取得と版の確認は接続の状態に関係なく続ける。定期取得を止めている画面
// （判断待ちのクローズ依頼・起票済みの相談など）は通知でも読み直さない。
// 一覧は全リポジトリぶんのGiteaを読むので、通知による読み直しは前の取得の開始から liveListGapMs 以上空ける
// （Issue #505。相談のAIが資料を読むたびの保存で、一覧が読み直しを繰り返さない）。
const liveDelayMs = 300,
  liveListGapMs = 3000,
  liveRetryMs = 5000,
  liveRetryMaxMs = 60000;
let liveSource: EventSource | null = null,
  liveWait = liveRetryMs,
  liveTimer: number | null | undefined = null,
  liveLost = false,
  liveProjects: number | null | undefined = null,
  // 最初の取得の間に通知が届いた画面（routeId）。定期取得の予約ができたら前倒しする（poll）。
  liveEarly: number | null = null,
  // 一覧の最後の取得を始めた時刻（client/src/ui/app-home.js の refresh()）。
  listReadAt = 0;
// 一覧を通知で読み直すまでの待ち。前の取得の開始から liveListGapMs たつまで待つ。
const listSoon = () =>
  Math.max(liveDelayMs, listReadAt + liveListGapMs - Date.now());
// 一覧の取得（通知・定期の双方）の開始直前に呼び、開始の枠を取る。前の取得（予約済みを含む）の開始から最短間隔を空け、
// 返す値は開始までに待つ ms（Issue #2530）。予約済みの別の要求と同じ時刻に始めない。listStartGapMs はテストが注入する間隔。
const listGap = () => {
  const gap = Reflect.get(globalThis, 'listStartGapMs') as unknown;
  return typeof gap === 'number' ? gap : liveListGapMs;
};
// listStartedAt は実際に要求を始めた時刻。待機タイマーが遅れても、次の枠は実際の開始から数える。
let listStartedAt = -Infinity;
function listReserve() {
  const start = Math.max(
    Date.now(),
    listReadAt + listGap(),
    listStartedAt + listGap(),
  );
  listReadAt = start;
  return start - Date.now();
}
// 待機の後（または待たずに）要求を始める直前に呼ぶ。実際の開始から最短間隔が空いていなければ、不足の ms を返す（0 なら開始を記録した）。
function listBegin() {
  const short = listStartedAt + listGap() - Date.now();
  if (short > 0) return short;
  listStartedAt = Date.now();
  listReadAt = Math.max(listReadAt, listStartedAt);
  return 0;
}
function hurry() {
  const entry = polling;
  if (!entry || entry.id !== routeId) {
    liveEarly = routeId;
    return;
  }
  if (!isCurrent(entry.id)) return;
  if (!entry.waiting) {
    entry.again = true;
    return;
  }
  if (entry.hurried) return;
  poll(entry.fn, listScreen?.id === routeId ? listSoon() : liveDelayMs);
  if (polling) polling.hurried = true;
}
const liveIssueKey = (
  project: string | number | boolean | null | undefined,
  number: number | null | undefined,
) => `${project}#${number}`;
// n: 受付の通知。null は「何が変わったか分からない」（切断からの復帰）で、表示中の画面をどれも読み直す。
function liveRelevant(n: ListNotice | null) {
  if (listScreen?.id === routeId) return n === null || n.type !== 'project';
  const c = current;
  if (!c) return false;
  if (n === null) return true;
  if (c.type === 'issue') {
    // 共通番号のURLから開き、最初の応答でプロジェクトとGitea番号が分かるまでの間（c.project が null）は、
    // Issueの個別通知を c.early に覚えておき、最初の応答で対象と照合する（client/src/ui/app-issue.js。Issue #505）。
    if (!c.project && n.type === 'issue' && c.early) {
      c.early.add(liveIssueKey(n.project, n.number));
      return false;
    }
    return (
      n.type === 'list' ||
      (n.type === 'issue' && n.project === c.project && n.number === c.number)
    );
  }
  if (c.type === 'draft') return n.type === 'draft' && n.id === c.id;
  if (c.type === 'close') return n.type === 'closeRequest' && n.id === c.id;
  if (c.type === 'project') return n.type === 'project' && n.id === c.id;
  if (c.type === 'projects') return n.type === 'project';
  return false;
}
function liveNotice(n: ListNotice | null = null) {
  if (n !== null && (!n || typeof n !== 'object')) return;
  if (n === null && pollResume && listScreen?.id === routeId) {
    listNeedFull = true;
    return;
  }
  // 一覧に出るものが変わった。対象を識別できれば1件だけ、できなければ全体を、更新待ちとして覚える。別の画面を見ていても
  // 捨てず、一覧に戻ったときに回収する。一覧の表示中は、一覧の更新（個別取得・全体照合）が定期確認とは別に予約される。
  listNote(n);
  if (listScreen?.id === routeId) return;
  if (!liveRelevant(n)) return;
  // 新しいプロジェクトの一覧は定期取得を持たないので、一覧の行だけを読み直す（見出し・スクロール位置はそのまま）。
  if (current?.type === 'projects') {
    if (liveProjects) return;
    const id = routeId;
    liveProjects = setTimeout(() => {
      liveProjects = null;
      if (isCurrent(id) && current?.type === 'projects') projectsRefresh(id);
    }, liveDelayMs);
    return;
  }
  hurry();
}
// 隠れたタブは接続を持たない。PCの画面（http://127.0.0.1、HTTP/1.1）ではブラウザーが同じ接続先への同時接続を
// 6本ほどに限るため、開いたままのタブが接続をふさぐと、どのタブの取得・送信も進まなくなる。
// 見えるようになったら接続し直し、隠れていた間の変化を一度読み直す。
const liveHidden = () =>
  typeof document !== 'undefined' && document.visibilityState === 'hidden';
let liveWatching = false;
let liveResumeConnection = false;
function watchLive() {
  if (liveWatching) return;
  liveWatching = true;
  document.addEventListener?.('visibilitychange', () => {
    if (liveHidden()) {
      pollResume = true;
      cancelTimer(listEntryTimer);
      listEntryTimer = null;
      return stopLive();
    }
    if (pollResume && polling && isCurrent(polling.id)) {
      if (listScreen?.id === routeId) {
        listNeedFull = true;
        cancelTimer(listEntryTimer);
        listEntryTimer = null;
      }
      if (polling.waiting) {
        poll(polling.fn, listScreen?.id === routeId ? listSoon() : 0);
        if (polling) polling.hurried = true;
      } else polling.again = true;
      liveLost = false;
      liveResumeConnection = true;
    }
    if (liveSource) return;
    cancelTimer(liveTimer);
    liveWait = liveRetryMs;
    startLive();
  });
}
// タブ間で接続を1本にまとめる（Issue #505）。見えているタブのうち1つ（ロック liveLockName を持つタブ）だけが接続し、
// 受け取った通知を BroadcastChannel で同じブラウザーの他のタブへ渡す。他の見えているタブはロックを待って並び、
// 持っているタブが閉じる・隠れると次のタブが引き継いで接続する。ロックか BroadcastChannel が無いブラウザーでは
// タブごとに接続する（従来どおり。受付は1つの画面のセッションの接続を liveOwnerLimit 本までにする）。
const liveLockName = 'issue-desk-live',
  liveChannelName = 'issue-desk-live';
let liveChannel: BroadcastChannel | null = null,
  // このタブが接続を持つ番か（ロックを持っている）。
  liveLeader = false,
  // ロックを求めて待っているか。
  liveQueued = false,
  // ロックを返す（持っている間はこの Promise を解かない）。
  liveRelease: (() => void) | null = null;
const liveShared = () =>
  typeof BroadcastChannel !== 'undefined' &&
  typeof navigator !== 'undefined' &&
  typeof navigator.locks?.request === 'function';
// 接続を持つタブは5秒ごとに状態を共有する。通知が無い間も確認でき、持ち主が消えた場合は15秒で失効する。
const liveHeartbeatMs = 5000,
  liveConfirmationMs = 15000;
let liveConnected = false,
  liveConfirmationTimer: number | null | undefined = null,
  liveHeartbeatTimer: number | null | undefined = null,
  // 接続の確認で周期が変わる定期取得の予約（一覧・Issueの詳細・相談）。ms はその画面の周期、at は周期の基準時刻。
  livePeriodic: {
    entry: Polling | null;
    at: number;
    ms: () => number;
  } | null = null;
const listPollMs = () =>
  liveConnected && !liveHidden() ? 30000 : listRefreshMs;
// Issueの詳細は取得のたびに受付がGiteaのIssueと全コメントを読む。受付の通知（type issue）が次の取得を前倒しするので、
// 接続を確認できる間は一覧と同じ30秒にする。通知されない変化（Giteaでの直接の操作・WorkerがGiteaへ直接書くコメント）は
// この周期で拾う（最大30秒遅れる）。接続を確認できない間は10秒のまま。
const issueRefreshMs = 10000;
const issuePollMs = () =>
  liveConnected && !liveHidden() ? 30000 : issueRefreshMs;
function pollPeriodic(
  fn: () => void | Promise<void>,
  ms: () => number,
  at = Date.now(),
) {
  at = Math.min(at, Date.now());
  poll(fn, Math.max(0, at + ms() - Date.now()));
  livePeriodic = { entry: polling, at, ms };
}
function pollList(fn: () => void | Promise<void>, at = Date.now()) {
  pollPeriodic(fn, listPollMs, at);
}
function pollIssue(fn: () => void | Promise<void>) {
  pollPeriodic(fn, issuePollMs);
}
// 相談は取得のたびに会話を含む全文を読む。相談の保存は受付の通知（type draft）が次の取得を前倒しするので、接続を確認でき、
// 受付の処理が動いていない間（moving が false。client/src/ui/app-draft.ts の draftMoving）は30秒にする。
// 動いている間と接続を確認できない間は5秒のまま。moving は周期を計算するたびに、最後に読めた相談で判定する。
const draftRefreshMs = 5000;
function pollDraft(fn: () => void | Promise<void>, moving: () => boolean) {
  pollPeriodic(fn, () =>
    liveConnected && !liveHidden() && !moving() ? 30000 : draftRefreshMs,
  );
}
function liveConnection(connected: boolean, shared = false) {
  if (shared && connected && !pollResume) liveResumeConnection = false;
  cancelTimer(liveConfirmationTimer);
  liveConfirmationTimer =
    shared && connected
      ? setTimeout(() => liveConnection(false), liveConfirmationMs)
      : null;
  const changed = liveConnected !== connected;
  liveConnected = connected;
  // 通知・再試行の予約は動かさない。通常の周期だけを、予約時の基準時刻から計算し直す。
  const pending = livePeriodic;
  if (
    changed &&
    pending?.entry &&
    pending.entry === polling &&
    pending.entry.waiting &&
    !pending.entry.hurried &&
    isCurrent(pending.entry.id)
  )
    pollPeriodic(pending.entry.fn, pending.ms, pending.at);
}
function liveStatus(request = false) {
  try {
    liveChannel?.postMessage(
      request ? { checkConnection: true } : { connected: liveConnected },
    );
  } catch {}
}
function liveHeartbeat() {
  cancelTimer(liveHeartbeatTimer);
  liveHeartbeatTimer = null;
  if (!liveLeader || !liveConnected) return;
  liveStatus();
  liveHeartbeatTimer = setTimeout(liveHeartbeat, liveHeartbeatMs);
}
function stopLive() {
  cancelTimer(liveTimer);
  liveTimer = null;
  liveConnection(false);
  cancelTimer(liveHeartbeatTimer);
  liveHeartbeatTimer = null;
  if (liveLeader) liveStatus();
  // 接続を持つ番を返し、見えている他のタブに引き継がせる。待っているタブは隠れている間の通知を受け取らない。
  liveLeader = false;
  if (liveRelease) liveRelease();
  liveRelease = null;
  if (liveChannel) liveLost = true;
  if (!liveSource) return;
  liveSource.close();
  liveSource = null;
  // 閉じていた間の通知は届かないので、つながり直したときに読み直す。
  liveLost = true;
}
function startLive() {
  if (liveSource || typeof EventSource === 'undefined' || liveHidden()) return;
  if (!liveShared() || liveLeader) return openLive();
  if (!liveQueued) return joinLive();
  liveStatus(true);
  // 他のタブが接続を持っている間に見えるようになった。隠れていた間の通知は届いていないので一度読み直す。
  if (liveLost) {
    liveLost = false;
    liveNotice(null);
  }
}
function joinLive() {
  if (!liveChannel) {
    liveChannel = new BroadcastChannel(liveChannelName);
    liveChannel.onmessage = (e) => {
      const data: unknown = e.data;
      if (!data || typeof data !== 'object' || liveHidden()) return;
      if ('checkConnection' in data && data.checkConnection === true) {
        if (liveLeader) liveStatus();
        return;
      }
      if (liveLeader) return;
      if ('connected' in data && typeof data.connected === 'boolean') {
        liveConnection(data.connected, true);
        return;
      }
      const n: unknown =
        data && typeof data === 'object' && 'notice' in data
          ? data.notice
          : undefined;
      if (n === null || validListNotice(n)) {
        liveConnection(true, true);
        liveNotice(n);
      }
    };
  }
  liveStatus(true);
  liveQueued = true;
  navigator.locks
    .request(liveLockName, { ifAvailable: true }, (lock) => {
      if (lock) return leadLive(false);
      // 他のタブが接続を持っている。隠れていた間の通知は届いていないので一度読み直す。
      if (liveLost && !liveHidden()) {
        liveLost = false;
        liveNotice(null);
      }
      // 持っているタブが閉じる・隠れるのを待って引き継ぐ。それまでは渡される通知で動く。
      navigator.locks
        .request(liveLockName, () => leadLive(true))
        .catch(() => (liveQueued = false));
    })
    .catch(() => {
      liveQueued = false;
      retryLive();
    });
}
// handover: 他のタブから引き継いだ。前のタブが閉じてからつながるまでの通知は届いていないので、つながったら読み直す。
function leadLive(handover: boolean) {
  liveQueued = false;
  // 隠れている間に番が来たら持たずに返す（見えたときに並び直す）。
  if (liveHidden()) return;
  liveConnection(false);
  liveLeader = true;
  if (handover && !liveResumeConnection) liveLost = true;
  openLive();
  return new Promise<void>((resolve) => (liveRelease = resolve));
}
// 通知を表示中の画面へ渡し、接続を持つ番なら同じブラウザーの他のタブへも渡す。null は「一度読み直す」。
function validListNotice(value: unknown): value is ListNotice {
  if (
    !value ||
    typeof value !== 'object' ||
    !('type' in value) ||
    typeof value.type !== 'string'
  )
    return false;
  return (
    (!('project' in value) || typeof value.project === 'string') &&
    (!('number' in value) || typeof value.number === 'number') &&
    (!('id' in value) || typeof value.id === 'string')
  );
}
function liveShare(n: ListNotice | null) {
  liveNotice(n);
  if (liveLeader && liveChannel)
    try {
      liveChannel.postMessage({ notice: n });
    } catch {}
}
function openLive() {
  let source;
  try {
    source = new EventSource('/api/events');
  } catch {
    return retryLive();
  }
  liveSource = source;
  source.onopen = () => {
    if (liveSource !== source) return;
    liveConnection(true);
    liveHeartbeat();
    liveWait = liveRetryMs;
    // 切れていた間の通知は届いていないので、つながり直したら表示中の画面を一度読み直す。
    if (liveLost && !liveResumeConnection) liveShare(null);
    liveLost = false;
    liveResumeConnection = false;
  };
  source.onmessage = (e) => {
    if (liveSource !== source) return;
    let n: unknown;
    try {
      n = JSON.parse(e.data);
    } catch {
      return;
    }
    if (validListNotice(n)) liveShare(n);
  };
  source.onerror = () => {
    if (liveSource !== source) return;
    liveResumeConnection = false;
    liveConnection(false);
    cancelTimer(liveHeartbeatTimer);
    liveHeartbeatTimer = null;
    if (liveLeader) liveStatus();
    liveLost = true;
    // 通信が切れただけならブラウザーが自分で接続し直す。受付に断られた（認証切れ・接続数の上限・停止中）ときは
    // 閉じられるので、待ち時間を延ばしながら作り直す。
    if (source.readyState !== 2) return;
    source.close();
    if (liveSource === source) liveSource = null;
    retryLive();
  };
}
function retryLive() {
  cancelTimer(liveTimer);
  liveTimer = setTimeout(startLive, liveWait);
  liveWait = Math.min(liveWait * 2, liveRetryMaxMs);
}
