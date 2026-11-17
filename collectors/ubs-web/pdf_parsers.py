"""PDF readers for the UBS Switzerland statement archive.

Each document kind is read by a module of its own, from the document as
pdfplumber extracted it (`collectorkit.words.Extracted`):

  soa_positions.parse(extracted, doc_token, label)
      → (position snapshots, transaction-list bookings, overview totals)
        from one Statement of assets (with soa_transaction_list).

  account_statement.parse(extracted, doc_token)
      → (cash-balance rows, movement rows) from one Account Statement.

  maturity_notice.parse / payment_advice.parse
      → a mortgage's outstanding principal; a payment's movement row.

  securities_advice.capital_call / securities_advice.contract_note
      → the price a holding was bought at, for the `advices` table.

  quarterly_report.parse
      → a private-markets quarterly report and its investment lines.

Each returns plain dicts ready for the loader to insert into silver. A
reader imports `collectorkit.words` and pdf_common.py and nothing that
extracts, so its parser fingerprint is its own code. This module is the
other half, and the names the loader and the tests use: `EXTRACTOR`,
the one extraction every reader reads, and a `parse_*(path, ...)`
wrapper per kind that extracts a document (from the extraction cache
when the loader passes one) and reads it.

Parsing strategy: PDF tables in UBS statements are not real PDF
tables (no row / column structure for pdfplumber to detect — see
the empty `extract_tables()` output during probing). They are
visually-aligned text columns. Two tables tell their columns apart
only by position, and are read from word positions instead: the
Account-Statement movement ledger and a Statement of assets'
transaction list (see their modules). Everything else is read from
the page text, line by line, anchoring on identifiable markers:

  - "Statement of assets as of <DDMMYYYY>" in the label → as_of_date,
    or the document's own "As of <D Month YYYY>" header where there is
    no listing row to carry a label
  - "Portfolio number BBB-AAAAAAAA-NN" in body → portfolio number
  - "Valued in <CCY>" header → portfolio base currency
  - "Valor <num> - ISIN <code>" line → securities position anchor
  - IBAN-shaped line → cash position anchor
  - "Account Statement / DD.MM.YYYY - DD.MM.YYYY" → period range
  - "Opening balance / Closing balance" lines → cash deltas

Performance note: pypdfium2 extracts text about 10× faster than
pdfplumber, and schwab-web reads its statements with it. ubs-web stays
on pdfplumber because the visual-line reconstruction PDFium needs
(count_rects() returns either cell-level granularity that splits a row
across many lines, or column-shared-baseline rects that merge columns
that pdfplumber keeps apart) cannot reproduce the line layout these
parsers read. The Account-Statement balance summary's squish-and-match
reading does round-trip byte-identical under pypdfium2, so that one
parser could move on its own.
"""
from __future__ import annotations

import functools
from pathlib import Path

from collectorkit import extract
from collectorkit.words import Extracted

import account_statement
import maturity_notice
import payment_advice
import quarterly_report
import securities_advice
import soa_positions
from account_statement import (
    _stmt_security, _stmt_split_multi, parse_account_statement_text,
)
from maturity_notice import parse_maturity_notice_text
from payment_advice import _advice_trx_no, parse_payment_advice_text
from pdf_common import document_text
from quarterly_report import parse_quarterly_report_text
from securities_advice import parse_capital_call_text, parse_contract_note_text
from soa_positions import (
    parse_label_statement_of_assets, parse_statement_of_assets_text,
    psn_portfolio_external_id, statement_of_assets_body_meta,
)
from soa_transaction_list import parse_transaction_list

__all__ = [
    "EXTRACTOR", "parse_account_statement_combined",
    "parse_account_statement_text",
    "parse_account_statement_transactions_pages", "parse_capital_call",
    "parse_capital_call_text", "parse_contract_note",
    "parse_contract_note_text", "parse_label_statement_of_assets",
    "parse_maturity_notice", "parse_maturity_notice_text",
    "parse_payment_advice", "parse_payment_advice_text",
    "parse_quarterly_report", "parse_quarterly_report_text",
    "parse_statement_of_assets", "parse_statement_of_assets_pages",
    "parse_statement_of_assets_text", "parse_transaction_list",
    "psn_portfolio_external_id", "statement_of_assets_body_meta",
    "statement_of_assets_text",
    "_advice_trx_no", "_stmt_security", "_stmt_split_multi",
]

# Every page's text, laid out as every text-walking reader reads it, and
# the two word sets the two word-position readers read: the transaction
# list's (`x_tolerance=2`) and the ledger's.
EXTRACTOR = functools.partial(
    extract.pdfplumber, text_params={"x_tolerance": 2},
    word_params=({"x_tolerance": 2},
                 {"x_tolerance": 1.5, "keep_blank_chars": False}))

# The readers that take an extracted document, under the names the tests
# and the loader know.
parse_statement_of_assets_pages = soa_positions.parse
parse_account_statement_transactions_pages = account_statement.movements


def extracted(pdf_path: Path, *, sha: str | None = None,
              cache_dir: Path | None = None) -> Extracted:
    """A document as `EXTRACTOR` reads it. With a `cache_dir`, from the
    extraction cache there, keyed on `sha`, the document's sha256."""
    if cache_dir is None or sha is None:
        return EXTRACTOR(pdf_path)
    return extract.cached(EXTRACTOR, pdf_path, sha, cache_dir)


def statement_of_assets_text(pdf_path: Path, **cache) -> str:
    """The text a Statement-of-assets walk reads. Separate from the walk
    so a caller that only needs to know WHICH document this is can ask
    without parsing its positions."""
    return document_text(extracted(pdf_path, **cache))


def parse_statement_of_assets(pdf_path: Path, doc_token: str, label: str,
                              **cache) -> tuple[list[dict], list[dict],
                                                list[dict]]:
    """`soa_positions.parse` of one Statement-of-assets PDF:
    (positions, trades, overview totals)."""
    return soa_positions.parse(extracted(pdf_path, **cache), doc_token, label)


def parse_account_statement_combined(pdf_path: Path, doc_token: str,
                                     **cache) -> tuple[list[dict], list[dict]]:
    """`account_statement.parse` of one Account-Statement PDF:
    (cash-balance rows, movement rows)."""
    return account_statement.parse(extracted(pdf_path, **cache), doc_token)


def parse_maturity_notice(pdf_path: Path, doc_token: str,
                          **cache) -> list[dict]:
    """`maturity_notice.parse` of one Maturity-notice PDF."""
    return maturity_notice.parse(extracted(pdf_path, **cache), doc_token)


def parse_payment_advice(pdf_path: Path, doc_token: str,
                         **cache) -> list[dict]:
    """`payment_advice.parse` of one Credit/Debit Advice PDF."""
    return payment_advice.parse(extracted(pdf_path, **cache), doc_token)


def parse_capital_call(pdf_path: Path, doc_token: str, **cache) -> list[dict]:
    """`securities_advice.capital_call` of one Private Market Letter."""
    return securities_advice.capital_call(extracted(pdf_path, **cache),
                                          doc_token)


def parse_contract_note(pdf_path: Path, doc_token: str,
                        **cache) -> list[dict]:
    """`securities_advice.contract_note` of one contract-note PDF."""
    return securities_advice.contract_note(extracted(pdf_path, **cache),
                                           doc_token)


def parse_quarterly_report(pdf_path: Path, doc_token: str,
                           **cache) -> tuple[list[dict], list[dict]]:
    """`quarterly_report.parse` of one Private Market Letter."""
    return quarterly_report.parse(extracted(pdf_path, **cache), doc_token)
