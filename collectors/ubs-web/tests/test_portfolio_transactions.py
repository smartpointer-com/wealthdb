"""Tests for the portfolio securities-transaction export.

Covers the CSV parser and the silver rows it writes. Reaching the
surface — switching portfolio, setting the window, taking the file —
is interaction with a live page, so it is not reachable from here.

Every identifier, security, figure and date below is invented: a
synthetic relationship prefix, ISINs in the XX reserved-looking range,
valors that are plainly sequential, and dates in a decade the source
cannot have booked in.
"""
from __future__ import annotations

import sqlite3
import sys
from pathlib import Path

import pytest

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
sys.path.insert(0, str(COLLECTOR))

import load as loader  # noqa: E402
from collectorkit import silver  # noqa: E402

MIGRATIONS_DIR = COLLECTOR / "migrations"

HEADER = ";".join(
    "Ccy." if c in ("ISIN currency", "Settlement currency") else c
    for c in loader.PORTFOLIO_TXN_COLUMNS
)

# A purchase: quantity and transaction value positive (cash leaving),
# carrying the bank's own reference.
BUY = ("31.12.2099;1234 00000001;1234 00000001 0006;1234 00000001.S5;"
       "05.01.2099;10:11:12;07.01.2099;07.01.2099;"
       "Stock Market Spot Purchase;Example Equity Fund;;"
       "10000001;XX0000000001;;1'500;USD;20.5;;USD;30'750;;;;"
       ";PM00000001;Equities;Common stock;Pooled vehicles")

# A sale of the same security: both signs flipped, its own reference.
SELL = ("31.12.2099;1234 00000001;1234 00000001 0006;1234 00000001.S5;"
        "06.01.2099;09:00:00;08.01.2099;08.01.2099;"
        "Stock Market Spot Sale;Example Equity Fund;;"
        "10000001;XX0000000001;;-500;USD;21.0;;USD;-10'500;;4.5;450.25;"
        ";PM00000002;Equities;Common stock;Pooled vehicles")

# A corporate action, which UBS publishes with NO external reference and
# no cash leg — the case the content hash exists for.
RIGHTS = ("31.12.2099;1234 00000001;1234 00000001 0006;1234 00000001.S5;"
          "09.01.2099;;09.01.2099;09.01.2099;"
          "Incoming Rights;Example Equity Fund;;"
          "10000002;XX0000000002;;25;;;;USD;;;;;"
          ";;Equities;Common stock;Pooled vehicles")

# A trade settling in a currency other than the valuation currency, so
# the exchange rate and the settlement currency both matter.
FX_SETTLED = ("31.12.2099;1234 00000001;1234 00000001 0006;1234 00000001.S5;"
              "10.01.2099;11:22:33;12.01.2099;12.01.2099;"
              "Stock Market Spot Purchase;Example Overseas Fund;;"
              "10000003;XX0000000003;;2'000;GBP;15.25;1.2345;USD;37'652;;;;"
              ";PM00000003;Equities;Common stock;Pooled vehicles")

FOOTER = ("Transaction list: With impact on position from 01.01.2099 "
          "to 31.12.2099;;;;;;;;;;;;;;;;;;;;;;;;;;;")


def _csv(*rows: str) -> str:
    return "\r\n".join((HEADER, *rows, FOOTER)) + "\r\n"


def _fresh_db(tmp_path: Path) -> sqlite3.Connection:
    conn = sqlite3.connect(str(tmp_path / "ubs-web.db"))
    conn.row_factory = sqlite3.Row
    silver.apply_migrations(conn, MIGRATIONS_DIR)
    return conn


def _seed(root: Path, text: str) -> Path:
    dump = root / "20990101T000000Z"
    (dump / "portfolio_transactions").mkdir(parents=True, exist_ok=True)
    (dump / "portfolio_transactions" / "transactions_abcd1234.csv").write_text(
        text, encoding="utf-8")
    return dump


# ----------------------------------------------------------------
# Identifier canonicalisation
# ----------------------------------------------------------------

def test_product_becomes_the_id_the_sibling_feed_uses():
    # The export's spaced, dotted form and the PSN feed's padded one are
    # the same custody account; both feeds must name it identically or
    # gold cannot tell one trade from two.
    assert loader.portfolio_account_canonical(
        "1234 00000001.S5") == "12340000000001S5"


def test_portfolio_column_becomes_the_id_gold_holds():
    # A portfolio id is NOT padded the way an account id is. The two
    # spaces look alike, and padding this one joins it to nothing.
    assert loader.portfolio_id_canonical(
        "1234 00000001 0006") == "1234000000010006"


@pytest.mark.parametrize("bad", ["", None, "not an account",
                                 "1234-00000001.S5", "1234 00000001"])
def test_unconvertible_identifiers_are_refused(bad):
    # A near-miss id would join to nothing and read as a new account.
    assert loader.portfolio_account_canonical(bad) is None


# ----------------------------------------------------------------
# The CSV parser
# ----------------------------------------------------------------

def test_loads_trades_with_signs_valor_and_isin(tmp_path):
    dump = _seed(tmp_path / "bronze", _csv(BUY, SELL))
    conn = _fresh_db(tmp_path)
    with conn:
        n = loader._load_portfolio_transactions(conn, 1700000000, dump)
    assert n == 2

    rows = {r["external_reference"]: r for r in conn.execute(
        "SELECT * FROM portfolio_transactions").fetchall()}
    buy = rows["PM00000001"]
    assert buy["transaction_external_id"] == "ptx:PM00000001"
    assert buy["safekeeping_account_external_id"] == "12340000000001S5"
    assert buy["portfolio_external_id"] == "1234000000010006"
    assert buy["booking_type"] == "Stock Market Spot Purchase"
    assert buy["valor"] == "10000001"
    assert buy["isin"] == "XX0000000001"
    assert buy["quantity"] == 1500.0
    assert buy["trans_value"] == 30750.0          # apostrophes stripped
    assert buy["settlement_currency_iso"] == "USD"
    assert buy["valuation_currency_iso"] == "USD"

    sell = rows["PM00000002"]
    # The export signs a disposal negative on both columns; the parser
    # keeps that rather than normalising, so the direction survives.
    assert sell["quantity"] == -500.0
    assert sell["trans_value"] == -10500.0
    assert sell["realized_pl"] == 450.25


def test_settlement_currency_and_rate_survive(tmp_path):
    dump = _seed(tmp_path / "bronze", _csv(FX_SETTLED))
    conn = _fresh_db(tmp_path)
    with conn:
        loader._load_portfolio_transactions(conn, 1700000000, dump)
    row = conn.execute("SELECT * FROM portfolio_transactions").fetchone()
    # Which cash account settled this is a question for gold, and the
    # settlement currency is half of its answer.
    assert row["settlement_currency_iso"] == "GBP"
    assert row["valuation_currency_iso"] == "USD"
    assert row["exchange_rate"] == 1.2345


def test_a_row_without_a_reference_is_still_keyed(tmp_path):
    dump = _seed(tmp_path / "bronze", _csv(RIGHTS))
    conn = _fresh_db(tmp_path)
    with conn:
        n = loader._load_portfolio_transactions(conn, 1700000000, dump)
    assert n == 1
    row = conn.execute("SELECT * FROM portfolio_transactions").fetchone()
    assert row["external_reference"] is None
    assert row["transaction_external_id"].startswith("ptx:")
    assert row["booking_type"] == "Incoming Rights"
    # No cash leg on this one; the security still moved.
    assert row["quantity"] == 25.0
    assert row["trans_value"] is None


def test_reloading_the_same_dump_does_not_duplicate(tmp_path):
    dump = _seed(tmp_path / "bronze", _csv(BUY, SELL, RIGHTS))
    conn = _fresh_db(tmp_path)
    with conn:
        loader._load_portfolio_transactions(conn, 1700000000, dump)
    with conn:
        # A later dump re-exports the same window; the referenced rows
        # and the hashed one must both land on their first copy.
        loader._load_portfolio_transactions(conn, 1800000000, dump)
    assert conn.execute(
        "SELECT count(*) FROM portfolio_transactions").fetchone()[0] == 3


def test_footer_is_not_data(tmp_path):
    dump = _seed(tmp_path / "bronze", _csv(BUY))
    conn = _fresh_db(tmp_path)
    with conn:
        n = loader._load_portfolio_transactions(conn, 1700000000, dump)
    assert n == 1


def test_a_changed_header_is_refused_rather_than_read_positionally(tmp_path):
    # A column inserted upstream would shift every value one place left;
    # the rows must be refused, not silently mis-parsed.
    text = _csv(BUY).replace("Valor;", "Valor;New Column;", 1)
    dump = _seed(tmp_path / "bronze", text)
    conn = _fresh_db(tmp_path)
    with conn:
        n = loader._load_portfolio_transactions(conn, 1700000000, dump)
    assert n == 0
    assert conn.execute(
        "SELECT count(*) FROM portfolio_transactions").fetchone()[0] == 0


def test_a_dump_without_the_surface_loads_unchanged(tmp_path):
    dump = tmp_path / "bronze" / "20990101T000000Z"
    dump.mkdir(parents=True)
    conn = _fresh_db(tmp_path)
    with conn:
        assert loader._load_portfolio_transactions(conn, 1700000000, dump) == 0


def test_rows_of_two_portfolios_coexist(tmp_path):
    other = BUY.replace("1234 00000001 0006", "1234 00000001 0002") \
               .replace("1234 00000001.S5", "1234 00000001.S1") \
               .replace("PM00000001", "PM00000009")
    dump = _seed(tmp_path / "bronze", _csv(BUY, other))
    conn = _fresh_db(tmp_path)
    with conn:
        n = loader._load_portfolio_transactions(conn, 1700000000, dump)
    assert n == 2
    accts = {r[0] for r in conn.execute(
        "SELECT DISTINCT safekeeping_account_external_id "
        "FROM portfolio_transactions")}
    assert accts == {"12340000000001S5", "12340000000001S1"}


# A consolidated view reports the same booking under a lettered id of
# its own, which names no portfolio and so cannot say which cash
# account settled the trade.
CONSOLIDATED_BUY = BUY.replace("1234 00000001 0006", "1234 00000001 R001")


def test_names_a_portfolio_rejects_a_consolidated_id():
    assert loader.names_a_portfolio("1234000000010006")
    assert not loader.names_a_portfolio("123400000001R001")
    assert not loader.names_a_portfolio("")
    assert not loader.names_a_portfolio(None)


@pytest.mark.parametrize("order", [
    (CONSOLIDATED_BUY, BUY),   # consolidated first, real copy after
    (BUY, CONSOLIDATED_BUY),   # and the other way round
])
def test_the_copy_naming_a_portfolio_wins(tmp_path, order):
    dump = _seed(tmp_path / "bronze", _csv(*order))
    conn = _fresh_db(tmp_path)
    with conn:
        loader._load_portfolio_transactions(conn, 1700000000, dump)
    rows = conn.execute("SELECT * FROM portfolio_transactions").fetchall()
    # One booking, whichever scope reported it.
    assert len(rows) == 1
    assert rows[0]["portfolio_external_id"] == "1234000000010006"


def test_a_booking_only_a_consolidated_scope_reported_is_kept(tmp_path):
    # Losing it would be worse than carrying one gold cannot place.
    dump = _seed(tmp_path / "bronze", _csv(CONSOLIDATED_BUY))
    conn = _fresh_db(tmp_path)
    with conn:
        n = loader._load_portfolio_transactions(conn, 1700000000, dump)
    assert n == 1
    row = conn.execute("SELECT * FROM portfolio_transactions").fetchone()
    assert row["portfolio_external_id"] == "123400000001R001"


# The same unreferenced booking as RIGHTS, as a consolidated view
# reports it: the scope's own id, and the securities valued in the
# scope's own currency rather than the portfolio's.
CONSOLIDATED_RIGHTS = ("31.12.2099;1234 00000001;1234 00000001 R001;"
                       "1234 00000001.S5;"
                       "09.01.2099;;09.01.2099;09.01.2099;"
                       "Incoming Rights;Example Equity Fund;;"
                       "10000002;XX0000000002;;25;;;;CHF;8'900;;;;"
                       ";;Equities;Common stock;Pooled vehicles")


@pytest.mark.parametrize("order", [
    (RIGHTS, CONSOLIDATED_RIGHTS),
    (CONSOLIDATED_RIGHTS, RIGHTS),
])
def test_a_scope_valuing_the_same_booking_differently_is_one_row(
        tmp_path, order):
    # Each scope values a booking in its own reporting currency, so the
    # figure is the one column that differs between two reports of one
    # event. Keyed on it, the booking reached silver twice — and gold
    # would have settled the same trade against the cash account twice.
    dump = _seed(tmp_path / "bronze", _csv(*order))
    conn = _fresh_db(tmp_path)
    with conn:
        loader._load_portfolio_transactions(conn, 1700000000, dump)
    rows = conn.execute("SELECT * FROM portfolio_transactions").fetchall()
    assert len(rows) == 1
    assert rows[0]["portfolio_external_id"] == "1234000000010006"
    assert rows[0]["valuation_currency_iso"] == "USD"


@pytest.mark.parametrize("raw,want", [
    ("1'234'567.89", 1234567.89),
    ("-98'765", -98765.0),
    # The unit a figure is counted in: pieces on a quantity, "à" on a
    # price per unit, percent on a bond or deposit. The number is the
    # same number; refusing the annotation lost the trade's quantity.
    ("4'000 p", 4000.0),
    ("-1'500 p", -1500.0),
    ("250.75 a", 250.75),
    ("100%", 100.0),
    ("", None),
    (None, None),
    ("   ", None),
    # An FX leg states a pair in one cell. Neither half is the cell's
    # value, and taking the first would book it in the other's currency.
    ("50'000 / -45'000.5", None),
    ("not a number", None),
])
def test_grouped_decimals_units_and_empty_cells(raw, want):
    assert loader.parse_grouped_decimal(raw) == want
