"""Every test gets a cache root of its own, so no parse or extraction a
load caches reaches another test."""
from __future__ import annotations

import pytest


@pytest.fixture(autouse=True)
def _own_cache_root(tmp_path, monkeypatch):
    monkeypatch.setenv("XDG_CACHE_HOME", str(tmp_path / "cache"))
