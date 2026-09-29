"""The household's books.

Every account keeps a ledger. A transaction changes cash, a holding, or
both; a snapshot records each holding at quantity x price and each cash
balance at its running sum. That one invariant is what makes the rest
line up downstream: holdings reconcile, cash coverage finds no gap
wherever it can measure one, and returns see a value series and the
flows that moved it.

A snapshot is complete for its source: on a snapshot day every open
account of the source is written, unless the findings switch silences
it, because gold reads a source's latest snapshot as its whole state.
"""

import dataclasses
import datetime as dt
import json

from . import dates
from .money import D, ZERO, cents, mul, q4, q8, text


@dataclasses.dataclass
class Holding:
    instrument: str
    qty: object = None      # Decimal units, or None for a holding valued by marks
    value: object = None    # Decimal mark for a holding with no unit price
    book: object = ZERO     # cost basis, instrument currency
    acquired: dt.date = None


@dataclasses.dataclass
class Account:
    id: str
    source: str
    kind: str
    display_name: str
    tax_wrapper: str
    style: str
    currency: str
    nickname: str = None
    portfolio: str = None
    opens: dt.date = None
    cash: dict = dataclasses.field(default_factory=dict)
    holdings: dict = dataclasses.field(default_factory=dict)
    seq: dict = dataclasses.field(default_factory=dict)

    def balance(self, ccy=None):
        return self.cash.get(ccy or self.currency, ZERO)

    def is_open(self, day):
        return self.opens <= day


class Book:
    """The in-memory ledger, and the silver rows it produces."""

    def __init__(self, market, instruments):
        self.market = market
        self.instruments = instruments  # id -> catalogue entry (current version)
        self.accounts = {}
        self.sources = {}               # source id -> {"cadence", "accounts": [...]}
        self.rows = {}                  # source id -> {"positions", "cash", "transactions"}
        self.portfolios = {}            # source id -> [portfolio rows]
        self.instrument_versions = {}   # source id -> {(instrument, valid_from): row}
        self.quiet = lambda aid, day: False  # a snapshot the source does not publish

    # ---- structure -----------------------------------------------------

    def add_source(self, source, cadence):
        self.sources[source] = {"cadence": cadence, "accounts": []}
        self.rows[source] = {"positions": [], "cash": [], "transactions": []}
        self.portfolios[source] = []
        self.instrument_versions[source] = {}

    def add_portfolio(self, source, pid, display_name, currency):
        self.portfolios[source].append({
            "portfolio_id": pid, "display_name": display_name,
            "base_currency": currency, "nickname": None})

    def add_account(self, acct):
        if acct.id in self.accounts:
            raise ValueError(f"account {acct.id} defined twice")
        self.accounts[acct.id] = acct
        self.sources[acct.source]["accounts"].append(acct)
        return acct

    def account(self, aid):
        return self.accounts[aid]

    def note_instrument(self, source, instrument):
        """Record every catalogue version of the instrument for the source."""
        inst = self.instruments[instrument]
        for version in inst["versions"]:
            key = (instrument, version["valid_from"])
            if key not in self.instrument_versions[source]:
                self.instrument_versions[source][key] = version

    # ---- transactions --------------------------------------------------

    def txn(self, aid, day, kind, amount, ccy=None, *, desc, counterparty=None,
            provider=None, instrument=None, qty=None, price=None,
            check=None, payload=None, cash=True):
        """Book one transaction; returns its row. `amount` carries the
        canonical sign (positive raises the account's balance). With
        cash=False the row moves no cash (an in-kind transfer, a split)."""
        acct = self.accounts[aid]
        if not acct.is_open(day):
            raise ValueError(f"{aid}: transaction on {day} outside its open range")
        ccy = ccy or acct.currency
        amount = cents(amount)
        seq = acct.seq.get(day, 0) + 1
        acct.seq[day] = seq
        if cash:
            acct.cash[ccy] = acct.cash.get(ccy, ZERO) + amount
        if instrument:
            self.note_instrument(acct.source, instrument)
        tid = f"{aid}-{day:%Y%m%d}-{seq:03d}"
        row = {
            "transaction_id": tid,
            "occurred_at": dates.txn_time(day, seq),
            "account_id": aid,
            "instrument_id": instrument,
            "kind": kind,
            "currency": ccy,
            "gross_amount": text(amount),
            "net_amount": text(amount),
            "quantity": text(qty, 8) if qty is not None else None,
            "price": text(price, 8) if price is not None else None,
            "description": desc,
            "memo": None,
            "counterparty": counterparty,
            "provider_category": provider,
            "check_number": check,
            "payload": json.dumps(payload or {}, sort_keys=True, separators=(",", ":")),
            "_day": day,
        }
        self.rows[acct.source]["transactions"].append(row)
        return row

    # ---- holdings ------------------------------------------------------

    def holding(self, aid, instrument):
        return self.accounts[aid].holdings.get(instrument)

    def qty(self, aid, instrument):
        h = self.holding(aid, instrument)
        return h.qty if h and h.qty is not None else ZERO

    def add_units(self, aid, instrument, qty, cost, day):
        acct = self.accounts[aid]
        h = acct.holdings.get(instrument)
        if h is None or (h.qty is not None and h.qty == 0):
            h = Holding(instrument=instrument, qty=ZERO, book=ZERO, acquired=day)
            acct.holdings[instrument] = h
        h.qty = q8(h.qty + qty)
        h.book = q4(h.book + cost)
        self.note_instrument(acct.source, instrument)
        return h

    def remove_units(self, aid, instrument, qty):
        """Take units out; returns the cost basis that left with them."""
        h = self.accounts[aid].holdings[instrument]
        if qty > h.qty:
            raise ValueError(f"{aid}: selling {qty} {instrument}, holding {h.qty}")
        basis = q4(mul(h.book, qty) / h.qty) if h.qty else ZERO
        h.qty = q8(h.qty - qty)
        h.book = q4(h.book - basis)
        if h.qty == 0:
            del self.accounts[aid].holdings[instrument]
        return basis

    def set_mark(self, aid, instrument, value, day, book=None, acquired=None):
        """Hold (or re-mark) a holding valued by marks rather than units."""
        acct = self.accounts[aid]
        h = acct.holdings.get(instrument)
        if h is None:
            h = Holding(instrument=instrument, value=ZERO, book=ZERO, acquired=acquired or day)
            acct.holdings[instrument] = h
        h.value = q4(value)
        if book is not None:
            h.book = q4(book)
        self.note_instrument(acct.source, instrument)
        return h

    def holding_value(self, h):
        """A holding's value at today's price, at working precision."""
        if h.qty is None:
            return h.value
        return market_value(self.instruments[h.instrument], h.qty, self.market.price(h.instrument))

    # ---- snapshots -----------------------------------------------------

    def snapshot(self, day):
        """Write the closing state of every source whose cadence covers `day`."""
        for meta in self.sources.values():
            if meta["cadence"] == "business" and not dates.is_business(day):
                continue
            for acct in meta["accounts"]:
                if acct.is_open(day) and not self.quiet(acct.id, day):
                    self._snapshot_account(acct, day)

    def _snapshot_account(self, acct, day):
        rows = self.rows[acct.source]
        at = dates.epoch(day)
        for key in sorted(acct.holdings):
            h = acct.holdings[key]
            inst = self.instruments[h.instrument]
            coupon = inst.get("coupon")
            # A bond's accrued interest runs from its last coupon date.
            accrued = q4(mul(h.qty, D(coupon["rate"]), accrual_fraction(coupon, day))) if coupon else None
            value = q4(self.holding_value(h))
            rows["positions"].append({
                "snapshot_at": at,
                "account_id": acct.id,
                "position_key": key,
                "instrument_id": h.instrument,
                "asset_class": inst["asset_class"],
                "vehicle": inst["vehicle"],
                "currency": inst["currency"],
                "quantity": text(h.qty, 8) if h.qty is not None else None,
                "market_value": text(value),
                "book_value": text(h.book),
                "accrued_interest": text(accrued) if accrued is not None else None,
                "acquisition_date": h.acquired.isoformat() if h.acquired else None,
            })
        for ccy in sorted(acct.cash):
            rows["cash"].append({
                "snapshot_at": at, "account_id": acct.id, "currency": ccy,
                "balance_kind": "closing", "amount": text(acct.cash[ccy]),
            })


def unit_price(inst, price):
    """The price of one unit of quantity. A bond's quantity is its face
    amount and its price is quoted per 100 of face."""
    return price / 100 if inst.get("coupon") else price


def market_value(inst, qty, price):
    """Quantity times price at working precision, a bond's per 100 of face."""
    value = mul(qty, price)
    return value / 100 if inst.get("coupon") else value


def accrual_fraction(coupon, day):
    """Share of a year since the bond's last coupon date (actual/365)."""
    months = sorted(coupon["months"])
    last = None
    for year in (day.year, day.year - 1):
        for m in reversed(months):
            d = dates.clamp_day(year, m, coupon["day"])
            if d <= day:
                last = d
                break
        if last:
            break
    return D((day - last).days) / 365
