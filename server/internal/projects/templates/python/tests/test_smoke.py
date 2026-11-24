"""固定テストの雛形（issue-desk の受付で作成）。最初の機能を実装したら、その機能のテストをこのフォルダーへ加える。"""

import sys
import tomllib
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


def test_project_documents_and_manifest_are_in_place():
    readme = (ROOT / "README.md").read_text(encoding="utf-8")
    assert "## 設計書の目次" in readme
    manifest = tomllib.loads((ROOT / "pyproject.toml").read_text(encoding="utf-8"))
    assert manifest["project"]["requires-python"] == ">=3.12"


def test_runtime_is_python_3_12_or_later():
    assert sys.version_info >= (3, 12)
