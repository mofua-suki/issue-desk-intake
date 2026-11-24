"""固定テストの環境を作る（issue-desk の setupCommand: {python} scripts/setup_test_env.py）。

作業ツリーの .venv に仮想環境を作り、pyproject.toml の test の依存（pytest）を入れる。
既に入っていれば何もしない。何度実行しても同じ結果になる。
"""

import subprocess
import sys
import venv
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
VENV = ROOT / ".venv"


def venv_python() -> Path:
    return VENV / "bin" / "python"


def main() -> int:
    python = venv_python()
    if not python.exists():
        venv.EnvBuilder(with_pip=True, clear=True).create(VENV)
    probe = [str(python), "-c", "import pytest"]
    if subprocess.run(probe, cwd=ROOT, check=False).returncode != 0:
        install = [str(python), "-m", "pip", "install", "pytest>=8"]
        subprocess.run(install, cwd=ROOT, check=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
