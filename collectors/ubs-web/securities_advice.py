"""Securities advices: a capital call (a Private Market Letter) and a
contract note, each the price a holding was bought at."""
from __future__ import annotations

import json
import re

from collectorkit.words import Extracted

from pdf_common import (
    ISIN_SHAPE, LONG_DATE, dmy_to_unix, document_text, long_date_to_unix,
    to_float,
)


# Two document kinds state the price a holding was bought at where the
# statement of assets and the MT535 feed state none. Each yields at most
# one `advices` row, every figure as printed.
#
# A capital call reaches the archive as a "Private Market Letter": a UBS
# cover page titled "Capital Call", then the fund administrator's
# notice. Quarterly reports and other letters share that label, so the
# cover title is what selects a call. The notice is the administrator's
# own prose with no fixed layout, so each figure is read from the
# sentence that states it (synthetic example, whitespace collapsed):
#
#   Example Fund ("EF") - Capital Call No. 23 Investing in Example LP
#   ISIN - XX0000000000
#   … will now make Capital Call No. 23 of USD 12,345.67, which
#   represents 7.25% of your Net Commitment of USD 170,285.10. …
#   … makes the amount of USD 12,345.67 available in your account by
#   value date 15 March 2030. …
#   Total 1,234,567.00 12,345.67
#
# The breakdown's "Total" line ends in the investor's column: the called
# amount plus anything charged on top of it, such as equalisation
# interest on a late closing. That is the cash the call takes.
#
# A contract note confirms a purchase outside the exchange (synthetic):
#
#   Contract note
#   Produced on 30 March 2030
#   New issue purchase
#   Trade date: 15.03.2030 Place of transaction Issuer
#   Settlement date: 30.03.2030
#   Quantity Security 1234567 ISIN XX0000000000 Price
#   1 000 Example Fund SICAV USD 90.00
#   E-USD-capitalisation
#   Market value in trading currency USD 90 000.00
#   Placement Fee USD 900.00
#   Swiss federal stamp duty USD 120.00
#   To the debit of account 0000 00000000.XX USD Value date 30.03.2030 USD 91 020.00
#
# A prepayment towards a subscription prints no quantity line: the ISIN
# stands on a line of its own below the fund name, and the conversion
# rate reads "USD / CHF at 0.90000". The subscription that settles
# against it prints "Minus your prepayment" and debits the difference.

_PRODUCED_ON_RE = re.compile(rf"^Produced on\s+{LONG_DATE}\s*$", re.M)

_CALL_TITLE_RE = re.compile(r"^Capital Call\s*$", re.M)
# Usually a line of its own; a notice may instead set it at the end of
# the "Investing in" line.
_CALL_ISIN_RE = re.compile(
    rf"(?:^|\s)ISIN\s*[-–]\s*(?P<isin>{ISIN_SHAPE})\s*$", re.M)
_CALL_LETTER_DATE_RE = re.compile(rf"^{LONG_DATE}\s*$")
_CALL_AMOUNT_RE = re.compile(
    r"(?P<ccy>[A-Z]{3})\s+(?P<amount>\d{1,3}(?:,\d{3})*\.\d{2}),?\s+"
    r"which represents")
_CALL_VALUE_DATE_RE = re.compile(
    rf"available in your account\s+(?:by\s+)?value date\s+{LONG_DATE}")
_CALL_PAYABLE_RE = re.compile(
    r"makes the amount of\s+[A-Z]{3}\s+(?P<amount>\d{1,3}(?:,\d{3})*\.\d{2})"
    r"\s+available")
_CALL_TOTAL_RE = re.compile(
    r"^Total\s+\d{1,3}(?:,\d{3})*\.\d{2}\s+"
    r"(?P<amount>\d{1,3}(?:,\d{3})*\.\d{2})\s*$", re.M)

_NOTE_TITLE_RE = re.compile(r"^Contract note\s*$", re.M)
_NOTE_TRADE_DATE_RE = re.compile(r"^Trade date:\s*(?P<date>\d{2}\.\d{2}\.\d{4})\b")
_NOTE_SETTLEMENT_DATE_RE = re.compile(
    r"^Settlement date:\s*(?P<date>\d{2}\.\d{2}\.\d{4})\b")
_NOTE_SECURITY_HEADER_RE = re.compile(
    rf"^Quantity\s+Security\s+(?P<valor>\d+)\s+ISIN\s+(?P<isin>{ISIN_SHAPE})"
    r"\s+Price\s*$")
_NOTE_SECURITY_RE = re.compile(
    r"^(?P<quantity>\d[\d ']*(?:\.\d+)?)\s+(?P<name>\S.*?)\s+"
    r"(?P<ccy>[A-Z]{3})\s+(?P<price>\d[\d ']*\.\d+)\s*$")
_NOTE_ISIN_LINE_RE = re.compile(rf"^(?P<isin>{ISIN_SHAPE})\s*$")
_NOTE_AMOUNT = r"\s+(?P<ccy>[A-Z]{3})\s+(?P<v>\d[\d ']*\.\d{2})\s*$"
_NOTE_FIGURES = {
    "amount": re.compile(r"^Market value in trading currency" + _NOTE_AMOUNT),
    "prepayment": re.compile(r"^Minus your prepayment" + _NOTE_AMOUNT),
    "placement_fee": re.compile(r"^Placement Fee" + _NOTE_AMOUNT),
    "stamp_duty": re.compile(r"^Swiss federal stamp duty" + _NOTE_AMOUNT),
}
_NOTE_DEBIT_RE = re.compile(
    r"^To the debit of account\s+.+?\s+[A-Z]{3}\s+Value date\s+"
    r"(?P<date>\d{2}\.\d{2}\.\d{4})" + _NOTE_AMOUNT)
_NOTE_FX_RES = (
    re.compile(r"^(?P<base>[A-Z]{3})\s*/\s*(?P<quote>[A-Z]{3})\s+at\s+"
               r"(?P<rate>\d+\.\d+)\s*$"),
    re.compile(r"^For\s+(?P<base>[A-Z]{3})\s*/\s*(?P<quote>[A-Z]{3})\s+"
               r"conversions, we have used the following rate:\s*"
               r"(?P<rate>\d+\.\d+)\.?\s*$"),
)


def _advice_row(kind: str, doc_token: str) -> dict:
    """An `advices` row with every figure unset."""
    return {
        "source_doc_token": doc_token, "kind": kind, "title": None,
        "doc_date": None, "trade_date": None, "value_date": None,
        "instrument_isin": None, "valor": None, "security_name": None,
        "currency_iso": None, "quantity": None, "price": None,
        "amount": None, "prepayment": None, "placement_fee": None,
        "stamp_duty": None, "settlement_amount": None,
        "settlement_currency_iso": None, "fx_rate": None,
        "fx_rate_pair": None, "payload": "{}",
    }


def capital_call(extracted: Extracted, doc_token: str) -> list[dict]:
    """Read a capital call from a Private Market Letter. Returns [] for a
    letter that is not one, which is told from the cover page alone so a
    long quarterly report is not read in full."""
    pages = extracted.pages
    if not pages or not _CALL_TITLE_RE.search(pages[0].text):
        return []
    return parse_capital_call_text(document_text(extracted, 4), doc_token)


def parse_capital_call_text(text: str, doc_token: str) -> list[dict]:
    """Pure-text variant of `parse_capital_call`. Returns [] unless the
    document is a capital call stating its ISIN and its called amount,
    the two facts the row is about."""
    if not _CALL_TITLE_RE.search(text):
        return []
    lines = [ln.strip() for ln in text.splitlines()]
    flat = " ".join(ln for ln in lines if ln)
    isin_m = _CALL_ISIN_RE.search(text)
    amount_m = _CALL_AMOUNT_RE.search(flat)
    if isin_m is None or amount_m is None:
        return []

    row = _advice_row("capital_call", doc_token)
    printed = {"amount": amount_m.group(0), "isin": isin_m.group(0).strip()}
    row["instrument_isin"] = isin_m["isin"]
    row["currency_iso"] = row["settlement_currency_iso"] = amount_m["ccy"]
    row["amount"] = to_float(amount_m["amount"])

    # The title is the notice's own heading: the lines between the
    # administrator's dated line and the ISIN line. An ISIN wrapped onto
    # the line after its label matches the text but no single line, and
    # then the heading cannot be placed.
    isin_at = next((k for k, ln in enumerate(lines)
                    if _CALL_ISIN_RE.search(ln)), None)
    title: list[str] = []
    if isin_at is not None:
        for ln in reversed(lines[max(0, isin_at - 4):isin_at]):
            if not ln or _CALL_LETTER_DATE_RE.match(ln):
                break
            title.insert(0, ln)
    row["title"] = " ".join(title) or None

    m = _PRODUCED_ON_RE.search(text)
    if m:
        row["doc_date"] = long_date_to_unix(m)
    m = _CALL_VALUE_DATE_RE.search(flat)
    if m:
        row["value_date"] = long_date_to_unix(m)
        printed["value_date"] = m.group(0)
    m = _CALL_TOTAL_RE.search(text) or _CALL_PAYABLE_RE.search(flat)
    if m:
        row["settlement_amount"] = to_float(m["amount"])
        printed["settlement_amount"] = m.group(0)
    row["payload"] = json.dumps(printed, ensure_ascii=False)
    return [row]


def contract_note(extracted: Extracted, doc_token: str) -> list[dict]:
    """Read the purchase a contract note confirms."""
    return parse_contract_note_text(document_text(extracted, 2), doc_token)


def parse_contract_note_text(text: str, doc_token: str) -> list[dict]:
    """Pure-text variant of `parse_contract_note`. Returns [] unless the
    document is a contract note stating an ISIN and a market value."""
    if not _NOTE_TITLE_RE.search(text):
        return []
    lines = [ln.strip() for ln in text.splitlines() if ln.strip()]
    row = _advice_row("contract_note", doc_token)
    printed: dict[str, str] = {}
    name: list[str] = []
    dates_end = None     # the line after the settlement date
    in_name = False      # reading the security name's wrapped lines

    for k, ln in enumerate(lines):
        m = _PRODUCED_ON_RE.match(ln)
        if m and row["doc_date"] is None:
            row["doc_date"] = long_date_to_unix(m)
            if k + 1 < len(lines):
                row["title"] = lines[k + 1]
            continue
        m = _NOTE_TRADE_DATE_RE.match(ln)
        if m:
            row["trade_date"] = dmy_to_unix(m["date"])
            continue
        m = _NOTE_SETTLEMENT_DATE_RE.match(ln)
        if m:
            row["value_date"] = dmy_to_unix(m["date"])
            dates_end = k + 1
            continue
        m = _NOTE_SECURITY_HEADER_RE.match(ln)
        if m:
            row["valor"], row["instrument_isin"] = m["valor"], m["isin"]
            printed["security"] = ln
            continue
        if row["valor"] is not None and row["quantity"] is None:
            m = _NOTE_SECURITY_RE.match(ln)
            if m:
                row["quantity"] = to_float(m["quantity"])
                row["price"] = to_float(m["price"])
                row["currency_iso"] = m["ccy"]
                name.append(m["name"])
                printed["quantity"] = ln
                in_name = True
                continue
        m = _NOTE_ISIN_LINE_RE.match(ln)
        if m and row["instrument_isin"] is None and dates_end is not None:
            # A prepayment: the fund name is the lines between the dates
            # and the ISIN, less the phrase that introduces it
            # ("Prepayment for Subscription of").
            row["instrument_isin"] = m["isin"]
            name = [n for n in lines[dates_end:k] if not n.endswith(" of")]
            printed["isin"] = ln
            continue
        col = next((c for c, rx in _NOTE_FIGURES.items() if rx.match(ln)),
                   None)
        if col:
            fm = _NOTE_FIGURES[col].match(ln)
            in_name = False
            row[col] = to_float(fm["v"])
            row["currency_iso"] = row["currency_iso"] or fm["ccy"]
            printed[col] = ln
            continue
        if in_name:
            name.append(ln)
            continue
        m = _NOTE_DEBIT_RE.match(ln)
        if m:
            row["value_date"] = dmy_to_unix(m["date"])
            row["settlement_amount"] = to_float(m["v"])
            row["settlement_currency_iso"] = m["ccy"]
            printed["settlement_amount"] = ln
            continue
        for rx in _NOTE_FX_RES:
            m = rx.match(ln)
            if m:
                row["fx_rate"] = to_float(m["rate"])
                row["fx_rate_pair"] = f"{m['base']}/{m['quote']}"
                printed["fx_rate"] = ln
                break

    if row["instrument_isin"] is None or row["amount"] is None:
        return []
    row["security_name"] = " ".join(name) or None
    row["payload"] = json.dumps(printed, ensure_ascii=False)
    return [row]
