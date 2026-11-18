"""The calls `download.main` sends to the provider, pinned as traces.

Each case runs `download.main` against a recording browser
(`collectorkit.testing`) that answers from a script, under a fake clock.
The trace it records (the launch, every page, locator, frame, request
and context call, and every sleep, in order) must equal the stored one
in `traces/<case>.trace`. A restructuring of the walk that keeps its
behaviour keeps these files as they are; a trace that changes is a walk
that sends something else. To record new traces on purpose, run the
tests with `COLLECTORKIT_RECORD_TRACES=1` (`collectorkit.testing.RECORD_ENV`).

The scripts are synthetic: invented tokens and labels, no provider
text, and a placeholder user agent, so a Playwright bump that moves the
real one leaves the traces as they are. A script answers just enough
for each pass to take its main path; the trace pins what the walk does
with those answers. One case per `--only` pass and the whole walk, the
dry run, a homepage that lists no account, and a pass that raises. That
last case also leaves one dialog open, so the dismiss loop is pinned
too.
"""

from __future__ import annotations

import sys
import types
from datetime import date
from pathlib import Path

import pytest

import download
import landmarks as ubs
from collectorkit import cli, testing
from collectorkit.testing import Compute, Raises, Recorder, Script, Seq

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
TRACES = HERE / "traces"
TODAY = date(2026, 1, 15)
SLUG = "20260115T120000Z"
SPA = "https://ebanking-ch1.ubs.com/app/OQJ/1/ebanking/spa.html"
USER_AGENT = "ua-synthetic"
OPEN_DIALOGS = "dialog[open]').length"

ACCOUNT_ROWS = [
    {"href": "#/cash-account-transactions?target=x&accountId=tok-a",
     "label": "Account A"},
    {"href": "#/cash-account-transactions?target=x&accountId=tok-b",
     "label": "Account B"},
]
PORTFOLIO_HREFS = ["#/portfolio?portfolioUid=pf-1&bankingRelationId=rel-1"]
DOCUMENT_ROWS = [
    {"href": "/api/v1/digital-banking/files/doc1?apikey=k", "label": "Doc"},
]


class _ApiRequest:
    """One of the SPA's own API calls, carrying its apikey header."""

    url = "https://ebanking-ch1.ubs.com/api/cards/v1/list"
    headers = {"apikey": "key-synthetic"}


def _goto(call):
    # The entry lands on the SPA; the homepage's widgets call its API.
    if call.arg == ubs.LOGIN_ENTRY_URL:
        call.rec.url = SPA + "#/home"
    elif call.arg.endswith(ubs.ROUTE_HOME):
        call.rec.emit("request", _ApiRequest())


def base_script(accounts=ACCOUNT_ROWS):
    s = Script(url="about:blank", download=b"a;b\n")
    s.on("launch", None, Compute(lambda c: c.rec.node("browser")))
    s.on("new_context", None, Compute(lambda c: c.rec.context))
    s.on("new_page", None, Compute(lambda c: c.rec.page))
    s.on("goto", None, Compute(_goto))
    # No dialog is open.
    s.on("evaluate", testing.js_contains(OPEN_DIALOGS), 0)
    s.on("evaluate", lambda c: c.args[1:] == (
        ubs.HOME_CASH_ACCOUNT_LINK_SELECTOR,), accounts)
    s.on("evaluate", lambda c: c.args[1:] == (
        ubs.HOME_PORTFOLIO_LINK_SELECTOR,), PORTFOLIO_HREFS)
    # Transactions: the period popover's two date inputs, and a count.
    s.on("count", ubs.PERIOD_POPOVER_DATE_INPUTS, 2)
    s.on("inner_text", ubs.TXN_COUNT_SELECTOR, "(12)")
    # Documents: one document link in the window.
    s.on("evaluate", lambda c: c.args[1:] == (ubs.DOC_LINK_SELECTOR,),
         DOCUMENT_ROWS)
    return s


def raising_script():
    s = base_script()
    # A dialog is open at the first look and closed at the next.
    s.on("evaluate", testing.js_contains(OPEN_DIALOGS), Seq(1, 0))
    s.on("click", ubs.TXN_PERIOD_FILTER_SELECTOR,
         Raises(RuntimeError("synthetic failure")), times=1)
    return s


def run_case(tmp_path, monkeypatch, *, script, flags=()):
    """Run one `download.main`; return its trace."""
    clock = testing.FakeClock().install(monkeypatch)
    testing.freeze_collector(monkeypatch, COLLECTOR, TODAY, SLUG)
    monkeypatch.setattr(cli, "_today_utc", lambda: TODAY)
    monkeypatch.setattr(download, "USER_AGENT", USER_AGENT)
    rec = Recorder(script, root=tmp_path, clock=clock)
    fake = types.ModuleType("playwright.sync_api")
    fake.sync_playwright = lambda: rec.node("playwright")
    monkeypatch.setitem(sys.modules, "playwright",
                        types.ModuleType("playwright"))
    monkeypatch.setitem(sys.modules, "playwright.sync_api", fake)
    state = tmp_path / "state.json"
    state.write_text("{}")
    argv = ["--state-path", str(state),
            "--bronze-dir", str(tmp_path / "bronze"),
            "--lookback", "2025-11-01", *flags]
    try:
        end = f"exit {download.main(argv)!r}"
    except SystemExit as e:
        end = f"SystemExit {rec.render(e.code)}"
    return [*rec.trace, end]


CASES = {
    "all": {},
    **{f"only-{p}": dict(flags=("--only", p)) for p in download.PASSES},
    "dry-run": dict(flags=("--dry-run",)),
    "no-accounts": dict(script=lambda: base_script([])),
    "pass-raises": dict(flags=("--debug",), script=raising_script),
}


def _case(name):
    """The keyword arguments of `run_case` for one case, with a fresh
    script."""
    kw = dict(CASES[name])
    kw["script"] = kw.get("script", base_script)()
    return kw


@pytest.mark.parametrize("name", sorted(CASES))
def test_main_sends_the_stored_trace(name, tmp_path, monkeypatch):
    trace = run_case(tmp_path, monkeypatch, **_case(name))
    testing.assert_trace(TRACES / f"{name}.trace", trace)
