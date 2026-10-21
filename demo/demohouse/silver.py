"""Writing silver files of the synthetic kind.

One SQLite file per source, created from the kind's schema file in the
engine's source tree, so generator and adapter share one definition.

A full build writes a file from scratch as one dump run. An append run
opens an existing file, checks that the history on disk was made by the
same generator from the same inputs, and adds only the rows that joined
the history after the as-of the file already reached, plus one dump run
whose window holds them. A row joins the history on the day it is dated
by, except a company's reported figure, which joins it on the day it is
filed, weeks after the period it describes. The window of a run that
adds one reaches back to that period's last day, so it holds the as_of
of every row the run adds, as the kind requires. Nothing already in the
file is rewritten.

Rows are inserted in a fixed order inside one transaction, so two full
builds from the same inputs are byte-identical files.
"""

import pathlib
import sqlite3

from . import dates, rates, reference
from .spec import SCHEMA_PATH

SCHEMA_VERSION = "4"

# The source that carries the reference records.
REFERENCE_SOURCE = "refdata"

_TABLES = {
    "portfolios": ("portfolio_id", "display_name", "base_currency", "nickname", "payload"),
    "accounts": ("account_id", "account_kind", "display_name", "base_currency", "nickname",
                 "account_category", "tax_wrapper", "management_style", "portfolio_id", "payload"),
    "instruments": ("instrument_id", "valid_from", "asset_class", "vehicle", "isin", "cusip",
                    "symbol", "name", "currency", "payload"),
    "positions": ("snapshot_at", "account_id", "position_key", "instrument_id", "asset_class",
                  "vehicle", "currency", "quantity", "market_value", "book_value",
                  "accrued_interest", "acquisition_date", "payload"),
    "position_lots": ("snapshot_at", "account_id", "position_key", "lot_key", "quantity", "book_value",
                      "acquisition_date", "term", "payload"),
    "cash_balances": ("snapshot_at", "account_id", "currency", "balance_kind", "amount", "payload"),
    "fx_rates": ("snapshot_at", "base_currency", "quote_currency", "mid_rate", "bid_rate",
                 "ask_rate", "payload"),
    "transactions": ("transaction_id", "occurred_at", "account_id", "instrument_id", "asset_class",
                     "vehicle", "instrument_hint", "kind", "currency", "gross_amount", "net_amount",
                     "quantity", "price", "description", "memo", "counterparty",
                     "provider_category", "check_number", "payload"),
    "realized_lots": ("realized_lot_id", "account_id", "instrument_id", "description", "document_kind",
                      "tax_year", "acquisition_date", "disposal_date", "currency", "quantity", "proceeds",
                      "book_value", "realized_gain_loss", "term", "payload"),
    "compositions": ("fund_ref", "as_of", "basis", "provider", "source_kind", "fund_name", "currency",
                     "net_assets", "line_count", "weight_sum", "units_outstanding", "run_at", "payload"),
    "lines": ("fund_ref", "as_of", "basis", "line_no", "isin", "cusip", "figi", "ticker", "exchange", "name",
              "asset_type", "weight", "quantity", "market_value", "currency", "country", "risk_country",
              "sector", "industry", "maturity", "coupon", "long_currency", "long_amount", "short_currency",
              "short_amount", "payload", "coupon_kind", "is_default", "in_arrears", "paid_in_kind", "exchange_rate",
              "duration", "modified_duration", "yield_to_maturity", "yield_to_worst", "yield_to_call",
              "real_duration", "real_yield", "accrual_date"),
    "attributes": ("fund_ref", "as_of", "domicile", "legal_form", "ucits", "replication", "distribution", "ter",
                   "hedged_currency", "lending", "index_name", "depositary", "run_at", "payload"),
    "counterparties": ("fund_ref", "as_of", "role", "name", "lei", "share", "run_at", "payload"),
    "securities": ("security_key", "as_of", "provider", "isin", "cusip", "figi", "lei", "ticker", "exchange",
                   "name", "security_type", "listing_currency", "run_at", "payload"),
    "security_facts": ("security_key", "field", "as_of", "provider", "value", "scheme", "run_at", "payload"),
    "fund_risk": ("fund_ref", "as_of", "currency", "tenor", "dv01", "dv100", "credit_ig_dv01", "credit_hy_dv01",
                  "run_at", "payload"),
    "characteristics": ("fund_ref", "as_of", "provider", "field", "label", "value", "run_at", "payload"),
    "period_facts": ("security_key", "field", "as_of", "period_start", "provider", "value", "scheme", "unit",
                     "fiscal_year", "fiscal_period", "form", "filed", "run_at", "payload"),
    "rates": ("as_of", "series_id", "value", "payload"),
}

# The reference tables: what the funds hold and what the securities are
# (demohouse/reference.py). Only the reference source writes them.
REFERENCE_TABLES = reference.TABLES

# The tables of dated facts. The rest are dimensions, which an append
# re-sends whole and the file keeps once.
FACT_TABLES = ("positions", "position_lots", "cash_balances", "fx_rates", "rates", "transactions",
               "realized_lots", *REFERENCE_TABLES)

# The meta keys an append run requires to match before it may extend a file.
IDENTITY_KEYS = ("schema_version", "generator_hash", "seed", "spec_hash", "catalogue_hash", "findings")


class AppendRefused(Exception):
    """The file on disk cannot be extended by this run."""


def identity(inputs, seed, findings):
    return {
        "schema_version": SCHEMA_VERSION,
        "generator_hash": inputs.code_hash,
        "seed": seed,
        "spec_hash": inputs.spec_hash,
        "catalogue_hash": inputs.catalogue_hash,
        "findings": "1" if findings else "0",
    }


def source_rows(sim, source, as_of):
    """Every silver row of `source` dated on or before as_of, per table."""
    book = sim.book
    end = _day_end(as_of)
    accounts = [a for a in book.sources[source]["accounts"] if a.opens <= as_of]
    portfolios = {a.portfolio for a in accounts if a.portfolio}
    rows = {
        "portfolios": [dict(p, payload="{}") for p in book.portfolios[source]
                       if p["portfolio_id"] in portfolios],
        "accounts": [{
            "account_id": a.id, "account_kind": a.kind, "display_name": a.display_name,
            "base_currency": a.currency, "nickname": a.nickname, "account_category": None,
            "tax_wrapper": a.tax_wrapper, "management_style": a.style, "portfolio_id": a.portfolio,
            "payload": "{}"} for a in accounts],
        "instruments": [v for (_, valid_from), v in sorted(book.instrument_versions[source].items())
                        if valid_from <= end],
        "positions": [dict(p, payload="{}") for p in book.rows[source]["positions"]],
        "position_lots": [dict(lot, payload="{}") for lot in book.rows[source]["lots"]],
        "cash_balances": book.rows[source]["cash"],
        "fx_rates": [dict(f, bid_rate=None, ask_rate=None, payload="{}")
                     for f in (sim.fx_rows if source == "fx" else [])],
        "rates": rates.rows({d: p for d, p in sim.curve.items() if d <= as_of}) if source == rates.SOURCE else [],
        "transactions": [{k: v for k, v in t.items() if not k.startswith("_")}
                         | {"asset_class": None, "vehicle": None, "instrument_hint": None}
                         for t in book.rows[source]["transactions"]],
        "realized_lots": book.rows[source]["realized"],
    }
    ref = reference.rows(sim.inputs, sim.seed, as_of, sim.curve) if source == REFERENCE_SOURCE else {}
    for table in REFERENCE_TABLES:
        rows[table] = ref.get(table, [])
    return rows


def _day_end(day):
    return dates.epoch(day) + dates.DAY - 1


def _time_of(table, row):
    """The instant a fact row is dated by, which its run's window holds;
    None for a dimension row."""
    if table in ("positions", "position_lots", "cash_balances", "fx_rates"):
        return row["snapshot_at"]
    if table == "transactions":
        return row["occurred_at"]
    if table == "realized_lots":
        return dates.epoch(dates.parse(row["disposal_date"]))
    if table in REFERENCE_TABLES or table == "rates":
        return row["as_of"]
    return None


def _joined_at(table, row):
    """The instant a row joins the history, which decides the run that
    adds it: its filed day for a company's figure, else its own date."""
    if table == "period_facts":
        return row["run_at"]
    return _time_of(table, row)


def _insert(con, table, rows):
    """Insert rows. A fact whose key is already taken fails the run; a
    dimension row already in the file is kept as it is."""
    cols = _TABLES[table]
    verb = "INSERT" if table in FACT_TABLES else "INSERT OR IGNORE"
    sql = f"{verb} INTO {table} ({', '.join(cols)}) VALUES ({', '.join('?' * len(cols))})"
    con.executemany(sql, [tuple(r.get(c) for c in cols) for r in rows])


def _insert_run(con, change_number, start, end, as_of):
    con.execute("INSERT INTO dump_runs (change_number, window_start, window_end, as_of) VALUES (?, ?, ?, ?)",
                (change_number, start, end, as_of.isoformat()))


def _count(rows):
    return sum(len(rows[t]) for t in FACT_TABLES)


def _span(rows):
    times = [t for table, rs in rows.items() for r in rs if (t := _time_of(table, r)) is not None]
    return (min(times), max(times)) if times else (None, None)


def write_full(path, rows, meta, history_floor, as_of):
    """A fresh file holding `rows` as one dump run. Returns the number of
    fact rows written."""
    path = pathlib.Path(path)
    path.unlink(missing_ok=True)
    lo, _ = _span(rows)
    start = lo if lo is not None else history_floor
    con = sqlite3.connect(path, isolation_level=None)
    try:
        con.execute("PRAGMA journal_mode=DELETE")
        con.executescript(SCHEMA_PATH.read_text())
        con.execute("BEGIN")
        _insert_meta(con, dict(meta, as_of=as_of.isoformat()))
        _insert_run(con, dates.epoch(as_of), start, _day_end(as_of), as_of)
        for table in _TABLES:
            _insert(con, table, rows[table])
        con.execute("COMMIT")
    finally:
        con.close()
    return _count(rows)


def _read_only(path):
    # as_uri() percent-encodes '#', '?' and '%', which a raw path in a
    # file: URI would read as the URI's own syntax.
    return sqlite3.connect(pathlib.Path(path).resolve().as_uri() + "?mode=ro", uri=True)


def recorded(path):
    """The meta of an existing file, as a dict."""
    con = _read_only(path)
    try:
        return dict(con.execute("SELECT key, value FROM meta").fetchall())
    finally:
        con.close()


def check_append(path, meta, as_of):
    """Refuse an append that would splice a different history on, or
    that would not move forward. Returns the as-of already on disk."""
    if not pathlib.Path(path).exists():
        raise AppendRefused(f"{path}: no silver file to append to; run a full build first")
    have = recorded(path)
    if have.get("findings") == "1":
        raise AppendRefused(f"{path}: the files hold a findings build, a one-off picture that cannot be "
                            "appended to; run a full build")
    for key in IDENTITY_KEYS:
        if have.get(key) != meta[key]:
            raise AppendRefused(
                f"{path}: {key} on disk is {have.get(key)!r}, this run's is {meta[key]!r}; "
                "a changed generator or input makes a different history, so only a full build can proceed")
    prev = dates.parse(have["as_of"])
    if as_of < prev:
        raise AppendRefused(f"{path}: as-of {as_of} is before the {prev} already on disk")
    return prev


def append(path, rows, meta, prev_as_of, as_of):
    """Add the rows that joined the history after prev_as_of, and the
    dimensions they bring. The run's window starts the day after
    prev_as_of, or at the earliest date of a row it adds, if earlier."""
    if as_of <= prev_as_of:
        raise AppendRefused(f"{path}: already at {prev_as_of}; nothing after it to add up to {as_of}")
    after = _day_end(prev_as_of) + 1
    new = {table: [r for r in rs if (t := _joined_at(table, r)) is None or t >= after]
           for table, rs in rows.items()}
    lo, _ = _span(new)
    con = sqlite3.connect(path, isolation_level=None)
    try:
        con.execute("BEGIN")
        _insert_run(con, dates.epoch(as_of), after if lo is None else min(after, lo), _day_end(as_of), as_of)
        for table in _TABLES:
            _insert(con, table, new[table])
        con.execute("UPDATE meta SET value = ? WHERE key = 'as_of'", (as_of.isoformat(),))
        con.execute("COMMIT")
    finally:
        con.close()
    return _count(new)


def _insert_meta(con, meta):
    con.executemany("INSERT INTO meta (key, value) VALUES (?, ?)", sorted(meta.items()))


def table_rows(path):
    """Every row of every data table, for comparing two files' content."""
    con = _read_only(path)
    try:
        return {t: con.execute(f"SELECT {', '.join(cols)} FROM {t} ORDER BY {', '.join(cols)}").fetchall()
                for t, cols in _TABLES.items()}
    finally:
        con.close()
