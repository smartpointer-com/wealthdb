"""
Unit tests for pdf_parsers_svbdep.py.

These statements reach the parser as OCR output, so the fixtures below
are what the OCR of a statement looks like: layout-ordered lines,
including the character losses the recognition actually makes. The PDF
and OCR seam itself (``parse_svbdep_statement_pdf``) is exercised with
the OCR call stood in for, one text per raster scale, so the gates and
their escalation are tested without a real PDF or a real recogniser.

Every fixture is **synthetic** (repo policy §4): all-zero account
serials in the 10-digit shape these products use, a fabricated
registration, and round example amounts that satisfy the statements'
own arithmetic. No real balances, names, or account numbers appear.
"""

from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import collectorkit.pdf as ckpdf  # noqa: E402
import pdf_parsers_svbdep as ps  # noqa: E402


# ============================================================
# Fixtures — synthetic OCR output
# ============================================================

# A combined statement: checking and savings, each its own section.
# The arithmetic closes for both, and each ledger's running balance
# chains from its beginning balance to its stated ending one.
_COMBINED_TEXT = """\
Page 1 of 2
svb A Member of SVB Financial Group
Private Bank Combined Account Statement
Statement Date: 02-01-23
Statement from 01-01-23 to 01-31-23
Accounts Included:
Checking 0000000000
Savings 0000000001
EXAMPLE HOLDER - Example Property
CHECKING ACCOUNT
Account Number: 0000000000
Balance Summary
Beginning Balance as of 01-01-23 $1,000.00 Ending Balance as of 01-31-23 $1,600.00
(+) Deposits $2,000.00 Average Statement Balance: $1,300.00
(+) Interest Paid $10.00 Annual Percentage Yield Earned: 0.25%
(-) Withdrawals $1,400.00
(-) Service Charges $10.00
TRANSACTION DETAIL: Checking Account
Date Description Deposit Withdrawal Balance
Beginning Balance $1,000.00
01-04 EXAMPLE PAYER ACH CREDIT $2,000.00 $3,000.00
ID: 0000000000
01-09 EXAMPLE PAYEE TRANSFER $-1,400.00 $1,600.00
01-31 Service Charge $-10.00 $1,590.00
01-31 Interest Credited Deposit $10.00 $1,600.00
Ending Balance $1,600.00
CHECKS OUTSTANDING
DATE OR # AMOUNT CHECKBOOK RECONCILIATION
SAVINGS ACCOUNT
Account Number: 0000000001
Balance Summary
Beginning Balance as of 01-01-23 $5,000.00 Ending Balance as of 01-31-23 $4,020.00
(+) Deposits $0.00 Average Statement Balance: $4,500.00
(+) Interest Paid $20.00 Annual Percentage Yield Earned: 3.40%
(-) Withdrawals $1,000.00
(-) Service Charges $0.00
TRANSACTION DETAIL: Savings Account
Date Description Deposit Withdrawal Balance
Beginning Balance $5,000.00
01-15 EXAMPLE TRANSFER OUT $-1,000.00 $4,000.00
01-31 Interest Credited Deposit $20.00 $4,020.00
Ending Balance $4,020.00
ACCOUNT SUMMARY
This is a snapshot of your overall account activity.
"""

# The single-account 2021 layout, carrying the character losses the
# recognition makes: a "(-)" whose closing paren is gone, a date
# separator read as a colon, and a margin fragment glued to the head
# of a continuation header.
_SINGLE_TEXT = """\
svb A Member of SVB Financial Group
Private Bank Checking Account Statement
Account Number: 0000000000
Statement Date: 10-01-21
Statement from 09-01-21 to 09-30-21
EXAMPLE HOLDER - Example Property
BALANCE SUMMARY
Beginning Balance as of 09-01-21 $500.00 Ending Balance as of 09-30-21 $700.00
(+) Deposits $900.00 Average Statement Balance: $600.00
(+) Interest Paid $0.00 Annual Percentage Yield Earned: 0.05%
(- Withdrawals $700.00
(- Service Charges $0.00
TRANSACTION DETAIL
Date Description Deposit Withdrawal Balance
Beginning Balance $500.00
09-02 EXAMPLE DEPOSIT $900.00 $1,400.00
09:07 EXAMPLE WITHDRAWAL $-400.00 $1,000.00
Page 2 of 2
000000-00000-0 TRANSACTION DETAIL (Cont.)
Date Description Deposit Withdrawal Balance
09-30 EXAMPLE WITHDRAWAL $-300.00 $700.00
Ending Balance $700.00
CHECKS OUTSTANDING
"""

_MORTGAGE_TEXT = """\
Mortgage Loan Statement
svb > Statement Date: 01/14/22
Account Number 0000000002
EXAMPLE HOLDER
Account Information
Outstanding Principal $900,000.00
Interest Rate (Until 01/30) 5.0000%
Prepayment Penalty No
Transaction Activity (12/18/21 to 01/14/22)
Date Description Charges Payments
01/03/22 Payment Received - Thank You $0.00 $1,600.00
Past Payment Breakdown
Paid Last Paid Year to Date
Month
Principal $0.00 $0.00
Interest $1,600.00 $4,800.00
"""

_ANNUAL_TEXT = """\
svb > SVB Private Bank ANNUAL LOAN STATEMENT FOR 2022
MORTGAGE LOAN NO. 0000000002
STATEMENT DATE 12/31/22
TRAN DUE TR DB PRIN. INTEREST ESCROW OTHER ESCROW PRINCIPAL
10122 RP 1,600.00 .00 900,000.00
"""

# The page-1 registration the loader's signature guard matches on,
# fabricated like everything else here.
_REGISTRATION = "EXAMPLE HOLDER - Example Property"


# ============================================================
# parse_svbdep_statement_pdf — the escalation
# ============================================================

def _patch_ocr(monkeypatch, by_scale):
    """Stand in for the OCR seam, returning different text per raster
    scale — which is what a retry at a finer one really changes.

    Returns the list of scales it was called at, so a test can assert
    the extra pass was paid exactly once.
    """
    calls = []

    def fake(path, scale=ckpdf.OCR_SCALE, **kw):
        calls.append(scale)
        return by_scale[scale]

    monkeypatch.setattr(ckpdf, "extract_text_ocr", fake)
    return calls


def test_a_refused_section_is_retried_at_a_finer_raster(monkeypatch):
    """A refusal says the recognition missed something, not that the
    document is unreadable. The recognisers differ on the worst scans,
    so the escalation is what lets one parser serve both."""
    assert ps.OCR_RETRY_SCALE > ckpdf.OCR_SCALE, "no escalation to test"
    misread = _COMBINED_TEXT.replace("$-1,400.00 $1,600.00",
                                     "$-1,400.00 $1,500.00")
    _patch_ocr(monkeypatch, {ps.OCR_RETRY_SCALE: _COMBINED_TEXT,
                             ckpdf.OCR_SCALE: misread})
    out = ps.parse_svbdep_statement_pdf("/fake/path.pdf")
    assert [a.get("_error") for a in out["accounts"]] == [None, None]
    assert len(out["accounts"][0]["activity"]) == 4


def test_a_retry_that_reads_no_better_does_not_win(monkeypatch):
    # The default raster is the tuned one, so a finer pass replaces it
    # only when the arithmetic accepts more of the document.
    misread = _COMBINED_TEXT.replace("(-) Withdrawals $1,400.00",
                                     "(-) Withdrawals $1,400.09")
    worse = misread.replace("(-) Withdrawals $1,000.00",
                            "(-) Withdrawals $1,000.09")
    _patch_ocr(monkeypatch, {ps.OCR_RETRY_SCALE: worse,
                             ckpdf.OCR_SCALE: misread})
    out = ps.parse_svbdep_statement_pdf("/fake/path.pdf")
    # The first pass refused one section; the retry refused two, so the
    # first stands.
    assert sum(1 for a in out["accounts"] if a.get("_error")) == 1


def test_a_clean_read_is_not_retried(monkeypatch):
    calls = _patch_ocr(monkeypatch, {ckpdf.OCR_SCALE: _COMBINED_TEXT})
    ps.parse_svbdep_statement_pdf("/fake/path.pdf")
    assert calls == [ckpdf.OCR_SCALE], "a second OCR pass is not free"


def test_an_unread_title_is_retried_before_it_is_dropped(monkeypatch):
    # Every PDF reaching this parser is already known to be a scan from
    # this archive, so an unrecognised title says the recognition missed
    # it far more often than it says the document is foreign.
    # The ledger is broken in the same read: a page degraded enough to lose
    # its title is degraded elsewhere too, so this also pins that the finer
    # text CARRIES the parse rather than merely being fetched.
    lost = (_COMBINED_TEXT
            .replace("Combined Account Statement", "Cornbmed")
            .replace("$-1,400.00 $1,600.00", "$-1,400.00 $1,500.00"))
    calls = _patch_ocr(monkeypatch, {ckpdf.OCR_SCALE: lost,
                                     ps.OCR_RETRY_SCALE: _COMBINED_TEXT})
    out = ps.parse_svbdep_statement_pdf("/fake/path.pdf")
    assert out["family"] == ps.FAMILY_DEPOSIT
    assert [a.get("_error") for a in out["accounts"]] == [None, None]
    # The finer read carried the parse too, rather than being paid twice.
    assert calls == [ckpdf.OCR_SCALE, ps.OCR_RETRY_SCALE]


def test_a_statement_whose_sections_vanish_is_retried(monkeypatch):
    # The account headings are what the section split keys on, so losing
    # them yields a recognised statement with nothing in it — and every
    # arithmetic gate then passes vacuously. That must read as a refusal,
    # not as a clean document, or the whole statement goes missing while
    # being counted as loaded.
    lost = _COMBINED_TEXT.replace("Account Number:", "Acc0unt Numbcr")
    calls = _patch_ocr(monkeypatch, {ckpdf.OCR_SCALE: lost,
                                     ps.OCR_RETRY_SCALE: _COMBINED_TEXT})
    out = ps.parse_svbdep_statement_pdf("/fake/path.pdf")
    assert out["family"] == ps.FAMILY_DEPOSIT
    assert [a.get("_error") for a in out["accounts"]] == [None, None]
    assert calls == [ckpdf.OCR_SCALE, ps.OCR_RETRY_SCALE]


def test_a_statement_no_raster_sections_is_not_counted_clean(monkeypatch):
    lost = _COMBINED_TEXT.replace("Account Number:", "Acc0unt Numbcr")
    _patch_ocr(monkeypatch, {ckpdf.OCR_SCALE: lost, ps.OCR_RETRY_SCALE: lost})
    out = ps.parse_svbdep_statement_pdf("/fake/path.pdf")
    # Named as unreadable, in the shape the loader reports and declines to
    # cache — not handed back as an empty but clean document.
    assert out["_error"] == "no-account-sections"
    assert "accounts" not in out


def test_a_title_no_raster_reads_stays_unknown(monkeypatch):
    lost = _COMBINED_TEXT.replace("Combined Account Statement", "Cornbmed")
    _patch_ocr(monkeypatch, {ckpdf.OCR_SCALE: lost, ps.OCR_RETRY_SCALE: lost})
    out = ps.parse_svbdep_statement_pdf("/fake/path.pdf")
    assert out["family"] == ps.FAMILY_UNKNOWN
    assert out["accounts"] == []


def test_a_registration_lost_to_the_raster_is_retried(monkeypatch):
    # The guard is an exact substring match against OCR output, so one
    # lost glyph in the registration fails the whole document — and it
    # would be counted as a misfiled PDF rather than an OCR miss.
    lost = (_COMBINED_TEXT
            .replace(_REGISTRATION, "EXAMPLE H0LDER & ...")
            .replace("$-1,400.00 $1,600.00", "$-1,400.00 $1,500.00"))
    calls = _patch_ocr(monkeypatch, {ckpdf.OCR_SCALE: lost,
                                     ps.OCR_RETRY_SCALE: _COMBINED_TEXT})
    out = ps.parse_svbdep_statement_pdf(
        "/fake/path.pdf", expected_signatures=(_REGISTRATION,))
    assert "_error" not in out
    assert [a.get("_error") for a in out["accounts"]] == [None, None]
    assert calls == [ckpdf.OCR_SCALE, ps.OCR_RETRY_SCALE]


def test_a_registration_no_raster_reads_is_still_a_mismatch(monkeypatch):
    lost = _COMBINED_TEXT.replace(_REGISTRATION, "EXAMPLE H0LDER & ...")
    _patch_ocr(monkeypatch, {ckpdf.OCR_SCALE: lost, ps.OCR_RETRY_SCALE: lost})
    out = ps.parse_svbdep_statement_pdf(
        "/fake/path.pdf", expected_signatures=(_REGISTRATION,))
    assert out["_error"] == "signature-mismatch"


def test_a_mortgage_reaches_the_seam_signed_and_dated(monkeypatch):
    _patch_ocr(monkeypatch, {ckpdf.OCR_SCALE: _MORTGAGE_TEXT})
    out = ps.parse_svbdep_statement_pdf("/fake/path.pdf")
    assert out["family"] == ps.FAMILY_MORTGAGE
    # The statement carries no range of its own, so its date is both ends.
    assert (out["period_start"], out["period_end"]) == ("2022-01-14",
                                                        "2022-01-14")
    account, = out["accounts"]
    assert account.get("_error") is None
    # The account the loan is paid from already books the outflow.
    assert account["activity"] == []
    holding, = account["holdings"]
    assert holding["description"] == ps.LOAN_DESC
    # NEGATIVE at the seam: the adapter passes market_value through, so a
    # liability that arrives unsigned reads as an asset of the same size.
    assert holding["market_value"] == -900000.0
    assert holding["payment_breakdown"]["interest"] == {
        "paid_last_month": 1600.0, "paid_year_to_date": 4800.0}


def test_a_refused_mortgage_is_retried_at_a_finer_raster(monkeypatch):
    lost = _MORTGAGE_TEXT.replace("Outstanding Principal $900,000.00", "")
    _patch_ocr(monkeypatch, {ckpdf.OCR_SCALE: lost,
                             ps.OCR_RETRY_SCALE: _MORTGAGE_TEXT})
    out = ps.parse_svbdep_statement_pdf("/fake/path.pdf")
    account, = out["accounts"]
    assert account.get("_error") is None
    assert account["holdings"][0]["market_value"] == -900000.0


# ============================================================
# classify_ocr_text
# ============================================================

def test_classify_each_form_by_its_title():
    assert ps.classify_ocr_text(_COMBINED_TEXT) == ps.FAMILY_DEPOSIT
    assert ps.classify_ocr_text(_SINGLE_TEXT) == ps.FAMILY_DEPOSIT
    assert ps.classify_ocr_text(_MORTGAGE_TEXT) == ps.FAMILY_MORTGAGE
    assert ps.classify_ocr_text("something else entirely") == ps.FAMILY_UNKNOWN


def test_the_annual_form_is_told_from_a_monthly_one():
    # It names a loan too; only its own title separates them, so it is
    # recognised first rather than parsed as an unreadable monthly one.
    assert ps.classify_ocr_text(_ANNUAL_TEXT) == ps.FAMILY_ANNUAL_LOAN


# ============================================================
# Deposit statements
# ============================================================

def test_combined_statement_splits_into_one_section_per_account():
    accounts, start, end = ps.parse_deposit_statement(_COMBINED_TEXT)
    assert [a.account_external_id for a in accounts] == ["0000000000",
                                                         "0000000001"]
    assert (start, end) == ("2023-01-01", "2023-01-31")
    assert all(a.error is None for a in accounts)


def test_ending_balance_is_what_the_statement_states():
    checking, savings = ps.parse_deposit_statement(_COMBINED_TEXT)[0]
    assert checking.ending_balance == 1600.0
    assert savings.ending_balance == 4020.0


def test_ledger_rows_are_signed_by_the_column_they_land_in():
    checking = ps.parse_deposit_statement(_COMBINED_TEXT)[0][0]
    assert [(r.date, r.amount) for r in checking.rows] == [
        ("2023-01-04", 2000.0),
        ("2023-01-09", -1400.0),
        ("2023-01-31", -10.0),
        ("2023-01-31", 10.0),
    ]


def test_ledger_rows_are_bucketed_by_the_wording_the_summary_uses():
    checking, savings = ps.parse_deposit_statement(_COMBINED_TEXT)[0]
    assert [r.bucket for r in checking.rows] == [
        "deposits", "withdrawals", "charges", "interest"]
    assert [r.bucket for r in savings.rows] == ["withdrawals", "interest"]


def test_a_ledger_row_reaches_the_seam_as_its_own_kind(monkeypatch):
    # A deposit account's whole economic return is its interest, so an
    # interest credit booked as capital in offsets the balance growth
    # exactly and the measured return collapses to nothing.
    _patch_ocr(monkeypatch, {ckpdf.OCR_SCALE: _COMBINED_TEXT})
    checking = ps.parse_svbdep_statement_pdf("/fake/path.pdf")["accounts"][0]
    assert [(r["verb"], r["amount"]) for r in checking["activity"]] == [
        ("DEPOSIT", 2000.0),
        ("WITHDRAWAL", -1400.0),
        ("FEE PAID", -10.0),
        ("INTEREST", 10.0),
    ]


def test_every_ledger_verb_is_one_the_loader_maps():
    # A verb the loader's map does not hold is dropped unbooked, so an
    # invented one would delete the very rows this split separates.
    import load

    assert set(ps._VERB_BY_BUCKET.values()) <= set(load._KIND_BY_VERB)


def test_a_split_the_summary_does_not_state_is_refused():
    # Wording that drifted puts a row in the wrong bucket. Falling back
    # to a plain deposit would book the interest as capital on exactly
    # the statements where the reading went wrong, so the section is
    # refused instead.
    text = _COMBINED_TEXT.replace("01-31 Interest Credited Deposit",
                                  "01-31 Earnings Credited Deposit")
    checking = ps.parse_deposit_statement(text)[0][0]
    assert checking.error is not None
    assert "ledger deposits total 2010.00" in checking.error


def test_a_charge_the_ledger_stops_naming_is_refused():
    text = _COMBINED_TEXT.replace("01-31 Service Charge ",
                                  "01-31 Account Upkeep ")
    checking = ps.parse_deposit_statement(text)[0][0]
    assert checking.error is not None
    assert "ledger withdrawals total 1410.00" in checking.error


def test_a_wrapped_description_is_not_a_row():
    checking = ps.parse_deposit_statement(_COMBINED_TEXT)[0][0]
    assert not any("ID:" in r.description for r in checking.rows)


def test_single_account_layout_survives_the_ocr_losses():
    # A lost "(-)" paren, a colon for a date separator, and a margin
    # fragment on a continuation header — each would drop a figure, a
    # row, or a whole page if the patterns were strict about them.
    accounts, _start, _end = ps.parse_deposit_statement(_SINGLE_TEXT)
    assert len(accounts) == 1
    assert accounts[0].error is None
    assert accounts[0].withdrawals == 700.0
    assert [r.date for r in accounts[0].rows] == [
        "2021-09-02", "2021-09-07", "2021-09-30"]


# Two tokens of page-edge furniture ahead of a ledger line, as the
# recogniser reads them onto a row near the page's edge.
_FURNITURE = "- 4 "
_SAVINGS_INTEREST_ROW = "01-31 Interest Credited Deposit $20.00 $4,020.00"


def test_an_interest_row_behind_two_tokens_of_furniture_is_read():
    # Lost, the row leaves the ledger short of the stated ending balance
    # by its own amount and the whole section is refused.
    text = _COMBINED_TEXT.replace(_SAVINGS_INTEREST_ROW,
                                  _FURNITURE + _SAVINGS_INTEREST_ROW)
    savings = ps.parse_deposit_statement(text)[0][1]
    assert savings.error is None
    assert [(r.date, r.amount, r.bucket) for r in savings.rows] == [
        ("2023-01-15", -1000.0, "withdrawals"),
        ("2023-01-31", 20.0, "interest")]
    assert savings.rows[-1].description == "Interest Credited Deposit"


def test_every_ledger_anchor_reads_through_two_tokens_of_furniture():
    text = _COMBINED_TEXT
    for line in ("(+) Deposits $2,000.00",
                 "(+) Interest Paid $20.00 Annual Percentage Yield",
                 "(-) Withdrawals $1,400.00",
                 "(-) Service Charges $10.00",
                 "TRANSACTION DETAIL: Savings Account",
                 "ACCOUNT SUMMARY",
                 "CHECKS OUTSTANDING"):
        text = text.replace(line, _FURNITURE + line)
    checking, savings = ps.parse_deposit_statement(text)[0]
    assert (checking.error, savings.error) == (None, None)
    assert (checking.deposits, checking.withdrawals, checking.charges,
            savings.interest) == (2000.0, 1400.0, 10.0, 20.0)
    assert len(checking.rows) == 4 and len(savings.rows) == 2


def test_a_closed_ledger_stays_closed_behind_furniture():
    # A worksheet line past the ledger's end has a row's shape; read as a
    # row it would break the running balance.
    text = _COMBINED_TEXT.replace(
        "CHECKS OUTSTANDING\n",
        _FURNITURE + "CHECKS OUTSTANDING\n"
        "01-20 EXAMPLE CHECK OUTSTANDING $-5.00 $1,595.00\n")
    checking = ps.parse_deposit_statement(text)[0][0]
    assert checking.error is None
    assert len(checking.rows) == 4


def test_a_row_opening_on_a_dated_description_keeps_its_own_date():
    m = ps._LEDGER_ROW_RE.match("01-31 02-15 EXAMPLE TRANSFER $-5.00 $5.00")
    assert m and m["date"] == "01-31"


def test_prose_is_not_a_row_however_it_ends():
    # Three words ahead of a date are a sentence, not furniture, even when
    # the line ends in a figure and a balance.
    prose = "Rates changed on 01-31 Interest Credited Deposit $20.00 $4,020.00"
    assert ps._LEDGER_ROW_RE.match(prose) is None
    for prefix in ("", "4 ", "- 4 "):
        m = ps._LEDGER_ROW_RE.match(prefix + _SAVINGS_INTEREST_ROW)
        assert m and m["date"] == "01-31", prefix
    # Inside a ledger it is skipped like any other text, so the section
    # still reads exactly its own rows.
    text = _COMBINED_TEXT.replace(_SAVINGS_INTEREST_ROW,
                                  _SAVINGS_INTEREST_ROW + "\n" + prose)
    savings = ps.parse_deposit_statement(text)[0][1]
    assert savings.error is None
    assert len(savings.rows) == 2


def test_prose_is_not_a_summary_figure():
    line = "Your rate on (+) Interest Paid $20.00"
    assert all(pattern.match(line) is None
               for _key, pattern in ps._SUMMARY_LINES)


def test_a_summary_that_does_not_close_is_refused():
    text = _COMBINED_TEXT.replace("(-) Withdrawals $1,400.00",
                                  "(-) Withdrawals $1,400.09")
    checking = ps.parse_deposit_statement(text)[0][0]
    assert checking.error is not None
    assert "balance summary does not close" in checking.error


def test_a_broken_running_balance_is_refused():
    # A misread digit in one row's balance. Nothing downstream of it can
    # be trusted, so the section is refused rather than truncated.
    text = _COMBINED_TEXT.replace("$-1,400.00 $1,600.00",
                                  "$-1,400.00 $1,500.00")
    checking = ps.parse_deposit_statement(text)[0][0]
    assert checking.error is not None
    assert "running balance breaks" in checking.error


def test_a_row_lost_off_the_end_is_refused():
    # A dropped LAST row breaks no chain, so only the stated ending
    # balance catches it.
    text = _COMBINED_TEXT.replace(
        "01-31 Interest Credited Deposit $10.00 $1,600.00\n", "")
    checking = ps.parse_deposit_statement(text)[0][0]
    assert checking.error is not None
    assert "ledger ends at" in checking.error


def test_an_unreadable_period_refuses_the_ledger():
    # A flat month: the summary closes and the running balance never
    # leaves the beginning one, so both balance checks pass on a ledger
    # that was never read. Only refusing on the header itself catches
    # it — otherwise real rows vanish with no signal and the finer
    # raster is never tried.
    text = (_SINGLE_TEXT
            .replace("Ending Balance as of 09-30-21 $700.00",
                     "Ending Balance as of 09-30-21 $500.00")
            .replace("(- Withdrawals $700.00", "(- Withdrawals $900.00")
            .replace("Statement from 09-01-21", "Statement fromn 09-01-21"))
    accounts, start, end = ps.parse_deposit_statement(text)
    assert (start, end) == (None, None)
    assert accounts[0].error == "statement period unreadable"
    assert accounts[0].rows == []


def test_a_separator_slip_does_not_cost_the_period():
    # The same hyphen-for-colon loss the ledger rows already tolerate.
    text = _COMBINED_TEXT.replace("Statement from 01-01-23 to 01-31-23",
                                  "Statement from 01:01-23 to 01-31-23")
    accounts, start, end = ps.parse_deposit_statement(text)
    assert (start, end) == ("2023-01-01", "2023-01-31")
    assert all(a.error is None for a in accounts)


def test_a_separator_slip_does_not_cost_the_mortgage_date():
    # The loan statement dates its header with slashes, but the recogniser
    # is as free to lose one there as anywhere else — and this date is the
    # one every row on the statement is stamped at.
    for sep in ("/", "-", ".", ":"):
        text = _MORTGAGE_TEXT.replace(
            "Statement Date: 01/14/22", f"Statement Date: 01{sep}14{sep}22")
        loan = ps.parse_mortgage_statement(text)
        assert loan.statement_date == "2022-01-14", sep
        assert loan.error is None, sep


def test_a_separator_slip_does_not_cost_the_balance_summary():
    # Those dates are matched but never read, so a slip in one used to
    # cost the whole summary — and with it the section.
    text = _COMBINED_TEXT.replace("Beginning Balance as of 01-01-23 $1,000.00",
                                  "Beginning Balance as of 01.01.23 $1,000.00")
    checking = ps.parse_deposit_statement(text)[0][0]
    assert checking.error is None
    assert checking.beginning_balance == 1000.0


def test_an_unreadable_summary_refuses_the_section():
    text = _COMBINED_TEXT.replace(
        "Beginning Balance as of 01-01-23 $1,000.00 "
        "Ending Balance as of 01-31-23 $1,600.00", "")
    checking = ps.parse_deposit_statement(text)[0][0]
    assert checking.error == "balance summary unreadable"
    assert checking.rows == []


# ============================================================
# Mortgage statements
# ============================================================

def test_mortgage_reads_its_balance_date_and_split():
    loan = ps.parse_mortgage_statement(_MORTGAGE_TEXT)
    assert loan.error is None
    assert loan.account_external_id == "0000000002"
    assert loan.statement_date == "2022-01-14"
    # POSITIVE here: signing a liability is the loader's decision, not
    # something the statement says.
    assert loan.outstanding_principal == 900000.0
    assert loan.breakdown["interest"] == {"paid_last_month": 1600.0,
                                          "paid_year_to_date": 4800.0}


def test_mortgage_missing_figures_are_named_not_guessed():
    text = _MORTGAGE_TEXT.replace("Outstanding Principal $900,000.00", "")
    loan = ps.parse_mortgage_statement(text)
    assert loan.error is not None
    assert "outstanding principal" in loan.error
