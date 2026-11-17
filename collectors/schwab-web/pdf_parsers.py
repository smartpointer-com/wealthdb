"""
Parsers for Schwab brokerage statement PDFs: the period, activity rows,
holdings and their tax lots, the cash summary, the account value, the
registration label and the account number; and the 3rd-Party-Distribution
letters.

Statements are this collector's only position history, and the only
record of a closed account's transactions: a closed account disappears
from the Transaction History page, while its archived statements stay
fetchable via download.py as long as an open account shares the login.
For an active account's transactions the Transaction History page is
the authoritative source; the statement rows serve cross-checks and
backfill.

PDF text extraction goes through pypdfium2 (Python bindings to
Google's PDFium, the renderer Chrome uses), which does the
text-layout work in C++ and so stays fast across hundreds of
statements.

Each document kind reads an extracted document in a module of its own:
`stmt_parse` the statement (with `stmt_header`, `stmt_tokens` and one
module per layout: `stmt_2025`, `stmt_legacy`, `stmt_2017`), and
`letters` the distribution letters. This module holds the wrappers
the loader and `dedup` call: each takes a PDF's path, extracts it
(`extraction`) and parses it.
"""

from __future__ import annotations

import extraction
import letters
import stmt_header
import stmt_parse


# ============================================================
# Convenience: open + parse a file path
# ============================================================

def parse_statement_pdf(path, statement_year: int | None = None) -> dict:
    """Open a Schwab brokerage statement PDF and return its path and
    what `stmt_parse.parse` reads from it: the statement period,
    transactions, positions, cash summary, account value, registration
    label and account number.

    `statement_year` dates the MM/DD activity rows of a statement
    whose period header does not read (parse_transactions); a printed
    period wins over it. The loader passes the year of the manifest
    doc-date.
    """
    extracted = extraction.pdfium(path)
    return {"path": str(path),
            **stmt_parse.parse(extracted, statement_year=statement_year)}


def parse_statement_account_number(path) -> str | None:
    """Header-only variant of parse_statement_pdf: extract the
    document text and return just the full account number. The
    loader's reconcile pass uses this to re-read statement
    headers from bronze — for rows ingested before the
    account-number parser existed, and to re-verify a freshly
    set key against every statement — without re-running the
    row parsers."""
    return stmt_header.parse_account_number(extraction.pdfium(path).text())


def parse_distribution_pdf(path) -> list[dict]:
    """Open a 3rd-Party-Distribution PDF and return normalised silver
    rows. Thin wrapper over `letters.parse` so the row logic is
    unit-testable without a real PDF."""
    name = str(path).rsplit("/", 1)[-1]
    return letters.parse(extraction.pdfium(path), filename=name)


# ============================================================
# CLI for standalone use
# ============================================================

if __name__ == "__main__":
    import sys

    from collectorkit import parser_cli
    sys.exit(parser_cli.dump_json(
        sys.argv[1:], parse_statement_pdf,
        description="Extract transactions from one or more Schwab "
                    "brokerage statement PDFs and emit JSON."))
