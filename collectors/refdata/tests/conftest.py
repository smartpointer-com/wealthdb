"""Put the collector's own modules on the path, as the wrapper runs them,
give the tests a fake provider in the registry, and parse each folder a
provider parses a second time, compressed."""

import shutil
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from collectorkit import compress  # noqa: E402
from providers import REGISTRY, sec_calendar  # noqa: E402
from providers.base import META, TEXT_SUFFIXES  # noqa: E402
from providers.inbox import Inbox  # noqa: E402
from tests.helpers import FakeProvider  # noqa: E402


@pytest.fixture(autouse=True)
def _reads(monkeypatch):
    """Each test counts the EDGAR filings it reads from zero: READS lives
    as long as the process, and every load logs sec-edgar's note."""
    monkeypatch.setattr(sec_calendar, "READS", sec_calendar.Reads())


def stored_compressed(f: Path) -> bool:
    """Whether download stores a file compressed: a text file but
    meta.json, or a filing's archive."""
    return f.is_file() and f.name != META and (f.suffix in TEXT_SUFFIXES or f.suffix == ".zip")


@pytest.fixture(autouse=True)
def _compressed_twin(monkeypatch, tmp_path_factory):
    """Each provider's parse, wherever a test calls it, parses a copy of
    the folder too, its text files and archives compressed as download
    and compact store them, and must give the same rows, or fail the
    same way: every reader takes either form of a file. The inbox's
    copies stay as placed."""
    owners = {next(c for c in type(p).__mro__ if "parse" in vars(c)) for p in REGISTRY.values()
              if not isinstance(p, Inbox | FakeProvider)}

    def outcome(parse, provider, indir):
        try:
            return parse(provider, indir), None
        except Exception as e:  # noqa: BLE001 - compared below, then re-raised
            return None, e

    for owner in owners:
        def parse(self, indir, _parse=owner.parse):
            out, error = outcome(_parse, self, indir)
            if any(stored_compressed(f) for f in indir.rglob("*")):
                twin = tmp_path_factory.mktemp("compressed") / indir.name
                shutil.copytree(indir, twin)
                for f in twin.rglob("*"):
                    if stored_compressed(f):
                        compress.compress_file(f)
                with monkeypatch.context() as m:
                    m.setattr(sec_calendar, "READS", sec_calendar.Reads())
                    got, failed = outcome(_parse, self, twin)
                assert got == out and type(failed) is type(error), f"{indir} parses otherwise compressed"
            if error is not None:
                raise error
            return out
        monkeypatch.setattr(owner, "parse", parse)


@pytest.fixture
def fake(monkeypatch):
    """A FakeProvider named `fake` in the one registry download and load
    share."""
    p = FakeProvider()
    monkeypatch.setitem(REGISTRY, "fake", p)
    return p
