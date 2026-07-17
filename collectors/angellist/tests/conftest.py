"""Keep the tests off the real bronze tree.

The test container bind-mounts the host's real bronze tree at /data —
load.py's default `--bronze-dir`, from which the silver-DB and
documents-dir defaults derive. A `load.main([...])` missing its path
flags would therefore ingest live runs and documents at /data and write
its silver there.

Redirecting the module default to a fresh empty tmp dir makes the
isolation structural — defense-in-depth on top of the derivation: a load
with no path flags scans an empty tree, ingests nothing, and leaves its
silver in the tmp dir, never under /data. Tests that scope
`--bronze-dir` / `--silver-db` themselves are unaffected.
"""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import load  # noqa: E402


@pytest.fixture(autouse=True)
def _bronze_dir_never_defaults_to_real_data(tmp_path_factory, monkeypatch):
    empty = tmp_path_factory.mktemp("no-bronze")
    monkeypatch.setattr(load, "DEFAULT_BRONZE_DIR", empty)
