#!/usr/bin/env python3
"""angellist silver loader.

Parses the venture GraphQL captured by download.py (bronze `captures.jsonl`)
plus the downloaded Schedule K-1 CSVs into the source-shaped SQLite silver DB
— the input contract to the gold engine.

Model (see migrations/0005 + DESIGN.md): immutable per-investment identity
lives in `offerings` (one row per position / SPV stake); the per-position
valuation TIME SERIES lives in `position_snapshots` — one row per capital
event, stamped at the EVENT date with a collector-computed mark. The
collector replays each position's timeline; the gold adapter only
forward-fills. Holdings as of day D = each position's latest snapshot with
as_of_date <= D, dropping the is_open=0 (exited) ones.

Tables:
  offerings           immutable per-position identity (company, SPV legal
                      name + EIN, investment date)
  position_snapshots  per-position valuation events: 'investment' (cost, at
                      the investment date), 'statement' (annual K-1 tax-basis
                      NAV, at year-end), 'valuation' (current portal FMV, at
                      the portfolio data date)
  vehicles            one row per company (investableGuid)
  k1_capital_accounts per (tax_year, SPV) capital-account analysis (K-1 CSV)
  tax_documents       downloaded-document provenance + completeness
  portfolio_summary   per-snapshot dashboard totals + IRR/TVPI/DPI
  portfolio_timeseries  ~monthly NAV history, latest-snapshot-wins
  commitments         per-snapshot open (unfunded) commitments
  dump_runs           per-snapshot bookkeeping

Money: GraphQL returns `{currency, fractional}` where `fractional` is the
amount in minor units (cents); K-1 CSV money cells are dollars. Both are
stored as minor-unit integers; the gold adapter scales.
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

DEFAULT_BRONZE_DIR = Path("/data")

_YEAR_RE = re.compile(r"\b(20\d{2})\b")


# --------------------------------------------------------------------------
# money helpers
# --------------------------------------------------------------------------

def money(m):
    """(minor_units, currency) from a GraphQL `{fractional, currency}`."""
    if not isinstance(m, dict):
        return None, None
    return m.get("fractional"), m.get("currency")


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


def _year_end(year) -> int:
    """Unix seconds for Dec-31 (UTC midnight) of `year`."""
    return int(datetime.datetime(int(year), 12, 31, tzinfo=datetime.timezone.utc).timestamp())


def _year_of(unix_seconds):
    if not unix_seconds:
        return None
    return datetime.datetime.fromtimestamp(unix_seconds, datetime.timezone.utc).year


# --------------------------------------------------------------------------
# position_snapshots writer
# --------------------------------------------------------------------------

def put_snapshot(conn, pid, as_of, event_type, currency, *, market, basis,
                 contributed, distributions, is_open, status, snapshot_at):
    """Upsert one per-position event into position_snapshots, keyed by
    (position, as_of_date). Same-dated re-loads overwrite cleanly."""
    if not as_of:
        return
    conn.execute(
        "INSERT OR REPLACE INTO position_snapshots "
        "(position_external_id, as_of_date, event_type, status, is_open, currency, "
        " market_value_minor, valuation_basis, contributed_minor, distributions_minor, "
        " snapshot_at, payload) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
        (pid, int(as_of), event_type, status, 1 if is_open else 0, currency,
         market, basis, contributed, distributions, int(snapshot_at), None))


# --------------------------------------------------------------------------
# GraphQL captures -> offerings + current valuation/investment events
# --------------------------------------------------------------------------

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

    acct_slug = None
    for c in captures:
        v = c.get("variables") or {}
        acct_slug = acct_slug or v.get("investAccountSlug")

    conn.execute("BEGIN")
    try:
        # Portfolio summary first — its dataDate is AngelList's "as of" for
        # the current marks, and dates the current valuation event.
        pdq = ops.get("PortfolioDashboardQuery")
        summary = (((_invest(pdq[0].get("data")).get("portfolio", {}) or {})
                    .get("summary")) or {}) if pdq else {}
        data_date = summary.get("dataDate") or snapshot_at

        # --- positions: union edges across all PositionsTableQuery pages ---
        nodes: dict[str, dict] = {}
        for c in ops.get("PositionsTableQuery", []):
            conn_pos = (_invest(c.get("data")).get("portfolio", {}) or {}).get("positions") or {}
            for e in conn_pos.get("edges", []):
                n = e.get("node") or {}
                if n.get("id"):
                    nodes[n["id"]] = n

        for nid, n in nodes.items():
            guid = n.get("investableGuid")
            contrib_m, cur = money(n.get("contributedAmount"))
            real_m, _ = money(n.get("realizedValue"))
            total_m, curt = money(n.get("totalValue"))
            currency = cur or curt or "USD"
            inv_date = n.get("investmentDate")
            status = n.get("status")
            kind = None
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
            # offering: immutable per-position identity (fund_name / fund_tax_id
            # are filled later from the linked K-1).
            conn.execute(
                "INSERT INTO offerings "
                "(position_external_id, vehicle_external_id, kind, company_name, "
                " investment_date, currency, first_seen_at, last_seen_at, payload) "
                "VALUES (?,?,?,?,?,?,?,?,?) "
                "ON CONFLICT(position_external_id) DO UPDATE SET "
                " vehicle_external_id=excluded.vehicle_external_id, kind=excluded.kind, "
                " company_name=excluded.company_name, investment_date=excluded.investment_date, "
                " currency=excluded.currency, "
                " first_seen_at=min(offerings.first_seen_at, excluded.first_seen_at), "
                " last_seen_at=max(offerings.last_seen_at, excluded.last_seen_at), "
                " payload=excluded.payload",
                (nid, guid, kind, n.get("investableName"), inv_date, currency,
                 snapshot_at, snapshot_at, json.dumps(n, separators=(",", ":"))),
            )

            # Investment event (original capital, at the investment date) — a
            # cost data point before the first K-1 statement. Nothing is
            # distributed at inception.
            if inv_date:
                put_snapshot(conn, nid, inv_date, "investment", currency,
                             market=contrib_m, basis="cost", contributed=contrib_m,
                             distributions=None, is_open=True, status=status,
                             snapshot_at=snapshot_at)
            # Current valuation event (FMV, at the portfolio data date).
            mv = total_m if total_m is not None else contrib_m
            put_snapshot(conn, nid, data_date, "valuation", currency,
                         market=mv, basis="fmv" if total_m is not None else "cost",
                         contributed=contrib_m, distributions=real_m,
                         is_open=True, status=status, snapshot_at=snapshot_at)

        # --- portfolio summary + NAV time series ---
        if pdq:
            s = summary
            ci, cur = money(s.get("totalCommittedAmount"))
            co, _ = money(s.get("totalContributedAmount"))
            iv, cur2 = money(s.get("totalInvestedAmount"))
            rv, _ = money(s.get("totalRealizedValue"))
            un, _ = money(s.get("totalUnrealizedValue"))
            tv, _ = money(s.get("totalValue"))
            conn.execute(
                "INSERT OR REPLACE INTO portfolio_summary "
                "(snapshot_at, invest_account_slug, currency, data_date, total_committed_minor, "
                " total_contributed_minor, total_invested_minor, total_realized_minor, "
                " total_unrealized_minor, total_value_minor, irr, tvpi, dpi, total_funds_count, "
                " total_investments_count, total_startups_count, payload) "
                "VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                (snapshot_at, acct_slug, cur or cur2, s.get("dataDate"), ci, co, iv, rv, un, tv,
                 s.get("irr"), s.get("tvpi"), s.get("dpi"), s.get("totalFundsCount"),
                 s.get("totalInvestmentsCount"), s.get("totalStartupsCount"),
                 json.dumps(s, separators=(",", ":"))),
            )
            for p in s.get("timeSeries") or []:
                y, mo, d = p.get("year"), p.get("month"), p.get("day")
                if not (y and mo and d):
                    continue
                as_of = f"{int(y):04d}-{int(mo):02d}-{int(d):02d}"
                tsv, tcur = money(p.get("totalValue"))
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
                    (acct_slug, as_of, tcur, tsv, ti, rz, uz, oc,
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

        n_fund = load_funding(conn, captures, snapshot_at)

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
    log.info("%s: %d position(s) / offering(s), %d vehicle(s), %d commitment(s), %d funding tx",
             run_dir.name, len(nodes),
             conn.execute("SELECT COUNT(*) FROM vehicles").fetchone()[0], n_commit, n_fund)


def load_funding(conn, captures, snapshot_at: int) -> int:
    """Parse InvestmentEntityQuery into the funding-account cash ledger:
    `funding_accounts` (current cash balance) + `funding_transactions` (the
    dated, SIGNED deposits / withdrawals / investments / disbursements /
    refunds). This is the real dated cash-flow source; it reconciles exactly
    to the balance. Idempotent by transaction id."""
    entity = None
    for c in captures:
        if c.get("op") == "InvestmentEntityQuery":
            entity = _invest(c.get("data")).get("investmentEntity") or entity
    if not entity:
        return 0
    eid = entity.get("entityId") or entity.get("id")
    bal_m, bal_cur = money(entity.get("balance"))
    conn.execute(
        "INSERT OR REPLACE INTO funding_accounts "
        "(funding_account_external_id, slug_name, legal_name, currency, balance_minor, "
        " snapshot_at, payload) VALUES (?,?,?,?,?,?,?)",
        (eid, entity.get("slugName"), entity.get("legalName"), bal_cur, bal_m, snapshot_at, None))
    n = 0
    for t in entity.get("transactions") or []:
        if not t.get("id") or t.get("date") is None:
            continue
        amt_m, cur = money(t.get("amount"))
        rb_m, _ = money(t.get("balance"))
        conn.execute(
            "INSERT OR REPLACE INTO funding_transactions "
            "(transaction_external_id, funding_account_external_id, occurred_at, type, "
            " amount_minor, currency, running_balance_minor, syndicate_name, description, "
            " snapshot_at, payload) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
            (t.get("id"), eid, t.get("date"), t.get("type"), amt_m, cur, rb_m,
             t.get("syndicateName"), t.get("description"), snapshot_at,
             json.dumps(t, separators=(",", ":"))))
        n += 1
    return n


# --------------------------------------------------------------------------
# K-1 CSV / tax documents -> k1_capital_accounts + tax_documents
# --------------------------------------------------------------------------

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
    n_docs = 0
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
                _parse_k1_csv(conn, path, year, doc_id, name_to_vehicle)
            conn.execute("COMMIT")
            n_docs += 1
        except Exception:
            conn.execute("ROLLBACK")
            raise
    if n_docs:
        log.info("documents: %d file(s); k1_capital_accounts rows now %d",
                 n_docs, conn.execute("SELECT COUNT(*) FROM k1_capital_accounts").fetchone()[0])


# --------------------------------------------------------------------------
# K-1 statements -> position_snapshots (dated tax-basis valuation events)
# --------------------------------------------------------------------------

def _pair_k1_to_positions(conn):
    """Pair each K-1 SPV (fund_name) to a position. By company name, and for
    the few multi-SPV companies by investment year + cumulative contribution
    amount (each SPV's is distinct). Returns {fund_name: position_external_id}."""
    # Positions per company, with their latest contributed + investment year.
    pos_by_company = {}
    for pid, company, inv_date, contributed in conn.execute(
            "SELECT o.position_external_id, o.company_name, o.investment_date, "
            "       (SELECT contributed_minor FROM position_snapshots s "
            "          WHERE s.position_external_id = o.position_external_id "
            "          ORDER BY as_of_date DESC LIMIT 1) "
            "  FROM offerings o").fetchall():
        pos_by_company.setdefault((company or "").strip().lower(), []).append(
            {"pid": pid, "year": _year_of(inv_date), "contrib": contributed or 0})

    # K-1 SPVs aggregated per company (first contribution year + lifetime
    # contributions).
    spv_by_company = {}
    for company, fund, year, contrib in conn.execute(
            "SELECT portfolio_company, fund_name, tax_year, contributions_minor "
            "FROM k1_capital_accounts").fetchall():
        ck = (company or "").strip().lower()
        d = spv_by_company.setdefault(ck, {}).setdefault(
            fund, {"first_year": None, "total": 0})
        if contrib:
            d["total"] += contrib
            d["first_year"] = year if d["first_year"] is None else min(d["first_year"], year)

    fund_to_pid = {}
    for ck, funds in spv_by_company.items():
        avail = dict(funds)
        for p in sorted(pos_by_company.get(ck, []), key=lambda x: -(x["contrib"] or 0)):
            best, best_s = None, None
            for fn, d in avail.items():
                ys = 0 if d["first_year"] == p["year"] else abs((d["first_year"] or 0) - (p["year"] or 0)) * 10**15
                s = ys + abs((d["total"] or 0) - (p["contrib"] or 0))
                if best_s is None or s < best_s:
                    best, best_s = fn, s
            if best is not None:
                fund_to_pid[best] = p["pid"]
                del avail[best]
    return fund_to_pid


def build_k1_statement_snapshots(conn) -> None:
    """For each K-1 SPV paired to a position, stamp the SPV's legal name + EIN
    onto the immutable offering, and emit one 'statement' position_snapshot
    per tax year at its Dec-31 — the tax-basis ending capital is the mark,
    cumulative contributions / distributions ride alongside. A final K-1
    marks the position exited (is_open=0)."""
    fund_to_pid = _pair_k1_to_positions(conn)
    if not fund_to_pid:
        return
    latest_run = conn.execute("SELECT MAX(snapshot_at) FROM dump_runs").fetchone()[0] or 0

    # Cumulative contributions / cash distributions per (fund, year).
    fund_years = {}
    for fund, year, contrib, cashdist, ending, final, ein in conn.execute(
            "SELECT fund_name, tax_year, contributions_minor, cash_distributions_minor, "
            "       ending_capital_minor, final_k1, fund_tax_id "
            "FROM k1_capital_accounts ORDER BY fund_name, tax_year").fetchall():
        fund_years.setdefault(fund, []).append(
            dict(year=year, contrib=contrib or 0, cashdist=cashdist or 0,
                 ending=ending, final=final, ein=ein))

    n = 0
    conn.execute("BEGIN")
    try:
        for fund, pid in fund_to_pid.items():
            cum_c = cum_d = 0
            ein = next((y["ein"] for y in fund_years.get(fund, []) if y["ein"]), None)
            # Stamp the SPV identity onto the immutable offering.
            conn.execute(
                "UPDATE offerings SET fund_name = ?, fund_tax_id = COALESCE(fund_tax_id, ?) "
                "WHERE position_external_id = ?", (fund, ein, pid))
            for y in fund_years.get(fund, []):
                cum_c += y["contrib"]
                cum_d += y["cashdist"]
                if y["ending"] is None or not y["year"]:
                    continue
                put_snapshot(conn, pid, _year_end(y["year"]), "statement", "USD",
                             market=y["ending"], basis="tax_basis",
                             contributed=cum_c or None, distributions=cum_d or None,
                             is_open=not y["final"],
                             status="exited" if y["final"] else None,
                             snapshot_at=latest_run)
                n += 1
        conn.execute("COMMIT")
    except Exception:
        conn.execute("ROLLBACK")
        raise
    if n:
        log.info("k1 statement events: %d (paired SPVs: %d)", n, len(fund_to_pid))


_PAREN_RE = re.compile(r"\s*\([^)]*\)")
_FUND_CO_PATTERNS = {
    "investment": re.compile(r"(?i)^\s*investment in (.+)$"),
    "refund": re.compile(r"(?i)^\s*refund (?:for|of) (.+?)(?:\s*\([^)]*\))?\s*$"),
    "disbursement": re.compile(r"(?i)^\s*disbursement\s*[-–]\s*(.+?)(?:\s+[-–]\s+|$)"),
}


def _norm_company(s: str) -> str:
    """Normalise a company name for matching: lowercase, drop parentheticals
    (e.g. a "(Series A)" suffix the funding ledger omits), drop dots/commas,
    collapse whitespace."""
    s = _PAREN_RE.sub("", (s or "").lower())
    s = re.sub(r"[.,]", "", s)
    return re.sub(r"\s+", " ", s).strip()


def _funding_company(ty: str, desc: str):
    """The company a funding transaction concerns, parsed from its description
    ("Investment in <company>", "Refund for <company> (…)", "Disbursement -
    <company> - …"). None for deposits / withdrawals / transfers."""
    pat = _FUND_CO_PATTERNS.get(ty)
    if not pat:
        return None
    m = pat.match(desc or "")
    return m.group(1).strip() if m else None


def link_funding_transactions(conn) -> None:
    """Link each funding transaction to the SPV/fund instrument it concerns.
    The company is parsed from the description and matched (normalised) to the
    current offerings:

      * exactly one current position -> link to it;
      * several current positions    -> a multi-SPV company (the description
        names only the company, not which SPV) -> pick the SPV whose
        investment date is closest to the transaction date (heuristic); if no
        invest dates are available, left account-level (NULL);
      * no current position          -> an EXITED investment: a thin offering
        is DERIVED FROM THE FUNDING LEDGER here (so the instrument exists for
        the transaction to link to), and the transaction links to it.

    External-bank deposits / withdrawals name no company and stay
    account-level. Returns nothing; logs the link / exited / multi-SPV tallies."""
    norm_to_pids: dict[str, set] = {}
    inv_date: dict[str, int] = {}
    for pid, name, idate in conn.execute(
            "SELECT position_external_id, company_name, investment_date FROM offerings "
            "WHERE company_name IS NOT NULL AND company_name <> ''"):
        norm_to_pids.setdefault(_norm_company(name), set()).add(pid)
        if idate is not None:
            inv_date[pid] = idate
    # bounded-substring patterns over the current companies, longest first so
    # the most specific name wins (a company name appears verbatim in every
    # description type, wherever it sits — so substring beats positional
    # parsing for matching KNOWN companies).
    pats = [(co, re.compile(r"(?<![a-z0-9])" + re.escape(co)))
            for co in sorted((c for c in norm_to_pids if c), key=len, reverse=True)]

    rows = conn.execute(
        "SELECT transaction_external_id, type, COALESCE(description, ''), occurred_at "
        "FROM funding_transactions "
        "WHERE type IN ('investment', 'disbursement', 'refund')").fetchall()
    n_link = n_exit = n_date = 0
    multi: dict[str, int] = {}
    exited: dict[str, str] = {}  # normalised company -> synthetic offering id
    conn.execute("BEGIN")
    try:
        for txid, ty, desc, occurred in rows:
            d = _norm_company(desc)  # normalise both sides (dots/parens) before matching
            hit = next((co for co, pat in pats if pat.search(d)), None)
            if hit is not None:
                pids = norm_to_pids[hit]
                if len(pids) == 1:
                    pid = next(iter(pids))
                else:
                    # multi-SPV company: the description names only the company,
                    # so pick the SPV whose investment date is closest to the
                    # transaction date (a contribution/refund settles around its
                    # deal's close). A heuristic: it holds while deal dates are
                    # well apart.
                    cand = [(p, inv_date[p]) for p in pids if p in inv_date]
                    if cand and occurred is not None:
                        pid = min(cand, key=lambda pi: abs(occurred - pi[1]))[0]
                        n_date += 1
                    else:
                        multi[hit] = multi.get(hit, 0) + 1
                        continue
            else:  # no current position — an EXITED investment; parse its name
                company = _funding_company(ty, desc)
                co = _norm_company(company) if company else ""
                if not co:
                    continue
                pid = exited.get(co)
                if pid is None:
                    pid = "funding:" + (re.sub(r"[^a-z0-9]+", "-", co).strip("-") or "unknown")
                    # kind defaults to 'spv': individual AngelList syndicate
                    # deals are single-company SPVs (no fund/spv signal here).
                    conn.execute(
                        "INSERT OR IGNORE INTO offerings "
                        "(position_external_id, kind, company_name, currency) "
                        "VALUES (?, 'spv', ?, 'USD')", (pid, company.strip()))
                    exited[co] = pid
                    n_exit += 1
            conn.execute(
                "UPDATE funding_transactions SET position_external_id = ? "
                "WHERE transaction_external_id = ?", (pid, txid))
            n_link += 1
        conn.execute("COMMIT")
    except Exception:
        conn.execute("ROLLBACK")
        raise
    log.info("funding tx linked: %d (exited instruments derived: %d; "
             "multi-SPV disambiguated by date: %d; still unlinked: %d tx across %d companies)",
             n_link, n_exit, n_date, sum(multi.values()), len(multi))


# --------------------------------------------------------------------------

def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument("--bronze-dir", type=Path, default=DEFAULT_BRONZE_DIR,
                   help="Bronze root to ingest from. Default: %(default)s.")
    p.add_argument("--silver-db", type=Path, default=None,
                   help="Silver SQLite DB path. DEFAULT: <bronze-dir>/angellist.db "
                        "— the bronze-root sibling of the run dirs (DESIGN.md).")
    p.add_argument("--documents-dir", type=Path, default=None,
                   help="Dir of downloaded tax documents (K-1 CSV/PDF, financial "
                        "statements) to parse. DEFAULT: "
                        "<bronze-dir>/angellist-documents — the bronze-root "
                        "sibling `download` saves into, so silver stays "
                        "reproducible from bronze alone. No-op when missing.")
    cli.add_standard_args(p, verb="load")
    return p.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)

    # The silver DB and the documents dir are bronze-ROOT siblings of the
    # timestamped run dirs (DESIGN.md), so their defaults derive from
    # --bronze-dir: scoping it to another tree scopes them too, and the
    # in-container default still lands on the /data paths the wrapper mounts.
    silver_db = (args.silver_db if args.silver_db is not None
                 else args.bronze_dir / "angellist.db")
    docs_dir = (args.documents_dir if args.documents_dir is not None
                else args.bronze_dir / "angellist-documents")

    # --force = delete the silver DB, then rebuild from all bronze (the
    # fleet-wide meaning). After a reset the DB is empty, so the
    # already-loaded skip below naturally re-ingests every run.
    if args.force:
        silver.reset(silver_db)
    conn = silver.open_db(silver_db)
    silver.apply_migrations(conn, MIGRATIONS)
    already = silver.loaded_snapshots(conn)

    n = 0
    for run_dir in bronze.iter_run_dirs(args.bronze_dir):
        try:
            snap = bronze.parse_run_ts(run_dir.name)
        except ValueError:
            continue
        if snap in already:
            log.debug("skip already-loaded %s", run_dir.name)
            continue
        load_snapshot(conn, snap, run_dir)
        n += 1
    load_documents(conn, docs_dir)
    build_k1_statement_snapshots(conn)
    link_funding_transactions(conn)
    conn.close()
    log.info("loaded %d snapshot(s) into %s", n, silver_db)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
