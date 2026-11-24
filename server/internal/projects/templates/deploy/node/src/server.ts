// 自動デプロイ（deploy/）で起動するアプリの最小の実装（issue-desk の受付で作成）。契約は docs/deploy.md。
// 起動時に受け取った反映版（40桁のコミットID）を GET /health で答え、POST /shutdown で終わる。127.0.0.1 だけで待ち受ける。
import http from 'node:http';
import type { AddressInfo } from 'node:net';
import path from 'node:path';
import { parseArgs } from 'node:util';
import { fileURLToPath } from 'node:url';

const revisionPattern = /^[0-9a-f]{40}$/;

export function createServer({
  revision,
  onShutdown = () => {},
}: {
  revision: unknown;
  onShutdown?: () => void;
}) {
  if (typeof revision !== 'string' || !revisionPattern.test(revision))
    throw Error('--revision には40桁のコミットIDを指定してください');
  const server = http.createServer((req, res) => {
    const reply = (
      status: number,
      body: unknown,
      headers: http.OutgoingHttpHeaders = {},
    ) => {
      res.writeHead(status, {
        'content-type': 'application/json; charset=utf-8',
        'cache-control': 'no-store',
        ...headers,
      });
      res.end(JSON.stringify(body));
    };
    // 別の名前（DNSの付け替え）で届いた要求は断る。
    if (
      req.headers.host !== `127.0.0.1:${(server.address() as AddressInfo).port}`
    )
      return reply(403, { error: 'forbidden' });
    if (req.method === 'GET' && req.url === '/health')
      return reply(200, { status: 'ok', revision });
    if (req.method === 'POST' && req.url === '/shutdown') {
      // ブラウザーは確認なしにこのヘッダーを送れないので、開いたページからは止められない。
      if (req.headers['x-app-control'] !== 'stop')
        return reply(403, { error: 'forbidden' });
      res.on('finish', () => {
        server.close(() => onShutdown());
        server.closeIdleConnections();
      });
      return reply(202, { status: 'stopping' }, { connection: 'close' });
    }
    return reply(404, { error: 'not found' });
  });
  return server;
}

if (
  process.argv[1] &&
  path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  const { values } = parseArgs({
    options: { port: { type: 'string' }, revision: { type: 'string' } },
  });
  const port = Number(values.port);
  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    console.error('--port には1〜65535のポート番号を指定してください');
    process.exit(2);
  }
  let server: http.Server;
  try {
    server = createServer({
      revision: values.revision,
      onShutdown: () => process.exit(0),
    });
  } catch (e) {
    console.error(e.message);
    process.exit(2);
  }
  server.listen(port, '127.0.0.1');
}
