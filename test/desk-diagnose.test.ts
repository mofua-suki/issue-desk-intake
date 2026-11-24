import { test } from 'node:test';
import assert from 'node:assert/strict';
import { diagnose, parseArgs, runCommand } from '../scripts/desk-diagnose.ts';

const sample = (color, instanceId, queries, count = 3) => ({
  color,
  instanceId,
  startedAt: '2026-01-01T00:00:00Z',
  pid: 42,
  queries,
  connections: [{ state: 'active', count }],
  maxConnections: 100,
});
const query = (
  seconds,
  state = 'active',
  waitType = null,
  text = 'SELECT ?',
) => ({ state, seconds, waitType, ...(text === null ? {} : { query: text }) });

test('実際の応答形式で形ごとの集計と接続数の推移を表示する', async () => {
  const responses = [
    sample('blue', 'a', [
      query(2),
      query(4, 'active', 'Lock'),
      query(3, 'unknown', null, null),
    ]),
    sample('blue', 'a', [query(6)], 5),
  ];
  const calls = [],
    waits = [],
    output = [];
  const code = await diagnose(
    async (...args) => {
      calls.push(args);
      return responses.shift();
    },
    { samples: 2, interval: 0.5 },
    {
      sleep: async (ms) => waits.push(ms),
      log: (s) => output.push(JSON.parse(s)),
    },
  );
  assert.equal(code, 0);
  assert.deepEqual(calls, [
    ['GET', '/api/diagnostics/db'],
    ['GET', '/api/diagnostics/db'],
  ]);
  assert.deepEqual(waits, [500]);
  const result = output[0],
    row = result.instances[0].queries[0];
  assert.deepEqual(row, {
    query: 'SELECT ?',
    count: 3,
    totalSeconds: 12,
    maxSeconds: 6,
    states: { active: 3 },
    waitTypes: { なし: 2, Lock: 1 },
    averageSeconds: 4,
  });
  assert.equal(result.instances[0].queries[1].query, null);
  assert.deepEqual(
    result.samples.map((s) => s.connections[0].count),
    [3, 5],
  );
  assert.equal(result.samples[0].maxConnections, 100);
});

test('失敗を挟む交代と復帰を検出し、インスタンス別に分離する', async () => {
  const responses = [
    sample('green', 'a', [query(1)]),
    Error('secret-token'),
    sample('blue', 'b', [query(2)]),
    sample('green', 'a', [query(3)]),
  ];
  let result;
  const code = await diagnose(
    async () => {
      const v = responses.shift();
      if (v instanceof Error) throw v;
      return v;
    },
    { samples: 4, interval: 0 },
    {
      sleep: async () => {},
      log: (s) => {
        assert.ok(!s.includes('secret-token'));
        result = JSON.parse(s);
      },
    },
  );
  assert.equal(code, 0);
  assert.equal(result.failedSamples, 1);
  assert.equal(result.instanceChanges, 2);
  assert.deepEqual(
    result.samples.map((s) => [s.sample, s.color, s.instanceId]),
    [
      [1, 'green', 'a'],
      [3, 'blue', 'b'],
      [4, 'green', 'a'],
    ],
  );
  assert.deepEqual(
    result.instances.map((g) => [g.instanceId, g.queries[0].totalSeconds]),
    [
      ['a', 4],
      ['b', 2],
    ],
  );
  assert.deepEqual(
    result.failures.map((f) => f.sample),
    [2],
  );
});

test('全件失敗と不完全な応答は失敗として数え、非ゼロで終わる', async () => {
  let result;
  const code = await diagnose(
    async () => ({}),
    { samples: 2, interval: 0 },
    {
      sleep: async () => {},
      log: (s) => {
        result = JSON.parse(s);
      },
    },
  );
  assert.equal(code, 1);
  assert.equal(result.failedSamples, 2);
  assert.deepEqual(result.instances, []);
});

test('接続引数と任意診断をGETで渡し、接続を閉じる', async () => {
  const calls = [],
    output = [];
  let closed = false,
    opened;
  const code = await runCommand(
    [
      '--url',
      'https://example.test',
      '--token-file',
      '/tmp/token',
      '--samples',
      '1',
      '--cpu',
      '1',
      '--goroutines',
    ],
    {
      env: {},
      open: async (conn) => {
        opened = conn;
        return {
          call: async (method, route) => {
            calls.push([method, route]);
            return route.endsWith('/db')
              ? sample('blue', 'a', [])
              : { color: 'green', instanceId: 'b', functions: [] };
          },
          close: () => {
            closed = true;
          },
        };
      },
      log: (s) => output.push(JSON.parse(s)),
    },
  );
  assert.equal(code, 0);
  assert.equal(opened.base, 'https://example.test');
  assert.equal(opened.tokenFile, '/tmp/token');
  assert.equal(closed, true);
  assert.deepEqual(
    calls.map((c) => c[1]),
    [
      '/api/diagnostics/db',
      '/api/diagnostics/cpu?seconds=1',
      '/api/diagnostics/goroutines',
    ],
  );
  assert.ok(calls.every((c) => c[0] === 'GET'));
  assert.equal(output[0].profiles[0].result.instanceId, 'b');
});

test('引数の既定値と範囲を検査する', () => {
  assert.deepEqual(parseArgs([]), {
    samples: 5,
    interval: 2,
    cpu: null,
    goroutines: false,
    activity: false,
  });
  for (const args of [
    ['--samples', '0'],
    ['--interval', '-1'],
    ['--cpu', '31'],
    ['--samples', '2', '--samples', '3'],
  ])
    assert.throws(() => parseArgs(args));
});

test('activityを一度取得し、不正な応答は本文を出さず失敗とする', async () => {
  for (const good of [true, false]) {
    const calls = [];
    let result;
    const activity = good
      ? {
          color: 'blue',
          instanceId: 'a',
          startedAt: '2026-01-01T00:00:00Z',
          hours: [
            {
              start: '2026-01-01T00:00:00Z',
              rows: [
                {
                  kind: 'http',
                  name: '/api/issues/{n}',
                  method: 'GET',
                  status: '2xx',
                  credential: 'pc',
                  count: 1,
                  failures: 0,
                  totalMilliseconds: 1.5,
                  maxMilliseconds: 1.5,
                },
              ],
            },
          ],
        }
      : { color: 'blue', instanceId: 'a', hours: 'secret' };
    const code = await diagnose(
      async (method, route) => {
        calls.push([method, route]);
        return route.endsWith('/db') ? sample('blue', 'a', []) : activity;
      },
      { samples: 1, interval: 0, activity: true },
      {
        log: (text) => {
          assert.ok(!text.includes('secret'));
          result = JSON.parse(text);
        },
      },
    );
    assert.deepEqual(calls, [
      ['GET', '/api/diagnostics/db'],
      ['GET', '/api/diagnostics/activity'],
    ]);
    assert.equal(code, good ? 0 : 1);
    assert.equal(result.failures.length, good ? 0 : 1);
    if (good)
      assert.deepEqual(result.profiles, [
        { type: 'activity', result: activity },
      ]);
  }
  assert.equal(parseArgs(['--activity']).activity, true);
});
