"""Synthetic transaction listings for the tests — never derived from a real
one. `render` lays postings out the way the bank's listing prints them (the
header block per page, the drifting column ruler, BUTAG on a day's first row,
SALDO on its last, continuation lines under each row, a totals page), and
`Ledger` is a tiny account history the tests cut listings and live histories
from, so every source they build agrees with every other by construction.

Account numbers are all-zero-padded placeholders, the IBAN uses the
placeholder letters of root CLAUDE.md §4, and amounts are round.
"""
from __future__ import annotations

import sys
from collections import Counter
from dataclasses import dataclass, field
from datetime import date, datetime, timedelta
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import listing_parser  # noqa: E402
import stitch  # noqa: E402

ACCOUNT = "1234"
IBAN = "ATkkBBBBB00000001234"          # ends in the zero-padded account number
DAY = timedelta(days=1)

# Base columns of the data row, from the listing layout: AN right-aligned to
# 15, PRNR from 16, HERK from 20, TXT right-aligned to 27, VAL. from 28; the
# UMSATZ digits end at 52 and the SALDO digits at 69 (a debit's minus follows
# in the next column); DRUCKDATUM from 70.
_HEADER = [("BUTAG", 1, False), ("AN", 13, False), ("PRNR", 16, False),
           ("HERK", 21, False), ("TXT", 26, False), ("VAL.", 30, False),
           ("UMSATZ", 46, True), ("SALDO", 64, True), ("DRUCKDATUM", 70, True),
           ("ART", 81, True)]


def money(cents: int) -> str:
    whole, frac = divmod(abs(cents), 100)
    text = f"{whole:,}".replace(",", ".") + f",{frac:02d}"
    return text + ("-" if cents < 0 else "")


@dataclass
class Posting:
    day: date
    amount: int                                   # cents, debits negative
    lines: list = field(default_factory=lambda: [(7, "Verwendungszweck: ITEM")])
    herk: str = "SELE"
    an: str = "1"
    prnr: str = "100"
    txt: str = "10"
    val: str | None = None                        # 'dd.mm'; default the day
    druck: date | None = None                     # statement print day


def _place(buf: list, col: int, text: str) -> None:
    for i, ch in enumerate(text):
        while len(buf) <= col + i:
            buf.append(" ")
        buf[col + i] = ch


def _header_line(code_shift: int, money_shift: int) -> str:
    buf: list = []
    for name, col, right in _HEADER:
        shift = money_shift if right else (code_shift if name != "BUTAG" else 0)
        _place(buf, col + shift, name)
    return "".join(buf)


def _row(p: Posting, first_of_day: bool, saldo: int | None, code_shift: int,
         money_shift: int, show_balance: bool) -> str:
    buf: list = [" "]
    _place(buf, 1, p.day.strftime("%d.%m.%Y") if first_of_day else " " * 10)
    c, m = code_shift, money_shift
    _place(buf, 15 + c - len(p.an), p.an)
    _place(buf, 16 + c, p.prnr)
    _place(buf, 20 + c, p.herk)
    _place(buf, 27 + c - len(p.txt), p.txt)
    _place(buf, 28 + c, p.val or p.day.strftime("%d.%m"))
    amount = money(p.amount)
    _place(buf, 52 + m - len(amount.rstrip("-")), amount)
    if saldo is not None and show_balance:
        text = money(saldo)
        _place(buf, 69 + m - len(text.rstrip("-")), text)
    if p.druck:
        _place(buf, 70 + m, p.druck.strftime("%d.%m.%Y"))
    return "".join(buf).rstrip()


def _page_header(page_no: int, *, account: str, coverage_start: date,
                 printed: datetime, selection: str, key_text: str,
                 show_balance: bool, show_text: bool, currency: str,
                 code_shift: int, money_shift: int) -> list[str]:
    acct = f"{int(account):,}".replace(",", ".")
    banner = (f"00000/Raiffeisenbank{' ' * 35}"
              f"{printed:%d.%m.%Y/%H:%M}/x0000xx Seite {page_no}")
    return [
        banner, "",
        f"Kontonummer:      {acct}    Ausgabe Datum ab: "
        f"{coverage_start:%d.%m.%y}    Auswahl Umsätze: {selection}",
        f"Schlüsseltext: {key_text}", "Schlüsseltext:",
        f"Saldo anzeigen J/N: {'J' if show_balance else 'N'}    Klartext anz. "
        f"J/N: {'J' if show_text else 'N'}    Respiro anz. J/N: N",
        "Rech.legung anz. J/N: N", "—" * 88, "",
        _header_line(code_shift, money_shift), "-" * 84,
        f"     {acct}/{currency}      TEST HOLDER        REF: P:0000/ M:0000/ "
        f"C:0000/ G:0000",
        f"{' ' * 41}KTO-WHG/{currency}      KTO-WHG/{currency}",
    ]


def render(postings: list[Posting], *, opening: int = 100_000,
           account: str = ACCOUNT, coverage_start: date | None = None,
           printed: datetime | None = None, per_page: int = 3,
           code_shifts: dict | None = None, money_shifts: dict | None = None,
           split_blocks: bool = False, units_mid_block: bool = False,
           totals: tuple[int, int] | None | bool = True,
           selection: str = "A", key_text: str = "alle Umsätze",
           show_balance: bool = True, show_text: bool = True,
           currency: str = "EUR", saldo_offsets: dict | None = None,
           page_numbers: list | None = None, accounts_per_page: dict | None = None) -> str:
    """Lay postings out as a listing. Balances chain from `opening`;
    `saldo_offsets` ({day: cents}) corrupts a day's printed balance.
    `split_blocks` carries each page's last continuation line onto the next
    page; `units_mid_block` prints the units banner inside a block."""
    postings = sorted(postings, key=lambda p: p.day)
    coverage_start = coverage_start or (postings[0].day if postings else date(2024, 1, 1))
    printed = printed or datetime.combine(
        (postings[-1].day if postings else coverage_start) + DAY, datetime.min.time()
    ).replace(hour=9, minute=30)
    code_shifts, money_shifts = code_shifts or {}, money_shifts or {}
    saldo_offsets = saldo_offsets or {}
    balance = opening
    rendered: list[tuple[Posting, bool, int | None]] = []
    for i, p in enumerate(postings):
        balance += p.amount
        first = i == 0 or postings[i - 1].day != p.day
        last = i == len(postings) - 1 or postings[i + 1].day != p.day
        saldo = balance + saldo_offsets.get(p.day, 0) if last else None
        rendered.append((p, first, saldo))
    chunks = [rendered[i:i + per_page] for i in range(0, len(rendered), per_page)] or [[]]
    pages: list[list[str]] = []
    carry: list = []
    for n, chunk in enumerate(chunks, 1):
        c, m = code_shifts.get(n, 0), money_shifts.get(n, 0)
        lines = _page_header(n, account=(accounts_per_page or {}).get(n, account),
                             coverage_start=coverage_start, printed=printed,
                             selection=selection, key_text=key_text,
                             show_balance=show_balance, show_text=show_text,
                             currency=currency, code_shift=c, money_shift=m)
        lines += [" " * indent + text for indent, text in carry]
        carry = []
        for k, (p, first, saldo) in enumerate(chunk):
            lines.append(_row(p, first, saldo, c, m, show_balance))
            block = list(p.lines) if show_text else []
            if split_blocks and k == len(chunk) - 1 and n < len(chunks) and len(block) > 1:
                carry, block = block[-1:], block[:-1]
            for j, (indent, text) in enumerate(block):
                if units_mid_block and j == 1:
                    lines.append(f"{' ' * 41}KTO-WHG/{currency}      KTO-WHG/{currency}")
                lines.append(" " * indent + text)
        pages.append(lines)
    if totals:
        if totals is True:
            debits = -sum(p.amount for p in postings if p.amount < 0)
            credits = sum(p.amount for p in postings if p.amount > 0)
        else:
            debits, credits = totals
        lines = _page_header(len(pages) + 1, account=account,
                             coverage_start=coverage_start, printed=printed,
                             selection=selection, key_text=key_text,
                             show_balance=show_balance, show_text=show_text,
                             currency=currency, code_shift=0, money_shift=0)
        lines += ["", f"             Summe Soll                {money(-debits)}",
                  f"             Summe Haben               {money(credits)}", ""]
        pages.append(lines)
    if page_numbers:
        for page, number in zip(pages, page_numbers):
            page[0] = page[0].rsplit("Seite", 1)[0] + f"Seite {number}"
    return "\f".join("\n".join(page) for page in pages) + "\f"


# ============================================================
# A ledger to cut listings and live histories from
# ============================================================

@dataclass
class Ledger:
    """One account's true history: an opening balance at the end of the day
    before `start`, and postings (cents) per booking day."""
    start: date
    opening: int
    postings: dict[date, list[int]]

    def balance_at(self, day: date) -> int:
        """Closing balance at the end of `day`."""
        return self.opening + sum(a for d, amounts in self.postings.items()
                                  if d <= day for a in amounts)

    def between(self, first: date, last: date) -> list[Posting]:
        return [Posting(d, a) for d in sorted(self.postings) if first <= d <= last
                for a in self.postings[d]]

    def listing_text(self, coverage_start: date, print_day: date, *,
                     hour: int = 9, **render_kw) -> str:
        postings = self.between(coverage_start, print_day - DAY)
        return render(postings, opening=self.balance_at(coverage_start - DAY),
                      coverage_start=coverage_start,
                      printed=datetime.combine(print_day, datetime.min.time()).replace(hour=hour),
                      **render_kw)

    def listing(self, coverage_start: date, print_day: date, **kw) -> listing_parser.Listing:
        return listing_parser.parse_listing(self.listing_text(coverage_start, print_day, **kw))

    def candidate(self, key: str, coverage_start: date, print_day: date, **kw) -> stitch.Candidate:
        return stitch.Candidate(key, key, self.listing(coverage_start, print_day, **kw))

    def live(self, runs: list[tuple[date, date]], lead: int = 1) -> stitch.LiveView:
        """A live history from downloads, each (window start, run day): the
        postings booked before the run day, and a balance series opening
        `lead` days before each window."""
        postings: dict[date, Counter] = {}
        balances: dict[date, int] = {}
        txn, bal = [], []
        for start, run_day in runs:
            txn.append(stitch.Window(start, run_day))
            von = start - lead * DAY
            bal.append(stitch.Window(von, run_day))
            balances[von] = self.balance_at(von)
            for d in sorted(self.postings):
                if start <= d < run_day:
                    postings[d] = Counter(self.postings[d])
                if von <= d < run_day:
                    balances[d] = self.balance_at(d)
        return stitch.LiveView(txn_windows=txn, balance_windows=bal,
                               postings=postings, balances=balances)


def ledger(start: date = date(2024, 1, 1), days: int = 180, every: int = 3,
           opening: int = 100_000) -> Ledger:
    """A deterministic history: a debit every `every` days and a credit on
    every fifth posting day, all round amounts."""
    out: dict[date, list[int]] = {}
    for i in range(0, days, every):
        d = start + i * DAY
        out[d] = [5_000] if (i // every) % 5 == 4 else [-1_000 - 100 * ((i // every) % 3)]
    return Ledger(start, opening, out)
