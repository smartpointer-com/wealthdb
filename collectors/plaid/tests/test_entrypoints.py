"""The entry points build and answer without acting.

`--help` evaluates every argparse default, so it catches a typo in one
that an import alone would not. The wrapper is run with its data root
pointed at a temp dir: help and an unknown verb must both end before
anything is resolved or created.
"""
import subprocess
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent.parent
WRAPPER = HERE / "plaid"


def test_login_help_builds():
    result = subprocess.run([sys.executable, str(HERE / "login.py"), "--help"],
                            capture_output=True, text=True)
    assert result.returncode == 0, result.stderr
    assert "--item" in result.stdout and "--check" in result.stdout


def _wrapper(tmp_path, *argv):
    return subprocess.run(
        [str(WRAPPER), *argv], capture_output=True, text=True,
        env={"PATH": "/usr/bin:/bin", "HOME": str(tmp_path),
             "WEALTHDB_DATA_ROOT": str(tmp_path / "data"),
             "WEALTHDB_SECRETS_DIR": str(tmp_path / "secrets")})


def test_the_wrapper_prints_help_for_no_arguments(tmp_path):
    for argv in ((), ("help",), ("--help",), ("-h",)):
        result = _wrapper(tmp_path, *argv)
        assert result.returncode == 0, result.stderr
        assert "login --item NAME" in result.stdout
    assert not (tmp_path / "data").exists()


def test_the_wrapper_refuses_a_verb_it_does_not_have(tmp_path):
    result = _wrapper(tmp_path, "transfer")
    assert result.returncode == 2
    assert "unknown command 'transfer'" in result.stderr
    assert not (tmp_path / "data").exists()


def test_login_without_an_item_runs_nothing(tmp_path):
    # No arguments must never start an action: here that means no link.
    result = _wrapper(tmp_path, "login")
    assert result.returncode == 2
    assert "--item NAME is required" in result.stderr
    assert not (tmp_path / "secrets").exists()
