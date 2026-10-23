"""
openfigi: a security's FIGI, name, ticker, exchange and type, from
OpenFIGI's mapping API.

- Claim: a security with an ISIN, else a CUSIP, else a US ticker.
- Fetch: one mapping job per security, five jobs to a request (the
  smaller of the two limits the API documents without a key), so a run
  asks once for every five securities. Each security's answer is kept in
  its own folder. A request that fails fails its five securities for the
  run, and only those.
- Parse: the US listing where the API returns one, else the first, as an
  identity row with its composite FIGI where it names one. A ticker is
  kept from a US listing only: a local ticker elsewhere could read as a
  different US symbol. OpenFIGI states no sector and no country. A
  bond's type names its market, and gold reads the bond's sector from
  it. An identifier it does not know is still a lookup.
- US ticker (us_ticker): the ticker of the US listing an ISIN maps to,
  so the SEC's providers find a US fund named by its ISIN alone. One job
  to a request, asked once a run for each ISIN, and only by a run that
  asks openfigi.

Terms: README.md, openfigi.
"""

from __future__ import annotations

import itertools
from datetime import date
from pathlib import Path

import fetcher
from collectorkit import bronze

from .base import Parsed, Provider, ProviderError, ident, present, read_json, read_meta, row, today_utc, write_meta

API = "https://api.openfigi.com/v3/mapping"
DOC = "mapping.json"
BATCH = 5


def job(security) -> dict | None:
    """The mapping job that names a security, the strongest identifier
    first."""
    if isin := ident(security, "isin"):
        return {"idType": "ID_ISIN", "idValue": isin}
    if cusip := ident(security, "cusip"):
        return {"idType": "ID_CUSIP", "idValue": cusip}
    if symbol := ident(security, "symbol"):
        return {"idType": "TICKER", "idValue": symbol, "exchCode": "US"}
    return None


def mapping(jobs: list, ctx) -> list:
    """OpenFIGI's results for up to BATCH mapping jobs, one request, in
    the jobs' order. An answer that is no list of one result per job is
    an error."""
    results = ctx.client.post_json(API, jobs).json()
    if not isinstance(results, list) or len(results) != len(jobs):
        raise ProviderError(f"{API} answered {len(jobs)} jobs with something else")
    return results


def listing(result: dict) -> tuple[dict, bool]:
    """The listing a mapping result is read by, and whether it is a US
    one: its first US listing, else its first; {} where it names none."""
    data = result.get("data") or []
    us = next((d for d in data if d.get("exchCode") == "US"), None)
    return (us, True) if us is not None else (data[0] if data else {}, False)


class OpenFigi(Provider):
    name = "openfigi"
    serves_securities = True

    def wants_constituent(self, security) -> bool:
        """A constituent is asked only for the US ticker of a stock that
        lacks a sector: the ticker lets the SEC's lookup find the company
        and its industry code. OpenFIGI states no company's sector and no
        country, so a security with a ticker, and one whose identifier is
        no US one (a CINS code starts with a letter), are not asked. A
        bond is not asked either: a held fund's bonds run to thousands,
        each a small share."""
        if security.symbol or "sector" not in security.missing or security.asset_class != "public_equity":
            return False
        return ident(security, "isin").startswith("US") or ident(security, "cusip")[:1].isdigit()

    def claim_security(self, security, ctx):
        j = job(security)
        if j is None:
            return None
        claim = {"key": security.key, "job": j}
        ctx.cache.setdefault("openfigi_pending", []).append(claim)
        return claim

    def _answer(self, claim, ctx) -> dict:
        """This claim's job result; the request also answers the next jobs
        waiting, up to BATCH in all. The jobs are answered in the order
        they were claimed, so a cursor past the answered ones finds the
        next."""
        answers = ctx.cache.setdefault("openfigi_answers", {})
        if claim["key"] not in answers:
            pending = ctx.cache.get("openfigi_pending", [])
            start = ctx.cache.get("openfigi_cursor", 0)
            while start < len(pending) and pending[start]["key"] in answers:
                start += 1
            ctx.cache["openfigi_cursor"] = start
            others = (c for c in itertools.islice(pending, start, None)
                      if c["key"] not in answers and c["key"] != claim["key"])
            batch = [claim, *itertools.islice(others, BATCH - 1)]
            try:
                results = mapping([c["job"] for c in batch], ctx)
            except (ProviderError, fetcher.FetchError) as e:
                for c in batch:
                    answers[c["key"]] = e
                raise
            for c, r in zip(batch, results, strict=True):
                answers[c["key"]] = r
        answer = answers[claim["key"]]
        if isinstance(answer, Exception):
            raise ProviderError(f"its batch failed: {answer}")
        return answer

    def fetch_security(self, claim, outdir: Path, ctx):
        result = self._answer(claim, ctx)
        bronze.atomic_write_json(outdir / DOC, {"job": claim["job"], "result": result})
        write_meta(outdir, provider=self.name, key=claim["key"], as_of=today_utc().isoformat())

    def parse(self, indir: Path) -> Parsed:
        meta = read_meta(indir)
        return parse_mapping(read_json(indir, DOC), meta["key"], date.fromisoformat(meta["as_of"]), provider=self.name)

    def probe(self, ctx) -> str:
        resp = ctx.client.post_json(API, [{"idType": "TICKER", "idValue": "IBM", "exchCode": "US"}])
        return f"OpenFIGI mapping answered {resp.status}"


def us_ticker(isin: str, ctx) -> str:
    """The ticker of the US listing OpenFIGI maps an ISIN to, for a
    provider that finds a US fund by its ticker. "" where it maps it to
    no US listing: a local ticker elsewhere could read as a different US
    symbol. "" too where the run does not ask openfigi (Context.asks).
    One job in one request, asked once a run for each ISIN; a failed
    request fails again without a second one (Context.once)."""
    if not ctx.asks(OpenFigi.name):
        return ""

    def load():
        (result,) = mapping([{"idType": "ID_ISIN", "idValue": isin}], ctx)
        first, us = listing(result or {})
        return str(first.get("ticker") or "").strip().upper() if us else ""
    return ctx.once(("openfigi_us_ticker", isin), load)


def parse_mapping(doc: dict, key: str, as_of: date, *, provider: str) -> Parsed:
    """One mapping job and its result as an identity row for `key`."""
    j, result = doc.get("job") or {}, doc.get("result") or {}
    data = result.get("data") or []
    first, us = listing(result)
    return Parsed(securities=[row(
        "securities", security_key=key, as_of=as_of, provider=provider,
        isin=j.get("idValue") if j.get("idType") == "ID_ISIN" else None,
        cusip=j.get("idValue") if j.get("idType") == "ID_CUSIP" else None,
        figi=first.get("compositeFIGI") or first.get("figi"), ticker=first.get("ticker") if us else None,
        exchange=first.get("exchCode"), name=first.get("name"),
        security_type=first.get("securityType2") or first.get("securityType"),
        # `found: False` and `listings: 0` are answers too, and stay.
        payload=present({"found": bool(data), "listings": len(data),
                         "local_ticker": None if us else first.get("ticker"),
                         "market_sector": first.get("marketSector"),
                         "share_class_figi": first.get("shareClassFIGI"),
                         "message": result.get("error") or result.get("warning")}, keep_falsy=True))])
