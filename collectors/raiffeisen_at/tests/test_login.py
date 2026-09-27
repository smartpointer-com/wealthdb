"""Unit tests for login.py's browserless surface: argument parsing (the
standard fleet flags + the collector's own), credential loading, the OIDC
Bearer harvest, and the auth-state helpers driven against stub objects — no
browser needed.

Synthetic values only.
"""
from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import login  # noqa: E402


# ============================================================
# Argument parsing — the wealthdb-collect / wealthdb-refresh contract
# ============================================================

def test_parse_args_defaults():
    args = login.parse_args([])
    assert args.profile_dir == Path("/secrets/raiffeisen_at-profile")
    assert args.env_file == Path("/secrets/raiffeisen_at.env")
    assert args.bronze_dir is None
    assert args.cli_mfa is True
    assert args.check is False
    assert args.fresh is False
    assert args.dry_run is False
    assert args.no_documents is False
    assert args.mfa_timeout == login.PUSHTAN_TIMEOUT_S
    assert args.lookback is None          # standard download flag present
    assert args.verbose is False


def test_parse_args_download_flags():
    args = login.parse_args([
        "--bronze-dir", "/data", "--lookback", "1y", "--dry-run",
        "--no-documents", "--no-cli-mfa", "--fresh", "-v",
    ])
    assert args.bronze_dir == Path("/data")
    assert args.lookback == "1y"
    assert args.dry_run and args.no_documents and args.fresh and args.verbose
    assert args.cli_mfa is False


def test_no_password_flag_exists():
    # Credentials reach the driver via env only (root AGENTS.md §3).
    import pytest
    with pytest.raises(SystemExit):
        login.parse_args(["--password", "x"])


def test_lookback_rejects_garbage():
    import pytest
    with pytest.raises(SystemExit):
        login.parse_args(["--lookback", "banana"])


# ============================================================
# Credential loading — the three env vars (username / PIN / region)
# ============================================================

def test_load_credentials_reads_all_three(monkeypatch, tmp_path):
    monkeypatch.setenv(login.USER_ENV, "3V000042")
    monkeypatch.setenv(login.PASS_ENV, "12345")
    monkeypatch.setenv(login.REGION_ENV, "rbgooe")
    user, pwd, region = login._load_credentials(tmp_path / "absent.env")
    assert (user, pwd, region) == ("3V000042", "12345", "rbgooe")


# ============================================================
# Bearer harvest + auth headers
# ============================================================

class _Req:
    def __init__(self, url, headers):
        self.url = url
        self.headers = headers


def test_watch_harvests_bearer_from_api_request():
    w = login._Watch()
    # A non-API request is ignored.
    w._on_request(_Req("https://mein.elba.raiffeisen.at/assets/x.js",
                       {"authorization": "Bearer nope"}))
    assert w.bearer is None
    # An /api/ request's Bearer is captured.
    w._on_request(_Req("https://mein.elba.raiffeisen.at/api/x/rest/produkte",
                       {"authorization": "Bearer TOK123"}))
    assert w.bearer == "TOK123"


def test_api_headers_includes_bearer_when_present():
    w = login._Watch()
    assert "Authorization" not in login.api_headers(w)
    w.bearer = "TOK123"
    h = login.api_headers(w)
    assert h["Authorization"] == "Bearer TOK123"
    assert h["Accept"] == "application/json"


def test_watch_harvests_bearer_from_token_response():
    # The token exchange's own response body is the authoritative Bearer
    # source (in case the per-request header harvest misses it).
    class _Resp:
        url = "https://sso.raiffeisen.at/as/token.oauth2"

        class request:
            method = "POST"

        @staticmethod
        def json():
            return {"access_token": "FROMTOKEN", "id_token": "x"}
    w = login._Watch()
    w._on_response(_Resp())
    assert w.bearer == "FROMTOKEN"


# ============================================================
# Auth-state detection — the Bearer is the gate, never the shared URL
# ============================================================

def test_probe_authenticated_requires_a_bearer():
    # Without a harvested Bearer the probe never fires (produkte 401s on
    # cookies alone — the live 2026-08-15 failure), so it is False before
    # any token is in hand — no context call is even attempted.
    w = login._Watch()
    assert w.bearer is None
    assert login._probe_authenticated(context=None, watch=w) is False