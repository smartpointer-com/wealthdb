"""Regression guard: each entrypoint's argparse parser must build cleanly.

Running `<script> --help` evaluates every `add_argument(default=...)`
expression before argparse prints and exits, so this catches a NameError
or typo in a default that `py_compile` and import-only checks miss. No
network, no side effects: `--help` exits before any request.
"""
import subprocess
import sys
from pathlib import Path

import pytest

_COLLECTOR_DIR = Path(__file__).resolve().parent.parent


@pytest.mark.parametrize("script", ["download.py", "load.py", "prune.py"])
def test_entrypoint_help_builds(script):
    result = subprocess.run(
        [sys.executable, str(_COLLECTOR_DIR / script), "--help"],
        capture_output=True, text=True,
    )
    assert result.returncode == 0, (
        f"`{script} --help` exited {result.returncode}; the argparse "
        f"parser failed to build. stderr:\n{result.stderr}"
    )
