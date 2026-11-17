"""A mortgage's Maturity notice: its outstanding principal."""
from __future__ import annotations

import json
import re
from datetime import date

from collectorkit.words import Extracted

from pdf_common import document_text, to_float, to_unix, undouble_bold


# UBS issues one "Maturity notice" PDF per mortgage per fixed-rate
# / SARON interest-roll period (typically quarterly). The PDF lays
# the salient fields out as labelled lines:
#
#     UBS SARON Mortgage CHF
#     Account no. <branch>-<base>.<MMM> <suffix>
#     Category <collateral address>
#     ...
#     As at DD.MM.YYYY
#     ...
#     Current debt capital N NNN NNN.NN
#
# The parser extracts the outstanding principal as the historical
# balance at the "As at" date and surfaces the product / currency for
# the gold adapter to roll up into a mortgage Position.
#
# The "Account no." in the maturity notice is the compact zero-
# stripped form (`BBB-AAAAAA.MMM NNNN`); the live positions.csv
# table normalises the same account to the padded 4-digit branch /
# 8-digit base form (`BBBB AAAAAAAA.MMM NNNN`).
# The parser re-pads it, so the resulting account_external_id matches
# `mortgages.account_external_id` exactly — gold's instruments/
# accounts upsert is keyed by ID, so cross-snapshot identity has
# to be byte-equal.

# Product line of the form "UBS SARON Mortgage CHF" or
# "UBS Fixed-Rate Mortgage CHF". `name` swallows everything between
# 'UBS ' and the trailing ' CCY'; the trailing 3-letter token is
# the currency.
_PRODUCT_LINE_RE = re.compile(
    r"^UBS\s+(?P<name>.+?)\s+Mortgage\s+(?P<ccy>[A-Z]{3})\s*$"
)

# Account no. line. Branch may be 3 or 4 digits, base 6-8 digits.
# `mmm` is the mortgage-type code; `suffix` is the trailing sub-
# account number.
_ACCT_NO_LINE_RE = re.compile(
    r"^Account\s+no\.\s+"
    r"(?P<branch>\d{3,4})-(?P<base>\d{6,8})"
    r"\.(?P<mmm>[A-Z0-9]{2,4})\s+(?P<suffix>\d+)\s*$"
)

# Collateral line. UBS prefixes the property address with the
# literal label `Category` in the side-column of the PDF.
_COLLATERAL_LINE_RE = re.compile(r"^Category\s+(?P<addr>.+\S)\s*$")

# Balance date. The maturity notice's primary date marker is
# `As at DD.MM.YYYY`.
_AS_AT_LINE_RE = re.compile(
    r"^As\s+at\s+(?P<d>\d{2})\.(?P<m>\d{2})\.(?P<y>\d{4})\s*$"
)

# Outstanding principal. A loose pattern because pdfplumber
# occasionally collapses multiple spaces between label and value.
_CURRENT_DEBT_RE = re.compile(
    r"^Current\s+debt\s+capital\s+(?P<v>[\d\s']+\.\d{2})\s*$"
)


def parse(extracted: Extracted, doc_token: str) -> list[dict]:
    """Walk a 'Maturity notice' and emit ONE row capturing the
    mortgage's outstanding principal at the notice's `As at` date."""
    return parse_maturity_notice_text(document_text(extracted, 2), doc_token)


def parse_maturity_notice_text(text: str, doc_token: str) -> list[dict]:
    """Pure-text variant of `parse` for fixture-based
    tests. Returns [] when the PDF doesn't actually look like a
    mortgage maturity notice (so non-mortgage 'Maturity notice'
    doc_types — bonds, time deposits, etc. — are skipped gracefully).
    """
    product_name = None
    currency = None
    account_external_id = None
    collateral = None
    as_of = None
    outstanding = None

    for raw_line in text.splitlines():
        ln = undouble_bold(raw_line.strip())
        if not ln:
            continue
        if product_name is None:
            m = _PRODUCT_LINE_RE.match(ln)
            if m:
                product_name = m["name"].strip()
                currency = m["ccy"]
                continue
        if account_external_id is None:
            m = _ACCT_NO_LINE_RE.match(ln)
            if m:
                branch = m["branch"].zfill(4)
                base = m["base"].zfill(8)
                account_external_id = (
                    f"{branch} {base}.{m['mmm']} {m['suffix']}"
                )
                continue
        if collateral is None:
            m = _COLLATERAL_LINE_RE.match(ln)
            if m:
                collateral = m["addr"]
                continue
        if as_of is None:
            m = _AS_AT_LINE_RE.match(ln)
            if m:
                as_of = date(int(m["y"]), int(m["m"]), int(m["d"]))
                continue
        if outstanding is None:
            m = _CURRENT_DEBT_RE.match(ln)
            if m:
                outstanding = to_float(m["v"])
                continue

    if (product_name is None or account_external_id is None or
            as_of is None or outstanding is None or currency is None):
        return []

    return [{
        "as_of_date": to_unix(as_of),
        "account_external_id": account_external_id,
        "currency_iso": currency,
        # The principal as the notice prints it: a positive amount. The
        # gold adapter gives the liability its sign, and the loader
        # reads the rate type off the product name.
        "outstanding_balance": outstanding,
        "product_name": f"UBS {product_name} Mortgage",
        "collateral_description": collateral,
        "source_doc_token": doc_token,
        "payload": json.dumps({
            "as_of": as_of.isoformat(),
            "product_name": product_name,
        }),
    }]
