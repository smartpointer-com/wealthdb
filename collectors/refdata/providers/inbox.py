"""
inbox: a composition obtained some other way, placed by hand.

    <data-dir>/inbox/<fund reference>/<YYYY-MM-DD>/<file>

The folder is the fund's reference as `wealthdb exposure funds` prints it
(`isin:IE00…`, or `isin_IE00…` where a colon is awkward), and the day the
composition describes. A folder holds one composition file, either

- a holdings table, CSV or XLSX: an issuer's own download, or a file
  made by hand with a header row naming at least `name` and `weight`
  (percent of net assets), and any of `isin`, `cusip`, `ticker`,
  `country`, `currency`, `sector`, `industry`, `asset_type`,
  `quantity`, `market_value`. Preamble lines above the header and notes
  below the table are skipped. A row with a name alone ends the table,
  as a footnote does, so holdings after it refuse the file; or
- an N-PORT `primary_doc.xml`.

A download copies each folder of a wanted fund into bronze once, beside
whatever provider serves the fund; the inbox itself is never changed. It
is an escape hatch, such as a composition sent as a document, not a plan
for any issuer.
"""

from __future__ import annotations

import itertools
import shutil
from datetime import date
from pathlib import Path

from . import table, xlsx
from .base import Parsed, Provider, ProviderError, Unavailable, folder_name, read_meta, write_meta
from .sec_nport import DOC as NPORT_DOC
from .sec_nport import parse_nport

INBOX = "inbox"


def folder_for(data_dir: Path, ref: str) -> Path | None:
    """The inbox folder of a fund reference, in either spelling, or by
    the name its bronze folder takes (base.folder_name)."""
    for name in dict.fromkeys((ref, ref.replace(":", "_", 1), folder_name(ref))):
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

    def fetch(self, claim, as_of, outdir: Path, ctx):
        """Copies the folder of the day asked for: the inbox serves the
        days dates() lists, never a current composition."""
        src = Path(claim["folder"]) / as_of.isoformat()
        if not src.is_dir():
            raise Unavailable(f"{src} does not exist")
        files = sorted(p for p in src.iterdir() if p.is_file() and not p.name.startswith("."))
        if not files:
            raise Unavailable(f"{src} holds no file")
        outdir.mkdir(parents=True, exist_ok=True)
        for f in files:
            shutil.copy2(f, outdir / f.name)
        write_meta(outdir, provider=self.name, ref=claim["ref"], as_of=as_of.isoformat(),
                   files=[f.name for f in files])
        return as_of

    def parse(self, indir: Path) -> Parsed:
        meta = read_meta(indir)
        ref, as_of = meta["ref"], date.fromisoformat(meta["as_of"])
        readable = [n for n in meta["files"] if n == NPORT_DOC or n.lower().endswith((".csv", ".xlsx"))]
        if len(readable) != 1:
            raise ProviderError(f"{indir}: an inbox folder holds one composition file, a CSV, an XLSX or an "
                                f"N-PORT {NPORT_DOC}; it holds {', '.join(meta['files'])}")
        name = readable[0]
        raw = (indir / name).read_bytes()
        if name == NPORT_DOC:
            out = parse_nport(raw, ref, provider=self.name)
            for c in out.compositions:
                c["source_kind"] = "inbox"
            return out
        if name.lower().endswith(".xlsx"):
            return parse_rows(xlsx.rows(raw), ref, as_of)
        return parse_csv(raw, ref, as_of)


def parse_csv(raw: bytes, ref: str, as_of: date) -> Parsed:
    """A CSV holdings table: an issuer's own download, or a hand-made
    file with at least `name` and `weight` columns."""
    return parse_rows(table.csv_rows(raw.decode("utf-8-sig")), ref, as_of)


def parse_rows(rows, ref: str, as_of: date) -> Parsed:
    """A holdings table, its header row found by its words (table.py),
    as the composition of `ref` on the folder's day. A row with a name
    alone ends the table, as a footnote does; holdings after it, before a
    blank row, refuse the file rather than go unread."""
    start = table.require_header(rows, "an inbox table needs a header row with at least a name and a weight column")
    end = table.end_index(rows, start)
    if any(table.filled(r) >= 2 for r in itertools.takewhile(table.filled, rows[end:])):
        raise ProviderError(f"the table ends at row {end + 1}, a row of one cell, yet holdings follow it; "
                            "every row states its weight")
    return table.parse(rows, start, ref, as_of, provider="inbox", source_kind="inbox")
