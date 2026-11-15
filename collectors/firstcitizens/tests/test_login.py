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
    assert args.mfa_timeout == 300


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
    # Credentials arrive via env only (root AGENTS.md §3) — never argv.
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
    # happened.
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


# ============================================================
# The front half: never submit a form that is not pre-filled
# ============================================================

class _StubPage:
    url = "about:blank"

    def goto(self, *_args, **_kwargs):
        pass

    def locator(self, _selector):
        raise LookupError("no DOM in a unit test")


@pytest.fixture
def front_half(monkeypatch, tmp_path):
    """`_reveal_prefill_submit` against a stub page: no env file, no
    credentials in the environment, and a recorder in place of `_submit`.
    Returns (run, submits), where `run(by_hand, creds, prefills)` calls it."""
    for var in (login.USER_ENV, login.PASS_ENV):
        monkeypatch.delenv(var, raising=False)
    submits = []
    monkeypatch.setattr(login, "_submit",
                        lambda page: submits.append(page) or True)
    args = login.parse_args(["--env-file", str(tmp_path / "absent.env")])

    def run(*, by_hand=False, creds=False, prefills=True):
        if creds:
            monkeypatch.setenv(login.USER_ENV, "example-user")
            monkeypatch.setenv(login.PASS_ENV, "example-pass")
        monkeypatch.setattr(login, "_prefill", lambda *_a: prefills)
        return login._reveal_prefill_submit(_StubPage(), args, by_hand=by_hand)
    return run, submits


def test_a_prefilled_form_is_submitted(front_half):
    run, submits = front_half
    assert run(creds=True) is True
    assert len(submits) == 1


def test_no_credentials_is_not_submitted(front_half):
    # A blank submit counts as a failed sign-in: login, download and
    # --check stop here instead.
    run, submits = front_half
    assert run() is False
    assert submits == []


def test_a_form_that_did_not_prefill_is_not_submitted(front_half):
    run, submits = front_half
    assert run(creds=True, prefills=False) is False
    assert submits == []


def test_vnc_login_leaves_sign_in_to_the_person(front_half):
    # vnc-login waits for the outcome without clicking Sign In itself.
    run, submits = front_half
    assert run(by_hand=True) is True
    assert run(by_hand=True, creds=True, prefills=False) is True
    assert submits == []


# ============================================================
# Device registration: a miss is said, not swallowed
# ============================================================

class _RegisterLocator:
    def __init__(self, present):
        self.present = present
        self.first = self

    def count(self):
        return 1 if self.present else 0


class _RegisterPage:
    def __init__(self, url, present):
        self.url = url
        self._present = present

    def locator(self, _selector):
        return _RegisterLocator(self._present)


@pytest.fixture
def register(monkeypatch):
    monkeypatch.setattr(login, "wait_for", lambda *_a, **_k: True)
    monkeypatch.setattr(login, "page_url", lambda page: page.url)
    monkeypatch.setattr(login, "_click_q2btn", lambda _loc: True)
    return login.parse_args([])


def test_a_register_button_is_clicked(register, caplog):
    page = _RegisterPage("https://bank.example/app/#/login/mfa/register",
                         present=True)
    with caplog.at_level("INFO", logger=login.log.name):
        assert login._register_device(page, register) is True
    assert "registered this device" in caplog.text


def test_a_missing_register_step_warns_with_the_route(register, caplog):
    # The login still succeeds, but the next download would meet the 2FA
    # challenge again: the warning names where the page went instead.
    page = _RegisterPage("https://bank.example/app/#/landingPage",
                         present=False)
    with caplog.at_level("INFO", logger=login.log.name):
        assert login._register_device(page, register) is False
    assert "registered this device" not in caplog.text
    assert "/landingPage" in caplog.text
    assert any(r.levelname == "WARNING" for r in caplog.records)


class _Button:
    """A q2-btn's inner button; `covered` makes the plain click fail the way
    Playwright's actionability check does under an overlay."""

    def __init__(self, covered):
        self.covered = covered
        self.first = self
        self.events = []

    def count(self):
        return 1

    def click(self, **_kwargs):
        if self.covered:
            raise TimeoutError("element is covered by another element")
        self.events.append("click")

    def dispatch_event(self, kind):
        self.events.append(f"dispatched {kind}")


class _Q2Btn:
    def __init__(self, inner):
        self.inner = inner

    def locator(self, _selector):
        return self.inner


def test_a_q2_button_gets_a_plain_click():
    inner = _Button(covered=False)
    assert login._click_q2btn(_Q2Btn(inner)) is True
    assert inner.events == ["click"]


def test_a_covered_q2_button_gets_the_click_dispatched():
    # The cookie banner covers the Register Device button; the banner itself
    # is never clicked.
    inner = _Button(covered=True)
    assert login._click_q2btn(_Q2Btn(inner)) is True
    assert inner.events == ["dispatched click"]
