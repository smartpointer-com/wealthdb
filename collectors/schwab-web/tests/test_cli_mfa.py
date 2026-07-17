"""
CLI-MFA outcome handling: each code is submitted when entered and the
outcome verified — rejections log Schwab's own on-page error and
re-prompt (a code is never submitted twice); a dead challenge aborts
fast instead of entering the long post-auth wait.
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


# ============================================================
# _verify_mfa_outcome
# ============================================================

def _outcome(monkeypatch, url, input_visible, iframe_visible,
             budget_s=0.5):
    monkeypatch.setattr(login, "_live_url", lambda p: url)
    page = MagicMock()
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
          codes=("111111", "222222", "333333", "444444")):
    calls = {"prompts": 0, "submitted_codes": []}
    monkeypatch.setattr(login, "_submit_login_form", lambda p: True)
    monkeypatch.setattr(login, "_wait_for_mfa_input",
                        lambda p, timeout_s: ("page :: #code", MagicMock()))

    codes_it = iter(codes)

    def prompt():
        calls["prompts"] += 1
        return next(codes_it)

    monkeypatch.setattr(login, "_prompt_for_mfa_code", prompt)

    submits_it = iter(submit_results or ["clicked"] * 8)

    def submit_mfa(p, l, c):
        calls["submitted_codes"].append(c)
        return next(submits_it)

    monkeypatch.setattr(login, "_submit_mfa_code", submit_mfa)
    outcomes_it = iter(outcomes)
    monkeypatch.setattr(login, "_verify_mfa_outcome",
                        lambda p, l: next(outcomes_it))
    monkeypatch.setattr(login, "_visible_error_text", lambda p: "")
    monkeypatch.setattr(login, "_continue_button_present", lambda p: True)
    monkeypatch.setattr(login.time, "sleep", lambda s: None)
    return calls


def test_clean_landing_needs_one_code(monkeypatch):
    calls = _wire(monkeypatch, outcomes=["auth"])
    assert login._run_cli_mfa(MagicMock(), None) == "ok"
    assert calls["prompts"] == 1


def test_rejection_reprompts_with_a_new_code(monkeypatch):
    calls = _wire(monkeypatch, outcomes=["rejected", "auth"])
    assert login._run_cli_mfa(MagicMock(), None) == "ok"
    assert calls["submitted_codes"] == ["111111", "222222"]


def test_exhausted_attempts_abort(monkeypatch):
    calls = _wire(monkeypatch,
                  outcomes=["rejected"] * login.MFA_CODE_ATTEMPTS)
    assert login._run_cli_mfa(MagicMock(), None) == "abort"
    assert calls["prompts"] == login.MFA_CODE_ATTEMPTS
    assert len(set(calls["submitted_codes"])) == login.MFA_CODE_ATTEMPTS


def test_login_form_back_aborts(monkeypatch):
    calls = _wire(monkeypatch, outcomes=["login"])
    assert login._run_cli_mfa(MagicMock(), None) == "abort"
    assert calls["prompts"] == 1


def test_pending_defers_to_the_long_wait(monkeypatch):
    calls = _wire(monkeypatch, outcomes=["pending"])
    assert login._run_cli_mfa(MagicMock(), None) == "ok"
    assert calls["prompts"] == 1


def test_empty_code_aborts(monkeypatch):
    calls = _wire(monkeypatch, outcomes=[], codes=("",))
    assert login._run_cli_mfa(MagicMock(), None) == "abort"
    assert calls["submitted_codes"] == []


def test_unsubmittable_form_falls_back_to_manual(monkeypatch):
    _wire(monkeypatch, outcomes=[], submit_results=[None])
    assert login._run_cli_mfa(MagicMock(), None) == "manual"


def test_vanished_continue_button_aborts_without_burning_codes(monkeypatch):
    # After a rejection the identity-verification lock removes the
    # Continue button; Enter submits nothing there, so further
    # prompts would only waste codes.
    calls = _wire(monkeypatch, outcomes=["rejected"])
    monkeypatch.setattr(login, "_continue_button_present", lambda p: False)
    assert login._run_cli_mfa(MagicMock(), None) == "abort"
    assert calls["prompts"] == 1


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
    _wire(monkeypatch, outcomes=["rejected", "auth"])
    monkeypatch.setattr(login, "_visible_error_text",
                        lambda p: "The code entered is not valid.")
    with caplog.at_level(_logging.WARNING, logger="schwab-web.login"):
        assert login._run_cli_mfa(MagicMock(), None) == "ok"
    assert any("Schwab reports:\n  - The code entered is not valid."
               in r.message for r in caplog.records)
