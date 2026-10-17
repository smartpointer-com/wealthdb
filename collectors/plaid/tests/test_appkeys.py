"""Tests for appkeys: which secret each environment reads, and how each
verb sources the env file a run names."""
from __future__ import annotations

import importlib

import pytest
from collectorkit import envfile

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


class _Sourced(Exception):
    """Stops a verb at its env-file step."""


# The arguments each verb needs to get past argparse.
VERB_ARGV = {"download": ["--bronze-dir", "unused"],
             "link": ["--item", "bank"], "login": []}


@pytest.mark.parametrize("verb", sorted(VERB_ARGV))
def test_every_verb_sources_the_named_env_file_first(
        verb, tmp_path, monkeypatch):
    # --env-file, else $PLAID_ENV_FILE, is sourced before the keys are
    # resolved; collectorkit.envfile owns how the file is read.
    calls = []

    def record(explicit, env_var):
        calls.append((explicit, env_var))
        raise _Sourced

    monkeypatch.setattr(envfile, "source_named_env_file", record)
    env = tmp_path / "plaid.env"
    with pytest.raises(_Sourced):
        importlib.import_module(verb).main(
            [*VERB_ARGV[verb], "--secrets-dir", str(tmp_path),
             "--env-file", str(env)])
    assert calls == [(env, "PLAID_ENV_FILE")]
