// NASだけに配置する一時キー接続。HTTP用チャットbundleには含めない。
import fs from 'node:fs';
import { spawnSync } from 'node:child_process';
import {
  deskContractHeader,
  deskContractVersion,
} from '../../scripts/lib/desk-contract.generated.ts';
import {
  connectionFailed,
  fetchDesk,
  retryableDeskCall,
} from '../../scripts/lib/desk-retry.ts';
import { checkContractResponse } from '../../scripts/lib/desk-contract-client.ts';

const file = process.env.ISSUE_DESK_CONFIG || '/state/config.local.json';
const application = process.env.ISSUE_DESK_APPLICATION_DIR || '/app';
const binary = process.env.ISSUE_DESK_INTAKE_BINARY || '/usr/local/bin/intake';
// adminの固定の保存先診断だけを通す。任意のstderrには接続の秘密が含まれ得る。
const storeErrors = new Set([
  '受付の保存先の確認値が不正です。NASの一時キー窓口を更新してください',
  '受付のPostgreSQLを開けません',
  '窓口が確認した受付の保存先がPostgreSQLではありません。受付はPostgreSQL専用です。NASの受付と一時キー窓口を更新してください',
  '保存先のPostgreSQLを指定してください。ISSUE_DESK_DATABASE_URL（またはPGHOST・PGUSER・PGDATABASE）が必要です',
  '切替の完了を確認できません',
  'ISSUE_DESK_APP_STOREにsqliteは使えません。保存先はPostgreSQLだけです（postgresまたは未設定にしてください）',
]);
function admin(action, input = '') {
  const result = spawnSync(
    binary,
    [
      'admin',
      '--config',
      file,
      '--application-dir',
      application,
      'cli-key',
      action,
    ],
    {
      input,
      encoding: 'utf8',
      maxBuffer: 4096,
      timeout: 15000,
      stdio: ['pipe', 'pipe', 'pipe'],
    },
  );
  if (result.error || result.status !== 0) {
    const stderr = result.stderr?.trim();
    // cmd/intakeはslogのerror属性として返す。値全体が固定診断と一致した場合だけ採用する。
    const diagnostic = [...storeErrors].find(
      (message) =>
        stderr === message ||
        stderr?.endsWith(` error=${JSON.stringify(message)}`) ||
        stderr?.endsWith(` error=${message}`),
    );
    if (!result.error && diagnostic)
      throw Object.assign(Error(diagnostic), { localStore: true });
    throw Error('NASの受付の一時キーを準備・回収できません。');
  }
  return result.stdout.trim();
}
globalThis.issueDeskLocal = () => {
  const config = JSON.parse(fs.readFileSync(file, 'utf8'));
  if (config.workerId !== 'nas' || config.serverUrl !== undefined)
    throw Error('NAS用の受付設定が必要です。');
  const port = config.intake?.port || 8787;
  if (!Number.isInteger(port) || port < 1 || port > 65535)
    throw Error('NASの受付ポートが不正です。');
  const origin = `http://127.0.0.1:${port}`;
  const handoff = ['read', 'notice'].includes(
    process.env.ISSUE_DESK_NAS_HANDOFF,
  );
  const call = async (method, route, payload) => {
    if (
      typeof route !== 'string' ||
      !route.startsWith('/api/') ||
      route.includes('\\') ||
      route.includes('\r') ||
      route.includes('\n')
    )
      throw Error('受付APIの経路が不正です。');
    const send = async (url, init) => {
      if (new URL(url).origin !== origin)
        throw Error('一時キーを外部へ送りません。');
      let key;
      try {
        key = admin('issue');
      } catch (error) {
        if (
          handoff &&
          method === 'GET' &&
          retryableDeskCall(method, route, payload)
        )
          error.nasHandoff = true;
        throw error;
      }
      if (!/^[A-Za-z0-9_-]{43}$/.test(key))
        throw Error('一時キーを準備できません。');
      try {
        return await fetch(url, {
          ...init,
          redirect: 'manual',
          headers: { ...init.headers, 'x-desk-key': key },
        });
      } finally {
        // 回収失敗で受信済みの応答や元の通信例外を上書きしない。キーは短時間で失効する。
        try {
          admin('revoke', key);
        } catch {}
      }
    };
    let response;
    try {
      response = await fetchDesk(
        origin + route,
        {
          method,
          redirect: 'manual',
          headers: {
            Origin: origin,
            [deskContractHeader]: deskContractVersion,
            ...(payload ? { 'Content-Type': 'application/json' } : {}),
          },
          body: payload ? JSON.stringify(payload) : undefined,
        },
        retryableDeskCall(method, route, payload),
        { send },
      );
    } catch (error) {
      if (error?.localStore || error?.nasHandoff) throw error;
      throw Object.assign(
        Error(
          'NASの受付へ接続できません。一時キーと稼働状態を確認してください。',
        ),
        {
          unreachable: true,
          nasHandoff:
            handoff &&
            method === 'GET' &&
            retryableDeskCall(method, route, payload) &&
            connectionFailed(error),
        },
      );
    }
    const value = await response.json().catch(() => ({}));
    try {
      checkContractResponse(response, value);
    } catch (error) {
      if (
        method === 'GET' &&
        retryableDeskCall(method, route, payload) &&
        response.status === 503 &&
        (!value.code || value.code === 'TEMPORARY_UNAVAILABLE') &&
        handoff
      )
        error.nasHandoff = true;
      throw error;
    }
    return value;
  };
  return { call, close() {}, config, remote: false };
};
