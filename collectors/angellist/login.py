#!/usr/bin/env python3
"""angellist — there is no automated login.

AngelList's venture SPA login (`venture.angellist.com/v/login`) is gated by
an invisible anti-bot challenge (Cloudflare Turnstile + Google reCAPTCHA)
that scores any automation a bot, so this collector never types credentials
or drives the login form. Authentication is **byo-login**: a human logs into
a genuine, un-instrumented Firefox (which clears the challenge) and
`download` reuses the lifted session cookie. See CLAUDE.md §3 / DESIGN.md.

This command is kept for CLI symmetry; it points you at byo-login.
"""
from __future__ import annotations

import sys


def main(argv: list[str]) -> int:
    print(
        "angellist has no automated login (the SPA login is bot-walled).\n"
        "Authenticate with:  ./angellist byo-login\n"
        "  — log into the real Firefox window by hand (+2FA); on close the\n"
        "    session cookie is lifted to ~/.secrets/angellist-cookies.json,\n"
        "    which `./angellist download` injects.",
        file=sys.stderr,
    )
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
