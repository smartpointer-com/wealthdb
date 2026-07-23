"""Coverage for envfile.resolve_env_file / load_env — the
first-existing-candidate resolver adopted by viac / schwab-web /
fidelity-web. Synthetic fixtures only."""
import os
from pathlib import Path

from collectorkit import envfile


def test_arg_path_wins_unconditionally(tmp_path):
    # An explicit path is returned as-is even if it does not exist; the
    # caller is responsible for any existence check.
    f = tmp_path / "explicit.env"
    assert envfile.resolve_env_file(f, [tmp_path / "a", tmp_path / "b"]) == f


def test_first_existing_candidate(tmp_path):
    c1, c2 = tmp_path / "a.env", tmp_path / "b.env"
    c2.write_text("K=v\n")
    assert envfile.resolve_env_file(None, [c1, c2]) == c2


def test_first_wins_when_both_exist(tmp_path):
    c1, c2 = tmp_path / "a.env", tmp_path / "b.env"
    c1.write_text("K=1\n")
    c2.write_text("K=2\n")
    assert envfile.resolve_env_file(None, [c1, c2]) == c1


def test_directory_candidate_skipped(tmp_path):
    # is_file(), not exists(): a directory is never a sourceable env file.
    d = tmp_path / "dir.env"
    d.mkdir()
    f = tmp_path / "real.env"
    f.write_text("K=v\n")
    assert envfile.resolve_env_file(None, [d, f]) == f


def test_none_when_no_candidate(tmp_path):
    assert envfile.resolve_env_file(None, [tmp_path / "a", tmp_path / "b"]) is None


def test_load_env_sources_first_existing(tmp_path, monkeypatch):
    monkeypatch.delenv("P9_SENTINEL", raising=False)
    c1, c2 = tmp_path / "a.env", tmp_path / "b.env"
    c2.write_text("P9_SENTINEL=xyz\n")
    assert envfile.load_env(None, [c1, c2]) == c2
    assert os.environ.get("P9_SENTINEL") == "xyz"


def test_load_env_none_when_absent(tmp_path):
    assert envfile.load_env(None, [tmp_path / "a", tmp_path / "b"]) is None
