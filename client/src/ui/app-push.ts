// app.js と同じスコープで配信する、画面の端末通知。
function pushKeyBytes(key: string) {
  const raw = atob(key.replaceAll('-', '+').replaceAll('_', '/'));
  return Uint8Array.from(raw, (character) => character.charCodeAt(0));
}
function pushPanel() {
  const main = document.querySelector('main');
  if (!main) return;
  const ios =
    /iPad|iPhone|iPod/.test(navigator.userAgent) ||
    (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1);
  const standalone =
    window.matchMedia('(display-mode: standalone)').matches ||
    ('standalone' in navigator && navigator.standalone === true);
  const supported =
    'serviceWorker' in navigator &&
    'PushManager' in window &&
    'Notification' in window &&
    (!ios || standalone);
  const status = el('p', {
    role: 'status',
    'aria-live': 'polite',
    text: '通知の状態を確認しています。',
  });
  const enable = el('button', {
    type: 'button',
    text: 'iPhoneに通知を受け取る',
    disabled: !supported,
  });
  const test = el('button', {
    type: 'button',
    text: 'テスト通知',
    disabled: true,
  });
  main.after(
    el(
      'section',
      { 'aria-label': 'iPhoneへの通知' },
      hint(
        'iPhoneではiOS 16.4以降で、Safariからホーム画面に追加した受付画面を開き、通知を許可してください。',
      ),
      enable,
      test,
      status,
    ),
  );
  if (!supported) {
    status.textContent =
      ios && !standalone
        ? 'ホーム画面に追加した受付画面から通知を設定してください。'
        : 'この端末・ブラウザーはWeb Pushに対応していません。';
    return;
  }
  let registration: ServiceWorkerRegistration,
    busy = false,
    testAvailable = false;
  const ready = navigator.serviceWorker
    .register('/sw.js', { scope: '/' })
    .then(() => navigator.serviceWorker.ready)
    .then(async (value) => {
      registration = value;
      await refresh();
      return value;
    });
  ready.catch(() => {
    status.textContent =
      '通知を準備できませんでした。画面を再読み込みしてください。';
  });
  async function refresh() {
    const subscription = await registration.pushManager.getSubscription();
    const allowed = Notification.permission === 'granted';
    testAvailable = !!subscription && allowed;
    test.disabled = busy || !testAvailable;
    status.textContent = !allowed
      ? Notification.permission === 'denied'
        ? '通知が拒否されています。端末の設定で許可してください。'
        : '通知はまだ許可されていません。'
      : subscription
        ? 'この端末は通知を購読しています。受信を確認するにはテスト通知を送ってください。'
        : '通知は許可されています。受け取るボタンで登録してください。';
  }
  async function run(fn: () => Promise<void>) {
    if (busy) return;
    busy = true;
    enable.disabled = test.disabled = true;
    operations++;
    try {
      if (!session?.authenticated)
        throw Error('受付に接続してから通知を設定してください。');
      await fn();
    } catch {
      status.textContent =
        '通知を設定・送信できませんでした。接続と通知の許可を確認して再操作してください。';
    } finally {
      operations--;
      busy = false;
      enable.disabled = false;
      test.disabled = !testAvailable;
    }
  }
  enable.onclick = () =>
    run(async () => {
      // iOS の許可要求はユーザーが押した直後、通信などの await より前に行う。
      const permission = await Notification.requestPermission();
      if (permission !== 'granted') {
        await ready;
        return refresh();
      }
      await ready;
      const key = await api<{ publicKey: string }>('/api/push/key');
      let subscription = await registration.pushManager.getSubscription();
      // 受付の鍵が復元・更新されていれば、ブラウザー側も同じ公開鍵で作り直す。
      const existingKey = subscription?.options?.applicationServerKey;
      if (
        subscription &&
        existingKey &&
        String(new Uint8Array(existingKey)) !==
          String(pushKeyBytes(key.publicKey))
      ) {
        await subscription.unsubscribe();
        subscription = null;
      }
      subscription ??= await registration.pushManager.subscribe({
        userVisibleOnly: true,
        applicationServerKey: pushKeyBytes(key.publicKey),
      });
      // 登録済みでも再送する。保存途中の通信失敗や、受付の退避からの復元にも対応する。
      await api('/api/push/subscriptions', subscription.toJSON());
      await refresh();
      test.disabled = false;
    });
  test.onclick = () =>
    run(async () => {
      await api('/api/push/test', {});
      await refresh();
      test.disabled = false;
      status.textContent =
        'テスト通知を送信しました。端末への到着を確認してください。';
    });
}
pushPanel();
