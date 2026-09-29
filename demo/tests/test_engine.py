"""A short demo household, loaded through the engine.

Builds the household's first months into a scratch root, runs the real
`wealthdb init` and `load -a` on it through the engine image, appends a
month and loads again, and reads the reports the demo is meant to keep
clean. The engine runs with the scratch root as its data root and its
config dir, so it sees nothing else.

Needs Docker and the engine image, so it runs only when
WEALTHDB_DEMO_TEST_ROOT names the scratch root (`make test-demo` sets it,
under the cache dir, where Docker can see it). WEALTHDB_BIN names the
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
from demohouse import config  # noqa: E402

ROOT = os.environ.get("WEALTHDB_DEMO_TEST_ROOT")
FIRST, SECOND = dt.date(2023, 9, 30), dt.date(2023, 10, 31)
SEED = "harlow-19"


@unittest.skipUnless(ROOT, "WEALTHDB_DEMO_TEST_ROOT is not set (make test-demo sets it)")
class TestThroughTheEngine(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.root = pathlib.Path(ROOT)
        if cls.root.exists():
            if not (cls.root / config.DEMO_MARKER).exists():
                raise unittest.SkipTest(f"{cls.root} exists and is not a demo root")
            shutil.rmtree(cls.root)
        generate.build(cls.root, FIRST, SEED)
        cls.run_engine("init")
        cls.first_load = cls.run_engine("load", "-a")

    @classmethod
    def run_engine(cls, *args, read_only=False, root=None):
        root = root or cls.root
        env = dict(os.environ, WEALTHDB_DATA_ROOT=str(root), XDG_CONFIG_HOME=str(root))
        wrapper = os.environ.get("WEALTHDB_BIN") or str(pathlib.Path(__file__).resolve().parents[2]
                                                         / "wealthdb" / "wealthdb")
        cmd = [wrapper] + (["-r"] if read_only else []) + ["-c", str(root / "wealthdb.cfg"), *args]
        out = subprocess.run(cmd, env=env, capture_output=True, text=True, timeout=600)
        if out.returncode != 0:
            raise AssertionError(f"{' '.join(args)} failed ({out.returncode}): {out.stderr or out.stdout}")
        return out.stdout

    def report(self, *args):
        return list(csv.DictReader(io.StringIO(self.run_engine(*args, "-f", "csv", read_only=True))))

    def test_1_every_source_loads(self):
        for src in ("fx", "brindlecove", "quayvane", "tamberlow", "aubervane", "driftwren", "homestead"):
            self.assertIn(f"load: {src}:", self.first_load)
        self.assertNotIn("*", self.run_engine("status", read_only=True))

    def test_2_nothing_uncategorised(self):
        rows = self.report("spending", "categories", "-", "today", "--period", "total")
        self.assertTrue(rows)
        self.assertFalse([r for r in rows if "uncategor" in r["category"].lower()])
        rows = self.report("income", "types", "-", "today", "--period", "total")
        self.assertFalse([r for r in rows if "uncategor" in r["type"].lower()])

    def test_3_cash_coverage_closes(self):
        rows = self.report("cashflow", "coverage", "-", "today")
        self.assertTrue(rows)
        for r in rows:
            if r["status"] == "measured":
                self.assertEqual(Decimal(r["gap"]), 0, r)
            else:
                # Currency conversions leave a gap no sign explains, and an
                # account born inside a month has no opening balance.
                self.assertIn(r["status"], ("obscured", "opening"), r)

    def test_4_holdings_reconcile(self):
        """Every grain sums to the global total, within a cent of currency
        rounding per row."""
        total = Decimal(self.report("holdings", "global")[0]["total_value_USD"])
        grains = [("sources", "total_value_USD"), ("portfolios", "total_value_USD"),
                  ("accounts", "total_value_USD")]
        for view, column in grains:
            rows = self.report("holdings", view)
            got = sum(Decimal(r[column] or 0) for r in rows)
            self.assertLessEqual(abs(got - total), Decimal("0.01") * len(rows), view)
        rows = self.report("holdings", "positions", "--with-cash")
        got = sum(Decimal(r["value_USD"] or 0) for r in rows)
        self.assertLessEqual(abs(got - total), Decimal("0.01") * len(rows), "positions --with-cash")

    def test_5_nothing_unpaired(self):
        rows = self.report("cashflow", "flows", "-", "today", "--period", "total", "--level", "group")
        self.assertFalse([r for r in rows if "unpaired" in r["class"].lower()])

    def test_6_returns_carry_no_stale_or_empty_tags(self):
        rows = self.report("returns", "accounts", FIRST.replace(day=1).isoformat(), FIRST.isoformat(),
                           "--method", "both", "--period", "monthly")
        self.assertTrue(rows)
        for r in rows:
            for bad in ("stale_snapshot", "empty_bucket", "pre_fx_history", "unknown_adapter_policy"):
                self.assertNotIn(bad, r["quality"], r)

    def test_7_an_append_loads_only_the_new_days(self):
        generate.build(self.root, SECOND, SEED, append=True)
        self.assertIn("*", self.run_engine("status", read_only=True))
        out = self.run_engine("load", "-a")
        self.assertIn("watermark 1696032000 → 1698710400", out)
        self.assertNotIn("*", self.run_engine("status", read_only=True))
        rows = self.report("cashflow", "coverage", "2023-10", "2023-10")
        self.assertTrue(rows)
        self.assertFalse([r for r in rows if r["status"] == "measured" and Decimal(r["gap"]) != 0])

    def test_8_the_rolled_gold_matches_a_fresh_one(self):
        """After the append, every report reads the same as a gold built
        from scratch at the later date (compared as row sets: some
        reports order ties arbitrarily)."""
        fresh = self.root.with_name(self.root.name + "-fresh")
        if fresh.exists():
            shutil.rmtree(fresh)
        self.addCleanup(shutil.rmtree, fresh, ignore_errors=True)
        generate.build(fresh, SECOND, SEED)
        self.run_engine("init", root=fresh)
        self.run_engine("load", "-a", root=fresh)
        reports = [
            ("holdings", "accounts", "-C", "all"), ("holdings", "positions", "--with-cash", "-C", "all"),
            ("returns", "accounts", "--method", "both", "--period", "monthly"),
            ("returns", "global", "--method", "both", "--period", "total"),
            ("spending", "transactions", "-", "today"), ("income", "transactions", "-", "today"),
            ("cashflow", "flows", "-", "today", "--level", "group"), ("cashflow", "coverage", "-", "today"),
            ("transactions", "-", "today", "-C", "all"),
        ]
        for args in reports:
            rolled = sorted(self.run_engine(*args, "-f", "csv", read_only=True).splitlines())
            built = sorted(self.run_engine(*args, "-f", "csv", read_only=True, root=fresh).splitlines())
            self.assertEqual(rolled, built, " ".join(args))


if __name__ == "__main__":
    unittest.main()
