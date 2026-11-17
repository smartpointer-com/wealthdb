"""An Account Statement: its balance summary, from the page text, and
its movement ledger, from word positions."""
from __future__ import annotations

import json
import re
from datetime import date

from collectorkit.words import Extracted, Row, Word
from collectorkit.words import rows as word_rows

from pdf_common import IBAN_RE, document_text, to_float, to_unix


# ============================================================
# The balance summary
# ============================================================

_IBAN_HEADER_RE = re.compile(
    r"IBAN\s*(?P<iban>CH\d{2}[A-Z0-9]{17})\b"
)
_PERIOD_RE = re.compile(
    r"(?P<from_day>\d{2})\.(?P<from_month>\d{2})\.(?P<from_year>\d{4})\s*-\s*"
    r"(?P<to_day>\d{2})\.(?P<to_month>\d{2})\.(?P<to_year>\d{4})"
)
# UBS Account-Statement headers come in a few variants, all
# rendered with no internal whitespace, e.g. 'UBSSavingsAccountCHF'.
# The common shape is "<account-type-words><CCY>" with the CCY
# being the trailing three uppercase letters. We anchor on the
# substring 'Account' and capture the trailing ISO code.
_CCY_HEADER_RE = re.compile(
    r"[A-Za-z]*[Aa]ccount[A-Za-z]*(?P<ccy>[A-Z]{3})\b"
)
# The four balance/total regexes run against the squished `flat`
# text (whitespace already removed). UBS uses thin spaces as
# thousand separators in the rendered PDF (e.g. "1 234.56"), so
# after the `replace(" ", "")` pass the value has neither spaces
# nor apostrophes between digits. We keep `'` in the value class
# defensively in case some historical statements use the Swiss
# apostrophe convention and pdfplumber preserves it. The decimal
# part is optional: zero-balance / closed-account statements
# render the value as a bare `0`, not `0.00`.
_VAL = r"-?[\d']+(?:\.\d{2})?"
_OPENING_BAL_RE = re.compile(rf"Openingbalance(?P<v>{_VAL})")
_CLOSING_BAL_RE = re.compile(rf"Closingbalance(?P<v>{_VAL})")
_TOTAL_CREDITS_RE = re.compile(rf"Totalcredits(?P<v>{_VAL})")
_TOTAL_DEBITS_RE = re.compile(rf"Totaldebits(?P<v>{_VAL})")


def parse_account_statement_text(text: str, doc_token: str) -> list[dict]:
    """Account-Statement balance-summary parser: from the first-two-page
    text emit ONE row with opening + closing balance + period bounds for
    the covered cash account. Takes already-extracted PDF text so the
    same text can serve both the summary and movement passes (and so
    tests can exercise the regex layer without a real PDF on disk)."""
    # UBS Account-Statement PDFs render text with all the
    # whitespace squished out within tokens (e.g. `IBANCHKK...`,
    # `Openingbalance1234.56`). pdfplumber preserves that. We
    # match all the headers against the squished text.
    flat = text.replace(" ", "").replace(" ", "")  #   = NBSP
    iban = None
    m = _IBAN_HEADER_RE.search(flat)
    if m:
        iban = m["iban"].upper()
    if not iban or len(iban) != 21:
        return []

    period = _PERIOD_RE.search(flat)
    if not period:
        return []
    period_start = date(int(period["from_year"]), int(period["from_month"]),
                        int(period["from_day"]))
    period_end = date(int(period["to_year"]), int(period["to_month"]),
                      int(period["to_day"]))

    ccy_m = _CCY_HEADER_RE.search(flat)
    currency = ccy_m["ccy"] if ccy_m else None
    opening = _OPENING_BAL_RE.search(flat)
    closing = _CLOSING_BAL_RE.search(flat)
    credits = _TOTAL_CREDITS_RE.search(flat)
    debits = _TOTAL_DEBITS_RE.search(flat)

    return [{
        "period_end": to_unix(period_end),
        "account_external_id": iban,
        "currency_iso": currency,
        "period_start": to_unix(period_start),
        "opening_balance": to_float(opening["v"]) if opening else None,
        "closing_balance": to_float(closing["v"]) if closing else None,
        "total_debits": to_float(debits["v"]) if debits else None,
        "total_credits": to_float(credits["v"]) if credits else None,
        "source_doc_token": doc_token,
        "payload": json.dumps({
            "currency_header_match": bool(ccy_m),
            "period_start": period_start.isoformat(),
            "period_end": period_end.isoformat(),
        }),
    }]


# ============================================================
# Account-Statement MOVEMENT parser (per-transaction ledger rows)
# ============================================================
#
# The summary parser above (`parse_account_statement_text`) reads
# only the opening/closing balances. This parser walks the ledger and
# emits every booking (movement) row, so transaction history from
# before the CSV feed's earliest available date can be backfilled
# from the PDF archive.
#
# Layout (see docs + probing): the ledger is a visually-aligned,
# NOT a real PDF table. Columns are:
#
#   Date | Information | Debits | Credits | Value date | Balance
#
# Only ONE of Debits / Credits is populated per row, and — crucially
# — the plain text gives no marker of WHICH column an amount sits in.
# We therefore work at the WORD level (`extract_words`) and bucket
# each numeric token by its x-position against the ledger header's
# per-column anchors. Alphabetic tokens are the booking-type text
# (Information); they never poison the amount columns. This is
# validated against the running Balance column: for every statement
# in the archive `opening + Σ(credit − debit) == balance` chains
# row-by-row to the printed Closing balance.
#
# Conventions handled: space (thin-space / NBSP) thousands
# separators ('199 993.01'); trailing-minus negatives ('16.05-');
# 2-digit years (DD.MM.YY); multi-page ledgers with a repeated
# header per page; the "not included in the closing balance"
# post-closing trailer (future-period bookings shown for
# information — flagged `post_closing`, excluded from the
# opening→closing reconciliation, deduped across statements by the
# loader's content id).

_STMT_ROW_DATE_RE = re.compile(r"^\d{2}\.\d{2}\.\d{2}$")   # DD.MM.YY
# A counter-account reference in a continuation line: either a full
# CH-IBAN (inter-account e-banking transfer) or a UBS mortgage
# account stamp ("HYPOTHEK <base>.<type> <sub-account>").
_STMT_HYPO_RE = re.compile(r"HYPOTHEK\s+[\d ]+\.[A-Z0-9]+\s+\d+")

def _stmt_amount(tokens: list[str]) -> float | None:
    """Join a ledger column's numeric word-fragments into a float.
    Handles space/apostrophe thousands separators and trailing-minus
    negatives ('16.05-' → -16.05). Returns None for an empty column
    or a non-numeric fragment (e.g. leaked Information text)."""
    if not tokens:
        return None
    s = "".join(tokens)
    for ch in (" ", " ", " ", "'", "’", ","):
        s = s.replace(ch, "")
    neg = s.endswith("-")
    s = s.rstrip("-").rstrip("+")
    if not re.match(r"^\d+(?:\.\d+)?$", s):
        return None
    return -float(s) if neg else float(s)


def _stmt_find_header(rows: list[Row]) -> dict | None:
    """Return the ledger header's per-column anchor x-centres.

    The REAL ledger header carries the full label set; a decoy
    summary box ("Your account at a glance  Debits Credits Balance")
    lacks Date/Information/Value, so require them all."""
    for r in rows:
        texts = [w.text for w in r.words]
        if all(t in texts for t in
               ("Date", "Information", "Value", "Debits", "Credits",
                "Balance")):
            def centre(label: str, words=r.words) -> float | None:
                for w in words:
                    if w.text == label:
                        return (w.x0 + w.x1) / 2
                return None
            return {
                "Debits": centre("Debits"),
                "Credits": centre("Credits"),
                "ValueDate": centre("Value"),
                "Balance": centre("Balance"),
                "top": r.top,
            }
    return None


def _stmt_assign_numeric(word: Word, anchors: dict) -> str | None:
    """Nearest NUMERIC-column anchor for a digit-leading token."""
    cx = (word.x0 + word.x1) / 2
    best, best_d = None, 1e9
    for col in ("Debits", "Credits", "ValueDate", "Balance"):
        a = anchors.get(col)
        if a is None:
            continue
        d = abs(cx - a)
        if d < best_d:
            best_d, best = d, col
    return best


def _stmt_counter_account(cont_lines: list[str]) -> str | None:
    """Pull a counter-account reference (CH-IBAN or HYPOTHEK stamp)
    from a movement's continuation lines, for the gold engine's
    internal-transfer / mortgage netting. Whitespace-normalised."""
    for c in cont_lines:
        m = IBAN_RE.search(c)
        if m:
            return re.sub(r"\s+", "", m.group(0))
    for c in cont_lines:
        m = _STMT_HYPO_RE.search(c)
        if m:
            return re.sub(r"\s+", " ", m.group(0)).strip()
    return None


# An order or settlement reference — a letter, a date and a number,
# "V 01.01.2020 12345678". Skipped before the valor scan rather than
# left to it: the reference opens a trade's continuation, so it is
# reached first, and its number passes the valor test.
_STMT_REFERENCE_RE = re.compile(r"^[A-Z]\s+\d{2}\.\d{2}\.\d{4}\b")
# The valor closing a security line: the last whitespace-separated
# token, all digits, with something bearing a letter in front of it
# ("EXAMPLEETF WORLD 1234567"). A turnover trailer ends in a decimal
# group and falls out on its own.
_STMT_VALOR_RE = re.compile(r"^(?P<caption>.*[A-Za-z].*?)\s+(?P<valor>\d{4,12})$")
# The same, where a long caption left no room for the separator and the
# valor is printed hard against it ("Example Group Rg199999991"). Six
# digits, not four: with no separator a short run is far likelier to be
# the tail of a name than a valor.
_STMT_GLUED_VALOR_RE = re.compile(r"^(?P<caption>.*[A-Za-z].*?)(?P<valor>\d{6,12})$")


def _stmt_security(cont_lines: list[str]) -> tuple[str | None, str | None]:
    """The instrument caption and Swiss VALOR a securities movement names.

    The valor identifies one security line, which is what lets the gold
    engine match the movement to an instrument by identity rather than
    by name. Returns (caption, valor), or (None, None) where no
    continuation line closes with one — every movement that is not a
    trade, and any whose caption itself ends in digits, which mints a
    number that matches no instrument and so resolves to nothing.

    The spaced form is tried across every line before the glued one, so
    the glued form reads only a movement no line of which closes with a
    spaced valor.
    """
    lines = [c.strip() for c in cont_lines if not _STMT_REFERENCE_RE.match(c.strip())]
    for pattern in (_STMT_VALOR_RE, _STMT_GLUED_VALOR_RE):
        for c in lines:
            m = pattern.match(c)
            if m:
                caption = re.sub(r"\s+", " ", m.group("caption")).strip()
                if caption:
                    return caption, m.group("valor")
    return None, None


# A bundled payment order. The statement books a batch of e-banking
# payments as ONE movement carrying the batch total, and prints the
# beneficiaries under it, closed by a "<N> times <rail>" trailer. With
# N > 1 that single row is several unrelated payments — different
# beneficiaries, different purposes — added together, and no consumer
# downstream can take them apart again: the gold description becomes
# every beneficiary concatenated, and one merchant signature stands for
# the lot.
#
# The trailer is what makes the batch legible, and it is printed by
# booking type rather than belonging to one: MULTI E-BANKING ORDER and
# MULTI PAYNET ORDER both bundle, and the same trailer closes an
# ordinary single order with "1 times". Matching the SHAPE rather than
# the booking type therefore splits every bundle the statement can
# print, including types not yet seen, and leaves every single order
# alone by construction.
_STMT_MULTI_TRAILER_RE = re.compile(r"^(\d+)\s+times\s+(\S.*)$", re.I)

# A leg's own amount, at the end of its first line ("EXAMPLE AG 1 234.50").
# The decimals are required: an address line ends in a postcode, and a
# bare integer would open a leg that does not exist. Thousands are
# grouped with a space or an apostrophe, as elsewhere in the ledger.
_STMT_LEG_AMOUNT_RE = re.compile(r"\s(\d{1,3}(?:[ '\u2019]\d{3})*|\d+)\.(\d{2})$")


def _stmt_multi_trailer(cont_lines: list[str]) -> tuple[int, str, int] | None:
    """The batch trailer as (count, rail, line index), or None when the
    movement carries none."""
    for i, line in enumerate(cont_lines):
        m = _STMT_MULTI_TRAILER_RE.match(str(line).strip())
        if m:
            return int(m.group(1)), m.group(2).strip(), i
    return None


def _stmt_split_multi(cont_lines: list[str],
                      total: float | None) -> list[dict] | None:
    """Split a bundled order's continuation into one entry per payment,
    as {amount, lines}. None when the movement is not a bundle, or when
    the split cannot be proved — a shape that does not add up is left
    whole rather than guessed at, because a wrong split moves money
    between beneficiaries.

    A line ending in an amount opens a payment and the amount is taken
    off its text; the lines under it are that beneficiary's name and
    address overflow, which run to as many lines as the beneficiary
    needs. Anything after the trailer is the page's own furniture and
    belongs to no payment.

    Proved means both of the trailer's claims hold: as many payments
    were found as it counts, and they add up to the movement's printed
    total. Either alone is too weak — equal counts with a misread
    amount still moves money, and an accidental sum with the wrong
    count still merges two payments.
    """
    trailer = _stmt_multi_trailer(cont_lines)
    if trailer is None or total is None:
        return None
    count, _rail, at = trailer
    if count < 2:
        return None
    legs: list[dict] = []
    for line in cont_lines[:at]:
        line = str(line).rstrip()
        m = _STMT_LEG_AMOUNT_RE.search(line)
        if m:
            legs.append({"amount": float(re.sub(r"[ '\u2019]", "",
                                                m.group(1) + "." + m.group(2))),
                         "lines": [line[:m.start()].rstrip()]})
        elif legs:
            legs[-1]["lines"].append(line)
        else:
            return None          # text before the first amount: unknown shape
    if len(legs) != count:
        return None
    if abs(round(sum(x["amount"] for x in legs), 2) - round(total, 2)) > 0.005:
        return None
    return legs


def parse(extracted: Extracted, doc_token: str
          ) -> tuple[list[dict], list[dict]]:
    """Both passes over an Account Statement: the balance summary
    (opening/closing balance and period bounds) from its first two
    pages' text, and the movement ledger (one dict per booking row).
    Returns (cash-balance rows, movement rows)."""
    cash = parse_account_statement_text(document_text(extracted, 2),
                                        doc_token)
    return cash, movements(extracted, doc_token)


def movements(extracted: Extracted, doc_token: str) -> list[dict]:
    """One dict per booking (movement) row of the ledger. The loader maps
    these into the silver `transactions` table (deterministic id +
    per-account MT940 cut-over); rows carry the statement-level
    reconciliation result so the loader can gate on it."""
    ledger = _read_ledger(extracted)
    return _ledger_rows(ledger, not _chain_problems(ledger), doc_token)


def _read_ledger(extracted: Extracted) -> dict:
    """The ledger as printed: the account and currency the page header
    names, the opening and closing balances, and each movement with its
    continuation lines (`_cont`), in order."""
    ledger = {"iban": None, "currency": None, "opening": None,
              "closing": None, "movements": []}
    movements = ledger["movements"]
    cur: dict | None = None
    seen_closing = False

    for page in extracted.pages:
        if ledger["iban"] is None:
            m = IBAN_RE.search(page.text)
            if m:
                ledger["iban"] = re.sub(r"\s+", "", m.group(0)).upper()
        if ledger["currency"] is None:
            cm = _CCY_HEADER_RE.search(page.text.replace(" ", ""))
            if cm:
                ledger["currency"] = cm["ccy"]

        # Rows by the top of their words, three points apart.
        rows = word_rows(page.words(x_tolerance=1.5, keep_blank_chars=False),
                         3.0, "first")
        anchors = _stmt_find_header(rows)
        if not anchors:
            continue

        for r in rows:
            if r.top <= anchors["top"] + 1:
                continue
            ws = r.words
            # Row date = leading DD.MM.YY token in the far-left Date
            # column (x0 < 80). Everything else is content.
            row_date = None
            content = ws
            if _STMT_ROW_DATE_RE.match(ws[0].text) and ws[0].x0 < 80:
                row_date = ws[0].text
                content = ws[1:]

            if row_date is None:
                # Continuation line — attach to the preceding movement.
                if cur is not None:
                    cur["_cont"].append(" ".join(w.text for w in ws))
                continue

            # Digit-leading tokens → numeric columns (by nearest
            # anchor); alphabetic tokens → Information booking type.
            buckets: dict[str, list[str]] = {}
            info_words: list[str] = []
            for w in content:
                if w.text[:1].isdigit():
                    col = _stmt_assign_numeric(w, anchors)
                    buckets.setdefault(col, []).append(w.text)
                else:
                    info_words.append(w.text)
            info = " ".join(info_words).strip()

            if info == "Opening balance":
                if ledger["opening"] is None:
                    ledger["opening"] = _stmt_amount(buckets.get("Balance", []))
                cur = None
                continue
            if info == "Closing balance":
                ledger["closing"] = _stmt_amount(buckets.get("Balance", []))
                seen_closing = True
                cur = None
                continue

            vd = None
            for t in buckets.get("ValueDate", []):
                if _STMT_ROW_DATE_RE.match(t):
                    vd = t
                    break
            cur = {
                "booking_dmy": row_date,
                "value_dmy": vd,
                "description_kind": info,
                "amount_debit": _stmt_amount(buckets.get("Debits", [])),
                "amount_credit": _stmt_amount(buckets.get("Credits", [])),
                "running_balance": _stmt_amount(buckets.get("Balance", [])),
                "post_closing": seen_closing,
                "_cont": [],
            }
            movements.append(cur)
    return ledger


def _chain_problems(ledger: dict) -> list[str]:
    """Where the statement-level reconciliation fails: opening +
    Σ(credit − debit) must chain to each row's Balance and the printed
    closing, over the MAIN ledger only (post-closing trailer excluded).
    Empty when it holds."""
    if ledger["opening"] is None:
        return ["no opening balance"]
    run = ledger["opening"]
    for mv in ledger["movements"]:
        if mv["post_closing"]:
            continue
        run += (mv["amount_credit"] or 0.0) - (mv["amount_debit"] or 0.0)
        bal = mv["running_balance"]
        if bal is not None and abs(run - bal) > 0.02:
            return [f"the running balance breaks at {mv['booking_dmy']}"]
    if ledger["closing"] is not None and abs(run - ledger["closing"]) > 0.02:
        return ["the movements do not reach the closing balance"]
    return []


def _ledger_row(ledger: dict, mv: dict, *, occurrence: int, reconciled: bool,
                doc_token: str, amount_debit, amount_credit, counterparty,
                counter_account, cont: list[str], balance,
                leg: dict | None = None) -> dict:
    """One movement row as the loader stores it: the printed figures,
    dates and narrative, and its payload. `leg` is the split of a
    bundled order this row is one payment of."""
    booking = _stmt_dmy_to_unix(mv["booking_dmy"])
    row = {
        "booking_date": booking,
        "value_date": _stmt_dmy_to_unix(mv["value_dmy"]) or booking,
        "account_external_id": ledger["iban"],
        "currency_iso": ledger["currency"],
        "amount_debit": amount_debit,
        "amount_credit": amount_credit,
        "description_kind": mv["description_kind"] or None,
        "counterparty": counterparty,
        "counter_account": counter_account,
        "running_balance": balance,
        "post_closing": mv["post_closing"],
        "occurrence": occurrence,
        "reconciled": reconciled,
        "source_doc_token": doc_token,
    }
    sec_caption, sec_valor = _stmt_security(cont)
    payload = {
        "booking_type": mv["description_kind"] or None,
        "running_balance": balance,
        "value_date": mv["value_dmy"],
        "counter_account": counter_account,
        "continuation": cont,
        "post_closing": mv["post_closing"],
        "source": "account_statement_pdf",
    }
    # Promoted beside `counter_account` and for the same reason: the
    # fact is in the continuation either way, and parsing it once here
    # beats every consumer re-deriving it.
    if sec_valor:
        payload["security_caption"] = sec_caption
        payload["security_valor"] = sec_valor
    if leg:
        row.update(leg["row"])
        payload["multi_leg"] = leg["payload"]
    row["payload"] = json.dumps(payload, ensure_ascii=False)
    return row


def _ledger_rows(ledger: dict, reconciled: bool, doc_token: str
                 ) -> list[dict]:
    """Each movement as one row, or one row per payment of a bundled
    order: dates, counter-account, per-statement occurrence index
    (disambiguates identical same-day bookings and keeps the id stable
    across the monthly/annual statement overlap and the post-closing
    trailer), and the payload."""
    occ: dict[tuple, int] = {}
    out: list[dict] = []
    for mv in ledger["movements"]:
        booking = _stmt_dmy_to_unix(mv["booking_dmy"])
        value = _stmt_dmy_to_unix(mv["value_dmy"]) or booking
        key = (booking, value, mv["description_kind"],
               mv["amount_debit"], mv["amount_credit"])
        idx = occ.get(key, 0)
        occ[key] = idx + 1
        shared = {"occurrence": idx, "reconciled": reconciled,
                  "doc_token": doc_token}

        # A bundle becomes one row per payment and the batch row itself
        # is not emitted: every consumer downstream sees plain single
        # transactions, and the total survives as the sum of the legs.
        # A movement that is not a bundle, or one whose split could not
        # be proved, is emitted whole, as printed.
        total = mv["amount_debit"] if mv["amount_debit"] is not None else mv["amount_credit"]
        legs = _stmt_split_multi(mv["_cont"], total)
        if legs is None:
            out.append(_ledger_row(
                ledger, mv, **shared,
                amount_debit=mv["amount_debit"],
                amount_credit=mv["amount_credit"],
                counterparty=mv["_cont"][0] if mv["_cont"] else None,
                counter_account=_stmt_counter_account(mv["_cont"]),
                cont=mv["_cont"], balance=mv["running_balance"]))
            continue
        count, rail, _at = _stmt_multi_trailer(mv["_cont"])
        debit_side = mv["amount_debit"] is not None
        for i, leg in enumerate(legs, start=1):
            lines = [ln for ln in leg["lines"] if ln.strip()]
            out.append(_ledger_row(
                ledger, mv, **shared,
                amount_debit=leg["amount"] if debit_side else None,
                amount_credit=None if debit_side else leg["amount"],
                counterparty=lines[0] if lines else None,
                # Each leg is read on its own: a counter-account belongs
                # to the payment that carries it, not to every payment
                # the batch happened to include.
                counter_account=_stmt_counter_account(lines),
                cont=lines,
                # The printed balance is the one after the whole batch
                # posted, so it belongs to the last leg. The balances
                # between legs were never printed, and deriving them
                # would state something the statement does not.
                balance=mv["running_balance"] if i == len(legs) else None,
                leg={"row": {"multi_leg_index": i,
                             "multi_parent_debit": mv["amount_debit"],
                             "multi_parent_credit": mv["amount_credit"]},
                     "payload": {"index": i, "count": count, "rail": rail}}))
    return out


def _stmt_dmy_to_unix(dmy: str | None) -> int | None:
    """DD.MM.YY (2-digit year, 20YY) → Unix seconds UTC midnight."""
    if not dmy:
        return None
    try:
        d, mo, yy = (int(x) for x in dmy.split("."))
    except ValueError:
        return None
    return to_unix(date(2000 + yy, mo, d))
