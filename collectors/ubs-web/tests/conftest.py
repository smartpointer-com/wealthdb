"""Pytest configuration for ubs-web.

Puts the repo root on `sys.path` so test modules can `import
pdf_parsers`, `load`, etc. without an installable package layout, and
gives every test a cache root of its own, so a load's parse cache never
carries a result from one test into another.
"""
from __future__ import annotations
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))


@pytest.fixture(autouse=True)
def _own_cache_root(tmp_path_factory, monkeypatch):
    monkeypatch.setenv("XDG_CACHE_HOME", str(tmp_path_factory.mktemp("cache")))
