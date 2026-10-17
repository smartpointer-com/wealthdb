"""
CLI-MFA outcome handling: the 2FA code is submitted at most ONCE per
run and the outcome verified — a rejection logs Schwab's own on-page
error verbatim and aborts (re-running the command is the fresh start),
and a dead challenge aborts fast instead of entering the long post-auth
wait. The login form gets one submit gesture plus one DOM-verified
fallback click, never more.
"""

from __future__ import annotations

import sys
from pathlib import Path
from unittest.mock import MagicMock

import pytest  # noqa: F401

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import login  # noqa: E402


POST_AUTH = "https://client.schwab.com/app/accounts/summary/"
PLACEHOLDER = "https://sws-gateway-nr.schwab.com/ui/host/#/placeholder"


def _page():
    page = MagicMock()
    page.frames = []
    return page


# ============================================================
# _verify_mfa_outcome
# ============================================================

def _outcome(monkeypatch, url, input_visible, iframe_visible,
             budget_s=0.5):
    monkeypatch.setattr(login, "_live_url", lambda p: url)
    page = _page()
    page.locator.return_value.is_visible.return_value = iframe_visible
    code_input = MagicMock()
    code_input.is_visible.return_value = input_visible
    return login._verify_mfa_outcome(
        page, code_input, budget_s=budget_s, grace_s=0, poll_s=0.05)


def test_outcome_auth(monkeypatch):
    assert _outcome(monkeypatch, POST_AUTH, False, False) == "auth"


def test_outcome_rejected(monkeypatch):
    assert _outcome(monkeypatch, PLACEHOLDER, True, False) == "rejected"


def test_outcome_login_session_expired(monkeypatch):
    assert _outcome(monkeypatch, PLACEHOLDER, False, True) == "login"


def test_outcome_pending(monkeypatch):
    assert _outcome(monkeypatch, PLACEHOLDER, False, False) == "pending"


# ============================================================
# _run_cli_mfa
# ============================================================

def _wire(monkeypatch, outcomes, submit_results=None,
          codes=("111111", "222222"), form_verdict="progress"):
    calls = {"prompts": 0, "submitted_codes": []}
    monkeypatch.setattr(login, "_submit_login_form", lambda p: form_verdict)
    monkeypatch.setattr(login, "_wait_for_mfa_input",
                        lambda p, timeout_s: ("page :: #code", MagicMock()))

    codes_it = iter(codes)

    def prompt():
        calls["prompts"] += 1
        return next(codes_it)

    monkeypatch.setattr(login, "_prompt_for_mfa_code", prompt)

    submits_it = iter(submit_results or ["clicked"] * 4)

    def submit_mfa(_page, _locator, c):
        calls["submitted_codes"].append(c)
        return next(submits_it)

    monkeypatch.setattr(login, "_submit_mfa_code", submit_mfa)
    outcomes_it = iter(outcomes)
    monkeypatch.setattr(login, "_verify_mfa_outcome",
                        lambda _page, _code_input: next(outcomes_it))
    monkeypatch.setattr(login, "_visible_error_text_anywhere", lambda p: "")
    monkeypatch.setattr(login.time, "sleep", lambda s: None)
    return calls


def test_clean_landing_needs_one_code(monkeypatch):
    calls = _wire(monkeypatch, outcomes=["auth"])
    assert login._run_cli_mfa(MagicMock(), None, mfa_wait_s=300) == "ok"
    assert calls["prompts"] == 1


def test_rejection_is_terminal_after_one_submission(monkeypatch):
    calls = _wire(monkeypatch, outcomes=["rejected"])
    assert login._run_cli_mfa(MagicMock(), None, mfa_wait_s=300) == "abort"
    assert calls["prompts"] == 1
    assert calls["submitted_codes"] == ["111111"]


def test_login_form_back_aborts(monkeypatch):
    calls = _wire(monkeypatch, outcomes=["login"])
    assert login._run_cli_mfa(MagicMock(), None, mfa_wait_s=300) == "abort"
    assert calls["prompts"] == 1


def test_pending_defers_to_the_long_wait(monkeypatch):
    calls = _wire(monkeypatch, outcomes=["pending"])
    assert login._run_cli_mfa(MagicMock(), None, mfa_wait_s=300) == "ok"
    assert calls["prompts"] == 1


def test_empty_code_aborts(monkeypatch):
    calls = _wire(monkeypatch, outcomes=[], codes=("",))
    assert login._run_cli_mfa(MagicMock(), None, mfa_wait_s=300) == "abort"
    assert calls["submitted_codes"] == []


def test_unsubmittable_form_falls_back_to_manual(monkeypatch):
    calls = _wire(monkeypatch, outcomes=[], form_verdict="failed")
    assert login._run_cli_mfa(MagicMock(), None, mfa_wait_s=300) == "manual"
    assert calls["prompts"] == 0


def test_login_error_aborts_before_any_mfa(monkeypatch):
    calls = _wire(monkeypatch, outcomes=[], form_verdict="error")
    assert login._run_cli_mfa(MagicMock(), None, mfa_wait_s=300) == "abort"
    assert calls["prompts"] == 0
    assert calls["submitted_codes"] == []


def test_error_text_prefers_alerts_and_dedupes():
    page = MagicMock()
    page.evaluate.return_value = {
        "alerts": ["Please call for assistance.",
                   "Enter a valid 6 digit security code."],
        "other": ["More info about security code ... Close"],
    }
    assert login._visible_error_text(page) == (
        "Please call for assistance.\n"
        "  - Enter a valid 6 digit security code.")
    page.evaluate.return_value = {
        "alerts": [],
        "other": ["Enter a valid 6 digit security code.",
                  "Enter a valid 6 digit security code."],
    }
    assert login._visible_error_text(page) == (
        "Enter a valid 6 digit security code.")


def test_rejection_reports_schwabs_own_words(monkeypatch, caplog):
    import logging as _logging
    _wire(monkeypatch, outcomes=["rejected"])
    monkeypatch.setattr(login, "_visible_error_text_anywhere",
                        lambda p: "The code entered is not valid.")
    with caplog.at_level(_logging.WARNING, logger="schwab-web.login"):
        assert login._run_cli_mfa(MagicMock(), None, mfa_wait_s=300) == "abort"
    assert any("Schwab reports:\n  - The code entered is not valid."
               in r.message for r in caplog.records)


# ============================================================
# _submit_login_form — one gesture + one DOM-verified fallback
# ============================================================

def _form_page(monkeypatch, *, pwd_count=1, progress=(),
               error_text="", pwd_still_present=True,
               button_counts=(1, 1, 1)):
    """A page whose gateway iframe exposes a password field and Log In
    button candidates. `progress` is the sequence of
    _wait_iframe_progress results."""
    page = MagicMock()
    page.frames = []
    gateway = MagicMock()
    page.frame_locator.return_value = gateway

    pwd = MagicMock()
    pwd.count.return_value = pwd_count
    buttons = []
    for c in button_counts:
        b = MagicMock()
        b.count.return_value = c
        buttons.append(b)

    def locate(sel):
        if "password" in sel.lower():
            return pwd
        loc = MagicMock()
        loc.first = buttons[locate.calls % len(buttons)]
        locate.calls += 1
        return loc
    locate.calls = 0
    gateway.locator.side_effect = locate

    progress_it = iter(progress)
    monkeypatch.setattr(login, "_wait_iframe_progress",
                        lambda p, b, timeout_s: next(progress_it, False))
    monkeypatch.setattr(login, "_iframe_url", lambda p: "about:blank")
    monkeypatch.setattr(login, "_visible_error_text_anywhere",
                        lambda p: error_text)
    monkeypatch.setattr(login, "_password_field_present",
                        lambda g: pwd_still_present)
    monkeypatch.setattr(login.time, "sleep", lambda s: None)
    return page, pwd, buttons


def test_enter_that_progresses_is_the_only_gesture(monkeypatch):
    page, pwd, buttons = _form_page(monkeypatch, progress=(True,))
    assert login._submit_login_form(page) == "progress"
    pwd.press.assert_called_once_with("Enter")
    assert not any(b.click.called for b in buttons)


def test_stalled_enter_gets_exactly_one_fallback_click(monkeypatch):
    page, pwd, buttons = _form_page(monkeypatch,
                                       progress=(False, True))
    assert login._submit_login_form(page) == "progress"
    pwd.press.assert_called_once_with("Enter")
    assert sum(1 for b in buttons if b.click.called) == 1


def test_no_progress_after_fallback_never_clicks_again(monkeypatch):
    page, pwd, buttons = _form_page(monkeypatch,
                                       progress=(False, False))
    assert login._submit_login_form(page) == "failed"
    assert sum(b.click.call_count for b in buttons) == 1


def test_on_page_error_is_reported_and_terminal(monkeypatch, caplog):
    import logging as _logging
    page, pwd, buttons = _form_page(
        monkeypatch, progress=(False,),
        error_text="Login information is incorrect.")
    with caplog.at_level(_logging.ERROR, logger="schwab-web.login"):
        assert login._submit_login_form(page) == "error"
    assert not any(b.click.called for b in buttons)
    assert any("Login information is incorrect." in r.message
               for r in caplog.records)


def test_ambiguous_mid_transition_state_never_resubmits(monkeypatch):
    # Enter dispatched, form gone, but the gateway never moved: a second
    # submit could double up, so nothing else may be clicked.
    page, pwd, buttons = _form_page(monkeypatch, progress=(False,),
                                       pwd_still_present=False)
    assert login._submit_login_form(page) == "failed"
    assert not any(b.click.called for b in buttons)


# ============================================================
# --trace on the download path
# ============================================================

def test_download_path_records_and_saves_a_trace(monkeypatch, tmp_path):
    import contextlib

    opened: list = []
    context = MagicMock()
    context.new_page.return_value.goto.side_effect = RuntimeError("stop")

    @contextlib.contextmanager
    def fake_context(profile_dir, trace):
        opened.append(trace)
        yield context

    monkeypatch.setattr(login, "open_camoufox_context", fake_context)
    monkeypatch.delenv(login.LOGIN_ID_ENV, raising=False)
    monkeypatch.delenv(login.PASSWORD_ENV, raising=False)
    rc = login.run_manual(tmp_path / "profile", tmp_path / "debug",
                          bronze_dir=tmp_path / "bronze", mfa_timeout_s=1,
                          trace=True)
    assert rc == 1
    assert opened == [True]
    context.tracing.stop.assert_called_once()


# ============================================================
# run_manual: the --mfa-timeout wait and the exit codes
# ============================================================

def _signed_in_run(monkeypatch, walk, cli_mfa=lambda *a, **k: "ok"):
    """Wire run_manual to a stub browser that signs in at once, with
    `walk` in place of download.walk. Returns run_manual's kwargs."""
    import contextlib

    context = MagicMock()

    @contextlib.contextmanager
    def fake_context(profile_dir, trace):
        yield context

    monkeypatch.setattr(login, "open_camoufox_context", fake_context)
    monkeypatch.setenv(login.LOGIN_ID_ENV, "example-id")
    monkeypatch.setenv(login.PASSWORD_ENV, "example-pass")
    monkeypatch.setattr(login, "_prefill_login_iframe", lambda *a: None)
    monkeypatch.setattr(login, "_run_cli_mfa", cli_mfa)
    monkeypatch.setattr(login, "_wait_for_post_auth",
                        lambda ctx, timeout_s: context.new_page.return_value)
    monkeypatch.setattr(login, "_live_url", lambda p: POST_AUTH)
    monkeypatch.setattr(login.time, "sleep", lambda s: None)
    monkeypatch.setattr(login.download, "walk", walk)


def _run(tmp_path, **kw):
    return login.run_manual(tmp_path / "profile", None,
                            bronze_dir=tmp_path / "bronze",
                            mfa_timeout_s=kw.pop("mfa_timeout_s", 300), **kw)


def test_mfa_timeout_reaches_the_first_mfa_wait(monkeypatch, tmp_path):
    waits = []
    _signed_in_run(monkeypatch, lambda *a, **k: {"status": "complete"},
                   cli_mfa=lambda page, shots, *, mfa_wait_s:
                   waits.append(mfa_wait_s) or "ok")
    assert _run(tmp_path, mfa_timeout_s=42) == 0
    assert waits == [42]


def test_a_dry_run_walk_exits_zero(monkeypatch, tmp_path):
    _signed_in_run(monkeypatch, lambda *a, **k: {"status": "dry-run"})
    assert _run(tmp_path, dry_run=True) == 0


def test_a_walk_that_finds_no_accounts_fails(monkeypatch, tmp_path):
    # walk() returns its summary still in-progress when the account
    # selector yields nothing; that is a failed run, not an empty one.
    _signed_in_run(monkeypatch, lambda *a, **k: {
        "status": "in-progress", "statements": [], "transactions": []})
    assert _run(tmp_path) == 1


def test_an_interrupt_exits_130(monkeypatch, tmp_path):
    def interrupted(*a, **k):
        raise KeyboardInterrupt
    _signed_in_run(monkeypatch, interrupted)
    assert _run(tmp_path) == 130
