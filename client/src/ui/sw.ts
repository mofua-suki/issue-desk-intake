const serviceWorker = self as unknown as ServiceWorkerGlobalScope;
// 受付全体（/）を範囲にし、版付きURLやキャッシュは使わない。
serviceWorker.addEventListener('install', (event) =>
  event.waitUntil(serviceWorker.skipWaiting()),
);
serviceWorker.addEventListener('activate', (event) =>
  event.waitUntil(serviceWorker.clients.claim()),
);
function pushTarget(value: unknown) {
  try {
    const url = new URL(
      typeof value === 'string' ? value : '/',
      serviceWorker.location.origin,
    );
    if (
      url.origin === serviceWorker.location.origin &&
      !url.username &&
      !url.password &&
      !url.search
    )
      return url.href;
  } catch {
    /* 不正な通知先は受付の先頭へ戻す。 */
  }
  return serviceWorker.location.origin + '/';
}
serviceWorker.addEventListener('push', (event) => {
  let payload: Record<string, unknown> = {};
  try {
    const data: unknown = event.data?.json();
    if (data && typeof data === 'object' && !Array.isArray(data))
      payload = data as Record<string, unknown>;
  } catch {
    /* 不正な本文でも固定文の通知を出す。 */
  }
  event.waitUntil(
    serviceWorker.registration.showNotification(
      typeof payload.title === 'string' ? payload.title : '開発受付',
      {
        body:
          typeof payload.body === 'string'
            ? payload.body
            : '受付を確認してください。',
        data: { url: pushTarget(payload.url || '/') },
      },
    ),
  );
});
serviceWorker.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const url = pushTarget(event.notification.data?.url || '/');
  event.waitUntil(
    (async () => {
      const windows = await serviceWorker.clients.matchAll({
        type: 'window',
        includeUncontrolled: true,
      });
      const existing = windows.find(
        (client: { url: string | URL }) =>
          new URL(client.url).origin === serviceWorker.location.origin,
      );
      if (existing) {
        try {
          await existing.focus();
          await existing.navigate(url);
        } catch {
          await serviceWorker.clients.openWindow(url);
        }
      } else await serviceWorker.clients.openWindow(url);
    })(),
  );
});
