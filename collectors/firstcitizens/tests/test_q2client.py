"""Unit tests for q2client — the browserless Q2 wire contract (endpoints,
logonUser classification, roster / statement / target parsing).

Synthetic payloads only: no real account numbers, balances, delivery ids, or
phone fragments.
"""
from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import q2client  # noqa: E402


# ============================================================
# Endpoint builders
# ============================================================

def test_history_page_url_paginates_and_sorts():
    url = q2client.account_history_page_url("A1", 2)
    assert "/accountHistory/A1?" in url
    assert "page[number]=2" in url
    assert f"page[size]={q2client.HISTORY_PAGE_SIZE}" in url
    assert "sort=postedDate%1Fd" in url          # postedDate, descending (US-sep)
    assert "page[size]=25" in q2client.account_history_page_url("A1", 1, 25)


def test_history_data_and_transactions():
    body = {"data": {"transactions": [{"transactionId": "t1"}],
                     "transactionCount": 5,
                     "oldestTransactionDate": "2023-08-15T00:00:00Z"}}
    d = q2client.history_data(body)
    assert d["transactionCount"] == 5
    assert q2client.history_transactions(body) == [{"transactionId": "t1"}]
    # Tolerant of junk / list-shaped data.
    assert q2client.history_data({}) == {}
    assert q2client.history_transactions({"data": []}) == []
    assert q2client.history_transactions({}) == []


def test_endpoint_builders():
    assert q2client.logon_url().endswith("/mobilews/logonUser")
    assert q2client.accounts_url().endswith("/mobilews/accounts")
    assert q2client.account_history_url("A1").endswith("/accountHistory/A1")
    assert q2client.account_statement_list_url("A1").endswith(
        "/accountStatement/A1")
    assert q2client.account_statement_pdf_url("A1", "DOC").endswith(
        "/accountStatement/A1/DOC/pdf")
    # All under the digitalbanking FCBTCOnline app base.
    assert q2client.accounts_url().startswith(q2client.MOBILEWS)


def test_export_url_maps_handle_to_q2_format():
    assert q2client.account_export_url("A1", "csv").endswith("/A1/Csv")
    assert q2client.account_export_url("A1", "qfx").endswith("/A1/QFX_1_0_2")
    import pytest
    with pytest.raises(KeyError):
        q2client.account_export_url("A1", "nope")


def test_default_formats_are_known_handles():
    for h in q2client.DEFAULT_EXPORT_FORMATS:
        assert h in q2client.EXPORT_FORMATS


# ============================================================
# SPA route auth detection (never a bare URL match)
# ============================================================

def test_is_authed_url():
    base = q2client.APP_UUX
    assert q2client.is_authed_url(f"{base}#/landingPage")
    assert not q2client.is_authed_url(f"{base}#/login")
    assert not q2client.is_authed_url(f"{base}#/login/mfa/targets")
    assert not q2client.is_authed_url("")


def test_is_mfa_url():
    base = q2client.APP_UUX
    assert q2client.is_mfa_url(f"{base}#/login/mfa/targets")
    assert q2client.is_mfa_url(f"{base}#/login/mfa/entertarget")
    assert not q2client.is_mfa_url(f"{base}#/login")
    assert not q2client.is_mfa_url(f"{base}#/landingPage")


def test_mfa_selectors_and_target_prefix():
    # The terminal-2FA driver keys the delivery button off these.
    assert q2client.MFA_TARGET_PREFIX["sms"] == "Text:"
    assert q2client.MFA_TARGET_PREFIX["voice"] == "Call:"
    assert "btnTacTarget" in q2client.SEL_MFA_TARGET
    assert q2client.SEL_MFA_CODE == "#tacEntry"
    assert "btnSubmit" in q2client.SEL_MFA_SUBMIT
    assert "btnRegister" in q2client.SEL_MFA_REGISTER
    assert q2client.URL_MARK_MFA_ENTER.endswith("/entertarget")


# ============================================================
# postedDate window (--lookback narrows the fetch server-side)
# ============================================================

def test_posted_date_range_format():
    from datetime import date
    v = q2client.posted_date_range(date(2026, 7, 1), date(2026, 8, 14))
    # M/D/YYYY, no leading zeros, US-separated, end-of-day on the `to` end.
    assert v == "7/1/2026\x1f8/14/2026 23:59:59.999"


def test_history_page_url_carries_posted_date():
    from datetime import date
    pd = q2client.posted_date_range(date(2026, 7, 1), date(2026, 8, 14))
    url = q2client.account_history_page_url("A1", 1, posted_date=pd)
    assert "&postedDate=7/1/2026%1F8/14/2026%2023%3A59%3A59.999" in url
    # No filter param when none is given (full history) — the sort key's
    # own "postedDate" is not the filter.
    assert "&postedDate=" not in q2client.account_history_page_url("A1", 1)


def test_export_url_carries_posted_date():
    from datetime import date
    pd = q2client.posted_date_range(date(2026, 1, 1), date(2026, 12, 31))
    url = q2client.account_export_url("A1", "csv", posted_date=pd)
    assert url.startswith("https://") and "/A1/Csv?postedDate=" in url
    assert "postedDate" not in q2client.account_export_url("A1", "csv")


# ============================================================
# logonUser classification (the auth signal)
# ============================================================

def _logon_2fa_body():
    return {"data": {"userProfileData": None, "accessCodeTargets": [
        {"notificationType": 3, "display": "Text: (XXX) XXX-XXXX", "value": "1001"},
        {"notificationType": 2, "display": "Call: (XXX) XXX-XXXX", "value": "1002"},
    ]}}


def _logon_authed_body():
    return {"data": {"userProfileData": {"name": "example"},
                     "accessCodeTargets": None}}


def test_classify_203_with_targets_needs_2fa():
    out = q2client.classify_logon(203, _logon_2fa_body())
    assert out.needs_2fa and not out.authenticated
    assert [t.kind for t in out.targets] == ["sms", "voice"]
    assert out.targets[0].value == "1001"


def test_classify_200_no_targets_is_authenticated():
    out = q2client.classify_logon(200, _logon_authed_body())
    assert out.authenticated and not out.needs_2fa


def test_classify_200_bare_is_authenticated():
    # A 200 with no targets and no explicit userProfileData is still treated
    # as authenticated (nothing signals a pending challenge).
    out = q2client.classify_logon(200, {"data": {}})
    assert out.authenticated and not out.needs_2fa


def test_classify_non2xx_without_targets_is_neither():
    out = q2client.classify_logon(500, {"data": {}})
    assert not out.authenticated and not out.needs_2fa
    assert out.status == 500


def test_parse_access_code_targets_tolerates_missing_fields():
    assert q2client.parse_access_code_targets({}) == ()
    targets = q2client.parse_access_code_targets(
        {"data": {"accessCodeTargets": [{"notificationType": 9, "value": "x"}]}})
    assert targets[0].kind == "other" and targets[0].value == "x"


# ============================================================
# Roster projection + deposit filter
# ============================================================

def _acct(code="D", ptype="Checking", acct_id="7000001"):
    return {
        "id": acct_id,
        "accountNumber": "XXXXXX0000",
        "extended": {
            "hydraProductTypeCode": code,
            "productTypeName": ptype,
            "productName": f"Example {ptype}",
            "nickName": ptype,
            "accountNumberInternal": "XXXXXX0000",
            "balanceDescription1": "Available Balance", "balance1": "0.00",
            "balanceDescription2": "Current Balance", "balance2": "0.00",
        },
    }


def test_is_deposit_account():
    assert q2client.is_deposit_account(_acct(code="D"))
    assert not q2client.is_deposit_account(_acct(code="C"))   # card/credit
    assert not q2client.is_deposit_account({"extended": {}})


def test_parse_accounts_keeps_only_deposits_by_default():
    body = {"data": [_acct(code="D", acct_id="7000001"),
                     _acct(code="C", ptype="Card", acct_id="7000002")]}
    rows = q2client.parse_accounts(body)
    assert [r["id"] for r in rows] == ["7000001"]
    r = rows[0]
    assert r["product_type_name"] == "Checking"
    assert r["hydra_product_type_code"] == "D"
    assert {"description": "Available Balance", "value": "0.00"} in r["balances"]


def test_parse_accounts_can_include_non_deposits():
    body = {"data": [_acct(code="D"), _acct(code="C", acct_id="7000002")]}
    assert len(q2client.parse_accounts(body, deposit_only=False)) == 2


def test_parse_accounts_tolerates_junk():
    assert q2client.parse_accounts({}) == []
    assert q2client.parse_accounts({"data": [None, 3, "x"]}) == []


# ============================================================
# Statement listing projection
# ============================================================

def test_parse_statement_list():
    body = {"data": [
        {"period": "07/31/2026", "value": "DOC1"},
        {"period": "06/30/2026", "value": "DOC2"},
        {"period": "05/31/2026", "value": ""},        # no doc id → dropped
    ]}
    rows = q2client.parse_statement_list(body)
    assert rows == [
        {"period": "07/31/2026", "doc_id": "DOC1"},
        {"period": "06/30/2026", "doc_id": "DOC2"},
    ]
