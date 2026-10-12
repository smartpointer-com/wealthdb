"""`login --check`: the token file's age, then a probe of the stored token.

The probe builds the client from the token file the way `download` does
and calls /accounts/accountNumbers. A fake client stands in for
schwab-py, so nothing here touches the network. Synthetic values only.
"""
from __future__ import annotations

import json
import sys
import time
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import _tokenclient  # noqa: E402
import login  # noqa: E402

CLIENT_ID = "synthetic-client-id"
CLIENT_SECRET = "synthetic-client-secret"


class FakeResponse:
    def __init__(self, status_code=200, text="[]"):
        self.status_code = status_code
        self.text = text


class FakeClient:
    def __init__(self, response=None, raises=None):
        self.response = response or FakeResponse()
        self.raises = raises
        self.calls = 0

    def get_account_numbers(self):
        self.calls += 1
        if self.raises is not None:
            raise self.raises
        return self.response


class FakeOAuthError(Exception):
    """Stands in for authlib's OAuth error classes."""


@pytest.fixture
def token(tmp_path):
    path = tmp_path / "schwab-api-token.json"
    path.write_text(json.dumps({
        "creation_timestamp": time.time() - 3600,
        "token": {"refresh_token": "synthetic"},
    }))
    return path


@pytest.fixture
def no_env_files(monkeypatch, tmp_path):
    """Keep the default env files out of the run, so the credentials come
    from the command line only."""
    monkeypatch.setattr(login, "_ENV_FILE_SETS", ())
    monkeypatch.delenv("SCHWAB_API_ENV_FILE", raising=False)
    monkeypatch.delenv("SCHWAB_CLIENT_ID", raising=False)
    monkeypatch.delenv("SCHWAB_CLIENT_SECRET", raising=False)


def _wire(monkeypatch, client):
    built = []

    def factory(token_path, client_id, client_secret):
        built.append((token_path, client_id, client_secret))
        return client
    monkeypatch.setattr(_tokenclient, "client_from_token_file", factory)
    monkeypatch.setattr(_tokenclient, "oauth_error_types",
                        lambda: (FakeOAuthError,))
    return built


def _check(token: Path) -> int:
    return login.main(["--check", "--token-path", str(token),
                       "--client-id", CLIENT_ID,
                       "--client-secret", CLIENT_SECRET])


def test_alive_token_reports_age_then_alive(monkeypatch, token, no_env_files,
                                            capsys):
    client = FakeClient(FakeResponse(200, '[{"accountNumber": "00000001"}]'))
    built = _wire(monkeypatch, client)
    assert _check(token) == 0
    assert built == [(token, CLIENT_ID, CLIENT_SECRET)]
    assert client.calls == 1
    out = capsys.readouterr().out
    assert "Age:" in out
    assert "ALIVE" in out
    # The response lists plain account numbers: never printed.
    assert "00000001" not in out


def test_rejected_token_explains_and_exits_nonzero(monkeypatch, token,
                                                   no_env_files, capsys):
    _wire(monkeypatch, FakeClient(raises=FakeOAuthError(
        "invalid_grant: Refresh token is invalid, expired or revoked")))
    assert _check(token) == login.CHECK_REJECTED
    out = capsys.readouterr().out
    assert "Age:" in out
    assert "invalid_grant" in out
    assert "REJECTED" in out
    assert "ALIVE" not in out


def test_http_error_is_a_failed_probe(monkeypatch, token, no_env_files,
                                      capsys):
    _wire(monkeypatch, FakeClient(FakeResponse(503, "service unavailable")))
    assert _check(token) == login.CHECK_PROBE_FAILED
    out = capsys.readouterr().out
    assert "HTTP 503" in out
    assert "ALIVE" not in out


def test_transport_error_is_a_failed_probe(monkeypatch, token, no_env_files,
                                           capsys):
    _wire(monkeypatch, FakeClient(raises=ConnectionError("unreachable")))
    assert _check(token) == login.CHECK_PROBE_FAILED
    assert "ConnectionError" in capsys.readouterr().out


def test_missing_token_file_probes_nothing(monkeypatch, tmp_path,
                                           no_env_files, capsys):
    built = _wire(monkeypatch, FakeClient())
    assert _check(tmp_path / "absent.json") == login.CHECK_NO_TOKEN
    assert built == []
    assert "No token file" in capsys.readouterr().out


def test_credentials_fall_back_to_the_env(monkeypatch, token, no_env_files):
    built = _wire(monkeypatch, FakeClient())
    monkeypatch.setenv("SCHWAB_CLIENT_ID", CLIENT_ID)
    monkeypatch.setenv("SCHWAB_CLIENT_SECRET", CLIENT_SECRET)
    assert login.main(["--check", "--token-path", str(token)]) == 0
    assert built == [(token, CLIENT_ID, CLIENT_SECRET)]


def test_missing_credentials_exit_before_any_call(monkeypatch, token,
                                                  no_env_files):
    built = _wire(monkeypatch, FakeClient())
    with pytest.raises(SystemExit, match="SCHWAB_CLIENT_ID"):
        login.main(["--check", "--token-path", str(token)])
    assert built == []


def test_expired_estimate_still_defers_to_the_probe(monkeypatch, tmp_path,
                                                    no_env_files, capsys):
    old = tmp_path / "old-token.json"
    old.write_text(json.dumps({
        "creation_timestamp": time.time() - 8 * 24 * 3600, "token": {}}))
    _wire(monkeypatch, FakeClient())
    assert _check(old) == 0
    out = capsys.readouterr().out
    assert "EXPIRED by the 7-day cap" in out
    assert "ALIVE" in out
