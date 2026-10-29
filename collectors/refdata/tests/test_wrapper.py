"""The refdata wrapper: FINRA's credential reaches only the verbs that
ask FINRA. Run on a copy of the wrapper whose interpreter is a stand-in
that prints what it sees; the credential is invented."""

import os
import shutil
import subprocess
from pathlib import Path

import pytest

HERE = Path(__file__).resolve().parents[1]
ROOT = HERE.parents[1]


@pytest.fixture
def wrapper(tmp_path):
    """A copy of the wrapper and the shared host libs, with a stand-in
    interpreter, a secrets dir that holds finra.env, and a clean
    environment."""
    home = tmp_path / "collectors" / "refdata"
    (home / ".venv" / "bin").mkdir(parents=True)
    (tmp_path / "shared" / "wrappers").mkdir(parents=True)
    shutil.copy(HERE / "refdata", home / "refdata")
    for lib in ("host-lib.sh", "envfile-lib.sh"):
        shutil.copy(ROOT / "shared" / "wrappers" / lib, tmp_path / "shared" / "wrappers" / lib)
    python = home / ".venv" / "bin" / "python"
    python.write_text('#!/bin/sh\n'
                      'echo "$(basename "$1") id=${FINRA_CLIENT_ID-unset} secret=${FINRA_CLIENT_SECRET-unset}"\n')
    python.chmod(0o755)
    secrets = tmp_path / "secrets"
    secrets.mkdir()
    (secrets / "finra.env").write_text("FINRA_CLIENT_ID=example-id\nFINRA_CLIENT_SECRET=example-secret\n")
    env = {k: v for k, v in os.environ.items()
           if not k.startswith(("FINRA_", "REFDATA_", "WEALTHDB_")) and k != "XDG_DATA_HOME"}
    env["XDG_DATA_HOME"] = str(tmp_path / "xdg")

    def run(*args):
        out = subprocess.run(["bash", str(home / "refdata"), *args, "--secrets-dir", str(secrets),
                              "--data-dir", str(tmp_path / "data")], env=env, capture_output=True, text=True,
                             check=True)
        return out.stdout.strip()
    return run


@pytest.mark.parametrize("verb,sees", [
    (("load",), "load.py id=unset secret=unset"),
    (("prune",), "prune.py id=unset secret=unset"),
    (("download", "--dry-run"), "download.py id=example-id secret=example-secret"),
    (("login", "--check"), "download.py id=example-id secret=example-secret"),
])
def test_finras_credential_reaches_only_the_verbs_that_ask_finra(wrapper, verb, sees):
    assert wrapper(*verb) == sees
