"""Shared test setup for fred.

Puts the collector dir on `sys.path` so the tests can `import download`,
`load` and `prune`, and provides the fake HTTP response the download
suites hand to a stubbed urlopen. Every test starts with no FRED_ENV_FILE
and no FRED_API_KEY in its environment, so a shell that exports either
never feeds a real env file or key into a test.
"""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))


@pytest.fixture(autouse=True)
def _no_ambient_credential(monkeypatch):
    # Set first, so the fixture also undoes what a sourced env file writes.
    for name in ("FRED_ENV_FILE", "FRED_API_KEY"):
        monkeypatch.setenv(name, "")
        monkeypatch.delenv(name)


class FakeResponse:
    """The slice of http.client.HTTPResponse fetch_observations touches."""

    def __init__(self, body: bytes, status: int = 200, headers=None):
        self._body = body
        self.status = status
        self.headers = headers or {"Content-Type": "application/json"}

    def read(self) -> bytes:
        return self._body

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False
