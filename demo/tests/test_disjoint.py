"""demo/disjoint.py compares columns gold has, and every table of
reference records.

A field naming a table or a column that no gold migration creates would
fail the comparison's query, or, for a table, be skipped as one the two
golds do not share. A new table of records that name a fund, a security
or a rate series would go uncompared. Reading the migrations keeps the
field list honest without a gold file.
"""

import pathlib
import re
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))

import disjoint  # noqa: E402

MIGRATIONS = pathlib.Path(__file__).resolve().parents[2] / "wealthdb" / "internal" / "gold" / "migrations"
_NOT_A_COLUMN = {"PRIMARY", "UNIQUE", "FOREIGN", "CHECK", "CONSTRAINT"}
# The columns that name a fund, a security or a rate series.
_IDENTIFIERS = {"fund_ref", "security_key", "series_id"}
# Tables of reference records whose identifiers a compared table holds:
# a fund's attributes come beside its compositions.
_COMPARED_ELSEWHERE = {"fund_attributes"}


def gold_columns():
    """{table: columns} over every CREATE TABLE and ADD COLUMN of the
    migrations."""
    out = {}
    for path in sorted(MIGRATIONS.glob("*.sql")):
        text = re.sub(r"--[^\n]*", "", path.read_text())
        for table, body in re.findall(r"CREATE TABLE (?:IF NOT EXISTS )?(\w+)\s*\((.*?)\n\s*\);", text, re.S):
            for line in body.splitlines():
                m = re.match(r"\s*(\w+)\s+\w", line)
                if m and m.group(1).upper() not in _NOT_A_COLUMN:
                    out.setdefault(table, set()).add(m.group(1))
        for table, column in re.findall(r"ALTER TABLE (\w+) ADD COLUMN (?:IF NOT EXISTS )?(\w+)", text):
            out.setdefault(table, set()).add(column)
    return out


class TestFields(unittest.TestCase):
    def test_every_field_is_a_gold_column(self):
        cols = gold_columns()
        for label, table, column in disjoint.TEXT_FIELDS:
            self.assertIn(column, cols.get(table, set()), f"{label}: {table}.{column}")

    def test_every_table_of_reference_records_is_compared(self):
        """A table of records a source states for a day (it has
        silver_source_id and as_of) that names a fund, a security or a
        rate series has a field. The config blocks and the exposure pass
        fill the other tables that name one."""
        tables = {table for table, cols in gold_columns().items()
                  if {"silver_source_id", "as_of"} <= cols and cols & _IDENTIFIERS}
        self.assertIn("fund_compositions", tables)
        compared = {table for _, table, _ in disjoint.TEXT_FIELDS}
        self.assertLessEqual(tables - _COMPARED_ELSEWHERE, compared)

    def test_every_pair_reads_gold_columns(self):
        cols = gold_columns()
        for label, table, columns, sql in disjoint.PAIRS:
            self.assertLessEqual(set(columns), cols.get(table, set()), label)
            for column in columns:
                self.assertIn(column, sql, label)

    def test_labels_fit_the_report_column(self):
        for label, *_ in disjoint.TEXT_FIELDS + disjoint.PAIRS:
            self.assertLessEqual(len(label), 20, label)


if __name__ == "__main__":
    unittest.main()
