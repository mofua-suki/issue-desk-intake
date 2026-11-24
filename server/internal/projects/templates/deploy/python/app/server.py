"""自動デプロイ（deploy/）で起動するアプリの最小の実装（issue-desk の受付で作成）。契約は docs/deploy.md。

起動時に受け取った反映版（40桁のコミットID）を GET /health で答え、POST /shutdown で終わる。127.0.0.1 だけで待ち受ける。
"""

import argparse
import json
import re
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

REVISION = re.compile(r"[0-9a-f]{40}")
HOST = "127.0.0.1"


def create_server(port, revision):
    if not isinstance(revision, str) or not REVISION.fullmatch(revision):
        raise ValueError("--revision には40桁のコミットIDを指定してください")

    class Handler(BaseHTTPRequestHandler):
        def reply(self, status, body):
            data = json.dumps(body).encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Cache-Control", "no-store")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def local(self):
            # 別の名前（DNSの付け替え）で届いた要求は断る。
            return self.headers.get("Host") == f"{HOST}:{self.server.server_address[1]}"

        def do_GET(self):
            if not self.local():
                return self.reply(403, {"error": "forbidden"})
            if self.path == "/health":
                return self.reply(200, {"status": "ok", "revision": revision})
            return self.reply(404, {"error": "not found"})

        def do_POST(self):
            if not self.local():
                return self.reply(403, {"error": "forbidden"})
            if self.path != "/shutdown":
                return self.reply(404, {"error": "not found"})
            # ブラウザーは確認なしにこのヘッダーを送れないので、開いたページからは止められない。
            if self.headers.get("X-App-Control") != "stop":
                return self.reply(403, {"error": "forbidden"})
            self.close_connection = True
            self.reply(202, {"status": "stopping"})
            threading.Thread(target=self.server.shutdown, daemon=True).start()
            return None

        def log_message(self, format, *args):
            pass

    return ThreadingHTTPServer((HOST, port), Handler)


def main(argv=None):
    parser = argparse.ArgumentParser(description="自動デプロイで起動するアプリ")
    parser.add_argument("--port", type=int, required=True)
    parser.add_argument("--revision", required=True)
    args = parser.parse_args(argv)
    if not 1 <= args.port <= 65535:
        parser.error("--port には1〜65535のポート番号を指定してください")
    try:
        server = create_server(args.port, args.revision)
    except ValueError as e:
        parser.error(str(e))
    try:
        server.serve_forever()
    finally:
        server.server_close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
