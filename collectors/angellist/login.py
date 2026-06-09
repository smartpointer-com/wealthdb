#!/usr/bin/env python3
"""angellist session minter — SCAFFOLD.

Drives the AngelList Investor Portal SPA login form, solves the 2FA
challenge (factor TBD by explore — likely a TOTP authenticator code or
an emailed OTP, read from stdin), and persists the authenticated session
to a Camoufox profile dir so `download` can reuse it.

This file is a stub. When implemented it will mirror the cointracking /
fidelity-web login flow:

  1. Launch a persistent browser context on the profile dir at
     /secrets/angellist-profile/ (Camoufox if explore shows the portal
     bot-checks the login; vanilla Playwright Firefox otherwise).
  2. Navigate to the Investor Portal. If a prior session is still valid,
     short-circuit straight to the dashboard.
  3. Fill credentials (from ANGELLIST_USERNAME / ANGELLIST_PASSWORD),
     submit, wait for the 2FA prompt.
  4. Read the 2FA code from stdin (CLI-MFA), submit, and — if the portal
     offers a "trust this device" option — tick it to extend the
     session lifetime.
  5. context.close() to flush cookies + storage back to the profile.

CLI surface (shared with the other angellist subcommands):

  --profile-dir PATH   Persistent browser profile (default
                       /secrets/angellist-profile/, shared with explore /
                       download). Holds the session + any device-trust
                       cookie. Treat the dir as a credential.
  --env-file PATH      Bash-sourced env with ANGELLIST_USERNAME /
                       ANGELLIST_PASSWORD.
  --check              Probe the existing profile (GET the dashboard),
                       exit 0 if authenticated, 1 if not. No credentials
                       posted, no 2FA push — safe for cron healthchecks.

Auth discipline (root CLAUDE.md §3): never weaken or skip 2FA, never add
a --password flag, never lower the profile dir below 0700.
"""
from __future__ import annotations

import argparse
import sys
from pathlib import Path

from collectorkit import cli

USER_ENV = "ANGELLIST_USERNAME"
PASS_ENV = "ANGELLIST_PASSWORD"

DEFAULT_PROFILE_DIR = Path("/secrets/angellist-profile")
DEFAULT_ENV_FILE = Path("/secrets/angellist.env")


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument(
        "--profile-dir", type=Path, default=DEFAULT_PROFILE_DIR,
        help=("Persistent browser profile dir. Holds the session cookie "
              "and any device-trust value. Treat as a credential. "
              "Default: %(default)s."),
    )
    p.add_argument(
        "--env-file", type=Path, default=DEFAULT_ENV_FILE,
        help=("Bash-sourced env file with ANGELLIST_USERNAME / "
              "ANGELLIST_PASSWORD. Skipped silently if absent. "
              "Default: %(default)s."),
    )
    p.add_argument(
        "--check", action="store_true",
        help=("Probe the stored profile. Exits 0 if authenticated, 1 if "
              "not. No credentials posted, no 2FA push — safe to call "
              "from cron / healthcheck."),
    )
    cli.add_common_args(p)
    return p.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)
    raise SystemExit(
        "angellist login is a SCAFFOLD and is not implemented yet.\n"
        "Run `./angellist explore` first to capture the login + 2FA flow, "
        "then implement this script (mirror "
        "collectors/cointracking/login.py). See DESIGN.md."
    )


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
