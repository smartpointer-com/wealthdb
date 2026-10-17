"""maybe_source_env_files' explicit-path guard: a named env file that does
not exist stops the run, and one that does is the file sourced. The
candidate search is collectorkit.envfile.resolve_env_file, tested there.
Synthetic fixtures only."""
import argparse
import sys
from pathlib import Path

import pytest

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent))
import login  # noqa: E402


def _args(env_file):
    return argparse.Namespace(env_file=env_file)


def _record_loads(monkeypatch):
    loaded: list[Path] = []
    monkeypatch.setattr(login, "_load_env_file", lambda p: loaded.append(Path(p)))
    return loaded


def test_explicit_missing_raises(tmp_path):
    with pytest.raises(SystemExit):
        login.maybe_source_env_files(_args(tmp_path / "nope.env"))


def test_explicit_existing_loads_it(tmp_path, monkeypatch):
    f = tmp_path / "creds.env"
    f.write_text("K=v\n")
    loaded = _record_loads(monkeypatch)
    login.maybe_source_env_files(_args(f))
    assert loaded == [f]
