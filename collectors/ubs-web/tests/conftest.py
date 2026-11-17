"""Pytest configuration for ubs-web.

Puts the collector dir on `sys.path` so test modules can `import
pdf_parsers`, `load`, etc. without an installable package layout, and
gives every test a cache root of its own, so a load's caches never
carry a result from one test into another, and never reach the real
cache that `WEALTHDB_CACHE_DIR` names inside a wrapper shell.
"""
from __future__ import annotations
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))


@pytest.fixture(autouse=True)
def _own_cache_root(tmp_path_factory, monkeypatch):
    monkeypatch.delenv("WEALTHDB_CACHE_DIR", raising=False)
    monkeypatch.setenv("XDG_CACHE_HOME", str(tmp_path_factory.mktemp("cache")))
