"""A Credit or Debit Advice: the payment it states."""
from __future__ import annotations

import json
import re
from datetime import date

from collectorkit.words import Extracted

from pdf_common import (
    IBAN_RE, document_text, long_date, to_float, to_unix, undouble_bold,
)


# UBS issues a one-page advice for every payment it books by hand or
# by order: a Credit Advice to the account the money lands on and a
# Debit Advice to the account it leaves. Both sides of one transfer
# are issued as two separate documents, each addressed to its own
# account, and — this is what makes them worth parsing — both print
# the SAME "TRX-No.", which is the "Transaction no." the CSV export
# uses as its transaction id.
#
# That matters because UBS does not issue the CSV export for every
# account kind — a managed mandate's cash sub-account has none — and
# the MT940 feed reaches back only as far as its own go-live. For a
# transfer into such an account, silver therefore holds the debit
# leaving the payment account and nothing receiving it, and a
# consumer that pairs legs to recognise an internal move sees capital
# leaving the household that never left. The advice is the only
# record of the missing leg, and it names the leg it belongs to with
# the same id its twin already carries.
#
# Layout (pdfplumber extract_text, one line per printed line):
#
#     Cash Account for investment solutions CHF
#     IBAN CH00 0000 0000 0000 0000 0
#     Account no.      000-000000.00A
#     Credit Advice
#     Produced on 2 January 2020
#     Information/References
#     TRX-No.          0000 000 XX 0000000   ABC/ABC
#     Bookkeeping entry date            1 January 2020
#     Description
#     By order of                  <- "Order of <date>" / "Beneficiary"
#     <holder name and address>       on a debit advice
#     Details of payment
#     <free-text purpose>
#     Currency            Amount
#     Total amount        CHF          0 000.00
#     Val.                         01.01.2020
#
# Older advices set the address block and the account block in two
# columns that extract_text merges into one printed line ("Herr IBAN
# CH00 ...") — which is what the IBAN scan below allows for.
#
# Not every document UBS labels an advice is a payment advice: other
# bank notices, such as an interest settlement or a fee bill, carry
# the same label and a different layout altogether. They print
# no TRX-No., so the absence of one is what rejects them — a
# document that does not state the id its row would be keyed by is
# not a movement this parser can place.

# "Credit Advice" / "Debit Advice" / "Debit advice" — the headline is the
# only place the document says which way the money went, so the match has
# to survive the casing UBS varies between advices. The optional tail is
# the production date, which the header sets in a column of its own: whether
# the two arrive as one printed line depends on how the page's columns
# resolve, and a direction lost to a merged line would drop the movement
# silently, which is the one failure this parser must not have.
_ADVICE_HEADLINE_RE = re.compile(
    r"^(?P<dir>Credit|Debit)\s+[Aa]dvice(?:\s+Produced on\b.*)?$")

# "TRX-No.  0000 000 XX 0000000  ABC/ABC" — the id printed in
# space-separated groups, optionally closed by the booking desk's
# initials. The groups are joined WITHOUT the spaces because that is
# how the CSV export spells the same number in its "Transaction no."
# column, and the two spellings have to be one string for the silver
# primary key to bring the two legs of a transfer together.
_ADVICE_TRX_LINE_RE = re.compile(r"^TRX-No\.\s+(?P<rest>\S.*?)\s*$")
_ADVICE_TRX_GROUP_RE = re.compile(r"^[A-Z0-9]+$")
_ADVICE_TRX_JOINED_RE = re.compile(r"^[A-Z0-9]{12,24}$")

# "<D> <Month> <YYYY>", with the month's casing left open (`_MONTHS`).
_ADVICE_BOOKED_RE = re.compile(
    r"^Bookkeeping entry date\s+"
    r"(?P<day>\d{1,2})\s+(?P<month>[A-Za-z]+)\s+(?P<year>\d{4})\s*$"
)
_ADVICE_VALUE_DATE_RE = re.compile(
    r"^Val\.\s+(?P<d>\d{2})\.(?P<m>\d{2})\.(?P<y>\d{4})\s*$"
)
_ADVICE_TOTAL_RE = re.compile(
    r"^Total amount\s+(?P<ccy>[A-Z]{3})\s+(?P<v>[\d\s'’]+\.\d{2})\s*$"
)

_ADVICE_DETAILS_LABEL = "Details of payment"
# The details block runs until the amount table starts. "Currency
# Amount" is that table's column header; the other two are the first
# rows of the table itself, in case a document omits the header.
_ADVICE_DETAILS_END = ("Currency", "Total amount", "Val.")


def _advice_trx_no(rest: str) -> str | None:
    """The TRX-No. groups joined into the CSV export's spelling, or
    None when the line does not hold one.

    The trailing token is the initials of the desk that booked the
    payment ("ABC/ABC", "ABC/XYZ") and is not part of the number; the
    slash is what marks it, and many advices carry none at all. Every
    token before it has to be a bare alphanumeric group — anything
    else means this is not the transaction-number line, and a wrong id
    would key a row onto a transfer it has nothing to do with.
    """
    groups: list[str] = []
    for token in rest.split():
        if "/" in token:
            break
        if not _ADVICE_TRX_GROUP_RE.match(token):
            return None
        groups.append(token)
    joined = "".join(groups)
    return joined if _ADVICE_TRX_JOINED_RE.match(joined) else None


def parse(extracted: Extracted, doc_token: str) -> list[dict]:
    """Walk a Credit/Debit Advice and emit at most ONE movement row for
    the account the advice is addressed to."""
    return parse_payment_advice_text(document_text(extracted, 2), doc_token)


def parse_payment_advice_text(text: str, doc_token: str) -> list[dict]:
    """Pure-text variant of `parse`, for fixture-based
    tests. Returns [] for any document that does not state every fact
    a movement row needs — the direction, the transaction number, the
    account, the booked date and the amount — so the advice labels
    that are really a mortgage settlement or a safe-box rental bill
    fall out here rather than landing as a row with holes in it."""
    direction = None
    trx_no = None
    trx_printed = None
    booked = None
    value_dmy = None
    currency = None
    amount = None
    iban = None
    details: list[str] = []
    in_details = False

    for raw_line in text.splitlines():
        ln = undouble_bold(raw_line.strip())
        if not ln:
            continue
        if iban is None:
            # Anywhere in the line, not anchored to its start: the
            # older layout sets the addressee column and the account
            # column on one printed line. An advice names one IBAN, the
            # account it is addressed to.
            m = IBAN_RE.search(ln)
            if m:
                iban = re.sub(r"\s+", "", m.group(0)).upper()
        if in_details:
            if ln.startswith(_ADVICE_DETAILS_END):
                in_details = False
            else:
                details.append(ln)
                continue
        if ln == _ADVICE_DETAILS_LABEL:
            in_details = True
            continue
        if direction is None:
            m = _ADVICE_HEADLINE_RE.match(ln)
            if m:
                direction = m["dir"].lower()
                continue
        if trx_no is None:
            m = _ADVICE_TRX_LINE_RE.match(ln)
            if m:
                trx_no = _advice_trx_no(m["rest"])
                if trx_no:
                    trx_printed = m["rest"]
                continue
        if booked is None:
            m = _ADVICE_BOOKED_RE.match(ln)
            if m:
                booked = long_date(m)
                continue
        if amount is None:
            m = _ADVICE_TOTAL_RE.match(ln)
            if m:
                currency = m["ccy"]
                amount = to_float(m["v"])
                continue
        if value_dmy is None:
            m = _ADVICE_VALUE_DATE_RE.match(ln)
            if m:
                value_dmy = date(int(m["y"]), int(m["m"]), int(m["d"]))
                continue

    if (direction is None or not trx_no or booked is None or iban is None
            or amount is None or not currency):
        return []

    booking_date = to_unix(booked)
    # The advice prints its amount as an unsigned total and says which
    # way it moved in the headline, so the figure goes in the column
    # the direction names — the same convention the Account-Statement
    # walker keeps, where the statement prints a debit as a positive
    # figure in its debit column and the column, not the sign, carries
    # the direction (DESIGN.md §3.6).
    debit = abs(amount) if direction == "debit" else None
    credit = abs(amount) if direction == "credit" else None
    return [{
        # UBS's own Transaction no., not a collector-minted id: the
        # advice states the number its twin leg already carries in the
        # CSV export, and storing it verbatim is what puts the two
        # legs under one id in silver's compound primary key.
        "transaction_external_id": trx_no,
        "booking_date": booking_date,
        "value_date": to_unix(value_dmy) if value_dmy else booking_date,
        "account_external_id": iban,
        "currency_iso": currency,
        "amount_debit": debit,
        "amount_credit": credit,
        # The purpose line, which is what the statement era puts here
        # too (its first continuation line). Deliberately NOT the "By
        # order of" / "Beneficiary" block: on an advice for a move
        # between the holder's own accounts that block is the holder,
        # and the purpose is the only text that says anything about
        # the movement.
        "counterparty": details[0] if details else None,
        # An advice prints no booking type and no counter IBAN. Both
        # stay empty rather than being invented: gold reads the
        # booking type to promote a row to external capital, and a
        # type this document never printed would do exactly that.
        "description_kind": None,
        "counter_account": None,
        "source_doc_token": doc_token,
        "payload": json.dumps({
            "booking_type": None,
            "counter_account": None,
            "continuation": details,
            # The document the row was read from. The gold adapter asks of
            # it whether a row came from the PDF archive or from the
            # MT940/CSV feed, and an advice is on the archive's side.
            "source": "payment_advice_pdf",
            # The marker the loader's document pass deletes on, since an
            # advice row is keyed by UBS's transaction number and so
            # carries no id prefix to recognise it by (documents_load.py
            # `_DOCUMENT_PASS_DELETES`).
            "document": "payment_advice_pdf",
            "advice_direction": direction,
            # The id as the advice PRINTS it. The normalisation from
            # this to the export's spelling is the whole hinge of the
            # pairing, so the printed form travels with the row and a
            # bad join can be read back out of silver.
            "trx_no_printed": trx_printed,
        }, ensure_ascii=False),
    }]
