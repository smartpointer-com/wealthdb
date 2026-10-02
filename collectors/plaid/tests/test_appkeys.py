"""Tests for appkeys: which secret each environment reads, and how an
env file is sourced."""
from __future__ import annotations

import os

import pytest

import appkeys


@pytest.mark.parametrize("environment,variable", [
    ("production", "PLAID_SECRET"), ("sandbox", "PLAID_SANDBOX_SECRET")])
def test_each_environment_reads_its_own_secret(
        monkeypatch, environment, variable):
    monkeypatch.setenv("PLAID_CLIENT_ID", "synthetic-id")
    monkeypatch.setenv("PLAID_SECRET", "synthetic-production-secret")
    monkeypatch.setenv("PLAID_SANDBOX_SECRET", "synthetic-sandbox-secret")
    client = appkeys.make_client(environment, None)
    assert client.environment == environment
    assert client._secret == f"synthetic-{environment}-secret"

    monkeypatch.delenv(variable)
    with pytest.raises(SystemExit, match=variable):
        appkeys.make_client(environment, None)


def test_a_missing_client_id_is_named(monkeypatch):
    monkeypatch.delenv("PLAID_CLIENT_ID", raising=False)
    monkeypatch.setenv("PLAID_SECRET", "synthetic-secret")
    with pytest.raises(SystemExit, match="PLAID_CLIENT_ID"):
        appkeys.make_client("production", None)
    assert appkeys.make_client("production", "from-flag")._client_id == (
        "from-flag")


def test_the_env_file_is_sourced_as_a_shell_script_and_wins(
        tmp_path, monkeypatch):
    # Quoting and `export` are the shell's to read, so the file goes
    # through bash; its values win over what the process inherited.
    # Set first, so the fixture restores each one after the file has
    # written over it.
    for name in ("PLAID_CLIENT_ID", "PLAID_SANDBOX_SECRET", "PLAID_PART"):
        monkeypatch.setenv(name, "inherited")
    monkeypatch.delenv("PLAID_ENV_FILE", raising=False)
    env = tmp_path / "plaid.env"
    env.write_text("export PLAID_CLIENT_ID='from $file'\n"
                   "PLAID_PART=synthetic\n"
                   "export PLAID_SANDBOX_SECRET=\"${PLAID_PART}-value\"\n")
    appkeys.source_env_file(env)
    assert os.environ["PLAID_CLIENT_ID"] == "from $file"
    assert os.environ["PLAID_SANDBOX_SECRET"] == "synthetic-value"


def test_the_env_file_variable_is_honoured(tmp_path, monkeypatch):
    env = tmp_path / "plaid.env"
    env.write_text("PLAID_CLIENT_ID=from-variable\n")
    monkeypatch.setenv("PLAID_ENV_FILE", str(env))
    monkeypatch.setenv("PLAID_CLIENT_ID", "inherited")
    appkeys.source_env_file(None)
    assert os.environ["PLAID_CLIENT_ID"] == "from-variable"


def test_a_missing_env_file_is_an_error(tmp_path, monkeypatch):
    monkeypatch.delenv("PLAID_ENV_FILE", raising=False)
    with pytest.raises(SystemExit, match="does not exist"):
        appkeys.source_env_file(tmp_path / "nope.env")


def test_an_env_file_with_a_syntax_error_never_shows_its_line(
        tmp_path, monkeypatch):
    # bash quotes the offending line; that line may hold the secret.
    monkeypatch.delenv("PLAID_ENV_FILE", raising=False)
    env = tmp_path / "plaid.env"
    env.write_text("export PLAID_CLIENT_ID=synthetic-id\n"
                   "export PLAID_SECRET=SYNTHETIC-VALUE)x\n")
    with pytest.raises(SystemExit) as caught:
        appkeys.source_env_file(env)
    message = str(caught.value)
    assert "at line 2" in message
    assert "SYNTHETIC-VALUE" not in message
