"""Invariants the demo household must hold, checked on the silver it writes.

The simulation promises one thing above all: balances are the running sum
of transactions and a holding is worth its quantity times its price. The
rest follows downstream (holdings reconcile, cash coverage finds no gap it
can measure, returns see the flows that moved each value), so these tests check
the promise on the written files rather than on the simulation's own
bookkeeping.
"""

import collections
import csv
import datetime as dt
import json
import pathlib
import re
import shutil
import sqlite3
import sys
import tempfile
import unittest
from decimal import Decimal

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))

import generate  # noqa: E402
from demohouse import dates, spec  # noqa: E402
from demohouse.book import unit_price  # noqa: E402
from demohouse.household import Simulation  # noqa: E402
from tests import goref  # noqa: E402

AS_OF = dt.date(2026, 9, 29)
SEED = "harlow-19"
FIXED_SIGN_ZERO_OK = {"corporate_action"}


def ledger(root, name):
    """The rows of one of the ledger CSVs the build writes."""
    return list(csv.DictReader((root / "overrides" / name).read_text().splitlines()))


class Built(unittest.TestCase):
    """One full build of the default household, shared by every test."""

    @classmethod
    def setUpClass(cls):
        cls.tmp = pathlib.Path(tempfile.mkdtemp(prefix="demo-inv-"))
        cls.root = cls.tmp / "root"
        generate.build(cls.root, AS_OF, SEED)
        cls.inputs = spec.load()
        cls.sources = [s["id"] for s in cls.inputs.spec["sources"]]
        cls.cadence = {s["id"]: s["cadence"] for s in cls.inputs.spec["sources"]}
        cls.rows = {}
        for src in cls.sources:
            con = sqlite3.connect(cls.root / "silver" / f"{src}.db")
            con.row_factory = sqlite3.Row
            cls.rows[src] = {t: [dict(r) for r in con.execute(f"SELECT * FROM {t}")]
                             for t in ("accounts", "portfolios", "instruments", "positions",
                                       "cash_balances", "fx_rates", "transactions", "dump_runs", "meta")}
            con.close()
        cls.config = json.loads((cls.root / "wealthdb.cfg").read_text())

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.tmp, ignore_errors=True)

    def all(self, table):
        for src in self.sources:
            for r in self.rows[src][table]:
                yield src, r


class TestLedger(Built):
    def test_every_cash_balance_is_the_running_sum_of_the_ledger(self):
        moves = collections.defaultdict(lambda: collections.defaultdict(Decimal))
        for _, t in self.all("transactions"):
            in_kind = t["kind"] in ("transfer_in", "transfer_out") and t["instrument_id"]
            if t["kind"] == "corporate_action" or in_kind:
                continue
            moves[(t["account_id"], t["currency"])][dates.day_of(t["occurred_at"])] += Decimal(t["net_amount"])
        snaps = collections.defaultdict(list)
        for _, c in self.all("cash_balances"):
            snaps[(c["account_id"], c["currency"])].append((dates.day_of(c["snapshot_at"]), Decimal(c["amount"])))
        self.assertTrue(snaps)
        for key, series in snaps.items():
            series.sort()
            by_day = moves[key]
            first_day, first_amt = series[0]
            base = first_amt - sum(v for d, v in by_day.items() if d <= first_day)
            running, days = base, sorted(by_day)
            i = 0
            for day, amount in series:
                while i < len(days) and days[i] <= day:
                    running += by_day[days[i]]
                    i += 1
                self.assertEqual(running, amount, f"{key} on {day}")
            if by_day:
                self.assertLessEqual(first_day, min(by_day), f"{key}: a transaction before the first snapshot")

    def test_every_position_is_quantity_times_price(self):
        cat = self.inputs.instruments
        wanted = collections.defaultdict(list)
        for _, p in self.all("positions"):
            if p["quantity"] is not None:
                wanted[dates.day_of(p["snapshot_at"])].append(p)
        # A second market from the same seed, advanced here day by day.
        sim = Simulation(self.inputs, SEED, AS_OF)
        market = sim.market
        checked = 0
        for day in dates.days(sim.fx_start, AS_OF):
            market.advance(day)
            for p in wanted.get(day, []):
                inst = cat[p["instrument_id"]]
                value = unit_price(inst, market.price(p["instrument_id"])) * Decimal(p["quantity"])
                self.assertEqual(value.quantize(Decimal("0.0001")), Decimal(p["market_value"]),
                                 f'{p["account_id"]} {p["instrument_id"]} on {day}')
                checked += 1
        self.assertGreater(checked, 10000)

    def test_signs_are_canonical(self):
        signs = goref.canonical_signs()
        for _, t in self.all("transactions"):
            sign = signs.get(t["kind"])
            amount = Decimal(t["net_amount"])
            if sign is None or (amount == 0 and t["kind"] in FIXED_SIGN_ZERO_OK):
                continue
            self.assertEqual(amount.copy_sign(1) * sign, amount, f'{t["transaction_id"]} {t["kind"]}')

    def test_liabilities_are_negative(self):
        for _, p in self.all("positions"):
            if p["vehicle"] == "mortgage":
                self.assertLess(Decimal(p["market_value"]), 0)
        for _, a in self.all("accounts"):
            if a["account_kind"] == "card":
                balances = [Decimal(c["amount"]) for _, c in self.all("cash_balances")
                            if c["account_id"] == a["account_id"]]
                self.assertTrue(balances and max(balances) <= 0)

    def test_no_cash_account_is_overdrawn(self):
        kinds = {a["account_id"]: a["account_kind"] for _, a in self.all("accounts")}
        for _, c in self.all("cash_balances"):
            if kinds[c["account_id"]] != "card":
                self.assertGreaterEqual(Decimal(c["amount"]), 0, f'{c["account_id"]} {c["currency"]}')


class TestTransfers(Built):
    def test_a_shared_reference_joins_exactly_two_legs(self):
        refs = collections.defaultdict(list)
        for src, t in self.all("transactions"):
            ref = json.loads(t["payload"]).get("bank_ref")
            if ref:
                refs[(src, ref)].append(t)
        self.assertGreater(len(refs), 50)
        for key, legs in refs.items():
            self.assertEqual(len(legs), 2, key)
            a, b = sorted(legs, key=lambda t: Decimal(t["net_amount"]))
            self.assertEqual(dates.day_of(a["occurred_at"]), dates.day_of(b["occurred_at"]), key)
            if a["kind"] != "fx":
                self.assertNotEqual(a["account_id"], b["account_id"])
            if a["currency"] == b["currency"]:
                self.assertEqual(Decimal(a["net_amount"]) + Decimal(b["net_amount"]), 0, key)
            else:
                pa, pb = json.loads(a["payload"]), json.loads(b["payload"])
                self.assertEqual((pa["counter_currency"], Decimal(pa["counter_amount"])),
                                 (b["currency"], Decimal(b["net_amount"])), key)
                self.assertEqual((pb["counter_currency"], Decimal(pb["counter_amount"])),
                                 (a["currency"], Decimal(a["net_amount"])), key)

    def test_a_leg_that_names_an_account_has_its_partner_there(self):
        """Same day, opposite amount, on the named account — or, for a move
        between currencies, the pair the transfer-override ledger states."""
        names = self.config["spending"]["internal_transfer_matching"]["names"]
        stated = {(r["account" + side], r["occurred_at" + side], Decimal(r["amount" + side]))
                  for r in ledger(self.root, "transfer_overrides.csv")
                  for side in ("", "_b")}
        by_account = collections.defaultdict(list)
        rows = list(self.all("transactions"))
        for _, t in rows:
            by_account[t["account_id"]].append(t)
        named = 0
        for src, t in rows:
            if t["kind"] not in ("deposit", "withdrawal"):
                continue
            text = f'{t["counterparty"] or ""} {t["description"] or ""}'
            for n in names:
                if n["source"] == src or not re.search(n["match"], text):
                    continue
                named += 1
                if (t["account_id"], dates.day_of(t["occurred_at"]).isoformat(), Decimal(t["net_amount"])) in stated:
                    continue
                partners = [p for p in by_account[n["account"]]
                            if dates.day_of(p["occurred_at"]) == dates.day_of(t["occurred_at"])
                            and p["currency"] == t["currency"]
                            and Decimal(p["net_amount"]) == -Decimal(t["net_amount"])]
                if len(partners) > 1:
                    # Twin moves of one size on one day: the partner is the
                    # leg that names this row's account back, as the
                    # matcher's named phase decides it.
                    back = [x for x in names if x["account"] == t["account_id"]]
                    partners = [p for p in partners
                                if any(re.search(x["match"], f'{p["counterparty"] or ""} {p["description"] or ""}')
                                       for x in back)]
                self.assertEqual(len(partners), 1, f'{t["transaction_id"]} names {n["account"]}')
        self.assertGreater(named, 50)

    def test_every_ledger_row_names_a_transaction_that_exists(self):
        by_key = collections.Counter()
        for src, t in self.all("transactions"):
            by_key[(src, t["account_id"], dates.day_of(t["occurred_at"]).isoformat(), Decimal(t["net_amount"]))] += 1
        pins = ledger(self.root, "spending_pins.csv")
        overrides = ledger(self.root, "transfer_overrides.csv")
        self.assertTrue(pins and overrides)
        for r in pins:
            self.assertEqual(by_key[(r["silver_source_id"], r["account"], r["occurred_at"], Decimal(r["amount"]))], 1, r)
        for r in overrides:
            for suffix in ("", "_b"):
                key = (r["silver_source_id" + suffix], r["account" + suffix], r["occurred_at" + suffix],
                       Decimal(r["amount" + suffix]))
                self.assertEqual(by_key[key], 1, r)

    def test_the_in_kind_exit_is_one_pair(self):
        rows = ledger(self.root, "equity_transfers.csv")
        self.assertEqual(sorted(r["direction"] for r in rows), ["in", "out"])
        self.assertEqual(len({(r["occurred_at"], r["value"]) for r in rows}), 1)


class TestPrivateMarkets(Built):
    def test_the_fund_is_held_at_cost_between_marks(self):
        """A call adds its amount and a distribution takes its amount out,
        so no call or distribution day books a gain. A mark day values the
        called capital at the mark's multiple, less what came back. The
        book value is always the capital called: a cash distribution does
        not reduce it."""
        f = self.inputs.spec["private"]["fund"]
        src = next(s["id"] for s in self.inputs.spec["sources"]
                   if any(a["id"] == f["account"] for a in s["accounts"]))
        rows = {dates.day_of(r["snapshot_at"]): r for r in self.rows[src]["positions"]
                if r["account_id"] == f["account"] and r["instrument_id"] == f["instrument"]}
        value = {d: Decimal(r["market_value"]) for d, r in rows.items()}
        events = sorted([(dates.parse(c["date"]), Decimal(c["amount"])) for c in f["calls"]] +
                        [(dates.parse(d["date"]), -Decimal(d["amount"])) for d in f["distributions"]])
        marks = {dates.parse(m["date"]): Decimal(m["multiple"]) for m in f["marks"]}
        called = distributed = Decimal(0)
        checked = 0
        for day in sorted(set(rows) & ({d for d, _ in events} | set(marks))):
            before = value.get(day - dt.timedelta(days=1), Decimal(0))
            moved = sum((a for d, a in events if d == day), Decimal(0))
            called += max(moved, Decimal(0))
            distributed += max(-moved, Decimal(0))
            want = called * marks[day] - distributed if day in marks else before + moved
            self.assertEqual(value[day], want, day)
            self.assertEqual(Decimal(rows[day]["book_value"]), called, day)
            checked += 1
        self.assertGreaterEqual(checked, len(f["calls"]) + len(f["marks"]) - 2)


class TestVocabulary(Built):
    def test_provider_categories_are_taxonomy_values_of_the_right_family(self):
        spend, income = goref.spending_values(), goref.income_values()
        # A refund nets inside the spending category it reverses.
        outflow = {"purchase", "refund", "withdrawal", "fee", "tax"}
        for _, t in self.all("transactions"):
            v = t["provider_category"]
            if v is None:
                continue
            fam = spend if t["kind"] in outflow else income
            self.assertIn(v, fam, f'{t["transaction_id"]} {t["kind"]}')

    def test_every_taxonomy_pair_is_admitted(self):
        pairs = goref.taxonomy_pairs()
        for _, p in self.all("positions"):
            self.assertIn((p["asset_class"], p["vehicle"]), pairs, p["instrument_id"])
        for _, i in self.all("instruments"):
            self.assertIn((i["asset_class"], i["vehicle"]), pairs, i["instrument_id"])

    def test_accounts_and_kinds_use_the_canonical_vocabulary(self):
        kinds, wrappers, styles = goref.account_kinds(), goref.tax_wrappers(), goref.management_styles()
        for _, a in self.all("accounts"):
            self.assertIn(a["account_kind"], kinds)
            self.assertIn(a["tax_wrapper"], wrappers)
            self.assertIn(a["management_style"], styles)
        tx = goref.tx_kinds()
        for _, t in self.all("transactions"):
            self.assertIn(t["kind"], tx)

    def test_catalogue_categories_are_specific_values(self):
        spend = goref.spending_values()
        for m in self.inputs.merchants.values():
            self.assertIn(m["category"], spend, m["id"])
            self.assertNotIn("_OTHER_", m["category"], f'{m["id"]}: a catch-all tells a report nothing')
        income = goref.income_values()
        for p in self.inputs.payers.values():
            self.assertIn(p["category"], income, p["id"])

    def test_config_values_are_valid(self):
        spend, income = goref.spending_values(), goref.income_values()
        for r in self.config["spending"]["rules"]:
            self.assertIn(r["category"], spend)
            if "far" in r:
                self.assertIn(r["far"], self.config["declared_accounts"])
        for r in self.config["income"]["rules"]:
            self.assertIn(r["type"], income)
        for d in self.config["declared_accounts"].values():
            self.assertIn(d["account_kind"], goref.account_kinds())
            self.assertIn(d["tax_wrapper"], goref.tax_wrappers())
        ids = {s["id"] for s in self.config["silver_sources"]}
        for block in ("returns_policy_overrides", "account_overrides"):
            self.assertTrue(set(self.config[block]) <= ids, block)
        self.assertTrue(set(self.config["inception_overrides"]["sources"]) <= ids)
        for n in self.config["spending"]["internal_transfer_matching"]["names"]:
            re.compile(n["match"])
            self.assertNotIn("\\ ", n["match"], "RE2 refuses an escaped space")


class TestCalendar(Built):
    def test_markets_rest_at_weekends(self):
        for src in self.sources:
            if self.cadence[src] != "business":
                continue
            for table, col in (("positions", "snapshot_at"), ("cash_balances", "snapshot_at"),
                               ("transactions", "occurred_at")):
                for r in self.rows[src][table]:
                    self.assertTrue(dates.is_business(dates.day_of(r[col])), f"{src}.{table} on {dates.day_of(r[col])}")

    def test_every_open_month_has_its_month_end_snapshot(self):
        for src in self.sources:
            days = collections.defaultdict(set)
            for r in self.rows[src]["cash_balances"] + self.rows[src]["positions"]:
                days[r["account_id"]].add(dates.day_of(r["snapshot_at"]))
            for acct, seen in days.items():
                first = min(seen)
                month = dt.date(first.year, first.month, 1)
                while dates.month_end(month.year, month.month) < AS_OF:
                    end = dates.month_end(month.year, month.month)
                    if self.cadence[src] == "business":
                        end = dates.prev_business(end)
                    if end >= first:
                        self.assertIn(end, seen, f"{src}/{acct} {month:%Y-%m}")
                    month = dates.add_months(month, 1)

    def test_every_source_is_fresh_at_as_of(self):
        for src in self.sources:
            latest = max(dates.day_of(r["snapshot_at"]) for t in ("positions", "cash_balances", "fx_rates")
                         for r in self.rows[src][t]) if any(self.rows[src][t] for t in
                                                            ("positions", "cash_balances", "fx_rates")) else None
            self.assertIsNotNone(latest, src)
            self.assertGreaterEqual(latest, dates.prev_business(AS_OF), src)

    def test_fx_starts_before_any_foreign_holding(self):
        first_fx = min(dates.day_of(r["snapshot_at"]) for r in self.rows["fx"]["fx_rates"])
        first_foreign = min(dates.day_of(r["snapshot_at"]) for _, r in self.all("positions") if r["currency"] != "USD")
        self.assertLessEqual(first_fx, first_foreign - dt.timedelta(days=30))

    def test_one_run_covers_every_row(self):
        for src in self.sources:
            (run,) = self.rows[src]["dump_runs"]
            for table, col in (("positions", "snapshot_at"), ("cash_balances", "snapshot_at"),
                               ("fx_rates", "snapshot_at"), ("transactions", "occurred_at")):
                for r in self.rows[src][table]:
                    self.assertTrue(run["window_start"] <= r[col] <= run["window_end"], f"{src}.{table}")


if __name__ == "__main__":
    unittest.main()
