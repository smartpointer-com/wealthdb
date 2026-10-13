"""The reference records: what the demo's funds hold and what its
securities are.

They depend on the catalogue (catalogue/funds.json) and the calendar
alone, never on the simulation, so a composition is the same whatever
the household traded. Each fund has one composition per month-end from
its first month to the as-of, minus its gaps; its weights are the
catalogue's targets jittered by keyed randomness and renormalised to
their total. The security facts are restated every quarter-end, as a
lookup refreshed on a schedule would.

Every row is dated by the day it describes, and fetched that day: the
rows of a month-end are the same in a full build and in an append that
reaches past it.
"""

import datetime as dt
import json
import re
from decimal import Decimal

from . import dates, keyed
from .money import ZERO, text
from .spec import CATALOGUE_DIR, name_on

FUNDS_PATH = CATALOGUE_DIR / "funds.json"
_ISIN = re.compile(r"^[A-Z]{2}[A-Z0-9]{9}[0-9]$")
JITTER = "0.05"


def load():
    return json.loads(FUNDS_PATH.read_text())


def fund_ref(inst):
    """The reference gold builds for an instrument (canonical.FundRef):
    isin:, else symbol:. The demo's instruments carry no CUSIP."""
    isin = (inst.get("isin") or "").strip().upper()
    if _ISIN.match(isin):
        return "isin:" + isin
    symbol = (inst.get("symbol") or "").strip().upper()
    return "symbol:" + symbol if symbol else None


def month_ends(first, as_of, gaps=()):
    """The month-ends from `first` to as_of, both inclusive, skipping the
    gap months (inclusive "YYYY-MM" pairs)."""
    def in_gap(day):
        ym = f"{day:%Y-%m}"
        return any(lo <= ym <= hi for lo, hi in gaps)

    day = dates.month_end(first.year, first.month)
    while day <= as_of:
        if not in_gap(day):
            yield day
        nxt = dates.add_months(dt.date(day.year, day.month, 1), 1)
        day = dates.month_end(nxt.year, nxt.month)


def quarter_ends(first, as_of):
    for day in month_ends(first, as_of):
        if day.month % 3 == 0:
            yield day


def _weights(seed, fund, day, basis, targets):
    """The targets jittered and renormalised to their total, in percent,
    four decimals, in the catalogue's order."""
    raw = {sid: Decimal(w) * keyed.lognormal_factor(keyed.rng(seed, "fund-weight", fund, basis, day.isoformat(), sid),
                                                     JITTER)
           for sid, w in targets.items()}
    total = sum(Decimal(w) for w in targets.values())
    scale = total / sum(raw.values())
    return {sid: Decimal(text(w * scale)) for sid, w in raw.items()}


def _lines(ref, at, basis, weights, securities, remainder):
    rows = []
    for n, (sid, w) in enumerate(weights.items(), start=1):
        s = securities[sid]
        rows.append({
            "fund_ref": ref, "as_of": at, "basis": basis, "line_no": n,
            "isin": s.get("isin"), "cusip": None, "figi": None, "ticker": s.get("ticker"), "exchange": None,
            "name": s["name"], "asset_type": s.get("asset_type", "Equity"), "weight": text(w),
            "quantity": None, "market_value": None, "currency": s["currency"], "country": s.get("country"),
            "risk_country": None, "sector": s.get("sector"), "industry": None, "maturity": None, "coupon": None,
            "long_currency": None, "long_amount": None, "short_currency": None, "short_amount": None,
            "payload": "{}"})
    left = Decimal(100) - sum(weights.values(), ZERO)
    if not remainder and left != 0:
        rows.append({
            "fund_ref": ref, "as_of": at, "basis": basis, "line_no": len(rows) + 1,
            "isin": None, "cusip": None, "figi": None, "ticker": None, "exchange": None,
            "name": "USD Cash", "asset_type": "Cash", "weight": text(left), "quantity": None,
            "market_value": None, "currency": "USD", "country": None, "risk_country": None, "sector": None,
            "industry": None, "maturity": None, "coupon": None, "long_currency": None, "long_amount": None,
            "short_currency": None, "short_amount": None, "payload": "{}"})
    return rows


def rows(inputs, seed, as_of):
    """Every reference row dated on or before as_of, per table."""
    cat = load()
    securities = {s["id"]: s for s in cat["securities"]}
    out = {t: [] for t in ("compositions", "lines", "attributes", "counterparties", "securities", "security_facts")}
    for fund in cat["funds"]:
        inst = inputs.instruments[fund["instrument"]]
        ref = fund_ref(inst)
        bases = [(fund.get("basis", "holdings"), fund["lines"])]
        if "index" in fund:
            bases.append(("index", fund["index"]))
        for day in month_ends(dates.parse(fund["from"]), as_of, fund.get("gaps", [])):
            at = dates.epoch(day)
            for basis, targets in bases:
                weights = _weights(seed, fund["instrument"], day, basis, targets)
                # An index lists no cash; its weights leave what they leave.
                remainder = fund.get("remainder", False) or basis == "index"
                lines = _lines(ref, at, basis, weights, securities, remainder)
                out["lines"] += lines
                out["compositions"].append({
                    "fund_ref": ref, "as_of": at, "basis": basis, "provider": cat["provider"],
                    "source_kind": "daily_file", "fund_name": name_on(inst, day), "currency": inst["currency"],
                    "net_assets": None, "line_count": len(lines),
                    "weight_sum": text(sum((Decimal(r["weight"]) for r in lines), ZERO)),
                    "units_outstanding": None, "run_at": at, "payload": "{}"})
            a = fund.get("attributes", {})
            out["attributes"].append({
                "fund_ref": ref, "as_of": at, "domicile": a.get("domicile"), "legal_form": a.get("legal_form"),
                "ucits": None, "replication": a.get("replication"), "distribution": None, "ter": a.get("ter"),
                "hedged_currency": a.get("hedged_currency"), "lending": None, "index_name": a.get("index_name"),
                "depositary": None, "run_at": at, "payload": "{}"})
            for cp in fund.get("counterparties", []):
                out["counterparties"].append({
                    "fund_ref": ref, "as_of": at, "role": cp["role"], "name": cp["name"], "lei": None,
                    "share": cp["share"], "run_at": at, "payload": "{}"})
    for day in quarter_ends(inputs.history_start, as_of):
        at = dates.epoch(day)
        for f in cat["facts"]:
            inst = inputs.instruments[f["instrument"]]
            key = fund_ref(inst)
            listed = securities.get(f["instrument"], {})
            if "figi" in f:
                out["securities"].append({
                    "security_key": key, "as_of": at, "provider": cat["provider"], "isin": listed.get("isin"),
                    "cusip": None, "figi": f["figi"], "lei": None, "ticker": inst.get("symbol"), "exchange": None,
                    "name": name_on(inst, day), "security_type": "Common Stock", "listing_currency": inst["currency"],
                    "run_at": at, "payload": "{}"})
            for field in ("sector", "hq_country", "legal_country", "market_cap"):
                if field in f:
                    out["security_facts"].append({
                        "security_key": key, "field": field, "as_of": at, "provider": cat["provider"],
                        "value": f[field], "scheme": "label" if field == "sector" else None,
                        "run_at": at, "payload": "{}"})
    return out
