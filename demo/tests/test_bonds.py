"""Bond arithmetic (demohouse/bonds.py), the curve a bond prices off
(demohouse/market.py), and the redemptions the household books, on terms
and curves invented here."""

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
from demohouse import bonds, dates, rates, spec  # noqa: E402
from demohouse.market import Market  # noqa: E402

SEED = "test-seed"
# A curve at 4% at every published tenor.
FLAT = {t: Decimal(4) for t in rates.TENOR_MONTHS}
TINY = Decimal("1e-9")


class TestPriceAndYield(unittest.TestCase):
    def test_the_yield_at_a_price_is_the_one_it_was_priced_at(self):
        for coupon, ytm, life, frequency in ((Decimal(5), Decimal("3.2"), Decimal(7), Decimal(2)),
                                             (Decimal(0), Decimal("4.5"), Decimal("12.5"), Decimal(2)),
                                             (Decimal("2.1"), Decimal("6.0"), Decimal("0.4"), Decimal(1))):
            value = bonds.price(coupon, ytm, life, frequency)
            self.assertLess(abs(bonds.yield_at(value, coupon, life, frequency) - ytm), TINY, (coupon, ytm))

    def test_a_bond_yielding_its_coupon_prices_at_par(self):
        self.assertLess(abs(bonds.price(Decimal(4), Decimal(4), Decimal(10), Decimal(2)) - 100), TINY)

    def test_a_zeros_duration_is_its_life_over_one_period_of_its_yield(self):
        life, ytm = Decimal(8), Decimal(5)
        want = life / (1 + ytm / 100 / bonds.ZERO_FREQUENCY)
        got = bonds.modified_duration(Decimal(0), ytm, life, bonds.ZERO_FREQUENCY)
        self.assertLess(abs(got - want), TINY)


class TestPricesOffTheCurve(unittest.TestCase):
    CALLABLE = {"coupon": {"rate": "0.06", "months": [6, 12], "day": 1}, "model": {"spread": "0"},
                "maturity": "2034-06-01", "call": "2028-06-01"}

    def to(self, day, end):
        """The callable bond's price on `day`, priced to `end`."""
        life = bonds.life(day, end)
        return bonds.price(Decimal(6), bonds.curve_yield(FLAT, life, Decimal(0)), life, Decimal(2))

    def test_a_callable_bond_above_par_prices_to_its_call(self):
        day = dt.date(2026, 3, 2)
        self.assertEqual(bonds.fixed_price(self.CALLABLE, day, FLAT), self.to(day, "2028-06-01"))
        self.assertLess(self.to(day, "2028-06-01"), self.to(day, "2034-06-01"))

    def test_once_its_call_date_passes_it_prices_to_maturity(self):
        for day in (dt.date(2028, 6, 1), dt.date(2029, 1, 2)):
            self.assertEqual(bonds.fixed_price(self.CALLABLE, day, FLAT), self.to(day, "2034-06-01"), day)

    def test_a_discount_note_is_its_face_less_the_discount_on_act_360(self):
        note = {"maturity": "2026-08-29", "model": {"spread": "0"}}
        got = bonds.discount_price(note, dt.date(2026, 3, 2), FLAT)
        self.assertLess(abs(got - 100 * (1 - Decimal("0.04") * 180 / 360)), TINY)


class TestCouponDates(unittest.TestCase):
    def test_a_coupon_on_the_31st_falls_on_a_short_months_last_day(self):
        coupon = {"day": 31, "months": [8, 2]}
        self.assertEqual(bonds.coupon_dates(coupon, 2024), [dt.date(2024, 2, 29), dt.date(2024, 8, 31)])
        self.assertEqual(bonds.coupon_dates(coupon, 2025), [dt.date(2025, 2, 28), dt.date(2025, 8, 31)])

    def test_the_last_and_the_next_coupon_cross_the_year_end(self):
        coupon = {"day": 31, "months": [6, 12]}
        self.assertEqual(bonds.last_coupon(coupon, dt.date(2026, 1, 15)), dt.date(2025, 12, 31))
        self.assertEqual(bonds.last_coupon(coupon, dt.date(2025, 12, 31)), dt.date(2025, 12, 31))
        self.assertEqual(bonds.next_coupon(coupon, dt.date(2025, 12, 31)), dt.date(2026, 6, 30))
        self.assertEqual(bonds.next_coupon(coupon, dt.date(2025, 12, 30)), dt.date(2025, 12, 31))

    def test_a_zero_compounds_twice_a_year(self):
        self.assertEqual(bonds.frequency(None), bonds.ZERO_FREQUENCY)
        self.assertEqual(bonds.frequency({"months": [3, 9]}), 2)


class TestLadder(unittest.TestCase):
    def test_each_note_matures_the_day_the_next_is_issued(self):
        terms = bonds.issue_dates("2026-03-04", 26, 3)
        self.assertEqual(terms[0], (dt.date(2025, 9, 3), dt.date(2026, 3, 4)))
        self.assertEqual(len(terms), 3)
        for (issued, matures), (next_issued, _) in zip(terms, terms[1:], strict=False):
            self.assertEqual(matures - issued, dt.timedelta(weeks=26))
            self.assertEqual(next_issued, matures)


class TestCurveOn(unittest.TestCase):
    def test_before_the_curve_is_first_published_it_reads_that_day(self):
        """A history whose FX starts on a weekend has its first curve on
        the Monday after."""
        saturday = dt.date(2026, 3, 7)
        curve = rates.curve(SEED, saturday, saturday + dt.timedelta(days=10))
        market = Market(SEED, [], saturday, {}, curve)
        monday = dates.next_business(saturday)
        self.assertEqual(min(curve), monday)
        for day in (saturday - dt.timedelta(days=3), saturday, monday):
            self.assertEqual(market.curve_on(day), curve[monday], day)
        self.assertEqual(market.curve_on(monday + dt.timedelta(days=5)), curve[dates.prev_business(
            monday + dt.timedelta(days=5))])


class TestRedemptions(unittest.TestCase):
    """A build over a catalogue copy in which the household's zero matures
    in the first months, and its ladder has one note."""

    AS_OF = dt.date(2023, 12, 15)
    ZERO, ZERO_MATURES = "wywa36", "2023-12-01"

    @classmethod
    def setUpClass(cls):
        cls.tmp = pathlib.Path(tempfile.mkdtemp(prefix="demo-bonds-"))
        catalogue = cls.tmp / "catalogue"
        shutil.copytree(spec.CATALOGUE_DIR, catalogue)
        path = catalogue / "instruments.json"
        cat = json.loads(path.read_text())
        (zero,) = [i for i in cat["instruments"] if i["id"] == cls.ZERO]
        zero["maturity"] = cls.ZERO_MATURES
        (ladder,) = cat["ladders"]
        ladder["issues"] = 1
        path.write_text(json.dumps(cat))
        cls.inputs = spec.load(catalogue_dir=catalogue)
        generate.build(cls.tmp / "root", cls.AS_OF, SEED, inputs=cls.inputs)
        cls.txns, cls.held = [], {}
        for db in sorted((cls.tmp / "root" / "silver").glob("*.db")):
            con = sqlite3.connect(db)
            con.row_factory = sqlite3.Row
            cls.txns += [dict(r) for r in con.execute("SELECT * FROM transactions")]
            for r in con.execute("SELECT instrument_id, max(snapshot_at) AS last FROM positions GROUP BY 1"):
                cls.held[r["instrument_id"]] = dates.day_of(r["last"])
            con.close()
        cls.ledger = (cls.tmp / "root" / "overrides" / "exposure_declarations.ndjson").read_text()
        (cls.note,) = [iid for iid, i in cls.inputs.instruments.items() if i.get("ladder") == ladder["id"]]

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.tmp, ignore_errors=True)

    def of(self, iid):
        return [t for t in self.txns if t["instrument_id"] == iid]

    def test_a_bond_outside_a_ladder_pays_back_its_face_at_par(self):
        day = bonds.redemption_day(self.inputs.instruments[self.ZERO])
        (sold,) = [t for t in self.of(self.ZERO) if t["kind"] == "sell"]
        self.assertEqual(dates.day_of(sold["occurred_at"]), day)
        self.assertTrue(sold["description"].startswith("REDEMPTION"), sold["description"])
        self.assertEqual(Decimal(sold["price"]), 100)
        self.assertEqual(Decimal(sold["net_amount"]), -Decimal(sold["quantity"]))
        self.assertLess(self.held[self.ZERO], day)

    def test_the_last_note_of_a_ladder_rolls_into_nothing(self):
        """It pays back its cost and earns its discount, and nothing is
        bought that day in its account; the ledger states its terms
        alone."""
        note = self.inputs.instruments[self.note]
        self.assertIsNone(note["next"])
        day = bonds.redemption_day(note)
        (sold,) = [t for t in self.of(self.note) if t["kind"] == "sell"]
        (earned,) = [t for t in self.of(self.note) if t["kind"] == "interest"]
        self.assertEqual(dates.day_of(sold["occurred_at"]), day)
        self.assertEqual(dates.day_of(earned["occurred_at"]), day)
        self.assertEqual(Decimal(sold["net_amount"]) + Decimal(earned["net_amount"]), -Decimal(sold["quantity"]))
        bought = [t for t in self.txns if t["kind"] == "buy" and t["account_id"] == sold["account_id"]
                  and dates.day_of(t["occurred_at"]) == day]
        self.assertFalse(bought, bought)
        self.assertEqual(self.ledger.count(f'"instrument":"{note["ladder"]}'), 1)


if __name__ == "__main__":
    unittest.main()
