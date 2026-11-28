"""Unit tests for login.py's browserless surface: argument parsing, the
bearer harvest, the signed-in probe, and the terminal sign-in driven
against a stub page that steps through the form, the push wait screen and
the redirect. Synthetic values only."""
from __future__ import annotations

import logging
import sys
from pathlib import Path
from types import SimpleNamespace

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import george_client as george  # noqa: E402
import login  # noqa: E402

TOKEN = "synthetic-bearer"
USER = "example-user"
REDIRECT = (f"https://george.sparkasse.at/index.html#access_token={TOKEN}"
            "&token_type=bearer&expires_in=3599")


# --- stubs ----------------------------------------------------------------

class Resp:
    def __init__(self, status, body):
        self.status = status
        self._body = body

    def json(self):
        if self._body is None:
            raise ValueError("no body")
        return self._body

    def body(self):
        return b""


class Request:
    """The context's `request` API: the roster answers 200 to TOKEN only."""

    def __init__(self, page=None):
        self.page = page
        self.gets = []

    def get(self, url, headers=None):
        self.gets.append((url, self.page.ticks if self.page else None))
        if headers.get("Authorization") == f"Bearer {TOKEN}":
            return Resp(200, {"accounts": []})
        return Resp(401, None)


class Context:
    def __init__(self, page=None):
        self.request = Request(page)
        self.handlers = {}

    def on(self, event, handler):
        self.handlers[event] = handler


class Locator:
    def __init__(self, page, selector):
        self.page = page
        self.selector = selector

    @property
    def first(self):
        return self

    def _el(self):
        return self.page.elements().get(self.selector)

    def count(self):
        return 1 if self._el() is not None else 0

    def is_visible(self):
        return self._el() is not None

    def inner_text(self):
        return self._el() or ""

    def click(self, timeout=None):
        self.page.clicked.append(self.selector)
        if self.selector == george.SEL_SUBMIT:
            self.page.state = "push"
            self.page.push_at = self.page.ticks

    def fill(self, value):
        self.page.typed = value

    def input_value(self):
        return self.page.typed

    def press(self, key):
        pass


class SignInPage:
    """Steps through the sign-in as the event loop is pumped: the form, a
    push wait screen, then (after `approve_after` ticks) the redirect — or
    an error text instead."""

    def __init__(self, *, approve_after=3, error=None):
        self.state = "blank"
        self.url = "about:blank"
        self.ticks = 0
        self.push_at = None
        self.approve_after = approve_after
        self.error = error
        self.typed = ""
        self.clicked = []

    def goto(self, url, **kw):
        self.state = "form"
        self.url = "https://login.sparkasse.at/sts/oauth/authorize?x=1"

    def reload(self, **kw):
        pass

    def wait_for_timeout(self, ms):
        self.ticks += 1
        if (self.state == "push" and self.error is None
                and self.ticks - self.push_at >= self.approve_after):
            self.state = "done"
            self.url = REDIRECT

    def locator(self, selector):
        return Locator(self, selector)

    def elements(self):
        if self.state == "form":
            return {george.SEL_USER: "", george.SEL_SUBMIT: "",
                    george.SEL_COOKIE_CLOSE: "Close"}
        if self.state == "push":
            if self.error:
                return {"#error": self.error}
            return {george.SEL_VERIFICATION_CODE: "Q7X2"}
        return {}


def _args(tmp_path, **kw):
    env = tmp_path / "erstebank_at.env"
    env.write_text(f"ERSTEBANK_AT_USERNAME='{USER}'\n")
    return SimpleNamespace(env_file=env, debug=False,
                           screenshot_dir=tmp_path, **kw)


@pytest.fixture(autouse=True)
def _no_user_env(monkeypatch):
    monkeypatch.delenv(login.USER_ENV, raising=False)


# --- argument parsing -----------------------------------------------------

def test_parse_args_defaults():
    args = login.parse_args([])
    assert args.profile_dir == Path("/secrets/erstebank_at-profile")
    assert args.env_file == Path("/secrets/erstebank_at.env")
    assert args.bronze_dir is None
    assert args.cli_mfa is True and args.check is False
    assert args.mfa_timeout == login.MFA_TIMEOUT_S == 300
    assert args.lookback is None and args.verbose is False


def test_parse_args_download_flags():
    args = login.parse_args(["--bronze-dir", "/data", "--lookback", "1y",
                             "--dry-run", "--no-documents", "--no-cli-mfa",
                             "--fresh", "-v"])
    assert args.bronze_dir == Path("/data") and args.lookback == "1y"
    assert args.dry_run and args.no_documents and args.fresh
    assert args.cli_mfa is False


def test_no_password_flag_exists():
    with pytest.raises(SystemExit):
        login.parse_args(["--password", "x"])


def _no_browser(*a, **kw):
    raise AssertionError("a browser was started")


def test_a_sign_in_without_a_bronze_dir_is_refused(monkeypatch):
    # No session survives the run, so a sign-in that fetches nothing would
    # fire a push for nothing: refused before any browser starts.
    monkeypatch.setattr(login, "camoufox", _no_browser)
    assert login.main([]) == 2


def test_check_with_fresh_is_refused(monkeypatch):
    monkeypatch.setattr(login, "camoufox", _no_browser)
    assert login.main(["--check", "--fresh"]) == 2


# --- the bearer harvest and the probe -------------------------------------

def test_watch_harvests_the_api_authorization_header_only():
    watch = login.Watch()
    other = SimpleNamespace(url="https://tools.sparkasse.at/x",
                            headers={"authorization": "Bearer OTHER"})
    watch._on_request(other)
    assert watch.token is None
    api = SimpleNamespace(url="https://api.sparkasse.at/rest/x",
                          headers={"authorization": f"Bearer {TOKEN}"})
    watch._on_request(api)
    assert watch.token == TOKEN


def test_watch_reads_the_redirect_and_the_frame_url():
    watch = login.Watch()
    resp = SimpleNamespace(url="https://login.sparkasse.at/sts/oauth/authorize",
                           status=302, headers={"location": REDIRECT},
                           request=SimpleNamespace(method="GET"))
    watch._on_response(resp)
    assert watch.token == TOKEN

    watch = login.Watch()
    watch._on_frame(SimpleNamespace(url=REDIRECT))
    assert watch.token == TOKEN


def test_watch_records_the_push_status_from_post_only():
    watch = login.Watch()
    preflight = SimpleNamespace(
        url="https://login.sparkasse.at/sts/secapp/secondfactor?client_id=x",
        status=200, headers={}, request=SimpleNamespace(method="OPTIONS"),
        json=lambda: {"secondFactorStatus": "DONE"})
    watch._on_response(preflight)
    assert watch.push_status is None
    poll = SimpleNamespace(
        url=preflight.url, status=200, headers={},
        request=SimpleNamespace(method="POST"),
        json=lambda: {"secondFactorStatus": "PENDING"})
    watch._on_response(poll)
    assert watch.push_status == "PENDING"


def test_probe_needs_a_token_that_reads_the_roster():
    ctx = Context()
    assert login.probe(ctx, None) is False
    assert ctx.request.gets == []
    assert login.probe(ctx, "WRONG") is False
    assert login.probe(ctx, TOKEN) is True


# --- the terminal sign-in -------------------------------------------------

def test_terminal_sign_in_announces_the_code_and_returns_the_bearer(
        tmp_path, caplog):
    page = SignInPage(approve_after=3)
    ctx = Context(page)
    caplog.set_level(logging.INFO, logger="erstebank_at.login")
    token = login.authenticate(ctx, page, login.Watch(), args=_args(tmp_path),
                               cli_mfa=True, mfa_timeout=30)
    assert token == TOKEN
    assert page.typed == USER
    assert george.SEL_SUBMIT in page.clicked
    assert "Comparison code: Q7X2" in caplog.text
    # No data-API call before the approval put a bearer in the URL.
    approved_at = page.push_at + page.approve_after
    assert ctx.request.gets
    assert all(tick >= approved_at for _, tick in ctx.request.gets)


def test_the_pages_error_text_ends_the_sign_in_verbatim(tmp_path, caplog):
    page = SignInPage(error="Synthetic error text from the sign-in page.")
    ctx = Context(page)
    token = login.authenticate(ctx, page, login.Watch(), args=_args(tmp_path),
                               cli_mfa=True, mfa_timeout=30)
    assert token is None
    assert "Synthetic error text from the sign-in page." in caplog.text
    assert ctx.request.gets == []


def test_a_missing_user_name_stops_before_the_page_opens(tmp_path):
    page = SignInPage()
    args = _args(tmp_path)
    args.env_file.write_text("")
    assert login.authenticate(Context(page), page, login.Watch(), args=args,
                              cli_mfa=True, mfa_timeout=30) is None
    assert page.state == "blank"


def test_a_candidate_that_fails_the_probe_is_dropped(tmp_path):
    page = SignInPage()
    page.url = "https://george.sparkasse.at/index.html#access_token=STALE"
    watch = login.Watch()

    def tick(ms):
        page.ticks += 1
        if page.ticks == 3:
            page.url = REDIRECT

    page.wait_for_timeout = tick
    assert login._wait_for_token(Context(page), page, watch, 30,
                                 watch_errors=True) == TOKEN


def test_no_approval_times_out(tmp_path, caplog):
    page = SignInPage(approve_after=10 ** 9)
    token = login.authenticate(Context(page), page, login.Watch(),
                               args=_args(tmp_path), cli_mfa=True,
                               mfa_timeout=1)
    assert token is None
    assert "no working session within 1s" in caplog.text
