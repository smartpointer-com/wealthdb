#!/usr/bin/env python3
"""Provision a fresh Metabase over its loopback REST API: create the
admin account (skipping the "tell us about yourself" setup wizard),
pre-add the gold DuckDB database, and create the pre-defined report
models, questions and dashboards. Idempotent — safe to run on
every `wealthdb web start`. Standard library only.

Called by web/web; not meant to be run by hand (but it can be). The
admin password arrives in WEALTHDB_WEB_ADMIN_PASSWORD, never on the
command line, where `ps` would show it.

Exit status: 0 when provisioned, EXIT_STALE_SNAPSHOT (3) when the
mounted gold snapshot is older than the cards need (MIN_GOLD_SCHEMA),
and 1 on any other failure.
"""
import argparse
import concurrent.futures
import json
import os
import sys
import time
import urllib.error
import urllib.request


def req(base, path, method="GET", data=None, session=None, timeout=30):
    """(HTTP status, decoded JSON body) for one API call. A call that
    gets no HTTP answer at all returns (0, {})."""
    body = json.dumps(data).encode() if data is not None else None
    r = urllib.request.Request(base + path, data=body, method=method)
    r.add_header("Content-Type", "application/json")
    if session:
        r.add_header("X-Metabase-Session", session)
    try:
        with urllib.request.urlopen(r, timeout=timeout) as resp:
            raw = resp.read().decode()
            return resp.status, (json.loads(raw) if raw else {})
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            return e.code, json.loads(raw)
        except Exception:
            return e.code, {"message": raw}
    except OSError:
        # Connection refused or reset, and a timeout: urlopen raises a
        # connect timeout as URLError but a read timeout as TimeoutError.
        return 0, {}


def wait_health(base, tries=90, delay=3):
    for _ in range(tries):
        st, _ = req(base, "/api/health", timeout=5)
        if st == 200:
            return True
        time.sleep(delay)
    return False


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", required=True)
    ap.add_argument("--email", required=True)
    ap.add_argument("--gold-path", required=True)
    ap.add_argument("--db-name", default="gold")
    ap.add_argument("--default-currency", default="USD")
    ap.add_argument("--missing-basis", default=MISSING_BASIS_READINGS[0], choices=MISSING_BASIS_READINGS)
    a = ap.parse_args()
    password = os.environ.get("WEALTHDB_WEB_ADMIN_PASSWORD", "")
    if not password:
        print("provision: WEALTHDB_WEB_ADMIN_PASSWORD is not set", file=sys.stderr)
        return 1
    global DEFAULT_CURRENCY, DEFAULT_MISSING_BASIS
    DEFAULT_CURRENCY = default_currency(a.default_currency)
    DEFAULT_MISSING_BASIS = a.missing_basis

    if not wait_health(a.base):
        print("provision: Metabase did not become healthy in time", file=sys.stderr)
        return 1

    _, props = req(a.base, "/api/session/properties")
    setup_done = bool(props.get("has-user-setup"))
    token = props.get("setup-token")

    if not setup_done and token:
        st, body = req(a.base, "/api/setup", "POST", {
            "token": token,
            "user": {"first_name": "wealthdb", "last_name": "admin",
                     "email": a.email, "password": password},
            "prefs": {"site_name": "wealthdb", "allow_tracking": False},
        })
        if st not in (200, 201) or not body.get("id"):
            print(f"provision: setup failed ({st}): {body.get('message')}", file=sys.stderr)
            return 1
        print(f"provision: created admin {a.email}; setup wizard skipped")
    else:
        print("provision: already set up; skipping admin creation")

    # Log in to add the database. If setup was done earlier with a
    # different password (changed since in Metabase), don't fail the
    # start — the database was added on the first provision.
    _, body = req(a.base, "/api/session", "POST",
                  {"username": a.email, "password": password})
    sid = body.get("id")
    if not sid:
        if setup_done:
            print("provision: couldn't verify admin login (password changed?) — leaving as-is")
            return 0
        print(f"provision: login failed: {body.get('message')}", file=sys.stderr)
        return 1

    db_id = ensure_database(a.base, sid, a.db_name, a.gold_path)
    if db_id is None:
        return 1

    status = check_gold_schema(a.base, sid, db_id)
    if status:
        return status
    tables = ensure_synced(a.base, sid, db_id)
    if tables is None:
        return 1
    global FIELD_IDS, ASOF_DEFAULT
    FIELD_IDS = field_ids_from(tables)
    ASOF_DEFAULT = last_history_day(a.base, sid, db_id)
    unfingerprint_lists(a.base, sid, tables)

    coll_id = ensure_collection(a.base, sid, COLLECTION_NAME)
    if coll_id is None:
        return 1
    by_name = pre_defined_cards(a.base, sid, coll_id)
    if by_name is None:
        return 1
    model_ids = ensure_models(a.base, sid, db_id, coll_id, by_name)
    if model_ids is None:
        return 1
    saved = ensure_cards(a.base, sid, db_id, coll_id, by_name, model_ids)
    if saved is None:
        return 1
    card_ids, descs = saved
    titles = tile_titles(withholding_table(a.base, sid, db_id))
    return ensure_dashboards(a.base, sid, coll_id, card_ids, model_ids, titles, descs)


# The reporting currencies: the value column sets the `_multi` report
# macros and the web_* serving views carry (gold migration 0114), and the
# rows report_returns carries. Every currency list, CASE and picker below
# is generated from this one tuple.
REPORTING_CURRENCIES = ("USD", "CHF", "EUR", "GBP")

# The currency every picker and every native card's {{currency}}
# variable defaults to: wealthdb.cfg's default_currency when it is a
# reporting currency (main() sets it), USD otherwise.
DEFAULT_CURRENCY = "USD"

# The As of day picker's default: the last day the holdings history
# reaches, which main() reads off the snapshot (last_history_day). The
# history ends on the day the serving tables were last materialized, so
# a snapshot not refreshed today still shows its latest holdings.
# ASOF_FALLBACK where the snapshot does not answer.
ASOF_FALLBACK = "thisday"
ASOF_DEFAULT = ASOF_FALLBACK


def default_currency(configured):
    """The picker default for configured default currency `configured`."""
    ccy = (configured or "").strip().upper()
    if ccy in REPORTING_CURRENCIES:
        return ccy
    print(f"provision: default_currency {ccy or '(unset)'} is not a reporting "
          f"currency ({_ccy_words()}); the dashboards default to USD",
          file=sys.stderr)
    return "USD"


def _ccy_words():
    """The reporting currencies as prose: 'USD, CHF, EUR and GBP'."""
    return ", ".join(REPORTING_CURRENCIES[:-1]) + " and " + REPORTING_CURRENCIES[-1]


def _ccy_slash():
    """The reporting currencies as a compact list: 'USD/CHF/EUR/GBP'."""
    return "/".join(REPORTING_CURRENCIES)


def _ccy_lower():
    return tuple(c.lower() for c in REPORTING_CURRENCIES)


def _sql_list(values):
    """`values` as a list of SQL string literals."""
    return ", ".join(f"'{v}'" for v in values)


def _ccy_values():
    """The reporting currencies as an inline table, one row each."""
    return "(VALUES " + ", ".join(f"('{c}')" for c in REPORTING_CURRENCIES) + ") AS c(currency)"


def _ccy_switch(selector, leg):
    """`CASE <selector> WHEN '<CCY>' THEN <leg(ccy)> … END` over every
    reporting currency, `ccy` lower-case. There is no ELSE: a currency the
    CASE does not name renders blank rather than as another currency's
    figures."""
    arms = "".join(f" WHEN '{c}' THEN {leg(c.lower())}" for c in REPORTING_CURRENCIES)
    return f"CASE {selector}{arms} END"


def _ccy_pick(alias, col, pct=False):
    """`<alias>.<col>_<ccy>` for the row's own reporting currency `c.currency`
    — the unpivot of a wide value set into the long model shape. pct=True
    divides each leg by that currency's latest net worth."""
    return _ccy_switch("c.currency", lambda c: f"{alias}.{col}_{c}"
                       + (f" / nw.nw_{c}" if pct else ""))


def _ccy_case(col, neg=False):
    """The `col`_<ccy> column set reduced to the one the required
    {{currency}} variable names. A template variable interpolates a
    VALUE, never an identifier, so a native card picks its column with a
    CASE rather than by splicing a column name in. `neg` negates it, so
    a figure gold stores negative by convention — a spending outflow, a
    card's owed balance — reads as a positive one."""
    s = "-" if neg else ""
    return _ccy_switch("{{currency}}", lambda c: f"{s}{col}_{c}")


# Collection the pre-defined models live in (kept apart from anything the
# user builds by hand).
COLLECTION_NAME = "wealthdb (pre-defined)"

# Model names retired by renames and removals, each group noted where it
# is listed; archived on provision so a re-run cleans them up.
RETIRED_MODEL_NAMES = [
                       # Un-suffixed snapshot reports -> `_latest`
                       "report_global", "report_portfolios",
                       "report_accounts", "report_positions",
                       # Per-widget flow models: the privacy charts read
                       # the serving views natively.
                       "report_income_monthly_pct", "report_costs_monthly_pct",
                       # Privacy variants no card, picker or drill-through
                       # read: the privacy charts are native SQL over the
                       # web_* serving views.
                       "report_sources_history_pct", "report_accounts_history_pct",
                       "report_positions_history_pct", "report_transactions_pct",
                       "report_asset_classes_history_pct", "report_vehicles_history_pct"]

# report_returns' absolute money columns: the redacted model drops them,
# and only the base by-source table lists them.
RETURNS_MONEY_COLUMNS = ("start_value", "end_value", "net_flow")


def report_models():
    """model name -> (native SQL, description). The report_*_multi DuckDB
    macros (migration 0024) are the single source of truth. Each model
    wraps the serving table `web refresh` fills with a macro's output
    (gold migration 0132), a macro computing its whole output on every
    read, and renders epoch columns as TIMESTAMP (`epoch_ms` -> zone-free
    UTC, not `to_timestamp`, which is TIMESTAMPTZ and renders in the
    reading session's zone) for Metabase — except the taxonomy, spending,
    income and cashflow models, which wrap the web_* serving views
    (migrations 0032, 0043, 0049, 0072, 0084 and 0132) that do that
    rendering, the cash fold-in and the '(uncategorized)' labelling
    themselves. The macros already emit DECIMAL
    money/quantity columns and one value column set per reporting currency
    (REPORTING_CURRENCIES), so no value casting is needed here. The `_latest` reports
    are as of each account's latest snapshot; the `_history` reports carry value
    forward per day; the two `_pct` models are the privacy variants the privacy
    twins' MBQL cards run over (values as % of the latest global net worth,
    absolute-value columns dropped) — every other privacy chart is native SQL
    over the gold web_* serving views, recomputing its denominator per query
    so the dashboard pickers rescale it (see privacy_card_defs). The returns
    models wrap the materialized report_returns TABLE (migration 0026) instead
    of a macro, with the same epoch-to-TIMESTAMP rendering."""
    def wrap(from_expr, ts_cols=(), exclude=(), where=""):
        parts = [f"epoch_ms({c} * 1000) AS {c}" for c in ts_cols]
        excl = f" EXCLUDE ({', '.join(exclude)})" if exclude else ""
        cond = f" WHERE {where}" if where else ""
        return f"SELECT *{excl} REPLACE ({', '.join(parts)}) FROM {from_expr}{cond}"

    # Scalar subquery with the latest global net worth per currency — the
    # shared normalization constant of the privacy (_pct) models. A fixed
    # constant keeps every aggregate's shape; the privacy dashboards'
    # charts recompute a selection-aware denominator per query instead
    # (privacy_card_defs), so this constant only backs the twins' MBQL
    # cards and their drill-through. Guarded to NULL
    # when the latest total is zero or negative (empty or under-water
    # gold): dividing would render inf/NaN resp. sign-flipped
    # percentages, where NULL just blanks the values.
    NW_LATEST = ("(SELECT " + ", ".join(
        f"CASE WHEN total_value_{c} > 0 THEN total_value_{c} END AS nw_{c}"
        for c in _ccy_lower()) +
        " FROM serving_global_latest) AS nw")
    NW_COLS = [f"nw_{c}" for c in _ccy_lower()]

    def pct_wrap(from_expr, value_cols, ts_cols=(), exclude=()):
        """Privacy wrapper: every monetary column becomes % of the latest
        global net worth in its currency (one constant scale per
        currency, so every aggregate keeps its shape), and columns that
        would leak absolute values are dropped."""
        repl = [f"epoch_ms({c} * 1000) AS {c}" for c in ts_cols]
        repl += [f"{c} / nw.nw_{c.rsplit('_', 1)[1]} * 100 AS {c}"
                 for c in value_cols]
        excl = ", ".join([*NW_COLS, *exclude])
        return (f"SELECT * EXCLUDE ({excl}) REPLACE ({', '.join(repl)}) "
                f"FROM {from_expr}, {NW_LATEST}")

    def taxonomy_history(view):
        """Breakdown-by-taxonomy models over the gold web_* serving views
        (migration 0032), which already render as_of_day as TIMESTAMP and
        fold each source's cash balance into a class/vehicle of its own so
        the rows sum exactly to net worth."""
        return f"SELECT * FROM {view}"

    def spending(pct=False):
        """The spending models over the gold web_spending view (migration
        0043), which already renders occurred_at as TIMESTAMP and labels an
        unresolved category '(uncategorized)'.

        `spend_primary` and `spend_detailed` carry the DISPLAY LABEL
        (migration 0058), not the vendored value: a dashboard picker's
        dropdown is the list of labels it filters by, so the label is what
        a reader both sees and selects. The vendored values travel beside
        them as `*_id` for a filter that must survive a label being
        reworded. Swapping the two is safe for the cards that compare
        them, because the labels preserve the relation the comparison
        turns on: a delta's primary and detailed read the same, a vendored
        pair does not.

        `provider_category` is the ISSUER's own classification of the line
        (migration 0057), kept for a reader who wants the card provider's
        view. It is never summed with ours and no tile aggregates it — the
        two disagree by design.

        `display_name` carries the account LABEL (migration 0063) on the
        same reasoning the category columns carry theirs: an account's
        own name is what its institution calls it and no more — a
        product nickname, a masked card number, an IBAN — so a bar or a
        picker entry wearing it says neither which institution the
        account belongs to nor whether the money moved through a deposit
        account or a card, and such names collide across sources. The
        label annotates the name with both, as `<name> (<source>
        <kind>)`, and the view keeps the bare name and the id beside
        it.

        The wide value set is unpivoted to one row per (spending line,
        reporting currency) — the long shape report_returns already has,
        and the only shape an MBQL card can switch currency in: a dashboard
        picker selects rows, so it can land on a `currency` DIMENSION but
        can never choose which value COLUMN a card sums.

        pct=True is the privacy variant: `value` becomes % of the latest
        global net worth in the row's own currency (one constant scale per
        currency, so every aggregate keeps its shape), and merchant_name —
        the counterparty a drill-through must never surface — is dropped.
        The account's own display_name stays because the BASE dashboard's
        Account picker lands on this model too (its privacy-exempt tile
        runs over it); the privacy dashboard carries no account picker at
        all, since a picker's dropdown IS the list of labels it filters
        by (dashboard_parameters). The label is what stays, not the bare
        name: it is the only column either dashboard shows an account
        by."""
        cols = ("s.occurred_at, s.silver_source_id, s.account_external_id,\n"
                "       s.account_label AS display_name, s.account_kind,\n"
                + ("" if pct else "       s.merchant_name,\n") +
                "       s.spend_primary_label AS spend_primary,\n"
                "       s.spend_label         AS spend_detailed,\n"
                "       s.spend_primary AS spend_primary_id,\n"
                "       s.spend_detailed AS spend_detailed_id,\n"
                "       s.provider_spend_label AS provider_category, c.currency")
        val = _ccy_pick("s", "value", pct) + (" * 100" if pct else "")
        return (f"SELECT {cols},\n"
                f"       {val} AS value\n"
                "  FROM web_spending s,\n"
                f"       {_ccy_values()}"
                + (f",\n       {NW_LATEST}" if pct else ""))

    def income():
        """The income model over gold's web_income view.

        The spending model's shape with the income vocabulary, and the
        same reasoning behind every choice: the type columns carry the
        DISPLAY LABEL, with the vendored values beside them as `*_id`;
        `display_name` carries the account LABEL; and the wide value
        set is unpivoted to one row per (line, reporting currency),
        which is the only shape an MBQL card can switch currency in.

        There is no `pct=True` variant. Every income tile is native
        (they all read the serving view directly), so the privacy twin
        has no MBQL card that would need a redacted model to drill
        through to — unlike the spending twin, whose privacy-exempt
        scalar runs over one. This model exists for the two dashboard
        PICKERS, which need a card-backed value list.

        `value` keeps the canonical sign: a receipt positive, a reversal
        negative. The cards sum it as it stands, which is the one place
        the two families differ — spending negates."""
        cols = ("i.occurred_at, i.silver_source_id, i.account_external_id,\n"
                "       i.account_label AS display_name, i.account_kind,\n"
                "       i.payer_name,\n"
                "       i.income_primary_label AS income_primary,\n"
                "       i.income_label         AS income_detailed,\n"
                "       i.income_primary AS income_primary_id,\n"
                "       i.income_detailed AS income_detailed_id,\n"
                "       i.provider_income_label AS provider_category, c.currency")
        return (f"SELECT {cols},\n"
                f"       {_ccy_pick('i', 'value')} AS value\n"
                "  FROM web_income i,\n"
                f"       {_ccy_values()}")

    def cashflow():
        """The cashflow model over gold's web_cashflow view.

        The income model's shape with the statement's vocabulary: the
        node columns carry the DISPLAY names, the keys sit beside them
        as `*_id`, and the wide value set is unpivoted to one row per
        (line, reporting currency).

        Like the income model it exists for a dashboard PICKER — the
        Section one, which needs a card-backed value list — rather than
        for a card: every cashflow tile is native over the serving view,
        because the diagram's sides depend on the sign of a net over the
        filtered window and no MBQL card can express that.

        It projects no account label and no account id. The Cash Flow
        dashboard carries no account picker (docs/CASHFLOW.md §9), so a
        model column for one would exist only to be drilled into."""
        cols = ("f.occurred_at, f.silver_source_id, f.kind,\n"
                "       f.section,\n"
                "       f.class_node AS class,\n"
                "       f.group_node AS \"group\",\n"
                "       f.class AS class_id,\n"
                "       f.grp   AS group_id,\n"
                "       f.name, c.currency")
        return (f"SELECT {cols},\n"
                f"       {_ccy_pick('f', 'value')} AS value\n"
                "  FROM web_cashflow f,\n"
                f"       {_ccy_values()}")

    V3 = [f"{p}_{c}" for p in ("positions_value", "cash_balance", "total_value")
          for c in _ccy_lower()]
    BASE3 = ("positions_value_base", "cash_balance_base", "total_value_base")

    # Each source's newest snapshot, the latest mark it rides on
    # (valued_at, gold migration 0123) and the instant a run last read it
    # (gold source_observed, migration 0122): a quiet source is observed
    # through its latest download although its latest mark is older.
    # Unknown reads as the snapshot itself.
    sources_latest = (
        "(SELECT r.*, GREATEST(COALESCE(o.observed_snapshots_through, r.snapshot_at),"
        " r.snapshot_at) AS observed_at FROM serving_sources_latest r"
        " LEFT JOIN source_observed o USING (silver_source_id)) s")
    SOURCES_TS = ["snapshot_at", "valued_at", "observed_at"]
    return {
        "report_global_latest": (
            wrap("serving_global_latest", ["min_snapshot_at", "max_snapshot_at"]),
            "Whole-portfolio rollup as of the latest snapshot: cash, positions and "
            f"total value in {_ccy_words()} (one column set per currency), with the "
            "min/max snapshot date span. Mirrors `wealthdb holdings global`."),
        "report_sources_latest": (
            wrap(sources_latest, SOURCES_TS),
            "One row per silver source as of the latest snapshot: positions + cash "
            f"totalled in the source's base currency and in {_ccy_slash()}, with rolled-up "
            "tax wrapper / management style, valued_at, the latest mark the snapshot "
            "rides on, and observed_at, when a run last read the source. Mirrors "
            "`wealthdb holdings sources`."),
        "report_portfolios_latest": (
            wrap("serving_portfolios_latest", ["snapshot_at"]),
            "One row per portfolio as of the latest snapshot: positions + cash totalled "
            f"in the portfolio's base currency and in {_ccy_slash()}, with rolled-up tax "
            "wrapper / management style. Mirrors `wealthdb holdings portfolios`."),
        "report_accounts_latest": (
            wrap("serving_accounts_latest", ["snapshot_at"]),
            "One row per account as of the latest snapshot: positions + cash totalled "
            f"in the account's base currency and in {_ccy_slash()}, with kind, tax wrapper "
            "and management style. Mirrors `wealthdb holdings accounts`."),
        "report_positions_latest": (
            wrap("serving_positions_latest", ["snapshot_at"]),
            "One row per held position as of the latest snapshot, with market value in "
            f"{_ccy_words()}. Mirrors `wealthdb holdings positions`."),
        "report_transactions": (
            wrap("serving_transactions", ["occurred_at"]),
            f"Every transaction over all time, with net amount in {_ccy_words()} at the "
            "transaction date. Mirrors `wealthdb transactions` (filter the date range in "
            "Metabase)."),
        # History reports: one row per entity per UTC day, from the first snapshot to
        # today, value carried forward between snapshots — per ACCOUNT (per account and
        # currency for cash), so a run that covered only part of a source carries the
        # rest rather than dropping it (gold migration 0051). For time-series charts;
        # filter / aggregate by as_of_day. history@today reconciles with the matching
        # _latest report on totals for a source that writes every account in one run;
        # where runs are partial the history carries every account while _latest values
        # only the last run's. In report_positions_history, snapshot_at is the ACCOUNT's
        # active snapshot, so several values can share one source-day. Each reads the
        # serving table `web refresh` fills from its macro (gold migration 0132): a
        # macro computes the whole history on every read, and every tile of a
        # dashboard would pay for it at once. So do the latest models and the
        # transactions ones.
        "report_global_history": (
            wrap("serving_global_history", ["as_of_day"]),
            "Whole-portfolio value for every day from the first snapshot to today "
            f"(carried forward between snapshots), in {_ccy_words()}. The net-worth-"
            "over-time series — chart total_value_usd (or another currency's "
            "column) against as_of_day."),
        "report_sources_history": (
            wrap("serving_sources_history", ["as_of_day"]),
            "Per-silver-source value for every day (carried forward), in the source's "
            f"base currency and in {_ccy_slash()}. Filter to a source and chart against "
            "as_of_day."),
        "report_accounts_history": (
            wrap("serving_accounts_history", ["as_of_day"]),
            "Per-account value for every day (carried forward), in the account's base "
            f"currency and in {_ccy_slash()}. Filter to an account and chart against as_of_day."),
        "report_portfolios_history": (
            wrap("serving_portfolios_history", ["as_of_day"]),
            "Per-portfolio value for every day (carried forward), in the portfolio's base "
            f"currency and in {_ccy_slash()}. Filter to a portfolio and chart against as_of_day."),
        "report_positions_history": (
            wrap("serving_positions_history", ["as_of_day", "snapshot_at"]),
            f"Per-position value for every day (carried forward), in {_ccy_words()}. "
            "Large (days x held positions) — filter to a position / account / date range "
            "before charting."),
        # Returns models wrap the materialized report_returns TABLE (written
        # by the engine on `web refresh`) rather than a macro. The redacted
        # variant backs the privacy dashboards: twr/mwr are scale-free
        # ratios, so privacy = redaction (drop the absolute money columns
        # and the account/portfolio grains, whose labels would leak) rather
        # than share normalization.
        "report_returns": (
            wrap("report_returns", ["computed_at", "start_day", "end_day"]),
            "Materialized TWR/MWR returns, refreshed on every `wealthdb web "
            "refresh`: each (grain, granularity, currency) slice is the verbatim "
            "output of `wealthdb returns <grain> --period <granularity> --method "
            "both -x <currency>`. twr/mwr are ratios (0.07 = 7%); NULL means not "
            "computable, with the reason in quality. Bucket rows carry TWR only; "
            "MWR lives on the is_summary (since-inception) rows."),
        "report_returns_redacted": (
            wrap("report_returns", ["computed_at", "start_day", "end_day"],
                 exclude=RETURNS_MONEY_COLUMNS,
                 where="grain IN ('sources', 'global')"),
            "Privacy variant of report_returns: returns are scale-free ratios, "
            "so privacy is redaction rather than normalization — the absolute "
            "money columns are dropped and only the sources and global grains "
            "are kept (no account / portfolio labels)."),
        # The privacy (_pct) variant the twins' freshness table runs over:
        # same columns and grain, but monetary values are % of the latest
        # global net worth (per currency) and the base-currency totals,
        # which would leak absolute values, are dropped. The other is
        # report_spending_pct, under the spending tiles that are MBQL. An
        # MBQL card and its drill-through read a model, so these two stay;
        # every other privacy card is native SQL over the web_* serving
        # views (privacy_card_defs), whose denominators follow the pickers.
        "report_sources_latest_pct": (
            pct_wrap(sources_latest, V3, SOURCES_TS, BASE3),
            "Privacy variant of report_sources_latest: totals as % of the "
            "latest global net worth (per currency); base-currency columns "
            "dropped."),
        # Asset classes incl. cash, so the asset-class widget sums exactly
        # to net worth: positions grouped by class, with each source's
        # cash balance folded in as a 'cash' class (money-market-fund
        # positions and cash balances share it — see migration 0032).
        # Liability classes (e.g. mortgages) stay negative — the widget
        # must be a bar chart, not a pie (pies silently drop negative
        # slices).
        "report_asset_classes_history": (
            taxonomy_history("web_asset_classes_history"),
            "One row per asset class (incl. a 'cash' class) per source per "
            f"day, carried forward, in {_ccy_slash()}. Sums to net worth by "
            "construction; liability classes are negative."),
        # Vehicle (wrapper) breakdown — the second taxonomy dimension.
        # Same construction as asset classes: sums to net worth, cash
        # balances counted as a 'demand_deposit' vehicle.
        "report_vehicles_history": (
            taxonomy_history("web_vehicles_history"),
            "One row per vehicle (wrapper, incl. a 'demand_deposit' vehicle "
            f"for cash) per source per day, carried forward, in {_ccy_slash()}. "
            "Sums to net worth by construction."),
        # Spending: one row per spending line per reporting currency, so
        # the Spending dashboard's currency picker is a row filter (see
        # spending() above). `value` keeps the canonical sign — spend
        # negative, refunds positive — and the cards negate it, so a
        # month's outflow reads as a positive bar.
        "report_spending": (
            spending(),
            "Every spending line over all time — merchant (the merchant "
            "store's name where it holds one, otherwise the line's own "
            "normalized counterparty; blank on a cash withdrawal or a gift, "
            "and a bill on a card not itemised names the issuer it "
            "was paid to), resolved category (both "
            "levels, '(uncategorized)' when unknown) and account — with its "
            f"net amount in {_ccy_words()} carried as "
            "one row per currency (pick one with a `currency` filter). "
            "Spend is negative and a refund positive. Mirrors "
            "`wealthdb spending transactions`."),
        "report_income": (
            income(),
            "Every income line over all time — payer (the payer store's "
            "name where it holds one, the instrument on a dividend or a "
            "staking reward, otherwise the line's own normalized "
            "counterparty; blank on an own-account move or a gift, which "
            "have none), resolved type (both levels, '(uncategorized)' "
            "when unknown) and account — with its net amount in "
            f"{_ccy_words()} carried as one row per currency (pick one with a "
            "`currency` filter). A receipt is positive and a reversal "
            "negative. Mirrors `wealthdb income transactions`."),
        "report_cashflow": (
            cashflow(),
            "Every cashflow line over all time — the node it landed on "
            "(section, class and group, with the keys beside the display "
            "names), the name the line carries where it has one, and the "
            f"kind it was booked as — with its amount in {_ccy_words()} "
            "carried as one row per currency (pick one with a `currency` "
            "filter). Positive is cash arriving in the household's pool "
            "and negative is cash leaving it. Mirrors `wealthdb cashflow "
            "transactions`."),
        "report_spending_pct": (
            spending(pct=True),
            "Privacy variant of report_spending: values as % of the latest "
            "global net worth (per currency), and the merchant column "
            "dropped so a drill-through cannot surface a counterparty."),
    }


# ---- pre-defined questions and dashboards ----------------------------
# Like the report models, everything below is a content-free definition —
# MBQL over the models (referenced by card id) or native SQL over the gold
# serving views and macros; no source data is baked in. Provisioning
# converges these to spec on every start, so a user who wants to customize
# one should duplicate it into another collection first.

# The label gold's spending views carry for a line whose category the
# enrichment pass could not resolve. Rendered at the view (migration
# 0043) rather than left NULL, so a breakdown shows the backlog as a
# bucket instead of an unlabelled slice; the uncategorized-share card
# counts rows carrying it.
UNCATEGORIZED = "(uncategorized)"

# Card names retired by renames and removals, each group noted where it
# is listed; archived on provision so a re-run cleans them up.
RETIRED_CARD_NAMES = [
                      # Identifier-style metric names -> prose names
                      "net_worth_usd_current", "net_worth_chf_current",
                      "net_worth_eur_current", "positions_value_usd_current",
                      "cash_balance_usd_current", "net_worth_usd_daily",
                      # Top 10 -> Top 100 (with the inline asset-class filter)
                      "Top 10 positions (USD)", "Top 10 positions (% of peak)",
                      # Returns rework: start-year rescoping + growth chart +
                      # global-as-pseudo-source. Renamed/dropped cards and the
                      # privacy twins the percentage-only cards no longer need.
                      "Return since inception (TWR)", "Return since inception (MWR)",
                      "Returns by source since inception",
                      "Quarterly returns by source (TWR)",
                      "Return since inception (TWR) (privacy)",
                      "Return since inception (MWR) (privacy)",
                      "Annualized return (TWR) (privacy)",
                      "Quarterly returns (TWR) (privacy)",
                      "Monthly returns (TWR) (privacy)",
                      "Annual returns (TWR) (privacy)",
                      "Returns by source since inception (privacy)",
                      "Quarterly returns by source (TWR) (privacy)",
                      # "% of peak" -> "(privacy)": the privacy cards'
                      # normalization moved from a fixed %-of-all-time-peak
                      # to selection-aware shares of the latest total.
                      "Net worth (% of peak)", "Positions value (% of peak)",
                      "Cash balance (% of peak)",
                      "Net worth — monthly trend (% of peak)",
                      "Net worth over time (% of peak)",
                      "Cash vs positions over time (% of peak)",
                      "Income by month (% of peak)",
                      "Fees & taxes by month (% of peak)",
                      "Allocation by asset class (% of peak)",
                      "Allocation by vehicle (% of peak)",
                      "Allocation by currency (% of peak)",
                      "Value by tax wrapper (% of peak)",
                      "Value by management style (% of peak)",
                      "Top 100 positions (% of peak)",
                      "Source freshness (% of peak)",
                      # "Income by month (USD)" -> "Investment income by
                      # month (USD)": the Wealth Overview's tile is named
                      # for the four types it charts.
                      "Income by month (USD)",
                      # The log axis Metabase draws is a linear axis over
                      # log values, so its ticks fall between powers of ten.
                      "Cumulative return (log scale)",
                      # The Wealth Overview and Allocation tiles take a
                      # Currency picker, so their names drop the USD
                      # marker; the per-currency metrics go with them.
                      "Net worth (USD)", "Net worth (CHF)", "Net worth (EUR)",
                      "Positions value (USD)", "Cash balance (USD)",
                      "Net worth — monthly trend (USD)",
                      "Net worth over time (USD)",
                      "Cash vs positions over time (USD)",
                      "Investment income by month (USD)",
                      "Fees & taxes by month (USD)",
                      "Allocation by asset class (USD)",
                      "Allocation by vehicle (USD)",
                      "Allocation by currency (USD)",
                      "Value by tax wrapper (USD)",
                      "Value by management style (USD)",
                      "Top 100 positions (USD)",
                      # The Gains dashboard splits its gain by what was
                      # sold and what was kept.
                      "Unrealized change",
                      # The Exposure charts over time show shares alone,
                      # so the twin reuses them.
                      "Asset classes over time (privacy)",
                      "Currencies over time (privacy)",
                      "Held directly or through funds (privacy)",
                      "How the figures are classified (privacy)",
                      "Regions over time (privacy)",
                      # The Income dashboard's received and expected cards
                      # by month and by account, which follow the Time
                      # range, supersede the income lines' two.
                      "Income by month", "Income by month (privacy)",
                      "Income by account", "Income by account (privacy)",
                      # The Exposure debt and cycle cards are named for
                      # the class they count. The maturity card's name
                      # says the debt alone: it counts the debt and the
                      # cash, each by its term.
                      "Exposure by credit rating", "Exposure by credit rating (privacy)",
                      "Exposure by maturity", "Exposure by maturity (privacy)",
                      "Exposure by coupon", "Exposure by coupon (privacy)",
                      # The cash flow projection's cards are named for
                      # what they show: a projection, not a plan.
                      "Cash flow plan by month", "Cash flow plan by month (privacy)",
                      "Cash flow plan", "Cash flow plan (privacy)",
                      # The issuers card lists a hundred on Allocation, as
                      # the positions card beside it does.
                      "Largest issuers", "Largest issuers (privacy)",
                      # The Wealth Overview shows a figure from each
                      # dashboard: the income Income's, the fees and
                      # taxes Cash Flow's operating outflows.
                      "Investment income by month", "Investment income by month (privacy)",
                      "Fees & taxes by month", "Fees & taxes by month (privacy)",
                      # The asset classes over time, below the net worth
                      # chart, show the mix the cash and positions chart
                      # and the one-day split showed.
                      "Cash vs positions over time", "Cash vs positions over time (privacy)",
                      "Asset class split", "Asset class split (privacy)"]

# Dashboard names retired by renames; archived on provision so a re-run
# cleans them up.
RETIRED_DASHBOARD_NAMES = ["Net Worth"]

# The reserved source id declared accounts carry in gold
# (canonical.DeclaredSourceID in the engine).
DECLARED_SOURCE = "declared"

# The Wealth Overview's returns of the current year: (card, method, column).
RETURN_YTD_CARDS = (("Return year to date (TWR)", "time-weighted", "twr"),
                    ("Return year to date (MWR)", "money-weighted", "mwr"))

# Every dashboard but those in NO_PRIVACY_TWIN has a privacy twin whose
# cards show shares (%) instead of money (each names its own denominator
# — see PRIVACY_DESC). Cards listed here show no monetary values
# (percentages, indices, source names), so the twin reuses them as-is. The returns scalars and charts
# are all percentage/index-only; only the by-source table carries money.
PRIVACY_EXEMPT_CARDS = {"Stalest source (days)", "Returns age (days)", "Asset class mix over time",
                        *(c for c, _m, _col in RETURN_YTD_CARDS),
                        "Return (TWR)", "Return (MWR)", "Annualized return (TWR)",
                        "Cumulative return (TWR)", "Monthly returns (TWR)",
                        "Quarterly returns (TWR)", "Annual returns (TWR)",
                        # A share of rows, not of money — and it already
                        # runs over the _pct model, so its drill-through
                        # is leak-free too.
                        #
                        # The income twin of the same tile is NOT here.
                        # That card is native, so it carries its own
                        # field-filter template tags, and one of them is
                        # an `account` filter — which Metabase renders as
                        # a dropdown of account labels wherever the card
                        # is opened on its own. A percentage is safe to
                        # reuse; a widget listing the accounts is not, so
                        # the twin builds its own from
                        # PRIVACY_INCOME_FILTERS.
                        "Uncategorized share",
                        # The Exposure charts over time: each month end's
                        # shares of the selection, with no value column.
                        "Asset classes over time", "Regions over time",
                        "Currencies over time", "Held directly or through funds",
                        "How the figures are classified",
                        # The Yield cards of rates, years and securities
                        # alone: the selection's yields, a stock's
                        # dividend yield and payout, and their history.
                        "Income yield", "Implied yield", "Realized yield",
                        "Dividend yield against payout", "Yields over time",
                        "Duration over time"}

# Denominator-neutral by design: each card's body text names its own
# denominator (latest total, chosen day's total, peak month, or the
# window's own net spend).
PRIVACY_DESC = " Privacy view: values are shares (%), not absolute amounts."

# The uncategorised-income card, which the Income dashboard and its
# privacy twin draw identically: a share of ROWS is already
# privacy-safe, so the twin adds PRIVACY_DESC and changes nothing else.
# Both the words and the SQL live here so the two cannot drift apart.
IN_UNCATEGORIZED_DESC = (
    "Share of the window's income lines no tier and no kind floor "
    f"could place — the ones labelled '{UNCATEGORIZED}'. The backlog "
    "`wealthdb categorize income` works through. Most income is "
    "placed by its transaction kind, so this counts deposits. "
    "It is a share of rows, so it is the same in every currency and "
    "declares no currency variable; opened standalone it covers the "
    "whole history, where the dashboard's time filter defaults to "
    "the trailing twelve months.")
IN_UNCATEGORIZED_SQL = (
    "SELECT count(*) FILTER (WHERE income_label = "
    f"'{UNCATEGORIZED}')::DOUBLE\n       / nullif(count(*), 0) AS uncategorized_share\n"
    "  FROM web_income")

# The whole-portfolio scalars — global grain, so the Source picker doesn't
# apply (their silver_source_id is '').
RETURNS_GLOBAL_SCALARS = {"Return (TWR)", "Return (MWR)", "Annualized return (TWR)"}

RETURNS_PRIVACY_DESC = (" Privacy view: returns are scale-free ratios and "
                        "show unchanged; the absolute money columns are "
                        "redacted.")


# The suffix that marks a privacy variant — of a card, and of the
# dashboard it sits on. One home, because it is also how a dashboard
# tells itself apart from its twin (dashboard_parameters,
# ensure_dashboards).
PRIVACY_SUFFIX = " (privacy)"


def privacy_name(name):
    """Card title for the privacy variant of card `name`: a uniform
    '(privacy)' suffix."""
    return f"{name}{PRIVACY_SUFFIX}"


def _f(col, btype, unit=None):
    """Legacy-MBQL field reference by column name (native-query models
    expose no field ids)."""
    opts = {"base-type": btype}
    if unit:
        opts["temporal-unit"] = unit
    return ["field", col, opts]


def _dec(col):
    return _f(col, "type/Decimal")


def _mbql(db_id, model_id, clauses):
    q = {"source-table": f"card__{model_id}"}
    q.update(clauses)
    return {"type": "query", "database": db_id, "query": q}


def _percent_viz(*cols):
    """Viz settings rendering ratio columns (0.07 -> 7%) as percent.
    column_settings keys are matched literally against what Metabase's
    JSON.stringify produces, so they must carry no spaces."""
    return {"column_settings": {f'["name","{c}"]': {"number_style": "percent"}
                                for c in cols}}


def _share_viz(*cols, decimals=None):
    """Viz settings for columns already in percent (7.0 -> 7%): a
    percent sign, on the axis, the labels and the tooltip alike, and
    with `decimals` that many decimal places, where Metabase would print
    a small share to its first significant digits."""
    fmt = {"suffix": "%"} if decimals is None else {"suffix": "%", "decimals": decimals}
    return {"column_settings": {f'["name","{c}"]': fmt for c in cols}}


# The native returns charts carry three template tags — the currency and
# source field filters and the start year variable. The Returns dashboard's
# Currency / Start-year / Source pickers map onto them; the same pickers map
# onto the MBQL cards' currency / window_from_year / silver_source_id
# dimensions. Currency and start year default so a card still runs standalone.
START_YEAR_TAG = {"id": "year-tag", "name": "start_year",
                  "display-name": "Start year", "type": "number",
                  "default": "0", "required": True}


def currency_tag():
    """The required {{currency}} text variable: the one a native card
    picks its value column with (_ccy_case). It defaults to
    DEFAULT_CURRENCY, which is the configured one only once main() has
    run, so it is built on call."""
    return {"id": "ccy-tag", "name": "currency", "display-name": "Currency",
            "type": "text", "default": DEFAULT_CURRENCY, "required": True}


# The readings of a missing cost basis, the default first
# (lots.MissingBasisReadings), and the Gains dashboard's: wealthdb.cfg's
# lots.missing_basis (main() sets it). report_gains carries a row set
# per reading, as it does per currency.
MISSING_BASIS_READINGS = ("ignore", "zero")
DEFAULT_MISSING_BASIS = MISSING_BASIS_READINGS[0]


def missing_basis_tag():
    """The required {{missing_basis}} text variable of the Gains cards:
    ignore leaves a gain that needs a missing cost basis out, zero counts
    the missing cost as 0. Built on call, like currency_tag."""
    return {"id": "mb-tag", "name": "missing_basis", "display-name": "Missing cost basis",
            "type": "text", "default": DEFAULT_MISSING_BASIS, "required": True}


# The Cash Flow dashboard's investing grain. Required with a `whole`
# default, so a card opened away from the dashboard draws the section as
# one movement — the reading a household opens the statement with —
# rather than running with the variable cleared.
INVESTING_TAG = {"id": "inv-tag", "name": "investing",
                 "display-name": "Investing", "type": "text",
                 "default": "whole", "required": True}

# The gold columns backing the native cards' field filters: the returns
# charts filter report_returns; every other native card filters the
# web_* serving views (gold migrations 0032, 0043, 0072, 0084, 0113,
# 0117, 0127 and 0132). Field ids are per-Metabase-instance (assigned when
# the DB syncs), so main() resolves them at provision time into FIELD_IDS,
# and ensure_synced refuses to build a card while any is missing.
FILTER_FIELD_COLUMNS = {
    "report_returns": ("currency", "silver_source_id"),
    # The Wealth Overview's headline figures (migration 0113). Their time
    # filter lands on the snapshot, so a source whose latest snapshot
    # falls outside the window drops out of them. The holdings views carry
    # each line's account tax wrapper and asset class, the flow views the
    # tax wrapper (migration 0132).
    "web_sources_latest": ("snapshot_at", "silver_source_id", "tax_wrapper", "asset_class"),
    "web_sources_history": ("as_of_day", "silver_source_id", "tax_wrapper", "asset_class"),
    "web_asset_classes_history": ("as_of_day", "silver_source_id", "tax_wrapper", "asset_class"),
    "web_vehicles_history": ("as_of_day", "silver_source_id", "tax_wrapper", "asset_class"),
    "web_accounts_history": ("as_of_day", "silver_source_id", "tax_wrapper", "asset_class"),
    "web_positions_history": ("as_of_day", "silver_source_id", "tax_wrapper",
                              "asset_class", "vehicle"),
    # The spending views (migration 0043). On the money dashboard both
    # bind their account picker to account_label rather than
    # account_external_id — see dashboard_parameters for why the readable
    # column wins, and why the privacy twin carries no such picker.
    # spend_primary_label, not spend_primary: a field filter's widget is
    # a dropdown of the values its column takes, and the Category picker
    # is SHARED with the money dashboard, whose model column of the same
    # name holds the display label. Binding the two to different
    # vocabularies would leave the picker offering labels and the native
    # cards matching them against vendored values — every tile empty.
    "web_spending": ("occurred_at", "silver_source_id", "account_label",
                     "spend_primary_label"),
    # The cashflow view. No account column is bound: the Cash Flow
    # dashboard carries no account picker, because a picker that moved
    # accounts in and out of the cash pool would turn every crossing it
    # split into an unexplained disappearance.
    "web_cashflow": ("occurred_at", "silver_source_id", "section"),
    # The income view. `income_label`, not `income_primary_label`: the
    # income taxonomy has ONE vendored primary, so a primary-level
    # dropdown would offer a handful of values and hide every distinction
    # a reader opens the dashboard to filter by.
    "web_income": ("occurred_at", "silver_source_id", "account_label",
                   "income_label", "tax_wrapper"),
    "web_card_balances_history": ("as_of_day", "silver_source_id",
                                  "account_label"),
    # The gains view (migration 0117). Its currency is a row filter
    # through the {{currency}} variable, so no field filter binds it.
    "web_gains": ("period", "silver_source_id", "tax_wrapper"),
    # The exposure view (migration 0127): month ends and today, a row set
    # per currency like web_gains, and per source and account wrapper
    # (migration 0130). The pickers narrow those and each leaf's asset
    # class.
    "web_exposure": ("as_of_day", "silver_source_id", "tax_wrapper", "asset_class"),
    # The exposure of the debt and the largest issuers (migration 0138),
    # at the same instants. The debt view carries no asset class. No field
    # filter binds the issuers view: the Allocation card reaches its as-of
    # day through web_sources_history.
    "web_exposure_debt": ("as_of_day", "silver_source_id", "tax_wrapper"),
    "web_exposure_issuers": (),
    # The yield report per holding and its DV01 per tenor (migration
    # 0138): today and the twelve month ends before, a row set per
    # currency, narrowed like web_exposure.
    "web_yield": ("as_of_day", "silver_source_id", "tax_wrapper", "asset_class"),
    "web_yield_curve": ("as_of_day", "silver_source_id", "tax_wrapper", "asset_class"),
    # Today's projection, its rates, the principals maturities return and
    # the income received, and the tax a source withheld on the income's
    # accounts (migration 0146). No field filter binds them: a Yield card
    # narrows them to the holdings web_yield selects, an Income card
    # through the views its pickers list (INCOME_PROJECTED_SELECTION).
    "web_yield_parts": (),
    "web_yield_rates": (),
    "web_yield_events": (),
    "web_yield_received": (),
    "web_income_withheld": (),
    # The days the income received covers (migration 0146): the Income
    # cards' Time range lands on them, and a card keeps the received rows
    # and the withheld tax of the days it selects.
    "web_income_days": ("day",),
    # The cash flow projection's look-back parts (migration 0149), per
    # currency and month ahead, and the accounts of the cash pool its cards
    # split the income projection by. No field filter binds them: those
    # cards take no picker but the currency and the projection's four.
    "web_cashflow_plan": (),
    "web_cashflow_pool": (),
    # A stock position's size (migration 0154), joined onto the Top 100
    # positions cards by label. No field filter binds it.
    "web_position_sizes": (),
}
FIELD_IDS = {}          # (table, column) -> field id, filled by main()


def returns_tags():
    """Template tags for the native returns charts. Currency and Source are
    field filters on report_returns (a dropdown, values from the column).
    Start year is a plain number variable (it drives a `year(end_day) >= …`
    range, which a field filter can't express)."""
    # A field-filter default is a LIST (string/= is multi-value-shaped),
    # unlike a plain text variable's bare-string default.
    return {"currency": {"id": "ccy-ff", "name": "currency", "display-name": "Currency",
                         "type": "dimension",
                         "dimension": ["field", FIELD_IDS[("report_returns", "currency")], None],
                         "widget-type": "string/=", "default": [DEFAULT_CURRENCY],
                         "required": True},
            "start_year": START_YEAR_TAG,
            "source_ff": {"id": "src-ff", "name": "source_ff",
                          "display-name": "Source", "type": "dimension",
                          "dimension": ["field", FIELD_IDS[("report_returns", "silver_source_id")],
                                        None],
                          "widget-type": "string/=", "default": None}}


def _native(db_id, sql, tags):
    """A native-SQL dataset_query with template variables."""
    return {"type": "native", "database": db_id,
            "native": {"query": sql, "template-tags": tags}}


# The currency and source clauses of the native returns charts: the
# currency field filter inside each UNION arm, and the source one as an
# optional [[…]] clause, omitted when nothing is selected.
RETURNS_CCY_CLAUSE = "\n     AND {{currency}}"
RETURNS_SOURCE_CLAUSE = "\n     [[AND {{source_ff}}]]"


def _returns_source_union(granularity, value_col):
    """A UNION selecting one column from the per-source rows plus the global
    grain relabelled as the toggleable '(all sources)' pseudo-source, for one
    granularity's per-period buckets. Filtered to {{currency}} and to periods
    ending on/after {{start_year}} (0 = all). The Source field filter
    narrows the real sources but never the '(all sources)' line, so the
    global reference stays visible while a subset is selected."""
    return (
        "WITH s AS (\n"
        "  SELECT silver_source_id, currency, end_day, " + value_col + " AS v\n"
        "    FROM report_returns\n"
        "   WHERE grain = 'sources' AND granularity = '" + granularity + "'\n"
        "     AND NOT is_summary" + RETURNS_CCY_CLAUSE + RETURNS_SOURCE_CLAUSE + "\n"
        "  UNION ALL\n"
        "  SELECT '(all sources)', currency, end_day, " + value_col + "\n"
        "    FROM report_returns\n"
        "   WHERE grain = 'global' AND granularity = '" + granularity + "'\n"
        "     AND NOT is_summary" + RETURNS_CCY_CLAUSE + ")\n"
        "SELECT silver_source_id AS source, end_day, v\n"
        "  FROM s\n"
        " WHERE year(epoch_ms(end_day * 1000)) >= {{start_year}}")


def returns_period_sql(granularity):
    """Per-period TWR, one row per (source, period) plus the '(all sources)'
    global line — the pseudo-source that fixes the split-by-source
    inconsistency (every chart shows sources and the global line together)."""
    return ("SELECT source, epoch_ms(end_day * 1000) AS period, v AS twr\n"
            "  FROM (\n" + _returns_source_union(granularity, "twr") + "\n) u\n"
            " ORDER BY end_day")


def returns_growth_sql():
    """Cumulative return since the start of the earliest visible year, per
    source and the '(all sources)' line, derived from the ENGINE's since-<year>
    windowed TWRs — NOT by chaining the per-period buckets. Chaining
    calendar-month/quarter Modified-Dietz returns is unsound here: a flow
    landing between two sparse snapshots poisons that bucket (a mid-month
    deposit with no fresh snapshot reads as a huge loss, then a huge gain next
    period), so a chained index can diverge by hundreds of points from the true
    TWR for sparse-snapshot sources — a real gainer chained all the way down to
    a spurious near-total loss. The windowed summaries use the engine's
    snapshot-aligned chain, so they are correct and — being the very figures
    the scalars and by-source table show — the chart agrees with them by
    construction.

    Since window_from_year=Y is the TWR from Jan 1 Y to today, growth from the
    start of Ymin to the start of Y is (1 + TWR_since_Ymin) / (1 + TWR_Y), and
    the cumulative return is that minus 1. {{start_year}} sets Ymin (the line
    rebases to the chosen start); null windows (degenerate inception) drop out,
    so the line begins where the return is first defined. Annual granularity —
    one point per year — is the price of correctness here; a finer curve would
    need per-month windowed summaries."""
    return (
        "WITH w AS (\n"
        "  SELECT silver_source_id AS source, currency, window_from_year AS yr, twr\n"
        "    FROM report_returns\n"
        "   WHERE grain = 'sources' AND granularity = 'total' AND is_summary\n"
        "     AND window_from_year > 0" + RETURNS_CCY_CLAUSE + RETURNS_SOURCE_CLAUSE + "\n"
        "  UNION ALL\n"
        "  SELECT '(all sources)', currency, window_from_year, twr\n"
        "    FROM report_returns\n"
        "   WHERE grain = 'global' AND granularity = 'total' AND is_summary\n"
        "     AND window_from_year > 0" + RETURNS_CCY_CLAUSE + "),\n"
        "f AS (\n"
        "  SELECT source, yr, twr FROM w\n"
        "   WHERE yr >= {{start_year}} AND twr IS NOT NULL)\n"
        "SELECT source, make_date(yr, 1, 1) AS year,\n"
        "       first_value(1 + twr) OVER (PARTITION BY source ORDER BY yr)\n"
        "           / (1 + twr) - 1 AS cumulative_return\n"
        "  FROM f")


def _returns_part(grain, granularity):
    """Filter to one (grain, granularity) partition of report_returns
    (the returns scalars/table use the summary 'total' partition; the
    Start-year picker then selects the window_from_year within it)."""
    return ["and", ["=", _f("grain", "type/Text"), grain],
            ["=", _f("granularity", "type/Text"), granularity]]


def _returns_by_source(db_id, model_id, money):
    """The by-source returns table's query over model `model_id`: each
    source's returns and quality flags, and with `money` the absolute
    money columns (RETURNS_MONEY_COLUMNS) too."""
    return _mbql(db_id, model_id,
                 {"filter": _returns_part("sources", "total"),
                  "fields": [_f("silver_source_id", "type/Text"),
                             _f("entity_label", "type/Text"),
                             *(_f(c, "type/Float")
                               for c in ("twr", "twr_annualized", "mwr", "mwr_annualized")),
                             *(_dec(c) for c in RETURNS_MONEY_COLUMNS if money),
                             _f("quality", "type/Text")],
                  "order-by": [["asc", _f("silver_source_id", "type/Text")]]})


def _series_order(sql, t, series, metric, by, largest_first=False):
    """A chart's query with its rows ordered by series, since Metabase
    takes the series in the order their rows first appear. `by` ranks a
    series by its value at the last `t` ("latest", a level that carries
    on) or by its sum over the window ("total", a flow). A stacked chart
    wants the smallest first: the band whose rows appear last lands on
    the axis, so the largest band sits at the bottom and, with the
    legend's default reversal switched off (STACKED_VIZ), the legend
    lists the largest first. A line chart wants `largest_first`, the
    legend's own order, top line first. A series with no value at the
    last `t` ranks last. `sql` ends without an ORDER BY, and `t`,
    `series` and `metric` are its column names; the result keeps its
    columns.

    The rank is an aggregate per series joined back, not a window: the
    DuckDB the Metabase driver bundles fails on a window partitioned by
    a constant over no rows, which is what a picker that contradicts a
    card's own filter leaves."""
    if by == "latest":
        rank = f"max(CASE WHEN {t} = (SELECT max({t}) FROM q) THEN {metric} END)"
    else:
        rank = f"sum({metric})"
    order = "series_rank DESC NULLS LAST" if largest_first else "series_rank NULLS FIRST"
    return (f"WITH q AS (\n{sql}\n),\n"
            f"ranks AS (SELECT {series} AS series_key, {rank} AS series_rank FROM q GROUP BY 1)\n"
            f"SELECT q.*\n"
            f"  FROM q LEFT JOIN ranks ON ranks.series_key IS NOT DISTINCT FROM q.{series}\n"
            f" ORDER BY {order}, {t}")


# A line chart with one line per series: the legend in the lines' order.
LINES_VIZ = {"legend.is_reversed": False}


# A stacked chart's viz: bands stacked, the legend in the bands' own
# order (Metabase reverses a stacked chart's legend by default). A
# stack of metrics rather than bands is the mirror case: the FIRST
# metric sits at the bottom, so such a card lists its metrics largest
# first and the legend then reads the same way.
STACKED_VIZ = {"stackable.stack_type": "stacked", "legend.is_reversed": False}


def _series_viz(time_col, series_col, metric, *, percent=False):
    """Viz for a native time series split by a category: x = time_col,
    one line per series_col, y = metric. Native queries need the axes named
    explicitly (there is no MBQL breakout for Metabase to infer them from)."""
    viz = {"graph.dimensions": [time_col, series_col], "graph.metrics": [metric]}
    if percent:
        viz.update(_percent_viz(metric))
    return viz


def _donut(threshold=0, total=True):
    """Viz for a `pie` card, which Metabase draws as a ring: the total in
    the hole, and each slice's share in the legend rather than crowded
    onto the ring itself.

    `threshold` is the share (%) below which slices fold into a single
    wedge Metabase labels "Other". Zero — the default here, not
    Metabase's — draws every slice, and is right wherever the breakout
    has few enough values to name: this taxonomy HAS a category called
    "Other", the `other` delta, and two legend entries by that name read
    as a rendering fault. The income catch-all, INCOME_OTHER, reads
    "Other income" and draws as a slice of its own. Set it only where
    the tail is genuinely too long to draw, and say so on the card.

    `total` draws the figure in the hole, which is the sum of the
    slices the ring DREW rather than of the rows the query returned.
    A ring cannot draw a negative slice, so on a breakdown where a
    bucket can go net-negative — refunds beating purchases over the
    window — the hole runs over the true total by exactly what it
    left out. Turn it off wherever it would carry that error without
    carrying anything else: on a breakdown already expressed as
    shares the hole reads ~100 by construction, so it restates the
    description's own sentence and is wrong while doing it."""
    return {"pie.show_total": total,
            "pie.percent_visibility": "legend",
            "pie.slice_threshold": threshold,
            # Metabase migrates settings saved without a version, and that
            # moves the percentages from the legend onto the slices.
            "version": 2}


def allocation_note():
    """The sentence an Allocation card's description ends with, on the
    base dashboard and the twin alike."""
    return (" Built for the Allocation dashboard, which supplies the as-of "
            f"day; opened standalone it runs in {DEFAULT_CURRENCY}, the "
            "currency variable's default, and sums every day, so set the As "
            "Of Day filter to a single day first.")


# The month a flow line falls in, as a zone-free TIMESTAMP column: the
# x-axis of every by-month flow chart.
OCCURRED_MONTH = "CAST(date_trunc('month', occurred_at) AS TIMESTAMP) AS month"

# A source is as stale as its last read, not its latest mark: a portal
# that marks quarterly and was read today is current, and the days since
# its latest mark are days it has been quiet. The mark, not the snapshot:
# a read that found a holding gone dates a snapshot at the read while
# the marks keep their dates.
DAYS_STALE = ["datetime-diff", _f("observed_at", "type/DateTime"), ["now"], "day"]
DAYS_QUIET = ["datetime-diff", _f("valued_at", "type/DateTime"),
              _f("observed_at", "type/DateTime"), "day"]
# The declared accounts (config `declared_accounts`) sit under their own
# reserved source, which has no feed and so no snapshot: its latest
# snapshot reads as the epoch, and counted here it would be the stalest
# source every day. Freshness is about feeds, so it is left out.
FED_SOURCES = ["!=", _f("silver_source_id", "type/Text"), DECLARED_SOURCE]


def source_freshness_card(db_id, model_id):
    """The Data Freshness table over sources model `model_id`: (display,
    description, dataset_query, viz). The twin runs it over the _pct
    model under a description of its own."""
    return ("table",
            "Per source: when a run last read it and the days since, its "
            "latest mark and the days it has been quiet (read with "
            f"nothing new), and the value riding on it ({DEFAULT_CURRENCY}).",
            _mbql(db_id, model_id,
                  {"expressions": {"days_stale": DAYS_STALE, "days_quiet": DAYS_QUIET},
                   "filter": FED_SOURCES,
                   "fields": [_f("silver_source_id", "type/Text"),
                              _f("observed_at", "type/DateTime"),
                              ["expression", "days_stale"],
                              _f("valued_at", "type/DateTime"),
                              ["expression", "days_quiet"],
                              _dec(f"total_value_{DEFAULT_CURRENCY.lower()}")],
                   "order-by": [["desc", ["expression", "days_stale"]]]}),
            {})


def question_defs(db_id, mid):
    """question name -> (display, description, dataset_query, viz
    settings). A tile that sums money in the dashboard's chosen currency
    is native SQL over a gold serving view: a picker selects rows and
    never a column, so the required {{currency}} variable picks the
    value column with a CASE, and the other pickers land as field
    filters. The returns charts are native too, for their window
    functions and pseudo-source UNION. The rest are MBQL over the
    models: the returns scalars and table (report_returns carries a row
    set per currency, so there the Currency picker is a row filter), the
    freshness cards, and the spending tiles that need no currency
    variable."""
    def returns_native(name, desc, sql, viz):
        """A native returns chart, its pickers registered from its tags:
        Currency and Source land as dimensions, Start year as a
        variable."""
        tags = returns_tags()
        register_native_targets(name, tags, [(CURRENCY_PARAM_ID, "currency"),
                                             (START_YEAR_PARAM_ID, "start_year"),
                                             (SOURCE_PARAM_ID, "source_ff")])
        return ("line", desc, _native(db_id, sql, tags), viz)

    # Spending cards: `value` carries gold's canonical sign (spend
    # negative, refunds positive), so every card charts its negation,
    # `net_spend`, and a month's outflow reads as a positive bar. The
    # long-format model means a card that runs without a currency filter
    # sums every reporting currency, hence the standalone note.

    def ccy_spend(ccy):
        """Net spend in ONE reporting currency, as a named column.

        The long model carries a row per (line, currency), so a card that
        wants them all at once cannot take the Currency picker — a row
        filter would empty every column it does not select. Each
        column instead carries its own currency predicate, which makes
        the card right on the dashboard and right opened standalone,
        where no picker reaches it."""
        return ["aggregation-options",
                ["sum-where", ["*", _dec("value"), -1],
                 ["=", _f("currency", "type/Text"), ccy]],
                {"name": ccy, "display-name": ccy}]
    spend_note = (" Built for the Spending dashboard, which supplies the "
                  "currency; opened standalone, filter currency to a single "
                  "value first — the model carries one row per currency.")
    # The native card reads its currency from a required template
    # variable instead, so it has a working default of its own.
    native_note = (" Built for the Spending dashboard; opened standalone it "
                   f"runs in {DEFAULT_CURRENCY}, the currency variable's default.")
    income_native_note = (" Built for the Income dashboard; opened standalone "
                          f"it runs in {DEFAULT_CURRENCY}, the currency variable's default.")
    bal_tags = native_tags("web_card_balances_history", CARD_BALANCE_FILTERS)
    register_native_targets("Card balances over time", bal_tags,
                            CARD_BALANCE_PICKERS)

    # The spending tiles read the serving view natively, the way the
    # privacy twin's do. The reason is the Currency picker: the
    # long-format model carries a row per (line, reporting currency), so
    # an MBQL tile over it is right only while a row filter holds it to
    # one — which the dashboard supplies and nothing else does. Opened on
    # its own, such a tile would sum every currency, silently and
    # plausibly. A template VARIABLE is substituted by the picker rather
    # than ANDed with the tile's own filters, and {{currency}} defaults
    # to the configured default currency, so a native tile reads the
    # dashboard's choice on the dashboard and the default anywhere else.
    #
    # What it costs is MBQL drill-through: a native result has no "see
    # these records". The merchant ranking keeps its by staying MBQL: it
    # aggregates, so a per-currency column collapses its duplicate rows
    # (ccy_spend).
    sp_where, sp_val, spend_native = _family_native_kit(
        db_id, "web_spending", SPEND_FILTERS, SPEND_PICKERS, True, native_note)

    # The income tiles are the same kit over the other serving view.
    # `neg` is the whole of the sign difference: gold stores an outflow
    # negative and a receipt positive, and both families report a
    # positive magnitude.
    in_where, in_val, income_native = _family_native_kit(
        db_id, "web_income", INCOME_FILTERS, INCOME_PICKERS, False, income_native_note)

    # The cashflow tiles are the same kit over the third serving view
    # (_cashflow_kit), plus one template variable of its own:
    # {{investing}}, the display grain of the investing section. It is a
    # VARIABLE and not a field filter because it changes how rows are
    # GROUPED rather than which rows are selected — `whole` nets the
    # section into one Investments node, `class` opens it into one node
    # per asset class — and a field filter can only narrow a population.
    #
    # THE SECTION PICKER REACHES ONLY THE CARDS IT MEANS SOMETHING ON.
    # A card that does not declare the {{section}} tag cannot be bound
    # to the picker, so the scope is expressed once, in the kit, rather
    # than as a rule a later card has to remember. The headline figures and
    # the diagram decline it: a statement whose sections have been
    # narrowed to one is not a statement, its savings rate and yield
    # share have lost a leg of their own ratio, and a one-section
    # diagram draws a Cash node that absorbs the whole section rather
    # than the residual it names (docs/CASHFLOW.md §9).
    cf_note = (" Built for the Cash Flow dashboard; opened standalone it "
               f"runs in {DEFAULT_CURRENCY} with investing netted as a whole, the two "
               "variables' defaults.")
    cf_where, cf_where_nosec, cf_val, cashflow_native = _cashflow_kit(
        db_id, CASHFLOW_FILTERS, cf_note)

    # The uncategorised-share tile's tags: every filter but the
    # currency variable, which a share of rows has no use for.
    in_share_tags = view_tags("web_income", INCOME_FILTERS)
    register_native_targets("Uncategorized income share", in_share_tags,
                            [t for t in INCOME_PICKERS if t[1] != "currency"])

    # The Wealth Overview and Allocation tiles read the serving views
    # natively, for the reason the spending tiles do: the Currency picker
    # has to choose a value COLUMN, which only a template variable can.
    # Each card reads the view at the grain it charts. The year-to-date
    # returns read report_returns, a row set per currency, where
    # {{currency}} filters rows.
    wo_note = (" Built for the Wealth Overview; opened standalone it runs "
               f"in {DEFAULT_CURRENCY}, the currency variable's default.")
    al_note = allocation_note()
    lat_tags = native_tags("web_sources_latest", holdings_filters("snapshot_at"))
    hist_tags = native_tags("web_sources_history", holdings_filters("as_of_day"))
    pos_tags = native_tags("web_positions_history", POSITION_FILTERS)
    cls_tags = native_tags("web_asset_classes_history", holdings_filters("as_of_day"))
    ccy_tags = {"currency": currency_tag()}

    def overview_native(name, tags, display, desc, sql, viz):
        register_native_targets(name, tags, OVERVIEW_PICKERS)
        return (display, desc + wo_note, _native(db_id, sql, tags), viz)

    def latest_scalar(name, col, desc):
        """A headline figure: one value column summed over the selected
        sources' latest holdings."""
        return overview_native(name, lat_tags, "scalar", desc,
            f"SELECT sum({_ccy_case(col)})::DOUBLE AS {col}\n"
            "  FROM web_sources_latest" + _native_where(lat_tags), {})

    def breakdown(name, view, dim, col, display, desc, viz=None):
        """An Allocation breakdown: `col` summed by `dim` over the as-of
        day's rows of serving view `view`, largest first."""
        tags = native_tags(view, ASOF_FILTERS)
        register_native_targets(name, tags, ALLOCATION_PICKERS)
        return (display, desc + al_note,
                _native(db_id, _breakdown_sql(view, dim, col, tags), tags),
                viz if viz is not None else
                {"graph.dimensions": [dim], "graph.metrics": ["value"]})

    register_native_targets("Top 100 positions", pos_tags, POSITION_PICKERS)

    twr = _f("twr", "type/Float")
    return {
        # Net worth = positions + cash by construction, so the three
        # headline figures reconcile exactly. They read the latest
        # holdings, like `wealthdb holdings sources`; the charts below read
        # the daily history, which agrees with them on each day.
        "Net worth": latest_scalar("Net worth", "total_value",
            "Total net worth in the chosen currency as of the latest "
            "snapshot (cash + positions across the selected sources)."),
        "Positions value": latest_scalar("Positions value", "positions_value",
            "Market value of all positions in the chosen currency as of "
            "the latest snapshot."),
        "Cash balance": latest_scalar("Cash balance", "cash_balance",
            "Total cash balance in the chosen currency as of the latest "
            "snapshot."),
        "Net worth — monthly trend": overview_native(
            "Net worth — monthly trend", hist_tags, "smartscalar",
            "Average daily net worth of the latest month, with the change "
            "vs the month before.",
            "SELECT CAST(date_trunc('month', as_of_day) AS TIMESTAMP) AS month,\n"
            f"       sum({_ccy_case('total_value')})::DOUBLE"
            " / count(DISTINCT as_of_day) AS net_worth\n"
            "  FROM web_sources_history" + _native_where(hist_tags) +
            "\n GROUP BY 1\n ORDER BY 1",
            {}),
        "Net worth over time": overview_native(
            "Net worth over time", hist_tags, "area",
            "Net worth for every day since the first snapshot (value "
            "carried forward between snapshots), stacked by source; the "
            "envelope is total net worth.",
            _series_order("SELECT as_of_day, silver_source_id,\n"
                     f"       sum({_ccy_case('total_value')})::DOUBLE AS net_worth\n"
                     "  FROM web_sources_history" + _native_where(hist_tags) +
                     "\n GROUP BY 1, 2", "as_of_day", "silver_source_id", "net_worth", "latest"),
            {**_series_viz("as_of_day", "silver_source_id", "net_worth"), **STACKED_VIZ}),
        # Shares, not money, so the twin shows the card as it is.
        "Asset class mix over time": overview_native(
            "Asset class mix over time", cls_tags, "area",
            "Each day's net worth by asset class, as shares of that day's "
            "total that stack to 100: each class's part of the value "
            "Allocation by asset class shows as of that day. The largest "
            "class at the latest day sits at the bottom. The Asset class "
            "picker narrows the mix to the chosen classes, which then sum to "
            "100. A class reads below zero where a debt outweighs what it is "
            "owed on, and the others then reach above 100. A day whose total "
            "is not positive shows nothing.",
            _series_order(
                "WITH v AS (\n"
                f"  SELECT as_of_day, asset_class, sum({_ccy_case('value')})::DOUBLE AS value\n"
                "    FROM web_asset_classes_history" + _native_where(cls_tags) + "\n"
                "   GROUP BY 1, 2),\n"
                "t AS (SELECT as_of_day, sum(value) AS total FROM v GROUP BY 1)\n"
                "SELECT v.as_of_day, v.asset_class, v.value / t.total * 100 AS share_pct\n"
                "  FROM v JOIN t ON t.as_of_day = v.as_of_day\n"
                " WHERE t.total > 1e-9", "as_of_day", "asset_class", "share_pct", "latest"),
            # Stacked, not normalized: Metabase pins a normalized stack's
            # axis to 0..100%, which drops a negative band and clips the
            # bands above it. The shares already sum to 100.
            {**_series_viz("as_of_day", "asset_class", "share_pct"), **STACKED_VIZ,
             **_share_viz("share_pct")}),
        **{name: overview_native(name, ccy_tags, "scalar",
               f"Whole-portfolio {method} return since 1 January of the latest year "
               "the returns cover, this year once a refresh has read it: the Returns "
               "dashboard's figure with the Start year set to that year.",
               "WITH g AS (\n"
               f"  SELECT window_from_year, {col}\n"
               "    FROM report_returns\n"
               "   WHERE grain = 'global' AND granularity = 'total'\n"
               "     AND currency = {{currency}})\n"
               f"SELECT {col}, window_from_year AS start_year\n"
               "  FROM g\n"
               " WHERE window_from_year = (SELECT max(window_from_year) FROM g)",
               {**_percent_viz(col), "scalar.field": col})
           for name, method, col in RETURN_YTD_CARDS},
        # The Allocation tiles read the daily-history views so the
        # dashboard can show holdings as of any chosen day, the history's
        # last day included. The dashboard supplies the required as-of day.
        "Allocation by asset class": breakdown("Allocation by asset class",
            "web_asset_classes_history", "asset_class", "value", "row",
            "Value by asset class as of a day, including a 'cash' class — "
            "the bars sum exactly to net worth; liability classes (e.g. "
            "mortgages) show as negative bars, which is why this is a bar "
            "chart and not a pie (pies silently drop negatives)."),
        "Allocation by vehicle": breakdown("Allocation by vehicle",
            "web_vehicles_history", "vehicle", "value", "row",
            "Value by vehicle (the wrapper an exposure is held through: "
            "stock, etf, fund, spv, bond, physical, …) as of a day, "
            "including a 'demand_deposit' vehicle for cash — the bars sum "
            "exactly to net worth. The wrapper-dimension companion to "
            "Allocation by asset class."),
        "Allocation by currency": breakdown("Allocation by currency",
            "web_positions_history", "currency", "value", "row",
            "Positions value by the position's native currency — the FX "
            "exposure of the invested part (cash not included) as of a "
            "day. The Currency picker sets the currency the bars are "
            "valued in, not the ones they break down by."),
        "Value by tax wrapper": breakdown("Value by tax wrapper",
            "web_accounts_history", "tax_wrapper", "total_value", "pie",
            "Total account value (incl. cash) by tax wrapper as of a day.",
            {"pie.dimension": "tax_wrapper", "pie.metric": "value"}),
        "Value by management style": breakdown("Value by management style",
            "web_accounts_history", "management_style", "total_value", "row",
            "Total account value (incl. cash) by management style as of a "
            "day."),
        # A fixed hundred: the inline vehicle filter and the dashboard's
        # pickers narrow the list rather than a K picker sizing it.
        "Top 100 positions": ("table",
            "The hundred largest positions by market value as of a day, "
            "aggregated across accounts; narrow with the widget's "
            "vehicle filter." + POSITION_SIZE_NOTE + al_note,
            _native(db_id,
                "WITH p AS (\n"
                "  SELECT symbol, name, asset_class, vehicle,\n"
                f"         sum({_ccy_case('value')})::DOUBLE AS value\n"
                "    FROM web_positions_history" + _native_where(pos_tags) +
                "\n   GROUP BY 1, 2, 3, 4\n   ORDER BY 5 DESC\n   LIMIT 100)\n"
                "SELECT p.symbol, p.name, p.asset_class, p.vehicle, z.size, p.value\n"
                "  FROM p" + POSITION_SIZE_JOIN + "\n ORDER BY p.value DESC",
                pos_tags),
            {}),
        # The spending tiles are native over web_spending (spend_native)
        # but for two MBQL ones: the uncategorized share, a share of rows
        # over the _pct model, and the merchant ranking, which carries
        # every currency as its own column (SPEND_ALL_CURRENCY_CARDS).
        # The card-balances chart reads web_card_balances_history, card
        # balances being a different grain (per account per day, carried
        # forward), with its pickers registered above.
        "Spend — monthly trend": spend_native("Spend — monthly trend",
            "smartscalar",
            "Net spend in the window's latest month, with the change vs the "
            "month before. Net spend is purchases minus refunds.",
            f"SELECT {OCCURRED_MONTH},\n       {sp_val} AS net_spend\n"
            "  FROM web_spending" + sp_where + "\n GROUP BY 1\n ORDER BY 1",
            {}),
        "Net spend": spend_native("Net spend", "scalar",
            "Total net spend over the selected window: purchases minus "
            "refunds, across the selected accounts and categories.",
            f"SELECT {sp_val} AS net_spend\n"
            "  FROM web_spending" + sp_where,
            {}),
        # Shows a percentage, so the privacy twin reuses it as-is
        # (PRIVACY_EXEMPT_CARDS). A scalar's "see these records"
        # drill-through opens the underlying model, so it runs over the
        # _pct model — the one without the merchant column — even though
        # the figure itself is a scale-free row count.
        "Uncategorized share": ("scalar",
            "Share of the window's spending lines the enrichment pass could "
            f"not place — the ones labelled '{UNCATEGORIZED}'. The backlog "
            "`wealthdb categorize` works through; it shrinks as merchants "
            "get categorised." + spend_note,
            _mbql(db_id, mid["report_spending_pct"],
                  {"aggregation": [["share",
                       ["=", _f("spend_primary", "type/Text"), UNCATEGORIZED]]]}),
            _percent_viz("share")),
        # The axes are PINNED, and this is the card that most needs it.
        # Left to infer them, Metabase puts the dimension with the most
        # distinct values on the x-axis — and there are more categories
        # than months in any window worth charting, so the card drew its
        # own transpose: a bar per CATEGORY stacked by month, under a
        # band of rotated category names deep enough to leave the bars a
        # sliver. A card called "by month" charts months. Only the
        # dimensions are pinned; the metric is left to default, since
        # naming it would be guessing at what Metabase calls an
        # aggregation column.
        #
        # Stacked areas rather than bars: months are a continuous axis,
        # so the categories read as bands whose thickness moves across
        # the window, and the envelope is the window's own shape. A
        # month whose refunds beat its purchases dips its band below the
        # line.
        "Spending by month": spend_native("Spending by month", "area",
            "Net spend per month, stacked by primary category — the shape of "
            "the window: which months were heavy and what carried them.",
            _series_order(f"SELECT {OCCURRED_MONTH},\n       spend_primary_label AS category,\n"
                     f"       {sp_val} AS net_spend\n"
                     "  FROM web_spending" + sp_where + "\n GROUP BY 1, 2",
                     "month", "category", "net_spend", "total"),
            {"graph.dimensions": ["month", "category"], **STACKED_VIZ}),
        # The two breakdowns are donuts: the question they answer is how
        # the window DIVIDES, and a ring reads that as one shape where a
        # bar chart reads it as a ranking. The ring's hole carries the
        # window's own total, so the tile answers "how much" and "of
        # what" at once, and the legend carries each slice's share.
        #
        # `_donut` is where the two settings that make that work live:
        # every slice drawn (Metabase would otherwise fold the small
        # ones into a wedge it calls "Other", which is also the name of
        # a real category here), and the shares shown in the legend
        # rather than crowded onto the ring.
        #
        # A category can go net-negative over a window whose refunds
        # beat its purchases; a ring has no way to draw that, so
        # Metabase leaves such a slice out. The figure is in the
        # transaction list either way. The hole sums what was drawn,
        # so it carries the drawn categories rather than the window —
        # the two differ by the net-negative ones, and the
        # description says which figure it is.
        "Spending by category": spend_native("Spending by category", "pie",
            "Net spend by primary category over the window, largest first, "
            "with the drawn categories' total in the middle. The subcategory "
            "tile beside it holds the same window at the detailed level, and "
            "the transaction list below holds the lines. A category whose "
            "refunds beat its purchases has no slice, and is not in that "
            "total.",
            f"SELECT spend_primary_label AS category,\n       {sp_val} AS net_spend\n"
            "  FROM web_spending" + sp_where + "\n GROUP BY 1\n ORDER BY 2 DESC",
            _donut()),
        "Spending by subcategory": spend_native("Spending by subcategory",
            "pie",
            "The detailed level of Spending by category: net spend by "
            "detailed category. The vocabulary holds nearly a hundred "
            "values, so the ring draws the ones worth a slice and folds the "
            "long tail into one — every category that spent is on it either "
            "way. One whose refunds beat its purchases has no slice, and is "
            "not in the total in the middle.",
            f"SELECT spend_label AS category,\n       {sp_val} AS net_spend\n"
            "  FROM web_spending" + sp_where + "\n GROUP BY 1\n ORDER BY 2 DESC",
            _donut(threshold=1.5)),
        # Ranks merchants only. A line resolved to a delta — a gift, a
        # bill on a card not itemised, cash out of an ATM — is not a
        # merchant transaction, and one kind of delta line carries a
        # name: a card bill labelled with the issuer it was paid to
        # (migration 0052). A blank merchant therefore does not mark a
        # delta, so the ranking excludes the delta CATEGORIES outright —
        # a delta is primary-level, so `spend_primary <> spend_detailed`
        # reads the dimension's own marker rather than restating a list
        # of values — and still needs a name to rank by. A line nothing
        # resolved is kept out by its two category columns being equal
        # at '(uncategorized)'. A line the merchant store never named
        # shows its own signature (migration 0054), so the rows are at
        # the signature's grain — a chain appears once per branch
        # signature, and a fold that is only the bank's own booking tag
        # ranks under that tag, since the gates that keep such a fold
        # from the model fence the merchant store and not this column.
        # Only the ranking filters: the transaction list below keeps
        # every line, since a line is a line.
        "Top 50 merchants": ("table",
            "The fifty merchants with the most net spend over the window. A "
            "merchant is the merchant store's name for the line's normalized "
            "counterparty, or that counterparty itself where the store holds "
            "no name — so a merchant here is an identity the enrichment pass "
            "computed, not a verdict a model wrote, and it is at the grain "
            "that counterparty folds to: a chain whose statement text names "
            "the branch ranks once per branch, and a line whose text was "
            "nothing but the bank's own booking code ranks under that code. "
            "The ranking is of merchants only: delta lines — a gift, a bill "
            "on a card not itemised (which names its issuer, not a "
            "merchant), cash out of an ATM — and lines nothing has resolved "
            "are outside the ranking, though inside every total and the "
            "transaction list. Net spend is shown in every reporting "
            f"currency at once and the ranking is by {DEFAULT_CURRENCY}, so this card "
            "answers to every picker except Currency — and reads the same "
            "opened on its own as it does on the dashboard.",
            _mbql(db_id, mid["report_spending"],
                  {"aggregation": [ccy_spend(c) for c in REPORTING_CURRENCIES],
                   "filter": ["and",
                              ["not-null", _f("merchant_name", "type/Text")],
                              ["!=", _f("spend_primary", "type/Text"),
                                     _f("spend_detailed", "type/Text")]],
                   "breakout": [_f("merchant_name", "type/Text")],
                   "order-by": [["desc", ["aggregation",
                                 REPORTING_CURRENCIES.index(DEFAULT_CURRENCY)]]],
                   "limit": 50}),
            {}),
        "Spend by account": spend_native("Spend by account", "row",
            "Net spend by account over the window — which card or deposit "
            "account the money left through. Each bar is labelled with the "
            "account's name, its source and its kind, since a name on its "
            "own says neither.",
            f"SELECT account_label AS account,\n       {sp_val} AS net_spend\n"
            "  FROM web_spending" + sp_where + "\n GROUP BY 1\n ORDER BY 2 DESC",
            {}),
        # Read the way an issuer states a card: what is OWED, as a
        # positive figure. Gold stores the same balance negative — a card
        # is a liability, the margin-debit precedent — and keeps doing so
        # everywhere else, the CLI included; the sign is flipped in this
        # projection and nowhere else, so the chart matches the statement
        # a reader compares it against.
        "Card balances over time": ("line",
            "What each credit card owed for every day of the window, "
            "carried forward between statement closings. Stated the way an "
            "issuer states it: the line is what is owed, and a paid-off "
            "card returns to zero." + native_note,
            _native(db_id, _series_order(
                "SELECT as_of_day, account_label,\n"
                f"       sum({_ccy_case('balance', neg=True)}) AS owed\n"
                "  FROM web_card_balances_history"
                + _native_where(bal_tags, " ") + "\n"
                " GROUP BY 1, 2", "as_of_day", "account_label", "owed", "latest",
                largest_first=True), bal_tags),
            {**_series_viz("as_of_day", "account_label", "owed"), **LINES_VIZ}),
        # The one tile that lists LINES rather than grouping them: over
        # the long model it would get each line once per reporting
        # currency, with no aggregation to fold them back.
        "Largest transactions": spend_native("Largest transactions", "table",
            "The fifty largest single spending lines of the window, with "
            "merchant (blank only where the line has none to show), account "
            "and both category levels. A refund sorts to the bottom (its "
            "net spend is negative).",
            "SELECT occurred_at,\n       account_label AS account,\n"
            "       merchant_name,\n       spend_primary_label AS category,\n"
            "       spend_label AS subcategory,\n"
            f"       {_ccy_case('value', neg=True)} AS net_spend\n"
            "  FROM web_spending" + sp_where + "\n ORDER BY net_spend DESC\n LIMIT 50",
            {}),
        # ---- Income -------------------------------------------------
        # Every income tile is native over web_income, for the reason
        # the spending ones are: the serving view carries a row per
        # (line, reporting currency), and a required {{currency}}
        # variable picks the column rather than filtering rows. Two
        # tiles the Spending dashboard has are deliberately absent —
        # there is no second ring, the income taxonomy having one
        # vendored primary and so no subcategory level worth one, and no
        # balance-history chart, nothing on the income side being a
        # liability.
        "Income — monthly trend": income_native("Income — monthly trend",
            "smartscalar",
            "Net income in the window's latest month, with the change vs the "
            "month before. Net income is receipts minus reversals.",
            f"SELECT {OCCURRED_MONTH},\n       {in_val} AS net_income\n"
            "  FROM web_income" + in_where + "\n GROUP BY 1\n ORDER BY 1",
            {}),
        "Net income": income_native("Net income", "scalar",
            "Total net income over the selected window: receipts minus "
            "reversals, across the selected accounts and types. Gross as "
            "booked: the Withholding tile shows the tax withheld at source.",
            f"SELECT {in_val} AS net_income\n"
            "  FROM web_income" + in_where,
            {}),
        # The data-quality canary. A percentage, so the FIGURE needs no
        # twin — but this is a native card, and a native card carries its
        # own field-filter tags wherever it is opened, one of which is an
        # `account` filter. So the twin builds its own over
        # PRIVACY_INCOME_FILTERS (income_privacy_defs) and this one is
        # NOT in PRIVACY_EXEMPT_CARDS.
        # The one income tile that does NOT read {{currency}}: it is a
        # share of ROWS, scale-free, and the same in every currency. It
        # therefore gets tags without the currency variable rather than
        # declaring a required one it never interpolates.
        "Uncategorized income share": ("scalar",
            IN_UNCATEGORIZED_DESC,
            _native(db_id,
                    IN_UNCATEGORIZED_SQL + _native_where(in_share_tags),
                    in_share_tags),
            _percent_viz("uncategorized_share")),
        # No slice threshold: the income vocabulary is small enough to
        # name in full, unlike the spending one, so nothing is folded
        # into an "other" wedge a reader cannot open. The ring rule
        # still applies — a net-negative type has no slice.
        "Income by type": income_native("Income by type", "pie",
            "Net income by type over the window, largest first, with the "
            "drawn types' total in the middle. No type is folded into an "
            "'other' wedge — the income vocabulary is short enough to name "
            "in full. A type whose reversals beat its receipts has no slice, "
            "and is not in that total.",
            f"SELECT income_label AS type,\n       {in_val} AS net_income\n"
            "  FROM web_income" + in_where + "\n GROUP BY 1\n ORDER BY 2 DESC",
            _donut(threshold=0, total=True)),
        # The only payer ranking there is: the CLI carries none, as it
        # carries no merchant ranking. Payers with no name — the delta
        # lines — are excluded rather than grouped into a blank slice.
        "Top 50 payers": income_native("Top 50 payers", "table",
            "The fifty payers the most income came from over the window. "
            "Lines with no payer to name — own-account moves, gifts, cash "
            "paid in — are left out rather than grouped under a blank.",
            f"SELECT payer_name,\n       {in_val} AS net_income\n"
            "  FROM web_income" + in_where +
            "\n   AND payer_name IS NOT NULL\n GROUP BY 1\n ORDER BY 2 DESC\n LIMIT 50",
            {}),
        "Largest receipts": income_native("Largest receipts", "table",
            "The fifty largest single income lines of the window, with payer "
            "(blank only where the line has none to show), account and type. "
            "A reversal sorts to the bottom (its net income is negative).",
            "SELECT occurred_at,\n       account_label AS account,\n"
            "       payer_name,\n       income_label AS type,\n"
            f"       {_ccy_case('value')} AS net_income\n"
            "  FROM web_income" + in_where + "\n ORDER BY net_income DESC\n LIMIT 50",
            {}),
        # ---- Cash Flow ----------------------------------------------
        # Every cashflow tile is native over web_cashflow, for the
        # reason the other two families' are — a row per (line,
        # reporting currency), and a required {{currency}} variable that
        # picks the column rather than filtering rows — plus one of its
        # own: {{investing}}, which changes how the investing section is
        # GROUPED and which no field filter could express.
        #
        # There is no account tile and no account picker. The household
        # boundary is what separates the household's cash flow from its
        # vehicles', and a per-account view would ask a question this
        # statement does not answer: a wire between two of the
        # household's own accounts is invisible here by construction.
        "Net cash flow": cashflow_native("Net cash flow", "scalar",
            "What the household's cash pool did over the window: every "
            "line summed, positive where cash arrived. It is the residual "
            "of the statement — operating plus investing plus financing "
            "plus the vehicles — and the Cash node of the diagram is its "
            "negative, because cash the household kept is cash the pool "
            "absorbed.",
            f"SELECT {cf_val} AS net_cash_flow\n  FROM web_cashflow" + cf_where_nosec,
            {}, section=False),
        # Named for the SECTION, not for "cash": the residual section
        # is also called cash and is a different number, and the CLI's
        # columns behind these two tiles are operating_in / operating_out.
        "Operating in": cashflow_native("Operating in", "scalar",
            "Everything the household received over the window: wages, "
            "yield, benefits and the other receipts. Smaller than "
            "`wealthdb income` reports, by the vehicles' own income and "
            "three other terms — see docs/CASHFLOW.md §8.",
            f"SELECT {cf_val} AS operating_in\n  FROM web_cashflow" + cf_where_nosec +
            "\n   AND section = 'operating_in'",
            {}, section=False),
        "Operating out": cashflow_native("Operating out", "scalar",
            "Everything the household spent over the window — consumption, "
            "fees, taxes and giving — as a positive magnitude. Buying and "
            "selling is NOT here: investing is its own section and is shown "
            "net.",
            f"SELECT -({cf_val}) AS operating_out\n  FROM web_cashflow" + cf_where_nosec +
            "\n   AND section = 'operating_out'",
            {}, section=False),
        "Savings rate": cashflow_native("Savings rate", "scalar",
            "Operating cash flow as a share of what came in: what the "
            "household kept of its receipts before it invested, serviced "
            "debt or funded a vehicle. Blank where nothing came in.",
            _savings_rate_sql(cf_where_nosec, "savings_rate"),
            _percent_viz("savings_rate"), section=False),
        "Yield share": cashflow_native("Yield share", "scalar",
            "What share of the household's receipts its assets produced "
            "without its labour — dividends, interest, fund distributions, "
            "staking, rent and royalties over everything that came in.",
            _yield_share_sql(cf_where_nosec, "yield_share"),
            _percent_viz("yield_share"), section=False),
        # The diagram, and the centre of the dashboard. A native query in
        # the three columns the BI layer's Sankey visualisation reads,
        # over the LINE-grain serving view: a node's side is the sign of
        # its net over the filtered window, so the nets and the sides
        # have to be computed where the pickers apply.
        "Cash flow": cashflow_native("Cash flow", "sankey",
            "Where the household's cash came from and where it went over "
            "the window, as one diagram. Every node is a place money came "
            "from or went to, and it sits on the left if cash came from it "
            "on net and on the right if cash went to it. Own-account moves "
            "are invisible; buying and selling is one net movement, as a "
            "whole or per asset class per the Investing picker.",
            _cashflow_sankey_sql(cf_where_nosec, cf_val),
            {"sankey.source": "source", "sankey.target": "target",
             "sankey.value": "value",
             # Metabase defaults this to "left", which lays a node out
             # at the depth its INCOMING edge puts it at. A class with
             # no leaves then sits on the middle level as a dead end
             # beside the classes that do have leaves, and the diagram
             # reads as though it were one. "justify" is left alignment
             # with one change: a node with no outgoing edge moves to
             # the last level, where every other ending sits.
             "sankey.node_align": "justify"}, section=False),
        "Cash flow statement by month": cashflow_native(
            "Cash flow statement by month", "combo",
            "The statement per month: operating, investing, financing and "
            "the vehicles as stacked signed bars, with net cash flow as a "
            "line over them. A section below the axis drew cash down that "
            "month.",
            _series_order(f"SELECT {OCCURRED_MONTH},\n"
                     "       CASE WHEN section IN ('operating_in', 'operating_out')\n"
                     "            THEN 'operating' ELSE section END AS statement_section,\n"
                     f"       {cf_val} AS value\n"
                     "  FROM web_cashflow" + cf_where + "\n GROUP BY 1, 2",
                     "month", "statement_section", "value", "total"),
            {"graph.dimensions": ["month", "statement_section"],
             "graph.metrics": ["value"], **STACKED_VIZ}),
        "Inflows by class by month": cashflow_native(
            "Inflows by class by month", "area",
            "What came in each month, stacked by class: earnings, yield, "
            "pensions and benefits, other receipts, and the backlog no "
            "tier could place.",
            _series_order(f"SELECT {OCCURRED_MONTH},\n       class_node AS class,\n"
                     f"       {cf_val} AS value\n"
                     "  FROM web_cashflow" + cf_where +
                     "\n   AND section = 'operating_in'\n GROUP BY 1, 2",
                     "month", "class", "value", "total"),
            {"graph.dimensions": ["month", "class"],
             "graph.metrics": ["value"], **STACKED_VIZ}),
        "Outflows by class by month": cashflow_native(
            "Outflows by class by month", "area",
            "What went out each month, stacked by class: consumption, "
            "fees, taxes, giving, and the backlog. Fees, taxes and giving "
            "are lifted out of consumption because each is worth a line "
            "of its own.",
            _series_order(f"SELECT {OCCURRED_MONTH},\n       class_node AS class,\n"
                     f"       -({cf_val}) AS value\n"
                     "  FROM web_cashflow" + cf_where +
                     "\n   AND section = 'operating_out'\n GROUP BY 1, 2",
                     "month", "class", "value", "total"),
            {"graph.dimensions": ["month", "class"],
             "graph.metrics": ["value"], **STACKED_VIZ}),
        "Investing by month": cashflow_native("Investing by month", "bar",
            "Investing per month, signed: above the axis the portfolio fed "
            "the household, below it the household fed the portfolio. One "
            "series or one per asset class, per the Investing picker — and "
            "the difference between the two is the reallocation the whole "
            "nets away.",
            _series_order(f"SELECT {OCCURRED_MONTH},\n"
                     "       CASE WHEN {{investing}} = 'whole' THEN 'Investments'\n"
                     "            ELSE class_node END AS class,\n"
                     f"       {cf_val} AS value\n"
                     "  FROM web_cashflow" + cf_where +
                     "\n   AND section = 'investing'\n GROUP BY 1, 2",
                     "month", "class", "value", "total"),
            {"graph.dimensions": ["month", "class"],
             "graph.metrics": ["value"], **STACKED_VIZ}),
        "Financing and vehicles by month": cashflow_native(
            "Financing and vehicles by month", "bar",
            "Debt serviced or drawn, and the earmarked pools funded or "
            "drawn on, per month and signed. A mortgage being repaid while "
            "a loan is drawn are two bars, and so are a retirement plan "
            "being funded while a trust pays out.",
            _series_order(f"SELECT {OCCURRED_MONTH},\n       class_node AS class,\n"
                     f"       {cf_val} AS value\n"
                     "  FROM web_cashflow" + cf_where +
                     "\n   AND section IN ('financing', 'vehicles')\n GROUP BY 1, 2",
                     "month", "class", "value", "total"),
            {"graph.dimensions": ["month", "class"],
             "graph.metrics": ["value"], **STACKED_VIZ}),
        "Largest flows": cashflow_native("Largest flows", "table",
            "The fifty largest single lines of the window, with the node "
            "each landed on and the account it moved through. The name is "
            "the instrument on an investing line and the payer or merchant "
            "on an operating one; a financing or vehicle line names "
            "nothing, its counterparty being an account.",
            "SELECT occurred_at,\n       section,\n       class_node AS class,\n"
            "       group_node AS \"group\",\n       name,\n"
            "       account_label AS account,\n"
            f"       {_ccy_case('value')} AS value\n"
            "  FROM web_cashflow" + cf_where +
            f"\n ORDER BY abs({_ccy_case('value')}) DESC\n LIMIT 50",
            {}),
        # The returns cards run over the materialized report_returns table.
        # The three scalars and the by-source table are MBQL (grain 'global' /
        # 'sources', granularity 'total'); the Returns dashboard's Currency and
        # Start-year pickers land on their currency / window_from_year
        # dimensions — window_from_year selects the since-inception (0) or
        # since-<year> summary, so the picker rescopes the whole figure exactly
        # (the since-inception TWR is often null around the degenerate first
        # months). twr/mwr are ratios rendered as percent ("max" is the name
        # Metabase gives the aggregate). Opened standalone the cards aggregate
        # across currencies / windows, so set Currency and Start year first.
        # The scalars run over the REDACTED model: they are privacy-exempt
        # (reused as-is on the privacy dashboard), and a scalar's "see these
        # records" drill-through opens the underlying model, which therefore
        # must not carry the absolute money columns.
        "Return (TWR)": ("scalar",
            "Whole-portfolio time-weighted return over the chosen window — "
            "performance with the timing and size of external flows stripped "
            "out. Built for the Returns dashboard (Currency + Start-year "
            "pickers); opened standalone, set those first.",
            _mbql(db_id, mid["report_returns_redacted"],
                  {"filter": _returns_part("global", "total"),
                   "aggregation": [["max", twr]]}),
            _percent_viz("max")),
        "Return (MWR)": ("scalar",
            "Whole-portfolio money-weighted return (XIRR) over the chosen "
            "window — the return the invested cash experienced, external flows "
            "included. Built for the Returns dashboard (Currency + Start-year "
            "pickers); opened standalone, set those first.",
            _mbql(db_id, mid["report_returns_redacted"],
                  {"filter": _returns_part("global", "total"),
                   "aggregation": [["max", _f("mwr", "type/Float")]]}),
            _percent_viz("max")),
        "Annualized return (TWR)": ("scalar",
            "The window's time-weighted return restated as a constant "
            "per-year rate (n/a for windows under a year). Built for the "
            "Returns dashboard (Currency + Start-year pickers); opened "
            "standalone, set those first.",
            _mbql(db_id, mid["report_returns_redacted"],
                  {"filter": _returns_part("global", "total"),
                   "aggregation": [["max", _f("twr_annualized", "type/Float")]]}),
            _percent_viz("max")),
        # Native cumulative + per-period charts. All split by source with the
        # global grain unioned in as a toggleable '(all sources)' line; the
        # Currency / Start-year / Source pickers map onto their {{currency}}
        # / {{start_year}} / {{source_ff}} tags.
        "Cumulative return (TWR)": returns_native("Cumulative return (TWR)",
            "Time-weighted return compounded from the chosen start year, per "
            "source and the '(all sources)' portfolio line — derived from the "
            "engine's since-<year> returns (so it matches the scalars and the "
            "by-source table exactly). Annual granularity. Built for the "
            "Returns dashboard.",
            _series_order(returns_growth_sql(), "year", "source", "cumulative_return", "latest",
                          largest_first=True),
            {**_series_viz("year", "source", "cumulative_return", percent=True), **LINES_VIZ}),
        "Monthly returns (TWR)": returns_native("Monthly returns (TWR)",
            "Time-weighted return per month, one line per source plus the "
            "'(all sources)' portfolio line. Built for the Returns dashboard "
            "(Currency + Start-year pickers).",
            returns_period_sql("monthly"),
            _series_viz("period", "source", "twr", percent=True)),
        "Quarterly returns (TWR)": returns_native("Quarterly returns (TWR)",
            "Time-weighted return per quarter, one line per source plus the "
            "'(all sources)' portfolio line. Built for the Returns dashboard "
            "(Currency + Start-year pickers).",
            returns_period_sql("quarterly"),
            _series_viz("period", "source", "twr", percent=True)),
        "Annual returns (TWR)": returns_native("Annual returns (TWR)",
            "Time-weighted return per calendar year, one line per source plus "
            "the '(all sources)' portfolio line. Built for the Returns "
            "dashboard (Currency + Start-year pickers).",
            returns_period_sql("annual"),
            _series_viz("period", "source", "twr", percent=True)),
        "Returns by source": ("table",
            "Returns per source over the chosen window: TWR and MWR, plain and "
            "annualized, with start/end values, net external flow and the "
            "quality flags that explain every n/a. Built for the Returns "
            "dashboard (Currency + Start-year pickers); opened standalone, set "
            "those first.",
            _returns_by_source(db_id, mid["report_returns"], money=True),
            _percent_viz("twr", "twr_annualized", "mwr", "mwr_annualized")),
        "Stalest source (days)": ("scalar",
            "Days since the source read longest ago was last read: how out "
            "of date the worst feed is. A source that marks its holdings "
            "quarterly counts from its latest download, not its latest "
            "mark.",
            _mbql(db_id, mid["report_sources_latest"],
                  {"expressions": {"days_stale": DAYS_STALE},
                   "filter": FED_SOURCES,
                   "aggregation": [["max", ["expression", "days_stale"]]]}),
            {}),
        "Returns age (days)": ("scalar",
            "Days since the returns table was last materialized (it "
            "refreshes with the snapshot on every `web refresh`).",
            # Every row of a run shares one computed_at, so max(age) is
            # the run's age. Runs over the REDACTED model although the
            # card itself shows no values: it is privacy-exempt (shared
            # by the Data Freshness twin), and a scalar's "see these
            # records" drill-through opens the underlying model — which
            # must therefore not carry absolute money columns.
            _mbql(db_id, mid["report_returns_redacted"],
                  {"expressions": {"age_days": ["datetime-diff",
                       _f("computed_at", "type/DateTime"), ["now"], "day"]},
                   "aggregation": [["max", ["expression", "age_days"]]]}),
            {}),
        "Source freshness": source_freshness_card(db_id, mid["report_sources_latest"]),
        **gains_question_defs(db_id),
        **exposure_cards(db_id, privacy=False),
        **issuer_cards(db_id, privacy=False),
        **yield_cards(db_id, privacy=False),
        **income_projected_cards(db_id, privacy=False),
        **cashflow_plan_cards(db_id, privacy=False),
    }


def gains_question_defs(db_id, privacy=False):
    """The Gains dashboard's cards: native SQL over web_gains (gold
    migration 0117), the monthly gains rows materialized per reporting
    currency. The view carries a row set per currency, so {{currency}}
    is a row filter here, where the other families pick a value column
    with it. A figure over the window sums the months in it; a figure at
    the window's end reads its last month. docs/GAINS.md defines the
    figures and the quality counters. It carries a row set per reading
    of a missing cost basis too, and {{missing_basis}} picks one the same
    way (docs/GAINS.md §8).

    With `privacy`, the privacy variants of the two headline gains the
    Wealth Overview's twin shows: each gain as a ratio of the cost basis
    it is a gain on. Gains has no twin of its own (NO_PRIVACY_TWIN)."""
    tags = {**native_tags("web_gains", GAINS_FILTERS), "missing_basis": missing_basis_tag()}
    note = (" Built for the " + ("Wealth Overview's privacy view" if privacy else "Gains dashboard") +
            f"; opened standalone it runs in {DEFAULT_CURRENCY}, the currency variable's "
            f"default, with a missing cost basis read as {DEFAULT_MISSING_BASIS}, over every "
            "month." + (PRIVACY_DESC if privacy else ""))
    ccy = "\n     AND currency = {{currency}}\n     AND missing_basis = {{missing_basis}}"
    where = _native_where(tags) + ccy
    # The whole-portfolio line answers every picker but Source, so
    # narrowing the sources keeps the whole in view.
    where_all = _native_where(tags, exclude=("source",)) + ccy
    # The window's rows, and its end: the positions held at the last
    # month in range.
    end = ("WITH w AS (\n  SELECT * FROM web_gains" + where + "),\n"
           "e AS (SELECT * FROM w\n"
           "       WHERE at_end AND period = (SELECT max(period) FROM w))\n")
    # The window's gain as the price move of what was sold and of what
    # was kept, a sale counted from its month's start (web_gains); the
    # total is their sum, as it is realized plus unrealized change.
    gain_split = [("sold_gain", "sold"), ("held_change", "held")]
    gain = " + ".join(f"coalesce(sum({c}), 0)" for c, _ in gain_split)
    position = "coalesce(symbol, name, instrument_key)"

    def native(name, display, desc, sql, viz):
        register_native_targets(name, tags, GAINS_PICKERS)
        return (display, desc + note, _native(db_id, sql, tags), viz)

    if privacy:
        return {
            "Realized gain (privacy)": native("Realized gain (privacy)", "scalar",
                "The gain or loss on what was sold over the window, as a percent of "
                "the cost basis sold: the sales whose lots state both.",
                "SELECT sum(realized) FILTER (WHERE realized_cost_basis > 0)\n"
                "       / nullif(sum(realized_cost_basis) FILTER (WHERE realized IS NOT NULL\n"
                "                                                 AND realized_cost_basis > 0), 0)"
                " AS realized_pct\n"
                "  FROM web_gains" + where, _percent_viz("realized_pct")),
            "Unrealized gain (privacy)": native("Unrealized gain (privacy)", "scalar",
                "The gain or loss on what is held at the window's end, as a percent "
                "of the cost basis held: the holdings that carry a cost basis. The "
                "end is the last month in the window.",
                end + "SELECT sum(unrealized_end) FILTER (WHERE cost_basis > 0)\n"
                "       / nullif(sum(cost_basis) FILTER (WHERE unrealized_end IS NOT NULL\n"
                "                                        AND cost_basis > 0), 0) AS unrealized_pct\n"
                "  FROM e", _percent_viz("unrealized_pct")),
        }

    def unrealized_table(name, desc, pred, order):
        """Ten positions held at the window's end, by unrealized gain or
        by its percent of the cost basis."""
        return native(name, "table", desc,
                      end + f"SELECT {position} AS position,\n"
                      "       unrealized_end AS unrealized,\n"
                      "       unrealized_end / nullif(cost_basis, 0) AS unrealized_pct,\n"
                      "       cost_basis,\n       value,\n       account\n"
                      f"  FROM e\n WHERE {pred}\n ORDER BY {order}\n LIMIT 10",
                      _percent_viz("unrealized_pct"))

    def realized_table(name, desc, pred, order):
        """Ten positions by what their sales over the window realized,
        or by its percent of the cost basis sold."""
        return native(name, "table", desc,
                      "WITH r AS (\n"
                      f"  SELECT any_value({position}) AS position,\n"
                      "         any_value(account) AS account,\n"
                      "         sum(proceeds) AS proceeds,\n"
                      "         sum(realized_cost_basis) AS cost_basis,\n"
                      "         sum(realized) AS realized\n"
                      "    FROM web_gains" + where +
                      "\n     AND realized IS NOT NULL\n"
                      "   GROUP BY silver_source_id, account_external_id, instrument_key)\n"
                      "SELECT position, realized,\n"
                      "       realized / nullif(cost_basis, 0) AS realized_pct,\n"
                      "       proceeds, cost_basis, account\n"
                      f"  FROM r\n WHERE {pred}\n ORDER BY {order}\n LIMIT 10",
                      _percent_viz("realized_pct"))

    def split_bars(name, desc, dim, label, metrics):
        """Stacked bars per value of SQL expression `dim`, one series per
        metric, largest total first; a value none of the metrics has is
        left out."""
        cols = ",\n".join(f"       sum({c}) AS {a}" for c, a in metrics)
        total = " + ".join(f"coalesce(sum({c}), 0)" for c, _ in metrics)
        some = " OR ".join(f"count({c}) > 0" for c, _ in metrics)
        return native(name, "bar", desc,
                      f"SELECT {dim} AS {label},\n{cols}\n"
                      "  FROM web_gains" + where +
                      f"\n GROUP BY 1\nHAVING {some}\n ORDER BY {total} DESC",
                      {"graph.dimensions": [label],
                       "graph.metrics": [a for _, a in metrics],
                       "stackable.stack_type": "stacked"})

    term_split = [("realized_short", "short_term"),
                  ("realized_long", "long_term"),
                  ("realized_other", "term_unstated")]
    return {
        "Realized gain": native("Realized gain", "scalar",
            "The gain or loss on what was sold over the window: the primary "
            "realized lots, at the gain their document states, the lot "
            "engine's where no document states the sale, or proceeds less "
            "cost basis. A sale neither covers is not in it; the coverage "
            "table counts those.",
            "SELECT sum(realized) AS realized\n  FROM web_gains" + where, {}),
        "Unrealized gain": native("Unrealized gain", "scalar",
            "The gain or loss on what is held at the window's end: clean "
            "value less cost basis, over the holdings that carry a cost "
            "basis. The end is the last month in the window.",
            end + "SELECT sum(unrealized_end) AS unrealized\n  FROM e", {}),
        "Gain on positions sold": native("Gain on positions sold", "scalar",
            "What the positions sold over the window gained in the month of "
            "their sale: from the month's start, or their purchase in it, to "
            "the sale. Realized gain counts from the purchase; the gain of "
            "the months before the sale is in the gain on positions held. In "
            "the output currency.",
            "SELECT sum(sold_gain) AS sold\n  FROM web_gains" + where, {}),
        "Gain on positions held": native("Gain on positions held", "scalar",
            "How far the unrealized gain of the positions kept moved over "
            "the window, summed month by month. A position sold in a month "
            "counts as sold in that month; a purchase adds no gain. In the "
            "output currency, so it includes the exchange-rate move on a "
            "gain held through the window.",
            "SELECT sum(held_change) AS held\n  FROM web_gains" + where, {}),
        "Total gain": native("Total gain", "scalar",
            "The gain on positions sold plus the gain on positions held: "
            "the window's price gain on what was held. It is also realized "
            "gain plus the change in unrealized gain. Income, fees and "
            "taxes are not in it; the Returns dashboard measures those.",
            f"SELECT {gain} AS gain\n  FROM web_gains" + where, {}),
        "Cost basis coverage": native("Cost basis coverage", "scalar",
            "Share of the value held at the window's end that carries a "
            "cost basis, among the holdings a cost basis describes (cash, "
            "mortgages and FX forwards are not among them). The rest is "
            "left out of every unrealized figure; the coverage table says "
            "where.",
            end + "SELECT sum(abs(value)) FILTER (WHERE NOT end_without_basis)\n"
            "       / nullif(sum(abs(value)), 0) AS basis_coverage\n"
            "  FROM e\n WHERE end_applies",
            _percent_viz("basis_coverage")),
        "Gains by month": native("Gains by month", "combo",
            "Each month's gain on positions sold and on positions held as "
            "stacked bars, and their sum as a line. A sale counts as sold "
            "for what it gained in its month; what it gained before is in "
            "the held bars of the months before.",
            "SELECT period AS month,\n" + ",\n".join(
                f"       sum({c}) AS {a}" for c, a in gain_split) +
            f",\n       {gain} AS gain\n"
            "  FROM web_gains" + where + "\n GROUP BY 1\n ORDER BY 1",
            {"graph.dimensions": ["month"],
             "graph.metrics": [a for _, a in gain_split] + ["gain"],
             "series_settings": {**{a: {"display": "bar"} for _, a in gain_split},
                                 "gain": {"display": "line"}},
             "stackable.stack_type": "stacked"}),
        "Realized gains by month, short vs long term": native(
            "Realized gains by month, short vs long term", "bar",
            "Each month's realized gain by holding period, as the lots "
            "state it: short term, long term, and lots whose document "
            "states no term.",
            "SELECT period AS month,\n" + ",\n".join(
                f"       sum({c}) AS {a}" for c, a in term_split) +
            "\n  FROM web_gains" + where + "\n     AND realized IS NOT NULL"
            "\n GROUP BY 1\n ORDER BY 1",
            {"graph.dimensions": ["month"],
             "graph.metrics": [a for _, a in term_split],
             "stackable.stack_type": "stacked"}),
        "Unrealized gain over time": native("Unrealized gain over time", "line",
            "The unrealized gain held at each month's end, one line per "
            "source plus an '(all sources)' line the Source picker does not "
            "narrow. In the output currency, so it moves with exchange "
            "rates as well as prices.",
            _series_order(
                "SELECT period AS month, silver_source_id AS source,\n"
                "       sum(unrealized_end) AS unrealized\n"
                "  FROM web_gains" + where + "\n     AND at_end\n GROUP BY 1, 2\n"
                "UNION ALL\n"
                "SELECT period, '(all sources)', sum(unrealized_end)\n"
                "  FROM web_gains" + where_all + "\n     AND at_end\n GROUP BY 1",
                "month", "source", "unrealized", "latest", largest_first=True),
            {**_series_viz("month", "source", "unrealized"), **LINES_VIZ}),
        "Largest unrealized gains": unrealized_table("Largest unrealized gains",
            "The ten positions held at the window's end with the largest "
            "unrealized gain.",
            "unrealized_end > 0", "unrealized DESC"),
        "Largest unrealized losses": unrealized_table("Largest unrealized losses",
            "The ten positions held at the window's end with the largest "
            "unrealized loss.",
            "unrealized_end < 0", "unrealized"),
        "Best unrealized %": unrealized_table("Best unrealized %",
            "The ten positions held at the window's end with the largest "
            "unrealized gain as a share of their cost basis.",
            "unrealized_end > 0 AND cost_basis > 0", "unrealized_pct DESC"),
        "Worst unrealized %": unrealized_table("Worst unrealized %",
            "The ten positions held at the window's end with the largest "
            "unrealized loss as a share of their cost basis.",
            "unrealized_end < 0 AND cost_basis > 0", "unrealized_pct"),
        "Largest realized gains": realized_table("Largest realized gains",
            "The ten positions whose sales over the window realized the "
            "largest gain. The cost basis is that of the lots sold.",
            "realized > 0", "realized DESC"),
        "Largest realized losses": realized_table("Largest realized losses",
            "The ten positions whose sales over the window realized the "
            "largest loss. The cost basis is that of the lots sold.",
            "realized < 0", "realized"),
        "Best realized %": realized_table("Best realized %",
            "The ten positions whose sales over the window realized the "
            "largest gain as a share of the cost basis sold.",
            "realized > 0 AND cost_basis > 0", "realized_pct DESC"),
        "Worst realized %": realized_table("Worst realized %",
            "The ten positions whose sales over the window realized the "
            "largest loss as a share of the cost basis sold.",
            "realized < 0 AND cost_basis > 0", "realized_pct"),
        "Gain by source": split_bars("Gain by source",
            "Each source's gain on positions sold and on positions held over "
            "the window, stacked. Bars, not a ring: a ring cannot draw a "
            "loss.",
            "silver_source_id", "source", gain_split),
        "Gain by asset class": split_bars("Gain by asset class",
            "Each asset class's gain on positions sold and on positions held "
            "over the window, stacked. '(none)' holds the lots of a holding "
            "no snapshot shows.",
            "coalesce(asset_class, '(none)')", "asset_class", gain_split),
        "Realized gains by tax wrapper": split_bars(
            "Realized gains by tax wrapper",
            "Realized gain over the window per tax wrapper, by holding "
            "period: what of it is taxable, and at which rate.",
            "tax_wrapper", "tax_wrapper", term_split),
        "Realized gains by tax year": native("Realized gains by tax year", "table",
            "Per calendar year in the window: the realized gain by holding "
            "period, wash sales disallowed, proceeds and the cost basis "
            "sold, with the lots behind it and the sells neither a document "
            "nor the lot engine covers. The documents and the engine's "
            "rebuilt lots summed; not a tax return.",
            "SELECT CAST(year(period) AS VARCHAR) AS tax_year,\n"
            "       sum(realized) AS realized,\n" + ",\n".join(
                f"       sum({c}) AS {a}" for c, a in term_split) + ",\n"
            "       sum(wash_disallowed) AS wash_disallowed,\n"
            "       sum(proceeds) AS proceeds,\n"
            "       sum(realized_cost_basis) AS cost_basis,\n"
            "       sum(realized_lots) AS realized_lots,\n"
            "       sum(sells_without_documents) AS sells_without_documents\n"
            "  FROM web_gains" + where + "\n GROUP BY 1\n ORDER BY 1 DESC",
            {}),
        "Unrealized gain vs. percent": native("Unrealized gain vs. percent",
            "scatter",
            "Every position held at the window's end with a cost basis: its "
            "unrealized gain against that gain as a share of its cost "
            "basis, sized by value and coloured by asset class. The corners "
            "are the four unrealized tables in one picture.",
            end + f"SELECT {position} AS position,\n"
            "       coalesce(asset_class, '(none)') AS asset_class,\n"
            "       unrealized_end / cost_basis AS unrealized_pct,\n"
            "       unrealized_end AS unrealized,\n"
            "       abs(value) AS value\n"
            "  FROM e\n WHERE unrealized_end IS NOT NULL AND cost_basis > 0",
            # The second dimension splits the dots into one series, and
            # so one colour, per asset class.
            {"graph.dimensions": ["unrealized_pct", "asset_class"],
             "graph.metrics": ["unrealized"],
             "scatter.bubble": "value",
             **_percent_viz("unrealized_pct")}),
        "Positions in the green": native("Positions in the green", "scalar",
            "Positions held at the window's end with an unrealized gain.",
            end + "SELECT count(*) AS positions\n  FROM e\n WHERE unrealized_end > 0",
            {}),
        "Positions in the red": native("Positions in the red", "scalar",
            "Positions held at the window's end with an unrealized loss.",
            end + "SELECT count(*) AS positions\n  FROM e\n WHERE unrealized_end < 0",
            {}),
        "Coverage by source": native("Coverage by source", "table",
            "Per source, how far the figures above can be trusted: the "
            "value held at the window's end and the part with a cost basis, "
            "then the window's counters, each a way a gain can be missing "
            "or misplaced (docs/GAINS.md §6). Sells without documents are "
            "missing from realized; sells rebuilt are realized by the lot "
            "engine from the trades; in-kind moves and corporate actions "
            "carry gain from before the window into unrealized change. "
            "Under Missing cost basis = zero the coverage is whole and "
            "basis_assumed_zero counts the positions and lots it assumed.",
            end + ", t AS (\n"
            "  SELECT silver_source_id,\n"
            "         sum(sells) AS sells,\n"
            "         sum(sells_without_documents) AS sells_without_documents,\n"
            "         sum(sells_rebuilt) AS sells_rebuilt,\n"
            "         sum(realized_lots) AS realized_lots,\n"
            "         sum(lots_without_gain) AS lots_without_gain,\n"
            "         sum(in_kind_moves) AS in_kind_moves,\n"
            "         sum(corporate_actions) AS corporate_actions,\n"
            "         count(*) FILTER (WHERE basis_changed) AS basis_changed,\n"
            "         sum(seed_lots) AS seed_lots,\n"
            "         sum(basis_assumed_zero) AS basis_assumed_zero,\n"
            "         sum(fx_missing) AS fx_missing\n"
            "    FROM w GROUP BY 1),\n"
            "v AS (\n"
            "  SELECT silver_source_id,\n"
            "         sum(value) AS value,\n"
            "         sum(value) FILTER (WHERE NOT end_without_basis) AS value_with_basis,\n"
            "         sum(abs(value)) FILTER (WHERE NOT end_without_basis)\n"
            "             / nullif(sum(abs(value)), 0) AS basis_coverage\n"
            "    FROM e WHERE end_applies GROUP BY 1)\n"
            "SELECT silver_source_id AS source, v.value, v.value_with_basis,\n"
            "       v.basis_coverage, t.sells, t.sells_without_documents,\n"
            "       t.sells_rebuilt, t.realized_lots, t.lots_without_gain,\n"
            "       t.in_kind_moves, t.corporate_actions, t.basis_changed,\n"
            "       t.seed_lots, t.basis_assumed_zero, t.fx_missing\n"
            "  FROM t FULL JOIN v USING (silver_source_id)\n ORDER BY 1",
            _percent_viz("basis_coverage")),
    }


# Dashboard filters: a required currency picker (default: the configured
# default currency) and a silver-source picker (default: all values) plus
# either a time range over flows/history (default: past 12 months) or a
# single as-of day over point-in-time holdings (default: the last day
# the holdings history reaches, ASOF_DEFAULT), each
# linked to every tile, plus a tax wrapper and an asset class picker on
# the Wealth Overview and Allocation and a vehicle picker that renders
# inline on the Top-positions tiles only. The Returns dashboards carry a
# start-year picker instead of a time filter — the periods are
# precomputed buckets; Spending, Income, Cash Flow, Gains and Exposure
# carry pickers of their own (dashboard_parameters). The parameter ids
# are arbitrary but must be stable across runs so re-provisioning
# converges instead of accumulating parameters, and they must be
# distinct — a reused id would make two pickers one.
TIME_PARAM_ID = "aa5df100"
SOURCE_PARAM_ID = "aa5df101"
ASOF_PARAM_ID = "aa5df102"
ASSET_PARAM_ID = "aa5df103"
CURRENCY_PARAM_ID = "aa5df104"
START_YEAR_PARAM_ID = "aa5df105"
VEHICLE_PARAM_ID = "aa5df106"
# The Spending dashboards' own three pickers, on top of the 'range'
# pair. The currency picker is separate from the Returns one
# (CURRENCY_PARAM_ID): it draws a static value list and lands on the
# spending cards' own currency dimension / {{currency}} variable.
SPEND_CURRENCY_PARAM_ID = "aa5df107"
ACCOUNT_PARAM_ID = "aa5df108"
CATEGORY_PARAM_ID = "aa5df109"
# The currency picker of the Wealth Overview and Allocation dashboards
# and their twins: the same static list as the Spending one, landing on
# every tile's {{currency}} variable.
WEALTH_CURRENCY_PARAM_ID = "aa5df110"
# Their tax wrapper picker. Their asset class picker is ASSET_PARAM_ID.
WEALTH_TAX_WRAPPER_PARAM_ID = "aa5df118"
# The Yield dashboards' currency, tax wrapper and asset class pickers.
YIELD_CURRENCY_PARAM_ID = "aa5df119"
YIELD_TAX_WRAPPER_PARAM_ID = "aa5df11a"
YIELD_ASSET_CLASS_PARAM_ID = "aa5df11b"
# The projection's four pickers, which the Yield dashboards and the
# Income dashboards' cards of the holdings' income share: the rate path,
# what a maturing principal does, the shift in basis points and the
# horizon.
RATES_PARAM_ID = "aa5df11c"
MATURING_PARAM_ID = "aa5df11d"
SHIFT_PARAM_ID = "aa5df11e"
HORIZON_PARAM_ID = "aa5df11f"
# The position the Income dashboards' calendar lists.
POSITION_PARAM_ID = "aa5df121"

# The dashboards carrying the spending pickers (the base view and its
# privacy twin). Named rather than modelled as a filter mode of their
# own: the mode is 'range' like Wealth Overview — a time window plus a
# source picker — and these three pickers are additions to it.
SPENDING_DASHBOARDS = {"Spending", "Spending" + PRIVACY_SUFFIX}
# The same for the Income dashboards.
INCOME_DASHBOARDS = {"Income", "Income" + PRIVACY_SUFFIX}

# The same for the Cash Flow dashboards.
CASHFLOW_DASHBOARDS = {"Cash Flow", "Cash Flow" + PRIVACY_SUFFIX}

# The Gains dashboard, which has no privacy twin. Its tiles are gains on
# named positions; a twin would redact the names and normalise the
# money, and what would remain is the percent columns the tables carry
# already.
GAINS_DASHBOARDS = {"Gains"}
NO_PRIVACY_TWIN = GAINS_DASHBOARDS

# The Exposure dashboards: the range pair with a currency, a tax wrapper
# and an asset class picker of their own.
EXPOSURE_DASHBOARDS = {"Exposure", "Exposure" + PRIVACY_SUFFIX}

# The Yield dashboards: Exposure's five pickers, with ids of their own,
# and the four the projection's cards take.
YIELD_DASHBOARDS = {"Yield", "Yield" + PRIVACY_SUFFIX}

# Tiles whose cells set a dashboard picker when clicked, Metabase's
# crossfilter click behaviour: card -> (its columns, the picker's id).
# A position's row in the positions table picks the position the
# calendar below it lists, by the value its cell shows; the twin's
# calendar also matches the name the twin shows (TWIN_POSITION_NAME).
CROSSFILTER_TILES = {name: (("symbol", "name"), POSITION_PARAM_ID)
                     for name in ("Received and expected by position",
                                  "Received and expected by position" + PRIVACY_SUFFIX)}

# The vehicle filter is linked only to these tiles, the Top-positions
# widgets, which list individual holdings, and renders on them: the
# breakdowns each group by a taxonomy dimension, so filtering them by
# the vehicle would mostly self-select. The asset class is a dashboard
# picker, on every tile.
POSITION_FILTERED_CARDS = {"Top 100 positions", "Top 100 positions (privacy)"}

# Spending cards that carry every reporting currency as its own column,
# and so must NOT be wired to the Currency picker: the long model has a
# row per (line, currency), so a row filter on `currency` would empty
# every column but the one the picker selects. Such a card also reads the
# same opened standalone, where no picker reaches it.
SPEND_ALL_CURRENCY_CARDS = {"Top 50 merchants"}

# The Wealth Overview and its twin: the dashboards that borrow the
# figures of the others.
OVERVIEW_DASHBOARDS = {"Wealth Overview", "Wealth Overview" + PRIVACY_SUFFIX}

# The dashboard each Wealth Overview tile restates, by its card on the
# base dashboard: a card borrowed from it, or one built for the overview
# from its data (OVERVIEW_BUILT_CARDS). A click on the tile opens that
# dashboard, or on the twin that dashboard's twin; a dashboard with no
# twin (Gains) opens from the base alone, so a click never leads from the
# privacy view to money. An MBQL tile takes the pickers its home
# dashboard gives it. The net worth tiles have no home and open nothing.
OVERVIEW_TILE_HOMES = {
    "Stalest source (days)": "Data Freshness",
    "Asset class mix over time": "Allocation",
    "Received income": "Income", "Expected remaining": "Income",
    "Net spend": "Spending", "Spend — monthly trend": "Spending",
    "Net cash flow": "Cash Flow", "Savings rate": "Cash Flow", "Projected net cash flow": "Cash Flow",
    "Realized gain": "Gains", "Unrealized gain": "Gains",
    "Return year to date (TWR)": "Returns", "Return year to date (MWR)": "Returns",
    "Income yield": "Yield",
}

# The overview's tiles built for it alone, each from its home dashboard's
# data: no other dashboard shows them.
OVERVIEW_BUILT_CARDS = {"Asset class mix over time", "Projected net cash flow",
                        *(c for c, _m, _col in RETURN_YTD_CARDS)}

# Full-width headings over a dashboard's rows, by base dashboard (the
# twin takes its base's): (row, text). The overview's say what sets each
# row of figures.
DASHBOARD_HEADINGS = {"Wealth Overview": [
    (19, "Over the time range"),
    (26, "At the range's end, today and ahead"),
]}

# A twin's own placing of its base's tiles, by base dashboard: card ->
# (row, col, size_x, size_y), or None for a tile the twin leaves out. On
# the overview's twin, received income and net spend read 100 by
# construction: each is the total its own twin divides by. The twin
# leaves them out, and the other four figures of their rows sit two to a
# row.
TWIN_TILES = {"Wealth Overview": {
    "Received income": None, "Net spend": None,
    "Spend — monthly trend": (20, 0, 12, 3), "Net cash flow": (20, 12, 12, 3),
    "Savings rate": (23, 0, 12, 3), "Realized gain": (23, 12, 12, 3),
}}


def dashboard_tile_titles(base):
    """card name -> the title its tiles show on base dashboard `base` and
    its twin. The twin's title takes the privacy form, unless the twin
    shows the card itself (PRIVACY_EXEMPT_CARDS). A title is set for two
    kinds of card: a borrowed card whose name leans on its home
    dashboard's pickers, and a card whose title is another card's name."""
    titles = {"Wealth Overview": {"Expected remaining": f"Expected income, {HORIZONS[0]} months",
                                  "Asset class mix over time": "Asset classes over time"}}.get(base, {})
    return {**titles, **{privacy_name(c): privacy_name(t) for c, t in titles.items()
                         if c not in PRIVACY_EXEMPT_CARDS}}


def base_dashboards():
    """dashboard name -> (description, filter mode, tiles). A tile is
    (card name, row, col, size_x, size_y, time column) on Metabase's
    24-column grid. The filter mode picks the global filters (see
    dashboard_parameters): 'range' for flows/history dashboards, 'asof'
    for point-in-time holdings dashboards, 'returns' for the returns
    dashboards (a start-year picker and no time filter — the periods are
    precomputed buckets, and the summary rows ignore windows by
    construction), None for no filters. Every filtered mode carries a
    required currency picker. The time filter lands on each card's time
    column (as_of_day for history cards, occurred_at for transactions,
    snapshot_at for latest-snapshot cards): a native card's own tag, or
    the tile's time column below for an MBQL one. A native tile ignores
    that column, and only the Spending dashboards carry MBQL tiles that
    take a time filter. A Wealth Overview tile's time column is unread:
    its mappings come from its card's tags or its home tile
    (overview_mappings). The source filter
    lands on silver_source_id — in 'returns' mode on every tile but the
    whole-portfolio scalars (RETURNS_GLOBAL_SCALARS), whose
    silver_source_id is ''. Data Freshness is deliberately unfiltered —
    its job is to show every source, especially the stale ones a time
    filter would hide."""
    note = ("Pre-defined by wealthdb and converged to spec on every `web "
            "start` — duplicate into another collection before customizing.")
    return {
        "Wealth Overview": (
            "The whole picture in a chosen currency (default "
            f"{DEFAULT_CURRENCY}): net worth and its asset classes over time, then a "
            "figure or two from the other dashboards, each opening the "
            "dashboard it restates. " + note, "range", [
            # Net worth = positions + cash by construction — the first
            # three tiles reconcile exactly; the trend tile is a monthly
            # AVERAGE, so it intentionally differs from today's value.
            # The stalest source sits beside them: the warning to read
            # before trusting any figure.
            ("Net worth", 0, 0, 4, 3, "snapshot_at"),
            ("Positions value", 0, 4, 5, 3, "snapshot_at"),
            ("Cash balance", 0, 9, 5, 3, "snapshot_at"),
            ("Net worth — monthly trend", 0, 14, 5, 3, "as_of_day"),
            ("Stalest source (days)", 0, 19, 5, 3, None),
            # The net worth chart, then its mix by asset class.
            ("Net worth over time", 3, 0, 24, 8, "as_of_day"),
            ("Asset class mix over time", 11, 0, 24, 8, "as_of_day"),
            # Under DASHBOARD_HEADINGS' two headings: the figures over
            # the time range, then those at its end, today and ahead,
            # three to a row so each title reads whole. Their time column
            # is unread (overview_mappings).
            ("Received income", 20, 0, 8, 3, None),
            ("Net spend", 20, 8, 8, 3, None),
            ("Spend — monthly trend", 20, 16, 8, 3, None),
            ("Net cash flow", 23, 0, 8, 3, None),
            ("Savings rate", 23, 8, 8, 3, None),
            ("Realized gain", 23, 16, 8, 3, None),
            ("Unrealized gain", 27, 0, 8, 3, None),
            ("Return year to date (TWR)", 27, 8, 8, 3, None),
            ("Return year to date (MWR)", 27, 16, 8, 3, None),
            ("Income yield", 30, 0, 8, 3, None),
            ("Expected remaining", 30, 8, 8, 3, None),
            ("Projected net cash flow", 30, 16, 8, 3, None),
        ]),
        "Allocation": (
            "Where the value sits — asset class, currency, tax wrapper, "
            "management style, the largest positions and the largest "
            "issuers — as of a chosen day (default: the latest holdings), "
            f"in a chosen currency (default {DEFAULT_CURRENCY}). "
            + note, "asof", [
            # The two taxonomy dimensions side by side on the top row.
            ("Allocation by asset class", 0, 0, 12, 8, "as_of_day"),
            ("Allocation by vehicle", 0, 12, 12, 8, "as_of_day"),
            ("Allocation by currency", 8, 0, 12, 8, "as_of_day"),
            ("Value by tax wrapper", 8, 12, 12, 8, "as_of_day"),
            ("Value by management style", 16, 0, 24, 6, "as_of_day"),
            ("Top 100 positions", 22, 0, 24, 8, "as_of_day"),
            ("Top 100 issuers", 30, 0, 24, 9, "as_of_day"),
        ]),
        "Returns": (
            "How the portfolio performed — time-weighted (TWR) and "
            "money-weighted (MWR) returns, per period and per source, in a "
            f"chosen currency (default {DEFAULT_CURRENCY}). Use the Start-year picker to "
            "rescope past the noisy inception period (0 = since inception); "
            "the since-inception TWR is often n/a because the first months "
            "are degenerate. " + note,
            "returns", [
            # Scalars rescope to the since-<start year> window; charts split
            # by source with the global grain as a toggleable '(all sources)'
            # line. MWR is a summary figure only (the per-period buckets
            # carry TWR); that is engine behavior the scalars mirror.
            ("Return (TWR)", 0, 0, 8, 3, None),
            ("Return (MWR)", 0, 8, 8, 3, None),
            ("Annualized return (TWR)", 0, 16, 8, 3, None),
            ("Cumulative return (TWR)", 3, 0, 24, 8, None),
            ("Monthly returns (TWR)", 11, 0, 8, 6, None),
            ("Quarterly returns (TWR)", 11, 8, 8, 6, None),
            ("Annual returns (TWR)", 11, 16, 8, 6, None),
            ("Returns by source", 17, 0, 24, 8, None),
        ]),
        "Spending": (
            "Where the money goes — the trend, the categories behind it, "
            "the merchants and accounts it left through, and what the cards "
            f"owe — over a chosen window in a chosen currency (default {DEFAULT_CURRENCY}). "
            "Spending is what the tracked accounts paid out; "
            "own-account moves are not spend and never appear. " + note,
            "range", [
            # The three headline figures, then the shape of the window
            # (months x category), then the two breakdown levels side by
            # side — the primary tile drills through to the lines behind
            # a bar, the detailed tile holds the same window one level
            # down. Merchants and accounts answer "to whom" and "from
            # where"; the card-balances chart is the only tile off
            # web_card_balances_history, and the transaction list is the
            # bottom of the drill-down.
            ("Spend — monthly trend", 0, 0, 8, 3, "occurred_at"),
            ("Net spend", 0, 8, 8, 3, "occurred_at"),
            ("Uncategorized share", 0, 16, 8, 3, "occurred_at"),
            ("Spending by month", 3, 0, 24, 9, "occurred_at"),
            ("Spending by category", 12, 0, 12, 8, "occurred_at"),
            ("Spending by subcategory", 12, 12, 12, 8, "occurred_at"),
            ("Top 50 merchants", 20, 0, 12, 8, "occurred_at"),
            ("Spend by account", 20, 12, 12, 8, "occurred_at"),
            ("Card balances over time", 28, 0, 24, 6, "as_of_day"),
            ("Largest transactions", 34, 0, 24, 8, "occurred_at"),
        ]),
        "Income": (
            "What the tracked accounts received and what the holdings are "
            "expected to pay. The income lines are wages, interest, "
            "dividends, staking rewards, rent and gifts, typed and "
            "attributed to a payer, gross as booked. The holdings' income "
            "shows by type, month, account and position: what was "
            "received, what the holdings and an account's rent or pension "
            "are expected to pay, and a calendar of the payments. Every "
            "received figure follows the Time range, and every expected "
            "one the Horizon, under the Rate path, Maturing and Shift "
            "pickers. The Withholding tile shows the tax a source "
            "withheld, and what the withholding table withholds from the "
            "income expected. The Type picker narrows the income lines' "
            "cards. " + note,
            "range", [
            # The headline figures first, then the charts, then the
            # tables: what was received and is expected, beside the
            # income lines' net income and backlog; the trend and the
            # withholding; both halves by month, then their interest and
            # dividends; the types beside the payers; then by account
            # and by position, the calendar a position's row picks, and
            # the largest receipts.
            ("Income in the window", 0, 0, 4, 3, None),
            ("Received income", 0, 4, 4, 3, None),
            ("Expected remaining", 0, 8, 4, 3, None),
            ("Net income", 0, 12, 4, 3, "occurred_at"),
            ("Monthly average income", 0, 16, 4, 3, None),
            ("Uncategorized income share", 0, 20, 4, 3, "occurred_at"),
            ("Income — monthly trend", 3, 0, 8, 3, "occurred_at"),
            ("Withholding", 3, 8, 8, 3, None),
            ("Income after withholding", 3, 16, 8, 3, None),
            ("Received and expected by month", 6, 0, 24, 9, None),
            ("Received interest", 15, 0, 6, 3, None),
            ("Received dividends", 15, 6, 6, 3, None),
            ("Expected interest remaining", 15, 12, 6, 3, None),
            ("Expected dividends remaining", 15, 18, 6, 3, None),
            ("Income by type", 18, 0, 12, 8, "occurred_at"),
            ("Top 50 payers", 18, 12, 12, 8, "occurred_at"),
            ("Received and expected by account", 26, 0, 24, 7, None),
            ("Received and expected by position", 33, 0, 24, 10, None),
            ("Expected payments", 43, 0, 24, 8, None),
            ("Largest receipts", 51, 0, 24, 8, "occurred_at"),
        ]),
        "Cash Flow": (
            "Where the household's cash came from and where it went: the "
            "cash flow statement and the Sankey that draws it. The "
            "household is the accounts in its own tax wrappers; "
            "retirement plans, trusts and charitable vehicles are "
            "vehicles it pays into and draws on. Own-account moves are "
            "invisible, and buying and selling is shown NET. The cash flow "
            "projection sets the income the holdings are expected to pay "
            "against the average outflows of the past months, month by "
            "month over the Horizon, under the Rate path, Maturing and Shift "
            "pickers. " + note,
            "range", [
            # Five headline figures, then the diagram at the centre, then
            # the shape of the window and the projection of the months ahead,
            # then the two sides of operating, then the swing sections,
            # then the lines.
            ("Operating in", 0, 0, 5, 3, "occurred_at"),
            ("Operating out", 0, 5, 5, 3, "occurred_at"),
            ("Net cash flow", 0, 10, 5, 3, "occurred_at"),
            ("Savings rate", 0, 15, 4, 3, "occurred_at"),
            ("Yield share", 0, 19, 5, 3, "occurred_at"),
            # Full width and tall: the diagram is the dashboard.
            ("Cash flow", 3, 0, 24, 12, "occurred_at"),
            ("Cash flow statement by month", 15, 0, 24, 8, "occurred_at"),
            ("Cash flow projection by month", 23, 0, 24, 8, None),
            ("Cash flow projection", 31, 0, 24, 9, None),
            ("Inflows by class by month", 40, 0, 12, 7, "occurred_at"),
            ("Outflows by class by month", 40, 12, 12, 7, "occurred_at"),
            ("Investing by month", 47, 0, 12, 7, "occurred_at"),
            ("Financing and vehicles by month", 47, 12, 12, 7, "occurred_at"),
            ("Largest flows", 54, 0, 24, 8, "occurred_at"),
        ]),
        "Gains": (
            "Realized and unrealized gains and losses over a chosen window, "
            f"in a chosen currency (default {DEFAULT_CURRENCY}): the totals, "
            "the tax years, the shape by month, where the gains came from, "
            "the positions behind them, and what the figures miss. The "
            "window selects whole months; a figure at its end reads the "
            "last month in it. Price gain only: income, fees and taxes are "
            "on the Returns dashboard. " + note,
            "range", [
            # Coarse to fine, so scrolling down reveals detail: the
            # totals, the tax years, the months, where the gains came
            # from, the monthly splits, then the positions, and last
            # what the figures miss.
            # The held total and the taxable realized gain first; then
            # the gains on positions sold and held side by side, which
            # add up to the total gain beside them.
            ("Unrealized gain", 0, 0, 4, 3, "period"),
            ("Realized gain", 0, 4, 4, 3, "period"),
            ("Gain on positions sold", 0, 8, 4, 3, "period"),
            ("Gain on positions held", 0, 12, 4, 3, "period"),
            ("Total gain", 0, 16, 4, 3, "period"),
            ("Cost basis coverage", 0, 20, 4, 3, "period"),
            ("Realized gains by tax year", 3, 0, 24, 6, "period"),
            ("Gains by month", 9, 0, 24, 8, "period"),
            ("Gain by source", 17, 0, 8, 8, "period"),
            ("Gain by asset class", 17, 8, 8, 8, "period"),
            ("Realized gains by tax wrapper", 17, 16, 8, 8, "period"),
            ("Realized gains by month, short vs long term", 25, 0, 12, 6, "period"),
            ("Unrealized gain over time", 25, 12, 12, 6, "period"),
            ("Unrealized gain vs. percent", 31, 0, 16, 8, "period"),
            ("Positions in the green", 31, 16, 8, 4, "period"),
            ("Positions in the red", 35, 16, 8, 4, "period"),
            # The winners and losers two to a row: a table narrower
            # than half the grid cannot show one name in full.
            ("Largest unrealized gains", 39, 0, 12, 8, "period"),
            ("Largest unrealized losses", 39, 12, 12, 8, "period"),
            ("Best unrealized %", 47, 0, 12, 8, "period"),
            ("Worst unrealized %", 47, 12, 12, 8, "period"),
            ("Largest realized gains", 55, 0, 12, 8, "period"),
            ("Largest realized losses", 55, 12, 12, 8, "period"),
            ("Best realized %", 63, 0, 12, 8, "period"),
            ("Worst realized %", 63, 12, 12, 8, "period"),
            ("Coverage by source", 71, 0, 24, 8, "period"),
        ]),
        "Exposure": (
            "What is really owned: every fund looked through to what it "
            "holds and added to what is held directly, by asset class, "
            "sector, size, region, country and currency, at each month end and "
            f"today, in a chosen currency (default {DEFAULT_CURRENCY}). A "
            "breakdown reads the latest day in the window; the map colours "
            "each country by its share; a chart over time shows every month "
            "end. Exposure by legal situs shows where each holding legally "
            "sits, a fund whole at its own. The Source, Tax wrapper and "
            "Asset class pickers narrow every chart to a selection, whose "
            "shares then read as shares of the selection. Credit and "
            "maturity show the debt and the cash, coupon the debt, the cycles "
            "the public equity, each bucket as a share of what its chart "
            "counts. The last chart says how much of net worth the figures "
            "rest on. " + note,
            "range", [
            # The allocation first, the ring beside the sectors and the
            # classes over time full width; then where it sits: the map
            # full width, the regions beside every region's countries, the
            # regions over time, and where it legally sits, full width;
            # then what it moves with, the currency ring beside its
            # history; then public equity by size beside what funds hold;
            # then the debt by credit beside its maturities, its coupons
            # beside the equity's cycles, and the issuers, full width;
            # last, how much the figures cover.
            ("Exposure by asset class", 0, 0, 12, 9, "as_of_day"),
            ("Exposure by sector", 0, 12, 12, 9, "as_of_day"),
            ("Asset classes over time", 9, 0, 24, 8, "as_of_day"),
            ("Exposure by country", 17, 0, 24, 10, "as_of_day"),
            ("Exposure by region", 27, 0, 11, 9, "as_of_day"),
            ("Exposure by region and country", 27, 11, 13, 9, "as_of_day"),
            ("Regions over time", 36, 0, 24, 8, "as_of_day"),
            ("Exposure by legal situs", 44, 0, 24, 8, "as_of_day"),
            ("Exposure by currency", 52, 0, 12, 9, "as_of_day"),
            ("Currencies over time", 52, 12, 12, 9, "as_of_day"),
            ("Public equity by size", 61, 0, 12, 7, "as_of_day"),
            ("Held directly or through funds", 61, 12, 12, 7, "as_of_day"),
            ("Debt by credit rating, share of debt and cash", 68, 0, 12, 8, "as_of_day"),
            ("Debt by maturity", 68, 12, 12, 8, "as_of_day"),
            ("Debt by coupon kind", 76, 0, 12, 7, "as_of_day"),
            ("Public equity by cycle", 76, 12, 12, 7, "as_of_day"),
            ("How the figures are classified", 83, 0, 24, 7, "as_of_day"),
        ]),
        "Yield": (
            "What the holdings earn and how their value moves with rates, as "
            "of the window's last day, in a chosen currency (default "
            f"{DEFAULT_CURRENCY}): the yields, each currency's, cash against "
            "its benchmark, yield against duration, the rate risk along the "
            "curve, what a one-percent move does to the income, dividends "
            "against payout, the yields and the duration at each month end, "
            "and what the figures rest on. It covers the last twelve months. "
            "The income yield and the projected income read today's "
            "projection under the Rate path, Maturing, Shift and Horizon "
            "pickers, the sensitivity under Maturing and Horizon. The Source, "
            "Tax wrapper and Asset class pickers narrow every chart. " + note,
            "range", [
            # The four headline yields, then each currency's beside what a
            # move in rates does to its income; cash against its
            # benchmark; the debt's yield against its duration beside its
            # DV01 along the curve; the stocks' dividends beside what the
            # figures rest on; last, the yields and the duration over
            # time.
            ("Income yield", 0, 0, 6, 3, "as_of_day"),
            ("Projected income", 0, 6, 6, 3, "as_of_day"),
            ("Implied yield", 0, 12, 6, 3, "as_of_day"),
            ("Realized yield", 0, 18, 6, 3, "as_of_day"),
            ("Yield by currency", 3, 0, 12, 8, "as_of_day"),
            ("Income sensitivity to rates", 3, 12, 12, 8, "as_of_day"),
            ("Cash against its benchmark", 11, 0, 24, 8, "as_of_day"),
            ("Yield against duration", 19, 0, 12, 9, "as_of_day"),
            ("DV01 along the curve", 19, 12, 12, 9, "as_of_day"),
            ("Dividend yield against payout", 28, 0, 12, 8, "as_of_day"),
            ("What the yields rest on", 28, 12, 12, 8, "as_of_day"),
            ("Yields over time", 36, 0, 12, 7, "as_of_day"),
            ("Duration over time", 36, 12, 12, 7, "as_of_day"),
        ]),
        "Data Freshness": (
            "How long ago each source was last read — which feeds need a "
            "collector run. Unfiltered by design: it must show every "
            "source, especially stale ones. " + note, None, [
            ("Stalest source (days)", 0, 0, 8, 3, None),
            ("Returns age (days)", 0, 8, 8, 3, None),
            ("Source freshness", 3, 0, 24, 10, None),
        ]),
    }


def _card_maker(out, db_id, privacy, pickers, tags, tail):
    """The `card` function of one family of native cards. It stores a
    tile's card in `out` as name -> (display, description, dataset_query,
    viz): its description ends in `tail`, it takes the template tags
    `tags` unless the tile names its own, and it registers the family's
    `pickers` (register_native_targets). With `privacy` it builds the
    twin's card under its privacy name, and none for a privacy-exempt
    tile."""
    def card(name, display, desc, sql, viz, card_tags=tags, tail=tail):
        if privacy and name in PRIVACY_EXEMPT_CARDS:
            return
        name = privacy_name(name) if privacy else name
        register_native_targets(name, card_tags, pickers)
        out[name] = (display, desc + tail, _native(db_id, sql, card_tags), viz)
    return card


def _window_end(view, tags, share=False, where=""):
    """The CTEs of a card that reads serving view `view` at the latest
    day in the window: `w`, the window's rows in the picked currency that
    `tags` and the predicate `where` keep, each with its share of its
    day's total as `share_pct` with `share`; and `e`, w's rows of its
    latest day. Returns (`w` alone, `w` and `e`), neither ending in a
    newline."""
    cols = (", 100 * value / NULLIF(sum(value) OVER (PARTITION BY as_of_day), 0) AS share_pct\n  FROM "
            if share else "\n    FROM ")
    w = ("WITH w AS (\n  SELECT *" + cols + view + _native_where(tags)
         + "\n     AND currency = {{currency}}" + where + ")")
    return w, w + ",\ne AS (SELECT * FROM w WHERE as_of_day = (SELECT max(as_of_day) FROM w))"


def exposure_cards(db_id, privacy):
    """The Exposure dashboard's cards, or with `privacy` their privacy
    variants: name -> (display, description, dataset_query, viz). Native
    SQL over web_exposure (gold migrations 0127 and 0130), the look-through at
    every month end and today, materialized per reporting currency, so
    {{currency}} is a row filter as on Gains, and per source and tax
    wrapper. The pickers narrow those and the asset class. Every share
    is of the selection, its day's net worth with nothing picked,
    computed over the rows a card reads. Sizes and cycles plot a share
    of the public equity instead, and the debt cards one of the debt, or
    of the debt and cash. A breakdown reads the latest day in the window
    and plots each share: a bar carries its figure, a ring its
    percentage in the legend, a map colours each country by its share.
    Its base card keeps the value as a column and shows it in the
    tooltip, or draws the ring from it; its privacy card has the share
    alone, a share being scale-free. A chart over time shows each month
    end's shares on both, so it is privacy-exempt and has no privacy
    variant.

    The debt cards read web_exposure_debt (migration 0138), at the same
    instants and currencies. docs/EXPOSURE.md defines every figure."""
    tags = native_tags("web_exposure", EXPOSURE_FILTERS)
    desc_end = (" Built for the Exposure dashboard; opened standalone it runs in "
                f"{DEFAULT_CURRENCY}, the currency variable's default, over every month end."
                + (PRIVACY_DESC if privacy else ""))
    # The selection, each row's share of its day's selected total, and
    # its rows at the window's end.
    last = _window_end("web_exposure", tags, share=True)[1]
    end = last + "\n"
    money = "" if privacy else ",\n       sum(value) AS value"
    out = {}
    card = _card_maker(out, db_id, privacy, EXPOSURE_PICKERS, tags, desc_end)

    def bars(label, metric, tooltip=(), decimals=None):
        """The viz of a breakdown's bars: the share, its figure on each
        bar, and in the tooltip the value on a base card, then the
        `tooltip` columns."""
        viz = {"graph.dimensions": [label], "graph.metrics": [metric],
               "graph.show_values": True, **_share_viz(metric, decimals=decimals)}
        cols = ([] if privacy else ["value"]) + list(tooltip)
        if cols:
            viz["graph.tooltip_columns"] = cols
        return viz

    def ring(label):
        """The viz of a breakdown's ring, slices under one percent folded.
        A base card draws the ring from the values, so a slice's hover
        gives the amount with the percent; a privacy card draws it from
        the shares, with no figure in the hole (see _donut). Metabase
        leaves a negative slice out with a warning, which the card's
        description says."""
        return {"pie.dimension": label, "pie.metric": "share_pct" if privacy else "value",
                **_donut(threshold=1, total=not privacy)}

    def at_end(name, display, desc, dim, label, extra=()):
        """A breakdown at the window's end, a ring or bars: one row per
        value of `dim` and of the `extra` columns shown beside it."""
        cols = "".join(f",\n       {c}" for c in extra)
        card(name, display, desc,
             end + f"SELECT {dim} AS {label}{cols}{money},\n       sum(share_pct) AS share_pct\n"
             "  FROM e\n GROUP BY ALL\n ORDER BY sum(share_pct) DESC, 1",
             ring(label) if display == "pie" else bars(label, "share_pct"))

    def over_time(name, desc, dim, label, fold=False):
        """Each month end's shares, stacked by the values of `dim`, the
        rows smallest first by the share at the window's end, as
        _series_order orders any stacked chart. With `fold`, a value
        under one percent at the window's end, or gone by then, joins the
        `other` band, which tops the stack. It ranks with an aggregate of
        its own: the DuckDB in Metabase's driver fails on that helper's
        window functions over w, whose share is a window function
        itself."""
        rank = "CASE WHEN r.latest >= 1 THEN r.latest ELSE -1 END" if fold else "COALESCE(r.latest, -1)"
        band = f"CASE WHEN r.latest >= 1 THEN w.{dim} ELSE 'other' END" if fold else f"w.{dim}"
        card(name, "area", desc,
             last + f",\nr AS (SELECT {dim} AS k, sum(share_pct) AS latest FROM e GROUP BY 1)\n"
             f"SELECT w.as_of_day, {band} AS {label},\n       sum(w.share_pct) AS share_pct\n"
             f"  FROM w\n  LEFT JOIN r ON r.k = w.{dim}\n GROUP BY 1, 2\n"
             f" ORDER BY max({rank}), 1",
             {"graph.dimensions": ["as_of_day", label], "graph.metrics": ["share_pct"],
              **STACKED_VIZ, **_share_viz("share_pct")})

    at_end("Exposure by asset class", "pie",
           "Net worth by asset class after every fund is looked through to "
           "what it holds, at the latest day in the window. A loan is a "
           "negative share, which a ring cannot draw: it is left out, with "
           "a warning, and the other slices read a little high.", "asset_class", "asset_class")
    over_time("Asset classes over time",
              "Each month end's net worth by asset class, after look-through, "
              "as shares that stack to 100, the largest at the latest day "
              "at the bottom.", "asset_class", "asset_class")
    at_end("Exposure by sector", "bar",
           "Net worth by sector at the latest day in the window: a "
           "company's sector, a bond's issuer kind, or cash, metal, crypto "
           "and real estate for holdings that are no company. Unknown "
           "is what no source describes.", "sector", "sector")
    at_end("Exposure by region", "row",
           "Net worth by region, at the latest day in the window: the "
           "region of the country of risk, or the one a declaration "
           "states. A metal or a coin has no country. A bar, not a pie: a "
           "loan can make a region negative.", "region", "region")
    over_time("Regions over time",
              "Each month end's net worth by region, as shares that stack to "
              "100, the largest at the latest day at the bottom.", "region", "region")
    card("Exposure by country", "map",
         "Where net worth sits, at the latest day in the window: each "
         "country coloured by its share, by the country of risk or the one "
         "a declaration states. Metals, coins and what no source places "
         "have no country and are not on the map.",
         end + f"SELECT country,\n       country_name{money},\n       sum(share_pct) AS share_pct\n"
         "  FROM e\n WHERE country NOT IN ('(none)', 'unknown')\n GROUP BY ALL\n"
         " ORDER BY sum(share_pct) DESC, 1",
         {"map.type": "region", "map.region": "world_countries", "map.dimension": "country",
          "map.metric": "share_pct", **_share_viz("share_pct")})
    card("Exposure by region and country",
         "table",
         "Every region with its total, then the countries inside it, at "
         "the latest day in the window, largest first. A region's own row "
         "names no country. Metals and coins have no country, and what no "
         "source places is unknown.",
         end + "SELECT * EXCLUDE (region_rank)\n"
         f"  FROM (SELECT region, country, country_name{money},\n"
         "               sum(share_pct) AS share_pct,\n"
         "               sum(sum(share_pct)) OVER (PARTITION BY region) AS region_rank\n"
         "          FROM e\n"
         "         GROUP BY GROUPING SETS ((region), (region, country, country_name)))\n"
         " ORDER BY region_rank DESC, region, country IS NOT NULL, share_pct DESC, country",
         _share_viz("share_pct"))
    at_end("Exposure by legal situs", "bar",
           "Net worth by legal situs, at the latest day in the window. A "
           "holding's situs is the one country whose law it is an interest "
           "under: a fund's domicile, a share's issuer, the bank that holds "
           "a deposit. No fund is looked through: a fund counts whole at its "
           "own situs, so this is not where the value is exposed. "
           "Unknown is what nothing places, coins among it.", "situs", "situs")
    at_end("Exposure by currency", "pie",
           "Net worth by the currency each value moves with, at the "
           "latest day in the window: a stock in its trading currency, a "
           "hedged class in its hedge currency, metals and coins as "
           "themselves. Currencies below one percent share the Other slice; "
           "a negative share cannot be drawn and is left out, with a "
           "warning.", "exposure_currency", "currency", extra=("currency_kind",))
    over_time("Currencies over time",
              "Each month end's net worth by the currency it moves with, as "
              "shares that stack to 100, the largest at the latest day at the bottom. "
              "A currency under one percent at the latest day, or gone by "
              "then, is in the other band.", "exposure_currency", "currency", fold=True)
    over_time("Held directly or through funds",
              "Each month end's net worth split into what is held directly "
              "and what through funds.", "held", "held")
    card("Public equity by size", "bar",
         "Public equity by the size of each company, at the latest day in "
         "the window, as shares of public equity: large from 10bn US "
         "dollars of market cap, mid from 2bn, small below, else by the "
         "company's rank in a broad fund. Unknown is what neither "
         "sizes, and any fund nothing looks through.",
         end + f"SELECT size{money},\n"
         "       100 * sum(share_pct) / sum(sum(share_pct)) OVER () AS class_share_pct\n"
         "  FROM e\n WHERE asset_class = 'public_equity'\n GROUP BY 1\n"
         " ORDER BY CASE size WHEN 'large' THEN 1 WHEN 'mid' THEN 2 WHEN 'small' THEN 3 ELSE 4 END",
         bars("size", "class_share_pct"))
    over_time("How the figures are classified",
              "Each month end's net worth by how its figures are classified: "
              "looked through, proxied, a security a source describes, "
              "declared, cash, metal or crypto, a fund's remainder, a fund "
              "too deep to follow, or unknown. The lower the unknown band, the more the "
              "other charts cover.", "how", "how")

    # The debt by its credit, maturity and coupon, and the public equity
    # by its cycle. Each dimension's buckets add up to the day's net
    # worth, `(none)` holding what the dimension does not describe. A
    # card leaves that row out and plots each bucket's share of the rest,
    # its class: the debt and the cash for credit and maturity, the debt
    # for coupons, the public equity for the cycles. So its bars sum to
    # 100, as the CLI's class_share does over the same selection; where
    # that rest sums to zero or less, a loan outweighing it, no bucket has
    # a share. A share under SMALLEST_BAR_PCT is left out of the bars,
    # where its label would read 0.00%; the CLI's table keeps it. The
    # size of the debt is the asset class ring's. These are compositions
    # as of a day, so no projection picker reaches them. The refresh
    # holds them, and the issuers, for today and the twelve month ends
    # before only, their look-through costing several times the plain
    # one.
    dtags = native_tags("web_exposure_debt", EXPOSURE_DEBT_FILTERS)
    recent = (" It covers the last twelve months: a window that ends before them shows "
              "nothing.")
    class_share = "100 * sum(value) / (SELECT CASE WHEN total > 0 THEN total END FROM c)"

    def class_bars(name, desc, dimension, label, buckets, extra=(), tooltip=()):
        """A dimension's bars at the window's end, in the view's own
        bucket order: each bucket's share of its class, with the `extra`
        columns beside it and the `tooltip` ones in its tooltip."""
        cols = "".join(f",\n       {c}" for c in extra)
        card(name, "bar", desc + f" A bucket under {SMALLEST_BAR_PCT}% of it is left out of the bars." + recent,
             _window_end("web_exposure_debt", dtags, where=f"\n     AND dimension = '{dimension}'")[1] + ",\n"
             "c AS (SELECT sum(value) AS total FROM e WHERE bucket <> '(none)')\n"
             f"SELECT bucket AS {label}{cols}{money},\n       {class_share} AS class_share_pct\n"
             "  FROM e\n WHERE bucket <> '(none)'\n GROUP BY bucket\n"
             f"HAVING coalesce(abs({class_share}) >= {SMALLEST_BAR_PCT}, TRUE)\n"
             f" ORDER BY list_position([{_sql_list(buckets)}], bucket)",
             bars(label, "class_share_pct", tooltip, decimals=2), dtags)

    grade = (f"CASE WHEN bucket IN ({_sql_list(INVESTMENT_GRADE)}) THEN 'IG'"
             f" WHEN bucket IN ({_sql_list(HIGH_YIELD)}) THEN 'HY' END AS grade")
    class_bars("Debt by credit rating, share of debt and cash",
               "Counts bonds and other debt held directly and through funds, by the bucket their "
               "terms, a ledger record or a rating-sliced fund give them, else by their fund's "
               "credit-quality breakdown, else unrated; and cash balances, as a bucket of their own. "
               "Equities are never rated here. Each bar is a bucket's share of that debt and cash at "
               "the latest day in the window: AAA to B (B and below), then unrated and cash. The "
               "grade is IG from AAA to BBB and HY below. A fund sliced by rating reads its range's "
               "worse end. Unrated's unknown share is the part of it nothing describes at all, as "
               "`wealthdb exposure credit` gives it.",
               "credit", "rating", CREDIT_BUCKETS,
               extra=(grade, "100 * sum(abs(value)) FILTER (WHERE unknown) / NULLIF(sum(abs(value)), 0)"
                             " AS unknown_pct"),
               tooltip=("grade", "unknown_pct"))
    class_bars("Debt by maturity",
               "Counts bonds and other debt held directly and through funds, and cash, by their "
               "remaining term: a term deposit to its term's end, any other cash under a year, a "
               "floating note by its maturity, not its next reset. Debt with no maturity of its own "
               "splits by its fund's maturity breakdown, and unknown is debt neither dates. Each bar "
               "is a term's share of that debt and cash at the latest day in the window, shortest first.",
               "maturity", "maturity", MATURITY_BUCKETS)
    class_bars("Debt by coupon kind",
               "Counts bonds and other debt held directly and through funds, by what their coupon "
               "follows: fixed, floating, inflation-linked or zero, as their terms or their fund's "
               "lines state it. Unknown is debt nothing states a coupon for. Cash and equities are "
               "not counted. Each bar is a kind's share of the debt at the latest day in the window.",
               "coupon", "coupon", COUPON_BUCKETS)
    class_bars("Public equity by cycle",
               "Counts public equity held directly and through funds, by how its sector moves with "
               "the economy: cyclical, defensive or sensitive, as the config's cycles override the "
               "built-in map. Unknown is equity with no sector. Debt and cash are not counted. Each "
               "bar is a cycle's share of the public equity at the latest day in the window.",
               "cycle", "cycle", CYCLE_BUCKETS)
    return out


def issuer_cards(db_id, privacy):
    """The Allocation dashboard's issuers card, or with `privacy` its
    privacy variant: name -> (display, description, dataset_query, viz).
    Native SQL over web_exposure_issuers (gold migration 0138): the
    largest issuers of the whole book, per reporting currency, today and
    at the twelve month ends before. The dashboard's as-of day reaches it
    through web_sources_history, which holds every day, and the card
    reads the latest instant on or before that day. The issuers view
    carries no source, tax wrapper or asset class, so those pickers do
    not reach it. A security no source names reads its key, which the
    privacy card hides where it is a holding's own; the privacy card also
    drops the value."""
    tags = {"currency": currency_tag(),
            **view_tags("web_sources_history", {"as_of_day": ("as_of_day", "date/single")})}
    tail = (" Built for the Allocation dashboard, which supplies the as-of day; opened standalone it "
            f"runs in {DEFAULT_CURRENCY}, the currency variable's default, at the latest day."
            + (PRIVACY_DESC if privacy else ""))
    out = {}
    card = _card_maker(out, db_id, privacy, ALLOCATION_PICKERS, tags, tail)
    issuer = (f"CASE WHEN issuer LIKE '{HOLDING_KEY_PREFIX}%' THEN '(unnamed)' ELSE issuer END"
              if privacy else "issuer")
    value = "" if privacy else ",\n       value"
    card("Top 100 issuers", "table",
         "The hundred issuers the most value is owed by or invested in, as of the day: a company's "
         "stock, bonds and deposits in one row, by its LEI, else by its name, a deposit by its "
         "custodian. Each row gives its share of net worth, the running share down to it, and how "
         "much of it is equity, debt and cash. What no issuer fits, such as a fund nothing looks "
         "through, real estate or metal, is left out. It reads the latest month end on or before "
         "the day, or the latest refresh, over the last twelve months: an earlier day shows "
         "nothing. It covers the whole book: the Source, Tax wrapper and Asset class pickers do "
         "not reach it.",
         "WITH d AS (\n  SELECT max(as_of_day) AS day\n    FROM web_sources_history"
         + _native_where(tags) + "),\n"
         "w AS (\n  SELECT *\n    FROM web_exposure_issuers\n   WHERE currency = {{currency}}\n"
         "     AND as_of_day <= (SELECT day FROM d)),\n"
         "e AS (SELECT * FROM w WHERE as_of_day = (SELECT max(as_of_day) FROM w))\n"
         f"SELECT rank,\n       {issuer} AS issuer{value},\n"
         "       share_pct, cumulative_share_pct, equity_pct, debt_pct, cash_pct\n"
         "  FROM e\n WHERE issuer <> '(none)'\n ORDER BY rank\n LIMIT 100",
         _share_viz("share_pct", "cumulative_share_pct", "equity_pct", "debt_pct", "cash_pct"))
    return out


# The key gold gives a holding no security key names
# (gold.HoldingKeyPrefix): it can carry an account's id.
HOLDING_KEY_PREFIX = "holding:"

# The exposure views' buckets in their order (gold migration 0137), each
# card's bars drawn in it, and the grades of the credit buckets.
CREDIT_BUCKETS = ("AAA", "AA", "A", "BBB", "BB", "B", "unrated", "cash")
INVESTMENT_GRADE = ("AAA", "AA", "A", "BBB")
HIGH_YIELD = ("BB", "B")
MATURITY_BUCKETS = ("under 1y", "1-3y", "3-5y", "5-10y", "10-20y", "over 20y", "perpetual", "unknown")
COUPON_BUCKETS = ("fixed", "floating", "inflation_linked", "zero", "unknown")
CYCLE_BUCKETS = ("cyclical", "defensive", "sensitive", "unknown")
# The smallest share, in percent, a debt card draws as a bar.
SMALLEST_BAR_PCT = 0.01


# ---- yield: the Yield cards and the Income cards of the holdings' income

def _rate(x):
    """The value-weighted average of rate column `x` over a group's rows
    that state it, weighed as yield.summarize weighs one: over the assets
    (`weighs`: no liability, no line of no positive value), or over every
    row where the group holds no asset (docs/YIELD.md §5)."""
    def avg(cond):
        return (f"sum(value * {x}) FILTER (WHERE {cond}{x} IS NOT NULL)"
                f" / NULLIF(sum(value) FILTER (WHERE {cond}{x} IS NOT NULL), 0)")
    return f"CASE WHEN bool_or(weighs) THEN {avg('weighs AND ')}\n            ELSE {avg('')} END"


# A group's realized yield, pooled as yield.summarize pools a row's: its
# holdings' income of the twelve months over the sum of their mean daily
# values, over the assets, else over every row (gold macro
# yield_realized_pct, migration 0138). The days are not in the table, so
# the engine's thin rule for a row does not apply to a selection.
REALIZED_YIELD = "yield_realized_pct(received_12m, mean_value, weighs)"

# The value a group's income yield divides by, weighed as its rates are.
EARNING_VALUE = "CASE WHEN bool_or(weighs) THEN sum(value) FILTER (WHERE weighs) ELSE sum(value) END"


def _income_yield(income="income_12m"):
    """A group's income yield: its income over the value that earns it."""
    return f"100 * sum({income}) / NULLIF({EARNING_VALUE}, 0)"


# A projection part's income under the picked rate path and shift, in its
# holding's currency (yield.Part.Income): the fixed part, the floating
# principal at the month's benchmark plus its spread and the shift, and a
# rolled holding's income at its rate on the path plus the shift. Each
# floating rate floors at zero. PARTS_FROM joins each part to its rate as
# `rt`; a currency the rates lack reads zero, as the engine's does.
_ROLL = "CASE WHEN {{rates}} = 'forward' THEN p.roll_forward ELSE p.roll_constant END"
PART_INCOME = ("p.fixed + p.principal * GREATEST(0, COALESCE(rt.rate_pct, 0) + p.spread + ({{shift}}) / 100.0) / 1200\n"
               "             + CASE WHEN p.roll_principal = 0 THEN " + _ROLL + "\n"
               "                    ELSE p.roll_principal * GREATEST(0, (" + _ROLL + ") * 1200 / p.roll_principal\n"
               "                                                     + ({{shift}}) / 100.0) / 1200 END")
PARTS_FROM = ("    FROM web_yield_parts p\n"
              "    LEFT JOIN web_yield_rates rt ON rt.currency = p.holding_currency AND rt.month = p.month\n"
              "         AND rt.path = {{rates}}")
HORIZON_MONTHS = "CAST({{horizon}} AS INTEGER)"
# The months of the horizon after the current one: month 0 never counts.
AFTER_THIS_MONTH = " BETWEEN 1 AND " + HORIZON_MONTHS
# The parts under the picked Maturing rule over those months.
PARTS_AHEAD = "\n   WHERE p.rule = {{maturing}}\n     AND p.month" + AFTER_THIS_MONTH
# A part of a holding web_yield's today rows select, as CTE `h`.
HELD_JOIN = ("    JOIN h ON h.silver_source_id = p.silver_source_id"
             " AND h.account_external_id = p.account_external_id\n"
             "         AND h.position_key = p.position_key")
# The kinds the cash card lists, and what the duration scatter reads:
# bonds and the funds that move with rates (docs/YIELD.md §3). A floating
# note implies no yield, so it never plots.
CASH_KINDS = ("deposit", "term_deposit", "money_market")
DURATION_KINDS = ("bond", "bill", "floating_note", "bond_fund", "multi_asset", "money_market")


def yield_cards(db_id, privacy):
    """The Yield dashboard's cards, or with `privacy` their privacy
    variants: name -> (display, description, dataset_query, viz). Native
    SQL over the yield report's figures per holding (web_yield, gold
    migration 0138), a row set per reporting currency and instant (today
    and the last second of each of the twelve months before), so
    {{currency}} is a row filter, as on Exposure. A figure as of a day
    reads the latest day in the window, and a rate weighs the rows that
    state it as yield.summarize does (_rate), so a figure is `wealthdb
    yield global`'s over the same selection.

    A card with an expected figure reads today's projection
    (web_yield_parts) for today's selected holdings under the four
    projection pickers, whatever the Time range. Its income is the
    holdings' alone, as `income_12m` is: an account's rent or pension
    has no holding.

    A privacy card shows shares and rates as they are, DV01 as a share of
    net worth per basis point, and expected income as a yield over the
    value it is earned on; no other money figure and no account label
    reaches it. A card of rates, years and securities alone is
    privacy-exempt, the twin showing it unchanged. docs/YIELD.md defines
    every figure."""
    tags = native_tags("web_yield", YIELD_FILTERS)
    # Today's selected holdings, whatever the window.
    held_tags = native_tags("web_yield", {k: v for k, v in YIELD_FILTERS.items() if k != "time_range"})
    note = (" It covers today and the twelve month ends before: a window that ends before "
            "them shows nothing. Built for the Yield dashboard; opened standalone it runs in "
            f"{DEFAULT_CURRENCY}, the currency variable's default, at the latest day.")
    proj_note = (" It reads today's projection whatever the window. Built for the Yield "
                 f"dashboard; opened standalone it runs in {DEFAULT_CURRENCY} {PROJECTION_DEFAULTS}, "
                 f"over a {HORIZONS[0]}-month horizon, the variables' defaults.")
    if privacy:
        note, proj_note = note + PRIVACY_DESC, proj_note + PRIVACY_DESC
    out = {}
    card = _card_maker(out, db_id, privacy, YIELD_PICKERS, tags, note)
    w, end = _window_end("web_yield", tags)
    money = "" if privacy else ",\n       sum(value) AS value"
    # A group's share of the value that earns, so a liability's group
    # does not turn the assets' shares past 100.
    share = f",\n       100 * {EARNING_VALUE} / NULLIF(sum({EARNING_VALUE}) OVER (), 0) AS share_pct"

    def scalar(name, rate, alias, desc):
        """The selection's `rate` at the window's end, as `alias`."""
        card(name, "scalar", desc, end + f"\nSELECT {rate} AS {alias}\n  FROM e", _share_viz(alias))

    # ---- the projection: today's holdings and their parts
    proj_tags = {**held_tags, **PROJECTION_TAGS}
    held = ("WITH h AS (\n"
            "  SELECT silver_source_id, account_external_id, position_key, holding_currency, value, weighs\n"
            "    FROM web_yield" + _native_where(held_tags) +
            "\n     AND currency = {{currency}}\n     AND today)")
    horizon = (held + ",\nx AS (\n"
               f"  SELECT p.month, ({PART_INCOME}) * {_ccy_case('p.fx')} AS income\n" + PARTS_FROM + "\n" + HELD_JOIN +
               PARTS_AHEAD + ")\n")
    # The horizon's income per year over the value that earns it.
    horizon_yield = ("SELECT 100 * (SELECT sum(income) FROM x) * 12 / " + HORIZON_MONTHS +
                     f"\n       / NULLIF((SELECT {EARNING_VALUE} FROM h), 0) AS income_yield_pct")

    card("Income yield", "scalar",
         "What today's holdings are expected to pay over the horizon after this month, per "
         "year, over the value that earns it, which leaves out liabilities and lines of no "
         "positive value. Under the default pickers it is `wealthdb yield global`'s income "
         "yield.",
         horizon + horizon_yield, _share_viz("income_yield_pct"), proj_tags, proj_note)
    if privacy:
        # As a yield it would be the Income yield beside it, so it shows
        # how evenly the horizon pays instead.
        card("Projected income", "scalar",
             "What today's holdings are expected to pay per month over the horizon after this "
             "month, as a share of its biggest month: 100 for income that comes evenly, less "
             "where coupons and dividends bunch.",
             horizon.rstrip("\n") + ",\n" + _peak_cte("x", "income") +
             "SELECT 100 * (SELECT sum(income) FROM x) / " + HORIZON_MONTHS
             + f"\n       / {PEAK} AS monthly_average_pct",
             _share_viz("monthly_average_pct"), proj_tags, proj_note)
    else:
        card("Projected income", "scalar",
             "What today's holdings are expected to pay over the horizon after this month: "
             "coupons on their dates, dividends at their frequency, cash and floating holdings "
             "at their rate. An account's rent or pension is not a holding's and is on the "
             "Income dashboard.",
             horizon + "SELECT sum(income) AS projected_income\n  FROM x", {}, proj_tags, proj_note)
    scalar("Implied yield", _rate("implied_pct"), "implied_yield_pct",
           "The yield the prices imply at the latest day in the window, weighted by value over "
           "the holdings that have one, in each market's own compounding.")
    scalar("Realized yield", REALIZED_YIELD, "realized_yield_pct",
           "The income the holdings received in the twelve months to the latest day in the "
           "window, over their mean daily value in those months. A day not held counts as "
           "zero. The share of thin holdings, near zero on most of those days, is on the "
           "quality card.")

    card("Yield by currency", "bar",
         "Per currency the holdings are in, at the latest day in the window: the income yield "
         "beside the implied yield. The income yield is what the twelve months after that "
         "day's month were expected to pay, under the default assumptions, over the value that "
         "earns it."
         + (" Each currency's share of the value that earns is in the tooltip." if privacy else ""),
         end + "\nSELECT COALESCE(holding_currency, 'unknown') AS holding_currency,\n"
         f"       {_income_yield()} AS income_yield_pct,\n"
         f"       {_rate('implied_pct')} AS implied_yield_pct" + (share if privacy else money) +
         "\n  FROM e\n GROUP BY 1\n ORDER BY sum(value) DESC, 1",
         {"graph.dimensions": ["holding_currency"], "graph.metrics": ["income_yield_pct", "implied_yield_pct"],
          "graph.show_values": True, "graph.tooltip_columns": ["share_pct" if privacy else "value"],
          **_share_viz("income_yield_pct", "implied_yield_pct", *(["share_pct"] if privacy else []))})

    # The worst paid first: the balances no gap is known for, then the
    # largest shortfall below the benchmark; a debit balance after the
    # credit ones, as `wealthdb yield cash` lists them.
    cash_order = "value < 0, gap_pct NULLS FIRST, value DESC"
    cash_rows = (end + ",\nc AS (\n  SELECT account_label, kind, holding_currency, stated_pct, benchmark_pct, gap_pct, value\n"
                 f"    FROM e\n   WHERE kind IN ({_sql_list(CASH_KINDS)}))\n")
    cash_desc = ("Each cash balance, term deposit and money-market holding at the latest day in the "
                 "window: the rate it earns, the currency's benchmark, and the gap between them, "
                 "the worst paid first. A balance with no gap, its rate or its benchmark unknown, "
                 "comes first; a debit balance comes last.")
    if privacy:
        card("Cash against its benchmark", "table",
             cash_desc + " Ranked, with no account named, each with its share of the credit "
             "balances; a debit balance's is negative.",
             cash_rows + f"SELECT row_number() OVER (ORDER BY {cash_order}) AS rank,\n"
             "       kind, holding_currency AS currency, stated_pct AS rate_pct, benchmark_pct, gap_pct,\n"
             "       100 * value / NULLIF(sum(GREATEST(value, 0)) OVER (), 0) AS share_pct\n"
             "  FROM c\n ORDER BY 1",
             _share_viz("rate_pct", "benchmark_pct", "gap_pct", "share_pct"))
    else:
        card("Cash against its benchmark", "table", cash_desc,
             cash_rows + "SELECT account_label AS account, kind, holding_currency AS currency,\n"
             "       stated_pct AS rate_pct, benchmark_pct, gap_pct, value\n"
             f"  FROM c\n ORDER BY {cash_order}, account",
             _share_viz("rate_pct", "benchmark_pct", "gap_pct"))

    size = "share_pct" if privacy else "value"
    card("Yield against duration", "scatter",
         "Each bond, bill, bond fund, multi-asset fund and money-market holding at the latest day in the window: "
         "its duration in years across, its implied yield up, a colour per currency. "
         + ("Each point is sized by its share of the value plotted." if privacy
            else "Each point is sized by its value.") +
         " A floating note implies no yield and is not plotted.",
         end + "\nSELECT holding_currency AS currency, duration, implied_pct AS implied_yield_pct, symbol, name,\n"
         + ("       100 * value / NULLIF(sum(value) OVER (), 0) AS share_pct" if privacy else "       value") +
         f"\n  FROM e\n WHERE kind IN ({_sql_list(DURATION_KINDS)})\n"
         "   AND duration IS NOT NULL AND implied_pct IS NOT NULL\n ORDER BY value DESC",
         {"graph.dimensions": ["duration", "currency"], "graph.metrics": ["implied_yield_pct"],
          "scatter.bubble": size, "graph.tooltip_columns": ["symbol", "name", size],
          **_share_viz("implied_yield_pct", *(["share_pct"] if privacy else []))})

    # DV01 per curve currency and tenor: a group of bars per currency, the
    # most rate risk first, a bar per tenor from the shortest. Every
    # currency carries every tenor of the day's curves, an empty bar where
    # it has none, so the groups line up. Rows come tenor by tenor, the
    # currencies in their order within each: Metabase takes the series and
    # the groups in the order they first appear.
    grid = (",\nk AS (SELECT curve_currency, sum(abs(dv01)) AS risk FROM e GROUP BY 1),\n"
            "t AS (SELECT DISTINCT tenor, tenor_order FROM e),\n"
            "g AS (SELECT curve_currency, tenor, sum(dv01) AS dv01 FROM e GROUP BY 1, 2)\n"
            "SELECT k.curve_currency AS curve, t.tenor, {dv01}\n"
            "  FROM k CROSS JOIN t\n"
            "  LEFT JOIN g ON g.curve_currency = k.curve_currency AND g.tenor = t.tenor\n"
            " ORDER BY t.tenor_order, k.risk DESC, k.curve_currency")
    grouped = {"graph.dimensions": ["curve", "tenor"], "graph.x_axis.scale": "ordinal"}
    split = " `wealthdb yield curve` splits each bar by what holds it."
    if privacy:
        # A share of net worth per basis point: the selection's DV01 over
        # its net worth that day. The curve rows carry no holding, so they
        # are the ones whose source, wrapper and class the selected
        # holdings carry.
        card("DV01 along the curve", "bar",
             "What a rise in rates of one basis point at each tenor takes off net worth, as a "
             "share of the selection's net worth, at the latest day in the window: a group of bars "
             "per curve currency, the most rate risk first, a bar per tenor. A liability's is "
             "negative." + split,
             w + ",\nd AS (SELECT max(as_of_day) AS day FROM w),\n"
             "nw AS (SELECT sum(value) AS nw FROM w WHERE as_of_day = (SELECT day FROM d)),\n"
             "sel AS (SELECT DISTINCT silver_source_id, tax_wrapper, asset_class FROM w\n"
             "         WHERE as_of_day = (SELECT day FROM d)),\n"
             "e AS (SELECT c.curve_currency, c.tenor, c.tenor_order, c.dv01\n"
             "        FROM web_yield_curve c\n"
             "        JOIN sel ON sel.silver_source_id = c.silver_source_id AND sel.tax_wrapper = c.tax_wrapper\n"
             "             AND sel.asset_class = c.asset_class\n"
             "       WHERE c.currency = {{currency}} AND c.as_of_day = (SELECT day FROM d))"
             + grid.format(dv01="100 * g.dv01 / (SELECT CASE WHEN nw > 0 THEN nw END FROM nw) AS dv01_pct"),
             {**grouped, "graph.metrics": ["dv01_pct"],
              "column_settings": {'["name","dv01_pct"]': {"suffix": "%", "decimals": 4}}})
    else:
        ctags = native_tags("web_yield_curve", YIELD_FILTERS)
        card("DV01 along the curve", "bar",
             "What a rise in rates of one basis point at each tenor takes off the holdings' "
             "value, at the latest day in the window: a group of bars per curve currency, the most "
             "rate risk first, a bar per tenor. A bond's DV01 lands where its cash flows fall, a "
             "fund's at its filed tenors or its duration. A liability's is negative, netting the "
             "assets' risk." + split,
             _window_end("web_yield_curve", ctags)[1] + grid.format(dv01="g.dv01 AS dv01"),
             {**grouped, "graph.metrics": ["dv01"]}, ctags)

    # The change in the horizon's income for rates one percent higher than
    # the picked path and shift: each part's income there less its income
    # here, as the engine's income per 100bp reads it. A floating rate
    # floors at zero, so below zero a point more earns less than a point.
    up = PART_INCOME.replace("({{shift}})", "({{shift}} + 100)")
    sens = (held + ",\ns AS (\n"
            f"  SELECT p.holding_currency, (({up}) - ({PART_INCOME})) * {_ccy_case('p.fx')} AS v\n" + PARTS_FROM +
            "\n" + HELD_JOIN + PARTS_AHEAD + ")")
    sens_desc = ("How much more today's holdings would pay over the horizon after this month were "
                 "rates one percent higher, per currency the holdings are in: the floating "
                 "principal — balances, money-market and floating holdings, and the principal "
                 "maturities return to cash or roll — at one percent. A fixed coupon adds nothing "
                 "until it matures. A floating rate never earns below zero, so where the path and "
                 "the shift take one below zero, a point more earns less than a point.")
    if privacy:
        card("Income sensitivity to rates", "bar",
             sens_desc + " As a yield: the change per year over the value that earns it in that "
             "currency, in percentage points.",
             sens + ",\nv AS (SELECT holding_currency, " + EARNING_VALUE + " AS earning FROM h GROUP BY 1)\n"
             "SELECT s.holding_currency,\n"
             "       100 * sum(s.v) * 12 / " + HORIZON_MONTHS + " / NULLIF(any_value(v.earning), 0) AS yield_change_pct\n"
             "  FROM s\n  LEFT JOIN v ON v.holding_currency = s.holding_currency\n"
             " GROUP BY 1\n ORDER BY 2 DESC NULLS LAST, 1",
             {"graph.dimensions": ["holding_currency"], "graph.metrics": ["yield_change_pct"],
              "graph.show_values": True, **_share_viz("yield_change_pct")}, proj_tags, proj_note)
    else:
        card("Income sensitivity to rates", "bar", sens_desc,
             sens + "\nSELECT holding_currency, sum(v) AS income_change\n"
             "  FROM s\n GROUP BY 1\n ORDER BY 2 DESC NULLS LAST, 1",
             {"graph.dimensions": ["holding_currency"], "graph.metrics": ["income_change"],
              "graph.show_values": True}, proj_tags, proj_note)

    card("Dividend yield against payout", "scatter",
         "Each stock at the latest day in the window: the share of its earnings it pays out "
         "across, its indicated dividend over its price up. A high yield on a payout near or "
         "above 100% is a dividend the earnings do not cover.",
         end + "\nSELECT DISTINCT symbol, name, payout_pct, dividend_yield_pct\n  FROM e\n"
         " WHERE kind = 'stock'\n   AND payout_pct IS NOT NULL AND dividend_yield_pct IS NOT NULL\n"
         " ORDER BY dividend_yield_pct DESC, symbol",
         {"graph.dimensions": ["payout_pct"], "graph.metrics": ["dividend_yield_pct"],
          "graph.tooltip_columns": ["symbol", "name"], **_share_viz("payout_pct", "dividend_yield_pct")})

    # Two charts, not one: a yield and a duration differ in unit, and one
    # chart would need an axis for each.
    yields = ("income_yield_pct", "implied_yield_pct", "realized_yield_pct")
    card("Yields over time", "line",
         "At each month end in the window and today: the income yield, what the next twelve "
         "months were expected to pay under the default assumptions over the value, the "
         "implied yield and the realized yield.",
         w + f"\nSELECT as_of_day,\n       {_income_yield()} AS income_yield_pct,\n"
         f"       {_rate('implied_pct')} AS implied_yield_pct,\n"
         f"       {REALIZED_YIELD} AS realized_yield_pct\n"
         "  FROM w\n GROUP BY 1\n ORDER BY 1",
         {"graph.dimensions": ["as_of_day"], "graph.metrics": list(yields), **LINES_VIZ,
          **_share_viz(*yields)})
    card("Duration over time", "line",
         "At each month end in the window and today: the holdings' modified duration in "
         "years, weighted by value over the holdings that have one.",
         w + f"\nSELECT as_of_day,\n       {_rate('duration')} AS duration\n  FROM w\n GROUP BY 1\n ORDER BY 1",
         {"graph.dimensions": ["as_of_day"], "graph.metrics": ["duration"]})

    # A holding carries every tag that describes a figure of it, so the
    # shares overlap; each is of the gross value, a liability counted
    # whole (docs/YIELD.md §7).
    card("What the yields rest on", "row",
         "The share of the holdings' gross value each tag covers at the latest day in the "
         "window: implied from a price, stated by a source, indicated by a dividend, realized "
         "from the transactions, declared in the ledger, and what is aged, stale, floating, in "
         "default, thin or unknown. A thin holding was near zero on most days of the year, so "
         "it has no realized yield. A holding carries every tag that applies, so the shares "
         "overlap.",
         end + ",\nt AS (SELECT unnest(tags) AS tag, abs(value) AS v FROM e)\n"
         "SELECT tag" + ("" if privacy else ",\n       sum(v) AS value") +
         ",\n       100 * sum(v) / NULLIF((SELECT sum(abs(value)) FROM e), 0) AS share_pct" +
         ("" if privacy else ",\n       count(*) AS holdings") +
         "\n  FROM t\n GROUP BY 1\n ORDER BY share_pct DESC, 1",
         {"graph.dimensions": ["tag"], "graph.metrics": ["share_pct"], "graph.show_values": True,
          **({} if privacy else {"graph.tooltip_columns": ["value", "holdings"]}), **_share_viz("share_pct")})
    return out


# The projection's income types (yield.IncomeTypes), each with its colour
# on the by-month chart: received in it, expected in a lighter shade. The
# hues are a categorical order checked for colour-blind separation of
# neighbours.
INCOME_TYPE_COLORS = {"interest": "#2a78d6", "coupons": "#eb6834", "dividends": "#1baf7a",
                      "distributions": "#eda100", "rent": "#e87ba4", "pension": "#008300"}
# The headline figures' interest, its coupons included, and dividends, a
# fund's distributions included.
INTEREST_TYPES = ("interest", "coupons")
DIVIDEND_TYPES = ("dividends", "distributions")


# The keys the engine gives a cash balance (`cash:<ccy>`) and an
# account's income (`income:<type>:<ccy>`), which name no holding.
SHOWN_KEY_PREFIXES = ("cash:", "income:")
# A position's name on a privacy card: its own, else its key where that
# starts with one of SHOWN_KEY_PREFIXES. Any other key is a holding's
# own, which can carry a contract's or an account's id, so it reads
# (unnamed), as the issuers twin hides one.
TWIN_POSITION_NAME = ("CASE WHEN name IS NOT NULL THEN name WHEN "
                      + " OR ".join(f"position_key LIKE '{k}%'" for k in SHOWN_KEY_PREFIXES)
                      + " THEN position_key ELSE '(unnamed)' END")


def _tint(color, share=0.55):
    """`color` mixed with white by `share`: a series' lighter shade."""
    rgb = [int(color[i:i + 2], 16) for i in (1, 3, 5)]
    return "#" + "".join(f"{round(c + (255 - c) * share):02x}" for c in rgb)


def income_projected_cards(db_id, privacy):
    """The Income dashboard's cards of the holdings' income, or with
    `privacy` their privacy variants: name -> (display, description,
    dataset_query, viz). Native SQL over every income line received
    (web_yield_received, a row set per reporting currency), the tax booked
    on the income's accounts (web_income_withheld), and today's
    projection of what the holdings are expected to pay (web_yield_parts,
    in each holding's currency at today's rate), gold migrations 0138 and
    0146.

    What was received is the Time range's: the picker reaches a card
    through the days web_income_days lists. What is expected is the
    Horizon's: the rest of the current month after today and the
    horizon's months after it, under the three other projection pickers
    (docs/INCOME.md §12). So a figure of income over either half is
    `wealthdb income`'s over the same days and pickers. An account's rent
    and pension count, as they do there. The withholding is not: what was
    received has the tax booked withheld from it, the summary's memo,
    where the forward views apply the withholding table to both halves.

    A privacy card shows a figure as a share of the income it covers, or
    of its biggest month; that income, and a position's, as a yield over
    the value it is earned on; and what the withholding takes as a share.
    No account label, amount, quantity or price reaches it, and it has no
    account picker."""
    selection = {k: v for k, v in INCOME_PROJECTED_SELECTION.items()
                 if not (privacy and k == "account")}
    sel_tags = {}
    for k, (view, col) in selection.items():
        sel_tags.update(view_tags(view, {k: (col, "string/=")}))

    def kept(alias=""):
        """The optional clauses keeping a row whose source and account
        the pickers select."""
        return "".join(f"\n     [[AND {alias}{col} IN (SELECT {col} FROM {view} WHERE {{{{{k}}}}})]]"
                       for k, (view, col) in selection.items() if k in sel_tags)

    # A card with a received figure takes the Time range, one with an
    # expected figure the projection's four, a twin's share of the whole
    # both; the calendar takes the Position picker too.
    range_tags = view_tags("web_income_days", {"time_range": ("day", "date/all-options")})
    received_tags = {"currency": currency_tag(), **range_tags, **sel_tags}
    expected_tags = {"currency": currency_tag(), **PROJECTION_TAGS, **sel_tags}
    both_tags = {**received_tags, **PROJECTION_TAGS}
    calendar_tags = {**expected_tags, "position": POSITION_TAG}

    def standalone(tags):
        """The sentence a card's description ends with: what it reads
        opened on its own, where no picker reaches it."""
        over = []
        if "time_range" in tags:
            over.append("every day to today from the first income received, the first tax withheld "
                        "or 1 January of last year, whichever is earliest")
        if "horizon" in tags:
            over.append(f"the rest of this month and the {HORIZONS[0]} months after it, {PROJECTION_DEFAULTS}")
        return (f" Built for the Income dashboard; opened standalone it runs in {DEFAULT_CURRENCY} over "
                "the variables' defaults: " + "; ".join(over) + "." + (PRIVACY_DESC if privacy else ""))

    out = {}
    make = _card_maker(out, db_id, privacy, INCOME_PROJECTED_PICKERS, both_tags, "")

    def card(name, display, desc, sql, viz, tags=both_tags):
        make(name, display, desc, sql, viz, tags, standalone(tags))

    # The days the Time range selects, every day web_income_days lists
    # with none.
    days_from = "FROM web_income_days" + (" [[WHERE {{time_range}}]]" if range_tags else "")
    days = "SELECT day " + days_from
    # Each part's income in its holding's currency and its rate to the
    # picked one, over the horizon; a payment of an estimated day reads
    # its month.
    expected = ("ex AS (\n"
                "  SELECT p.type, p.month_start, p.month AS m, CASE WHEN NOT p.month_only THEN p.day END AS day,\n"
                f"         p.holding_currency, ({PART_INCOME}) AS amount,\n"
                f"         {_ccy_case('p.fx')} AS fx, p.withholding_pct,\n"
                "         p.basis, p.reinvested, p.silver_source_id, p.account_external_id, p.position_key,\n"
                "         p.account_label, p.vehicle, p.symbol, p.name\n" + PARTS_FROM +
                "\n   WHERE p.rule = {{maturing}}\n     AND p.month <= " + HORIZON_MONTHS + kept("p.") + ")")
    received = ("rx AS (\n  SELECT * FROM web_yield_received\n   WHERE currency = {{currency}}\n"
                f"     AND day IN ({days})" + kept() + ")")
    keys = "silver_source_id, account_external_id, position_key, account_label, vehicle, symbol, name"
    arms = {"expected": (expected,
                         "SELECT 'expected' AS status, type, month_start, m, day, holding_currency, amount,\n"
                         "           amount * fx AS value, amount * fx * withholding_pct / 100 AS withheld,\n"
                         f"           {keys}\n      FROM ex"),
            "received": (received,
                         "SELECT 'received' AS status, type, CAST(date_trunc('month', day) AS DATE) AS month_start,\n"
                         "           NULL AS m, day, NULL AS holding_currency, NULL AS amount, value,\n"
                         f"           NULL AS withheld, {keys}\n      FROM rx")}
    both = ("expected", "received")

    def rows(parts, *extra):
        """The CTEs a card reads, ending with `a`, then the `extra` ones:
        each amount of the `parts`, 'expected' and 'received', in the
        picked currency, an expected one with what the withholding table
        withholds from it."""
        ctes = [arms[x][0] for x in parts]
        ctes.append("a AS (\n    " + "\n    UNION ALL\n    ".join(arms[x][1] for x in parts) + ")")
        return "WITH " + ",\n".join(ctes + list(extra)) + "\n"

    # The months the figures cover: each day received its share of its
    # month, and the horizon's months with the rest of this one.
    months = ("mo AS (\n"
              f"  SELECT (SELECT coalesce(sum(1 / day(last_day(day))), 0) {days_from}) AS received,\n"
              f"         {HORIZON_MONTHS} + coalesce((SELECT (day(last_day(max(day))) - day(max(day)))"
              " / day(last_day(max(day))) FROM web_income_days), 0) AS expected)")
    # The tax a source withheld on the days received, which the
    # withholding table does not model.
    taxed = ("tx AS (\n  SELECT sum(" + _ccy_case("value") + ") AS withheld\n    FROM web_income_withheld\n"
             f"   WHERE day IN ({days})" + kept() + ")")
    # The biggest month, for the twins (PEAK).
    peak = _peak_cte("a", "value", "month_start").rstrip()

    # ---- the headline figures
    def tile(name, col, cond, desc, part=None):
        """The income of the rows `cond` selects: what was received over
        the Time range with `part` 'received', what is expected over the
        Horizon with 'expected', both without one. On the privacy card,
        its share of what was received, or of both."""
        pick = f" FILTER (WHERE {cond})" if cond else ""
        if privacy:
            parts, tags, of = (((part,), received_tags, "what was received: 100 for the total")
                               if part == "received" else (both, both_tags, "the income received and expected"))
            card(name, "scalar", desc + f" As a share of {of}, 0 where that is nothing.",
                 rows(parts) + f"SELECT coalesce(100 * coalesce(sum(value){pick}, 0) / NULLIF(sum(value), 0), 0)"
                 f" AS {col}_pct\n  FROM a", _share_viz(f"{col}_pct"), tags)
        else:
            parts, tags = {"received": ((part,), received_tags), "expected": ((part,), expected_tags),
                           None: (both, both_tags)}[part]
            card(name, "scalar", desc, rows(parts) + f"SELECT coalesce(sum(value){pick}, 0) AS {col}\n  FROM a",
                 {}, tags)

    interest, dividends = (f"type IN ({_sql_list(t)})" for t in (INTEREST_TYPES, DIVIDEND_TYPES))
    expect = "status = 'expected'"
    if privacy:
        h = ("h AS (SELECT value, weighs FROM web_yield\n"
             "        WHERE today\n          AND currency = {{currency}}" + kept() + ")")
        card("Income in the window", "scalar",
             "The income received over the Time range and expected over the Horizon, as a yield: per "
             "year, over the value today's holdings earn it on; 0 where there is none.",
             rows(both, months, h) + "SELECT coalesce(100 * (SELECT sum(value) FROM a) * 12"
             " / (SELECT received + expected FROM mo)\n"
             f"       / NULLIF((SELECT {EARNING_VALUE} FROM h), 0), 0) AS income_yield_pct",
             _share_viz("income_yield_pct"))
        card("Monthly average income", "scalar",
             "The income per month, as a share of its biggest month; 0 where no month has any.",
             rows(both, months, peak) +
             "SELECT coalesce(100 * (SELECT sum(value) FROM a) / (SELECT received + expected FROM mo)\n"
             f"       / {PEAK}, 0) AS monthly_average_pct",
             _share_viz("monthly_average_pct"))
    else:
        tile("Income in the window", "income", "",
             "The income received over the Time range and expected over the Horizon.")
        card("Monthly average income", "scalar",
             "The income received over the Time range and expected over the Horizon, over the months "
             "they cover: each day received counts its share of its month, and the Horizon its months "
             "and the rest of this one.",
             rows(both, months) + "SELECT coalesce(sum(value), 0) / (SELECT received + expected FROM mo)"
             " AS monthly_average\n  FROM a", {})
    tile("Received income", "received", "", "The income received over the Time range.", "received")
    tile("Received interest", "received_interest", interest,
         "The interest and coupons received over the Time range.", "received")
    tile("Received dividends", "received_dividends", dividends,
         "The dividends and fund distributions received over the Time range.", "received")
    tile("Expected remaining", "expected", expect,
         "What the holdings, and an account's rent or pension, are expected to pay after today, over "
         "the Horizon.", "expected")
    tile("Expected interest remaining", "expected_interest", f"{expect} AND {interest}",
         "The interest and coupons expected after today, over the Horizon.", "expected")
    tile("Expected dividends remaining", "expected_dividends", f"{expect} AND {dividends}",
         "The dividends and fund distributions expected after today, over the Horizon.", "expected")

    # What was withheld from the income received is the tax booked on the
    # income's accounts, every negative tax row there, as the CLI's
    # `withheld` memo counts it; what will be withheld from the income
    # expected is what the config's withholding table withholds at each
    # holding's situs and account wrapper, a model. A line the table names
    # no rate for withholds nothing, and with no table the expected part
    # is nothing.
    withheld = "coalesce((SELECT withheld FROM tx), 0) + coalesce(sum(withheld), 0)"
    withheld_desc = ("The tax withheld at source: over the Time range, the tax booked on the income's "
                     "accounts, every tax line `wealthdb income summary -C +withheld` counts, whatever "
                     "income it was withheld from; over the Horizon, what the withholding table in the "
                     "config withholds from the income expected at each holding's situs and account "
                     "wrapper, a model. With no table it is the tax booked alone, and the tile's title "
                     "says so. It reads 0 where nothing was withheld.")
    net_desc = ("The income received over the Time range and expected over the Horizon, less what "
                "was and will be withheld from it.")
    if privacy:
        card("Withholding", "scalar", withheld_desc + " As the share of the income it takes, 0 where there "
             "is no income.",
             rows(both, taxed) + f"SELECT coalesce(100 * ({withheld}) / NULLIF(sum(value), 0), 0) AS withheld_pct"
             "\n  FROM a", _share_viz("withheld_pct"))
        card("Income after withholding", "scalar", net_desc + " As a share of the income, 0 where there is none.",
             rows(both, taxed) + f"SELECT coalesce(100 * (sum(value) - ({withheld})) / NULLIF(sum(value), 0), 0)"
             " AS net_pct\n  FROM a", _share_viz("net_pct"))
    else:
        card("Withholding", "scalar", withheld_desc,
             rows(both, taxed) + f"SELECT {withheld} AS withheld\n  FROM a", {})
        card("Income after withholding", "scalar", net_desc,
             rows(both, taxed) + f"SELECT coalesce(sum(value), 0) - ({withheld}) AS net\n  FROM a", {})

    # ---- received and expected by month, on one axis
    series = "type || ' (' || status || ')' AS series"
    colors = {f"{t} ({s})": {"color": c if s == "received" else _tint(c)}
              for t, c in INCOME_TYPE_COLORS.items() for s in ("received", "expected")}
    month_desc = ("The income per month, stacked by type: what was received over the Time range in full "
                  "colour, and what is expected over the Horizon in a lighter one. The current month "
                  "can hold both.")
    if privacy:
        sql = (rows(both, peak) + f"SELECT CAST(month_start AS TIMESTAMP) AS month, {series},\n"
               f"       100 * sum(value) / {PEAK} AS income_pct\n  FROM a\n GROUP BY 1, 2")
        metric, month_desc = "income_pct", month_desc + " As a share of the biggest month."
    else:
        sql = (rows(both) + f"SELECT CAST(month_start AS TIMESTAMP) AS month, {series},\n"
               "       sum(value) AS income\n  FROM a\n GROUP BY 1, 2")
        metric = "income"
    card("Received and expected by month", "bar", month_desc,
         _series_order(sql, "month", "series", metric, "total"),
         {"graph.dimensions": ["month", "series"], "graph.metrics": [metric], **STACKED_VIZ,
          "series_settings": colors})

    # ---- by account, by position, and the calendar
    acc = ("acc AS (\n"
           "  SELECT account_label,\n"
           "         coalesce(sum(value) FILTER (WHERE status = 'received'), 0) AS received,\n"
           "         coalesce(sum(value) FILTER (WHERE status = 'expected'), 0) AS expected,\n"
           "         coalesce(sum(value), 0) AS total\n"
           "    FROM a\n   GROUP BY 1\n  HAVING bool_or(value <> 0))")
    account_desc = ("Each account's income: what it received over the Time range, what it is expected "
                    "to be paid over the Horizon, and the two together, the largest first.")
    if privacy:
        card("Received and expected by account", "table",
             account_desc + " Ranked, with no account named: each figure a share of every account's total.",
             rows(both, acc) + "SELECT row_number() OVER (ORDER BY total DESC) AS rank,\n" +
             ",\n".join(f"       100 * {c} / NULLIF(sum(total) OVER (), 0) AS {c}_pct"
                        for c in ("received", "expected", "total")) +
             "\n  FROM acc\n ORDER BY 1",
             _share_viz("received_pct", "expected_pct", "total_pct"))
    else:
        card("Received and expected by account", "table", account_desc,
             rows(both, acc) + "SELECT account_label AS account, received, expected, total\n"
             "  FROM acc\n ORDER BY total DESC, account", {})

    # Every holding of today, the ones that pay nothing included, and any
    # held no longer that received income over the Time range.
    hold = ("hold AS (\n"
            f"  SELECT {keys}, quantity, price, value\n"
            "    FROM web_yield\n   WHERE today\n     AND currency = {{currency}}" + kept() + ")")
    inc = ("inc AS (\n  SELECT silver_source_id, account_external_id, position_key,\n"
           "         any_value(account_label) AS account_label, any_value(vehicle) AS vehicle,\n"
           "         any_value(symbol) AS symbol, any_value(name) AS name,\n"
           "         sum(value) FILTER (WHERE status = 'received') AS received,\n"
           "         sum(value) FILTER (WHERE status = 'expected') AS expected\n"
           "    FROM a\n   GROUP BY 1, 2, 3)")
    positions = ("r AS (\n"
                 "  SELECT COALESCE(h.vehicle, i.vehicle) AS vehicle,\n"
                 "         COALESCE(h.account_label, i.account_label) AS account_label,\n"
                 "         COALESCE(h.symbol, i.symbol) AS symbol, COALESCE(h.name, i.name) AS name, position_key,\n"
                 "         h.quantity, h.price, h.value,\n"
                 "         coalesce(i.received, 0) AS received, coalesce(i.expected, 0) AS expected\n"
                 "    FROM hold h\n"
                 "    FULL JOIN inc i USING (silver_source_id, account_external_id, position_key))")
    position_order = "\n  FROM r\n ORDER BY vehicle NULLS LAST, received + expected DESC, symbol, name"
    position_desc = ("Each position, grouped by vehicle: what it received over the Time range, what it is "
                     "expected to pay over the Horizon, and the two together. Every holding of today is "
                     "listed, one that pays nothing too, and any holding sold that received income over "
                     "the Time range. A position with no name "
                     + ("reads its key where that is a cash balance's or an account income's, else "
                        "(unnamed)." if privacy else "reads its key.") +
                     " Click its symbol or name to list its payments in the calendar.")
    # A position's income over the value it is earned on: none for a
    # position no longer held, or of no positive value.
    on_value = "NULLIF(GREATEST(value, 0), 0)"
    if privacy:
        per_year = {"received": "(SELECT received FROM mo)", "expected": "(SELECT expected FROM mo)",
                    "total": "(SELECT received + expected FROM mo)"}
        figure = {"received": "received", "expected": "expected", "total": "(received + expected)"}
        card("Received and expected by position", "table",
             position_desc + " Each figure is a yield: per year, over the position's value today, beside "
             "the position's share of the selection's assets; a liability's is negative.",
             rows(both, months, hold, inc, positions) + f"SELECT vehicle, symbol, {TWIN_POSITION_NAME} AS name,\n"
             "       100 * value / NULLIF(sum(GREATEST(value, 0)) OVER (), 0) AS share_pct,\n" +
             ",\n".join(f"       100 * {figure[c]} * 12 / NULLIF({per_year[c]}, 0) / {on_value} AS {c}_pct"
                        for c in figure) + position_order,
             _share_viz("share_pct", "received_pct", "expected_pct", "total_pct"))
    else:
        card("Received and expected by position", "table", position_desc,
             rows(both, hold, inc, positions) +
             "SELECT vehicle, account_label AS account, symbol, COALESCE(name, position_key) AS name,\n"
             "       quantity, price, value, received, expected, received + expected AS total" + position_order, {})

    # Each payment expected over the Horizon, a dated one on its day, an
    # accrual or an estimated day in its month after the month's dated
    # ones, and each principal a maturity returns. The Position picker
    # takes a symbol, a name or a key; on the twin also the name the twin
    # shows, so a click on (unnamed) lists every unnamed position's
    # payments.
    picked = "{{position}} IN (symbol, name, position_key" + (f", {TWIN_POSITION_NAME}" if privacy else "") + ")"
    cal = ("WITH " + expected + ",\ncal AS (\n"
           "  SELECT month_start, day, account_label, symbol, name, type, 'income' AS kind, basis, reinvested,\n"
           "         holding_currency, silver_source_id, account_external_id, position_key,\n"
           "         sum(amount) AS amount, sum(amount * fx) AS value\n"
           "    FROM ex\n   [[WHERE " + picked + "]]"
           "\n   GROUP BY ALL\n  HAVING sum(amount) <> 0\n"
           "  UNION ALL\n"
           "  SELECT CAST(date_trunc('month', day) AS DATE), day, account_label, symbol, name, type, 'principal',\n"
           "         NULL, NULL, holding_currency, silver_source_id, account_external_id, position_key,\n"
           f"         amount, amount * {_ccy_case('fx')}\n"
           "    FROM web_yield_events\n   WHERE rule = {{maturing}}\n"
           "     AND month <= " + HORIZON_MONTHS + "\n     [[AND " + picked + "]]" + kept() + ")\n")
    date = ("CASE WHEN day IS NULL THEN strftime(month_start, '%Y-%m') ELSE strftime(day, '%Y-%m-%d') END"
            " AS date")
    cal_desc = ("Each payment expected over the Horizon, a dated one on its day and an accrual or a payment "
                "of an estimated day in its month, and each principal a maturity returns, in date "
                "order, a holding's own income apart from what its matured principal earns. The "
                "Position picker lists one position's, by its symbol, its name or its position key; "
                "a click on a position's symbol or name in the table above picks it."
                + (" A click on (unnamed) lists every unnamed position's." if privacy else ""))
    if privacy:
        card("Expected payments", "table",
             cal_desc + " Each amount is a percentage of its position's value today.",
             cal + ",\nhold AS (SELECT silver_source_id, account_external_id, position_key, value\n"
             "           FROM web_yield WHERE today AND currency = {{currency}})\n"
             f"SELECT {date}, symbol, {TWIN_POSITION_NAME} AS name, type, kind, basis,\n"
             "       100 * c.value / NULLIF(GREATEST(h.value, 0), 0) AS value_pct\n"
             "  FROM cal c\n  LEFT JOIN hold h USING (silver_source_id, account_external_id, position_key)\n"
             " ORDER BY month_start, day IS NULL, day, symbol, name, kind",
             _share_viz("value_pct"), calendar_tags)
    else:
        card("Expected payments", "table",
             cal_desc + " Its basis says what an amount rests on; under the forward path a floating "
             "amount keeps its rate's, where `wealthdb income calendar` reads forward.",
             cal + f"SELECT {date}, account_label AS account, symbol, COALESCE(name, position_key) AS name,\n"
             "       type, kind, basis, holding_currency AS currency, amount, value\n"
             "  FROM cal\n ORDER BY month_start, day IS NULL, day, account, symbol, name, kind",
             {}, calendar_tags)
    return out


# The cash flow projection's look-back parts (web_cashflow_plan) and the
# order its chart stacks them: what comes in, then what goes out.
PLAN_LOOKBACK_PARTS = ("other_income", "operating_out", "debt_service", "vehicles")
PLAN_INCOME_PARTS = ("investment_income", "other_income", "principal_returned", "vehicles")
PLAN_OUTFLOW_PARTS = ("operating_out", "debt_service")
# Whether a projected part's account is in the cash pool: _pool_join
# finds it in web_cashflow_pool, and _POOL selects the flag.
_POOL = "pl.silver_source_id IS NOT NULL AS pool"


def _pool_join(a):
    return ("\n    LEFT JOIN web_cashflow_pool pl ON pl.silver_source_id = " + a + ".silver_source_id"
            " AND pl.account_external_id = " + a + ".account_external_id")


def cashflow_plan_cards(db_id, privacy):
    """The Cash Flow dashboard's projection cards and the Wealth
    Overview's projected net, or with `privacy` their privacy variants:
    name -> (display, description, dataset_query, viz).
    `wealthdb cashflow projection` month by month over the Horizon
    (docs/CASHFLOW.md §12): the income projection's investment income and
    the principal its maturities return, read from today's projection
    (web_yield_parts, web_yield_rates, web_yield_events, gold migration
    0138) under the four projection pickers as the Income cards read
    them, and split by the cash pool's accounts (web_cashflow_pool): the
    rest is the vehicles' income. They are set against the look-back's
    monthly averages a refresh stores (web_cashflow_plan, migration
    0149). The cash flow projection starts with the month after this
    one, so month 0 never counts.

    The cards take the Currency and the projection's pickers and none of
    the dashboard's own: the look-back is fixed when a refresh writes
    it, so a Time range, Source, Section or Investing picker would move
    nothing.

    A privacy card shows each part as a share of all the income the
    projection expects over the Horizon, on every account; blank where it
    expects none."""
    tags = {"currency": currency_tag(), **PROJECTION_TAGS}
    tail = (" The projected parts follow the projection's pickers. The other "
            "parts are the averages of the look-back a refresh stores, so the Time range, Source, "
            "Section and Investing pickers do not reach this card. Built for the Cash Flow dashboard; "
            f"opened standalone it runs in {DEFAULT_CURRENCY} over the variables' defaults: the "
            f"{HORIZONS[0]} months after this one, {PROJECTION_DEFAULTS}." + (PRIVACY_DESC if privacy else ""))
    out = {}
    card = _card_maker(out, db_id, privacy, CASHFLOW_PLAN_PICKERS, tags, tail)

    # Each month of the horizon: the projection's income and principal,
    # on the pool's accounts and on the others, and the look-back's
    # parts, every month of which the view holds. The principal counts
    # towards the net unless it rolls, buying its successor; the
    # vehicles' income never does.
    lookback = ",\n".join(f"         coalesce(sum(value) FILTER (WHERE part = '{x}'), 0) AS {x}"
                          for x in PLAN_LOOKBACK_PARTS)
    plan = ("WITH ex AS (\n"
            f"  SELECT p.month AS m, {_POOL}, sum(({PART_INCOME}) * {_ccy_case('p.fx')}) AS v\n" + PARTS_FROM +
            _pool_join("p") + PARTS_AHEAD + "\n   GROUP BY 1, 2),\n"
            "pr AS (\n"
            f"  SELECT e.month AS m, {_POOL}, sum(e.amount * {_ccy_case('e.fx')}) AS v\n"
            "    FROM web_yield_events e" + _pool_join("e") + "\n"
            "   WHERE e.rule = {{maturing}}\n     AND e.month" + AFTER_THIS_MONTH + "\n   GROUP BY 1, 2),\n"
            "lb AS (\n  SELECT month AS m, any_value(month_start) AS month_start,\n" + lookback +
            ",\n         any_value(quality) AS quality\n    FROM web_cashflow_plan\n"
            "   WHERE currency = {{currency}}\n     AND month" + AFTER_THIS_MONTH + "\n   GROUP BY 1),\n"
            "j AS (\n"
            "  SELECT CAST(lb.month_start AS TIMESTAMP) AS month, coalesce(ex.v, 0) AS investment_income,\n"
            "         coalesce(pr.v, 0) AS principal_returned,\n"
            "         CASE WHEN {{maturing}} = 'roll' THEN 0 ELSE coalesce(pr.v, 0) END AS principal_counted,\n"
            "         coalesce(xv.v, 0) + coalesce(rv.v, 0) AS vehicles_income,\n"
            "         coalesce(ex.v, 0) + coalesce(xv.v, 0) AS expected,\n"
            "         " + ", ".join(f"lb.{x}" for x in PLAN_LOOKBACK_PARTS) + ",\n"
            "         trim('projected ' || lb.quality) AS quality\n"
            "    FROM lb\n"
            "    LEFT JOIN ex ON ex.m = lb.m AND ex.pool\n    LEFT JOIN pr ON pr.m = lb.m AND pr.pool\n"
            "    LEFT JOIN ex xv ON xv.m = lb.m AND NOT xv.pool\n"
            "    LEFT JOIN pr rv ON rv.m = lb.m AND NOT rv.pool),\n"
            "proj AS (\n"
            "  SELECT *, investment_income + principal_counted + other_income - operating_out - debt_service\n"
            "            + vehicles AS net\n    FROM j)")
    # The twin's denominator: all the income the projection expects over
    # the horizon, on every account, where it expects some.
    plan += (",\nd AS (SELECT CASE WHEN sum(expected) > 0 THEN sum(expected) END AS total\n"
             "        FROM proj)\n" if privacy else "\n")

    def name_of(c):
        return f"{c}_pct" if privacy else c

    def col(expr, name):
        """`expr` as column `name`, on the twin its share of the total."""
        return (f"100 * {expr} / (SELECT total FROM d)" if privacy else expr) + f" AS {name_of(name)}"

    # The chart stacks what comes in above the axis and what goes out
    # below it, the principal where it counts; the net and the vehicles'
    # income, which stays outside the pool, are lines.
    chart_expr = {**{x: x for x in PLAN_INCOME_PARTS}, "principal_returned": "principal_counted",
                  **{x: f"-{x}" for x in PLAN_OUTFLOW_PARTS}, "net": "net", "vehicles_income": "vehicles_income"}
    chart_cols = [*PLAN_INCOME_PARTS, *PLAN_OUTFLOW_PARTS, "net", "vehicles_income"]
    share = (" Each figure is a share of all the income the holdings are expected to pay over the "
             "Horizon, on every account; blank where none is expected." if privacy else "")
    card("Cash flow projection by month", "combo",
         "The cash flow projection per month over the Horizon, as `wealthdb cashflow projection` reads "
         "it: what the holdings are expected to pay, set against the average outflows of the past "
         "months. Above the axis what comes into the cash pool: the investment income the income "
         "projection expects on the pool's accounts, the other income, the principal maturities return "
         "there where it lands in cash, and the vehicles where they pay out. Below it what goes out: "
         "the operating outflows and the debt service. The net is a line: what the month adds to the "
         "cash pool. The vehicles' income is the other line: the income and principal the income "
         "projection expects on the accounts outside the pool. It stays there." + share,
         plan + "SELECT month,\n       " + ",\n       ".join(col(chart_expr[c], c) for c in chart_cols) +
         "\n  FROM proj\n ORDER BY month",
         {"graph.dimensions": ["month"], "graph.metrics": [name_of(c) for c in chart_cols], **STACKED_VIZ,
          "series_settings": {name_of(c): {"display": "line" if c in ("net", "vehicles_income") else "bar"}
                              for c in chart_cols},
          **(_share_viz(*(name_of(c) for c in chart_cols)) if privacy else {})})
    table_cols = ["investment_income", "principal_returned", "vehicles_income", *PLAN_LOOKBACK_PARTS, "net"]
    card("Cash flow projection", "table",
         "The cash flow projection as a table, one row per month of the Horizon: each part, the net "
         "and what the figures rest on. The investment income and the principal returned are the "
         "pool's; the vehicles' income is the income projection's income and principal on the other "
         "accounts, which the net leaves out. The operating outflows and the debt service are "
         "positive, the vehicles signed. The net counts the principal returned unless the Maturing "
         "rule is roll, under which it buys its successor. The quality lists projected, and trailing, "
         "terms or thin where they apply." + share,
         plan + "SELECT month,\n       " + ",\n       ".join(col(c, c) for c in table_cols) +
         ",\n       quality\n  FROM proj\n ORDER BY month",
         _share_viz(*(name_of(c) for c in table_cols)) if privacy else {})
    # The Wealth Overview's figure of the projection: the net over the
    # Horizon, which no picker of that dashboard sets.
    card("Projected net cash flow", "scalar",
         "What the months of the Horizon add to the cash pool, summed: the net of the cash flow "
         "projection, as the Cash Flow dashboard's projection cards show it month by month." + share,
         plan + "SELECT " + col("sum(net)", "net") + "\n  FROM proj",
         _share_viz(name_of("net")) if privacy else {},
         tail=(" Built for the Wealth Overview, which runs it in its Currency at the projection's "
               f"defaults: the {HORIZONS[0]} months after this one, {PROJECTION_DEFAULTS}."
               + (PRIVACY_DESC if privacy else "")))
    return out


def view_tags(table, spec):
    """Field-filter template tags for a native card over serving view
    `table`. spec maps tag name -> (column, widget-type). A tag whose
    field id is missing is omitted, and the SQL builders then drop its
    [[AND {{tag}}]] clause (referencing an undefined tag would invalidate
    the query) and register_native_targets its picker mapping. That is a
    guard only: ensure_synced aborts the run before a card is built with
    an id missing."""
    tags = {}
    for name, (col, widget) in spec.items():
        fid = FIELD_IDS.get((table, col))
        if fid is None:
            continue
        tags[name] = {"id": f"{table}.{name}", "name": name,
                      "display-name": name.replace("_", " ").title(),
                      "type": "dimension", "dimension": ["field", fid, None],
                      "widget-type": widget, "default": None}
    return tags


def _cl(tags, name):
    """The optional filter clause for tag `name`; empty when the tag is
    absent (see view_tags)."""
    return "\n     [[AND {{" + name + "}}]]" if name in tags else ""


def range_filters(col):
    """A time range on `col` plus the source filter: the pair every
    'range' dashboard's native cards carry, before any filter of their
    own."""
    return {"time_range": (col, "date/all-options"),
            "source": ("silver_source_id", "string/=")}


# ---- spending: the shared pieces of the money and privacy cards -------

# The field filters a native spending card may carry, and the pickers
# they answer to. web_spending is the transaction grain (a category
# filter applies); web_card_balances_history is the per-account daily
# carry-forward of card balances, which has no category dimension. The
# privacy variants below are what the twin's cards actually take.
SPEND_FILTERS = {**range_filters("occurred_at"),
                 "account": ("account_label", "string/="),
                 "category": ("spend_primary_label", "string/=")}
CARD_BALANCE_FILTERS = {**range_filters("as_of_day"),
                        "account": ("account_label", "string/=")}
# The same specs for the privacy twin, WITHOUT the account filter. A
# field filter's widget is a dropdown of the values its column takes, so
# an account filter on a privacy card offers account labels — which is
# the one thing the twin exists not to show. It is dropped rather than
# rebound: no column identifies an account without naming it, so a
# picker over some other column would be a different filter wearing the
# Account name (see dashboard_parameters, which drops the twin's Account
# picker for the same reason).
PRIVACY_SPEND_FILTERS = {k: v for k, v in SPEND_FILTERS.items()
                         if k != "account"}
PRIVACY_CARD_BALANCE_FILTERS = {k: v for k, v in CARD_BALANCE_FILTERS.items()
                                if k != "account"}
# The income dashboards' own picker ids. Distinct from the spending
# ones because a Metabase parameter id is per-dashboard-parameter, and
# the two dashboards carry different filter sets over different views.
INCOME_CURRENCY_PARAM_ID = "aa5df10a"
INCOME_ACCOUNT_PARAM_ID = "aa5df10b"
INCOME_TYPE_PARAM_ID = "aa5df10c"

# The Income filters. `type` binds to income_label rather than to the
# primary label its spending twin uses: the income taxonomy has ONE
# vendored primary, so a primary-level picker would collapse every
# earned type behind a single "Income" value and offer the deltas
# beside it — hiding every distinction a reader opens the dashboard
# for.
INCOME_FILTERS = {**range_filters("occurred_at"),
                  "account": ("account_label", "string/="),
                  "type": ("income_label", "string/=")}
# The twin's, without the account filter, for the reason the spending
# twin drops its own: a field filter renders as a dropdown of its
# column's values, and account labels are the one thing the twin exists
# not to show.
PRIVACY_INCOME_FILTERS = {k: v for k, v in INCOME_FILTERS.items()
                          if k != "account"}
INCOME_PICKERS = [(INCOME_CURRENCY_PARAM_ID, "currency"),
                  (TIME_PARAM_ID, "time_range"), (SOURCE_PARAM_ID, "source"),
                  (INCOME_ACCOUNT_PARAM_ID, "account"),
                  (INCOME_TYPE_PARAM_ID, "type")]

# The Cash Flow dashboards' own picker ids.
CASHFLOW_CURRENCY_PARAM_ID = "aa5df10d"
CASHFLOW_INVESTING_PARAM_ID = "aa5df10e"
CASHFLOW_SECTION_PARAM_ID = "aa5df10f"

# The Cash Flow filters, and what is NOT among them.
#
# There is no ACCOUNT filter and no level filter, and both absences are
# the design rather than an omission. The household boundary is what
# separates the household's cash flow from its vehicles', and a picker
# that moved accounts in and out of the pool would turn every crossing
# it split into an unexplained disappearance — a wire between two of the
# household's own accounts is invisible only while both are in the pool.
# A level picker would say the same thing the Investing one says, less
# clearly.
#
# `section` is the one field filter of its own: the statement's sections
# are a short, stable vocabulary, and narrowing to one is how a reader
# asks "what did investing do" without leaving the dashboard. It is NOT
# a picker the design asked for (docs/CASHFLOW.md §9), and it reaches
# only the cards it means something on — the by-month charts and the
# line list. `_cashflow_kit` draws that line, by withholding the tag
# from the cards that decline it.
CASHFLOW_FILTERS = {**range_filters("occurred_at"),
                    "section": ("section", "string/=")}
# The twin's filters are the same: none of the three renders a dropdown
# of anything that identifies an account, which is the reason the other
# two twins drop theirs.
PRIVACY_CASHFLOW_FILTERS = dict(CASHFLOW_FILTERS)
CASHFLOW_PICKERS = [(CASHFLOW_CURRENCY_PARAM_ID, "currency"),
                    (CASHFLOW_INVESTING_PARAM_ID, "investing"),
                    (TIME_PARAM_ID, "time_range"), (SOURCE_PARAM_ID, "source"),
                    (CASHFLOW_SECTION_PARAM_ID, "section")]

# The Gains dashboard's own picker ids.
GAINS_CURRENCY_PARAM_ID = "aa5df111"
GAINS_TAX_WRAPPER_PARAM_ID = "aa5df112"
GAINS_MISSING_PARAM_ID = "aa5df113"

# The Gains filters. The tax wrapper is the one of its own: a realized
# gain in a taxable account is a tax bill, in a retirement account it is
# not. No account filter: the tables name the accounts already.
GAINS_FILTERS = {**range_filters("period"),
                 "tax_wrapper": ("tax_wrapper", "string/=")}
GAINS_PICKERS = [(GAINS_CURRENCY_PARAM_ID, "currency"),
                 (TIME_PARAM_ID, "time_range"), (SOURCE_PARAM_ID, "source"),
                 (GAINS_TAX_WRAPPER_PARAM_ID, "tax_wrapper"),
                 (GAINS_MISSING_PARAM_ID, "missing_basis")]

# The Exposure dashboard's picker ids and filters: the currency (a row
# filter, as on Gains), the time range over the month ends, the source,
# the tax wrapper and the asset class of each leaf.
EXPOSURE_CURRENCY_PARAM_ID = "aa5df114"
EXPOSURE_TAX_WRAPPER_PARAM_ID = "aa5df115"
EXPOSURE_ASSET_CLASS_PARAM_ID = "aa5df116"
EXPOSURE_FILTERS = {**range_filters("as_of_day"), "tax_wrapper": ("tax_wrapper", "string/="),
                    "asset_class": ("asset_class", "string/=")}
EXPOSURE_PICKERS = [(EXPOSURE_CURRENCY_PARAM_ID, "currency"), (TIME_PARAM_ID, "time_range"),
                    (SOURCE_PARAM_ID, "source"), (EXPOSURE_TAX_WRAPPER_PARAM_ID, "tax_wrapper"),
                    (EXPOSURE_ASSET_CLASS_PARAM_ID, "asset_class")]
# Its debt cards (migration 0138). The debt view carries no asset class,
# so the class picker does not reach those cards.
EXPOSURE_DEBT_FILTERS = {k: v for k, v in EXPOSURE_FILTERS.items() if k != "asset_class"}

SPEND_PICKERS = [(SPEND_CURRENCY_PARAM_ID, "currency"),
                 (TIME_PARAM_ID, "time_range"), (SOURCE_PARAM_ID, "source"),
                 (ACCOUNT_PARAM_ID, "account"), (CATEGORY_PARAM_ID, "category")]
CARD_BALANCE_PICKERS = [t for t in SPEND_PICKERS if t[1] != "category"]

# The filters of the Wealth Overview's own tiles and of Allocation's (the
# Overview's pair is range_filters). Neither dashboard names an account
# anywhere, so the base and the twin share them. Every such tile reads a
# serving view natively and takes the Currency picker as the
# {{currency}} variable: the views carry the reporting currencies as
# columns, and a picker selects rows but never a column.
#
# Both narrow to a tax wrapper and an asset class: every holdings view
# carries each line's account wrapper and class, a cash balance being the
# class `cash` (gold migration 0132).
SELECTION_FILTERS = {"tax_wrapper": ("tax_wrapper", "string/="),
                     "asset_class": ("asset_class", "string/=")}
ASOF_FILTERS = {"as_of_day": ("as_of_day", "date/single"),
                "source": ("silver_source_id", "string/="), **SELECTION_FILTERS}
POSITION_FILTERS = {**ASOF_FILTERS, "vehicle": ("vehicle", "string/=")}
# The Top 100 positions cards show a stock's size beside it (gold
# migration 0154), joined after the ranking on the position's label, so
# the list and its values stay those of web_positions_history alone.
POSITION_SIZE_JOIN = ("\n  LEFT JOIN web_position_sizes z ON z.symbol IS NOT DISTINCT FROM p.symbol"
                      "\n       AND z.name IS NOT DISTINCT FROM p.name"
                      "\n       AND z.asset_class IS NOT DISTINCT FROM p.asset_class"
                      "\n       AND z.vehicle IS NOT DISTINCT FROM p.vehicle")
POSITION_SIZE_NOTE = (" Size is a stock's market-cap size (large, mid, small), as the "
                      "Exposure dashboard sizes it; blank for a fund or any other vehicle.")


def holdings_filters(col):
    """A holdings tile's filters on the Wealth Overview: the range pair on
    `col`, the tax wrapper and the asset class."""
    return {**range_filters(col), **SELECTION_FILTERS}


OVERVIEW_PICKERS = [(WEALTH_CURRENCY_PARAM_ID, "currency"),
                    (TIME_PARAM_ID, "time_range"), (SOURCE_PARAM_ID, "source"),
                    (WEALTH_TAX_WRAPPER_PARAM_ID, "tax_wrapper"), (ASSET_PARAM_ID, "asset_class")]
ALLOCATION_PICKERS = [(WEALTH_CURRENCY_PARAM_ID, "currency"),
                      (ASOF_PARAM_ID, "as_of_day"), (SOURCE_PARAM_ID, "source"),
                      (WEALTH_TAX_WRAPPER_PARAM_ID, "tax_wrapper"), (ASSET_PARAM_ID, "asset_class")]
POSITION_PICKERS = ALLOCATION_PICKERS + [(VEHICLE_PARAM_ID, "vehicle")]

# The projection's assumptions (docs/INCOME.md §12), each list's default
# first: the rate path (yield.Paths), what a maturing principal earns
# (yield.Rules), and the horizon in months, up to the 36 the projection
# covers. The pickers and the template variables read them, and a test
# holds them to the engine's.
RATE_PATHS = ("constant", "forward")
MATURING_RULES = ("cash", "idle", "roll")
HORIZONS = ("12", "24", "36")


def _text_tag(name, label, default):
    """A required text template variable with a default, so a card still
    runs opened on its own."""
    return {"id": f"{name}-tag", "name": name, "display-name": label,
            "type": "text", "default": default, "required": True}


# The four variables every card with an expected figure reads. The shift
# is a number of basis points, below zero to lower the rates. The horizon
# is text, read as a number in the SQL: its picker offers the three
# horizons as a list, which Metabase draws for a text picker.
PROJECTION_TAGS = {"rates": _text_tag("rates", "Rate path", RATE_PATHS[0]),
                   "maturing": _text_tag("maturing", "Maturing", MATURING_RULES[0]),
                   "shift": {"id": "shift-tag", "name": "shift", "display-name": "Shift (bp)",
                             "type": "number", "default": "0", "required": True},
                   "horizon": _text_tag("horizon", "Horizon (months)", HORIZONS[0])}
# The projection pickers' defaults in words: the first rate path, the
# first Maturing rule and a zero shift.
PROJECTION_DEFAULTS = f"under {RATE_PATHS[0]} rates, maturing principal to {MATURING_RULES[0]} and no shift"
# Optional: the calendar lists every position's payments until one is
# picked, by its symbol, its name or its position key.
POSITION_TAG = {"id": "position-tag", "name": "position", "display-name": "Position",
                "type": "text", "required": False}
PROJECTION_PICKERS = [(RATES_PARAM_ID, "rates"), (MATURING_PARAM_ID, "maturing"),
                      (SHIFT_PARAM_ID, "shift"), (HORIZON_PARAM_ID, "horizon")]
# The Cash Flow dashboard's projection cards take its currency and the
# projection's four, and none of its other pickers (cashflow_plan_cards).
CASHFLOW_PLAN_PICKERS = [(CASHFLOW_CURRENCY_PARAM_ID, "currency"), *PROJECTION_PICKERS]

# The Yield dashboards' filters over web_yield and web_yield_curve: the
# Exposure set, the window's last day being the day a card reads.
YIELD_FILTERS = EXPOSURE_FILTERS
YIELD_PICKERS = [(YIELD_CURRENCY_PARAM_ID, "currency"), (TIME_PARAM_ID, "time_range"),
                 (SOURCE_PARAM_ID, "source"), (YIELD_TAX_WRAPPER_PARAM_ID, "tax_wrapper"),
                 (YIELD_ASSET_CLASS_PARAM_ID, "asset_class"), *PROJECTION_PICKERS]

# The Income dashboards' cards of the holdings' income read several
# views each, the income received, the tax withheld and the parts
# expected, and a picker reaches one template tag of a card. So each of
# its field filters is bound to the view whose values its picker's
# dropdown lists, and every view the card reads keeps the rows whose
# source or account that view's selected rows carry (`kept` in
# income_projected_cards). A source with no income line is still a
# source, so the source's view is the sources', not the income's. The
# Time range reaches them the same way, through the days the income
# received covers.
INCOME_PROJECTED_SELECTION = {"source": ("web_sources_latest", "silver_source_id"),
                              "account": ("web_income", "account_label")}
INCOME_PROJECTED_PICKERS = [(INCOME_CURRENCY_PARAM_ID, "currency"),
                            (TIME_PARAM_ID, "time_range"), *PROJECTION_PICKERS,
                            (SOURCE_PARAM_ID, "source"), (INCOME_ACCOUNT_PARAM_ID, "account"),
                            (POSITION_PARAM_ID, "position")]


def native_tags(table, spec):
    """Template tags for a native money card over serving view `table`:
    the required {{currency}} text variable, plus a field filter per
    column in `spec` (view_tags)."""
    return {"currency": currency_tag(), **view_tags(table, spec)}


def _cashflow_nodes(where, sum_expr):
    """The node grain the Cash Flow tiles group by, with the Investing
    picker applied. `sum_expr` is an AGGREGATE over the picked currency
    column — the CTE groups, so a bare column would not bind.

    `--investing whole`, the default, nets the section into one
    Investments node and folds its leaves with it: a class on the other
    side cannot be drawn as a child of a node on this one, so "as a
    whole" means as a whole. `class` is a no-op on everything else."""
    return ("  SELECT section,\n"
            "         CASE WHEN section = 'investing' AND {{investing}} = 'whole'\n"
            "              THEN 'Investments' ELSE class_node END AS class,\n"
            "         CASE WHEN section = 'investing' AND {{investing}} = 'whole'\n"
            "              THEN 'Investments' ELSE group_node END AS grp,\n"
            f"         {sum_expr} AS v\n"
            "    FROM web_cashflow" + where + "\n   GROUP BY 1, 2, 3")


def _cashflow_atoms_cte(where, sum_expr):
    """The CTE chain every Cash Flow figure that needs a HUB is built
    on: the node grain, its class and leaf nets, which leaves stayed
    with their class, the atomic nodes, and the hub itself.

    Shared so the diagram and the twin's scalars cannot divide by two
    different hubs. They would: the hub is the sum of the positive nets
    AT THE LEVEL DRAWN, and a leaf that nets against its class appears
    at the group level and not at the class level, so a class-level sum
    is a different number. A reader comparing a scalar against the
    diagram beside it would find they did not agree."""
    return (
        "WITH n AS (\n" + _cashflow_nodes(where, sum_expr) + "),\n"
        "cls AS (SELECT section, class, sum(v) AS net FROM n GROUP BY 1, 2\n"
        # The residual, named as the gold macro names it
        # (`cashflow_class_label`): the section is the four others summed
        # and negated, so cash the household kept is cash the pool
        # absorbed — savings, in either direction. `Cash` alone read as
        # physical money, which the diagram already has a node for in
        # the `Cash withdrawal` consumption leaf.
        "        UNION ALL SELECT 'cash', cashflow_class_label('cash'),\n"
        "                          -sum(v) FROM n),\n"
        # A class drawn into a leaf of the same NAME is a self-edge,
        # which a Sankey renders as a node pointing at itself, so it
        # attaches to the hub directly instead. That rule is stated as
        # itself rather than left to a list of sections and class names
        # to imply — the backlog class IS its own leaf, and so is every
        # vehicle class, and both fall out of `class <> grp` without
        # being named. Financing takes a leaf stage, since a mortgage
        # instalment splits into interest and amortisation.
        #
        # Investing stays out deliberately: the Investing picker rather
        # than a level is what opens it.
        "leaf AS (SELECT section, class, grp, sum(v) AS net FROM n\n"
        "          WHERE section IN ('operating_in', 'operating_out', 'financing')\n"
        "            AND class <> grp\n"
        "          GROUP BY 1, 2, 3),\n"
        "att AS (SELECT l.*, c.net AS cnet,\n"
        "               (l.net > 0 AND c.net > 0) OR (l.net < 0 AND c.net < 0) AS stays\n"
        "          FROM leaf l JOIN cls c ON c.section = l.section AND c.class = l.class),\n"
        # A class enters the hub in its own right exactly when it has
        # no leaves to enter through. Keyed on the leaves themselves
        # rather than on a section list, so a class can never enter BOTH
        # here and as the sum of its leaves below, which would draw the
        # same edge twice.
        "atoms AS (SELECT c.class AS node, c.net FROM cls c\n"
        "           WHERE NOT EXISTS (SELECT 1 FROM att a\n"
        "                              WHERE a.section = c.section\n"
        "                                AND a.class = c.class)\n"
        "          UNION ALL SELECT grp, CASE WHEN stays THEN 0 ELSE net END FROM att\n"
        "          UNION ALL SELECT class, sum(CASE WHEN stays THEN net ELSE 0 END)\n"
        "                      FROM att GROUP BY 1),\n"
        "hub AS (SELECT nullif(sum(CASE WHEN net > 0 THEN net ELSE 0 END), 0)"
        " AS total FROM atoms)")


def _savings_rate_sql(where, alias):
    """The Savings rate tile's SQL, shared with its twin, which shows the
    rate as it is. The view's values are already signed — an outflow is
    negative — so operating is the plain SUM over the two halves, not a
    difference between two magnitudes."""
    value = _ccy_case("value")
    return (f"SELECT sum({value})::DOUBLE\n"
            f"       / nullif(sum(CASE WHEN section = 'operating_in'"
            f" THEN {value} END), 0) AS {alias}\n"
            "  FROM web_cashflow" + where +
            "\n   AND section IN ('operating_in', 'operating_out')")


def _yield_share_sql(where, alias):
    """The Yield share tile's SQL, shared with its twin like the savings
    rate's: the yield class over everything that came in."""
    value = _ccy_case("value")
    return (f"SELECT sum(CASE WHEN class = 'yield' THEN {value} END)::DOUBLE\n"
            f"       / nullif(sum({value})::DOUBLE, 0) AS {alias}\n"
            "  FROM web_cashflow" + where +
            "\n   AND section = 'operating_in'")


def _cashflow_sankey_sql(where, sum_expr, share=False):
    """The window's diagram as an edge list, computed where the pickers
    apply.

    A node's SIDE is the sign of its net over the FILTERED window, so a
    pre-netted table would be netted over the wrong window the moment a
    reader moved a picker. The stages, the hub and the swing rule are
    the `report_cashflow_sankey` macro's, in the three columns the BI
    layer's Sankey visualisation reads.

    A class gets a leaf stage when it has something finer to say than
    its own name — the operating sections, and financing, whose
    mortgage instalment splits into interest and amortisation. The
    vehicles and cash have nothing finer and attach to the hub directly;
    investing is held back deliberately, the Investing picker rather
    than a level being what opens it. A leaf whose net runs opposite to its class
    attaches to the hub directly too, and its class then carries only
    the leaves that stayed with it — so every stage conserves flow and
    no node appears twice.

    `share` divides by the hub, which is what the privacy twin draws:
    with the hub at 100 there is nothing left to hide, the nodes being
    vocabulary and never a merchant, a payer, an account or an
    instrument."""
    value = "e.value / (SELECT total FROM hub) * 100" if share else "e.value"
    return (
        _cashflow_atoms_cte(where, sum_expr) + ",\n"
        # `kin` groups a class's leaves together in the emitted edge
        # list: a leaf sorts by ITS CLASS's size first and its own
        # second, so reading the rows one can see which class each leaf
        # belongs to. An atomic node is its own kin, so stages 2 and 3
        # sort by value alone.
        #
        # It does NOT decide the diagram's layout, and it was measured
        # rather than assumed. The renderer seeds each column in the
        # order the rows arrive and then RELAXES, pulling every node
        # toward the weighted mean of its neighbours for a fixed number
        # of iterations; the seed is forgotten. Running the real edge
        # list through that layout with the rows in this order and in
        # plain value order gives the same column, node for node. If the
        # leaf column reads wrongly, the lever is `sankey.node_align`
        # (see the parameter list) and not this ORDER BY.
        "e AS (SELECT CASE WHEN net > 0 THEN 2 ELSE 3 END AS stage,\n"
        "             CASE WHEN net > 0 THEN node ELSE 'Household' END AS source,\n"
        "             CASE WHEN net > 0 THEN 'Household' ELSE node END AS target,\n"
        "             abs(net) AS value, abs(net) AS kin\n"
        "        FROM atoms WHERE net <> 0\n"
        "      UNION ALL\n"
        "      SELECT CASE WHEN cnet > 0 THEN 1 ELSE 4 END,\n"
        "             CASE WHEN cnet > 0 THEN grp ELSE class END,\n"
        "             CASE WHEN cnet > 0 THEN class ELSE grp END,\n"
        "             abs(net), abs(cnet)\n"
        "        FROM att WHERE stays AND net <> 0)\n"
        f"SELECT e.stage, e.source, e.target, {value} AS value\n"
        "  FROM e ORDER BY e.stage, e.kin DESC, e.value DESC")


def _family_native_kit(db_id, view, filters, pickers, neg, note, privacy=False):
    """One family's native-card kit: the WHERE clause its serving
    view's template tags imply, the summed value expression in the
    picked currency, and a builder that registers a card's pickers as it
    defines it. The tags stay inside — every card that needs them goes
    through the builder.

    Shared because the families and their twins differ only in what this
    takes — the view, the filter specs, the picker list, the SIGN and
    the note a card's description ends with — and a second copy of this
    would be those few constants and a drift risk. `neg` flips
    the sum so an outflow, which gold stores negative, reads as a
    positive magnitude; income is already positive and passes False.
    With `privacy` the builder returns a twin card's shape, the card
    type first (privacy_card_defs).
    """
    tags = native_tags(view, filters)
    where = _native_where(tags)
    val = f"sum({_ccy_case('value', neg=neg)})::DOUBLE"

    def native(name, display, desc, sql, viz):
        register_native_targets(name, tags, pickers)
        card = (display, desc + note, _native(db_id, sql, tags), viz)
        return ("question", *card) if privacy else card

    return where, val, native


def _cashflow_kit(db_id, filters, note, privacy=False):
    """The Cash Flow cards' kit: _family_native_kit's over web_cashflow,
    plus the {{investing}} variable and a second WHERE clause without the
    Section filter. Returns (where, where_nosec, val, native).
    `native(..., section=False)` builds a card that declines the Section
    picker; its SQL must use `where_nosec`, since a tag a card does not
    declare cannot appear in its SQL. The dashboard and its twin share
    it, so their tiles answer the same pickers card for card."""
    tags = {**native_tags("web_cashflow", filters), "investing": INVESTING_TAG}
    tags_nosec = {k: v for k, v in tags.items() if k != "section"}
    pickers_nosec = [t for t in CASHFLOW_PICKERS if t[1] != "section"]

    def native(name, display, desc, sql, viz, section=True):
        card_tags = tags if section else tags_nosec
        register_native_targets(name, card_tags,
                                CASHFLOW_PICKERS if section else pickers_nosec)
        card = (display, desc + note, _native(db_id, sql, card_tags), viz)
        return ("question", *card) if privacy else card

    return (_native_where(tags), _native_where(tags_nosec),
            f"sum({_ccy_case('value')})::DOUBLE", native)


def _field_clauses(tags, exclude=()):
    """One optional [[AND {{tag}}]] clause per FIELD FILTER on the card,
    but those named in `exclude`.

    A plain variable — {{currency}}, {{investing}} — picks a column or a
    grain rather than filtering rows, and an [[AND {{x}}]] clause around
    one is not valid SQL. The test is the tag's own type, so a variable
    added to a family later cannot silently produce a broken predicate.
    A filter absent from `tags` (see view_tags) is left out entirely:
    referencing an undefined tag would invalidate the query. Excluding by
    name keeps "every filter but X" in step with a filter added later."""
    return "".join(_cl(tags, n) for n in tags
                   if tags[n].get("type") == "dimension" and n not in exclude)


def _native_where(tags, indent="   ", exclude=()):
    """`WHERE TRUE` plus the card's field-filter clauses (_field_clauses)."""
    return f"\n{indent}WHERE TRUE" + _field_clauses(tags, exclude)


def _breakdown_sql(view, dim, col, tags, share=False):
    """An Allocation breakdown's SQL: `col` summed by `dim` over serving
    view `view`'s rows that the field filters in `tags` keep, largest
    first; with `share` each as % of the sum, the twin's (liability
    buckets read negative, and a zero sum blanks them)."""
    where = _native_where(tags)
    if not share:
        return (f"SELECT {dim}, sum({_ccy_case(col)})::DOUBLE AS value\n"
                f"  FROM {view}" + where + "\n GROUP BY 1\n ORDER BY 2 DESC")
    return ("WITH r AS (\n"
            f"  SELECT {dim}, sum({_ccy_case(col)})::DOUBLE AS v\n"
            f"    FROM {view}" + where + "\n"
            "   GROUP BY 1)\n"
            f"SELECT {dim},\n"
            "       v / (SELECT CASE WHEN sum(v) <> 0 THEN sum(v) END FROM r)"
            " * 100 AS value_pct\n"
            "  FROM r\n ORDER BY 2 DESC")


# Dashboard picker -> template tag wiring for every native card, rebuilt
# whenever the card definitions are built: every entry is keyed by card
# name, and building a card rewrites its entry (question_defs the base
# cards, privacy_card_defs the twins'). ensure_dashboards reads it to map
# those cards' pickers; cards absent here take the default MBQL
# dimension mappings.
NATIVE_PARAM_TARGETS = {}


def register_native_targets(card, tags, pairs):
    """Record card `card`'s picker -> template-tag mapping. `pairs` is
    (parameter id, tag name); a tag absent from `tags` (see view_tags)
    leaves its picker unmapped. A field-filter tag maps as a `dimension`
    target; a plain template variable ({{currency}}, {{start_year}},
    {{investing}}) maps as a `variable` one."""
    NATIVE_PARAM_TARGETS[card] = [
        (pid, ["dimension" if tags[t].get("type") == "dimension" else "variable",
               ["template-tag", t]])
        for pid, t in pairs if t in tags]


# The peak-month denominator of the twins' charts over months: the
# biggest month's POSITIVE sum, so a mixed-sign month's stacked bar
# cannot exceed 100, and blank when no month is positive at all. A card
# names CTE `p` (_peak_cte) and divides by PEAK; PEAK_CTE and PEAK_DIV
# are the two over a CTE `m` of (month, ..., v) rows.
def _peak_cte(rows="m", value="v", month="month"):
    """CTE `p`: the biggest positive sum of `value` over `rows` grouped by
    `month`."""
    return (f"p AS (SELECT max(t) AS peak FROM"
            f" (SELECT sum({value}) FILTER (WHERE {value} > 0) AS t FROM {rows} GROUP BY {month}))\n")


PEAK = "(SELECT CASE WHEN peak > 0 THEN peak END FROM p)"
PEAK_CTE = _peak_cte()
PEAK_DIV = f"v / {PEAK} * 100"


def _month_cte(view, where, value, extra=""):
    """The opening of a twin's `m` CTE: `value` per month (times
    `extra`'s dimension) over serving view `view`, its GROUP BY left to
    the caller."""
    return ("WITH m AS (\n"
            f"  SELECT {OCCURRED_MONTH},\n         {extra}{value} AS v\n"
            f"    FROM {view}" + where + "\n")


def privacy_card_defs(db_id, model_ids):
    """name -> (card type, display, description, dataset_query, viz
    settings) for the privacy variants of every card the base dashboards
    show. Every card here that shares out money is native SQL over the
    gold web_* serving views, reads the dashboard's currency through the
    {{currency}} variable, and recomputes its normalization denominator
    in-query with the same filters applied: the Wealth Overview's
    figures divide by the selected sources' latest total (net worth
    itself always 100), its holdings charts by that total at the
    window's end, the flow charts by their own peak month within the
    selected window and sources (the tallest bar always reads 100). The
    returns twin redacts instead — returns are already scale-free
    ratios. The Spending twin both normalizes and redacts: its cards are
    shares of their own window's total (or of its peak month) and never
    render a merchant or account label — see spending_privacy_defs.
    Exposure's shares are scale-free already, so its twin only drops
    the value columns — see exposure_cards. The Yield twin and the Income
    twin's cards of the holdings' income show rates as they are and
    income as a yield — see yield_cards and income_projected_cards."""
    out = {}

    # -- Wealth Overview scalars: a ratio of sums is scale-free, and both
    # legs see the dashboard's filters, so each reads as a share of the
    # selected sources' latest total. The > 0 guard blanks an empty or
    # under-water selection rather than rendering inf or a sign-flipped
    # share.
    lat_tags = native_tags("web_sources_latest", holdings_filters("snapshot_at"))

    def share(name, num_col, desc):
        out[name] = ("question", "scalar", desc + PRIVACY_DESC,
            _native(db_id,
                f"SELECT sum({_ccy_case(num_col)})::DOUBLE\n"
                f"       / CASE WHEN sum({_ccy_case('total_value')}) > 0\n"
                f"              THEN sum({_ccy_case('total_value')})::DOUBLE END"
                " * 100 AS share_pct\n"
                "  FROM web_sources_latest" + _native_where(lat_tags), lat_tags),
            {})
        register_native_targets(name, lat_tags, OVERVIEW_PICKERS)

    share("Net worth (privacy)", "total_value",
        "Always 100 by construction — the selected sources' latest net "
        "worth as a share of itself, the anchor every other percentage "
        "on this dashboard is relative to.")
    share("Positions value (privacy)", "positions_value",
        "Market value of all positions as % of the selected sources' "
        "latest net worth; sums to 100 with the cash share.")
    share("Cash balance (privacy)", "cash_balance",
        "Cash as % of the selected sources' latest net worth; sums to "
        "100 with the positions share.")

    # -- The holdings time series: % of the total at the END of the
    # selected window — the last charted day, which is today whenever
    # the window is open-ended. The nw CTE re-applies both pickers: the
    # time filter picks the anchor day, the source filter makes a subset
    # rescale to its own anchor total — so the envelope ends at 100 at
    # the window's last day and tops 100 wherever it previously peaked
    # higher. The anchor day is the latest day of the view the Time
    # range keeps, not of the selection; the other pickers then filter
    # that day's rows. The > 0 guard blanks a selection whose anchor
    # total is zero or negative — dividing would render inf/NaN resp.
    # sign-flipped bands.
    sh_tags = native_tags("web_sources_history", holdings_filters("as_of_day"))
    total = _ccy_case("total_value")
    nw_cte = (
        "WITH nw AS (\n"
        f"  SELECT CASE WHEN sum({total}) > 0\n"
        f"              THEN sum({total})::DOUBLE END AS denom\n"
        "    FROM web_sources_history\n"
        "   WHERE as_of_day = (SELECT max(as_of_day) FROM web_sources_history\n"
        "                       WHERE TRUE" + _cl(sh_tags, "time_range") + ")"
        + _field_clauses(sh_tags, exclude=("time_range",)) + ")\n")
    sh_where = _native_where(sh_tags, " ")
    out["Net worth — monthly trend (privacy)"] = ("question", "smartscalar",
        "Average daily net worth per month as % of the selected sources' "
        "net worth at the window's end, with the change vs the month "
        "before." + PRIVACY_DESC,
        _native(db_id, nw_cte +
            "SELECT CAST(date_trunc('month', as_of_day) AS TIMESTAMP) AS month,\n"
            f"       sum({total})::DOUBLE / count(DISTINCT as_of_day)\n"
            "           / (SELECT denom FROM nw) * 100 AS avg_pct\n"
            "  FROM web_sources_history" + sh_where + "\n"
            " GROUP BY 1\n ORDER BY 1", sh_tags),
        {})
    out["Net worth over time (privacy)"] = ("question", "area",
        "Daily net worth as % of the selected sources' total at the "
        "window's end, stacked by source: the envelope ends at 100 and "
        "exceeds 100 wherever earlier net worth topped the window-end "
        "total." + PRIVACY_DESC,
        _native(db_id, _series_order(nw_cte +
            "SELECT as_of_day, silver_source_id,\n"
            f"       sum({total})::DOUBLE / (SELECT denom FROM nw)"
            " * 100 AS total_value_pct\n"
            "  FROM web_sources_history" + sh_where + "\n"
            " GROUP BY 1, 2", "as_of_day", "silver_source_id", "total_value_pct", "latest"), sh_tags),
        {**_series_viz("as_of_day", "silver_source_id", "total_value_pct"), **STACKED_VIZ})
    for name in ("Net worth — monthly trend (privacy)",
                 "Net worth over time (privacy)"):
        register_native_targets(name, sh_tags, OVERVIEW_PICKERS)

    # -- The Allocation breakdowns: each bucket as % of the summed total
    # over the same filtered rows, so the buckets total 100 across the
    # selected sources as of the chosen day (liability buckets read
    # negative; the sum <> 0 guard blanks a degenerate zero day). Built
    # for the Allocation twin, which supplies the required as-of day;
    # run standalone they aggregate across all days, so filter As Of Day
    # to a single day first.
    standalone = allocation_note()

    def breakdown(name, view, dim, col, display, desc, viz=None):
        tags = native_tags(view, ASOF_FILTERS)
        out[name] = ("question", display, desc + standalone + PRIVACY_DESC,
                     _native(db_id, _breakdown_sql(view, dim, col, tags, share=True), tags),
                     viz if viz is not None else
                     {"graph.dimensions": [dim], "graph.metrics": ["value_pct"]})
        register_native_targets(name, tags, ALLOCATION_PICKERS)

    breakdown("Allocation by asset class (privacy)",
              "web_asset_classes_history", "asset_class", "value", "row",
              "Asset-class shares (%) of the selected sources' net worth "
              "as of a day, including a 'cash' class — always sums to 100; "
              "liability classes (e.g. mortgages) read negative, which is "
              "why this is a bar chart and not a pie.")
    breakdown("Allocation by vehicle (privacy)",
              "web_vehicles_history", "vehicle", "value", "row",
              "Vehicle shares (%) of the selected sources' net worth as of "
              "a day, including a 'demand_deposit' vehicle for cash — "
              "always sums to 100. The wrapper-dimension companion to "
              "Allocation by asset class.")
    breakdown("Allocation by currency (privacy)",
              "web_positions_history", "currency", "value", "row",
              "Native-currency shares (%) of the selected sources' "
              "positions value (cash not included) as of a day — the FX "
              "exposure of the invested part; always sums to 100.")
    breakdown("Value by tax wrapper (privacy)",
              "web_accounts_history", "tax_wrapper", "total_value", "pie",
              "Tax-wrapper shares (%) of the selected sources' total "
              "account value (incl. cash) as of a day; always sums to 100.",
              viz={"pie.dimension": "tax_wrapper", "pie.metric": "value_pct"})
    breakdown("Value by management style (privacy)",
              "web_accounts_history", "management_style", "total_value",
              "row",
              "Management-style shares (%) of the selected sources' total "
              "account value (incl. cash) as of a day; always sums to 100.")

    # -- Top positions: each position's share of the selection's TOTAL
    # positions value at the day: the selected sources, tax wrappers and
    # asset classes. The inline vehicle picker narrows the list but not
    # the denominator, so a position's share reads the same however the
    # list is narrowed.
    top_tags = native_tags("web_positions_history", POSITION_FILTERS)
    value = _ccy_case("value")
    out["Top 100 positions (privacy)"] = ("question", "table",
        "The hundred largest positions as of a day, each as % of the "
        "selection's total positions value; the widget's vehicle "
        "filter narrows the list but not the denominator."
        + POSITION_SIZE_NOTE + standalone + PRIVACY_DESC,
        _native(db_id,
            "WITH tot AS (\n"
            f"  SELECT sum({value})::DOUBLE AS t\n"
            "    FROM web_positions_history"
            + _native_where(top_tags, exclude=("vehicle",)) + "),\n"
            "p AS (\n"
            "  SELECT symbol, name, asset_class, vehicle,"
            f" sum({value})::DOUBLE AS v\n"
            "    FROM web_positions_history" + _native_where(top_tags) + "\n"
            "   GROUP BY 1, 2, 3, 4)\n"
            "SELECT p.symbol, p.name, p.asset_class, p.vehicle, z.size,\n"
            "       p.v / (SELECT CASE WHEN t <> 0 THEN t END FROM tot)"
            " * 100 AS value_pct\n"
            "  FROM p" + POSITION_SIZE_JOIN + "\n ORDER BY value_pct DESC\n LIMIT 100", top_tags),
        {})
    register_native_targets("Top 100 positions (privacy)", top_tags,
                            POSITION_PICKERS)

    # -- Data Freshness twin: the freshness table re-run over the _pct
    # sources model. The dashboard is unfiltered by design, so the
    # model's fixed %-of-latest scale IS the selected-sources scale, and
    # the source rows total 100. Own description: the base card's
    # promises a money column, which here holds shares.
    display, _desc, query, viz = source_freshness_card(
        db_id, model_ids["report_sources_latest_pct"])
    out["Source freshness (privacy)"] = ("question", display,
        "Per source: when a run last read it and the days since, its "
        "latest mark and the days it has been quiet, and the share (%) "
        "of the latest total riding on it." + PRIVACY_DESC, query, viz)

    # -- Returns twin: report_returns_redacted drops the money columns,
    # so the by-source table's privacy variant lists its fields without
    # them (a reference to a dropped column would error against it) and
    # gets a description that doesn't promise them.
    out["Returns by source (privacy)"] = ("question", "table",
        "Returns per source over the chosen window: TWR and MWR, "
        "plain and annualized, with the quality flags that explain "
        "every n/a." + RETURNS_PRIVACY_DESC,
        _returns_by_source(db_id, model_ids["report_returns_redacted"], money=False),
        _percent_viz("twr", "twr_annualized", "mwr", "mwr_annualized"))

    out.update(spending_privacy_defs(db_id, model_ids))
    out.update(income_privacy_defs(db_id))
    out.update(cashflow_privacy_defs(db_id))
    for cards in (exposure_cards, issuer_cards, yield_cards, income_projected_cards, cashflow_plan_cards,
                  gains_question_defs):
        out.update({name: ("question", *card) for name, card in cards(db_id, privacy=True).items()})
    return out


def cashflow_privacy_defs(db_id):
    """The Cash Flow twin's cards: the same tiles with the hub at 100.

    NORMALISATION ALONE, almost. The diagram's nodes are vocabulary —
    no node is ever a merchant, a payer, an account or an instrument —
    so with the hub at 100 there is nothing left to hide in it, and the
    twin's Sankey is the same edge list divided through. That is the
    property §5 of docs/CASHFLOW.md exists to guarantee, and the
    assertion in web/test_provision.py is what holds it.

    ONE card does redact, and for the reason every twin card redacts:
    the fix for a leaking column is to drop it. `Largest flows` names
    the account a line moved through and, on an operating line, the
    payer or merchant behind it. The twin's version ranks instead and
    projects neither.

    THE HUB, not a total, is the denominator, and it is recomputed
    in-query so a narrowed selection rescales to itself. It is the sum
    of the positive nets at the level drawn — which equals the sum of
    the negative ones, because the statement sums to zero once cash is a
    node — so it moves with the Investing picker exactly as the base
    dashboard's diagram does."""
    # The Section picker's scope is the base dashboard's, card for card:
    # a twin tile stands in for a base tile and has to answer the same
    # pickers, or a reader comparing the two would find one narrowed and
    # the other not.
    where, where_nosec, val, native = _cashflow_kit(
        db_id, PRIVACY_CASHFLOW_FILTERS, PRIVACY_DESC, privacy=True)
    out = {}

    def cashflow_card(name, display, desc, sql, viz, section=True):
        out[name] = native(name, display, desc, sql, viz, section)

    # The hub, built by the same helper the diagram uses, so a scalar
    # and the diagram beside it cannot divide by two different numbers.
    hub_cte = _cashflow_atoms_cte(where, val) + "\n"
    # A tile that declines the Section picker must divide by a hub that
    # declines it too, or the numerator and the denominator would answer
    # different filters.
    hub_cte_nosec = _cashflow_atoms_cte(where_nosec, val) + "\n"
    # The statement's bars are signed both ways, so a month's size is its
    # gross movement rather than PEAK_CTE's positive sum.
    peak_cte = ("p AS (SELECT max(t) AS peak FROM"
                " (SELECT sum(abs(v)) AS t FROM m GROUP BY month))\n")

    def month_cte(extra="", filt="", sign=""):
        # `sign` is built in rather than applied to the finished SQL: a
        # post-hoc string replacement would depend on this function's own
        # indentation, and a reflow here would silently turn a card's
        # bars negative with every shape test still green.
        v = f"{sign}({val})" if sign else val
        return _month_cte("web_cashflow", where + filt, v, extra)

    # `neg` is what keeps each twin scalar reading the same way round as
    # the base tile it stands in for: gold stores an outflow negative
    # and the base "Operating out" prints it as a positive magnitude, so
    # its share has to be one too.
    for name, section, neg, label in (
            ("Operating in", "operating_in", False, "came in"),
            ("Operating out", "operating_out", True, "went out"),
            ("Net cash flow", None, False, "the pool kept")):
        filt = "" if section is None else f"\n   AND section = '{section}'"
        expr = f"sum({_ccy_case('value', neg=neg)})::DOUBLE"
        cashflow_card(privacy_name(name), "scalar",
            f"What {label} over the window as a share (%) of the hub — "
            "the total the diagram flows through, which is the sum of "
            "the positive nets at the level drawn and moves with the "
            "Investing picker.",
            hub_cte_nosec +
            f"SELECT (SELECT {expr} FROM web_cashflow" + where_nosec + filt + ")\n"
            "       / (SELECT total FROM hub) AS share_pct",
            _percent_viz("share_pct"), section=False)

    # The diagram, with the hub at 100: the same edges divided through.
    cashflow_card(privacy_name("Cash flow"), "sankey",
        "The window's diagram with every edge as a share (%) of the hub. "
        "The nodes are unchanged — they are vocabulary, never a merchant, "
        "a payer, an account or an instrument — so this twin is the base "
        "diagram normalised rather than redacted.",
        _cashflow_sankey_sql(where_nosec, val, share=True),
        {"sankey.source": "source", "sankey.target": "target",
         "sankey.value": "value", "sankey.node_align": "justify"}, section=False)

    cashflow_card(privacy_name("Cash flow statement by month"), "combo",
        "The statement per month, each section as % of the window's "
        "biggest month by gross movement — the peak month's bars sum to "
        "100.",
        _series_order(
            month_cte("CASE WHEN section IN ('operating_in', 'operating_out')\n"
                      "              THEN 'operating' ELSE section END"
                      " AS statement_section,\n         ")
            + "   GROUP BY 1, 2),\n" + peak_cte +
            f"SELECT month, statement_section,\n       {PEAK_DIV} AS share_pct\n"
            "  FROM m", "month", "statement_section", "share_pct", "total"),
        {"graph.dimensions": ["month", "statement_section"],
         "graph.metrics": ["share_pct"], **STACKED_VIZ})

    # The grouping expression rides in the tuple rather than being
    # inferred from the filter text: the investing row is the one that
    # folds its classes under the Investing picker, and a substring test
    # against the filter would break the day another row's filter names
    # a section list containing it.
    investing_grp = ("CASE WHEN {{investing}} = 'whole' THEN 'Investments'\n"
                     "              ELSE class_node END")
    for name, filt, neg, grp, blurb in (
            ("Inflows by class by month", "\n   AND section = 'operating_in'", False,
             "class_node", "What came in each month by class"),
            ("Outflows by class by month", "\n   AND section = 'operating_out'", True,
             "class_node", "What went out each month by class"),
            ("Investing by month", "\n   AND section = 'investing'", False,
             investing_grp, "Investing per month, signed"),
            ("Financing and vehicles by month",
             "\n   AND section IN ('financing', 'vehicles')", False,
             "class_node", "Debt and the earmarked pools per month, signed")):
        sql = (month_cte(f"{grp} AS class,\n         ", filt, sign="-" if neg else "")
               + "   GROUP BY 1, 2),\n" + peak_cte +
               f"SELECT month, class,\n       {PEAK_DIV} AS share_pct\n  FROM m")
        cashflow_card(privacy_name(name), "area" if "class by month" in name else "bar",
            f"{blurb}, each as % of the window's biggest month by gross "
            "movement.",
            _series_order(sql, "month", "class", "share_pct", "total"),
            {"graph.dimensions": ["month", "class"],
             "graph.metrics": ["share_pct"], **STACKED_VIZ})

    # The one card that redacts rather than normalising: the base
    # version names the account a line moved through and the payer or
    # merchant behind it, and the fix for a leaking column is to drop
    # it. Ranked, so the shape of "how concentrated was this window"
    # survives without naming anyone.
    cashflow_card(privacy_name("Largest flows"), "table",
        "The fifty largest single lines of the window, ranked and "
        "unnamed, each as a share (%) of the hub, with the node it "
        "landed on but no name and no account.",
        hub_cte + ", m AS (\n"
        "  SELECT occurred_at, section, class_node AS class,\n"
        "         group_node AS \"group\",\n"
        f"         {_ccy_case('value')} AS v\n"
        "    FROM web_cashflow" + where + ")\n"
        "SELECT row_number() OVER (ORDER BY abs(v) DESC) AS rank,\n"
        "       section, class, \"group\",\n"
        "       v / (SELECT total FROM hub) * 100 AS share_pct\n"
        "  FROM m\n ORDER BY 1\n LIMIT 50",
        {})

    for name, rate_sql in (("Savings rate", _savings_rate_sql),
                           ("Yield share", _yield_share_sql)):
        # A rate is a proportion already: the twin shows it as it is,
        # the way the returns percentages survive their own twin.
        cashflow_card(privacy_name(name), "scalar",
            f"The {name.lower()} as it is: a rate is a proportion, so it needs no "
            "normalising and hides nothing.",
            rate_sql(where_nosec, "share_pct"),
            _percent_viz("share_pct"), section=False)
    return out


def income_privacy_defs(db_id):
    """The Income twin's cards: the same tiles, every figure a share,
    and no card rendering a payer or an account.

    The denominators mirror the spending twin's, and for the same
    reason: the breakdowns and the lists divide by the window's own net
    income (they sum to 100), the trend by the window's peak month (the
    peak reads 100). A window with no positive month, or a total of
    zero, blanks rather than rendering inf or a sign-flipped share.

    REDACTION on top of normalization, as everywhere else in the twin:
    the fix for a leaking column is to drop it. No card here projects
    `payer_name`, `display_name` or `account_external_id` — the payer
    list keeps its shape by RANKING instead, so `payer_name` is a GROUP
    BY key and never a projection. Nor does any card filter on an
    account, a field filter being a dropdown of its column's values
    (PRIVACY_INCOME_FILTERS)."""
    where, val, native = _family_native_kit(
        db_id, "web_income", PRIVACY_INCOME_FILTERS, INCOME_PICKERS, False,
        PRIVACY_DESC, privacy=True)
    out = {}

    def income_card(name, display, desc, sql, viz):
        out[name] = native(name, display, desc, sql, viz)

    total_cte = ("t AS (SELECT nullif(sum(v), 0) AS total FROM m)\n")

    income_card("Income — monthly trend (privacy)", "smartscalar",
        "Net income per month as % of the window's biggest income month — "
        "the latest month with the change vs the one before it. A window "
        "with no positive month shows blank.",
        _month_cte("web_income", where, val) + "   GROUP BY 1),\n" + PEAK_CTE +
        f"SELECT month, {PEAK_DIV} AS income_pct\n  FROM m\n ORDER BY 1", {})
    # The share ring carries no figure in its hole: a total of shares is
    # 100 by construction and says nothing, and the hole is where the
    # base ring puts the money this one exists not to show.
    income_card("Income by type (privacy)", "pie",
        "Share (%) of the window's net income by type. Every type is "
        "drawn.",
        "WITH m AS (\n"
        f"  SELECT income_label AS type,\n         {val} AS v\n"
        "    FROM web_income" + where + "\n   GROUP BY 1),\n" + total_cte +
        "SELECT type, v / (SELECT total FROM t) * 100 AS income_pct\n"
        "  FROM m\n ORDER BY 2 DESC",
        _donut(threshold=0, total=False))
    # Ranked, unnamed: the shape of "how concentrated is this
    # household's income" without naming anyone it comes from.
    income_card("Top 50 payers (privacy)", "table",
        "The fifty payers the most income came from, ranked and unnamed, "
        "each as a share (%) of the window's net income.",
        "WITH m AS (\n"
        f"  SELECT payer_name, {val} AS v\n"
        "    FROM web_income" + where +
        "\n     AND payer_name IS NOT NULL\n   GROUP BY 1),\n" + total_cte +
        "SELECT row_number() OVER (ORDER BY v DESC) AS rank,\n"
        "       v / (SELECT total FROM t) * 100 AS income_pct\n"
        "  FROM m\n ORDER BY 1\n LIMIT 50",
        {})
    # The anchor every other percentage here is relative to. Native
    # like the rest of this twin — there is no income _pct model,
    # because no income tile is MBQL and none needs a drill-through
    # target.
    income_card("Net income (privacy)", "scalar",
        "Always 100 by construction — the window's net income as a share "
        "of itself, the anchor every other percentage on this dashboard "
        "is relative to.",
        "WITH m AS (\n"
        f"  SELECT {val} AS v FROM web_income" + where + ")\n"
        "SELECT v / nullif(v, 0) * 100 AS income_pct FROM m",
        {})
    # The data-quality canary, rebuilt rather than reused. The base
    # card is a share of ROWS, so its FIGURE is already safe for the
    # twin — but it is a native card, and a native card carries its own
    # field-filter template tags wherever it is opened. One of the base
    # card's is an `account` filter, which renders as a dropdown of
    # account labels: the widget this twin exists not to show. So it is
    # built here over PRIVACY_INCOME_FILTERS like every other card on
    # this dashboard, and left out of PRIVACY_EXEMPT_CARDS.
    #
    # It is the one card here that does not read {{currency}} — a share
    # of rows is the same in every currency — so it takes plain
    # view_tags rather than the native_tags the rest share.
    share_tags = view_tags("web_income", PRIVACY_INCOME_FILTERS)
    out["Uncategorized income share (privacy)"] = ("question", "scalar",
        IN_UNCATEGORIZED_DESC + PRIVACY_DESC,
        _native(db_id, IN_UNCATEGORIZED_SQL + _native_where(share_tags),
                share_tags),
        _percent_viz("uncategorized_share"))
    register_native_targets("Uncategorized income share (privacy)", share_tags,
                            [t for t in INCOME_PICKERS if t[1] != "currency"])
    income_card("Largest receipts (privacy)", "table",
        "The fifty largest single income lines of the window, each as a "
        "share (%) of the window's net income, with the type but no payer "
        "and no account.",
        "WITH m AS (\n"
        "  SELECT occurred_at, income_label AS type,\n"
        f"         {_ccy_case('value')} AS v\n"
        "    FROM web_income" + where + "),\n"
        "t AS (SELECT nullif(sum(v), 0) AS total FROM m)\n"
        "SELECT occurred_at, type,\n"
        "       v / (SELECT total FROM t) * 100 AS income_pct\n"
        "  FROM m\n ORDER BY v DESC\n LIMIT 50",
        {})
    return out


def spending_privacy_defs(db_id, model_ids):
    """The Spending twin's cards: same tiles, but every figure is a share
    and no card renders a counterparty.

    Two denominators, both recomputed in-query with the dashboard's
    pickers applied, so a narrowed selection rescales to itself. The
    breakdowns, the merchant and account lists and the largest lines
    divide by the window's own total net spend (they sum to 100); the
    trend and the monthly bands divide by the window's peak month (the
    peak reads 100). The card-balances chart takes the same shape
    with the window's deepest total owed, so its peak reads 100.
    Guards blank a degenerate window (no positive month, or a total of
    zero) rather than render inf / sign-flipped shares.

    REDACTION on top of normalization, the way the Returns twin does it:
    the fix for a leaking column is to drop it, not to disguise it. No
    card here projects `merchant_name`, `display_name` or
    `account_external_id` — the merchant list keeps its shape by ranking
    instead, so `merchant_name` appears only as a GROUP BY key and in
    the ranking's fixed WHERE predicate, never in a projection. Nor does
    any card here FILTER on an account: a
    field filter renders as a dropdown of its column's values, so an
    account filter would print the labels the projections just dropped
    (PRIVACY_SPEND_FILTERS). `merchant_name` is safe as a GROUP BY key
    and as a fixed predicate because neither shows anything; a filter
    widget does."""
    where, val, native = _family_native_kit(
        db_id, "web_spending", PRIVACY_SPEND_FILTERS, SPEND_PICKERS, True,
        PRIVACY_DESC, privacy=True)
    out = {}

    def spend_card(name, display, desc, sql, viz):
        out[name] = native(name, display, desc, sql, viz)

    def month_cte(extra=""):
        return _month_cte("web_spending", where, val, extra)

    spend_card("Spend — monthly trend (privacy)", "smartscalar",
        "Net spend per month as % of the window's biggest spending month — "
        "the latest month with the change vs the one before it. A window "
        "with no positive month shows blank.",
        month_cte() + "   GROUP BY 1),\n" + PEAK_CTE +
        f"SELECT month, {PEAK_DIV} AS spend_pct\n  FROM m\n ORDER BY 1", {})
    spend_card("Spending by month (privacy)", "area",
        "Net spend per month, stacked by primary category, as % of the "
        "window's biggest spending month — the peak reads 100. A "
        "category can dip negative (a month whose refunds beat its "
        "purchases).",
        _series_order(month_cte("spend_primary_label AS category,\n         ")
                 + "   GROUP BY 1, 2),\n" + PEAK_CTE +
                 f"SELECT month, category, {PEAK_DIV} AS spend_pct\n"
                 "  FROM m", "month", "category", "spend_pct", "total"),
        {"graph.dimensions": ["month", "category"],
         "graph.metrics": ["spend_pct"], **STACKED_VIZ})

    # The window-total denominator: every bucket over the summed total of
    # the same filtered rows, so the buckets total 100. Signed, so a
    # net-refunded bucket reads negative — and the <> 0 guard blanks a
    # window whose spend and refunds cancel exactly.
    def total_cte(select, group):
        """The `r` CTE: one row per bucket, with the value the shares
        divide by."""
        return ("WITH r AS (\n"
                f"  SELECT {select}{val} AS v\n"
                "    FROM web_spending" + where
                + f"\n   GROUP BY {group})\n")
    share_of_total = ("v / (SELECT CASE WHEN sum(v) <> 0 THEN sum(v) END"
                      " FROM r) * 100")

    # Rings, like the money tiles they twin — and the shape suits a
    # share even better than an amount, since the slices are already the
    # percentages the ring would compute. Neither ring carries a LIMIT:
    # a limited ring renormalizes onto what it drew and would print
    # shares disagreeing with its own values, where the threshold folds
    # the tail into a wedge and keeps the whole window on the ring.
    #
    # No figure in the hole. The shares are of a SIGNED total, so a
    # category whose refunds beat its purchases carries a negative one
    # and a ring cannot draw it — leaving the hole to sum the rest and
    # read just over 100. The values are right and total exactly 100;
    # it is the hole that cannot represent them, and a hole reading
    # ~100 on a breakdown already expressed as shares says nothing the
    # description does not.
    for name, col, threshold, what in (
            ("Spending by category (privacy)", "spend_primary_label", 0,
             "Primary-category shares (%) of the window's net spend; they "
             "sum to 100. A category whose refunds beat its purchases has a "
             "negative share and no slice, so the drawn ones can run just "
             "over."),
            ("Spending by subcategory (privacy)", "spend_label", 1.5,
             "Detailed-category shares (%) of the window's net spend, the "
             "ones worth a slice — the long tail folds into one wedge, so "
             "the ring is still the whole window. They sum to 100, except "
             "that a category whose refunds beat its purchases has a "
             "negative share and no slice.")):
        spend_card(name, "pie", what,
            total_cte(f"{col} AS category,\n         ", "1") +
            f"SELECT category, {share_of_total} AS spend_pct\n"
            "  FROM r\n ORDER BY 2 DESC",
            _donut(threshold=threshold, total=False))

    # Merchants, ranked and unnamed: merchant_name is a GROUP BY key and
    # a WHERE predicate only, so the counterparty decides the rows
    # without ever reaching a column. The list still answers the
    # question the money tile answers — how concentrated the spending
    # is. As on the money tile, a delta line is not a merchant
    # transaction and is outside the ranking — including a card bill,
    # which names the issuer it was paid to rather than a merchant
    # (migration 0052) — but not outside the denominator: the shares
    # are of the window's whole net spend, the anchor the twin's scalar
    # reads as 100, so the total is summed apart from the ranked rows.
    # A line the merchant store never named carries its own signature
    # (migration 0054), so the ranking covers every non-delta line
    # carrying a signature, at the signature's grain.
    spend_card("Top 50 merchants (privacy)", "table",
        "The fifty merchants with the most net spend, each as % of the "
        "window's net spend — ranked, and unnamed: merchant labels are "
        "redacted, so only the shape of the distribution is shown. Delta "
        "lines (a gift, a bill on a card not itemised, which names its "
        "issuer, cash out of an ATM) and lines nothing has "
        "resolved are outside the ranking, though inside the total the "
        "shares are of. Rank 1's share is how concentrated the window is.",
        "WITH r AS (\n"
        f"  SELECT {val} AS v\n"
        "    FROM web_spending" + where + "\n"
        "     AND merchant_name IS NOT NULL\n"
        "     AND spend_primary_label <> spend_label\n"
        "   GROUP BY merchant_name),\n"
        "t AS (\n"
        f"  SELECT CASE WHEN {val} <> 0 THEN {val} END AS total\n"
        "    FROM web_spending" + where + ")\n"
        "SELECT row_number() OVER (ORDER BY v DESC) AS merchant_rank,\n"
        "       v / (SELECT total FROM t) * 100 AS spend_pct\n"
        "  FROM r\n ORDER BY 1\n LIMIT 50", {})

    # Accounts: the label is dropped, and the rows regroup onto the two
    # dimensions the privacy dashboards already show — the source and the
    # account kind — concatenated into one bar label.
    spend_card("Spend by account (privacy)", "row",
        "Shares (%) of the window's net spend by source and account kind; "
        "sums to 100. Account labels are redacted, so accounts of the same "
        "kind within a source share a bar.",
        total_cte("silver_source_id || ' / '\n             || COALESCE("
                  "account_kind, 'unknown') AS account_group,\n         ", "1") +
        f"SELECT account_group, {share_of_total} AS spend_pct\n"
        "  FROM r\n ORDER BY 2 DESC",
        {"graph.dimensions": ["account_group"],
         "graph.metrics": ["spend_pct"]})

    # Card balances: its own view, its own picker set (no category
    # dimension), and a denominator of its own — the deepest total owed
    # within the window, so the peak reads 100 and a paid-off card
    # returns to 0. Split by source rather than by account. The sign is
    # flipped as on the money tile: what is owed reads positive.
    bal_tags = native_tags("web_card_balances_history",
                          PRIVACY_CARD_BALANCE_FILTERS)
    bal_name = "Card balances over time (privacy)"
    out[bal_name] = ("question", "line",
        "What the cards owed for every day of the window as % of the "
        "window's deepest total owed: the peak reads 100, and a "
        "paid-off card returns to 0. Split by source; account labels are "
        "redacted." + PRIVACY_DESC,
        _native(db_id, _series_order(
            "WITH d AS (\n"
            "  SELECT as_of_day, silver_source_id,\n"
            f"         sum({_ccy_case('balance', neg=True)})::DOUBLE AS v\n"
            "    FROM web_card_balances_history" + _native_where(bal_tags) + "\n"
            "   GROUP BY 1, 2),\n"
            "p AS (SELECT CASE WHEN max(abs(t)) > 0 THEN max(abs(t)) END AS peak\n"
            "        FROM (SELECT sum(v) AS t FROM d GROUP BY as_of_day))\n"
            "SELECT as_of_day, silver_source_id,\n"
            "       v / (SELECT peak FROM p) * 100 AS balance_pct\n"
            "  FROM d", "as_of_day", "silver_source_id", "balance_pct", "latest",
            largest_first=True), bal_tags),
        {**_series_viz("as_of_day", "silver_source_id", "balance_pct"), **LINES_VIZ})
    register_native_targets(bal_name, bal_tags, CARD_BALANCE_PICKERS)

    # The largest lines, with both counterparty columns dropped: what is
    # left is when, from which source, and what it was categorised as —
    # each as a share of the window.
    spend_card("Largest transactions (privacy)", "table",
        "The fifty largest single spending lines, each as % of the "
        "window's net spend, with the merchant and the account label "
        "redacted — date, source and both category levels remain.",
        "WITH r AS (\n"
        "  SELECT occurred_at, silver_source_id, spend_primary_label, spend_label,\n"
        f"         ({_ccy_case('value', neg=True)})::DOUBLE AS v\n"
        "    FROM web_spending" + where + "),\n"
        "t AS (SELECT CASE WHEN sum(v) <> 0 THEN sum(v) END AS total FROM r)\n"
        "SELECT occurred_at, silver_source_id, spend_primary_label, spend_label,\n"
        "       v / (SELECT total FROM t) * 100 AS spend_pct\n"
        "  FROM r\n ORDER BY v DESC\n LIMIT 50", {})

    # The scalar twin: a ratio of sums over the _pct model, so it is
    # scale-free, follows every picker, and its "see these records"
    # drill-through opens a model with no merchant column. Always 100 —
    # the window total the other percentages are relative to.
    net_spend = {"net_spend": ["*", _dec("value"), -1]}
    total = ["sum", ["expression", "net_spend"]]
    out["Net spend (privacy)"] = ("question", "scalar",
        "Always 100 by construction — the window's net spend as a share of "
        "itself, the anchor every other percentage on this dashboard is "
        "relative to." + PRIVACY_DESC,
        _mbql(db_id, model_ids["report_spending_pct"],
              {"expressions": net_spend,
               "aggregation": [["*", ["/", total, total], 100]]}),
        {})
    return out


def dashboard_defs():
    """dashboard name -> (description, filter mode, sibling dashboard
    name, tiles). Every base dashboard but those in NO_PRIVACY_TWIN gets
    a privacy twin: same layout and filters, cards swapped for their
    share-normalized '(privacy)' variants. The sibling name links the
    two views — ensure_dashboards renders it as a switch link at the top
    of each dashboard. A dashboard without a twin has None there."""
    # The twin blurb names each mode's denominator; absolute amounts
    # never show on any of them. Every 'range' dashboard with a twin
    # names its own (own_pdesc).
    pdesc = {
        "returns": (
            "Privacy view: returns are scale-free ratios and show "
            "unchanged; the absolute money columns and the account / "
            "portfolio grains are redacted. "),
        "asof": (
            "Privacy view: every breakdown shows shares (%) of the "
            "selected sources' total as of the chosen day, summing to "
            "100; absolute amounts never show. "),
        None: (
            "Privacy view: values are shares (%) of the latest total "
            "across all sources, not absolute amounts. "),
    }
    # Spending, Income and Cash Flow are 'range' dashboards whose
    # denominators are their own window rather than a holdings total, and
    # the Spending and Income twins also redact; Exposure's shares are of
    # each month end's net worth or of a chart's class and need no
    # normalising, and Yield's are rates. The Wealth Overview shows the
    # others' figures beside its own holdings shares. So each names what
    # it does in a blurb of its own.
    own_pdesc = {"Wealth Overview": (
        "Privacy view: the net worth figures and charts are shares (%) of "
        "the selected sources' total at the window's end; the asset classes "
        "of each day's total. Each figure from another dashboard "
        "is the privacy figure of that dashboard's data: income, spending "
        "and cash flow as shares of their own totals or of their biggest "
        "month, the projected net as a share "
        "of the income expected over the horizon, rates and returns as "
        "they are. The gains are a percent of the cost basis they are gains "
        "on. Absolute amounts never show. "),
        "Spending": (
        "Privacy view: values are shares (%) of the window's own net "
        "spend, or of its biggest month; merchant and account labels are "
        "redacted and absolute amounts never show. "),
        "Income": (
        "Privacy view: the income lines' values are shares (%) of the "
        "window's own net income, or of its biggest month. A figure of the "
        "holdings' income is a share of the income it covers, or of its "
        "biggest month; the income in the window, and each position's, is "
        "a yield on the value it is earned on. Payer and account labels are "
        "redacted and absolute amounts never show. "),
        "Exposure": (
        "Privacy view: every figure is a share (%) of the selection's net "
        "worth, or of the class a chart counts (public equity, the debt, or "
        "the debt and cash), at its month end, which needs no "
        "normalisation; the value columns are dropped and absolute amounts "
        "never show. "),
        "Yield": (
        "Privacy view: rates and shares show as they are; DV01 is a share "
        "of net worth per basis point, and expected income a yield on the "
        "value it is earned on. Account labels are redacted and absolute "
        "amounts never show. "),
        "Cash Flow": (
        "Privacy view: values are shares (%) of the window's own hub — "
        "the total the diagram flows through, which moves with the "
        "Investing picker — or of its biggest month; the cash flow "
        "projection's figures are shares of all the income the holdings are "
        "expected to pay over the Horizon. The diagram's nodes "
        "are vocabulary and never a merchant, a payer, an account or an "
        "instrument, so the twin needs normalisation rather than "
        "redaction; the one card that named an account drops the column. ")}
    out = {}
    for name, (desc, mode, tiles) in base_dashboards().items():
        if name in NO_PRIVACY_TWIN:
            out[name] = (desc, mode, None, tiles)
            continue
        pname = f"{name}{PRIVACY_SUFFIX}"
        out[name] = (desc, mode, pname, tiles)
        placed = TWIN_TILES.get(name, {})
        ptiles = [(c if c in PRIVACY_EXEMPT_CARDS else privacy_name(c),
                   *placed.get(c, (r, col, sx, sy)), t)
                  for c, r, col, sx, sy, t in tiles if placed.get(c, ()) is not None]
        out[pname] = ((own_pdesc[name] if name in own_pdesc else pdesc[mode]) + desc, mode, name, ptiles)
    return out


def dashboard_parameters(model_ids, mode, name=""):
    """The global filters a pre-defined dashboard carries, by mode:
    'range' pairs the source picker with a time range (flows / history
    dashboards), 'asof' pairs it with a single as-of day (point-in-time
    holdings dashboards), 'returns' pairs it with a start-year picker
    (the returns dashboards), None means no filters. Every mode but None
    adds a required currency picker. The source picker draws its
    dropdown values from the sources model. The Exposure, Yield, Gains,
    Spending, Income and Cash Flow dashboards are 'range' with pickers of
    their own, so they are matched by NAME rather than by mode: Exposure
    first (a tax wrapper and an asset class beside the source), then
    Yield, Gains, Cash Flow, Income and Spending, with the Wealth Overview as
    the plain 'range' case, which adds a tax wrapper and an asset class
    as Allocation does. Spending and Income each carry one more picker than
    their twin, which has no account picker; Cash Flow's twin carries
    the same set, having no account picker to drop. Yield is Exposure's
    set plus the projection's four pickers, which Income takes too,
    beside a position of its own, and Cash Flow for its projection cards."""
    if mode is None:
        return []

    def card_picker(pid, label, slug, model, field):
        """A multi-select text picker (no default = all values) whose
        dropdown lists the values `field` takes on pre-defined model
        `model` — the shape every non-currency picker here has."""
        return {"id": pid, "name": label, "slug": slug, "type": "string/=",
                "sectionId": "string", "isMultiSelect": True,
                "values_source_type": "card",
                "values_source_config": {
                    "card_id": model_ids[model],
                    "value_field": ["field", field,
                                    {"base-type": "type/Text"}]}}

    def static_picker(pid, name, slug, default, values):
        """A required single-value picker over a fixed list: the
        product's own vocabulary, not the data's, so a card-backed list
        would re-scan the whole population for a few known strings.
        Required with a default, because a required parameter resets to
        its default rather than clearing. `values_query_type` is what
        makes Metabase render a dropdown instead of a free-text box."""
        return {"id": pid, "name": name, "slug": slug,
                "type": "string/=", "sectionId": "string",
                "isMultiSelect": False, "default": [default],
                "required": True,
                "values_query_type": "list",
                "values_source_type": "static-list",
                "values_source_config": {"values": list(values)}}

    def currency_picker(pid):
        """The required reporting-currency picker every filtered
        dashboard carries, defaulting to the configured currency
        (DEFAULT_CURRENCY). A card running with the currency cleared
        would be wrong: a tile over a model with one row per (line,
        reporting currency) would sum every currency, and a native
        tile's {{currency}} variable needs a value."""
        return static_picker(pid, "Currency", "currency", DEFAULT_CURRENCY, REPORTING_CURRENCIES)

    def tax_wrapper_picker(pid):
        """A tax wrapper picker. Its values come off the accounts model:
        every view reads a line's tax wrapper off its account."""
        return card_picker(pid, "Tax wrapper", "tax_wrapper",
                           "report_accounts_latest", "tax_wrapper")

    def asset_class_picker(pid):
        """An asset class picker. Its values come off the asset class
        history, which reads cash as the class `cash`, the way every
        holdings view does. That taxonomy is also the one every exposure
        leaf's class is one of."""
        return card_picker(pid, "Asset class", "asset_class",
                           "report_asset_classes_history", "asset_class")

    source = card_picker(SOURCE_PARAM_ID, "Source", "source",
                         "report_sources_latest", "silver_source_id")
    if mode == "returns":
        # report_returns carries one row set per currency, so a card
        # running with the currency cleared would show every period once
        # per currency. The values come off the materialized table's own
        # currency column, like the start-year list below.
        currency = {**currency_picker(CURRENCY_PARAM_ID),
                    "values_source_type": "card",
                    "values_source_config": {
                        "card_id": model_ids["report_returns"],
                        "value_field": ["field", "currency",
                                        {"base-type": "type/Text"}]}}
        # Start year: rescopes the summary scalars/table to the since-<year>
        # window (window_from_year) and clips the charts to periods ending
        # on/after that year. 0 = since inception. Required + default 0 so a
        # card never aggregates across windows; values come from the
        # materialized window_from_year set (auto-syncs to the data's span).
        start_year = {"id": START_YEAR_PARAM_ID,
                      "name": "Start year (0 = since inception)",
                      "slug": "start_year", "type": "number/=",
                      "sectionId": "number", "isMultiSelect": False,
                      "default": [0], "required": True,
                      "values_source_type": "card",
                      "values_source_config": {
                          "card_id": model_ids["report_returns"],
                          "value_field": ["field", "window_from_year",
                                          {"base-type": "type/Integer"}]}}
        # The source picker narrows the by-source table and the charts (the
        # global scalars are whole-portfolio, so it doesn't touch them). Not
        # required / no default = all sources; the charts keep the global
        # '(all sources)' reference line regardless.
        return [currency, start_year, source]
    if mode == "asof":
        # Required, with the history's last day as the default
        # (ASOF_DEFAULT): the as-of cards sum daily history (one row per
        # entity per day), so they must never run with the day filter
        # cleared — a required parameter resets to its default instead
        # of clearing. A day the history does not reach reads nothing.
        # The vehicle picker draws its values from the positions model
        # and is linked only to POSITION_FILTERED_CARDS.
        return [currency_picker(WEALTH_CURRENCY_PARAM_ID),
                {"id": ASOF_PARAM_ID, "name": "As of day", "slug": "as_of_day",
                 "type": "date/single", "sectionId": "date",
                 "default": ASOF_DEFAULT, "required": True},
                source, tax_wrapper_picker(WEALTH_TAX_WRAPPER_PARAM_ID),
                asset_class_picker(ASSET_PARAM_ID),
                card_picker(VEHICLE_PARAM_ID, "Vehicle", "vehicle",
                            "report_positions_history", "vehicle")]
    # "past12months~": the trailing ~ means "include this month".
    # Without it Metabase takes the previous 12 COMPLETE months, which
    # silently drops every row stamped in the current partial month —
    # for latest-snapshot cards that nulls out precisely the sources
    # that are freshest (their snapshot_at is this month).
    time_range = {"id": TIME_PARAM_ID, "name": "Time range",
                  "slug": "time_range", "type": "date/all-options",
                  "sectionId": "date", "default": "past12months~"}
    # The projection's four: required, as each card reads one rate path,
    # one maturing rule and one horizon of the parts, with the CLI's
    # defaults. The shift is a number of basis points.
    projection = [static_picker(RATES_PARAM_ID, "Rate path", "rates", RATE_PATHS[0], RATE_PATHS),
                  static_picker(MATURING_PARAM_ID, "Maturing", "maturing", MATURING_RULES[0],
                                MATURING_RULES),
                  {"id": SHIFT_PARAM_ID, "name": "Shift (bp)", "slug": "shift", "type": "number/=",
                   "sectionId": "number", "isMultiSelect": False, "default": [0], "required": True},
                  static_picker(HORIZON_PARAM_ID, "Horizon (months)", "horizon", HORIZONS[0], HORIZONS)]
    if name in EXPOSURE_DASHBOARDS:
        # The currency is required, as report_exposure holds a row set
        # per currency; the source, the tax wrapper and the asset class
        # narrow the look-through to a selection, whose shares the cards
        # compute.
        return [currency_picker(EXPOSURE_CURRENCY_PARAM_ID), time_range, source,
                tax_wrapper_picker(EXPOSURE_TAX_WRAPPER_PARAM_ID),
                asset_class_picker(EXPOSURE_ASSET_CLASS_PARAM_ID)]
    if name in YIELD_DASHBOARDS:
        # Exposure's five, report_yield_positions holding a row set per
        # currency too, and the projection's four.
        return [currency_picker(YIELD_CURRENCY_PARAM_ID), time_range, source,
                tax_wrapper_picker(YIELD_TAX_WRAPPER_PARAM_ID),
                asset_class_picker(YIELD_ASSET_CLASS_PARAM_ID), *projection]
    if name in GAINS_DASHBOARDS:
        # Missing cost basis is required, like the currency:
        # report_gains holds a row set per reading, and a tile with the
        # picker cleared would sum both.
        missing = static_picker(GAINS_MISSING_PARAM_ID, "Missing cost basis", "missing_basis",
                                DEFAULT_MISSING_BASIS, MISSING_BASIS_READINGS)
        return [currency_picker(GAINS_CURRENCY_PARAM_ID), time_range, source,
                tax_wrapper_picker(GAINS_TAX_WRAPPER_PARAM_ID), missing]
    if name in CASHFLOW_DASHBOARDS:
        # The Investing grain: net the section as one movement — the
        # question a reader opens with, "did the portfolio feed the
        # household this year or the household feed the portfolio" — or
        # per asset class, which is the step in. Required with a `whole`
        # default so a card never runs with it cleared, and a static list
        # because the two grains are the feature's, not the data's.
        investing = static_picker(CASHFLOW_INVESTING_PARAM_ID, "Investing", "investing",
                                  "whole", ("whole", "class"))
        # BOTH dashboards carry the same pickers: unlike the spending and
        # income pairs, the twin drops nothing, because none of these
        # pickers renders a dropdown of anything that identifies an
        # account. There is no account picker to drop — docs/CASHFLOW.md
        # §9 — and a section picker offers the feature's own vocabulary,
        # the sections the serving view carries. The Section picker is
        # declared here for both dashboards but BINDS only to the cards
        # that declare its tag; the headline figures and the diagram
        # decline it, identically on the base and the twin. The
        # projection's four follow, the Horizon first as on Income; they
        # bind only to the projection cards (CASHFLOW_PLAN_PICKERS), which
        # take none of the others but the currency.
        rates, maturing, shift, horizon = projection
        return [currency_picker(CASHFLOW_CURRENCY_PARAM_ID), investing,
                time_range, source,
                card_picker(CASHFLOW_SECTION_PARAM_ID, "Section", "section",
                            "report_cashflow", "section"),
                horizon, rates, maturing, shift]
    if name in INCOME_DASHBOARDS:
        # The income pickers, in the shape the spending ones take and
        # for the same reasons — including no account picker on the
        # twin, a picker being unable to redact what its own dropdown
        # offers. The Horizon sits beside the Time range: the one sets
        # every expected figure as the other sets every received one.
        rates, maturing, shift, horizon = projection
        pickers = [currency_picker(INCOME_CURRENCY_PARAM_ID), time_range, horizon, source]
        if not name.endswith(PRIVACY_SUFFIX):
            pickers.append(card_picker(INCOME_ACCOUNT_PARAM_ID, "Account",
                                       "account", "report_income", "display_name"))
        # Bound to the DETAILED label: the income taxonomy has one
        # vendored primary, so a primary-level picker would offer a
        # handful of values and hide every distinction worth filtering by.
        #
        # `income_detailed` is the MODEL'S ALIAS for that label
        # (report_income projects `i.income_label AS income_detailed`),
        # not the view column of the same name. A picker's dropdown is
        # the values its value_field takes on the model, so naming a
        # column the model does not project leaves the dropdown empty —
        # and the tiles then match a filter nobody could set. The
        # matching field filter is bound to the VIEW's income_label
        # (INCOME_FILTERS), so picker and predicate offer the same
        # vocabulary, which is the contract the spending pair states.
        pickers.append(card_picker(INCOME_TYPE_PARAM_ID, "Type", "type",
                                   "report_income", "income_detailed"))
        # The position the calendar lists, a text box the positions
        # table's cells also fill (CROSSFILTER_TILES), then the
        # projection's three assumptions.
        position = {"id": POSITION_PARAM_ID, "name": "Position", "slug": "position",
                    "type": "string/=", "sectionId": "string", "isMultiSelect": False}
        return [*pickers, position, rates, maturing, shift]
    if name not in SPENDING_DASHBOARDS:
        # The Wealth Overview: the tax wrapper and the asset class, as
        # Allocation.
        return [currency_picker(WEALTH_CURRENCY_PARAM_ID), time_range, source,
                tax_wrapper_picker(WEALTH_TAX_WRAPPER_PARAM_ID),
                asset_class_picker(ASSET_PARAM_ID)]
    currency = currency_picker(SPEND_CURRENCY_PARAM_ID)
    # The account picker targets `display_name` — the model's name for
    # the account LABEL (migration 0063) — and NOT
    # `account_external_id`: a picker lists the raw values of the column
    # it is bound to and there is no field-remapping machinery here, so
    # binding it to the id would offer a list of opaque identifiers. The
    # label carries the source and the kind, so two accounts wearing the
    # same generic product name at two institutions are two distinct
    # entries rather than one indistinguishable pair; two named alike
    # WITHIN one source still select together, accepted on the same
    # reasoning, and the money tiles group by the same column.
    #
    # THE PRIVACY TWIN CARRIES NO ACCOUNT PICKER. A picker cannot redact
    # what it offers: its dropdown IS the list of values the bound column
    # takes, and every column that identifies an account is a label — the
    # account label, the display name it is built from (a card's falls
    # back to its masked last four digits), or the external id. Rebinding to a column that identifies no
    # account would make it a different filter wearing the same name, so
    # the twin drops it, the way its cards drop the columns they cannot
    # show (spending_privacy_defs). The Source picker still narrows by
    # institution, and the account breakdown there regroups onto source ×
    # account kind.
    pickers = [currency, time_range, source]
    if not name.endswith(PRIVACY_SUFFIX):
        pickers.append(card_picker(ACCOUNT_PARAM_ID, "Account", "account",
                                   "report_spending", "display_name"))
    pickers.append(card_picker(CATEGORY_PARAM_ID, "Category", "category",
                               "report_spending", "spend_primary"))
    return pickers


def gold_metadata(base, sid, db_id, tries=3, delay=2):
    """The synced gold tables metadata, with a short retry. A transient
    failure here must fail the run loudly rather than resolve zero field
    ids — an empty resolution would silently converge a working install
    down to the degraded no-filters card shape. Returns the tables list,
    or None when the metadata stays unreadable."""
    for _ in range(tries):
        st, meta = req(base, f"/api/database/{db_id}/metadata", session=sid)
        if st == 200 and isinstance(meta, dict):
            return meta.get("tables", [])
        time.sleep(delay)
    return None


def field_ids_from(tables):
    """{(table, column): field id} for every column named in
    FILTER_FIELD_COLUMNS present in `tables` — the ids behind the native
    cards' field filters, which render the dashboard pickers as
    dropdowns / date widgets (a plain template variable would be a
    free-text box instead)."""
    ids = {}
    for t in tables:
        cols = FILTER_FIELD_COLUMNS.get(t.get("name"))
        if not cols:
            continue
        for f in t.get("fields", []):
            if f.get("name") in cols:
                ids[(t["name"], f["name"])] = f.get("id")
    return ids


def list_fields(tables):
    """The id and name of every list column in `tables` that Metabase
    would still fingerprint: a DuckDB list, whose type ends in `[]`. A
    marked field leaves the metadata, so a later run finds nothing to
    mark, and no run here undoes a mark."""
    return [(f["id"], f"{t.get('name')}.{f.get('name')}") for t in tables for f in t.get("fields", [])
            if (f.get("database_type") or "").endswith("[]") and f.get("visibility_type") != "sensitive"]


def unfingerprint_lists(base, sid, tables):
    """Keep Metabase's field sync off the gold's list columns. Its
    fingerprint reads a table in one query that cuts each text column to
    a prefix with substring(). The DuckDB driver reports a list as text,
    and DuckDB has no substring() for a list, so the whole table's
    fingerprint fails, every hour. Metabase never fingerprints a field
    marked `sensitive`, and such a field leaves its table browser and
    query builder. The native cards read the column as before; no model
    or picker binds one. A field that cannot be marked leaves only the
    warning, so it is reported, not fatal."""
    marked = 0
    for fid, name in list_fields(tables):
        st, body = req(base, f"/api/field/{fid}", "PUT", {"visibility_type": "sensitive"}, session=sid)
        if st == 200:
            marked += 1
        else:
            print(f"provision: could not keep {name} from fingerprinting ({st}): {body.get('message')}",
                  file=sys.stderr)
    if marked:
        print(f"provision: {marked} list column(s) kept from fingerprinting")


def missing_filter_columns(tables):
    """The FILTER_FIELD_COLUMNS tables not fully synced in `tables` —
    checked per COLUMN, not per table: Metabase's sync can expose a
    table before its fields, and a provision that ran in that gap would
    wire zero filters."""
    ids = field_ids_from(tables)
    return sorted({t for t, cols in FILTER_FIELD_COLUMNS.items()
                   for c in cols if (t, c) not in ids})


def web_views_wanted():
    """The web_* serving views the cards read, from the one registry
    that names them all."""
    return sorted(t for t in FILTER_FIELD_COLUMNS if t.startswith("web_"))


def probe_rows(base, sid, db_id, sql):
    """The rows native `sql` returns through the driver, or None when the
    probe does not answer."""
    st, res = req(base, "/api/dataset", "POST",
                  {"type": "native", "database": db_id,
                   "native": {"query": sql, "template-tags": {}}}, session=sid)
    return (res.get("data") or {}).get("rows") if st == 202 else None


# The last day of the holdings history: the sources' history carries
# every line, cash included, so it reaches the end where any line does.
LAST_HISTORY_DAY_SQL = "SELECT CAST(CAST(max(as_of_day) AS DATE) AS VARCHAR) FROM web_sources_history"


def last_history_day(base, sid, db_id):
    """The last day the snapshot's holdings history reaches, as
    YYYY-MM-DD: the UTC day the serving tables were last materialized,
    before today when the snapshot was not refreshed today. ASOF_FALLBACK
    where the probe does not answer or the history is empty."""
    rows = probe_rows(base, sid, db_id, LAST_HISTORY_DAY_SQL)
    return rows[0][0] if rows and rows[0] and rows[0][0] else ASOF_FALLBACK


def missing_web_views(base, sid, db_id):
    """The wanted web_* serving views the mounted gold snapshot does not
    carry, probed through the driver itself. A snapshot predating the
    migration that adds one cannot be fixed by a Metabase schema sync —
    only `wealthdb web refresh` re-materializes and re-snapshots gold.

    Probed BY NAME rather than by counting web_* views: a count says
    nothing about WHICH views are there, so a snapshot missing one would
    pass the moment gold grew any other web_* view.
    A probe that fails to answer reports everything missing, which halts
    provisioning — the conservative reading, since the alternative is
    converging every card to a degraded shape."""
    want = web_views_wanted()
    rows = probe_rows(base, sid, db_id,
                      "SELECT view_name FROM duckdb_views() "
                      f"WHERE NOT internal AND view_name IN ({_sql_list(want)})")
    if rows is None:
        return want
    have = {r[0] for r in rows if r}
    return [t for t in want if t not in have]


# The oldest gold schema the cards can read: the migration that adds the
# stock positions' sizes the Top 100 positions cards read (0154). A
# snapshot older than 0154 lacks that view, one older than 0149 the cash
# flow projection's look-back parts, one older than 0146 the days the
# Income cards' received figures cover and the tax withheld on them, and
# one older than 0138 the yield views and the exposure of the debt, which the
# by-name probe finds. One older than 0133 carries every view by name but
# reads a missing tax wrapper blank, and one older than 0132 lacks the
# serving tables' columns: only the version tells those apart.
MIN_GOLD_SCHEMA = 154

# main()'s exit status for a snapshot older than MIN_GOLD_SCHEMA; every
# other failure exits 1. web/web tells the two apart, because only
# `wealthdb web refresh` mends this one and the browser wizard does not.
EXIT_STALE_SNAPSHOT = 3


def gold_schema_version(base, sid, db_id):
    """The mounted gold snapshot's schema version, probed through the
    driver, or None when the probe does not answer."""
    rows = probe_rows(base, sid, db_id, "SELECT max(gold_schema_version) FROM schema_meta")
    try:
        return int(rows[0][0])
    except (TypeError, IndexError, ValueError):
        return None


def check_gold_schema(base, sid, db_id):
    """0 when the mounted gold snapshot is at MIN_GOLD_SCHEMA or newer,
    else the status main() exits with: EXIT_STALE_SNAPSHOT for an older
    snapshot, 1 when the version probe does not answer. Checked before
    anything is synced or converged: an older snapshot carries its views
    by name but not what the cards read from them, so every card would
    converge onto SQL that fails or reads them wrong."""
    version = gold_schema_version(base, sid, db_id)
    if version is None:
        print("provision: cannot read the gold snapshot's schema version; "
              "leaving the existing cards untouched", file=sys.stderr)
        return 1
    if version < MIN_GOLD_SCHEMA:
        print(f"provision: the gold snapshot is at schema {version} and the "
              f"cards need {MIN_GOLD_SCHEMA} — run `wealthdb web refresh` to "
              "re-snapshot a migrated gold; leaving the existing cards "
              "untouched", file=sys.stderr)
        return EXIT_STALE_SNAPSHOT
    return 0


def withholding_table(base, sid, db_id):
    """Whether the mounted gold snapshot holds a withholding table with
    any rate in it (yield_withholding, the config's yield.withholding),
    probed through the driver; None when the probe does not answer."""
    rows = probe_rows(base, sid, db_id, "SELECT count(*) > 0 FROM yield_withholding")
    try:
        return bool(rows[0][0])
    except (TypeError, IndexError):
        return None


# The Withholding tiles' titles where the snapshot holds no withholding
# table: the tiles then read the tax booked on the income's accounts
# alone, and nothing for the income expected.
NO_WITHHOLDING_TABLE_TITLES = {"Withholding": "Received withholding — no withholding table",
                               "Income after withholding": "Income after received withholding"}


def tile_titles(table):
    """card name -> the title its tiles show in place of the card's name,
    on the base dashboards and the twins: the Withholding tiles' where
    `table` is False, the snapshot holding no withholding table. A probe
    that did not answer, None, keeps the cards' names."""
    if table is not False:
        return {}
    titles = dict(NO_WITHHOLDING_TABLE_TITLES)
    titles.update({privacy_name(c): privacy_name(t) for c, t in NO_WITHHOLDING_TABLE_TITLES.items()})
    return titles


def ensure_synced(base, sid, db_id, tries=20, delay=3):
    """Make every column behind FILTER_FIELD_COLUMNS available: when
    some are missing from the synced metadata, either Metabase simply
    has not synced the new gold DDL yet (trigger a sync, wait bounded)
    or the snapshot predates a serving view the cards read — which no
    sync can fix. Returns the tables metadata to resolve field ids from,
    or None when provisioning must not proceed: metadata unreadable, a
    stale snapshot that would break the view-backed models, or a sync
    that was still incomplete when the budget ran out. All three end the
    same way, because a partial answer is what a filter-less card is
    built from. main() has already refused a snapshot older than
    MIN_GOLD_SCHEMA (check_gold_schema)."""
    tables = gold_metadata(base, sid, db_id)
    if tables is None:
        print("provision: cannot read the gold metadata — aborting before "
              "converging cards to a degraded shape", file=sys.stderr)
        return None
    gone = missing_filter_columns(tables)
    if not gone:
        # The columns are already there. A sync would not change them.
        print("provision: schema sync skipped")
        return tables
    absent = missing_web_views(base, sid, db_id)
    if absent:
        print("provision: the gold snapshot is missing serving views the "
              f"cards read ({', '.join(absent)}) — run `wealthdb web "
              "refresh` to re-snapshot a migrated gold; leaving the "
              "existing cards untouched", file=sys.stderr)
        return None
    print(f"provision: syncing gold schema (missing: {', '.join(gone)})")
    req(base, f"/api/database/{db_id}/sync_schema", "POST", {}, session=sid)
    for _ in range(tries):
        time.sleep(delay)
        latest = gold_metadata(base, sid, db_id)
        # A read that failed says nothing about the sync; only overwrite on a
        # successful one, so `tables` is always the newest metadata actually
        # read — at worst the pre-sync one. Coercing a failed last read to []
        # would resolve no field ids at all and converge every native card to
        # the degraded no-filters shape, which is what the guard above aborts
        # to prevent.
        if latest is None:
            continue
        tables = latest
        if not missing_filter_columns(tables):
            return tables
    # The budget ran out with columns still missing. Proceeding would resolve
    # no field id for them, `view_tags` would drop each one's tag and
    # `_native_where` the `[[AND {{tag}}]]` clause that reads it — and
    # `upsert_card` would then PUT that filter-less definition over a card
    # that already works. That is the same degradation the two guards above
    # abort to prevent, arrived at slowly; a first install reaches it too,
    # creating every card filter-less with nothing to say so.
    print("provision: gold schema sync did not complete in time (still "
          f"missing: {', '.join(missing_filter_columns(tables))}) — leaving "
          "the existing cards untouched; re-run `wealthdb web start` once "
          "Metabase has finished syncing, or `wealthdb web refresh` to "
          "re-snapshot a migrated gold", file=sys.stderr)
    return None


def ensure_database(base, sid, db_name, gold_path):
    """Add the gold DuckDB connection, or converge an existing one's
    settings. Returns the database id, or None on failure.

    DuckDB runs in-process in the Metabase JVM; without a cap it helps
    itself to 80% of the machine's RAM and the kernel OOM-kills the JVM
    when several history-heavy dashboard tiles query concurrently. Both
    keys land as instance-level DuckDB config (the driver forwards
    unknown detail keys as JDBC properties); threads is capped because
    peak memory scales with per-query parallelism.

    The cap is INSTANCE-level, so every tile of a dashboard shares it,
    and a dashboard opens by firing all of its tiles at once — so it is
    sized against the busiest page rather than against one query. Each
    tile reads a serving table (gold migration 0132), not a report
    macro, so a tile's memory grows with the rows it keeps, not with the
    length of the history. Four GB leaves a page room to gain tiles; a
    pool too small surfaces as "There was a problem displaying this
    chart" on most of the page rather than as anything naming memory.

    Lowering `threads` instead does NOT trade off the same way — it
    makes matters worse, because a query that cannot parallelise holds
    its intermediates longer. Spill goes to the driver's hard-wired
    "<database_file>.tmp", which web/web mounts writable. Do NOT move
    these into init_sql: that runs per connection, and DuckDB refuses to
    re-SET a used temp_directory, which breaks every query after the
    first connection cycle ("" converges the key away from older
    provisions)."""
    details = {"database_file": gold_path, "read_only": True,
               "memory_limit": "4GB", "threads": "8", "init_sql": ""}

    db_id, cur = None, None
    st, dbs = req(base, "/api/database", session=sid)
    if st != 200:
        # Read as empty, a failed listing would add the database twice.
        print(f"provision: could not list databases ({st}): {dbs.get('message')}",
              file=sys.stderr)
        return None
    for d in (dbs.get("data") or []):
        if d.get("name") == db_name and d.get("engine") == "duckdb":
            db_id, cur = d.get("id"), (d.get("details") or {})
            break

    if db_id is None:
        st, body = req(base, "/api/database", "POST", {
            "engine": "duckdb", "name": db_name, "details": details,
        }, session=sid)
        if st in (200, 201) and body.get("id"):
            print(f"provision: added DuckDB database '{db_name}' -> {gold_path}")
            return body["id"]
        print(f"provision: failed to add database ({st}): {body.get('message')}",
              file=sys.stderr)
        return None
    if any(cur.get(k) != v for k, v in details.items()):
        st, body = req(base, f"/api/database/{db_id}", "PUT",
                       {"details": {**cur, **details}}, session=sid)
        if st not in (200, 201):
            print(f"provision: could not update database settings ({st}): "
                  f"{body.get('message')}", file=sys.stderr)
            return None
        print(f"provision: database '{db_name}' present; connection "
              "settings converged (memory limit, spill dir)")
        return db_id
    print(f"provision: database '{db_name}' already present")
    return db_id


def ensure_collection(base, sid, name):
    """The id of the collection named `name`, created if absent, or None
    on failure.

    There is no fallback to the root collection: card and dashboard
    matching is scoped to this collection, so provisioning into the root
    one would update or archive any user item there that shares a
    pre-defined name. A listing that fails aborts too, since reading it
    as empty would create a second collection of the same name."""
    st, cols = req(base, "/api/collection", session=sid)
    if st != 200 or not isinstance(cols, list):
        print(f"provision: could not list collections ({st})", file=sys.stderr)
        return None
    for c in cols:
        if c.get("name") == name and not c.get("archived"):
            return c.get("id")
    st, body = req(base, "/api/collection", "POST", {"name": name}, session=sid)
    if st in (200, 201) and body.get("id"):
        print(f"provision: created collection '{name}'")
        return body["id"]
    print(f"provision: could not create collection '{name}' ({st}): "
          f"{body.get('message')}", file=sys.stderr)
    return None


def pre_defined_cards(base, sid, coll_id):
    """name -> card, for the non-archived cards in the pre-defined
    collection, or None when the card list is unreadable — read as
    empty, it would create every card a second time. Scoped to that
    collection so a user card that happens to share a name is never
    touched."""
    st, cards = req(base, "/api/card", session=sid)
    if st != 200 or not isinstance(cards, list):
        print(f"provision: could not list cards ({st})", file=sys.stderr)
        return None
    return {c.get("name"): c for c in cards
            if c.get("collection_id") == coll_id and not c.get("archived")}


def _empty(value):
    return value is None or value == [] or value == {}


def _typed_field_ref(got, want):
    """True when `got` is the field reference `want` with the base type
    Metabase adds to a template tag's dimension on save."""
    return (isinstance(got, list) and isinstance(want, list)
            and len(got) == len(want) == 3 and want[0] == "field"
            and got[:2] == want[:2] and want[2] is None
            and isinstance(got[2], dict) and set(got[2]) == {"base-type"})


def _same_value(got, want):
    """True when the server's value equals the payload's. A key on one
    side only counts, so a setting the payload drops is sent. Metabase
    stores an empty list or map as null, or leaves it out, and adds a
    base type to a template tag's field reference: those match."""
    if _empty(got) and _empty(want):
        return True
    if isinstance(want, dict):
        if not isinstance(got, dict):
            return False
        return all(_same_value(got.get(key), want.get(key)) for key in got.keys() | want.keys())
    if isinstance(want, list):
        if _typed_field_ref(got, want):
            return True
        if not isinstance(got, list) or len(got) != len(want):
            return False
        return all(_same_value(item, value) for item, value in zip(got, want, strict=True))
    return got == want


def card_same(existing, payload):
    """True when the stored card already is the payload. `existing`
    must carry its query in the legacy form the payload uses
    (stored_cards). A missing field is not a match."""
    if not isinstance(existing, dict) or existing.get("id") is None:
        return False
    for key, want in payload.items():
        if key not in existing or not _same_value(existing[key], want):
            return False
    return True


def _get_all(base, sid, paths):
    """key -> the body of GET `path`, for each key -> path in `paths`,
    read in parallel. A read that fails or answers anything but a JSON
    object is left out, so the caller sends that object."""
    def fetch(item):
        key, path = item
        try:
            st, body = req(base, path, session=sid)
        except Exception:
            # A body that is no JSON, or a read cut short.
            return key, None
        return key, body if st == 200 and isinstance(body, dict) else None

    with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
        return {key: body for key, body in pool.map(fetch, paths.items()) if body}


def stored_cards(base, sid, by_name, names):
    """name -> the server's card, for each of `names` already in
    `by_name`. The card list carries each query in MBQL 5, which no
    payload equals, so every card is read again in the legacy form.
    A card that cannot be read is left out, and so is sent."""
    return _get_all(base, sid, {name: f"/api/card/{by_name[name]['id']}?legacy-mbql=true"
                                for name in names if name in by_name})


def upsert_card(base, sid, by_name, name, payload, stored=None):
    """Create card `name` or update it in place. `stored` is the
    server's card (stored_cards); one equal to the payload is left in
    place. Returns (card id, 'created'|'updated'|'skipped') on success,
    (None, error text) on failure."""
    existing = by_name.get(name)
    if card_same(stored, payload):
        return stored["id"], "skipped"
    if existing:
        st, body = req(base, f"/api/card/{existing['id']}", "PUT", payload, session=sid)
        if st in (200, 201):
            return existing["id"], "updated"
    else:
        st, body = req(base, "/api/card", "POST", payload, session=sid)
        if st in (200, 201) and body.get("id"):
            return body["id"], "created"
    return None, f"({st}): {body.get('message')}"


def card_payload(coll_id, name, ctype, display, desc, query, viz):
    """The full /api/card payload shared by models and questions."""
    return {
        "name": name,
        "type": ctype,
        "description": desc,
        "collection_id": coll_id,
        "display": display,
        "visualization_settings": viz,
        "dataset_query": query,
    }


def upsert_cards(base, sid, by_name, payloads, label):
    """Create or update-in-place every card in `payloads` (name -> full
    /api/card payload). An unchanged card is left in place. Returns
    (name -> card id, #created, #updated, #skipped), or None on the
    first failure."""
    ids, n = {}, {"created": 0, "updated": 0, "skipped": 0}
    stored = stored_cards(base, sid, by_name, payloads)
    for name, payload in payloads.items():
        cid, how = upsert_card(base, sid, by_name, name, payload, stored.get(name))
        if cid is None:
            print(f"provision: failed to save {label} '{name}' {how}",
                  file=sys.stderr)
            return None
        ids[name] = cid
        n[how] += 1
    return ids, n["created"], n["updated"], n["skipped"]


def archive_all(base, sid, kind, ids):
    """Archive /api/<kind>/<id> for every id; returns how many succeeded."""
    n = 0
    for i in ids:
        st, _ = req(base, f"/api/{kind}/{i}", "PUT", {"archived": True},
                    session=sid)
        n += st in (200, 201)
    return n


def ensure_models(base, sid, db_id, coll_id, by_name):
    """Create/refresh the pre-defined report models in `coll_id`, and
    archive any retired (renamed-away) ones. An unchanged model is left
    in place. The models are content-free shims over the gold serving
    tables and views — no source data is baked in. Returns model name ->
    card id, or None on failure."""
    payloads = {
        name: card_payload(coll_id, name, "model", "table", desc,
                           {"type": "native", "database": db_id,
                            "native": {"query": query, "template-tags": {}}},
                           {})
        for name, (query, desc) in report_models().items()
    }
    saved = upsert_cards(base, sid, by_name, payloads, "model")
    if saved is None:
        return None
    ids, created, updated, skipped = saved
    archived = archive_all(base, sid, "card",
                           [by_name[n]["id"] for n in RETIRED_MODEL_NAMES
                            if n in by_name])
    print(f"provision: report models ({_ccy_slash()}) — {created} created, "
          f"{updated} updated, {skipped} unchanged, {archived} retired "
          f"(collection '{COLLECTION_NAME}')")
    return ids


def ensure_cards(base, sid, db_id, coll_id, by_name, model_ids):
    """Create/refresh the pre-defined questions over the models in
    `model_ids`, and archive any retired (renamed-away) ones. Returns
    card name -> id (the dashboards' tile lookup) and card name -> its
    description, or None on failure."""
    payloads = {}
    for name, (display, desc, query, viz) in question_defs(db_id, model_ids).items():
        payloads[name] = card_payload(coll_id, name, "question", display,
                                      desc, query, viz)
    for name, (ctype, display, desc, query, viz) in privacy_card_defs(db_id, model_ids).items():
        payloads[name] = card_payload(coll_id, name, ctype, display,
                                      desc, query, viz)
    saved = upsert_cards(base, sid, by_name, payloads, "card")
    if saved is None:
        return None
    ids, created, updated, skipped = saved
    archived = archive_all(base, sid, "card",
                           [by_name[n]["id"] for n in RETIRED_CARD_NAMES
                            if n in by_name])
    print(f"provision: questions — {created} created, {updated} updated, "
          f"{skipped} unchanged, {archived} retired")
    return ids, {name: body["description"] for name, body in payloads.items()}


def text_dashcard(dc_id, text, row=0, display="text"):
    """A virtual tile (no backing card) spanning row `row`: the absolute
    <-> privacy switch link on the top row as `text`, a section heading
    as `heading`."""
    return {"id": dc_id, "card_id": None, "row": row, "col": 0,
            "size_x": 24, "size_y": 1, "series": [], "parameter_mappings": [],
            "visualization_settings": {
                "virtual_card": {"name": None, "display": display,
                                 "visualization_settings": {},
                                 "dataset_query": {}, "archived": False},
                "text": text,
                "dashcard.background": False,
                "text.align_vertical": "middle"}}


def card_mappings(card, tcol, mode, name, card_ids):
    """The picker mappings of `card`'s tile on dashboard `name` (filter
    mode `mode`), before they are kept to the pickers the dashboard
    carries. A native card takes the pickers as template tags, with the
    target shape recorded when its SQL was built (a field filter is a
    dimension, a plain variable is not; only tags whose field id
    resolved are present). An MBQL card takes them as dimensions of its
    model, the time picker on the tile's time column `tcol`."""
    if not mode:
        return []
    native = NATIVE_PARAM_TARGETS.get(card)
    if native is not None:
        return [{"parameter_id": pid, "card_id": card_ids[card],
                 "target": target} for pid, target in native]
    if mode == "returns":
        # The MBQL returns scalars + table (the native charts took the
        # path above) take Currency + Start-year as dimensions: currency,
        # and window_from_year to pick the since-<year> summary. The
        # Source picker lands on the by-source table but NOT the global
        # scalars, whose silver_source_id is '' — a source filter would
        # blank them.
        maps = [
            {"parameter_id": CURRENCY_PARAM_ID, "card_id": card_ids[card],
             "target": ["dimension", _f("currency", "type/Text")]},
            {"parameter_id": START_YEAR_PARAM_ID, "card_id": card_ids[card],
             "target": ["dimension", _f("window_from_year", "type/Integer")]},
        ]
        if card not in RETURNS_GLOBAL_SCALARS:
            maps.append(
                {"parameter_id": SOURCE_PARAM_ID, "card_id": card_ids[card],
                 "target": ["dimension", _f("silver_source_id", "type/Text")]})
        return maps
    # The MBQL tiles that take a time filter sit on 'range' dashboards
    # (base_dashboards), so it is the range picker.
    maps = [{"parameter_id": TIME_PARAM_ID, "card_id": card_ids[card],
             "target": ["dimension", _f(tcol, "type/DateTime")]},
            {"parameter_id": SOURCE_PARAM_ID, "card_id": card_ids[card],
             "target": ["dimension",
                        _f("silver_source_id", "type/Text")]}]
    if name in SPENDING_DASHBOARDS:
        # The Spending pickers land on the spending models' dimensions.
        # Currency is a row filter here (the model carries one row per
        # reporting currency) where a native spending card takes it as a
        # {{currency}} variable — the same split the returns tiles
        # already make.
        maps += [{"parameter_id": pid, "card_id": card_ids[card],
                  "target": ["dimension", _f(col, "type/Text")]}
                 for pid, col in (
                     (SPEND_CURRENCY_PARAM_ID, "currency"),
                     (ACCOUNT_PARAM_ID, "display_name"),
                     (CATEGORY_PARAM_ID, "spend_primary"))
                 if not (pid == SPEND_CURRENCY_PARAM_ID
                         and card in SPEND_ALL_CURRENCY_CARDS)]
    return maps


def overview_home(card, dash):
    """The dashboard tile `card` of overview dashboard `dash` comes from
    (OVERVIEW_TILE_HOMES): on the twin, that dashboard's twin. None for
    the overview's own tiles and for a twin tile whose home has no twin."""
    twin = dash.endswith(PRIVACY_SUFFIX)
    for base, home in OVERVIEW_TILE_HOMES.items():
        if card == (privacy_name(base) if twin and base not in PRIVACY_EXEMPT_CARDS else base):
            if not twin:
                return home
            return None if home in NO_PRIVACY_TWIN else home + PRIVACY_SUFFIX
    return None


def overview_mappings(card, dash, defs, params, card_ids):
    """The picker mappings of `card`'s tile on overview dashboard `dash`:
    each mapping the card has, on its home dashboard or by its own
    template tags, moved onto the overview's picker of the same slug (a
    native card's tags bear the slugs' names). A mapping with no such
    picker here drops, and the card runs at that tag's default."""
    slug_pid = {x["slug"]: x["id"] for x in params[dash]}
    native = NATIVE_PARAM_TARGETS.get(card)
    if native is not None:
        return [{"parameter_id": slug_pid[target[1][1]], "card_id": card_ids[card], "target": target}
                for _pid, target in native if target[1][1] in slug_pid]
    home = overview_home(card, dash)
    if home is None:
        return []
    _desc, mode, _sibling, tiles = defs[home]
    tcol = next(t for c, *_r, t in tiles if c == card)
    home_slug = {x["id"]: x["slug"] for x in params[home]}
    return [{**m, "parameter_id": slug_pid[home_slug[m["parameter_id"]]]}
            for m in card_mappings(card, tcol, mode, home, card_ids)
            if home_slug.get(m["parameter_id"]) in slug_pid]


def overview_click(card, dash, params, dash_ids):
    """The click behaviour of `card`'s tile on overview dashboard `dash`:
    open its home dashboard (overview_home), passing each overview
    picker that dashboard carries by slug, and a year-to-date return's
    year to the Returns Start year. Empty for a tile with no home."""
    home = overview_home(card, dash)
    if home is None:
        return {}
    target = {x["slug"]: x["id"] for x in params[home]}

    def link(tid, source):
        return {"id": tid, "source": source, "target": {"type": "parameter", "id": tid}}
    mapping = {target[x["slug"]]: link(target[x["slug"]], {"type": "parameter", "id": x["id"], "name": x["name"]})
               for x in params[dash] if x["slug"] in target}
    if card in {c for c, _m, _col in RETURN_YTD_CARDS} and "start_year" in target:
        mapping[target["start_year"]] = link(target["start_year"],
                                             {"type": "column", "id": "start_year", "name": "start_year"})
    return {"click_behavior": {"type": "link", "linkType": "dashboard",
                               "targetId": dash_ids[home], "parameterMapping": mapping}}


def unreached_note(dash, mapped, params):
    """The sentence an overview tile's description ends in, naming the
    pickers of dashboard `dash` (parameters `params`) that the parameter
    ids `mapped` leave out; empty where every picker reaches the tile."""
    names = [x["name"] for x in params if x["id"] not in mapped]
    if not names:
        return ""
    if len(names) == len(params):
        return f" On the {dash} no picker reaches it."
    listed = names[0] if len(names) == 1 else ", ".join(names[:-1]) + " and " + names[-1]
    return f" On the {dash} the {listed} picker{'s' if len(names) > 1 else ''} do{'' if len(names) > 1 else 'es'} not reach it."


def crossfilter_viz(card, param_ids):
    """A tile's own viz settings: for a CROSSFILTER_TILES card on a
    dashboard carrying its picker, the click behaviour that sets the
    picker to the value of the cell clicked in each of its columns. Empty
    for any other tile."""
    cols, pid = CROSSFILTER_TILES.get(card, ((), None))
    if pid not in param_ids:
        return {}
    return {"column_settings": {
        f'["name","{c}"]': {"click_behavior": {
            "type": "crossfilter",
            "parameterMapping": {pid: {"id": pid, "source": {"type": "column", "id": c, "name": c},
                                       "target": {"type": "parameter", "id": pid}}}}}
        for c in cols}}


def ensure_dashboards(base, sid, coll_id, card_ids, model_ids, titles=None, descs=None):
    """Create/refresh the pre-defined dashboards (and their privacy twins)
    with their global filters (see dashboard_parameters) linked to every
    tile and, where there is a twin, a switch link to it on the top row,
    and archive any retired (renamed-away) ones. A card in `titles`
    (tile_titles) shows that title on its tiles. `descs`, card name ->
    description, lets a Wealth Overview tile carry its card's description
    with the pickers that do not reach it named. A dashboard in
    DASHBOARD_HEADINGS gets its full-width headings, and a Wealth Overview
    tile opens its home dashboard (overview_click). An unchanged dashboard
    is left in place. One that differs has its parameter list and its
    tiles replaced wholesale, so the layout converges to spec and a tile
    added by hand does not survive. The dashboard description says to
    duplicate before customizing."""
    titles = titles or {}
    st, items = req(base, f"/api/collection/{coll_id}/items?models=dashboard",
                    session=sid)
    if st != 200:
        # Read as empty, a failed listing would create every dashboard twice.
        print(f"provision: could not list dashboards ({st}): {items.get('message')}",
              file=sys.stderr)
        return 1
    existing = {d.get("name"): d.get("id") for d in (items.get("data") or [])}

    defs = dashboard_defs()
    n = {"created": 0, "updated": 0, "skipped": 0}
    # First pass: make sure every dashboard exists, so the switch links
    # can point at the sibling's id.
    dash_ids = {}
    for name, (desc, _mode, _sibling, _tiles) in defs.items():
        did = existing.get(name)
        if did is None:
            st, body = req(base, "/api/dashboard", "POST",
                           {"name": name, "description": desc,
                            "collection_id": coll_id}, session=sid)
            if st not in (200, 201) or not body.get("id"):
                print(f"provision: failed to create dashboard '{name}' ({st}): "
                      f"{body.get('message')}", file=sys.stderr)
                return 1
            did = body["id"]
            n["created"] += 1
        dash_ids[name] = did
    # The dashboards listed before this run, read for the comparison. One
    # created above has no tiles yet, so it is sent without a read.
    stored = _get_all(base, sid, {name: f"/api/dashboard/{existing[name]}"
                                  for name in defs if existing.get(name) is not None})

    params = {name: dashboard_parameters(model_ids, mode, name)
              for name, (_desc, mode, _sibling, _tiles) in defs.items()}
    for name, (desc, mode, sibling, tiles) in defs.items():
        parameters = params[name]
        param_ids = {x["id"] for x in parameters}
        # Both returns twins show the same scale-free ratios, so their
        # link says "redacted" where the others say "shares".
        privacy = name.endswith(PRIVACY_SUFFIX)
        if mode == "returns":
            label = ("Switch to the full view — money columns included"
                     if privacy else
                     "Switch to the privacy view — money columns redacted")
        else:
            label = ("Switch to absolute values" if privacy else
                     "Switch to the privacy view — values as shares (%), "
                     "not amounts")
        icon = "🔓" if privacy else "🔒"
        link = (f"{icon} [{label}](/dashboard/{dash_ids[sibling]})"
                if sibling else None)
        base_name = sibling if privacy else name
        overview = name in OVERVIEW_DASHBOARDS
        tile_titles_here = {**titles, **dashboard_tile_titles(base_name)}

        # The switch link occupies row 0, so the tiles and headings shift
        # down one row; a dashboard without a twin has no link and no
        # shift. The vehicle filter renders on the Top-positions tile
        # itself (inline_parameters) rather than in the dashboard's
        # filter bar: it only applies to that one widget.
        dashcards = [text_dashcard(-99, link)] if link else []
        shift = 1 if link else 0
        dashcards += [text_dashcard(-(100 + k), text, row + shift, "heading")
                      for k, (row, text) in enumerate(DASHBOARD_HEADINGS.get(base_name, []))]
        for i, (card, row, col, sx, sy, tcol) in enumerate(tiles):
            # The pickers a tile answers to, kept to those the dashboard
            # carries: the Spending twin has no Account picker
            # (dashboard_parameters drops it), and a mapping naming a
            # parameter not on the dashboard is a target with no filter
            # behind it. A Wealth Overview tile takes the overview's
            # pickers by slug (overview_mappings).
            maps = [m for m in (overview_mappings(card, name, defs, params, card_ids) if overview
                                else card_mappings(card, tcol, mode, name, card_ids))
                    if m["parameter_id"] in param_ids]
            viz = {**crossfilter_viz(card, param_ids),
                   **({"card.title": tile_titles_here[card]} if card in tile_titles_here else {})}
            # A Wealth Overview tile opens its home dashboard, and its
            # description names the pickers that do not reach it.
            if overview:
                viz.update(overview_click(card, name, params, dash_ids))
                note = unreached_note(base_name, {m["parameter_id"] for m in maps}, parameters)
                if note and descs and card in descs:
                    viz["card.description"] = descs[card] + note
            dashcards.append({"id": -(i + 1), "card_id": card_ids[card], "row": row + shift,
                              "col": col, "size_x": sx, "size_y": sy, "series": [],
                              "visualization_settings": viz,
                              "inline_parameters":
                                  [VEHICLE_PARAM_ID] if mode == "asof"
                                  and card in POSITION_FILTERED_CARDS else [],
                              "parameter_mappings": maps})
        current = stored.get(name)
        if dashboard_unchanged(current, name, desc, parameters, dashcards):
            n["skipped"] += 1
            continue
        if name in existing:
            n["updated"] += 1
        # A change to the pickers alone (Allocation's As of day default
        # follows the snapshot) is sent alone, so the tiles keep their ids.
        if dashboard_unchanged(current, name, desc, (current or {}).get("parameters"), dashcards):
            if not _put_parameters(base, sid, dash_ids[name], name, parameters,
                                   "update the filters of"):
                return 1
            continue
        st, body = req(base, f"/api/dashboard/{dash_ids[name]}", "PUT",
                       {"name": name, "description": desc,
                        "parameters": parameters,
                        "dashcards": dashcards}, session=sid)
        if st not in (200, 201):
            print(f"provision: failed to lay out dashboard '{name}' ({st}): "
                  f"{body.get('message')}", file=sys.stderr)
            return 1
        # Deleting a dashcard that carries an inline filter makes Metabase
        # drop that filter from the dashboard — and the wholesale dashcard
        # replacement above deletes every old tile, so an inline parameter
        # sent in the same PUT gets cleaned right back up. Re-assert the
        # parameter list now that the new tiles are in place.
        if parameters and not _put_parameters(base, sid, dash_ids[name], name, parameters,
                                              "re-assert filters on"):
            return 1

    archived = archive_all(base, sid, "dashboard",
                           [existing[name] for name in RETIRED_DASHBOARD_NAMES
                            if existing.get(name) is not None])
    print(f"provision: dashboards — {n['created']} created, {n['updated']} updated, "
          f"{n['skipped']} unchanged, {archived} retired")
    return 0


def _put_parameters(base, sid, did, name, parameters, what):
    """PUT dashboard `did`'s parameter list alone. False when the server
    refuses it, with "failed to `what` `name`" printed."""
    st, body = req(base, f"/api/dashboard/{did}", "PUT",
                   {"parameters": parameters}, session=sid)
    if st in (200, 201):
        return True
    print(f"provision: failed to {what} '{name}' ({st}): {body.get('message')}",
          file=sys.stderr)
    return False


def _dashcards_same(server, desired):
    """True when the stored tiles already match the payload. Tiles pair
    by their place on the grid, whatever order the server lists them
    in. A tile is compared on the keys the payload sends; the rest of
    a stored tile is the server's own. A missing tile list is not a
    match."""
    if not isinstance(server, list) or len(server) != len(desired):
        return False
    if not all(isinstance(t, dict) for t in server):
        return False

    def place(tile):
        return (tile.get("row") or 0, tile.get("col") or 0)

    server, desired = sorted(server, key=place), sorted(desired, key=place)
    for got, want in zip(server, desired, strict=True):
        for key, value in want.items():
            if key != "id" and not _same_value(got.get(key), value):
                return False
    return True


def dashboard_unchanged(current, name, desc, parameters, dashcards):
    """True when the stored dashboard already matches the payload."""
    return (isinstance(current, dict)
            and current.get("name") == name
            and current.get("description") == desc
            and _same_value(current.get("parameters"), parameters)
            and _dashcards_same(current.get("dashcards"), dashcards))


if __name__ == "__main__":
    sys.exit(main())
