"""Invariants the demo household must hold, checked on the silver it writes.

The simulation promises one thing above all: balances are the running sum
of transactions and a holding is worth its quantity times its price. The
rest follows downstream (holdings reconcile, cash coverage finds no gap it
can measure, returns see the flows that moved each value), so these tests check
the promise on the written files rather than on the simulation's own
bookkeeping.
"""

import ast
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
from demohouse import bonds, dates, filings, rates, reference, silver, spec  # noqa: E402
from demohouse.book import unit_price  # noqa: E402
from demohouse.household import Simulation  # noqa: E402
from tests import goref  # noqa: E402

AS_OF = dt.date(2026, 9, 29)
SEED = "harlow-19"
FIXED_SIGN_ZERO_OK = {"corporate_action"}
SEC_EDGAR = pathlib.Path(__file__).resolve().parents[2] / "collectors" / "refdata" / "providers" / "sec_edgar.py"


def ledger(root, name):
    """The rows of one of the ledger CSVs the build writes."""
    return list(csv.DictReader((root / "overrides" / name).read_text().splitlines()))


def sec_figures():
    """{field: {field, family, unit, instant, gaap, ifrs}}: the figures the
    refdata collector reads from the SEC, from its FIGURES declaration."""
    tree = ast.parse(SEC_EDGAR.read_text())
    (figures,) = [n.value for n in tree.body
                  if isinstance(n, ast.Assign) and [getattr(x, "id", None) for x in n.targets] == ["FIGURES"]]
    names = ("field", "family", "unit", "instant", "gaap", "ifrs")
    out = {}
    for call in figures.elts:
        f = dict(zip(names, map(ast.literal_eval, call.args), strict=False))
        f |= {k.arg: ast.literal_eval(k.value) for k in call.keywords}
        out[f["field"]] = f
    return out


BUILT = {}


def setUpModule():
    """One full build of the default household, shared by every test:
    no test writes to it."""
    tmp = pathlib.Path(tempfile.mkdtemp(prefix="demo-inv-"))
    root = tmp / "root"
    generate.build(root, AS_OF, SEED)
    inputs = spec.load()
    sources = [s["id"] for s in inputs.spec["sources"]]
    rows = {}
    for src in sources:
        con = sqlite3.connect(root / "silver" / f"{src}.db")
        con.row_factory = sqlite3.Row
        rows[src] = {t: [dict(r) for r in con.execute(f"SELECT * FROM {t}")]
                     for t in ("accounts", "portfolios", "instruments", "positions", "position_lots",
                               "cash_balances", "fx_rates", "rates", "transactions", "realized_lots",
                               "dump_runs", "meta", *silver.REFERENCE_TABLES)}
        con.close()
    BUILT.update(tmp=tmp, root=root, inputs=inputs, sources=sources, rows=rows,
                 cadence={s["id"]: s["cadence"] for s in inputs.spec["sources"]},
                 config=json.loads((root / "wealthdb.cfg").read_text()))


def tearDownModule():
    shutil.rmtree(BUILT["tmp"], ignore_errors=True)


class Built(unittest.TestCase):
    """A test of the module's one build."""

    @classmethod
    def setUpClass(cls):
        for name, value in BUILT.items():
            setattr(cls, name, value)

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
        """A position's value is its quantity at the day's price, plus a
        bond's accrued interest, which gold's market value includes."""
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
                value = value.quantize(Decimal("0.0001")) + Decimal(p["accrued_interest"] or 0)
                self.assertEqual(value, Decimal(p["market_value"]),
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


class TestLots(Built):
    def test_open_lots_add_up_to_their_position(self):
        """A unit holding's lots sum to its quantity and its book value,
        and date it by the oldest of them. Every lot has its position."""
        lots = collections.defaultdict(list)
        for src, lot in self.all("position_lots"):
            lots[(src, lot["snapshot_at"], lot["account_id"], lot["position_key"])].append(lot)
        checked = 0
        for src, p in self.all("positions"):
            own = lots.pop((src, p["snapshot_at"], p["account_id"], p["position_key"]), [])
            if p["quantity"] is None or p["book_value"] is None:
                # A mark, or a source that states no basis: no lots.
                self.assertEqual(own, [], p["position_key"])
                continue
            where = f'{p["account_id"]} {p["position_key"]} on {dates.day_of(p["snapshot_at"])}'
            self.assertTrue(own, where)
            self.assertEqual(sum(Decimal(lot["quantity"]) for lot in own), Decimal(p["quantity"]), where)
            self.assertEqual(sum(Decimal(lot["book_value"]) for lot in own), Decimal(p["book_value"]), where)
            self.assertEqual(min(lot["acquisition_date"] for lot in own), p["acquisition_date"], where)
            checked += 1
        self.assertEqual(dict(lots), {})
        self.assertGreater(checked, 10000)

    def test_every_sale_has_its_realized_lots(self):
        """The lots one day's sales of an instrument relieve add up to the
        quantity sold and the proceeds received."""
        sold = collections.defaultdict(lambda: [Decimal(0), Decimal(0)])
        unstated = {s["id"] for s in self.inputs.spec["sources"] if not s.get("states_basis", True)}
        for src, t in self.all("transactions"):
            if t["kind"] == "sell" and src not in unstated:
                key = (t["account_id"], t["instrument_id"], dates.day_of(t["occurred_at"]).isoformat())
                sold[key][0] -= Decimal(t["quantity"])
                sold[key][1] += Decimal(t["net_amount"])
        realized = collections.defaultdict(lambda: [Decimal(0), Decimal(0)])
        for _, r in self.all("realized_lots"):
            key = (r["account_id"], r["instrument_id"], r["disposal_date"])
            realized[key][0] += Decimal(r["quantity"])
            realized[key][1] += Decimal(r["proceeds"])
        self.assertGreater(len(sold), 10)
        self.assertEqual(dict(realized), dict(sold))

    def test_a_realized_lot_states_its_term_and_year(self):
        for _, r in self.all("realized_lots"):
            acquired, disposed = dates.parse(r["acquisition_date"]), dates.parse(r["disposal_date"])
            self.assertLessEqual(acquired, disposed, r["realized_lot_id"])
            self.assertEqual(r["term"], "long" if (disposed - acquired).days > 365 else "short", r["realized_lot_id"])
            self.assertEqual(r["tax_year"], disposed.year, r["realized_lot_id"])

    def test_one_document_kind_states_the_gain(self):
        """A statement states each lot's gain, as proceeds less cost. A
        Form 1099-B leaves it to that arithmetic downstream."""
        stated = collections.defaultdict(set)
        for _, r in self.all("realized_lots"):
            gain = r["realized_gain_loss"]
            stated[r["document_kind"]].add(gain is not None)
            if gain is not None:
                self.assertEqual(Decimal(gain), Decimal(r["proceeds"]) - Decimal(r["book_value"]), r["realized_lot_id"])
        self.assertEqual(dict(stated), {"form_1099b": {False}, "statement": {True}})


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
        # The reference source is dated by the month-ends it describes.
        for src in self.sources:
            if src == silver.REFERENCE_SOURCE:
                continue
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
                               ("fx_rates", "snapshot_at"), ("rates", "as_of"), ("transactions", "occurred_at"),
                               *((t, "as_of") for t in silver.REFERENCE_TABLES)):
                for r in self.rows[src][table]:
                    self.assertTrue(run["window_start"] <= r[col] <= run["window_end"], f"{src}.{table}")


class TestReference(Built):
    """The reference source: compositions that add up, on the bases and
    with the gaps the catalogue gives, keyed the way gold keys the
    holdings they describe."""

    @classmethod
    def setUpClass(cls):
        super().setUpClass()
        cls.ref = cls.rows[silver.REFERENCE_SOURCE]
        cls.cat = reference.load()

    def test_only_the_reference_source_states_reference_rows(self):
        for src in self.sources:
            if src == silver.REFERENCE_SOURCE:
                continue
            for t in silver.REFERENCE_TABLES:
                self.assertEqual(self.rows[src][t], [], f"{src}.{t}")

    def test_weights_add_up(self):
        lines = collections.defaultdict(list)
        for r in self.ref["lines"]:
            lines[(r["fund_ref"], r["as_of"], r["basis"])].append(Decimal(r["weight"]))
        remainder = {reference.fund_ref(self.inputs.instruments[f["instrument"]])
                     for f in self.cat["funds"] if f.get("remainder")}
        for c in self.ref["compositions"]:
            key = (c["fund_ref"], c["as_of"], c["basis"])
            total = sum(lines[key])
            self.assertEqual(Decimal(c["weight_sum"]), total, key)
            self.assertEqual(c["line_count"], len(lines[key]), key)
            if c["fund_ref"] in remainder or c["basis"] == "index":
                self.assertLess(total, Decimal(100), key)
            else:
                self.assertEqual(total, Decimal(100), key)

    def test_compositions_are_month_ends_inside_the_history(self):
        bases = goref.composition_bases()
        for c in self.ref["compositions"]:
            day = dates.day_of(c["as_of"])
            self.assertEqual(day, dates.month_end(day.year, day.month))
            self.assertLessEqual(day, AS_OF)
            self.assertIn(c["basis"], bases)

    def test_the_catalogue_exercises_every_rule(self):
        swap = [f for f in self.cat["funds"] if f.get("basis") == "collateral"]
        self.assertTrue(swap and all("index" in f for f in swap), "a swap fund with index lines")
        self.assertTrue(any(a.get("hedged_currency") for f in self.cat["funds"] for a in [f.get("attributes", {})]))
        self.assertTrue(any(f.get("gaps") for f in self.cat["funds"]), "a fund with a gap")
        funds = {f["instrument"] for f in self.cat["funds"]}
        self.assertTrue(any(sid in funds for f in self.cat["funds"] for sid in f["lines"]), "a fund of funds")
        late = [f for f in self.cat["funds"] if dates.parse(f["from"]) > self.inputs.history_start]
        self.assertTrue(late, "a fund whose compositions start after it is held")

    def test_each_fund_has_the_compositions_its_entry_gives(self):
        """From the month-end of `from`, none inside a gap, on the bases
        the entry names, and no cash line where the fund leaves a
        remainder."""
        days, bases = collections.defaultdict(set), collections.defaultdict(set)
        for c in self.ref["compositions"]:
            days[c["fund_ref"]].add(dates.day_of(c["as_of"]))
            bases[c["fund_ref"]].add(c["basis"])
        cash = {r["fund_ref"] for r in self.ref["lines"] if r["asset_type"] == "Cash"}
        for f in self.cat["funds"]:
            ref = reference.fund_ref(self.inputs.instruments[f["instrument"]])
            self.assertTrue(days[ref], f"{f['instrument']} has compositions under {ref}")
            first = dates.parse(f["from"])
            self.assertEqual(min(days[ref]), dates.month_end(first.year, first.month), ref)
            for lo, hi in f.get("gaps", []):
                self.assertFalse([d for d in days[ref] if lo <= f"{d:%Y-%m}" <= hi], ref)
            want = {f.get("basis", "holdings")} | ({"index"} if "index" in f else set())
            self.assertEqual(bases[ref], want, ref)
            if f.get("remainder"):
                self.assertNotIn(ref, cash, ref)

    def test_the_config_proxies_and_declarations_name_what_exists(self):
        exposure = self.config["exposure"]
        self.assertLessEqual(set(exposure["custody"]), set(self.sources))
        refs = {reference.fund_ref(i) for i in self.inputs.instruments.values()}
        for key, p in exposure["proxies"].items():
            self.assertIn(key, refs)
            self.assertIn(p["fund"], refs)
        self.assertEqual(exposure["declarations"], "overrides/exposure_declarations.ndjson")
        lines = (self.root / "overrides" / "exposure_declarations.ndjson").read_text().splitlines()
        self.assertTrue(lines, "the declarations ledger has records")
        dims = ("region", "country", "currency", "sector", "asset_class")
        split, situs = 0, []
        for line in lines:
            d = json.loads(line)
            if "account" in d:
                # A rate names an account the source states, and nothing else.
                accounts = {a["account_id"] for a in self.rows[d["silver_source_id"]]["accounts"]}
                self.assertIn(d["account"], accounts, line)
                self.assertFalse(set(d) - {"silver_source_id", "account", "as_of", "rate", "note"}, line)
                continue
            name = f"{d['silver_source_id']}/{d['instrument']}"
            held = {r["instrument_id"] for r in self.rows[d["silver_source_id"]]["positions"]}
            self.assertIn(d["instrument"], held, name)
            stated = [k for k in dims if k in d]
            self.assertTrue(stated or "situs" in d or set(d) & set(goref.yield_records()), name)
            for k in stated:
                self.assertAlmostEqual(sum(d[k].values()), 100, delta=0.5, msg=f"{name} {k}")
            split += any(len(d[k]) > 1 for k in stated)
            if "situs" in d:
                situs.append(name)
                self.assertEqual(list(d["situs"].values()), [100], name)
        self.assertEqual(split, 1, "one record splits a dimension")
        self.assertEqual(situs, ["emberwright/fund-ii"], "the private fund declares its situs")

    def test_isins_have_the_demo_shape(self):
        """Every ISIN the catalogues state, and so every one a line or a
        holding carries."""
        isins = [s["isin"] for s in self.cat["securities"] if s.get("isin")]
        isins += [i["isin"] for i in self.inputs.instruments.values() if i.get("isin")]
        self.assertTrue(isins)
        for isin in isins:
            self.assertRegex(isin, r"^[A-Z]{2}000[A-Z0-9]{4}[0-9]{3}$")

    def test_no_us_government_line_could_be_a_treasury_issue(self):
        """None names the Treasury, and none has a coupon that is a
        multiple of 1/8, as every Treasury coupon is."""
        bonds = [s for s in self.cat["securities"]
                 if s.get("sector") == "Government" and s.get("country") == "United States"]
        self.assertTrue(bonds)
        for s in bonds:
            self.assertNotIn("treasury", s["name"].lower(), s["id"])
            if "coupon" in s:
                self.assertNotEqual(Decimal(s["coupon"]) * 8 % 1, 0, s["id"])

    def test_a_security_that_is_an_instrument_restates_none_of_it(self):
        for s in self.cat["securities"]:
            inst = self.inputs.instruments.get(s["id"])
            if inst:
                self.assertFalse({"name", "currency", "ticker", "coupon"} & set(s), s["id"])
                self.assertFalse(inst.get("isin") and "isin" in s, s["id"])

    def test_the_yield_records_name_only_the_demos_own_identifiers(self):
        """Every fund a risk row or a characteristic names is a catalogue
        fund, and every company figure a demo instrument's; a label is a
        rating bucket or Not Rated."""
        funds = {reference.fund_ref(self.inputs.instruments[f["instrument"]])
                 for f in self.cat["funds"] + self.cat["money_market"] + self.cat["pages"]}
        refs = {reference.fund_ref(i) for i in self.inputs.instruments.values()}
        self.assertTrue(self.ref["fund_risk"] and self.ref["characteristics"] and self.ref["period_facts"])
        for r in self.ref["fund_risk"] + self.ref["characteristics"]:
            self.assertIn(r["fund_ref"], funds)
        for r in self.ref["period_facts"]:
            self.assertIn(r["security_key"], refs)
        labels = {r["label"] for r in self.ref["characteristics"] if r["field"] == "credit_quality"}
        self.assertLessEqual(labels, {*goref.rating_buckets(), reference.NOT_RATED})
        self.assertEqual(list(reference.RATINGS), goref.rating_buckets())
        bands = {r["label"] for r in self.ref["characteristics"] if r["field"] == "maturity_breakdown"}
        self.assertLessEqual(bands, {label for label, _ in reference.MATURITY_BANDS} | {reference.CASH_BAND})
        self.assertGreater(len(bands), 4)
        self.assertEqual({r["label"] for r in self.ref["characteristics"]} - labels - bands, {""})


class TestRates(Built):
    """The dollar curve the rates source publishes."""

    @classmethod
    def setUpClass(cls):
        super().setUpClass()
        cls.curve = collections.defaultdict(dict)
        for r in cls.rows[rates.SOURCE]["rates"]:
            cls.curve[dates.day_of(r["as_of"])][r["series_id"]] = Decimal(r["value"])

    def test_one_value_per_business_day_and_tenor(self):
        want = [d for d in dates.days(self.inputs.fx_start, AS_OF) if dates.is_business(d)]
        self.assertEqual(sorted(self.curve), want)
        ids = {rates.series_id(t) for t in rates.TENOR_MONTHS}
        for day, points in self.curve.items():
            self.assertEqual(set(points), ids, day)

    def test_only_the_rates_source_publishes_rates(self):
        for src in self.sources:
            if src != rates.SOURCE:
                self.assertEqual(self.rows[src]["rates"], [], src)

    def test_the_series_ids_are_the_demos_own(self):
        """Not FRED's (DGS10, DTB3): the demo names a series by its
        currency and tenor."""
        for sid in {r["series_id"] for r in self.rows[rates.SOURCE]["rates"]}:
            self.assertRegex(sid, r"^USD-\d+[MY]$")

    def test_levels_are_plausible_and_move_slowly(self):
        days = sorted(self.curve)
        for day in days:
            for sid, v in self.curve[day].items():
                self.assertTrue(0 <= v <= 6, f"{sid} {v} on {day}")
        for a, b in zip(days, days[1:], strict=False):
            for sid, v in self.curve[b].items():
                self.assertLessEqual(abs(v - self.curve[a][sid]), Decimal("0.30"), f"{sid} on {b}")

    def test_the_history_holds_inverted_and_upward_spells(self):
        """Runs of a month or more in which three months yields more than
        ten years, and runs in which it yields less."""
        short, long_ = rates.series_id("3M"), rates.series_id("10Y")
        longest, run, prev = {True: 0, False: 0}, 0, None
        for day in sorted(self.curve):
            inverted = self.curve[day][short] > self.curve[day][long_]
            run = run + 1 if inverted == prev else 1
            longest[inverted] = max(longest[inverted], run)
            prev = inverted
        self.assertGreaterEqual(longest[True], 21, "an inverted spell")
        self.assertGreaterEqual(longest[False], 21, "an upward-sloping spell")


class TestBondFund(Built):
    """The bond fund's lines, its risk rows and its page figures agree with
    one another."""

    @classmethod
    def setUpClass(cls):
        super().setUpClass()
        cat = reference.load()
        cls.provider = cat["provider"]
        (fund,) = [f for f in cat["funds"] if f.get("risk")]
        cls.fund_ref = reference.fund_ref(cls.inputs.instruments[fund["instrument"]])
        ref = cls.rows[silver.REFERENCE_SOURCE]
        cls.lines = collections.defaultdict(list)
        for r in ref["lines"]:
            if r["fund_ref"] == cls.fund_ref:
                cls.lines[r["as_of"]].append(r)
        cls.risk = collections.defaultdict(list)
        for r in ref["fund_risk"]:
            cls.risk[r["as_of"]].append(r)
        cls.figures = collections.defaultdict(dict)
        for r in ref["characteristics"]:
            if r["fund_ref"] == cls.fund_ref:
                cls.figures[r["as_of"]][(r["field"], r["label"])] = Decimal(r["value"])
        cls.providers = {r["provider"] for r in ref["characteristics"]}
        cls.nav = {c["as_of"]: Decimal(c["net_assets"]) for c in ref["compositions"] if c["fund_ref"] == cls.fund_ref}

    def test_only_the_bond_fund_states_risk(self):
        self.assertEqual({r["fund_ref"] for rs in self.risk.values() for r in rs}, {self.fund_ref})

    def test_every_bond_line_states_its_terms_and_figures(self):
        stated = ("maturity", "coupon", "coupon_kind", "duration", "modified_duration", "yield_to_maturity",
                  "yield_to_worst")
        extra = ("is_default", "in_arrears", "paid_in_kind", "yield_to_call", "real_duration", "real_yield")
        called = 0
        self.assertTrue(self.lines)
        for at, lines in self.lines.items():
            day = dates.day_of(at)
            bonds = [r for r in lines if r["asset_type"] == "Fixed Income"]
            self.assertEqual(collections.Counter(r["coupon_kind"] for r in bonds)["Floating"], 1, day)
            self.assertEqual({r["coupon_kind"] for r in bonds}, {"Fixed", "Floating"}, day)
            self.assertEqual(len([r for r in bonds if r["real_yield"] is not None]), 1, day)
            for r in bonds:
                where = f'{r["name"]} on {day}'
                self.assertTrue(all(r[c] is not None for c in stated), where)
                self.assertEqual((r["is_default"], r["in_arrears"], r["paid_in_kind"]), (0, 0, 0), where)
                self.assertGreater(dates.parse(r["maturity"]), day, where)
                ytm, ytw = Decimal(r["yield_to_maturity"]), Decimal(r["yield_to_worst"])
                # The worst date of a callable bond shortens its effective duration.
                self.assertLessEqual(ytw, ytm, where)
                self.assertTrue(0 < Decimal(r["duration"]) <= Decimal(r["modified_duration"]), where)
                if r["yield_to_call"] is not None:
                    called += 1
                    self.assertEqual(ytw, min(ytm, Decimal(r["yield_to_call"])), where)
                if r["real_yield"] is not None:
                    self.assertIsNotNone(r["real_duration"], where)
                    self.assertLess(Decimal(r["real_yield"]), ytm, where)
            for r in lines:
                if r["asset_type"] != "Fixed Income":
                    self.assertTrue(all(r[c] is None for c in stated + extra), f'{r["name"]} on {day}')
        self.assertEqual(called, len(self.lines), "one callable bond on every day")

    def test_a_government_bond_yields_inside_the_days_curve(self):
        published = collections.defaultdict(list)
        for r in self.rows[rates.SOURCE]["rates"]:
            published[dates.day_of(r["as_of"])].append(Decimal(r["value"]))
        for at, lines in self.lines.items():
            curve = published[dates.prev_business(dates.day_of(at))]
            for r in lines:
                if r["sector"] == "Government" and r["currency"] == "USD" and r["coupon_kind"] == "Fixed":
                    ytm = Decimal(r["yield_to_maturity"])
                    self.assertTrue(min(curve) - Decimal("0.01") <= ytm <= max(curve) + Decimal("0.01"),
                                    f'{r["name"]} on {dates.day_of(at)}')

    def test_risk_rows_are_quarter_ends_at_the_filings_tenors(self):
        tenors, none = goref.risk_tenors(), goref.no_currency()
        self.assertEqual(sorted(self.risk), sorted(at for at in self.nav if dates.day_of(at).month % 3 == 0))
        for rows in self.risk.values():
            by_ccy = collections.defaultdict(list)
            for r in rows:
                by_ccy[r["currency"]].append(r["tenor"])
            self.assertIn("USD", by_ccy)
            self.assertIn(none, by_ccy)
            for ccy, got in by_ccy.items():
                self.assertEqual(got, tenors, ccy)
            for r in rows:
                rate = (r["dv01"], r["dv100"])
                credit = (r["credit_ig_dv01"], r["credit_hy_dv01"])
                if r["currency"] == none:
                    self.assertEqual(rate, (None, None))
                    self.assertNotIn(None, credit)
                else:
                    self.assertEqual(credit, (None, None))
                    self.assertNotIn(None, rate)

    def test_dv01_is_value_times_duration_times_a_basis_point(self):
        """Summed over the curves' tenors, as the page's duration states it.
        A hundred basis points move a little less than a hundred times one:
        convexity earns some back."""
        for at, rows in self.risk.items():
            rate = [r for r in rows if r["dv01"] is not None]
            total = sum(Decimal(r["dv01"]) for r in rate)
            want = self.nav[at] * self.figures[at][("effective_duration", "")] / 10000
            self.assertLess(abs(total - want), want * Decimal("0.0001"), dates.day_of(at))
            for r in rate:
                dv01, dv100 = Decimal(r["dv01"]), Decimal(r["dv100"])
                if dv01:
                    self.assertTrue(90 * dv01 < dv100 < 100 * dv01, f'{r["currency"]} {r["tenor"]}')

    def test_credit_spread_rows_leave_out_the_governments(self):
        for at, rows in self.risk.items():
            ig = sum(Decimal(r["credit_ig_dv01"] or 0) for r in rows)
            hy = sum(Decimal(r["credit_hy_dv01"] or 0) for r in rows)
            total = sum(Decimal(r["dv01"] or 0) for r in rows)
            self.assertTrue(ig > 0 and hy > 0, dates.day_of(at))
            self.assertLess(ig + hy, total, dates.day_of(at))

    def test_the_page_states_duration_yields_and_credit_quality(self):
        """Every month-end: a duration near 6, yields, and a credit-quality
        breakdown whose shares sum to 100."""
        self.assertEqual(sorted(self.figures), sorted(self.nav))
        self.assertEqual(self.providers, {self.provider})
        labels = set()
        for at, f in self.figures.items():
            day = dates.day_of(at)
            for field in ("effective_duration", "yield_to_maturity", "sec_yield", "distribution_yield"):
                self.assertIn((field, ""), f, f"{field} on {day}")
            self.assertTrue(Decimal("4.5") <= f[("effective_duration", "")] <= Decimal("7.5"), day)
            self.assertLess(f[("sec_yield", "")], f[("yield_to_maturity", "")], day)
            quality = {label: v for (field, label), v in f.items() if field == "credit_quality"}
            self.assertEqual(sum(quality.values()), 100, day)
            labels |= set(quality)
        self.assertEqual(labels, {"AAA", "AA", "A", "BBB", "BB", reference.NOT_RATED})


class TestFundYields(Built):
    def test_a_fund_with_a_payout_states_its_distribution_yield(self):
        """Every month-end it has a composition, near the payout rate the
        instruments catalogue gives; a fund that pays nothing states
        none."""
        ref = self.rows[silver.REFERENCE_SOURCE]
        days = collections.defaultdict(set)
        for c in ref["compositions"]:
            days[c["fund_ref"]].add(c["as_of"])
        stated = collections.defaultdict(dict)
        for r in ref["characteristics"]:
            if r["field"] == "distribution_yield":
                stated[r["fund_ref"]][r["as_of"]] = Decimal(r["value"])
        paying = 0
        for f in reference.load()["funds"]:
            inst = self.inputs.instruments[f["instrument"]]
            key = reference.fund_ref(inst)
            payout = inst.get("income", {}).get("yield")
            if not payout:
                self.assertNotIn(key, stated, key)
                continue
            paying += 1
            self.assertEqual(set(stated[key]), days[key], key)
            for v in stated[key].values():
                self.assertLess(abs(v - Decimal(payout) * 100), Decimal(payout) * 20, key)
        self.assertGreater(paying, 3)


class TestFundFigures(Built):
    """The money-market fund's filings and the page of the fund that
    publishes no holdings: month-end figures, in the fields and the units
    the yield report and the exposure views read."""

    @classmethod
    def setUpClass(cls):
        super().setUpClass()
        cls.cat = reference.load()
        cls.figures = collections.defaultdict(lambda: collections.defaultdict(dict))
        for r in cls.rows[silver.REFERENCE_SOURCE]["characteristics"]:
            cls.figures[r["fund_ref"]][dates.day_of(r["as_of"])][(r["field"], r["label"])] = Decimal(r["value"])

    def month_ends(self, fund):
        return list(reference.month_ends(dates.parse(fund["from"]), AS_OF))

    def test_the_money_market_fund_files_its_yields_and_maturity(self):
        """Its seven-day yield after expenses, below the yield before them,
        and a weighted average maturity of at most the 60 days the rule
        allows, in days."""
        self.assertTrue(self.cat["money_market"])
        self.assertLessEqual({"seven_day_yield", "weighted_average_maturity"}, goref.fund_fields())
        for fund in self.cat["money_market"]:
            ref = reference.fund_ref(self.inputs.instruments[fund["instrument"]])
            self.assertEqual(sorted(self.figures[ref]), self.month_ends(fund), ref)
            for day, f in self.figures[ref].items():
                self.assertEqual(set(f), {("seven_day_yield", ""), ("seven_day_gross_yield", ""),
                                          ("weighted_average_maturity", ""), ("weighted_average_life", "")}, day)
                self.assertLess(f[("seven_day_yield", "")], f[("seven_day_gross_yield", "")], day)
                self.assertTrue(1 <= f[("weighted_average_maturity", "")] <= 60, day)
                self.assertLessEqual(f[("weighted_average_maturity", "")], f[("weighted_average_life", "")], day)

    def test_the_money_market_fund_keeps_its_dollar_and_pays_its_yield(self):
        """Its price is 1.00 on every snapshot. On each month's last business
        day it pays its units times the seven-day yield its filing states
        for the month, over twelve, and reinvests the payment at its
        dollar."""
        for fund in self.cat["money_market"]:
            iid = fund["instrument"]
            ref = reference.fund_ref(self.inputs.instruments[iid])
            held = sorted((dates.day_of(p["snapshot_at"]), p) for _, p in self.all("positions")
                          if p["instrument_id"] == iid)
            self.assertTrue(held)
            for day, p in held:
                self.assertEqual(Decimal(p["market_value"]), Decimal(p["quantity"]), day)
            moves = collections.defaultdict(dict)
            for _, t in self.all("transactions"):
                if t["instrument_id"] == iid:
                    moves[dates.day_of(t["occurred_at"])][t["kind"]] = t
            self.assertGreater(len(moves), 12)
            for day, m in moves.items():
                self.assertEqual(day, dates.last_business(day.year, day.month), day)
                units = [Decimal(p["quantity"]) for d, p in held if d < day][-1]
                pct = self.figures[ref][dates.month_end(day.year, day.month)][("seven_day_yield", "")]
                self.assertLessEqual(abs(Decimal(m["dividend"]["net_amount"]) - units * pct / 100 / 12),
                                     Decimal("0.01"), day)
                self.assertEqual(Decimal(m["buy"]["net_amount"]), -Decimal(m["dividend"]["net_amount"]), day)
                self.assertEqual(Decimal(m["buy"]["quantity"]), Decimal(m["dividend"]["net_amount"]), day)

    def test_a_page_fund_states_what_the_report_reads_and_its_maturity_bands(self):
        """Every month-end, and no composition: its yields, the worst below
        the one to maturity and the SEC yield below that, its duration and
        average life, and maturity bands whose shares sum to 100."""
        self.assertTrue(self.cat["pages"])
        self.assertIn("maturity_breakdown", goref.page_breakdowns())
        scalars = {"effective_duration", "weighted_average_maturity_years", "yield_to_maturity", "yield_to_worst",
                   "sec_yield", "distribution_yield"}
        self.assertLessEqual(scalars, goref.fund_fields())
        composed = {c["fund_ref"] for c in self.rows[silver.REFERENCE_SOURCE]["compositions"]}
        for fund in self.cat["pages"]:
            ref = reference.fund_ref(self.inputs.instruments[fund["instrument"]])
            self.assertNotIn(ref, composed)
            self.assertEqual(sorted(self.figures[ref]), self.month_ends(fund), ref)
            for day, f in self.figures[ref].items():
                self.assertEqual({field for field, label in f if not label}, scalars, day)
                self.assertTrue(f[("sec_yield", "")] < f[("yield_to_worst", "")] < f[("yield_to_maturity", "")], day)
                bands = {label: v for (field, label), v in f.items() if field == "maturity_breakdown"}
                self.assertEqual(sum(bands.values()), 100, day)
                self.assertLessEqual(set(bands), {label for label, _ in reference.MATURITY_BANDS} |
                                     {reference.CASH_BAND}, day)

    def test_the_bond_funds_maturity_bands_hold_its_lines(self):
        """The bond fund's bands sum to 100 every month-end, and its cash
        band is its cash lines' weight."""
        (fund,) = [f for f in self.cat["funds"] if f.get("risk")]
        ref = reference.fund_ref(self.inputs.instruments[fund["instrument"]])
        cash = collections.defaultdict(Decimal)
        for r in self.rows[silver.REFERENCE_SOURCE]["lines"]:
            if r["fund_ref"] == ref and r["asset_type"] == "Cash":
                cash[dates.day_of(r["as_of"])] += Decimal(r["weight"])
        for day, f in self.figures[ref].items():
            bands = {label: v for (field, label), v in f.items() if field == "maturity_breakdown"}
            self.assertEqual(sum(bands.values()), 100, day)
            self.assertEqual(bands.get(reference.CASH_BAND, Decimal(0)), cash[day], day)


class TestHeldBonds(Built):
    """The bonds the household holds directly: one of each coupon kind and
    a ladder of discount notes, priced off the curve, each stating its
    terms where the yield report reads them."""

    @classmethod
    def setUpClass(cls):
        super().setUpClass()
        cls.bonds = {iid: i for iid, i in cls.inputs.instruments.items() if i["vehicle"] == "bond"}
        cls.held = collections.defaultdict(list)
        for src, p in cls.all(cls, "positions"):
            if p["instrument_id"] in cls.bonds:
                cls.held[p["instrument_id"]].append((src, p))
        cls.txns = [(src, t) for src, t in cls.all(cls, "transactions") if t["instrument_id"] in cls.bonds]
        cls.ledger = [json.loads(line) for line in
                      (cls.root / "overrides" / "exposure_declarations.ndjson").read_text().splitlines()]

    def held_on(self, day):
        return {iid for iid, ps in self.held.items() for _, p in ps if dates.day_of(p["snapshot_at"]) == day}

    def test_a_bond_of_each_coupon_kind_and_a_discount_note(self):
        on = self.held_on(dates.prev_business(AS_OF))
        kinds = set()
        for iid in on:
            cp = self.bonds[iid].get("coupon")
            kinds.add("ladder" if "ladder" in self.bonds[iid] else "zero" if cp is None
                      else "floating" if "floating" in cp else "fixed")
        self.assertEqual(kinds, {"fixed", "zero", "floating", "ladder"})

    def price(self, p):
        """A bond position's clean price per 100 of face."""
        return (Decimal(p["market_value"]) - Decimal(p["accrued_interest"] or 0)) / Decimal(p["quantity"]) * 100

    def test_a_discount_note_trades_below_par_and_pays_par_at_maturity(self):
        notes = [iid for iid in self.held if "ladder" in self.bonds[iid]]
        self.assertGreater(len(notes), 3)
        for iid in notes:
            for _, p in self.held[iid]:
                self.assertLess(self.price(p), 100, f'{iid} on {dates.day_of(p["snapshot_at"])}')
                self.assertLess(dates.day_of(p["snapshot_at"]), dates.parse(self.bonds[iid]["maturity"]), iid)

    def test_the_ladder_rolls_into_its_next_note(self):
        """On its maturity a note pays back its cost, and its discount, the
        face less the cost, as interest that names it; so its face comes
        back whole, with no gain realized. The next note is bought the same
        day, in the same account, with a terms record of paper that pays
        no coupon and counts ACT/360. The note held at the as-of matures
        after it."""
        redeemed = [t for _, t in self.txns if t["description"].startswith("REDEMPTION")]
        self.assertTrue(redeemed)
        terms = {(d["silver_source_id"], d["instrument"]): d["terms"] for d in self.ledger if "terms" in d}
        realized = {r["realized_lot_id"].rsplit("-", 1)[0]: r for _, r in self.all("realized_lots")}
        for t in redeemed:
            inst, day = self.bonds[t["instrument_id"]], dates.day_of(t["occurred_at"])
            face, proceeds = -Decimal(t["quantity"]), Decimal(t["net_amount"])
            self.assertEqual(day, dates.next_business(dates.parse(inst["maturity"])))
            cost = [Decimal(p["book_value"]) for _, p in self.held[t["instrument_id"]]
                    if dates.day_of(p["snapshot_at"]) < day][-1]
            self.assertEqual(proceeds, cost.quantize(Decimal("0.01")), t["transaction_id"])
            self.assertLessEqual(abs(proceeds - Decimal(realized[t["transaction_id"]]["book_value"])), Decimal("0.01"))
            (earned,) = [i for _, i in self.txns if i["kind"] == "interest" and i["instrument_id"] == t["instrument_id"]
                         and dates.day_of(i["occurred_at"]) == day and i["account_id"] == t["account_id"]]
            self.assertEqual(Decimal(earned["net_amount"]) + proceeds, face, t["transaction_id"])
            self.assertGreater(Decimal(earned["net_amount"]), 0)
            buys = [b for _, b in self.txns if b["kind"] == "buy" and b["instrument_id"] == inst["next"]
                    and dates.day_of(b["occurred_at"]) == day and b["account_id"] == t["account_id"]]
            self.assertEqual(len(buys), 1, t["transaction_id"])
            self.assertLessEqual(-Decimal(buys[0]["net_amount"]), face)
        for iid, ps in self.held.items():
            if "ladder" in self.bonds[iid]:
                self.assertEqual(terms[(ps[0][0], iid)], {"frequency": 0, "coupon_kind": "zero", "day_count": "ACT/360"},
                                 iid)
        (last,) = [iid for iid in self.held_on(dates.prev_business(AS_OF)) if "ladder" in self.bonds[iid]]
        self.assertGreater(dates.parse(self.bonds[last]["maturity"]), AS_OF)

    def test_a_floating_note_restates_its_coupon_and_pays_it(self):
        """One record per coupon period, each stating the period's coupon,
        and every coupon is the face times the coupon of the period it
        ends."""
        floating = [iid for iid in self.held if "floating" in self.bonds[iid].get("coupon", {})]
        self.assertTrue(floating)
        for iid in floating:
            cp = self.bonds[iid]["coupon"]
            versions = sorted((r["valid_from"], json.loads(r["payload"])) for _, r in self.all("instruments")
                              if r["instrument_id"] == iid)
            self.assertEqual(versions[0][0], dates.epoch(self.inputs.history_start))
            # The first record holds from history start, at the coupon set
            # before it; the next from each reset after it, to the as-of.
            starts = [dates.day_of(v) for v, _ in versions[1:]]
            self.assertEqual(starts[0], bonds.next_coupon(cp, self.inputs.history_start - dt.timedelta(days=1)), iid)
            for a, b in zip(starts, starts[1:], strict=False):
                self.assertEqual(b, bonds.next_coupon(cp, a), iid)
            self.assertGreater(bonds.next_coupon(cp, starts[-1]), AS_OF, iid)
            coupons = [t for _, t in self.txns if t["kind"] == "coupon" and t["instrument_id"] == iid]
            self.assertTrue(coupons)
            for t in coupons:
                due = dates.day_of(t["occurred_at"])
                due = dates.clamp_day(due.year, due.month, cp["day"])
                stated = [p for v, p in versions if v < dates.epoch(due)][-1]
                self.assertEqual(stated["maturity_date"], self.bonds[iid]["maturity"])
                face = [Decimal(p["quantity"]) for _, p in self.held[iid]
                        if dates.day_of(p["snapshot_at"]) < dates.day_of(t["occurred_at"])][-1]
                want = (face * Decimal(stated["interest_rate"]) / 100 / len(cp["months"])).quantize(Decimal("0.01"))
                self.assertEqual(Decimal(t["net_amount"]), want, t["transaction_id"])

    def test_a_bond_that_states_its_terms_states_the_catalogues(self):
        """In the keys the yield pass reads from an instrument's record."""
        self.assertLessEqual({"interest_rate", "maturity_date"}, goref.payload_term_keys())
        stating = [iid for iid in self.held if self.bonds[iid].get("states_terms")]
        self.assertGreater(len(stating), 1)
        for _, r in self.all("instruments"):
            inst = self.bonds.get(r["instrument_id"])
            stated = json.loads(r["payload"])
            if not inst or not inst.get("states_terms"):
                self.assertEqual(stated, {}, r["instrument_id"])
                continue
            self.assertEqual(stated["maturity_date"], inst["maturity"])
            if "rate" in inst["coupon"]:
                self.assertEqual(Decimal(stated["interest_rate"]), Decimal(inst["coupon"]["rate"]) * 100)

    def test_a_callable_bond_trades_above_par_and_its_record_states_its_call(self):
        callable_ = [iid for iid in self.held if "call" in self.bonds[iid]]
        self.assertTrue(callable_)
        terms = {(d["silver_source_id"], d["instrument"]): d["terms"] for d in self.ledger if "terms" in d}
        for iid in callable_:
            src, last = self.held[iid][-1]
            self.assertLess(AS_OF, dates.parse(self.bonds[iid]["call"]), iid)
            self.assertGreater(self.price(last), 100, iid)
            self.assertEqual(terms[(src, iid)], {"call_date": self.bonds[iid]["call"]})

    def test_a_rating_record_rates_a_bond_its_name_does_not(self):
        """In a bucket the engine knows, and as the funds' lines rate the
        same bond."""
        ratings = [d for d in self.ledger if "rating" in d]
        self.assertTrue(ratings)
        lines = {sid: s.get("rating") for sid, s in reference.securities(reference.load(), self.inputs.instruments).items()}
        for d in ratings:
            inst = self.bonds[d["instrument"]]
            self.assertIn(d["instrument"], self.held)
            self.assertNotRegex(inst["name"].upper(), r"\b(MOODY'?S|S&P|FITCH)\b")
            self.assertIn(d["rating"]["bucket"], goref.rating_buckets())
            self.assertEqual(d["rating"]["bucket"], inst["rating"])
            if d["instrument"] in lines:
                self.assertEqual(lines[d["instrument"]], inst["rating"])


class TestStatedRates(Built):
    """The rates the household's balances earn: a tiered savings rate and
    a term deposit, both in UBS's shape on the balance's snapshot, and a
    rate only the declarations ledger states. Each balance earns what its
    rate says."""

    UBS_KEYS = ("CdtIntrstRate1", "CdtLmtAmt1", "CdtIntrstRate2", "CdtIntrstVldFrmDt", "EndOfDurtn",
                "CalIntrstBasisDesc")

    @classmethod
    def setUpClass(cls):
        super().setUpClass()
        cls.spec = cls.inputs.spec
        cls.cash = collections.defaultdict(dict)
        for _, c in cls.all(cls, "cash_balances"):
            cls.cash[(c["account_id"], c["currency"])][dates.day_of(c["snapshot_at"])] = c
        cls.interest = collections.defaultdict(list)
        cls.moves = collections.defaultdict(list)
        for _, t in cls.all(cls, "transactions"):
            cls.moves[(t["account_id"], t["currency"])].append(t)
            if t["kind"] == "interest" and not t["instrument_id"]:
                cls.interest[(t["account_id"], t["currency"])].append(t)

    def test_the_rate_keys_are_the_ones_the_yield_pass_reads(self):
        """It names a credit tier's keys by their side's prefix (Cdt) and
        the tier's number."""
        self.assertLessEqual(set(self.UBS_KEYS), goref.balance_rate_keys())

    def before(self, key, t):
        """A balance just before a transaction: its last snapshot before the
        day, and the day's moves booked ahead of it."""
        day = dates.day_of(t["occurred_at"])
        last = max(d for d in self.cash[key] if d < day)
        ahead = sum((Decimal(m["net_amount"]) for m in self.moves[key]
                     if dates.day_of(m["occurred_at"]) == day and m["occurred_at"] < t["occurred_at"]), Decimal(0))
        return Decimal(self.cash[key][last]["amount"]) + ahead

    def test_the_savings_balance_states_its_tiers_and_earns_them(self):
        si = self.spec["savings_interest"]
        key = (si["account"], "USD")
        for day, c in self.cash[key].items():
            stated = json.loads(c["payload"])
            self.assertEqual(set(stated), set(self.UBS_KEYS), day)
            self.assertEqual(stated["EndOfDurtn"], "9999-12-31")
            self.assertLessEqual(dates.parse(stated["CdtIntrstVldFrmDt"]), day)
            self.assertGreater(Decimal(stated["CdtIntrstRate1"]), Decimal(stated["CdtIntrstRate2"]))
        self.assertTrue(self.interest[key])
        for t in self.interest[key]:
            stated = json.loads(self.cash[key][dates.day_of(t["occurred_at"])]["payload"])
            balance, limit = self.before(key, t), Decimal(stated["CdtLmtAmt1"])
            earned = (min(balance, limit) * Decimal(stated["CdtIntrstRate1"])
                      + max(balance - limit, Decimal(0)) * Decimal(stated["CdtIntrstRate2"]))
            self.assertEqual(Decimal(t["net_amount"]), (earned / 100 / 12).quantize(Decimal("0.01")),
                             t["transaction_id"])

    def test_the_term_deposit_states_its_term_and_rolls_at_its_end(self):
        """Every snapshot states a term that ends after it. At its end the
        deposit earns its rate, simple on ACT/365, and the next term starts
        that day, at a rate near the curve's for its tenor."""
        td = self.spec["term_deposit"]
        key = (td["account"], "USD")
        terms = {}
        for day, c in sorted(self.cash[key].items()):
            stated = json.loads(c["payload"])
            start, end = dates.parse(stated["CdtIntrstVldFrmDt"]), dates.parse(stated["EndOfDurtn"])
            self.assertTrue(start <= day < end, day)
            self.assertEqual(end, dates.add_months(start, td["months"]))
            terms[start] = (end, Decimal(stated["CdtIntrstRate1"]))
        self.assertGreater(len(terms), 2)
        paid = {dates.day_of(t["occurred_at"]): Decimal(t["net_amount"]) for t in self.interest[key]}
        for start, (end, rate) in terms.items():
            if end <= AS_OF:
                principal = Decimal(self.cash[key][end - dt.timedelta(days=1)]["amount"])
                want = (principal * rate / 100 * (end - start).days / 365).quantize(Decimal("0.01"))
                self.assertEqual(paid[end], want, end)
                self.assertIn(end, terms)
        self.assertEqual(set(paid), {end for end, _ in terms.values() if end <= AS_OF})

    def test_a_balance_only_the_ledger_rates_earns_the_records_rate(self):
        """Every month, on its source's last day of the month, at the rate
        of the record in force for its account and currency."""
        records = [d for d in self.config_ledger() if "rate" in d]
        self.assertGreater(len({(d["account"], d["rate"]["currency"]) for d in records}), 1)
        cadence = {a["account_id"]: self.cadence[src] for src, a in self.all("accounts")}
        for d in records:
            key = (d["account"], d["rate"]["currency"])
            self.assertFalse(any(json.loads(c["payload"]) for c in self.cash[key].values()), key)
            self.assertTrue(self.interest[key], key)
        for key, ts in self.interest.items():
            mine = [d for d in records if (d["account"], d["rate"]["currency"]) == key]
            if not mine:
                continue
            for t in ts:
                day = dates.day_of(t["occurred_at"])
                end = dates.last_business(day.year, day.month) if cadence[key[0]] == "business" else \
                    dates.month_end(day.year, day.month)
                self.assertEqual(day, end, t["transaction_id"])
                rec = max((d for d in mine if dates.parse(d["as_of"]) <= day), key=lambda d: d["as_of"])
                want = (self.before(key, t) * Decimal(str(rec["rate"]["pct"])) / 100 / 12).quantize(Decimal("0.01"))
                self.assertEqual(Decimal(t["net_amount"]), want, t["transaction_id"])

    def config_ledger(self):
        return [json.loads(line) for line in
                (self.root / "overrides" / "exposure_declarations.ndjson").read_text().splitlines()]


class TestLedgerRecords(Built):
    """The declarations ledger's other yield records agree with what the
    household's simulation does."""

    @classmethod
    def setUpClass(cls):
        super().setUpClass()
        cls.ledger = [json.loads(line) for line in
                      (cls.root / "overrides" / "exposure_declarations.ndjson").read_text().splitlines()]

    def test_a_declared_dividend_pays_the_records_amount(self):
        declared = [d for d in self.ledger if "dividend" in d]
        self.assertTrue(declared)
        paid = 0
        for src, t in self.all("transactions"):
            mine = [d for d in declared if (d["silver_source_id"], d["instrument"]) == (src, t["instrument_id"])]
            if t["kind"] != "dividend" or not mine:
                continue
            day = dates.day_of(t["occurred_at"])
            rec = max((d for d in mine if dates.parse(d["as_of"]) <= day), key=lambda d: d["as_of"])
            (qty,) = [Decimal(p["quantity"]) for p in self.rows[src]["positions"]
                      if p["instrument_id"] == t["instrument_id"] and p["account_id"] == t["account_id"]
                      and dates.day_of(p["snapshot_at"]) == day]
            self.assertEqual(Decimal(t["net_amount"]),
                             (qty * Decimal(str(rec["dividend"]["amount"]))).quantize(Decimal("0.01")))
            self.assertEqual(t["currency"], rec["dividend"]["currency"])
            paid += 1
        self.assertGreater(paid, 1)

    def test_the_loans_terms_are_the_mortgages(self):
        m = self.inputs.spec["mortgage"]
        loans = [d for d in self.ledger if "terms" in d and d["instrument"] == "loan"]
        (terms,) = [d["terms"] for d in loans]
        first = dates.parse(m["first_payment"])
        self.assertEqual(Decimal(str(terms["coupon"])), Decimal(m["rate"]) * 100)
        self.assertEqual(terms["frequency"], 12)
        self.assertEqual(terms["maturity"], dates.add_months(first, m["term_months"] - 1).isoformat())
        self.assertEqual(terms["fixed_until"], dates.add_months(first, m["fixed_months"] - 1).isoformat())


class TestPeriodFacts(Built):
    """The companies' reported figures: filed weeks after their periods,
    in the forms and periods a 10-Q and a 10-K state."""

    @classmethod
    def setUpClass(cls):
        super().setUpClass()
        cls.facts = cls.rows[silver.REFERENCE_SOURCE]["period_facts"]
        cls.filers = {reference.fund_ref(cls.inputs.instruments[f["instrument"]]): f
                      for f in reference.load()["facts"] if "filings" in f}

    def test_every_filer_and_only_a_filer_reports(self):
        self.assertTrue(self.filers)
        self.assertEqual({r["security_key"] for r in self.facts}, set(self.filers))

    def test_a_figure_is_filed_weeks_after_its_period_and_by_the_as_of(self):
        for r in self.facts:
            end, filed = dates.day_of(r["as_of"]), dates.parse(r["filed"])
            self.assertTrue(30 <= (filed - end).days <= 50, r)
            self.assertLessEqual(filed, AS_OF)
            self.assertTrue(dates.is_business(filed), r)
            self.assertEqual(r["run_at"], dates.epoch(filed))

    def test_periods_forms_schemes_and_units(self):
        for r in self.facts:
            end, start = dates.day_of(r["as_of"]), dates.parse(r["period_start"])
            period = r["fiscal_period"]
            self.assertEqual(r["form"], "10-Q" if period in ("Q1", "Q2", "Q3") else "10-K", r)
            scheme, unit = (filings.FLOWS | filings.BALANCES)[r["field"]]
            self.assertEqual((r["scheme"], r["unit"]), (scheme, unit))
            self.assertEqual(unit, "USD/shares" if r["field"] in filings.PER_SHARE else "USD")
            if r["field"] in filings.BALANCES:
                self.assertEqual(start, end, r)
                self.assertNotEqual(period, "Q4", r)
            else:
                months = 12 if period == "FY" else 3
                self.assertEqual(start, dates.add_months(end + dt.timedelta(days=1), -months), r)
            if period == "FY":
                self.assertEqual(r["fiscal_year"], end.year, r)
        year_ends = {(r["security_key"], dates.day_of(r["as_of"]).month) for r in self.facts if r["fiscal_period"] == "FY"}
        want = {(key, f["filings"].get("fiscal_year_end", 12)) for key, f in self.filers.items()}
        self.assertLessEqual(year_ends, want)
        self.assertGreater(len({m for _, m in want}), 1, "a company whose fiscal year ends before December")

    def test_the_fields_are_the_ones_gold_counts(self):
        """Gold's wanted list reads a company's figures by field name, so a
        field it does not name would be written and never read."""
        self.assertEqual(set(filings.FLOWS) | set(filings.BALANCES), goref.period_fields())

    def test_schemes_and_units_are_the_collectors(self):
        """Each figure is stated under a us-gaap concept the refdata
        collector reads it from, in the unit it reads, and the balances are
        exactly its figures that hold on a day."""
        figures = sec_figures()
        stated = filings.FLOWS | filings.BALANCES
        self.assertEqual(set(stated), set(figures))
        for field, (scheme, unit) in stated.items():
            taxonomy, concept = scheme.split(":")
            self.assertEqual(taxonomy, "us-gaap", field)
            self.assertIn(concept, figures[field]["gaap"], field)
            self.assertEqual(unit, figures[field]["unit"], field)
        self.assertEqual(set(filings.BALANCES), {k for k, f in figures.items() if f["instant"]})

    def test_a_year_is_the_sum_of_its_quarters(self):
        sums = collections.defaultdict(lambda: collections.defaultdict(Decimal))
        quarters = collections.defaultdict(set)
        years = {}
        for r in self.facts:
            if r["field"] in filings.FLOWS and r["field"] != "eps_diluted":
                key = (r["security_key"], r["fiscal_year"], r["field"])
                if r["fiscal_period"] == "FY":
                    years[key] = Decimal(r["value"])
                else:
                    sums[key][r["fiscal_period"]] = Decimal(r["value"])
                    quarters[key].add(r["fiscal_period"])
        checked = 0
        for key, total in years.items():
            if quarters[key] == {"Q1", "Q2", "Q3", "Q4"}:
                self.assertEqual(sum(sums[key].values()), total, key)
                checked += 1
        self.assertGreater(checked, 50)

    def test_payers_state_dividends_and_one_company_pays_none(self):
        fields = collections.defaultdict(set)
        for r in self.facts:
            fields[r["security_key"]].add(r["field"])
        everything = set(filings.FLOWS) | set(filings.BALANCES)
        unpaid = 0
        for key, f in self.filers.items():
            pays = "income" in self.inputs.instruments[f["instrument"]]
            self.assertEqual(fields[key], everything if pays else everything - {"dividends_per_share"}, key)
            unpaid += not pays
        self.assertGreater(unpaid, 0)
        self.assertTrue(any(Decimal(r["value"]) < 0 for r in self.facts if r["field"] == "eps_diluted"),
                        "a company that loses money")


class TestYieldConfig(Built):
    """The config's yield block, in the engine's vocabulary."""

    @classmethod
    def setUpClass(cls):
        super().setUpClass()
        cls.block = cls.config["yield"]

    def test_the_block_names_curves_withholding_conventions_cycles_and_a_horizon(self):
        self.assertEqual(set(self.block), {"curves", "withholding", "rating_funds", "conventions", "horizon_months",
                                           "cycles"})
        self.assertEqual(self.block["horizon_months"], 12)

    def test_a_rating_fund_is_a_held_fund_that_publishes_no_holdings(self):
        """Sliced by rating to a range of buckets, best first: so its credit
        comes from the config alone."""
        pages = {reference.fund_ref(self.inputs.instruments[f["instrument"]]) for f in reference.load()["pages"]}
        held = {reference.fund_ref(self.inputs.instruments[p["instrument_id"]])
                for _, p in self.all("positions") if p["instrument_id"] in self.inputs.instruments}
        buckets = goref.rating_buckets()
        self.assertTrue(self.block["rating_funds"])
        for ref, rating in self.block["rating_funds"].items():
            self.assertIn(ref, pages & held)
            best, _, worst = rating.partition("-")
            self.assertLessEqual(buckets.index(best), buckets.index(worst or best), rating)

    def test_the_curve_names_the_published_series(self):
        published = {r["series_id"] for r in self.rows[rates.SOURCE]["rates"]}
        ids = {s["id"] for s in self.config["silver_sources"]}
        [(ccy, curve)] = self.block["curves"].items()
        self.assertEqual(ccy, rates.CURRENCY)
        self.assertEqual(set(curve), set(rates.TENOR_MONTHS))
        for tenor, series in curve.items():
            source, sid = series.split(":")
            self.assertIn(source, ids)
            self.assertEqual(sid, rates.series_id(tenor))
            self.assertIn(sid, published)

    def test_withholding_is_what_the_household_is_withheld(self):
        """A country's default rate is the one the simulation withholds on
        the household's holdings there, in percent."""
        withheld = {}
        for inst in self.inputs.instruments.values():
            rate = inst.get("income", {}).get("withholding")
            if rate and inst.get("isin"):
                withheld[inst["isin"][:2]] = Decimal(rate) * 100
        table = self.block["withholding"]
        self.assertEqual(set(table), set(withheld))
        default = goref.withholding_default()
        wrappers = goref.tax_wrappers() | {default}
        for country, rates_ in table.items():
            self.assertEqual(Decimal(str(rates_[default])), withheld[country], country)
            self.assertLessEqual(set(rates_), wrappers, country)
        self.assertTrue(any(len(r) > 1 for r in table.values()), "a rate for one tax wrapper")

    def test_conventions_and_cycles_use_the_engine_vocabulary(self):
        for market, c in self.block["conventions"].items():
            self.assertRegex(market, r"^[A-Z]{3}$")
            self.assertIn(c["frequency"], goref.coupon_frequencies())
            self.assertIn(c["day_count"], goref.day_counts())
        self.assertEqual(len(self.block["conventions"]), 1)
        for sector, cycle in self.block["cycles"].items():
            self.assertIn(sector, goref.equity_sectors())
            self.assertIn(cycle, goref.cycles())
        self.assertEqual(len(self.block["cycles"]), 1)


if __name__ == "__main__":
    unittest.main()
