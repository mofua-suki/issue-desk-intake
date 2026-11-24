import { test } from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { measure, summarize } from '../scripts/measure-project-list.ts';

const id = '11111111-1111-4111-8111-111111111111';
test(
  'measures concurrent list and project responses and rejects unsuccessful responses',
  { timeout: 10000 },
  async (t) => {
    let pending;
    let fail = false;
    const server = http.createServer((req, res) => {
      if (fail) {
        res.writeHead(401).end();
        return;
      }
      assert.equal(req.headers.cookie, 'session=test');
      if (!pending) pending = res;
      else {
        pending.end('[]');
        pending = undefined;
        res.end('{}');
      }
    });
    await new Promise<void>((resolve) =>
      server.listen(0, '127.0.0.1', resolve),
    );
    t.after(
      () =>
        new Promise((resolve) => {
          server.closeAllConnections();
          server.close(resolve);
        }),
    );
    const origin = `http://127.0.0.1:${(server.address() as import('node:net').AddressInfo).port}`;
    const results = await measure(origin, id, 2, 'session=test');
    assert.equal(results.length, 2);
    assert.ok(results.every((r) => r.listMs > 0 && r.projectMs > 0));
    assert.ok(
      summarize(results).projectMs.p95 >= summarize(results).projectMs.p50,
    );
    fail = true;
    await assert.rejects(measure(origin, id, 1), /HTTP 401/);
  },
);
