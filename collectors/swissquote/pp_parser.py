"""Swissquote Portfolio Performance PDF parser.

Each Portfolio Performance PDF is an annual snapshot. Its page-2
"Asset allocation" table lists every held position at year-end (date in
the title: "Portfolio performance at DD.MM.YYYY") with quantity, ISIN,
average cost, market price, and CHF valuation. load.py writes the parse
as a `pp:<doc_id>`-tagged set of `positions` rows, so the silver
`positions` table also covers historical year-ends rather than only the
cluster of recent live snapshots.

Cash rows are intentionally skipped — cash belongs in
`currency_balances` (silver). Securities-only here, matching the
`positions` table's existing scope.

A module of its own, so load.py can fingerprint it (collectorkit.srcfp)
as the generation of the statement rows it derives.
"""

from __future__ import annotations

import re
from pathlib import Path


_PP_ISIN_RE = re.compile(r"\b([A-Z]{2}[A-Z0-9]{10})\b")
_PP_DATE_RE = re.compile(r"\b(\d{2})\.(\d{2})\.(\d{4})\b")
_PP_TITLE_DATE_RE = re.compile(
    r"Portfolio performance at\s+(\d{2})\.(\d{2})\.(\d{4})"
)
_PP_ACCOUNT_RE = re.compile(r"Account(?:\s*number)?\s+(\d{6,10})")
# Section header on the asset-allocation page: "Bonds in CHF",
# "Funds in CHF", "Equities in CHF", "Metals in CHF", etc.
_PP_SECTION_RE = re.compile(
    r"^\s*(Bonds|Funds|Equities|Metals|Structured products|Options|Other)\s+in\s+([A-Z]{3})\s*$"
)


# A line holding nothing but one Swiss-locale amount: a bond's accrued
# interest under its row.
_PP_LONE_NUMBER_RE = re.compile(r"^-?[\d']+\.\d+$")


def _pp_to_number(s: str) -> float:
    """Parse a Swiss-locale number like "1'234'567.89" or "106.150%"."""
    return float(s.replace("'", "").rstrip("%").strip())


def parse_portfolio_performance(pdf_path: Path) -> dict:
    """Parse a Portfolio Performance PDF into a snapshot dict.

    Returns: {
        "snapshot_date":        "YYYY-MM-DD",       # as-of date from title
        "account_external_id":  "<customer id>",
        "positions":            [{
            "asset_class":    "Bonds" | "Funds" | ...,
            "currency":       "CHF",
            "name":           "<security description>",
            "isin":           "<ISIN>",
            "quantity":       <float>,
            "avg_price":      <float>,
            "market_price":   <float>,
            "price_quote":    "unit" | "percent",   # "%" on the prices
            "price_date":     "YYYY-MM-DD",
            "valuation_chf":  <float>,
            "account_pct":    <float>,
        }, ...]
    }

    Raises SystemExit on structural surprises (missing title, no
    asset-allocation page, unparseable row, etc.) — the goal is to
    fail loud rather than silently drop data.
    """
    import pypdf  # local import — only this code path needs it

    reader = pypdf.PdfReader(str(pdf_path))

    # Title + account ID can appear on any page; grep across all.
    snapshot_date = account_id = None
    for page in reader.pages:
        t = page.extract_text()
        if not snapshot_date:
            m = _PP_TITLE_DATE_RE.search(t)
            if m:
                snapshot_date = f"{m.group(3)}-{m.group(2)}-{m.group(1)}"
        if not account_id:
            m = _PP_ACCOUNT_RE.search(t)
            if m:
                account_id = m.group(1)
    if not snapshot_date:
        raise SystemExit(
            f"{pdf_path}: no 'Portfolio performance at <date>' title found"
        )
    if not account_id:
        raise SystemExit(f"{pdf_path}: no 'Account <number>' field found")

    # The asset-allocation table is on a single page; layout-mode
    # extraction keeps columns aligned enough for line-based parse.
    # Multiple pages may match the surface "Asset allocation" text
    # (the Table of Contents on page 0 includes it). Every candidate
    # is tried and the first that yields rows is returned; the TOC will
    # parse to zero rows and quietly skip.
    positions: list[dict] = []
    for page in reader.pages:
        layout = page.extract_text(extraction_mode="layout")
        if "Asset allocation" not in layout:
            continue
        rows = _pp_parse_asset_allocation(layout, pdf_path)
        if rows:
            positions = rows
            break
    if not positions:
        raise SystemExit(
            f"{pdf_path}: no asset-allocation page yielded any "
            "positions — PDF layout may have shifted."
        )

    return {
        "snapshot_date": snapshot_date,
        "account_external_id": account_id,
        "positions": positions,
    }


def _pp_parse_asset_allocation(layout_text: str, pdf_path: Path) -> list[dict]:
    """Walk the asset-allocation page's text lines, emit one dict per
    securities row. Cash rows are skipped (they belong in
    currency_balances). Section context comes from "<Class> in <CCY>"
    header lines preceding each block.
    """
    out: list[dict] = []
    current_class: str | None = None
    current_currency: str | None = None
    # A bond's row is followed by a line holding one figure, its accrued
    # interest, right under the valuation column ("Valuation in CHF incl.
    # accrued interests"): a CHF amount on top of the row's valuation,
    # which is the clean quantity × price. The section total adds the
    # two.
    accrual_due: dict | None = None
    for raw_line in layout_text.splitlines():
        line = raw_line.strip()
        if not line:
            continue
        if accrual_due is not None:
            due, accrual_due = accrual_due, None
            if _PP_LONE_NUMBER_RE.match(line):
                due["accrued_interest_chf"] = _pp_to_number(line)
                continue

        # Section header? Sets context for following rows.
        m = _PP_SECTION_RE.match(line)
        if m:
            current_class, current_currency = m.group(1), m.group(2)
            continue
        # Plain "Cash" header — skip section entirely; cash is not a
        # security and goes elsewhere in silver.
        if line == "Cash":
            current_class, current_currency = "Cash", None
            continue
        # Total / subtotal / header rows — not positions.
        if line.lower().startswith("total "):
            continue
        if "Currency" in line and "Reference" in line:
            continue
        if "Quantity" in line and "Security" in line:
            continue
        if "incl. accrued interests" in line:
            continue

        # Skip cash-section content rows.
        if current_class == "Cash":
            continue

        # A securities row contains an ISIN. The line layout is
        # quantity | security | ISIN | avg price | market price |
        # price date | valuation_chf | %. Numbers are Swiss-locale.
        m = _PP_ISIN_RE.search(line)
        if not m:
            continue
        isin = m.group(1)
        before, after = line.split(isin, 1)
        # `before` is "<quantity><whitespace><security text>" — the
        # PDF layout extraction collapses inner whitespace so the
        # quantity is the leading token.
        before_tokens = before.split()
        if not before_tokens:
            continue
        try:
            quantity = _pp_to_number(before_tokens[0])
        except ValueError:
            continue
        name = " ".join(before_tokens[1:]).strip()

        # The tail has 5 tokens: avg, market, date, valuation, %.
        # A bond's accrued interest follows on a line of its own
        # (accrual_due, above).
        tail_tokens = after.split()
        if len(tail_tokens) < 5:
            raise SystemExit(
                f"{pdf_path}: positions row for {isin} has only "
                f"{len(tail_tokens)} tail tokens, expected 5+: {line!r}"
            )
        avg_price_raw, market_price_raw, price_date_raw = tail_tokens[:3]
        valuation_raw, pct_raw = tail_tokens[3], tail_tokens[4]

        # A price printed with "%" is in percent of nominal (a bond).
        # The two prices of a row share one quote.
        percent = avg_price_raw.endswith("%")
        if percent != market_price_raw.endswith("%"):
            raise SystemExit(
                f"{pdf_path}: average and market price of {isin} are "
                f"quoted differently: {line!r}"
            )

        md = _PP_DATE_RE.match(price_date_raw)
        if not md:
            raise SystemExit(
                f"{pdf_path}: unparseable price date {price_date_raw!r}"
                f" in row {line!r}"
            )
        price_date = f"{md.group(3)}-{md.group(2)}-{md.group(1)}"

        out.append({
            "asset_class": current_class,
            "currency": current_currency,
            "name": name,
            "isin": isin,
            "quantity": quantity,
            "avg_price": _pp_to_number(avg_price_raw),
            "market_price": _pp_to_number(market_price_raw),
            "price_quote": "percent" if percent else "unit",
            "price_date": price_date,
            "valuation_chf": _pp_to_number(valuation_raw),
            "account_pct": _pp_to_number(pct_raw),
        })
        if percent:
            accrual_due = out[-1]

    return out
