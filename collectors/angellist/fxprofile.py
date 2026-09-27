#!/usr/bin/env python3
"""Seed the stock-Firefox profile the `login` verb signs into.

Part of the BYO-session path: AngelList's venture login is gated by an
invisible Turnstile / reCAPTCHA challenge that flags any automation stack
(see AGENTS.md §3), so `login` starts a genuine, un-instrumented Firefox
binary under Xvfb + VNC for a by-hand sign-in and lifts the cookie jar
afterwards. A plain binary takes no Playwright `firefox_user_prefs`; it
reads `<profile>/user.js` at startup.

So the prefs are rendered from the shared `collectorkit.launch` set — the
same one the driven browsers launch with — plus the AngelList overrides
below. Rendering from the shared set keeps the entrypoint out of the
business of hand-writing prefs, keeps the profile session-state-sized
(the disk cache stays capped), and prevents pref drift.

Nothing here alters the fingerprint web content can observe — that is the
whole point of the stock-Firefox path. Firefox's own blocklist data
(`security_state/`, `safebrowsing/`) is deliberately left to populate: it
is regenerable, but it is also part of looking like a real browser.
"""
from __future__ import annotations

import argparse
import logging
import sys
from pathlib import Path

from collectorkit import cli, launch

log = logging.getLogger("angellist.fxprofile")

DEFAULT_PROFILE_DIR = Path("/secrets/angellist-fxprofile")

# Downloads (K-1 CSV/PDF, financial statements) land in the mounted
# documents dir (= angellist-documents/ in the wealthdb data dir on the
# host) rather than the container-ephemeral ~/Downloads, so anything
# grabbed from the Taxes & Documents page survives the run. The document
# endpoints reject the injected cookie, so a real-browser download during
# the by-hand login is the way to get them onto the host.
DEFAULT_DOWNLOAD_DIR = Path("/data/angellist-documents")

# Content types saved straight to disk rather than opening a dialog no one
# is watching, with the built-in PDF viewer out of the way.
SAVE_TO_DISK = ("text/csv,application/pdf,application/octet-stream,"
                "application/vnd.ms-excel,application/zip")


def angellist_prefs(download_dir: Path) -> dict[str, bool | int | str]:
    """The AngelList overrides layered onto the shared pref set."""
    return {
        # This profile is signed into by hand and reused across the session
        # cookie's ~27-day life, so Firefox's password manager stays on to
        # autofill the saved AngelList login. The shared set disables it for
        # the driven, programmatic profiles, where an autofill would collide
        # with the injected credentials.
        **launch.PASSWORD_MANAGER_ON,

        # Restore the session on shutdown, so a session-scoped login cookie
        # is flushed to cookies.sqlite for extract_cookies.py to lift.
        # Load-bearing for the whole BYO-session path.
        "browser.startup.page": 3,

        # Come up on the login page rather than onboarding.
        "browser.aboutwelcome.enabled": False,
        "browser.shell.checkDefaultBrowser": False,
        "trailhead.firstrun.didSeeAboutWelcome": True,

        # Firefox's content-process sandbox needs a user namespace, which
        # Colima's default seccomp profile blocks (EPERM) — left on, page
        # rendering crashes. Pairs with MOZ_DISABLE_CONTENT_SANDBOX=1 in the
        # entrypoint. An internal process-isolation setting, invisible to
        # web content, so it has no bearing on the anti-bot fingerprint.
        "security.sandbox.content.level": 0,

        # Save documents to the mounted dir without a dialog.
        "browser.download.folderList": 2,
        "browser.download.dir": str(download_dir),
        "browser.download.useDownloadDir": True,
        "browser.download.manager.showWhenStarting": False,
        "pdfjs.disabled": True,
        "browser.helperApps.neverAsk.saveToDisk": SAVE_TO_DISK,
    }


def seed(profile_dir: Path, download_dir: Path) -> Path:
    """Write `<profile_dir>/user.js` and ensure the download dir exists.
    Returns the path written."""
    # Create + 0700 the profile (holds the live session cookie and saved
    # login) and relocate its regenerable startupCache out of the secrets
    # tree — the same prep the driven browsers get.
    launch.prepare_profile_dir(profile_dir)
    download_dir.mkdir(parents=True, exist_ok=True)
    user_js = profile_dir / "user.js"
    user_js.write_text(
        launch.firefox_user_js(**angellist_prefs(download_dir)),
        encoding="utf-8",
    )
    return user_js


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip().split("\n\n")[0],
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument(
        "--profile-dir", type=Path, default=DEFAULT_PROFILE_DIR,
        help=("Stock-Firefox profile dir to seed. Holds the session cookie. "
              "Treat as a credential. Default: %(default)s."),
    )
    p.add_argument(
        "--download-dir", type=Path, default=DEFAULT_DOWNLOAD_DIR,
        help=("Where the by-hand login saves tax documents. Created if "
              "absent. Default: %(default)s."),
    )
    cli.add_standard_args(p, verb="login")
    return p.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)
    user_js = seed(args.profile_dir, args.download_dir)
    log.info("seeded %s", user_js)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
