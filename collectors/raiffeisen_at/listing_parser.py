"""Raiffeisen core-banking transaction listings — the parser.

A transaction listing is a back-office extract (a "SMARTBank Abfrage") that a
branch prints on request: one account, every posting from a requested start
date up to the print day, as a fixed-width table. It reaches years further back
than Mein ELBA's rolling ~36-month history, so listings dropped into the data
dir's `supplied/` directory backfill the ledger (DESIGN.md §I). How several
listings and the live history combine into one ledger is stitch.py's job.

Everything here is pure: `parse_listing` reads a listing's `pdftotext -layout`
text (collectorkit.pdftotext extracts it) into a `Listing`, and `problems`
holds a listing to its own arithmetic and selection.

The layout, and the trap behind each rule:

- Every page repeats a header block, including the column header
  `BUTAG AN PRNR HERK TXT VAL. UMSATZ SALDO DRUCKDATUM ART`, and the columns
  drift by a few characters from page to page — so each page is read against
  its own column header.
- The header labels are not flush with their data. The column header is used
  only to tell the two right-aligned money columns apart (UMSATZ and SALDO end
  ~17 columns apart), never to slice the left-hand code fields.
- BUTAG (the booking day) prints on a day's first row only and is never
  reprinted, not even across a page break.
- SALDO prints on a day's last row only: the day's closing balance.
- A debit carries a trailing minus. VAL. (the value date) has no year.
- DRUCKDATUM is the day the posting was printed on a statement and stays blank
  until it has been, so two extracts of one account differ there. It is kept
  as provenance and never enters an id.
- Continuation lines (payee, purpose, mandate, …) belong to the row above,
  run on across page breaks and past a mid-page units banner, and the bank's
  fixed-width text record can split a label across two lines
  ("… EUR 1,00Auftrag" / "geber: …").
"""
from __future__ import annotations

import hashlib
import re
from collections import Counter
from collections.abc import Sequence
from dataclasses import dataclass, field
from datetime import date, datetime


class ListingError(ValueError):
    """The text is a listing, but its layout could not be read."""


# ============================================================
# Layout patterns
# ============================================================

_COLUMNS_RE = re.compile(
    r"^\s*BUTAG\s+AN\s+PRNR\s+HERK\s+TXT\s+VAL\.\s+UMSATZ\s+SALDO\s+"
    r"DRUCKDATUM\s+ART\s*$", re.M)
_BANNER_RE = re.compile(
    r"(\d{2}\.\d{2}\.\d{4})/(\d{2}:\d{2})/\S*\s+Seite\s+(\d+)\s*$")
_ACCOUNT_NO_RE = re.compile(r"Kontonummer:\s*([\d.]+)")
_FROM_RE = re.compile(r"Ausgabe Datum ab:\s*(\d{2}\.\d{2}\.(?:\d{4}|\d{2}))\b")
_SELECTION_RE = re.compile(r"Auswahl Umsätze:\s*(\S*)")
_KEY_TEXT_RE = re.compile(r"^\s*Schlüsseltext:\s*(.*?)\s*$")
_BALANCE_FLAG_RE = re.compile(r"Saldo anzeigen J/N:\s*([JN])")
_TEXT_FLAG_RE = re.compile(r"Klartext anz\. J/N:\s*([JN])")
# The account line under the column header: "<account no>/<ccy>  <holder>
# REF: …". The holder's name is never read.
_ACCOUNT_LINE_RE = re.compile(r"^\s+([\d.]+)/([A-Z]{3})\s.*\bREF:")
_UNITS_RE = re.compile(r"^\s*(?:KTO-WHG/[A-Z]{3}\s*)+$")
_RULE_RE = re.compile(r"^\s*(?:—{10,}|-{10,})\s*$")
_TOTAL_RE = re.compile(r"^\s*Summe (Soll|Haben)\s+(\S+)\s*$")
_ROW_RE = re.compile(
    r"^ (?P<butag>\d{2}\.\d{2}\.\d{4}| {10}) (?P<codes>.{1,26}?) "
    r"(?P<val>\d{2}\.\d{2})(?P<tail>\s.*)$")
_MONEY_RE = re.compile(r"^\d{1,3}(?:\.\d{3})*,\d{2}-?$")
# The row's tail is tokenised by pattern, not by whitespace: a negative SALDO
# prints its minus in the column right before DRUCKDATUM, with no space.
_TAIL_TOKEN_RE = re.compile(r"\d{1,3}(?:\.\d{3})*,\d{2}-?|\d{2}\.\d{2}\.\d{4}|\S+")
_DATE_RE = re.compile(r"^\d{2}\.\d{2}\.\d{4}$")
_HERK_RE = re.compile(r"^[A-Z][A-Z0-9]{1,3}$")
_LABEL_RE = re.compile(r"^([A-ZÄÖÜ][A-Za-zÄÖÜäöüß\-]*):(?:\s+(.*))?$")
_FRAGMENT_RE = re.compile(r"^([a-zäöüß][A-Za-zÄÖÜäöüß\-]*):")

# Right-aligned money tokens end within this many columns of their header
# label's end; the two money columns are ~17 apart, so this never confuses
# them.
_MONEY_TOLERANCE = 2

# Labels the continuation lines use. Only needed to re-join a label the text
# record split across two lines; any other `Word:` line is read as a label
# too.
_KNOWN_LABELS = (
    "Auftraggeberinformation", "Auftraggeber", "Zahlungsempfänger",
    "Empfänger-Kennung", "Empfänger", "Verwendungszweck", "Zahlungsreferenz",
    "Mandat", "Kundendaten", "Entgelte",
)
_COUNTERPARTY_LABELS = ("Zahlungsempfänger", "Empfänger", "Auftraggeber")
_PURPOSE_LABELS = ("Kundendaten", "Verwendungszweck")


# ============================================================
# The parsed shape
# ============================================================

@dataclass(frozen=True)
class Posting:
    butag: date                        # booking day
    an: str
    prnr: str
    herk: str                          # operation code, e.g. 'SELE', 'ABS'
    txt: str                           # text key
    val: str                           # value date as printed, 'dd.mm'
    amount_cents: int                  # signed: debits negative
    saldo_cents: int | None            # the day's closing balance, last row only
    druckdatum: date | None            # statement print day, None until printed
    art: str
    fields: tuple[tuple[str | None, tuple[str, ...]], ...]  # (label, lines)
    page: int

    @property
    def valuta(self) -> date:
        return value_date(self.butag, self.val)

    @property
    def lines(self) -> tuple[str, ...]:
        """The continuation text, one entry per printed line."""
        out = []
        for label, lines in self.fields:
            if label is None:
                out.extend(lines)
            else:
                out.append(f"{label}: {lines[0]}".rstrip() if lines else f"{label}:")
                out.extend(lines[1:])
        return tuple(out)

    def labelled(self, label: str) -> tuple[str, ...] | None:
        for lab, lines in self.fields:
            if lab == label:
                return lines
        return None

    @property
    def counterparty(self) -> str | None:
        """The payee / payer: the first line of the first counterparty field."""
        for label in _COUNTERPARTY_LABELS:
            lines = self.labelled(label)
            if lines and lines[0]:
                return lines[0]
        return None

    @property
    def description(self) -> str | None:
        """The purpose: the customer-data and purpose fields, else the payment
        reference, else the unlabelled booking text (closing entries, cash
        withdrawals, standing orders)."""
        parts = [" ".join(lines) for lab, lines in self.fields
                 if lab in _PURPOSE_LABELS and lines]
        if not parts:
            ref = self.labelled("Zahlungsreferenz")
            parts = [" ".join(ref)] if ref else []
        if not parts:
            parts = [" ".join(lines) for lab, lines in self.fields if lab is None]
        text = " ".join(p for p in parts if p).strip()
        return text or None


@dataclass(frozen=True)
class Listing:
    account_number: str                # digits only
    currency: str
    coverage_start: date               # "Ausgabe Datum ab"
    printed_at: datetime               # the page banner, as printed (local)
    selection: str                     # "Auswahl Umsätze"
    key_text: str                      # "Schlüsseltext"
    shows_balance: bool                # "Saldo anzeigen J/N"
    shows_text: bool                   # "Klartext anz. J/N"
    postings: tuple[Posting, ...]
    stated_debits_cents: int | None    # "Summe Soll", as a positive figure
    stated_credits_cents: int | None   # "Summe Haben"

    @property
    def print_day(self) -> date:
        return self.printed_at.date()

    def closing_balances(self) -> dict[date, int]:
        """The printed closing balance per booking day."""
        return {p.butag: p.saldo_cents for p in self.postings
                if p.saldo_cents is not None}

    def opening_balance(self) -> int | None:
        """The balance at the end of the day before `coverage_start`: the
        first printed closing balance less that day's postings. Exact because
        the listing covers every day from `coverage_start` on."""
        if not self.postings:
            return None
        first = self.postings[0].butag
        day = [p for p in self.postings if p.butag == first]
        if day[-1].saldo_cents is None:
            return None
        return day[-1].saldo_cents - sum(p.amount_cents for p in day)

    def postings_by_day(self) -> dict[date, Counter]:
        out: dict[date, Counter] = {}
        for p in self.postings:
            out.setdefault(p.butag, Counter())[p.amount_cents] += 1
        return out


# ============================================================
# Scalars
# ============================================================

def parse_money(token: str) -> int:
    """German-formatted money ('1.234,56', trailing '-' for a debit) → signed
    cents."""
    negative = token.endswith("-")
    cents = int(token.rstrip("-").replace(".", "").replace(",", ""))
    return -cents if negative else cents


def _strptime(text: str, fmt: str) -> datetime:
    """strptime whose failure is a layout error, not a crash: a token can fit
    the date pattern and still name no day (31.02.)."""
    try:
        return datetime.strptime(text, fmt)
    except ValueError as exc:
        raise ListingError("a date that names no day") from exc


def _parse_day(token: str) -> date:
    return _strptime(token, "%d.%m.%Y").date()


def value_date(butag: date, val: str) -> date:
    """VAL. prints 'dd.mm' with no year. It lies within weeks of the booking
    day, so a month more than six away belongs to the neighbouring year: a
    January value date on a December booking is next year's, a December one
    on a January booking last year's."""
    day, month = int(val[:2]), int(val[3:5])
    year = butag.year
    if month - butag.month > 6:
        year -= 1
    elif month - butag.month < -6:
        year += 1
    return date(year, month, day)


def is_listing(text: str) -> bool:
    """Whether extracted text has a transaction listing's header block."""
    return bool(_COLUMNS_RE.search(text)) and "Kontonummer:" in text


# ============================================================
# The parser
# ============================================================

@dataclass
class _Header:
    """One page's header block. `columns` holds where this page's PRNR column
    starts and its UMSATZ and SALDO columns end — the geometry drifts."""
    printed_at: datetime | None = None
    page_no: int | None = None
    account_number: str | None = None
    account_line_number: str | None = None
    currency: str | None = None
    units: set[str] = field(default_factory=set)
    coverage_start: date | None = None
    selection: str | None = None
    key_text: list[str] = field(default_factory=list)
    shows_balance: bool | None = None
    shows_text: bool | None = None
    columns: dict[str, int] | None = None


def _read_header(lines: list[str]) -> _Header:
    h = _Header()
    for line in lines:
        if (m := _BANNER_RE.search(line)):
            h.printed_at = _strptime(f"{m[1]} {m[2]}", "%d.%m.%Y %H:%M")
            h.page_no = int(m[3])
        if (m := _ACCOUNT_NO_RE.search(line)):
            h.account_number = m[1].replace(".", "")
        if (m := _FROM_RE.search(line)):
            fmt = "%d.%m.%Y" if len(m[1]) == 10 else "%d.%m.%y"
            h.coverage_start = _strptime(m[1], fmt).date()
        if (m := _SELECTION_RE.search(line)):
            h.selection = m[1]
        if (m := _KEY_TEXT_RE.match(line)) and m[1]:
            h.key_text.append(m[1])
        if (m := _BALANCE_FLAG_RE.search(line)):
            h.shows_balance = m[1] == "J"
        if (m := _TEXT_FLAG_RE.search(line)):
            h.shows_text = m[1] == "J"
        if (m := _ACCOUNT_LINE_RE.match(line)):
            h.account_line_number = m[1].replace(".", "")
            h.currency = m[2]
        if _UNITS_RE.match(line):
            h.units.update(re.findall(r"KTO-WHG/([A-Z]{3})", line))
        if _COLUMNS_RE.match(line) and h.columns is None:
            h.columns = {"prnr_start": line.index("PRNR"),
                         "umsatz_end": line.index("UMSATZ") + len("UMSATZ"),
                         "saldo_end": line.index("SALDO") + len("SALDO")}
    return h


def _is_furniture(line: str) -> bool:
    stripped = line.strip()
    return (not stripped
            or bool(_BANNER_RE.search(line))
            or stripped.startswith(("Kontonummer:", "Schlüsseltext:",
                                    "Saldo anzeigen", "Rech.legung"))
            or bool(_RULE_RE.match(line))
            or bool(_COLUMNS_RE.match(line))
            or bool(_ACCOUNT_LINE_RE.match(line))
            or bool(_UNITS_RE.match(line))
            or bool(_TOTAL_RE.match(line)))


def _split_codes(m: re.Match, columns: dict[str, int]) -> tuple[str, str, str, str] | None:
    """AN / PRNR / HERK / TXT from the code field, or None when it does not
    look like one. TXT is always last and HERK is the token with a letter; the
    leading digit tokens are AN then PRNR, and a lone one is placed by where
    it ends against the page's PRNR column."""
    raw = m.group("codes")
    tokens = [(t.group(0), m.start("codes") + t.end())
              for t in re.finditer(r"\S+", raw)]
    if not tokens or len(raw) - len(raw.lstrip()) > 6:
        return None
    txt, _ = tokens.pop()
    if not txt.isdigit():
        return None
    herk = ""
    if tokens and _HERK_RE.match(tokens[-1][0]):
        herk, _ = tokens.pop()
    if len(tokens) > 2 or any(not t.isdigit() for t, _ in tokens):
        return None
    an = prnr = ""
    if len(tokens) == 2:
        an, prnr = tokens[0][0], tokens[1][0]
    elif tokens:
        tok, end = tokens[0]
        if end <= columns["prnr_start"] + 1:
            an = tok
        else:
            prnr = tok
    return an, prnr, herk, txt


def _read_tail(line: str, m: re.Match, columns: dict[str, int]
               ) -> tuple[int, int | None, date | None, str] | None:
    """UMSATZ, SALDO, DRUCKDATUM and ART from the rest of the row, or None
    when it does not read as a row's tail. Money tokens are told apart by
    where their digits end."""
    umsatz = saldo = druck = None
    art = []
    for t in _TAIL_TOKEN_RE.finditer(line[m.start("tail"):]):
        token = t.group(0)
        end = m.start("tail") + t.end()
        if _MONEY_RE.match(token):
            digits_end = end - (1 if token.endswith("-") else 0)
            if abs(digits_end - columns["umsatz_end"]) <= _MONEY_TOLERANCE and umsatz is None:
                umsatz = parse_money(token)
            elif abs(digits_end - columns["saldo_end"]) <= _MONEY_TOLERANCE and saldo is None:
                saldo = parse_money(token)
            else:
                return None
        elif _DATE_RE.match(token) and druck is None:
            druck = _parse_day(token)
        else:
            art.append(token)
    if umsatz is None:
        return None
    return umsatz, saldo, druck, " ".join(art)


def _repair_split_labels(block: list[tuple[int, str]]) -> list[tuple[int, str]]:
    """Re-join a label the fixed-width text record split across two lines
    ("… EUR 1,00Auftrag" / "geber: …" → "… EUR 1,00" / "Auftraggeber: …")."""
    block = list(block)
    for i in range(1, len(block)):
        indent, text = block[i]
        m = _FRAGMENT_RE.match(text)
        if not m:
            continue
        frag = m.group(1)
        prev_indent, prev = block[i - 1]
        for label in _KNOWN_LABELS:
            head = label[:-len(frag)]
            if label.endswith(frag) and head and prev.endswith(head):
                block[i - 1] = (prev_indent, prev[:-len(head)].rstrip())
                block[i] = (indent, label + text[len(frag):])
                break
    return [(indent, text) for indent, text in block if text]


def _fields(block: list[tuple[int, str]]) -> tuple:
    """Group a posting's continuation lines: a `Label:` line at the block's
    base indent opens a field, deeper-indented lines continue the field above
    (wrapped addresses and purposes), and other base-indent lines are
    unlabelled booking text."""
    block = _repair_split_labels(block)
    if not block:
        return ()
    base = min(indent for indent, _ in block)
    fields: list[list] = []
    for indent, text in block:
        m = _LABEL_RE.match(text) if indent == base else None
        if m:
            fields.append([m.group(1), [m.group(2) or ""]])
        elif indent > base and fields:
            fields[-1][1].append(text)
        else:
            fields.append([None, [text]])
    return tuple((label, tuple(line for line in lines if line) if label is None
                  else tuple(lines)) for label, lines in fields)


_HEADER_KEYS = ("printed_at", "account_number", "coverage_start", "selection",
                "shows_balance", "shows_text")


def parse_listing(text: str) -> Listing:
    """Parse a listing's `pdftotext -layout` text. Raises ListingError when
    the layout cannot be read — pages that disagree on the header or are
    missing from the sequence, a date that names no day, a row before the
    first booking day. A line that fits no row's columns is read as
    continuation text; `problems` refuses the listing if that lost a real
    row's amount."""
    pages = [p for p in text.split("\f") if p.strip()]
    if not pages:
        raise ListingError("no pages")
    headers = []
    rows: list[dict] = []
    totals: dict[str, int] = {}
    butag = None
    current = None
    for page_no, page in enumerate(pages, 1):
        lines = page.split("\n")
        h = _read_header(lines)
        if h.columns is None:
            raise ListingError(f"page {page_no} has no column header")
        headers.append(h)
        for line in lines:
            if (t := _TOTAL_RE.match(line)):
                totals[t.group(1)] = abs(parse_money(t.group(2)))
                continue
            # A row is what fits every column: the code field, UMSATZ, and
            # any other amount in SALDO. Anything else is continuation text —
            # a purpose line can carry a 'dd.mm' too. A real row misread here
            # loses its amount, which the balance chain then refuses.
            m = _ROW_RE.match(line)
            codes = _split_codes(m, h.columns) if m else None
            tail = _read_tail(line, m, h.columns) if codes else None
            if tail:
                an, prnr, herk, txt = codes
                umsatz, saldo, druck, art = tail
                if m.group("butag").strip():
                    butag = _parse_day(m.group("butag"))
                elif butag is None:
                    raise ListingError("a row before the first booking day")
                try:
                    value_date(butag, m.group("val"))
                except ValueError as exc:
                    raise ListingError(f"invalid value date on {butag}") from exc
                current = {"butag": butag, "an": an, "prnr": prnr, "herk": herk,
                           "txt": txt, "val": m.group("val"),
                           "amount_cents": umsatz, "saldo_cents": saldo,
                           "druckdatum": druck, "art": art, "block": [],
                           "page": page_no}
                rows.append(current)
            elif not _is_furniture(line):
                if current is None:
                    raise ListingError("text before the first posting")
                current["block"].append(
                    (len(line) - len(line.lstrip()), line.strip()))

    first = headers[0]
    for name in _HEADER_KEYS:
        values = {getattr(h, name) for h in headers}
        if len(values) != 1 or None in values:
            raise ListingError(f"pages disagree on, or lack, the header's {name}")
    if [h.page_no for h in headers] != list(range(1, len(headers) + 1)):
        raise ListingError("pages missing or out of order")
    if any(h.account_line_number not in (None, first.account_number)
           for h in headers):
        raise ListingError("the account line names a different account")
    currencies = {h.currency for h in headers if h.currency}
    currencies |= set().union(*(h.units for h in headers))
    if len(currencies) != 1:
        raise ListingError("pages disagree on, or lack, the account currency")
    key_texts = {"; ".join(h.key_text) for h in headers}
    if len(key_texts) != 1:
        raise ListingError("pages disagree on the header's Schlüsseltext")

    postings = tuple(
        Posting(butag=r["butag"], an=r["an"], prnr=r["prnr"], herk=r["herk"],
                txt=r["txt"], val=r["val"], amount_cents=r["amount_cents"],
                saldo_cents=r["saldo_cents"], druckdatum=r["druckdatum"],
                art=r["art"], fields=_fields(r["block"]), page=r["page"])
        for r in rows)
    return Listing(
        account_number=first.account_number,
        currency=currencies.pop(),
        coverage_start=first.coverage_start,
        printed_at=first.printed_at,
        selection=first.selection,
        key_text=key_texts.pop(),
        shows_balance=first.shows_balance,
        shows_text=first.shows_text,
        postings=postings,
        stated_debits_cents=totals.get("Soll"),
        stated_credits_cents=totals.get("Haben"),
    )


# ============================================================
# A listing against its own arithmetic
# ============================================================

def problems(listing: Listing) -> list[str]:
    """Why a listing cannot be trusted on its own — empty when it can. Checks
    that it selected every posting with balances shown, that its days are in
    order inside its declared coverage, that every day's printed closing
    balance follows from the day before (the chain), and that its postings
    net to its stated totals."""
    out = []
    if listing.selection != "A":
        out.append("not a selection of all postings (Auswahl Umsätze)")
    if listing.key_text != "alle Umsätze":
        out.append("filtered by text key (Schlüsseltext)")
    if not listing.shows_balance:
        out.append("printed without closing balances (Saldo anzeigen: N)")
        return out
    postings = listing.postings
    if postings:
        if postings[0].butag < listing.coverage_start:
            out.append("postings before the declared coverage start")
        if postings[-1].butag > listing.print_day:
            out.append("postings after the print day")
    days: list[list[Posting]] = []
    for p in postings:
        if days and p.butag == days[-1][0].butag:
            days[-1].append(p)
        elif days and p.butag < days[-1][0].butag:
            out.append(f"booking days out of order at {p.butag}")
            return out
        else:
            days.append([p])
    previous = None
    for day in days:
        printed = [p for p in day if p.saldo_cents is not None]
        if len(printed) != 1 or day[-1].saldo_cents is None:
            out.append(f"no single closing balance on {day[0].butag}")
            return out
        if previous is not None and previous + sum(
                p.amount_cents for p in day) != day[-1].saldo_cents:
            out.append(f"the balance chain breaks on {day[0].butag} "
                       f"(page {day[0].page})")
            return out
        previous = day[-1].saldo_cents
    out.extend(_totals_problems(listing))
    return out


def _totals_problems(listing: Listing) -> list[str]:
    """The stated totals net out reversal pairs (an original debit and its
    same-day reversal credit are both left out), so the net identity is exact
    while each gross side may exceed its stated total — by the same amount on
    both sides, and by no more than same-day equal-and-opposite pairs can
    account for."""
    if listing.stated_debits_cents is None or listing.stated_credits_cents is None:
        return ["no stated totals (Summe Soll / Summe Haben) — truncated?"]
    debits = -sum(p.amount_cents for p in listing.postings if p.amount_cents < 0)
    credits = sum(p.amount_cents for p in listing.postings if p.amount_cents > 0)
    stated_net = listing.stated_credits_cents - listing.stated_debits_cents
    if credits - debits != stated_net:
        return ["postings do not net to the stated totals"]
    residual = debits - listing.stated_debits_cents
    if residual < 0:
        return ["postings fall short of the stated totals"]
    if residual:
        per_day: dict[tuple[date, int], list[int]] = {}
        for p in listing.postings:
            side = per_day.setdefault((p.butag, abs(p.amount_cents)), [0, 0])
            side[p.amount_cents > 0] += 1
        pairable = sum(amount * min(sides)
                       for (_, amount), sides in per_day.items())
        if residual > pairable:
            return ["gross totals differ from the stated totals by more than "
                    "same-day reversals explain"]
    return []


# ============================================================
# Ids
# ============================================================

def occurrences(postings: Sequence[Posting]) -> list[int]:
    """For each posting, how many earlier postings of the same day share its
    structural key — the same direct debit charged twice on one day prints
    identically."""
    seen: Counter = Counter()
    out = []
    for p in postings:
        key = (p.butag, p.an, p.prnr, p.herk, p.txt, p.val, p.amount_cents)
        out.append(seen[key])
        seen[key] += 1
    return out


def txn_id(iban: str, posting: Posting, occurrence: int) -> str:
    """A listing posting's stable id. Built from the structural columns only —
    never the parsed text (a parser change would re-key rows) and never
    DRUCKDATUM (a later extract fills it in) — so every extract of one
    account gives a posting the same id."""
    basis = "|".join([iban, posting.butag.isoformat(), posting.an,
                      posting.prnr, posting.herk, posting.txt,
                      posting.valuta.isoformat(), str(posting.amount_cents),
                      str(occurrence)])
    return "doc_" + hashlib.sha256(basis.encode("utf-8")).hexdigest()[:24]
