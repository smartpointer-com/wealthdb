"""Unit tests for watchlist.py, against a stand-in for the engine."""
import contextlib
import datetime
import io
import json
import os
import stat
import sys
import tempfile
import textwrap
import unittest
from unittest import mock
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent))
import watchlist as w  # noqa: E402

# What the stand-in engine reports, by as-of date and view. Dates it
# does not list hold nothing.
ROWS = {
    ("2026-06-30", "accounts"): [
        {"account": "Brindlecove Credit Union Joint savings",
         "account_id": "bc-sav-01"},
        {"account": "Quayvane Securities Roth IRA - Priya",
         "account_id": "QV-4829-17", "account_nickname": "Priya's Roth",
         "relationship_id": "R-77031"},
    ],
    ("2026-06-30", "portfolios"): [
        {"portfolio": "(no portfolio)"},
        {"portfolio": "Discretionary mandate", "portfolio_id": "pf-90311"},
    ],
    ("2025-12-31", "accounts"): [
        {"account": "Tamberlow 401(k)", "account_id": "tb-55120",
         "account_nickname": "401(k)"},
    ],
    ("2023-12-31", "accounts"): [
        {"account": "Never reached", "account_id": "nr-66219"},
    ],
}

STUB = textwrap.dedent("""\
    #!{python}
    import json, sys
    args = sys.argv[1:]
    rows = json.loads({rows!r})
    key = args[args.index("-d") + 1] + "|" + args[args.index("holdings") + 1]
    with open({log!r}, "a") as f:
        f.write(" ".join(args) + "\\n")
    print(json.dumps(rows.get(key, [])))
    """)


class Generic(unittest.TestCase):
    INSTITUTIONS = frozenset({"ubs", "schwab"})

    def test_kinds_currencies_and_institutions_are_generic(self):
        for name in ("Joint savings", "UBS Savings CHF", "401(k)",
                     "Schwab Roth IRA", "12-month term deposit"):
            self.assertTrue(w.generic(name, self.INSTITUTIONS), name)

    def test_a_proper_word_keeps_the_name(self):
        for name in ("Brindlecove Joint savings", "Priya's Roth", "Kids' savings"):
            self.assertFalse(w.generic(name, self.INSTITUTIONS), name)

    def test_one_ordinary_word_is_generic(self):
        self.assertTrue(w.generic("Rainy", frozenset(), frozenset({"rainy"})))
        self.assertFalse(w.generic("Rainy day pot", frozenset(), frozenset({"rainy"})))

    def test_institution_words_split_collector_names(self):
        with tempfile.TemporaryDirectory() as d:
            for name in ("ubs-web", "raiffeisen_at"):
                (Path(d) / "collectors" / name).mkdir(parents=True)
            self.assertEqual(w.institution_words(Path(d)),
                             {"ubs", "web", "raiffeisen", "at"})


class Collect(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.log = Path(self.tmp.name) / "calls.log"
        stub = Path(self.tmp.name) / "wealthdb"
        # main() reads as of the real today; so does the stand-in.
        today = datetime.date.today().isoformat()
        rows = {**ROWS, **{(today, v): r for (d, v), r in ROWS.items()
                           if d == "2026-06-30"}}
        stub.write_text(STUB.format(
            python=sys.executable, log=str(self.log),
            rows=json.dumps({f"{d}|{v}": r for (d, v), r in rows.items()})))
        stub.chmod(0o755)
        self.stub = str(stub)

    def tearDown(self):
        self.tmp.cleanup()

    def test_ids_and_names_back_to_the_first_empty_year(self):
        ids, names = w.collect([self.stub, "-r"], 2000,
                               datetime.date(2026, 6, 30), frozenset())
        self.assertEqual(ids, {"QV-4829-17", "R-77031", "pf-90311", "tb-55120"})
        self.assertEqual(names, {"Brindlecove Credit Union Joint savings",
                                 "Quayvane Securities Roth IRA - Priya",
                                 "Priya's Roth", "Tamberlow 401(k)"})
        dates = [line.split()[line.split().index("-d") + 1]
                 for line in self.log.read_text().splitlines()]
        self.assertEqual(sorted(set(dates)), ["2024-12-31", "2025-12-31", "2026-06-30"])

    def test_a_null_cell_is_no_term(self):
        rows = [{"account": None, "account_id": None}]
        with mock.patch.object(w, "holdings", return_value=rows):
            self.assertEqual(w.collect([self.stub, "-r"], 2026, datetime.date(2026, 6, 30), frozenset()),
                             (set(), set()))

    def test_an_existing_directory_keeps_its_mode(self):
        out = Path(self.tmp.name) / "here"
        out.mkdir(mode=0o755)
        w.write(out / "x.txt", [], datetime.date(2026, 6, 30))
        self.assertEqual(stat.S_IMODE(out.stat().st_mode), 0o755)

    def test_main_writes_a_private_file_and_prints_counts_only(self):
        out = Path(self.tmp.name) / "cfg" / "wealthdb-pii" / "watchlist.txt"
        printed = io.StringIO()
        with contextlib.redirect_stdout(printed):
            w.main(["--wealthdb", self.stub, "--out", str(out)])
        self.assertNotIn("QV-4829-17", printed.getvalue())
        self.assertEqual(stat.S_IMODE(out.stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(out.parent.stat().st_mode), 0o700)
        terms = [t for t in out.read_text().splitlines() if not t.startswith("#")]
        self.assertIn("QV-4829-17", terms)
        self.assertIn("Priya's Roth", terms)

    def test_a_failing_engine_stops_the_run(self):
        failing = Path(self.tmp.name) / "failing"
        failing.write_text("#!/bin/sh\necho 'no config' >&2\nexit 2\n")
        failing.chmod(0o755)
        with self.assertRaises(SystemExit) as cm:
            w.collect([str(failing)], 2025, datetime.date(2026, 1, 1), frozenset())
        self.assertIn("no config", str(cm.exception))


class Environment(unittest.TestCase):
    def test_data_root_is_a_term(self):
        os.environ["WEALTHDB_DATA_ROOT"] = "/srv/finance/wdb"
        try:
            self.assertIn("/srv/finance/wdb", w.environment())
        finally:
            del os.environ["WEALTHDB_DATA_ROOT"]


if __name__ == "__main__":
    unittest.main()
