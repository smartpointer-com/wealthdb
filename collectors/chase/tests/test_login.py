"""Unit tests for login.py's browserless parts — the authenticated-response
predicate, the response watcher, the event-loop-pumping waiter, and argument
parsing. The browser flow (pre-fill, submit, CLI 2FA, handoff to
download.walk) needs a live session and is validated on a real run. No
credentials or live navigation.
"""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import login  # noqa: E402


def test_is_authenticated_response():
    B = "https://secure.chase.com"
    # Authenticated: the post-auth router + the account API (2xx).
    assert login.is_authenticated_response(
        B + "/svc/wl/auth/l4/v1/user/router/list", 200)
    assert login.is_authenticated_response(
        B + "/svc/rr/accounts/secure/v2/account/detail/dda/list", 200)
    assert login.is_authenticated_response(
        B + "/svc/rl/accounts/secure/v1/dashboard/module/list", 200)
    # Pre-auth /svc/ calls (site availability, the 2FA challenge) are NOT auth.
    assert not login.is_authenticated_response(
        B + "/svc/wl/auth/public/v1/site/availability/list", 200)
    assert not login.is_authenticated_response(
        B + "/svc/wl/auth/public/gateway/ccb/fraud/authentication/"
            "challenge-options/v8/options", 200)
    # A marker path with a non-2xx/3xx status is not a completed sign-in.
    assert not login.is_authenticated_response(
        B + "/svc/rr/accounts/secure/v2/account/detail/dda/list", 401)
    # Non-/svc/ URLs never count.
    assert not login.is_authenticated_response(
        B + "/web/auth/dashboard#/dashboard/overview", 200)


AUTHED_URL = ("https://secure.chase.com/svc/rr/accounts/secure/v2/"
              "account/detail/dda/list")


class _StubRequest:
    def __init__(self, method):
        self.method = method


class _StubResponse:
    """Stands in for a Playwright response; `request.method` is what the
    preflight guard reads."""

    def __init__(self, url, status=200, method="GET"):
        self.url = url
        self.status = status
        self.request = _StubRequest(method)


class _StubContext:
    def __init__(self):
        self.handlers = []

    def on(self, _event, handler):
        self.handlers.append(handler)

    def fire(self, resp):
        for h in self.handlers:
            h(resp)


def test_watch_flips_ok_on_an_authenticated_response():
    ctx = _StubContext()
    watch = login._AuthWatch().attach(ctx)
    ctx.fire(_StubResponse(AUTHED_URL))
    assert watch.ok


def test_watch_ignores_the_cors_preflight():
    # The browser preflights a cross-origin /svc/ call, and that OPTIONS
    # answers 200 for the marker URL about a second before the real call —
    # so an auth signal keyed on URL + status reads it as a completed
    # sign-in. The amex sibling hit exactly this live.
    ctx = _StubContext()
    watch = login._AuthWatch().attach(ctx)
    ctx.fire(_StubResponse(AUTHED_URL, method="OPTIONS"))
    assert not watch.ok


def test_the_preflight_does_not_hide_the_real_response():
    # Both arrive, preflight first; the signal must still be the real one's.
    ctx = _StubContext()
    watch = login._AuthWatch().attach(ctx)
    ctx.fire(_StubResponse(AUTHED_URL, method="OPTIONS"))
    ctx.fire(_StubResponse(AUTHED_URL))
    assert watch.ok


def test_parse_args_defaults():
    a = login.parse_args([])
    assert a.profile_dir == Path("/secrets/chase-profile")
    assert a.env_file == Path("/secrets/chase.env")
    assert a.bronze_dir is None            # login-only unless the verb sets it
    assert a.check is False
    assert a.cli_mfa is True               # terminal 2FA by default (no VNC)
    assert a.mfa_timeout == 600            # human-in-the-loop, long
    assert a.no_documents is False
    assert a.dry_run is False
    assert a.lookback is None              # standard download flag present


def test_parse_args_download_verb_flags():
    a = login.parse_args([
        "--bronze-dir", "/data", "--lookback", "1y",
        "--no-documents", "--dry-run", "-v",
    ])
    assert a.bronze_dir == Path("/data")
    assert a.lookback == "1y"
    assert a.no_documents and a.dry_run and a.verbose


def test_parse_args_no_cli_mfa():
    # The vnc-login fallback flips off the terminal-2FA default.
    assert login.parse_args(["--no-cli-mfa"]).cli_mfa is False
    assert login.parse_args(["--cli-mfa"]).cli_mfa is True


def test_parse_args_check():
    a = login.parse_args(["--check"])
    assert a.check is True


def test_no_password_flag():
    # Credentials via env only (root CLAUDE.md §3).
    with pytest.raises(SystemExit):
        login.parse_args(["--password", "x"])


class _El:
    """A resolved element; only visibility matters for _locate."""

    def __init__(self, visible=True):
        self._visible = visible

    def is_visible(self):
        return self._visible


class _Match:
    """What frame.locator(sel) returns: N elements, addressable by nth()."""

    def __init__(self, els):
        self.els = list(els)

    def count(self):
        return len(self.els)

    def nth(self, i):
        return self.els[i]

    @property
    def first(self):
        return _Match(self.els[:1])


class _StubFrame:
    def __init__(self, url, matches=None):
        self.url = url
        self._matches = matches or {}

    def locator(self, selector):
        return self._matches.get(selector, _Match([]))


class _StubFramedPage:
    def __init__(self, frames):
        self.frames = frames
        self.main_frame = frames[0] if frames else None


PWD_SEL = "input[type='password']"


def test_locate_finds_element_in_chase_iframe():
    # The logon form is in a chase.com iframe (#logonbox); a main-frame-only
    # lookup misses it — _locate searches all chase frames.
    el = _El()
    page = _StubFramedPage([
        _StubFrame("https://secure.chase.com/web/auth/dashboard"),   # main, no match
        _StubFrame("https://secure05c.chase.com/logon", {PWD_SEL: _Match([el])}),
    ])
    assert login._locate(page, PWD_SEL) is el


def test_locate_returns_first_visible_past_hidden_duplicate():
    # The challenge ids are duplicated (a hidden template beside the live
    # control); _locate must skip the hidden .first and return the visible one.
    hidden, visible = _El(visible=False), _El(visible=True)
    page = _StubFramedPage([
        _StubFrame("https://secure05c.chase.com/logon",
                   {"#sms": _Match([hidden, visible])})])
    assert login._locate(page, "#sms") is visible


def test_locate_skips_foreign_frames():
    page = _StubFramedPage([
        _StubFrame("https://widgets.evil.example/x", {PWD_SEL: _Match([_El()])})])
    assert login._locate(page, PWD_SEL) is None


def test_locate_none_when_absent_or_all_hidden():
    assert login._locate(
        _StubFramedPage([_StubFrame("https://secure.chase.com/x")]), PWD_SEL) is None
    page = _StubFramedPage([_StubFrame(
        "https://secure.chase.com/x", {PWD_SEL: _Match([_El(False), _El(False)])})])
    assert login._locate(page, PWD_SEL) is None


def test_factor_control_covers_drivable_factors():
    # Each automatable factor maps to (id, visible label) — ids/labels from
    # the captured "Confirm Your Identity" DOM.
    import auth_dialog
    assert set(login.FACTOR_CONTROL) == set(auth_dialog.SUPPORTED_FACTORS)
    assert login.FACTOR_CONTROL[auth_dialog.FACTOR_OTP_SMS] == ("#sms", "Get a text")
    assert login.FACTOR_CONTROL[auth_dialog.FACTOR_INAPP][0] == "#inAppSend"


def test_reuses_explore_prefill():
    # login.py must not fork a second copy of the prefill; the origin gate is
    # centralised in mdsui.chase_frames (itself built on explore.HOST_RE).
    import explore
    import mdsui
    assert login._maybe_prefill_login is explore._maybe_prefill_login
    assert mdsui.HOST_RE is explore.HOST_RE
