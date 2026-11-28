"""Unit tests for the browserless half of explore.py: argument parsing,
the sparkasse.at origin gate, the click-recorder-JS ↔ Python contract, and
how the user name reaches the redactor.

Synthetic values only (no real credentials or account data).
"""
from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import explore  # noqa: E402

USERNAME = "example-user"


# ============================================================
# Argument parsing
# ============================================================

def test_parse_args_defaults():
    args = explore.parse_args([])
    assert args.url == "https://george.sparkasse.at/"
    assert args.profile_dir == Path("/secrets/erstebank_at-profile")
    assert args.env_file == Path("/secrets/erstebank_at.env")
    assert args.debug_dir is None          # → /debug/<UTC-ts>/ at runtime
    assert args.max_duration == 3600       # human-in-the-loop: 1 hour
    assert args.dom_interval == 3          # DOM snapshots on by default
    assert args.trace is False             # opt-in: a trace holds input values
    assert args.fresh is False
    assert args.verbose is False


def test_parse_args_overrides():
    args = explore.parse_args([
        "--url", "https://login.sparkasse.at/",
        "--profile-dir", "/tmp/p",
        "--debug-dir", "/tmp/d",
        "--env-file", "/tmp/e.env",
        "--max-duration", "60",
        "--fresh", "-v",
    ])
    assert args.url == "https://login.sparkasse.at/"
    assert args.profile_dir == Path("/tmp/p")
    assert args.debug_dir == Path("/tmp/d")
    assert args.env_file == Path("/tmp/e.env")
    assert args.max_duration == 60
    assert args.fresh and args.verbose


def test_no_password_flag_exists():
    # Credentials reach the harness via env only (root AGENTS.md §3);
    # a --password flag must never parse.
    with pytest.raises(SystemExit):
        explore.parse_args(["--password", "x"])


# ============================================================
# The sparkasse.at origin gate
# ============================================================

def test_host_gate_accepts_sparkasse_at_and_subdomains():
    for host in ("sparkasse.at", "www.sparkasse.at", "george.sparkasse.at",
                 "login.sparkasse.at", "GEORGE.SPARKASSE.AT"):
        assert explore.HOST_RE.search(host), host


def test_host_gate_rejects_lookalikes_and_other_countries():
    # The collector is namespaced to Austria: George in other countries is
    # a distinct system, so those hosts are rejected along with the usual
    # suffix-spoof lookalikes.
    for host in ("sparkasse.at.evil.example", "notsparkasse.at",
                 "evil-sparkasse.at", "george-sparkasse.at",
                 "sparkasse.de", "csas.cz", "slsp.sk", "erstebank.hr",
                 "erstebank.at", ""):
        assert not explore.HOST_RE.search(host), host


# ============================================================
# Click-recorder JS ↔ Python contract
# ============================================================

def test_js_uses_the_python_event_prefix():
    # The console listener matches on EVENT_PREFIX (trailing space
    # included); the injected JS must emit exactly that sentinel or every
    # event is silently dropped.
    assert json.dumps(explore.EVENT_PREFIX) in explore.CLICK_RECORDER_JS


def test_js_carries_the_python_host_gate():
    assert json.dumps(explore.HOST_RE.pattern) in explore.CLICK_RECORDER_JS


def test_js_redacts_password_values():
    assert "<redacted>" in explore.CLICK_RECORDER_JS
    assert "type === 'password'" in explore.CLICK_RECORDER_JS


def test_user_selector_carries_the_german_hooks():
    for hook in ("verfueger", "benutzer"):
        assert hook in explore.USER_SELECTOR
    assert json.dumps(explore.USER_SELECTOR) in explore.CLICK_RECORDER_JS


def test_code_field_detector_watches_tan_fields():
    assert "name*='tan' i" in explore.OTP_SELECTOR
    assert json.dumps(explore.OTP_SELECTOR) in explore.CLICK_RECORDER_JS


# ============================================================
# The user name feeds the redactor, from env or the env file
# ============================================================

def test_username_comes_from_the_env_file(tmp_path, monkeypatch):
    monkeypatch.delenv(explore.USER_ENV, raising=False)
    env = tmp_path / "erstebank_at.env"
    env.write_text(f"{explore.USER_ENV}='{USERNAME}'\n", encoding="utf-8")
    assert explore.load_username(env) == USERNAME


def test_an_inherited_username_wins_over_the_file(tmp_path, monkeypatch):
    monkeypatch.setenv(explore.USER_ENV, "inherited-user")
    env = tmp_path / "erstebank_at.env"
    env.write_text(f"{explore.USER_ENV}='{USERNAME}'\n", encoding="utf-8")
    assert explore.load_username(env) == "inherited-user"


def test_a_missing_username_is_loud(tmp_path, monkeypatch, caplog):
    monkeypatch.delenv(explore.USER_ENV, raising=False)
    assert explore.load_username(tmp_path / "absent.env") == ""
    assert explore.USER_ENV in caplog.text


def test_the_username_is_masked_in_captures():
    from collectorkit import debugcap
    redact = debugcap.secret_redactor(USERNAME)
    body = json.dumps({"user": USERNAME})
    assert USERNAME not in redact(body)
    assert USERNAME not in redact(f"/identify?user={USERNAME}")
