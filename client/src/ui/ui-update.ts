interface UIHealth {
  ready?: boolean;
  ui?: unknown;
}
export interface ReloadRecord {
  from: string;
  to: string;
  at: number;
}
interface UpdateOptions {
  baseline: unknown;
  health(options: { signal?: AbortSignal }): Promise<UIHealth>;
  hold(): string | null | undefined;
  prepare(record: ReloadRecord): void;
  reload(): void;
  show(message: string | null): void;
  schedule(callback: () => void, delay: number): number;
  cancel(timer: number): void;
  visible?: () => boolean;
  now?: () => number;
  last?: ReloadRecord | null;
  interval?: number;
  holdEvery?: number;
  timeout?: number;
}
// 受付画面の新版の検知と自動再読み込み。配信中の画面の版（/api/health の ui）を定期的に確かめ、読み込んだ
// 画面の版と違えば案内し、利用者の要求か画面を離れた時機に入力・送信を守って読み込み直す。本文・画像・資格情報は扱わない。
export const updateInterval = 15000;
// 保留中に安全な状態へ戻ったかを見る間隔。通信はせず、画面の状態だけを見る。
export const holdInterval = 1000;
// 同じ版へ向けた再読み込みが効かなかったと判断する期間。この間は同じ版へ自動で読み込み直さない。
export const reloadGuardMs = 300000;
// 版の確認を待つ上限。応答が返らない確認は中断し、失敗として後で確かめ直す（次の確認を止めない）。
export const checkTimeout = 10000;
export const validVersion = (v: unknown): v is string =>
  typeof v === 'string' && /^[a-f0-9]{64}$/.test(v);
export const updateMessages = {
  available: '更新があります。「今すぐ更新」で新しい画面へ読み込み直せます。',
  hold: (reason: string) =>
    `更新があります。入力・送信が終わると「今すぐ更新」で読み込み直せます（${reason}）。`,
  storage:
    '更新があります。表示中の画面を復元する情報をこのブラウザーに保存できないため、自動更新を保留しています。入力を送信・保存してから手動で再読み込みしてください。',
  stuck:
    '更新があります。自動の再読み込みで新しい画面に切り替わりませんでした。入力を確かめてから「今すぐ更新」を押してください。',
};
// baseline: 読み込んだ画面の版（配信時に index.html へ埋め込んだ値）。
// health: 配信中の版を返す（失敗は例外。{ signal } を受け取り、中断されたら通信をやめる）。hold: 更新を待たせる理由（無ければ null）。
// prepare: 再読み込み後に画面を戻すための記録を保存する（失敗は例外）。last: 前回の再読み込みの記録。
export function updateWatcher({
  baseline,
  health,
  hold,
  prepare,
  reload,
  show,
  schedule,
  cancel,
  visible = () => true,
  now = Date.now,
  last = null,
  interval = updateInterval,
  holdEvery = holdInterval,
  timeout = checkTimeout,
}: UpdateOptions) {
  const enabled = validVersion(baseline);
  // 再読み込みしたのに同じ画面の版のまま戻った。同じ版へは繰り返し読み込み直さない（リロードループの防止）。
  // 期間は判定のたびに測り直し、記録から5分たてば同じ版へも再び読み込み直す。
  function stuck() {
    if (!enabled || last?.from !== baseline || !validVersion(last.to))
      return null;
    const age = now() - last.at;
    return age >= 0 && age < reloadGuardMs ? last.to : null;
  }
  let timer: number | null = null,
    checking: Promise<void> | null = null,
    target: string | null = null,
    reloading = false,
    automatic: string | null = null;
  function plan(fn: () => void | Promise<void>, ms: number): void {
    if (timer !== null) cancel(timer);
    timer = schedule(() => {
      timer = null;
      fn();
    }, ms);
  }
  function apply(manual = false): void {
    // 裏に回った後、確認や保留の間に表示へ戻ったら自動更新を取り消す。
    if (automatic === 'hidden' && visible()) automatic = null;
    if (!target) {
      automatic = null;
      show(null);
      return plan(check, interval);
    }
    if (!manual && target === stuck()) {
      show(updateMessages.stuck);
      return plan(check, interval);
    }
    const reason = hold();
    if (reason) {
      show(updateMessages.hold(reason));
      // 安全になったら、その時点の配信状態を確かめ直してから読み込み直す。
      return plan(() => (hold() ? apply() : check()), holdEvery);
    }
    if (!manual && !automatic) {
      show(updateMessages.available);
      return plan(check, interval);
    }
    try {
      prepare({ from: String(baseline), to: target, at: now() });
    } catch {
      show(updateMessages.storage);
      return plan(check, interval);
    }
    reloading = true;
    if (timer !== null) cancel(timer);
    timer = null;
    reload();
  }
  // 確認は常に1本だけ。実行中に呼ばれたら同じ確認を待つ。
  function check(): Promise<void> {
    if (!enabled || reloading) return Promise.resolve();
    if (checking) return checking;
    if (!visible() && !automatic) {
      plan(check, interval);
      return Promise.resolve();
    }
    checking = (async () => {
      let served: string | null = null,
        expire: number | null = null;
      const abort =
        typeof AbortController === 'function' ? new AbortController() : null;
      try {
        const h = await new Promise<UIHealth>((resolve, reject) => {
          // 期限を過ぎたら通信を中断して失敗とする。遅れて届いた応答は使わない。
          expire = schedule(() => {
            expire = null;
            abort?.abort();
            reject(Error('版の確認が時間内に終わりませんでした。'));
          }, timeout);
          Promise.resolve(health({ signal: abort?.signal })).then(
            resolve,
            reject,
          );
        });
        // 応答できない（再起動中・ready=false）ときや、版が欠けた・壊れたときは、画面と入力をそのままにする。
        if (h?.ready === true && validVersion(h.ui)) served = h.ui;
      } catch {
      } finally {
        if (expire !== null) cancel(expire);
      }
      checking = null;
      if (reloading) return;
      target = served && served !== baseline ? served : null;
      if (!served) {
        automatic = null;
        show(null);
        return plan(check, interval);
      }
      apply();
      if (automatic === 'navigation') automatic = null;
    })();
    return checking;
  }
  // 自動更新は新版を確認済みの画面を離れた時機だけ。進行中の確認を待ってからもう一度確認する。
  async function leave(kind: string) {
    if (!enabled || reloading || !target) return;
    if (checking) await checking;
    if (reloading || !target || (kind === 'hidden' && visible())) return;
    automatic = kind;
    await check();
  }
  return {
    enabled,
    start: () => check(),
    check,
    requestUpdate: () => {
      if (enabled && !reloading && target) apply(true);
    },
    hidden: () => leave('hidden'),
    navigate: () => leave('navigation'),
    // タブの復帰・スリープ明け・通信の回復。見えていればすぐ確かめる。
    wake: () => {
      if (!visible()) return Promise.resolve();
      automatic = null;
      return check();
    },
    state: () => ({ target, checking: !!checking, reloading, stuck: stuck() }),
  };
}
