"""The calls `walk` sends to the provider, pinned as traces.

Each case drives `download.walk` against a recording page
(`collectorkit.testing`) that answers from a script, under a fake clock.
The trace it records (every page, locator, request and context call, and
every sleep, in order) must equal the stored one in `traces/<case>.trace`.
A restructuring of the walk that keeps its behaviour keeps these files
as they are; a trace that changes is a walk that sends something else.
To record new traces on purpose, run the tests with
`COLLECTORKIT_RECORD_TRACES=1` (`collectorkit.testing.RECORD_ENV`).

The scripts are synthetic: invented account ids, no provider text. A
script answers just enough for each phase to take its main path; the
trace pins what the walk does with those answers. One case per `--mode`,
the dry run, an enumeration that finds nothing, and a phase that raises.
"""

from __future__ import annotations

import json
import random
import sys
from datetime import date
from pathlib import Path

import pytest

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
sys.path.insert(0, str(COLLECTOR))

import download  # noqa: E402
from collectorkit import testing  # noqa: E402
from collectorkit.testing import Compute, Raises, Recorder, Script  # noqa: E402

TRACES = HERE / "traces"
TODAY = date(2026, 1, 15)
SLUG = "20260115T120000Z"

# Invented account ids: two 9-digit retail accounts and a 7-digit DAF.
RETAIL_A, RETAIL_B, DAF_NBR = "100000001", "200000002", "7000001"
DIMENSIONS = [
    {"account_id": RETAIL_A, "portfolio": "Portfolio A", "nickname": "A"},
    {"account_id": RETAIL_B, "portfolio": "Portfolio B", "nickname": "B"},
    {"account_id": DAF_NBR, "portfolio": "Charitable Giving",
     "nickname": "Giving"},
]
POSITIONS_BODY = {"accts": [RETAIL_A, RETAIL_B], "pico": None}


class _PositionsRequest:
    """The positions query the page sends on load; the lot step copies
    its body and headers."""

    method = "POST"
    url = f"{download.POSWEB_API}/positions"
    post_data = json.dumps(POSITIONS_BODY)
    headers = {"x-csrf-token": "csrf-synthetic"}

    def all_headers(self):
        return dict(self.headers)


def _goto(call):
    if call.arg == download.URL_POSITIONS:
        call.rec.emit("request", _PositionsRequest())


def base_script(dimensions=DIMENSIONS):
    s = Script(url=download.URL_PORTFOLIO_SUMMARY, download=b"a,b\n")
    s.on("evaluate", testing.js_contains("location.href"), Compute(lambda c: c.rec.url))
    s.on("evaluate", testing.js_contains("ap143528-accounts-selector-account-link"),
         dimensions)
    s.on("goto", None, Compute(_goto))
    # Positions: the preset select, the kebab and its Download item.
    for sel in (download.SEL_PRESET_VIEW_SELECT, download.SEL_KEBAB_MENU,
                "role=menuitem[name=/download/i]"):
        s.on("count", sel, 1)
    s.on("is_visible", "role=menuitem[name=/download/i]", True)
    # Lots: every in-page poswebex query answers with no rows.
    s.on("evaluate", testing.js_contains("credentials: \"include\""),
         {"status": 200, "text": json.dumps({"rowData": []})})
    # Activity: the picker opens on its Custom tab, both date inputs,
    # Apply, the Download trigger and its CSV item are there.
    for sel in (download.SEL_TIMEPICKER_OPEN,
                "input.fds-segment__radio[type='radio'][value='custom' i]",
                download.SEL_ACTIVITY_FROM_DATE, download.SEL_ACTIVITY_TO_DATE,
                download.SEL_ACTIVITY_APPLY, "button[aria-label='Download']",
                download.SEL_ACTIVITY_CSV_ITEM,
                download.SEL_ACTIVITY_CSV_SELECTORS[0]):
        s.on("count", sel, 1)
    s.on("evaluate", testing.js_contains("el.min"), {"min": None, "max": None})
    # Documents: both rail links are there, each type offers two
    # years.
    s.on("count", "a[href='statements']:has-text('Personal')", 1)
    s.on("count", "a[href='tax-forms']:has-text('Tax forms')", 1)
    s.on("eval_on_selector_all", "TimeFilter", ["2026", "2025"])
    # Balances: the total-value label has rendered.
    s.on("count", "-totalaccountvalue-label", 1)
    # DAF: the hop lands, the bootstrap names a party, the roster holds
    # one giving account.
    s.on("json", "identity/self", {"partyId": "party-1"})
    s.on("body", "/user/party-1/accounts",
         json.dumps([{"accountNbr": DAF_NBR}]).encode())
    return s


def run_case(tmp_path, monkeypatch, *, script, mode="all", dry_run=False):
    """Drive one walk; return its trace."""
    clock = testing.FakeClock().install(monkeypatch)
    testing.freeze_collector(monkeypatch, COLLECTOR, TODAY, SLUG)
    # Every lot pause is the short one, at the middle of its range.
    monkeypatch.setattr(random, "randrange", lambda n: 1)
    monkeypatch.setattr(random, "uniform", lambda a, b: (a + b) / 2)
    rec = Recorder(script, root=tmp_path, clock=clock)
    dest = tmp_path / "bronze"
    download.walk(rec.context, rec.page, {
        "dest": str(dest), "mode": mode,
        "dry_run": "true" if dry_run else "false",
        "since": "2025-11-01", "until": "2026-01-14",
    })
    return rec.trace


def raising_script():
    s = base_script()
    s.on("count", download.SEL_TIMEPICKER_OPEN,
         Raises(RuntimeError("synthetic failure")))
    return s


CASES = {
    "mode-all": dict(mode="all"),
    "mode-positions": dict(mode="positions"),
    "mode-activity": dict(mode="activity"),
    "mode-documents": dict(mode="documents"),
    "mode-balances": dict(mode="balances"),
    "mode-performance": dict(mode="performance"),
    "dry-run": dict(mode="all", dry_run=True),
    "empty-enumeration": dict(mode="all", script=lambda: base_script([])),
    "phase-raises": dict(mode="all", script=raising_script),
}


def _case(name):
    """The keyword arguments of `run_case` for one case, with a fresh
    script."""
    kw = dict(CASES[name])
    kw["script"] = kw.get("script", base_script)()
    return kw


@pytest.mark.parametrize("name", sorted(CASES))
def test_walk_sends_the_stored_trace(name, tmp_path, monkeypatch):
    trace = run_case(tmp_path, monkeypatch, **_case(name))
    testing.assert_trace(TRACES / f"{name}.trace", trace)
