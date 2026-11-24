// 自動デプロイで起動するアプリ（src/server.ts）の雛形のテスト（issue-desk の受付で作成）。契約は docs/deploy.md。
import test from 'node:test';
import assert from 'node:assert/strict';
import { once } from 'node:events';
import type { AddressInfo } from 'node:net';
import type { TestContext } from 'node:test';
import { createServer } from '../src/server.ts';

const revision = 'a'.repeat(40);

async function started(t: TestContext, onShutdown?: () => void) {
  const server = createServer({ revision, onShutdown });
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  t.after(() => server.close());
  return `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
}

test('GET /health answers the revision the app was started with', async (t) => {
  const url = await started(t);
  const res = await fetch(`${url}/health`);
  assert.equal(res.status, 200);
  assert.deepEqual(await res.json(), { status: 'ok', revision });
});

test('the app does not start without a 40-digit commit id', () => {
  for (const bad of [undefined, '', 'abc', 'A'.repeat(40), `${revision}\n`])
    assert.throws(() => createServer({ revision: bad }), /40桁/);
});

test('POST /shutdown needs the control header and then stops the app', async (t) => {
  let stopped: () => void;
  const done = new Promise<void>((resolve) => (stopped = resolve));
  const url = await started(t, () => stopped());
  assert.equal(
    (await fetch(`${url}/shutdown`, { method: 'POST' })).status,
    403,
  );
  const res = await fetch(`${url}/shutdown`, {
    method: 'POST',
    headers: { 'x-app-control': 'stop' },
  });
  assert.equal(res.status, 202);
  assert.deepEqual(await res.json(), { status: 'stopping' });
  await done;
});
