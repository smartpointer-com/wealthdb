"""The entry points build and answer without acting.

`--help` evaluates every argparse default, so it catches a typo in one
that an import alone would not. The wrapper is run with its data root
pointed at a temp dir: help and an unknown verb must both end before
anything is resolved or created.
"""
import subprocess
import sys
from pathlib import Path

import pytest

HERE = Path(__file__).resolve().parent.parent
WRAPPER = HERE / "plaid"


@pytest.mark.parametrize("script,flag", [
    ("login.py", "--check"), ("download.py", "--lookback"),
    ("prune.py", "--min-age-hours")])
def test_each_entry_point_builds_its_help(script, flag):
    result = subprocess.run([sys.executable, str(HERE / script), "--help"],
                            capture_output=True, text=True)
    assert result.returncode == 0, result.stderr
    assert flag in result.stdout


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
        for verb in ("login --item NAME", "download", "prune"):
            assert verb in result.stdout
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


@pytest.mark.parametrize("verb", ["login", "download", "prune"])
def test_the_silver_db_flag_is_refused_where_nothing_reads_it(tmp_path,
                                                               verb):
    result = _wrapper(tmp_path, verb, "--silver-db", str(tmp_path / "x.db"))
    assert result.returncode == 2
    assert "--silver-db does not apply" in result.stderr


def test_prune_runs_over_the_data_dir(tmp_path):
    # Local files only, so the wrapper can be run for real here.
    tree = tmp_path / "data" / "plaid" / "bank" / "20260101T000000Z"
    tree.mkdir(parents=True)
    (tree / "run.json").write_text('{"status": "complete", "item": "bank"}')
    result = _wrapper(tmp_path, "prune", "--dry-run")
    assert result.returncode == 0, result.stderr
    assert "== bank" in result.stdout and "nothing to prune" in result.stdout


def test_download_runs_over_the_data_dir(tmp_path):
    # With no Item stored, download ends before it asks Plaid anything.
    (tmp_path / "secrets").mkdir(mode=0o700)
    result = _wrapper(tmp_path, "download", "--sandbox")
    assert result.returncode == 1, result.stderr
    assert "no sandbox Item is linked" in result.stderr
    assert (tmp_path / "data" / "plaid").is_dir()
    assert not any((tmp_path / "data" / "plaid").iterdir())
