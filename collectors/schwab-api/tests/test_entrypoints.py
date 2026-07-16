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


@pytest.mark.parametrize("script",
                         ["download.py", "load.py", "prune.py",
                          "recompress.py"])
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


def test_login_entrypoint_does_not_hardcode_tracing():
    """Regression: the login entrypoint must not inject --trace /
    --screenshot-dir. Playwright's tracing.start() crashes the base
    image's Firefox, so a hardcoded --trace killed every login;
    diagnostics stay opt-in, passed through the wrapper (fleet convention).
    """
    ep = _COLLECTOR_DIR / "entrypoint.sh"
    exec_lines = [ln for ln in ep.read_text().splitlines()
                  if "exec python3 /app/login.py" in ln]
    assert exec_lines, "no login exec line found in entrypoint.sh"
    for ln in exec_lines:
        assert "--trace" not in ln, f"entrypoint hardcodes --trace: {ln!r}"
        assert "--screenshot-dir" not in ln, \
            f"entrypoint hardcodes --screenshot-dir: {ln!r}"
