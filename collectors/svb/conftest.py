"""Every test gets a cache root of its own, so no parse a load caches
reaches another test or the host's cache. `WEALTHDB_CACHE_DIR` names the
root itself and wins over `XDG_CACHE_HOME`, so the test sets it."""
from __future__ import annotations

import pytest


@pytest.fixture(autouse=True)
def _own_cache_root(tmp_path, monkeypatch):
    monkeypatch.setenv("WEALTHDB_CACHE_DIR", str(tmp_path / "cache"))
