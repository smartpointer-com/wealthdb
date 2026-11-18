"""Tests for login.py's exit statuses (viac).

A failed sign-in step or an HTTP error is a failure (exit 1). A missing or
malformed VIAC_LOGIN is a usage error (exit 2). No request leaves the test:
the client is a stand-in and the env file is never read.

Synthetic credentials only.
"""

from __future__ import annotations

import sys
from pathlib import Path

import httpx
import pytest

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
sys.path.insert(0, str(COLLECTOR))

import login  # noqa: E402
from collectorkit import cli  # noqa: E402


class _Response:
    def __init__(self, status_code: int, payload=None):
        self.status_code = status_code
        self._payload = payload
        self.text = ""

    def raise_for_status(self) -> None:
        pass

    def json(self):
        return self._payload


class _Client:
    """Bootstraps, then rejects the password check."""

    csrf_cookie_name = "CSRFT000-S"
    csrf_header_name = "X-CSRFT000"

    def __enter__(self):
        return self

    def __exit__(self, *exc) -> bool:
        return False

    def _ensure_csrf_known(self) -> None:
        pass

    def get(self, path, **kw):
        return _Response(200)

    def delete(self, path, **kw):
        return _Response(204)

    def post(self, path, **kw):
        return _Response(401, {"errors": [{"code": "USERNAME_PASSWORD_WRONG"}]})


@pytest.fixture()
def env(monkeypatch, tmp_path):
    monkeypatch.setattr(login.envfile, "load", lambda *a, **kw: None)
    monkeypatch.setenv(login.LOGIN_ENV, "+41000000000")
    monkeypatch.setenv(login.PASSWORD_ENV, "not-a-real-password")
    return ["--state-path", str(tmp_path / "viac-state.json")]


def test_a_rejected_sign_in_step_is_a_failure(env, monkeypatch):
    monkeypatch.setattr(login, "ViacClient", _Client)
    assert login.main(env) == cli.EXIT_FAILED


def test_an_http_error_is_a_failure(env, monkeypatch):
    def broken(*a, **kw):
        raise httpx.ConnectError("no route")

    monkeypatch.setattr(login, "mint_session", broken)
    assert login.main(env) == cli.EXIT_FAILED


def test_a_missing_login_is_a_usage_error(env, monkeypatch):
    monkeypatch.delenv(login.LOGIN_ENV)
    assert login.main(env) == cli.EXIT_USAGE


def test_a_login_without_a_country_code_is_a_usage_error(env, monkeypatch):
    monkeypatch.setenv(login.LOGIN_ENV, "0000000000")
    assert login.main(env) == cli.EXIT_USAGE
