"""maybe_source_env_files' wiring: the named env file, else the first of
the collector's candidates, is sourced, and its values win over the
environment. The rule itself is collectorkit.envfile.load's, tested
there. Synthetic fixtures only."""
import argparse
import os
import sys
from pathlib import Path

from collectorkit import envfile

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent))
import download  # noqa: E402


def _args(env_file):
    return argparse.Namespace(env_file=env_file)


def test_the_flag_and_the_candidates_reach_the_loader(tmp_path, monkeypatch):
    calls = []
    monkeypatch.setattr(
        envfile, "load",
        lambda explicit, **kwargs: calls.append((explicit, kwargs)))
    f = tmp_path / "creds.env"
    download.maybe_source_env_files(_args(f))
    assert calls == [(f, {"candidates": download.DEFAULT_ENV_FILE_CANDIDATES,
                          "logger": download.log})]


def test_a_named_file_wins_over_the_environment(tmp_path, monkeypatch):
    f = tmp_path / "creds.env"
    f.write_text(f"{download.USERNAME_ENVS[0]}=from-file\n")
    monkeypatch.setenv(download.USERNAME_ENVS[0], "exported")
    download.maybe_source_env_files(_args(f))
    assert os.environ[download.USERNAME_ENVS[0]] == "from-file"
