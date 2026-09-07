"""Unit tests for login.py's browserless surface: argument parsing, the
q2token header helper and the logon watcher. The browser-driven flow itself
needs a live site and is validated separately (DESIGN.md §4.2).

Synthetic values only.
"""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import login  # noqa: E402
import q2client  # noqa: E402


def test_parse_args_defaults():
    args = login.parse_args([])
    assert args.profile_dir == Path("/secrets/firstcitizens-profile")
    assert args.env_file == Path("/secrets/firstcitizens.env")
    assert args.state_path == Path("/secrets/firstcitizens-state.json")
    assert args.cli_mfa is True          # terminal 2FA by default
    assert args.check is False
    assert args.mfa_timeout == 600


def test_no_cli_mfa_flag_toggles_vnc_path():
    assert login.parse_args(["--no-cli-mfa"]).cli_mfa is False


def test_check_flag():
    assert login.parse_args(["--check"]).check is True


def test_fresh_flag_defaults_off_and_parses():
    assert login.parse_args([]).fresh is False
    assert login.parse_args(["--fresh"]).fresh is True


def test_fresh_with_check_is_rejected():
    # A read-only probe must never wipe device trust.
    assert login.main(["--check", "--fresh"]) == 2


def test_no_password_flag_exists():
    # Credentials arrive via env only (root CLAUDE.md §3) — never argv.
    with pytest.raises(SystemExit):
        login.parse_args(["--password", "x"])


class _StubContext:
    def __init__(self, cookies):
        self._cookies = cookies

    def cookies(self):
        return self._cookies


def test_q2_headers_reads_the_q2token_cookie():
    ctx = _StubContext([{"name": "other", "value": "x"},
                        {"name": q2client.Q2TOKEN, "value": "tok123"}])
    headers = login.q2_headers(ctx)
    assert headers[q2client.Q2TOKEN] == "tok123"
    assert headers["Accept"] == "application/json"


def test_q2_headers_without_cookie_omits_token():
    headers = login.q2_headers(_StubContext([]))
    assert q2client.Q2TOKEN not in headers
    assert headers["Accept"] == "application/json"


# ============================================================
# The logon watcher
# ============================================================

class _StubRequest:
    def __init__(self, method):
        self.method = method


class _StubResponse:
    def __init__(self, url, status=200, body=None, method="POST"):
        self.url = url
        self.status = status
        self._body = body
        self.request = _StubRequest(method)

    def json(self):
        if self._body is None:
            raise ValueError("no body")
        return self._body


class _ResponseContext:
    def __init__(self):
        self.handlers = []

    def on(self, _event, handler):
        self.handlers.append(handler)

    def fire(self, resp):
        for h in self.handlers:
            h(resp)


def _watch_with(status, body, method="POST"):
    ctx = _ResponseContext()
    watch = login._LogonWatch().attach(ctx)
    ctx.fire(_StubResponse(q2client.logon_url(), status, body, method))
    return watch


def test_watch_reads_a_trusted_logon_as_authenticated():
    watch = _watch_with(200, {"data": {"userProfileData": {"name": "example"},
                                       "accessCodeTargets": None}})
    assert watch.outcome().authenticated


def test_watch_ignores_the_cors_preflight():
    # The browser preflights the logon URL, and that OPTIONS answers 200
    # with an EMPTY BODY — which classify_logon reads as a trusted-device
    # login. Taken for the outcome it declares a sign-in that never
    # happened; the amex sibling hit exactly this live.
    assert _watch_with(200, None, method="OPTIONS").outcome() is None


def test_the_preflight_does_not_mask_the_real_response():
    # Both arrive, preflight first; the verdict must be the POST's.
    ctx = _ResponseContext()
    watch = login._LogonWatch().attach(ctx)
    ctx.fire(_StubResponse(q2client.logon_url(), 200, None, "OPTIONS"))
    ctx.fire(_StubResponse(q2client.logon_url(), 203, {"data": {
        "userProfileData": None,
        "accessCodeTargets": [{"notificationType": 3,
                               "display": "Text: (XXX) XXX-XXXX",
                               "value": "1001"}]}}))
    assert watch.outcome().needs_2fa


def test_watch_ignores_an_unrelated_response():
    ctx = _ResponseContext()
    watch = login._LogonWatch().attach(ctx)
    ctx.fire(_StubResponse(q2client.accounts_url(), 200, {"data": []}))
    assert watch.outcome() is None
