import { test } from 'node:test';
import assert from 'node:assert/strict';
import { deskGet, runCommand, parseArgs } from '../scripts/desk-get.ts';
import { forbiddenHint } from '../scripts/desk-alerts.ts';
import { checkContractResponse } from '../scripts/lib/desk-contract-client.ts';

test('GET と安全な経路以外は接続前に拒否する', async () => {
  let opened = false;
  for (const args of [
    [],
    ['POST', '/api/x'],
    ['/api/x'],
    ['GET', 'https://x/api/x'],
    ['GET', '/api/../x'],
    ['GET', '/api/%2e%2e/x'],
    ['GET', '/api//x'],
    ['GET', '/api/%2f%2fx'],
    ['GET', '/api/x\\y'],
  ]) {
    await assert.rejects(
      runCommand(args, {
        configured: () => null,
        open: async () => {
          opened = true;
        },
      }),
    );
  }
  assert.equal(opened, false);
  assert.equal(
    parseArgs(['GET', '/api/drafts?limit=1']).route,
    '/api/drafts?limit=1',
  );
});

test('応答と失敗に資格情報を出さない', async () => {
  const lines = [];
  assert.equal(
    await deskGet(
      async () => ({
        token: 'idk_secret',
        Authorization: 'Bearer idk_secret',
        text: 'Bearer idk_secret',
        ok: 1,
      }),
      { route: '/api/x' },
      (x) => lines.push(x),
    ),
    0,
  );
  assert.equal(
    await deskGet(
      async () => {
        throw Object.assign(Error('Authorization: Bearer idk_secret'), {
          status: 403,
        });
      },
      { route: '/api/x' },
      (x) => lines.push(x),
    ),
    1,
  );
  assert.doesNotMatch(lines.join('\n'), /idk_secret|Authorization/);
  assert.match(lines[0], /"ok": 1/);
  assert.ok(lines[1].includes(forbiddenHint));
  assert.match(lines[1], /一時キー/);
});

test('上限超過は本文を切り詰めて出さず大きさと続き方を案内する', async () => {
  const lines = [];
  const result = await deskGet(
    async () => ({ content: 'abcdef' }),
    { route: '/api/x', maxBytes: 3 },
    (x) => lines.push(x),
  );
  assert.equal(result, 1);
  assert.doesNotMatch(lines[0], /abcdef/);
  assert.match(lines[0], /bytes.*limit・page.*--max-bytes/);
});

test('統計のトークン数を残し資格情報だけを隠す', async () => {
  const lines = [];
  const data = {
    inputTokens: 12,
    uncachedInputTokens: 8,
    tokens: { total: 20 },
    nested: [{ token: 'opaque', csrfToken: 'opaque', value: 'idk_secret' }],
    secret: 'opaque',
  };
  assert.equal(
    await deskGet(
      async () => data,
      { route: '/api/stats/overview' },
      (x) => lines.push(x),
    ),
    0,
  );
  assert.deepEqual(JSON.parse(lines[0]), {
    inputTokens: 12,
    uncachedInputTokens: 8,
    tokens: { total: 20 },
    nested: [{ value: '[非表示]' }],
  });
});

test('共通接続のエラーと対処を状態に関係なく安全に表示する', async () => {
  for (const status of [401, 403, 404, 500]) {
    const lines = [];
    const call = async () =>
      checkContractResponse(
        { ok: false, status, headers: { get: () => null } },
        {
          error: '読取に失敗 Authorization: Bearer idk_secret',
          recovery: { action: 'report', guidance: '設定を確認 idh_secret' },
        },
        () => {},
      );
    assert.equal(
      await deskGet(call, { route: '/api/x' }, (x) => lines.push(x)),
      1,
    );
    assert.match(lines[0], new RegExp(`/api/x: 状態コード ${status}`));
    assert.match(lines[0], /読取に失敗/);
    assert.match(lines[0], /設定を確認/);
    assert.doesNotMatch(lines[0], /Authorization|idk_secret|idh_secret/);
    if (status === 403) assert.ok(lines[0].includes(forbiddenHint));
  }
});

test('NAS交代の印を持つ読取失敗は秘匿した案内を出して専用終了する', async () => {
  const lines = [];
  const error = Object.assign(Error('接続できません'), { nasHandoff: true });
  const code = await deskGet(
    async () => {
      throw error;
    },
    { route: '/api/issues' },
    (line) => lines.push(line),
  );
  assert.equal(code, 75);
  assert.match(lines.join('\n'), /接続できません/);
});
