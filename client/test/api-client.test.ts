import test from 'node:test';
import assert from 'node:assert/strict';
import { requestJson } from '../src/api/client.ts';

test('read requests use the browser session without adding CSRF or a body', async () => {
  let request: RequestInit | undefined;
  const result = await requestJson<{ authenticated: boolean }>('/api/session', {
    fetch: async (url, options) => {
      assert.equal(url, '/api/session');
      request = options;
      return Response.json({ authenticated: true });
    },
  });
  assert.equal(result.value.authenticated, true);
  assert.equal(request?.method, 'GET');
  assert.equal(request?.credentials, 'same-origin');
  assert.equal(request?.body, undefined);
  assert.deepEqual(request?.headers, {});
});

test('mutations carry JSON and the current session CSRF token', async () => {
  let request: RequestInit | undefined;
  await requestJson('/api/comment', {
    data: { text: '返信', id: 'same-request-id' },
    csrf: 'session-csrf',
    fetch: async (_url, options) => {
      request = options;
      return Response.json({ accepted: true });
    },
  });
  assert.equal(request?.method, 'POST');
  assert.deepEqual(request?.headers, {
    'Content-Type': 'application/json',
    'X-CSRF-Token': 'session-csrf',
  });
  assert.deepEqual(JSON.parse(String(request?.body)), {
    text: '返信',
    id: 'same-request-id',
  });
  assert.equal(request?.redirect, 'error');
});

test('HTTP errors retain their status and structured refusal for caller policy', async () => {
  const result = await requestJson('/api/comment', {
    data: {},
    fetch: async () =>
      Response.json(
        { error: '権限がありません', code: 'FORBIDDEN', retryable: false },
        { status: 403 },
      ),
  });
  assert.equal(result.response.status, 403);
  assert.deepEqual(result.value, {
    error: '権限がありません',
    code: 'FORBIDDEN',
    retryable: false,
  });
});

test('network errors are returned to the caller without automatically resending a mutation', async () => {
  let calls = 0;
  await assert.rejects(
    requestJson('/api/comment', {
      data: {},
      fetch: async () => {
        calls++;
        throw Error('connection interrupted');
      },
    }),
    /connection interrupted/,
  );
  assert.equal(calls, 1);
});

test('API credentials are not sent to external or malformed destinations', async () => {
  let calls = 0;
  for (const path of [
    'https://other.test/api/comment',
    '//other.test/api/comment',
    '/api/\\other',
    '/api/comment\n',
  ]) {
    await assert.rejects(
      requestJson(path, {
        data: {},
        csrf: 'secret',
        fetch: async () => {
          calls++;
          return Response.json({});
        },
      }),
      /接続先/,
    );
  }
  assert.equal(calls, 0);
});

test('abort signals reach fetch and empty successful responses are supported', async () => {
  const controller = new AbortController();
  const result = await requestJson('/api/example', {
    method: 'DELETE',
    signal: controller.signal,
    fetch: async (_url, options) => {
      assert.equal(options?.signal, controller.signal);
      return new Response(null, { status: 204 });
    },
  });
  assert.equal(result.value, null);
});

test('stopped reads retry finitely and preserve the final result', async () => {
  for (const method of ['GET', 'HEAD']) {
    let calls = 0;
    const result = await requestJson('/api/session', {
      method,
      fetch: async () => {
        calls++;
        return calls < 3
          ? Response.json(
              {},
              { status: 502, headers: { 'X-Issue-Desk-Relay-Stop': '1' } },
            )
          : method === 'HEAD'
            ? new Response(null)
            : Response.json({ ok: true });
      },
    });
    assert.equal(calls, 3);
    assert.equal(result.response.status, 200);
    assert.deepEqual(result.value, method === 'HEAD' ? null : { ok: true });
  }
  let calls = 0;
  const failed = await requestJson('/api/session', {
    fetch: async () => {
      calls++;
      return Response.json(
        { error: 'stopped' },
        { status: 502, headers: { 'X-Issue-Desk-Relay-Stop': '1' } },
      );
    },
  });
  assert.equal(calls, 3);
  assert.equal(failed.response.status, 502);
  assert.deepEqual(failed.value, { error: 'stopped' });
});

test('other failures, mutations and next are not retried', async () => {
  for (const [method, path, marker] of [
    ['GET', '/api/session', false],
    ['POST', '/api/comment', true],
    ['GET', '/api/jobs/next', true],
  ] as const) {
    let calls = 0;
    const result = await requestJson(path, {
      method,
      fetch: async () => {
        calls++;
        return Response.json(
          { error: 'failed' },
          {
            status: 502,
            headers: marker ? { 'X-Issue-Desk-Relay-Stop': '1' } : {},
          },
        );
      },
    });
    assert.equal(calls, 1);
    assert.equal(result.response.status, 502);
  }
  let calls = 0;
  await assert.rejects(
    requestJson('/api/session', {
      fetch: async () => {
        if (++calls === 1)
          return Response.json(
            {},
            { status: 502, headers: { 'X-Issue-Desk-Relay-Stop': '1' } },
          );
        throw new Error('retry failed');
      },
    }),
    /retry failed/,
  );
  assert.equal(calls, 2);
});

test('aborting after a stopped response prevents another request', async () => {
  const controller = new AbortController();
  let calls = 0;
  await assert.rejects(
    requestJson('/api/session', {
      signal: controller.signal,
      fetch: async () => {
        calls++;
        controller.abort();
        return Response.json(
          {},
          { status: 502, headers: { 'X-Issue-Desk-Relay-Stop': '1' } },
        );
      },
    }),
    { name: 'AbortError' },
  );
  assert.equal(calls, 1);
});
