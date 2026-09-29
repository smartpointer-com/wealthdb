#!/usr/bin/env python3
"""List every identifying value a demo gold shares with another gold.

    python3 demo/disjoint.py --demo DEMO_GOLD --live OTHER_GOLD

Proof that nothing real is in the demo: both files are attached read-only
in one DuckDB session and every identifying field is intersected —
account, portfolio and instrument ids, display names, nicknames,
symbols, ISINs, CUSIPs, counterparties, descriptions, merchant and payer
signatures, cheque numbers, and (date, amount) pairs of transactions.
Text compares case- and space-insensitively. Prints counts and the
shared values, and nothing else.

A shared value is not by itself a leak: a round amount on a common date,
or a word like INTEREST PAID, can occur in any two ledgers. Each one is
for a person to read. Taxonomy labels are not compared: every gold holds
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
]


def text_query(table, column):
    norm = f"regexp_replace(lower(trim(CAST({column} AS VARCHAR))), '\\s+', ' ', 'g')"
    side = "SELECT DISTINCT {norm} AS v FROM {db}.{table} WHERE {col} IS NOT NULL AND trim(CAST({col} AS VARCHAR)) <> ''"
    return (f"SELECT v FROM ({side.format(norm=norm, db='demo', table=table, col=column)}) "
            f"INTERSECT SELECT v FROM ({side.format(norm=norm, db='live', table=table, col=column)}) ORDER BY v")


# A (day, amount) pair, the amount at cent precision and signed.
PAIR_QUERY = """
SELECT d, a FROM (SELECT DISTINCT CAST(to_timestamp(occurred_at) AS DATE) AS d, CAST(net_amount AS DECIMAL(28,2)) AS a
                    FROM demo.transactions WHERE net_amount IS NOT NULL AND net_amount <> 0)
INTERSECT
SELECT d, a FROM (SELECT DISTINCT CAST(to_timestamp(occurred_at) AS DATE) AS d, CAST(net_amount AS DECIMAL(28,2)) AS a
                    FROM live.transactions WHERE net_amount IS NOT NULL AND net_amount <> 0)
ORDER BY d, a"""


def run(demo, live, sql):
    script = (f"ATTACH '{demo}' AS demo (READ_ONLY); ATTACH '{live}' AS live (READ_ONLY);\n"
              f".mode json\n{sql};")
    out = subprocess.run(["duckdb", ":memory:"], input=script, capture_output=True,
                         text=True, check=False)
    if out.returncode != 0:
        raise SystemExit(f"duckdb failed: {out.stderr.strip()}")
    return json.loads(out.stdout) if out.stdout.strip() else []


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument("--demo", required=True, help="the demo's gold file")
    p.add_argument("--live", required=True, help="the gold file to compare against")
    p.add_argument("--show", type=int, default=20, help="shared values to print per field")
    a = p.parse_args(argv)
    if not shutil.which("duckdb"):
        print("disjoint: the duckdb command-line tool is not on PATH", file=sys.stderr)
        return 2
    for q in (a.demo, a.live):
        if "'" in q:
            print(f"disjoint: refusing a path with a quote in it: {q}", file=sys.stderr)
            return 2
    shared_total = 0
    for label, table, column in TEXT_FIELDS:
        rows = run(a.demo, a.live, text_query(table, column))
        shared_total += len(rows)
        print(f"{label:20} {len(rows):6} shared")
        for r in rows[:a.show]:
            print(f"    {r['v']}")
    pairs = run(a.demo, a.live, PAIR_QUERY)
    print(f"{'(date, amount)':20} {len(pairs):6} shared")
    for r in pairs[:a.show]:
        print(f"    {r['d']}  {r['a']}")
    print(f"\n{shared_total} shared identifying values, {len(pairs)} shared (date, amount) pairs")
    return 0


if __name__ == "__main__":
    sys.exit(main())
