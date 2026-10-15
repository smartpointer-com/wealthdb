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

# The silver tables the reference records fill.
TABLES = ("compositions", "lines", "attributes", "counterparties", "securities", "security_facts")


def load():
    return json.loads(FUNDS_PATH.read_text())


def securities(cat, instruments):
    """The catalogue's securities by id. One that is also a demo
    instrument takes its name, currency and ticker from it, and its ISIN
    when it has one, so the two catalogues cannot disagree; the entry in
    funds.json adds what a fund's line states beside them."""
    out = {}
    for s in cat["securities"]:
        inst = instruments.get(s["id"])
        if inst:
            s = {**s, "name": inst["name"], "currency": inst["currency"], "ticker": inst.get("symbol"),
                 "isin": inst.get("isin") or s.get("isin")}
        out[s["id"]] = s
    return out


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
        nxt = day + dt.timedelta(days=1)
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


def _line(ref, at, basis, n, **fields):
    """Line `n` of a composition; a column it does not name is null."""
    return {"fund_ref": ref, "as_of": at, "basis": basis, "line_no": n, "payload": "{}", **fields}


def _lines(ref, at, basis, weights, securities, remainder):
    rows = []
    for n, (sid, w) in enumerate(weights.items(), start=1):
        s = securities[sid]
        rows.append(_line(ref, at, basis, n, isin=s.get("isin"), ticker=s.get("ticker"), name=s["name"],
                          asset_type=s.get("asset_type", "Equity"), weight=text(w), currency=s["currency"],
                          country=s.get("country"), sector=s.get("sector")))
    left = Decimal(100) - sum(weights.values(), ZERO)
    if not remainder and left != 0:
        rows.append(_line(ref, at, basis, len(rows) + 1, name="USD Cash", asset_type="Cash", weight=text(left),
                          currency="USD"))
    return rows


def rows(inputs, seed, as_of):
    """Every reference row dated on or before as_of, per table. A column
    a row does not name is null."""
    cat = load()
    listed = securities(cat, inputs.instruments)
    out = {t: [] for t in TABLES}
    for fund in cat["funds"]:
        inst = inputs.instruments[fund["instrument"]]
        ref = fund_ref(inst)
        # An index lists no cash; its weights leave what they leave.
        bases = [(fund.get("basis", "holdings"), fund["lines"], fund.get("remainder", False))]
        if "index" in fund:
            bases.append(("index", fund["index"], True))
        a = fund.get("attributes", {})
        for day in month_ends(dates.parse(fund["from"]), as_of, fund.get("gaps", [])):
            at = dates.epoch(day)
            for basis, targets, remainder in bases:
                weights = _weights(seed, fund["instrument"], day, basis, targets)
                lines = _lines(ref, at, basis, weights, listed, remainder)
                out["lines"] += lines
                out["compositions"].append({
                    "fund_ref": ref, "as_of": at, "basis": basis, "provider": cat["provider"],
                    "source_kind": "daily_file", "fund_name": name_on(inst, day), "currency": inst["currency"],
                    "line_count": len(lines), "weight_sum": text(sum((Decimal(r["weight"]) for r in lines), ZERO)),
                    "run_at": at, "payload": "{}"})
            out["attributes"].append({
                "fund_ref": ref, "as_of": at, "domicile": a.get("domicile"), "legal_form": a.get("legal_form"),
                "replication": a.get("replication"), "ter": a.get("ter"), "hedged_currency": a.get("hedged_currency"),
                "index_name": a.get("index_name"), "run_at": at, "payload": "{}"})
            for cp in fund.get("counterparties", []):
                out["counterparties"].append({
                    "fund_ref": ref, "as_of": at, "role": cp["role"], "name": cp["name"], "share": cp["share"],
                    "run_at": at, "payload": "{}"})
    facts = []
    for f in cat["facts"]:
        inst = inputs.instruments[f["instrument"]]
        facts.append((f, inst, fund_ref(inst), listed.get(f["instrument"], {}).get("isin")))
    for day in quarter_ends(inputs.history_start, as_of):
        at = dates.epoch(day)
        for f, inst, key, isin in facts:
            if "figi" in f:
                out["securities"].append({
                    "security_key": key, "as_of": at, "provider": cat["provider"], "isin": isin, "figi": f["figi"],
                    "ticker": inst.get("symbol"), "name": name_on(inst, day), "security_type": "Common Stock",
                    "listing_currency": inst["currency"], "run_at": at, "payload": "{}"})
            for field in ("sector", "hq_country", "legal_country", "market_cap"):
                if field in f:
                    out["security_facts"].append({
                        "security_key": key, "field": field, "as_of": at, "provider": cat["provider"],
                        "value": f[field], "scheme": "label" if field == "sector" else None,
                        "run_at": at, "payload": "{}"})
    return out
