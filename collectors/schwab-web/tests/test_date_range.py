"""
Date-range resolution tests: the Statements preset ladder boundaries
(notably the shared 5y preset = exactly 1825 days landing on
Last5Years, safe because Schwab's Last5Years resolves to the
5-calendar-year anniversary — always >= 1826 days), the exact-window
selection for ISO --lookback values, and the tx-history custom
date-range fill (SpecifyDateRange + two positionally-addressed
datepicker inputs, mm/dd/yyyy).
"""

from __future__ import annotations

import sys
from datetime import date, timedelta
from pathlib import Path
from unittest.mock import MagicMock

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import download  # noqa: E402
import login  # noqa: E402


# ============================================================
# _since_to_schwab_preset ladder boundaries
# ============================================================

UNTIL = date(2026, 7, 17)


@pytest.mark.parametrize("days,preset", [
    (90, "Last3Months"),
    (91, "Last6Months"),
    (180, "Last6Months"),
    (181, "Last5Years"),
    (1825, "Last5Years"),   # the shared 5y preset, exactly
    (1826, "Last10Years"),
    (3650, "Last10Years"),
])
def test_preset_ladder_boundaries(days, preset):
    since = UNTIL - timedelta(days=days)
    assert login._since_to_schwab_preset(since, UNTIL) == preset


def test_wider_than_ten_years_caps_with_warning():
    since = UNTIL - timedelta(days=4000)
    log = MagicMock()
    assert login._since_to_schwab_preset(since, UNTIL, log=log) \
        == "Last10Years"
    log.warning.assert_called_once()


# ============================================================
# _exact_window — ISO dates ride along, presets don't
# ============================================================

def test_iso_lookback_yields_exact_window():
    since, until = date(2024, 1, 1), UNTIL
    assert login._exact_window("2024-01-01", since, until) \
        == (since, until)


@pytest.mark.parametrize("lookback", [None, "2y", "5y", "all", "1w"])
def test_preset_or_absent_lookback_yields_no_window(lookback):
    assert login._exact_window(
        lookback, UNTIL - timedelta(days=10), UNTIL) is None


# ============================================================
# fill_custom_date_range — SpecifyDateRange + positional fills
# ============================================================

def _page_with_datepickers(n_selects_set=1):
    page = MagicMock()
    page.evaluate.return_value = n_selects_set
    from_input, to_input = MagicMock(), MagicMock()
    locator = MagicMock()
    locator.nth.side_effect = lambda i: (from_input, to_input)[i]
    page.locator.return_value = locator
    return page, from_input, to_input


def test_fill_custom_date_range_fills_from_and_to():
    page, from_input, to_input = _page_with_datepickers()
    download.fill_custom_date_range(page, date(2024, 1, 5), date(2026, 7, 17))
    page.wait_for_function.assert_called_once()
    from_input.fill.assert_called_once_with("01/05/2024")
    to_input.fill.assert_called_once_with("07/17/2026")
    from_input.dispatch_event.assert_called_once_with("change")
    to_input.dispatch_event.assert_called_once_with("change")


def test_fill_custom_date_range_requires_the_option():
    page, _, _ = _page_with_datepickers(n_selects_set=0)
    with pytest.raises(RuntimeError, match="SpecifyDateRange"):
        download.fill_custom_date_range(
            page, date(2024, 1, 5), date(2026, 7, 17))
    page.locator.assert_not_called()


# ============================================================
# select_date_range vocabulary — the live 2026-07 option set
# ============================================================

def test_specify_date_range_needs_the_fill_helper():
    with pytest.raises(NotImplementedError, match="fill_custom_date_range"):
        download.select_date_range(MagicMock(), "SpecifyDateRange")


def test_sample_era_custom_value_is_gone():
    with pytest.raises(ValueError, match="unknown date range"):
        download.select_date_range(MagicMock(), "Custom")
