"""
fidelity-web silver: the rows statement PDFs state.

The holdings of the 529, DAF and supplied statements in
`historical_position_snapshots`, the supplied statements' account-level
activity and its match to the scraped feed, and a master row for each
account that only statements name.
"""

from __future__ import annotations

import logging
import re
import sqlite3

from collectorkit import bronze, channels
from collectorkit.parse import iso_date_to_epoch

from silver_retail import _occurrence_id, normalize_payload

log = logging.getLogger("fidelity-web.load")


def _drop_document_holdings(conn, sha):
    """Drop the `historical_position_snapshots` rows one source PDF
    materialised, so re-parsing that PDF REPLACES them instead of leaving
    the old ones beside them.

    A holding's DESCRIPTION — read off the page — is part of that table's
    primary key, so a parser edit re-keys the row and `INSERT OR REPLACE`
    has nothing to replace. Scoped by the document rather than by the pass
    because three passes share the table and nothing on a row says which of
    them wrote it, while `source_sha256` says exactly which PDF did. The
    document is also the only unit any of them can re-derive.

    Called only once a parse has SUCCEEDED. A statement that fails to parse,
    fails its signature guard or fails reconciliation is never dropped:
    stale rows beat no rows when nothing can re-derive them.

    The one case this grain cannot reach: a row whose last writer was a
    statement since removed from bronze carries THAT statement's sha, so the
    statement still present does not delete it and a re-key would leave both.
    Removing a supplied statement is a deliberate act; `load --force` is the
    repair.
    """
    conn.execute(
        "DELETE FROM historical_position_snapshots WHERE source_sha256 = ?",
        (sha,))


def _drop_supplied_activity(conn):
    """Drop every statement-derived activity row, for a re-derivation of all
    of them.

    Scope-wide rather than per-document, because it can be: the `stmt_`
    prefix names exactly the rows the supplied pass writes into
    `transactions`, so unlike the holdings above there is no row this cannot
    reach — including one whose statement has since left the tree. Rows of
    the scraped feed carry structural ids and are untouched.
    """
    return conn.execute(
        r"DELETE FROM transactions WHERE activity_id LIKE 'stmt\_%' "
        r"ESCAPE '\'").rowcount


def _statement_pdf_candidates(dump_dir):
    """The 529 statement PDFs in a dump's ``documents/`` — files
    matching ``Statement<MMDDYYYY>.pdf`` (the CSV companions carry
    only cash-flow lines, tax forms use a ``<YYYY>-…`` prefix), in
    the sorted order the rows are inserted."""
    docs_dir = dump_dir / "documents"
    if not docs_dir.is_dir():
        return []
    return sorted(docs_dir.glob("Statement*.pdf"))


def _insert_historical_rows(conn, pdf_path, parsed, sha):
    """Insert one ``historical_position_snapshots`` row per
    holding in the parsed statement. Cross-walks the human-
    readable fund description to an `instrument_key` when a
    matching `positions.description` exists in silver; leaves
    `instrument_key` NULL otherwise (gold can resolve)."""
    period_end = parsed.get("period_end")
    if not period_end:
        log.debug(
            "historical: no period in %s; skipping", pdf_path.name,
        )
        return 0
    as_of = iso_date_to_epoch(period_end)
    if as_of is None:
        return 0
    inserted = 0
    for account in parsed.get("accounts", []):
        aid = account.get("account_external_id")
        if not aid:
            continue
        for holding in account.get("holdings", []):
            desc = (holding.get("description") or "").strip()
            if not desc:
                continue
            instrument_key = _crosswalk_description_to_instrument(
                conn, aid, desc,
            )
            try:
                conn.execute(
                    "INSERT OR REPLACE INTO historical_position_snapshots ("
                    "as_of_date, account_external_id, description, "
                    "instrument_key, quantity, price, market_value, "
                    "percent_of_total, currency, source_sha256, payload"
                    ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                    (
                        as_of, aid, desc, instrument_key,
                        holding.get("quantity"),
                        holding.get("price"),
                        holding.get("market_value"),
                        holding.get("percent_of_total"),
                        "USD", sha,
                        normalize_payload(holding),
                    ),
                )
                inserted += 1
            except sqlite3.IntegrityError as e:
                log.debug(
                    "historical: insert skipped for %s @ %s: %s",
                    aid, period_end, e,
                )
    return inserted


def _crosswalk_description_to_instrument(conn, account_external_id,
                                          description):
    """Look the description up against any live ``positions.description``
    for the same account; return the matching ``instrument_key``
    when there's exactly one. NULL when the description never
    appears (e.g. fund was sold before any live snapshot ran) or
    when it's ambiguous."""
    cur = conn.execute(
        "SELECT DISTINCT instrument_key FROM positions "
        " WHERE account_external_id = ? AND description = ? "
        "   AND instrument_key IS NOT NULL "
        " LIMIT 2",
        (account_external_id, description),
    )
    hits = [r[0] for r in cur.fetchall()]
    if len(hits) == 1:
        return hits[0]
    return None


# ------------------------------------------------------------
# Supplied statements (legacy monthly statements)
# ------------------------------------------------------------

# Monthly supplied statements follow Fidelity's legacy naming
# convention: ``<Registration> <M>.<YY> Statement.PDF`` (e.g.
# ``Example 1.24 Statement.PDF`` for January 2024). Year-end
# statements in the same directory carry ``Year End`` between the
# registration and ``Statement.PDF`` (e.g. ``Example 2024 Year End
# Statement.PDF``) — those use a different per-asset-class layout
# the parser doesn't handle yet and are excluded here (the
# December monthly statement covers the same period end).
_SUPPLIED_STATEMENT_FILENAME_RE = re.compile(
    r"^[A-Za-z][A-Za-z\s]*?\s+\d{1,2}\.\d{2}\s+Statement\.pdf$",
    re.IGNORECASE,
)


_SIGNATURE_SIDECAR = "signature.txt"


def _read_signature_sidecar(supplied_dir):
    """Resolve the supplied-statement signature from
    ``<supplied_dir>/signature.txt`` (first non-empty, non-``#``-comment
    line). This keeps the account registration — which is PII — in the
    local data directory next to the PDFs, rather than in argv / shell
    history or a committed orchestration script. Returns None when
    the file is absent or carries no usable line."""
    sidecar = supplied_dir / _SIGNATURE_SIDECAR
    if not sidecar.is_file():
        return None
    try:
        for line in sidecar.read_text(encoding="utf-8").splitlines():
            s = line.strip()
            if s and not s.startswith("#"):
                return s
    except OSError as e:
        log.warning("supplied-statements: could not read %s: %s", sidecar, e)
    return None


def _supplied_pdf_candidates(supplied_dir):
    """Monthly supplied statement PDFs in ``supplied_dir`` (matching the
    ``<name> <M>.<YY> Statement.pdf`` convention), in the sorted
    order the rows are inserted. Empty when the dir is None/absent."""
    if supplied_dir is None or not supplied_dir.is_dir():
        return []
    return [
        p for p in sorted(supplied_dir.iterdir())
        if p.is_file() and _SUPPLIED_STATEMENT_FILENAME_RE.match(p.name)
    ]


# How far a statement row's date may sit from a scraped row's and
# still be the same event, in days. The two describe one payment from
# either side of the settlement, so a day or two of drift is ordinary;
# three is generous without being loose enough for two genuinely
# different payments of the same amount on one account to collide.
_ACTIVITY_MATCH_DAYS = 3

# A fee is the exception. The statement dates it when it is assessed and the
# feed when the cash is debited, so the two can sit further apart than other
# payments, and the window is wider for that reason. Two different fees of one
# signed amount, to the cent, on one account inside the window are left to the
# one-to-one claim to settle.
_FEE_MATCH_DAYS = 14


def _same_amount(statement, feed):
    """One signed amount to the cent. Signed, not absolute: a core
    redemption of +450.00 raises the cash that the -450.00 fee then
    spends, and those two must not cancel each other out."""
    return abs(feed - statement) < 0.005


def _match_window_days(leg):
    return _FEE_MATCH_DAYS if leg.kind == "FEE" else _ACTIVITY_MATCH_DAYS


def _feed_legs(conn):
    """The SCRAPED feed rows a statement row may match, by account.

    Statement-derived rows are left out. They converge on their own
    content-derived id instead, which is what lets the monthly and
    year-end statements both carry a row (the year-end repeats the whole
    year) without inserting it twice."""
    feed = {}
    for activity_id, account, ts, amount in conn.execute(
        "SELECT activity_id, account_external_id, timestamp, amount "
        "FROM transactions "
        "WHERE activity_id NOT LIKE 'stmt\\_%' ESCAPE '\\' "
        "  AND timestamp IS NOT NULL AND amount IS NOT NULL",
    ):
        feed.setdefault(account, []).append(
            channels.Leg(activity_id, account, ts, amount))
    return feed


def _claim_feed_match(leg, feed, claims):
    """The scraped feed row that already carries the statement row
    ``leg``, or None. A match is claimed in ``claims``.

    The statement and the feed number their rows differently and cannot
    be joined on an id, so the match is the only thing both agree on:
    `collectorkit.channels.pair_one_to_one` with one account, the same
    signed amount to the cent, and a window of `_ACTIVITY_MATCH_DAYS`,
    or `_FEE_MATCH_DAYS` for a fee.

    Only a feed row nothing has claimed counts. The match is a
    resemblance rather than an identity, so unclaimed, the one feed row
    carrying one of a same-day same-amount pair would absorb both and
    the payment the feed never carried would vanish. Candidates come
    closest-in-time first, then by id, because with claiming the pick
    is not arbitrary: which row is taken decides what the next statement
    row can still find.

    The match is taken one candidate at a time, so that each feed row
    passed over because another statement row claimed it is logged."""
    taken = set(claims.values())
    offered = feed.get(leg.account, [])
    while True:
        pick = channels.pair_one_to_one(
            [leg], offered, _same_amount, _match_window_days).get(leg.id)
        if pick is None or pick not in taken:
            break
        log.info(
            "supplied-statements: feed row %s already absorbed another "
            "statement row; %s %+.2f keeps looking",
            pick, bronze.log_id(leg.account), leg.amount,
        )
        offered = [r for r in offered if r.id != pick]
    if pick is not None:
        claims[leg.id] = pick
    return pick


def _insert_supplied_activity_rows(conn, pdf_path, parsed, sha, *, claims=None):
    """Insert the statement's ACCOUNT-LEVEL activity into
    ``transactions`` — the money in and out, and the account fees.

    Returns ``(inserted, skipped)``. Skipped rows are the ones the
    scraped feed already carries; see `_claim_feed_match`. `claims`
    is the pass-wide one-to-one ledger, ``{statement row id: feed row
    id}``; a caller that omits it gets a private one, which is right
    for a single statement read in isolation and wrong for a walk (the
    walk threads its own).

    Only rows the feed ABSORBED are claimed. A row that found no feed
    match is simply inserted, and its id converges on a re-sighting
    through INSERT OR REPLACE, so the ledger can never suppress an
    insert.

    The id is derived from the row's own content plus an occurrence
    index within the PDF, on the same reasoning as
    `_synthesise_activity_id`: one real payment printed on both the
    monthly and the year-end statement hashes the same and converges,
    while two genuinely distinct payments that share a day and an
    amount differ in their reference and payee, and so keep separate
    rows.
    """
    inserted = skipped = 0
    claims = {} if claims is None else claims
    feed = _feed_legs(conn)
    occurrence: dict[str, int] = {}
    for account in parsed.get("accounts", []):
        aid = account.get("account_external_id")
        if not aid:
            continue
        for row in account.get("activity", []):
            ts = iso_date_to_epoch(row.get("date"))
            amount = row.get("amount")
            if ts is None or amount is None:
                continue
            amount = float(amount)
            desc = (row.get("description") or "").strip()
            identity = "|".join(
                (aid, row["date"], row.get("section") or "", f"{amount:.2f}", desc))
            activity_id = _occurrence_id("stmt_", identity, occurrence)
            # Keyed on the STATEMENT row's own id, not the feed row's:
            # the year-end statement repeats the whole year, so this
            # payment may be offered again later in the pass and hashes
            # the same both times. The second sighting re-uses the claim
            # the first made rather than reaching for a second feed row.
            absorbed_by = claims.get(activity_id)
            if absorbed_by is None:
                absorbed_by = _claim_feed_match(
                    channels.Leg(activity_id, aid, ts, amount,
                                 row.get("section")),
                    feed, claims)
                if absorbed_by is not None:
                    # The description stays out of the line: on a wire it
                    # names the beneficiary. The account, date, section,
                    # signed amount and feed id locate the row.
                    log.info(
                        "supplied-statements: %s %s %s %+.2f is already in "
                        "the scraped feed as %s; not deriving it from %s",
                        aid, row["date"], row.get("section") or "other",
                        amount, absorbed_by, pdf_path.name,
                    )
            if absorbed_by is not None:
                # A run before the feed reached this date derived the row;
                # the feed carries it now, so that derivation goes.
                conn.execute("DELETE FROM transactions WHERE activity_id = ?",
                             (activity_id,))
                skipped += 1
                continue
            conn.execute(
                "INSERT OR REPLACE INTO transactions ("
                "activity_id, timestamp, account_external_id, kind, "
                "instrument_key, amount, currency, source_sha256, payload"
                ") VALUES (?, ?, ?, ?, NULL, ?, 'USD', ?, ?)",
                (
                    activity_id, ts, aid, row.get("section") or "other",
                    amount, sha,
                    # `Action` is the key gold's payloadNarrative
                    # reads, and the statement's own words are the
                    # action: "Wire Tfr To Bank <ref> <beneficiary>".
                    normalize_payload({
                        "Action": desc,
                        "section": row.get("section"),
                        "basis": "supplied_statement",
                        "statement": pdf_path.name,
                    }),
                ),
            )
            inserted += 1
    return inserted, skipped


def _insert_supplied_historical_rows(conn, pdf_path, parsed, sha):
    """Insert one ``historical_position_snapshots`` row per holding
    in the parsed supplied statement. That parser surfaces the
    ticker (or CUSIP) directly as ``instrument_key`` so no
    cross-walk against live ``positions`` is needed. ``sha`` is the
    source PDF's content hash, stored in ``source_sha256``."""
    period_end = parsed.get("period_end")
    if not period_end:
        log.debug(
            "supplied-statements: no period in %s; skipping", pdf_path.name,
        )
        return 0
    as_of = iso_date_to_epoch(period_end)
    if as_of is None:
        return 0
    inserted = 0
    for account in parsed.get("accounts", []):
        aid = account.get("account_external_id")
        if not aid:
            continue
        for holding in account.get("holdings", []):
            desc = (holding.get("description") or "").strip()
            if not desc:
                continue
            mv = holding.get("market_value")
            # The supplied statements print no portfolio total, so the
            # holding's share of it stays NULL.
            try:
                conn.execute(
                    "INSERT OR REPLACE INTO historical_position_snapshots ("
                    "as_of_date, account_external_id, description, "
                    "instrument_key, quantity, price, market_value, "
                    "percent_of_total, cost_basis, unrealized_gain_loss, "
                    "currency, source_sha256, payload"
                    ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                    (
                        as_of, aid, desc,
                        holding.get("instrument_key"),
                        holding.get("quantity"),
                        holding.get("price"),
                        mv, None,
                        holding.get("cost_basis"),
                        holding.get("unrealized_gain"),
                        "USD", sha,
                        normalize_payload(holding),
                    ),
                )
                inserted += 1
            except sqlite3.IntegrityError as e:
                log.debug(
                    "supplied-statements: insert skipped for %s @ %s: %s",
                    aid, period_end, e,
                )
    return inserted


# Default portfolio + management classification for an account
# synthesised from supplied historical statements. Supplied
# statements stand in for the web document center, which may serve
# no statement PDFs for the `Authorized` group (DESIGN.md §4.5), so
# the defaults are that group's: `_load_master` maps the selector
# label to portfolios.kind = 'trust_managed', and the gold adapter
# maps the kind to its wrapper and management style. The synthesised
# row pre-fills the same shape, so the account rolls up under that
# portfolio. Groups the web document center serves statements for
# need no synthesis.
_SUPPLIED_SYNTHETIC_PORTFOLIO = "Authorized"
_SUPPLIED_SYNTHETIC_KIND = "trust_managed"
_SUPPLIED_SYNTHETIC_MANAGEMENT = "discretionary"


def _synthesize_missing_account_masters(conn):
    """For every ``account_external_id`` mentioned in
    ``historical_position_snapshots`` but absent from
    ``accounts``, insert one synthetic accounts row at the
    account's latest historical ``as_of_date``. Gold's
    `appendHistoricalAccounts` joins on account_external_id alone
    (taking MAX(snapshot_at) per id), so a single row is enough
    for the master projection to fire.

    Returns the number of synthetic rows inserted. Idempotent —
    INSERT OR IGNORE means a re-run after the live download
    finally observes the account is a no-op (the live row's
    snapshot_at outranks the synthetic one, and gold uses MAX)."""
    cur = conn.execute("""
SELECT h.account_external_id, MAX(h.as_of_date)
  FROM historical_position_snapshots h
 WHERE NOT EXISTS (
       SELECT 1 FROM accounts a
        WHERE a.account_external_id = h.account_external_id
 )
 GROUP BY h.account_external_id
""")
    missing = cur.fetchall()
    if not missing:
        return 0
    payload = normalize_payload({"source": "supplied-statement-synthetic"})
    inserted = 0
    for aid, latest_as_of in missing:
        # Portfolio master too: the historical projection joins
        # accounts → portfolios on (snapshot_at, portfolio_external_id),
        # so the synthetic accounts row needs a same-snapshot_at
        # portfolio companion. INSERT OR IGNORE so it never tramples
        # a real portfolio row that the live loader already wrote.
        try:
            conn.execute(
                "INSERT OR IGNORE INTO portfolios ("
                "snapshot_at, portfolio_external_id, kind, payload"
                ") VALUES (?, ?, ?, ?)",
                (latest_as_of, _SUPPLIED_SYNTHETIC_PORTFOLIO,
                 _SUPPLIED_SYNTHETIC_KIND, payload),
            )
            conn.execute(
                "INSERT OR IGNORE INTO accounts ("
                "snapshot_at, account_external_id, portfolio_external_id, "
                "nickname, payload, management_style"
                ") VALUES (?, ?, ?, ?, ?, ?)",
                (latest_as_of, aid, _SUPPLIED_SYNTHETIC_PORTFOLIO,
                 None, payload, _SUPPLIED_SYNTHETIC_MANAGEMENT),
            )
            inserted += 1
            log.info(
                "supplied-statements: synthesised accounts master for "
                "%s @ %s (absent from every live download)",
                aid, latest_as_of,
            )
        except sqlite3.IntegrityError as e:
            log.debug(
                "supplied-statements: synthesise skipped for %s: %s", aid, e,
            )
    return inserted
