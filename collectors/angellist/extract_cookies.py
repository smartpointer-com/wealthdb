#!/usr/bin/env python3
"""Lift the AngelList session from a real Firefox cookies.sqlite.

Part of the BYO-session path: AngelList's venture login is gated by an
invisible Turnstile / reCAPTCHA challenge that flags the Camoufox /
Playwright automation stack (see DESIGN.md). The workaround is to log in
once in a genuine, un-instrumented Firefox driven by a human over VNC
(`./angellist login`), then read that Firefox profile's *plaintext*
`cookies.sqlite` and hand the cookies to the collector — no manual export
or copying.

This reads the `angellist`-host cookies out of a Firefox cookie DB and
writes them as a Playwright `add_cookies()`-shaped JSON list (the format
download.py / explore.py inject with `context.add_cookies`). Cookie
VALUES are never printed/logged — only names, hosts, and flags — and the
output file is chmod 0600 (it is a credential).

Firefox keeps cookies.sqlite open (WAL), so we copy the DB + -wal + -shm
to a temp dir and read the copy, which replays any pending WAL writes;
that way a freshly-set login cookie is visible even while Firefox runs.
Run it AFTER closing Firefox for the most reliable capture of any
session-scoped cookies (Firefox flushes those on a clean shutdown when
session-restore is enabled — login seeds that pref).
"""
from __future__ import annotations

import argparse
import json
import logging
import os
import shutil
import sqlite3
import sys
import tempfile
from pathlib import Path

log = logging.getLogger("angellist.extract_cookies")

DEFAULT_DB = Path("/secrets/angellist-fxprofile/cookies.sqlite")
DEFAULT_OUT = Path("/secrets/angellist-cookies.json")

# Firefox moz_cookies.sameSite -> Playwright sameSite. Firefox: 0=None,
# 1=Lax, 2=Strict. Anything unexpected falls back to "Lax".
SAMESITE = {0: "None", 1: "Lax", 2: "Strict"}


def _columns(con: sqlite3.Connection, table: str) -> set[str]:
    return {r[1] for r in con.execute(f"PRAGMA table_info({table})")}


def extract(db: Path, host_filter: str) -> list[dict]:
    """Return Playwright-shaped cookie dicts for hosts matching
    `host_filter`. Copies the DB (+ wal/shm) so a live Firefox doesn't
    block the read and pending WAL writes are seen."""
    db = Path(db)
    if not db.exists():
        raise SystemExit(
            f"Firefox cookie DB not found: {db}\n"
            f"Log in via `./angellist login` first (it writes the "
            f"profile here)."
        )
    tmp = Path(tempfile.mkdtemp(prefix="alck_"))
    try:
        for ext in ("", "-wal", "-shm"):
            src = Path(str(db) + ext)
            if src.exists():
                shutil.copy2(src, tmp / ("cookies.sqlite" + ext))
        con = sqlite3.connect(f"file:{tmp/'cookies.sqlite'}?mode=ro", uri=True)
        try:
            cols = _columns(con, "moz_cookies")
            has_ss = "sameSite" in cols
            sel = ["host", "name", "value", "path", "expiry",
                   "isSecure", "isHttpOnly"]
            if has_ss:
                sel.append("sameSite")
            rows = con.execute(
                f"SELECT {', '.join(sel)} FROM moz_cookies "
                f"WHERE host LIKE ? ORDER BY host, name",
                (f"%{host_filter}%",),
            ).fetchall()
        finally:
            con.close()
    finally:
        shutil.rmtree(tmp, ignore_errors=True)

    cookies = []
    for r in rows:
        host, name, value, path, expiry, secure, httponly = r[:7]
        ss = SAMESITE.get(r[7], "Lax") if has_ss else "Lax"
        cookie = {
            "name": name,
            "value": value,
            "domain": host,                  # Firefox host incl. leading dot
            "path": path or "/",
            "httpOnly": bool(httponly),
            "secure": bool(secure),
            "sameSite": ss,
        }
        # Playwright wants expires as epoch SECONDS, or -1 for a session
        # cookie. Firefox's moz_cookies.expiry is epoch time but this
        # build stores it in milliseconds (13-digit) — interpreted as
        # seconds that's year ~59000, which Playwright rejects. Normalise:
        # non-positive => session; millisecond-magnitude => //1000.
        exp = int(expiry) if expiry else 0
        if exp <= 0:
            cookie["expires"] = -1
        elif exp > 100_000_000_000:   # > ~year 5138 in seconds => ms
            cookie["expires"] = exp // 1000
        else:
            cookie["expires"] = exp
        # sameSite=None must be secure or Playwright rejects it.
        if cookie["sameSite"] == "None" and not cookie["secure"]:
            cookie["sameSite"] = "Lax"
        cookies.append(cookie)
    return cookies


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument("--db", type=Path, default=DEFAULT_DB,
                   help="Firefox cookies.sqlite. Default: %(default)s.")
    p.add_argument("--out", type=Path, default=DEFAULT_OUT,
                   help="Output cookie JSON (0600). Default: %(default)s.")
    p.add_argument("--host-filter", default="angellist",
                   help="Substring match on cookie host. Default: %(default)s.")
    p.add_argument("-v", "--verbose", action="store_true")
    return p.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    cookies = extract(args.db, args.host_filter)
    if not cookies:
        log.error("No cookies matching host ~ %r found in %s. Did the "
                  "login complete? (If the auth cookie is session-scoped, "
                  "close Firefox cleanly so it flushes to disk, then "
                  "re-run.)", args.host_filter, args.db)
        return 1

    args.out.parent.mkdir(parents=True, exist_ok=True)
    # Write 0600 from the start (mode honored only on create), then chmod
    # to be sure on an overwrite.
    fd = os.open(str(args.out), os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as fp:
        json.dump(cookies, fp, indent=2)
        fp.write("\n")
    os.chmod(args.out, 0o600)

    # Summary — names/hosts/flags only, never values.
    hosts = sorted({c["domain"] for c in cookies})
    sess = sum(1 for c in cookies if c["expires"] == -1)
    log.info("wrote %d cookie(s) across %d host(s) -> %s (0600)",
             len(cookies), len(hosts), args.out)
    log.info("  hosts: %s", ", ".join(hosts))
    log.info("  names: %s", ", ".join(sorted(c["name"] for c in cookies)))
    log.info("  (%d session-scoped, %d persistent)", sess, len(cookies) - sess)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
