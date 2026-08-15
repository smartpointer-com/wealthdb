"""Unit tests for auth_dialog — the browserless Q2 access-code 2FA dialog.

Driven with stub input/output callables, so no stdin and no browser.
Synthetic delivery ids / masks only.
"""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import auth_dialog  # noqa: E402
from q2client import AccessCodeTarget  # noqa: E402

SMS = AccessCodeTarget(value="1001", display="Text: (XXX) XXX-XXXX", kind="sms")
VOICE = AccessCodeTarget(value="1002", display="Call: (XXX) XXX-XXXX", kind="voice")


def _io(inputs):
    """Return (input_fn, output_lines) — input_fn pops from `inputs`, raising
    EOFError when exhausted (a piped, closed stdin)."""
    it = iter(inputs)
    out = []

    def input_fn(_prompt=""):
        try:
            return next(it)
        except StopIteration:
            raise EOFError
    return input_fn, out


# ============================================================
# choose_target
# ============================================================

def test_single_target_is_auto_selected():
    input_fn, out = _io([])          # no prompt needed
    chosen = auth_dialog.choose_target((SMS,), input_fn=input_fn,
                                       output_fn=out.append)
    assert chosen is SMS
    assert any("2FA:" in line for line in out)


def test_multi_target_picks_by_number():
    input_fn, out = _io(["2"])
    chosen = auth_dialog.choose_target((SMS, VOICE), input_fn=input_fn,
                                       output_fn=out.append)
    assert chosen is VOICE


def test_multi_target_reprompts_then_gives_up():
    input_fn, out = _io(["9", "x", "0"])
    with pytest.raises(auth_dialog.ChallengeError):
        auth_dialog.choose_target((SMS, VOICE), input_fn=input_fn,
                                  output_fn=out.append)


def test_empty_targets_raise():
    with pytest.raises(auth_dialog.ChallengeError):
        auth_dialog.choose_target(())


def test_targets_without_value_are_dropped():
    empty = AccessCodeTarget(value="", display="broken", kind="sms")
    input_fn, out = _io([])
    # Only VOICE is usable → auto-selected, no prompt consumed.
    chosen = auth_dialog.choose_target((empty, VOICE), input_fn=input_fn,
                                       output_fn=out.append)
    assert chosen is VOICE


def test_eof_on_pick_raises():
    input_fn, out = _io([])          # EOFError on first read
    with pytest.raises(auth_dialog.ChallengeError):
        auth_dialog.choose_target((SMS, VOICE), input_fn=input_fn,
                                  output_fn=out.append)


# ============================================================
# read_otp
# ============================================================

def test_read_otp_strips_spaces_and_dashes():
    input_fn, out = _io(["123 456-78"])
    assert auth_dialog.read_otp(input_fn=input_fn, output_fn=out.append) \
        == "12345678"


def test_read_otp_rejects_non_digits_then_accepts():
    input_fn, out = _io(["abc", "999999"])
    assert auth_dialog.read_otp(input_fn=input_fn, output_fn=out.append) \
        == "999999"


def test_read_otp_eof_raises():
    input_fn, out = _io([])
    with pytest.raises(auth_dialog.ChallengeError):
        auth_dialog.read_otp(input_fn=input_fn, output_fn=out.append)
