"""Unit tests for download.py's browserless surface: the bronze-run helpers
(filename sanitising, the manifest), export-format resolution, and argument
parsing. The REST fetch itself needs a live authenticated session and is
validated separately (DESIGN.md §4.2).

Synthetic values only.
"""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import download  # noqa: E402
import login  # noqa: E402
import q2client  # noqa: E402


# ============================================================
# _fetch_history — paginates to exhaustion (stubbed transport)
# ============================================================

def _fake_history(total, page_size=q2client.HISTORY_PAGE_SIZE, seen=None):
    """A stub for login.q2_get_json that serves `total` synthetic
    transactions across pages of `page_size`, reading page[number] from the
    URL — no browser. If `seen` is a list, appends each requested URL."""
    def q2_get_json(context, url):
        import re
        if seen is not None:
            seen.append(url)
        pg = int(re.search(r"page\[number\]=(\d+)", url).group(1))
        start = (pg - 1) * page_size
        txs = [{"transactionId": f"t{i}"} for i in range(start,
                                                          min(start + page_size, total))]
        return 200, {"data": {"transactions": txs, "transactionCount": total,
                              "oldestTransactionDate": "2023-08-15T00:00:00Z"}}
    return q2_get_json


def test_fetch_history_collects_all_pages(monkeypatch):
    monkeypatch.setattr(login, "q2_get_json", _fake_history(250))
    merged = download._fetch_history(None, "A1")
    assert merged["transactionCount"] == 250
    assert len(merged["transactions"]) == 250          # 100 + 100 + 50
    assert merged["oldestTransactionDate"].startswith("2023-08-15")


def test_fetch_history_exact_multiple_of_page_size(monkeypatch):
    # A full last page must still terminate (count guard, not just short page).
    monkeypatch.setattr(login, "q2_get_json", _fake_history(200))
    merged = download._fetch_history(None, "A1")
    assert len(merged["transactions"]) == 200


def test_fetch_history_page1_failure_returns_none(monkeypatch):
    monkeypatch.setattr(login, "q2_get_json", lambda ctx, url: (500, None))
    assert download._fetch_history(None, "A1") is None


def test_fetch_history_passes_posted_date_window(monkeypatch):
    # The --lookback window must ride every page request.
    seen: list = []
    monkeypatch.setattr(login, "q2_get_json", _fake_history(5, seen=seen))
    pd = q2client.posted_date_range(__import__("datetime").date(2026, 7, 1),
                                    __import__("datetime").date(2026, 8, 14))
    download._fetch_history(None, "A1", posted_date=pd)
    assert seen and all("postedDate=" in u for u in seen)


# ============================================================
# _statement_in_window — document date filter
# ============================================================

def test_statement_in_window():
    from datetime import date
    since = date(2026, 7, 1)
    assert download._statement_in_window("07/31/2026", since)     # in window
    assert not download._statement_in_window("06/30/2026", since)  # before
    assert download._statement_in_window("06/30/2026", None)       # no window → keep
    assert download._statement_in_window("not-a-date", since)      # unparseable → keep


# ============================================================
# safe_stem
# ============================================================

def test_safe_stem_sanitises():
    assert download.safe_stem("7000001") == "7000001"
    assert download.safe_stem("07/31/2026") == "07-31-2026"
    assert download.safe_stem("../evil") == "evil"
    assert download.safe_stem("") == "item"


# ============================================================
# build_manifest
# ============================================================

def test_manifest_records_ids_not_balances():
    accounts = [{"id": "7000001", "account_external_id": "XXXXXX0000",
                 "balances": [{"description": "Available", "value": "1.00"}]}]
    m = download.build_manifest("complete", accounts=accounts,
                                counts={"exports": 2}, since=None, until=None,
                                formats=("csv", "qfx"))
    assert m["status"] == "complete"
    assert m["account_ids"] == ["7000001"]
    assert m["formats"] == ["csv", "qfx"]
    # The manifest must not embed balances (root AGENTS.md §4).
    assert "1.00" not in repr(m)


# ============================================================
# _resolve_formats
# ============================================================

def test_resolve_formats_default():
    assert download._resolve_formats(None) == q2client.DEFAULT_EXPORT_FORMATS
    assert download._resolve_formats([]) == q2client.DEFAULT_EXPORT_FORMATS


def test_resolve_formats_explicit():
    assert download._resolve_formats(["ofx"]) == ("ofx",)


def test_resolve_formats_rejects_unknown():
    with pytest.raises(SystemExit):
        download._resolve_formats(["csv", "nope"])


# ============================================================
# Argument parsing
# ============================================================

def test_parse_args_requires_bronze_dir():
    with pytest.raises(SystemExit):
        download.parse_args([])


def test_parse_args_defaults():
    args = download.parse_args(["--bronze-dir", "/data"])
    assert args.bronze_dir == Path("/data")
    assert args.profile_dir == Path("/secrets/firstcitizens-profile")
    assert args.no_documents is False
    assert args.dry_run is False
    assert args.format is None


def test_format_flag_is_repeatable():
    args = download.parse_args(["--bronze-dir", "/data",
                                "--format", "csv", "--format", "ofx"])
    assert args.format == ["csv", "ofx"]


def test_no_password_flag_exists():
    with pytest.raises(SystemExit):
        download.parse_args(["--bronze-dir", "/data", "--password", "x"])
