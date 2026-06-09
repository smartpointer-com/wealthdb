"""Unit tests for extract_cookies.py.

Builds a synthetic Firefox cookies.sqlite (moz_cookies) and checks the
extraction's tricky bits: host filtering, the millisecond→second expiry
normalisation, session-cookie sentinel, and sameSite mapping.
"""
from __future__ import annotations

import sqlite3
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import extract_cookies as ec  # noqa: E402


def build_cookiedb(path):
    c = sqlite3.connect(path)
    c.execute(
        "CREATE TABLE moz_cookies (id INTEGER PRIMARY KEY, host TEXT, name TEXT, "
        "value TEXT, path TEXT, expiry INTEGER, isSecure INTEGER, isHttpOnly INTEGER, "
        "sameSite INTEGER)")
    rows = [
        # angellist, millisecond expiry (13-digit), Lax, httpOnly+secure
        (".angellist.com", "_angellist_v2", "sess", "/", 1783326718663, 1, 1, 1),
        # angellist, Strict, secure
        ("venture.angellist.com", "last-login-brand", "b", "/", 1815466977000, 1, 0, 2),
        # angellist, session cookie (expiry 0 -> -1); sameSite None + secure -> stays None
        (".angellist.com", "session_only", "s", "/", 0, 1, 1, 0),
        # non-angellist host -> filtered out
        ("example.com", "other", "y", "/", 1800000000000, 0, 0, 1),
    ]
    c.executemany(
        "INSERT INTO moz_cookies (host,name,value,path,expiry,isSecure,isHttpOnly,sameSite) "
        "VALUES (?,?,?,?,?,?,?,?)", rows)
    c.commit()
    c.close()


def test_extract(tmp_path):
    db = tmp_path / "cookies.sqlite"
    build_cookiedb(db)
    cookies = ec.extract(db, "angellist")
    by = {c["name"]: c for c in cookies}

    # only angellist hosts; example.com dropped
    assert set(by) == {"_angellist_v2", "last-login-brand", "session_only"}

    # millisecond expiry normalised to seconds
    assert by["_angellist_v2"]["expires"] == 1783326718
    assert by["last-login-brand"]["expires"] == 1815466977
    # session cookie (expiry 0) -> -1 sentinel
    assert by["session_only"]["expires"] == -1

    # sameSite mapping
    assert by["_angellist_v2"]["sameSite"] == "Lax"
    assert by["last-login-brand"]["sameSite"] == "Strict"
    # None + secure stays None (Playwright requires secure for None)
    assert by["session_only"]["sameSite"] == "None"

    # flags + domain
    assert by["_angellist_v2"]["httpOnly"] is True
    assert by["_angellist_v2"]["secure"] is True
    assert by["last-login-brand"]["httpOnly"] is False
    assert by["_angellist_v2"]["domain"] == ".angellist.com"
    assert by["_angellist_v2"]["path"] == "/"


def test_no_match(tmp_path):
    db = tmp_path / "cookies.sqlite"
    build_cookiedb(db)
    assert ec.extract(db, "nonexistent-host") == []
