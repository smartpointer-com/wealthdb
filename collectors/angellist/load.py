#!/usr/bin/env python3
"""angellist silver loader.

Parses the venture GraphQL captured by download.py (bronze
`captures.jsonl`) into the source-shaped SQLite silver DB — the input
contract to the gold engine. Idempotent: snapshots already in `dump_runs`
are skipped unless --force; every per-snapshot table is keyed by
`snapshot_at` and written with INSERT OR REPLACE so a re-load is clean.

Tables (see migrations/0001_initial.sql + DESIGN.md):
  vehicles           one row per investable (SPV / fund deal), by investableGuid
  positions          per-snapshot funded position (commitment / contributed /
                     invested / realized / value), minor units + currency
  portfolio_summary  per-snapshot dashboard totals + IRR/TVPI/DPI
  portfolio_timeseries  ~monthly NAV history (value/invested/realized/
                     unrealized per date), latest-snapshot-wins
  commitments        per-snapshot open (unfunded) commitments
  dump_runs          per-snapshot bookkeeping

Money: GraphQL returns `{currency, fractional}` where `fractional` is the
amount in minor units (cents). We store the minor integer + currency
verbatim; the gold adapter scales.
"""
from __future__ import annotations

import argparse
import csv
import datetime
import hashlib
import json
import logging
import re
import sys
from pathlib import Path

from collectorkit import bronze, cli, silver

log = logging.getLogger("angellist.load")

HERE = Path(__file__).resolve().parent
MIGRATIONS = HERE / "migrations"

DEFAULT_DEST = Path("/data")
DEFAULT_DB = Path("/data/angellist.db")
DEFAULT_DOCS = Path("/data/angellist-documents")

_YEAR_RE = re.compile(r"\b(20\d{2})\b")


def parse_money_cents(s):
    """Parse a K-1 CSV money cell ('$-7,994', '(1,234)', '1234.56') to minor
    units (cents). Empty / non-numeric -> None."""
    s = (s or "").strip()
    if not s:
        return None
    neg = s.startswith("(") and s.endswith(")")
    s = s.strip("()").replace("$", "").replace(",", "").strip()
    if not s or s in ("-", "."):
        return None
    try:
        val = float(s)
    except ValueError:
        return None
    return int(round((-val if neg else val) * 100))


def _hdr_index(header, *needles, exact=None):
    """Column index by exact (case-insensitive) header, or by all `needles`
    being substrings of the header. None if not found."""
    for i, h in enumerate(header):
        hl = h.strip().lower()
        if exact is not None and hl == exact.lower():
            return i
        if needles and all(n.lower() in hl for n in needles):
            return i
    return None


def _vehicle_name_map(conn):
    """investableName(lower) -> vehicle_external_id, only for names held by
    exactly one vehicle (so the K-1 company match is unambiguous)."""
    counts, last = {}, {}
    for vid, name in conn.execute(
            "SELECT vehicle_external_id, name FROM vehicles WHERE name IS NOT NULL"):
        k = name.strip().lower()
        counts[k] = counts.get(k, 0) + 1
        last[k] = vid
    return {k: v for k, v in last.items() if counts[k] == 1}


def _parse_k1_csv(conn, path, year, doc_id, name_to_vehicle) -> int:
    rows = list(csv.reader(path.open(newline="", encoding="utf-8-sig")))
    if not rows:
        return 0
    h = rows[0]
    ix = {
        "company": _hdr_index(h, exact="portfolio company"),
        "fund": _hdr_index(h, exact="fund"),
        "status": _hdr_index(h, "k-1 status"),
        "final": _hdr_index(h, "final k-1"),
        "ein": _hdr_index(h, "fund tax id"),
        "beg": _hdr_index(h, exact="beginning capital"),
        "contrib": _hdr_index(h, exact="contributions"),
        "ni": _hdr_index(h, "current year net income"),
        "other": _hdr_index(h, "other increase"),
        "dist": _hdr_index(h, "withdrawals & distributions"),
        "cashdist": _hdr_index(h, "cash distributions"),
        "end": _hdr_index(h, exact="ending capital"),
        "endpct": _hdr_index(h, "ending capital %"),
    }

    def cell(r, key):
        i = ix.get(key)
        return r[i].strip() if i is not None and i < len(r) else ""

    n = 0
    for r in rows[1:]:
        fund = cell(r, "fund") or cell(r, "company")
        company = cell(r, "company")
        if not fund:
            continue
        endpct_raw = cell(r, "endpct").replace("%", "").replace(",", "")
        try:
            endpct = float(endpct_raw) if endpct_raw else None
        except ValueError:
            endpct = None
        conn.execute(
            "INSERT OR REPLACE INTO k1_capital_accounts "
            "(tax_year, fund_name, fund_tax_id, portfolio_company, k1_status, final_k1, "
            " beginning_capital_minor, contributions_minor, net_income_minor, other_change_minor, "
            " distributions_minor, cash_distributions_minor, ending_capital_minor, ending_capital_pct, "
            " vehicle_external_id, source_document_id, payload) "
            "VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
            (year, fund, cell(r, "ein") or None, company or None, cell(r, "status") or None,
             1 if cell(r, "final").lower() in ("yes", "true", "1") else 0,
             parse_money_cents(cell(r, "beg")), parse_money_cents(cell(r, "contrib")),
             parse_money_cents(cell(r, "ni")), parse_money_cents(cell(r, "other")),
             parse_money_cents(cell(r, "dist")), parse_money_cents(cell(r, "cashdist")),
             parse_money_cents(cell(r, "end")), endpct,
             name_to_vehicle.get(company.strip().lower()) if company else None,
             doc_id, json.dumps(dict(zip(h, r)), separators=(",", ":"))))
        n += 1
    return n


def load_documents(conn, docs_dir: Path) -> None:
    """Parse downloaded tax documents in docs_dir into tax_documents +
    k1_capital_accounts. Idempotent by content sha: a re-downloaded, updated
    K-1 CSV (e.g. an incomplete year that gained more K-1s) re-parses and
    upserts; an unchanged file is skipped. PDFs are recorded for provenance
    only."""
    docs_dir = Path(docs_dir)
    if not docs_dir.is_dir():
        return
    name_to_vehicle = _vehicle_name_map(conn)
    n_docs = n_k1 = 0
    for path in sorted(docs_dir.iterdir()):
        if not path.is_file() or path.suffix.lower() not in (".csv", ".pdf"):
            continue
        low = path.name.lower()
        is_k1 = "schedule k-1" in low
        ym = _YEAR_RE.search(path.name)
        year = int(ym.group(1)) if ym else None
        status = "estimate_provided" if "estimate provided" in low else "complete"
        # period: 'Q1'..'Q4' from an explicit Qn or a quarter-end date.
        pm = re.search(r"\bQ([1-4])\b", path.name)
        if pm:
            period = f"Q{pm.group(1)}"
        else:
            dm = re.search(r"\b20\d{2}-(\d{2})-\d{2}\b", path.name)
            period = ({"03": "Q1", "06": "Q2", "09": "Q3", "12": "Q4"}
                      .get(dm.group(1)) if dm else None)
        doc_type = "k1_packet" if is_k1 else "financial_report"
        suffix = path.suffix.lower().lstrip(".")
        if is_k1:
            # stable per (year, suffix) so an estimate->complete update of the
            # same year reuses (and re-parses into) the same row.
            doc_id = f"k1_packet_{year}_{suffix}"
        else:
            # financials: one row per file (year+period can still collide, e.g.
            # regular vs audited Q4) -> disambiguate by filename.
            disc = hashlib.sha256(path.name.encode()).hexdigest()[:8]
            doc_id = f"financial_report_{year}_{period or 'NA'}_{disc}"
        sha = hashlib.sha256(path.read_bytes()).hexdigest()
        prior = conn.execute(
            "SELECT content_sha256 FROM tax_documents WHERE document_id=?",
            (doc_id,)).fetchone()

        conn.execute("BEGIN")
        try:
            conn.execute(
                "INSERT OR REPLACE INTO tax_documents "
                "(document_id, doc_type, tax_year, period, document_status, "
                " filename, content_sha256, retrieved_at, payload) "
                "VALUES (?,?,?,?,?,?,?,strftime('%s','now'),?)",
                (doc_id, doc_type, year, period, status, path.name, sha, None))
            if is_k1 and suffix == "csv" and (prior is None or prior[0] != sha):
                n_k1 += _parse_k1_csv(conn, path, year, doc_id, name_to_vehicle)
            conn.execute("COMMIT")
            n_docs += 1
        except Exception:
            conn.execute("ROLLBACK")
            raise
    if n_docs:
        log.info("documents: %d file(s); k1_capital_accounts rows now %d",
                 n_docs, conn.execute("SELECT COUNT(*) FROM k1_capital_accounts").fetchone()[0])


def feed_k1_positions(conn) -> None:
    """Emit dated Dec-31 tax-basis valuation snapshots from k1_capital_accounts
    into the positions history — populating the separate
    `tax_basis_capital_minor` column (FMV columns stay NULL, never mixed).

    Each K-1 SPV is paired to a current GraphQL position: by company name,
    and for the few multi-SPV companies by investment year + cumulative
    contribution amount (each SPV's is distinct). Exited SPVs that have no
    current position aren't fed here — they remain in k1_capital_accounts.
    Idempotent: re-runs upsert the same (Dec-31, position) rows.
    """
    # Current GraphQL positions (latest non-tax-basis row) per company.
    pos_by_company = {}
    pid_to_vehicle = {}
    for pid, name, inv_date, contributed, vehicle in conn.execute(
            "SELECT p.position_external_id, v.name, p.investment_date, p.contributed_minor, "
            "       p.vehicle_external_id "
            "FROM positions p JOIN ("
            "  SELECT position_external_id, MAX(snapshot_at) m FROM positions "
            "  WHERE tax_basis_capital_minor IS NULL GROUP BY position_external_id) x "
            "ON x.position_external_id=p.position_external_id AND x.m=p.snapshot_at "
            "LEFT JOIN vehicles v ON v.vehicle_external_id=p.vehicle_external_id").fetchall():
        yr = (datetime.datetime.fromtimestamp(inv_date, datetime.timezone.utc).year
              if inv_date else None)
        pid_to_vehicle[pid] = vehicle
        pos_by_company.setdefault((name or "").strip().lower(), []).append(
            {"pid": pid, "year": yr, "contrib": contributed or 0})

    # K-1 SPVs aggregated per company (first contribution year + lifetime
    # contributions + whether it ever filed a final K-1).
    spv_by_company = {}
    for company, fund, year, contrib, final in conn.execute(
            "SELECT portfolio_company, fund_name, tax_year, contributions_minor, final_k1 "
            "FROM k1_capital_accounts").fetchall():
        ck = (company or "").strip().lower()
        d = spv_by_company.setdefault(ck, {}).setdefault(
            fund, {"first_year": None, "total": 0, "final": 0})
        if contrib:
            d["total"] += contrib
            d["first_year"] = year if d["first_year"] is None else min(d["first_year"], year)
        if final:
            d["final"] = 1

    # Greedy pairing fund_name -> position_external_id within each company.
    fund_to_pid = {}
    for ck, funds in spv_by_company.items():
        avail = dict(funds)
        for p in sorted(pos_by_company.get(ck, []), key=lambda x: -(x["contrib"] or 0)):
            best, best_s = None, None
            for fn, d in avail.items():
                # year mismatch dominates (huge weight), then contribution gap
                ys = 0 if d["first_year"] == p["year"] else abs((d["first_year"] or 0) - (p["year"] or 0)) * 10**15
                s = ys + abs((d["total"] or 0) - (p["contrib"] or 0))
                if best_s is None or s < best_s:
                    best, best_s = fn, s
            if best is not None:
                fund_to_pid[best] = p["pid"]
                del avail[best]

    n = 0
    conn.execute("BEGIN")
    try:
        for fund, year, ending, final in conn.execute(
                "SELECT fund_name, tax_year, ending_capital_minor, final_k1 "
                "FROM k1_capital_accounts WHERE ending_capital_minor IS NOT NULL").fetchall():
            pid = fund_to_pid.get(fund)
            if not pid or not year:
                continue
            vid = pid_to_vehicle.get(pid)
            snap = int(datetime.datetime(year, 12, 31, tzinfo=datetime.timezone.utc).timestamp())
            conn.execute(
                "INSERT INTO positions (snapshot_at, position_external_id, vehicle_external_id, "
                " status, currency, tax_basis_capital_minor) VALUES (?,?,?,?,?,?) "
                "ON CONFLICT(snapshot_at, position_external_id) DO UPDATE SET "
                " tax_basis_capital_minor=excluded.tax_basis_capital_minor, "
                " status=COALESCE(excluded.status, positions.status)",
                (snap, pid, vid, "exited" if final else None, "USD", ending))
            n += 1
        conn.execute("COMMIT")
    except Exception:
        conn.execute("ROLLBACK")
        raise
    if n:
        log.info("k1 tax-basis snapshots fed into positions: %d (paired SPVs: %d)",
                 n, len(fund_to_pid))


def money(m):
    """(minor_units, currency) from a GraphQL `{fractional, currency}`."""
    if not isinstance(m, dict):
        return None, None
    return m.get("fractional"), m.get("currency")


def _by_op(captures: list[dict]) -> dict[str, list[dict]]:
    out: dict[str, list[dict]] = {}
    for c in captures:
        out.setdefault(c.get("op"), []).append(c)
    return out


def _invest(data) -> dict:
    return ((data or {}).get("invest") or {}) if isinstance(data, dict) else {}


def load_snapshot(conn, snapshot_at: int, run_dir: Path) -> None:
    cap_path = run_dir / "captures.jsonl"
    if not cap_path.is_file():
        log.warning("%s: no captures.jsonl, skipping", run_dir.name)
        return
    captures = [json.loads(l) for l in cap_path.read_text().splitlines() if l.strip()]
    ops = _by_op(captures)

    # invest account slug — from any captured op's variables
    acct_slug = None
    for c in captures:
        v = c.get("variables") or {}
        acct_slug = acct_slug or v.get("investAccountSlug")

    conn.execute("BEGIN")
    try:
        # --- positions: union edges across all PositionsTableQuery pages ---
        nodes: dict[str, dict] = {}
        for c in ops.get("PositionsTableQuery", []):
            conn_pos = (_invest(c.get("data")).get("portfolio", {}) or {}).get("positions") or {}
            for e in conn_pos.get("edges", []):
                n = e.get("node") or {}
                if n.get("id"):
                    nodes[n["id"]] = n
        # Change-based history: write a positions row only when a position's
        # value-affecting fields differ from its latest prior snapshot, so
        # `positions` accumulates one row per *change* — a per-investment
        # valuation history — rather than re-dumping every position each run.
        # (To read holdings as-of a date: take each position's latest snapshot
        # <= date and drop the EXITED ones.)
        value_cols = ("status", "commitment_minor", "contributed_minor",
                      "investment_minor", "realized_minor", "recycled_minor",
                      "unrealized_minor", "total_value_minor", "tvpi")
        prior = {}
        for r in conn.execute(
            "SELECT p.position_external_id, " + ", ".join("p." + c for c in value_cols) + " "
            "FROM positions p JOIN ("
            "  SELECT position_external_id, MAX(snapshot_at) m FROM positions "
            "  WHERE snapshot_at < ? AND tax_basis_capital_minor IS NULL "
            "  GROUP BY position_external_id) x "
            "ON x.position_external_id = p.position_external_id AND x.m = p.snapshot_at",
            (snapshot_at,),
        ).fetchall():
            prior[r[0]] = tuple(r[1:])

        n_pos_written = 0
        for nid, n in nodes.items():
            guid = n.get("investableGuid")
            commit_m, cur = money(n.get("commitmentAmount"))
            contrib_m, cur2 = money(n.get("contributedAmount"))
            invest_m, _ = money(n.get("investmentAmount"))
            real_m, _ = money(n.get("realizedValue"))
            recyc_m, _ = money(n.get("recycledValue"))
            unreal_m, _ = money(n.get("unrealizedValue"))
            total_m, _ = money(n.get("totalValue"))
            currency = cur or cur2
            # Vehicle dimension is upserted every run (keeps last_seen_at
            # current even when the position itself didn't change).
            if guid:
                # investableGuid suffix discriminates the vehicle type:
                # '-f' = multi-company fund, else single-company SPV / RUV.
                kind = "fund" if guid.endswith("-f") else "spv"
                conn.execute(
                    "INSERT INTO vehicles "
                    "(vehicle_external_id, name, avatar_url, kind, first_seen_at, last_seen_at, payload) "
                    "VALUES (?,?,?,?,?,?,?) "
                    "ON CONFLICT(vehicle_external_id) DO UPDATE SET "
                    "name=excluded.name, avatar_url=excluded.avatar_url, kind=excluded.kind, "
                    "first_seen_at=min(vehicles.first_seen_at, excluded.first_seen_at), "
                    "last_seen_at=max(vehicles.last_seen_at, excluded.last_seen_at)",
                    (guid, n.get("investableName"), n.get("investableAvatarUrl"), kind,
                     snapshot_at, snapshot_at, None),
                )
            value_tuple = (n.get("status"), commit_m, contrib_m, invest_m,
                           real_m, recyc_m, unreal_m, total_m, n.get("tvpi"))
            if prior.get(nid) == value_tuple:
                continue  # unchanged — don't store a redundant snapshot row
            conn.execute(
                "INSERT OR REPLACE INTO positions "
                "(snapshot_at, position_external_id, vehicle_external_id, status, status_label, "
                " currency, commitment_minor, contributed_minor, investment_minor, realized_minor, "
                " recycled_minor, unrealized_minor, total_value_minor, tvpi, investment_date, "
                " fc_id, is_online, has_non_standard_reporting, payload) "
                "VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                (snapshot_at, nid, guid, n.get("status"), n.get("statusLabel"), currency,
                 commit_m, contrib_m, invest_m, real_m, recyc_m, unreal_m, total_m,
                 n.get("tvpi"), n.get("investmentDate"), n.get("fcId"),
                 1 if n.get("isOnline") else 0,
                 1 if n.get("hasNonStandardReporting") else 0,
                 json.dumps(n, separators=(",", ":"))),
            )
            n_pos_written += 1

        # --- portfolio summary ---
        pdq = ops.get("PortfolioDashboardQuery")
        if pdq:
            s = ((_invest(pdq[0].get("data")).get("portfolio", {}) or {}).get("summary")) or {}
            ci, cur = money(s.get("totalCommittedAmount"))
            co, _ = money(s.get("totalContributedAmount"))
            iv, cur2 = money(s.get("totalInvestedAmount"))
            re, _ = money(s.get("totalRealizedValue"))
            un, _ = money(s.get("totalUnrealizedValue"))
            tv, _ = money(s.get("totalValue"))
            conn.execute(
                "INSERT OR REPLACE INTO portfolio_summary "
                "(snapshot_at, invest_account_slug, currency, data_date, total_committed_minor, "
                " total_contributed_minor, total_invested_minor, total_realized_minor, "
                " total_unrealized_minor, total_value_minor, irr, tvpi, dpi, total_funds_count, "
                " total_investments_count, total_startups_count, payload) "
                "VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                (snapshot_at, acct_slug, cur or cur2, s.get("dataDate"), ci, co, iv, re, un, tv,
                 s.get("irr"), s.get("tvpi"), s.get("dpi"), s.get("totalFundsCount"),
                 s.get("totalInvestmentsCount"), s.get("totalStartupsCount"),
                 json.dumps(s, separators=(",", ":"))),
            )

            # --- portfolio NAV time series (latest-snapshot-wins per date) ---
            for p in s.get("timeSeries") or []:
                y, mo, d = p.get("year"), p.get("month"), p.get("day")
                if not (y and mo and d):
                    continue
                as_of = f"{int(y):04d}-{int(mo):02d}-{int(d):02d}"
                tv, tcur = money(p.get("totalValue"))
                ti, _ = money(p.get("totalInvestedAmount"))
                rz, _ = money(p.get("realizedValue"))
                uz, _ = money(p.get("unrealizedValue"))
                oc, _ = money(p.get("offlineValueChange"))
                conn.execute(
                    "INSERT INTO portfolio_timeseries "
                    "(invest_account_slug, as_of_date, currency, total_value_minor, "
                    " total_invested_minor, realized_minor, unrealized_minor, "
                    " offline_change_minor, is_approximate, snapshot_at) "
                    "VALUES (?,?,?,?,?,?,?,?,?,?) "
                    "ON CONFLICT(invest_account_slug, as_of_date) DO UPDATE SET "
                    "currency=excluded.currency, total_value_minor=excluded.total_value_minor, "
                    "total_invested_minor=excluded.total_invested_minor, "
                    "realized_minor=excluded.realized_minor, "
                    "unrealized_minor=excluded.unrealized_minor, "
                    "offline_change_minor=excluded.offline_change_minor, "
                    "is_approximate=excluded.is_approximate, snapshot_at=excluded.snapshot_at "
                    "WHERE excluded.snapshot_at >= portfolio_timeseries.snapshot_at",
                    (acct_slug, as_of, tcur, tv, ti, rz, uz, oc,
                     1 if p.get("isApproximate") else 0, snapshot_at),
                )

        # --- open commitments ---
        oiq = ops.get("OpenInvestmentsQuery")
        n_commit = 0
        if oiq:
            for o in _invest(oiq[0].get("data")).get("openInvestments", []) or []:
                opp = o.get("opportunity") or {}
                synd = opp.get("syndicate") or {}
                cm, cur = money(o.get("commitmentAmount"))
                pm, _ = money(o.get("paymentAmount"))
                rm, _ = money(o.get("remainingAmountNeededToFund"))
                conn.execute(
                    "INSERT OR REPLACE INTO commitments "
                    "(snapshot_at, commitment_external_id, name, investable_slug, syndicate_name, "
                    " syndicate_slug, opportunity_type, state, currency, commitment_minor, "
                    " payment_minor, remaining_to_fund_minor, close_date, funding_deadline, "
                    " needs_to_wire, needs_to_sign, payload) "
                    "VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                    (snapshot_at, o.get("id"), opp.get("investableName"), opp.get("investableSlug"),
                     synd.get("name"), synd.get("slug"), opp.get("type"), o.get("state"), cur,
                     cm, pm, rm, opp.get("closeDate"), opp.get("fundingDeadlineDate"),
                     1 if o.get("needsToWire") else 0, 1 if o.get("needsToSign") else 0,
                     json.dumps(o, separators=(",", ":"))),
                )
                n_commit += 1

        manifest = (run_dir / "run.json")
        conn.execute(
            "INSERT OR REPLACE INTO dump_runs (snapshot_at, invest_account_slug, payload) "
            "VALUES (?,?,?)",
            (snapshot_at, acct_slug,
             manifest.read_text() if manifest.is_file() else None),
        )
        conn.execute("COMMIT")
    except Exception:
        conn.execute("ROLLBACK")
        raise
    log.info("%s: positions %d changed / %d seen, %d vehicle(s), %d commitment(s)",
             run_dir.name, n_pos_written, len(nodes), conn.execute(
                 "SELECT COUNT(*) FROM vehicles").fetchone()[0], n_commit)


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument("--dest", type=Path, default=DEFAULT_DEST,
                   help="Bronze root to ingest from. Default: %(default)s.")
    p.add_argument("--db", type=Path, default=DEFAULT_DB,
                   help="Silver SQLite DB path. Default: %(default)s.")
    p.add_argument("--documents-dir", type=Path, default=DEFAULT_DOCS,
                   help="Dir of downloaded tax documents (K-1 CSV/PDF, financial "
                        "statements) to parse. Default: %(default)s.")
    p.add_argument("--force", action="store_true",
                   help="Re-load snapshots already recorded in dump_runs.")
    cli.add_common_args(p)
    return p.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)

    conn = silver.open_db(args.db)
    silver.apply_migrations(conn, MIGRATIONS)
    already = silver.loaded_snapshots(conn)

    n = 0
    for run_dir in bronze.iter_run_dirs(args.dest):
        try:
            snap = bronze.parse_run_ts(run_dir.name)
        except ValueError:
            continue
        if snap in already and not args.force:
            log.debug("skip already-loaded %s", run_dir.name)
            continue
        load_snapshot(conn, snap, run_dir)
        n += 1
    load_documents(conn, args.documents_dir)
    feed_k1_positions(conn)
    conn.close()
    log.info("loaded %d snapshot(s) into %s", n, args.db)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
