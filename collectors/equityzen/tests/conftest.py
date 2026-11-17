"""Shared test setup for equityzen: the loader's extraction cache lives
inside each test's own directory."""
from __future__ import annotations

import pytest


@pytest.fixture(autouse=True)
def cache_dir(tmp_path, monkeypatch):
    """The collector's cache root (collectorkit.extract.cache_root)."""
    path = tmp_path / "cache"
    monkeypatch.setenv("WEALTHDB_CACHE_DIR", str(path))
    return path
