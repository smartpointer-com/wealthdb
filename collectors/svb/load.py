#!/usr/bin/env python3
"""Build the standalone ``svb`` silver DB from SVB Wealth Advisory statements.

A family of SVB Wealth Advisory / NFS-custodied brokerage accounts (ids of the
form ``SV[MRT]-NNNNNN``) that predate the live collectors — STATIC historical
data, so this is a one-shot builder rather than a recurring docker collector. It
parses the statement PDFs with :mod:`pdf_parsers_svbwa` into a silver SQLite that
uses the **fidelity-web** schema, so the existing fidelity gold adapter projects
it — but under a SEPARATE source id (``svb``). Keeping it a separate source is
load-bearing: the gold history macros carry positions forward per *source*, so
folding these staggered-date accounts into ``fidelity-web`` would let unrelated
fidelity snapshots supersede and drop them. See DESIGN.md.

Carry-forward policy: a statement with no holdings (an account's empty unwind) is
SKIPPED, so an account carries its last real value forward until a later
statement supersedes it, rather than zeroing mid-stream. Real exits are modelled
instead by a synthetic $0 closure injected for the still-held accounts at
``--closure-date``, so they zero out at that date.

Runs on the host with stdlib sqlite3 + pdfplumber. Idempotent
and reproducible-from-bronze: re-running against the same bronze dir converges.

Parsing the statement PDFs dominates the run and is CPU-bound, so it is fanned
out across a process pool and memoised in a persistent sidecar cache keyed by
(statement sha256, parser-logic fingerprint, signature) — the fingerprint
folds in the parser's import closure and the pdfplumber / pdfminer.six versions.
Since the bronze is a static, closed-account archive, a warm run replays every
parse from the sidecar and re-emits byte-identical silver. See
:func:`parse_statements`.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import logging
import os
import sqlite3
import sys
from concurrent.futures import ProcessPoolExecutor
from datetime import datetime, timezone
from itertools import repeat
from pathlib import Path

from collectorkit import srcfp

import pdf_parsers_svbwa

log = logging.getLogger("svb")

_SYNTHETIC_PORTFOLIO = "SVB-Sleeves"
_SYNTHETIC_KIND = "other"  # gold default → taxable_personal; config sets the real wrapper
_CLOSURE_SHA = "synthetic-closure"
_CLOSURE_DESC = "Account closed — assets transferred"
_SIGNATURE_SIDECAR = "signature.txt"
_PARSE_CACHE_FILE = "parse-cache.json"
_PARSE_CACHE_SCHEMA = 1  # bump whenever the sidecar's on-disk layout changes
_MIGRATIONS = (
    "0001_initial.sql",
    "0002_currency_asset_class_core_position.sql",
    "0003_management_style.sql",
    "0004_historical_position_snapshots.sql",
)


def ts_from_iso(d: str) -> int:
    """ISO ``YYYY-MM-DD`` → Unix seconds at midnight UTC."""
    return int(
        datetime.strptime(d, "%Y-%m-%d").replace(tzinfo=timezone.utc).timestamp()
    )


def read_signature(bronze_dir: Path, override: str | None) -> str | None:
    if override:
        return override
    sidecar = bronze_dir / _SIGNATURE_SIDECAR
    if not sidecar.is_file():
        return None
    for line in sidecar.read_text(encoding="utf-8").splitlines():
        s = line.strip()
        if s and not s.startswith("#"):
            return s
    return None


def apply_migrations(conn: sqlite3.Connection, migrations_dir: Path) -> None:
    for name in _MIGRATIONS:
        sql = (migrations_dir / name).read_text(encoding="utf-8")
        conn.executescript(sql)


def insert_statement(conn: sqlite3.Connection, parsed: dict, sha: str) -> int:
    """Insert one historical row per holding. Empty-holdings accounts are
    skipped (carry-forward policy); accounts with holdings overwrite any prior
    row at the same (as_of, account, description)."""
    period_end = parsed.get("period_end")
    if not period_end:
        return 0
    as_of = ts_from_iso(period_end)
    inserted = 0
    for acct in parsed.get("accounts", []):
        aid = acct.get("account_external_id")
        holdings = acct.get("holdings", [])
        if not aid or not holdings:
            continue
        # The PK is (as_of, account, description), but option legs share a
        # description (the strike lives on a separate line), so a naive insert
        # collapses them and drops the short legs. Disambiguate a colliding
        # description with its unique instrument key (the OCC symbol), falling
        # back to an index, so every leg survives.
        seen: set[str] = set()
        for h in holdings:
            desc = (h.get("description") or "").strip()
            if not desc:
                continue
            key = h.get("instrument_key")
            uniq = desc
            if uniq in seen:
                uniq = f"{desc} [{key}]" if key else desc
                i = 2
                while uniq in seen:
                    uniq = f"{desc} [{key or ''}#{i}]"
                    i += 1
            seen.add(uniq)
            conn.execute(
                "INSERT OR REPLACE INTO historical_position_snapshots ("
                "as_of_date, account_external_id, description, instrument_key, "
                "quantity, price, market_value, percent_of_total, currency, "
                "source_sha256, payload) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
                (
                    as_of, aid, uniq, key,
                    h.get("quantity"), h.get("price"), h.get("market_value"),
                    None, "USD", sha, json.dumps(h, separators=(",", ":")),
                ),
            )
            inserted += 1
    return inserted


def inject_closures(conn: sqlite3.Connection, closure_date: str) -> list[str]:
    """Emit a synthetic $0 row at ``closure_date`` for every account that is
    still held at the latest *real* statement, so those accounts drop to zero at
    the handoff instead of carrying their last value forward forever (which would double-count). Returns the closed
    accounts. An account already superseded before the latest statement is not in
    this set, so it is correctly left to carry forward then drop out."""
    row = conn.execute(
        "SELECT MAX(as_of_date) FROM historical_position_snapshots "
        "WHERE source_sha256 <> ?", (_CLOSURE_SHA,)
    ).fetchone()
    if row is None or row[0] is None:
        return []
    last_real = row[0]
    accts = [
        r[0] for r in conn.execute(
            "SELECT DISTINCT account_external_id FROM historical_position_snapshots "
            "WHERE as_of_date = ? AND source_sha256 <> ?", (last_real, _CLOSURE_SHA)
        )
    ]
    as_of = ts_from_iso(closure_date)
    for aid in accts:
        conn.execute(
            "INSERT OR REPLACE INTO historical_position_snapshots ("
            "as_of_date, account_external_id, description, instrument_key, "
            "quantity, price, market_value, percent_of_total, currency, "
            "source_sha256, payload) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
            (
                as_of, aid, _CLOSURE_DESC, None, None, None, 0.0, None, "USD",
                _CLOSURE_SHA, json.dumps({"synthetic": "closure"}),
            ),
        )
    return accts


def synthesize_masters(conn: sqlite3.Connection) -> int:
    """One synthetic accounts + portfolios master per account (neutral kind, so
    the gold adapter defaults to taxable_personal and config overrides set the
    precise wrapper/management). Mirrors fidelity-web's
    _synthesize_missing_account_masters but with SVB-neutral classifiers."""
    rows = conn.execute(
        "SELECT account_external_id, MAX(as_of_date) FROM historical_position_snapshots "
        "GROUP BY account_external_id"
    ).fetchall()
    payload = json.dumps({"source": "svb-sleeve-synthetic"})
    n = 0
    for aid, latest in rows:
        conn.execute(
            "INSERT OR IGNORE INTO portfolios (snapshot_at, portfolio_external_id, "
            "kind, payload) VALUES (?,?,?,?)",
            (latest, _SYNTHETIC_PORTFOLIO, _SYNTHETIC_KIND, payload),
        )
        cur = conn.execute(
            "INSERT OR IGNORE INTO accounts (snapshot_at, account_external_id, "
            "portfolio_external_id, nickname, payload, management_style) "
            "VALUES (?,?,?,?,?,?)",
            (latest, aid, _SYNTHETIC_PORTFOLIO, None, payload, None),
        )
        n += cur.rowcount
    return n


def mark_dump_run(conn: sqlite3.Connection) -> None:
    """Record one synthetic dump_runs row at the latest as_of. The fidelity
    gold adapter keys its change-trigger on dump_runs/transactions (historical
    content alone never fires a load), so a historical-only silver needs this
    marker for `wealthdb load` to pick it up. positions/activity/etc. are all
    absent (this is a one-shot statement build, not a live scrape)."""
    row = conn.execute(
        "SELECT MAX(as_of_date) FROM historical_position_snapshots").fetchone()
    if row is None or row[0] is None:
        return
    # Only the NOT-NULL columns; the *_present flags default to 0 (no live
    # positions/activity/documents in a one-shot statement build).
    conn.execute(
        "INSERT OR REPLACE INTO dump_runs (snapshot_at, silver_schema_version, "
        "run_dir, mode) VALUES (?,?,?,?)",
        (row[0], 4, "svb-sleeves-build", "historical"),
    )


# ============================================================
# Statement parsing: process pool + persistent parse cache
# ============================================================
#
# Parsing the statement PDFs (pdfplumber text extraction) is ~99% of a load and
# strictly CPU-bound. Two layers cut it down without touching what silver holds:
#
#   * a persistent sidecar cache keyed by (statement sha256, parser-logic
#     fingerprint, signature) — bronze is a static,
#     closed-account archive, so a warm run replays every parse from the sidecar
#     and never opens a PDF; and
#   * a process pool for the misses (a cold run, or after a parser edit), so the
#     28 independent parses fan out across cores instead of running serially.
#
# The cached value is exactly the dict the parser returns and insert_statement
# consumes it identically, so cached and freshly-parsed silver are byte-for-byte
# identical. Statement sha256 is computed in the parent (statements are tens of
# KB, so hashing is negligible) so a cache HIT can skip the parse entirely — a
# warm run therefore never spawns a pool.


def _parse_statement(path: str, signature: str | None) -> dict:
    """Parse one statement PDF into the parser's structured dict.

    Module-level (not a closure) so it pickles for
    :class:`~concurrent.futures.ProcessPoolExecutor` under the ``spawn`` start
    method used on macOS.
    """
    return pdf_parsers_svbwa.parse_svbwa_statement_pdf(
        path, expected_signature=signature)


def _default_cache_dir() -> Path:
    """XDG cache location for the parse sidecar.

    Defaults to ``$XDG_CACHE_HOME/wealthdb/svb`` (``~/.cache/wealthdb/svb`` when
    the variable is unset). The sidecar holds parsed statement data — derived
    PII — so it stays outside the repo, exactly like the silver DB, and never
    under a secrets dir.
    """
    base = os.environ.get("XDG_CACHE_HOME") or str(Path.home() / ".cache")
    return Path(base) / "wealthdb" / "svb"


_EXTRACTOR_DISTS = ("pdfplumber", "pdfminer.six")


def _parser_logic_fingerprint() -> str:
    """Fingerprint of the parsing logic, folded into every cache key so a code
    edit to ``pdf_parsers_svbwa.py`` (or anything in its import closure) or a
    pdfplumber / pdfminer.six upgrade auto-invalidates cached parses, while a
    comment / formatting / docstring edit — which can't change a parse — does
    not. See :func:`collectorkit.srcfp.parser_fingerprint`."""
    return srcfp.parser_fingerprint([pdf_parsers_svbwa], _EXTRACTOR_DISTS)


def _cache_key(file_sha: str, logic_fp: str, signature: str | None) -> str:
    # Opaque key; the statement sha and logic fingerprint are hex (no ':'), and
    # the signature is last, so the join stays unambiguous.
    return f"{file_sha}:{logic_fp}:{signature or ''}"


def _load_parse_cache(cache_dir: Path | None) -> dict[str, dict]:
    """Load the sidecar's parsed-dict entries, or ``{}`` when caching is off
    (``cache_dir is None``), the sidecar is absent/corrupt, or its schema is
    from an older layout (in which case it is ignored and rebuilt)."""
    if cache_dir is None:
        return {}
    try:
        blob = json.loads(
            (cache_dir / _PARSE_CACHE_FILE).read_text(encoding="utf-8"))
    except (FileNotFoundError, ValueError):
        return {}
    if blob.get("schema") != _PARSE_CACHE_SCHEMA:
        return {}
    return blob.get("entries", {})


def _save_parse_cache(cache_dir: Path | None, entries: dict[str, dict]) -> None:
    """Persist the parsed-dict entries to the sidecar (atomic rename). No-op
    when caching is off (``cache_dir is None``)."""
    if cache_dir is None:
        return
    cache_dir.mkdir(parents=True, exist_ok=True)
    path = cache_dir / _PARSE_CACHE_FILE
    tmp = path.with_name(path.name + ".tmp")
    tmp.write_text(
        json.dumps({"schema": _PARSE_CACHE_SCHEMA, "entries": entries},
                   separators=(",", ":")),
        encoding="utf-8")
    tmp.replace(path)


def parse_statements(pdfs: list[Path], shas: list[str], *,
                     signature: str | None, cache_dir: Path | None,
                     max_workers: int | None) -> list[dict]:
    """Return the parsed dict for each PDF in ``pdfs`` order.

    Cache hits (matched against the sidecar's pre-run state only, so that two
    statements with identical content are still each parsed rather than one
    shadowing the other) are replayed directly; misses are parsed — in a process
    pool when more than one needs parsing and ``max_workers`` allows it, else in
    process — and their results folded back into the sidecar.
    """
    logic_fp = _parser_logic_fingerprint()
    cached = _load_parse_cache(cache_dir)

    parsed: list[dict | None] = [None] * len(pdfs)
    misses: list[int] = []
    for i, sha in enumerate(shas):
        hit = cached.get(_cache_key(sha, logic_fp, signature))
        if hit is not None:
            parsed[i] = hit
        else:
            misses.append(i)

    if misses:
        paths = [str(pdfs[i]) for i in misses]
        workers = (max_workers if max_workers is not None
                   else min(len(paths), os.cpu_count() or 1))
        if workers > 1 and len(paths) > 1:
            with ProcessPoolExecutor(max_workers=workers) as pool:
                outputs = list(pool.map(_parse_statement, paths,
                                        repeat(signature)))
        else:
            outputs = [_parse_statement(p, signature) for p in paths]
        fresh = dict(cached)
        for i, out in zip(misses, outputs):
            parsed[i] = out
            if not out.get("_error"):
                fresh[_cache_key(shas[i], logic_fp, signature)] = out
        _save_parse_cache(cache_dir, fresh)

    return parsed  # type: ignore[return-value]


def build(silver_db: Path, bronze_dir: Path, *, signature: str | None,
          closure_date: str, migrations_dir: Path,
          cache_dir: Path | None = None, max_workers: int | None = None) -> None:
    # Validate the bronze BEFORE touching the existing silver: a mis-pointed
    # --bronze-dir must fail loudly, not silently replace a good svb.db with
    # an empty rebuild (which then zeroes the source out of gold).
    pdfs = sorted(p for p in bronze_dir.iterdir()
                  if p.is_file() and p.suffix.lower() == ".pdf") if bronze_dir.is_dir() else []
    if not pdfs:
        raise SystemExit(
            f"svb load: no statement PDFs in {bronze_dir} — refusing to "
            f"rebuild {silver_db} from an empty bronze. Point --data-dir / "
            f"--bronze-dir at the archive (PDFs live in <data-dir>/bronze/).")
    # Hash then parse every statement (cache replay + process pool) before the
    # existing silver is touched, so a parse crash also leaves the good svb.db in
    # place. Inserts still run serially below in sorted-PDF order, preserving the
    # INSERT OR REPLACE last-writer semantics and the per-statement log order.
    shas = [hashlib.sha256(pdf.read_bytes()).hexdigest() for pdf in pdfs]
    results = parse_statements(pdfs, shas, signature=signature,
                               cache_dir=cache_dir, max_workers=max_workers)
    if silver_db.exists():
        silver_db.unlink()  # full rebuild — reproducible from bronze
    conn = sqlite3.connect(str(silver_db))
    try:
        apply_migrations(conn, migrations_dir)
        inserted = parsed_ok = skipped = 0
        for pdf, sha, res in zip(pdfs, shas, results):
            if res.get("_error"):
                log.warning("skip %s: %s", pdf.name, res["_error"])
                skipped += 1
                continue
            n = insert_statement(conn, res, sha)
            parsed_ok += 1
            inserted += n
            if n == 0:
                log.info("carry-forward (no holdings): %s", pdf.name)
        closed = inject_closures(conn, closure_date)
        synth = synthesize_masters(conn)
        mark_dump_run(conn)
        conn.commit()
        log.info(
            "svb silver built: %d holdings rows from %d statement(s) "
            "(%d skipped), %d account(s) closed @ %s, %d master(s) synthesised",
            inserted, parsed_ok, skipped, len(closed), closure_date, synth,
        )
    finally:
        conn.close()


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description=__doc__.strip().splitlines()[0])
    p.add_argument("--silver-db", type=Path, required=True,
                   help="output svb silver SQLite path")
    p.add_argument("--bronze-dir", type=Path, required=True,
                   help="directory of SVB statement PDFs + signature.txt")
    p.add_argument("--signature", default=None,
                   help="page-1 signature substring (else read signature.txt)")
    p.add_argument("--closure-date", default="2023-09-30",
                   help="synthetic $0 closure date for still-held accounts "
                        "(the handoff to the destination source); default "
                        "2023-09-30")
    p.add_argument("--migrations-dir", type=Path,
                   default=Path(__file__).parent / "migrations")
    p.add_argument("--cache-dir", type=Path, default=_default_cache_dir(),
                   help="directory for the persistent parse cache sidecar "
                        "(default $XDG_CACHE_HOME/wealthdb/svb). Keyed by "
                        "(statement sha256, parser-logic fingerprint, signature), "
                        "so a parser edit or a pdfplumber / pdfminer.six upgrade "
                        "auto-invalidates it; a warm run replays every parse from "
                        "it. It holds parsed statement data, so it lives outside "
                        "the repo like the silver DB.")
    p.add_argument("-v", "--verbose", action="store_true")
    args = p.parse_args(argv)
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(levelname)s %(message)s")
    sig = read_signature(args.bronze_dir, args.signature)
    if sig is None:
        log.warning("no signature configured (--signature or signature.txt); "
                    "ingesting every PDF unverified")
    build(args.silver_db, args.bronze_dir, signature=sig,
          closure_date=args.closure_date, migrations_dir=args.migrations_dir,
          cache_dir=args.cache_dir)
    return 0


if __name__ == "__main__":
    sys.exit(main())
