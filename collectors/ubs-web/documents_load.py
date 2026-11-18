"""The ubs-web document passes: the archive index, the supplied
documents, and what the readers derive from them, parsed once per
parser generation."""

from __future__ import annotations

import functools
import hashlib
import json
import logging
import re
import sqlite3
from pathlib import Path

from collectorkit import bronze, extract, parsecache, silver, srcfp
from collectorkit.parsecache import ParseCache, ParsePool

import account_statement  # local modules: the document readers
import maturity_notice
import payment_advice
import pdf_parsers
import quarterly_report
import securities_advice
import soa_positions
from load_rows import _mortgage_rate_type, ts_from_dmy
from statement_ledger import _insert_advice_transactions, _insert_hist_transactions

log = logging.getLogger("ubs-web.load")

# `parser_generations` (migration 0009) records which generation of the
# document readers produced the document-derived rows in hand, so that a
# moved reader drops them before re-deriving instead of landing new ones
# beside them. See `_purge_stale_document_rows`.
DOCUMENT_GENERATION_SCOPE = "documents"

# What a supplied document's `doc_token` is built from — the content hash,
# since there is no UBS token to record. The prefix keeps the two origins
# legible in the table and lets the supplied rows be found without a join.
SUPPLIED_DOC_TOKEN_PREFIX = "supplied:"

# The `doc_type` a supplied Statement of assets is indexed under: the title
# the document prints on its own first page. Scraped statements of assets
# carry no doc_type at all — DOC_LABEL_RE needs letters where their listing
# label has digits — so this admits the supplied ones to the archive walk
# without changing the road any scraped document travels.
SUPPLIED_STMT_OF_ASSETS_DOC_TYPE = "Statement of assets"

# The tables the PDF passes own. Each `historical_*` table is written by
# exactly one of them and holds nothing else; `transactions` is shared with
# the live CSV export, whose ids are UBS's own transaction numbers, so the
# statement era is named by its `stmt:` prefix (DESIGN.md §3.6).
#
# The advice pass is the exception the prefix rule cannot cover. An advice
# row is deliberately keyed by UBS's own transaction number — that is the
# whole point of it, since the number is what carries the row to the twin
# leg the export already holds, and why `_assign_export_txn_ids` leaves that
# number on the payment rather than on its fee — so from the id alone it is
# indistinguishable from an export row, and a prefix delete would either
# miss it or take the export with it. It is recognised by the marker the
# parser writes into its payload instead (`document` = `payment_advice_pdf`),
# which no export row carries — an export payload being the CSV line
# verbatim — and no statement row spells.
_DOCUMENT_PASS_DELETES = (
    "DELETE FROM historical_position_snapshots",
    "DELETE FROM historical_cash_balances",
    "DELETE FROM historical_mortgages",
    "DELETE FROM transactions WHERE transaction_external_id LIKE 'stmt:%'",
    "DELETE FROM transactions WHERE payload LIKE '%payment_advice_pdf%'",
    "DELETE FROM advices",
    "DELETE FROM statement_trades",
    "DELETE FROM private_market_reports",
    "DELETE FROM private_market_report_lines",
    "DELETE FROM statement_overview_totals",
)

# The `doc_type` spellings of the per-movement payment advices. UBS labels
# them inconsistently — the same advice type appears as 'Debit Advice' and
# 'Debit advice' — so the match is case-folded and the next casing it
# invents still routes. 'UBS Advice' (a fee the bank bills itself) and
# 'Advice _ Statement' (a securities confirmation) are deliberately NOT in
# the set: neither is a payment, and neither carries the movement block
# this pass reads.
ADVICE_DOC_TYPES = frozenset({"credit advice", "debit advice"})

# The `doc_type`s that carry a securities advice: what a holding was
# bought for, where the statement of assets states no price (`advices`,
# migration 0013). A Private Market Letter is a capital call only when its
# cover page says so. The quarterly reports filed under the same label are
# read apart, into `private_market_reports` (migration 0017), and the other
# letters are declined.
CONTRACT_NOTE_DOC_TYPE = "Contract note"
PRIVATE_MARKET_LETTER_DOC_TYPE = "Private Market Letter"


# The version of the result shape `_parse_one_pdf` stores: its kinds and
# the keys and row shapes under each. The shape is this module's code,
# which no reader fingerprint covers, so any change to it bumps the
# version and every document is parsed again.
_RESULT_SHAPE = "r1"


@functools.lru_cache(maxsize=1)
def _namespaces() -> dict[str, str]:
    """The parse-cache namespace of each document scope: the scope, the
    result shape (`_RESULT_SHAPE`), the fingerprint of the readers it
    runs, and the extraction's key. A reader's fingerprint is its own
    code; the engine and its versions are the extraction key's. So an
    edit to one reader re-parses only its scope's documents, and an
    engine upgrade re-extracts all of them."""
    fingerprints = {
        "statement_of_assets": srcfp.parser_fingerprint(
            [soa_positions], python=False),
        "account_statement": srcfp.parser_fingerprint(
            [account_statement], python=False),
        "maturity_notice": srcfp.parser_fingerprint(
            [maturity_notice], python=False),
        "payment_advice": srcfp.parser_fingerprint(
            [payment_advice], python=False),
        "contract_note": srcfp.parser_fingerprint(
            [securities_advice], python=False),
        # A Private Market Letter is a capital call or a quarterly report,
        # and both readers read it.
        "private_market_letter": srcfp.parser_fingerprint(
            [securities_advice, quarterly_report], python=False),
    }
    key = extract.extraction_key(pdf_parsers.EXTRACTOR)
    return {scope: f"{scope}.{_RESULT_SHAPE}.{fp}.{key}"
            for scope, fp in fingerprints.items()}


# The version of what the document pass writes from a parse result. It
# is part of the pass's generation and of no parse-cache namespace, so a
# change to the writes re-derives the archive from the cached parses.
_WRITES = "w1"


@functools.lru_cache(maxsize=1)
def _document_generation() -> str:
    """The generation of the whole document pass: every scope's
    namespace and the version of its writes (`_WRITES`), hashed."""
    joined = "\n".join([*sorted(_namespaces().values()), _WRITES])
    return hashlib.sha256(joined.encode("utf-8")).hexdigest()[:32]


def _extract_dir() -> Path:
    """Where the documents' extractions are cached: beside the parse
    cache, under the collector's cache root."""
    return extract.default_dir("ubs-web")


def _purge_stale_document_rows(conn: sqlite3.Connection) -> int:
    """Drop the document-derived rows when the parser that produced them has
    moved, so the pass re-derives rather than adds to them.

    Whole-pass rather than per-document, though every row carries a
    `source_doc_token`: an advice is written only where the export holds no
    row under its key, so the slice is re-derived as a whole.
    """
    if not silver.stale_generation(conn, DOCUMENT_GENERATION_SCOPE,
                                   _document_generation()):
        return 0
    dropped = sum(conn.execute(sql).rowcount for sql in _DOCUMENT_PASS_DELETES)
    log.info("the document parsers have changed since these rows were "
             "written; dropped %d for re-derivation", dropped)
    return dropped


# ----------------------------------------------------------------
# Documents
# ----------------------------------------------------------------

# Extract the doc type from a listing-row label of the form
# "<doctype> <DD.MM.YYYY> <DD Month YYYY> P. <name> <...>".
DOC_LABEL_RE = re.compile(
    r"^\s*\W*\s*(?P<type>[A-Za-z][A-Za-z _]+?)\s+"
    r"(?P<date>\d{2}\.\d{2}\.\d{4})\b"
)


def _parse_doc_label(label: str) -> tuple[str | None, int | None]:
    if not label:
        return None, None
    line = label.splitlines()[0]
    m = DOC_LABEL_RE.search(line)
    if not m:
        return None, None
    return m.group("type").strip(), ts_from_dmy(m.group("date"))


def _load_documents(conn: sqlite3.Connection, snapshot_at: int,
                    dump_dir: Path, run_meta: dict) -> int:
    """Index every PDF in dump_dir/documents/ into the documents
    table, with metadata harvested from run.json where present.

    `doc_token` is UBS's full URL-resolvable token (taken from
    run.json), not the truncated filename prefix.
    """
    docs_dir = dump_dir / "documents"
    if not docs_dir.is_dir():
        return 0
    # Index run.json items by their on-disk filename — that is the
    # only stable key between the manifest and the files on disk.
    items_by_filename: dict[str, dict] = {}
    for item in (run_meta.get("documents") or {}).get("items") or []:
        fname = item.get("filename")
        if fname:
            items_by_filename[fname] = item
    inserted = 0
    for pdf in sorted(docs_dir.glob("*.pdf")):
        item = items_by_filename.get(pdf.name, {})
        token = item.get("token") or pdf.stem  # fallback to stem
        label = item.get("label") or ""
        doc_type, doc_date = _parse_doc_label(label)
        sha = bronze.sha256_file(pdf)[0]
        try:
            conn.execute(
                "INSERT INTO documents ("
                "doc_token, content_sha256, file_path, size_bytes, "
                "snapshot_at, doc_type, doc_date, account_external_id, "
                "portfolio_external_id, label"
                ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (
                    token, sha, str(pdf.resolve()), pdf.stat().st_size,
                    snapshot_at, doc_type, doc_date, None, None,
                    label,
                ),
            )
            inserted += 1
        except sqlite3.IntegrityError:
            # Same doc_token or sha already loaded in a previous dump.
            log.debug("doc %s already in silver; skipping", token[:12])
    return inserted


def _supplied_document_meta(pdf: Path, sha: str
                            ) -> tuple[str, int, str] | None:
    """Identify a bank-delivered PDF from its own text. The extraction is
    cached under its content hash `sha`, so the walk that parses it later
    reads it from there.

    Returns (doc_type, doc_date, portfolio_external_id), or None when the
    document is not one this loader knows how to read. Only the Statement
    of assets is recognised today; a type joins it once its own text
    carries enough to identify it.

    Reading the PDF rather than a filename is the whole point: a hand-placed
    file can be called anything, and a name is not evidence of what is
    inside it.
    """
    meta = soa_positions.statement_of_assets_body_meta(
        pdf_parsers.statement_of_assets_text(
            pdf, sha=sha, cache_dir=_extract_dir()))
    if meta is None:
        return None
    return (SUPPLIED_STMT_OF_ASSETS_DOC_TYPE, meta["as_of_date"],
            soa_positions.psn_portfolio_external_id(meta))


def _load_supplied_documents(conn: sqlite3.Connection,
                             supplied_dir: Path) -> int:
    """Index every recognised PDF in `supplied_dir` into the documents
    table. Returns how many rows were NEW.

    Identity is the content hash, so a re-run changes nothing and a
    statement delivered by hand and later published through the archive
    stays one document rather than two deriving the same positions.

    An unrecognised PDF is named in a warning and left out of the table
    entirely, rather than catalogued with no type — a row nothing can parse
    would sit there looking ingested.

    `snapshot_at` records when the copy was placed, there being no dump that
    captured it.
    """
    if not supplied_dir.is_dir():
        # The default (<bronze-dir>/supplied-documents) simply not existing
        # is the normal case, so this is not worth a line on every run.
        log.debug("supplied documents: %s is not a directory", supplied_dir)
        return 0
    pdfs = sorted(p for p in supplied_dir.glob("*.pdf") if p.is_file())
    if not pdfs:
        return 0
    inserted = 0
    for pdf in pdfs:
        sha = bronze.sha256_file(pdf)[0]
        token = SUPPLIED_DOC_TOKEN_PREFIX + sha
        if conn.execute("SELECT 1 FROM documents WHERE content_sha256 = ?",
                        (sha,)).fetchone():
            # Indexed by an earlier run, or served by the scraped archive
            # under its own token. Caught on the hash before the PDF is
            # opened, so a steady-state load reads no document twice.
            log.debug("supplied document %s already in silver; skipping",
                      pdf.name)
            continue
        meta = _supplied_document_meta(pdf, sha)
        if meta is None:
            log.warning("supplied documents: %s is not a document this "
                        "loader recognises; not indexed", pdf.name)
            continue
        doc_type, doc_date, portfolio = meta
        try:
            conn.execute(
                "INSERT INTO documents ("
                "doc_token, content_sha256, file_path, size_bytes, "
                "snapshot_at, doc_type, doc_date, account_external_id, "
                "portfolio_external_id, label"
                ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (
                    token, sha, str(pdf.resolve()), pdf.stat().st_size,
                    int(pdf.stat().st_mtime), doc_type, doc_date, None,
                    portfolio, "",
                ),
            )
            inserted += 1
        except sqlite3.IntegrityError:
            # The hash check above covers the ordinary case; this catches a
            # token collision, which only a hash collision could produce.
            log.debug("supplied document %s collided on insert; skipping",
                      pdf.name)
    if inserted:
        log.info("supplied documents: indexed %d new of %d in %s",
                 inserted, len(pdfs), supplied_dir)
    return inserted


# ----------------------------------------------------------------
# Historical snapshots from PDF documents
# ----------------------------------------------------------------

def _document_scope(label: str | None, doc_type: str | None) -> str:
    """Which reader a document of the archive walk goes to, by its listing
    label or its type: the scope its parse is cached and generated
    under (`_namespaces`)."""
    # The listing label names the type for a scraped document; a
    # supplied one has none and is indexed under the title it prints
    # on itself. Either way the parser reads the same document.
    if ("Statement of assets" in (label or "")
            or doc_type == SUPPLIED_STMT_OF_ASSETS_DOC_TYPE):
        return "statement_of_assets"
    if doc_type == "Maturity notice":
        return "maturity_notice"
    # Before the Account-Statement fallthrough below, which would
    # otherwise take every doc_type the SELECT admits.
    if (doc_type or "").strip().lower() in ADVICE_DOC_TYPES:
        return "payment_advice"
    if doc_type == CONTRACT_NOTE_DOC_TYPE:
        return "contract_note"
    if doc_type == PRIVATE_MARKET_LETTER_DOC_TYPE:
        return "private_market_letter"
    return "account_statement"


def _parse_one_pdf(args: tuple) -> dict:
    """Worker-side: parse one PDF with its scope's reader. Pure (no DB
    access) so it can run in a process-pool worker, and JSON-ready so it
    can be cached. `args` is (scope, token, file path, label, content
    sha256, extraction cache dir). Returns ``{"kind": ..., "rows": ...}``,
    or ``{"_error": message}`` when the parser raised (an error result is
    never cached)."""
    scope, token, fp, label, sha, extract_dir = args
    path = Path(fp)
    cache = {"sha": sha, "cache_dir": extract_dir}
    try:
        if scope == "statement_of_assets":
            positions, trades, totals = pdf_parsers.parse_statement_of_assets(
                path, token, label, **cache)
            return _parsed("statement_of_assets",
                           {"positions": positions, "trades": trades,
                            "overview_totals": totals})
        if scope == "maturity_notice":
            return _parsed("mortgage", pdf_parsers.parse_maturity_notice(
                path, token, **cache))
        if scope == "payment_advice":
            return _parsed("payment_advice", pdf_parsers.parse_payment_advice(
                path, token, **cache))
        if scope == "contract_note":
            return _parsed("securities_advice",
                           pdf_parsers.parse_contract_note(path, token, **cache))
        if scope == "private_market_letter":
            # The cover title says which a letter is. Each parser reads it
            # from the first page and declines a letter of the other kind.
            rows = pdf_parsers.parse_capital_call(path, token, **cache)
            if rows:
                return _parsed("securities_advice", rows)
            reports, lines = pdf_parsers.parse_quarterly_report(
                path, token, **cache)
            return _parsed("private_market_report",
                           {"reports": reports, "lines": lines})
        # Account Statement: one extraction yields BOTH the summary
        # balances (for historical_cash_balances) and the per-transaction
        # movement rows (for the transactions backfill).
        cash, txns = pdf_parsers.parse_account_statement_combined(
            path, token, **cache)
        return _parsed("account_statement",
                       {"cash": cash, "transactions": txns})
    except Exception as e:  # noqa: BLE001
        return {"_error": f"{type(e).__name__}: {e}"}


def _parsed(kind: str, rows) -> dict:
    """A worker's result as the parse cache stores it. Passed through
    JSON here too, so a result parsed in this load and one read back from
    the cache in a later load are the same values."""
    return json.loads(json.dumps({"kind": kind, "rows": rows}))


def _parse_key(sha: str, token: str, label: str | None) -> str:
    """The parse cache's key for one archive document: its content and
    what the parsers read beside it (its token and label; the scope is
    the namespace's). One silver ties a content hash to one documents
    row, but two silvers built from one bronze need not."""
    ident = "\0".join((sha, token, label or ""))
    return hashlib.sha256(ident.encode("utf-8")).hexdigest()


class DocumentParses:
    """The parse results of the document archive, for a whole load.

    Each document is parsed once per reader generation: a result is kept
    in the parse cache (`collectorkit.parsecache`) under its scope's
    namespace (`_namespaces`), and replayed by every later dump and every
    later load. Documents not in the cache are parsed across a process
    pool, each read from the extraction cache in `extract_dir` when it
    was extracted before. pdfplumber extraction is CPU-bound and
    GIL-bound, so the pool runs OS processes, not threads. A failed parse
    is not cached, so the next walk tries it again."""

    def __init__(self, cache: ParseCache, workers: int,
                 extract_dir: Path | None = None) -> None:
        self.cache = cache
        self.workers = workers
        self.extract_dir = extract_dir
        self._pool = ParsePool(cache, workers)
        self.walked = False

    def results(self, work: list[tuple]) -> list[tuple[str, dict]]:
        """(file name, result) per document of `work`, in its order.
        `work` rows are (doc_token, file_path, label, doc_type,
        content_sha256). A document whose file is gone and that the cache
        does not hold is left out, with no line."""
        namespaces = _namespaces()
        self.walked = True
        docs, misses = [], 0
        for token, fp, label, doc_type, sha in work:
            scope = _document_scope(label, doc_type)
            namespace = namespaces[scope]
            key = _parse_key(sha, token, label)
            args = (scope, token, fp, label, sha, self.extract_dir)
            if self.cache.get(key, namespace) is None:
                if not Path(fp).is_file():
                    continue
                misses += 1
                self._pool.enqueue(key, namespace, _parse_one_pdf, args)
            docs.append((key, namespace, Path(fp).name, args))
        if misses:
            log.info("parsing %d PDFs across %d workers", misses,
                     self.workers)
            self._pool.dispatch()
        return [(name, self._pool.resolve(key, namespace, _parse_one_pdf, args))
                for key, namespace, name, args in docs]

    def close(self) -> None:
        """End the pool, and drop the cache entries of every reader
        generation but the ones this load walked under, and the
        extractions of every extractor but `pdf_parsers.EXTRACTOR`."""
        self._pool.close()
        if self.walked:
            self.cache.sweep(set(_namespaces().values()))
            extract.sweep(self.extract_dir,
                          {extract.extraction_key(pdf_parsers.EXTRACTOR)})


def _load_historical_from_pdfs(conn: sqlite3.Connection, snapshot_at: int,
                               dump_dir: Path, parses: DocumentParses,
                               ) -> tuple[int, int, int, int]:
    """Walk every PDF tracked in the documents table whose label
    indicates a Statement of assets, an Account Statement, a
    Maturity notice, a payment advice, a securities advice or a quarterly
    report; parse it (`DocumentParses`), and upsert into the
    historical_* / transactions / advices / statement_trades /
    private_market_report* tables on the main thread.
    Returns (position_rows, cash_rows, mortgage_rows, transaction_rows) —
    the payment-advice rows are transactions and are counted with the
    statement ones; the securities advices and the statements'
    transaction lists are logged. SQLite writes stay on the main
    connection.

    The documents table accumulates across dumps, so each dump's SELECT
    re-lists the whole archive. `parses` holds each document's
    successful parse result across dumps and loads, so it is parsed once
    per parser generation; later dumps replay the cached rows, in the
    archive's order. A skipped (missing-file) or errored parse is left
    uncached and re-attempted per dump. The per-dump inserts below still
    run for every dump, so the silver tables stay byte-identical to
    parsing every dump afresh — including the last-dump snapshot_at
    stamping on statement-derived transaction upserts.

    Every statement movement and every advice is written, whatever else
    records the same booking: silver keeps each era's rows as the
    documents print them, and the UBS adapter decides which record of a
    booking gold keeps."""
    docs_dir = dump_dir / "documents"
    if not docs_dir.is_dir():
        return 0, 0, 0, 0

    # Pull (doc_token, file_path, label, doc_type, content_sha256) for
    # relevant docs from the documents table — that's where bronze
    # metadata lives. content_sha256, with the token, label and type,
    # keys the parse cache.
    # The advice spellings come from the one set the router reads too, so a
    # casing UBS invents is admitted here and routed there by the same edit.
    advice_types = sorted(ADVICE_DOC_TYPES)
    advice_slots = ", ".join("?" for _ in advice_types)
    cur = conn.execute(
        "SELECT doc_token, file_path, label, doc_type, content_sha256 "
        "FROM documents "
        "WHERE label LIKE '%Statement of assets%' "
        "   OR doc_type = ? "
        "   OR doc_type = 'Account Statement' "
        "   OR doc_type = 'Maturity notice' "
        "   OR doc_type IN (?, ?) "
        f"   OR LOWER(doc_type) IN ({advice_slots})",
        (SUPPLIED_STMT_OF_ASSETS_DOC_TYPE, CONTRACT_NOTE_DOC_TYPE,
         PRIVATE_MARKET_LETTER_DOC_TYPE, *advice_types),
    )
    work = cur.fetchall()
    if not work:
        return 0, 0, 0, 0

    # Inside the function, and below both early returns, because the purge
    # has no refill of its own: a dump with no `documents/` dir, or one
    # whose archive lists nothing parseable, returns above this line — and
    # purging there would empty the historical tables and put nothing back.
    _purge_stale_document_rows(conn)

    results = parses.results(work)

    pos_rows = 0
    cash_rows = 0
    mortgage_rows = 0
    txn_rows = 0
    txn_reject_stmts = 0
    securities_advice_rows = 0
    report_rows = 0
    trade_rows = 0
    advice_rows = 0
    for name, result in results:
        if parsecache.is_error(result):
            log.warning("PDF parse failed for %s: %s", name, result["_error"])
            continue
        kind, rows = result["kind"], result["rows"]
        if kind == "statement_of_assets":
            pos_rows += _insert_hist_positions(
                conn, (rows or {}).get("positions") or [])
            trade_rows += silver.upsert_rows(
                conn, "statement_trades", _STATEMENT_TRADE_COLUMNS,
                (rows or {}).get("trades") or [])
            silver.upsert_rows(
                conn, "statement_overview_totals", _OVERVIEW_TOTAL_COLUMNS,
                (rows or {}).get("overview_totals") or [])
        elif kind == "mortgage":
            mortgage_rows += _insert_hist_mortgages(conn, rows or [])
        elif kind == "securities_advice":
            securities_advice_rows += silver.upsert_rows(
                conn, "advices", _ADVICE_COLUMNS, rows or [])
        elif kind == "private_market_report":
            report_rows += _insert_private_market_reports(conn, rows or {})
        elif kind == "payment_advice":
            advice_rows += _insert_advice_transactions(
                conn, snapshot_at, rows or [])
        elif kind == "account_statement":
            cash_rows += _insert_hist_cash_balances(
                conn, (rows or {}).get("cash") or [])
            n, rejected = _insert_hist_transactions(
                conn, snapshot_at, (rows or {}).get("transactions") or [])
            txn_rows += n
            txn_reject_stmts += rejected
    if txn_reject_stmts:
        log.warning("%d Account-Statement PDF(s) failed movement "
                    "reconciliation; their transactions were NOT ingested",
                    txn_reject_stmts)
    if advice_rows:
        log.info("%d movement(s) from payment advices", advice_rows)
    txn_rows += advice_rows
    if securities_advice_rows:
        log.info("%d capital call(s) and contract note(s)",
                 securities_advice_rows)
    if report_rows:
        log.info("%d private-markets quarterly report(s)", report_rows)
    if trade_rows:
        log.info("%d booking(s) from statement transaction lists", trade_rows)
    # Stamped here rather than by the caller, so it records a walk that
    # actually ran: the early returns above leave the older generation in
    # place and the next dump tries again.
    silver.stamp_generation(conn, DOCUMENT_GENERATION_SCOPE,
                            _document_generation())
    return pos_rows, cash_rows, mortgage_rows, txn_rows


# What identifies a CASH row in `historical_position_snapshots` — the
# primary key with the ISIN taken out, since a cash line has none.
_HIST_CASH_IDENTITY = ("as_of_date", "portfolio_external_id",
                       "account_external_id", "currency_iso")


def _replace_hist_cash_row(conn: sqlite3.Connection, r: dict) -> None:
    """Clear the cash row `r` is about to replace.

    `INSERT OR REPLACE` cannot do it: the table's primary key ends in
    `instrument_isin`, a cash line has none, and SQLite treats NULLs in a
    primary key as DISTINCT — so the upsert never fires and every re-derived
    copy lands as a new row. The pass re-lists the whole document archive on
    every dump, so that is one extra copy of every cash row per dump, without
    limit (migration 0011 collapsed the ones already stored).

    Deleting the match first is exactly what the key would do if NULLs
    compared equal, which keeps last-writer-wins for cash the same as it is
    for securities.
    """
    conn.execute(
        "DELETE FROM historical_position_snapshots WHERE instrument_isin IS NULL"
        + "".join(f" AND {col} = ?" for col in _HIST_CASH_IDENTITY),
        tuple(r[col] for col in _HIST_CASH_IDENTITY),
    )


# The columns `soa_positions` fills on every position row it emits.
_HIST_POSITION_COLUMNS = (
    "as_of_date", "portfolio_external_id", "account_external_id",
    "instrument_isin", "currency_iso", "units", "market_value",
    "market_value_currency", "cost_price", "market_price",
    "accrued_interest", "current_fx_rate", "acquisition_fx_rate",
    "cost_basis", "nav_date", "last_purchase_date", "description", "sector",
    "source_doc_token", "payload",
)


def _upsert_each(conn: sqlite3.Connection, table: str, columns, rows,
                 before=None) -> int:
    """Upsert `rows` one at a time, so a row the table refuses costs only
    itself. `before(conn, row)` runs ahead of each row's write. Returns
    how many were written."""
    n = 0
    for r in rows:
        try:
            if before is not None:
                before(conn, r)
            silver.upsert_rows(conn, table, columns, [r])
            n += 1
        except sqlite3.IntegrityError as e:
            log.debug("%s insert failed: %s", table, e)
    return n


def _insert_hist_positions(conn: sqlite3.Connection,
                           rows: list[dict]) -> int:
    def clear_cash(c: sqlite3.Connection, r: dict) -> None:
        if r["instrument_isin"] is None:
            _replace_hist_cash_row(c, r)
    return _upsert_each(conn, "historical_position_snapshots",
                        _HIST_POSITION_COLUMNS, rows, before=clear_cash)


# The columns `soa_positions` fills on every `statement_overview_totals` row:
# one per statement copy, portfolio and asset class.
_OVERVIEW_TOTAL_COLUMNS = (
    "source_doc_token", "portfolio_external_id", "asset_class", "as_of_date",
    "currency_iso", "market_value", "payload",
)


# The columns `quarterly_report` fills on every `private_market_reports`
# row, and on every `private_market_report_lines` row: one report per
# document.
_REPORT_COLUMNS = (
    "source_doc_token", "instrument_isin", "fund_name", "as_of",
    "lines_as_of", "currency_iso", "commitment", "capital_called",
    "cumulative_expenses", "valuation", "shares_in_issue", "nav_per_share",
    "line_count", "lines_total", "lines_reconciled", "payload",
)
_REPORT_LINE_COLUMNS = (
    "source_doc_token", "line_no", "name", "region", "sector", "strategy",
    "cost", "residual_value", "payload",
)


def _insert_private_market_reports(conn: sqlite3.Connection,
                                   parsed: dict) -> int:
    """Write a quarterly report and its investment lines. A report's lines
    replace the ones it had, so a re-derive never keeps a line the table no
    longer prints. Returns how many reports were written."""
    reports = parsed.get("reports") or []
    for r in reports:
        conn.execute("DELETE FROM private_market_report_lines "
                     "WHERE source_doc_token = ?", (r["source_doc_token"],))
    n = silver.upsert_rows(conn, "private_market_reports", _REPORT_COLUMNS,
                           reports)
    silver.upsert_rows(conn, "private_market_report_lines",
                       _REPORT_LINE_COLUMNS, parsed.get("lines") or [])
    return n


# The columns `securities_advice` fills on every `advices` row: one
# capital call or contract note per document.
_ADVICE_COLUMNS = (
    "source_doc_token", "kind", "title", "doc_date", "trade_date",
    "value_date", "instrument_isin", "valor", "security_name",
    "currency_iso", "quantity", "price", "amount", "prepayment",
    "placement_fee", "stamp_duty", "settlement_amount",
    "settlement_currency_iso", "fx_rate", "fx_rate_pair", "payload",
)


# The columns `soa_transaction_list` fills on every `statement_trades`
# row: one booking a statement's transaction list prints, keyed by the
# statement and the booking's place in its list.
_STATEMENT_TRADE_COLUMNS = (
    "source_doc_token", "seq", "as_of_date", "portfolio_external_id",
    "reporting_currency_iso", "period_start", "period_end", "trade_date",
    "trade_time", "value_date", "booking_text", "quantity",
    "security_name", "valor", "isin", "currency_iso", "cost_price",
    "acquisition_fx_rate", "cost_basis", "transaction_price",
    "transaction_fx_rate", "transaction_gain_pct", "exchange_gain_pct",
    "realized_pl_pct", "transaction_value", "accrued_interest",
    "settlement_amount", "settlement_currency_iso", "taxes", "fees",
    "commission", "stock_exchange_fees", "third_party_fees",
    "financial_transaction_tax", "charges_currency_iso",
    "place_of_execution", "settlement_no", "order_no", "custody_account",
    "account_iban", "payload",
)


# The columns `account_statement` fills on every
# `historical_cash_balances` row.
_HIST_CASH_BALANCE_COLUMNS = (
    "period_end", "account_external_id", "currency_iso", "period_start",
    "opening_balance", "closing_balance", "total_debits", "total_credits",
    "source_doc_token", "payload",
)


def _insert_hist_cash_balances(conn: sqlite3.Connection,
                               rows: list[dict]) -> int:
    return _upsert_each(conn, "historical_cash_balances",
                        _HIST_CASH_BALANCE_COLUMNS, rows)


# The columns of every `historical_mortgages` row: what `maturity_notice`
# reads, and the rate type the loader reads off the product name.
_HIST_MORTGAGE_COLUMNS = (
    "as_of_date", "account_external_id", "currency_iso",
    "outstanding_balance", "product_name", "rate_type",
    "collateral_description", "source_doc_token", "payload",
)


def _insert_hist_mortgages(conn: sqlite3.Connection,
                           rows: list[dict]) -> int:
    return _upsert_each(conn, "historical_mortgages", _HIST_MORTGAGE_COLUMNS,
                        [{**r, "rate_type": _mortgage_rate_type(
                            r.get("product_name"))} for r in rows])
