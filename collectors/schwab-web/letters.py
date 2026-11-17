"""Schwab's 3rd-Party-Distribution letters, read into transfer rows.

:func:`parse` reads the text PDFium extracts from a letter
(``collectorkit.extract.pdfium``).
"""

from __future__ import annotations

import logging
import re

from collectorkit import statement_period
from collectorkit.words import Extracted

log = logging.getLogger("schwab-web.letters")


# ============================================================
# 3rd-Party-Distribution letters
# ============================================================
#
# Schwab files a "3rd Party Distribution" confirmation letter (under
# the Letters document type) for every movement of money or securities
# OUT of an account to a third party. These are the ONLY record of
# securities transferred out — gifts to people / DAFs, transfers to
# other custodians or accounts. The brokerage statements show such a
# move only as an unexplained position drop; the letter itemises it.
#
# Three known layout families, all OUT, all single-confirmation:
#   1. Wire transfer(s) — cash leaving by wire. Recipient rendered as
#      "To the account of <NAME> at <BANK>" + a cash amount.
#   2. Transfer(s) to Schwab accounts of third parties — CASH to
#      another Schwab account: "Account name: <NAME>" + a cash amount.
#   3. Transfer(s) to Schwab accounts of third parties — SECURITIES:
#      same recipient block plus a "Security(ies) transferred:" table
#      (Symbol / Quantity / Market Value).
#
# Text comes from the same pypdfium2 extractor the statement parser
# uses (one row per line, clean enough that simple line anchors work).

_DIST_BODY_DATE_RE = re.compile(
    rf"\b({'|'.join(statement_period.MONTHS)})\s+(\d{{1,2}}),\s+(\d{{4}})\b"
)
_DIST_FILENAME_DATE_RE = re.compile(r"_(\d{4})-(\d{2})-(\d{2})_")
_DIST_WIRE_RE = re.compile(r"\bWire transfer", re.I)
_DIST_SCHWAB_3P_RE = re.compile(
    r"Transfer\(s\) to Schwab accounts of third parties", re.I)
_DIST_CASH_AMT_RE = re.compile(
    r"Cash transfer amount requested:\s*\$([\d,]+\.\d{2})", re.I)
_DIST_RECIP_SUFFIX_RE = re.compile(
    r"(?:To account ending in|Account ending in):\s*(\d{3,5})", re.I)
_DIST_WIRE_RECIP_RE = re.compile(
    r"^To the account of\s+(.*\S)\s+at\s+(.*\S)\s*$", re.I)
_DIST_ACCT_NAME_RE = re.compile(r"^Account name:\s*(.*\S)\s*$", re.I)
# A securities row: SYMBOL  QUANTITY  $MARKET_VALUE (pypdfium2 renders
# the three columns space-separated on one line).
_DIST_SEC_ROW_RE = re.compile(
    r"^([A-Z][A-Z0-9./]{0,9})\s+([\d,]+\.\d+)\s+\$([\d,]+\.\d+)\s*$")
# Lines that close the multi-line "Account name:" continuation block.
_DIST_NAME_STOP_RE = re.compile(
    r"^(Security\(ies\)|Cash transfer|Total|Thank you|Please|Symbol)\b", re.I)


def _distribution_date(lines: list[str], filename: str | None) -> str | None:
    """Transfer date as ISO. Prefer the body letter date ("Month D,
    YYYY"); fall back to the YYYY-MM-DD embedded in the filename."""
    for ln in lines:
        m = _DIST_BODY_DATE_RE.search(ln)
        if m:
            mon, d, y = m.group(1), int(m.group(2)), int(m.group(3))
            return f"{y:04d}-{statement_period.MONTH_NUMS[mon]:02d}-{d:02d}"
    if filename:
        m = _DIST_FILENAME_DATE_RE.search(filename)
        if m:
            return f"{m.group(1)}-{m.group(2)}-{m.group(3)}"
    return None


def _distribution_counterparty(lines: list[str], method: str
                               ) -> tuple[str | None, str | None, str | None]:
    """Return (counterparty, bank, recipient_account_suffix).

    Wire letters render the recipient as "To the account of <NAME> at
    <BANK>"; Schwab-to-Schwab letters render "Account name: <NAME>"
    (which can wrap across lines)."""
    counterparty: str | None = None
    bank: str | None = None
    suffix: str | None = None

    for ln in lines:
        m = _DIST_RECIP_SUFFIX_RE.search(ln)
        if m and suffix is None:
            suffix = m.group(1)

    if method == "wire":
        for ln in lines:
            m = _DIST_WIRE_RECIP_RE.match(ln)
            if m:
                counterparty = m.group(1).strip()
                bank = re.sub(r"\s+and$", "", m.group(2).strip()) or None
                break
    else:
        for i, ln in enumerate(lines):
            m = _DIST_ACCT_NAME_RE.match(ln)
            if m:
                parts = [m.group(1).strip()]
                for cont in lines[i + 1:]:
                    if _DIST_NAME_STOP_RE.match(cont) or ":" in cont:
                        break
                    parts.append(cont.strip())
                counterparty = " ".join(p for p in parts if p) or None
                break
    return counterparty, bank, suffix


def _distribution_securities(lines: list[str]) -> list[tuple[str, float, float]]:
    """Parse the "Security(ies) transferred:" table → list of
    (symbol, quantity, market_value). Empty when there is no table
    (cash-only letters)."""
    out: list[tuple[str, float, float]] = []
    in_table = False
    for ln in lines:
        if ln.lower().startswith("security(ies) transferred"):
            in_table = True
            continue
        if not in_table:
            continue
        if ln.lower().startswith("total market value"):
            break
        m = _DIST_SEC_ROW_RE.match(ln)
        if m:
            out.append((
                m.group(1),
                float(m.group(2).replace(",", "")),
                float(m.group(3).replace(",", "")),
            ))
    return out


def parse_distribution_text(text: str, filename: str | None = None) -> list[dict]:
    """Parse the extracted text of a 3rd-Party-Distribution letter into
    normalised silver rows (source='third_party_distribution').

    One row per security line for a securities transfer; one row for a
    cash transfer. Rows carry the counterparty, direction, method, and
    (for securities) symbol / quantity / market value. Returns [] for
    an unrecognised layout (logged) so an unseen format drops
    cleanly instead of corrupting the load."""
    lines = [ln.strip() for ln in text.splitlines() if ln.strip()]
    joined = "\n".join(lines)

    date_iso = _distribution_date(lines, filename)
    # A distribution letter is an outbound move ("moved money out of
    # the account"); an inbound variant is detected defensively.
    direction = "in" if re.search(r"\b(received into|moved money in|"
                                  r"deposited into)\b", joined, re.I) else "out"

    if _DIST_WIRE_RE.search(joined):
        method = "wire"
    elif _DIST_SCHWAB_3P_RE.search(joined):
        method = "schwab_third_party"
    else:
        method = "unknown"

    counterparty, bank, recip_suffix = _distribution_counterparty(lines, method)
    sec_rows = _distribution_securities(lines)
    cash = None
    m = _DIST_CASH_AMT_RE.search(joined)
    if m:
        cash = float(m.group(1).replace(",", ""))

    kind = "Transfer Out" if direction == "out" else "Transfer In"
    base = {
        "date": date_iso,
        "direction": direction,
        "method": method,
        "counterparty": counterparty,
        "counterparty_bank": bank,
        "counterparty_account_suffix": recip_suffix,
        "kind": kind,
    }

    rows: list[dict] = []
    if sec_rows:
        for sym, qty, mv in sec_rows:
            rows.append({
                **base,
                "amount": mv,            # market value = flow magnitude + id
                "symbol": sym,
                "instrument_key": sym,
                "description": f"Securities transfer {direction} "
                               f"to {counterparty or 'third party'}",
                "transfer_kind": "securities",
                "security_symbol": sym,
                "quantity": qty,
                "market_value": mv,
            })
    elif cash is not None:
        rows.append({
            **base,
            "amount": cash,
            "symbol": None,
            "instrument_key": None,
            "description": f"Cash transfer {direction} "
                           f"to {counterparty or 'third party'}",
            "transfer_kind": "cash",
            "cash_amount": cash,
        })
    else:
        log.warning("3rd-party-distribution %s: no securities table or cash "
                    "amount recognised (method=%s) — emitting no rows",
                    filename or "<text>", method)
    return rows


def parse(extracted: Extracted, filename: str | None = None) -> list[dict]:
    """An extracted 3rd-Party-Distribution letter as normalised silver
    rows (parse_distribution_text). `filename` is the letter's file
    name, whose date stands in for a body date that does not read."""
    return parse_distribution_text(extracted.text(), filename=filename)
