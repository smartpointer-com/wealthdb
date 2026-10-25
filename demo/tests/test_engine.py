"""A short demo household, loaded through the engine.

Builds the household's first months into scratch demo roots, runs the
real `wealthdb init` and `load -a` on them through the engine image, and
reads the reports the demo is meant to keep clean. A second case appends
six weeks to a loaded root, loads only the new days, and requires every
report to match a gold built from scratch at the later date. The six
weeks hold the filings of the quarter that ends on the first build's
day, so the reference source's run reaches back past its new days. A
third case builds the whole history to today, as `make demo` does: the
income projection starts from today's holdings, so only a household
held to today shows what it expects. The engine runs with a scratch root
as its data root and its config dir, so it sees nothing else.

Needs Docker and the engine image, so it runs only when
WEALTHDB_DEMO_TEST_ROOT names a directory for the scratch roots
(`make test-demo` sets it, under the cache dir, where Docker can see
it). The directory is the tests' own: it carries a marker, and a
directory that exists without one is left alone. The `short` and
`today` golds are kept when the generator and the engine image are
the same; `fresh` is too, at its later date. `rolled` is built
again, because an append changes it. WEALTHDB_BIN names the engine
wrapper.
"""

import collections
import csv
import datetime as dt
import io
import json
import os
import pathlib
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import unittest
import unittest.mock
from decimal import Decimal

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))

import generate  # noqa: E402
from demohouse import bonds, dates, rates, reference, spec  # noqa: E402

PARENT = os.environ.get("WEALTHDB_DEMO_TEST_ROOT")
MARKER = ".wealthdb-demo-tests"
FIRST, SECOND = dt.date(2023, 9, 30), dt.date(2023, 11, 15)
TODAY = dt.datetime.now(dt.timezone.utc).date()
SEED = "harlow-19"
REPO = pathlib.Path(__file__).resolve().parents[2]
# The kinds of holding the yield report tells apart (wealthdb/docs/YIELD.md
# §3): every one but `note`, a loan held, which the household does not
# hold. Its target-date and age-based funds are of class multi_asset.
YIELD_KINDS = {"deposit", "term_deposit", "money_market", "bond", "bill", "floating_note", "bond_fund", "equity_fund",
               "multi_asset", "stock", "liability", "other"}
# The kinds that need no figure of their own: no income line names an
# `other` holding. In the first months the multi-asset funds have paid
# nothing yet: the 529 portfolio distributes in December, and the
# target-date funds never.
FIGURELESS_KINDS = {"other"}
FIRST_FIGURELESS_KINDS = FIGURELESS_KINDS | {"multi_asset"}
# The views of exposure that group the debt, the stocks and the issuers.
DEBT_VIEWS = ("credit", "maturities", "coupons", "cycles", "issuers")


def scratch(name):
    """A fresh demo root under the tests' own directory."""
    parent = tests_dir()
    root = parent / name
    if root.exists():
        shutil.rmtree(root)
    return root


def tests_dir():
    """The directory the scratch roots live in. One that exists without
    the tests' marker is left alone."""
    parent = pathlib.Path(PARENT)
    if parent.exists() and not (parent / MARKER).exists():
        raise unittest.SkipTest(f"{parent} exists and is not the demo tests' directory")
    parent.mkdir(parents=True, exist_ok=True)
    (parent / MARKER).write_text("Scratch demo roots of demo/tests/test_engine.py.\n")
    return parent


# short, today and fresh are built and then only read. rolled is
# appended to, so a later run cannot reuse it.
REUSED = {"short", "today", "fresh"}
STAMP = ".gold-stamp"
LOAD_OUT = ".gold-load"


def inputs_hash():
    """The spec, the catalogue and the generator, as each silver file records them."""
    inputs = spec.load()
    return f"{inputs.spec_hash}\n{inputs.catalogue_hash}\n{inputs.code_hash}"


def image_tag(root=REPO):
    """The tag the wrappers use for the checkout at root, as
    shared/wrappers/image-tag.sh reads it, or empty when root's
    .image-tag names no valid tag."""
    out = subprocess.run(["sh", "-c", '. "$1" && wealthdb_image_tag "$2"', "_",
                          str(REPO / "shared" / "wrappers" / "image-tag.sh"), str(root)],
                         capture_output=True, text=True, timeout=60)
    return out.stdout.strip() if out.returncode == 0 else ""


def engine_image_id():
    """The id of the engine image this checkout runs, or empty when it cannot be read."""
    tag = image_tag()
    if not tag:
        return ""
    out = subprocess.run(["docker", "image", "inspect", f"wealthdb:{tag}",
                          "--format", "{{.Id}}"], capture_output=True, text=True, timeout=60)
    if out.returncode != 0:
        return ""
    return out.stdout.strip()


def gold_stamp(as_of, image_id, files_hash, seed):
    return f"{as_of.isoformat()}\n{files_hash}\n{image_id}\n{seed}\n"


def reused_gold(root, stamp):
    """True when root was built from stamp and has not been left mid-write."""
    db = root / "wealthdb.db"
    return (root / STAMP).is_file() and (root / LOAD_OUT).is_file() and db.is_file() \
        and not db.with_name(db.name + ".wal").exists() \
        and (root / STAMP).read_text() == stamp


def build_and_load(name, as_of):
    image_id = engine_image_id()
    stamp = gold_stamp(as_of, image_id, inputs_hash(), SEED)
    if name in REUSED and image_id:
        root = tests_dir() / name
        if reused_gold(root, stamp):
            return root, (root / LOAD_OUT).read_text()
    root = scratch(name)
    generate.build(root, as_of, SEED)
    engine(root, "init")
    loaded = engine(root, "load", "-a")
    if name in REUSED and image_id:
        (root / STAMP).write_text(stamp)
        (root / LOAD_OUT).write_text(loaded)
    return root, loaded


def engine(root, *args, read_only=False, quiet=False):
    """The engine's stdout for args on root; `quiet` also requires that
    it print nothing on stderr, no warning or note."""
    env = dict(os.environ, WEALTHDB_DATA_ROOT=str(root), XDG_CONFIG_HOME=str(root))
    wrapper = os.environ.get("WEALTHDB_BIN") or str(REPO / "wealthdb" / "wealthdb")
    cmd = [wrapper] + (["-r"] if read_only else []) + ["-c", str(root / "wealthdb.cfg"), *args]
    out = subprocess.run(cmd, env=env, capture_output=True, text=True, timeout=600)
    if out.returncode != 0:
        raise AssertionError(f"{' '.join(args)} failed ({out.returncode}): {out.stderr or out.stdout}")
    if quiet and out.stderr:
        raise AssertionError(f"{' '.join(args)} printed on stderr: {out.stderr}")
    return out.stdout


def report(root, *args):
    return list(csv.DictReader(io.StringIO(engine(root, *args, "-f", "csv", read_only=True))))


def amount(row, column):
    return Decimal(row[column] or 0)


def bond_kind(inst):
    """The yield kind of a bond the catalogue lists."""
    if "ladder" in inst:
        return "bill"
    return "floating_note" if "floating" in inst.get("coupon", {}) else "bond"


def held_kinds(day):
    """The kinds the household holds on `day`: YIELD_KINDS, less the kind
    of bonds that have all matured by then. A ladder lasts to its last
    note."""
    inputs = spec.load()
    opening = {iid for s in inputs.spec["sources"] for a in s["accounts"] for iid in a.get("holdings", {})}
    alive = collections.defaultdict(bool)
    for iid, inst in inputs.instruments.items():
        if inst["vehicle"] == "bond" and (iid in opening or "ladder" in inst):
            alive[bond_kind(inst)] |= dates.parse(inst["maturity"]) > day
    return {kind for kind in YIELD_KINDS if alive.get(kind, True)}


def readings(row):
    """What a holding's figures rest on: its quality tags but `thin`. In
    the first months of history every holding was held under half the
    year, so it has no realized yield and carries `thin` beside them."""
    return set(row["quality"].split()) - {"thin"}


def check_yield_kinds(test, root, day, figureless=FIGURELESS_KINDS):
    """Every kind the household holds shows in `yield positions`, each
    but `figureless` with a non-zero figure of its own: a yield, a stated
    rate or a dividend. The holdings' figures rest on every reading a
    source or the ledger gives."""
    kinds = held_kinds(day)
    rows = report(root, "yield", "positions", "-d", day.isoformat(), "-C", "all")
    by_kind = collections.defaultdict(list)
    for r in rows:
        by_kind[r["kind"]].append(r)
    test.assertEqual(set(by_kind), kinds)
    for kind, rs in by_kind.items():
        if kind not in figureless:
            test.assertTrue(any(amount(r, f) != 0 for r in rs
                                for f in ("implied_yield_pct", "stated_pct", "dividend_yield_pct")), kind)
    tags = {t for r in rows for t in r["quality"].split()}
    want = {"implied", "stated", "indicated", "declared", "stale_price", "realized"}
    test.assertLessEqual(want | ({"floating"} if "floating_note" in kinds else set()), tags)
    return by_kind


def check_views_reconcile(test, root, day, views=DEBT_VIEWS):
    """Each exposure view sums to the global total, within a cent of
    rounding per row. Returns the total and each view's rows."""
    total = Decimal(report(root, "holdings", "global", "-d", day.isoformat())[0]["total_value_USD"])
    out = {}
    for view in views:
        rows = out[view] = report(root, "exposure", view, "-d", day.isoformat())
        got = sum(amount(r, "value_USD") for r in rows)
        test.assertLessEqual(abs(got - total), Decimal("0.01") * len(rows), view)
    return total, out


@unittest.skipUnless(PARENT, "WEALTHDB_DEMO_TEST_ROOT is not set (make test-demo sets it)")
class TestThroughTheEngine(unittest.TestCase):
    """The reports a freshly built demo is meant to keep clean."""

    @classmethod
    def setUpClass(cls):
        cls.root, cls.load = build_and_load("short", FIRST)

    def test_every_source_loads(self):
        for src in spec.load().spec["sources"]:
            self.assertIn(f"load: {src['id']}:", self.load)
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

    def test_exposure_reconciles(self):
        """Every exposure view sums to the global total, and coverage's
        `how` section explains all of it, each within a cent of rounding
        per row. Every fund with a composition is looked through, and
        every proxied fund proxied. The money-market fund and the fund
        that publishes only its page have no holdings to look through:
        coverage lists them as `unmatched`."""
        total, views = check_views_reconcile(self, self.root, FIRST, (
            "classes", "sectors", "sizes", "countries", "currencies", "situs", "securities", "funds"))
        coverage = report(self.root, "exposure", "coverage", "-d", FIRST.isoformat())
        how = [r for r in coverage if r["section"] == "how"]
        got = sum(Decimal(r["value_USD"]) for r in how)
        self.assertLessEqual(abs(got - total), Decimal("0.01") * len(how), "coverage")
        inputs = spec.load()
        covered = {reference.fund_ref(inputs.instruments[f["instrument"]])
                   for f in reference.load()["funds"] if dates.parse(f["from"]) <= FIRST}
        funds = {r["fund"]: r["via"] for r in views["funds"]}
        self.assertTrue(covered)
        for ref in covered:
            self.assertEqual(funds.get(ref), "direct", ref)
        proxies = inputs.spec["config"]["exposure"]["proxies"]
        self.assertTrue(proxies)
        for ref in proxies:
            self.assertEqual(funds.get(ref), "proxy", ref)
        cat = reference.load()
        leaves = {reference.fund_ref(inputs.instruments[f["instrument"]]) for f in cat["money_market"] + cat["pages"]}
        unmatched = {r["reason"] for r in coverage if r["section"] == "unmatched"}
        self.assertTrue(leaves)
        self.assertLessEqual(leaves, unmatched)

    def test_every_kind_of_holding_has_its_yield_figures(self):
        """The mortgage, held three months of the year, is thin: it has no
        realized yield."""
        by_kind = check_yield_kinds(self, self.root, FIRST, FIRST_FIGURELESS_KINDS)
        (loan,) = by_kind["liability"]
        self.assertEqual(loan["quality"].split(), ["declared", "thin"])
        self.assertEqual(loan["realized_yield_pct"], "")
        self.assertLess(Decimal(loan["dv01_USD"]), 0)

    def test_a_bond_priced_off_the_curve_implies_its_curve_yield(self):
        """The yield the engine implies from a bond's price is the curve at
        its life plus its spread, within two basis points and the report's
        rounding: to its call where the call prices lower. A discount note
        implies its discount rate."""
        day = dates.prev_business(FIRST)
        inputs = spec.load()
        points = rates.on(rates.curve(SEED, inputs.fx_start, day), day)
        rows = report(self.root, "yield", "positions", "-d", day.isoformat(), "-C", "all")
        checked = 0
        for r in rows:
            inst = inputs.instruments.get(r["position_key"])
            kind = inst and inst["model"].get("type")
            if kind not in ("curve", "discount"):
                continue
            spread = Decimal(inst["model"]["spread"])
            if kind == "discount":
                days = Decimal((dates.parse(inst["maturity"]) - day).days)
                want = bonds.curve_yield(points, days / bonds.YEAR_DAYS, spread)
            else:
                ends = [inst["maturity"]] + ([inst["call"]] if "call" in inst else [])
                rate = Decimal(inst["coupon"]["rate"]) * 100 if "coupon" in inst else Decimal(0)
                worst = min(ends, key=lambda e: bonds.price(
                    rate, bonds.curve_yield(points, bonds.life(day, e), spread), bonds.life(day, e),
                    bonds.frequency(inst.get("coupon"))))
                want = bonds.curve_yield(points, bonds.life(day, worst), spread)
            self.assertLessEqual(abs(amount(r, "implied_yield_pct") - want), Decimal("0.025"), r["position_key"])
            checked += 1
        self.assertGreater(checked, 4)

    def test_a_stock_unpaid_yet_reads_its_filings_dividend(self):
        """A company that reports to the SEC and pays in none of the first
        months of history has no payment to repeat: its indicated
        dividend is its filings'."""
        inputs = spec.load()
        months = {d.month for d in dates.days(inputs.history_start, FIRST)}
        filers = {f["instrument"] for f in reference.load()["facts"] if "filings" in f}
        unpaid = [i for i in inputs.instruments.values() if i["id"] in filers and "income" in i
                  and not set(i["income"]["months"]) & months]
        self.assertTrue(unpaid)
        rows = {r["symbol"]: r for r in report(self.root, "yield", "positions", "-d", FIRST.isoformat(), "-C", "all")}
        con = sqlite3.connect(self.root / "silver" / "quayvane.db")
        self.addCleanup(con.close)
        for inst in unpaid:
            self.assertFalse(con.execute("SELECT 1 FROM transactions WHERE kind = 'dividend' AND instrument_id = ?",
                                         (inst["id"],)).fetchall(), inst["id"])
            row = rows[inst["symbol"]]
            self.assertGreater(amount(row, "dividend_yield_pct"), 0, inst["id"])
            self.assertEqual(readings(row), {"indicated"}, inst["id"])

    def test_each_kind_of_cash_states_its_rate(self):
        """A tiered savings rate and a term deposit, both stated, and a
        rate only the ledger declares."""
        rows = report(self.root, "yield", "cash", "-d", FIRST.isoformat(), "-C", "all")
        (term,) = [r for r in rows if r["kind"] == "term_deposit"]
        self.assertGreater(dates.parse(term["term_end"]), FIRST)
        self.assertEqual(readings(term), {"stated"})
        self.assertTrue([r for r in rows if r["kind"] == "deposit" and r["tiers"] == "2" and readings(r) == {"stated"}])
        self.assertTrue([r for r in rows if r["currency"] == "EUR" and readings(r) == {"declared"} and r["rate_pct"]])
        self.assertTrue([r for r in rows if r["kind"] == "money_market" and r["rate_pct"]])

    def test_the_debt_and_issuer_views_reconcile(self):
        """Each sums to net worth, the credit view in every bucket from AAA
        to B, the coupons in every kind."""
        _total, views = check_views_reconcile(self, self.root, FIRST)
        buckets = {r["bucket"] for r in views["credit"]}
        self.assertLessEqual({"AAA", "AA", "A", "BBB", "BB", "B", "unrated", "cash", "(none)"}, buckets)
        kinds = {r["coupon_kind"] for r in views["coupons"]}
        self.assertLessEqual({"fixed", "floating", "inflation_linked", "zero"}, kinds)

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
        self.assertIn("watermark 1696032000 → 1700006400", self.load)

    def test_every_report_matches_a_fresh_build(self):
        """Compared as row sets: some reports order ties arbitrarily."""
        reports = [
            ("holdings", "accounts", "-C", "all"), ("holdings", "positions", "--with-cash", "-C", "all"),
            ("returns", "accounts", "--method", "both", "--period", "monthly"),
            ("returns", "global", "--method", "both", "--period", "total"),
            ("spending", "transactions", "-", "today"), ("income", "transactions", "-", "today"),
            ("cashflow", "flows", "-", "today", "--level", "group"), ("cashflow", "coverage", "-", "today"),
            ("gains", "positions", "-", "today", "-C", "all"), ("gains", "realized", "-", "today", "--documents", "all"),
            ("transactions", "-", "today", "-C", "all"),
            *(("exposure", view, "-d", SECOND.isoformat()) for view in ("funds", "classes", "coverage", *DEBT_VIEWS)),
            *(("yield", view, "-d", SECOND.isoformat(), "-C", "all")
              for view in ("positions", "cash", "funds", "curve", "coverage")),
        ]
        for args in reports:
            rolled = sorted(engine(self.rolled, *args, "-f", "csv", read_only=True).splitlines())
            fresh = sorted(engine(self.fresh, *args, "-f", "csv", read_only=True).splitlines())
            self.assertEqual(rolled, fresh, " ".join(args))

    def test_the_wanted_list_matches_a_fresh_build(self):
        """It reads the reference records no report compared above reads:
        the funds' risk rows, characteristics and line durations, and the
        companies' reported figures. rate_observations has no reader to
        compare."""
        def wanted(root):
            out = json.loads(engine(root, "refdata-wanted", read_only=True))
            del out["generated_at"]
            return out

        rolled = wanted(self.rolled)
        self.assertTrue(rolled["funds"] and rolled["securities"])
        self.assertEqual(rolled, wanted(self.fresh))


@unittest.skipUnless(PARENT, "WEALTHDB_DEMO_TEST_ROOT is not set (make test-demo sets it)")
class TestToToday(unittest.TestCase):
    """The whole household to today, as `make demo` builds it."""

    @classmethod
    def setUpClass(cls):
        cls.root, _ = build_and_load("today", TODAY)

    def test_every_kind_of_holding_has_its_yield_figures(self):
        check_yield_kinds(self, self.root, TODAY)

    def test_a_holding_held_most_of_the_year_has_its_realized_yield(self):
        """A holding held for more than half the year has a realized
        yield; one thin, near zero on most of its days, has none and was
        bought within half a year."""
        rows = report(self.root, "yield", "positions", "-d", TODAY.isoformat(), "-C", "all")
        bought = {(r["account_id"], r["position_key"]): r["acquisition_date"]
                  for r in report(self.root, "holdings", "positions", "-d", TODAY.isoformat(), "-C", "all")}
        thin = [r for r in rows if "thin" in r["quality"].split()]
        self.assertTrue([r for r in rows if r["realized_yield_pct"]])
        for r in rows:
            self.assertEqual(r["realized_yield_pct"] == "", r in thin, r["position_key"])
        for r in thin:
            day = bought.get((r["account_id"], r["position_key"]))
            self.assertTrue(day and (TODAY - dates.parse(day)).days < 183, (r["position_key"], day))

    def test_the_projection_holds_each_kind_of_income_and_a_principal(self):
        """Over the projection's horizon: the coupons, interest, dividends
        and distributions the holdings pay, a principal the term deposit
        returns, and the tax the withholding table withholds. While the
        ladder lasts, its maturing note earns the rate of the brokerage's
        cash: the default rule reinvests it, and leaving it idle earns
        less."""
        def total(*args):
            (row,) = [r for r in report(self.root, "income", "projection", "-C", "+principal", *args)
                      if r["period"] == "total"]
            return row

        cash = total()
        for kind in ("interest", "coupons", "dividends", "distributions", "principal", "withheld"):
            self.assertGreater(amount(cash, f"{kind}_USD"), 0, kind)
        self.assertTrue([r for r in report(self.root, "income", "calendar") if r["kind"] == "principal"])
        if "bill" in held_kinds(TODAY):
            self.assertIn("reinvested", cash["basis"])
            self.assertGreater(amount(cash, "expected_USD"), amount(total("--maturing", "idle"), "expected_USD"))

    def test_the_debt_and_issuer_views_reconcile(self):
        check_views_reconcile(self, self.root, TODAY)

    def test_the_dividend_calendar_states_and_teaches_record_days(self):
        """One company's calendar states the record day of each payment.
        Another's ends, and the gap its earlier events show gives the
        record day of its later payments."""
        rows = {r["reason"]: r for r in report(self.root, "yield", "coverage", "-d", TODAY.isoformat())
                if r["section"] == "record_day"}
        for reason in ("stated", "learned"):
            self.assertIn(reason, rows)
            self.assertGreater(int(rows[reason]["payments"] or 0), 0, reason)

    def test_with_nothing_configured_every_yield_view_answers(self):
        """A new installation's first day: the household with no `yield`
        block in its config (no withholding table, no curves, the default
        horizon), no declarations ledger and no dividend calendar loads,
        and every yield view, the income projection and the cash-flow
        plan answer, printing nothing on stderr and no null."""
        root = scratch("bare")
        for d in ("silver", "overrides"):
            shutil.copytree(self.root / d, root / d)
        (root / "overrides" / "exposure_declarations.ndjson").unlink()
        with sqlite3.connect(root / "silver" / "refdata.db") as db:
            db.execute("DELETE FROM dividend_events")
        cfg = json.loads((self.root / "wealthdb.cfg").read_text())
        del cfg["yield"], cfg["exposure"]["declarations"]
        (root / "wealthdb.cfg").write_text(json.dumps(cfg))
        engine(root, "init")
        engine(root, "load", "-a", quiet=True)
        for args in (("yield", "global"), ("yield", "sources"), ("yield", "portfolios"), ("yield", "accounts"),
                     ("yield", "positions"), ("yield", "securities"), ("yield", "funds"), ("yield", "cash"),
                     ("yield", "curve"), ("yield", "coverage"), ("income", "projection"), ("income", "calendar"),
                     ("income", "positions"), ("cashflow", "projection")):
            rows = list(csv.reader(io.StringIO(engine(root, *args, "-f", "csv", read_only=True, quiet=True))))
            self.assertGreater(len(rows), 1, args)
            self.assertFalse({c.lower() for r in rows for c in r} & {"null", "<nil>", "nan", "inf", "-inf"}, args)
        # With no withholding table the forward views withhold nothing.
        for r in report(root, "income", "projection", "-C", "+withheld"):
            self.assertEqual(r["withheld_USD"], "", r["period"])
        reasons = {r["reason"] for r in report(root, "yield", "coverage") if r["section"] == "record_day"}
        self.assertNotIn("stated", reasons)
        self.assertNotIn("learned", reasons)


class TestReusedGold(unittest.TestCase):
    """A saved gold is reused only when its stamp still matches."""

    def scratch_dir(self):
        """An empty directory, removed when the test ends."""
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        return pathlib.Path(tmp.name)

    def test_a_matching_stamp_is_reused(self):
        d = self.scratch_dir()
        stamp = gold_stamp(dt.date(2023, 9, 30), "sha256:abc", "hash", SEED)
        (d / STAMP).write_text(stamp)
        (d / LOAD_OUT).write_text("load: demo: 1\n")
        (d / "wealthdb.db").write_bytes(b"")
        self.assertTrue(reused_gold(d, stamp))
        (d / "wealthdb.db.wal").write_bytes(b"")
        self.assertFalse(reused_gold(d, stamp))
        (d / "wealthdb.db.wal").unlink()
        self.assertFalse(reused_gold(d, stamp + "x"))

    def test_the_inputs_hash_names_spec_catalogue_and_generator(self):
        self.assertEqual(len(inputs_hash().splitlines()), 3)

    def test_the_image_tag_is_the_wrappers(self):
        d = self.scratch_dir()
        env = {k: v for k, v in os.environ.items() if k != "WEALTHDB_IMAGE_TAG"}
        with unittest.mock.patch.dict(os.environ, env, clear=True):
            self.assertEqual(image_tag(d), "latest")
            (d / ".image-tag").write_text("  feature-x  \n")
            self.assertEqual(image_tag(d), "feature-x")
            (d / ".image-tag").write_text("not a tag\n")
            self.assertEqual(image_tag(d), "")
        with unittest.mock.patch.dict(os.environ, {"WEALTHDB_IMAGE_TAG": "pinned"}):
            self.assertEqual(image_tag(d), "pinned")

    def test_build_and_load_reuses_only_a_matching_stamp(self):
        parent = self.scratch_dir() / "roots"
        calls = []
        saved = (PARENT, generate.build, engine, engine_image_id, inputs_hash)

        def fake_build(root, *_args, **_kwargs):
            calls.append("build")
            root.mkdir(parents=True, exist_ok=True)
            (root / "wealthdb.db").write_bytes(b"")

        def fake_engine(*_args, **_kwargs):
            calls.append("engine")
            return "load: demo: 1\n"

        try:
            globals()["PARENT"] = str(parent)
            generate.build = fake_build
            globals()["engine"] = fake_engine
            globals()["engine_image_id"] = lambda: "sha256:abc"
            globals()["inputs_hash"] = lambda: "spec\ncatalogue\ncode"
            day = dt.date(2023, 9, 30)
            build_and_load("short", day)
            self.assertEqual(build_and_load("short", day)[1], "load: demo: 1\n")
            self.assertEqual(calls.count("build"), 1)
            globals()["inputs_hash"] = lambda: "spec\nother\ncode"
            build_and_load("short", day)
            self.assertEqual(calls.count("build"), 2)
            globals()["engine_image_id"] = lambda: ""
            build_and_load("short", day)
            self.assertEqual(calls.count("build"), 3)
            globals()["engine_image_id"] = lambda: "sha256:abc"
            globals()["inputs_hash"] = lambda: "spec\ncatalogue\ncode"
            build_and_load("fresh", day)
            build_and_load("fresh", day)
            self.assertEqual(calls.count("build"), 4)
            build_and_load("rolled", day)
            build_and_load("rolled", day)
            self.assertEqual(calls.count("build"), 6)
        finally:
            globals()["PARENT"] = saved[0]
            generate.build = saved[1]
            globals()["engine"] = saved[2]
            globals()["engine_image_id"] = saved[3]
            globals()["inputs_hash"] = saved[4]


if __name__ == "__main__":
    unittest.main()
