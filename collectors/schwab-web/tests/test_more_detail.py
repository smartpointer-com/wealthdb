"""
More-detail walk liveness tests: the SIGALRM lap deadline (the only
escape from a protocol call queued behind a wedged page main thread),
the reload-and-resume recovery, its cap, and the filter re-application
helper's probe gating.
"""

from __future__ import annotations

import sys
import time
from datetime import date
from pathlib import Path
from unittest.mock import MagicMock

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import download  # noqa: E402


# ============================================================
# _deadline — the SIGALRM watchdog
# ============================================================

def test_deadline_raises_out_of_a_blocked_call():
    with pytest.raises(download._WalkStalled, match="lap"):
        with download._deadline(1, "lap"):
            time.sleep(3)


def test_deadline_no_op_when_fast():
    with download._deadline(5, "lap"):
        pass  # returns without firing


# ============================================================
# _scrape_more_details — stall recovery
# ============================================================

def _walk_page():
    page = MagicMock()
    page.locator.return_value.all.return_value = []  # no rows
    return page


def test_stall_recovers_and_resumes(monkeypatch):
    page = _walk_page()
    scrolls = []
    monkeypatch.setattr(download, "_scroll_tx_table",
                        lambda p: scrolls.append(1))
    flips = iter([download._WalkStalled("more-detail page 1"), False])
    def _flip(p, pid):
        v = next(flips)
        if isinstance(v, Exception):
            raise v
        return v
    monkeypatch.setattr(download, "click_next_page", _flip)
    recovered = []
    monkeypatch.setattr(download, "_recover_tx_page",
                        lambda p, r: recovered.append(r) or True)
    reapply = MagicMock()
    out = download._scrape_more_details(page, "999", reapply=reapply)
    assert out == []
    assert recovered == [reapply]
    assert len(scrolls) == 2  # original lap + post-recovery restart


def test_recovery_cap_keeps_partials(monkeypatch):
    page = _walk_page()
    scrolls = []
    monkeypatch.setattr(download, "_scroll_tx_table",
                        lambda p: scrolls.append(1))
    def _always_stall(p, pid):
        raise download._WalkStalled("more-detail page 1")
    monkeypatch.setattr(download, "click_next_page", _always_stall)
    recovered = []
    monkeypatch.setattr(download, "_recover_tx_page",
                        lambda p, r: recovered.append(1) or True)
    out = download._scrape_more_details(page, "999", reapply=MagicMock())
    assert out == []
    # Initial lap + one restart per allowed recovery, then stop.
    assert len(recovered) == download.MAX_DETAIL_RECOVERIES
    assert len(scrolls) == 1 + download.MAX_DETAIL_RECOVERIES


def test_stall_without_reapply_stops_immediately(monkeypatch):
    page = _walk_page()
    monkeypatch.setattr(download, "_scroll_tx_table", lambda p: None)
    def _stall(p, pid):
        raise download._WalkStalled("more-detail page 1")
    monkeypatch.setattr(download, "click_next_page", _stall)
    recover = MagicMock()
    monkeypatch.setattr(download, "_recover_tx_page", recover)
    assert download._scrape_more_details(page, "999") == []
    recover.assert_not_called()


# ============================================================
# _recover_tx_page
# ============================================================

def test_recover_reloads_and_reapplies():
    page = MagicMock()
    reapply = MagicMock()
    assert download._recover_tx_page(page, reapply) is True
    page.reload.assert_called_once()
    reapply.assert_called_once()


def test_recover_reports_reload_failure():
    page = MagicMock()
    page.reload.side_effect = RuntimeError("gone")
    assert download._recover_tx_page(page, MagicMock()) is False


# ============================================================
# _apply_tx_filter — probe gating and window preference
# ============================================================

def test_apply_filter_probe_only_under_debug(monkeypatch):
    page = MagicMock()
    probed = []
    monkeypatch.setattr(download, "select_date_range", lambda p, v: None)
    monkeypatch.setattr(download, "probe_custom_date_range",
                        lambda *a: probed.append(1))
    download._apply_tx_filter(page, "999", "All", None)
    assert probed == []
    download._apply_tx_filter(page, "999", "All", None, debug=True)
    assert probed == [1]


def test_apply_filter_prefers_exact_window(monkeypatch):
    page = MagicMock()
    filled = []
    monkeypatch.setattr(download, "fill_custom_date_range",
                        lambda p, s, u: filled.append((s, u)))
    preset = MagicMock()
    monkeypatch.setattr(download, "select_date_range", preset)
    window = (date(2024, 1, 1), date(2026, 7, 17))
    download._apply_tx_filter(page, "999", "All", window)
    assert filled == [window]
    preset.assert_not_called()
