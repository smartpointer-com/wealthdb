"""Each Go adapter's copy of its silver schema is held to the collector's
migrations.

The adapter tests run in a container that sees only the gold module, so
each adapter keeps a copy of its silver schema under
``wealthdb/internal/silver/<kind>/testdata/``. Most copies are minimal:
they hold the tables and columns the adapter reads. This suite runs on
the host, where both trees are visible. For each copy it builds an empty
silver from the collector's migrations and checks that

* every table of the copy is a migrated table;
* every column of the copy is a column of that migrated table, with the
  same declared type;
* the migrated tables the copy leaves out are exactly the ones listed
  for it below, each with a reason.

A copy may leave out columns: an adapter reads a subset. A new migrated
table fails the last check until it is listed or added to the copy.
"""
from __future__ import annotations

import sqlite3
from dataclasses import dataclass, field
from pathlib import Path

import pytest

from collectorkit import silver

REPO = Path(__file__).resolve().parents[3]
GO_SILVER = REPO / "wealthdb" / "internal" / "silver"
COLLECTORS = REPO / "collectors"

VERSION = "the schema version; no adapter reads it"
BOOKKEEPING = "loader bookkeeping; the adapter does not read it"
UNREAD = "the adapter does not read it"


@dataclass(frozen=True)
class Copy:
    collector: str
    # Under GO_SILVER, applied in order to one database.
    scripts: tuple[str, ...]
    # Migrated tables the copy leaves out, and why.
    omitted: dict[str, str] = field(default_factory=dict)


COPIES = {
    "amex": Copy("amex", ("amex/testdata/silver_schema.sql",), {
        "schema_meta": VERSION, "parser_generations": BOOKKEEPING}),
    "angellist": Copy("angellist", ("angellist/testdata/silver_schema.sql",), {
        "commitments": UNREAD, "portfolio_timeseries": UNREAD,
        "tax_documents": UNREAD, "vehicles": UNREAD}),
    "carta": Copy("carta", ("carta/testdata/silver_schema.sql",), {
        "k1_capital_accounts": "read; basis_test.go creates it inline"}),
    "chase": Copy("chase", ("chase/testdata/silver_schema.sql",), {
        "loader_state": BOOKKEEPING, "parser_generations": BOOKKEEPING}),
    "curves": Copy("curves", ("curves/testdata/silver_schema.sql",), {
        "schema_meta": VERSION}),
    "equityzen": Copy("equityzen", ("equityzen/testdata/silver_schema.sql",), {
        "schema_meta": VERSION, "capital_account_statements": UNREAD,
        "k1_documents": UNREAD, "tax_documents": UNREAD}),
    "fidelity-web": Copy("fidelity-web", (
        "fidelity/testdata/silver_schema.sql",
        "fidelity/testdata/lots_schema.sql"), {
        "schema_meta": VERSION, "parser_generations": BOOKKEEPING,
        "documents": UNREAD}),
    "firstcitizens": Copy("firstcitizens", ("firstcitizens/testdata/silver_schema.sql",), {
        "parsed_statements": BOOKKEEPING, "parser_generations": BOOKKEEPING}),
    "fred": Copy("fred", ("fred/testdata/silver_schema.sql",), {
        "schema_meta": VERSION}),
    "manual": Copy("manual", ("manual/testdata/silver_schema.sql",)),
    "plaid": Copy("plaid", ("plaid/testdata/silver_schema.sql",)),
    "raiffeisen_at": Copy("raiffeisen_at", ("raiffeisen_at/testdata/silver_schema.sql",), {
        "run_windows": BOOKKEEPING}),
    "refdata": Copy("refdata", ("refdata/testdata/silver_schema.sql",)),
    "relevate": Copy("relevate", ("relevate/testdata/silver_schema.sql",), {
        "schema_meta": VERSION, "parser_generations": BOOKKEEPING,
        "documents": UNREAD, "performance_points": UNREAD,
        "historical_position_snapshots": "read by historical.go; no test holds it",
        "historical_cash_balances": "read by historical.go; no test holds it"}),
    "schwab-api": Copy("schwab-api", ("schwab/testdata/silver_schema.sql",), {
        "schema_meta": VERSION, "open_orders": UNREAD, "user_preference": UNREAD}),
    "schwab-web": Copy("schwab-web", ("schwab/testdata/web_silver_schema.sql",), {
        "schema_meta": VERSION, "parser_generations": BOOKKEEPING,
        "parsed_documents": BOOKKEEPING, "documents": UNREAD,
        "cost_basis_methods": UNREAD}),
    "swissquote": Copy("swissquote", ("swissquote/testdata/silver_schema.sql",), {
        "schema_meta": VERSION, "documents": UNREAD}),
    "ubs-psn": Copy("ubs-psn", ("ubs/testdata/silver_schema.sql",), {
        "schema_meta": VERSION, "cash_account_pricing": UNREAD,
        "portfolio_performance": UNREAD}),
    "viac": Copy("viac", ("viac/testdata/silver_schema.sql",), {
        "schema_meta": VERSION, "parser_generations": BOOKKEEPING,
        "documents": UNREAD, "wealth_history": UNREAD}),
}

# Where a copy differs from the migrations today, the problems it has.
# A fix to the copy or to the migrations fails the test until its entry
# goes.
DRIFT = {
    # Migration 0005 renamed the column to account_product.
    "swissquote": ["accounts.account_type is in no migration"],
    # The adapter reads the column when a silver has it.
    "relevate": ["accounts.management_style is in no migration"],
}

# Schemas under testdata/ that no case above checks, and why.
EXEMPT = {
    "cointracking/testdata/silver_schema.sql":
        "DuckDB; its migrations do not run on SQLite",
    "synthetic/testdata/silver_schema.sql":
        "no collector writes it; fred and refdata check their tables in it",
}
# The ubs adapter also reads the ubs-web silver; its tests build that
# schema inline, so there is no copy here to check.


def _columns(conn: sqlite3.Connection) -> dict[str, dict[str, str]]:
    """Each table's columns and their declared types."""
    tables = [r[0] for r in conn.execute(
        "SELECT name FROM sqlite_master WHERE type = 'table'"
        " AND name NOT LIKE 'sqlite\\_%' ESCAPE '\\' ORDER BY name")]
    return {t: {c[1]: c[2] for c in conn.execute(f'PRAGMA table_info("{t}")')}
            for t in tables}


def _migrated(collector: str) -> dict[str, dict[str, str]]:
    conn = sqlite3.connect(":memory:")
    try:
        silver.apply_migrations(conn, COLLECTORS / collector / "migrations")
        return _columns(conn)
    finally:
        conn.close()


def _copy(scripts: tuple[str, ...]) -> dict[str, dict[str, str]]:
    conn = sqlite3.connect(":memory:")
    try:
        for script in scripts:
            conn.executescript((GO_SILVER / script).read_text(encoding="utf-8"))
        return _columns(conn)
    finally:
        conn.close()


@pytest.mark.parametrize("name", sorted(COPIES))
def test_the_go_copy_matches_the_migrations(name):
    copy = COPIES[name]
    migrated, held = _migrated(copy.collector), _copy(copy.scripts)
    problems = [f"table {t} is in no migration" for t in held if t not in migrated]
    for table, columns in held.items():
        for col, typ in columns.items():
            want = migrated.get(table, {}).get(col)
            if table in migrated and want is None:
                problems.append(f"{table}.{col} is in no migration")
            elif want is not None and want != typ:
                problems.append(f"{table}.{col} is {typ!r}, migrated {want!r}")
    left_out = sorted(set(migrated) - set(held))
    assert problems == DRIFT.get(name, [])
    assert left_out == sorted(copy.omitted)


def test_every_schema_copy_is_checked_or_exempt():
    checked = {s for c in COPIES.values() for s in c.scripts}
    found = {str(p.relative_to(GO_SILVER))
             for p in GO_SILVER.glob("*/testdata/*.sql")}
    assert sorted(found - checked - set(EXEMPT)) == []
    assert sorted((checked | set(EXEMPT)) - found) == []
