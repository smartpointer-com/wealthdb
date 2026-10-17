"""Unit tests for the Chase 2FA CLI dialog (auth_dialog.py).

Browserless and networkless: the dialog is driven with scripted
`input_fn` / `output_fn` stand-ins. Synthetic contact ids and masked
phone strings only (never a real id or phone fragment).
"""
from __future__ import annotations

import io
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import auth_dialog as ad  # noqa: E402


def menu(show_all, phones=(), devices=()):
    body = {
        "challengeTokenIdentifier": "TOK",
        "challengeMethodsDisplay": {"showAll": list(show_all)},
        "phoneList": [
            {"contactReferenceIdentifier": cid,
             "last4DgtsPhoneNumber": masked, "smsEnabledIndicator": 1}
            for cid, masked in phones
        ],
        # devices given as (device_id, name) pairs, or bare names for brevity
        "inAppDeviceList": [
            {"oneTimeDeviceIdentifier": d[0], "digitalDeviceName": d[1]}
            if isinstance(d, tuple)
            else {"oneTimeDeviceIdentifier": "2000", "digitalDeviceName": d}
            for d in devices
        ],
    }
    return ad.parse_challenge_options(body)


class Driver:
    """Scripted input_fn/output_fn pair."""

    def __init__(self, *answers):
        self.answers = list(answers)
        self.out = []

    def input_fn(self, prompt=""):
        if not self.answers:
            raise EOFError
        return self.answers.pop(0)

    def output_fn(self, msg=""):
        self.out.append(msg)

    def text(self):
        return "\n".join(self.out)


# ============================================================
# parse_challenge_options
# ============================================================

def test_parse_extracts_factors_phones_devices():
    m = menu(["INAPP", "OTP_SMS", "OTP_VOICE", "CALL_US"],
             phones=[("1001", "xxx-xxx-1111"), ("1002", "xxx-xxx-2222")],
             devices=[("2001", "Example Phone")])
    assert m.token == "TOK"
    assert m.factors == ("INAPP", "OTP_SMS", "OTP_VOICE", "CALL_US")
    assert [p.contact_id for p in m.phones] == ["1001", "1002"]
    assert [p.label for p in m.phones] == ["xxx-xxx-1111", "xxx-xxx-2222"]
    assert [(d.contact_id, d.label) for d in m.devices] == [("2001", "Example Phone")]


def test_parse_tolerates_missing_fields():
    m = ad.parse_challenge_options({})
    assert m.token == ""
    assert m.factors == ()
    assert m.phones == ()
    assert m.offerable() == []


def test_offerable_filters_unusable_factors():
    # CALL_US is never offerable; SMS/voice need a phone; INAPP needs a device.
    assert menu(["CALL_US"]).offerable() == []
    assert menu(["OTP_SMS"]).offerable() == []          # no phone
    assert menu(["INAPP"]).offerable() == []            # no device
    m = menu(["INAPP", "OTP_SMS", "OTP_VOICE", "CALL_US"],
             phones=[("1001", "xxx-xxx-1111")], devices=["Example Phone"])
    # order follows Chase's showAll, CALL_US dropped
    assert m.offerable() == ["INAPP", "OTP_SMS", "OTP_VOICE"]


# ============================================================
# choose_factor
# ============================================================

def test_choose_factor_auto_selects_single_offerable():
    m = menu(["OTP_SMS", "CALL_US"], phones=[("1001", "xxx-xxx-1111")])
    d = Driver()  # no input needed
    assert ad.choose_factor(m, input_fn=d.input_fn, output_fn=d.output_fn) == "OTP_SMS"


def test_choose_factor_prompts_when_multiple():
    m = menu(["INAPP", "OTP_SMS"], phones=[("1001", "xxx-xxx-1111")],
             devices=["Example Phone"])
    d = Driver("2")  # pick OTP_SMS
    assert ad.choose_factor(m, input_fn=d.input_fn, output_fn=d.output_fn) == "OTP_SMS"


def test_choose_factor_raises_when_only_call_us():
    m = menu(["CALL_US"])
    with pytest.raises(ad.ChallengeError):
        ad.choose_factor(m, input_fn=Driver().input_fn, output_fn=Driver().output_fn)


# ============================================================
# choose_phone
# ============================================================

def test_choose_phone_lists_and_selects():
    m = menu(["OTP_SMS"], phones=[("1001", "xxx-xxx-1111"),
                                  ("1002", "xxx-xxx-2222")])
    d = Driver("2")
    p = ad.choose_phone(m, input_fn=d.input_fn, output_fn=d.output_fn)
    assert p.contact_id == "1002"
    # both masked numbers were shown to the human
    assert "xxx-xxx-1111" in d.text() and "xxx-xxx-2222" in d.text()


def test_choose_phone_auto_selects_single():
    m = menu(["OTP_SMS"], phones=[("1001", "xxx-xxx-1111")])
    d = Driver()
    p = ad.choose_phone(m, input_fn=d.input_fn, output_fn=d.output_fn)
    assert p.contact_id == "1001"


def test_choose_phone_raises_without_phones():
    with pytest.raises(ad.ChallengeError):
        ad.choose_phone(menu(["OTP_SMS"]),
                        input_fn=Driver().input_fn, output_fn=Driver().output_fn)


# ============================================================
# read_otp
# ============================================================

def test_read_otp_shows_no_anti_phishing_prefix():
    # Chase's code arrives numeric-only; the prompt must not invent a prefix.
    d = Driver("12345678")
    ad.read_otp(input_fn=d.input_fn, output_fn=d.output_fn)
    assert not any("begins with" in line for line in d.out)


def test_read_otp_accepts_off_length_with_note():
    # A code of unexpected length is still submitted (Chase makes the final
    # call) but the human is warned.
    d = Driver("123456")
    code = ad.read_otp(input_fn=d.input_fn, output_fn=d.output_fn)
    assert code == "123456"
    assert any("digits" in line for line in d.out)


def test_read_otp_never_echoes_code():
    d = Driver("12345678")
    ad.read_otp(input_fn=d.input_fn, output_fn=d.output_fn)
    assert "12345678" not in d.text()


# ============================================================
# the CLI entry point and its --demo replay
# ============================================================
#
# `--demo` is advertised in README.md and DESIGN.md, so it is covered
# here: it is the one path that calls the dialog helpers through their
# DEFAULTS rather than through scripted stand-ins, which is exactly
# where a signature change (a keyword-only argument, a renamed helper)
# goes unnoticed until a human runs it.

def _scripted_input(monkeypatch, *answers):
    """Answer `_demo`'s prompts in order. The dialog helpers bind the
    builtin `input` as a default argument, so the script has to arrive
    through stdin rather than by patching the name."""
    monkeypatch.setattr(sys, "stdin",
                        io.StringIO("".join(a + "\n" for a in answers)))


def test_demo_sms_branch_runs_end_to_end(monkeypatch, capsys):
    # Factor 2 (text me a code), first destination, then the code.
    _scripted_input(monkeypatch, "2", "1", "12345678")
    assert ad.main(["--demo"]) == 0
    out = capsys.readouterr().out
    # It reports the page actions login.py takes; the code is never echoed.
    assert "xxx-xxx-1111" in out
    assert "type the code into the secure field" in out
    assert "12345678" not in out


def test_demo_push_branch_runs_end_to_end(monkeypatch, capsys):
    # Factor 1 (approve in the app), then the "press Enter" acknowledgement —
    # no phone is picked and no code is read on this branch.
    _scripted_input(monkeypatch, "1", "")
    assert ad.main(["--demo"]) == 0
    out = capsys.readouterr().out
    assert "wait for the sign-in to complete" in out
    assert "xxx-xxx-1111" not in out


def test_demo_reports_a_dialog_that_ends(monkeypatch, capsys):
    # Junk at the factor menu exhausts the retries; the demo reports it
    # and exits non-zero rather than raising.
    _scripted_input(monkeypatch, "9", "9", "9")
    assert ad.main(["--demo"]) == 1
    assert "dialog ended" in capsys.readouterr().err


def test_no_args_prints_help(capsys):
    assert ad.main([]) == 0
    assert "--demo" in capsys.readouterr().out
