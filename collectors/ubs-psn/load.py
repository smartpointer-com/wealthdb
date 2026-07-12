#!/usr/bin/env python3
"""
ubs-psn silver loader.

Reads bronze dump directories (produced by download.py) and inserts them
into a SQLite silver database. Applies any pending schema migrations on
startup. Each dump is loaded atomically: a failure mid-load rolls back
to the prior state.

Usage:
    load.py --silver-db <path> --bronze-dir <path> [--relationship-id ID]

Each immediate subdirectory of <bronze-dir> whose name matches the
ubs-psn timestamp format (YYYYMMDDTHHMMSSZ) is considered a dump.
Already-loaded dumps (recorded in dump_runs) are skipped.

Currently loaded:
  - SDCL / SDCA / SDSA / SDPO / SDFI from ZMD.zip
  - TDFXR / TDFWD from ZME.zip (other TD* types are loaded if non-empty;
    empty <Data> sections are skipped)
  - MT535 holdings from ZAH.zip
  - MT537 pending securities from ZM5.zip
  - MT940 cash balances + cash_movement events from Z40.zip
  - MT515 trade_confirmation events from ZAG.zip
  - MT566 corporate_action_confirmation events from ZAN.zip

ZAY.zip (MT950) is intentionally not loaded — see migration 0001's
header. ZMH (MT536) and other MT types will be added when we have
samples.

Account identifiers are canonicalised at load time (since migration
0002): cash side uses IBAN everywhere (MT940 :25: is translated via
cash_accounts.payload.AcctId), safekeeping side uses the MT535
:97A::SAFE// / UBS AcctId form everywhere (load_sdsa picks AcctId
rather than the dashed ExtAcctId).
"""

from __future__ import annotations

import argparse
import logging
import re
import sqlite3
import sys
import xml.etree.ElementTree as ET
import zipfile
from datetime import datetime, timezone
from pathlib import Path

from collectorkit import bronze, cli, silver

log = logging.getLogger("ubs-load")

MIGRATIONS_DIR = Path(__file__).parent / "migrations"
SNAPSHOT_DIR_RE = re.compile(r"^(\d{8}T\d{6}Z)$")


# --------------------------------------------------------------------------
# Small helpers
# --------------------------------------------------------------------------

def parse_snapshot_at(dump_dir_name: str) -> int:
    """'20200101T000000Z' -> Unix seconds UTC."""
    m = SNAPSHOT_DIR_RE.match(dump_dir_name)
    if not m:
        raise ValueError(f"Not a snapshot directory name: {dump_dir_name!r}")
    return bronze.parse_run_ts(m.group(1))


def parse_yymmdd(s: str) -> int:
    """'YYMMDD' (e.g. '260511') -> Unix seconds UTC at 00:00:00.

    UBS PSN consistently uses 2-digit years; we map YY 00-69 -> 2000-2069
    and 70-99 -> 1970-1999. PSN doesn't ship data older than the contract,
    so this is safe.
    """
    yy = int(s[0:2]); mm = int(s[2:4]); dd = int(s[4:6])
    yyyy = 2000 + yy if yy < 70 else 1900 + yy
    return int(datetime(yyyy, mm, dd, tzinfo=timezone.utc).timestamp())


def canonical_json(obj) -> str:
    """Compact JSON, sorted keys. Used both for storage and for dedup compare.

    Delegates to the shared serializer with ``ascii=True`` (``ensure_ascii``)
    so payloads keep their historical byte encoding."""
    return silver.canonical_json(obj, ascii=True)


_FILENAME_DATE_RE = re.compile(r"^(\d{4})-(\d{2})-(\d{2})_")


def file_snapshot_at(fname: str) -> int | None:
    """Extract the 'YYYY-MM-DD_' prefix from a PSN filename and return
    Unix seconds at 00:00 UTC of that date. Returns None if absent.

    Every PSN-emitted file inside a dump zip is named
    '<YYYY-MM-DD>_<ZTYPE>_<...>.{xml,txt}' where the date is the as-of
    date of the data, NOT the dump-retrieval date. This is the natural
    `snapshot_at` value for the silver rows derived from that file —
    using it (rather than the dump-directory timestamp) lets a single
    dump correctly land multiple as-of dates when download.py was
    skipped for a day and the next dump arrives with a catch-up batch.
    """
    m = _FILENAME_DATE_RE.match(fname)
    if not m:
        return None
    return int(datetime(int(m.group(1)), int(m.group(2)), int(m.group(3)),
                        tzinfo=timezone.utc).timestamp())


# --------------------------------------------------------------------------
# Database / migrations
# --------------------------------------------------------------------------

# open_db + schema versioning + the migration runner live in
# collectorkit.silver. ubs-psn wraps each dump in `with conn:` for per-dump
# atomicity, so it shares silver's default-isolation opener (mkdir + connect +
# foreign_keys, no row_factory/WAL) rather than keeping its own copy.
open_db = silver.open_db_default_isolation


# --------------------------------------------------------------------------
# UBS XML helpers
# --------------------------------------------------------------------------

# Per-batch noise fields that change every nightly run even when content is
# semantically identical. Stripped for content-based dedup of slow-changing
# master data. Bronze keeps the originals on disk.
PSN_HEADER_NOISE = ("DWHMsgId", "MsqSeqNo", "CrtnDtTm")


def _strip_ns(tag: str) -> str:
    """Drop the {namespace} prefix from an etree element tag."""
    return re.sub(r"^\{[^}]+\}", "", tag)


def elem_to_dict(elem: ET.Element) -> dict:
    """Recursively convert an XML element into a nested dict/list/str.

    Each element becomes {tag: text_or_children}. Repeated child tags
    collapse into a list. Leaf text is stored as a string.
    """
    tag = _strip_ns(elem.tag)
    children = list(elem)
    if not children:
        return {tag: (elem.text or "").strip()}
    out: dict = {}
    for c in children:
        ctag = _strip_ns(c.tag)
        sub = elem_to_dict(c)[ctag]
        if ctag in out:
            if isinstance(out[ctag], list):
                out[ctag].append(sub)
            else:
                out[ctag] = [out[ctag], sub]
        else:
            out[ctag] = sub
    return {tag: out}


_WRAPPER_RE = re.compile(r"^(Clnt\w+Data|\w+RateData)$")


def parse_psn_xml(xml_bytes: bytes) -> tuple[str, list[ET.Element]]:
    """Return (type_code, entities) at the natural-entity level.

    `type_code` is the value of <TypeCd> in the header (e.g. 'SDCL', 'TDFXR').
    `entities` is the list of immediate children of <Data>, with one
    transparent auto-descent applied: if <Data> contains exactly one
    child whose tag matches `Clnt<X>Data` or `<X>RateData` (a per-client
    or per-feed wrapper), we descend into its children. This makes SDCA
    (<ClntCshAcctData>), SDSA (<ClntSfkData>), SDPO (<ClntPrtflCompData>),
    TDFWD (<ClntFwdCtrctData>), TDFXR (<ForeignExchangeRateData>), etc.
    look the same to loaders as SDFI/SDCL (no wrapper).

    Empty <Data> -> empty list.
    """
    root = ET.fromstring(xml_bytes)
    type_code = None
    data_elem = None
    for e in root.iter():
        t = _strip_ns(e.tag)
        if t == "TypeCd" and type_code is None:
            type_code = (e.text or "").strip()
        elif t == "Data":
            data_elem = e
            break
    if type_code is None:
        raise ValueError("PSN XML missing <TypeCd>")
    if data_elem is None:
        return type_code, []
    entities = list(data_elem)
    # One-level transparent descent into a per-client wrapper.
    if len(entities) == 1 and _WRAPPER_RE.match(_strip_ns(entities[0].tag)):
        entities = list(entities[0])
    return type_code, entities


def deep_find(d, key: str) -> str | None:
    """Depth-first search for the first leaf string under `key` anywhere
    in a nested dict/list parsed-XML structure. Returns None if missing.
    """
    if isinstance(d, dict):
        if key in d and isinstance(d[key], str):
            return d[key]
        for v in d.values():
            r = deep_find(v, key)
            if r is not None:
                return r
    elif isinstance(d, list):
        for v in d:
            r = deep_find(v, key)
            if r is not None:
                return r
    return None


def strip_header_noise(payload_obj):
    """Drop per-batch noise from the parsed PSN payload so dedup works."""
    if isinstance(payload_obj, dict):
        return {
            k: strip_header_noise(v)
            for k, v in payload_obj.items()
            if k not in PSN_HEADER_NOISE
        }
    if isinstance(payload_obj, list):
        return [strip_header_noise(v) for v in payload_obj]
    return payload_obj


# --------------------------------------------------------------------------
# SWIFT MT helpers (block-tag format)
# --------------------------------------------------------------------------

_MT_BLOCK4_RE = re.compile(r"\{4:\s*(.*?)\s*-\}", re.S)
_BAL_RE = re.compile(r"^([CD])R?(\d{6})([A-Z]{3})([0-9,]+)$")
_TAG_LINE_RE = re.compile(r"^:([0-9A-Z]+):(.*)$")
# ISIN inside a :35B: value: the literal token followed by a 12-char code.
_ISIN_RE = re.compile(r"ISIN\s+([A-Z0-9]{12})")


def parse_mt_block4(text: str) -> list[tuple[str, str]]:
    """Yield (tag, value) for each :tag: in block 4.

    Values may span multiple lines; continuation lines (lines that do not
    start with ':TAG:') are joined with '\n'. Tag is the part between the
    first two colons (e.g. '97A', '60F'). Qualifier sub-tags like
    '::SAFE//xxx' stay in the value.
    """
    m = _MT_BLOCK4_RE.search(text)
    if not m:
        return []
    body = m.group(1)
    out: list[tuple[str, str]] = []
    current_tag: str | None = None
    current_val: list[str] = []
    for line in body.splitlines():
        mm = _TAG_LINE_RE.match(line)
        if mm:
            if current_tag is not None:
                out.append((current_tag, "\n".join(current_val).rstrip()))
            current_tag = mm.group(1)
            current_val = [mm.group(2)]
        else:
            if current_tag is not None:
                current_val.append(line)
    if current_tag is not None:
        out.append((current_tag, "\n".join(current_val).rstrip()))
    return out


def parse_mt_balance(s: str) -> dict | None:
    """Parse :60F: / :62F: / :64: UBS balance line, form <C|D>YYMMDD<CCY><amount>."""
    m = _BAL_RE.match(s)
    if not m:
        return None
    cd, yymmdd, ccy, amt = m.groups()
    return {
        "credit_debit": cd,
        "date_unix": parse_yymmdd(yymmdd),
        "currency_iso": ccy,
        "amount": amt.replace(",", "."),
    }


def _extract_safe_id(fields: list[tuple[str, str]]) -> str | None:
    """Return the safekeeping account ID from a `:97A::SAFE//<id>` field
    in a parsed MT block 4, or None if absent. Used by every MT loader
    whose subject is a single safekeeping account.
    """
    for tag, val in fields:
        if tag == "97A":
            m = re.match(r"^:SAFE//(.+)$", val, re.S)
            if m:
                return m.group(1).strip()
    return None


# --------------------------------------------------------------------------
# Zip helpers
# --------------------------------------------------------------------------

def iter_zip_entries(zip_path: Path, suffix: str | None = None):
    """Yield (filename, bytes) for each entry of a zip, optionally filtered."""
    with zipfile.ZipFile(zip_path) as zf:
        for info in zf.infolist():
            if info.is_dir():
                continue
            if suffix and not info.filename.endswith(suffix):
                continue
            yield info.filename, zf.read(info.filename)


# --------------------------------------------------------------------------
# PSN XML loaders (slow-changing master data — content-dedup)
# --------------------------------------------------------------------------

def _insert_if_changed_master(
    conn, table: str, pk_cols: tuple[str, ...], pk_vals: tuple,
    payload_canon: str, snapshot_at: int,
    extra_cols: tuple[tuple[str, object], ...] = (),
) -> int:
    """Insert a row into `table` only if the most-recent row for the same
    PK suffix (everything after snapshot_at) has a different payload.

    Returns 1 if inserted, 0 if deduped.

    `extra_cols` are non-PK columns whose values come from the row being
    inserted (e.g. promoted-from-payload fields like portfolio_external_id).
    They are not part of the dedup compare — payload is the canonical
    source — but they will reflect the most recent value on every insert.
    """
    where = " AND ".join(f"{c} = ?" for c in pk_cols)
    row = conn.execute(
        f"SELECT payload FROM {table} WHERE {where} "
        f"ORDER BY snapshot_at DESC LIMIT 1",
        pk_vals,
    ).fetchone()
    if row is not None and row[0] == payload_canon:
        return 0
    cols = ("snapshot_at",) + pk_cols \
           + tuple(c for c, _ in extra_cols) + ("payload",)
    vals = (snapshot_at,) + pk_vals \
           + tuple(v for _, v in extra_cols) + (payload_canon,)
    placeholders = ",".join("?" * len(cols))
    conn.execute(
        f"INSERT INTO {table} ({','.join(cols)}) VALUES ({placeholders})",
        vals,
    )
    return 1


def _find_text(parsed: dict, *path: str) -> str | None:
    """Walk a parsed-XML dict by tag-name path; return leaf string or None."""
    cur = parsed
    for p in path:
        if not isinstance(cur, dict) or p not in cur:
            return None
        cur = cur[p]
    return cur if isinstance(cur, str) else None


def load_sdcl(conn, snapshot_at, relationship_id, entities):
    """One row per client per snapshot. SDCL typically has 1 <ClntInfo>."""
    n = 0
    for elem in entities:
        if _strip_ns(elem.tag) != "ClntInfo":
            continue
        parsed = elem_to_dict(elem)["ClntInfo"]
        client_id = _find_text({"ClntInfo": parsed}, "ClntInfo", "ClntId")
        if not client_id:
            log.warning("SDCL entity missing ClntId — skipping")
            continue
        payload = canonical_json(strip_header_noise(parsed))
        n += _insert_if_changed_master(
            conn, "account_holders",
            ("relationship_id", "client_external_id"),
            (relationship_id, client_id),
            payload, snapshot_at,
        )
    return n


def load_sdca(conn, snapshot_at, relationship_id, entities):
    """One row per cash account per snapshot. IBAN as account_external_id.

    `portfolio_external_id` is promoted from payload.PrtflId (nullable —
    standalone bank accounts have no portfolio assignment).
    """
    n = 0
    for elem in entities:
        if _strip_ns(elem.tag) != "CshAcctInfo":
            continue
        parsed = elem_to_dict(elem)["CshAcctInfo"]
        iban = parsed.get("Iban")
        if not iban:
            log.warning("SDCA entity missing Iban — skipping")
            continue
        prtfl_id = parsed.get("PrtflId") or deep_find(parsed, "PrtflId")
        payload = canonical_json(strip_header_noise(parsed))
        n += _insert_if_changed_master(
            conn, "cash_accounts",
            ("relationship_id", "account_external_id"),
            (relationship_id, iban),
            payload, snapshot_at,
            extra_cols=(("portfolio_external_id", prtfl_id),),
        )
    return n


def load_sdsa(conn, snapshot_at, relationship_id, entities):
    """One row per safekeeping account per snapshot.

    Account ID is `AcctId` (the MT535/`:97A::SAFE//` form) — the same
    form used in `holdings` and the safekeeping-side
    `events.account_external_id`. UBS also emits `ExtAcctId` as a
    dashed display form, kept in payload for trace.

    `portfolio_external_id` is promoted from payload.PrtflId (nullable).
    """
    n = 0
    for elem in entities:
        if _strip_ns(elem.tag) != "SfkInfo":
            continue
        parsed = elem_to_dict(elem)["SfkInfo"]
        acct = parsed.get("AcctId") or parsed.get("ExtAcctId")
        if not acct:
            log.warning("SDSA entity missing AcctId — skipping")
            continue
        prtfl_id = parsed.get("PrtflId") or deep_find(parsed, "PrtflId")
        payload = canonical_json(strip_header_noise(parsed))
        n += _insert_if_changed_master(
            conn, "safekeeping_accounts",
            ("relationship_id", "account_external_id"),
            (relationship_id, acct),
            payload, snapshot_at,
            extra_cols=(("portfolio_external_id", prtfl_id),),
        )
    return n


def load_sdpo(conn, snapshot_at, relationship_id, entities):
    """SDPO is a flat sequence (after parse_psn_xml's descent into
    <ClntPrtflCompData>) of <ClntKey>, then alternating <PrtflKey> and
    <PrtflElmntData> sibling groups. PrtflKey acts as a "section header";
    each <PrtflKey> opens a new portfolio and the following <PrtflElmntData>
    elements belong to it until the next <PrtflKey>. One row per portfolio.
    """
    current: dict | None = None
    n = 0
    for child in entities:
        tag = _strip_ns(child.tag)
        if tag == "ClntKey":
            continue                                  # client-level metadata, drop
        if tag == "PrtflKey":
            if current is not None:
                n += _flush_portfolio(conn, snapshot_at, relationship_id, current)
            current = {
                "PrtflKey": elem_to_dict(child)["PrtflKey"],
                "PrtflElmntData": [],
            }
        elif tag == "PrtflElmntData" and current is not None:
            current["PrtflElmntData"].append(
                elem_to_dict(child)["PrtflElmntData"]
            )
    if current is not None:
        n += _flush_portfolio(conn, snapshot_at, relationship_id, current)
    return n


def _flush_portfolio(conn, snapshot_at, relationship_id, portfolio) -> int:
    key = portfolio["PrtflKey"] if isinstance(portfolio["PrtflKey"], dict) else {}
    pid = key.get("PrtflId")
    if not pid:
        log.warning("SDPO portfolio missing PrtflId — skipping")
        return 0
    base_ccy = key.get("PrtflCcyIsoCd")        # promoted as base_currency
    payload = canonical_json(strip_header_noise(portfolio))
    return _insert_if_changed_master(
        conn, "portfolios",
        ("relationship_id", "portfolio_external_id"),
        (relationship_id, pid),
        payload, snapshot_at,
        extra_cols=(("base_currency", base_ccy),),
    )


def load_sdfi(conn, snapshot_at, relationship_id, entities):
    """One row per instrument per snapshot. ISIN is located anywhere
    inside <FiInfo> via a depth-first search (UBS nests it differently
    depending on instrument class)."""
    n = 0
    for elem in entities:
        if _strip_ns(elem.tag) != "FiInfo":
            continue
        parsed = elem_to_dict(elem)["FiInfo"]
        isin = deep_find(parsed, "ISIN")
        if not isin:
            log.debug("SDFI entity missing ISIN — skipping")
            continue
        payload = canonical_json(strip_header_noise(parsed))
        n += _insert_if_changed_master(
            conn, "instruments",
            ("relationship_id", "isin"),
            (relationship_id, isin),
            payload, snapshot_at,
        )
    return n


# --------------------------------------------------------------------------
# PSN XML loaders (daily state / open contracts)
# --------------------------------------------------------------------------

def load_tdfxr(conn, snapshot_at, entities):
    """One row per (snapshot, base, quote) currency from
    <ForeignExchangeRateInfo>.

    Snapshot table — no content-dedup, rates change every batch.
    The base currency lives in the <ForeignExchangeRateBase> sibling
    block (singleton); we hoist it onto each quote row's payload.
    """
    base_ccy = None
    base_meta = None
    for elem in entities:
        if _strip_ns(elem.tag) == "ForeignExchangeRateBase":
            base_meta = elem_to_dict(elem)["ForeignExchangeRateBase"]
            base_ccy = base_meta.get("BaseCcyIsoCd")
            break
    if not base_ccy:
        log.warning("TDFXR missing base currency — skipping")
        return 0

    rows = []
    for elem in entities:
        if _strip_ns(elem.tag) != "ForeignExchangeRateInfo":
            continue
        parsed = elem_to_dict(elem)["ForeignExchangeRateInfo"]
        quote_ccy = parsed.get("CcyIsoCd")
        if not quote_ccy:
            continue
        full = {"_base": base_meta, **parsed}
        rows.append((snapshot_at, base_ccy, quote_ccy,
                     canonical_json(strip_header_noise(full))))
    if rows:
        conn.executemany(
            "INSERT INTO fx_rates"
            "(snapshot_at, base_currency_iso, quote_currency_iso, payload) "
            "VALUES (?, ?, ?, ?)",
            rows,
        )
    return len(rows)


def _load_contract_table(conn, snapshot_at, relationship_id, entities,
                         child_tag, table) -> int:
    """Generic per-contract loader: iterate sibling <child_tag> entities
    (after parse_psn_xml's wrapper descent) and insert one snapshot row each.
    """
    n = 0
    for child in entities:
        if _strip_ns(child.tag) != child_tag:
            continue
        parsed = elem_to_dict(child)[child_tag]
        cid = deep_find(parsed, "CtrctId") or deep_find(parsed, "ExtCtrctId")
        if not cid:
            log.warning("%s contract missing contract ID — skipping", table)
            continue
        payload = canonical_json(strip_header_noise(parsed))
        conn.execute(
            f"INSERT INTO {table}"
            "(snapshot_at, relationship_id, contract_external_id, payload) "
            "VALUES (?, ?, ?, ?)",
            (snapshot_at, relationship_id, str(cid), payload),
        )
        n += 1
    return n


def load_tdfwd(conn, snapshot_at, relationship_id, entities):
    return _load_contract_table(conn, snapshot_at, relationship_id, entities,
                                "FwdCtrctInf", "forward_contracts")


# --------------------------------------------------------------------------
# SWIFT MT loaders
# --------------------------------------------------------------------------

def load_mt535(conn, snapshot_at, relationship_id, mt_text):
    """MT535 Statement of Holdings.

    One MT535 message per safekeeping account. Body has
        :97A::SAFE//<safekeeping_id>
        :16R:FIN ... :16S:FIN     (repeated, one per holding)
    Inside each FIN block we extract ISIN from :35B: and store the
    whole block content as the holdings.payload.
    """
    fields = parse_mt_block4(mt_text)
    safe = _extract_safe_id(fields)
    if not safe:
        log.debug("MT535 missing :97A::SAFE//, skipping")
        return 0

    n = 0
    # Re-parse block 4 in sequence to find FIN sub-sequences.
    sequence: list[tuple[str, str]] = []
    in_fin = False
    for tag, val in fields:
        if tag == "16R" and val == "FIN":
            in_fin = True
            sequence = []
            continue
        if tag == "16S" and val == "FIN":
            in_fin = False
            # extract ISIN from :35B:
            isin = None
            entries = {}
            for t, v in sequence:
                if t == "35B" and not isin:
                    m = _ISIN_RE.search(v)
                    if m:
                        isin = m.group(1)
                entries.setdefault(t, []).append(v)
            if not isin:
                continue
            payload = canonical_json({"safekeeping": safe, "fields": entries})
            conn.execute(
                "INSERT OR REPLACE INTO holdings"
                "(snapshot_at, relationship_id, safekeeping_external_id, isin, payload) "
                "VALUES (?, ?, ?, ?, ?)",
                (snapshot_at, relationship_id, safe, isin, payload),
            )
            n += 1
            continue
        if in_fin:
            sequence.append((tag, val))
    return n


def load_mt537(conn, snapshot_at, relationship_id, mt_text):
    """MT537 Pending Transactions. One message per safekeeping account.

    Header-only "no activity" messages are stored as such — payload
    carries ACTI//N so consumers can tell.
    """
    fields = parse_mt_block4(mt_text)
    safe = _extract_safe_id(fields)
    if not safe:
        return 0

    payload = canonical_json({"fields": [(t, v) for t, v in fields]})
    conn.execute(
        "INSERT OR REPLACE INTO pending_securities"
        "(snapshot_at, relationship_id, safekeeping_external_id, payload) "
        "VALUES (?, ?, ?, ?)",
        (snapshot_at, relationship_id, safe, payload),
    )
    return 1


def _resolve_iban(conn, relationship_id: str,
                  acct_mt_form: str) -> str | None:
    """Look up the IBAN for an MT940 ':25:'-style account number.

    Uses the most-recent `cash_accounts` row (across all snapshots,
    within the same relationship) whose payload `AcctId` matches.
    Returns the IBAN, or None if no mapping is known yet (e.g. a dump
    that contains MT940 but no SDCA, before any earlier dump has
    loaded SDCA for this relationship).
    """
    row = conn.execute(
        "SELECT account_external_id FROM cash_accounts "
        "WHERE relationship_id = ? "
        "  AND json_extract(payload, '$.AcctId') = ? "
        "ORDER BY snapshot_at DESC LIMIT 1",
        (relationship_id, acct_mt_form),
    ).fetchone()
    return row[0] if row else None


def load_mt940(conn, snapshot_at, relationship_id, mt_text):
    """MT940 Customer Statement.

    Produces:
      - 1-2 cash_balances rows  (closing :62F:, optional available :64:)
      - N events rows of kind='cash_movement'  (one per :61: line)

    Window-DELETE-then-INSERT for the events: per (account, kind=cash_movement,
    value-date range from :60F: to :62F:). For rows already present from a
    prior dump with the same statement, this catches upstream amendments.
    """
    fields = parse_mt_block4(mt_text)
    if not fields:
        return (0, 0)

    account: str | None = None
    opening: dict | None = None
    closing: dict | None = None
    available: dict | None = None
    # (parsed_61, [86 lines following]); parsed_61 is None when the :61:
    # line did not match the SWIFT shape — see filter below.
    movements: list[tuple[dict | None, list[str]]] = []

    for tag, val in fields:
        if tag == "25":
            account = val.strip()
        elif tag == "60F":
            opening = parse_mt_balance(val)
        elif tag == "62F":
            closing = parse_mt_balance(val)
        elif tag == "64":
            available = parse_mt_balance(val)
        elif tag == "61":
            parsed = _parse_mt940_61(val)
            if parsed is None:
                log.warning(
                    "MT940 :61: did not match expected shape; "
                    "dropping this movement and any following :86: narrative: %r",
                    val.split("\n", 1)[0],
                )
            movements.append((parsed, []))
        elif tag == "86" and movements:
            # Narrative attaches to the most recent :61:; if that :61: was
            # unparseable, the whole pair (with this narrative) is filtered
            # out below.
            movements[-1][1].append(val)

    # Drop unparseable movements before the insert loop so the eid-synthesis
    # path below can rely on every parsed_61 having the expected keys.
    movements = [(p, n) for p, n in movements if p is not None]

    if not account or not closing:
        log.warning("MT940 missing :25: or :62F: — skipping")
        return (0, 0)

    # Canonicalise :25: → IBAN via cash_accounts.payload.AcctId lookup.
    # SDCA loads earlier in the same dump transaction, so within a normal
    # daily dump the lookup hits the row inserted seconds ago; cross-dump
    # lookups also work because cash_accounts is append-only.
    raw_acct = account
    iban = _resolve_iban(conn, relationship_id, raw_acct)
    if iban is None:
        log.warning(
            "MT940: no IBAN mapping for %r (no cash_accounts row found); "
            "falling back to raw MT940 form for this dump",
            raw_acct,
        )
    else:
        account = iban

    currency = closing["currency_iso"]
    balances_inserted = 0
    for kind_value, bal in (("opening", opening), ("closing", closing),
                             ("available", available)):
        if bal is None:
            continue
        conn.execute(
            "INSERT OR REPLACE INTO cash_balances"
            "(snapshot_at, relationship_id, account_external_id, balance_kind, "
            " currency_iso, payload) "
            "VALUES (?, ?, ?, ?, ?, ?)",
            (snapshot_at, relationship_id, account, kind_value, bal["currency_iso"],
             canonical_json(bal)),
        )
        balances_inserted += 1

    # Window-DELETE-INSERT for events.
    window_start = opening["date_unix"] if opening else closing["date_unix"]
    window_end   = closing["date_unix"]
    conn.execute(
        "DELETE FROM events WHERE account_external_id = ? AND kind = 'cash_movement' "
        "AND timestamp >= ? AND timestamp <= ?",
        (account, window_start, window_end),
    )
    events_inserted = 0
    for parsed_61, narrative in movements:
        # event_external_id: prefer the bank reference; fall back to a deterministic
        # synthesis from (account, value_date, sign, amount, customer_ref).
        bank_ref = parsed_61.get("bank_ref")
        if bank_ref:
            eid = f"mt940:{account}:{bank_ref}"
        else:
            eid = (f"mt940:{account}:{parsed_61['value_date']}:"
                   f"{parsed_61['credit_debit']}:{parsed_61['amount']}:"
                   f"{parsed_61.get('customer_ref','')}")
        payload = canonical_json({
            **parsed_61,
            "narrative": "\n".join(narrative),
            "account": account,
        })
        conn.execute(
            "INSERT OR REPLACE INTO events"
            "(event_external_id, timestamp, relationship_id, account_external_id, "
            " kind, currency_iso, payload) VALUES (?, ?, ?, ?, ?, ?, ?)",
            (eid, parse_yymmdd(parsed_61["value_date"]), relationship_id,
             account, "cash_movement", currency, payload),
        )
        events_inserted += 1
    return (balances_inserted, events_inserted)


_MT940_61_RE = re.compile(
    r"^(?P<value_date>\d{6})(?P<entry_date>\d{4})?"
    # SWIFT debit/credit-mark grammar is `C | D | RC | RD`: an optional
    # `R` (reversal) prefix followed by `C` or `D`. The prefix must come
    # *before* the C/D character, not after — getting this order wrong
    # silently drops reversal entries.
    r"(?P<credit_debit>R?[CD])(?P<funds>[A-Z])?"
    r"(?P<amount>[0-9,]+)"
    r"(?P<txn_type>[NSFCT][A-Z0-9]{3})?"
    r"(?P<customer_ref>[^/\n]*)"
    r"(?://(?P<bank_ref>[^\n]+))?"
)


def _parse_mt940_61(line: str) -> dict | None:
    """Parse a :61: statement line into a field dict, or return None if
    the line does not match the expected SWIFT MT940 :61: shape.

    Returning None (rather than a sentinel dict) makes the caller's
    contract explicit: a non-matching line cannot yield a usable event,
    so it must be skipped at the call site rather than silently turned
    into a row with missing keys.
    """
    m = _MT940_61_RE.match(line.split("\n", 1)[0])
    if not m:
        return None
    d = m.groupdict()
    d["amount"] = d["amount"].replace(",", ".")
    return {k: (v.strip() if isinstance(v, str) else v) for k, v in d.items()}


def load_mt566(conn, snapshot_at, relationship_id, mt_text):
    """MT566 Corporate Action Confirmation.

    Event id = SEME (sender's reference) from :20C::SEME//.
    timestamp = posting/payment date (try :98A::POST// / :98A::PAYD// /
    statement-prep date, in that order).
    account = safekeeping ID from :97A::SAFE//, when present.
    Row-level upsert (INSERT OR REPLACE) on event_external_id; UBS issues
    a fresh MT566 with status CANC for retractions, not a silent delete.
    """
    fields = parse_mt_block4(mt_text)
    by_q = _by_qualifier(fields)
    def g(tag: str, qual: str) -> str | None:
        v = by_q.get(tag, {}).get(qual)
        return v.strip() if isinstance(v, str) else None

    seme = g("20C", "SEME")
    if not seme:
        log.debug("MT566 missing :20C::SEME// — skipping")
        return 0

    corp = g("20C", "CORP")
    caev = g("22F", "CAEV")
    safe = _extract_safe_id(fields)
    isin = None
    for tag, val in fields:
        if tag == "35B":
            m = _ISIN_RE.search(val)
            if m:
                isin = m.group(1)
            break

    timestamp = None
    for qual in ("POST", "PAYD", "VALU"):
        v = g("98A", qual)
        if v:
            timestamp = _parse_unix_dt(v, "%Y%m%d")
            if timestamp is not None:
                break
    if timestamp is None:
        timestamp = snapshot_at  # fall back to snapshot time

    eid = f"mt566:{seme}"
    payload = canonical_json({
        "seme": seme, "corp": corp, "caev": caev, "isin": isin,
        "safekeeping": safe,
        "fields": fields,
    })
    conn.execute(
        "INSERT OR REPLACE INTO events"
        "(event_external_id, timestamp, relationship_id, account_external_id, "
        " kind, currency_iso, payload) VALUES (?, ?, ?, ?, ?, ?, ?)",
        (eid, timestamp, relationship_id, safe or "(unknown)",
         "corporate_action_confirmation", None, payload),
    )
    return 1


# --------------------------------------------------------------------------
# MT515 — trade confirmations
# --------------------------------------------------------------------------

# Map :22H::BUSE// raw code -> conventional side label.
_MT515_SIDE = {"BUYI": "BUY", "SELL": "SELL"}


def _by_qualifier(fields: list[tuple[str, str]]) -> dict[str, dict[str, str]]:
    """Build {tag: {qualifier: value-after-//}} for ':<QUAL>//value' entries.

    Tags that appear multiple times with different qualifiers (e.g.
    :19A::DEAL//, :19A::SETT//, :19A::TRAX//) become a sub-dict.
    """
    out: dict[str, dict[str, str]] = {}
    for tag, val in fields:
        m = re.match(r"^:([A-Z]+)//(.*)$", val, re.S)
        if m:
            out.setdefault(tag, {})[m.group(1)] = m.group(2)
    return out


def _parse_unix_dt(s: str | None, fmt: str) -> int | None:
    """Parse a date/datetime string in the given strptime format; return Unix
    seconds UTC. Returns None on any failure (missing field, bad format)."""
    if not s:
        return None
    try:
        return int(datetime.strptime(s, fmt)
                   .replace(tzinfo=timezone.utc).timestamp())
    except (ValueError, TypeError):
        return None


def _parse_amount_ccy(s: str | None) -> tuple[str | None, float | None]:
    """Parse '<CCY><amount-with-comma>' (e.g. 'XXX1234,56') into ('XXX', 1234.56)."""
    if not s:
        return None, None
    m = re.match(r"^([A-Z]{3})([0-9.,]+)", s)
    if not m:
        return None, None
    try:
        return m.group(1), float(m.group(2).rstrip(",").replace(",", "."))
    except ValueError:
        return m.group(1), None


def _parse_unit_amount(s: str | None) -> float | None:
    """Parse 'UNIT/5087,' (or any single-prefix code) into 5087.0."""
    if not s:
        return None
    m = re.match(r"\w+/([0-9.,]+)", s)
    if not m:
        return None
    try:
        return float(m.group(1).rstrip(",").replace(",", "."))
    except ValueError:
        return None


def _parse_35b(val: str) -> tuple[str | None, str | None]:
    """Extract (ISIN, security-name) from a :35B: multi-line value.

    UBS emits either:
        ISIN <code>
        <NAME>
    or:
        ISIN <code>
        /CH/<local-id>
        <NAME>
    """
    if not val:
        return None, None
    lines = [l.strip() for l in val.splitlines() if l.strip()]
    isin = None
    for l in lines:
        m = _ISIN_RE.match(l)
        if m:
            isin = m.group(1)
            break
    name_lines = [l for l in lines
                  if not l.startswith("ISIN") and not l.startswith("/")]
    name = " ".join(name_lines) if name_lines else None
    return isin, name


def load_mt515(conn, snapshot_at, relationship_id, mt_text):
    """MT515 Client Confirmation of Purchase or Sale.

    One MT515 message per executed trade leg. Emits one row in `events`
    with kind='trade_confirmation' and a structured payload so common
    queries (side, ISIN, qty, price, fees, settlement) work without
    re-parsing MT lines.

    Promoted `account_external_id` = safekeeping account, matching the
    convention used for `corporate_action_confirmation`. The cash
    settlement account is preserved in payload for cash-side joins.
    """
    fields = parse_mt_block4(mt_text)
    if not fields:
        return 0
    by_q = _by_qualifier(fields)
    g = lambda tag, qual: by_q.get(tag, {}).get(qual)

    seme = g("20C", "SEME")
    if not seme:
        log.debug("MT515 missing :20C::SEME// — skipping")
        return 0

    action = next((v for t, v in fields if t == "23G"), None)
    related = g("20C", "RELA")
    buse = g("22H", "BUSE")
    side = _MT515_SIDE.get(buse, buse)

    trade_time_unix = _parse_unix_dt(g("98C", "TRAD"), "%Y%m%d%H%M%S")
    prep_time_unix = _parse_unix_dt(g("98C", "PREP"), "%Y%m%d%H%M%S")
    settlement_date_unix = _parse_unix_dt(g("98A", "SETT"), "%Y%m%d")

    # :35B: comes through fields list (no qualifier syntax there)
    isin = None
    security_name = None
    for tag, val in fields:
        if tag == "35B":
            isin, security_name = _parse_35b(val)
            break

    quantity = _parse_unit_amount(g("36B", "CONF"))
    raw_deal_price = g("90B", "DEAL")             # 'ACTU/<CCY><price>'
    price_currency, price = (None, None)
    if raw_deal_price and "/" in raw_deal_price:
        price_currency, price = _parse_amount_ccy(raw_deal_price.split("/", 1)[1])

    raw_venue = g("94B", "TRAD")                  # 'EXCH/XMAD'
    venue_mic = raw_venue.split("/", 1)[1].strip() if raw_venue and "/" in raw_venue else None

    settlement_currency = g("11A", "FXIB")
    gross_ccy, gross_amt = _parse_amount_ccy(g("19A", "DEAL"))
    net_ccy, net_amt = _parse_amount_ccy(g("19A", "SETT"))
    trax_ccy, trax_amt = _parse_amount_ccy(g("19A", "TRAX"))
    stam_ccy, stam_amt = _parse_amount_ccy(g("19A", "STAM"))

    safe_acct = g("97A", "SAFE")
    cash_acct = g("97A", "CASH")
    buyer_bic = g("95P", "BUYR")
    seller_bic = g("95P", "SELL")

    payload_obj = {
        "action": action,
        "seme": seme,
        "related_order_id": related,
        "trade_time_unix": trade_time_unix,
        "prep_time_unix": prep_time_unix,
        "settlement_date_unix": settlement_date_unix,
        "side": side,
        "isin": isin,
        "security_name": security_name,
        "venue_mic": venue_mic,
        "quantity": quantity,
        "price": price,
        "price_currency": price_currency,
        "settlement_currency": settlement_currency,
        "gross_amount": gross_amt,
        "gross_currency": gross_ccy,
        "net_amount": net_amt,
        "net_currency": net_ccy,
        "transaction_tax_amount": trax_amt,
        "transaction_tax_currency": trax_ccy,
        "stamp_duty_amount": stam_amt,
        "stamp_duty_currency": stam_ccy,
        "safekeeping_external_id": safe_acct,
        "cash_account_external_id": cash_acct,
        "buyer_bic": buyer_bic,
        "seller_bic": seller_bic,
        "raw_fields": fields,
    }

    # Timestamp = trade execution time when present (the natural "when"),
    # else settlement, else prep, else the dump's snapshot_at.
    timestamp = (trade_time_unix or settlement_date_unix or prep_time_unix
                 or snapshot_at)

    conn.execute(
        "INSERT OR REPLACE INTO events"
        "(event_external_id, timestamp, relationship_id, account_external_id, "
        " kind, currency_iso, payload) VALUES (?, ?, ?, ?, ?, ?, ?)",
        (f"mt515:{seme}", timestamp, relationship_id,
         safe_acct or "(unknown)",
         "trade_confirmation", settlement_currency,
         canonical_json(payload_obj)),
    )
    return 1


# --------------------------------------------------------------------------
# Dispatch tables — which zip contains which type, and which loader to call
# --------------------------------------------------------------------------

# XML loaders keyed by TypeCd. Each takes (conn, snapshot_at, relationship_id,
# entities) and returns rows-inserted count.
XML_LOADERS = {
    "SDCL":  load_sdcl,
    "SDCA":  load_sdca,
    "SDSA":  load_sdsa,
    "SDPO":  load_sdpo,
    "SDFI":  load_sdfi,
    "TDFXR": lambda c, s, r, e: load_tdfxr(c, s, e),       # base rates: relationship-agnostic
    "TDFWD": load_tdfwd,
    "TDOPT": lambda c, s, r, e: _load_contract_table(
        c, s, r, e, "OptCtrctInf", "option_contracts"),
    "TDMM":  lambda c, s, r, e: _load_contract_table(
        c, s, r, e, "MMCtrctInf", "money_market_contracts"),
    "TDOTC": lambda c, s, r, e: _load_contract_table(
        c, s, r, e, "OtcCtrctInf", "otc_contracts"),
    # TDCAPI / TDPOPF loaders added when we have non-empty samples.
}

# MT loaders keyed by zip basename. Each takes (conn, snapshot_at,
# relationship_id, message_text) and returns either int (rows) or
# tuple (balances, events).
MT_LOADERS = {
    "ZAH": load_mt535,
    "ZM5": load_mt537,
    "Z40": load_mt940,
    "ZAG": load_mt515,
    "ZAN": load_mt566,
    # ZAY (MT950) is intentionally not loaded; see migration header.
}


# --------------------------------------------------------------------------
# Per-dump driver
# --------------------------------------------------------------------------

def load_dump(conn: sqlite3.Connection, dump_dir: Path,
              relationship_id: str) -> dict:
    name = dump_dir.name
    snapshot_at = parse_snapshot_at(name)

    if conn.execute(
        "SELECT 1 FROM dump_runs WHERE snapshot_at = ?", (snapshot_at,)
    ).fetchone():
        return {"name": name, "skipped": True}

    log.info("Loading dump %s (snapshot_at=%d)", name, snapshot_at)
    stats: dict = {"name": name, "skipped": False}

    with conn:  # BEGIN on entry, COMMIT on clean exit, ROLLBACK on exception
        # Per-file snapshot_at: the file's as-of date from its 'YYYY-MM-DD_'
        # filename prefix (see file_snapshot_at). Falls back to the dump
        # directory's timestamp if the prefix is missing.
        #
        # 1. PSN XML containers (ZMD, ZME) — iterate every .xml entry
        xml_rows = 0
        for zip_path in sorted(dump_dir.glob("Z*.zip")):
            for fname, blob in iter_zip_entries(zip_path, suffix=".xml"):
                try:
                    type_code, entities = parse_psn_xml(blob)
                except ET.ParseError as e:
                    log.warning("XML parse failed for %s: %s", fname, e)
                    continue
                if not entities:
                    log.debug("Empty <Data> in %s (type %s) — skipping",
                              fname, type_code)
                    continue
                loader = XML_LOADERS.get(type_code)
                if loader is None:
                    log.debug("No XML loader for type %s (%s) — skipping",
                              type_code, fname)
                    continue
                file_at = file_snapshot_at(fname) or snapshot_at
                xml_rows += loader(conn, file_at, relationship_id, entities)
        stats["xml_rows"] = xml_rows

        # 2. MT containers — iterate every .txt entry per zip basename
        mt_balances = 0
        mt_events = 0
        mt_other = 0
        for zip_path in sorted(dump_dir.glob("Z*.zip")):
            stem = zip_path.stem.upper()                # 'ZAH', 'Z40', ...
            loader = MT_LOADERS.get(stem)
            if loader is None:
                continue
            for fname, blob in iter_zip_entries(zip_path, suffix=".txt"):
                text = blob.decode("utf-8", errors="replace")
                file_at = file_snapshot_at(fname) or snapshot_at
                result = loader(conn, file_at, relationship_id, text)
                if isinstance(result, tuple):
                    mt_balances += result[0]
                    mt_events += result[1]
                else:
                    mt_other += result
        stats["mt_balances"] = mt_balances
        stats["mt_events"] = mt_events
        stats["mt_other"] = mt_other

        # 3. dump_runs LAST so a mid-load failure leaves no trace.
        conn.execute(
            "INSERT INTO dump_runs"
            "(snapshot_at, silver_schema_version, run_dir) VALUES (?, ?, ?)",
            (snapshot_at, silver.current_schema_version(conn), str(dump_dir.resolve())),
        )
    return stats


# --------------------------------------------------------------------------
# CLI
# --------------------------------------------------------------------------

def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__.strip())
    p.add_argument("--silver-db", type=Path,
                   default=cli.default_data_root() / "ubs-psn" / "ubs-psn.db",
                   help="Path to the silver SQLite database "
                        "(default: %(default)s). Created if missing.")
    p.add_argument("--bronze-dir", type=Path,
                   default=cli.default_data_root() / "ubs-psn",
                   help="Directory containing snapshot subdirectories "
                        "(default: %(default)s).")
    p.add_argument("--relationship-id", default="SFTPCH01",
                   help="UBS Server ID for the banking relationship the "
                        "bronze dumps belong to (e.g. SFTPCH01, SFTPCH02). "
                        "Default: SFTPCH01.")
    p.add_argument("-v", "--verbose", action="store_true",
                   help="DEBUG-level logging.")
    cli.add_force_arg(p)
    return p.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    cli.configure_logging(args.verbose)

    if not args.bronze_dir.is_dir():
        raise SystemExit(f"Bronze directory not found: {args.bronze_dir}")

    if args.force:
        silver.reset(args.silver_db)

    conn = open_db(args.silver_db)
    silver.apply_migrations(conn, MIGRATIONS_DIR)

    dumps = list(bronze.iter_run_dirs(args.bronze_dir))
    log.info("Found %d dump directory(s) under %s", len(dumps), args.bronze_dir)

    for d in dumps:
        stats = load_dump(conn, d, args.relationship_id)
        if stats.get("skipped"):
            log.info("  %s: skipped (already loaded)", stats["name"])
        else:
            log.info(
                "  %s: xml_rows=%d mt_balances=%d mt_events=%d mt_other=%d",
                stats["name"], stats["xml_rows"],
                stats["mt_balances"], stats["mt_events"],
                stats["mt_other"],
            )

    conn.close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
