// K2 の3項目による受付のトークン接続。管理設定・資格情報の中身は読まない。
import fs from 'node:fs';

// The address of the intake. Only the existing Serve HTTPS URL is meant here; plain http is accepted for
// this PC's own loopback only. The value is never echoed back, in case it carries a password.
export function remoteBase(value, { internal = false } = {}) {
  let u;
  try {
    u = new URL(value);
  } catch {
    throw Error(
      '接続先URLを確認してください（例: https://issue-desk.example.ts.net/）。',
    );
  }
  if (u.username || u.password)
    throw Error(
      '接続先URLに資格情報を含めないでください。トークンは --token-file で渡します。',
    );
  const loopback = ['127.0.0.1', 'localhost', '[::1]'].includes(u.hostname);
  if (
    u.protocol !== 'https:' &&
    !(
      u.protocol === 'http:' &&
      (loopback ||
        (internal && u.hostname === 'issue-desk-intake' && u.port === '8787'))
    )
  )
    throw Error(
      '接続先は受付のHTTPSのURLを指定してください（例: https://issue-desk.example.ts.net/）。',
    );
  if (u.search || u.hash)
    throw Error('接続先URLに ? や # を含めないでください。');
  return u.origin + u.pathname.replace(/\/+$/, '');
}

// The file name of the default token file of the setting (the same value as shared/src/intake-remote.mjs).
export const remoteTokenName = 'intake-client-token';

// A file URL as a local path without node:url, and the path helpers needed without node:path.
// Paths in config.local.json are relative to its folder.
function localPath(url) {
  const p = decodeURIComponent(url.pathname);
  if (url.host) return `//${url.host}${p}`;
  return p;
}
const absolutePath = (p) => p.startsWith('/');
const folderOf = (p) => p.slice(0, p.lastIndexOf('/')) || '.';
const joinPath = (dir, p) =>
  absolutePath(p) ? p : `${dir.replace(/\/+$/, '')}/${p}`;
// config.local.json of this checkout (beside scripts/).
export const configFile = () =>
  localPath(new URL('../config.local.json', import.meta.url));

// K2 の3項目からトークン接続を選ぶ。受付用の設定には接続先が無く、一時キー方式を使う。
// 移せる旧設定は、元のバイト列を退避してから K2 に置き換える。
export function configuredRemote(
  file = configFile(),
  {
    now = () => new Date(),
    warn = (line) => process.stderr.write(`${line}\n`),
  } = {},
) {
  let text;
  try {
    text = fs.readFileSync(file);
  } catch {
    return null;
  }
  let c;
  try {
    c = JSON.parse(text.toString('utf8').replace(/^\uFEFF/, ''));
  } catch {
    throw Error(
      `設定ファイルを読めません（${file}）。JSONの形式を確認してください。`,
    );
  }
  // client は K2 の3項目。
  if (Object.hasOwn(c ?? {}, 'serverUrl')) {
    if (
      Object.keys(c).length !== 3 ||
      typeof c.serverTokenFile !== 'string' ||
      !c.serverTokenFile.trim() ||
      typeof c.workerId !== 'string' ||
      !c.workerId.trim()
    )
      throw Error(
        'client の設定は serverUrl・serverTokenFile・workerId の3項目にしてください',
      );
    return {
      base: remoteBase(c.serverUrl, { internal: true }),
      tokenFile: joinPath(folderOf(file), c.serverTokenFile.trim()),
      hint: '',
      configured: true,
    };
  }
  if (
    c?.intake?.remoteUrl !== undefined ||
    c?.intake?.remoteTokenFile !== undefined
  ) {
    migrateK2(file, c, text, now, warn);
    return configuredRemote(file);
  }
  return null;
}

// temporary-code: {"id":"chat-config-k2-2895","until":"全PCのチャット作業ツリーの config.local.json が K2 の3項目になり、旧形式の写しを戻す予定が無くなった"}
function migrateK2(file, c, original, now, warn) {
  const fail = (field) =>
    Error(
      `intake.remoteUrl・remoteTokenFile は廃止しました。${field} を確認してください。--url・--token-file を付けて呼ぶことで接続でき、このことを利用者に伝えてください。`,
    );
  if (typeof c.workerId !== 'string' || !c.workerId.trim())
    throw fail('workerId');
  try {
    if (
      typeof c.intake.remoteUrl !== 'string' ||
      new URL(c.intake.remoteUrl).protocol !== 'https:'
    )
      throw Error();
    remoteBase(c.intake.remoteUrl);
  } catch {
    throw fail('intake.remoteUrl');
  }
  const token =
    c.intake.remoteTokenFile ??
    (typeof c.tokenFile === 'string'
      ? joinPath(folderOf(c.tokenFile), remoteTokenName)
      : `private/${remoteTokenName}`);
  try {
    if (
      typeof token !== 'string' ||
      !token.trim() ||
      !fs.statSync(joinPath(folderOf(file), token.trim())).isFile()
    )
      throw Error();
  } catch {
    throw fail('intake.remoteTokenFile');
  }
  const next = {
    serverUrl: c.intake.remoteUrl,
    serverTokenFile: token.trim(),
    workerId: c.workerId.trim(),
  };
  let temporary;
  try {
    let backup = `${file}.before-k2`;
    const timestamp = new Date(now()).getTime();
    for (let offset = 0; ; offset++) {
      try {
        fs.writeFileSync(backup, original, { flag: 'wx', mode: 0o600 });
        break;
      } catch (error) {
        if (error.code !== 'EEXIST') throw error;
        backup = joinPath(
          folderOf(file),
          `config.local.${new Date(timestamp + offset).toISOString().replace(/[:.]/g, '-')}.before-k2`,
        );
      }
    }
    temporary = fs.mkdtempSync(`${file}.k2-`);
    const replacement = `${temporary}/config.json`;
    fs.writeFileSync(replacement, `${JSON.stringify(next, null, 2)}\n`, {
      flag: 'wx',
      mode: 0o600,
    });
    fs.renameSync(replacement, file);
  } catch {
    throw fail('config.local.json の退避・置き換え');
  } finally {
    if (temporary) fs.rmSync(temporary, { recursive: true, force: true });
  }
  warn('config.local.json の旧設定を退避し、K2 の3項目へ移しました。');
}
