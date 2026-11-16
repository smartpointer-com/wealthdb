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
`letters` the distribution letters. This module keeps the names the
loader and the tests import, and the wrappers that take a PDF's path,
extract it (`extraction`) and parse it.
"""

from __future__ import annotations

import extraction
import letters
import stmt_parse
from letters import parse_distribution_text  # noqa: F401
from stmt_2025 import _CATEGORY_KEYWORDS, _NEW_TX_ROW_STOP_RE  # noqa: F401
from stmt_header import (  # noqa: F401
    parse_account_number,
    parse_account_registration,
    parse_account_value,
    parse_statement_period,
)
from stmt_legacy import (  # noqa: F401
    _LEGACY_TX_ROW_STOP_RE,
    _parse_legacy_lot_line,
)
from stmt_parse import (  # noqa: F401
    detect_era,
    parse_cash_interest_rate,
    parse_cash_summary,
    parse_positions,
    parse_transactions,
)
from stmt_tokens import TransactionRow, _parse_number, _row_date  # noqa: F401


# ============================================================
# Convenience: open + parse a file path
# ============================================================

def parse_statement_pdf(path, statement_year: int | None = None, *,
                        sha256: str | None = None, cache_dir=None) -> dict:
    """Open a Schwab brokerage statement PDF and return its path and
    what `stmt_parse.parse` reads from it: the statement period,
    transactions, positions, cash summary, account value, registration
    label and account number.

    `statement_year` dates the MM/DD activity rows of a statement
    whose period header does not read (parse_transactions); a printed
    period wins over it. The loader passes the year of the manifest
    doc-date. With the PDF's `sha256` and a `cache_dir` the extraction
    is cached by content.
    """
    extracted = extraction.pdfium(path, sha256, cache_dir)
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
    return parse_account_number(extraction.pdfium(path).text())


def parse_distribution_pdf(path, *, sha256: str | None = None,
                           cache_dir=None) -> list[dict]:
    """Open a 3rd-Party-Distribution PDF and return normalised silver
    rows. Thin wrapper over `parse_distribution_text` so the row logic
    is unit-testable without a real PDF."""
    name = str(path).rsplit("/", 1)[-1]
    return letters.parse(extraction.pdfium(path, sha256, cache_dir),
                         filename=name)


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
