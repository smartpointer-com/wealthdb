"""
fidelity-web silver: the retail rows of one dump.

The account master (portfolios and accounts), the positions exports,
the activity feed and the document index, each read from a dump's
bronze. Also the cell readers, the content-derived row ids and the
payload form that every silver module of the loader shares.
"""

from __future__ import annotations

import csv
import hashlib
import json
import logging
import re
import sqlite3
from datetime import datetime, timezone

from collectorkit import bronze, compress
from collectorkit.parse import is_cusip

log = logging.getLogger("fidelity-web.load")


# Fidelity selector group labels the loader classifies. Anything
# else falls through to 'other' so the loader doesn't fail on a
# future label change.
PORTFOLIO_KIND = {
    "Education": "529",
    "Authorized": "trust_managed",
}

# Per-portfolio-kind default management style. Fidelity does not
# emit a per-account style indicator (see DESIGN.md §11.6), but
# kind alone pins the style for the categories silver models:
#   529            → automated (the plan offers only percentage-wise
#                    allocation across a small menu of funds and
#                    age-based strategies — a model portfolio, not
#                    free security selection).
#   trust_managed  → discretionary (a third-party manager places
#                    trades; custodian executes).
# 'other' / unknown labels stay NULL — gold handles them.
MANAGEMENT_STYLE_BY_KIND = {
    "529": "automated",
    "trust_managed": "discretionary",
    # A Fidelity DAF invests in model pools (§12.3) — automated, like
    # the 529.
    "daf": "automated",
}


def ts_from_mdy(s):
    """Fidelity's CSV dates are MM/DD/YYYY (US) at midnight UTC."""
    if not s or s == "--" or s.strip() == "":
        return None
    try:
        dt = datetime.strptime(s.strip(), "%m/%d/%Y").replace(tzinfo=timezone.utc)
        return int(dt.timestamp())
    except (TypeError, ValueError):
        return None


_NUMERIC_CLEAN_RE = re.compile(r"[\s,$]+")


def parse_decimal(s):
    """Parse a Fidelity-emitted dollar / percent / count cell.

    Strips '$', commas, and whitespace. Returns None for empty,
    '--', or unparseable strings. '+12.34' is fine; '+12.34%' is
    converted to 0.1234 (fraction)."""
    if s is None:
        return None
    raw = s.strip() if isinstance(s, str) else str(s)
    if not raw or raw == "--":
        return None
    is_pct = raw.endswith("%")
    if is_pct:
        raw = raw[:-1]
    cleaned = _NUMERIC_CLEAN_RE.sub("", raw)
    if not cleaned or cleaned in ("+", "-"):
        return None
    try:
        v = float(cleaned)
    except ValueError:
        return None
    return v / 100.0 if is_pct else v


def _row_ci(row):
    """Case-insensitive view of a CSV row, keyed on casefolded header
    names. Fidelity's exports have shipped the same headers in more
    than one casing ('Account Number' / 'Account number', 'Last Price'
    / 'Last price', …), and an exact-case lookup reads a re-cased file
    as empty; casefolded lookups read every casing identically. The
    original row (source-cased keys) still lands in payload."""
    return {(k or "").strip().casefold(): v for k, v in row.items()
            if k is not None}


def _ci_get(ci_row, *names):
    """First present value among casefolded column ``names`` — for
    columns Fidelity has renamed outright (e.g. the dividend view's
    'Dist. yield' → 'Dist. rate')."""
    for name in names:
        v = ci_row.get(name)
        if v is not None:
            return v
    return None


def normalize_payload(data):
    # csv.DictReader bundles extra columns (trailing commas) under
    # a None key. Drop those — they're not safely sortable in JSON.
    return json.dumps(
        _strip_none_keys(data),
        ensure_ascii=False, sort_keys=True, default=str,
    )


def _strip_none_keys(data):
    if isinstance(data, dict):
        return {k: _strip_none_keys(v) for k, v in data.items()
                if k is not None}
    if isinstance(data, list):
        return [_strip_none_keys(v) for v in data]
    return data


# ------------------------------------------------------------
# Master data: portfolios + accounts (from run.json's
# account_dimensions, which captures the account-selector groups
# and per-account nicknames the bronze fetch read at walk-start).
# ------------------------------------------------------------

def _load_master(conn, snapshot_at, run_meta):
    # A dump only vouches for the accounts its phases covered. A
    # mode='daf' dump ran no retail phase — its account_dimensions is
    # an enumeration-only capture of the account selector — so writing
    # retail master rows at that snapshot would pair accounts with no
    # value rows, which latest-snapshot-wins consumers read as the
    # accounts having emptied. Partial coverage must never masquerade
    # as observation (the DAF master rows come from _load_daf, whose
    # phase did run).
    mode = (run_meta.get("cli_config") or {}).get("mode")
    if mode == "daf":
        log.debug("mode=daf dump: retail master load skipped "
                  "(enumeration-only capture)")
        return 0, 0
    dims = run_meta.get("account_dimensions") or {}
    if not dims:
        log.debug("no account_dimensions in run.json; skipping master load")
        return 0, 0
    # account_dimensions is keyed by sha256-prefix; the raw 9-digit
    # id lives in accounts_in_scope + accounts_enumerated.
    enumerated = set(run_meta.get("accounts_enumerated") or [])
    in_scope = set(run_meta.get("accounts_in_scope") or [])
    # Build hash → raw-id map.
    hash_to_id = {account_key(aid): aid for aid in enumerated}

    portfolios_inserted = 0
    accounts_inserted = 0
    seen_portfolios = set()
    for hashed, entry in dims.items():
        aid = hash_to_id.get(hashed)
        if not aid:
            log.debug("account_dimensions key %s not in enumerated set", hashed)
            continue
        if aid not in in_scope:
            # DAF / auto-excluded — don't pollute silver.
            continue
        portfolio_ext = entry.get("portfolio")
        nickname = entry.get("nickname")
        kind = PORTFOLIO_KIND.get(portfolio_ext, "other") if portfolio_ext else None
        management_style = MANAGEMENT_STYLE_BY_KIND.get(kind) if kind else None
        if portfolio_ext and portfolio_ext not in seen_portfolios:
            seen_portfolios.add(portfolio_ext)
            conn.execute(
                "INSERT OR REPLACE INTO portfolios ("
                "snapshot_at, portfolio_external_id, kind, payload"
                ") VALUES (?, ?, ?, ?)",
                (snapshot_at, portfolio_ext, kind,
                 normalize_payload({"source": "run.json/account_dimensions"})),
            )
            portfolios_inserted += 1
        conn.execute(
            "INSERT OR REPLACE INTO accounts ("
            "snapshot_at, account_external_id, portfolio_external_id, "
            "nickname, payload, management_style"
            ") VALUES (?, ?, ?, ?, ?, ?)",
            (snapshot_at, aid, portfolio_ext, nickname,
             normalize_payload({"source": "run.json/account_dimensions",
                                "hash": hashed}),
             management_style),
        )
        accounts_inserted += 1
    return portfolios_inserted, accounts_inserted


def account_key(account_external_id):
    """Mirrors download.account_key() — first 16 hex of sha256."""
    return bronze.short_token(account_external_id)


# ------------------------------------------------------------
# Positions
# ------------------------------------------------------------

POSITIONS_FILES = {
    "summary": "positions_summary.csv",
    "dividend": "positions_dividend.csv",
}


def _parse_positions(dump_dir):
    """Parse both positions views into a (account, instrument) →
    {view: row} map without touching silver — so `load_dump` can judge
    the dump's positions completeness BEFORE anything is inserted (the
    all-or-nothing guard)."""
    pos_dir = dump_dir / "positions"
    merged = {}
    if not pos_dir.is_dir():
        return merged
    for view, fname in POSITIONS_FILES.items():
        # download / recompress may have written positions_<view>.csv
        # as .csv.zst; resolve whichever variant is on disk (plain wins).
        path = compress.resolve_variant(pos_dir / fname)
        if path is None:
            continue
        for row in _iter_positions_rows(path):
            ci = _row_ci(row)
            account_ext = (ci.get("account number") or "").strip()
            instr = (ci.get("symbol") or "").strip()
            if not account_ext or not instr:
                continue
            key = (account_ext, instr)
            entry = merged.setdefault(key, {})
            entry[view] = row
    return merged


def _load_positions(conn, snapshot_at, merged):
    # The summary view's columns land first; dividend-view columns
    # merge over (``merged`` comes from _parse_positions).
    inserted = 0
    for (account_ext, raw_instr), views in merged.items():
        # Strip trailing '*' chars Fidelity appends to money-market
        # core-position symbols (e.g. 'CORE_X**' → 'CORE_X'). The
        # asterisks are a channel signal promoted to the
        # is_core_position flag; the silver instrument_key joins
        # cleanly against transactions where the same fund appears
        # without the suffix.
        is_core = 1 if raw_instr.endswith("*") else 0
        instr = raw_instr.rstrip("*")
        summary = views.get("summary", {})
        dividend = views.get("dividend", {})
        # Case-insensitive views for lookups (payload keeps the
        # source-cased rows). The distribution column appears as both
        # 'Dist. yield' and 'Dist. rate' (_ci_get takes either).
        ci_sum = _row_ci(summary)
        ci_div = _row_ci(dividend)
        ci_pri = ci_sum or ci_div
        # Prefer summary's quantity/value/cost; fall back to dividend.
        description = ci_pri.get("description") or None
        asset_class = _classify_asset_class(instr, description, is_core)
        conn.execute(
            "INSERT OR REPLACE INTO positions ("
            "snapshot_at, account_external_id, instrument_key, "
            "description, quantity, last_price, current_value, "
            "cost_basis_total, average_cost_basis, type, "
            "ex_date, amount_per_share, pay_date, "
            "distribution_yield, sec_yield, est_annual_income, "
            "payload, currency, asset_class, is_core_position"
            ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            (
                snapshot_at, account_ext, instr,
                description,
                parse_decimal(ci_pri.get("quantity")),
                parse_decimal(ci_pri.get("last price")),
                parse_decimal(ci_pri.get("current value")),
                parse_decimal(ci_sum.get("cost basis total")),
                parse_decimal(ci_sum.get("average cost basis")),
                ci_pri.get("type") or None,
                ts_from_mdy(ci_div.get("ex-date")),
                parse_decimal(ci_div.get("amount per share")),
                ts_from_mdy(ci_div.get("pay date")),
                parse_decimal(_ci_get(ci_div, "dist. yield", "dist. rate")),
                parse_decimal(ci_div.get("sec yield")),
                parse_decimal(ci_div.get("est. annual income")),
                normalize_payload({"summary": summary, "dividend": dividend}),
                "USD",
                asset_class,
                is_core,
            ),
        )
        inserted += 1
    return inserted


# Regexes for the instrument-shape rules in _classify_asset_class.
# CUSIP-9: 9 alphanumeric chars with a trailing check digit.
_CUSIP9_RE = re.compile(r"^[A-Z0-9]{8}[0-9]$")
# Fidelity's 529-plan investment-option codes: 3 letters + 6 digits
# (an internal Fidelity scheme for target-date / risk-bucket
# sleeves inside a state 529 plan). The strictness matters — some
# ADRs share the first three letters of a state's plan prefix, so
# the regex pins the trailing 6 characters to digits.
_PLAN_FUND_RE = re.compile(r"^[A-Z]{3}[0-9]{6}$")
# Industry mutual-fund convention: 5-char ticker ending in 'X'.
_MUTUAL_FUND_RE = re.compile(r"^[A-Z]{4}X$")
# Word-boundary "ETF" in the Description field. Fidelity groups
# ETFs with stocks (no structured signal), but the fund sponsors
# put "ETF" in the security name itself. The word boundary
# matters: a substring match would also catch N-ETF-LIX.
_ETF_DESC_RE = re.compile(r"\bETF\b", re.IGNORECASE)


def _classify_asset_class(instrument_key, description, is_core_position):
    """Heuristic asset-class derivation from the Fidelity Symbol +
    Description fields. Order matters — first match wins. Returns
    one of 'money_market' / 'bond' / 'plan_fund' / 'mutual_fund' /
    'etf' / 'equity'. Gold can override via the config's
    instrument_overrides; this populates the column for the common
    cases."""
    if is_core_position:
        return "money_market"
    if not instrument_key:
        return "equity"
    # plan_fund BEFORE the broader CUSIP-9 check because the
    # plan-fund shape ([A-Z]{3}[0-9]{6}) is a stricter subset of
    # CUSIP-9 (any alphanumeric ending in a digit). Real CUSIPs
    # almost never fit the strict 3-letters-then-6-digits pattern.
    if _PLAN_FUND_RE.match(instrument_key):
        return "plan_fund"
    if _CUSIP9_RE.match(instrument_key):
        # 9-char alphanumeric with trailing check digit → CUSIP.
        # Fidelity exposes the CUSIP in the Symbol column for
        # bonds when no ticker exists.
        return "bond"
    if _MUTUAL_FUND_RE.match(instrument_key):
        return "mutual_fund"
    if description and _ETF_DESC_RE.search(description):
        # Exchange-traded funds trade like stocks and Fidelity
        # groups them with equities; the security name is the only
        # signal. Misses name-shy exchange-traded products (e.g. a
        # commodity trust whose name says "SHS") — those are what
        # the gold config's instrument_overrides are for.
        return "etf"
    return "equity"


def _iter_positions_rows(csv_path):
    """Yield meaningful holding rows from a Fidelity positions
    export CSV. Stops at the first 'Brokerage services...'
    disclaimer line (Fidelity glues legalese onto the end with
    rows that pollute the data otherwise).

    Some rows have a literal 'Pending Activity' or 'Account Total'
    in the Symbol column — skip those too."""
    SKIP_INSTRUMENTS = {"Pending Activity", "Account Total"}
    # csv_path may be plain .csv or a .csv.zst variant — open_text
    # decompresses by suffix in memory. Keep utf-8-sig (BOM strip) +
    # newline="" so a compressed load reads byte-identical rows.
    with compress.open_text(csv_path, encoding="utf-8-sig", newline="") as f:
        # Read raw lines first so the parse can stop at the disclaimer.
        text = f.read()
    # Disclaimers start with 'Brokerage services' or '"The data...'.
    cutoff = len(text)
    for marker in ('\n"The data ',
                   '\nBrokerage services',
                   '\nDate downloaded'):
        i = text.find(marker)
        if i != -1 and i < cutoff:
            cutoff = i
    text = text[:cutoff]
    reader = csv.DictReader(text.splitlines())
    for row in reader:
        ci = _row_ci(row)
        sym = (ci.get("symbol") or "").strip()
        acct = (ci.get("account number") or "").strip()
        if not acct:
            continue
        if not sym or sym in SKIP_INSTRUMENTS:
            continue
        yield row


# ------------------------------------------------------------
# Transactions
# ------------------------------------------------------------

ACTIVITY_FILE_RE = re.compile(r"^activity_.*\.csv$")


def _resolved_bronze_files(directory, matches):
    """The on-disk file of each LOGICAL name (any .zst/.gz stripped) in
    `directory` that `matches(logical_path)` accepts, sorted by name.
    A plain + .zst twin that coexist (a recompress interrupted between
    verify and unlink) resolve to one file, the plain original."""
    logical_names = set()
    for path in directory.iterdir():
        if not path.is_file():
            continue
        logical = compress.logical_bronze_path(path)
        if matches(logical):
            logical_names.add(logical.name)
    resolved = (compress.resolve_variant(directory / name)
                for name in sorted(logical_names))
    return [p for p in resolved if p is not None]


def _load_transactions(conn, dump_dir):
    act_dir = dump_dir / "activity"
    if not act_dir.is_dir():
        return 0
    # ACTIVITY_FILE_RE matches the logical `activity_*.csv` name.
    return sum(_ingest_activity_csv(conn, path)
               for path in _resolved_bronze_files(
                   act_dir, lambda p: ACTIVITY_FILE_RE.match(p.name)))


def _ingest_activity_csv(conn, csv_path):
    # source_sha256 is the DECOMPRESSED content hash, not the on-disk
    # file's hash: a .csv.zst and its plain twin must produce identical
    # transactions rows (the convergence invariant), and source_sha256
    # is a stored column. read_text_and_sha hashes those decompressed
    # bytes while reading them, so pre-compression dumps are unaffected
    # and the file is decompressed once.
    text, src_sha, _ = compress.read_text_and_sha(csv_path, encoding="utf-8-sig")
    inserted = 0
    # Fidelity prefixes a BOM + blank line before the header on
    # every activity export. Skip everything up to the line that
    # starts with "Run Date,".
    lines = text.splitlines()
    header_idx = next(
        (i for i, ln in enumerate(lines) if ln.startswith("Run Date,")),
        None,
    )
    if header_idx is None:
        log.warning("no 'Run Date,' header in %s; skipping", csv_path.name)
        return 0
    body = "\n".join(lines[header_idx:])
    reader = csv.DictReader(body.splitlines())
    if not reader.fieldnames or "Run Date" not in reader.fieldnames:
        log.warning("unexpected activity CSV header in %s; skipping",
                    csv_path.name)
        return 0
    # Per-file occurrence counter, keyed by the row's structural
    # identity. See _synthesise_activity_id for why this — rather
    # than the file sha256 + global CSV row index — is the dedup key.
    occ_counter: dict[str, int] = {}
    for row in reader:
        # Case-insensitive lookups (_row_ci): a re-cased header
        # must not read as an empty column (the positions export
        # can change header casing).
        # Values (which the activity identity hashes) are
        # untouched, so ids are stable across header re-casings.
        ci = _row_ci(row)
        run_date = (ci.get("run date") or "").strip()
        # Footer "Date downloaded..." rows surface as a single
        # field that doesn't match Fidelity's data shape.
        if not run_date or not re.match(r"^\d{2}/\d{2}/\d{4}$", run_date):
            continue
        account_ext = (ci.get("account number") or "").strip()
        if not account_ext:
            continue
        ts = ts_from_mdy(run_date)
        if ts is None:
            continue
        action = (ci.get("action") or "").strip()
        kind = _classify_action(action)
        symbol = (ci.get("symbol") or "").strip() or None
        instrument_key = symbol or instrument_from_action(action)
        quantity = parse_decimal(ci.get("quantity"))
        price = parse_decimal(ci.get("price ($)"))
        amount = parse_decimal(ci.get("amount ($)"))
        settlement = ts_from_mdy(ci.get("settlement date"))
        payload = normalize_payload(dict(row))
        identity = _activity_identity(
            account_ext, ts, kind, quantity, price, amount, settlement)
        occ = occ_counter.get(identity, 0)
        occ_counter[identity] = occ + 1
        activity_id = _synthesise_activity_id(identity, occ)
        if symbol is None and _stored_symbol(conn, activity_id):
            # Another export printed this transaction's Symbol; that
            # copy names the security the way the positions do, so it
            # stays whatever order the exports load in.
            #
            # Limitation: the identity names no security, so two
            # transactions with the same economics in one account on
            # one day (say, two securities bought for the same
            # quantity, price and amount) differ only by occurrence
            # index, which follows print order. When export A prints
            # them X, Y and a later export B prints Y, X with Y's
            # Symbol blank, B's Y lands on X's id and is skipped
            # here, and B's X then replaces A's Y under the other id.
            # Silver keeps X twice and loses Y until an export that
            # prints both Symbols loads after B.
            continue
        conn.execute(
            "INSERT OR REPLACE INTO transactions ("
            "activity_id, timestamp, account_external_id, kind, "
            "instrument_key, quantity, price, amount, "
            "settlement_date, source_sha256, payload, currency"
            ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            (
                activity_id, ts, account_ext, kind, instrument_key,
                quantity, price, amount, settlement,
                src_sha,
                payload,
                "USD",
            ),
        )
        inserted += 1
    return inserted


# Some activity exports leave the Symbol column blank for a security
# and name it only in the action text, by CUSIP: in parentheses after
# the name (`YOU BOUGHT <name> (<CUSIP>) (Cash)`), or bare right after
# the verb when no name is printed (`YOU BOUGHT <CUSIP> (Cash)`). The
# bare form is read only behind a trade or income verb, so a check or
# journal number elsewhere in the text is never taken for a CUSIP.
_ACTION_CUSIP_RE = re.compile(r"\(([0-9A-Z]{8}[0-9])\)")
_ACTION_BARE_CUSIP_RE = re.compile(
    r"^(?:YOU BOUGHT|YOU SOLD|DIVIDEND RECEIVED|REINVESTMENT|"
    r"REVERSE SPLIT R/S TO)\s+([0-9A-Z]{8}[0-9])(?![0-9A-Z])",
    re.IGNORECASE)


def instrument_from_action(action):
    """The CUSIP an activity row's action text names, or None. In
    parentheses, the last valid one wins, since the security's CUSIP
    follows its name; otherwise a bare CUSIP right after the verb. A
    token whose check digit fails is not a CUSIP."""
    for token in reversed(_ACTION_CUSIP_RE.findall(action or "")):
        if is_cusip(token):
            return token
    m = _ACTION_BARE_CUSIP_RE.match((action or "").strip())
    if m and is_cusip(m.group(1).upper()):
        return m.group(1).upper()
    return None


def _fill_instrument_keys(conn):
    """Give an activity row loaded with a blank Symbol the CUSIP its
    action text names, the way ingest does for new rows. Idempotent;
    rows of the supplied statements (`stmt_`) and rows with no Action
    are left alone. Returns the number of rows filled."""
    filled = 0
    rows = conn.execute(
        "SELECT activity_id, json_extract(payload, '$.Action') "
        "FROM transactions WHERE instrument_key IS NULL "
        "AND activity_id NOT LIKE 'stmt\\_%' ESCAPE '\\' "
        "AND json_extract(payload, '$.Action') IS NOT NULL").fetchall()
    for activity_id, action in rows:
        cusip = instrument_from_action(action)
        if cusip:
            conn.execute(
                "UPDATE transactions SET instrument_key = ? "
                "WHERE activity_id = ?", (cusip, activity_id))
            filled += 1
    return filled


# Fidelity's Action column is free-text. Pull out the user-
# meaningful action verb so silver can filter by `kind` directly.
# Order matters — most specific first; the catch-all is the first
# whitespace-delimited token, uppercased.
_ACTION_PATTERNS = (
    (re.compile(r"^YOU\s+BOUGHT\b", re.I), "BUY"),
    (re.compile(r"^YOU\s+SOLD\b", re.I), "SELL"),
    (re.compile(r"^REINVESTMENT\b", re.I), "REINVESTMENT"),
    (re.compile(r"^DIVIDEND\s+RECEIVED\b", re.I), "DIVIDEND"),
    (re.compile(r"^DISTRIBUTION\b", re.I), "DISTRIBUTION"),
    (re.compile(r"^MUNI\s+EXEMPT\s+INT\b", re.I), "INTEREST"),
    (re.compile(r"^INTEREST\b", re.I), "INTEREST"),
    (re.compile(r"^FOREIGN\s+TAX\b", re.I), "TAX"),
    (re.compile(r"^FEE\b", re.I), "FEE"),
    (re.compile(r"^PURCHASE\s+INTO\s+CORE", re.I), "CASH_SWEEP_IN"),
    (re.compile(r"^REDEMPTION\s+FROM\s+CORE", re.I), "CASH_SWEEP_OUT"),
    (re.compile(r"^TRANSFERRED\b", re.I), "TRANSFER"),
    (re.compile(r"^JOURNALED\b", re.I), "JOURNAL"),
    (re.compile(r"^IN\s+LIEU\s+OF", re.I), "CASH_IN_LIEU"),
    (re.compile(r"^NAME\s+CHANGE", re.I), "NAME_CHANGE"),
    (re.compile(r"^MERGER\b", re.I), "MERGER"),
    (re.compile(r"^REVERSE\s+SPLIT", re.I), "REVERSE_SPLIT"),
    (re.compile(r"^RETURN\s+OF\s+CAPITAL", re.I), "RETURN_OF_CAPITAL"),
    (re.compile(r"^TENDERED\b", re.I), "TENDER"),
    (re.compile(r"^ADJUST(?:MENT)?\b", re.I), "ADJUSTMENT"),
    (re.compile(r"^ADJ\b", re.I), "ADJUSTMENT"),
    (re.compile(r"^(?:SHORT|LONG)-TERM\s+CAP\b", re.I), "DISTRIBUTION"),
    (re.compile(r"^EXPIRED\b", re.I), "EXPIRATION"),
    (re.compile(r"^REDEMPTION\b", re.I), "REDEMPTION"),
)


def _classify_action(action):
    if not action:
        return "UNKNOWN"
    for pat, kind in _ACTION_PATTERNS:
        if pat.search(action):
            return kind
    return action.split()[0].upper()


def _stored_symbol(conn, activity_id):
    """The Symbol cell of the stored copy of an activity row, or ''."""
    row = conn.execute(
        "SELECT payload FROM transactions WHERE activity_id = ?",
        (activity_id,)).fetchone()
    if row is None:
        return ""
    return (_row_ci(json.loads(row[0])).get("symbol") or "").strip()


def _activity_identity(account_ext, ts, kind, quantity, price, amount,
                       settlement_ts):
    """Structural fingerprint of one activity row: the parsed
    economic columns, none of the text ones.

    Fidelity re-labels securities between exports — the same
    transaction's Action/Description text drifts (e.g. "SPONSORED
    ADR" one month, an abbreviated form the next) — and fills the
    Symbol cell in one export and leaves it blank in another, the
    security then named only inside the Action text. Any of those
    columns in the identity splits one transaction into two rows. The
    economics (who, when, what verb, how many, at what price, for how
    much, settling when) do not drift, so only they participate.
    Numbers enter parsed (not as raw CSV strings) so formatting changes
    ("1,250.00" vs "1250.00") can't split the key either.

    Two distinct transactions with the same economics on the same day
    in one account still get two rows: the occurrence index of
    _synthesise_activity_id tells them apart, and every export that
    covers the day carries both.
    """
    return json.dumps(
        [account_ext, ts, kind, quantity, price, amount, settlement_ts],
        separators=(",", ":"))


def _synthesise_activity_id(identity, occurrence):
    """Stable, file-independent dedup key for one activity row.

    Fidelity's Activity export is consolidated across all accounts
    and the backfill downloads it window by window
    (`download.MAX_ACTIVITY_WINDOW_DAYS`-day chunks)
    on every run. The SAME real-world transaction therefore lands
    in many distinct CSV files — different windows, different runs.
    The dedup key must depend only on the transaction's intrinsic
    content so those copies collapse onto one row.

    The fingerprint is the row's structural identity (see
    _activity_identity) plus a per-file ``occurrence`` index. The
    occurrence index disambiguates genuinely-repeated identical
    rows within a single export (e.g. two same-day, same-amount
    fills) without breaking cross-file convergence: every file that
    covers a given day sees that day's complete set of rows, so the
    Nth identical copy is assigned the same occurrence index N in
    every file.

    Neither the source file nor the description text enters the key:
    both vary between copies of one transaction. source_sha256 is
    stored as a column for provenance only.
    """
    h = hashlib.sha256()
    h.update(identity.encode("utf-8"))
    h.update(b"|")
    h.update(str(occurrence).encode("ascii"))
    return h.hexdigest()[:32]


def _occurrence_id(prefix, identity, occurrence):
    """A row id derived from the row's own content, ``identity``, plus
    its occurrence index among the rows of one document that share it,
    counted in ``occurrence``. The same row read again converges on its
    id, while two identical rows in one document keep two."""
    occ = occurrence.get(identity, 0)
    occurrence[identity] = occ + 1
    return prefix + hashlib.sha256(
        f"{identity}|#{occ}".encode()).hexdigest()[:28]


# ------------------------------------------------------------
# Documents
# ------------------------------------------------------------

TAX_FORM_YEAR_RE = re.compile(r"^(\d{4})-")


def _load_documents(conn, snapshot_at, dump_dir):
    """Index every PDF / HTML capture in the dump dir into the
    documents table. Dedups on content_sha256; the first dump that
    surfaced a given file's bytes wins the snapshot_at column."""
    inserted = 0
    # Statements + tax forms.
    docs_dir = dump_dir / "documents"
    if docs_dir.is_dir():
        # PDFs are never compressed (already internally compressed) —
        # hashed + sized by their raw bytes, unchanged.
        for pdf in sorted(docs_dir.glob("*.pdf")):
            inserted += _ingest_document(
                conn, snapshot_at, pdf,
                _classify_documents_pdf(pdf.name),
            )
        # Statement CSV companions ARE compressible: a `.csv.zst` has
        # suffix `.zst`, so match on the LOGICAL name and resolve the
        # on-disk variant (plain wins). The decompressed-hash path keeps
        # the documents row identical to a plain-CSV load.
        for resolved in _resolved_bronze_files(
                docs_dir, lambda p: p.suffix.lower() == ".csv"):
            inserted += _ingest_document(
                conn, snapshot_at, resolved,
                {"doc_kind": "statement", "file_format": "csv"},
                compressible=True,
            )
    # Balances HTML (compressible: balances.html or balances.html.zst).
    bal = compress.resolve_variant(dump_dir / "balances" / "balances.html")
    if bal is not None:
        inserted += _ingest_document(
            conn, snapshot_at, bal,
            {"doc_kind": "balances_html", "file_format": "html"},
            compressible=True,
        )
    # Performance HTML (compressible).
    perf = compress.resolve_variant(
        dump_dir / "performance" / "performance.html")
    if perf is not None:
        inserted += _ingest_document(
            conn, snapshot_at, perf,
            {"doc_kind": "performance_html", "file_format": "html"},
            compressible=True,
        )
    return inserted


def _classify_documents_pdf(filename):
    """Heuristic kind + tax_year for documents/*.pdf
    based on Fidelity's filename conventions:

      Statement<MMDDYYYY>.pdf
          → statement
      <YYYY>-<Nickname>-<NNNN>-Consolidated-Form-1099.pdf
      <YYYY>-<Nickname>-<NNNN>-CORRECTED-Consolidated-Form-1099.pdf
      <YYYY>-<Nickname>-<NNNN>-Form-1099-Q-Instructions.pdf
          → tax_form, tax_year=<YYYY>
      Consolidated_Form_1099.pdf, CORRECTED_Consolidated_Form_1099__1.pdf,
      Form_1099_Q_….pdf
          → tax_form; the name states no year (the form itself does)
    """
    info = {"file_format": "pdf"}
    if filename.lower().startswith("statement"):
        info["doc_kind"] = "statement"
        return info
    m = TAX_FORM_YEAR_RE.match(filename)
    if m:
        info["doc_kind"] = "tax_form"
        info["tax_year"] = int(m.group(1))
    elif "1099" in filename:
        info["doc_kind"] = "tax_form"
    else:
        info["doc_kind"] = "statement"  # fallback
    return info


def _ingest_document(conn, snapshot_at, path, classification, *,
                     compressible=False):
    """Index one bronze artefact into the documents table.

    PDFs (``compressible=False``, the default — PDFs are never
    compressed) are hashed and sized by their raw on-disk bytes.

    Compressible artefacts (HTML / CSV, which download + recompress may
    have written as ``<name>.zst``) are keyed on their DECOMPRESSED
    content: content_sha256 + size_bytes are of the logical uncompressed
    bytes, and file_path / file_name drop the compression suffix. That
    makes a documents row byte-identical whether the file on disk is
    ``balances.html`` or ``balances.html.zst`` — the convergence
    invariant a ``load --force`` on a compressed vs plain bronze tree
    relies on, and the reason the content_sha256 dedup still collapses
    the same artefact across runs regardless of compression state."""
    if compressible:
        sha, size = compress.decompressed_sha256(path)
        logical = compress.logical_bronze_path(path)
    else:
        sha = bronze.sha256_file(path)[0]
        size = path.stat().st_size
        logical = path
    info = dict(classification)
    info.setdefault("file_format", logical.suffix.lstrip(".").lower() or "bin")
    info.setdefault("doc_kind", "statement")
    try:
        conn.execute(
            "INSERT INTO documents ("
            "content_sha256, snapshot_at, file_path, file_name, "
            "size_bytes, doc_kind, file_format, tax_year, "
            "account_external_id, payload"
            ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            (
                sha, snapshot_at, str(logical.resolve()), logical.name,
                size, info["doc_kind"], info["file_format"],
                info.get("tax_year"),
                info.get("account_external_id"),
                normalize_payload({"filename": logical.name, **info}),
            ),
        )
        return 1
    except sqlite3.IntegrityError:
        # Same bytes already loaded from an earlier dump.
        return 0
