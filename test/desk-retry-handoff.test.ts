// 受付の入れ替え中の送り直し（scripts/lib/desk-retry.ts の fetchDesk）。
import assert from 'node:assert/strict';
import test from 'node:test';
import { fetchDesk } from '../scripts/lib/desk-retry.ts';

const json = (status, body, headers = {}) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json', ...headers },
  });
const init = { method: 'POST', headers: {}, body: '{"text":"a"}' };

function harness(responses) {
  const sent = [];
  const slept = [];
  const warned = [];
  return {
    sent,
    slept,
    warned,
    options: {
      send: async (_url, request) => {
        sent.push(request.body);
        return responses[Math.min(sent.length - 1, responses.length - 1)]();
      },
      sleep: async (ms) => void slept.push(ms),
      warn: (line) => void warned.push(line),
    },
  };
}

test('未受理の印が付いた503は、送信IDの無いPOSTも同じ本文で送り直す', async () => {
  const notAccepted = () =>
    json(
      503,
      { error: '受付を交代しています。' },
      { 'x-issue-desk-not-accepted': '1' },
    );
  const h = harness([notAccepted, notAccepted, () => json(200, { ok: true })]);
  const res = await fetchDesk(
    'http://desk/api/comment',
    init,
    false,
    h.options,
  );
  assert.equal(res.status, 200);
  assert.deepEqual(h.sent, [init.body, init.body, init.body]);
  assert.deepEqual(h.slept, [5000, 5000]);
  assert.match(h.warned[0], /受付の入れ替え中です/);
});

test('印の無い503は、送信IDの無いPOSTを送り直さない', async () => {
  const h = harness([() => json(503, { error: 'Serverは待機中です。' })]);
  const res = await fetchDesk(
    'http://desk/api/comment',
    init,
    false,
    h.options,
  );
  assert.equal(res.status, 503);
  assert.equal(h.sent.length, 1);
});

test('起票の停止（SERVER_DRAINING）は送信IDで照合できる経路だけ待って送り直す', async () => {
  const draining = () =>
    json(503, { error: '止めています。', code: 'SERVER_DRAINING' });
  const safe = harness([draining, () => json(200, { ok: true })]);
  assert.equal(
    (await fetchDesk('http://desk/api/issues', init, true, safe.options))
      .status,
    200,
  );
  assert.deepEqual(safe.slept, [5000]);
  const unsafe = harness([draining]);
  assert.equal(
    (await fetchDesk('http://desk/api/comment', init, false, unsafe.options))
      .status,
    503,
  );
  assert.equal(unsafe.sent.length, 1);
});

test('入れ替え待ちは60回で打ち切り、最後の503を返す', async () => {
  for (const safe of [false, true]) {
    const h = harness([
      () =>
        json(503, { error: '交代中' }, { 'x-issue-desk-not-accepted': '1' }),
    ]);
    const res = await fetchDesk(
      'http://desk/api/comment',
      init,
      safe,
      h.options,
    );
    assert.equal(res.status, 503);
    assert.equal(h.sent.length, 61, `safe=${safe}`);
  }
});

test('入れ替え待ちは最初の未受理から5分で打ち切る（受付が1回ごとに保留しても延びない）', async () => {
  for (const safe of [false, true]) {
    const h = harness([
      () =>
        json(503, { error: '交代中' }, { 'x-issue-desk-not-accepted': '1' }),
    ]);
    let clock = 0;
    const send = async (url, request) => {
      // 1回の試行に125秒かかる（受付の保留120秒＋待ち5秒）とする。
      clock += 125000;
      return h.options.send(url, request);
    };
    const res = await fetchDesk('http://desk/api/issues', init, safe, {
      ...h.options,
      send,
      now: () => clock,
    });
    assert.equal(res.status, 503);
    // 125秒・250秒・375秒の応答で送り直し、500秒の応答（最初の未受理から375秒）で返す。
    assert.equal(h.sent.length, 4, `safe=${safe}`);
  }
});
