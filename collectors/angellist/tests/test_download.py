"""Unit tests for download.py pure helpers.

Covers the tax-document completeness decision (which years get re-downloaded
each run until they settle) and relative→absolute URL resolution. The
browser/network paths are exercised live, not here.
"""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import download  # noqa: E402


def test_is_incomplete():
    # not yet marked complete -> re-download
    assert download._is_incomplete({"documentType": "estimate_provided"}) is True
    # complete label but fewer K-1s than expected -> still incomplete
    assert download._is_incomplete(
        {"documentType": "complete", "k1Count": 3, "totalK1Count": 5}) is True
    # all expected K-1s in and complete -> done, skip
    assert download._is_incomplete(
        {"documentType": "complete", "k1Count": 4, "totalK1Count": 4}) is False
    # complete with no count info (older years) -> done
    assert download._is_incomplete({"documentType": "complete"}) is False
    assert download._is_incomplete(
        {"documentType": "complete", "k1Count": None, "totalK1Count": None}) is False


def test_check_exits_3_when_browser_cannot_launch(tmp_path, monkeypatch):
    # A --check probe whose browser cannot even start must exit 3 — distinct
    # from 1 ("stale") — so the entrypoint surfaces the environment failure
    # instead of forcing a needless by-hand 2FA re-login.
    import json
    import types

    jar = tmp_path / "cookies.json"
    jar.write_text(json.dumps([{
        "name": "_angellist_v2", "value": "s", "domain": ".angellist.com",
        "path": "/", "expires": 4102444800, "httpOnly": True,
        "secure": True, "sameSite": "Lax"}]))

    class BoomFox:
        def __init__(self, **kwargs):
            raise PermissionError(13, "Permission denied", "GeoLite2-City.mmdb")

    stub = types.ModuleType("camoufox.sync_api")
    stub.Camoufox = BoomFox
    monkeypatch.setitem(sys.modules, "camoufox", types.ModuleType("camoufox"))
    monkeypatch.setitem(sys.modules, "camoufox.sync_api", stub)

    assert download.main(["--cookies", str(jar), "--check"]) == 3


def test_documents_dir_derives_from_bronze_dir():
    # parse_args leaves the default None; main() resolves it through
    # _resolve_documents_dir as <bronze-dir>/angellist-documents — the
    # bronze-root sibling `load` parses (its side is pinned end-to-end by
    # test_load.test_default_paths_derive_from_bronze_dir against the same
    # literal) — so scoping --bronze-dir scopes the document drop too.
    assert download.parse_args([]).documents_dir is None
    assert download._resolve_documents_dir(None, Path("/b")) == \
        Path("/b/angellist-documents")
    # an explicit flag wins over the derivation
    assert download.parse_args(["--documents-dir", "/x"]).documents_dir == Path("/x")
    assert download._resolve_documents_dir(Path("/x"), Path("/b")) == Path("/x")


def test_abs_url():
    assert download._abs_url("/k1_packets/1/csv") == \
        "https://venture.angellist.com/k1_packets/1/csv"
    assert download._abs_url("https://cdn.example/x.pdf") == "https://cdn.example/x.pdf"
    assert download._abs_url(None) is None
    assert download._abs_url("") == ""


def test_check_flag():
    # `login` uses `download --check` as the authoritative server
    # probe (a cookie can be unexpired yet server-rejected).
    assert download.parse_args(["--check"]).check is True
    assert download.parse_args([]).check is False


def test_debug_flag():
    # Gates the per-route DOM + screenshot captures under
    # <run>/screenshots/. Off by default: a normal run writes none, and
    # `prune` reclaims them from the dumps that do.
    assert download.parse_args(["--debug"]).debug is True
    assert download.parse_args([]).debug is False


def test_debug_help_promises_bronze_captures(capsys):
    # Guards against the flag regressing to the warn-only stub it was.
    with pytest.raises(SystemExit):
        download.parse_args(["--help"])
    out = capsys.readouterr().out
    assert "--debug" in out
    assert "gates nothing" not in out
    assert "NOT YET IMPLEMENTED" not in out


def test_lookback_flag():
    # Accepted for fleet uniformity: angellist always captures the
    # full portfolio snapshot (no server-side date filter), so --lookback only
    # drives a warning; the value is still validated against the
    # shared presets so a typo fails loudly.
    assert download.parse_args(["--lookback", "4w"]).lookback == "4w"
    assert download.parse_args([]).lookback is None
    with pytest.raises(SystemExit):
        download.parse_args(["--lookback", "1m"])  # not a preset


def test_no_documents_flag():
    # The fleet-wide document opt-out, a real skip: main() calls
    # download_documents only when it is absent. Default off, so a bare
    # run still fetches.
    assert download.parse_args(["--no-documents"]).no_documents is True
    assert download.parse_args([]).no_documents is False


def test_no_documents_help_promises_a_skip(capsys):
    # Guards against the flag regressing to a warn-only stub: the help must
    # still declare it and must not advertise it as unimplemented.
    with pytest.raises(SystemExit):
        download.parse_args(["--help"])
    out = capsys.readouterr().out
    assert "--no-documents" in out
    assert "NOT YET IMPLEMENTED" not in out
