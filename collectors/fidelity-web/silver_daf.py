"""
fidelity-web silver: the Donor-Advised Fund.

The giving accounts, pool positions, grant and contribution events and
documents of a dump's `daf/` tree, and the completeness gate the retail
positions share with the pools.
"""

from __future__ import annotations

import hashlib
import json
import logging
import re

from collectorkit import bronze
from collectorkit.parse import iso_date_to_epoch

from silver_retail import (
    _ingest_document, normalize_payload, parse_decimal, ts_from_mdy,
)

log = logging.getLogger("fidelity-web.load")


# The portfolio grouping the DAF phase lands under. The retail
# portfolios key on the account-selector group label; the DAF bronze
# comes from the charitable API, not the selector, so the loader stamps
# this stable label (matching the selector's 'Fidelity Charitable®
# Giving' section) with kind='daf'.
DAF_PORTFOLIO_LABEL = "Fidelity Charitable® Giving"


# ============================================================
# Donor-Advised Fund (Fidelity Charitable) — §12
# ============================================================
#
# The DAF bronze under <dump>/daf/ is the charitable API's own JSON,
# not the retail CSV/HTML exports. It lands in the SAME silver tables
# as the retail data so the gold adapter composes uniformly:
#   * one portfolio row  (kind='daf')            per dump
#   * one accounts row   (management_style='automated')  per giving account
#   * positions rows      from the investment pools (instrument = poolId)
#   * transactions rows   from grants / contributions / gifts / pool
#                         exchanges / adjustments (stable id per source id)
#   * documents rows      from the statement / confirmation / tax-form PDFs
# The rich per-event fields the retail columns don't model live in the
# JSON `payload`. See DESIGN.md §12.


def _read_json_file(path):
    """Read + parse a JSON bronze file, or None if absent/unreadable."""
    if not path.is_file():
        return None
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (ValueError, OSError) as e:
        log.warning("DAF: could not read %s: %s", path.name, e)
        return None


def _daf_ts(value):
    """Parse a DAF date (ISO 'YYYY-MM-DD', ISO datetime, or US
    'MM/DD/YYYY') to Unix seconds UTC, or None."""
    if not value or not isinstance(value, str):
        return None
    return iso_date_to_epoch(value) or ts_from_mdy(value)


def _daf_first(rec, keys):
    """First present, non-empty value among ``keys`` in ``rec``."""
    for k in keys:
        v = rec.get(k)
        if v not in (None, "", "--"):
            return v
    return None


def _daf_activity_id(kind, account_ext, source_id, payload):
    """Deterministic transactions PK for a DAF event. Keyed on the
    source's own stable id (grantId, contributionId, …) when present so
    a re-download collapses onto one row; falls back to a hash of the
    account + normalized payload when the source offers no id."""
    basis = (f"{kind}:{account_ext}:{source_id}" if source_id
             else f"{kind}:{account_ext}:{normalize_payload(payload)}")
    return "daf-" + hashlib.sha256(basis.encode("utf-8")).hexdigest()[:28]


# One spec per DAF event file. ``unwrap`` names a nested object the row
# fields live under (grants wrap them in "grant"); ``sign`` fixes the
# amount direction (grants/gifts out = -1, contributions in = +1,
# exchanges/adjustments kept as-is = 0). Candidate key lists absorb
# field-name variation across the endpoints without over-fitting a
# single observed shape.
_DAF_EVENT_SPECS = (
    dict(file="grants.json", kind="GRANT", unwrap="grant", sign=-1,
         id_keys=("grantId",), amount_keys=("amount",),
         date_keys=("approvalDate", "settlementDate", "submitDate",
                    "creationDate", "updateDate"),
         instrument_keys=()),
    dict(file="contributions.json", kind="CONTRIBUTION", unwrap=None, sign=1,
         id_keys=("contributionId", "id"),
         amount_keys=("estimatedAmount", "fmv", "FMV", "netProceeds",
                      "amount"),
         date_keys=("receivedDate", "tradeDate", "settlementDate",
                    "submittedDate", "submitDate"),
         instrument_keys=("cusip", "CUSIP", "symbol", "Symbol")),
    dict(file="gifts.json", kind="GIFT", unwrap=None, sign=-1,
         id_keys=("id", "giftId"), amount_keys=("giftAmount", "amount"),
         date_keys=("processed", "processedDate", "date"),
         instrument_keys=()),
    dict(file="pool_exchanges.json", kind="EXCHANGE", unwrap=None, sign=0,
         id_keys=("id", "exchangeId"), amount_keys=("amount",),
         date_keys=("processDate", "submitDate", "date"),
         instrument_keys=()),
    dict(file="adjustments.json", kind="ADJUSTMENT", unwrap=None, sign=0,
         id_keys=("id", "adjustmentId"), amount_keys=("amount",),
         date_keys=("processed", "processedDate", "date"),
         instrument_keys=()),
)


def _daf_pool_count(dump_dir):
    """Count parseable pool entries across the dump's DAF account
    dirs, without touching silver — the completeness gate's DAF-side
    signal."""
    daf_dir = dump_dir / "daf"
    if not daf_dir.is_dir():
        return 0
    n = 0
    for acct_dir in daf_dir.iterdir():
        if not acct_dir.is_dir():
            continue
        pb = _read_json_file(acct_dir / "pool_balances.json")
        if isinstance(pb, list) and pb:
            n += len((pb[0] or {}).get("poolInfoList") or [])
    return n


def _positions_completeness_gate(dump_dir, run_meta, merged):
    """Decide whether this dump's positions may enter silver at all.
    Returns (skip, reason); (False, None) means the observation is
    complete and both retail and DAF pool rows load.

    A positions-bearing snapshot is read downstream (gold's per-source
    latest-snapshot anchor) as a COMPLETE holdings observation, so a
    dump that observed only one channel must contribute NO positions:
    stale beats partial, and the anchor stays on the last complete
    dump. Events, documents, and master data are unaffected (keyed
    rows, not snapshots). See DESIGN.md §12.3.

    Partiality signals:
      * mode='daf' — the retired DAF-only mode; such a legacy dump
        runs no retail phase, so it is partial by construction.
      * daf_results.status='error', or an error in its per_account
        map — the walk expected a DAF (the account selector
        enumerated one) but the charitable phase, or one giving
        account of it, failed; loading the rest alone would zero the
        missing pools.
      * retail rows parse to zero while the dump carries DAF pool
        rows and the roster says retail accounts were in scope — the
        retail export failed (or its format drifted); loading the
        pool alone would zero the retail holdings.
    """
    mode = (run_meta.get("cli_config") or {}).get("mode")
    if mode == "daf":
        return True, "legacy mode=daf dump covers only the DAF"
    daf_res = run_meta.get("daf_results")
    pool_rows = _daf_pool_count(dump_dir)
    if isinstance(daf_res, dict) and (merged or pool_rows) and (
            daf_res.get("status") == "error"
            or any(isinstance(r, dict) and r.get("status") == "error"
                   for r in (daf_res.get("per_account") or {}).values())):
        return True, "the DAF phase failed while other positions landed"
    if not merged and pool_rows and (run_meta.get("accounts_in_scope")
                                     or []):
        return True, ("retail positions parsed to zero rows while the "
                      "DAF pool landed")
    return False, None


def _load_daf(conn, snapshot_at, dump_dir, *, skip_positions=False):
    """Load the Donor-Advised Fund bronze (<dump>/daf/) into silver.
    Returns a per-table count dict (empty if the dump has no DAF).
    ``skip_positions`` (the completeness gate) suppresses the pool
    position rows while master / events / documents still load."""
    daf_dir = dump_dir / "daf"
    if not daf_dir.is_dir():
        return {}
    counts = {"portfolios": 0, "accounts": 0, "positions": 0,
              "transactions": 0, "documents": 0}
    portfolio_written = False
    for acct_dir in sorted(p for p in daf_dir.iterdir() if p.is_dir()):
        master = _read_json_file(acct_dir / "account.json")
        if not isinstance(master, dict):
            continue
        account_ext = _daf_account_id(master)
        if not account_ext:
            continue
        if not portfolio_written:
            conn.execute(
                "INSERT OR REPLACE INTO portfolios ("
                "snapshot_at, portfolio_external_id, kind, payload"
                ") VALUES (?, ?, 'daf', ?)",
                (snapshot_at, DAF_PORTFOLIO_LABEL,
                 normalize_payload({"source": "daf/accounts.json"})),
            )
            portfolio_written = True
            counts["portfolios"] = 1
        conn.execute(
            "INSERT OR REPLACE INTO accounts ("
            "snapshot_at, account_external_id, portfolio_external_id, "
            "nickname, payload, management_style"
            ") VALUES (?, ?, ?, ?, ?, 'automated')",
            (snapshot_at, account_ext, DAF_PORTFOLIO_LABEL,
             master.get("gaName"),
             normalize_payload({"source": "daf/account.json", **master})),
        )
        counts["accounts"] += 1
        if not skip_positions:
            counts["positions"] += _load_daf_pools(
                conn, snapshot_at, account_ext, acct_dir)
        counts["transactions"] += _load_daf_events(
            conn, snapshot_at, account_ext, acct_dir)
        counts["documents"] += _load_daf_documents(
            conn, snapshot_at, account_ext, acct_dir)
    return counts


def _load_daf_pools(conn, snapshot_at, account_ext, acct_dir):
    """Load the investment-pool positions from pool_balances.json. The
    latest date bucket's poolInfoList is the current holding snapshot;
    each pool is one positions row keyed on poolId."""
    pb = _read_json_file(acct_dir / "pool_balances.json")
    if not isinstance(pb, list) or not pb:
        return 0
    bucket = pb[0] or {}
    price_date = bucket.get("poolPriceDate")
    inserted = 0
    for pool in bucket.get("poolInfoList") or []:
        instrument_key = str(pool.get("poolId")
                             or pool.get("poolName") or "").strip()
        if not instrument_key:
            continue
        conn.execute(
            "INSERT OR REPLACE INTO positions ("
            "snapshot_at, account_external_id, instrument_key, "
            "description, quantity, last_price, current_value, "
            "type, currency, asset_class, is_core_position, payload"
            ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'USD', 'daf_pool', 0, ?)",
            (snapshot_at, account_ext, instrument_key,
             pool.get("poolName"),
             parse_decimal(pool.get("unitQuantity")),
             parse_decimal(pool.get("poolUnitPrice")),
             parse_decimal(pool.get("marketValue")),
             pool.get("poolCategory"),
             normalize_payload({"pool_price_date": price_date, **pool})),
        )
        inserted += 1
    return inserted


def _load_daf_events(conn, snapshot_at, account_ext, acct_dir):
    """Load grants / contributions / gifts / pool-exchanges / adjustments
    into the transactions table, one row per event, keyed on a stable
    per-source id (see _daf_activity_id)."""
    inserted = 0
    for spec in _DAF_EVENT_SPECS:
        path = acct_dir / spec["file"]
        items = _read_json_file(path)
        if not isinstance(items, list) or not items:
            continue
        source_sha = bronze.sha256_file(path)[0]
        for item in items:
            if not isinstance(item, dict):
                continue
            rec = item.get(spec["unwrap"]) if spec["unwrap"] else item
            if not isinstance(rec, dict):
                rec = item
            amount = parse_decimal(_daf_first(rec, spec["amount_keys"]))
            if amount is not None and spec["sign"]:
                amount = spec["sign"] * abs(amount)
            ts = _daf_ts(_daf_first(rec, spec["date_keys"])) or snapshot_at
            instr = _daf_first(rec, spec["instrument_keys"])
            source_id = _daf_first(rec, spec["id_keys"])
            activity_id = _daf_activity_id(
                spec["kind"], account_ext, source_id, item)
            conn.execute(
                "INSERT OR REPLACE INTO transactions ("
                "activity_id, timestamp, account_external_id, kind, "
                "instrument_key, amount, currency, source_sha256, payload"
                ") VALUES (?, ?, ?, ?, ?, ?, 'USD', ?, ?)",
                (activity_id, ts, account_ext, spec["kind"],
                 (str(instr).strip() or None) if instr else None,
                 amount, source_sha, normalize_payload(item)),
            )
            inserted += 1
    return inserted


# DAF document filename stems (download._daf_doc_stem): '<TYPE>_<date>',
# TYPE ∈ {STATEMENT, GRANT, CONTRIBUTION, FORM_8283}. The kind is
# namespaced 'daf_*' so DAF documents never mix with the retail
# statement / tax-form rows.
_DAF_DOC_KINDS = {
    "STATEMENT": "daf_statement",
    "FORM_8283": "daf_tax_form",
    "GRANT": "daf_grant_confirmation",
    "CONTRIBUTION": "daf_contribution_confirmation",
}
_DAF_DOC_STEM_RE = re.compile(
    r"^(STATEMENT|FORM_8283|GRANT|CONTRIBUTION)_(\d{4})")


def _classify_daf_pdf(filename, account_ext):
    """Kind + tax_year + account for a DAF documents/*.pdf, from the
    '<TYPE>_<YYYY>_...' stem download.py writes."""
    info = {"file_format": "pdf", "doc_kind": "daf_statement",
            "account_external_id": account_ext}
    m = _DAF_DOC_STEM_RE.match(filename)
    if m:
        info["doc_kind"] = _DAF_DOC_KINDS.get(m.group(1), "daf_statement")
        if info["doc_kind"] == "daf_tax_form":
            info["tax_year"] = int(m.group(2))
    return info


def _load_daf_documents(conn, snapshot_at, account_ext, acct_dir):
    """Index the DAF PDF archive into the documents table (deduped on
    content_sha256 like every other document)."""
    docs_dir = acct_dir / "documents"
    if not docs_dir.is_dir():
        return 0
    inserted = 0
    for pdf in sorted(docs_dir.glob("*.pdf")):
        inserted += _ingest_document(
            conn, snapshot_at, pdf,
            _classify_daf_pdf(pdf.name, account_ext))
    return inserted


def _daf_statement_pdf_candidates(dump_dir):
    """The DAF statement PDFs across a dump's giving-account dirs
    (``daf/<account_key>/documents/STATEMENT_*.pdf``), sorted for
    deterministic INSERT OR REPLACE ordering."""
    daf_dir = dump_dir / "daf"
    if not daf_dir.is_dir():
        return []
    return sorted(daf_dir.glob("*/documents/STATEMENT_*.pdf"))


def _daf_account_for_statement(path):
    """The giving-account id owning a statement PDF, from the
    ``account.json`` beside its documents dir; None when absent."""
    master = _read_json_file(path.parents[1] / "account.json")
    if not isinstance(master, dict):
        return None
    return _daf_account_id(master)


def _daf_account_id(master):
    """The giving-account id an ``account.json`` master names, or None."""
    aid = str(master.get("accountNbr") or master.get("accountNumber")
              or "").strip()
    return aid or None
