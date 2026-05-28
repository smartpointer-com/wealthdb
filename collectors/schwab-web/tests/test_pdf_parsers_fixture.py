"""
Fixture-driven sanity test for pdf_parsers against a real
Schwab brokerage statement PDF.

The fixture path is intentionally outside the repo
(samples/ is gitignored) so no real account data lives in
version control. To enable the test locally, symlink or copy a
single PDF to either:

  - ./tests/fixtures/sample-statement.pdf  (inside the repo, gitignored)
  - $SCHWAB_TEST_PDF                       (env var pointing anywhere)

When neither is present, the test auto-skips.

This is a coarse smoke test (counts + plausibility bounds), not a
golden-output assert — the assertions deliberately avoid embedding
any per-account specifics so the test remains stable across
different statement months and different user accounts.
"""

from __future__ import annotations

import os
import sys
from datetime import date
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import pdf_parsers as pp  # noqa: E402


def _fixture_path() -> Path | None:
    env = os.environ.get("SCHWAB_TEST_PDF")
    if env:
        p = Path(env)
        if p.is_file():
            return p
    local = Path(__file__).resolve().parent / "fixtures" / "sample-statement.pdf"
    if local.is_file():
        return local
    return None


pytestmark = pytest.mark.skipif(
    _fixture_path() is None,
    reason=(
        "no statement PDF fixture available — set $SCHWAB_TEST_PDF or "
        "drop a PDF at tests/fixtures/sample-statement.pdf"
    ),
)


@pytest.fixture(scope="module")
def parsed():
    """Parse the fixture PDF once per module."""
    pytest.importorskip("pdfplumber")
    path = _fixture_path()
    return pp.parse_statement_pdf(path)


def test_period_present(parsed):
    """The statement-period header is the year source for every
    transaction date; parsing it must succeed or the rest is junk."""
    assert parsed["period_start"] is not None
    assert parsed["period_end"] is not None
    start = date.fromisoformat(parsed["period_start"])
    end = date.fromisoformat(parsed["period_end"])
    # A Schwab monthly statement spans at most ~31 days.
    assert 25 <= (end - start).days <= 35
    # Sanity: not from a fake year.
    assert 2000 <= start.year <= 2100


def test_extracts_some_transactions(parsed):
    """A statement with zero parsed transactions almost certainly
    means the section header / row regex drifted out from under us."""
    assert len(parsed["transactions"]) > 0


def test_every_transaction_has_a_date_in_period(parsed):
    """If date inheritance or year-attachment regresses, dates
    drift out of the statement period."""
    if not parsed["transactions"]:
        pytest.skip("no transactions to validate")
    start = date.fromisoformat(parsed["period_start"])
    end = date.fromisoformat(parsed["period_end"])
    for tx in parsed["transactions"]:
        d_str = tx.get("date")
        assert d_str, f"transaction has no date: {tx}"
        d = date.fromisoformat(d_str)
        # Schwab sometimes back-dates pending settlements; give a
        # small margin on each side. Stay tight enough to catch
        # year-drift bugs (where we'd see dates a year off).
        assert (start.replace(day=1).replace(month=max(1, start.month - 1))
                <= d
                <= end.replace(day=min(28, end.day)) + (end - start)), (
            f"transaction date {d} outside reasonable window for "
            f"period {start}..{end}: {tx}"
        )


def test_every_transaction_has_amount(parsed):
    """An amount-less transaction is a parser miss in every Schwab
    statement format we've seen."""
    if not parsed["transactions"]:
        pytest.skip("no transactions to validate")
    no_amount = [tx for tx in parsed["transactions"] if tx.get("amount") is None]
    assert not no_amount, f"{len(no_amount)} transactions with no amount"


def test_every_transaction_has_category(parsed):
    """Categories drive downstream classification in load.py; a
    None category means the leading-token regex didn't match."""
    if not parsed["transactions"]:
        pytest.skip("no transactions to validate")
    no_cat = [tx for tx in parsed["transactions"] if not tx.get("category")]
    assert not no_cat, f"{len(no_cat)} transactions with no category"


def test_categories_in_known_set(parsed):
    """Catches a new category Schwab introduces — we'd want to know
    and update _CATEGORY_KEYWORDS."""
    if not parsed["transactions"]:
        pytest.skip("no transactions to validate")
    known = set(pp._CATEGORY_KEYWORDS)
    seen = {tx["category"] for tx in parsed["transactions"] if tx.get("category")}
    unknown = seen - known
    assert not unknown, (
        f"transactions in unknown categories {unknown} — extend "
        f"_CATEGORY_KEYWORDS"
    )


def test_realized_gains_have_term(parsed):
    """Sales with realized_gain_loss must have ST or LT — that
    tag drives tax treatment downstream."""
    if not parsed["transactions"]:
        pytest.skip("no transactions to validate")
    bad = [
        tx for tx in parsed["transactions"]
        if tx.get("realized_gain_loss") is not None and tx.get("term") not in ("ST", "LT")
    ]
    assert not bad, f"{len(bad)} realized-gain rows missing ST/LT tag"


def test_sales_have_negative_quantity(parsed):
    """Sells render qty in parens (negative); buys positive. A
    Sale with positive qty indicates a sign-handling bug."""
    if not parsed["transactions"]:
        pytest.skip("no transactions to validate")
    bad = [
        tx for tx in parsed["transactions"]
        if tx.get("category") == "Sale"
        and tx.get("quantity") is not None
        and tx.get("quantity") > 0
    ]
    assert not bad, f"{len(bad)} Sale rows with positive quantity"


def test_purchases_have_positive_quantity(parsed):
    """Counterpart of test_sales_have_negative_quantity."""
    if not parsed["transactions"]:
        pytest.skip("no transactions to validate")
    bad = [
        tx for tx in parsed["transactions"]
        if tx.get("category") == "Purchase"
        and tx.get("quantity") is not None
        and tx.get("quantity") < 0
    ]
    assert not bad, f"{len(bad)} Purchase rows with negative quantity"
