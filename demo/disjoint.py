#!/usr/bin/env python3
"""List every identifying value a demo gold shares with another gold.

    python3 demo/disjoint.py --demo DEMO_GOLD --live OTHER_GOLD

Proof that nothing real is in the demo: both files are attached read-only
in one DuckDB session and every identifying field is intersected —
account, portfolio and instrument ids, display names, nicknames,
symbols, ISINs, CUSIPs, counterparties, descriptions, merchant and payer
signatures, cheque numbers, fund references and fund lines, security
keys and names, rate series ids, the funds that state figures or rate
risk of their own, the keys of companies' reported figures, the notes
of the declarations ledger's records, (date, amount) pairs of
transactions, and (maturity, coupon) pairs of bonds' terms. A table one
gold lacks is reported and skipped.
Text compares case- and space-insensitively. Prints each field's count
and its shared values, and nothing else; --show N prints at most N
values per field and says how many it left out.

A shared value is not by itself a leak: a round amount on a common date,
or a word like INTEREST PAID, can occur in any two ledgers. Each one
needs review. Taxonomy labels are not compared: every gold holds
the same vocabulary.

Runs the `duckdb` command-line tool, which must be on PATH and able to
read the engine's file format. Point --live at a copy of a gold file, or
run it while nothing is writing the file: DuckDB allows one writer, and a
reader holds the file for as long as this takes.
"""

import argparse
import json
import shutil
import subprocess
import sys

TEXT_FIELDS = [
    ("account id", "accounts", "account_external_id"),
    ("account name", "accounts", "display_name"),
    ("account nickname", "accounts", "nickname"),
    ("account category", "accounts", "account_category"),
    ("portfolio id", "portfolios", "portfolio_external_id"),
    ("portfolio name", "portfolios", "display_name"),
    ("portfolio nickname", "portfolios", "nickname"),
    ("instrument id", "instruments", "instrument_external_id"),
    ("instrument name", "instruments", "name"),
    ("symbol", "instruments", "symbol"),
    ("ISIN", "instruments", "isin"),
    ("CUSIP", "instruments", "cusip"),
    ("transaction id", "transactions", "transaction_external_id"),
    ("counterparty", "transactions", "counterparty"),
    ("description", "transactions", "description"),
    ("cheque number", "transactions", "check_number"),
    ("merchant signature", "spend_txn_enrichment", "merchant_signature"),
    ("payer signature", "income_txn_enrichment", "payer_signature"),
    ("fund reference", "fund_compositions", "fund_ref"),
    ("fund name", "fund_compositions", "fund_name"),
    ("fund line ISIN", "fund_lines", "isin"),
    ("fund line ticker", "fund_lines", "ticker"),
    ("fund line name", "fund_lines", "name"),
    ("fund counterparty", "fund_counterparties", "name"),
    ("security key", "security_facts", "security_key"),
    ("security name", "security_identities", "name"),
    ("security FIGI", "security_identities", "figi"),
    ("rate series", "rate_observations", "series_id"),
    ("characteristic fund", "fund_characteristics", "fund_ref"),
    ("period fact key", "security_period_facts", "security_key"),
    ("fund risk reference", "fund_risk", "fund_ref"),
    ("declaration note", "exposure_declarations", "note"),
    ("rate record note", "yield_declared_rates", "note"),
    ("rating record note", "yield_declared_ratings", "note"),
    ("dividend note", "yield_declared_dividends", "note"),
    ("terms record note", "yield_declared_terms", "note"),
]


def text_query(table, column):
    norm = f"regexp_replace(lower(trim(CAST({column} AS VARCHAR))), '\\s+', ' ', 'g')"
    side = "SELECT DISTINCT {norm} AS v FROM {db}.{table} WHERE {col} IS NOT NULL AND trim(CAST({col} AS VARCHAR)) <> ''"
    return (f"SELECT v FROM ({side.format(norm=norm, db='demo', table=table, col=column)}) "
            f"INTERSECT SELECT v FROM ({side.format(norm=norm, db='live', table=table, col=column)})")


def fields_query(fields):
    """The shared values of every (label, table, column) field, as
    (f, v) rows of one query, so DuckDB attaches both golds once."""
    return ("\nUNION ALL\n".join(f"SELECT '{label}' AS f, v FROM ({text_query(table, column)})"
                                  for label, table, column in fields) + "\nORDER BY f, v")


# The tables each gold holds; a gold older than a table has none to share.
TABLES_QUERY = """
SELECT table_name, COUNT(DISTINCT table_catalog) AS n FROM information_schema.tables
 WHERE table_catalog IN ('demo', 'live') GROUP BY 1"""


def pair_query(table, d, a, where):
    """The (d, a) pairs of `table` both golds hold, d and a SQL over its
    columns."""
    side = f"SELECT DISTINCT {d} AS d, {a} AS a FROM {{db}}.{table} WHERE {where}"
    return f"{side.format(db='demo')}\nINTERSECT\n{side.format(db='live')}\nORDER BY d, a"


# The pairs compared, each with the columns it reads: a transaction's day
# and its amount, at cent precision and signed; a bond's maturity and its
# coupon, at a thousandth of a percent. A renamed copy of a real holding
# shares either.
PAIRS = [
    ("(date, amount)", "transactions", ("occurred_at", "net_amount"), pair_query(
        "transactions", "CAST(to_timestamp(occurred_at) AS DATE)", "CAST(net_amount AS DECIMAL(28,2))",
        "net_amount IS NOT NULL AND net_amount <> 0")),
    ("(maturity, coupon)", "instrument_terms", ("maturity", "coupon_pct"), pair_query(
        "instrument_terms", "maturity", "CAST(coupon_pct AS DECIMAL(9,3))", "coupon_pct IS NOT NULL AND maturity IS NOT NULL")),
]


def run(demo, live, sql):
    script = (f"ATTACH '{demo}' AS demo (READ_ONLY); ATTACH '{live}' AS live (READ_ONLY);\n"
              f".mode json\n{sql};")
    out = subprocess.run(["duckdb", ":memory:"], input=script, capture_output=True,
                         text=True, check=False)
    if out.returncode != 0:
        raise SystemExit(f"duckdb failed: {out.stderr.strip()}")
    return json.loads(out.stdout) if out.stdout.strip() else []


def show(rows, limit, fmt):
    for r in rows[:limit]:
        print(f"    {fmt(r)}")
    if limit is not None and len(rows) > limit:
        print(f"    ... {len(rows) - limit} more")


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument("--demo", required=True, help="the demo's gold file")
    p.add_argument("--live", required=True, help="the gold file to compare against")
    p.add_argument("--show", type=int, default=None, help="print at most N shared values per field")
    a = p.parse_args(argv)
    if not shutil.which("duckdb"):
        print("disjoint: the duckdb command-line tool is not on PATH", file=sys.stderr)
        return 2
    for q in (a.demo, a.live):
        if "'" in q:
            print(f"disjoint: refusing a path with a quote in it: {q}", file=sys.stderr)
            return 2
    both = {r["table_name"] for r in run(a.demo, a.live, TABLES_QUERY) if r["n"] == 2}
    fields = [f for f in TEXT_FIELDS if f[1] in both]
    shared = {label: [] for label, _table, _column in fields}
    if fields:
        for r in run(a.demo, a.live, fields_query(fields)):
            shared[r["f"]].append(r["v"])
    for label, table, _column in TEXT_FIELDS:
        if label not in shared:
            print(f"{label:20} {'n/a':>6} ({table} is not in both golds)")
            continue
        print(f"{label:20} {len(shared[label]):6} shared")
        show(shared[label], a.show, str)
    shared_total = sum(len(v) for v in shared.values())
    counts = []
    for label, table, _columns, sql in PAIRS:
        if table not in both:
            print(f"{label:20} {'n/a':>6} ({table} is not in both golds)")
            continue
        pairs = run(a.demo, a.live, sql)
        print(f"{label:20} {len(pairs):6} shared")
        show(pairs, a.show, lambda r: f"{r['d']}  {r['a']}")
        counts.append(f"{len(pairs)} shared {label} pairs")
    print("\n" + ", ".join([f"{shared_total} shared identifying values", *counts]))
    return 0


if __name__ == "__main__":
    sys.exit(main())
