"""
inbox: a composition obtained some other way, placed by hand.

    <data-dir>/inbox/<fund reference>/<YYYY-MM-DD>/<file>

The folder is the fund's reference as `wealthdb exposure funds` prints it
(`isin:IE00…`, or `isin_IE00…` where a colon is awkward), and the day the
composition describes. A folder holds one composition file, either

- a CSV with a header row naming at least `name` and `weight` (percent
  of net assets), and any of `isin`, `cusip`, `ticker`, `country`,
  `currency`, `sector`, `industry`, `asset_type`, `quantity`,
  `market_value`; or
- a file another provider parses, recognised by its name: an N-PORT
  `primary_doc.xml`.

A download copies each folder of a wanted fund into bronze once, beside
whatever provider serves the fund; the inbox itself is never changed. It
is an escape hatch, such as a composition sent as a document, not a plan
for any issuer.
"""

from __future__ import annotations

import csv
import io
import shutil
from datetime import date
from pathlib import Path

from .base import Parsed, Provider, ProviderError, Unavailable, blank_to_none, dec, read_meta, write_meta
from .sec_nport import DOC as NPORT_DOC
from .sec_nport import parse_nport

INBOX = "inbox"
CSV_COLUMNS = ("isin", "cusip", "ticker", "name", "weight", "country", "currency", "sector", "industry",
               "asset_type", "quantity", "market_value")


def folder_for(data_dir: Path, ref: str) -> Path | None:
    """The inbox folder of a fund reference, in either spelling."""
    for name in (ref, ref.replace(":", "_", 1)):
        p = data_dir / INBOX / name
        if p.is_dir():
            return p
    return None


def _dates(folder: Path) -> list:
    out = []
    for p in folder.iterdir():
        try:
            if p.is_dir():
                out.append(date.fromisoformat(p.name))
        except ValueError:
            continue
    return sorted(out)


class Inbox(Provider):
    name = "inbox"
    serves_funds = True
    additive = True

    def claim(self, fund, ctx):
        if ctx.data_dir is None:
            return None
        folder = folder_for(ctx.data_dir, fund.ref)
        if folder is None or not _dates(folder):
            return None
        return {"ref": fund.ref, "folder": str(folder)}

    def dates(self, claim, since, until, ctx):
        return [d for d in _dates(Path(claim["folder"])) if since <= d <= until]

    def latest(self, claim, ctx):
        days = _dates(Path(claim["folder"]))
        return days[-1] if days else None

    def fetch(self, claim, as_of, outdir: Path, ctx):
        folder = Path(claim["folder"])
        days = _dates(folder)
        if not days:
            raise Unavailable(f"{folder} holds no dated folder")
        day = days[-1] if as_of is None else as_of
        src = folder / day.isoformat()
        if not src.is_dir():
            raise Unavailable(f"{src} does not exist")
        files = sorted(p for p in src.iterdir() if p.is_file() and not p.name.startswith("."))
        if not files:
            raise Unavailable(f"{src} holds no file")
        outdir.mkdir(parents=True, exist_ok=True)
        for f in files:
            shutil.copy2(f, outdir / f.name)
        write_meta(outdir, provider=self.name, ref=claim["ref"], as_of=day.isoformat(),
                   files=[f.name for f in files])
        return day

    def parse(self, indir: Path) -> Parsed:
        meta = read_meta(indir)
        ref, as_of = meta["ref"], date.fromisoformat(meta["as_of"])
        readable = [n for n in meta["files"] if n == NPORT_DOC or n.lower().endswith(".csv")]
        if len(readable) != 1:
            raise ProviderError(f"{indir}: an inbox folder holds one composition file, a CSV or an N-PORT "
                                f"{NPORT_DOC}; it holds {', '.join(meta['files'])}")
        name = readable[0]
        if name == NPORT_DOC:
            out = parse_nport((indir / name).read_bytes(), ref, provider=self.name)
            for c in out.compositions:
                c["source_kind"] = "inbox"
            return out
        return parse_csv((indir / name).read_bytes(), ref, as_of)


def parse_csv(raw: bytes, ref: str, as_of: date) -> Parsed:
    """The inbox CSV: one line per holding, weights in percent."""
    text = raw.decode("utf-8-sig")
    reader = csv.DictReader(io.StringIO(text))
    cols = {(h or "").strip().lower(): h for h in reader.fieldnames or []}
    if "name" not in cols or "weight" not in cols:
        raise ProviderError("an inbox CSV needs a header with at least name and weight")
    lines = []
    for n, row in enumerate(reader, start=1):
        get = {k: blank_to_none(row.get(v)) for k, v in cols.items()}
        if not any(get.values()):
            continue
        lines.append({
            "fund_ref": ref, "as_of": as_of, "basis": "holdings", "line_no": n,
            "isin": get.get("isin"), "cusip": get.get("cusip"), "figi": None, "ticker": get.get("ticker"),
            "exchange": None, "name": get.get("name"), "asset_type": get.get("asset_type"),
            "weight": dec(get.get("weight")), "quantity": dec(get.get("quantity")),
            "market_value": dec(get.get("market_value")), "currency": get.get("currency"),
            "country": get.get("country"), "risk_country": None, "sector": get.get("sector"),
            "industry": get.get("industry"), "maturity": None, "coupon": None, "long_currency": None,
            "long_amount": None, "short_currency": None, "short_amount": None,
            "payload": {k: v for k, v in get.items() if k not in CSV_COLUMNS and v},
        })
    weights = [float(x["weight"]) for x in lines if x["weight"] is not None]
    out = Parsed(lines=lines)
    out.compositions.append({
        "fund_ref": ref, "as_of": as_of, "basis": "holdings", "provider": "inbox", "source_kind": "inbox",
        "fund_name": None, "currency": None, "net_assets": None, "line_count": len(lines),
        "weight_sum": dec(round(sum(weights), 8)) if weights else None, "units_outstanding": None, "payload": {},
    })
    return out
