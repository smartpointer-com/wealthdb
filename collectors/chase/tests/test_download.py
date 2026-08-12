"""Unit tests for download.py's browserless helpers — the roster reducer,
export window, filename sanitising, and manifest. The scrape itself needs a
live authenticated page and is validated on a real run. Synthetic data only.
"""
from __future__ import annotations

import sys
from datetime import date
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import download  # noqa: E402


def test_safe_stem():
    assert download.safe_stem("900001") == "900001"
    assert download.safe_stem("../etc/passwd") == ".._etc_passwd"
    assert download.safe_stem("") == "account"
    assert download.safe_stem("a b/c") == "a_b_c"


def test_roster_url_re_matches_known_endpoints():
    for u in ("/svc/rr/accounts/secure/v2/account/detail/dda/list",
              "/svc/rr/accounts/secure/v1/account/activity/download/options/list",
              "/svc/rr/accounts/secure/v1/account/activity/download/dda/list"):
        assert download.ROSTER_URL_RE.search(u), u
    for u in ("/svc/rr/accounts/secure/v1/menu/list",
              "/svc/rr/documents/secure/idal/v5/pdfdoc/star/list"):
        assert not download.ROSTER_URL_RE.search(u), u


def test_url_stem_is_safe_and_query_stripped():
    assert download._url_stem(
        "https://x/svc/rr/account/detail/dda/list?id=SECRET") == "dda-list"
    assert "SECRET" not in download._url_stem("https://x/a/b?id=SECRET")
    assert download._url_stem("/") == "svc"


def test_export_formats_are_option_testid_prefixes():
    # CSV + QFX only (DESIGN.md §E); values are the option data-testid
    # prefixes on the file-select.
    assert set(download.EXPORT_FORMATS) == {"csv", "qfx"}
    assert download.EXPORT_FORMATS["csv"].startswith("showing-file-select")
    assert download.EXPORT_FORMATS["qfx"].startswith("showing-file-select")


def test_css_attr_value_quotes_specials():
    # data-testids contain spaces / parens / commas — must be quoted.
    assert download._css_attr_value("a (b), c") == '"a (b), c"'
    assert download._css_attr_value('x"y') == '"x\\"y"'


def test_parse_download_options_keeps_deposit_drops_cards():
    body = {"downloadAccountActivityOptions": [
        {"accountId": 900001, "detailType": "CHK", "summaryType": "DDA",
         "nickName": "TOTAL CHECKING", "mask": "1234"},
        {"accountId": 900002, "detailType": "SAV", "summaryType": "DDA",
         "nickName": "SAVINGS", "mask": "5678"},
        {"accountId": 900003, "detailType": "BAC", "summaryType": "CARD",
         "nickName": "Some Card", "mask": "9012"},
    ]}
    out = download.parse_download_options(body)
    assert [a["account_external_id"] for a in out] == ["900001", "900002"]
    assert [a["account_type"] for a in out] == ["CHK", "SAV"]
    assert out[0]["mask"] == "…1234"          # normalised to last-4
    assert out[0]["currency"] == "USD"
    # credit card (CARD summaryType) is out of scope
    assert all(a["account_type"] != "BAC" for a in out)


def test_parse_download_options_empty():
    assert download.parse_download_options({}) == []


def test_parse_account_detail():
    # The per-account shape the overview fires (DESIGN.md §B).
    body = {"accountId": 900001, "nickname": "TOTAL CHECKING", "mask": "1234",
            "detail": {"detailType": "CHK", "presentBalance": 2257.50,
                       "available": 2200.00}}
    out = download.parse_account_detail(body)
    assert out == [{
        "account_external_id": "900001", "account_type": "CHK",
        "nickname": "TOTAL CHECKING", "mask": "…1234", "currency": "USD",
        "balance": 2257.50,
    }]
    # falls back to `available` when presentBalance is absent
    body2 = {"accountId": 900002, "mask": "5678",
             "detail": {"detailType": "SAV", "available": 10.0}}
    assert download.parse_account_detail(body2)[0]["balance"] == 10.0
    # a non-account body yields nothing
    assert download.parse_account_detail({"downloadAccountActivityOptions": []}) == []


def test_collect_accounts_dedupes_both_shapes():
    detail = {"accountId": 900001, "nickname": "TOTAL CHECKING", "mask": "1234",
              "detail": {"detailType": "CHK", "presentBalance": 2257.50}}
    options = {"downloadAccountActivityOptions": [
        {"accountId": 900001, "detailType": "CHK", "summaryType": "DDA",
         "nickName": "TOTAL CHECKING", "mask": "1234"},
        {"accountId": 900002, "detailType": "SAV", "summaryType": "DDA",
         "nickName": "SAVINGS", "mask": "5678"},
        {"accountId": 900003, "detailType": "BAC", "summaryType": "CARD",
         "nickName": "Card", "mask": "9999"},
    ]}
    roster = download.collect_accounts([detail, options])
    by_id = {a["account_external_id"]: a for a in roster}
    assert set(by_id) == {"900001", "900002"}          # card dropped, deduped
    # the download-options record wins but keeps the account-detail balance
    assert by_id["900001"]["balance"] == 2257.50


def test_collect_accounts_from_detail_only():
    roster = download.collect_accounts([
        {"accountId": 900001, "mask": "1234", "detail": {"detailType": "CHK"}},
        {"accountId": 900002, "mask": "5678", "detail": {"detailType": "SAV"}},
    ])
    assert sorted(a["account_external_id"] for a in roster) == ["900001", "900002"]


def test_mask_normalisation():
    assert download._mask("1234") == "…1234"
    assert download._mask("…1234") == "…1234"
    assert download._mask(None) is None
    assert download._mask("") is None


def test_parse_statement_date():
    assert download.parse_statement_date("Dec 17, 2025") == date(2025, 12, 17)
    assert download.parse_statement_date("Jan 21, 2025") == date(2025, 1, 21)
    assert download.parse_statement_date(" September 3, 2024 ") == date(2024, 9, 3)
    # unparseable / header / blank rows are skipped, not fatal
    assert download.parse_statement_date("Statement") is None
    assert download.parse_statement_date("") is None
    assert download.parse_statement_date("Foo 40, 2025") is None    # bad day
    assert download.parse_statement_date("Zzz 1, 2025") is None     # bad month


def test_statement_years_window_and_availability():
    # newest-first, bounded by the window
    assert download.statement_years(date(2024, 3, 1), date(2026, 8, 1)) == \
        [2026, 2025, 2024]
    # no since → just the until year unless availability widens it
    assert download.statement_years(None, date(2026, 8, 1)) == [2026]
    # intersected with the years the filter actually offers
    assert download.statement_years(
        date(2023, 1, 1), date(2026, 1, 1),
        available=[2019, 2020, 2024, 2025, 2026]) == [2026, 2025, 2024]
    # since None + availability → page all offered years
    assert download.statement_years(
        None, date(2026, 1, 1), available=[2024, 2025, 2026]) == \
        [2026, 2025, 2024]


def test_build_manifest_lifecycle():
    accts = [{"account_external_id": "900001"}]
    m = download.build_manifest("in-progress", accounts=accts,
                                counts={}, since=date(2026, 1, 1),
                                until=date(2026, 8, 11), dry_run=False,
                                documents=True)
    assert m["status"] == "in-progress"
    assert m["source"] == "chase"
    assert m["account_external_ids"] == ["900001"]
    assert m["window"] == {"since": "2026-01-01", "until": "2026-08-11"}

    done = download.build_manifest("complete", accounts=accts,
                                   counts={"transactions_files": 2},
                                   since=None, until=date(2026, 8, 11),
                                   dry_run=False, documents=False)
    assert done["status"] == "complete"
    assert done["window"]["since"] is None
    assert done["counts"]["transactions_files"] == 2
