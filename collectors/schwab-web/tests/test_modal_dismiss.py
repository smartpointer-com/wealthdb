"""
Modal-dismissal escalation and the account-selector guard: Escape
probes first, then the dialog's own dismiss-only close control
(observed live: the wire-details modal ignores Escape), and a leaked
overlay is re-dismissed at every account boundary so one stuck dialog
cannot cascade across the account loop. Synthetic mocks only.
"""

from __future__ import annotations

import logging
import sys
from pathlib import Path
from unittest.mock import MagicMock

import pytest  # noqa: F401

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import download  # noqa: E402


def _modal_page(monkeypatch, overlay_seq, close_present=False):
    """A page whose overlay probe yields `overlay_seq` in order (True =
    a modal is up, exhausted = clean), with an optional visible close
    control on the open dialog. Clock and sleeps are virtual."""
    page = MagicMock()
    presses = []
    page.keyboard.press.side_effect = lambda k: presses.append(k)
    close_btn = MagicMock()
    close_btn.count.return_value = 1 if close_present else 0
    close_btn.is_visible.return_value = close_present
    page.locator.return_value.first = close_btn
    seq = iter(overlay_seq)
    monkeypatch.setattr(download, "_overlay_open",
                        lambda p: next(seq, False))
    t = [0.0]

    def _tick():
        t[0] += 0.25
        return t[0]

    monkeypatch.setattr(download.time, "monotonic", _tick)
    monkeypatch.setattr(download.time, "sleep", lambda s: None)
    return page, presses, close_btn


# ============================================================
# _dismiss_open_modal escalation
# ============================================================

def test_clean_page_is_a_cheap_no_op(monkeypatch):
    # The clean verdict needs TWO clean probes (settle) — one instant
    # probe races an in-flight dialog.
    page, presses, close_btn = _modal_page(monkeypatch, [False, False])
    download._dismiss_open_modal(page)
    assert presses == []
    assert not close_btn.click.called


def test_escape_responsive_modal_needs_no_close_control(monkeypatch):
    page, presses, close_btn = _modal_page(monkeypatch,
                                           [True, False, False])
    download._dismiss_open_modal(page)
    assert presses == ["Escape"]
    assert not close_btn.click.called


def test_escape_immune_modal_falls_back_to_its_close_control(monkeypatch):
    # The wire-details shape: Escape never clears the overlay; the
    # dialog's own X does.
    page, presses, close_btn = _modal_page(
        monkeypatch, [True, True, True, False, False], close_present=True)
    download._dismiss_open_modal(page)
    assert presses == ["Escape"] * 3
    close_btn.click.assert_called_once()


def test_hopeless_overlay_warns_and_returns(monkeypatch, caplog):
    page, presses, close_btn = _modal_page(monkeypatch, [])
    monkeypatch.setattr(download, "_overlay_open", lambda p: True)
    with caplog.at_level(logging.WARNING, logger="schwab-web.download"):
        download._dismiss_open_modal(page)          # must not raise
    assert not close_btn.click.called               # no control to click
    assert any("still open" in r.message and "no close control" in r.message
               for r in caplog.records)


def test_close_control_click_failure_still_warns_not_raises(monkeypatch,
                                                            caplog):
    page, presses, close_btn = _modal_page(monkeypatch, [],
                                           close_present=True)
    monkeypatch.setattr(download, "_overlay_open", lambda p: True)
    close_btn.click.side_effect = RuntimeError("detached")
    with caplog.at_level(logging.WARNING, logger="schwab-web.download"):
        download._dismiss_open_modal(page)
    assert any("still open" in r.message for r in caplog.records)


def test_overlay_probe_ignores_a_hidden_stale_overlay():
    # sdps can leave the --open class on an already-hidden overlay; a
    # hidden overlay intercepts nothing, so it must not read as open.
    counts = {".sdps-modal__overlay--open:visible": 0,
              '[role="dialog"]:visible': 0}
    page = MagicMock()
    page.locator.side_effect = lambda sel: MagicMock(
        count=MagicMock(return_value=counts.get(sel, 0)))
    assert download._overlay_open(page) is False
    counts[".sdps-modal__overlay--open:visible"] = 1
    assert download._overlay_open(page) is True


def test_expect_modal_waits_for_the_dialog_to_materialize(monkeypatch):
    # The SPA under load can lag the dialog's DOM insertion seconds
    # behind the click that opened it; expect_modal callers wait for
    # it instead of trusting an instant clean probe.
    page, presses, close_btn = _modal_page(monkeypatch,
                                           [True, False, False])
    download._dismiss_open_modal(page, expect_modal=True)
    page.locator.return_value.first.wait_for.assert_called_once()
    assert presses == ["Escape"]


# ============================================================
# select_account: boundary dismissal + bounded click fallback
# ============================================================

def _selector_page():
    page = MagicMock()
    btn = MagicMock()
    page.locator.return_value.first = btn
    return page, btn


def test_select_account_dismisses_before_touching_the_page(monkeypatch):
    order = []
    monkeypatch.setattr(download, "_dismiss_open_modal",
                        lambda p, **kw: order.append("dismiss"))
    page, btn = _selector_page()
    btn.click.side_effect = lambda timeout: order.append("click")
    download.select_account(page, "account-selector-header-0-account-1")
    assert order == ["dismiss", "click"]
    assert not btn.dispatch_event.called            # clean path: real click
    page.locator.return_value.dispatch_event.assert_called_with("click")


def test_intercepted_click_dismisses_again_and_dispatches(monkeypatch):
    dismissals = []
    monkeypatch.setattr(download, "_dismiss_open_modal",
                        lambda p, **kw: dismissals.append(1))
    page, btn = _selector_page()
    btn.click.side_effect = RuntimeError(
        "Locator.click: Timeout 10000ms exceeded.\nCall log:\n  - ...")
    download.select_account(page, "account-selector-header-0-account-1")
    assert len(dismissals) == 2                     # boundary + retry
    btn.dispatch_event.assert_called_once_with("click")


# ============================================================
# More-detail walk: wire skip, retryable rows, wedge breaker
# ============================================================

def _walk_page(rows):
    page = MagicMock()
    page.locator.return_value.all.return_value = rows
    return page


def _failing_row(cells):
    row = MagicMock()
    row.cells = cells
    btn = MagicMock()
    btn.count.return_value = 1
    btn.click.side_effect = RuntimeError("intercepted by overlay")
    row.locator.return_value.first = btn
    return row


def _wire_walk(monkeypatch, *, overlay_open):
    monkeypatch.setattr(download, "_extract_tx_row_cells",
                        lambda r: r.cells)
    monkeypatch.setattr(download, "_scroll_tx_table", lambda p: None)
    monkeypatch.setattr(download, "click_next_page", lambda p, pid: False)
    dismissals = []
    monkeypatch.setattr(download, "_dismiss_open_modal",
                        lambda p, **kw: dismissals.append(1))
    monkeypatch.setattr(download, "_overlay_open",
                        lambda p: overlay_open)
    return dismissals


def test_wire_rows_never_open_their_modal(monkeypatch):
    _wire_walk(monkeypatch, overlay_open=False)
    row = _failing_row(["01/02/2026", "Wire", "", "Wire Sent", "-1.00"])
    details = download._scrape_more_details(_walk_page([row]), "000")
    assert details == []
    assert not row.locator.called           # More link never even queried


def test_intercepted_more_click_attempts_dismissal(monkeypatch):
    dismissals = _wire_walk(monkeypatch, overlay_open=False)
    row = _failing_row(["01/02/2026", "Buy", "VTI", "Bought", "-1.00"])
    details = download._scrape_more_details(_walk_page([row]), "000")
    assert details == []
    assert dismissals == [1]                # failed click still dismisses


def test_zero_export_account_is_marked_failed(monkeypatch):
    # The export files are the authoritative tx data; an account whose
    # Export clicks were all eaten must not read as "ok" in run.json.
    entries = iter([
        {"label": "a", "suffix": "000", "exports": [],
         "more_details_count": 0},
        {"label": "b", "suffix": "001",
         "exports": [{"format": "Csv"}], "more_details_count": 0},
    ])
    monkeypatch.setattr(download, "capture_transactions",
                        lambda *a, **k: next(entries))
    monkeypatch.setattr(download, "maybe_screenshot", lambda *a: None)
    res = download.run_transactions(
        MagicMock(), [{"label": "a", "suffix": "000", "entry_id": "e0"},
                      {"label": "b", "suffix": "001", "entry_id": "e1"}],
        Path("/nonexistent"), None, "Last3Months")
    assert res[0]["error"] == "no export captured"
    assert "error" not in res[1]


def test_wedged_overlay_takes_the_recovery_path(monkeypatch):
    # WEDGED_ROW_FAILS consecutive interceptions with the overlay still
    # standing: capture ground truth, then reload-recover (bounded by
    # MAX_DETAIL_RECOVERIES) instead of burning 5s on every row.
    _wire_walk(monkeypatch, overlay_open=True)
    captures = []
    monkeypatch.setattr(download, "_capture_dialog_html",
                        lambda p, d: captures.append(d))
    recoveries = []

    def _recover(p, r):
        recoveries.append(1)
        return True

    monkeypatch.setattr(download, "_recover_tx_page", _recover)
    rows = [_failing_row(["01/02/2026", "Buy", f"T{i}", "Bought", "-1.00"])
            for i in range(download.WEDGED_ROW_FAILS)]
    details = download._scrape_more_details(
        _walk_page(rows), "000", reapply=lambda: None)
    assert details == []
    assert captures                         # dialog DOM ground truth taken
    assert len(recoveries) == download.MAX_DETAIL_RECOVERIES
