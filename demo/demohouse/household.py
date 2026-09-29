"""The household, simulated one day at a time.

Each day the market moves, then a fixed sequence of event generators
books that day's transactions, then every source whose cadence covers
the day writes its closing snapshot. The order of the generators is
fixed, so a transaction's sequence number within its account and day,
and with it the transaction's id, is the same in every run.

With findings off, nothing here depends on the as-of date except where
the run stops, so the first N days of a longer run are identical to a
run that ends after N days. That is what lets an append run extend a
silver file. A findings build stops the venture marks a fixed number of
days before the as-of, so it is a one-off picture that cannot be
extended.
"""

import dataclasses
import datetime as dt
from decimal import ROUND_CEILING, ROUND_FLOOR, Decimal

from . import dates, keyed
from .book import Account, Book, market_value, unit_price
from .market import Market
from .money import CENT, ZERO, D, cents, q4, q8, text
from .spec import name_on

ONE = Decimal(1)
DAYS_PER_WEEK = Decimal(7)
# The year the spec's pay and bill amounts are quoted in; other years grow
# from it by the stated raise or growth rate.
BASE_YEAR = 2024


@dataclasses.dataclass
class LedgerNotes:
    """What the simulation hands the config renderer besides silver:
    rows of the three ledgers gold reads from CSV."""
    equity_transfers: list = dataclasses.field(default_factory=list)
    spending_pins: list = dataclasses.field(default_factory=list)
    transfer_overrides: list = dataclasses.field(default_factory=list)


class Simulation:
    def __init__(self, inputs, seed, as_of, findings=False):
        self.inputs = inputs
        self.spec = inputs.spec
        self.seed = seed
        self.as_of = as_of
        self.findings = findings
        self.start = inputs.history_start
        self.opening = self.start - dt.timedelta(days=1)
        self.fx_start = self.opening - dt.timedelta(days=self.spec["calendar"]["fx_lead_days"])
        if as_of < self.start:
            raise ValueError(f"as-of {as_of} is before history starts ({self.start})")
        self.market = Market(seed, list(inputs.instruments.values()), self.fx_start,
                             self.spec["fx"]["start_usd_per"])
        for s in inputs.splits:
            self.market.add_split(s["instrument"], dates.parse(s["date"]), s["ratio"])
        self.book = Book(self.market, inputs.instruments)
        self.notes = LedgerNotes()
        self.fx_rows = []
        self._card_spend_quarter = ZERO
        self._card_statement = D(self.spec["card"]["first_statement"])
        self._loan_balance = ZERO
        self._loan_posting = None
        self._mortgage_payment = ZERO
        self._fund = {"called": ZERO, "distributed": ZERO, "nav": ZERO}
        self._refunds = {}
        self.book.quiet = self._quiet
        self._build_accounts()

    # ---- the run -------------------------------------------------------

    def run(self):
        for day in dates.days(self.fx_start, self.as_of):
            self.market.advance(day)
            self._fx_row(day)
            if day < self.opening:
                continue
            if day == self.opening:
                self._open()
            else:
                self._day(day)
            self.book.snapshot(day)
        return self

    def _day(self, day):
        self._post_loan(day)
        self._mortgage(day)
        self._payroll(day)
        self._plan_contributions(day)
        self._bills(day)
        self._budget(day)
        self._refunds_due(day)
        self._travel(day)
        self._one_offs(day)
        self._card(day)
        self._income_misc(day)
        self._brokerage(day)
        self._securities_income(day)
        self._corporate_actions(day)
        self._roth(day)
        self._plan_529(day)
        self._hsa_reimbursements(day)
        self._mandate(day)
        self._private(day)
        self._crypto(day)
        self._multicurrency(day)
        self._home(day)
        self._declared_transfer(day)
        self._findings(day)
        self._savings_interest(day)
        self._sweep(day)

    # ---- structure -----------------------------------------------------

    def _build_accounts(self):
        self._sources = {s["id"]: s for s in self.spec["sources"]}
        self._tags = {a["id"]: a["tag"] for s in self.spec["sources"] for a in s["accounts"] if "tag" in a}
        for src in self.spec["sources"]:
            self.book.add_source(src["id"], src["cadence"])
            pf = src.get("portfolio")
            if pf:
                self.book.add_portfolio(src["id"], pf["id"], pf["name"], pf["currency"])
            for a in src["accounts"]:
                opens = dates.parse(a["opens"]) if a.get("opens") else self.opening
                acct = Account(
                    id=a["id"], source=src["id"], kind=a["kind"],
                    display_name=f'{src["institution"]} {a["name"]}',
                    nickname=a.get("nickname"), tax_wrapper=a["wrapper"], style=a["style"],
                    currency=a["currency"], portfolio=a.get("portfolio"), opens=opens)
                self.book.add_account(acct)

    def _open(self):
        """The opening snapshot: balances and holdings the household
        already had on the day before history starts."""
        day = self.opening
        for src in self.spec["sources"]:
            for a in src["accounts"]:
                acct = self.book.account(a["id"])
                if acct.opens != day:
                    continue
                # An account holds cash only in the currencies it opens with
                # or later transacts in; a home or a loan holds none.
                cash = a.get("cash", {})
                for ccy, amount in (cash.items() if isinstance(cash, dict) else [(acct.currency, cash)]):
                    acct.cash[ccy] = D(amount)
                for iid, qty in a.get("holdings", {}).items():
                    value = market_value(self.inputs.instruments[iid], D(qty), self.market.price(iid))
                    # Opening lots carry a cost below today's value, as
                    # holdings bought over earlier years would.
                    r = keyed.rng(self.seed, "opening-basis", a["id"], iid)
                    basis = q4(value * (Decimal("0.72") + Decimal("0.2") * keyed.uniform(r)))
                    acquired = self.opening - dt.timedelta(days=400 + r.randrange(1500))
                    h = self.book.add_units(a["id"], iid, D(qty), basis, day)
                    h.acquired = acquired
        self._open_mortgage(day)
        self._open_home(day)

    def _fx_row(self, day):
        at = dates.epoch(day)
        for ccy in sorted(self.market.fx_rates):
            self.fx_rows.append({"snapshot_at": at, "base_currency": "USD", "quote_currency": ccy,
                                 "mid_rate": text(self.market.usd_per(ccy), 10)})

    # ---- helpers -------------------------------------------------------

    def _rng(self, stream, *keys):
        return keyed.rng(self.seed, stream, *keys)

    def _txn(self, aid, day, kind, amount, **kw):
        return self.book.txn(aid, day, kind, amount, **kw)

    def _open_on(self, aid, day):
        return self.book.account(aid).is_open(day)

    def _ref(self, aid, day):
        """A movement reference both legs of an own-account move inside
        one institution carry, named after the debit leg."""
        acct = self.book.account(aid)
        return f"REF{day:%Y%m%d}{acct.id.upper().replace('-', '')}{acct.seq.get(day, 0) + 1:03d}"

    def _move(self, day, debit, credit, amount, debit_desc, credit_desc, *, same_source=False):
        """Both legs of a movement between two of the household's accounts:
        a withdrawal on one and a deposit on the other, same day, same
        amount. A move inside one institution carries a shared reference
        on both legs."""
        payload = {"bank_ref": self._ref(debit, day)} if same_source else {}
        self._txn(debit, day, "withdrawal", -amount, desc=debit_desc, payload=payload)
        self._txn(credit, day, "deposit", amount, desc=credit_desc, payload=payload)

    def _short(self, aid):
        """The institution's short name, as statement narratives print it."""
        return self._sources[self.book.account(aid).source]["short"]

    def _institution(self, aid):
        return self._sources[self.book.account(aid).source]["institution"]

    def _label(self, aid):
        """How another institution's statement names this account: the
        institution's short name and the account's tag. The config's
        matcher `names` are built from the same labels."""
        return f"{self._short(aid)} {self._tags[aid]}"

    def _ach(self, day, debit, credit, amount):
        """An own-account move by ACH between two institutions, each leg
        naming the other account."""
        self._move(day, debit, credit, amount, f"ACH TRANSFER {self._label(credit)}",
                   f"ACH DEPOSIT {self._label(debit)}")

    def _wire(self, day, debit, credit, amount):
        self._move(day, debit, credit, amount, f"WIRE OUT TO {self._label(credit)}",
                   f"INCOMING WIRE {self._label(debit)}")

    def _jitter_cents(self, stream, *keys):
        """A few cents added to an amount that recurs, so two unrelated
        moves of a round sum never look alike to the amount matcher."""
        return D(self._rng(stream, *keys).randrange(1, 100)) / 100

    def _buy(self, aid, day, iid, amount, *, desc=None):
        """Buy `amount` worth (instrument currency) of iid, rounded down
        to the instrument's trading step."""
        price = self.market.price(iid)
        inst = self.inputs.instruments[iid]
        per = unit_price(inst, price)
        qty = _floor_to(D(amount) / per, _unit_step(inst))
        if qty <= 0:
            return None
        cost = cents(qty * per)
        self.book.add_units(aid, iid, qty, cost, day)
        name = name_on(inst, day)
        return self._txn(aid, day, "buy", -cost, ccy=inst["currency"], instrument=iid,
                         qty=qty, price=price, desc=desc or f"BOUGHT {qty.normalize():f} {name.upper()}")

    def _sell(self, aid, day, iid, qty, *, desc=None):
        inst = self.inputs.instruments[iid]
        price = self.market.price(iid)
        per = unit_price(inst, price)
        held = self.book.qty(aid, iid)
        qty = held if D(qty) >= held else _floor_to(D(qty), _unit_step(inst))
        if qty <= 0:
            return None
        proceeds = cents(qty * per)
        self.book.remove_units(aid, iid, qty)
        name = name_on(inst, day)
        return self._txn(aid, day, "sell", proceeds, ccy=inst["currency"], instrument=iid,
                         qty=-qty, price=price, desc=desc or f"SOLD {qty.normalize():f} {name.upper()}")

    def _value_usd(self, aid):
        """An account's value in USD at today's prices and rates."""
        acct = self.book.account(aid)
        total = ZERO
        for ccy, amt in acct.cash.items():
            total += self.market.to_usd(amt, ccy)
        for h in acct.holdings.values():
            total += self.market.to_usd(self.book.holding_value(h), self.inputs.instruments[h.instrument]["currency"])
        return total

    # ---- mortgage ------------------------------------------------------

    def _open_mortgage(self, day):
        m = self.spec["mortgage"]
        principal, rate, n = D(m["principal"]), D(m["rate"]) / 12, m["term_months"]
        self._mortgage_rate = rate
        self._mortgage_payment = cents(principal * rate / (ONE - (ONE + rate) ** -n))
        balance = principal
        d = dates.parse(m["first_payment"])
        while d <= day:
            interest = cents(balance * rate)
            balance -= self._mortgage_payment - interest
            d = dates.add_months(d, 1)
        self._loan_balance = balance
        self.book.set_mark(m["loan"], "loan", -balance, day, book=-principal,
                           acquired=dates.add_months(dates.parse(m["first_payment"]), -1))

    def _mortgage(self, day):
        m = self.spec["mortgage"]
        if day != dates.next_business(dt.date(day.year, day.month, m["pay_day"])):
            return
        interest = cents(self._loan_balance * self._mortgage_rate)
        principal = self._mortgage_payment - interest
        self._txn(m["funding"], day, "withdrawal", -self._mortgage_payment,
                  desc=f"{self._short(m['loan'])} MORTGAGE PAYMENT", counterparty=self._institution(m["loan"]),
                  payload={"counter_account": m["loan"]})
        # The servicer posts the principal the next day, so the loan's
        # balance on a payment day is still the balance the payment paid.
        self._loan_posting = (day + dt.timedelta(days=1), principal)

    def _post_loan(self, day):
        if self._loan_posting and self._loan_posting[0] == day:
            self._loan_balance -= self._loan_posting[1]
            self._loan_posting = None
            self.book.set_mark(self.spec["mortgage"]["loan"], "loan", -self._loan_balance, day)

    # ---- income --------------------------------------------------------

    def _net_pay(self, amount, raise_, year):
        return cents(D(amount) * (ONE + D(raise_)) ** (year - BASE_YEAR))

    def _payroll(self, day):
        for p in self.spec["payroll"]:
            payer = self.inputs.payers[p["payer"]]
            if p["schedule"] == "semi_monthly":
                pays = [dates.prev_business(dt.date(day.year, day.month, 15)),
                        dates.last_business(day.year, day.month)]
            else:
                pays = [dates.last_business(day.year, day.month)]
            if day in pays:
                self._txn(p["account"], day, "deposit", self._net_pay(p["net_2024"], p["raise"], day.year),
                          desc=payer["descriptor"], counterparty=payer["name"],
                          provider=payer["category"] if p["provider"] else None)
            bonus = p.get("bonus")
            if bonus and day == dates.next_business(dt.date(day.year, bonus["month"], bonus["day"])):
                self._txn(p["account"], day, "deposit", self._net_pay(bonus["net_2024"], p["raise"], day.year),
                          desc=payer["descriptor"] + " BONUS", counterparty=payer["name"],
                          provider=payer["category"] if p["provider"] else None)

    def _plan_contributions(self, day):
        if day != dates.last_business(day.year, day.month):
            return
        for c in self.spec["plan_contributions"]:
            payer = self.inputs.payers[c["payer"]]
            aid = c["account"]
            if c.get("hsa"):
                parts = [(payer["hsa_descriptor"], c["employee"])]
            else:
                parts = [(payer["plan_descriptor"], c["employee"]), (payer["match_descriptor"], c["match"])]
            for desc, amount in parts:
                if D(amount) > 0:
                    self._txn(aid, day, "deposit", D(amount), desc=desc, counterparty=payer["name"],
                              provider="INCOME_WAGES")
            self._buy(aid, day, c["instrument"], self.book.account(aid).balance())

    def _income_misc(self, day):
        feed = self.spec["feed_in"]
        if day == dates.next_business(dt.date(day.year, day.month, feed["day"])):
            payer = self.inputs.payers[feed["payer"]]
            self._txn(feed["account"], day, "deposit", D(feed["monthly"][day.month - 1]),
                      desc=payer["descriptor"], counterparty=payer["name"], provider=payer["category"])
        ref = self.spec["tax_refund"]
        if day.month == ref["month"] and day == dates.next_business(dt.date(day.year, ref["month"], ref["day"])):
            payer = self.inputs.payers[ref["payer"]]
            # A refund differs by a few dollars from year to year.
            amount = D(ref["amount"]) + self._jitter_cents("refund", day.year) * 100
            self._txn(ref["account"], day, "deposit", amount, desc=payer["descriptor"],
                      counterparty=payer["name"], provider=payer["category"])
        gift = self.spec["family_gift"]
        if day == dates.parse(gift["date"]):
            payer = self.inputs.payers[gift["payer"]]
            self._txn(gift["account"], day, "deposit", D(gift["amount"]), desc=payer["descriptor"],
                      provider=payer["category"])

    def _savings_interest(self, day):
        si = self.spec["savings_interest"]
        if day != dates.month_end(day.year, day.month):
            return
        acct = self.book.account(si["account"])
        apy = D(si["apy_by_year"][str(day.year)] if str(day.year) in si["apy_by_year"]
                else list(si["apy_by_year"].values())[-1])
        interest = cents(acct.balance() * apy / 12)
        if interest > 0:
            self._txn(acct.id, day, "interest", interest, desc="INTEREST PAID")

    # ---- spending ------------------------------------------------------

    def _bills(self, day):
        for b in self.spec["bills"]:
            if "months" in b and day.month not in b["months"]:
                continue
            aid = b["account"]
            due = dt.date(day.year, day.month, b["day"])
            # A card bill posts on its day; a debit from the bank waits for a business day.
            if self.book.account(aid).kind != "card":
                due = dates.next_business(due)
            if day != due:
                continue
            m = self.inputs.merchants[b["merchant"]]
            amount = D(b["amount"])
            if "growth" in b:
                amount = cents(amount * (ONE + D(b["growth"])) ** (day.year - BASE_YEAR))
            if "seasonal" in b:
                amount = cents(amount * D(b["seasonal"][day.month - 1]))
            if "jitter" in b:
                amount = cents(amount * keyed.lognormal_factor(self._rng("bill", b["merchant"], day.isoformat()), b["jitter"]))
            self._spend(aid, day, m, amount)

    def _spend(self, aid, day, m, amount, *, ccy=None, store_rng=None):
        """One purchase from merchant m: a `purchase` on a card, a
        `withdrawal` on a bank account. The provider files it under the
        merchant's category unless the catalogue says it files nothing."""
        kind = "purchase" if self.book.account(aid).kind == "card" else "withdrawal"
        desc = m["descriptor"]
        if "{n}" in desc:
            r = store_rng or self._rng("store", m["id"], day.isoformat())
            desc = desc.replace("{n}", str(keyed.pick(r, m["stores"])))
        filed = m["category"] if m.get("provider", True) else None
        if kind == "purchase":
            self._card_spend_quarter += amount
        return self._txn(aid, day, kind, -amount, ccy=ccy, desc=desc, counterparty=m["name"], provider=filed)

    def _away(self, day):
        """True on the days the household is abroad (no local spending)."""
        for t in self.spec["travel"]:
            if "abroad" not in t:
                continue
            start = dt.date(day.year, t["start"]["month"], t["start"]["day"])
            if start <= day <= start + dt.timedelta(days=t["nights"]):
                return True
        return False

    def _budget(self, day):
        if self._away(day):
            return
        for b in self.spec["budget"]:
            aid = b["account"]
            if self.book.account(aid).kind != "card" and not dates.is_business(day):
                continue
            rate = D(b["per_week"]) / DAYS_PER_WEEK
            if self.book.account(aid).kind != "card":
                rate = rate * 7 / 5  # the same weekly count, on business days only
            season = D(b["seasonal"][day.month - 1]) if "seasonal" in b else ONE
            r = self._rng("budget", b["bucket"], day.isoformat())
            n = _poisson(r, rate * season)
            if not n:
                continue
            merchants = [m for m in self.inputs.merchants.values() if m.get("bucket") == b["bucket"]]
            mean = D(b["annual"]) / (D(b["per_week"]) * Decimal("52.18"))
            for _ in range(n):
                if b["bucket"] == "atm":
                    self._atm(day, r, mean)
                    continue
                m = keyed.pick(r, merchants, [x["weight"] for x in merchants])
                amount = cents(mean * keyed.lognormal_factor(r, m.get("spread", "0.4")))
                if amount < 1:
                    amount = Decimal("1.00")
                # Drawn in both builds, so a findings build plants purchases
                # on the days a clean build buys. A planted purchase replaces
                # an ordinary one, so that day's later draws, the card's
                # rewards and what follows from them can differ.
                unmapped = keyed.chance(r, self.spec["findings"]["unmapped_card_share"])
                if self.findings and unmapped:
                    desc = keyed.pick(r, self.spec["findings"]["unmapped_merchants"])
                    self._txn(aid, day, "purchase", -amount, desc=desc, counterparty=desc)
                    continue
                row = self._spend(aid, day, m, amount, store_rng=r)
                ref = self.spec["refunds"]
                if b["bucket"] in ref["buckets"] and keyed.chance(r, ref["chance"]):
                    # The merchant refunds the purchase some days later.
                    when = day + dt.timedelta(days=3 + r.randrange(12))
                    self._refunds.setdefault(when, []).append((aid, m, row["description"], amount))

    def _refunds_due(self, day):
        for aid, m, desc, amount in self._refunds.pop(day, []):
            self._txn(aid, day, "refund", amount, desc=f"REFUND {desc}", counterparty=m["name"],
                      provider=m["category"] if m.get("provider", True) else None)

    def _atm(self, day, r, mean):
        a = self.spec["atm"]
        amount = max(Decimal(40), (mean * keyed.lognormal_factor(r, "0.35") / 20).quantize(ONE) * 20)
        self._txn(a["account"], day, "withdrawal", -amount, desc=f'ATM WITHDRAWAL {a["branch"]}',
                  provider="cash_withdrawal")
        if keyed.chance(r, a["fee_chance"]):
            # Named without the machine token: the engine's built-in cash
            # rule reads that word on any row and outranks the provider.
            self._txn(a["account"], day, "fee", -D(a["fee"]), desc=f'NON-NETWORK SURCHARGE {a["fee_network"]}',
                      provider="BANK_FEES_ATM_FEES")

    def _travel(self, day):
        for t in self.spec["travel"]:
            start = dt.date(day.year, t["start"]["month"], t["start"]["day"])
            end = start + dt.timedelta(days=t["nights"])
            for bk in t.get("bookings", []):
                if bk.get("pay") == "checkout":
                    due = day == end
                else:
                    # Paid ahead: for this year's trip, or next year's when
                    # the lead time crosses New Year.
                    lead = dt.timedelta(days=bk["days_before"])
                    due = any(dt.date(y, t["start"]["month"], t["start"]["day"]) - lead == day
                              for y in (day.year, day.year + 1))
                if due:
                    m = self.inputs.merchants[bk["merchant"]]
                    jitter = keyed.lognormal_factor(self._rng("booking", bk["merchant"], day.isoformat()), "0.08")
                    self._spend(self.spec["card"]["account"], day, m, cents(D(bk["amount"]) * jitter))
            if not (start <= day <= end):
                continue
            daily = t.get("daily")
            if daily:
                r = self._rng("trip", t["name"], day.isoformat())
                bucket = [m for m in self.inputs.merchants.values() if m.get("bucket") == daily["bucket"]]
                for _ in range(_poisson(r, D(daily["per_day"]))):
                    m = keyed.pick(r, bucket, [x["weight"] for x in bucket])
                    self._spend(self.spec["card"]["account"], day, m,
                                cents(D(daily["amount"]) * keyed.lognormal_factor(r, "0.35")), store_rng=r)
            for item in t.get("abroad", []):
                self._abroad(day, t, start, item)

    def _abroad(self, day, trip, start, item):
        w = self.spec["multicurrency"]
        lo, hi = trip["legs"][item["leg"]]
        leg_start, leg_end = start + dt.timedelta(days=lo), start + dt.timedelta(days=hi)
        if not (leg_start <= day <= leg_end):
            return
        if "merchant" in item:
            if day != leg_end:
                return
            m = self.inputs.merchants[item["merchant"]]
            amount = cents(D(item["amount"]) * keyed.lognormal_factor(self._rng("abroad", item["merchant"], day.isoformat()), "0.1"))
            self._spend(w["account"], day, m, amount, ccy=item["ccy"])
            part = item.get("card_part")
            if part:
                # The rest of the same bill goes on the card, in dollars.
                self._spend(self.spec["card"]["account"], day, self.inputs.merchants[part["merchant"]],
                            cents(D(part["amount"]) * keyed.lognormal_factor(self._rng("abroad-card", day.isoformat()), "0.1")))
            return
        r = self._rng("abroad", item["bucket"], day.isoformat())
        bucket = [m for m in self.inputs.merchants.values() if m.get("bucket") == item["bucket"]]
        for _ in range(_poisson(r, D(item["per_day"]))):
            m = keyed.pick(r, bucket, [x["weight"] for x in bucket])
            amount = cents(D(m["mean"]) * keyed.lognormal_factor(r, m.get("spread", "0.4")))
            if self.book.account(w["account"]).balance(item["ccy"]) - amount < 0:
                continue  # a card declined for want of funds books nothing
            self._spend(w["account"], day, m, amount, ccy=item["ccy"])

    def _one_offs(self, day):
        for o in self.spec["one_offs"]:
            if day != dates.parse(o["date"]):
                continue
            if "merchant" in o:
                self._spend(o["account"], day, self.inputs.merchants[o["merchant"]], D(o["amount"]))
                continue
            self._txn(o["account"], day, "withdrawal", -D(o["amount"]), desc=o["descriptor"],
                      check=o.get("check"))
            if "pin" in o:
                self.notes.spending_pins.append({
                    "silver_source_id": self.book.account(o["account"]).source, "account": o["account"],
                    "occurred_at": day.isoformat(), "amount": text(-D(o["amount"]), 2), "currency": "USD",
                    "spend_detailed": o["pin"], "note": o["note"]})

    # ---- the card ------------------------------------------------------

    def _card(self, day):
        c = self.spec["card"]
        card = self.book.account(c["account"])
        if day == dates.next_business(dt.date(day.year, day.month, c["pay_day"])) and self._card_statement > 0:
            amount = self._card_statement
            ref = self._ref(c["funding"], day)
            self._txn(c["funding"], day, "withdrawal", -amount, desc=f"{self._short(card.id)} AUTOPAY PAYMENT TO REWARDS CARD",
                      payload={"bank_ref": ref})
            self._txn(card.id, day, "card_payment", amount, desc="AUTOPAY CREDIT POSTED, THANK YOU",
                      payload={"bank_ref": ref})
            self._card_statement = ZERO
        if day.month in c["reward_months"] and day.day == 21 and self._card_spend_quarter > 0:
            reward = cents(self._card_spend_quarter * D(c["reward_rate"]))
            self._txn(card.id, day, "reward", reward, desc="CASHBACK REWARD CREDIT")
            self._card_spend_quarter = ZERO
        fee = c["annual_fee"]
        if day.month == fee["month"] and day.day == fee["day"]:
            self._txn(card.id, day, "fee", -D(fee["amount"]), desc="YEARLY MEMBERSHIP CHARGE",
                      provider="BANK_FEES_OTHER_BANK_FEES")
        if day.day == c["close_day"]:
            self._card_statement = -card.balance()

    # ---- investing -----------------------------------------------------

    def _brokerage(self, day):
        b = self.spec["investing"]["brokerage"]
        aid = b["account"]
        if day == dates.nth_business(day.year, day.month, b["business_day"]):
            amount = D(b["amount"]) + self._jitter_cents("dca", day.year, day.month)
            self._ach(day, b["from"], aid, amount)
            for iid, share in sorted(b["split"].items()):
                self._buy(aid, day, iid, cents(amount * D(share)))
            if day.month in b["stock_every_months"]:
                k = (day.year * 12 + day.month) // 3
                stock = b["stock_rotation"][k % len(b["stock_rotation"])]
                if self.book.account(aid).balance() >= D(b["stock_amount"]):
                    self._buy(aid, day, stock, D(b["stock_amount"]))
        rb = b["rebalance"]
        if day.month == rb["month"] and day == dates.nth_weekday(day.year, rb["month"], rb["weekday"], rb["week"]):
            sold = self._sell(aid, day, rb["sell"], self.book.qty(aid, rb["sell"]) * D(rb["fraction"]))
            if sold:
                self._buy(aid, day, rb["buy"], D(sold["net_amount"]))
        tl = b["tax_loss"]
        if day.month == tl["month"] and day == dates.next_business(dt.date(day.year, tl["month"], tl["day"])):
            losers = []
            for iid, h in sorted(self.book.account(aid).holdings.items()):
                if self.inputs.instruments[iid]["vehicle"] != "stock" or h.qty is None:
                    continue
                loss = self.book.holding_value(h) - h.book
                if loss < 0:
                    losers.append((loss, iid))
            if losers:
                _, iid = min(losers)
                self._sell(aid, day, iid, self.book.qty(aid, iid) * D(tl["fraction"]))
            else:
                self._sell(aid, day, tl["fallback"], self.book.qty(aid, tl["fallback"]) * D(tl["fallback_fraction"]))

    def _securities_income(self, day):
        """Dividends, coupons and fund capital-gain payouts, on every
        account that holds the paying instrument."""
        reinvest = set(self.spec["investing"]["reinvest"])
        for aid in sorted(self.book.accounts):
            acct = self.book.account(aid)
            if not acct.is_open(day):
                continue
            for iid in sorted(acct.holdings):
                h = acct.holdings[iid]
                inst = self.inputs.instruments[iid]
                if h.qty is None or h.qty <= 0:
                    continue
                name = name_on(inst, day).upper()
                inc = inst.get("income")
                pay_day = inc and day.month in inc["months"] and \
                    day == dates.next_business(dt.date(day.year, day.month, 20))
                per_share = q4(self.market.price(iid) * D(inc["yield"]) / len(inc["months"])) if pay_day else ZERO
                amount = cents(h.qty * per_share)
                if amount > 0:
                    self._txn(aid, day, "dividend", amount, ccy=inst["currency"], instrument=iid,
                              desc=f"DIVIDEND {name}")
                    net = amount
                    if "withholding" in inc:
                        tax = cents(amount * D(inc["withholding"]))
                        self._txn(aid, day, "tax", -tax, ccy=inst["currency"], instrument=iid,
                                  desc=f"FOREIGN TAX WITHHELD {name}")
                        net -= tax
                    if aid in reinvest:
                        self._buy(aid, day, iid, net, desc=f"REINVEST {name}")
                cg = inst.get("capital_gain")
                if cg and day.month == cg["month"] and day == dates.next_business(dt.date(day.year, cg["month"], 18)):
                    amount = cents(h.qty * D(cg["per_share"]))
                    self._txn(aid, day, "capital_gain", amount, ccy=inst["currency"], instrument=iid,
                              desc=f"CAP GAIN DISTRIBUTION {name}")
                    if aid in reinvest:
                        self._buy(aid, day, iid, amount, desc=f"REINVEST {name}")
                cp = inst.get("coupon")
                if cp and day.month in cp["months"] and day == dates.next_business(
                        dates.clamp_day(day.year, day.month, cp["day"])):
                    amount = cents(h.qty * D(cp["rate"]) / len(cp["months"]))
                    self._txn(aid, day, "coupon", amount, ccy=inst["currency"], instrument=iid,
                              desc=f"COUPON {name}")

    def _corporate_actions(self, day):
        for s in self.inputs.splits:
            if day != dates.parse(s["date"]):
                continue
            iid = s["instrument"]
            for aid in sorted(self.book.accounts):
                h = self.book.holding(aid, iid)
                if not h or h.qty is None or not self._open_on(aid, day):
                    continue
                added = q8(h.qty * (D(s["ratio"]) - 1))
                h.qty = q8(h.qty + added)
                self._txn(aid, day, "corporate_action", ZERO, instrument=iid, qty=added, cash=False,
                          desc=f'STOCK SPLIT {s["ratio"]}-FOR-1 {name_on(self.inputs.instruments[iid], day).upper()}')

    def _roth(self, day):
        for r in self.spec["investing"]["roth"]:
            if day.year < r["first_year"] or day.month != r["month"] or \
                    day != dates.next_business(dt.date(day.year, r["month"], r["day"])):
                continue
            self._ach(day, r["from"], r["account"], D(r["amount"]))
            for iid, share in sorted(r["split"].items()):
                self._buy(r["account"], day, iid, cents(D(r["amount"]) * D(share)))

    def _plan_529(self, day):
        p = self.spec["investing"]["plan_529"]
        if day != dates.next_business(dt.date(day.year, day.month, p["day"])):
            return
        amount = D(p["amount"]) + self._jitter_cents("529", day.year, day.month)
        self._ach(day, p["from"], p["account"], amount)
        self._buy(p["account"], day, p["instrument"], amount)

    def _hsa_reimbursements(self, day):
        h = self.spec["hsa_reimbursements"]
        if day.month not in h["months"] or day != dates.next_business(dt.date(day.year, day.month, h["day"])):
            return
        k = (day.year * 12 + day.month) % len(h["amounts"])
        amount = D(h["amounts"][k])
        acct = self.book.account(h["account"])
        short = amount - acct.balance()
        if short > 0:
            price = self.market.price(h["instrument"])
            self._sell(acct.id, day, h["instrument"], (short / price).quantize(Decimal("0.0001")) + Decimal("0.0001"))
        self._move(day, acct.id, h["to"], amount, f"HSA DISTRIBUTION TO {self._label(h['to'])}",
                   f"HSA REIMBURSEMENT {self._label(acct.id)}")

    # ---- the mandate ---------------------------------------------------

    def _mandate(self, day):
        m = self.spec["mandate"]
        aid = m["account"]
        if not dates.is_business(day):
            return
        if day.month in m["fee_months"] and day == dates.nth_business(day.year, day.month, 1):
            fee = cents(self._value_usd(aid) * D(m["fee_rate"]) / 4)
            self._raise_cash(aid, day, "USD", fee)
            quarter = (day.month - 2) // 3 + 1 if day.month > 1 else 4
            year = day.year if day.month > 1 else day.year - 1
            self._txn(aid, day, "fee", -fee, desc=f"MANAGEMENT FEE Q{quarter} {year}")
        if day.month in m["rebalance_months"] and day == dates.next_business(
                dt.date(day.year, day.month, m["rebalance_day"])):
            self._rebalance(day, m)

    def _rebalance(self, day, m):
        aid = m["account"]
        acct = self.book.account(aid)
        total = self._value_usd(aid)
        invest = total * (ONE - D(m["cash_buffer"]))
        wants = {}
        for iid, w in sorted(m["targets"].items()):
            inst = self.inputs.instruments[iid]
            h = acct.holdings.get(iid)
            have = self.market.to_usd(self.book.holding_value(h), inst["currency"]) if h else ZERO
            diff = invest * D(w) - have
            if abs(diff) > invest * D(w) * Decimal("0.03"):
                wants[iid] = diff
        # Sells first, then fund each currency's buys from USD, then buy.
        for iid, diff in sorted(wants.items()):
            if diff < 0:
                inst = self.inputs.instruments[iid]
                per = unit_price(inst, self.market.price(iid))
                self._sell(aid, day, iid, self.market.from_usd(-diff, inst["currency"]) / per)
        needs = {}
        for iid, diff in sorted(wants.items()):
            if diff > 0:
                ccy = self.inputs.instruments[iid]["currency"]
                needs[ccy] = needs.get(ccy, ZERO) + self.market.from_usd(diff, ccy)
        reserve = total * D("0.003")
        for ccy in sorted(set(acct.cash) - {"USD"}):
            spare = acct.balance(ccy) - self.market.from_usd(reserve, ccy)
            short = needs.get(ccy, ZERO) - spare
            if short > 0:
                # Fund the currency's buys from USD, never beyond the USD held.
                usd = min(cents(self.market.to_usd(short, ccy) * Decimal("1.002")), acct.balance("USD"))
                if usd > 0:
                    self._convert(aid, day, "USD", ccy, usd)
            elif short < -self.market.from_usd(Decimal("2000"), ccy):
                self._convert(aid, day, ccy, "USD", cents(-short))
        for iid, diff in sorted(wants.items()):
            if diff > 0:
                ccy = self.inputs.instruments[iid]["currency"]
                spare = acct.balance(ccy) - (reserve if ccy == "USD" else ZERO)
                budget = min(self.market.from_usd(diff, ccy), spare)
                if budget > 0:
                    self._buy(aid, day, iid, budget)

    def _raise_cash(self, aid, day, ccy, need):
        """Sell from the account's largest holding in `ccy` until its cash
        in that currency covers `need`; a mandate never runs an overdraft."""
        acct = self.book.account(aid)
        short = need - acct.balance(ccy)
        if short <= 0:
            return
        held = [(self.book.holding_value(h), i) for i, h in acct.holdings.items()
                if h.qty and self.inputs.instruments[i]["currency"] == ccy]
        _, iid = max(held)
        inst = self.inputs.instruments[iid]
        per = unit_price(inst, self.market.price(iid))
        step = _unit_step(inst)
        qty = ((short * Decimal("1.05")) / per / step).to_integral_value(rounding=ROUND_CEILING) * step
        self._sell(aid, day, iid, qty)

    def _convert(self, aid, day, from_ccy, to_ccy, amount):
        """A currency conversion inside one account: two `fx` legs that
        describe each other and share a reference, at the day's mid rate."""
        got = cents(self.market.from_usd(self.market.to_usd(amount, from_ccy), to_ccy))
        ref = self._ref(aid, day)
        self._txn(aid, day, "fx", -amount, ccy=from_ccy, desc=f"FX CONVERSION {from_ccy}/{to_ccy}",
                  payload={"bank_ref": ref, "counter_currency": to_ccy, "counter_amount": text(got, 2)})
        self._txn(aid, day, "fx", got, ccy=to_ccy, desc=f"FX CONVERSION {from_ccy}/{to_ccy}",
                  payload={"bank_ref": ref, "counter_currency": from_ccy, "counter_amount": text(-amount, 2)})

    # ---- private markets -----------------------------------------------

    def _private(self, day):
        f = self.spec["private"]["fund"]
        fund = self._fund
        for n, call in enumerate(f["calls"], 1):
            if day == dates.parse(call["date"]):
                amount = D(call["amount"])
                self._wire(day, f["from"], f["account"], amount)
                self._txn(f["account"], day, "contribution", -amount, instrument=f["instrument"],
                          desc=f"CAPITAL CALL {n} {self._instrument_name(f['instrument'], day)}")
                fund["called"] += amount
                fund["nav"] += amount  # called capital sits at cost until the next mark
                self._mark_fund(day, f)
        for dist in f["distributions"]:
            if day == dates.parse(dist["date"]):
                amount = D(dist["amount"])
                self._txn(f["account"], day, "distribution", amount, instrument=f["instrument"],
                          desc=f"DISTRIBUTION {self._instrument_name(f['instrument'], day)}")
                self._wire(day, f["account"], f["from"], amount)
                fund["distributed"] += amount
                fund["nav"] -= amount
                self._mark_fund(day, f)
        for mark in f["marks"]:
            if day == dates.parse(mark["date"]):
                fund["nav"] = fund["called"] * D(mark["multiple"]) - fund["distributed"]
                self._mark_fund(day, f)
        s = self.spec["private"]["spv"]
        if day == dates.parse(s["date"]):
            amount = D(s["amount"])
            self._wire(day, s["from"], s["account"], amount)
            self._txn(s["account"], day, "contribution", -amount, instrument=s["instrument"],
                      desc=f"SPV SUBSCRIPTION {self._instrument_name(s['instrument'], day)}")
            self.book.set_mark(s["account"], s["instrument"], amount, day, book=amount)
        if day == dates.parse(s["markup"]["date"]):
            self.book.set_mark(s["account"], s["instrument"], D(s["amount"]) * D(s["markup"]["multiple"]), day)
        ex = s["exit"]
        if day == dates.parse(ex["date"]):
            self._spv_exit(day, s, ex)

    def _instrument_name(self, iid, day):
        return name_on(self.inputs.instruments[iid], day).upper()

    def _mark_fund(self, day, f):
        fund = self._fund
        self.book.set_mark(f["account"], f["instrument"], fund["nav"], day,
                           book=fund["called"] - fund["distributed"])

    def _spv_exit(self, day, s, ex):
        """The SPV pays out in listed shares that land in the brokerage:
        one equity-transfer ledger row out of the venture account, one into
        the brokerage, same day, same value. The SPV holding leaves the
        venture account's snapshot that day."""
        shares = D(ex["shares"])
        value = cents(shares * self.market.price(ex["listed"]))
        spv = self.book.account(s["account"])
        basis = spv.holdings.pop(s["instrument"]).book
        self.book.add_units(ex["to"], ex["listed"], shares, basis, day)
        name = self.inputs.instruments[ex["listed"]]["name"]
        for source, aid, direction, instrument, qty in (
                (spv.source, s["account"], "out", s["instrument"], ""),
                (self.book.account(ex["to"]).source, ex["to"], "in", ex["listed"], text(shares, 8))):
            self.notes.equity_transfers.append({
                "silver_source_id": source, "account": aid, "occurred_at": day.isoformat(),
                "direction": direction, "quantity": qty, "cost_basis": text(basis, 2),
                "value": text(value, 2), "currency": "USD", "instrument": instrument,
                "note": f"SPV distributed {name} shares in kind"})

    # ---- crypto --------------------------------------------------------

    def _crypto(self, day):
        c = self.spec["crypto"]
        aid = c["account"]
        init = c["initial"]
        if day == dates.parse(init["date"]):
            self._ach(day, init["from"], aid, D(init["amount"]))
            self._crypto_buys(day, c, D(init["amount"]), init["split"])
        if not self._open_on(aid, day) or day == dates.parse(init["date"]):
            return
        mo = c["monthly"]
        if day.day == mo["day"]:
            amount = D(mo["amount"]) + self._jitter_cents("crypto", day.year, day.month)
            self._ach(day, mo["from"], aid, amount)
            self._crypto_buys(day, c, amount, mo["split"])
        st = c["staking"]
        if day == dates.month_end(day.year, day.month) and self.book.qty(aid, st["instrument"]) > 0:
            value = self.book.qty(aid, st["instrument"]) * self.market.price(st["instrument"])
            reward = cents(value * D(st["apy"]) / 12)
            if reward > 0:
                self._txn(aid, day, "staking", reward, desc="STAKING REWARD ETH (PAID IN USD)")
        if day == dates.parse(c["sell"]["date"]):
            iid = c["sell"]["instrument"]
            self._sell(aid, day, iid, self.book.qty(aid, iid) * D(c["sell"]["fraction"]))
        w = c["withdrawal"]
        if day == dates.parse(w["date"]):
            qty = D(w["qty"])
            value = cents(qty * self.market.price(w["instrument"]))
            self.book.remove_units(aid, w["instrument"], qty)
            self._txn(aid, day, "transfer_out", -value, instrument=w["instrument"], qty=-qty, cash=False,
                      desc="WITHDRAWAL TO EXTERNAL WALLET")

    def _crypto_buys(self, day, c, amount, split):
        aid = c["account"]
        fee_rate = D(c["fee_rate"])
        for iid, share in sorted(split.items()):
            # A cent of margin keeps cost plus fee, each rounded to the
            # cent, inside the amount deposited.
            gross = (amount * D(share) / (ONE + fee_rate)).quantize(CENT, rounding=ROUND_FLOOR) - CENT
            row = self._buy(aid, day, iid, gross)
            if row:
                self._txn(aid, day, "fee", -cents(-D(row["net_amount"]) * fee_rate), instrument=iid,
                          desc=f'TRADING FEE {self.inputs.instruments[iid]["symbol"]}')

    # ---- the multi-currency account ------------------------------------

    def _multicurrency(self, day):
        w = self.spec["multicurrency"]
        wire = w["wire"]
        if day.month == wire["month"] and day == dates.next_business(dt.date(day.year, wire["month"], wire["day"])):
            usd = D(wire["usd"]) + self._jitter_cents("wire", day.year)
            eur = cents(self.market.from_usd(usd, "EUR"))
            src = self.book.account(w["funding"]).source
            self._txn(w["funding"], day, "withdrawal", -usd, desc=f"WIRE {self._short(w['account'])} EUR ACCOUNT")
            self._txn(w["funding"], day, "fee", -D(wire["fee"]), desc="WIRE FEE OUTGOING INTERNATIONAL")
            self._txn(w["account"], day, "deposit", eur, ccy="EUR", desc=f"INCOMING WIRE {self._label(w['funding'])}")
            self.notes.transfer_overrides.append({
                "verb": "match", "silver_source_id": src, "account": w["funding"],
                "occurred_at": day.isoformat(), "amount": text(-usd, 2), "currency": "USD",
                "silver_source_id_b": self.book.account(w["account"]).source, "account_b": w["account"],
                "occurred_at_b": day.isoformat(), "amount_b": text(eur, 2), "currency_b": "EUR",
                "note": "yearly funding wire for the summer trip, USD to EUR"})
        cv = w["convert"]
        if day.month == cv["month"] and day.day == cv["day"]:
            self._convert(w["account"], day, cv["from"], cv["to"], D(cv["amount"]))

    # ---- home ----------------------------------------------------------

    def _open_home(self, day):
        h = self.spec["homestead"]
        value = ZERO
        for a in h["appraisals"]:
            if dates.parse(a["date"]) <= day:
                value = D(a["value"])
        self.book.set_mark(h["account"], h["instrument"], value, day, book=D(h["price"]),
                           acquired=dates.parse(h["purchased"]))

    def _home(self, day):
        h = self.spec["homestead"]
        for a in h["appraisals"]:
            if day == dates.parse(a["date"]):
                self.book.set_mark(h["account"], h["instrument"], D(a["value"]), day)

    # ---- the declared account, savings sweeps --------------------------

    def _declared_transfer(self, day):
        d = self.spec["declared_transfer"]
        if day == dates.next_business(dt.date(day.year, day.month, d["day"])):
            self._txn(d["from"], day, "withdrawal", -D(d["amount"]), desc=d["descriptor"])

    def _sweep(self, day):
        s = self.spec["sweep"]
        chk = self.book.account(s["checking"])
        floor, target, ceiling = D(s["floor"]), D(s["target"]), D(s["ceiling"])
        sweep_day = day in [dates.next_business(dt.date(day.year, day.month, d)) for d in s["days"]]
        bal = chk.balance()
        if sweep_day and bal > ceiling:
            self._move(day, chk.id, s["savings"], bal - target, "ONLINE TRANSFER TO JOINT SAVINGS",
                       "ONLINE TRANSFER FROM JOINT CHECKING", same_source=True)
        elif (sweep_day and bal < floor) or bal < Decimal("1000"):
            # Below the floor on a sweep day, or near empty on any day:
            # the savings account tops checking back up to the target.
            self._move(day, s["savings"], chk.id, target - bal, "ONLINE TRANSFER TO JOINT CHECKING",
                       "ONLINE TRANSFER FROM JOINT SAVINGS", same_source=True)
        if self.book.account(s["savings"]).balance() < 0:
            raise ValueError(f"savings overdrawn on {day}; the household spends more than it has")

    # ---- findings ------------------------------------------------------

    def _findings(self, day):
        if not self.findings:
            return
        ob = self.spec["findings"]["old_brokerage"]
        if day == dates.parse(ob["date"]):
            self._txn(ob["account"], day, "withdrawal", -D(ob["amount"]), desc=ob["descriptor"],
                      provider="internal_transfer")

    def _quiet(self, aid, day):
        """With findings on, two feeds go quiet: the multi-currency
        account's statement for one month, and the venture account (fund
        and SPV) for the last weeks before as-of. Their snapshots are not
        written."""
        if not self.findings:
            return False
        f = self.spec["findings"]
        ms = f["missing_statement"]
        if aid == ms["account"] and day.strftime("%Y-%m") == ms["month"]:
            return True
        stale_from = self.as_of - dt.timedelta(days=f["stale_mark_days"])
        venture = (self.spec["private"]["fund"]["account"], self.spec["private"]["spv"]["account"])
        return aid in venture and day > stale_from


# ---- module helpers ---------------------------------------------------------


def _unit_step(inst):
    """The smallest quantity the instrument trades in: whole shares of a
    foreign stock, lots of 1,000 face of a bond, satoshi-sized coin, gold
    to the hundredth ounce, and anything else to four decimals."""
    if inst["vehicle"] == "stock" and inst["asset_class"] == "public_equity" and inst["currency"] != "USD":
        return ONE
    if inst.get("coupon"):
        return Decimal(1000)
    if inst["asset_class"] == "crypto":
        return Decimal("0.00000001")
    if inst["id"] == "gold":
        return Decimal("0.01")
    return Decimal("0.0001")


def _floor_to(x, step):
    """x rounded down to a whole number of steps."""
    return (x / step).to_integral_value(rounding=ROUND_FLOOR) * step


def _poisson(r, lam):
    """A Poisson draw by inversion, in Decimal."""
    lam = D(lam)
    if lam <= 0:
        return 0
    u = keyed.uniform(r)
    p = (-lam).exp()
    cdf = p
    k = 0
    while u > cdf and k < 50:
        k += 1
        p = p * lam / k
        cdf += p
    return k

