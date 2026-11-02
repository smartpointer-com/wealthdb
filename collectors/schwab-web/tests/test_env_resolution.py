"""maybe_source_env_files' wiring: a named env file is the file sourced,
with the credential keys overriding the environment. The candidate search
and the exit on a missing named file are
collectorkit.envfile.source_first_env_file's, tested there. Synthetic
fixtures only."""
import argparse
import sys
from pathlib import Path

from collectorkit import envfile

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent))
import login  # noqa: E402


def _args(env_file):
    return argparse.Namespace(env_file=env_file)


def _record_loads(monkeypatch):
    loaded: list[tuple] = []
    monkeypatch.setattr(
        envfile, "load_env_file",
        lambda p, override_vars, *, logger, warn_on_override=False:
            loaded.append((Path(p), tuple(override_vars), warn_on_override)))
    return loaded


def test_explicit_existing_loads_it(tmp_path, monkeypatch):
    f = tmp_path / "creds.env"
    f.write_text("K=v\n")
    loaded = _record_loads(monkeypatch)
    login.maybe_source_env_files(_args(f))
    assert loaded == [(f, tuple(login._CRED_OVERRIDE_VARS), True)]
