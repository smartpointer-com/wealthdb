"""Tests for explore.py, the hand-driven discovery recorder.

The harness issues no navigation and no clicks of its own, so there is no
route policy to pin here. What is worth pinning is everything that decides
what reaches disk: the redactor that keeps the contract number out of the
logs, the DOM-dedup fingerprint that decides whether a screen is written,
the download naming that keeps two files from colliding, the host test that
selects which frames are captured, and the CLI defaults that keep artefacts
out of bronze.
"""
from __future__ import annotations

from pathlib import Path

import pytest

import explore
from collectorkit import debugcap


# --------------------------------------------------------------------
# Redaction — the contract number must not reach an artefact
# --------------------------------------------------------------------

# collectorkit's SecretRedactorTest owns the redactor's contract — every
# wire spelling, longest-secret-first, the falsy identity. What belongs
# here is only that this collector's own artefacts route through it.


# --------------------------------------------------------------------
# DOM dedup — a screen is written once, not every tick
# --------------------------------------------------------------------

def test_skeleton_ignores_text_and_attribute_values():
    a = '<div id="root"><span class="a">CHF 1.00</span></div>'
    b = '<div id="root"><span class="b">CHF 999.00</span></div>'
    assert explore.dom_skeleton(a) == explore.dom_skeleton(b)


def test_skeleton_tracks_element_ids():
    a = '<div id="cards"></div>'
    b = '<div id="accounts"></div>'
    assert explore.dom_skeleton(a) != explore.dom_skeleton(b)


def test_skeleton_tracks_structure():
    a = "<div><span></span></div>"
    b = "<div><span></span><span></span></div>"
    assert explore.dom_skeleton(a) != explore.dom_skeleton(b)


# --------------------------------------------------------------------
# Frame selection
# --------------------------------------------------------------------

@pytest.mark.parametrize("url", [
    "https://ebanking-ch.ubs.com/workbench/WorkbenchOpenAction.do",
    "https://ebanking-ch3.ubs.com/app/OQJ/1/ebanking/spa.html",
    "https://secure.ubs.com/global/en/legal/country.html",
])
def test_ubs_frames_are_captured(url):
    assert explore.is_ubs_host(url)


@pytest.mark.parametrize("url", [
    "https://example.test/tracker.html",
    "about:blank",
    "",
    # A lookalike host must not match: the anchor is the dot, not a substring.
    "https://ubs.com.example.test/phish",
    "https://notubs.com/x",
])
def test_other_frames_are_not_captured(url):
    assert not explore.is_ubs_host(url)


# --------------------------------------------------------------------
# Download naming
# --------------------------------------------------------------------

def test_download_names_are_sequenced_so_repeats_do_not_collide():
    # UBS reuses one suggested name across accounts and periods.
    first = explore.safe_download_name("statement.pdf", 1)
    second = explore.safe_download_name("statement.pdf", 2)
    assert first != second
    assert first.endswith("statement.pdf")


def test_download_name_strips_path_separators():
    name = explore.safe_download_name("../../etc/passwd", 3)
    assert "/" not in name and ".." not in name


def test_download_name_survives_a_missing_suggestion():
    assert explore.safe_download_name(None, 7).startswith("07-")
    assert explore.safe_download_name("", 8).startswith("08-")


def test_download_name_is_length_capped():
    assert len(explore.safe_download_name("x" * 500, 1)) < 140


# --------------------------------------------------------------------
# Env-file resolution
# --------------------------------------------------------------------

def test_env_file_prefers_an_explicit_path(tmp_path):
    explicit = tmp_path / "custom.env"
    assert explore.resolve_env_file(explicit) == explicit


def test_env_file_returns_none_when_nothing_exists(monkeypatch, tmp_path):
    monkeypatch.setattr(explore, "DEFAULT_ENV_FILE", tmp_path / "a.env")
    monkeypatch.setattr(explore, "LEGACY_ENV_FILE", tmp_path / "b.env")
    assert explore.resolve_env_file(None) is None


def test_env_file_falls_back_to_the_bank_level_file(monkeypatch, tmp_path):
    primary, legacy = tmp_path / "ubs-web.env", tmp_path / "ubs.env"
    legacy.write_text("x=1\n")
    monkeypatch.setattr(explore, "DEFAULT_ENV_FILE", primary)
    monkeypatch.setattr(explore, "LEGACY_ENV_FILE", legacy)
    assert explore.resolve_env_file(None) == legacy


# --------------------------------------------------------------------
# CLI defaults
# --------------------------------------------------------------------

def test_artefacts_default_to_the_debug_mount_not_bronze():
    args = explore.parse_args([])
    assert args.debug_dir is None          # resolved to a stamped /debug subdir
    assert explore.DEFAULT_DEBUG_ROOT == Path("/debug")


def test_the_single_opened_url_is_the_login_entry_point():
    import landmarks
    assert explore.parse_args([]).url == landmarks.LOGIN_ENTRY_URL


def test_state_and_user_agent_are_shared_with_download():
    import download
    assert explore.DEFAULT_STATE_PATH == download.DEFAULT_STATE_PATH
    assert explore.USER_AGENT == download.USER_AGENT


def test_recording_is_time_capped_by_default():
    assert explore.parse_args([]).max_duration > 0


def test_prefill_and_state_saving_are_on_by_default():
    args = explore.parse_args([])
    assert args.no_prefill is False
    assert args.no_save_state is False


def test_trace_is_opt_in():
    assert explore.parse_args([]).trace is False


# --------------------------------------------------------------------
# The init script
# --------------------------------------------------------------------

def test_click_recorder_redacts_secret_fields():
    # The contract-number field is the login's identifying input; its value
    # must never be echoed through the console channel.
    assert "loginalias" in explore.CLICK_RECORDER_JS
    assert "<redacted>" in explore.CLICK_RECORDER_JS


def test_click_recorder_tags_events_with_the_shared_prefix():
    assert explore.EVENT_PREFIX.strip() in explore.CLICK_RECORDER_JS


def test_click_recorder_records_the_qr_challenge_but_not_its_image():
    js = explore.CLICK_RECORDER_JS
    assert "qr-challenge-detected" in js
    # Recording the QR's src would persist the challenge itself.
    assert ".src" not in js


# --------------------------------------------------------------------
# The state save must not clobber a working session
# --------------------------------------------------------------------

class _Ctx:
    def __init__(self, urls):
        self.pages = [type("P", (), {"url": u})() for u in urls]


def test_authenticated_when_a_page_is_on_the_workbench():
    ctx = _Ctx(["https://ebanking-ch3.ubs.com/app/OQJ/1/ebanking/spa.html"])
    assert explore._looks_authenticated(ctx)


def test_not_authenticated_at_the_login_dialog():
    # The login URL differs from the post-auth one only by its query, which
    # is exactly the trap: a state saved here holds no session.
    ctx = _Ctx(["https://ebanking-ch3.ubs.com/workbench/"
                "WorkbenchOpenAction.do?login"])
    assert not explore._looks_authenticated(ctx)


def test_not_authenticated_with_no_pages_left():
    # The browser-closed path: nothing to read, so nothing is claimed.
    assert not explore._looks_authenticated(_Ctx([]))


def test_not_authenticated_when_the_context_is_dead():
    class _Dead:
        @property
        def pages(self):
            raise RuntimeError("target closed")
    assert not explore._looks_authenticated(_Dead())


# --------------------------------------------------------------------
# DOM captures are redacted too
# --------------------------------------------------------------------

def test_dom_capture_redacts_the_contract_number(tmp_path):
    # The contract number rides in the post-auth URL as well as in the
    # login form, so a DOM capture would otherwise carry the one value
    # every other artefact masks.
    contract = "987-654-321"
    redact = debugcap.secret_redactor(contract)

    class _Frame:
        url = "https://ebanking-ch3.ubs.com/app/x/ebanking/spa.html"

        def content(self):
            return f'<html><body><a href="?loginalias={contract}">x</a></body></html>'

    class _Page:
        url = f"https://ebanking-ch3.ubs.com/app/x/spa.html#/home?loginalias={contract}"
        frames = [_Frame()]

        def screenshot(self, **_kw):
            pass

    class _Ctx:
        pages = [_Page()]

    seq, _ = explore.capture_dom_snapshot(_Ctx(), tmp_path, 0, "", redact)
    assert seq == 1
    body = (tmp_path / "001" / "frame0.html").read_text()
    urls = (tmp_path / "001" / "url.txt").read_text()
    assert contract not in body and "<redacted>" in body
    assert contract not in urls and "<redacted>" in urls
