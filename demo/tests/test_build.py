"""The build contract: deterministic, append-only, and safe about where
it writes."""

import datetime as dt
import json
import pathlib
import shutil
import sqlite3
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))

import generate  # noqa: E402
from demohouse import config, keyed, silver, spec  # noqa: E402

SEED = "test-seed"
EARLY, LATE = dt.date(2024, 2, 10), dt.date(2024, 5, 20)


def files(root):
    return {p.relative_to(root).as_posix(): p.read_bytes() for p in sorted(root.rglob("*")) if p.is_file()}


class TempRoot(unittest.TestCase):
    def setUp(self):
        self.tmp = pathlib.Path(tempfile.mkdtemp(prefix="demo-test-"))
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)

    def root(self, name):
        return self.tmp / name


class TestKeyedRandomness(unittest.TestCase):
    def test_same_keys_same_draws(self):
        a = [keyed.normal(keyed.rng("s", "stream", "2024-01-02")) for _ in range(3)]
        b = [keyed.normal(keyed.rng("s", "stream", "2024-01-02")) for _ in range(3)]
        self.assertEqual(a, b)

    def test_different_keys_differ(self):
        self.assertNotEqual(keyed.rng("s", "stream", 1).random(), keyed.rng("s", "stream", 2).random())
        self.assertNotEqual(keyed.rng("s", "a", 1).random(), keyed.rng("t", "a", 1).random())

    def test_normal_moments(self):
        xs = [float(keyed.normal(keyed.rng("moments", i))) for i in range(4000)]
        mean = sum(xs) / len(xs)
        var = sum((x - mean) ** 2 for x in xs) / len(xs)
        self.assertLess(abs(mean), 0.06)
        self.assertLess(abs(var - 1), 0.08)


class TestDeterminism(TempRoot):
    def test_two_builds_are_byte_identical(self):
        generate.build(self.root("a"), LATE, SEED)
        generate.build(self.root("b"), LATE, SEED)
        self.assertEqual(files(self.root("a")), files(self.root("b")))

    def test_another_seed_is_another_household(self):
        generate.build(self.root("a"), EARLY, SEED)
        generate.build(self.root("b"), EARLY, SEED + "-2")
        self.assertNotEqual(files(self.root("a"))["silver/quayvane.db"],
                            files(self.root("b"))["silver/quayvane.db"])


class TestAppend(TempRoot):
    def test_append_equals_a_full_build(self):
        full, rolled = self.root("full"), self.root("rolled")
        generate.build(full, LATE, SEED)
        generate.build(rolled, EARLY, SEED)
        generate.build(rolled, LATE, SEED, append=True)
        for db in sorted((full / "silver").glob("*.db")):
            self.assertEqual(silver.table_rows(db), silver.table_rows(rolled / "silver" / db.name), db.name)
        for name in ("wealthdb.cfg", "overrides/equity_transfers.csv", "overrides/spending_pins.csv",
                     "overrides/transfer_overrides.csv"):
            self.assertEqual((full / name).read_bytes(), (rolled / name).read_bytes(), name)

    def test_append_rewrites_nothing_already_on_disk(self):
        root = self.root("r")
        generate.build(root, EARLY, SEED)
        before = {db.name: silver.table_rows(db) for db in (root / "silver").glob("*.db")}
        generate.build(root, LATE, SEED, append=True)
        for db in (root / "silver").glob("*.db"):
            after = silver.table_rows(db)
            for table, rows in before[db.name].items():
                self.assertTrue(set(rows) <= set(after[table]), f"{db.name}.{table} lost a row")

    def test_the_new_run_windows_exactly_the_new_days(self):
        root = self.root("r")
        generate.build(root, EARLY, SEED)
        generate.build(root, LATE, SEED, append=True)
        con = sqlite3.connect(root / "silver" / "brindlecove.db")
        runs = con.execute("SELECT change_number, window_start, window_end, as_of FROM dump_runs ORDER BY 1").fetchall()
        self.assertEqual(len(runs), 2)
        (cn1, _, end1, _), (cn2, start2, end2, as_of2) = runs
        self.assertLess(cn1, cn2)
        self.assertEqual(start2, end1 + 1)
        self.assertEqual(as_of2, LATE.isoformat())
        lo, hi = con.execute("""SELECT MIN(t), MAX(t) FROM (
            SELECT snapshot_at t FROM positions UNION ALL SELECT snapshot_at FROM cash_balances
            UNION ALL SELECT occurred_at FROM transactions) WHERE t > ?""", (end1,)).fetchone()
        self.assertGreaterEqual(lo, start2)
        self.assertLessEqual(hi, end2)

    def test_append_at_the_same_as_of_adds_nothing(self):
        root = self.root("r")
        generate.build(root, EARLY, SEED)
        before = files(root)
        lines = generate.build(root, EARLY, SEED, append=True)
        self.assertIn("nothing to append", lines[0])
        self.assertEqual(before, files(root))

    def test_append_refuses_an_earlier_as_of(self):
        root = self.root("r")
        generate.build(root, LATE, SEED)
        with self.assertRaises(silver.AppendRefused):
            generate.build(root, EARLY, SEED, append=True)

    def test_append_refuses_another_seed(self):
        root = self.root("r")
        generate.build(root, EARLY, SEED)
        with self.assertRaises(silver.AppendRefused):
            generate.build(root, LATE, "other", append=True)

    def test_append_refuses_changed_inputs(self):
        root = self.root("r")
        generate.build(root, EARLY, SEED)
        inputs = spec.load()
        inputs.spec_hash = "0" * 64
        with self.assertRaises(silver.AppendRefused):
            generate.build(root, LATE, SEED, append=True, inputs=inputs)

    def test_append_refuses_a_findings_build(self):
        root = self.root("r")
        generate.build(root, EARLY, SEED, findings=True)
        with self.assertRaises(generate.Refused):
            generate.build(root, LATE, SEED, append=True, findings=True)
        with self.assertRaises(silver.AppendRefused):
            generate.build(root, LATE, SEED, append=True)

    def test_append_needs_a_build_to_extend(self):
        with self.assertRaises(silver.AppendRefused):
            generate.build(self.root("empty"), LATE, SEED, append=True)


class TestRootSafety(TempRoot):
    def test_refuses_a_root_that_looks_live(self):
        for marker in ("wealthdb.db", "wealthdb.cfg", "silver"):
            root = self.root(marker.replace(".", "-"))
            root.mkdir()
            target = root / marker
            if marker == "silver":
                target.mkdir()
            else:
                target.write_text("not a demo")
            with self.assertRaises(generate.Refused):
                generate.build(root, EARLY, SEED)
            self.assertFalse((root / config.DEMO_MARKER).exists())

    def test_marks_the_root_it_writes(self):
        root = self.root("new")
        generate.build(root, EARLY, SEED)
        self.assertTrue((root / config.DEMO_MARKER).exists())
        generate.build(root, EARLY, SEED)  # a marked root may be rebuilt

    def test_the_config_names_nothing_outside_the_root(self):
        root = self.root("r")
        generate.build(root, EARLY, SEED)
        cfg = json.loads((root / "wealthdb.cfg").read_text())
        paths = [cfg["gold_db"], cfg["equity_transfers"], cfg["spending"]["pins"],
                 cfg["spending"]["transfer_overrides"]] + [s["path"] for s in cfg["silver_sources"]]
        for path in paths:
            self.assertFalse(path.startswith(("/", "~")) or "$" in path or ".." in path, path)


if __name__ == "__main__":
    unittest.main()
