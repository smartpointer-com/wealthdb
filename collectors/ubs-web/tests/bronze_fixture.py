"""A small synthetic bronze for the loader's end-to-end tests: three runs
that touch every pass a load runs.

* Run one: a positions export with a holding and a mortgage, a cash
  export, and an archive of five PDFs: a statement of assets valued in
  USD (a holding and a precious-metals overview total), a CHF account
  statement whose ledger carries a mandate-funding credit and a payment,
  a credit advice for a mandate transfer, a debit advice for a payment,
  and a maturity notice.
* Run two: the cash export again with one more booking, and a second
  copy of the statement of assets, valued in CHF.
* Run three: a `--only` pass of the cash export (status `partial`).

Every identifier, amount and date is invented; the year is one the
bank cannot have booked in (AGENTS.md §4).
"""
from __future__ import annotations

import json
from pathlib import Path

import load as loader
from collectorkit import testpdf

STMT_IBAN = "CH00 0000 0000 0000 0000 1"
EXPORT_IBAN = "CH00 0000 0000 0000 00AA A"
PAYEE_IBAN = "CH00 0000 0000 0000 0000 9"

# The PDFs' text: Courier at 6 points on an A4 page.
_SIZE = 6


def _pdf(words: list[tuple[float, float, str]]) -> bytes:
    """A one-page PDF drawing each (x, top, text), its left edge at x and
    its line `top` points below the page's top edge."""
    return testpdf.from_top([[(x, top + _SIZE, t) for x, top, t in words]],
                            size=_SIZE, font="Courier", page=testpdf.A4)


def _lines_pdf(lines: list[str]) -> bytes:
    """A one-page PDF printing `lines`, one under the other."""
    return _pdf([(20.0, 20.0 + 10 * i, line) for i, line in enumerate(lines)])


SOA_LABEL = ("Statement of assets as of 31102098 02.11.2098 02 November 2098 "
             "P. Placeholder 999-00000000-06 300 KB")


def _soa(base_ccy: str, market_value: str) -> bytes:
    return _lines_pdf([
        f"Valued in {base_ccy}",
        "Portfolio 01",
        "Liquidity 11 111 11 111 22.58",
        "Precious metals & commodities 987 654 987 654 75.00",
        "Net assets 1 245 678",
        "Detailed positions",
        f"100 Reg.shs Placeholder Equity AG USD 10.000000 12.50 25.00% "
        f"{market_value} 5.00",
        "Financials",
        "Valor 111 - ISIN XX0000000011",
        "Additional information Abbreviations",
    ])


# The ledger's columns, by the x of each header word's left edge, and
# the x a figure in that column is centred on.
_HEADER = (("Date", 42), ("Information", 85), ("Debits", 302),
           ("Credits", 384), ("Value", 430), ("date", 457), ("Balance", 520))
_CENTRE = {"debit": 315.8, "credit": 396.6, "value": 439.0, "balance": 532.6}


def _at(column: str, text: str, top: float) -> tuple[float, float, str]:
    return (_CENTRE[column] - len(text) * 1.8, top, text)


def _info(text: str, top: float) -> list[tuple[float, float, str]]:
    out, x = [], 85.0
    for word in text.split():
        out.append((x, top, word))
        x += len(word) * 3.6 + 4
    return out


def _account_statement() -> bytes:
    words = [(20.0, 20.0, "UBS personal account CHF"),
             (20.0, 30.0, f"IBAN {STMT_IBAN}"),
             (20.0, 40.0, "Account Statement"),
             (20.0, 50.0, "01.10.2098 - 31.10.2098 / Monthly: number 10")]
    words += [(x, 100.0, label) for label, x in _HEADER]
    words += [(42.0, 120.0, "01.10.98"), *_info("Opening balance", 120.0),
              _at("balance", "500.00", 120.0)]
    words += [(42.0, 140.0, "05.10.98"), *_info("CREDIT", 140.0),
              _at("credit", "1000.00", 140.0), _at("value", "05.10.98", 140.0),
              _at("balance", "1500.00", 140.0)]
    words += [(85.0, 155.0, "UEBERTRAG EXAMPLE MANDATE")]
    words += [(42.0, 175.0, "10.10.98"), *_info("E-BANKING PAYMENT ORDER", 175.0),
              _at("debit", "200.00", 175.0), _at("value", "10.10.98", 175.0),
              _at("balance", "1300.00", 175.0)]
    words += [(85.0, 190.0, "EXAMPLE SHOP"), (85.0, 205.0, PAYEE_IBAN)]
    words += [(42.0, 225.0, "31.10.98"), *_info("Closing balance", 225.0),
              _at("balance", "1300.00", 225.0)]
    return _pdf(words)


def _advice(direction: str, iban: str, trx: str, purpose: str,
            amount: str) -> bytes:
    return _lines_pdf([
        "Cash Account for investment solutions CHF",
        f"IBAN {iban}",
        f"{direction} Advice",
        "Produced on 3 October 2098",
        f"TRX-No. {trx} ABC/ABC",
        "Bookkeeping entry date 2 October 2098",
        "Details of payment",
        purpose,
        "Currency Amount",
        f"Total amount CHF {amount}",
        "Val. 02.10.2098",
    ])


_MATURITY_NOTICE = _lines_pdf([
    "UBS Fixed-Rate Mortgage CHF",
    "Account no. 999-12345678.MMM 0001",
    "Category EXAMPLE ROAD 1, 0000 EXAMPLECITY",
    "MMaattuurriittyy nnoottiiccee",
    "AAss aatt 3300..0099..22009988",
    "General information Amount in CHF",
    "Current debt capital 1 234 567.89",
])

_RUN_ONE_DOCUMENTS = (
    ("soa-usd.pdf", "tok-soa-usd", SOA_LABEL, _soa("USD", "1 250")),
    ("stmt.pdf", "tok-stmt",
     "Account Statement 31.10.2098 02 November 2098 P. Placeholder",
     _account_statement()),
    ("adv-credit.pdf", "tok-adv-credit",
     "Credit Advice 02.10.2098 03 October 2098 P. Placeholder",
     _advice("Credit", EXPORT_IBAN, "0000 036 ED 0000123",
             "Increase Example Mandate", "12 345.67")),
    ("adv-debit.pdf", "tok-adv-debit",
     "Debit Advice 02.10.2098 03 October 2098 P. Placeholder",
     _advice("Debit", EXPORT_IBAN, "0000 036 ED 0000456",
             "Example invoice 42", "321.00")),
    ("maturity.pdf", "tok-maturity",
     "Maturity notice 30.09.2098 01 October 2098 P. Placeholder",
     _MATURITY_NOTICE),
)
_RUN_TWO_DOCUMENTS = (
    ("soa-chf.pdf", "tok-soa-chf", SOA_LABEL, _soa("CHF", "1 125")),
)

_EXPORT_HEADER = (
    "Trade date;Trade time;Booking date;Value date;Currency;"
    "Debit;Credit;Individual amount;Balance;Transaction no.;"
    "Description1;Description2;Description3;Footnotes;"
)
_EXPORT_ROWS = (
    "2098-11-03;;2098-11-03;2098-11-03;CHF;-50.00;;;950.00;"
    "AB00000XX0000001;\"Example Shop\";Debit card payment;;;",
    "2098-11-05;;2098-11-05;2098-11-05;CHF;;100.00;;1050.00;"
    "AB00000XX0000002;\"Example Employer\";Salary;;;",
    "2098-11-09;;2098-11-09;2098-11-09;CHF;-25.00;;;1025.00;"
    "AB00000XX0000003;\"Example Grocer\";Debit card payment;;;",
)


def _export(rows: tuple[str, ...]) -> str:
    head = "\r\n".join([
        "Account number:;0000 00000000.00;", f"IBAN:;{EXPORT_IBAN};",
        "From:;2098-11-01;", "To:;2098-12-31;", "Currency:;CHF;",
        "Valued in:;CHF;", ";", ";"])
    return "﻿" + head + "\r\n" + _EXPORT_HEADER + "\r\n" + \
        "\r\n".join(rows) + "\r\n"


def _positions() -> str:
    holding = {"Banking relationship": "1234 00000001",
               "Portfolio": "1234 00000001 R001", "Group of products": "Equities",
               "Product": "Share", "Ccy.": "CHF", "Number/Amt.": "10",
               "ISIN": "XX0000000022", "Description": "Example Share",
               "Market value": "1500.00"}
    mortgage = {"Banking relationship": "1234 00000001",
                "Portfolio": "1234 00000001 R001",
                "Group of products": "Pro memoria - Mortgages",
                "Product": "0999 12345678.MMM 0001", "Ccy.": "CHF",
                "Number/Amt.": "-1234567.89",
                "Description": "UBS Fixed-Rate Mortgage, EXAMPLE ROAD 1",
                "Description 1": "UBS Fixed-Rate Mortgage",
                "Description 3": "EXAMPLE ROAD 1, 0000 EXAMPLECITY",
                "IBAN": "01.01.2098 - 31.12.2099"}
    rows = [";".join(r.get(c, "") for c in loader.POSITIONS_COLS)
            for r in (holding, mortgage)]
    return "\r\n".join([";".join(loader.POSITIONS_COLS), *rows,
                        "Valued in: CHF"]) + "\r\n"


def _run(root: Path, slug: str, status: str, *, documents=(),
         export=(), positions=False) -> Path:
    run = root / slug
    run.mkdir(parents=True)
    meta: dict = {"status": status,
                  "window": {"since": "2098-11-01", "until": "2098-12-31"}}
    if documents:
        (run / "documents").mkdir()
        for name, _token, _label, data in documents:
            (run / "documents" / name).write_bytes(data)
        meta["documents"] = {"items": [
            {"filename": name, "token": token, "label": label}
            for name, token, label, _ in documents]}
    if export:
        (run / "transactions").mkdir()
        (run / "transactions" / "cash_a.csv").write_text(
            _export(export), encoding="utf-8")
    if positions:
        (run / "positions").mkdir()
        (run / "positions" / "positions_a.csv").write_text(
            _positions(), encoding="utf-8")
    (run / "run.json").write_text(json.dumps(meta), encoding="utf-8")
    return run


def write_runs(root: Path) -> list[Path]:
    """The three runs under `root`, oldest first."""
    return [
        _run(root, "20981110T000000Z", "complete", documents=_RUN_ONE_DOCUMENTS,
             export=_EXPORT_ROWS[:2], positions=True),
        _run(root, "20981120T000000Z", "complete", documents=_RUN_TWO_DOCUMENTS,
             export=_EXPORT_ROWS, positions=True),
        _run(root, "20981130T000000Z", "partial", export=_EXPORT_ROWS[1:]),
    ]
