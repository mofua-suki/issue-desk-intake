/* global require, module, process, Buffer, fetch, AbortSignal */
/* eslint-disable @typescript-eslint/no-require-imports */
'use strict';
const fs = require('node:fs');
const crypto = require('node:crypto');
const permissions = Object.freeze({
  contents: 'write',
  pull_requests: 'write',
  checks: 'read',
  statuses: 'read',
  administration: 'read',
});
const repositories = new Set([
  'mofua-suki/issue-desk-intake',
  'mofua-suki/issue-desk-worker',
]);
const reject = () => {
  throw new Error('GitHub App validation failed');
};

function registration(value) {
  if (!value || !Number.isSafeInteger(value.id)) reject();
  if (value.id <= 0 || !repositories.has(value.full_name)) reject();
  return value;
}
function requestFor(value) {
  const target = registration(value);
  return { repository_ids: [target.id], permissions: { ...permissions } };
}
function validateToken(response, issuedAt) {
  if (!response || typeof response.token !== 'string') reject();
  if (!response.token.trim() || !Number.isFinite(issuedAt)) reject();
  const expiry = response.expires_at;
  if (
    typeof expiry !== 'string' ||
    !/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,3})?Z$/.test(expiry)
  )
    reject();
  const date = new Date(expiry);
  if (
    !Number.isFinite(date.getTime()) ||
    date.toISOString().replace('.000Z', 'Z') !==
      expiry
        .replace(/\.0{1,3}Z$/, 'Z')
        .replace(/\.(\d{1,2})Z$/, (_, n) => `.${n.padEnd(3, '0')}Z`)
  )
    reject();
  const duration = date.getTime() - issuedAt;
  if (duration < 3590000 || duration > 3600000) reject();
  const actual = response.permissions;
  if (!actual || typeof actual !== 'object' || Array.isArray(actual)) reject();
  const expected = { ...permissions, metadata: 'read' };
  if (
    Object.keys(actual).some((key) => actual[key] !== expected[key]) ||
    Object.entries(permissions).some(([key, value]) => actual[key] !== value)
  )
    reject();
  return response.token;
}
function validateRepositories(target, result, hasNextPage) {
  registration(target);
  if (hasNextPage !== false || !result || result.total_count !== 1) reject();
  const repos = result.repositories;
  if (!Array.isArray(repos) || repos.length !== 1) reject();
  if (repos[0]?.id !== target.id || repos[0]?.full_name !== target.full_name)
    reject();
  return true;
}
function readPrivateFile(path) {
  let fd;
  try {
    fd = fs.openSync(
      path,
      fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW | fs.constants.O_NONBLOCK,
    );
    const info = fs.fstatSync(fd);
    if (!info.isFile() || info.nlink !== 1) reject();
    if (info.uid !== process.getuid() || (info.mode & 0o077) !== 0) reject();
    return fs.readFileSync(fd, 'utf8');
  } catch {
    reject();
  } finally {
    if (fd !== undefined) fs.closeSync(fd);
  }
}
function createJwt(appId, key, now = Date.now()) {
  try {
    if (
      !(typeof appId === 'string' && /^[1-9]\d*$/.test(appId)) &&
      !(Number.isSafeInteger(appId) && appId > 0)
    )
      reject();
    if (!Number.isFinite(now)) reject();
    const privateKey =
      key instanceof crypto.KeyObject ? key : crypto.createPrivateKey(key);
    if (privateKey.type !== 'private' || privateKey.asymmetricKeyType !== 'rsa')
      reject();
    const seconds = Math.floor(now / 1000);
    const encode = (value) =>
      Buffer.from(JSON.stringify(value)).toString('base64url');
    const payload = `${encode({ alg: 'RS256', typ: 'JWT' })}.${encode({ iat: seconds - 60, exp: seconds + 300, iss: String(appId) })}`;
    return `${payload}.${crypto.sign('RSA-SHA256', Buffer.from(payload), privateKey).toString('base64url')}`;
  } catch {
    reject();
  }
}

async function run(args, fetcher = fetch, output = process.stdout) {
  try {
    if (args.length !== 2) reject();
    const config = JSON.parse(readPrivateFile(args[0]));
    const target = config[args[1]];
    const body = requestFor(target);
    if (
      !/^[1-9]\d*$/.test(String(target.installationId)) ||
      !Number.isSafeInteger(Number(target.installationId)) ||
      typeof target.credentialRef !== 'string' ||
      !/^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/.test(target.credentialRef) ||
      typeof target.keyPath !== 'string'
    )
      reject();
    const jwt = createJwt(target.appId, readPrivateFile(target.keyPath));
    const headers = {
      Accept: 'application/vnd.github+json',
      'X-GitHub-Api-Version': '2026-03-10',
    };
    const issued = await fetcher(
      `https://api.github.com/app/installations/${target.installationId}/access_tokens`,
      {
        method: 'POST',
        headers: {
          ...headers,
          Authorization: `Bearer ${jwt}`,
          'Content-Type': 'application/json',
        },
        body: JSON.stringify(body),
        redirect: 'error',
        signal: AbortSignal.timeout(10000),
      },
    );
    if (issued.status !== 201) reject();
    const data = await issued.json();
    const token = validateToken(data, Date.now());
    const listed = await fetcher(
      'https://api.github.com/installation/repositories?per_page=2',
      {
        method: 'GET',
        headers: { ...headers, Authorization: `Bearer ${token}` },
        redirect: 'error',
        signal: AbortSignal.timeout(10000),
      },
    );
    if (listed.status !== 200) reject();
    const link = listed.headers.get('link');
    validateRepositories(target, await listed.json(), Boolean(link));
    output.write(
      `${JSON.stringify({ token, metadata: { repository: target.full_name, credentialRef: target.credentialRef, permissions: data.permissions, expiresAt: data.expires_at } })}\n`,
    );
    return 0;
  } catch {
    return 1;
  }
}

module.exports = {
  requestFor,
  validateToken,
  validateRepositories,
  readPrivateFile,
  createJwt,
  run,
};
if (require.main === module) {
  run(process.argv.slice(2)).then((code) => {
    if (code) process.stderr.write('GitHub App issuance failed.\n');
    process.exitCode = code;
  });
}
