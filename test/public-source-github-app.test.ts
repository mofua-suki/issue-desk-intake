import { test } from 'node:test';
import assert from 'node:assert/strict';
import { generateKeyPairSync, verify } from 'node:crypto';
import * as fs from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const app = require('../scripts/issue-public-source-github-app.cjs');
const target = { id: 123, full_name: 'mofua-suki/issue-desk-intake' };
const perms = {
  contents: 'write',
  pull_requests: 'write',
  checks: 'read',
  statuses: 'read',
  administration: 'read',
};
const now = Date.UTC(2026, 0, 1);
const response = () => ({
  token: 'synthetic-token-secret',
  expires_at: new Date(now + 3600000).toISOString(),
  permissions: { ...perms, metadata: 'read' },
});

test('registered target and issuance response enforce scope, permissions and expiry', () => {
  assert.deepEqual(app.requestFor(target), {
    repository_ids: [123],
    permissions: perms,
  });
  assert.deepEqual(
    app.requestFor({ id: 456, full_name: 'mofua-suki/issue-desk-worker' })
      .repository_ids,
    [456],
  );
  for (const value of [
    null,
    { id: 0, full_name: target.full_name },
    { id: 1, full_name: 'other/repo' },
    { id: '123', full_name: target.full_name },
  ])
    assert.throws(() => app.requestFor(value));
  assert.equal(app.validateToken(response(), now), 'synthetic-token-secret');
  const withoutMetadata = response();
  delete withoutMetadata.permissions.metadata;
  assert.equal(
    app.validateToken(withoutMetadata, now),
    'synthetic-token-secret',
  );
  for (const invalid of [
    { ...response(), permissions: { ...perms, contents: 'read' } },
    { ...response(), permissions: { ...perms, workflows: 'write' } },
    { ...response(), permissions: { ...perms, metadata: 'write' } },
    { ...response(), permissions: { contents: 'write' } },
    ...[
      'bad',
      '2026-02-30T01:00:00Z',
      '2026-01-01T01:00:00+00:00',
      new Date(now).toISOString(),
      new Date(now + 3601000).toISOString(),
    ].map((expires_at) => ({ ...response(), expires_at })),
    { ...response(), token: '' },
  ])
    assert.throws(() => app.validateToken(invalid, now));
});

test('repository result must match exactly one registered target with no next page', () => {
  const result = { total_count: 1, repositories: [target] };
  assert.equal(app.validateRepositories(target, result, false), true);
  for (const [data, next] of [
    [null, false],
    [{ total_count: 0, repositories: [] }, false],
    [{ total_count: 1, repositories: [] }, false],
    [{ total_count: 2, repositories: [target, target] }, false],
    [{ total_count: 1, repositories: [{ ...target, id: 124 }] }, false],
    [
      { ...result, repositories: [{ ...target, full_name: 'other/repo' }] },
      false,
    ],
    [result, true],
    [result, undefined],
  ])
    assert.throws(() => app.validateRepositories(target, data, next));
});

test('JWT signature and lifetime are independently verified', () => {
  const { privateKey, publicKey } = generateKeyPairSync('rsa', {
    modulusLength: 2048,
  });
  const jwt = app.createJwt('123', privateKey, now);
  const [header, payload, signature] = jwt.split('.');
  assert.deepEqual(JSON.parse(Buffer.from(header, 'base64url').toString()), {
    alg: 'RS256',
    typ: 'JWT',
  });
  const claims = JSON.parse(Buffer.from(payload, 'base64url').toString());
  assert.deepEqual(claims, {
    iat: now / 1000 - 60,
    exp: now / 1000 + 300,
    iss: '123',
  });
  assert.ok(claims.exp - claims.iat <= 600);
  const data = Buffer.from(`${header}.${payload}`);
  const sig = Buffer.from(signature, 'base64url');
  assert.equal(verify('RSA-SHA256', data, publicKey, sig), true);
  assert.throws(
    () => app.createJwt(jwt, privateKey, now),
    (error) => !String(error).includes(jwt),
  );
  const ec = generateKeyPairSync('ec', { namedCurve: 'P-256' });
  assert.throws(() => app.createJwt('123', ec.privateKey, now));
});

test('private files reject access by others and links without exposing secrets in errors', () => {
  const dir = fs.mkdtempSync(join(tmpdir(), 'github-app-'));
  try {
    const config = join(dir, 'config');
    const key = join(dir, 'key');
    fs.writeFileSync(config, 'synthetic-config-secret', { mode: 0o600 });
    fs.writeFileSync(key, 'synthetic-key-secret', { mode: 0o600 });
    assert.equal(app.readPrivateFile(config), 'synthetic-config-secret');
    assert.equal(app.readPrivateFile(key), 'synthetic-key-secret');
    for (const path of [config, key]) {
      fs.chmodSync(path, 0o644);
      assert.throws(
        () => app.readPrivateFile(path),
        (error) => !String(error).includes('synthetic-'),
      );
      fs.chmodSync(path, 0o600);
      const symlink = `${path}-symlink`;
      fs.symlinkSync(path, symlink);
      assert.throws(() => app.readPrivateFile(symlink));
      const hardlink = `${path}-hardlink`;
      fs.linkSync(path, hardlink);
      assert.throws(() => app.readPrivateFile(path));
    }
    for (const fn of [
      () => app.createJwt('123', 'synthetic-key-secret', now),
      () =>
        app.validateToken(
          { ...response(), expires_at: 'synthetic-token-secret' },
          now,
        ),
      () => app.requestFor({ id: 1, full_name: 'synthetic-config-secret' }),
    ])
      assert.throws(fn, (error) => !String(error).includes('synthetic-'));
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test('issuance CLI passes only validated single-target results to stdin', () => {
  const dir = fs.mkdtempSync(join(process.cwd(), 'github-app-test-'));
  const keyDir = fs.mkdtempSync(join(tmpdir(), 'github-app-key-'));
  try {
    const { privateKey } = generateKeyPairSync('rsa', { modulusLength: 2048 });
    const key = privateKey.export({ type: 'pkcs8', format: 'pem' }).toString();
    const keyPath = join(keyDir, 'key');
    const configPath = join(dir, 'config');
    const mockPath = join(dir, 'mock.cjs');
    fs.writeFileSync(keyPath, key, { mode: 0o600 });
    for (const full_name of [
      target.full_name,
      'mofua-suki/issue-desk-worker',
    ]) {
      const registered = {
        id: 123,
        full_name,
        appId: '42',
        installationId: '43',
        credentialRef: 'fixture',
        keyPath,
      };
      fs.writeFileSync(configPath, JSON.stringify({ fixture: registered }), {
        mode: 0o600,
      });
      for (const scenario of [
        'ok',
        'missing',
        'wrong-id',
        'wrong-name',
        'multiple',
        'next',
        'permissions',
        'expiry',
        'expired',
        'malformed-expiry',
        'denied',
        'timeout',
      ]) {
        const mock = `
const assert = require('node:assert/strict');
let calls = 0;
let clock = ${now};
Date.now = () => clock;
global.fetch = async (url, options) => {
  calls++;
  assert.equal(options.redirect, 'error');
  if (calls === 1) {
    assert.equal(url, 'https://api.github.com/app/installations/43/access_tokens');
    assert.equal(options.method, 'POST');
    assert.match(options.headers.Authorization, /^Bearer [^.]+\\.[^.]+\\.[^.]+$/);
    assert.deepEqual(JSON.parse(options.body), ${JSON.stringify({ repository_ids: [123], permissions: perms })});
    if (${JSON.stringify(scenario)} === 'timeout') throw Error('synthetic timeout synthetic-token-secret');
    if (${JSON.stringify(scenario)} === 'denied') return { status: 403 };
    return { status: 201, json: async () => { clock += 2000; return { token: 'synthetic-token-secret', expires_at: ${JSON.stringify(scenario === 'malformed-expiry' ? 'invalid' : new Date(now + 2000 + (scenario === 'expiry' ? 3600001 : scenario === 'expired' ? -1 : 3600000)).toISOString())}, permissions: ${JSON.stringify(scenario === 'permissions' ? { ...perms, workflows: 'write' } : { ...perms, metadata: 'read' })} }; } };
  }
  assert.equal(calls, 2);
  assert.equal(url, 'https://api.github.com/installation/repositories?per_page=2');
  assert.equal(options.method, 'GET');
  assert.equal(options.headers.Authorization, 'Bearer synthetic-token-secret');
  return { status: 200, headers: { get: () => ${JSON.stringify(scenario === 'next' ? '<https://api.github.com/x>; rel="next"' : null)} }, json: async () => (${JSON.stringify(scenario === 'missing' ? { total_count: 0, repositories: [] } : scenario === 'multiple' ? { total_count: 2, repositories: [registered, registered] } : { total_count: 1, repositories: [{ id: scenario === 'wrong-id' ? 124 : 123, full_name: scenario === 'wrong-name' ? 'other/repo' : full_name }] })}) };
};`;
        fs.writeFileSync(mockPath, mock);
        const result = spawnSync(
          process.execPath,
          [
            '--require',
            mockPath,
            'scripts/issue-public-source-github-app.cjs',
            configPath,
            'fixture',
          ],
          { encoding: 'utf8' },
        );
        const sink = spawnSync(
          process.execPath,
          [
            '-e',
            "let s='';process.stdin.on('data',x=>s+=x);process.stdin.on('end',()=>{const x=JSON.parse(s);if(!x.token||!x.metadata)process.exit(1)})",
          ],
          { input: result.stdout, encoding: 'utf8' },
        );
        if (scenario === 'ok') {
          assert.equal(result.status, 0, result.stderr);
          assert.equal(sink.status, 0);
          const value = JSON.parse(result.stdout);
          assert.equal(value.token, 'synthetic-token-secret');
          assert.equal(value.metadata.repository, full_name);
          assert.equal(value.metadata.credentialRef, 'fixture');
          assert.equal(
            value.metadata.expiresAt,
            new Date(now + 3602000).toISOString(),
          );
          assert.deepEqual(value.metadata.permissions, {
            ...perms,
            metadata: 'read',
          });
        } else {
          assert.notEqual(result.status, 0, scenario);
          assert.equal(result.stdout, '', scenario);
          assert.notEqual(sink.status, 0, scenario);
        }
        const keyBody = key.replace(/-----[^-]+-----|\s/g, '');
        const checkOutput = (content: string, allowToken = false) => {
          if (!allowToken)
            assert.ok(!content.includes('synthetic-token-secret'));
          assert.ok(!content.replace(/\s/g, '').includes(keyBody));
          assert.doesNotMatch(
            content,
            /-----BEGIN (?:RSA )?PRIVATE KEY-----|Bearer\s+\S+/i,
          );
        };
        checkOutput(result.stdout, scenario === 'ok');
        checkOutput(result.stderr);
        if (scenario === 'ok') {
          assert.equal(
            result.stdout.split('synthetic-token-secret').length - 1,
            1,
          );
        }
        const scan = (path: string) => {
          for (const entry of fs.readdirSync(path, { withFileTypes: true })) {
            const child = join(path, entry.name);
            if (child === mockPath) continue;
            if (entry.isDirectory()) scan(child);
            else if (entry.isFile())
              checkOutput(fs.readFileSync(child, 'utf8'));
          }
        };
        scan(dir);
      }
    }
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
    fs.rmSync(keyDir, { recursive: true, force: true });
  }
});
