"""Tests for download.py's bronze-persistence discipline.

The load-bearing guarantee: ``download --dry-run`` is the read-only
walk (root CLAUDE.md §2) and must persist NOTHING under the bronze
``--bronze-dir`` — not even a ``run.json`` shell, because ``load``'s
``scan_bronze`` has no status guard and would ingest such a shell as a
dump run. These tests pin that invariant plus the real-run counterpart.

``--debug`` is covered here too, since it writes into that same tree:
its captures must land inside the run dir on a real run, and must not
resurrect a bronze write under ``--dry-run``, where there is no run dir
to hold them.
"""
from __future__ import annotations

import json
import sys
import types
from pathlib import Path

import download


# --------------------------------------------------------------------
# _prepare_run_dir: the run-dir-target decision in isolation
# --------------------------------------------------------------------

def test_prepare_run_dir_dry_run_creates_nothing(tmp_path):
    dest = tmp_path / "bronze"
    run_dir = download._prepare_run_dir(dest, dry_run=True)
    assert run_dir is None
    # --bronze-dir must be left entirely untouched (not even created).
    assert not dest.exists()


def test_prepare_run_dir_real_run_writes_in_progress_marker(tmp_path):
    dest = tmp_path / "bronze"
    run_dir = download._prepare_run_dir(dest, dry_run=False)
    assert run_dir is not None
    assert run_dir.parent == dest
    run_json = run_dir / "run.json"
    assert run_json.is_file()
    import json
    assert json.loads(run_json.read_text())["status"] == "in-progress"


# --------------------------------------------------------------------
# End-to-end dry-run: main() with the Playwright layer mocked
# --------------------------------------------------------------------

class _FakePage:
    def set_default_navigation_timeout(self, _ms):
        pass

    # The two calls debugcap.capture_page drives.
    def content(self):
        return "<html>synthetic home</html>"

    def screenshot(self, *, path, full_page=False):
        Path(path).write_bytes(b"\x89PNG synthetic")


class _FakeContext:
    def __init__(self):
        # download.py registers a request listener to harvest the card
        # API's apikey header; the fake records the registration so the
        # control flow under test is the real one.
        self.listeners = {}

    def on(self, event, handler):
        self.listeners.setdefault(event, []).append(handler)

    def new_page(self):
        return _FakePage()

    def close(self):
        pass


class _FakeBrowser:
    def close(self):
        pass


class _FakeSyncPlaywright:
    """Stand-in for playwright.sync_api.sync_playwright()."""

    def __enter__(self):
        return object()  # the `pw` handle; _new_context is mocked away

    def __exit__(self, *_a):
        return False


def _mock_playwright_layer(monkeypatch):
    """Neutralise the browser layer so main() exercises pure control
    flow: no real Chromium, no network, no live UBS session."""
    fake_mod = types.ModuleType("playwright.sync_api")
    fake_mod.sync_playwright = lambda: _FakeSyncPlaywright()
    monkeypatch.setitem(sys.modules, "playwright", types.ModuleType("playwright"))
    monkeypatch.setitem(sys.modules, "playwright.sync_api", fake_mod)
    monkeypatch.setattr(
        download, "_new_context", lambda _pw, _sp: (_FakeBrowser(), _FakeContext())
    )
    monkeypatch.setattr(download, "_verify_session", lambda _page: None)
    monkeypatch.setattr(download, "enumerate_accounts", lambda _page, _sd: [])


def test_dry_run_download_persists_nothing_to_bronze(tmp_path, monkeypatch):
    _mock_playwright_layer(monkeypatch)
    dest = tmp_path / "bronze"
    state = tmp_path / "state.json"
    state.write_text("{}")

    rc = download.main(
        ["--state-path", str(state), "--bronze-dir", str(dest), "--dry-run"]
    )

    assert rc == 0
    # The core regression: no run dir — and nothing at all — under
    # --bronze-dir.
    if dest.exists():
        assert list(dest.iterdir()) == []
    # Belt and suspenders: no UTC-timestamped dump dir anywhere below it.
    assert list(Path(dest).glob("*/run.json")) == []


def test_dry_run_with_debug_still_persists_nothing(tmp_path, monkeypatch):
    # --debug must not resurrect a bronze write under --dry-run: there is
    # no run dir for captures to live in, and materialising one would
    # hand `load`'s scan_bronze a dump run. The gate warns instead.
    _mock_playwright_layer(monkeypatch)
    dest = tmp_path / "bronze"
    state = tmp_path / "state.json"
    state.write_text("{}")

    rc = download.main(
        ["--state-path", str(state), "--bronze-dir", str(dest),
         "--dry-run", "--debug"]
    )

    assert rc == 0
    if dest.exists():
        assert list(dest.iterdir()) == []
    assert list(Path(dest).glob("*/screenshots")) == []


# --------------------------------------------------------------------
# --debug: bronze-resident landmark captures
# --------------------------------------------------------------------

def _mock_export_layer(monkeypatch):
    """Stub the exports so a real run reaches write_run_json without a
    browser. enumerate_accounts already returns [], so the transaction
    loop is empty and only the 10-home capture fires."""
    monkeypatch.setattr(download, "export_positions", lambda *a, **k: [])
    monkeypatch.setattr(download, "harvest_documents", lambda *a, **k: [])


def _real_run(tmp_path, monkeypatch, *flags):
    _mock_playwright_layer(monkeypatch)
    _mock_export_layer(monkeypatch)
    dest = tmp_path / "bronze"
    state = tmp_path / "state.json"
    state.write_text("{}")
    rc = download.main(
        ["--state-path", str(state), "--bronze-dir", str(dest), *flags])
    return rc, dest


def test_debug_off_writes_no_captures(tmp_path, monkeypatch):
    # The default: a routine dump holds only what `load` reads.
    rc, dest = _real_run(tmp_path, monkeypatch)
    assert rc == 0
    assert list(dest.glob("*/run.json"))     # a real dump was written
    assert list(dest.glob("*/screenshots")) == []


def test_debug_captures_home_inside_run_dir(tmp_path, monkeypatch):
    # The homepage is scraped for anchors it may simply not have, and a
    # miss only warns — so the capture is the only record of what the DOM
    # held. It must land INSIDE the run dir, beside the load inputs, so
    # prune reclaims it with the dump.
    rc, dest = _real_run(tmp_path, monkeypatch, "--debug")
    assert rc == 0
    run_dirs = [p.parent for p in dest.glob("*/run.json")]
    assert len(run_dirs) == 1
    shots = run_dirs[0] / "screenshots"
    assert {p.name for p in shots.iterdir()} == {"10-home.html", "10-home.png"}
    assert (shots / "10-home.html").read_text() == "<html>synthetic home</html>"


def test_debug_flag_defaults_off():
    assert download.parse_args(["--dry-run"]).debug is False
    assert download.parse_args(["--dry-run", "--debug"]).debug is True


# --------------------------------------------------------------------
# _fetch_document: content-addressed naming
# --------------------------------------------------------------------

class _FakeResp:
    def __init__(self, body, ok=True, status=200):
        self._body, self.ok, self.status = body, ok, status

    def body(self):
        return self._body


class _FakeReqContext:
    """A Playwright context whose request.get returns a fixed body."""

    def __init__(self, body):
        self._body = body

        class _Req:
            def get(_self, _href, timeout=None):
                return _FakeResp(self._body)

        self.request = _Req()


def test_fetch_document_named_by_content_hash(tmp_path):
    import hashlib
    docs = tmp_path / "documents"
    docs.mkdir()
    body = b"%PDF-1.4 synthetic statement bytes\n"
    sha = hashlib.sha256(body).hexdigest()

    meta = download._fetch_document(
        _FakeReqContext(body),
        "https://ubs.example/doc?apikey=TENANTSECRET&Accept=application/pdf",
        "persessiontoken0000", "Account statement 01.02.2026", docs)

    # Named by content, not by the per-session token.
    assert meta["filename"] == f"{sha}.pdf"
    assert "persessiontoken" not in meta["filename"]
    assert (docs / f"{sha}.pdf").read_bytes() == body
    assert meta["content_sha256"] == sha
    assert meta["label"] == "Account statement 01.02.2026"
    # The tenant apikey secret is never persisted in the recorded url.
    assert "apikey" not in meta["url"] and "TENANTSECRET" not in meta["url"]


def test_fetch_document_identical_bytes_collapse_to_one_file(tmp_path):
    # Two different session tokens, identical bytes → one content-addressed
    # file (the old token naming would have written two copies).
    docs = tmp_path / "documents"
    docs.mkdir()
    body = b"%PDF-1.4 same statement\n"
    ctx = _FakeReqContext(body)
    m1 = download._fetch_document(ctx, "https://u?apikey=K", "tokenAAAA", "L", docs)
    m2 = download._fetch_document(ctx, "https://u?apikey=K", "tokenBBBB", "L", docs)
    assert m1["filename"] == m2["filename"]
    assert len(list(docs.glob("*.pdf"))) == 1


def test_fetch_document_non_pdf_returns_none(tmp_path):
    docs = tmp_path / "documents"
    docs.mkdir()
    meta = download._fetch_document(
        _FakeReqContext(b"<html>login</html>"),
        "https://u?apikey=K", "tok", "L", docs)
    assert meta is None
    assert list(docs.glob("*.pdf")) == []


# --------------------------------------------------------------------
# The card pass is gated by its flags, and by the apikey being there
# --------------------------------------------------------------------

def _card_run(tmp_path, monkeypatch, *flags, capture=None):
    """A real run with the card pass stubbed, returning its run.json and
    one entry per invocation holding the `statements` sense it was passed.
    Recording the kwarg rather than a bare marker is what lets a test see
    --no-card-statements arrive, instead of only that the pass ran."""
    calls = []

    def _stub(*_a, **_kw):
        calls.append(_kw.get("statements"))
        return {"accounts": []} if capture is None else capture()

    monkeypatch.setattr(download, "_capture_cards", _stub)
    rc, dest = _real_run(tmp_path, monkeypatch, *flags)
    assert rc == 0
    return json.loads(next(dest.glob("*/run.json")).read_text()), calls


def test_cards_are_captured_by_default(tmp_path, monkeypatch):
    manifest, calls = _card_run(tmp_path, monkeypatch)
    assert calls == [True]
    assert "cards" in manifest


def test_no_cards_skips_the_pass_entirely(tmp_path, monkeypatch):
    manifest, calls = _card_run(tmp_path, monkeypatch, "--no-cards")
    assert calls == []
    # Absent, not empty: a reader must tell "not fetched" from "none found".
    assert "cards" not in manifest


def test_no_card_statements_reaches_the_card_pass(tmp_path, monkeypatch):
    """The flag has one job and no observable effect on the run.json, so
    without this the wiring could invert — statements fetched when the run
    asked for none — and every other assertion would still hold."""
    _, calls = _card_run(tmp_path, monkeypatch, "--no-card-statements")
    assert calls == [False]


def test_an_unreachable_card_surface_leaves_the_rest_of_the_dump(tmp_path,
                                                                 monkeypatch):
    # No apikey observed is the realistic failure; the run still completes
    # and still finalises as a complete dump.
    manifest, _ = _card_run(tmp_path, monkeypatch, capture=lambda: None)
    assert manifest["status"] == "complete"
    assert "cards" not in manifest


# --------------------------------------------------------------------
# _export_with_split: the 1000-transaction export cap
#
# UBS refuses a transaction export whose period holds more than
# TRX_EXPORT_CAP rows, in BOTH formats: MT940 swaps its variant
# chooser for an info dialog, CSV answers the click with a dialog and
# no download. The bisect exists so no window is ever exported over
# the cap — a window that is costs DOWNLOAD_TIMEOUT_MS and yields
# nothing, reproducibly, for as long as the account stays that busy.
# --------------------------------------------------------------------

from datetime import date, timedelta  # noqa: E402


class _FakeTxnPage:
    """Stand-in for the transactions page: remembers the applied
    period and answers the count from a fixed synthetic ledger."""

    url = "https://example.invalid/app/#/home"

    def __init__(self, txn_dates):
        self.txn_dates = txn_dates
        self.window = None

    def goto(self, _url, **_kw):
        pass

    def wait_for_selector(self, _sel, **_kw):
        pass


def _install_fake_txn_page(monkeypatch, page):
    """Replace the three browser-driving helpers the split calls, so
    the bisect arithmetic is what's under test."""
    monkeypatch.setattr(download, "_navigate_to_account_fresh",
                        lambda _p, _account: None)

    def set_period(p, since, until, _screenshot_dir, _account_id):
        p.window = (since, until)
        return since, until

    monkeypatch.setattr(download, "_set_transaction_period_custom", set_period)
    monkeypatch.setattr(
        download, "_read_transaction_count",
        lambda p: sum(1 for d in p.txn_dates
                      if p.window[0] <= d <= p.window[1]))


def _recording_exporter(calls, page):
    """Per-window exporter that records the window it was handed and
    writes a synthetic file for it."""

    def export(_page, _account, out_dir, since, until):
        count = sum(1 for d in page.txn_dates if since <= d <= until)
        calls.append((since, until, count))
        path = out_dir / f"cash_{since:%Y%m%d}_{until:%Y%m%d}.out"
        path.write_text("synthetic export")
        return path

    return export


_ACCOUNT = {"kind": "cash", "account_id": "synthetic-account-id",
            "route": "#/accounts?target=cash-account-transactions"}


def _run_split(monkeypatch, tmp_path, txn_dates, since, until, fmt="CSV",
               export=None):
    page = _FakeTxnPage(txn_dates)
    _install_fake_txn_page(monkeypatch, page)
    calls: list[tuple] = []
    paths, count, gaps = download._export_with_split(
        page, _ACCOUNT, since, until, tmp_path, None,
        depth=0, fmt=fmt, export=export or _recording_exporter(calls, page),
    )
    return paths, count, calls, gaps


def test_split_under_cap_exports_the_window_whole(tmp_path, monkeypatch):
    since, until = date(2026, 1, 1), date(2026, 3, 31)
    dates = [since + timedelta(days=i % 90) for i in range(50)]
    paths, count, calls, gaps = _run_split(monkeypatch, tmp_path, dates,
                                          since, until)
    assert count == 50
    assert calls == [(since, until, 50)]
    assert len(paths) == 1


def test_split_never_exports_a_window_over_the_cap(tmp_path, monkeypatch):
    """The regression: an over-cap window must be bisected, not handed
    to the exporter to wait out a download UBS will never produce."""
    since, until = date(2024, 1, 1), date(2026, 9, 7)
    span = (until - since).days
    # Dense enough that the full window is far over the cap.
    dates = [since + timedelta(days=i % span)
             for i in range(4 * download.TRX_EXPORT_CAP)]
    paths, count, calls, gaps = _run_split(monkeypatch, tmp_path, dates,
                                          since, until)
    assert count == 4 * download.TRX_EXPORT_CAP
    assert calls, "the window was never exported at all"
    assert all(c <= download.TRX_EXPORT_CAP for _s, _u, c in calls), \
        f"exported an over-cap window: {calls}"
    assert len(paths) == len(calls) > 1


def test_split_windows_tile_the_period_without_gap_or_overlap(tmp_path,
                                                              monkeypatch):
    since, until = date(2024, 1, 1), date(2026, 9, 7)
    span = (until - since).days
    dates = [since + timedelta(days=i % span)
             for i in range(3 * download.TRX_EXPORT_CAP)]
    _paths, _count, calls, _gaps = _run_split(monkeypatch, tmp_path, dates,
                                              since, until)
    windows = sorted((s, u) for s, u, _c in calls)
    assert windows[0][0] == since
    assert windows[-1][1] == until
    for (_s1, u1), (s2, _u2) in zip(windows, windows[1:]):
        assert s2 == u1 + timedelta(days=1), \
            f"windows are not contiguous at {u1} -> {s2}"
    # Every transaction lands in exactly one exported window.
    assert sum(c for _s, _u, c in calls) == len(dates)


def test_split_gives_up_inside_a_one_day_window(tmp_path, monkeypatch):
    """A single day over the cap cannot be bisected further; it is
    reported and skipped rather than exported into a stall."""
    day = date(2026, 5, 4)
    dates = [day] * (download.TRX_EXPORT_CAP + 1)
    paths, count, calls, gaps = _run_split(monkeypatch, tmp_path, dates,
                                          day, day)
    assert count == download.TRX_EXPORT_CAP + 1
    assert calls == []
    assert paths == []
    assert gaps == [(day, day)], "the day it gave up on was not reported"


def test_split_reports_a_window_the_exporter_could_not_download(tmp_path,
                                                                monkeypatch):
    """Under the cap and still no file — UBS answered the click with a
    dialog, or the download timed out. The window is lost to the
    statement PDFs, so the split says which one."""
    since, until = date(2026, 1, 1), date(2026, 3, 31)
    dates = [since + timedelta(days=i % 90) for i in range(50)]
    paths, _count, _calls, gaps = _run_split(
        monkeypatch, tmp_path, dates, since, until,
        export=lambda *_a, **_kw: None)
    assert paths == []
    assert gaps == [(since, until)]


def test_split_reports_no_gap_when_every_window_lands(tmp_path, monkeypatch):
    # The field is always written, so an empty list has to mean covered.
    since, until = date(2024, 1, 1), date(2026, 9, 7)
    span = (until - since).days
    dates = [since + timedelta(days=i % span)
             for i in range(3 * download.TRX_EXPORT_CAP)]
    _p, _c, calls, gaps = _run_split(monkeypatch, tmp_path, dates,
                                     since, until)
    assert len(calls) > 1
    assert gaps == []


def test_both_formats_bisect_on_the_same_cap(tmp_path, monkeypatch):
    """MT940 always bisected; CSV used to be exported "one shot over
    the full window (no observed cap)" and stalled on a busy account."""
    since, until = date(2024, 1, 1), date(2026, 9, 7)
    span = (until - since).days
    dates = [since + timedelta(days=i % span)
             for i in range(3 * download.TRX_EXPORT_CAP)]
    _p, _c, csv_calls, _g = _run_split(monkeypatch, tmp_path, dates,
                                       since, until, fmt="CSV")
    _p, _c, mt_calls, _g = _run_split(monkeypatch, tmp_path, dates,
                                      since, until, fmt="MT940")
    assert [w[:2] for w in csv_calls] == [w[:2] for w in mt_calls]
    assert len(csv_calls) > 1


def test_export_transactions_records_every_csv_chunk(tmp_path, monkeypatch):
    """run.json carries a list per format: a busy account's CSV is
    several files, and naming only one of them would hide the rest
    from anyone reading the dump's manifest."""
    since, until = date(2024, 1, 1), date(2026, 9, 7)
    span = (until - since).days
    dates = [since + timedelta(days=i % span)
             for i in range(3 * download.TRX_EXPORT_CAP)]
    page = _FakeTxnPage(dates)
    _install_fake_txn_page(monkeypatch, page)
    monkeypatch.setattr(download, "_dismiss_open_overlays", lambda _p: None)
    calls: list[tuple] = []
    exporter = _recording_exporter(calls, page)
    monkeypatch.setattr(download, "_export_csv", exporter)
    monkeypatch.setattr(download, "_export_mt940", exporter)

    meta = download.export_transactions(page, _ACCOUNT, since, until,
                                        tmp_path, None)

    assert meta["transaction_count"] == 3 * download.TRX_EXPORT_CAP
    assert len(meta["csv_filenames"]) > 1
    assert len(meta["mt940_filenames"]) > 1
    assert meta["csv_gaps"] == [] and meta["mt940_gaps"] == []
    written = {p.name for p in (tmp_path / "transactions").iterdir()}
    assert set(meta["csv_filenames"]) <= written


def test_export_transactions_records_a_format_that_produced_nothing(
        tmp_path, monkeypatch):
    """The silent loss this exists to stop: when one format's export
    produces nothing but the other succeeds, the manifest still reads as
    complete and the missing format goes unnoticed."""
    since, until = date(2026, 1, 1), date(2026, 3, 31)
    dates = [since + timedelta(days=i % 90) for i in range(50)]
    page = _FakeTxnPage(dates)
    _install_fake_txn_page(monkeypatch, page)
    monkeypatch.setattr(download, "_dismiss_open_overlays", lambda _p: None)
    calls: list[tuple] = []
    monkeypatch.setattr(download, "_export_csv", lambda *_a, **_kw: None)
    monkeypatch.setattr(download, "_export_mt940",
                        _recording_exporter(calls, page))

    meta = download.export_transactions(page, _ACCOUNT, since, until,
                                        tmp_path, None)

    assert meta["csv_filenames"] == []
    assert meta["csv_gaps"] == ["2026-01-01..2026-03-31"]
    assert len(meta["mt940_filenames"]) == 1
    assert meta["mt940_gaps"] == []
