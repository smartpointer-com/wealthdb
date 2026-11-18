"""The ubs-web export passes: the positions export (and the mortgages
it lists), the cash export's transactions, and the portfolio
transaction list."""

from __future__ import annotations

import csv
import io
import logging
import re
import sqlite3
from collections.abc import Iterator
from datetime import datetime, timezone
from pathlib import Path

from collectorkit import bronze

import card_parsers  # local module
import landmarks as ubs  # local module: the export's shared vocabulary
from load_rows import (
    _TXN_INSERT,
    _TXN_NOT_OLDER,
    _mortgage_rate_type,
    _written_by_a_newer_dump,
    normalize_payload,
    ts_from_dmy,
)
from txn_ids import (
    _assign_export_txn_ids,
    _bare_number_holders,
    names_a_portfolio,
    portfolio_account_canonical,
    portfolio_id_canonical,
    portfolio_txn_id,
)

log = logging.getLogger("ubs-web.load")

# The UBS positions.csv columns the loader reads (semicolon-delimited,
# UTF-8 BOM, CRLF). Header row defines them in the order below.
POSITIONS_COLS = [
    "Banking relationship", "Portfolio", "Group of products",
    "Product", "Ccy.", "Number/Amt.", "Intraday number/amount",
    "Valor", "ISIN", "Cost price", "Buy exchange rate",
    "Sector", "Rating", "Description", "Description 1",
    "Description 2", "Description 3", "Description 4",
    "Description 5", "Lending value", "Intraday lending value",
    "Lending value ratio", "Intraday lending value ratio",
    "IBAN", "Category", "Interest", "Date", "Duration",
    "Market value", "% of market value", "Accrued interest",
    "% of accrued interest",
]


def parse_decimal(s: str | None) -> float | None:
    """Parse a UBS decimal cell. Web CSVs use '.' as decimal sep;
    return None for empty / whitespace cells."""
    if s is None:
        return None
    s = s.strip()
    if not s:
        return None
    try:
        return float(s)
    except ValueError:
        return None


def iban_to_psn_acct_id(iban_c: str | None) -> str | None:
    """Compute the 21-char PSN AcctId form from a canonical CH IBAN.

    Mapping (UBS Switzerland personal accounts):
        CH<2chk><bank:4><branch:4><base:8><chk:1>     (21 chars)
        -> <branch:4>0000 00<base:8>0000<chk:1>        (21 chars)
    """
    if not iban_c or len(iban_c) != 21 or not iban_c.startswith("CH"):
        return None
    branch = iban_c[8:12]
    base = iban_c[12:20]
    chk = iban_c[20]
    return branch + "0000" + "00" + base + "0000" + chk


def portfolio_external_id_from_full(full: str | None) -> str | None:
    """Extract the trailing portfolio code from
    `<prefix> <portfolio_code>` (e.g. 'BBBB AAAAAAAA RNNN' -> 'RNNN')."""
    if not full:
        return None
    parts = full.split()
    return parts[-1] if parts else None


def _cell(raw: list[str], idx: dict[str, int], name: str) -> str:
    """Defensive column read: returns '' if column index is out of range."""
    i = idx.get(name)
    if i is None or i >= len(raw):
        return ""
    return raw[i].strip()


# ----------------------------------------------------------------
# Positions
# ----------------------------------------------------------------

def _load_positions(conn: sqlite3.Connection, snapshot_at: int,
                    dump_dir: Path) -> int:
    """Parse every `positions/*.csv` in the dump dir; upsert into
    banking_relationships, portfolios, accounts, positions.

    Each per-portfolio CSV is parsed independently. The portfolio
    base currency for the `market_value` column is extracted from
    the CSV's "Valued in: …" footer line.
    """
    pos_dir = dump_dir / "positions"
    if not pos_dir.is_dir():
        return 0
    inserted = 0
    seen_accounts: set[str] = set()
    seen_portfolios: set[str] = set()
    seen_relationships: set[str] = set()
    seen_positions: set[tuple[str, str | None]] = set()
    seen_mortgages: set[str] = set()
    # Process per-portfolio CSVs first (`positions_<sha>.csv`), then
    # the consolidated default view (`positions.csv`). The default
    # view files unassigned accounts under a synthetic catch-all
    # portfolio code; loading it last means the `INSERT OR IGNORE`
    # on accounts keeps the real per-portfolio mapping where one
    # exists, and only the truly-unassigned accounts inherit the
    # catch-all. For positions the loader also tracks (account, isin)
    # pairs already inserted from a per-portfolio CSV and skip the
    # consolidated row for them, otherwise every holding ends up
    # double-counted (once under its real portfolio, once under the
    # catch-all).
    per_portfolio_csvs = sorted(pos_dir.glob("positions_*.csv"))
    consolidated_csvs = sorted(pos_dir.glob("positions.csv"))
    for csv_path in per_portfolio_csvs + consolidated_csvs:
        base_ccy = _read_positions_base_currency(csv_path)
        for row in _iter_positions_rows(csv_path):
            inserted += _ingest_positions_row(
                conn, snapshot_at, row, base_ccy,
                seen_accounts, seen_portfolios, seen_relationships,
                seen_positions, seen_mortgages,
            )
    return inserted


VALUED_IN_RE = re.compile(r"^Valued in:\s*([A-Z]{3})\b", re.MULTILINE)


def _read_positions_base_currency(csv_path: Path) -> str | None:
    """Extract the 'Valued in: <CCY>' footer line; default None."""
    text = csv_path.read_text(encoding="utf-8-sig", errors="replace")
    m = VALUED_IN_RE.search(text)
    return m.group(1) if m else None


RELATIONSHIP_PREFIX_RE = re.compile(r"^\d{4}\s+\d{8}$")


def _iter_positions_rows(csv_path: Path) -> Iterator[dict]:
    """Yield meaningful holding rows from a positions.csv.

    Skips:
      - the header row itself
      - footer rows ("Export created on: …", "Number of positions: …")
      - blank rows
      - currency-conversion appendix rows whose column 0 is e.g.
        "EUR/CHF" instead of a "<branch> <base>" prefix.
    """
    with csv_path.open("r", encoding="utf-8-sig", newline="") as f:
        reader = csv.reader(f, delimiter=";")
        try:
            header = next(reader)
        except StopIteration:
            return
        # Header row sanity-check: must contain "Banking relationship".
        if not header or header[0].strip() != POSITIONS_COLS[0]:
            log.warning("unexpected positions.csv header in %s: %r",
                        csv_path.name, header[:3])
            return
        # Map header names to indices defensively (UBS may re-order).
        idx = {name: i for i, name in enumerate(header) if name}
        for raw in reader:
            if not raw or all(not c.strip() for c in raw):
                continue
            relationship = _cell(raw, idx, "Banking relationship")
            # Require the canonical "<4-digit branch> <8-digit base>"
            # form. Filters out:
            #   - real footer rows (no value)
            #   - currency-pair appendix rows (e.g. 'EUR/CHF')
            #   - "Portfolio number: …", "Valued in: …", etc. (text)
            if not RELATIONSHIP_PREFIX_RE.match(relationship):
                continue
            row = {col: _cell(raw, idx, col) for col in POSITIONS_COLS}
            yield row


def _ingest_positions_row(conn: sqlite3.Connection, snapshot_at: int,
                          row: dict, base_currency: str | None,
                          seen_accounts: set[str],
                          seen_portfolios: set[str],
                          seen_relationships: set[str],
                          seen_positions: set[tuple[str, str | None]],
                          seen_mortgages: set[str]) -> int:
    relationship_prefix = row["Banking relationship"] or None
    portfolio_full = row["Portfolio"] or None
    portfolio_ext_id = portfolio_external_id_from_full(portfolio_full)

    # Mortgage rows ('Pro memoria - Mortgages' group) have the
    # IBAN column re-used for the fixed-rate term and the Product
    # column holds the UBS-internal mortgage account number — so
    # they need their own ingest path before the IBAN-based
    # account/position inserts below.
    if (row.get("Group of products") or "").strip() == \
            "Pro memoria - Mortgages":
        return _ingest_mortgage_row(
            conn, snapshot_at, row,
            relationship_prefix, portfolio_ext_id,
            seen_mortgages,
        )

    iban_raw = row["IBAN"] or None
    iban_c = ubs.iban_canonical(iban_raw)
    isin = row["ISIN"] or None

    # banking_relationships (one synthetic row per relationship-prefix).
    # positions.csv does not carry the opaque bankingRelationId token,
    # so the prefix doubles as the id here.
    if relationship_prefix and relationship_prefix not in seen_relationships:
        seen_relationships.add(relationship_prefix)
        conn.execute(
            "INSERT OR IGNORE INTO banking_relationships ("
            "snapshot_at, banking_relationship_id, account_number_prefix, "
            "description, payload"
            ") VALUES (?, ?, ?, ?, ?)",
            (snapshot_at, relationship_prefix, relationship_prefix, None,
             normalize_payload({"source": "positions.csv"})),
        )

    # portfolios
    if portfolio_ext_id and portfolio_ext_id not in seen_portfolios:
        seen_portfolios.add(portfolio_ext_id)
        conn.execute(
            "INSERT OR IGNORE INTO portfolios ("
            "snapshot_at, portfolio_external_id, banking_relationship_id, "
            "portfolio_full_id, portfolio_uid, base_currency, "
            "description, payload"
            ") VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
            (snapshot_at, portfolio_ext_id, relationship_prefix,
             portfolio_full, None, base_currency, None,
             normalize_payload({"source": "positions.csv"})),
        )

    # accounts (cash only — securities-only rows have no IBAN)
    if iban_c and iban_c not in seen_accounts:
        seen_accounts.add(iban_c)
        conn.execute(
            "INSERT OR IGNORE INTO accounts ("
            "snapshot_at, account_external_id, kind, iban, "
            "account_acct_id_psn_form, account_number_raw, "
            "account_opaque_id, banking_relationship_id, "
            "portfolio_external_id, currency_iso, description, payload"
            ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            (snapshot_at, iban_c, "cash", iban_raw,
             iban_to_psn_acct_id(iban_c),
             row["Product"] or None, None,
             relationship_prefix, portfolio_ext_id,
             row["Ccy."] or None,
             row["Description"] or None,
             normalize_payload({"source": "positions.csv"})),
        )

    # positions
    # Resolve account_external_id: IBAN for cash, '' for securities-only
    # rows (positions table PK includes instrument_isin to disambiguate).
    account_ext = iban_c or ""
    if not isin and not iban_c:
        # Pure aggregate / informational row — skip.
        return 0
    pos_key = (account_ext, isin)
    if pos_key in seen_positions:
        # Already loaded from an earlier (per-portfolio) CSV in this
        # dump. The consolidated view would attach it to the synthetic
        # catch-all portfolio; that's a duplicate, so skip.
        return 0
    seen_positions.add(pos_key)
    conn.execute(
        "INSERT OR REPLACE INTO positions ("
        "snapshot_at, portfolio_external_id, account_external_id, "
        "instrument_isin, valor, currency_iso, units, market_value, "
        "market_value_currency, cost_price, accrued_interest, "
        "lending_value, lending_value_ratio, description, payload"
        ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
        (
            snapshot_at, portfolio_ext_id or "", account_ext,
            isin, row["Valor"] or None, row["Ccy."] or None,
            parse_decimal(row["Number/Amt."]),
            parse_decimal(row["Market value"]),  # base-ccy market value
            base_currency,
            parse_decimal(row["Cost price"]),
            parse_decimal(row["Accrued interest"]),
            parse_decimal(row["Lending value"]),
            parse_decimal(row["Lending value ratio"]),
            row["Description"] or None,
            normalize_payload({k: row[k] for k in POSITIONS_COLS}),
        ),
    )
    return 1


MORTGAGE_TERM_RE = re.compile(
    r"^\s*(\d{2})\.(\d{2})\.(\d{4})\s*-\s*(\d{2})\.(\d{2})\.(\d{4})\s*$"
)


def _parse_mortgage_term(s: str | None) -> tuple[int | None, int | None]:
    """Extract (start_ts, end_ts) from the 'IBAN' column when it
    actually carries a 'dd.mm.yyyy - dd.mm.yyyy' fixed-rate term.
    Returns (None, None) on anything that doesn't match exactly.
    """
    if not s:
        return None, None
    m = MORTGAGE_TERM_RE.match(s)
    if not m:
        return None, None
    sd, sm, sy, ed, em, ey = m.groups()
    try:
        start = datetime(int(sy), int(sm), int(sd), tzinfo=timezone.utc)
        end = datetime(int(ey), int(em), int(ed), tzinfo=timezone.utc)
    except ValueError:
        return None, None
    return int(start.timestamp()), int(end.timestamp())


def _ingest_mortgage_row(conn: sqlite3.Connection, snapshot_at: int,
                         row: dict,
                         relationship_prefix: str | None,
                         portfolio_ext_id: str | None,
                         seen_mortgages: set[str]) -> int:
    """Insert a single 'Pro memoria - Mortgages' row into the
    `mortgages` silver table. The CSV uses the IBAN / Category /
    Date columns as overflow slots for mortgage-specific data —
    the known parts are promoted and the rest is kept in `payload`
    for forensics. See migration 0004."""
    account_ext = (row.get("Product") or "").strip()
    if not account_ext:
        return 0
    if account_ext in seen_mortgages:
        return 0
    seen_mortgages.add(account_ext)

    start_ts, end_ts = _parse_mortgage_term(row.get("IBAN"))
    rate_type = _mortgage_rate_type(row.get("Description 1"))
    collateral = row.get("Description 3") or row.get("Sector") or None
    description = row.get("Description") or row.get("Description 1") or None
    currency = (row.get("Ccy.") or "").strip() or None
    if currency is None:
        # `Ccy.` should always be set on UBS mortgages; if it
        # isn't, the row is dropped rather than a NULL inserted into
        # a NOT NULL column.
        log.warning("mortgage row %s has no currency; skipping",
                    bronze.log_id(account_ext))
        return 0

    conn.execute(
        "INSERT OR REPLACE INTO mortgages ("
        "snapshot_at, account_external_id, banking_relationship_id, "
        "portfolio_external_id, currency_iso, outstanding_balance, "
        "start_date, end_date, rate_type, collateral_description, "
        "description, payload"
        ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
        (
            snapshot_at, account_ext, relationship_prefix,
            portfolio_ext_id, currency,
            parse_decimal(row.get("Number/Amt.")),
            start_ts, end_ts, rate_type, collateral, description,
            normalize_payload({k: row[k] for k in POSITIONS_COLS}),
        ),
    )
    return 1


# ----------------------------------------------------------------
# Portfolio securities transactions
# ----------------------------------------------------------------

# The export's columns, in the order it writes them. Two are headed
# `Ccy.`: the first sits beside the ISIN and is always empty, the second
# carries the settlement currency. csv.DictReader would keep only one of
# them, so the header is matched positionally against this list instead
# and the duplicate resolved by position.
PORTFOLIO_TXN_COLUMNS = (
    "Valuation date", "Banking relationship", "Portfolio", "Product",
    "Trade date", "Trade time", "Booking", "Value date",
    "Description 1", "Description 2", "Description 3",
    "Valor", "ISIN", "ISIN currency", "Number/Amt.", "Settlement currency",
    "Trans. price", "Exchange rate", "Valuation currency", "Trans. value",
    "Accrued interest", "Realized P/L in %", "Realized P/L",
    "Order no.", "External reference",
    "Asset class", "Sub-asset class", "Instrument category",
)
# The two headings the export repeats, renamed above by position.
_PORTFOLIO_TXN_RAW_HEADER = tuple(
    "Ccy." if c in ("ISIN currency", "Settlement currency") else c
    for c in PORTFOLIO_TXN_COLUMNS
)


def _load_portfolio_transactions(conn: sqlite3.Connection, snapshot_at: int,
                                 dump_dir: Path) -> int:
    """Parse every `portfolio_transactions/*.csv`; UPSERT into
    portfolio_transactions (migration 0012).

    A dump taken before this surface was harvested has no such dir and
    loads as it always did."""
    src_dir = dump_dir / "portfolio_transactions"
    if not src_dir.is_dir():
        return 0
    inserted = 0
    for csv_path in sorted(src_dir.glob("*.csv")):
        try:
            rows = _read_portfolio_txn_csv(csv_path)
        except ValueError as e:
            log.warning("portfolio transactions %s: %s", csv_path.name, e)
            continue
        for row in rows:
            inserted += _ingest_portfolio_txn_row(conn, snapshot_at, row)
    return inserted


def _read_portfolio_txn_csv(csv_path: Path) -> list[dict]:
    """Rows of one export, as dicts keyed by PORTFOLIO_TXN_COLUMNS.

    The file closes with a footer line stating the window it covers;
    that line and anything after it is not data. A header that is not
    the one this parser was written against raises rather than being
    read positionally anyway — a column UBS inserts would otherwise
    shift every value one place to the left in silence."""
    text = csv_path.read_text(encoding="utf-8-sig", errors="replace")
    reader = csv.reader(io.StringIO(text), delimiter=";")
    try:
        header = next(reader)
    except StopIteration:
        return []
    header = [h.strip() for h in header]
    if tuple(header) != _PORTFOLIO_TXN_RAW_HEADER:
        raise ValueError(
            f"unexpected header ({len(header)} columns); the export's "
            f"layout has changed and the parser must be revisited")
    out: list[dict] = []
    for raw in reader:
        if not raw or not raw[0].strip():
            continue
        if raw[0].startswith(ubs.TXN_EXPORT_FOOTER_PREFIX):
            break
        # A short row is padded rather than dropped: the export omits
        # trailing empties on some rows.
        raw = raw + [""] * (len(PORTFOLIO_TXN_COLUMNS) - len(raw))
        out.append({k: raw[i].strip()
                    for i, k in enumerate(PORTFOLIO_TXN_COLUMNS)})
    return out


def parse_grouped_decimal(s: str | None) -> float | None:
    """Parse a decimal written with apostrophe thousands separators
    ("-98'765", "1'234.50"). Distinct from `parse_decimal`, which
    reads the plain form the cash CSVs use, and from
    `rate_parsers.parse_amount`, which drops a leading currency and
    comma separators but reads no trailing unit.

    The portfolio export annotates some figures with the unit they are
    counted in — a quantity in pieces ("4'000 p"), a price per unit
    ("250.75 a"), a bond or deposit quoted in percent ("100%"). The
    number is the same number whatever unit it is stated in, so the
    annotation is read and dropped; refusing it would leave a real trade
    with no quantity at all.

    An FX leg states a PAIR in one cell ("50'000 / -45'000.5": bought
    the one, sold the other), and that is not a figure this returns.
    Taking the first half would silently book one leg's amount against
    the other leg's currency, so the pair reads as no value and the
    two numbers stay in the payload where both are visible."""
    if s is None:
        return None
    s = s.strip().replace("'", "").replace("’", "")
    if not s:
        return None
    m = _GROUPED_DECIMAL_RE.fullmatch(s)
    if not m:
        return None
    try:
        return float(m.group(1))
    except ValueError:
        return None


# A signed decimal, optionally followed by the unit it is counted in.
_GROUPED_DECIMAL_RE = re.compile(r"([+-]?\d*\.?\d+)\s*(?:%|[a-zA-Z]{1,3})?")


def _ingest_portfolio_txn_row(conn: sqlite3.Connection, snapshot_at: int,
                              row: dict) -> int:
    account = portfolio_account_canonical(row.get("Product"))
    portfolio = portfolio_id_canonical(row.get("Portfolio"))
    value_date = ts_from_dmy(row.get("Value date"))
    booking_type = (row.get("Description 1") or "").strip()
    # The three columns without which a row cannot be placed, keyed or
    # classified. The export pads its own footer area with blank-ish
    # lines, so this is also what keeps those out of silver.
    if not account or value_date is None or not booking_type:
        return 0
    txn_id = portfolio_txn_id(account, row)
    if _written_by_a_newer_dump(conn, "portfolio_transactions", {
            "transaction_external_id": txn_id,
            "safekeeping_account_external_id": account}, snapshot_at):
        return 0
    # The same booking is published under every scope that covers it, so
    # a consolidated copy arrives keyed identically to the real
    # portfolio's own. Whichever lands second would otherwise win and
    # take the portfolio id down with it. A copy that names a portfolio
    # is kept over one that does not; a booking only a consolidated
    # scope reported is still kept, because the alternative is losing
    # it.
    if not names_a_portfolio(portfolio):
        held = conn.execute(
            "SELECT portfolio_external_id FROM portfolio_transactions "
            "WHERE transaction_external_id = ? "
            "  AND safekeeping_account_external_id = ?",
            (txn_id, account),
        ).fetchone()
        if held is not None and names_a_portfolio(held[0]):
            return 0
    conn.execute(
        "INSERT OR REPLACE INTO portfolio_transactions ("
        "transaction_external_id, safekeeping_account_external_id, "
        "portfolio_external_id, snapshot_at, trade_date, booking_date, "
        "value_date, booking_type, security_name, valor, isin, quantity, "
        "settlement_currency_iso, trans_price, exchange_rate, "
        "valuation_currency_iso, trans_value, accrued_interest, "
        "realized_pl, order_no, external_reference, asset_class, "
        "sub_asset_class, instrument_category, payload"
        ") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
        (
            txn_id, account,
            portfolio or "", snapshot_at,
            ts_from_dmy(row.get("Trade date")),
            ts_from_dmy(row.get("Booking")),
            value_date, booking_type,
            row.get("Description 2") or None,
            row.get("Valor") or None,
            row.get("ISIN") or None,
            parse_grouped_decimal(row.get("Number/Amt.")),
            row.get("Settlement currency") or None,
            parse_grouped_decimal(row.get("Trans. price")),
            parse_grouped_decimal(row.get("Exchange rate")),
            row.get("Valuation currency") or None,
            parse_grouped_decimal(row.get("Trans. value")),
            parse_grouped_decimal(row.get("Accrued interest")),
            parse_grouped_decimal(row.get("Realized P/L")),
            row.get("Order no.") or None,
            row.get("External reference") or None,
            row.get("Asset class") or None,
            row.get("Sub-asset class") or None,
            row.get("Instrument category") or None,
            normalize_payload(row),
        ),
    )
    return 1


# ----------------------------------------------------------------
# Transactions
# ----------------------------------------------------------------

# Transactions CSV (UTF-8 BOM, semicolon, CRLF): a metadata block, then
# the data header row that starts with "Trade date;".
TXN_HEADER_IBAN = "IBAN:"
# The metadata block sits within the first lines of the file.
TXN_HEADER_SCAN_LINES = 15


def _txn_header_value(line: str) -> str | None:
    """The value of a `<Label>:;<value>;` metadata line."""
    return line.split(";", 1)[1].rstrip(";").strip() or None


def _load_transactions(conn: sqlite3.Connection, snapshot_at: int,
                       dump_dir: Path) -> int:
    """Parse every `transactions/cash_*.csv` in the dump dir;
    UPSERT into transactions keyed by (transaction_external_id,
    account_external_id).

    Every CSV of the dump is parsed before anything is written, because
    the id a row gets depends on the other rows sharing its transaction
    number (`_assign_export_txn_ids`) and UBS splits one account's
    history across several files. Grouping per file would let a product
    whose movements straddle a split boundary mint the same ids twice.
    """
    txn_dir = dump_dir / "transactions"
    if not txn_dir.is_dir():
        return 0
    rows: list[dict] = []
    for csv_path in sorted(txn_dir.glob("cash_*.csv")):
        rows.extend(_parse_transactions_csv(csv_path))
    ids = _assign_export_txn_ids(rows, _bare_number_holders(conn, rows))
    for txn_id, row in zip(ids, rows, strict=True):
        _upsert_export_transaction(conn, snapshot_at, txn_id, row)
    return len(rows)


def _upsert_export_transaction(conn: sqlite3.Connection, snapshot_at: int,
                               txn_id: str, row: dict) -> None:
    """Write one parsed export row under the id `_assign_export_txn_ids`
    gave it. A row a newer dump wrote stays as it is."""
    conn.execute(
        _TXN_INSERT +
        "DO UPDATE SET "
        "snapshot_at = excluded.snapshot_at, "
        "trade_date = excluded.trade_date, "
        "booking_date = excluded.booking_date, "
        "value_date = excluded.value_date, "
        "currency_iso = excluded.currency_iso, "
        "amount_debit = excluded.amount_debit, "
        "amount_credit = excluded.amount_credit, "
        "counterparty = excluded.counterparty, "
        "description_kind = excluded.description_kind, "
        "payload = excluded.payload "
        + _TXN_NOT_OLDER,
        (
            txn_id, row["account_external_id"], snapshot_at,
            row["trade_date"], row["booking_date"], row["value_date"],
            row["currency_iso"], row["amount_debit"], row["amount_credit"],
            row["counterparty"], row["description_kind"], row["payload"],
        ),
    )


def _parse_transactions_csv(csv_path: Path) -> list[dict]:
    """Parse the 8-line metadata block + 1-line column header + data."""
    with csv_path.open("r", encoding="utf-8-sig", newline="") as f:
        lines = f.read().splitlines()
    # Locate the data header by literal match (UBS may add fields).
    header_idx = None
    iban = None
    for i, ln in enumerate(lines[:TXN_HEADER_SCAN_LINES]):
        if ln.startswith(TXN_HEADER_IBAN):
            iban = _txn_header_value(ln)
        if ln.startswith("Trade date;"):
            header_idx = i
            break
    if header_idx is None:
        log.warning("no transactions header in %s; skipping", csv_path.name)
        return []
    account_ext = ubs.iban_canonical(iban) or ""
    if not account_ext:
        log.warning("no IBAN in %s; skipping", csv_path.name)
        return []
    header = [c.strip() for c in lines[header_idx].split(";")]
    idx = {name: i for i, name in enumerate(header) if name}
    rows: list[dict] = []
    for raw_line in lines[header_idx + 1:]:
        if not raw_line.strip():
            continue
        raw = next(csv.reader([raw_line], delimiter=";"))
        # The "Turnover total" footer block carries blank columns 1-9
        # and "Turnover total" in description1 — skip.
        trade_date = _cell(raw, idx, "Trade date")
        txn_no = _cell(raw, idx, "Transaction no.")
        if not txn_no:
            continue
        value_date_s = _cell(raw, idx, "Value date") or trade_date
        value_date_ts = card_parsers.ts(value_date_s)
        if not value_date_ts:
            log.warning("unparseable value_date %r in %s; skipping row",
                        value_date_s, csv_path.name)
            continue
        description1 = _cell(raw, idx, "Description1")
        rows.append({
            "txn_no": txn_no,
            "account_external_id": account_ext,
            "trade_date": card_parsers.ts(trade_date),
            "booking_date": card_parsers.ts(_cell(raw, idx, "Booking date")),
            "value_date": value_date_ts,
            "currency_iso": _cell(raw, idx, "Currency"),
            "amount_debit": parse_decimal(_cell(raw, idx, "Debit")),
            "amount_credit": parse_decimal(_cell(raw, idx, "Credit")),
            # description1's first semi-line is usually the counterparty
            "counterparty": (description1.split(";", 1)[0] or None),
            "description_kind": _cell(raw, idx, "Description2") or None,
            "payload": normalize_payload(dict(zip(header, raw, strict=False))),
            # Raw cells, kept only to fingerprint a row against its
            # siblings when UBS gives several of them one number.
            "booking_date_raw": _cell(raw, idx, "Booking date"),
            "value_date_raw": value_date_s,
            "debit_raw": _cell(raw, idx, "Debit"),
            "credit_raw": _cell(raw, idx, "Credit"),
            "description1": description1,
        })
    return rows
