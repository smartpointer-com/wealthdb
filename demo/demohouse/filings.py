"""The companies' reported figures.

A directly held US company that the catalogue gives `filings` reports
each quarter on a 10-Q, and each fiscal year on a 10-K that covers the
fourth quarter too. A filing states the period's revenue, operating
income, interest expense, net income, diluted earnings per share,
dividends per share, operating cash flow and capital spending, and the
debt and cash on the period's last day, each under the us-gaap concept
the SEC serves it as. A company that pays no dividend states none.

The figures follow from the catalogue's terms at the start of history
(price-earnings ratio, net margin, growth, debt, cash) and the
instrument's price, payout and splits, jittered by keyed randomness.
They never read the simulation. A share figure is per share on the
period's last day: after a split, the shares are the split ones.

A filing lands about forty days after its period ends. Its figures are
dated by the period's last day (as_of) and fetched on the filed day
(run_at). Only the figures filed on or before the as-of are written, so
an append adds the ones filed in its new days, however long before them
their periods ended.
"""

import datetime as dt
from decimal import ROUND_HALF_EVEN, Decimal, localcontext

from . import dates, keyed
from .money import PRECISE, ZERO, D, cents, compound, text

# Flow figures cover a period; balance figures hold on its last day.
FLOWS = {
    "revenue": ("us-gaap:Revenues", "USD"),
    "operating_income": ("us-gaap:OperatingIncomeLoss", "USD"),
    "interest_expense": ("us-gaap:InterestExpense", "USD"),
    "net_income": ("us-gaap:NetIncomeLoss", "USD"),
    "eps_diluted": ("us-gaap:EarningsPerShareDiluted", "USD/shares"),
    "dividends_per_share": ("us-gaap:CommonStockDividendsPerShareDeclared", "USD/shares"),
    "operating_cash_flow": ("us-gaap:NetCashProvidedByUsedInOperatingActivities", "USD"),
    "capex": ("us-gaap:PaymentsToAcquirePropertyPlantAndEquipment", "USD"),
}
BALANCES = {
    "long_term_debt": ("us-gaap:LongTermDebtNoncurrent", "USD"),
    "cash": ("us-gaap:CashAndCashEquivalentsAtCarryingValue", "USD"),
}
PER_SHARE = ("eps_diluted", "dividends_per_share")

TAX = Decimal("0.21")
# Interest a year on the debt; depreciation, which operating cash adds
# back, and capital spending, each as a share of revenue.
DEBT_RATE = Decimal("0.045")
DEPRECIATION = Decimal("0.06")
CAPEX = Decimal("0.05")
# A payer raises its dividend by this each fiscal year.
DIVIDEND_GROWTH = Decimal("0.05")
# A filing lands this many days after its period ends, give or take the
# jitter, on a business day.
FILING_DAYS = 40
FILING_JITTER = 4
JITTER = {"revenue": "0.03", "margin": "0.08", "debt": "0.03", "cash": "0.08", "capex": "0.10"}
_MILLION = Decimal(1000000)


def _millions(x):
    """A money figure as the filings round it: whole millions."""
    v = (D(x) / _MILLION).quantize(Decimal(1), rounding=ROUND_HALF_EVEN) * _MILLION
    return abs(v) if v == 0 else v


def _fiscal_years(year_end, floor, as_of):
    """(fiscal year, its first day, its four (start, end) quarters) for
    every fiscal year that ends on or after `floor` and starts by as_of.
    A fiscal year is named by the calendar year it ends in."""
    year = floor.year
    while True:
        end = dates.month_end(year, year_end)
        start = dates.add_months(end + dt.timedelta(days=1), -12)
        if start > as_of:
            return
        if end >= floor:
            quarters = []
            for q in range(4):
                first = dates.add_months(start, 3 * q)
                last = dates.add_months(first, 2)
                quarters.append((first, dates.month_end(last.year, last.month)))
            yield year, start, quarters
        year += 1


class _Company:
    """One filer's terms, and its figures for any quarter."""

    def __init__(self, inputs, seed, fact, inst):
        terms = fact["filings"]
        self.seed, self.id = seed, fact["instrument"]
        self.anchor = inputs.history_start
        self.year_end = terms.get("fiscal_year_end", 12)
        self.floor = dates.parse(terms["from"]) if "from" in terms else self.anchor.replace(year=self.anchor.year - 1)
        self.margin, self.growth = D(terms["margin"]), D(terms["growth"])
        self.debt, self.cash = D(terms["debt"]), D(terms["cash"])
        price = D(inst["price"])
        with localcontext(PRECISE):
            self.shares = D(fact["market_cap"]) / price
            self.revenue = price / D(terms["pe"]) * self.shares / self.margin
        self.splits = [(dates.parse(s["date"]), D(s["ratio"])) for s in inputs.splits if s["instrument"] == self.id]
        income = inst.get("income")
        self.payout = (price * D(income["yield"]), income["months"]) if income else None
        # The fiscal year the history starts in pays the catalogue's payout.
        self.first_year = self.anchor.year + (self.anchor.month > self.year_end)

    def _draw(self, name, end):
        r = keyed.rng(self.seed, "filing", self.id, name, end.isoformat())
        return keyed.lognormal_factor(r, JITTER[name])

    def shares_on(self, day):
        n = self.shares
        for when, ratio in self.splits:
            if when <= day:
                n *= ratio
        return n

    def balances(self, end):
        with localcontext(PRECISE):
            grown = compound(self.growth, dates.years(self.anchor, end))
            return {"long_term_debt": _millions(self.debt * self._draw("debt", end)),
                    "cash": _millions(self.cash * grown * self._draw("cash", end))}

    def quarter_flows(self, fiscal_year, start, end):
        """The flow figures of the quarter from start to end, rounded as
        filed."""
        with localcontext(PRECISE):
            grown = compound(self.growth, dates.years(self.anchor, end))
            revenue = self.revenue / 4 * grown * self._draw("revenue", end)
            net = revenue * self.margin * self._draw("margin", end)
            interest = self.debt * self._draw("debt", end) * DEBT_RATE / 4
            pretax = net / (1 - TAX) if net > 0 else net
            out = {"revenue": _millions(revenue), "operating_income": _millions(pretax + interest),
                   "interest_expense": _millions(interest), "net_income": _millions(net),
                   "operating_cash_flow": _millions(net + revenue * DEPRECIATION),
                   "capex": _millions(revenue * CAPEX * self._draw("capex", end))}
            out["eps_diluted"] = cents(out["net_income"] / self.shares_on(end))
            if self.payout:
                annual, months = self.payout
                paid = sum(1 for m in months if _in_quarter(m, start, end))
                per = annual * compound(DIVIDEND_GROWTH, fiscal_year - self.first_year) / len(months)
                out["dividends_per_share"] = cents(per) * paid
        return out

    def year_flows(self, quarters, end):
        """A fiscal year's flow figures: the sum of its quarters', and its
        earnings per share over the shares on its last day."""
        out = {k: sum((q[k] for q in quarters), ZERO) for k in quarters[0]}
        with localcontext(PRECISE):
            out["eps_diluted"] = cents(out["net_income"] / self.shares_on(end))
        return out

    def filed(self, end, form):
        r = keyed.rng(self.seed, "filed", self.id, form, end.isoformat())
        return dates.next_business(end + dt.timedelta(days=FILING_DAYS + r.randint(-FILING_JITTER, FILING_JITTER)))


def _in_quarter(month, start, end):
    return any(d.month == month for d in (start, dates.add_months(start, 1), end))


def _row(key, provider, field, end, start, value, year, period, form, filed):
    scheme, unit = (FLOWS | BALANCES)[field]
    return {"security_key": key, "field": field, "as_of": dates.epoch(end), "period_start": start.isoformat(),
            "provider": provider, "value": text(value, 2) if field in PER_SHARE else format(value, "f"),
            "scheme": scheme, "unit": unit, "fiscal_year": year, "fiscal_period": period, "form": form,
            "filed": filed.isoformat(), "run_at": dates.epoch(filed), "payload": "{}"}


def rows(inputs, seed, as_of, fact, inst, key, provider):
    """The period_facts rows of one company filed on or before as_of."""
    co = _Company(inputs, seed, fact, inst)
    out = []
    for year, start, quarters in _fiscal_years(co.year_end, co.floor, as_of):
        figures = [co.quarter_flows(year, a, b) for a, b in quarters]
        fy_end = quarters[-1][1]
        annual = co.filed(fy_end, "10-K")
        for n, ((first, last), flows) in enumerate(zip(quarters, figures, strict=True), start=1):
            if last < co.floor:
                continue
            # The 10-K covers the fourth quarter.
            form, filed = ("10-K", annual) if n == 4 else ("10-Q", co.filed(last, "10-Q"))
            if filed > as_of:
                continue
            period = f"Q{n}"
            for field, value in flows.items():
                out.append(_row(key, provider, field, last, first, value, year, period, form, filed))
            if n < 4:
                for field, value in co.balances(last).items():
                    out.append(_row(key, provider, field, last, last, value, year, period, form, filed))
        if annual <= as_of:
            for field, value in co.year_flows(figures, fy_end).items():
                out.append(_row(key, provider, field, fy_end, start, value, year, "FY", "10-K", annual))
            for field, value in co.balances(fy_end).items():
                out.append(_row(key, provider, field, fy_end, fy_end, value, year, "FY", "10-K", annual))
    return out
