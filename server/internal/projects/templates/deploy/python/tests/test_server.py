"""自動デプロイで起動するアプリ（app/server.py）の雛形のテスト（issue-desk の受付で作成）。契約は docs/deploy.md。"""

import json
import threading
import urllib.error
import urllib.request

import pytest

from app.server import create_server

REVISION = "a" * 40
# 環境のプロキシ設定を使わずに 127.0.0.1 へ直接つなぐ。
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


@pytest.fixture
def app():
    server = create_server(0, REVISION)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    yield f"http://127.0.0.1:{server.server_address[1]}", thread
    server.shutdown()
    server.server_close()


def request(url, method="GET", headers=None):
    req = urllib.request.Request(url, method=method, headers=headers or {})
    try:
        with OPENER.open(req, timeout=10) as res:
            return res.status, json.loads(res.read())
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read())


def test_health_answers_the_revision_the_app_was_started_with(app):
    url, _ = app
    assert request(f"{url}/health") == (200, {"status": "ok", "revision": REVISION})


@pytest.mark.parametrize("bad", [None, "", "abc", "A" * 40, REVISION + "\n"])
def test_the_app_does_not_start_without_a_40_digit_commit_id(bad):
    with pytest.raises(ValueError, match="40桁"):
        create_server(0, bad)


def test_shutdown_needs_the_control_header_and_then_stops_the_app(app):
    url, thread = app
    assert request(f"{url}/shutdown", "POST")[0] == 403
    assert request(f"{url}/shutdown", "POST", {"X-App-Control": "stop"}) == (202, {"status": "stopping"})
    thread.join(timeout=10)
    assert not thread.is_alive()
