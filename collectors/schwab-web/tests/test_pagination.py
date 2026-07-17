"""
click_next_page outcome tests. The flipper must (a) wait on the
aria-current marker's id ATTRIBUTE, tolerating a display-hidden
marker, (b) retry a click the SPA dropped exactly once, and (c)
return False for a page that never advanced so callers stop instead
of re-scraping the page they are on.
"""

from __future__ import annotations

import sys
from pathlib import Path
from unittest.mock import MagicMock

import pytest  # noqa: F401

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import download  # noqa: E402


def _mk_page(marker_ids, wait_raises, marker_count=1):
    """A MagicMock page whose aria-current marker reports the given ids
    across successive reads and whose wait_for_function raises per the
    given schedule."""
    page = MagicMock()
    marker = MagicMock()
    marker.count.return_value = marker_count
    marker.get_attribute.side_effect = list(marker_ids)
    next_link = MagicMock()
    next_link.count.return_value = 1
    next_link.evaluate.return_value = None  # hit-test: nothing covering

    def locator(sel):
        loc = MagicMock()
        loc.first = marker if sel.startswith('a[aria-current') else next_link
        return loc

    page.locator.side_effect = locator
    waits = list(wait_raises)

    def wait_for_function(*args, **kwargs):
        if waits.pop(0):
            raise TimeoutError("swap timeout")

    page.wait_for_function.side_effect = wait_for_function
    return page, next_link


def test_swap_confirmed_by_wait():
    page, link = _mk_page(["pagination-1-link"], [False])
    assert download.click_next_page(page, "pagination") is True
    assert link.click.call_count == 1


def test_hidden_marker_late_arrival_still_succeeds():
    # The wait times out (marker display-hidden / slow), but the
    # attribute re-read shows the swap landed.
    page, link = _mk_page(
        ["pagination-1-link", "pagination-2-link"], [True])
    assert download.click_next_page(page, "pagination") is True
    assert link.click.call_count == 1


def test_dropped_click_is_retried_once():
    page, link = _mk_page(
        ["pagination-1-link", "pagination-1-link"], [True, False])
    assert download.click_next_page(page, "pagination") is True
    assert link.click.call_count == 2


def test_stuck_page_returns_false_after_retry():
    page, link = _mk_page(
        ["pagination-1-link"] * 3, [True, True])
    assert download.click_next_page(page, "pagination") is False
    assert link.click.call_count == 2


def test_no_marker_means_single_page():
    page, link = _mk_page([], [], marker_count=0)
    assert download.click_next_page(page, "pagination") is False
    link.click.assert_not_called()


def test_unrecognised_marker_id_stops():
    page, link = _mk_page(["bogus-id"], [])
    assert download.click_next_page(page, "pagination") is False
    link.click.assert_not_called()


def test_covered_link_is_dismissed_before_clicking(monkeypatch):
    page, link = _mk_page(["pagination-1-link"], [False])
    link.evaluate.return_value = "DIV  sdps-modal__overlay--open"
    dismissed = []
    monkeypatch.setattr(download, "_dismiss_open_modal",
                        lambda p: dismissed.append(True))
    assert download.click_next_page(page, "pagination") is True
    assert dismissed == [True]
    assert link.click.call_count == 1


def test_dismiss_open_modal_clean_page_presses_nothing():
    page = MagicMock()
    page.wait_for_function.return_value = None
    download._dismiss_open_modal(page)
    page.keyboard.press.assert_not_called()


def test_dismiss_open_modal_escapes_until_clear():
    page = MagicMock()
    # Two probes fail, the third succeeds after Escapes.
    page.wait_for_function.side_effect = [
        TimeoutError(), TimeoutError(), None]
    download._dismiss_open_modal(page)
    assert page.keyboard.press.call_count == 2
    assert all(c.args == ("Escape",)
               for c in page.keyboard.press.call_args_list)
