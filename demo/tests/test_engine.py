"""A short demo household, loaded through the engine.

Builds the household's first months into scratch demo roots, runs the
real `wealthdb init` and `load -a` on them through the engine image, and
reads the reports the demo is meant to keep clean. A second case appends
a month to a loaded root, loads only the new days, and requires every
report to match a gold built from scratch at the later date. The engine
runs with a scratch root as its data root and its config dir, so it sees
nothing else.

Needs Docker and the engine image, so it runs only when
WEALTHDB_DEMO_TEST_ROOT names a directory for the scratch roots
(`make test-demo` sets it, under the cache dir, where Docker can see
it). The directory is the tests' own: it carries a marker, and a
directory that exists without one is left alone. WEALTHDB_BIN names the
engine wrapper.
"""

import csv
import datetime as dt
import io
import os
import pathlib
import shutil
import subprocess
import sys
import unittest
from decimal import Decimal

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))

import generate  # noqa: E402

PARENT = os.environ.get("WEALTHDB_DEMO_TEST_ROOT")
MARKER = ".wealthdb-demo-tests"
FIRST, SECOND = dt.date(2023, 9, 30), dt.date(2023, 10, 31)
SEED = "harlow-19"


def scratch(name):
    """A fresh demo root under the tests' own directory."""
    parent = pathlib.Path(PARENT)
    if parent.exists() and not (parent / MARKER).exists():
        raise unittest.SkipTest(f"{parent} exists and is not the demo tests' directory")
    parent.mkdir(parents=True, exist_ok=True)
    (parent / MARKER).write_text("Scratch demo roots of demo/tests/test_engine.py.\n")
    root = parent / name
    if root.exists():
        shutil.rmtree(root)
    return root


def engine(root, *args, read_only=False):
    env = dict(os.environ, WEALTHDB_DATA_ROOT=str(root), XDG_CONFIG_HOME=str(root))
    wrapper = os.environ.get("WEALTHDB_BIN") or str(pathlib.Path(__file__).resolve().parents[2]
                                                     / "wealthdb" / "wealthdb")
    cmd = [wrapper] + (["-r"] if read_only else []) + ["-c", str(root / "wealthdb.cfg"), *args]
    out = subprocess.run(cmd, env=env, capture_output=True, text=True, timeout=600)
    if out.returncode != 0:
        raise AssertionError(f"{' '.join(args)} failed ({out.returncode}): {out.stderr or out.stdout}")
    return out.stdout


def build_and_load(name, as_of):
    root = scratch(name)
    generate.build(root, as_of, SEED)
    engine(root, "init")
    return root, engine(root, "load", "-a")


def report(root, *args):
    return list(csv.DictReader(io.StringIO(engine(root, *args, "-f", "csv", read_only=True))))


@unittest.skipUnless(PARENT, "WEALTHDB_DEMO_TEST_ROOT is not set (make test-demo sets it)")
class TestThroughTheEngine(unittest.TestCase):
    """The reports a freshly built demo is meant to keep clean."""

    @classmethod
    def setUpClass(cls):
        cls.root, cls.load = build_and_load("short", FIRST)

    def test_every_source_loads(self):
        for src in ("fx", "brindlecove", "quayvane", "tamberlow", "aubervane", "driftwren", "homestead"):
            self.assertIn(f"load: {src}:", self.load)
        self.assertNotIn("*", engine(self.root, "status", read_only=True))

    def test_nothing_uncategorised(self):
        rows = report(self.root, "spending", "categories", "-", "today", "--period", "total")
        self.assertTrue(rows)
        self.assertFalse([r for r in rows if "uncategor" in r["category"].lower()])
        rows = report(self.root, "income", "types", "-", "today", "--period", "total")
        self.assertFalse([r for r in rows if "uncategor" in r["type"].lower()])

    def test_cash_coverage_closes(self):
        rows = report(self.root, "cashflow", "coverage", "-", "today")
        self.assertTrue(rows)
        for r in rows:
            if r["status"] == "measured":
                self.assertEqual(Decimal(r["gap"]), 0, r)
            else:
                # Currency conversions and in-kind moves leave a gap no sign
                # explains, and an account born inside a month has no
                # opening balance.
                self.assertIn(r["status"], ("obscured", "opening"), r)

    def test_holdings_reconcile(self):
        """Every grain sums to the global total, within a cent of currency
        rounding per row."""
        total = Decimal(report(self.root, "holdings", "global")[0]["total_value_USD"])
        for view in ("sources", "portfolios", "accounts"):
            rows = report(self.root, "holdings", view)
            got = sum(Decimal(r["total_value_USD"] or 0) for r in rows)
            self.assertLessEqual(abs(got - total), Decimal("0.01") * len(rows), view)
        rows = report(self.root, "holdings", "positions", "--with-cash")
        got = sum(Decimal(r["value_USD"] or 0) for r in rows)
        self.assertLessEqual(abs(got - total), Decimal("0.01") * len(rows), "positions --with-cash")

    def test_nothing_unpaired(self):
        rows = report(self.root, "cashflow", "flows", "-", "today", "--period", "total", "--level", "group")
        self.assertFalse([r for r in rows if "unpaired" in r["class"].lower()])

    def test_returns_carry_no_stale_or_empty_tags(self):
        rows = report(self.root, "returns", "accounts", FIRST.replace(day=1).isoformat(), FIRST.isoformat(),
                      "--method", "both", "--period", "monthly")
        self.assertTrue(rows)
        for r in rows:
            for bad in ("stale_snapshot", "empty_bucket", "pre_fx_history", "unknown_adapter_policy"):
                self.assertNotIn(bad, r["quality"], r)


@unittest.skipUnless(PARENT, "WEALTHDB_DEMO_TEST_ROOT is not set (make test-demo sets it)")
class TestRollForward(unittest.TestCase):
    """An append loads only the new days, and the gold it reaches reads
    the same as one built from scratch at the later date."""

    @classmethod
    def setUpClass(cls):
        cls.rolled, _ = build_and_load("rolled", FIRST)
        generate.build(cls.rolled, SECOND, SEED, append=True)
        cls.waiting = engine(cls.rolled, "status", read_only=True)
        cls.load = engine(cls.rolled, "load", "-a")
        cls.fresh, _ = build_and_load("fresh", SECOND)

    def test_status_shows_the_append_waiting(self):
        self.assertIn("*", self.waiting)
        self.assertNotIn("*", engine(self.rolled, "status", read_only=True))

    def test_the_load_takes_only_the_appended_run(self):
        self.assertIn("watermark 1696032000 → 1698710400", self.load)

    def test_every_report_matches_a_fresh_build(self):
        """Compared as row sets: some reports order ties arbitrarily."""
        reports = [
            ("holdings", "accounts", "-C", "all"), ("holdings", "positions", "--with-cash", "-C", "all"),
            ("returns", "accounts", "--method", "both", "--period", "monthly"),
            ("returns", "global", "--method", "both", "--period", "total"),
            ("spending", "transactions", "-", "today"), ("income", "transactions", "-", "today"),
            ("cashflow", "flows", "-", "today", "--level", "group"), ("cashflow", "coverage", "-", "today"),
            ("transactions", "-", "today", "-C", "all"),
        ]
        for args in reports:
            rolled = sorted(engine(self.rolled, *args, "-f", "csv", read_only=True).splitlines())
            fresh = sorted(engine(self.fresh, *args, "-f", "csv", read_only=True).splitlines())
            self.assertEqual(rolled, fresh, " ".join(args))


if __name__ == "__main__":
    unittest.main()
