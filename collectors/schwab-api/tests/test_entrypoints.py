"""Regression guard: each entrypoint's argparse parser must build cleanly.

Running `<script> --help` evaluates every `add_argument(default=...)`
expression before argparse prints and exits, so this catches NameError /
typos in defaults — e.g. a `cli.default_data_root()` default where `cli`
was never imported — that `py_compile` and import-only checks miss
(argparse defaults are evaluated at runtime, when the parser is built).
No network / no side effects: `--help` exits before any login / download
/ SFTP work.
"""
import subprocess
import sys
from pathlib import Path

import pytest

_COLLECTOR_DIR = Path(__file__).resolve().parent.parent


@pytest.mark.parametrize("script", ["download.py", "load.py", "prune.py"])
def test_entrypoint_help_builds(script):
    path = _COLLECTOR_DIR / script
    if not path.exists():
        pytest.skip(f"{script} not present for this collector")
    result = subprocess.run(
        [sys.executable, str(path), "--help"],
        capture_output=True,
        text=True,
    )
    assert result.returncode == 0, (
        f"`{script} --help` exited {result.returncode}; the argparse "
        f"parser failed to build. stderr:\n{result.stderr}"
    )
