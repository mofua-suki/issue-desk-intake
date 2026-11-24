interface RequestOptions {
  method?: string;
  data?: unknown;
  csrf?: string;
  signal?: AbortSignal;
  fetch?: typeof globalThis.fetch;
}

interface JsonResponse<T> {
  response: Response;
  value: T;
}

/** The response is decoded here; state-specific permissions remain on the server. */
export async function requestJson<T = unknown>(
  path: string,
  options: RequestOptions = {},
): Promise<JsonResponse<T>> {
  // Never send browser session credentials or CSRF to another origin.
  if (
    !path.startsWith('/api/') ||
    path.startsWith('//') ||
    // eslint-disable-next-line no-control-regex -- 入力や表示から制御文字を除外する安全性検証。
    /[\\\u0000-\u0020]/.test(path)
  ) {
    throw new TypeError('APIの接続先が不正です。');
  }
  const method = (
    options.method ?? (options.data === undefined ? 'GET' : 'POST')
  ).toUpperCase();
  const headers: Record<string, string> = {};
  if (method !== 'GET') {
    headers['Content-Type'] = 'application/json';
    headers['X-CSRF-Token'] = options.csrf ?? '';
  }
  let response: Response;
  for (let attempt = 0; ; attempt++) {
    options.signal?.throwIfAborted();
    response = await (options.fetch ?? globalThis.fetch)(path, {
      method,
      headers,
      credentials: 'same-origin',
      redirect: 'error',
      signal: options.signal,
      body:
        options.data === undefined ? undefined : JSON.stringify(options.data),
    });
    if (
      attempt >= 2 ||
      (method !== 'GET' && method !== 'HEAD') ||
      new URL(path, 'http://localhost').pathname.endsWith('/next') ||
      response.status !== 502 ||
      response.headers.get('X-Issue-Desk-Relay-Stop') !== '1'
    )
      break;
    await response.body?.cancel();
  }
  // Preserve error status and response fields for the existing caller's policy.
  const value = (
    method === 'HEAD' || response.status === 204 ? null : await response.json()
  ) as T;
  return { response, value };
}
