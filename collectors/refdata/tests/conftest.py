"""Put the collector's own modules on the path, as the wrapper runs them,
and give the tests a fake provider in the registry."""

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from providers import REGISTRY, sec_calendar  # noqa: E402
from tests.helpers import FakeProvider  # noqa: E402


@pytest.fixture(autouse=True)
def _reads(monkeypatch):
    """Each test counts the EDGAR filings it reads from zero: READS lives
    as long as the process, and every load logs sec-edgar's note."""
    monkeypatch.setattr(sec_calendar, "READS", sec_calendar.Reads())


@pytest.fixture
def fake(monkeypatch):
    """A FakeProvider named `fake` in the one registry download and load
    share."""
    p = FakeProvider()
    monkeypatch.setitem(REGISTRY, "fake", p)
    return p
