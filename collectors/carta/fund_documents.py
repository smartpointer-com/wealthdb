"""A fund's documents: the document index, the capital-account statements
and the capital-call and distribution notices read as text, the fund's
ledger they give, and the Schedule K-1 face pages (k1.py)."""
from __future__ import annotations

import datetime as dt
import functools
import json
import re
from pathlib import Path

from collectorkit import bronze, derivations, docdedup, extract, pdftotext

import k1
from common import (_cj, _date_ts, _insert_cash_flow, _read_json,
                    _read_transactions_csv, _s, log)


def _index_rows(docs_dir: Path) -> list[dict]:
    """The rows of documents/index.json; empty when the run holds no index
    (a `download --no-documents` run)."""
    if not (docs_dir / "index.json").is_file():
        return []
    idx = _read_json(docs_dir / "index.json")
    rows = idx.get("results") if isinstance(idx, dict) else None
    return [row for row in rows or [] if isinstance(row, dict)]


def _doc_id(row: dict):
    """An index row's document id: its `id`, or its `uuid` when it carries
    none. download names the row's blob by the same rule."""
    return row.get("id") or row.get("uuid")


def _document_path(docs_dir: Path, row: dict) -> Path:
    """Where the run keeps the blob of an index row: `doc_<id>.pdf`."""
    return docs_dir / f"doc_{_doc_id(row)}.pdf"


def _document_pdf(docs_dir: Path, row: dict) -> Path | None:
    """The PDF an index row names, or None when the run holds none: the file
    is absent, or does not start with the PDF magic number (such as the JSON
    envelope document_url answers with). Both read as not downloaded."""
    pdf = _document_path(docs_dir, row)
    return pdf if pdf.is_file() and docdedup.is_pdf(pdf) else None


def _fund_ledger_kind(document_type: str | None) -> str | None:
    """'statement' for a capital-account statement, 'notice' for a capital
    call or distribution notice, None for any other document type."""
    dtype = document_type or ""
    if "apital account" in dtype:
        return "statement"
    if "apital call" in dtype or "istribution" in dtype:
        return "notice"
    return None


def _fund_documents(docs_dir: Path, fund_eid) -> list[dict]:
    """The index rows of one fund's documents: those whose `fund_id` is the
    fund entity's id. One login's index holds the documents of every fund it
    is a partner in."""
    return [row for row in _index_rows(docs_dir)
            if row.get("fund_id") is not None
            and str(row["fund_id"]) == str(fund_eid)]


def _warn_unowned_fund_documents(docs_dir: Path, fund_eids: list) -> None:
    """Log each statement or notice whose `fund_id` names no fund entity of
    the run. No fund's NAV history or ledger reads it."""
    owners = {str(eid) for eid in fund_eids}
    for row in _index_rows(docs_dir):
        if (_fund_ledger_kind(row.get("document_type"))
                and str(row.get("fund_id")) not in owners):
            log.warning("document %s (%s) belongs to no fund entity of this "
                        "run; not read", _doc_id(row), row.get("document_type"))


def load_documents(conn, snap: int, run_name: str, docs_dir: Path) -> int:
    """Index documents/index.json, content-deduping each PDF on its SHA-256.
    bronze_path is stored relative to the bronze root. A file that is not a
    PDF (_document_pdf) is counted in one log line and not indexed."""
    n = not_pdf = 0
    for row in _index_rows(docs_dir):
        doc_id = _doc_id(row)
        pdf = _document_pdf(docs_dir, row)
        if pdf is None:
            # Not downloaded: the row has no document_url, the fetch failed,
            # or the file holds no PDF. A later run fetches it again.
            not_pdf += _document_path(docs_dir, row).is_file()
            continue
        sha, size = bronze.sha256_file(pdf)
        bronze_path = f"{run_name}/documents/{pdf.name}"
        existing = conn.execute(
            "SELECT first_seen_at, last_seen_at FROM documents "
            "WHERE content_sha256=?", (sha,)).fetchone()
        if existing is None:
            # New content. Supersede any stale row for this doc_id (e.g. a
            # superseded capture under the same doc id) —
            # doc_id is unique, and the latest fetch is authoritative.
            conn.execute("DELETE FROM documents WHERE doc_id=?", (doc_id,))
            conn.execute(
                "INSERT INTO documents "
                "(content_sha256, doc_id, uuid, document_name, document_type, "
                " document_date, fund_id, fund_name, firm_id, firm_name, "
                " capital_account_name, stakeholder_name, file_size, "
                " bronze_path, first_seen_at, last_seen_at, payload) "
                "VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                (sha, doc_id, _s(row.get("uuid")), _s(row.get("document_name")),
                 _s(row.get("document_type")), _s(row.get("document_date")),
                 _s(row.get("fund_id")), _s(row.get("fund_name")),
                 _s(row.get("firm_id")), _s(row.get("firm_name")),
                 _s(row.get("capital_account_name")),
                 _s(row.get("stakeholder_name")), size, bronze_path,
                 snap, snap, _cj(row)),
            )
            n += 1
        else:
            first, last = existing
            conn.execute(
                "UPDATE documents SET first_seen_at=?, last_seen_at=? "
                "WHERE content_sha256=?",
                (min(first, snap), max(last, snap), sha))
    if not_pdf:
        log.info("%s: %d document file(s) hold no PDF; read as not "
                 "downloaded", run_name, not_pdf)
    return n


# How statements and notices are read: `pdftotext -layout`.
_LAYOUT = functools.partial(extract.pdftotext_layout, timeout=30)


def _extract_dir() -> Path:
    """Where extracted text is kept between runs and loads, keyed on the
    document's bytes and the engine (collectorkit.extract.cached)."""
    return extract.cache_root("carta") / "extract"


def _pdf_text(pdf: Path) -> str | None:
    """A PDF's `pdftotext -layout` text, or None (logged) when it cannot be
    extracted. Each document is extracted once: a run reads a statement
    twice (its NAV row and the fund ledger), and later runs and loads read
    the same bytes again, all from the cache."""
    try:
        return extract.cached(_LAYOUT, pdf, bronze.sha256_file(pdf)[0],
                              _extract_dir()).text()
    except pdftotext.ExtractionError as exc:
        log.warning("pdftotext failed on %s: %s", pdf.name, exc)
        return None


def _statement_nav_from_text(text: str) -> str | None:
    """The LP's ending capital balance (= NAV) from a capital-account
    statement's pdftotext output, as a digit string; None if the layout
    doesn't match."""
    m = re.search(r"Ending balance\s*\$?\s*\(?([\d,]+)\)?", text)
    return m.group(1).replace(",", "") if m else None


def _itd_figure(text: str, label: str) -> str | None:
    """The INCEPTION-TO-DATE figure on a capital-account statement line, as
    printed: `(1,234)`, `1,234` or the nil dash `—`. Each line carries three
    columns (statement period / year to date / inception to date); the
    inception-to-date one is the line's last token. None when no line
    starts with `label`. Balance-sheet lines (contributions receivable /
    received in advance) are skipped."""
    for line in text.splitlines():
        if not re.match(rf"\s*{re.escape(label)}\s", line):
            continue
        if "receivable" in line or "advance" in line:
            continue
        toks = re.findall(r"\(?[\d,]+(?:\.\d+)?\)?|—",
                          line[line.find(label) + len(label):])
        if toks:
            return toks[-1]
    return None


def _itd_decimal(raw: str | None) -> str | None:
    """A printed statement figure as a signed decimal string: parentheses
    are negative, the nil dash is zero."""
    if raw is None:
        return None
    if raw == "—":
        return "0"
    digits = raw.strip("()").replace(",", "")
    return "-" + digits if raw.startswith("(") else digits


def _statement_flows_from_text(text: str) -> tuple[float | None, float | None]:
    """The partner's INCEPTION-TO-DATE capital contributions + distributions
    (USD, as magnitudes) from a capital-account statement's pdftotext output.
    The inception-to-date column reads cleanly as the line's last figure and
    is monotonic, whereas the period columns mis-align under pdftotext when
    '—' placeholders are present. load_cash_flows differences consecutive
    statements into per-period flows. Returns (contributions_itd,
    distributions_itd); a component is None if its line is absent. Split from
    the PDF call for testability."""
    def magnitude(label: str) -> float | None:
        dec = _itd_decimal(_itd_figure(text, label))
        return None if dec is None else abs(float(dec))

    return magnitude("Capital contributions"), magnitude("Capital distributions")


# Statement lines carried onto the statement's fund_metrics row, by column:
# each line's inception-to-date figure, signed as the statement prints it.
_STATEMENT_ITD_LINES = {
    "management_fees": "Management fees",
    "net_operating_income": "Net operating income (loss)",
    "realized_gain": "Net realized gain (loss)",
    "unrealized_gain": "Net unrealized gain (loss)",
    "carried_interest": "Carried interest accrued",
}


def _statement_from_text(text: str) -> dict:
    """Everything a capital-account statement's pdftotext output states for
    its fund_metrics row: `net_asset_value` (the ending balance, a digit
    string), `capital_contributed` (inception-to-date contributions, "%.2f")
    and each _STATEMENT_ITD_LINES column. A value is None when its line is
    absent."""
    contributed = _statement_flows_from_text(text)[0]
    out = {
        "net_asset_value": _statement_nav_from_text(text),
        "capital_contributed": (None if contributed is None
                                else f"{contributed:.2f}"),
    }
    for col, label in _STATEMENT_ITD_LINES.items():
        out[col] = _itd_decimal(_itd_figure(text, label))
    return out


def _parse_statement(pdf: Path) -> dict | None:
    """_statement_from_text over one capital-account statement PDF (via
    pdftotext -layout); None when the PDF cannot be read."""
    text = _pdf_text(pdf)
    return None if text is None else _statement_from_text(text)


def _parse_statement_flows(pdf: Path) -> tuple[float | None, float | None]:
    """Inception-to-date contributions + distributions from a capital-account
    statement PDF (via pdftotext -layout); see _statement_flows_from_text."""
    text = _pdf_text(pdf)
    return (None, None) if text is None else _statement_flows_from_text(text)


# A notice's fields, as pdftotext -layout renders them: a label at the left
# and its value flushed right on the same line. The two document kinds share
# the shape and differ in three labels, which is the whole of the difference.
_NOTICE_KINDS = {
    "Capital Call Notice": {
        "kind": "capital_call",
        "date": "Due date",
        "amount": "Contribution",
        "cumulative": "Called capital (post call)",
    },
    "Distribution Notice": {
        "kind": "distribution",
        "date": "Distribution date",
        "amount": "Distribution",
        "cumulative": "Distributed capital to date (post distribution)",
    },
}


def _notice_field(text: str, label: str) -> str | None:
    """The value printed against `label`, or None. Anchored at the start of a
    line so `Distribution` does not also read `Distribution date`, and so the
    `Amount due to ...` restatement below it is never mistaken for the figure
    itself."""
    m = re.search(rf"(?m)^\s*{re.escape(label)}\s{{2,}}(.+?)\s*$", text)
    return m.group(1).strip() if m else None


def _notice_money(raw: str | None) -> float | None:
    if not raw:
        return None
    try:
        return float(raw.replace("$", "").replace(",", "").strip())
    except ValueError:
        return None


def parse_notice_text(text: str) -> dict | None:
    """One capital-call or distribution notice -> its dated amount.

    The notices are why this exists: a capital-account statement reports
    inception-to-date figures, so differencing consecutive ones can only place
    a call in the PERIOD it appeared in, and every call ends up dated at the
    period end that follows it — up to a full statement period after the
    money actually moved. A notice states the date the money was really due
    and the amount to the cent.

    `cumulative` is the fund's own running total AFTER this event. On the
    earliest notice it is what makes the pre-coverage lump computable: called
    capital post-call, less this call, is everything called before Carta
    shared anything.

    Returns None for a document that is neither notice — the caller passes
    every PDF it has.
    """
    for header, spec in _NOTICE_KINDS.items():
        if header not in text:
            continue
        when = _notice_field(text, spec["date"])
        amount = _notice_money(_notice_field(text, spec["amount"]))
        if not when or amount is None:
            log.warning("notice: %s missing its date or amount; skipped", header)
            return None
        try:
            day = dt.datetime.strptime(when, "%B %d, %Y").date()
        except ValueError:
            log.warning("notice: unparseable %s %r; skipped", spec["date"], when)
            return None
        issued = _notice_field(text, "Date of notice")
        try:
            issued = dt.datetime.strptime(issued, "%B %d, %Y").date().strftime("%m/%d/%Y")
        except (TypeError, ValueError):
            issued = None
        return {
            "kind": spec["kind"],
            "date": day.strftime("%m/%d/%Y"),
            "issued": issued,
            "amount": amount,
            "cumulative": _notice_money(_notice_field(text, spec["cumulative"])),
        }
    return None


def _parse_notice(pdf: Path) -> dict | None:
    """parse_notice_text over one PDF; split for testability."""
    text = _pdf_text(pdf)
    return None if text is None else parse_notice_text(text)


def _period_deltas(statements) -> list[tuple]:
    """Difference inception-to-date statement figures into per-period flows.
    `statements` is an iterable of (date 'MM/DD/YYYY', doc_id, contributions_itd,
    distributions_itd). Sorted by date; each positive jump in the cumulative
    becomes a per-period flow. The first statement's value lumps anything before
    the earliest available statement, so the running total reconciles to the
    fund's contributed-capital basis. Yields (doc_id, date, kind, amount)."""
    out: list[tuple] = []
    prev_c = prev_d = 0.0
    for date, docid, contrib, dist in sorted(
            statements, key=lambda s: _flow_sort_key(s[0])):
        if contrib is not None and contrib > prev_c:
            out.append((docid, date, "capital_call", contrib - prev_c))
            prev_c = contrib
        if dist is not None and dist > prev_d:
            out.append((docid, date, "distribution", dist - prev_d))
            prev_d = dist
    return out


def load_statement_nav(conn, docs_dir: Path, fund_eid) -> int:
    """Parse one fund's capital-account-statement PDFs (_fund_documents)
    into a quarterly NAV time series — the history the structured
    partner-metrics doesn't carry. One fund_metrics delta per statement
    date, carrying the NAV, the inception-to-date capital contributed the
    same statement states (so the row has a book value as well as a value),
    and its inception-to-date fees, operating income, gains and carry.

    A structured partner-metrics row loaded at the same date is richer and
    keeps its own figures; the statement adds only the lines partner-metrics
    lacks (_STATEMENT_ITD_LINES) to it."""
    itd_cols = list(_STATEMENT_ITD_LINES)
    n = 0
    for row in _fund_documents(docs_dir, fund_eid):
        if _fund_ledger_kind(row.get("document_type")) != "statement":
            continue
        pdf = _document_pdf(docs_dir, row)
        if pdf is None:
            continue
        st = _parse_statement(pdf)
        snap = _date_ts(row.get("document_date"))
        if st is None or st["net_asset_value"] is None or snap is None:
            continue
        nav, contributed = st["net_asset_value"], st["capital_contributed"]
        conn.execute(
            "INSERT INTO fund_metrics "
            "(snapshot_at, entity_external_id, currency, net_asset_value, "
            " capital_contributed, sharing_date, "
            f" {', '.join(itd_cols)}, payload) "
            f"VALUES (?,?,?,?,?,?,{','.join('?' * len(itd_cols))},?) "
            "ON CONFLICT (snapshot_at, entity_external_id) DO UPDATE SET "
            + ", ".join(f"{c} = excluded.{c}" for c in itd_cols),
            (snap, fund_eid, "USD", nav, contributed,
             _s(row.get("document_date")), *(st[c] for c in itd_cols),
             _cj({"source": "capital_account_statement",
                  "document_id": _doc_id(row), "net_asset_value": nav,
                  "capital_contributed": contributed})))
        n += 1
    return n


# k1_capital_accounts money columns, in k1.parse_face_page's keys.
_K1_COLUMNS = ("beginning_capital", "contributions", "net_income",
               "other_change", "distributions", "cash_distributions",
               "property_distributions", "ending_capital", "short_term_gain",
               "long_term_gain")


# The K-1 face page reader's scope in document_derivations, and the
# generation of what it reads. A change to k1.py that changes its results
# raises the generation, and the next run that holds a tax document derives
# it again.
K1_SCOPE = "k1"
K1_GENERATION = "1"


def load_k1_capital_accounts(conn, docs_dir: Path) -> int:
    """Parse each tax document's federal Schedule K-1 face page into a
    k1_capital_accounts row (k1.py), keyed on the PDF's sha256 like
    `documents`. The fund it belongs to is the index row's `fund_id`, which
    is the fund entity's id. The tax year and period are the form's own
    (k1.parse_face_page); the index's `tax_year` fills in only when the
    form prints no year.

    A document is read once per K-1 generation (document_derivations): one
    already derived under K1_GENERATION is skipped. Deriving it replaces the
    rows it derived before, and a re-issue under the same document id
    replaces the stale row, whose document is then derived again should it
    come back. A tax document with no federal face page (a 1042-S) yields no
    row, with a warning when the index types it as a K-1. Returns the number
    of rows written."""
    tax: dict[str, tuple[dict, Path]] = {}
    for row in _index_rows(docs_dir):
        if "tax" not in (row.get("document_type") or "").lower():
            continue
        pdf = _document_pdf(docs_dir, row)
        if pdf is not None:
            tax.setdefault(bronze.sha256_file(pdf)[0], (row, pdf))
    pending = {sha for (sha,) in derivations.pending_derivations(
        conn, K1_SCOPE, K1_GENERATION, "SELECT value FROM json_each(?)",
        (json.dumps(list(tax)),))}
    n = 0
    for sha, (row, pdf) in tax.items():
        if sha not in pending:
            continue
        try:
            parsed = k1.parse_bbox(k1.bbox_xhtml(pdf, timeout=60))
        except pdftotext.ExtractionError as exc:
            log.warning("pdftotext -bbox failed on %s: %s", pdf.name, exc)
            continue
        conn.execute("DELETE FROM k1_capital_accounts WHERE content_sha256 = ?",
                     (sha,))
        if parsed is None:
            if "k-1" in row["document_type"].lower():
                log.warning("%s is typed %r but has no federal K-1 face page; "
                            "no k1_capital_accounts row", pdf.name,
                            row["document_type"])
            derivations.record_derivation(conn, sha, K1_SCOPE, K1_GENERATION, 0)
            continue
        year = parsed["tax_year"]
        indexed = str(row.get("tax_year") or "")
        if indexed.isdigit():
            if year is None:
                year = int(indexed)
            elif int(indexed) != year:
                log.info("%s: the index files it under tax year %s; the "
                         "form states %d", pdf.name, indexed, year)
        fund = row.get("fund_id")
        conn.execute(
            "DELETE FROM document_derivations WHERE scope = ? AND "
            "content_sha256 IN (SELECT content_sha256 FROM "
            "k1_capital_accounts WHERE doc_id = ?)", (K1_SCOPE, _doc_id(row)))
        conn.execute("DELETE FROM k1_capital_accounts WHERE doc_id = ?",
                     (_doc_id(row),))
        conn.execute(
            "INSERT INTO k1_capital_accounts "
            "(content_sha256, doc_id, entity_external_id, tax_year, "
            f" period_start, period_end, {', '.join(_K1_COLUMNS)}, payload) "
            f"VALUES (?,?,?,?,?,?,{','.join('?' * len(_K1_COLUMNS))},?)",
            (sha, _doc_id(row),
             int(fund) if str(fund or "").isdigit() else None, year,
             parsed["period_start"], parsed["period_end"],
             *(parsed[c] for c in _K1_COLUMNS), _cj(parsed["printed"])))
        derivations.record_derivation(conn, sha, K1_SCOPE, K1_GENERATION, 1)
        n += 1
    return n


def _fund_cash_flows(conn, eid, docs_dir: Path, snap: int,
                     bronze_root: Path) -> int:
    """Fund cash flows: `capital_call` (deposit+contribution in gold) and
    `distribution` (distribution+withdrawal), from the fund's own documents
    (_fund_documents).

    Two sources, and the better one wins per kind. A NOTICE states the day the
    money was due and the amount to the cent, so where the fund has issued
    notices they are the ledger. A capital-account STATEMENT reports only
    inception-to-date figures, so differencing consecutive ones can place a
    flow no more precisely than the period it fell in — every call ends up
    dated at the period end that follows it, up to a full period after the
    money moved — and it is the fallback for a fund that shares no notices.

    The two are reconciled rather than mixed: the statements' inception-to-date
    total is what the fund says was called in all, the notices account for the
    part Carta shares, and the difference is emitted as ONE residue row —
    everything called before Carta shared anything, which is not itemised
    anywhere. The residue is dated at the earliest notice, the last day on
    which it is KNOWN to have been fully called; it is a bound, not an event,
    and its description says so. Without notices the first statement's figure
    is the same kind of lump.

    A supplied `<eid>-transactions.csv` itemises that lump from the holder's
    own records: each `capital_call` / `distribution` row dated on or before
    the lump is emitted at its own date, and the lump keeps only what the
    rows do not account for. A row dated after the lump cannot be part of it
    and is skipped with a warning. Where the fund reports no lump for a kind
    at all, the supplied rows are that kind's whole ledger.

    A run without a document index (`download --no-documents`) leaves the
    ledger as the last run with documents built it.
    """
    if not (docs_dir / "index.json").is_file():
        return 0
    supplied = []
    path = bronze_root / f"{eid}-transactions.csv"
    for i, tx in enumerate(_read_transactions_csv(path)):
        if tx["kind"] not in ("capital_call", "distribution") or not tx["amount"]:
            log.warning("fund %s: supplied row %d is not a capital_call or "
                        "distribution with an amount; skipped", eid, i)
            continue
        supplied.append({**tx, "row": i})
    stmts, notices = [], []
    for row in _fund_documents(docs_dir, eid):
        kind = _fund_ledger_kind(row.get("document_type"))
        pdf = _document_pdf(docs_dir, row)
        if kind is None or pdf is None:
            continue
        if kind == "statement":
            date = _s(row.get("document_date"))
            if not date:
                continue
            contrib, dist = _parse_statement_flows(pdf)
            stmts.append((date, _doc_id(row), contrib, dist))
        else:
            parsed = _parse_notice(pdf)
            if parsed:
                parsed["docid"] = _doc_id(row)
                notices.append(parsed)

    # The fund ledger is DERIVED WHOLLY from the documents this run holds, so
    # it is rebuilt rather than accumulated: a run that can read the notices
    # emits dated calls where an earlier run — whose copies of those notices
    # were url envelopes, unreadable — emitted statement-differenced ones under
    # different ids. Left to accumulate, both shapes survive and the fund's
    # called capital doubles. Runs load oldest-first, so the newest view wins.
    # A run without a document index never reaches this delete (see above).
    conn.execute("DELETE FROM cash_flows WHERE entity_external_id = ? "
                 "  AND kind IN ('capital_call', 'distribution')", (eid,))

    deltas = _period_deltas(stmts)
    from_stmts = {"capital_call": 0.0, "distribution": 0.0}
    for _docid, _date, kind, amount in deltas:
        from_stmts[kind] = from_stmts.get(kind, 0.0) + amount
    noticed = {"capital_call": 0.0, "distribution": 0.0}
    for nt in notices:
        noticed[nt["kind"]] = noticed.get(nt["kind"], 0.0) + nt["amount"]

    n = 0
    for kind in ("capital_call", "distribution"):
        prefix = "call" if kind == "capital_call" else "dist"
        desc = "fund capital call" if kind == "capital_call" else "fund distribution"
        mine = sorted((x for x in notices if x["kind"] == kind),
                      key=lambda x: _flow_sort_key(x["date"]))
        mine_supplied = [tx for tx in supplied if tx["kind"] == kind]
        if not mine:
            # No notices for this kind: the statements are all there is, and
            # the first one's figure lumps everything before it.
            periods = [(docid, date, amount) for docid, date, k, amount in deltas
                       if k == kind]
            if periods:
                docid, date, amount = periods[0]
                itemised, rest = _itemise(eid, kind, amount, date, mine_supplied)
                periods[0] = (docid, date, rest)
            else:
                itemised = mine_supplied
            n += _insert_supplied(conn, eid, snap, prefix, desc, itemised)
            for docid, date, amount in periods:
                if amount > 0.01:
                    n += _insert_cash_flow(conn, f"{prefix}:{eid}:{docid}", eid,
                                           snap, kind, date, amount, None, None, desc)
            continue
        for nt in mine:
            n += _insert_cash_flow(conn, f"{prefix}:{eid}:notice:{nt['docid']}",
                                   eid, snap, kind, nt["date"], nt["amount"],
                                   None, None, desc)
        residue = round(from_stmts.get(kind, 0.0) - noticed[kind], 2)
        stated = _residue_from_cumulative(mine[0])
        if stated is not None:
            # The notice's own running total is the fund's statement of what
            # preceded it, so it wins. A statement-derived figure BELOW it is
            # the ordinary case of a notice issued since the last statement —
            # not a disagreement, and not worth saying. Above it means there
            # is called capital that neither the notices nor the residue
            # account for, which is.
            if residue - stated > 0.01:
                log.warning("%s: the statements imply %.2f called before the "
                            "earliest notice, which itself says %.2f; %.2f is "
                            "accounted for by neither",
                            kind, residue, stated, residue - stated)
            residue = stated
        residue_date = mine[0].get("issued") or mine[0]["date"]
        itemised, residue = _itemise(eid, kind, residue, residue_date, mine_supplied)
        n += _insert_supplied(conn, eid, snap, prefix, desc, itemised)
        if residue > 0.01:
            # Dated at the notice, not at its due date: the notice is what
            # STATES the residue was already called, and dating it on the due
            # date would stack it on top of that call — recreating on one day
            # the very lump this exists to take apart.
            n += _insert_cash_flow(
                conn, f"{prefix}:{eid}:pre:{mine[0]['docid']}", eid, snap, kind,
                residue_date, residue, None, None,
                f"{desc} before Carta's coverage (not itemised; dated at the "
                "earliest notice, the last day it is known to have been called)")
    return n


def _itemise(eid, kind: str, lump: float, lump_date: str,
             supplied: list[dict]) -> tuple[list[dict], float]:
    """Split a lump the fund reports without dates into the supplied rows that
    itemise it and what they leave. Rows dated after the lump are not part of
    it and are dropped; rows that together exceed it are kept — the holder's
    records are dated, the lump is not — and the excess is logged."""
    bound = _date_ts(lump_date)
    rows, rest = [], lump
    for tx in supplied:
        at = _date_ts(tx["flow_date"])
        if at is None or bound is None or at > bound:
            log.warning("fund %s: supplied %s of %s is not dated on or before "
                        "the lump it would itemise (%s); skipped",
                        eid, kind, tx["flow_date"], lump_date)
            continue
        rows.append(tx)
        rest -= tx["amount"]
    rest = round(rest, 2)
    if rest < -0.01:
        log.warning("fund %s: the supplied %s rows exceed what the fund reports "
                    "before %s by %.2f", eid, kind, lump_date, -rest)
    return rows, max(rest, 0.0)


def _insert_supplied(conn, eid, snap: int, prefix: str, desc: str,
                     rows: list[dict]) -> int:
    """Write the supplied rows that itemise a fund lump, keyed by file row."""
    n = 0
    for tx in rows:
        n += _insert_cash_flow(conn, f"{prefix}:{eid}:supplied:{tx['row']}", eid,
                               snap, tx["kind"], tx["flow_date"], tx["amount"],
                               None, None, tx["description"] or desc)
    return n


def _flow_sort_key(date: str) -> tuple:
    """`MM/DD/YYYY` -> a sortable tuple, the same reading _period_deltas uses."""
    return (date[6:10], date[0:2], date[3:5])


def _residue_from_cumulative(notice: dict) -> float | None:
    """What the earliest notice says was already called before it: the fund's
    own running total after the event, less the event. Stated by the fund
    rather than derived from a difference of statement figures, so it is the
    better of the two when they disagree."""
    if notice.get("cumulative") is None:
        return None
    return round(notice["cumulative"] - notice["amount"], 2)
