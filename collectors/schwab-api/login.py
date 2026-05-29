#!/usr/bin/env python3
"""
Schwab OAuth login helper.

Schwab access tokens last 30 minutes and refresh transparently from a
refresh token. Schwab refresh tokens last 7 days and CANNOT be renewed
programmatically — they require a fresh authorization-code grant via
the user's browser. This script handles that browser dance and writes
the resulting token bundle to a file that `download.py` can consume.

Run this script:
  - once initially, to mint the first token file, or
  - whenever `download.py` reports a refresh failure (i.e. the 7-day
    window has expired), to mint a new one.

Three modes:
  - default (no flag): spins up a local HTTPS server on the callback URL,
    opens the system browser, captures the redirect, exchanges the code.
  - --manual: prints the auth URL; you open it in any browser; after
    approving, paste the redirected URL back into the terminal.
  - --check: loads the existing token file and reports its age and
    estimated remaining refresh-window lifetime. No browser, no network.

Usage:
    login.py --token-path <file> [--callback-url <url>] [--manual | --check]
"""

from __future__ import annotations

import argparse
import json
import logging
import os
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

from collectorkit import session

log = logging.getLogger("schwab-login")

# Schwab refresh tokens are valid for 7 days from issue. We surface this
# as a constant so --check output is consistent with the actual cap.
REFRESH_TOKEN_TTL_SECONDS = 7 * 24 * 3600

DEFAULT_CALLBACK_URL = "https://127.0.0.1:8182"


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__.strip())
    p.add_argument(
        "--token-path",
        type=Path,
        default=Path.home() / ".secrets" / "schwab-api-token.json",
        help="Path to read/write the OAuth token JSON file. "
             "Default: ~/.secrets/schwab-api-token.json.",
    )
    p.add_argument(
        "--client-id",
        default=None,
        help="Schwab OAuth Client ID. Falls back to the SCHWAB_CLIENT_ID "
             "environment variable if omitted.",
    )
    p.add_argument(
        "--client-secret",
        default=None,
        help="Schwab OAuth Client Secret. Falls back to the SCHWAB_CLIENT_SECRET "
             "environment variable if omitted. Avoid passing on the command "
             "line in shared environments — prefer the env var.",
    )
    p.add_argument(
        "--callback-url",
        default=DEFAULT_CALLBACK_URL,
        help=f"OAuth callback URL registered with your Schwab app "
             f"(default: {DEFAULT_CALLBACK_URL}). Must match the value in the "
             f"Schwab developer portal exactly.",
    )
    mode = p.add_mutually_exclusive_group()
    mode.add_argument(
        "--manual",
        action="store_true",
        help="Use the paste-the-URL flow instead of the automated local-HTTPS "
             "server flow. Useful for SSH sessions or headless hosts.",
    )
    mode.add_argument(
        "--check",
        action="store_true",
        help="Inspect the existing token file and report its age. No browser, "
             "no network calls.",
    )
    p.add_argument(
        "-v", "--verbose", action="store_true", help="DEBUG-level logging.",
    )
    return p.parse_args(argv)


def resolve_credential(value: str | None, env_name: str, flag_name: str) -> str:
    """Return the credential value, falling back to the env var.

    Prefers the explicit CLI argument when provided; otherwise reads the
    environment variable. Raises SystemExit if neither is available."""
    if value:
        return value
    env_value = os.environ.get(env_name)
    if env_value:
        return env_value
    raise SystemExit(
        f"Missing credential: pass {flag_name} or set {env_name}. "
        f"Source your Schwab credentials env file before running."
    )


def read_token_creation_time(path: Path) -> datetime | None:
    """Return the issue timestamp of the token in `path`, or None if unknown.

    schwab-py records the token under a `creation_timestamp` key (unix
    seconds). Falls back to the file's mtime if the key is absent."""
    try:
        with path.open("r", encoding="utf-8") as fh:
            blob = json.load(fh)
    except (OSError, json.JSONDecodeError) as exc:
        log.warning("Could not parse token file %s: %s", path, exc)
        return None
    ts = blob.get("creation_timestamp")
    if isinstance(ts, (int, float)):
        return datetime.fromtimestamp(ts, tz=timezone.utc)
    try:
        return datetime.fromtimestamp(path.stat().st_mtime, tz=timezone.utc)
    except OSError:
        return None


def cmd_check(args: argparse.Namespace) -> int:
    if not args.token_path.is_file():
        print(f"No token file at {args.token_path}.")
        print("Run login.py without --check to mint one.")
        return 1
    issued = read_token_creation_time(args.token_path)
    if issued is None:
        print(f"Token file exists at {args.token_path}, "
              f"but its issue timestamp could not be determined.")
        return 2
    now = datetime.now(timezone.utc)
    age = now - issued
    age_s = age.total_seconds()
    remaining_s = REFRESH_TOKEN_TTL_SECONDS - age_s
    print(f"Token file:        {args.token_path}")
    print(f"Issued (UTC):      {issued.isoformat(timespec='seconds')}")
    print(f"Age:               {age}")
    if remaining_s > 0:
        remaining_h = remaining_s / 3600
        renew_by = datetime.fromtimestamp(
            issued.timestamp() + REFRESH_TOKEN_TTL_SECONDS, tz=timezone.utc,
        )
        print(f"Estimated expiry:  {renew_by.isoformat(timespec='seconds')} "
              f"({remaining_h:.1f}h from now)")
        if remaining_h < 24:
            print("WARNING: refresh window expires in under 24 hours. "
                  "Re-run login.py soon.")
        return 0
    print("Estimated expiry:  EXPIRED. Re-run login.py to mint a new token.")
    return 3


def cmd_login(args: argparse.Namespace) -> int:
    try:
        from schwab import auth as schwab_auth  # type: ignore
    except ImportError:
        raise SystemExit(
            "schwab-py is not installed. Run: pip install -r requirements.txt"
        )

    client_id = resolve_credential(args.client_id, "SCHWAB_CLIENT_ID", "--client-id")
    client_secret = resolve_credential(args.client_secret, "SCHWAB_CLIENT_SECRET", "--client-secret")
    args.token_path.parent.mkdir(parents=True, exist_ok=True)

    log.info("Starting %s OAuth flow", "manual" if args.manual else "automated")
    log.info("Callback URL: %s", args.callback_url)
    log.info("Token will be written to: %s", args.token_path)

    # schwab-py's parameter names (api_key, app_secret) are a historical
    # quirk; they accept the OAuth Client ID / Client Secret that Schwab
    # issues in its developer portal.
    if args.manual:
        client = schwab_auth.client_from_manual_flow(
            api_key=client_id,
            app_secret=client_secret,
            callback_url=args.callback_url,
            token_path=str(args.token_path),
        )
    else:
        # interactive=False skips schwab-py's "Press ENTER to open the
        # browser" prompt. In VSCode remote terminals the browser handoff
        # can fail noisily via a stale IPC socket; the ENTER prompt only
        # adds a chance for the auth code to expire before the browser
        # finishes loading.
        client = schwab_auth.client_from_login_flow(
            api_key=client_id,
            app_secret=client_secret,
            callback_url=args.callback_url,
            token_path=str(args.token_path),
            interactive=False,
        )

    # We don't actually use the client here; constructing it has already
    # written the token file as a side effect. Discard.
    del client

    if not args.token_path.is_file():
        log.error("OAuth flow completed but no token file was written to %s",
                  args.token_path)
        return 4
    session.secure_file(args.token_path)
    issued = read_token_creation_time(args.token_path) or datetime.now(timezone.utc)
    renew_by = datetime.fromtimestamp(
        issued.timestamp() + REFRESH_TOKEN_TTL_SECONDS, tz=timezone.utc,
    )
    log.info("Token minted successfully.")
    log.info("Renew by (UTC): %s", renew_by.isoformat(timespec="seconds"))
    return 0


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    return cmd_check(args) if args.check else cmd_login(args)


if __name__ == "__main__":
    sys.exit(main())
