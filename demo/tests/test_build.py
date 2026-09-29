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
from decimal import Decimal

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))

import generate  # noqa: E402
from demohouse import config, dates, keyed, silver, spec  # noqa: E402
from demohouse.household import Simulation  # noqa: E402

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
            self.assertEqual(silver.recorded(db), silver.recorded(rolled / "silver" / db.name), db.name)
        for name in ("wealthdb.cfg", "overrides/equity_transfers.csv", "overrides/spending_pins.csv",
                     "overrides/transfer_overrides.csv"):
            self.assertEqual((full / name).read_bytes(), (rolled / name).read_bytes(), name)

    def test_an_append_across_an_opening_and_a_rename_equals_a_full_build(self):
        # The venture account opens and a fund is renamed inside the
        # appended days, so the append adds dimension rows, not facts alone.
        before, after = dt.date(2024, 12, 20), dt.date(2025, 4, 10)
        full, rolled = self.root("full"), self.root("rolled")
        generate.build(full, after, SEED)
        generate.build(rolled, before, SEED)
        generate.build(rolled, after, SEED, append=True)
        for db in sorted((full / "silver").glob("*.db")):
            self.assertEqual(silver.table_rows(db), silver.table_rows(rolled / "silver" / db.name), db.name)
        con = sqlite3.connect(rolled / "silver" / "emberwright.db")
        self.assertEqual(con.execute("SELECT COUNT(*) FROM accounts").fetchone()[0], 1)
        con.close()

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
        self.addCleanup(con.close)
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
        with self.assertRaisesRegex(silver.AppendRefused, "findings build"):
            generate.build(root, LATE, SEED, append=True)

    def test_append_refuses_a_changed_generator(self):
        root = self.root("r")
        generate.build(root, EARLY, SEED)
        inputs = spec.load()
        inputs.code_hash = "0" * 64
        with self.assertRaisesRegex(silver.AppendRefused, "generator_hash"):
            generate.build(root, LATE, SEED, append=True, inputs=inputs)

    def test_an_append_cut_short_completes_on_the_next_run(self):
        full, rolled = self.root("full"), self.root("rolled")
        generate.build(full, LATE, SEED)
        generate.build(rolled, EARLY, SEED)
        # One file already appended, as if the run stopped after it.
        inputs = spec.load()
        meta = silver.identity(inputs, SEED, False)
        sim = Simulation(inputs, SEED, LATE).run()
        silver.append(rolled / "silver" / "brindlecove.db", silver.source_rows(sim, "brindlecove", LATE),
                      meta, EARLY, LATE)
        lines = generate.build(rolled, LATE, SEED, append=True)
        self.assertIn(f"brindlecove: already at {LATE}", lines)
        for db in sorted((full / "silver").glob("*.db")):
            self.assertEqual(silver.table_rows(db), silver.table_rows(rolled / "silver" / db.name), db.name)

    def test_append_refuses_a_run_that_does_not_move_forward(self):
        root = self.root("r")
        generate.build(root, EARLY, SEED)
        inputs = spec.load()
        sim = Simulation(inputs, SEED, EARLY).run()
        with self.assertRaisesRegex(silver.AppendRefused, "already at"):
            silver.append(root / "silver" / "fx.db", silver.source_rows(sim, "fx", EARLY),
                          silver.identity(inputs, SEED, False), EARLY, EARLY)

    def test_append_needs_a_build_to_extend(self):
        with self.assertRaises(silver.AppendRefused):
            generate.build(self.root("empty"), LATE, SEED, append=True)


class TestFindings(unittest.TestCase):
    AS_OF = dt.date(2025, 9, 30)

    @classmethod
    def setUpClass(cls):
        cls.tmp = pathlib.Path(tempfile.mkdtemp(prefix="demo-find-"))
        generate.build(cls.tmp / "root", cls.AS_OF, SEED, findings=True)
        cls.findings = spec.load().spec["findings"]

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.tmp, ignore_errors=True)

    def query(self, source, sql, *args):
        con = sqlite3.connect(self.tmp / "root" / "silver" / f"{source}.db")
        try:
            return con.execute(sql, args).fetchall()
        finally:
            con.close()

    def test_unmapped_purchases_carry_no_provider_value(self):
        names = self.findings["unmapped_merchants"]
        rows = self.query("brindlecove", f"""SELECT provider_category FROM transactions
            WHERE description IN ({",".join("?" * len(names))})""", *names)
        self.assertGreater(len(rows), 10)
        self.assertEqual({r[0] for r in rows}, {None})

    def test_one_transfer_goes_to_an_undeclared_brokerage(self):
        ob = self.findings["old_brokerage"]
        rows = self.query("brindlecove", "SELECT occurred_at, net_amount FROM transactions WHERE description = ?",
                          ob["descriptor"])
        self.assertEqual([(dates.day_of(t), Decimal(a)) for t, a in rows],
                         [(dates.parse(ob["date"]), -Decimal(ob["amount"]))])

    def test_one_statement_month_is_missing(self):
        ms = self.findings["missing_statement"]
        months = {dates.day_of(t).strftime("%Y-%m") for (t,) in self.query(
            "driftwren", "SELECT snapshot_at FROM cash_balances WHERE account_id = ?", ms["account"])}
        year, month = map(int, ms["month"].split("-"))
        self.assertNotIn(ms["month"], months)
        self.assertIn(f"{year}-{month - 1:02d}", months)
        self.assertIn(f"{year}-{month + 1:02d}", months)

    def test_the_venture_marks_stop_before_as_of(self):
        last = self.query("emberwright", "SELECT MAX(snapshot_at) FROM positions")[0][0]
        self.assertEqual(dates.day_of(last), self.AS_OF - dt.timedelta(days=self.findings["stale_mark_days"]))


class TestRootSafety(TempRoot):
    def test_refuses_an_unmarked_root_that_holds_anything(self):
        for name in ("wealthdb.db", "wealthdb.cfg", "silver", "web", "notes.txt"):
            root = self.root(name.replace(".", "-"))
            root.mkdir()
            target = root / name
            if "." in name:
                target.write_text("not a demo")
            else:
                target.mkdir()
            with self.assertRaisesRegex(generate.Refused, "not a demo root"):
                generate.build(root, EARLY, SEED)
            self.assertFalse((root / config.DEMO_MARKER).exists())

    def test_refuses_a_root_that_is_a_file(self):
        root = self.root("file")
        root.write_text("not a directory")
        with self.assertRaisesRegex(generate.Refused, "not a directory"):
            generate.build(root, EARLY, SEED)

    def test_accepts_a_root_holding_only_finder_metadata(self):
        root = self.root("finder")
        root.mkdir()
        (root / ".DS_Store").write_bytes(b"\0")
        generate.build(root, EARLY, SEED)
        self.assertTrue((root / config.DEMO_MARKER).exists())

    def test_accepts_the_wrappers_empty_config(self):
        # The engine wrapper leaves an empty wealthdb.cfg in a root it saw
        # before the first build; that alone does not make a root live.
        root = self.root("seen")
        root.mkdir()
        (root / "wealthdb.cfg").write_text("")
        generate.build(root, EARLY, SEED)
        self.assertTrue((root / config.DEMO_MARKER).exists())
        self.assertGreater((root / "wealthdb.cfg").stat().st_size, 0)

    def test_marks_the_root_it_writes(self):
        root = self.root("new")
        generate.build(root, EARLY, SEED)
        self.assertTrue((root / config.DEMO_MARKER).exists())
        generate.build(root, EARLY, SEED)  # a marked root may be rebuilt

    def test_a_full_build_removes_the_old_gold(self):
        root = self.root("gold")
        generate.build(root, EARLY, SEED)
        for name in generate.GOLD_FILES:
            (root / name).write_text("old gold")
        generate.build(root, LATE, SEED, append=True)
        self.assertTrue(all((root / n).exists() for n in generate.GOLD_FILES))
        generate.build(root, LATE, SEED)
        self.assertFalse(any((root / n).exists() for n in generate.GOLD_FILES))

    def test_reads_a_root_whose_path_has_uri_characters(self):
        root = self.root("demo#1?x")
        generate.build(root, EARLY, SEED)
        generate.build(root, LATE, SEED, append=True)
        self.assertEqual(sorted(p.name for p in root.parent.iterdir() if p.name.startswith("demo")),
                         ["demo#1?x"])

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
