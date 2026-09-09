"""Unit tests for download.py's browserless helpers — the roster reducers,
export window, filename sanitising, coverage and manifest — plus the
browser-side helpers whose failure is silent: the statement row-save driver,
the statement pass's failure accounting, and the export form's account-picker
guard, all exercised against a stub page. The stub models sync Playwright's
download expectation faithfully enough to price a missing trigger. The scrape
itself needs a live authenticated page and is validated on a real run.
Synthetic data only.
"""
from __future__ import annotations

import json
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
              "/svc/rr/accounts/secure/v2/account/detail/card/list",
              "/svc/rr/accounts/secure/overview/card/v2/list",
              "/svc/rl/accounts/secure/v1/dashboard/module/list?context=X",
              "/svc/rr/documents/secure/v1/menu/list",
              "/svc/rr/accounts/secure/v1/account/activity/download/options/list",
              "/svc/rr/accounts/secure/v1/account/activity/download/dda/list"):
        assert download.ROSTER_URL_RE.search(u), u
    for u in ("/svc/rr/accounts/secure/v1/menu/list",
              "/svc/rr/accounts/secure/card/rewards/v2/summary/list",
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


def test_parse_download_options_keeps_deposit_accounts():
    body = {"downloadAccountActivityOptions": [
        {"accountId": 900001, "detailType": "CHK", "summaryType": "DDA",
         "nickName": "Example Checking", "mask": "1234"},
        {"accountId": 900002, "detailType": "SAV", "summaryType": "DDA",
         "nickName": "SAVINGS", "mask": "5678"},
    ]}
    out = download.parse_download_options(body)
    assert [a["account_external_id"] for a in out] == ["900001", "900002"]
    assert [a["account_type"] for a in out] == ["CHK", "SAV"]
    assert out[0]["mask"] == "…1234"          # normalised to last-4
    assert out[0]["currency"] == "USD"
    assert [a["product"] for a in out] == ["dda", "dda"]


def test_parse_download_options_keeps_cards_and_stamps_product():
    # Both kinds survive the reducer and are told apart by `product`; a row
    # that is neither (the documents menu's ATM/UKN entry) is dropped.
    body = {"downloadAccountActivityOptions": [
        {"accountId": 900001, "detailType": "CHK", "summaryType": "DDA",
         "nickName": "Example Checking", "mask": "1234"},
        {"accountId": 900003, "detailType": "BAC", "summaryType": "CARD",
         "nickName": "Example Card", "mask": "9012"},
        {"accountId": 900009, "detailType": "ATM", "summaryType": "UKN",
         "nickName": "Debit card", "mask": "3456"},
    ]}
    out = download.parse_download_options(body)
    assert [a["account_external_id"] for a in out] == ["900001", "900003"]
    assert [a["product"] for a in out] == ["dda", "card"]
    assert out[1]["account_type"] == "BAC"
    assert out[1]["mask"] == "…9012"


def test_parse_download_options_empty():
    assert download.parse_download_options({}) == []


def test_parse_download_options_skips_malformed_entries():
    # One unusable entry drops its own row, never the walk: this reducer runs
    # after the login has already been paid for, so a crash here would cost a
    # second one.
    body = {"downloadAccountActivityOptions": [
        None, "x", 7,
        {"accountId": 900001, "detailType": "CHK", "summaryType": "DDA",
         "nickName": "Example Checking", "mask": "1234"},
    ]}
    out = download.parse_download_options(body)
    assert [a["account_external_id"] for a in out] == ["900001"]


def test_parse_account_detail():
    # The per-account shape the overview fires (DESIGN.md §B).
    body = {"accountId": 900001, "nickname": "Example Checking", "mask": "1234",
            "detail": {"detailType": "CHK", "presentBalance": 2257.50,
                       "available": 2200.00}}
    out = download.parse_account_detail(body)
    assert out == [{
        "account_external_id": "900001", "account_type": "CHK",
        "nickname": "Example Checking", "mask": "…1234", "currency": "USD",
        "product": "dda", "balance": 2257.50,
    }]
    # falls back to `available` when presentBalance is absent
    body2 = {"accountId": 900002, "mask": "5678",
             "detail": {"detailType": "SAV", "available": 10.0}}
    assert download.parse_account_detail(body2)[0]["balance"] == 10.0
    # a non-account body yields nothing
    assert download.parse_account_detail({"downloadAccountActivityOptions": []}) == []


CARD_DETAIL_BODY = {
    "accountId": 900003, "nickname": "Example Card", "mask": "9012",
    "detail": {
        "detailType": "BAC", "cardType": "VISA", "productCode": "EXAMPLE",
        "currentBalance": 1200.00, "creditLimit": 10000.00,
        "availableCredit": 8800.00, "pendingChargesAmount": 75.00,
        "lastStmtBalance": 900.00, "lastStmtDate": "20260828",
        "nextPaymentDueDate": "20260925", "nextPaymentAmount": 40.00,
        "nextClosingDate": "20260928",
    },
}


def test_parse_account_detail_card_branch_keeps_owed_balance_positive():
    # A card carries none of the deposit balance keys; without its own branch
    # the reducer would emit balance=None and lose the liability entirely.
    out = download.parse_account_detail(CARD_DETAIL_BODY)
    assert out == [{
        "account_external_id": "900003", "account_type": "BAC",
        "nickname": "Example Card", "mask": "…9012", "currency": "USD",
        "product": "card",
        # Provider-verbatim: owed is POSITIVE and limit − owed == available.
        "balance": 1200.00,
        "credit_limit": 10000.00, "available_credit": 8800.00,
        "pending_charges_amount": 75.00,
        "last_statement_balance": 900.00, "last_statement_date": "2026-08-28",
        "next_payment_due_date": "2026-09-25", "next_payment_amount": 40.00,
        "next_closing_date": "2026-09-28",
    }]
    rec = out[0]
    assert rec["credit_limit"] - rec["balance"] == rec["available_credit"]


def test_parse_account_detail_card_with_optional_keys_absent():
    # Every card key is optional: a sparse detail yields the full record shape
    # with None in the gaps, never a missing key or a crash.
    sparse = download.parse_account_detail(
        {"accountId": 900004, "mask": "3456",
         "detail": {"detailType": "BAC", "currentBalance": 0.0}})[0]
    full = download.parse_account_detail(CARD_DETAIL_BODY)[0]
    assert sparse["balance"] == 0.0             # zero, never negative
    assert set(sparse) == set(full)             # same record shape
    for key in ("credit_limit", "available_credit", "pending_charges_amount",
                "last_statement_balance", "last_statement_date",
                "next_payment_due_date", "next_payment_amount",
                "next_closing_date", "nickname"):
        assert sparse[key] is None, key


def test_parse_account_detail_ignores_a_body_that_claims_neither_product():
    # Both branches are chosen positively. An error or partial body that still
    # carries an accountId must reduce to NOTHING: taking the deposit branch
    # would stamp product='dda' on what may be a card, and the merge below
    # would then carry a debt into gold as a cash asset.
    assert download.parse_account_detail(
        {"accountId": 900003, "mask": "9012", "detail": {}}) == []
    assert download.parse_account_detail(
        {"accountId": 900003, "mask": "9012",
         "detail": {"errorCode": "SERVICE_UNAVAILABLE"}}) == []
    assert download.parse_account_detail(
        {"accountId": 900003, "detail": {"detailType": "ZZZ"}}) == []
    # the account itself is not lost: the download-options roster still has it
    assert download.collect_accounts([
        {"accountId": 900003, "mask": "9012", "detail": {}},
        {"downloadAccountActivityOptions": [
            {"accountId": 900003, "detailType": "BAC", "summaryType": "CARD",
             "nickName": "Example Card", "mask": "9012"}]},
    ])[0]["product"] == "card"


def test_collect_accounts_never_downgrades_an_established_card():
    # A later body reducing to the deposit shape must not flip an account
    # already known to be a card.
    card = {"account_external_id": "900003", "product": "card",
            "balance": 1200.00}
    roster = download.collect_accounts([
        {"cardAccountOverviews": [{"cardAccounts": [
            {"accountId": 900003, "mask": "9012", "accountType": "BAC",
             "cardAccountDetail": {"currentBalance": 1200.00}}]}]},
        {"downloadAccountActivityOptions": [
            {"accountId": 900003, "detailType": "CHK", "summaryType": "DDA",
             "nickName": "Example", "mask": "9012"}]},
    ])
    assert roster[0]["product"] == card["product"]
    assert roster[0]["balance"] == card["balance"]


def test_card_record_accepts_the_overview_balance_alias():
    # The overview roster names the same figure `outstandingBalance`.
    rec = download.card_record(
        900003, "Example Card", "9012",
        {"outstandingBalance": 1200.00, "creditLimit": 10000.00},
        account_type="BAC")
    assert rec["balance"] == 1200.00
    assert rec["product"] == "card"


def test_card_record_keeps_pending_charges_apart_from_the_balance():
    # The exports carry posted rows only, so a balance reconstructed from the
    # transaction history lands short of the live balance by exactly the
    # unposted activity. Carrying that figure makes the gap an identity a
    # canary can assert, instead of a drift indistinguishable from a bug.
    rec = download.card_record(
        900003, "Example Card", "9012",
        {"currentBalance": 1200.00, "pendingChargesAmount": 75.00},
        account_type="BAC")
    assert rec["pending_charges_amount"] == 75.00
    derived_from_posted_rows = 1125.00
    assert derived_from_posted_rows + rec["pending_charges_amount"] == \
        rec["balance"]


def test_card_record_leaves_pending_charges_absent_not_zero():
    # A detail block without the key must not fabricate a 0.0 — that reads as
    # "nothing pending" and would silently turn a real gap into a failed canary.
    rec = download.card_record(900005, "Other Card", "7788",
                               {"currentBalance": 0.0}, account_type="BAC")
    assert rec["pending_charges_amount"] is None
    # the record shape stays fixed, so the column is present-and-NULL in silver
    assert "pending_charges_amount" in rec


def test_deposit_record_carries_no_card_only_fields():
    # The card fields live on the card branch alone: even with the key planted
    # on it, a deposit detail keeps its own shape unchanged.
    dda = download.parse_account_detail(
        {"accountId": 900001, "nickname": "Example Checking", "mask": "1234",
         "detail": {"detailType": "CHK", "presentBalance": 2257.50,
                    "pendingChargesAmount": 75.00}})[0]
    assert set(dda) == {"account_external_id", "account_type", "nickname",
                        "mask", "currency", "product", "balance"}


def test_ymd_normalises_and_rejects_sentinels():
    assert download._ymd("20260828") == "2026-08-28"
    assert download._ymd(None) is None
    assert download._ymd("") is None
    assert download._ymd("2026-08-28") is None      # already ISO, not YYYYMMDD
    assert download._ymd("00000000") is None        # zero sentinel
    assert download._ymd("20261332") is None        # not a real date


def test_parse_card_overview():
    body = {"cardAccountOverviews": [{"cardAccounts": [
        {"accountId": 900003, "nickname": "Example Card", "mask": "9012",
         "accountType": "BAC",
         "cardAccountDetail": {"currentBalance": 1200.00,
                               "creditLimit": 10000.00,
                               "availableCredit": 8800.00}},
        {"accountId": 900005, "nickname": "Other Card", "mask": "7788",
         "accountType": "BAC",
         "cardAccountDetail": {"currentBalance": 0.0}},
    ]}]}
    out = download.parse_card_overview(body)
    assert [a["account_external_id"] for a in out] == ["900003", "900005"]
    assert all(a["product"] == "card" for a in out)
    assert out[0]["available_credit"] == 8800.00
    assert download.parse_card_overview({}) == []
    # a malformed group, or a malformed card inside one, drops its own row
    assert download.parse_card_overview(
        {"cardAccountOverviews": [None, "x", {"cardAccounts": [None]}]}) == []


def test_expand_module_cache_unwraps_the_dashboard_envelope():
    # The card roster/detail calls never hit the wire on their own — they are
    # only ever cached inside dashboard/module/list.
    inner = {"cardAccountOverviews": [{"cardAccounts": [
        {"accountId": 900003, "mask": "9012", "accountType": "BAC",
         "cardAccountDetail": {"currentBalance": 1200.00}}]}]}
    envelope = {"code": "SUCCESS", "modules": [], "cache": [
        {"url": "/svc/rr/accounts/l4/creditjourney/v1/status/list",
         "request": {}, "response": {"score": 800}},
        {"url": "/svc/rr/accounts/secure/overview/card/v2/list",
         "request": {}, "response": inner},
    ]}
    out = download.expand_module_cache([envelope])
    assert envelope in out and inner in out
    # the noise entry stays wrapped
    assert {"score": 800} not in out
    # a body that is not an envelope passes through untouched
    assert download.expand_module_cache([{"accountId": 1}]) == [{"accountId": 1}]


def test_unwrap_roster_bodies_persists_the_inner_bodies_not_the_envelope():
    # What reaches raw/: the roster body the envelope cached, not the envelope
    # with its dozen unrelated cached responses.
    inner = {"cardAccountOverviews": []}
    envelope = {"code": "SUCCESS", "modules": [], "cache": [
        {"url": "/svc/rr/accounts/l4/creditjourney/v1/status/list",
         "response": {"score": 800}},
        {"url": "/svc/rr/accounts/secure/overview/card/v2/list",
         "response": inner},
    ]}
    assert download.unwrap_roster_bodies(envelope) == [inner]
    # a plain roster body is kept as-is …
    assert download.unwrap_roster_bodies({"accountId": 1}) == [{"accountId": 1}]
    # … and so is an envelope whose inner URLs no longer match, so a shape
    # drift stays diagnosable from the run that hit it
    drifted = {"cache": [{"url": "/svc/rr/accounts/secure/overview/card/v9/list",
                          "response": {"cardAccountOverviews": []}}]}
    assert download.unwrap_roster_bodies(drifted) == [drifted]


def test_collect_accounts_walks_the_envelope_and_merges_card_detail():
    envelope = {"cache": [
        {"url": "/svc/rr/accounts/secure/overview/card/v2/list",
         "response": {"cardAccountOverviews": [{"cardAccounts": [
             {"accountId": 900003, "nickname": "Example Card", "mask": "9012",
              "accountType": "BAC",
              "cardAccountDetail": {"currentBalance": 1200.00,
                                    "creditLimit": 10000.00}}]}]}},
    ]}
    options = {"downloadAccountActivityOptions": [
        {"accountId": 900001, "detailType": "CHK", "summaryType": "DDA",
         "nickName": "Example Checking", "mask": "1234"},
        {"accountId": 900003, "detailType": "BAC", "summaryType": "CARD",
         "nickName": "Example Card", "mask": "9012"},
    ]}
    by_id = {a["account_external_id"]: a
             for a in download.collect_accounts([envelope, options])}
    assert set(by_id) == {"900001", "900003"}
    assert by_id["900001"]["product"] == "dda"
    # the download-options row wins on the shared fields but does not erase
    # the detail it has no opinion about
    assert by_id["900003"]["product"] == "card"
    assert by_id["900003"]["balance"] == 1200.00
    assert by_id["900003"]["credit_limit"] == 10000.00


def test_accounts_by_product_defaults_to_deposit():
    grouped = download.accounts_by_product([
        {"account_external_id": "900001", "product": "dda"},
        {"account_external_id": "900003", "product": "card"},
        {"account_external_id": "900002"},          # pre-card bronze shape
    ])
    assert sorted(grouped) == ["card", "dda"]
    assert [a["account_external_id"] for a in grouped["dda"]] == \
        ["900001", "900002"]


def test_parse_documents_menu_drops_the_non_account_row():
    body = {"items": [
        {"accountId": 900001, "type": "CHK", "summaryType": "DDA",
         "docItems": ["STATEMENTS", "TAX_DOCUMENTS"]},
        {"accountId": 900009, "type": "ATM", "summaryType": "UKN",
         "docItems": ["ATM_RECEIPTS"]},
        {"accountId": 900003, "type": "BAC", "summaryType": "CARD",
         "docItems": ["STATEMENTS", "YEAR_END_STATEMENTS"]},
    ]}
    out = download.parse_documents_menu(body)
    assert [r["account_external_id"] for r in out] == ["900001", "900003"]
    assert [r["product"] for r in out] == ["dda", "card"]
    assert download.statement_accounts([body]) == {"900001", "900003"}
    # a card the menu lists WITHOUT statements is excluded
    assert download.statement_accounts([{"items": [
        {"accountId": 900003, "type": "BAC", "summaryType": "CARD",
         "docItems": ["NOTICES"]}]}]) == set()
    # no menu seen at all → None, so the caller filters nothing
    assert download.statement_accounts([{"accountId": 900001}]) is None


def test_export_route_is_a_product_tuple():
    dda = download.export_route(
        {"account_external_id": "900001", "product": "dda"})
    card = download.export_route(
        {"account_external_id": "900003", "product": "card"})
    assert dda.endswith("/downloads/900001/DDA/CHK")
    assert card.endswith("/downloads/900003/CARD/BAC")
    # a savings account routes through the deposit tuple like checking, and a
    # record with no product predates the discriminator
    assert download.export_route(
        {"account_external_id": "900002", "product": "dda",
         "account_type": "SAV"}).endswith("/DDA/CHK")
    assert download.export_route(
        {"account_external_id": "900002"}).endswith("/DDA/CHK")


def test_collect_accounts_dedupes_both_shapes():
    detail = {"accountId": 900001, "nickname": "Example Checking", "mask": "1234",
              "detail": {"detailType": "CHK", "presentBalance": 2257.50}}
    options = {"downloadAccountActivityOptions": [
        {"accountId": 900001, "detailType": "CHK", "summaryType": "DDA",
         "nickName": "Example Checking", "mask": "1234"},
        {"accountId": 900002, "detailType": "SAV", "summaryType": "DDA",
         "nickName": "SAVINGS", "mask": "5678"},
    ]}
    roster = download.collect_accounts([detail, options])
    by_id = {a["account_external_id"]: a for a in roster}
    assert set(by_id) == {"900001", "900002"}          # deduped across shapes
    # the download-options record wins but keeps the account-detail balance
    assert by_id["900001"]["balance"] == 2257.50


def test_collect_accounts_keeps_cards_alongside_deposits():
    # The roster carries both products; a card known only from the download
    # options (its detail never arrived) is still rostered, without a balance.
    options = {"downloadAccountActivityOptions": [
        {"accountId": 900001, "detailType": "CHK", "summaryType": "DDA",
         "nickName": "Example Checking", "mask": "1234"},
        {"accountId": 900003, "detailType": "BAC", "summaryType": "CARD",
         "nickName": "Example Card", "mask": "9012"},
    ]}
    by_id = {a["account_external_id"]: a
             for a in download.collect_accounts([options])}
    assert set(by_id) == {"900001", "900003"}
    assert by_id["900003"]["product"] == "card"
    assert by_id["900003"].get("balance") is None


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


def test_statement_click_paths_prefer_the_anchor_then_the_dropdown():
    anchor, legacy = download.statement_click_paths(3)
    # newest shape: one click on the row's direct download anchor
    assert anchor == ["#icon-accountsTable-STATEMENTS-row3-cell3"
                      "-requestThisDocumentLink-download"]
    # legacy shape: open the row dropdown, then pick "Save as PDF"
    assert legacy == ["#header-accountsTable-STATEMENTS-row3-cell3"
                      "-downloadDocumentDropdown",
                      "#item-0-3-downloadPDFOption"]


class _StubElement:
    """An element the browser-side helpers read rather than click. `raises`
    models a node that detached mid-rerender: reading an attribute throws
    instead of returning a value."""

    def __init__(self, *, text: str = "", raises: bool = False, **attrs):
        self.text = text
        self.attrs = attrs
        self.raises = raises

    def get_attribute(self, name):
        if self.raises:
            raise RuntimeError("node is detached from the document")
        return self.attrs.get(name)

    def text_content(self):
        return self.text


class _StubPage:
    """Minimal stand-in for the Playwright page the browser-side helpers drive.

    `clickable` selectors accept a click and count as visible; `nodes` are
    elements a helper reads (the account picker's value, an option's label, a
    statement row's date cell); `roles` are the accessible names a role-based
    click fires. `mounts_after` delays every lookup by that many polls, so a
    control that renders late can be told from one that is absent. A successful
    activation calls `on_activate(target)`, which lets a test rebind the page
    the way clicking a picker option would."""

    def __init__(self, clickable=(), *, nodes=None, roles=(), mounts_after=0,
                 on_activate=None):
        self.clickable = set(clickable)
        self.nodes: dict = dict(nodes or {})
        self.roles = set(roles)
        self.mounts_after = mounts_after
        self.on_activate = on_activate
        self.clicks: list[str] = []
        self.role_clicks: list[tuple[str, str]] = []
        self.saved: list[str] = []
        self.polls = 0
        self.download_wait_ms = 0
        self._fired = False

    # -- the mdsui surface, wired up by _stub_mdsui ----------------------
    def _mounted(self) -> bool:
        return self.polls >= self.mounts_after

    def find(self, selector: str):
        """mdsui.first_in_frames: the element if it exists at all."""
        if not self._mounted():
            return None
        node = self.nodes.get(selector)
        if node is not None:
            return node
        return _StubElement() if selector in self.clickable else None

    def locate(self, selector: str):
        """mdsui.locate: the element if it is visible."""
        return self.find(selector)

    def click(self, selector: str) -> bool:
        self.clicks.append(selector)
        if not self._mounted() or selector not in self.clickable:
            return False
        self._fired = True
        self._activate(selector)
        return True

    def click_role(self, role: str, name: str) -> bool:
        self.role_clicks.append((role, name))
        if name not in self.roles:
            return False
        self._activate(name)
        return True

    def _activate(self, target: str) -> None:
        if self.on_activate is not None:
            self.on_activate(target)

    # -- the page API the helpers use ------------------------------------
    def on(self, _event, _handler):
        return None

    def goto(self, *_a, **_k):
        return None

    def reload(self, *_a, **_k):
        return None

    def wait_for_load_state(self, *_a, **_k):
        return None

    def wait_for_timeout(self, _ms):
        self.polls += 1

    def expect_download(self, timeout=None):
        return _StubExpectDownload(self, timeout or 0)

    def save_as(self, path):
        self.saved.append(str(path))


class _StubExpectDownload:
    """Sync Playwright's download expectation, including the property that
    makes it expensive: __exit__ waits on the download future EVEN WHEN THE
    BODY RAISED, so a body that fails in milliseconds still pays the full
    timeout. The wait it would have cost is recorded in `download_wait_ms` — a
    generator-based context manager would let a body exception escape at the
    yield and model none of this."""

    def __init__(self, page, timeout: int):
        self._page = page
        self._timeout = timeout

    def __enter__(self):
        self._page._fired = False
        return _StubDownloadInfo(self._page)

    def __exit__(self, *_exc):
        if not self._page._fired:
            self._page.download_wait_ms += self._timeout
            raise TimeoutError("no download")
        return False


class _StubDownloadInfo:
    def __init__(self, page):
        self._page = page

    @property
    def value(self):
        return self._page


def _stub_mdsui(monkeypatch):
    """Route mdsui's frame-aware lookups at the stub page's own methods.

    Two real distinctions collapse here: `activate`'s shadow-piercing
    fallbacks reduce to the same click, and `locate` (visible) to
    `first_in_frames` (present). Both collapse conservatively — a control the
    stub refuses is one the real helpers might still have reached, never the
    other way round."""
    monkeypatch.setattr(download.mdsui, "click",
                        lambda page, selector, **_k: page.click(selector))
    monkeypatch.setattr(download.mdsui, "activate",
                        lambda page, selector, **_k: page.click(selector))
    monkeypatch.setattr(download.mdsui, "click_role",
                        lambda page, role, name, **_k: page.click_role(role, name))
    monkeypatch.setattr(download.mdsui, "locate",
                        lambda page, selector, **_k: page.locate(selector))
    monkeypatch.setattr(download.mdsui, "first_in_frames",
                        lambda page, selector, **_k: page.find(selector))


def test_save_statement_row_uses_the_anchor(monkeypatch, tmp_path):
    page = _StubPage(download.statement_click_paths(0)[0])
    _stub_mdsui(monkeypatch)
    assert download._save_statement_row(
        page, 0, tmp_path / "2026-08-28.pdf", 1000) is True
    # the anchor alone; the legacy dropdown is never touched
    assert page.clicks == download.statement_click_paths(0)[0]
    assert page.saved == [str(tmp_path / "2026-08-28.pdf")]
    assert page.download_wait_ms == 0


def test_save_statement_row_falls_back_to_the_legacy_dropdown(monkeypatch,
                                                              tmp_path):
    legacy = download.statement_click_paths(1)[1]
    page = _StubPage(legacy)                    # no anchor on this page
    _stub_mdsui(monkeypatch)
    assert download._save_statement_row(
        page, 1, tmp_path / "2026-07-28.pdf", 1000) is True
    # the legacy shape fires; the absent anchor is never clicked, because the
    # trigger is probed BEFORE the expectation is opened
    assert page.clicks == legacy
    assert page.saved == [str(tmp_path / "2026-07-28.pdf")]
    # and the missing anchor cost no download timeout — the whole point of the
    # probe: on a deep pass that tax is paid once per row.
    assert page.download_wait_ms == 0


def test_save_statement_row_reports_failure_when_no_shape_fires(monkeypatch,
                                                                tmp_path):
    page = _StubPage([])
    _stub_mdsui(monkeypatch)
    assert download._save_statement_row(
        page, 2, tmp_path / "2026-06-28.pdf", 1000) is False
    assert page.saved == []
    assert page.download_wait_ms == 0           # neither shape opened a wait


def test_save_statement_row_waits_only_for_a_trigger_that_is_there(monkeypatch,
                                                                   tmp_path):
    # The anchor exists but its click never produces a download: that row DOES
    # pay the timeout, which is what the probe cannot (and must not) avoid.
    anchor = download.statement_click_paths(0)[0][0]
    page = _StubPage([], nodes={anchor: _StubElement()})
    _stub_mdsui(monkeypatch)
    assert download._save_statement_row(
        page, 0, tmp_path / "2026-05-28.pdf", 1000) is False
    assert page.download_wait_ms == 1000


def test_save_visible_statements_counts_the_rows_that_failed(monkeypatch,
                                                             tmp_path):
    # Coverage is decided by failures, not by successes: a table that renders
    # and saves nothing must not read as a completed pass.
    page = _StubPage(nodes={
        download.STMT_DATE_CELL.format(n=0): _StubElement(text="Aug 28, 2026"),
        download.STMT_DATE_CELL.format(n=1): _StubElement(text="Jul 28, 2026"),
    })
    _stub_mdsui(monkeypatch)
    monkeypatch.setattr(download, "_save_statement_row",
                        lambda _page, n, _dest, _t: n == 0)
    assert download._save_visible_statements(
        page, tmp_path, date(2026, 1, 1), date(2026, 12, 31), set(), 1000) \
        == (1, 1)


def test_save_statements_fails_a_pass_that_could_select_no_year(monkeypatch,
                                                                tmp_path):
    # The year filter reading empty is a shape drift, not an account without
    # statements — a pass that paged nothing must not report coverage.
    monkeypatch.setattr(download, "_expand_statements", lambda _page: None)
    monkeypatch.setattr(download, "_available_statement_years",
                        lambda _page: None)
    monkeypatch.setattr(download, "_select_statement_year",
                        lambda _page, _year: False)
    saved, failed = download._save_statements(
        _StubPage(), tmp_path, date(2025, 1, 1), date(2026, 8, 11), 1000)
    assert (saved, failed) == (0, 1)
    # but a window the filter legitimately offers nothing for is not a failure
    monkeypatch.setattr(download, "_available_statement_years",
                        lambda _page: [2019])
    assert download._save_statements(
        _StubPage(), tmp_path, date(2025, 1, 1), date(2026, 8, 11), 1000) \
        == (0, 0)


def test_deposit_statements_file_under_a_deterministic_bucket(monkeypatch,
                                                              tmp_path):
    # The relationship-wide surface has no per-account selector, so the
    # directory is a bucket, not a claim about whose statement it is — and it
    # must not depend on the order the roster bodies arrived in. Under an
    # arbitrary "first account" the whole set moved between directories from
    # one run to the next and the loader's balance chain found nothing to
    # attribute.
    _stub_mdsui(monkeypatch)
    monkeypatch.setattr(download, "_wait_visible", lambda *_a, **_k: True)
    dirs = []
    monkeypatch.setattr(download, "_save_statements",
                        lambda _page, out_dir, *_a, **_k: (dirs.append(
                            out_dir.name), (2, 0))[1])
    accounts = [{"account_external_id": "900003"},
                {"account_external_id": "900001"}]
    saved, covered = download._download_deposit_statements(
        _StubPage(), accounts, tmp_path, None, date(2026, 8, 11), 1000)
    assert (saved, covered) == (2, 2)
    download._download_deposit_statements(
        _StubPage(), list(reversed(accounts)), tmp_path, None,
        date(2026, 8, 11), 1000)
    assert dirs == ["900001", "900001"]


def test_download_card_statements_leaves_a_failed_card_uncovered(monkeypatch,
                                                                 tmp_path):
    _stub_mdsui(monkeypatch)
    monkeypatch.setattr(download, "_wait_visible", lambda *_a, **_k: True)
    # (saved, failed) per card: the first card's rows all failed — the drift
    # that would otherwise report full coverage on an empty directory.
    per_card = {"900003": (0, 4), "900005": (2, 0)}
    monkeypatch.setattr(download, "_save_statements",
                        lambda _page, out_dir, *_a, **_k: per_card[out_dir.name])
    cards = [{"account_external_id": "900003", "mask": "…9012"},
             {"account_external_id": "900005", "mask": "…7788"}]
    saved, covered = download._download_card_statements(
        _StubPage(), cards, [], tmp_path, None, date(2026, 8, 11), 1000)
    assert saved == 2
    assert covered == 1


def test_download_card_statements_skips_a_card_the_menu_lists_without_them(
        monkeypatch, tmp_path):
    # The documents menu is the only thing that says which cards carry
    # statements at all. A card it lists without them is covered — there is
    # nothing to fetch — and must not be paged; a card it does list is paged
    # as usual, so the filter cannot quietly skip the whole roster.
    _stub_mdsui(monkeypatch)
    monkeypatch.setattr(download, "_wait_visible", lambda *_a, **_k: True)
    paged = []
    monkeypatch.setattr(download, "_save_statements",
                        lambda _page, out_dir, *_a, **_k: (paged.append(
                            out_dir.name), (2, 0))[1])
    menu = {"items": [
        {"accountId": 900003, "type": "BAC", "summaryType": "CARD",
         "docItems": ["NOTICES"]},
        {"accountId": 900005, "type": "BAC", "summaryType": "CARD",
         "docItems": ["STATEMENTS"]},
    ]}
    cards = [{"account_external_id": "900003", "mask": "…9012"},
             {"account_external_id": "900005", "mask": "…7788"}]
    saved, covered = download._download_card_statements(
        _StubPage(), cards, [menu], tmp_path, None, date(2026, 8, 11), 1000)
    assert paged == ["900005"]
    assert (saved, covered) == (2, 2)

    # No menu captured at all: unknown filters nothing, so every card is
    # paged rather than every card being skipped on missing evidence.
    paged.clear()
    saved, covered = download._download_card_statements(
        _StubPage(), cards, [], tmp_path, None, date(2026, 8, 11), 1000)
    assert paged == ["900003", "900005"]
    assert (saved, covered) == (4, 2)


# ---- the export form's account-picker guard -----------------------------

PICKER = download.SEL_ACCOUNT_SELECT


def _picker_page(bound, clickable=(PICKER,), nodes=None, **kwargs):
    """A page whose export form carries the account picker, bound to `bound`."""
    all_nodes = {PICKER: _StubElement(value=bound)}
    all_nodes.update(nodes or {})
    return _StubPage(clickable, nodes=all_nodes, **kwargs)


def test_ensure_export_account_proceeds_when_the_form_has_no_picker(monkeypatch):
    # No picker at all: the route alone decides the account, which is how the
    # deposit path has always worked — but only after polling for it.
    page = _StubPage()
    _stub_mdsui(monkeypatch)
    assert download._ensure_export_account(page, "900001") is True
    assert page.polls == int(download.PICKER_WAIT_SECS * 2)


def test_ensure_export_account_waits_for_a_picker_that_mounts_late(monkeypatch):
    # A hash-only navigation re-renders the form under the previous account's
    # controls; reading too early would see no picker and export the WRONG
    # account under this one's id.
    page = _picker_page("900003", mounts_after=3)
    _stub_mdsui(monkeypatch)
    assert download._ensure_export_account(page, "900001") is False
    assert page.polls == 3


def test_ensure_export_account_refuses_a_picker_it_cannot_read(monkeypatch):
    _stub_mdsui(monkeypatch)
    # a node detached mid-rerender: present, but unreadable — never "absent"
    detached = _StubPage([PICKER], nodes={PICKER: _StubElement(raises=True)})
    assert download._ensure_export_account(detached, "900001") is False
    # and a picker that reports no binding at all
    assert download._ensure_export_account(_picker_page(""), "900001") is False


def test_ensure_export_account_refuses_the_positional_picker_variant(monkeypatch):
    # The other variant of the same picker keys its options by position, not by
    # account id, so nothing in it names the bound account. Refuse: the
    # exact-id picker's absence here does NOT mean the form has no picker.
    page = _StubPage([download.SEL_ACCOUNT_SELECT_INDEXED],
                     nodes={download.SEL_ACCOUNT_SELECT_INDEXED:
                            _StubElement(value="1")})
    _stub_mdsui(monkeypatch)
    assert download._ensure_export_account(page, "900001") is False


def test_ensure_export_account_accepts_a_form_already_bound(monkeypatch):
    page = _picker_page("900001")
    _stub_mdsui(monkeypatch)
    assert download._ensure_export_account(page, "900001") is True
    assert page.clicks == []                    # no switch attempted


def test_ensure_export_account_switches_through_the_role_click(monkeypatch):
    # The option's data-testid is built from its label, not from the account
    # id, so `_select_option`'s testid path is unusable here — but the option
    # carries the label as an attribute, which reaches the role-based click
    # mdsui documents as the proven MDS activation.
    label = "Example Checking (...1234)"
    picker = _StubElement(value="900003")
    page = _picker_page(
        "900003", nodes={PICKER: picker,
                         download.OPT_ACCOUNT.format(ext="900001"):
                         _StubElement(label=label)}, roles=[label])
    page.on_activate = lambda target: (picker.attrs.update(value="900001")
                                       if target == label else None)
    _stub_mdsui(monkeypatch)
    assert download._ensure_export_account(page, "900001") is True
    assert page.role_clicks == [("option", label)]


def test_ensure_export_account_refuses_when_the_switch_does_not_take(monkeypatch):
    # The option clicks, but the form stays bound elsewhere: exporting now
    # would file another account's activity under this account's id.
    option = download.OPT_ACCOUNT.format(ext="900001")
    page = _picker_page("900003", clickable=(PICKER, option))
    _stub_mdsui(monkeypatch)
    assert download._ensure_export_account(page, "900001") is False
    assert option in page.clicks


def test_new_coverage_counts_accounts_per_product():
    by_product = download.accounts_by_product([
        {"account_external_id": "900001", "product": "dda"},
        {"account_external_id": "900003", "product": "card"},
        {"account_external_id": "900005", "product": "card"},
    ])
    assert download.new_coverage(by_product) == {
        "dda": {"accounts": 1, "exported": 0, "statements": 0},
        "card": {"accounts": 2, "exported": 0, "statements": 0},
    }


def test_score_coverage_flags_each_product_on_its_own():
    full = {"dda": {"accounts": 1, "exported": 1, "statements": 1},
            "card": {"accounts": 2, "exported": 2, "statements": 2}}
    scored = download.score_coverage(full, documents=True)
    assert scored["dda"]["complete"] is True
    assert scored["card"]["complete"] is True
    assert scored["dda"]["accounts"] == 1           # counts survive scoring
    # a card export that failed flags the CARD product and nothing else
    short_export = download.score_coverage(
        {**full, "card": {"accounts": 2, "exported": 1, "statements": 2}},
        documents=True)
    assert short_export["card"]["complete"] is False
    assert short_export["dda"]["complete"] is True
    # a statement pass that didn't complete counts only when documents were on
    short_stmts = {**full, "card": {"accounts": 2, "exported": 2,
                                    "statements": 1}}
    assert download.score_coverage(
        short_stmts, documents=True)["card"]["complete"] is False
    assert download.score_coverage(
        short_stmts, documents=False)["card"]["complete"] is True
    # a deposit shortfall flags the deposit product, never the card one
    deposits_only = download.score_coverage(
        {"dda": {"accounts": 2, "exported": 0, "statements": 0}},
        documents=True)
    assert deposits_only["dda"]["complete"] is False
    assert download.score_coverage({}, documents=True) == {}


def test_walk_flags_a_card_shortfall_without_failing_the_run(monkeypatch,
                                                             tmp_path):
    # The protection a partial card dump needs lives in the per-product flag,
    # NOT in the run's status: `load` skips a non-complete run wholesale, so a
    # card shortfall must leave the deposit ledger loadable (and unprunable).
    roster = [{"account_external_id": "900001", "product": "dda",
               "mask": "…1234"},
              {"account_external_id": "900003", "product": "card",
               "mask": "…9012"}]
    monkeypatch.setattr(download, "_discover_accounts", lambda *_a, **_k: roster)
    monkeypatch.setattr(
        download, "_export_account",
        lambda _page, acct, *_a, **_k: (len(download.EXPORT_FORMATS)
                                        if acct["product"] == "dda" else 0))
    monkeypatch.setattr(download, "_download_statements",
                        lambda *_a, **_k: (4, {"dda": 1, "card": 0}))
    stats = download.walk(_StubPage(), tmp_path, until=date(2026, 8, 11))
    run = json.loads((Path(stats["run_dir"]) / "run.json").read_text())
    assert run["status"] == "complete"           # the deposit side still loads
    assert run["coverage"]["dda"]["complete"] is True
    assert run["coverage"]["card"] == {"accounts": 1, "exported": 0,
                                       "statements": 0, "complete": False}
    assert run["counts"]["transactions_files"] == len(download.EXPORT_FORMATS)


def test_walk_marks_a_fully_covered_run_complete_per_product(monkeypatch,
                                                             tmp_path):
    roster = [{"account_external_id": "900003", "product": "card",
               "mask": "…9012"}]
    monkeypatch.setattr(download, "_discover_accounts", lambda *_a, **_k: roster)
    monkeypatch.setattr(download, "_export_account",
                        lambda *_a, **_k: len(download.EXPORT_FORMATS))
    monkeypatch.setattr(download, "_download_statements",
                        lambda *_a, **_k: (7, {"card": 1}))
    stats = download.walk(_StubPage(), tmp_path, until=date(2026, 8, 11))
    run = json.loads((Path(stats["run_dir"]) / "run.json").read_text())
    assert run["status"] == "complete"
    assert run["coverage"]["card"]["complete"] is True
    assert run["counts"]["statements"] == 7


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
    assert m["coverage"] == {}

    done = download.build_manifest("complete", accounts=accts,
                                   counts={"transactions_files": 2},
                                   since=None, until=date(2026, 8, 11),
                                   dry_run=False, documents=False)
    assert done["status"] == "complete"
    assert done["window"]["since"] is None
    assert done["counts"]["transactions_files"] == 2


def test_build_manifest_carries_per_product_coverage():
    coverage = download.score_coverage(
        {"dda": {"accounts": 1, "exported": 1, "statements": 1},
         "card": {"accounts": 2, "exported": 1, "statements": 2}},
        documents=True)
    m = download.build_manifest(
        "complete", accounts=[{"account_external_id": "900003"}],
        counts={"transactions_files": 3, "statements": 9},
        since=None, until=date(2026, 8, 11), dry_run=False, documents=True,
        coverage=coverage)
    assert m["coverage"] == coverage
    # the run finished, so it loads; the shortfall is carried per product
    assert m["status"] == "complete"
    assert m["coverage"]["card"]["complete"] is False
    assert m["coverage"]["dda"]["complete"] is True
