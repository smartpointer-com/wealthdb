"""Unit tests for download.py's browserless surface: argument parsing, the
format resolver, the 24-month clamp, the statement window, the bronze
manifest, and the activity pagination — the last driven against a stub
request context, so no browser and no network are needed.

Synthetic payloads only.
"""
from __future__ import annotations

import json
import sys
from datetime import date
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import amexclient  # noqa: E402
import download  # noqa: E402
import login  # noqa: E402

KEY = "0123456789ABCDEF0123456789ABCDEF"
TOKEN = "AAAA1B2C3D4E5F6"


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
    """Answers each activity POST from a scripted list of (status, body),
    recording the offsets it was asked for."""

    def __init__(self, pages):
        self.pages = list(pages)
        self.offsets: list[int] = []

    def post(self, url, headers=None, data=None):
        self.offsets.append((data or {}).get("transactionFilters", {})
                            .get("offset"))
        status, body = (self.pages.pop(0) if self.pages else (200, None))
        return _StubResponse(status, body)


class _StubResponse:
    def __init__(self, status, body):
        self.status, self._body = status, body

    def json(self):
        if self._body is None:
            raise ValueError("no body")
        return self._body


class _StubContext:
    def __init__(self, pages):
        self.request = _StubRequest(pages)


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

def _stub_walk(monkeypatch):
    """Stub every collaborator walk() calls, recording the window each was
    handed. Returns the dict those land in."""
    seen = {}
    monkeypatch.setattr(download.login, "fn_post",
                        lambda ctx, fn: (200, {"roster": "synthetic"}))
    monkeypatch.setattr(download.amexclient, "parse_accounts",
                        lambda body, cards_only=True: [
                            {"account_key": KEY, "account_token": TOKEN}])

    def _paginate(ctx, token, *, since, until):
        seen["activity_since"] = since
        return {"transactions": [], "totalTransactionCount": 0,
                "categories": {}}

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
