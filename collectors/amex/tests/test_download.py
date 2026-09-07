"""Unit tests for download.py's browserless surface: argument parsing, the
format resolver, the 24-month clamp, the statement window, the bronze
manifest, the activity pagination, the per-card fan-out, and the export and
statement passes — all driven against a stub request context, so no browser
and no network are needed.

Synthetic payloads only.
"""
from __future__ import annotations

import contextlib
import json
import logging
import sys
from datetime import date, timedelta
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import amexclient  # noqa: E402
import download  # noqa: E402
import login  # noqa: E402

KEY = "0123456789ABCDEF0123456789ABCDEF"
KEY_B = "FEDCBA9876543210FEDCBA9876543210"
KEY_C = "00112233445566778899AABBCCDDEEFF"
TOKEN = "AAAA1B2C3D4E5F6"
TOKEN_B = "BBBB2C3D4E5F6A7"


# ============================================================
# Argument parsing
# ============================================================

def test_parse_args_defaults():
    args = download.parse_args(["--bronze-dir", "/data"])
    assert args.bronze_dir == Path("/data")
    assert args.profile_dir == Path("/secrets/amex-profile")
    assert args.no_documents is False      # documents on by default
    assert args.dry_run is False
    assert args.format is None
    assert args.lookback is None           # resolved to ~90 days


def test_bronze_dir_is_required():
    with pytest.raises(SystemExit):
        download.parse_args([])


def test_lookback_parses_a_preset_and_an_iso_date():
    assert download.parse_args(
        ["--bronze-dir", "/d", "--lookback", "2y"]).lookback == "2y"
    assert download.parse_args(
        ["--bronze-dir", "/d", "--lookback", "2024-01-01"]
    ).lookback == "2024-01-01"


def test_lookback_rejects_junk():
    with pytest.raises(SystemExit):
        download.parse_args(["--bronze-dir", "/d", "--lookback", "yesterday"])


def test_no_password_flag_exists():
    with pytest.raises(SystemExit):
        download.parse_args(["--bronze-dir", "/d", "--password", "x"])


# ============================================================
# How a passcode challenge is answered
# ============================================================

def test_the_default_prompts_only_when_there_is_a_terminal():
    # A login → download pair is two sign-ins and the provider rate-limits,
    # so a challenge on the second is routine. Prompt when there is someone
    # to prompt; keep the unattended contract when there is not.
    assert download.resolve_two_factor(None, isatty=True) == login.TWOFACTOR_CLI
    assert download.resolve_two_factor(None, isatty=False) == login.TWOFACTOR_NONE


def test_cli_mfa_forces_the_prompt_and_no_cli_mfa_forbids_it():
    assert download.resolve_two_factor(True, isatty=False) == login.TWOFACTOR_CLI
    assert download.resolve_two_factor(False, isatty=True) == login.TWOFACTOR_NONE


def test_vnc_mode_beats_every_other_setting():
    # `vnc-login` passes it, and it is the only mode that can answer a
    # captcha — so it must not be overridden by a TTY or by --no-cli-mfa.
    assert download.resolve_two_factor(None, vnc=True,
                                       isatty=False) == login.TWOFACTOR_VNC
    assert download.resolve_two_factor(False, vnc=True,
                                       isatty=True) == login.TWOFACTOR_VNC


def test_the_mfa_flags_parse_and_default_to_auto():
    assert download.parse_args(["--bronze-dir", "/d"]).cli_mfa is None
    assert download.parse_args(["--bronze-dir", "/d", "--cli-mfa"]).cli_mfa is True
    assert download.parse_args(["--bronze-dir", "/d",
                                "--no-cli-mfa"]).cli_mfa is False
    assert download.parse_args(["--bronze-dir", "/d"]).vnc_mfa is False
    assert download.parse_args(["--bronze-dir", "/d", "--vnc-mfa"]).vnc_mfa is True


def test_download_owns_the_sign_in_flags_login_used_to():
    # login folded into download, and its flags came with it.
    args = download.parse_args(["--bronze-dir", "/d", "--fresh"])
    assert args.fresh is True
    assert download.parse_args(["--bronze-dir", "/d"]).fresh is False


# ============================================================
# Export formats
# ============================================================

def test_formats_default_to_csv_and_qfx():
    assert download._resolve_formats(None) == ("csv", "qfx")


def test_formats_are_validated_loudly():
    with pytest.raises(SystemExit):
        download._resolve_formats(["csv", "ofx"])


def test_a_repeated_format_flag_accumulates():
    args = download.parse_args(["--bronze-dir", "/d",
                                "--format", "csv", "--format", "xls"])
    assert download._resolve_formats(args.format) == ("csv", "xls")


# ============================================================
# The 24-month clamp
# ============================================================

def test_a_window_inside_the_horizon_is_untouched():
    since, clamped = download.clamp_since(date(2026, 1, 1), date(2026, 3, 1))
    assert since == date(2026, 1, 1) and clamped is False


def test_a_window_past_the_horizon_is_narrowed_and_flagged():
    # The activity and exports stop at 24 months; the fleet rule is to
    # narrow to what the source honours and warn, never to fail silently.
    since, clamped = download.clamp_since(date(2015, 1, 1), date(2026, 3, 1))
    assert clamped is True
    assert since > date(2024, 1, 1)


def test_no_window_is_left_alone():
    assert download.clamp_since(None, date(2026, 3, 1)) == (None, False)


@pytest.mark.parametrize("until", [
    date(2026, 9, 7),      # 24 months back is exactly 730 days
    date(2028, 3, 1),      # ...and 731 across a leap day
])
def test_a_two_year_lookback_is_inside_the_horizon(until):
    # `--lookback 2y` is 730 days. Counting 24 thirty-day months put the
    # floor about ten days inside that, so the preset warned about a window
    # the source honours and moved the manifest's `since` off the request.
    asked = until - timedelta(days=730)
    assert download.clamp_since(asked, until) == (asked, False)


def test_a_day_past_the_horizon_is_clamped_to_the_calendar_floor():
    until = date(2026, 9, 7)
    floor = download._months_back(until, amexclient.MAX_AVAILABLE_MONTHS)
    assert download.clamp_since(floor - timedelta(days=1), until) == (floor,
                                                                      True)


@pytest.mark.parametrize("until,floor", [
    (date(2028, 2, 29), date(2026, 2, 28)),   # no 29 Feb two years back
    (date(2026, 5, 31), date(2024, 5, 31)),
    (date(2026, 1, 15), date(2024, 1, 15)),
])
def test_the_month_rollback_clamps_to_the_target_months_last_day(until, floor):
    assert download._months_back(until, 24) == floor


# ============================================================
# The statement window
# ============================================================

def test_a_statement_on_or_after_since_is_kept():
    assert download._statement_in_window("2026-03-12", date(2026, 1, 1))
    assert download._statement_in_window("2026-01-01", date(2026, 1, 1))


def test_an_older_statement_is_skipped():
    assert not download._statement_in_window("2025-12-31", date(2026, 1, 1))


def test_an_unparseable_period_is_kept():
    # Fail open: a document is cheap, a silently missing one is not.
    assert download._statement_in_window("not-a-date", date(2026, 1, 1))


def test_no_window_keeps_everything():
    assert download._statement_in_window("2001-01-01", None)


# ============================================================
# Filesystem-safe stems
# ============================================================

def test_log_id_shortens_an_account_key_but_files_keep_it():
    # Terminal output is the fleet's primary debugging channel and gets
    # pasted around, so a 32-hex account key is shortened for the log — while
    # the file name keeps the whole key, which is what the loader joins on.
    key = "0123456789ABCDEF0123456789ABCDEF"
    assert download.log_id(key) == "01234567…"
    assert download.safe_stem(key) == key


def test_log_id_leaves_a_short_key_alone():
    assert download.log_id("ABC123") == "ABC123"


@pytest.mark.parametrize("raw,expected", [
    ("0123ABC", "0123ABC"),
    ("2026-03-12", "2026-03-12"),
    ("../../etc/passwd", "etc-passwd"),
    ("a/b", "a-b"),
    (".hidden", "hidden"),
    ("", "item"),
    ("///", "item"),
])
def test_safe_stem(raw, expected):
    assert download.safe_stem(raw) == expected


# ============================================================
# The run manifest
# ============================================================

def test_manifest_records_the_window_and_keys_but_no_balances():
    accounts = [{"account_key": KEY, "balance": {"amount": "1.00"}}]
    m = download.build_manifest("complete", accounts=accounts,
                                counts={"transactions": 3},
                                since=date(2026, 1, 1), until=date(2026, 3, 1),
                                formats=("csv",), clamped=True,
                                documents_since=date(2024, 1, 1))
    assert m["source"] == "amex" and m["status"] == "complete"
    assert m["account_keys"] == [KEY]
    assert m["since"] == "2026-01-01" and m["until"] == "2026-03-01"
    assert m["window_clamped"] is True
    # A clamped run still asked the statement archive for the full window.
    assert m["documents_since"] == "2024-01-01"
    # No balance may reach the manifest, which prune and load both read.
    assert "1.00" not in json.dumps(m)


def test_manifest_without_a_window():
    m = download.build_manifest("dry-run", accounts=[], counts={},
                                since=None, until=None, formats=(),
                                clamped=False, documents_since=None)
    assert m["since"] is None and m["until"] is None
    assert m["documents_since"] is None
    assert m["status"] == "dry-run"


# ============================================================
# Activity pagination
# ============================================================

def _tx(n):
    return {"identifier": f"1000000000000000{n:02d}",
            "referenceNumber": f"1000000000000000{n:02d}",
            "status": "posted"}


def _activity_body(rows, total):
    return {"activityData": {"data": [{"transactions": rows}],
                             "totalTransactionCount": total,
                             "categories": {"C1": "Groceries"}}}


class _StubRequest:
    """Answers each POST from a scripted list of (status, body), recording
    the offsets it was asked for, and each GET from its own script,
    recording the URLs. One stub therefore serves the activity/statement
    POSTs and the document/export GETs alike."""

    def __init__(self, pages, gets=()):
        self.pages = list(pages)
        self.gets = list(gets)
        self.offsets: list[int] = []
        self.urls: list[str] = []

    def post(self, url, headers=None, data=None):
        self.offsets.append((data or {}).get("transactionFilters", {})
                            .get("offset"))
        status, body = (self.pages.pop(0) if self.pages else (200, None))
        return _StubResponse(status, body)

    def get(self, url, headers=None):
        self.urls.append(url)
        status, body = (self.gets.pop(0) if self.gets else (404, None))
        return _StubResponse(status, body)


class _StubResponse:
    def __init__(self, status, body):
        self.status, self._body = status, body

    def json(self):
        if self._body is None:
            raise ValueError("no body")
        return self._body

    def body(self):
        return self._body if isinstance(self._body, bytes) else b""


class _StubContext:
    def __init__(self, pages=(), gets=()):
        self.request = _StubRequest(pages, gets)


def test_a_single_short_page_needs_no_continuation():
    ctx = _StubContext([(200, _activity_body([_tx(1), _tx(2)], 2))])
    merged = download._paginate(ctx, TOKEN, since=None, until=None)
    assert len(merged["transactions"]) == 2
    assert merged["totalTransactionCount"] == 2
    assert merged["categories"] == {"C1": "Groceries"}
    assert ctx.request.offsets == [1]


def test_row_based_offsets_walk_a_full_page():
    size = amexclient.ACTIVITY_PAGE_SIZE
    first = [_tx(i) for i in range(size)]
    second = [_tx(size + i) for i in range(5)]
    ctx = _StubContext([(200, _activity_body(first, size + 5)),
                        (200, _activity_body(second, size + 5))])
    merged = download._paginate(ctx, TOKEN, since=None, until=None)
    assert len(merged["transactions"]) == size + 5
    # The row-based step is tried first: offset 1, then size + 1.
    assert ctx.request.offsets == [1, size + 1]


def test_page_based_offsets_are_detected_and_retried():
    # If the offset counts PAGES, a row-based continuation returns the page
    # we already have — which is the signal to switch, not to stop.
    size = amexclient.ACTIVITY_PAGE_SIZE
    first = [_tx(i) for i in range(size)]
    second = [_tx(size + i) for i in range(5)]
    ctx = _StubContext([(200, _activity_body(first, size + 5)),
                        (200, _activity_body(first, size + 5)),   # repeat
                        (200, _activity_body(second, size + 5))])
    merged = download._paginate(ctx, TOKEN, since=None, until=None)
    assert len(merged["transactions"]) == size + 5
    assert ctx.request.offsets == [1, size + 1, 2]


def test_duplicate_rows_are_never_double_counted():
    size = amexclient.ACTIVITY_PAGE_SIZE
    first = [_tx(i) for i in range(size)]
    overlap = first[-5:] + [_tx(size + i) for i in range(3)]
    ctx = _StubContext([(200, _activity_body(first, size + 3)),
                        (200, _activity_body(overlap, size + 3))])
    merged = download._paginate(ctx, TOKEN, since=None, until=None)
    ids = [t["identifier"] for t in merged["transactions"]]
    assert len(ids) == len(set(ids)) == size + 3


def test_a_failed_first_page_raises():
    ctx = _StubContext([(401, None)])
    with pytest.raises(RuntimeError):
        download._paginate(ctx, TOKEN, since=None, until=None)


def test_a_failed_later_page_keeps_what_was_fetched():
    size = amexclient.ACTIVITY_PAGE_SIZE
    ctx = _StubContext([(200, _activity_body([_tx(i) for i in range(size)],
                                             size + 10)),
                        (500, None)])
    merged = download._paginate(ctx, TOKEN, since=None, until=None)
    assert len(merged["transactions"]) == size


def test_the_window_rides_the_request_body():
    ctx = _StubContext([(200, _activity_body([_tx(1)], 1))])
    merged = download._paginate(ctx, TOKEN, since=date(2026, 1, 1),
                                until=date(2026, 3, 1))
    assert merged["since"] == "2026-01-01" and merged["until"] == "2026-03-01"


# ============================================================
# walk(): the two windows
# ============================================================

def _stub_walk(monkeypatch, accounts=None):
    """Stub every collaborator walk() calls, recording the window each was
    handed. Returns the dict those land in. `accounts` is the roster the
    fan-out runs over; the default is one card."""
    seen = {}
    accounts = accounts if accounts is not None else [
        {"account_key": KEY, "account_token": TOKEN}]
    monkeypatch.setattr(download.login, "fn_post",
                        lambda ctx, fn: (200, {"roster": "synthetic"}))
    monkeypatch.setattr(download.amexclient, "parse_accounts",
                        lambda body, cards_only=True: accounts)

    def _paginate(ctx, token, *, since, until):
        seen["activity_since"] = since
        return {"transactions": [{"identifier": token}],
                "totalTransactionCount": 1, "categories": {}}

    def _export(ctx, acct, run_dir, formats, *, since, until):
        seen["export_since"] = since
        return 0

    def _statements(ctx, acct, run_dir, *, since):
        seen["documents_since"] = since
        return 0, 0

    monkeypatch.setattr(download, "_paginate", _paginate)
    monkeypatch.setattr(download, "_export_account", _export)
    monkeypatch.setattr(download, "_download_statements", _statements)
    return seen


def test_the_clamp_narrows_the_activity_window_but_never_the_documents(
        tmp_path, monkeypatch):
    """The statement archive is the only channel that reaches past the
    24-month horizon (DESIGN.md §E), so the clamp must stop at the activity
    and export calls. Following it into the document pass would starve the
    deep backfill on every invocation, including `--lookback all`."""
    seen = _stub_walk(monkeypatch)

    asked = date(2015, 1, 1)
    summary = download.walk(object(), tmp_path, since=asked,
                            until=date(2026, 3, 1))

    manifest = json.loads((Path(summary["run_dir"]) / "run.json").read_text())
    assert manifest["window_clamped"] is True
    # Both structured channels were narrowed to what the source honours...
    assert seen["activity_since"] == seen["export_since"] > asked
    assert manifest["since"] == seen["activity_since"].isoformat()
    # ...while the statement archive was asked for the window as given.
    assert seen["documents_since"] == asked
    assert manifest["documents_since"] == asked.isoformat()


def test_an_unclamped_window_reaches_both_channels_alike(
        tmp_path, monkeypatch):
    seen = _stub_walk(monkeypatch)
    asked = date(2026, 1, 1)
    summary = download.walk(object(), tmp_path, since=asked,
                            until=date(2026, 3, 1))
    manifest = json.loads((Path(summary["run_dir"]) / "run.json").read_text())
    assert manifest["window_clamped"] is False
    assert manifest["since"] == manifest["documents_since"] == asked.isoformat()
    assert seen["activity_since"] == seen["documents_since"] == asked


def test_no_documents_leaves_the_document_window_unclaimed(
        tmp_path, monkeypatch):
    # A run that fetches no PDF must not record a window it never used.
    seen = _stub_walk(monkeypatch)
    summary = download.walk(object(), tmp_path, since=date(2015, 1, 1),
                            until=date(2026, 3, 1), documents=False)
    manifest = json.loads((Path(summary["run_dir"]) / "run.json").read_text())
    assert manifest["documents_since"] is None
    assert "documents_since" not in seen
    assert manifest["window_clamped"] is True


# ============================================================
# walk(): the per-card fan-out
# ============================================================

def test_walk_fans_out_per_card_and_skips_a_tokenless_one(tmp_path,
                                                          monkeypatch,
                                                          caplog):
    # Every card is walked, and one the roster gives no activity token is
    # skipped loudly rather than silently costing its whole ledger. The
    # tokenless card still belongs on the roster the manifest records.
    _stub_walk(monkeypatch, accounts=[
        {"account_key": KEY, "account_token": TOKEN},
        {"account_key": KEY_B, "account_token": TOKEN_B},
        {"account_key": KEY_C, "account_token": ""},
    ])
    with caplog.at_level(logging.WARNING, logger="amex.download"):
        summary = download.walk(object(), tmp_path, since=date(2026, 1, 1),
                                until=date(2026, 3, 1))
    run = Path(summary["run_dir"])
    assert (run / "activity" / f"{KEY}.json").is_file()
    assert (run / "activity" / f"{KEY_B}.json").is_file()
    assert not (run / "activity" / f"{KEY_C}.json").exists()
    manifest = json.loads((run / "run.json").read_text())
    assert manifest["account_keys"] == [KEY, KEY_B, KEY_C]
    assert manifest["counts"]["transactions"] == 2
    assert "has no activity token" in caplog.text
    # It gets a coverage row too: a roster key with none is indistinguishable
    # from one the block forgot to write.
    assert manifest["coverage"][KEY_C] == {"transactions": 0,
                                           "expected": None,
                                           "complete": False}


# ============================================================
# What the manifest says about a window the source honoured only in part
# ============================================================

def test_a_full_fetch_is_marked_covered(tmp_path, monkeypatch):
    _stub_walk(monkeypatch)
    summary = download.walk(object(), tmp_path, since=date(2026, 1, 1),
                            until=date(2026, 3, 1))
    manifest = json.loads((Path(summary["run_dir"]) / "run.json").read_text())
    assert manifest["coverage"][KEY] == {"transactions": 1, "expected": 1,
                                         "complete": True}


def test_a_short_activity_fetch_is_flagged_beside_a_complete_status(
        tmp_path, monkeypatch):
    # The run is still loadable and still 'complete' — what it holds is real
    # — but `since` alone would claim a window the source did not honour.
    _stub_walk(monkeypatch)
    monkeypatch.setattr(download, "_paginate",
                        lambda ctx, token, *, since, until: {
                            "transactions": [{"identifier": "1"}],
                            "totalTransactionCount": 500, "categories": {}})
    summary = download.walk(object(), tmp_path, since=date(2026, 1, 1),
                            until=date(2026, 3, 1))
    manifest = json.loads((Path(summary["run_dir"]) / "run.json").read_text())
    assert manifest["status"] == "complete"
    assert manifest["coverage"][KEY] == {"transactions": 1, "expected": 500,
                                         "complete": False}


def test_an_unmeasured_fetch_claims_nothing_either_way(tmp_path,
                                                       monkeypatch):
    # A total that comes back null (or as a string) says nothing about
    # coverage, and a run must not turn that silence into a positive claim
    # of a complete fetch.
    _stub_walk(monkeypatch)
    monkeypatch.setattr(download, "_paginate",
                        lambda ctx, token, *, since, until: {
                            "transactions": [{"identifier": "1"}],
                            "totalTransactionCount": None, "categories": {}})
    summary = download.walk(object(), tmp_path, since=date(2026, 1, 1),
                            until=date(2026, 3, 1))
    manifest = json.loads((Path(summary["run_dir"]) / "run.json").read_text())
    assert manifest["coverage"][KEY] == {"transactions": 1, "expected": None,
                                         "complete": None}


def test_an_account_whose_activity_fetch_failed_is_flagged(tmp_path,
                                                           monkeypatch):
    _stub_walk(monkeypatch)

    def _boom(ctx, token, *, since, until):
        raise RuntimeError("activity fetch failed (HTTP 500)")
    monkeypatch.setattr(download, "_paginate", _boom)
    summary = download.walk(object(), tmp_path, since=None,
                            until=date(2026, 3, 1))
    manifest = json.loads((Path(summary["run_dir"]) / "run.json").read_text())
    assert manifest["status"] == "complete"
    assert manifest["coverage"][KEY] == {"transactions": 0, "expected": None,
                                         "complete": False}


# ============================================================
# The statement and export passes, against the stub request context
# ============================================================

def _statements_body(periods=(), summaries=()):
    return {"billingStatements": {
        "recentStatements": [
            {"statementEndDate": end,
             "downloadOptions": {
                 "STATEMENT_PDF":
                     f"/api/servicing/v1/documents/statements/T{end}"}}
            for end in periods],
        "olderStatements": [],
        "yearEndSummaries": [
            {"year": year, "downloadOptions": {
                "YES_PDF":
                    f"/api/servicing/v2/financials/documents?year={year}"}}
            for year in summaries],
    }}


PDF = b"%PDF-1.4 synthetic\n"


def test_statements_land_only_for_periods_in_the_window(tmp_path):
    # `--lookback` reaches the documents unclamped, and the window is applied
    # to the PERIOD END. The year-end summary carries no period, so it is
    # always fetched.
    ctx = _StubContext(
        [(200, _statements_body(periods=("2026-03-12", "2025-06-12"),
                                summaries=(2025,)))],
        [(200, PDF), (200, PDF)])
    acct = {"account_key": KEY, "account_token": TOKEN}
    assert download._download_statements(ctx, acct, tmp_path,
                                         since=date(2026, 1, 1)) == (1, 1)
    out = tmp_path / "statements" / KEY
    assert (out / "2026-03-12.pdf").read_bytes() == PDF
    assert not (out / "2025-06-12.pdf").exists()
    assert (out / "yes-2025.pdf").read_bytes() == PDF
    # The archive body is kept as provenance, and each GET went to the URL
    # the entry named.
    assert (tmp_path / "raw" / f"statements-{KEY}.json").is_file()
    assert ctx.request.urls == [
        "https://global.americanexpress.com"
        "/api/servicing/v1/documents/statements/T2026-03-12",
        "https://global.americanexpress.com"
        "/api/servicing/v2/financials/documents?year=2025",
    ]


def test_a_document_the_source_refuses_lands_no_file(tmp_path):
    ctx = _StubContext([(200, _statements_body(periods=("2026-03-12",)))],
                       [(404, None)])
    acct = {"account_key": KEY, "account_token": TOKEN}
    assert download._download_statements(ctx, acct, tmp_path,
                                         since=None) == (0, 0)
    assert not (tmp_path / "statements" / KEY / "2026-03-12.pdf").exists()


def test_export_writes_one_file_per_format_and_skips_a_non_2xx(tmp_path):
    ctx = _StubContext(gets=[(200, b"synthetic,export"), (500, None)])
    acct = {"account_key": KEY, "account_token": TOKEN}
    written = download._export_account(ctx, acct, tmp_path, ("csv", "qfx"),
                                       since=date(2026, 1, 1),
                                       until=date(2026, 3, 1))
    assert written == 1
    assert (tmp_path / "transactions" / f"{KEY}.csv").read_bytes() == (
        b"synthetic,export")
    assert not (tmp_path / "transactions" / f"{KEY}.qfx").exists()


# ============================================================
# What an unanswerable sign-in exits with
# ============================================================

def _stub_sign_in(monkeypatch, raises):
    @contextlib.contextmanager
    def _camoufox(profile_dir, fresh=False):
        yield object(), object()

    def _drive(context, page, args, *, two_factor):
        raise raises
    monkeypatch.setattr(download.login, "camoufox", _camoufox)
    monkeypatch.setattr(download.login, "drive_to_auth", _drive)


def test_an_unattended_run_that_meets_a_challenge_exits_two(tmp_path,
                                                            monkeypatch):
    # Distinct from a refusal: nothing is wrong with the credentials, the
    # device trust simply expired and this run cannot answer a passcode.
    _stub_sign_in(monkeypatch, login.NeedsLogin("device trust has expired"))
    args = download.parse_args(["--bronze-dir", str(tmp_path), "--no-cli-mfa"])
    assert download.run_download(args) == 2


def test_a_refused_sign_in_exits_one(tmp_path, monkeypatch):
    _stub_sign_in(monkeypatch, login.LogonFailed("example refusal"))
    args = download.parse_args(["--bronze-dir", str(tmp_path), "--no-cli-mfa"])
    assert download.run_download(args) == 1
