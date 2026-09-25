"""
Login-flow guards (see DESIGN.md §3.1's incident note): page
classification gates every advance click, clicks are budgeted and
progress-verified, the 2FA challenge is a hard barrier with at most one
code submission per run, and the gateway's terminal notice route stops
every wait loop immediately. Synthetic URLs and DOM only.
"""

from __future__ import annotations

import sys
import types
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import login  # noqa: E402
import oauth_landmarks as lm  # noqa: E402


CALLBACK = "https://127.0.0.1:8182"
NOTICE = "https://sws-gateway.schwab.com/ui/host/#/information/12345"
PLACEHOLDER = "https://sws-gateway.schwab.com/ui/host/#/placeholder"


# ============================================================
# Fakes: just enough Playwright surface for the classifier
# ============================================================

class FakeLocator:
    def __init__(self, page, present, visible=True):
        self._page = page
        self._present = present
        self._visible = visible
        self.sel = None

    @property
    def first(self):
        return self

    def count(self):
        return 1 if self._present else 0

    def is_visible(self, timeout=None):
        return self._present and self._visible

    def click(self, timeout=None):
        self._page.clicked.append(self.sel)

    def fill(self, value):
        self._page.filled.append(value)

    def press(self, key):
        self._page.pressed.append(key)

    def inner_text(self, timeout=None):
        return self._page.body_text


class FakePage:
    """A page with a URL, visible selectors, visible text snippets, and
    a structural signature that tests mutate to simulate page changes."""

    def __init__(self, url, selectors=(), texts=(), body_text=""):
        self.url = url
        self.selectors = set(selectors)
        self.texts = tuple(texts)
        self.body_text = body_text
        self.struct = "struct-0"
        self.frames = []
        self.clicked = []
        self.filled = []
        self.pressed = []

    def evaluate(self, js):
        if "location.href" in js:
            return self.url
        if "innerText" in js:
            return self.body_text
        if "role=\"alert\"" in js or "role=\\\"alert\\\"" in js \
                or "alerts" in js:
            return {"alerts": [], "other": []}
        return self.struct

    def locator(self, sel):
        loc = FakeLocator(self, sel in self.selectors)
        loc.sel = sel
        return loc

    def get_by_text(self, text, exact=False):
        return FakeLocator(self, any(text in t for t in self.texts))


# ============================================================
# oauth_landmarks URL shapes
# ============================================================

@pytest.mark.parametrize("url", [
    NOTICE,
    "https://sws-gateway-nr.schwab.com/ui/host/#/information/999",
    "https://sws-gateway.schwab.com/ui/host/#/information",
])
def test_notice_route_family_matches(url):
    assert lm.is_gateway_notice_url(url)


@pytest.mark.parametrize("url", [
    PLACEHOLDER,
    CALLBACK + "/?code=synthetic",
    "https://api.schwabapi.com/v1/oauth/authorize?x=1",
    "https://evil.example/#/information/1",
    "",
])
def test_non_notice_urls_do_not_match(url):
    assert not lm.is_gateway_notice_url(url)


def test_looks_locked():
    assert lm.looks_locked("Your account is locked. Please contact us.")
    # The live lockout wording (identity-verification lock).
    assert lm.looks_locked(
        "For your protection, we need to verify your identity before "
        "proceeding. Please call for assistance.")
    # The generic problem-logging-in wording is NOT a lockout marker.
    assert not lm.looks_locked("There's a problem logging you in.")
    assert not lm.looks_locked("Select your Schwab accounts to link")


# ============================================================
# classify_page
# ============================================================

def test_classify_callback():
    page = FakePage(CALLBACK + "/?code=synthetic")
    assert login.classify_page(page, CALLBACK) == "callback"


def test_classify_notice():
    page = FakePage(NOTICE)
    assert login.classify_page(page, CALLBACK) == "notice"


def test_classify_mfa():
    page = FakePage(PLACEHOLDER, selectors={"#placeholderCode"})
    assert login.classify_page(page, CALLBACK) == "mfa"


def test_classify_consent_pages():
    for heading, expected in ((lm.TERMS_HEADING, "terms"),
                              (lm.ACCOUNT_LINK_HEADING, "account-link"),
                              (lm.REVIEW_HEADING, "review")):
        page = FakePage("https://example.schwab.com/consent",
                        texts=[heading])
        assert login.classify_page(page, CALLBACK) == expected


def test_classify_login_form():
    page = FakePage("https://api.schwabapi.com/v1/oauth/authorize",
                    selectors={"#passwordInput"})
    assert login.classify_page(page, CALLBACK) == "login"


def test_classify_unknown():
    page = FakePage("https://sws-gateway.schwab.com/ui/host/#/other")
    assert login.classify_page(page, CALLBACK) == "unknown"


def test_notice_beats_a_lingering_mfa_input():
    page = FakePage(NOTICE, selectors={"#placeholderCode"})
    assert login.classify_page(page, CALLBACK) == "notice"


def test_challenge_beats_a_consent_heading():
    page = FakePage(PLACEHOLDER, selectors={"#placeholderCode"},
                    texts=[lm.TERMS_HEADING])
    assert login.classify_page(page, CALLBACK) == "mfa"


# ============================================================
# AdvanceGuard
# ============================================================

@pytest.fixture()
def clock(monkeypatch):
    t = [0.0]
    monkeypatch.setattr(login.time, "monotonic", lambda: t[0])
    monkeypatch.setattr(login.time, "sleep", lambda s: None)
    return t


def test_guard_allows_click_only_after_page_change(clock):
    guard = login.AdvanceGuard()
    guard.observe("terms", "sig-a")
    assert guard.may_click()
    guard.record_click()
    guard.observe("terms", "sig-a")
    assert not guard.may_click()          # unchanged page: never re-click
    guard.observe("account-link", "sig-b")
    assert guard.may_click()              # page moved: next click allowed


def test_guard_click_budget_is_hard(clock):
    guard = login.AdvanceGuard()
    for n in range(login.AdvanceGuard.CLICK_BUDGET):
        guard.observe("terms", f"sig-{n}")
        assert guard.may_click()
        guard.record_click()
    guard.observe("terms", "sig-final")
    with pytest.raises(login.LoginFlowError, match="budget"):
        guard.may_click()


def test_guard_raises_on_a_stalled_page(clock):
    guard = login.AdvanceGuard()
    guard.observe("unknown", "sig-a")
    clock[0] = login.AdvanceGuard.STALL_LIMIT_S + 1
    with pytest.raises(login.LoginFlowError, match="stopped changing"):
        guard.observe("unknown", "sig-a")


# ============================================================
# _drive_consent: classification-gated clicking
# ============================================================

ADVANCE = "button:has-text('Continue')"


def test_no_click_on_a_challenge_page(clock):
    # The incident shape: 2FA challenge showing, its own Continue button
    # visible and matching the advance candidates. Must never be clicked.
    page = FakePage(PLACEHOLDER, selectors={"#placeholderCode", ADVANCE})
    guard = login.AdvanceGuard()
    for _ in range(5):
        login._drive_consent(page, guard, CALLBACK)
    assert page.clicked == []


def test_no_click_on_login_or_unknown_pages(clock):
    for page in (
        FakePage("https://api.schwabapi.com/v1/oauth/authorize",
                 selectors={"#passwordInput", ADVANCE}),
        FakePage("https://sws-gateway.schwab.com/ui/host/#/other",
                 selectors={ADVANCE}),
    ):
        login._drive_consent(page, login.AdvanceGuard(), CALLBACK)
        assert page.clicked == []


def test_consent_page_clicked_once_until_it_changes(clock):
    page = FakePage("https://example.schwab.com/consent",
                    texts=[lm.TERMS_HEADING], selectors={ADVANCE})
    guard = login.AdvanceGuard()
    for _ in range(4):
        login._drive_consent(page, guard, CALLBACK)
    assert page.clicked == [ADVANCE]      # one click, not one per poll
    page.struct = "struct-1"              # the click finally took effect
    page.texts = (lm.ACCOUNT_LINK_HEADING,)
    login._drive_consent(page, guard, CALLBACK)
    assert page.clicked == [ADVANCE, ADVANCE]


# ============================================================
# _wait_for_mfa_input / _wait_for_callback terminal states
# ============================================================

def test_wait_for_mfa_input_raises_on_notice(clock):
    page = FakePage(NOTICE, selectors={"#msgLabel"},
                    body_text="Account locked. Please call.")
    with pytest.raises(login.TerminalPageError) as ei:
        login._wait_for_mfa_input(page, 5, CALLBACK)
    assert ei.value.locked
    assert "Account locked. Please call." in ei.value.page_text


def test_notice_message_falls_back_to_body_text(clock, monkeypatch):
    # No message container ever renders: after the settle window the
    # whole-body text is still reported verbatim.
    page = FakePage(NOTICE, body_text="Some notice text.")

    def tick():
        clock[0] += 1.0
        return clock[0]
    monkeypatch.setattr(login.time, "monotonic", tick)
    assert login._notice_message(page) == "Some notice text."


def test_wait_for_mfa_input_returns_mfa_locator(clock):
    page = FakePage(PLACEHOLDER, selectors={"#placeholderCode"})
    state, loc = login._wait_for_mfa_input(page, 5, CALLBACK)
    assert state == "mfa" and loc is not None


def test_wait_for_mfa_input_reports_a_skipped_challenge(clock):
    page = FakePage("https://example.schwab.com/consent",
                    texts=[lm.TERMS_HEADING])
    assert login._wait_for_mfa_input(page, 5, CALLBACK) == ("consent", None)


def test_wait_for_mfa_input_timeout_is_terminal(clock, monkeypatch):
    # A timeout must never be read as "no 2FA needed".
    page = FakePage("https://sws-gateway.schwab.com/ui/host/#/other")

    def tick():
        clock[0] += 1.0
        return clock[0]
    monkeypatch.setattr(login.time, "monotonic", tick)
    with pytest.raises(login.LoginFlowError, match="unrecognized"):
        login._wait_for_mfa_input(page, 3, CALLBACK)


def test_wait_for_callback_raises_on_notice(clock):
    page = FakePage(NOTICE, selectors={"#msgLabel"},
                    body_text="Account locked.")
    with pytest.raises(login.TerminalPageError):
        login._wait_for_callback(page, [], CALLBACK, timeout_s=5,
                                 drive=True)


# ============================================================
# _attempt_cli_mfa: one credential submit, at most one 2FA submit
# ============================================================

def _args(**over):
    ns = types.SimpleNamespace(mfa_page_timeout=5.0, callback_url=CALLBACK)
    for k, v in over.items():
        setattr(ns, k, v)
    return ns


def _wire(monkeypatch, *, state=("mfa", "LOC"), code="123456",
          outcome="advanced", error_text=""):
    calls = {"clicked": [], "prompts": 0}

    def click_first(page, candidates):
        calls["clicked"].append(tuple(candidates))
        return True

    monkeypatch.setattr(login, "_click_first", click_first)
    loc = FakeLocator(FakePage("x"), True)
    resolved = (state[0], loc if state[1] == "LOC" else None)
    monkeypatch.setattr(login, "_wait_for_mfa_input",
                        lambda p, t, cb: resolved)

    def prompt():
        calls["prompts"] += 1
        return code

    monkeypatch.setattr(login, "_prompt_for_mfa_code", prompt)
    monkeypatch.setattr(login, "_verify_mfa_outcome",
                        lambda p, cb: outcome)
    monkeypatch.setattr(login, "_visible_error_text", lambda p: error_text)
    monkeypatch.setattr(login, "_visible_page_text",
                        lambda p, limit=600: "")
    return calls, loc


def test_happy_path_submits_the_code_once(monkeypatch):
    calls, loc = _wire(monkeypatch)
    login._attempt_cli_mfa(FakePage(PLACEHOLDER), _args())
    # Two clicks total: login submit + MFA continue. Nothing else.
    assert [c[0] for c in calls["clicked"]] == [
        lm.LOGIN_SUBMIT_CANDIDATES[0], lm.MFA_CONTINUE_BUTTON_CANDIDATES[0]]
    assert calls["prompts"] == 1


def test_empty_code_is_terminal_before_any_submission(monkeypatch):
    calls, loc = _wire(monkeypatch, code="")
    with pytest.raises(login.LoginFlowError, match="no 2FA code"):
        login._attempt_cli_mfa(FakePage(PLACEHOLDER), _args())
    assert len(calls["clicked"]) == 1     # only the login submit


def test_rejected_code_is_terminal_with_schwabs_words(monkeypatch):
    calls, loc = _wire(monkeypatch, outcome="rejected",
                       error_text="The code entered is not valid.")
    with pytest.raises(login.LoginFlowError, match="one submission") as ei:
        login._attempt_cli_mfa(FakePage(PLACEHOLDER), _args())
    assert "The code entered is not valid." in ei.value.page_text
    assert calls["prompts"] == 1          # never re-prompted


def test_stuck_challenge_is_terminal(monkeypatch):
    _wire(monkeypatch, outcome="stuck")
    with pytest.raises(login.LoginFlowError, match="did not move"):
        login._attempt_cli_mfa(FakePage(PLACEHOLDER), _args())


def test_trusted_device_skips_the_prompt(monkeypatch):
    calls, _ = _wire(monkeypatch, state=("consent", None))
    login._attempt_cli_mfa(FakePage(PLACEHOLDER), _args())
    assert calls["prompts"] == 0
    assert len(calls["clicked"]) == 1     # only the login submit


# ============================================================
# main(): --cli-mfa refuses a non-TTY stdin before any browser
# ============================================================

class _NoTty:
    def isatty(self):
        return False

    def readline(self):
        return ""


def test_cli_mfa_requires_a_tty(monkeypatch):
    monkeypatch.setattr(login, "source_env_files", lambda e: None)
    monkeypatch.setattr(login, "cmd_login_browser",
                        lambda a: pytest.fail("browser must not open"))
    monkeypatch.setattr(login.sys, "stdin", _NoTty())
    with pytest.raises(SystemExit, match="TTY"):
        login.main([])
