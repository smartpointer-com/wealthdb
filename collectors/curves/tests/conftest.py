"""Shared test setup for curves.

Puts the collector dir on `sys.path` so the tests can `import download`,
`load` and `prune`, and provides a fake of both portals that the
download suites hand to a stubbed urlopen. The answers are invented, in
each portal's real layout.
"""
from __future__ import annotations

import io
import json
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import download  # noqa: E402

ECB_HEADER = ("KEY,FREQ,REF_AREA,CURRENCY,PROVIDER_FM,INSTRUMENT_FM,"
              "PROVIDER_FM_ID,DATA_TYPE_FM,TIME_PERIOD,OBS_VALUE")


def ecb_csv(rows) -> str:
    """The ECB's CSV answer for [(series id, date, value), ...]: CRLF
    line ends, as the portal writes them."""
    lines = [ECB_HEADER]
    for sid, day, value in rows:
        item = sid.rsplit(".", 1)[1]
        lines.append(f"{sid},B,U2,EUR,4F,G_N_A,SV_C_YM,{item},{day},{value}")
    return "\r\n".join(lines) + "\r\n"


def snb_csv(rows, published: str = "2026-01-06 08:30",
            cube: str = "SNB1A@SNB.NSS.KZS.EID") -> str:
    """The SNB's CSV answer for [(maturity item, date, value), ...] as
    the portal writes it: a byte-order mark, the cube (named with its
    `@`) and its publishing date, a blank line, then the table, with CRLF
    line ends and an empty value unquoted."""
    lines = [f'\ufeff"CubeId";"{cube}"', f'"PublishingDate";"{published}"',
             "", '"Date";"LAUFZEIT";"ZEITPUNKT";"frequency";'
             '"AGGREGATIONSMETHODE";"Value"']
    for item, day, value in rows:
        cell = f'"{value}"' if value else ""
        lines.append(f'"{day}";"{item}";"A1100";"P1D_L";"ZZ";{cell}')
    return "\r\n".join(lines) + "\r\n"


class FakeResponse:
    """The slice of http.client.HTTPResponse fetch touches."""

    def __init__(self, body: bytes, status: int = 200, headers=None):
        self._body = body
        self.status = status
        self.headers = headers or {"Content-Type": "text/csv"}

    def read(self) -> bytes:
        return self._body

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


class FakePortals:
    """A urlopen stand-in for both portals. The ECB answers every series
    of its curve it is asked for with two invented days; the SNB states
    `published` as its latest update and answers every maturity with two
    days. A path other than the real curve's or cube's answers 404, as
    the portals do. A portal in `failing` answers HTTP `code`; one in
    `bodies` answers that body instead. Every request URL is recorded."""

    DAYS = ("2026-01-05", "2026-01-06")
    ECB_PATH = "/service/data/YC/B.U2.EUR.4F.G_N_A.SV_C_YM."
    SNB_PATH = "/api/warehouse/cube/SNB1A.SNB.NSS.KZS.EID/"

    def __init__(self, failing=(), code=500, published="20260106_0830",
                 bodies=None):
        self.failing = set(failing)
        self.code = code
        self.published = published
        self.bodies = bodies or {}
        self.urls: list[str] = []

    def requested(self, host: str) -> list[urllib.parse.SplitResult]:
        return [u for u in map(urllib.parse.urlsplit, self.urls)
                if u.netloc == host]

    def _error(self, url: str, code: int):
        return urllib.error.HTTPError(url, code, "synthetic", None,
                                      io.BytesIO(b"synthetic"))

    def __call__(self, req, timeout=None):
        self.urls.append(req.full_url)
        url = urllib.parse.urlsplit(req.full_url)
        portal = "ecb" if url.netloc == "data-api.ecb.europa.eu" else "snb"
        if portal in self.failing:
            raise self._error(req.full_url, self.code)
        if portal == "ecb":
            if not url.path.startswith(self.ECB_PATH):
                raise self._error(req.full_url, 404)
            if "ecb" in self.bodies:
                return FakeResponse(self.bodies["ecb"])
            items = url.path[len(self.ECB_PATH):].split("+")
            prefix = f"{download.ECB_FLOW}.{download.ECB_KEY}"
            return FakeResponse(ecb_csv(
                (f"{prefix}.{item}", day, "2.5")
                for item in items for day in self.DAYS).encode())
        if not url.path.startswith(self.SNB_PATH):
            raise self._error(req.full_url, 404)
        if url.path.endswith("/lastUpdate"):
            return FakeResponse(json.dumps({
                "editionDate": self.published,
                "publicSinceDate": self.published}).encode())
        if "snb" in self.bodies:
            return FakeResponse(self.bodies["snb"])
        dims = urllib.parse.parse_qs(url.query)["dimSel"][0]
        items = dims[len("LAUFZEIT("):dims.index(")")].split(",")
        return FakeResponse(snb_csv(
            (item, day, "0.5") for item in items for day in self.DAYS
        ).encode())


@pytest.fixture
def portals(monkeypatch) -> FakePortals:
    fake = FakePortals()
    monkeypatch.setattr(urllib.request, "urlopen", fake)
    return fake
