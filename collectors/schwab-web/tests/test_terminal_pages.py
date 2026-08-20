"""
Terminal-page (gateway notice) handling: the `#/information/<code>`
route family ends the run immediately in every wait loop — no retries,
no burning the timeout — and a lockout notice carries the page's own
text verbatim. Synthetic URLs and DOM only.
"""

from __future__ import annotations

import sys
from pathlib import Path
from unittest.mock import MagicMock

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import landmarks as schwab  # noqa: E402
import login  # noqa: E402


NOTICE = "https://sws-gateway-nr.schwab.com/ui/host/#/information/12345"
PLACEHOLDER = "https://sws-gateway-nr.schwab.com/ui/host/#/placeholder"
POST_AUTH = "https://client.schwab.com/app/accounts/summary/"


# ============================================================
# landmarks URL shapes
# ============================================================

@pytest.mark.parametrize("url", [
    NOTICE,
    "https://sws-gateway.schwab.com/ui/host/#/information/999",
    "https://sws-gateway-nr.schwab.com/ui/host/#/information",
    "https://sws-gateway-nr.schwab.com/ui/host/#/information/12345?x=1",
])
def test_notice_route_family_matches(url):
    assert schwab.is_gateway_notice_url(url)


@pytest.mark.parametrize("url", [
    PLACEHOLDER,
    POST_AUTH,
    "https://www.schwab.com/",
    "https://evil.example/#/information/1",
    "https://sws-gateway-nr.schwab.com/ui/host/#/placeholder/information",
    "",
])
def test_non_notice_urls_do_not_match(url):
    assert not schwab.is_gateway_notice_url(url)


def test_looks_locked_on_lockout_wording():
    assert schwab.looks_locked("Your account is locked. Please contact us.")
    # The live lockout wording (identity-verification lock).
    assert schwab.looks_locked(
        "For your protection, we need to verify your identity before "
        "proceeding. Please call for assistance.")
    # The generic problem-logging-in wording is NOT a lockout marker.
    assert not schwab.looks_locked("There's a problem logging you in.")


# ============================================================
# _notice_scope / _notice_error
# ============================================================

def _with_msg_label(mock, text):
    """Expose a rendered #msgLabel container on a page/frame mock, so
    _notice_message finds the text without waiting out its settle."""
    label = MagicMock()
    label.count.return_value = 1
    label.inner_text.return_value = text
    mock.locator.return_value.first = label
    return mock


def _page(url, frames=(), body="Notice text."):
    page = MagicMock()
    page.evaluate.side_effect = Exception("no js in tests")
    page.url = url
    page.frames = list(frames)
    return _with_msg_label(page, body)


def _frame(url):
    f = MagicMock()
    f.url = url
    f.parent_frame = object()
    f.evaluate.return_value = "This account is locked. Call for help."
    return _with_msg_label(f, "This account is locked. Call for help.")


def test_notice_scope_sees_top_level(monkeypatch):
    page = _page(NOTICE)
    assert login._notice_scope(page) is page


def test_notice_scope_sees_the_gateway_iframe():
    frame = _frame(NOTICE)
    page = _page("https://www.schwab.com/", frames=[frame])
    assert login._notice_scope(page) is frame


def test_notice_scope_none_elsewhere():
    page = _page(PLACEHOLDER, frames=[_frame(PLACEHOLDER)])
    assert login._notice_scope(page) is None


def test_notice_error_carries_verbatim_text_and_lock_flag():
    exc = login._notice_error(_frame(NOTICE))
    assert exc.locked
    assert "This account is locked. Call for help." in exc.page_text
    assert exc.exit_code == 9


def test_notice_message_prefers_the_container_over_body_text():
    # The body carries footer boilerplate from the moment the route
    # mounts; the message container is what the notice actually says.
    page = _page(NOTICE, body="footer boilerplate")
    _with_msg_label(page, "For your protection, we need to verify your "
                          "identity before proceeding.")
    assert login._notice_message(page).startswith("For your protection")


def test_notice_message_falls_back_to_body_text(monkeypatch):
    # No message container ever renders: after the settle window the
    # whole-body text is still reported verbatim.
    page = _page(NOTICE)
    page.locator.return_value.first.count.return_value = 0
    page.evaluate.side_effect = None
    page.evaluate.return_value = "Whole body text."
    t = [0.0]

    def tick():
        t[0] += 1.0
        return t[0]
    monkeypatch.setattr(login.time, "monotonic", tick)
    monkeypatch.setattr(login.time, "sleep", lambda s: None)
    assert login._notice_message(page) == "Whole body text."


# ============================================================
# Wait loops stop immediately on a notice page
# ============================================================

def test_wait_for_mfa_input_raises_on_notice(monkeypatch):
    page = _page(NOTICE)
    monkeypatch.setattr(login, "_live_url", lambda p: NOTICE)
    monkeypatch.setattr(login.time, "sleep", lambda s: None)
    with pytest.raises(login.TerminalPageError):
        login._wait_for_mfa_input(page, timeout_s=5)


def test_verify_mfa_outcome_raises_on_notice(monkeypatch):
    page = _page(NOTICE)
    monkeypatch.setattr(login, "_live_url", lambda p: NOTICE)
    monkeypatch.setattr(login.time, "sleep", lambda s: None)
    with pytest.raises(login.TerminalPageError):
        login._verify_mfa_outcome(page, MagicMock(), budget_s=5,
                                  grace_s=0, poll_s=0.01)


def test_wait_for_post_auth_raises_on_notice(monkeypatch):
    page = _page(NOTICE)
    page.is_closed.return_value = False
    context = MagicMock()
    context.pages = [page]
    monkeypatch.setattr(login, "_live_url", lambda p: NOTICE)
    monkeypatch.setattr(login.time, "sleep", lambda s: None)
    with pytest.raises(login.TerminalPageError):
        login._wait_for_post_auth(page, context, timeout_s=5, poll_s=0.01)


def test_wait_for_post_auth_still_finds_the_app(monkeypatch):
    page = _page(POST_AUTH)
    page.is_closed.return_value = False
    context = MagicMock()
    context.pages = [page]
    monkeypatch.setattr(login, "_live_url", lambda p: POST_AUTH)
    assert login._wait_for_post_auth(page, context, timeout_s=5,
                                     poll_s=0.01) is page
