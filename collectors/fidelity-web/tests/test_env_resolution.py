"""maybe_source_env_files now resolves the env file via
collectorkit.envfile.resolve_env_file (P9). These pin the resolution
behaviour: explicit-path existence guard, first-existing candidate, and
the directory-skip that comes with is_file(). Synthetic fixtures only."""
import argparse
import sys
from pathlib import Path

import pytest

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent))
import download  # noqa: E402


def _args(env_file):
    return argparse.Namespace(env_file=env_file)


def _record_loads(monkeypatch):
    loaded: list[Path] = []
    monkeypatch.setattr(download, "_load_env_file", lambda p: loaded.append(Path(p)))
    return loaded


def test_explicit_missing_raises(tmp_path):
    with pytest.raises(SystemExit):
        download.maybe_source_env_files(_args(tmp_path / "nope.env"))


def test_explicit_existing_loads_it(tmp_path, monkeypatch):
    f = tmp_path / "creds.env"
    f.write_text("K=v\n")
    loaded = _record_loads(monkeypatch)
    download.maybe_source_env_files(_args(f))
    assert loaded == [f]


def test_first_candidate_wins_when_both_exist(tmp_path, monkeypatch):
    c1, c2 = tmp_path / "a.env", tmp_path / "b.env"
    c1.write_text("K=1\n")
    c2.write_text("K=2\n")
    monkeypatch.setattr(download, "DEFAULT_ENV_FILE_CANDIDATES", (c1, c2))
    loaded = _record_loads(monkeypatch)
    download.maybe_source_env_files(_args(None))
    assert loaded == [c1]


def test_second_candidate_when_first_absent(tmp_path, monkeypatch):
    c1, c2 = tmp_path / "a.env", tmp_path / "b.env"
    c2.write_text("K=v\n")
    monkeypatch.setattr(download, "DEFAULT_ENV_FILE_CANDIDATES", (c1, c2))
    loaded = _record_loads(monkeypatch)
    download.maybe_source_env_files(_args(None))
    assert loaded == [c2]


def test_directory_candidate_skipped(tmp_path, monkeypatch):
    d = tmp_path / "dir.env"
    d.mkdir()
    f = tmp_path / "real.env"
    f.write_text("K=v\n")
    monkeypatch.setattr(download, "DEFAULT_ENV_FILE_CANDIDATES", (d, f))
    loaded = _record_loads(monkeypatch)
    download.maybe_source_env_files(_args(None))
    assert loaded == [f]


def test_no_candidate_is_noop(tmp_path, monkeypatch):
    monkeypatch.setattr(download, "DEFAULT_ENV_FILE_CANDIDATES",
                        (tmp_path / "a.env", tmp_path / "b.env"))
    loaded = _record_loads(monkeypatch)
    download.maybe_source_env_files(_args(None))
    assert loaded == []
