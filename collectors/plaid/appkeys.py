"""
The app keys: where each verb finds them, and the client they make.

The client id is the same in both environments. Each environment has its
own secret. The secrets reach a verb through the process environment
only: the wrapper sources <secrets-dir>/plaid.env, and --env-file (or
PLAID_ENV_FILE) names another file for a direct run. The client id is an
identifier, not a secret, so --client-id can also pass it.
"""

from __future__ import annotations

import argparse
import os
from pathlib import Path

from collectorkit import envfile

import plaidapi

# Each environment's secret, by the variable that holds it.
SECRET_ENV = {"production": "PLAID_SECRET", "sandbox": "PLAID_SANDBOX_SECRET"}


def add_args(parser: argparse.ArgumentParser, script: str) -> None:
    """The flags every verb that talks to Plaid shares."""
    parser.add_argument(
        "--secrets-dir", type=Path, default=Path.home() / ".secrets",
        help="Where the Items' token files live (default ~/.secrets).")
    parser.add_argument(
        "--sandbox", action="store_true",
        help="Use Plaid's Sandbox: test institutions, test data, the "
             "PLAID_SANDBOX_SECRET, and only the Items made there.")
    parser.add_argument(
        "--env-file", type=Path, default=None,
        help="Credentials env file, sourced before the keys are resolved "
             "(also PLAID_ENV_FILE). The wrapper already sources "
             f"<secrets-dir>/plaid.env; this is for running {script} "
             "directly.")
    parser.add_argument(
        "--client-id", default=None,
        help="Plaid client id. Falls back to PLAID_CLIENT_ID. The secrets "
             "are read from the environment only.")


def environment(args: argparse.Namespace) -> str:
    return "sandbox" if args.sandbox else "production"


def source_env_file(explicit: Path | None) -> None:
    """Source --env-file (or $PLAID_ENV_FILE) so its values win. A path
    that was asked for and is absent is an error, not a fallback to
    whatever the environment happens to hold."""
    named = explicit or (Path(os.environ["PLAID_ENV_FILE"])
                         if os.environ.get("PLAID_ENV_FILE") else None)
    if named is None:
        return
    if not named.is_file():
        raise SystemExit(f"--env-file does not exist: {named}")
    try:
        envfile.source_env_file(named, prefer_file=True)
    except ValueError as e:
        raise SystemExit(str(e)) from e


def make_client(environment: str, client_id: str | None) -> plaidapi.Client:
    """The client for one environment, from the keys in the process
    environment."""
    resolved_id = envfile.resolve_credential(
        client_id, "PLAID_CLIENT_ID", "--client-id")
    secret = os.environ.get(SECRET_ENV[environment])
    if not secret:
        raise SystemExit(
            f"Missing credential: set {SECRET_ENV[environment]} in the env "
            f"file. It is the {environment} secret from the Plaid "
            f"Dashboard.")
    return plaidapi.Client(resolved_id, secret, environment)
