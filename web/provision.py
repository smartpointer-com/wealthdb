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

With --dump FILE it opens no connection: it writes every model, card
and dashboard payload a provision into an empty collection sends, with
ids numbered in definition order, to FILE as JSON (payload_dump). Two
trees' dumps diff to what a change does to the payloads.
"""
import argparse
import concurrent.futures
import contextlib
import json
import os
import re
import sys
import time
from typing import NamedTuple

import metabase


def wait_health(client, limit=270, delay=0.25):
    """Polls Metabase's health endpoint until it answers, for at most
    `limit` seconds; once it answers, prints how long the wait took. A
    short poll: a restarted server answers within seconds of coming up,
    and each poll is one local request."""
    start = time.monotonic()
    while time.monotonic() - start < limit:
        st, _ = client.get("/api/health", timeout=5)
        if st == 200:
            print(f"provision: timing: wait for Metabase {time.monotonic() - start:.3f}s", file=sys.stderr)
            return True
        time.sleep(delay)
    return False


def main(client=None):
    """Provision the Metabase --base names, or `client`, the tests' stand-in."""
    ap = argparse.ArgumentParser()
    ap.add_argument("--base")
    ap.add_argument("--email")
    ap.add_argument("--gold-path")
    ap.add_argument("--db-name", default="gold")
    ap.add_argument("--default-currency", default="USD")
    ap.add_argument("--missing-basis", default=MISSING_BASIS_READINGS[0], choices=MISSING_BASIS_READINGS)
    ap.add_argument("--source-colors", default="",
                    help="the file that keeps each source's colour (SOURCE_PALETTE)")
    ap.add_argument("--dump", metavar="FILE",
                    help="write the payloads to FILE as JSON and exit, with no connection;"
                         " --base, --email and --gold-path are then not needed")
    ap.add_argument("--withholding", default="yes", choices=DUMP_WITHHOLDING,
                    help="with --dump: whether the snapshot holds a withholding table"
                         " (unknown: the probe did not answer)")
    ap.add_argument("--sources", default="", metavar="A,B",
                    help="with --dump: the sources that hold an account, in load order")
    a = ap.parse_args()
    missing = [f for f, v in (("--base", a.base), ("--email", a.email), ("--gold-path", a.gold_path))
               if v is None]
    if missing and not a.dump:
        ap.error("the following arguments are required: " + ", ".join(missing))
    global DEFAULT_CURRENCY, DEFAULT_MISSING_BASIS
    DEFAULT_CURRENCY = default_currency(a.default_currency)
    DEFAULT_MISSING_BASIS = a.missing_basis
    if a.dump:
        dump = payload_dump(DUMP_WITHHOLDING[a.withholding], [s for s in a.sources.split(",") if s])
        with open(a.dump, "w", encoding="utf-8") as f:
            f.write(json.dumps(dump, sort_keys=True, indent=1, ensure_ascii=False) + "\n")
        return 0
    password = os.environ.get("WEALTHDB_WEB_ADMIN_PASSWORD", "")
    if not password:
        print("provision: WEALTHDB_WEB_ADMIN_PASSWORD is not set", file=sys.stderr)
        return 1

    client = client or metabase.Client(a.base)
    if not wait_health(client):
        print("provision: Metabase did not become healthy in time", file=sys.stderr)
        return 1

    _, props = client.get("/api/session/properties")
    setup_done = bool(props.get("has-user-setup"))
    token = props.get("setup-token")

    if not setup_done and token:
        st, body = client.post("/api/setup", {
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
    _, body = client.post("/api/session", {"username": a.email, "password": password})
    sid = body.get("id")
    if not sid:
        if setup_done:
            print("provision: couldn't verify admin login (password changed?) — leaving as-is")
            return 0
        print(f"provision: login failed: {body.get('message')}", file=sys.stderr)
        return 1

    client = client.with_auth(session=sid)
    db_id = ensure_database(client, a.db_name, a.gold_path)
    if db_id is None:
        return 1

    status = check_gold_schema(client, db_id)
    if status:
        return status
    tables = ensure_synced(client, db_id)
    if tables is None:
        return 1
    field_ids = field_ids_from(tables)
    unfingerprint_lists(client, tables)

    coll_id = ensure_collection(client, COLLECTION_NAME)
    if coll_id is None:
        return 1
    by_name = pre_defined_cards(client, coll_id)
    if by_name is None:
        return 1
    model_ids = ensure_models(client, db_id, coll_id, by_name)
    if model_ids is None:
        return 1
    colors = source_colors(client, db_id, a.source_colors)
    saved = ensure_cards(client, db_id, coll_id, by_name, field_ids, model_ids, colors)
    if saved is None:
        return 1
    card_ids, descs = saved
    titles = tile_titles(withholding_table(client, db_id))
    return ensure_dashboards(client, coll_id, card_ids, model_ids, titles, descs)


# --dump's --withholding: what the snapshot's withholding probe answers
# (withholding_table), unknown standing for a probe that did not answer.
DUMP_WITHHOLDING = {"yes": True, "no": False, "unknown": None}


def offline_field_ids():
    """(table, column) -> field id for every column of
    filter_field_columns, numbered from 1 in its order: the ids a sync
    would resolve, for a run with no Metabase."""
    cols = [(t, c) for t, names in filter_field_columns().items() for c in names]
    return {key: i for i, key in enumerate(cols, start=1)}


def payload_dump(withholding, sources):
    """{"models", "cards", "dashboards"}: name -> payload for every
    model, question and dashboard that a provision into an empty
    collection sends, built without a connection, and "ids": the id of
    each of them and of each field ("table.column"), which the payloads
    refer to. The database and the collection are 1, fields are numbered
    by offline_field_ids, models then cards from 1 in the order they are
    saved, dashboards from 1 in definition order. `withholding` is what
    withholding_table answers, `sources` the snapshot's sources that
    hold an account (source_colors with no kept file). A dashboard's
    payload is its full layout PUT."""
    field_ids = offline_field_ids()
    models = model_payloads(1, 1)
    model_ids = {name: i for i, name in enumerate(models, start=1)}
    cards = card_payloads(1, 1, field_ids, model_ids, account_source_colors({}, sources))
    card_ids = {name: i for i, name in enumerate(cards, start=len(models) + 1)}
    defs = dashboard_defs(card_defs())
    dash_ids = {name: i for i, name in enumerate(defs, start=1)}
    descs = {name: body["description"] for name, body in cards.items()}
    payloads = dashboard_payloads(defs, card_defs(), model_ids, card_ids, dash_ids,
                                  tile_titles(withholding), descs)
    dashboards = {name: {"name": name, "description": desc, "parameters": parameters,
                         "dashcards": dashcards}
                  for name, (desc, parameters, dashcards) in payloads.items()}
    return {"models": models, "cards": cards, "dashboards": dashboards,
            "ids": {"models": model_ids, "cards": card_ids, "dashboards": dash_ids,
                    "fields": {f"{t}.{c}": i for (t, c), i in field_ids.items()}}}


# The reporting currencies: the value column sets the `_multi` report
# macros and the web_* serving views carry, and the rows report_returns
# carries. Every currency list, CASE and picker below is generated from
# this one tuple.
REPORTING_CURRENCIES = ("USD", "CHF", "EUR", "GBP")

# The currency every picker and every native card's {{currency}}
# variable defaults to: wealthdb.cfg's default_currency when it is a
# reporting currency (main() sets it), USD otherwise.
DEFAULT_CURRENCY = "USD"

# The As of day picker's default: today. A card reads the last day of
# the holdings history on or before the day (web_history_calendar), so a
# snapshot not refreshed today still shows its latest holdings.
ASOF_DEFAULT = "thisday"


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
    macros (gold derived/30_holdings.sql) are the single source of truth.
    Each model wraps the serving table `web refresh` fills with a macro's
    output (gold servingTables), a macro computing its whole output on
    every read, and renders epoch columns as TIMESTAMP (`epoch_ms` ->
    zone-free UTC, not `to_timestamp`, which is TIMESTAMPTZ and renders
    in the reading session's zone) for Metabase. The taxonomy, spending,
    income and cashflow models instead wrap the web_* serving views (gold
    derived/90_web.sql) that do that rendering, the cash fold-in and the
    '(uncategorized)' labelling themselves, and the gains- and
    yield-classes models list the classes of web_gains and of web_yield
    and web_yield_curve. The macros already emit DECIMAL money/quantity
    columns and one value column set per reporting currency
    (REPORTING_CURRENCIES), so no value casting is needed here. The
    `_latest` reports are as of each account's latest snapshot; the
    `_history` reports carry value forward per day;
    the two `_pct` models are the privacy variants the privacy twins'
    MBQL cards run over (values as % of the latest global net worth,
    absolute-value columns dropped) — every other privacy chart is
    native SQL over the gold web_* serving views, recomputing its
    denominator per query so the dashboard pickers rescale it (see
    card_defs). The returns models wrap the materialized
    report_returns TABLE (migration 0026) instead of a macro, with the
    same epoch-to-TIMESTAMP rendering."""
    def wrap(from_expr, ts_cols=(), exclude=(), where=""):
        parts = [f"epoch_ms({c} * 1000) AS {c}" for c in ts_cols]
        excl = f" EXCLUDE ({', '.join(exclude)})" if exclude else ""
        cond = f" WHERE {where}" if where else ""
        return f"SELECT *{excl} REPLACE ({', '.join(parts)}) FROM {from_expr}{cond}"

    # Scalar subquery with the latest global net worth per currency — the
    # shared normalization constant of the privacy (_pct) models. A fixed
    # constant keeps every aggregate's shape; the privacy dashboards'
    # charts recompute a selection-aware denominator per query instead
    # (card_defs), so this constant only backs the twins' MBQL
    # cards and their drill-through. Guarded to NULL when the latest
    # total is zero or negative (empty or under-water gold): dividing
    # would render inf/NaN resp. sign-flipped percentages, where NULL
    # just blanks the values.
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
        (derived/90_web.sql), which already render as_of_day as TIMESTAMP and
        fold each source's cash balance into a class/vehicle of its own so
        the rows sum exactly to net worth."""
        return f"SELECT * FROM {view}"

    def spending(pct=False):
        """The spending models over the gold web_spending view, which
        already renders occurred_at as TIMESTAMP and labels an
        unresolved category '(uncategorized)'.

        `spend_primary` and `spend_detailed` carry the DISPLAY LABEL, not
        the vendored value: a dashboard picker's dropdown is the list of
        labels it filters by, so the label is what a reader both sees and
        selects. The vendored values travel beside them as `*_id` for a
        filter that must survive a label being reworded. Swapping the two
        is safe for the cards that compare them, because the labels
        preserve the relation the comparison turns on: a delta's primary
        and detailed read the same, a vendored pair does not.

        `provider_category` is the ISSUER's own classification of the line
        (migration 0057), kept for a reader who wants the card provider's
        view. It is never summed with ours and no tile aggregates it — the
        two disagree by design.

        `display_name` carries the account LABEL (account_label) on the
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
        by (PICKERS). The label is what stays, not the bare
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
    # (valued_at) and the instant a run last read it (gold's
    # source_observed view): a quiet source is observed through its
    # latest download although its latest mark is older. Unknown
    # reads as the snapshot itself.
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
        # rest rather than dropping it. For time-series charts; filter / aggregate by
        # as_of_day. history@today reconciles with the matching _latest report on totals
        # for a source that writes every account in one run; where runs are partial the
        # history carries every account while _latest values only the last run's. In
        # report_positions_history, snapshot_at is the ACCOUNT's active snapshot, so
        # several values can share one source-day. Each reads the serving table `web
        # refresh` fills from its macro: a macro computes the whole history on every
        # read, and every tile of a dashboard would pay for it at once. So do the latest
        # models and the transactions ones.
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
        # views (card_defs), whose denominators follow the pickers.
        "report_sources_latest_pct": (
            pct_wrap(sources_latest, V3, SOURCES_TS, BASE3),
            "Privacy variant of report_sources_latest: totals as % of the "
            "latest global net worth (per currency); base-currency columns "
            "dropped."),
        # Asset classes incl. cash, so the asset-class widget sums exactly
        # to net worth: positions grouped by class, with each source's
        # cash balance folded in as a 'cash' class (money-market-fund
        # positions and cash balances share it).
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
        # The classes the gains rows carry, `unknown` among them where
        # nothing classes a position: the Gains dashboard's Asset class
        # picker lists them, as the asset class history may never hold
        # `unknown`. It backs that picker alone.
        "report_gains_classes": (
            "SELECT DISTINCT asset_class FROM web_gains",
            "The asset classes the gains rows carry, `unknown` where nothing classes a "
            "position. It backs the Gains dashboard's Asset class picker."),
        # The classes web_yield and web_yield_curve carry, `unknown` among
        # them where nothing classes a holding: the Yield dashboard's
        # Asset class picker lists them, as the asset class history may
        # never hold `unknown`. It backs that picker alone.
        "report_benchmarks": (
            "SELECT benchmark FROM web_benchmarks",
            "The benchmarks, built-in and configured, and None. It backs the "
            "Benchmark picker."),
        "report_yield_classes": (
            "SELECT asset_class FROM web_yield\nUNION\nSELECT asset_class FROM web_yield_curve",
            "The asset classes the yield rows carry, `unknown` where nothing classes a "
            "holding. It backs the Yield dashboard's Asset class picker."),
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
# enrichment pass could not resolve. Rendered at the view rather than left
# NULL, so a breakdown shows the backlog as a bucket instead of an
# unlabelled slice; the uncategorized-share card counts rows carrying it.
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
                      # The other Exposure charts over time show shares
                      # alone, so the twin reuses them.
                      "Asset classes over time (privacy)",
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
                      "Asset class split", "Asset class split (privacy)",
                      # The Income dashboard's cards of received and
                      # expected income count every income line; the
                      # twin's yield is the holdings' and says so.
                      "Income in the window (privacy)",
                      "Received income", "Received income (privacy)",
                      "Expected remaining", "Expected remaining (privacy)",
                      "Holdings' income in the window", "Holdings' income in the window (privacy)",
                      "Holdings' income received", "Holdings' income received (privacy)",
                      "Holdings' income expected", "Holdings' income expected (privacy)",
                      "Holdings' monthly average income", "Holdings' monthly average income (privacy)",
                      "Received interest", "Received interest (privacy)",
                      "Received dividends", "Received dividends (privacy)",
                      "Expected interest remaining", "Expected interest remaining (privacy)",
                      "Expected dividends remaining", "Expected dividends remaining (privacy)"]

# Dashboard names retired by renames; archived on provision so a re-run
# cleans them up.
RETIRED_DASHBOARD_NAMES = ["Net Worth"]

# The reserved source id declared accounts carry in gold
# (canonical.DeclaredSourceID in the engine).
DECLARED_SOURCE = "declared"

# The Benchmark picker's value for no benchmark, and the name of the
# household's side on a card that sets the two beside each other. Gold
# holds the first too (canonical.NoBenchmark).
NO_BENCHMARK = "None"
PORTFOLIO_SERIES = "Portfolio"
# The picked benchmark on a line chart, under one label whichever is picked:
# the small tiles draw no legend, so its look tells it from the sources, and
# a card's series_settings keys on the label. The picker names the benchmark.
# No such series, no effect: under None a card draws as if the key were absent.
BENCHMARK_SERIES = "Benchmark"
# The colour Metabase gives each share column on its own, kept for the
# portfolio's series on a card that breaks out by series.
EXPOSURE_BAR_COLORS = {"share_pct": "#7172AD", "class_share_pct": "#A989C5"}
# A mid-tone in no palette: it has to read on a light and on a dark theme.
BENCHMARK_COLOR = "#8A99A8"
BENCHMARK_LINE = {"series_settings": {BENCHMARK_SERIES: {
    "color": BENCHMARK_COLOR, "line.style": "dashed", "line.size": "L", "line.marker_enabled": False}}}
BENCHMARK_PARAM_ID = "aa5df124"

BENCHMARK_TAG = {"id": "benchmark-tag", "name": "benchmark", "display-name": "Benchmark",
                 "type": "text", "default": NO_BENCHMARK, "required": True}

EXCESS_CARDS = ("Excess return (TWR)", "Excess return (MWR)", "Excess annualized return (TWR)")


# A source's colour. Metabase colours a series by its place, and changes
# palette between eight series and nine, so a picked benchmark or a
# narrowed Source picker would recolour every line. A card that draws a
# series per source (SOURCE_SERIES_COLUMNS) therefore names each source's
# colour in its series_settings, which key on the series' label: the
# source id. The ids are gold's, so provisioning reads them from the
# snapshot, and the file --source-colors names keeps the assignment
# (source_colors). Twenty-four colours, so no two of a household's first
# 24 sources share one. Each reads at 3:1 or better on a light and on a
# dark theme. The order is farthest first: each slot is the colour
# farthest from every slot before it, under normal vision and under
# simulated red-green colour blindness, so a household with few sources
# gets the strongest contrast. Past 24 sources a slot is shared.
SOURCE_PALETTE = ("#89579e", "#fd1a06", "#1955f9", "#238c0a", "#987aff", "#cf2cc8",
                  "#bf6c5a", "#c8105c", "#197ece", "#289b7d", "#bf5ea4", "#2b7440",
                  "#fd1f80", "#6990e2", "#5d6ff5", "#a481c1", "#972de1", "#9c5132",
                  "#1089a0", "#b88848", "#5b65b9", "#a4476d", "#af55df", "#648746")
# The global line beside the sources, solid where the benchmark is dashed.
ALL_SOURCES_SERIES = "(all sources)"
ALL_SOURCES_COLOR = "#6b7785"
# The breakout columns that hold a source id.
SOURCE_SERIES_COLUMNS = ("silver_source_id", "source")


def assign_source_colors(kept, sources):
    """Each of `sources` with a colour, first come, first served: a
    source in `kept` keeps its colour, and a new one takes the first slot
    of SOURCE_PALETTE no source holds, in the order `sources` lists them.
    So a source keeps its colour on every load, and a new source never
    recolours another. With every slot held, a new one takes the slot
    the fewest hold, the first of those. `kept` entries for sources gone
    from gold stay, so their slot stays theirs."""
    out = {k: v for k, v in kept.items() if v in SOURCE_PALETTE}
    for src in sources:
        if src in out:
            continue
        held = list(out.values())
        out[src] = min(SOURCE_PALETTE, key=lambda c: (held.count(c), SOURCE_PALETTE.index(c)))
    return out


def load_source_colors(path):
    """The kept assignment in `path`, or none when it is absent or not
    an object of strings."""
    try:
        with open(path) as f:
            kept = json.load(f)
    except (OSError, ValueError):
        return {}
    if not isinstance(kept, dict):
        return {}
    return {k: v for k, v in kept.items() if isinstance(k, str) and isinstance(v, str)}


def save_source_colors(path, colors):
    """Write the assignment to `path` whole, through a file beside it."""
    tmp = path + ".tmp"
    try:
        with open(tmp, "w") as f:
            json.dump(colors, f, indent=2, sort_keys=True)
            f.write("\n")
        os.replace(tmp, path)
    except OSError as e:
        with contextlib.suppress(OSError):
            os.remove(tmp)
        print(f"provision: could not save {path}: {e}", file=sys.stderr)


def source_colors(client, db_id, path):
    """Each source's colour: the kept assignment, with the snapshot's
    sources that hold an account added in the order gold first loaded
    them, then `declared`. A reference source, such as FX rates, holds
    no account and draws no series. A probe that does not answer adds
    none, and the cards keep the colours already kept."""
    kept = load_source_colors(path) if path else {}
    rows = probe_rows(client, db_id,
                      "SELECT silver_source_id FROM silver_sources"
                      " WHERE silver_source_id IN (SELECT silver_source_id FROM accounts)"
                      " ORDER BY first_loaded_at, silver_source_id")
    colors = account_source_colors(kept, None if rows is None else
                                   [r[0] for r in rows if r and r[0]])
    if path and colors != kept:
        save_source_colors(path, colors)
    return colors


def account_source_colors(kept, sources):
    """assign_source_colors over `sources`, the snapshot's sources that
    hold an account, then DECLARED_SOURCE. None, a probe that did not
    answer, adds no source."""
    return assign_source_colors(kept, [] if sources is None else [*sources, DECLARED_SOURCE])


def with_source_colors(viz, colors):
    """`viz` with each source's colour, and the fixed colour of the
    '(all sources)' line, where its breakout is a source column. A
    setting the card names for a series already wins over the colour, so
    the benchmark keeps its own. Any other card comes back unchanged."""
    dims = viz.get("graph.dimensions") or []
    if len(dims) != 2 or dims[1] not in SOURCE_SERIES_COLUMNS:
        return viz
    settings = {src: {"color": c} for src, c in colors.items()}
    settings[ALL_SOURCES_SERIES] = {"color": ALL_SOURCES_COLOR}
    for key, value in (viz.get("series_settings") or {}).items():
        settings[key] = {**settings.get(key, {}), **value}
    return {**viz, "series_settings": settings}


# Denominator-neutral by design: a twin card's own text says what its
# shares are of (latest total, chosen day's total, peak month, the
# window's own net spend) where its title does not.
PRIVACY_DESC = " Absolute amounts never show."

# The uncategorised-income card, which the Income dashboard and its
# privacy twin draw identically: a share of ROWS is already
# privacy-safe, so the twin adds PRIVACY_DESC and changes nothing else.
# Both the words and the SQL live here so the two cannot drift apart.
IN_UNCATEGORIZED_DESC = (
    f"The share of the window's income lines nothing placed, labelled '{UNCATEGORIZED}', in any currency.")
IN_UNCATEGORIZED_SQL = (
    "SELECT count(*) FILTER (WHERE income_label = "
    f"'{UNCATEGORIZED}')::DOUBLE\n       / nullif(count(*), 0) AS uncategorized_share\n"
    "  FROM web_income")

# The whole-portfolio scalars — global grain, so the Source picker doesn't
# apply (their silver_source_id is '').
RETURNS_GLOBAL_SCALARS = {"Return (TWR)", "Return (MWR)", "Annualized return (TWR)"}

RETURNS_PRIVACY_DESC = " Money columns are redacted."


# The suffix that marks a privacy variant — of a card, and of the
# dashboard it sits on. One home, because it is also how a dashboard
# tells itself apart from its twin (dashboard_parameters,
# ensure_dashboards).
PRIVACY_SUFFIX = " (privacy)"

# A postal address closing a label: a comma, a street and a house number,
# a comma, a postal code of four or five digits and a town, to the end of
# the value. A bank can name a mortgage by the property. columns.go's
# sensitiveIDPatterns holds the same pattern for -p, and both engines
# read it as RE2.
ADDRESS_PATTERN = r", [^,]* \d+[a-zA-Z]?, \d{4,5} .*$"


def twin_name(column):
    """SQL for a name `column` on a privacy card: the name less any
    address, which keeps the product name before it. Every twin card
    that shows a position's or a security's name goes through this."""
    return f"regexp_replace({column}, '{ADDRESS_PATTERN}', '')"


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


def _mbql(model, clauses):
    """An MBQL dataset_query over pre-defined model `model`, its source
    table a reference bind resolves."""
    q = {"source-table": ("model", model)}
    q.update(clauses)
    return {"type": "query", "query": q}


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


def returns_tags(sql=None):
    """Template tags for the native returns charts. Currency and Source are
    field filters on report_returns (a dropdown, values from the column).
    Start year is a plain number variable (it drives a `year(end_day) >= …`
    range, which a field filter can't express). With `sql` the Source tag
    only where that query reads it, so a tile it misses says so."""
    # A field-filter default is a LIST (string/= is multi-value-shaped),
    # unlike a plain text variable's bare-string default.
    tags = {"currency": {"id": "ccy-ff", "name": "currency", "display-name": "Currency",
                         "type": "dimension",
                         "dimension": ("field", "report_returns", "currency"),
                         "widget-type": "string/=", "default": [DEFAULT_CURRENCY],
                         "required": True},
            "start_year": START_YEAR_TAG,
            "benchmark": BENCHMARK_TAG,
            "source_ff": {"id": "src-ff", "name": "source_ff",
                          "display-name": "Source", "type": "dimension",
                          "dimension": ("field", "report_returns", "silver_source_id"),
                          "widget-type": "string/=", "default": None}}
    if sql is not None and "{{source_ff}}" not in sql:
        del tags["source_ff"]
    return tags


def _native(sql, tags):
    """A native-SQL dataset_query with template variables."""
    return {"type": "native", "native": {"query": sql, "template-tags": tags}}


# The currency and source clauses of the native returns charts: the
# currency field filter inside each UNION arm, and the source one as an
# optional [[…]] clause, omitted when nothing is selected.
RETURNS_CCY_CLAUSE = "\n     AND {{currency}}"
RETURNS_SOURCE_CLAUSE = "\n     [[AND {{source_ff}}]]"
# A benchmark table's currency, by the {{currency}} field filter, which
# binds report_returns' column and so reaches another table through it.
BENCHMARK_CCY_CLAUSE = ("\n     AND currency IN (SELECT currency FROM report_returns WHERE TRUE"
                        + RETURNS_CCY_CLAUSE + ")")
PICKED_BENCHMARK = "benchmark = {{benchmark}}"


def _returns_source_union(granularity, value_col):
    """A UNION selecting one column from the per-source rows plus the global
    grain relabelled as the toggleable '(all sources)' pseudo-source, for one
    granularity's per-period buckets. Filtered to {{currency}} and to periods
    ending on/after {{start_year}} (0 = all). The Source field filter
    narrows the real sources but never the '(all sources)' line, so the
    global reference stays visible while a subset is selected, and the
    picked benchmark's buckets join them as one more source."""
    return (
        "WITH s AS (\n"
        "  SELECT silver_source_id, currency, end_day, " + value_col + " AS v\n"
        "    FROM report_returns\n"
        "   WHERE grain = 'sources' AND granularity = '" + granularity + "'\n"
        "     AND NOT is_summary" + RETURNS_CCY_CLAUSE + RETURNS_SOURCE_CLAUSE + "\n"
        "  UNION ALL\n"
        f"  SELECT '{ALL_SOURCES_SERIES}', currency, end_day, " + value_col + "\n"
        "    FROM report_returns\n"
        "   WHERE grain = 'global' AND granularity = '" + granularity + "'\n"
        "     AND NOT is_summary" + RETURNS_CCY_CLAUSE + "\n"
        "  UNION ALL\n"
        f"  SELECT '{BENCHMARK_SERIES}', currency, end_day, " + value_col + "\n"
        "    FROM benchmark_returns\n"
        "   WHERE " + PICKED_BENCHMARK + " AND granularity = '" + granularity + "'"
        + BENCHMARK_CCY_CLAUSE + ")\n"
        "SELECT silver_source_id AS source, end_day, v\n"
        "  FROM s\n"
        " WHERE year(epoch_ms(end_day * 1000)) >= {{start_year}}")


def returns_period_sql(granularity):
    """Per-period TWR, one row per (source, period) plus the '(all sources)'
    global line — the pseudo-source that fixes the split-by-source
    inconsistency (every chart shows sources and the global line together).
    The benchmark's rows come last: Metabase draws series in the order their
    rows first appear, so its line lies over the sources'. The source breaks
    a day's ties, so a load keeps each series' place."""
    return ("SELECT source, epoch_ms(end_day * 1000) AS period, v AS twr\n"
            "  FROM (\n" + _returns_source_union(granularity, "twr") + "\n) u\n"
            f" ORDER BY source = '{BENCHMARK_SERIES}', end_day, source")


def returns_growth_sql():
    """Cumulative return since the start of the earliest visible year, per
    source and the '(all sources)' line, derived from the ENGINE's since-<year>
    windowed TWRs — NOT by chaining the per-period buckets. Chaining
    calendar-month/quarter Modified-Dietz returns is unsound here: a flow
    landing between two sparse snapshots poisons that bucket (a mid-month
    deposit with no fresh snapshot reads as a huge loss, then a huge gain next
    period), so a chained index can diverge badly from the true TWR for
    sparse-snapshot sources, even turning a gain into a spurious loss. The
    windowed summaries use the engine's snapshot-aligned chain, so they are
    correct and — being the very figures the scalars and by-source table
    show — the chart agrees with them by construction.

    Since window_from_year=Y is the TWR from Jan 1 Y to today, growth from the
    start of Ymin to the start of Y is (1 + TWR_since_Ymin) / (1 + TWR_Y), and
    the cumulative return is that minus 1. {{start_year}} sets Ymin (the line
    rebases to the chosen start); null windows (degenerate inception) drop out,
    so the line begins where the return is first defined. Annual granularity —
    one point per year — is the price of correctness here; a finer curve would
    need per-month windowed summaries.

    The picked benchmark's line comes from its own windows the same way. Its
    windows all end on its last month, and the ratio cancels that end. A window
    the benchmark's months do not cover from its start stays off the line, so it
    starts on the first year it covers whole, at the '(all sources)' line's
    level that year: the two then read as money from one point."""
    return (
        "WITH w AS (\n"
        "  SELECT silver_source_id AS source, currency, window_from_year AS yr, twr\n"
        "    FROM report_returns\n"
        "   WHERE grain = 'sources' AND granularity = 'total' AND is_summary\n"
        "     AND window_from_year > 0" + RETURNS_CCY_CLAUSE + RETURNS_SOURCE_CLAUSE + "\n"
        "  UNION ALL\n"
        f"  SELECT '{ALL_SOURCES_SERIES}', currency, window_from_year, twr\n"
        "    FROM report_returns\n"
        "   WHERE grain = 'global' AND granularity = 'total' AND is_summary\n"
        "     AND window_from_year > 0" + RETURNS_CCY_CLAUSE + "),\n"
        "f AS (\n"
        "  SELECT source, yr, twr FROM w\n"
        "   WHERE yr >= {{start_year}} AND twr IS NOT NULL),\n"
        "c AS (\n"
        "  SELECT source, yr, first_value(1 + twr) OVER (PARTITION BY source ORDER BY yr)\n"
        "             / (1 + twr) - 1 AS cumulative_return\n"
        "    FROM f),\n"
        "b AS (\n"
        "  SELECT window_from_year AS yr, twr\n"
        "    FROM benchmark_returns\n"
        "   WHERE " + PICKED_BENCHMARK + " AND granularity = 'total'\n"
        "     AND window_from_year > 0 AND window_from_year >= {{start_year}}\n"
        "     AND twr IS NOT NULL AND quality NOT LIKE '%benchmark_start_clipped%'"
        + BENCHMARK_CCY_CLAUSE + "),\n"
        "o AS (\n"
        "  SELECT COALESCE((SELECT cumulative_return FROM c\n"
        f"                    WHERE source = '{ALL_SOURCES_SERIES}' AND yr = (SELECT min(yr) FROM b)), 0) AS level)\n"
        "SELECT source, make_date(yr, 1, 1) AS year, cumulative_return\n"
        "  FROM c\n"
        "UNION ALL\n"
        f"SELECT '{BENCHMARK_SERIES}', make_date(yr, 1, 1),\n"
        "       (1 + o.level) * first_value(1 + twr) OVER (ORDER BY yr) / (1 + twr) - 1\n"
        "  FROM b, o")


def _returns_part(grain, granularity):
    """Filter to one (grain, granularity) partition of report_returns
    (the returns scalars/table use the summary 'total' partition; the
    Start-year picker then selects the window_from_year within it)."""
    return ["and", ["=", _f("grain", "type/Text"), grain],
            ["=", _f("granularity", "type/Text"), granularity]]


def returns_by_source_sql(money):
    """The by-source returns table: each source's returns and quality
    flags, with `money` the absolute money columns (RETURNS_MONEY_COLUMNS)
    too, and the picked benchmark as one more line over the same window.
    Native, for that line: a question over a model has no union."""
    cols = ("twr", "twr_annualized", "mwr", "mwr_annualized")
    money_cols = RETURNS_MONEY_COLUMNS if money else ()
    blanks = "".join(f", NULL AS {c}" for c in money_cols)
    return ("SELECT silver_source_id, entity_label, " + ", ".join(cols)
            + "".join(f", {c}" for c in money_cols) + ", quality\n"
            "  FROM report_returns\n"
            " WHERE grain = 'sources' AND granularity = 'total'\n"
            "   AND window_from_year = {{start_year}}" + RETURNS_CCY_CLAUSE + RETURNS_SOURCE_CLAUSE + "\n"
            "UNION ALL\n"
            "SELECT benchmark, " + _months_compared() + ", twr, twr_annualized, mwr, mwr_annualized" + blanks
            + ", quality\n"
            "  FROM benchmark_returns\n"
            " WHERE " + PICKED_BENCHMARK + " AND granularity = 'total'\n"
            "   AND window_from_year = {{start_year}}" + BENCHMARK_CCY_CLAUSE + "\n"
            " ORDER BY 1")


def _months_compared():
    """A window's days as a reader names them, first month to last. A
    window starts at the end of its first day, so a month end opens the
    month after it. A window past the benchmark's months names its last."""
    first = "strftime(epoch_ms((start_day + 86400) * 1000), '%b %Y')"
    last = "strftime(epoch_ms(end_day * 1000), '%b %Y')"
    return f"CASE WHEN start_day < end_day THEN {first} || ' to ' || {last} ELSE 'through ' || {last} END"


def benchmark_scalar_sql(expression, label):
    """A figure of the picked benchmark over the months it covers of the
    start year's window, which `Returns by source` names. A window its
    months cannot answer has a row with no figure. No benchmark, no row."""
    return (f"SELECT {expression} AS {label}\n"
            "  FROM benchmark_returns\n"
            " WHERE " + PICKED_BENCHMARK + " AND granularity = 'total'\n"
            "   AND window_from_year = {{start_year}}" + BENCHMARK_CCY_CLAUSE)


def benchmark_line_sql(value, *, index=False):
    """A line of benchmark_mwr per side over the start year's window:
    `value` of the household and of the same money in the picked benchmark.
    With `index` each side reads 100 at the window's start, the twin's
    reading, as the amounts hide. No benchmark, no row."""
    metric = f"100 * {value} / first_value({value}) OVER (PARTITION BY series ORDER BY end_day)" if index else value
    return ("SELECT CAST(epoch_ms(end_day * 1000) AS DATE) AS day,\n"
            f"       CASE series WHEN 'portfolio' THEN '{PORTFOLIO_SERIES}' ELSE '{BENCHMARK_SERIES}' END AS series,\n"
            f"       {metric} AS {'indexed_value' if index else value}\n"
            "  FROM benchmark_mwr\n"
            " WHERE " + PICKED_BENCHMARK + " AND window_from_year = {{start_year}}" + BENCHMARK_CCY_CLAUSE + "\n"
            " ORDER BY series, day")


def _series_order(sql, t, series, metric, by, largest_first=False, legend_last=None, rows_last=None):
    """A chart's query with its rows ordered by series, since Metabase
    takes the series in the order their rows first appear. `by` ranks a
    series by its value at the last `t` ("latest", a level that carries
    on) or by its sum over the window ("total", a flow). A stacked chart
    wants the smallest first: the band whose rows appear last lands on
    the axis, so the largest band sits at the bottom and, with the
    legend's default reversal switched off (STACKED_VIZ), the legend
    lists the largest first. A line chart wants `largest_first`, the
    legend's own order, top line first. A series with no value at the
    last `t` ranks last. The series' name breaks a tie on rank, a NULL
    rank included, so every load gives the rows one order. `sql` ends
    without an ORDER BY, and `t`, `series` and `metric` are its column
    names; the result keeps its columns. `legend_last` names a series
    with no rank, whose rows come first, which puts it at the end of a
    stacked chart's legend. `rows_last` names a series whose rows come
    after every other's, NULL ranks too. So a picked benchmark keeps the
    other series' order, and its line lies over theirs. The colours do
    not follow the order where a card names them: a source's comes from
    SOURCE_PALETTE (with_source_colors).

    The rank is an aggregate per series joined back, not a window: the
    DuckDB the Metabase driver bundles fails on a window partitioned by
    a constant over no rows, which is what a picker that contradicts a
    card's own filter leaves."""
    if by == "latest":
        rank = f"max(CASE WHEN {t} = (SELECT max({t}) FROM q) THEN {metric} END)"
    else:
        rank = f"sum({metric})"
    if legend_last:
        rank = f"CASE WHEN {series} <> '{legend_last}' THEN {rank} END"
    order = "series_rank DESC NULLS LAST" if largest_first else "series_rank NULLS FIRST"
    if rows_last:
        order = f"q.{series} = '{rows_last}', {order}"
    return (f"WITH q AS (\n{sql}\n),\n"
            f"ranks AS (SELECT {series} AS series_key, {rank} AS series_rank FROM q GROUP BY 1)\n"
            f"SELECT q.*\n"
            f"  FROM q LEFT JOIN ranks ON ranks.series_key IS NOT DISTINCT FROM q.{series}\n"
            f" ORDER BY {order}, q.{series}, {t}")


# A line chart with one line per series: the legend in the lines' order.
LINES_VIZ = {"legend.is_reversed": False}


# A stacked chart's viz: bands stacked, the legend in the bands' own
# order (Metabase reverses a stacked chart's legend by default). A
# stack of metrics rather than bands is the mirror case: the FIRST
# metric sits at the bottom, so such a card lists its metrics largest
# first and the legend then reads the same way.
STACKED_VIZ = {"stackable.stack_type": "stacked", "legend.is_reversed": False}

# The Cash Flow statement's series: a combo draws its last series as a line
# unless each says what it is, so the sections are bars and the net a line.
NET_CASH_FLOW = "net cash flow"
STATEMENT_SERIES = {"series_settings": {**{s: {"display": "bar"}
                                           for s in ("operating", "investing", "financing", "vehicles")},
                                        NET_CASH_FLOW: {"display": "line", "color": BENCHMARK_COLOR}}}


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


def overview_cards(f):
    """The Wealth Overview's own cards, or on the twin their privacy
    variants. Its other tiles are other dashboards' cards, each with
    that dashboard as its home. The cards read the serving views
    natively, for the reason the spending ones do: the Currency picker
    has to choose a value COLUMN, which only a template variable can.
    Each reads the view at the grain it charts. The year-to-date returns
    read report_returns, a row set per currency, where {{currency}}
    filters rows.

    On the twin, a ratio of sums is scale-free, and both legs see the
    dashboard's filters, so each headline figure reads as a share of the
    selected sources' latest total. The > 0 guard blanks an empty or
    under-water selection rather than rendering inf or a sign-flipped
    share. The holdings time series read as % of the total at the END of
    the selected window — the last charted day, which is today whenever
    the window is open-ended. The nw CTE re-applies both pickers: the
    time filter picks the anchor day, the source filter makes a subset
    rescale to its own anchor total — so the envelope ends at 100 at the
    window's last day and tops 100 wherever it previously peaked higher.
    The anchor day is the latest day of the view the Time range keeps,
    not of the selection; the other pickers then filter that day's rows.
    The > 0 guard blanks a selection whose anchor total is zero or
    negative — dividing would render inf/NaN resp. sign-flipped bands."""
    lat_tags = native_tags("web_sources_latest", HOLDINGS_FILTERS)
    hist_tags = native_tags("web_sources_history", HISTORY_FILTERS)
    cls_tags = native_tags("web_asset_classes_history", HISTORY_FILTERS)
    f = f.using(tail=PRIVACY_DESC if f.privacy else "")

    def latest_scalar(name, col, desc):
        """A headline figure: one value column summed over the selected
        sources' latest holdings, on the twin its share of their total."""
        f.card(name, "scalar", desc,
               f"SELECT sum({_ccy_case(col)})::DOUBLE\n"
               f"       / CASE WHEN sum({_ccy_case('total_value')}) > 0\n"
               f"              THEN sum({_ccy_case('total_value')})::DOUBLE END"
               " * 100 AS share_pct\n"
               "  FROM web_sources_latest" + _native_where(lat_tags) if f.privacy else
               f"SELECT sum({_ccy_case(col)})::DOUBLE AS {col}\n"
               "  FROM web_sources_latest" + _native_where(lat_tags), {}, tags=lat_tags)

    # Net worth = positions + cash by construction, so the three
    # headline figures reconcile exactly. They read the latest
    # holdings, like `wealthdb holdings sources`; the charts below read
    # the daily history, which agrees with them on each day.
    latest_scalar("Net worth", "total_value",
                  "Always 100, the anchor of every other share." + LATEST_DESC if f.privacy else
                  "Cash plus positions across the selected sources." + LATEST_DESC)
    latest_scalar("Positions value", "positions_value",
                  "Positions as a share of net worth." + LATEST_DESC if f.privacy else
                  "Market value of all positions." + LATEST_DESC)
    latest_scalar("Cash balance", "cash_balance",
                  "Cash as a share of net worth." + LATEST_DESC if f.privacy else
                  "Total cash balance." + LATEST_DESC)
    total = _ccy_case("total_value")
    nw_cte = (
        "WITH nw AS (\n"
        f"  SELECT CASE WHEN sum({total}) > 0\n"
        f"              THEN sum({total})::DOUBLE END AS denom\n"
        "    FROM web_sources_history\n"
        "   WHERE as_of_day = (SELECT max(as_of_day) FROM web_sources_history\n"
        "                       WHERE TRUE" + _cl("time_range") + ")"
        + _field_clauses(hist_tags, exclude=("time_range",)) + ")\n")
    if f.privacy:
        sh_where = _native_where(hist_tags, " ")
        f.card("Net worth — monthly trend", "smartscalar",
               "Average daily net worth per month as a share of net worth at the "
               "window's end.",
               nw_cte +
               "SELECT CAST(date_trunc('month', as_of_day) AS TIMESTAMP) AS month,\n"
               f"       sum({total})::DOUBLE / count(DISTINCT as_of_day)\n"
               "           / (SELECT denom FROM nw) * 100 AS avg_pct\n"
               "  FROM web_sources_history" + sh_where + "\n"
               " GROUP BY 1\n ORDER BY 1", {}, tags=hist_tags)
        f.card("Net worth over time", "area",
               "Daily net worth by source, as a share of the total at the window's "
               "end.",
               _series_order(nw_cte +
                             "SELECT as_of_day, silver_source_id,\n"
                             f"       sum({total})::DOUBLE / (SELECT denom FROM nw)"
                             " * 100 AS total_value_pct\n"
                             "  FROM web_sources_history" + sh_where + "\n"
                             " GROUP BY 1, 2", "as_of_day", "silver_source_id", "total_value_pct", "latest"),
               {**_series_viz("as_of_day", "silver_source_id", "total_value_pct"), **STACKED_VIZ},
               tags=hist_tags)
    else:
        f.card("Net worth — monthly trend", "smartscalar",
               "Average daily net worth of the latest month, with the change "
               "from the month before.",
               "SELECT CAST(date_trunc('month', as_of_day) AS TIMESTAMP) AS month,\n"
               f"       sum({_ccy_case('total_value')})::DOUBLE"
               " / count(DISTINCT as_of_day) AS net_worth\n"
               "  FROM web_sources_history" + _native_where(hist_tags) +
               "\n GROUP BY 1\n ORDER BY 1",
               {}, tags=hist_tags)
        f.card("Net worth over time", "area",
               "Daily net worth since the first snapshot, stacked by source. "
               "Values carry forward between snapshots.",
               _series_order("SELECT as_of_day, silver_source_id,\n"
                             f"       sum({_ccy_case('total_value')})::DOUBLE AS net_worth\n"
                             "  FROM web_sources_history" + _native_where(hist_tags) +
                             "\n GROUP BY 1, 2", "as_of_day", "silver_source_id", "net_worth", "latest"),
               {**_series_viz("as_of_day", "silver_source_id", "net_worth"), **STACKED_VIZ}, tags=hist_tags)
    # Shares, not money, so the twin shows the card as it is. A click
    # opens Allocation as of the day clicked.
    f.card("Asset class mix over time", "area",
           "Each day's net worth by asset class, as shares that stack to "
           "100. A class in net debt reads below zero.",
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
            **_share_viz("share_pct")},
           tags=cls_tags, exempt=True, home="Allocation", cell_clicks=(Link("as_of_day", "as_of_day"),))
    # The returns of the current year. A click opens the Returns at the
    # start year of the figure.
    for name, method, col in (("Return year to date (TWR)", "time-weighted", "twr"),
                              ("Return year to date (MWR)", "money-weighted", "mwr")):
        f.card(name, "scalar",
               f"Whole-portfolio {method} return since 1 January of the latest year "
               "the returns cover.",
               "WITH g AS (\n"
               f"  SELECT window_from_year, {col}\n"
               "    FROM report_returns\n"
               "   WHERE grain = 'global' AND granularity = 'total'\n"
               "     AND currency = {{currency}})\n"
               f"SELECT {col}, window_from_year AS start_year\n"
               "  FROM g\n"
               " WHERE window_from_year = (SELECT max(window_from_year) FROM g)",
               {**_percent_viz(col), "scalar.field": col},
               tags={"currency": currency_tag()}, exempt=True, home="Returns",
               cell_clicks=(Link("start_year", "start_year"),))


def allocation_cards(f):
    """The Allocation dashboard's cards, or on the twin their privacy
    variants. They read the daily-history views so the dashboard can
    show holdings as of any chosen day, the history's last day for a
    later one; the dashboard supplies the required as-of day. On the
    twin each breakdown's bucket is a % of the summed total over the
    same filtered rows, so the buckets total 100 across the selected
    sources as of the chosen day (liability buckets read negative; the
    sum <> 0 guard blanks a degenerate zero day)."""
    f = f.using(tail=PRIVACY_DESC if f.privacy else "")
    metric = "value_pct" if f.privacy else "value"

    def breakdown(name, view, dim, col, display, desc, viz=None, benchmark=None):
        """`col` summed by `dim` over the as-of day's rows of serving view
        `view`, largest first, and with `benchmark` that dimension of the
        picked benchmark beside it."""
        tags = asof_tags(view, HOLDINGS_FILTERS)
        if benchmark:
            tags["benchmark"] = BENCHMARK_TAG
        f.card(name, display, desc,
               _breakdown_sql(view, dim, col, tags, share=f.privacy, benchmark=benchmark),
               viz if viz is not None else
               {"graph.dimensions": [dim], "graph.metrics": [metric],
                **(_beside_viz(dim) if benchmark else {})}, tags=tags)

    twin = f.privacy
    breakdown("Allocation by asset class", "web_asset_classes_history", "asset_class", "value", "row",
              "Asset-class shares (%) of net worth as of the chosen day, cash "
              "included. Debts read negative." if twin else
              "Value by asset class, cash included, as of the chosen day. The "
              "bars sum to net worth, debts below zero. A benchmark's bar is the same "
              "total in its mix.", benchmark="asset_class")
    breakdown("Allocation by vehicle", "web_vehicles_history", "vehicle", "value", "row",
              "Vehicle shares (%) of net worth as of the chosen day. Cash shows "
              "as demand_deposit." if twin else
              "Value by vehicle (stock, etf, fund, bond and others) as of the "
              "chosen day. Cash shows as demand_deposit.")
    breakdown("Allocation by currency", "web_currencies_history", "currency", "value", "row",
              "Native-currency shares (%) of net worth as of the chosen day, "
              "cash included. A forward counts at its mark; Exposure shows its "
              "legs." if twin else
              "Value by native currency, cash included, as of the chosen day. "
              "A forward counts at its mark; Exposure shows its legs. The "
              "Currency picker only sets the valuation currency. A benchmark's bar "
              "is its funds' currencies.", benchmark="fund_currency")
    breakdown("Value by tax wrapper", "web_accounts_history", "tax_wrapper", "total_value", "pie",
              "Tax-wrapper shares (%) of account value, cash included, as of the "
              "chosen day." if twin else
              "Account value, cash included, by tax wrapper as of the chosen day.",
              {"pie.dimension": "tax_wrapper", "pie.metric": metric})
    breakdown("Value by management style", "web_accounts_history", "management_style", "total_value", "row",
              "Management-style shares (%) of account value, cash included, as of "
              "the chosen day." if twin else
              "Account value, cash included, by management style as of the "
              "chosen day.")
    # A fixed hundred: the inline vehicle filter and the dashboard's
    # pickers narrow the list rather than a K picker sizing it.
    tags = asof_tags("web_positions_history", POSITION_FILTERS)
    value = _ccy_case("value")
    if twin:
        # Each position's share of the selection's TOTAL value at the
        # day, cash included: the selected sources, tax wrappers and
        # asset classes. The inline vehicle picker narrows the list but
        # not the denominator, so a position's share reads the same
        # however the list is narrowed.
        f.card("Top 100 positions", "table",
               "The hundred largest positions, as shares of the day's value. Each "
               "account's cash in a currency is a row." + POSITION_SIZE_NOTE,
               "WITH tot AS (\n"
               f"  SELECT sum({value})::DOUBLE AS t\n"
               "    FROM web_positions_history"
               + _asof_where(tags, exclude=("vehicle",)) + "),\n"
               "p AS (\n"
               "  SELECT symbol, name, asset_class, vehicle,"
               f" sum({value})::DOUBLE AS v\n"
               "    FROM web_positions_history" + _asof_where(tags) + "\n"
               "   GROUP BY 1, 2, 3, 4, line_key)\n"
               f"SELECT p.symbol, {twin_name('p.name')} AS name, p.asset_class, p.vehicle, z.size,\n"
               "       p.v / (SELECT CASE WHEN t <> 0 THEN t END FROM tot)"
               " * 100 AS value_pct\n"
               "  FROM p" + POSITION_SIZE_JOIN + "\n ORDER BY value_pct DESC\n LIMIT 100",
               {}, tags=tags)
    else:
        f.card("Top 100 positions", "table",
               "The hundred largest positions by value as of the chosen day, "
               "across accounts. Each account's cash in a currency is a position "
               "of its own." + POSITION_SIZE_NOTE,
               "WITH p AS (\n"
               "  SELECT symbol, name, asset_class, vehicle,\n"
               f"         sum({value})::DOUBLE AS value\n"
               "    FROM web_positions_history" + _asof_where(tags) +
               "\n   GROUP BY 1, 2, 3, 4, line_key\n   ORDER BY 5 DESC\n   LIMIT 100)\n"
               "SELECT p.symbol, p.name, p.asset_class, p.vehicle, z.size, p.value\n"
               "  FROM p" + POSITION_SIZE_JOIN + "\n ORDER BY p.value DESC",
               {}, tags=tags)
    issuer_cards(f)


def spending_cards(f):
    """The Spending dashboard's cards, or on the twin their privacy
    variants. `value` carries gold's canonical sign (spend negative,
    refunds positive), so every card charts its negation, `net_spend`,
    and a month's outflow reads as a positive bar.

    The tiles read the serving view natively, for the Currency picker:
    the long-format model carries a row per (line, reporting currency),
    so an MBQL tile over it is right only while a row filter holds it to
    one — which the dashboard supplies and nothing else does. Opened on
    its own, such a tile would sum every currency, silently and
    plausibly. A template VARIABLE is substituted by the picker rather
    than ANDed with the tile's own filters, and {{currency}} defaults to
    the configured default currency, so a native tile reads the
    dashboard's choice on the dashboard and the default anywhere else.
    What it costs is MBQL drill-through: a native result has no "see
    these records". Two tiles stay MBQL: the uncategorized share, a
    share of rows over the _pct model, and the merchant ranking, which
    aggregates, so a per-currency column collapses its duplicate rows
    (ccy_spend). The card-balances chart reads web_card_balances_history,
    card balances being a different grain (per account per day, carried
    forward), with tags of its own.

    The twin's figures are shares and no card renders a counterparty.
    Two denominators, both recomputed in-query with the dashboard's
    pickers applied, so a narrowed selection rescales to itself. The
    breakdowns, the merchant and account lists and the largest lines
    divide by the window's own total net spend (they sum to 100); the
    trend and the monthly bands divide by the window's peak month (the
    peak reads 100). The card-balances chart takes the same shape with
    the window's deepest total owed, so its peak reads 100. Guards blank
    a degenerate window (no positive month, or a total of zero) rather
    than render inf / sign-flipped shares.

    REDACTION on top of normalization, the way the Returns twin does it:
    the fix for a leaking column is to drop it, not to disguise it. No
    twin card projects `merchant_name`, `display_name` or
    `account_external_id` — the merchant list keeps its shape by ranking
    instead, so `merchant_name` appears only as a GROUP BY key and in
    the ranking's fixed WHERE predicate, never in a projection. Nor does
    any twin card FILTER on an account: a field filter renders as a
    dropdown of its column's values, so an account filter would print
    the labels the projections just dropped (PRIVACY_SPEND_FILTERS).
    `merchant_name` is safe as a GROUP BY key and as a fixed predicate
    because neither shows anything; a filter widget does."""
    twin = f.privacy
    tags = native_tags("web_spending", PRIVACY_SPEND_FILTERS if twin else SPEND_FILTERS)
    where = _native_where(tags)
    val = f"sum({_ccy_case('value', neg=True)})::DOUBLE"
    f = f.using(tags=tags, tail=PRIVACY_DESC if twin else "")
    spend = f.card

    def month_cte(extra=""):
        return _month_cte("web_spending", where, val, extra)

    # The window-total denominator of a twin card: every bucket over the
    # summed total of the same filtered rows, so the buckets total 100.
    # Signed, so a net-refunded bucket reads negative — and the <> 0
    # guard blanks a window whose spend and refunds cancel exactly.
    def total_cte(select, group):
        """The `r` CTE: one row per bucket, with the value the shares
        divide by."""
        return ("WITH r AS (\n"
                f"  SELECT {select}{val} AS v\n"
                "    FROM web_spending" + where
                + f"\n   GROUP BY {group})\n")
    share_of_total = ("v / (SELECT CASE WHEN sum(v) <> 0 THEN sum(v) END"
                      " FROM r) * 100")

    if twin:
        spend("Spend — monthly trend", "smartscalar",
            "Net spend per month as a share of the biggest month, with the "
            "change from the month before.",
            month_cte() + "   GROUP BY 1),\n" + PEAK_CTE +
            f"SELECT month, {PEAK_DIV} AS spend_pct\n  FROM m\n ORDER BY 1", {}, home="Spending")
        # Always 100 — the window total the other percentages are
        # relative to. A ratio of sums over the _pct model, so it is
        # scale-free, follows every picker, and its "see these records"
        # drill-through opens a model with no merchant column.
        net_spend = {"net_spend": ["*", _dec("value"), -1]}
        total = ["sum", ["expression", "net_spend"]]
        spend("Net spend", "scalar",
            "Always 100, the anchor of every other share.",
            _mbql("report_spending_pct",
                  {"expressions": net_spend,
                   "aggregation": [["*", ["/", total, total], 100]]}),
            {}, home="Spending")
    else:
        spend("Spend — monthly trend",
            "smartscalar",
            "Net spend in the window's latest month, with the change from the "
            "month before.",
            f"SELECT {OCCURRED_MONTH},\n       {val} AS net_spend\n"
            "  FROM web_spending" + where + "\n GROUP BY 1\n ORDER BY 1",
            {}, home="Spending")
        spend("Net spend", "scalar",
            "Purchases minus refunds over the window, across the selected "
            "accounts and categories.",
            f"SELECT {val} AS net_spend\n"
            "  FROM web_spending" + where,
            {}, home="Spending")
    # Shows a percentage, so the twin shows it as it is. A scalar's
    # "see these records" drill-through opens the underlying model, so
    # it runs over the _pct model — the one without the merchant column
    # — even though the figure itself is a scale-free row count.
    spend("Uncategorized share", "scalar",
        f"Share of the window's spending lines labelled '{UNCATEGORIZED}', "
        "the backlog `wealthdb categorize` works through.",
        _mbql("report_spending_pct",
              {"aggregation": [["share",
                   ["=", _f("spend_primary", "type/Text"), UNCATEGORIZED]]]}),
        _percent_viz("share"), exempt=True)
    # The axes are PINNED, and this is the card that most needs it.
    # Left to infer them, Metabase puts the dimension with the most
    # distinct values on the x-axis — and there are more categories
    # than months in any window worth charting, so the card drew its
    # own transpose: a bar per CATEGORY stacked by month, under a
    # band of rotated category names deep enough to leave the bars a
    # sliver. A card called "by month" charts months. On the base card
    # only the dimensions are pinned; the metric is left to default,
    # since naming it would be guessing at what Metabase calls an
    # aggregation column.
    #
    # Stacked areas rather than bars: months are a continuous axis,
    # so the categories read as bands whose thickness moves across
    # the window, and the envelope is the window's own shape. A
    # month whose refunds beat its purchases dips its band below the
    # line.
    if twin:
        spend("Spending by month", "area",
            "Net spend per month by primary category, as a share of the window's "
            "biggest month.",
            _series_order(month_cte("spend_primary_label AS category,\n         ")
                     + "   GROUP BY 1, 2),\n" + PEAK_CTE +
                     f"SELECT month, category, {PEAK_DIV} AS spend_pct\n"
                     "  FROM m", "month", "category", "spend_pct", "total"),
            {"graph.dimensions": ["month", "category"],
             "graph.metrics": ["spend_pct"], **STACKED_VIZ})
    else:
        spend("Spending by month", "area",
            "Net spend per month, stacked by primary category.",
            _series_order(f"SELECT {OCCURRED_MONTH},\n       spend_primary_label AS category,\n"
                     f"       {val} AS net_spend\n"
                     "  FROM web_spending" + where + "\n GROUP BY 1, 2",
                     "month", "category", "net_spend", "total"),
            {"graph.dimensions": ["month", "category"], **STACKED_VIZ})
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
    #
    # The twin's rings suit a share even better than an amount, since
    # the slices are already the percentages the ring would compute.
    # Neither carries a LIMIT: a limited ring renormalizes onto what it
    # drew and would print shares disagreeing with its own values, where
    # the threshold folds the tail into a wedge and keeps the whole
    # window on the ring. No figure in their hole. The shares are of a
    # SIGNED total, so a category whose refunds beat its purchases
    # carries a negative one and a ring cannot draw it — leaving the
    # hole to sum the rest and read just over 100. The values are right
    # and total exactly 100; it is the hole that cannot represent them,
    # and a hole reading ~100 on a breakdown already expressed as shares
    # says nothing the description does not.
    for name, col, threshold, what, twin_what in (
            ("Spending by category", "spend_primary_label", 0,
             "Net spend by primary category. A category whose refunds beat "
             "its purchases has no slice and is not in the total.",
             "Primary-category shares (%) of net spend. A category whose "
             "refunds beat its purchases has no slice."),
            ("Spending by subcategory", "spend_label", 1.5,
             "Net spend by detailed category, the long tail in one slice. A "
             "net-refunded category is not in the total.",
             "Detailed-category shares (%) of net spend, the long tail in one "
             "slice. Net-refunded categories have no slice.")):
        if twin:
            spend(name, "pie", twin_what,
                total_cte(f"{col} AS category,\n         ", "1") +
                f"SELECT category, {share_of_total} AS spend_pct\n"
                "  FROM r\n ORDER BY 2 DESC",
                _donut(threshold=threshold, total=False))
        else:
            spend(name, "pie", what,
                f"SELECT {col} AS category,\n       {val} AS net_spend\n"
                "  FROM web_spending" + where + "\n GROUP BY 1\n ORDER BY 2 DESC",
                _donut(threshold=threshold))
    # Ranks merchants only. A line resolved to a delta — a gift, a
    # bill on a card not itemised, cash out of an ATM — is not a
    # merchant transaction, and one kind of delta line carries a
    # name: a card bill labelled with the issuer it was paid to.
    # A blank merchant therefore does not mark a delta, so the
    # ranking excludes the delta CATEGORIES outright — a delta is
    # primary-level, so `spend_primary <> spend_detailed` reads the
    # dimension's own marker rather than restating a list of values
    # — and still needs a name to rank by. A line nothing resolved
    # is kept out by its two category columns being equal at
    # '(uncategorized)'. A line the merchant store never named shows
    # its own signature, so the rows are at the signature's grain —
    # a chain appears once per branch signature, and a fold that is
    # only the bank's own booking tag ranks under that tag, since
    # the gates that keep such a fold from the model fence the
    # merchant store and not this column. Only the ranking filters:
    # the transaction list below keeps every line, since a line is a
    # line.
    #
    # The twin's merchants are ranked and unnamed: merchant_name is a
    # GROUP BY key and a WHERE predicate only, so the counterparty
    # decides the rows without ever reaching a column. The list still
    # answers the question the money tile answers — how concentrated
    # the spending is. Its shares are of the window's whole net spend,
    # the anchor the twin's scalar reads as 100, so the total is summed
    # apart from the ranked rows.
    if twin:
        spend("Top 50 merchants", "table",
            "Top fifty merchants, unnamed, as shares of net spend. A card "
            "issuer's bill is outside the ranking.",
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
    else:
        spend("Top 50 merchants", "table",
            f"Top fifty merchants by net spend, ranked in {DEFAULT_CURRENCY}. Delta lines, a card "
            "issuer's bill among them, and lines nothing resolved are outside the ranking.",
            _mbql("report_spending",
                  {"aggregation": [ccy_spend(c) for c in REPORTING_CURRENCIES],
                   "filter": ["and",
                              ["not-null", _f("merchant_name", "type/Text")],
                              ["!=", _f("spend_primary", "type/Text"),
                                     _f("spend_detailed", "type/Text")]],
                   "breakout": [_f("merchant_name", "type/Text")],
                   "order-by": [["desc", ["aggregation",
                                 REPORTING_CURRENCIES.index(DEFAULT_CURRENCY)]]],
                   "limit": 50}),
            {})
    if twin:
        # The label is dropped, and the rows regroup onto the two
        # dimensions the privacy dashboards already show — the source and
        # the account kind — concatenated into one bar label.
        spend("Spend by account", "row",
            "Net spend shares (%) by source and account kind, with account labels "
            "redacted.",
            total_cte("silver_source_id || ' / '\n             || COALESCE("
                      "account_kind, 'unknown') AS account_group,\n         ", "1") +
            f"SELECT account_group, {share_of_total} AS spend_pct\n"
            "  FROM r\n ORDER BY 2 DESC",
            {"graph.dimensions": ["account_group"],
             "graph.metrics": ["spend_pct"]})
    else:
        spend("Spend by account", "row",
            "Net spend by account over the window, each labelled with its "
            "name, source and kind.",
            f"SELECT account_label AS account,\n       {val} AS net_spend\n"
            "  FROM web_spending" + where + "\n GROUP BY 1\n ORDER BY 2 DESC",
            {})
    # Read the way an issuer states a card: what is OWED, as a
    # positive figure. Gold stores the same balance negative — a card
    # is a liability, the margin-debit precedent — and keeps doing so
    # everywhere else, the CLI included; the sign is flipped in this
    # projection and nowhere else, so the chart matches the statement
    # a reader compares it against. The twin's has no category
    # dimension and a denominator of its own — the deepest total owed
    # within the window, so the peak reads 100 and a paid-off card
    # returns to 0 — and splits by source rather than by account.
    bal_tags = native_tags("web_card_balances_history",
                           PRIVACY_CARD_BALANCE_FILTERS if twin else CARD_BALANCE_FILTERS)
    if twin:
        spend("Card balances over time", "line",
            "What the cards owed each day by source, as a share of the window's "
            "deepest total owed.",
            _series_order(
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
                largest_first=True),
            {**_series_viz("as_of_day", "silver_source_id", "balance_pct"), **LINES_VIZ}, tags=bal_tags)
    else:
        spend("Card balances over time", "line",
            "What each credit card owed on every day of the window, carried "
            "forward between statements. A paid-off card returns to zero.",
            _series_order(
                "SELECT as_of_day, account_label,\n"
                f"       sum({_ccy_case('balance', neg=True)}) AS owed\n"
                "  FROM web_card_balances_history"
                + _native_where(bal_tags, " ") + "\n"
                " GROUP BY 1, 2", "as_of_day", "account_label", "owed", "latest",
                largest_first=True),
            {**_series_viz("as_of_day", "account_label", "owed"), **LINES_VIZ}, tags=bal_tags)
    # The one tile that lists LINES rather than grouping them: over
    # the long model it would get each line once per reporting
    # currency, with no aggregation to fold them back. The twin drops
    # both counterparty columns: what is left is when, from which
    # source, and what it was categorised as — each as a share of the
    # window.
    if twin:
        spend("Largest transactions", "table",
            "The fifty largest spending lines as shares of net spend, with "
            "merchant and account redacted.",
            "WITH r AS (\n"
            "  SELECT occurred_at, silver_source_id, spend_primary_label, spend_label,\n"
            f"         ({_ccy_case('value', neg=True)})::DOUBLE AS v\n"
            "    FROM web_spending" + where + "),\n"
            "t AS (SELECT CASE WHEN sum(v) <> 0 THEN sum(v) END AS total FROM r)\n"
            "SELECT occurred_at, silver_source_id, spend_primary_label, spend_label,\n"
            "       v / (SELECT total FROM t) * 100 AS spend_pct\n"
            "  FROM r\n ORDER BY v DESC\n LIMIT 50", {})
    else:
        spend("Largest transactions", "table",
            "The fifty largest spending lines of the window, with merchant, "
            "account and categories. A refund sorts last.",
            "SELECT occurred_at,\n       account_label AS account,\n"
            "       merchant_name,\n       spend_primary_label AS category,\n"
            "       spend_label AS subcategory,\n"
            f"       {_ccy_case('value', neg=True)} AS net_spend\n"
            "  FROM web_spending" + where + "\n ORDER BY net_spend DESC\n LIMIT 50",
            {})
    fee_cards(f)


def ccy_spend(ccy):
    """Net spend in ONE reporting currency, as a named column.

    The long model carries a row per (line, currency), so a card that
    wants them all at once cannot take the Currency picker — a row
    filter would empty every column it does not select. Each column
    instead carries its own currency predicate, which makes the card
    right on the dashboard and right opened standalone, where no picker
    reaches it."""
    return ["aggregation-options",
            ["sum-where", ["*", _dec("value"), -1],
             ["=", _f("currency", "type/Text"), ccy]],
            {"name": ccy, "display-name": ccy}]


def income_cards(f):
    """The Income dashboard's cards, or on the twin their privacy
    variants. Every income line's tile is native over web_income, for
    the reason the spending ones are: the serving view carries a row per
    (line, reporting currency), and a required {{currency}} variable
    picks the column rather than filtering rows. Two tiles the Spending
    dashboard has are deliberately absent — there is no second ring, the
    income taxonomy having one vendored primary and so no subcategory
    level worth one, and no balance-history chart, nothing on the income
    side being a liability. `neg` is False: gold stores a receipt
    positive. The cards of received and expected income follow
    (income_projected_cards).

    The twin's figures are shares, and no card renders a payer or an
    account. The denominators mirror the spending twin's, and for the
    same reason: the breakdowns and the lists divide by the window's own
    net income (they sum to 100), the trend by the window's peak month
    (the peak reads 100). A window with no positive month, or a total of
    zero, blanks rather than rendering inf or a sign-flipped share.
    REDACTION on top of normalization, as everywhere else in the twin:
    the fix for a leaking column is to drop it. No twin card projects
    `payer_name`, `display_name` or `account_external_id` — the payer
    list keeps its shape by RANKING instead, so `payer_name` is a GROUP
    BY key and never a projection. Nor does any twin card filter on an
    account, a field filter being a dropdown of its column's values
    (PRIVACY_INCOME_FILTERS)."""
    twin = f.privacy
    filters = PRIVACY_INCOME_FILTERS if twin else INCOME_FILTERS
    tags = native_tags("web_income", filters)
    where = _native_where(tags)
    val = f"sum({_ccy_case('value')})::DOUBLE"
    income = f.using(tags=tags, tail=PRIVACY_DESC if twin else "").card
    total_cte = ("t AS (SELECT nullif(sum(v), 0) AS total FROM m)\n")

    if twin:
        income("Income — monthly trend", "smartscalar",
            "The latest month's net income as a share of the window's biggest month, with the change.",
            _month_cte("web_income", where, val) + "   GROUP BY 1),\n" + PEAK_CTE +
            f"SELECT month, {PEAK_DIV} AS income_pct\n  FROM m\n ORDER BY 1", {})
        # The anchor every other percentage here is relative to. Native
        # like the rest of this twin — there is no income _pct model,
        # because no income tile is MBQL and none needs a drill-through
        # target.
        income("Net income", "scalar",
            "Always 100, the window's net income as a share of itself.",
            "WITH m AS (\n"
            f"  SELECT {val} AS v FROM web_income" + where + ")\n"
            "SELECT v / nullif(v, 0) * 100 AS income_pct FROM m",
            {})
    else:
        income("Income — monthly trend",
            "smartscalar",
            "Net income in the window's latest month, with the change from the month before.",
            f"SELECT {OCCURRED_MONTH},\n       {val} AS net_income\n"
            "  FROM web_income" + where + "\n GROUP BY 1\n ORDER BY 1",
            {})
        income("Net income", "scalar",
            "Every income line over the window, receipts less reversals, gross as booked.",
            f"SELECT {val} AS net_income\n"
            "  FROM web_income" + where,
            {})
    # The data-quality canary. A percentage, so the FIGURE needs no
    # twin — but this is a native card, and a native card carries its
    # own field-filter tags wherever it is opened, one of which is an
    # `account` filter, which renders as a dropdown of account labels.
    # So the twin builds its own over PRIVACY_INCOME_FILTERS like every
    # other card on its dashboard, and the card is not exempt.
    # The one income tile that does NOT read {{currency}}: it is a
    # share of ROWS, scale-free, and the same in every currency. It
    # therefore gets tags without the currency variable rather than
    # declaring a required one it never interpolates.
    share_tags = view_tags("web_income", filters)
    income("Uncategorized income share", "scalar",
        IN_UNCATEGORIZED_DESC,
        IN_UNCATEGORIZED_SQL + _native_where(share_tags),
        _percent_viz("uncategorized_share"), tags=share_tags)
    # No slice threshold: the income vocabulary is small enough to
    # name in full, unlike the spending one, so nothing is folded
    # into an "other" wedge a reader cannot open. The ring rule
    # still applies — a net-negative type has no slice. The twin's ring
    # carries no figure in its hole: a total of shares is 100 by
    # construction and says nothing, and the hole is where the base
    # ring puts the money the twin exists not to show.
    if twin:
        income("Income by type", "pie",
            "The window's net income by type, each as a share of it.",
            "WITH m AS (\n"
            f"  SELECT income_label AS type,\n         {val} AS v\n"
            "    FROM web_income" + where + "\n   GROUP BY 1),\n" + total_cte +
            "SELECT type, v / (SELECT total FROM t) * 100 AS income_pct\n"
            "  FROM m\n ORDER BY 2 DESC",
            _donut(threshold=0, total=False))
    else:
        income("Income by type", "pie",
            "Net income by type over the window, the total in the middle. "
            "A type whose reversals beat its receipts has no slice.",
            f"SELECT income_label AS type,\n       {val} AS net_income\n"
            "  FROM web_income" + where + "\n GROUP BY 1\n ORDER BY 2 DESC",
            _donut(threshold=0, total=True))
    # The only payer ranking there is: the CLI carries none, as it
    # carries no merchant ranking. Payers with no name — the delta
    # lines — are excluded rather than grouped into a blank slice. The
    # twin's are ranked and unnamed: the shape of "how concentrated is
    # this household's income" without naming anyone it comes from.
    if twin:
        income("Top 50 payers", "table",
            "The fifty payers the most income came from, ranked and unnamed, as shares of net income.",
            "WITH m AS (\n"
            f"  SELECT payer_name, {val} AS v\n"
            "    FROM web_income" + where +
            "\n     AND payer_name IS NOT NULL\n   GROUP BY 1),\n" + total_cte +
            "SELECT row_number() OVER (ORDER BY v DESC) AS rank,\n"
            "       v / (SELECT total FROM t) * 100 AS income_pct\n"
            "  FROM m\n ORDER BY 1\n LIMIT 50",
            {})
        income("Largest receipts", "table",
            "The fifty largest income lines as shares of net income, with the type but no payer or account.",
            "WITH m AS (\n"
            "  SELECT occurred_at, income_label AS type,\n"
            f"         {_ccy_case('value')} AS v\n"
            "    FROM web_income" + where + "),\n"
            "t AS (SELECT nullif(sum(v), 0) AS total FROM m)\n"
            "SELECT occurred_at, type,\n"
            "       v / (SELECT total FROM t) * 100 AS income_pct\n"
            "  FROM m\n ORDER BY v DESC\n LIMIT 50",
            {})
    else:
        income("Top 50 payers", "table",
            "The fifty payers the most income came from over the window. "
            "Lines with no payer to name are left out.",
            f"SELECT payer_name,\n       {val} AS net_income\n"
            "  FROM web_income" + where +
            "\n   AND payer_name IS NOT NULL\n GROUP BY 1\n ORDER BY 2 DESC\n LIMIT 50",
            {})
        income("Largest receipts", "table",
            "The fifty largest income lines of the window, with payer, account and type.",
            "SELECT occurred_at,\n       account_label AS account,\n"
            "       payer_name,\n       income_label AS type,\n"
            f"       {_ccy_case('value')} AS net_income\n"
            "  FROM web_income" + where + "\n ORDER BY net_income DESC\n LIMIT 50",
            {})
    income_projected_cards(f)


def cashflow_cards(f):
    """The Cash Flow dashboard's cards, or on the twin their privacy
    variants. Every tile of the statement is native over web_cashflow,
    for the reason the other two families' are — a row per (line,
    reporting currency), and a required {{currency}} variable that picks
    the column rather than filtering rows — plus one of its own:
    {{investing}}, the display grain of the investing section. It is a
    VARIABLE and not a field filter because it changes how rows are
    GROUPED rather than which rows are selected — `whole` nets the
    section into one Investments node, `class` opens it into one node
    per asset class — and a field filter can only narrow a population.

    There is no account tile and no account picker. The household
    boundary is what separates the household's cash flow from its
    vehicles', and a per-account view would ask a question this
    statement does not answer: a wire between two of the household's
    own accounts is invisible here by construction.

    THE SECTION PICKER REACHES ONLY THE CARDS IT MEANS SOMETHING ON. A
    card that does not declare the {{section}} tag cannot be bound to
    the picker, so the scope is expressed once, by the maker a card is
    made with (`whole`), rather than as a rule a later card has to
    remember; its SQL uses `where_nosec`, since a tag a card does not
    declare cannot appear in its SQL. The headline figures and the
    diagram decline it: a statement whose sections have been narrowed
    to one is not a statement, its savings rate and yield share have
    lost a leg of their own ratio, and a one-section diagram draws a
    Cash node that absorbs the whole section rather than the residual it
    names (docs/CASHFLOW.md §9). The twin's tiles answer the same
    pickers card for card: a twin tile stands in for a base tile, or a
    reader comparing the two would find one narrowed and the other not.

    The twin is NORMALISATION ALONE, almost. The diagram's nodes are
    vocabulary — no node is ever a merchant, a payer, an account or an
    instrument — so with the hub at 100 there is nothing left to hide in
    it, and the twin's Sankey is the same edge list divided through.
    That is the property §5 of docs/CASHFLOW.md exists to guarantee, and
    the assertion in web/test_provision.py is what holds it. ONE card
    does redact, and for the reason every twin card redacts: the fix for
    a leaking column is to drop it. `Largest flows` names the account a
    line moved through and, on an operating line, the payer or merchant
    behind it. The twin's version ranks instead and projects neither.
    THE HUB, not a total, is the denominator, and it is recomputed
    in-query so a narrowed selection rescales to itself. It is the sum of
    the positive nets at the level drawn — which equals the sum of the
    negative ones, because the statement sums to zero once cash is a
    node — so it moves with the Investing picker exactly as the base
    dashboard's diagram does.

    The projection's cards and the bridge's follow
    (cashflow_plan_cards, bridge_cards)."""
    twin = f.privacy
    tags = {**native_tags("web_cashflow", CASHFLOW_FILTERS), "investing": INVESTING_TAG}
    tags_nosec = {k: v for k, v in tags.items() if k != "section"}
    where, where_nosec = _native_where(tags), _native_where(tags_nosec)
    val = f"sum({_ccy_case('value')})::DOUBLE"
    tail = PRIVACY_DESC if twin else ""
    flow = f.using(tags=tags, tail=tail).card
    whole = f.using(tags=tags_nosec, tail=SECTION_DECLINED_DESC + tail).card
    sankey_viz = {"sankey.source": "source", "sankey.target": "target",
                  "sankey.value": "value",
                  # Metabase defaults this to "left", which lays a node out
                  # at the depth its INCOMING edge puts it at. A class with
                  # no leaves then sits on the middle level as a dead end
                  # beside the classes that do have leaves, and the diagram
                  # reads as though it were one. "justify" is left alignment
                  # with one change: a node with no outgoing edge moves to
                  # the last level, where every other ending sits.
                  "sankey.node_align": "justify"}

    if twin:
        # The hub, built by the same helper the diagram uses, so a scalar
        # and the diagram beside it cannot divide by two different
        # numbers. A tile that declines the Section picker divides by a
        # hub that declines it too, or the numerator and the denominator
        # would answer different filters.
        hub_cte = _cashflow_atoms_cte(where, val) + "\n"
        hub_cte_nosec = _cashflow_atoms_cte(where_nosec, val) + "\n"

        # The statement's bars are signed both ways, so a month's size is
        # its gross movement rather than PEAK_CTE's positive sum.
        def gross_peak(rows="m"):
            return ("p AS (SELECT max(t) AS peak FROM"
                    f" (SELECT sum(abs(v)) AS t FROM {rows} GROUP BY month))\n")

        def month_cte(extra="", filt="", sign=""):
            # `sign` is built in rather than applied to the finished SQL:
            # a post-hoc string replacement would depend on this
            # function's own indentation, and a reflow here would silently
            # turn a card's bars negative with every shape test still
            # green.
            v = f"{sign}({val})" if sign else val
            return _month_cte("web_cashflow", where + filt, v, extra)

        # `neg` is what keeps each twin scalar reading the same way round
        # as the base tile it stands in for: gold stores an outflow
        # negative and the base "Operating out" prints it as a positive
        # magnitude, so its share has to be one too.
        for name, section, neg, label in (
                ("Net cash flow", None, False, "was kept"),
                ("Operating in", "operating_in", False, "came in"),
                ("Operating out", "operating_out", True, "went out")):
            filt = "" if section is None else f"\n   AND section = '{section}'"
            expr = f"sum({_ccy_case('value', neg=neg)})::DOUBLE"
            whole(name, "scalar",
                f"What {label}, as a share of the hub.",
                hub_cte_nosec +
                f"SELECT (SELECT {expr} FROM web_cashflow" + where_nosec + filt + ")\n"
                "       / (SELECT total FROM hub) AS share_pct",
                _percent_viz("share_pct"), home="Cash Flow" if name == "Net cash flow" else None)
        # A rate is a proportion already: the twin shows it as it is,
        # the way the returns percentages survive their own twin.
        for name, rate_sql in (("Savings rate", _savings_rate_sql),
                               ("Yield share", _yield_share_sql)):
            whole(name, "scalar",
                f"The {name.lower()}, shown as it is.",
                rate_sql(where_nosec, "share_pct"),
                _percent_viz("share_pct"), home="Cash Flow" if name == "Savings rate" else None)
        # The diagram, with the hub at 100: the same edges divided through.
        whole("Cash flow", "sankey",
            "The diagram, each edge a share of the hub.",
            _cashflow_sankey_sql(where_nosec, val, share=True), sankey_viz)
        # The Section picker narrows the bars, `m`. The net and the
        # biggest month read every section, `a`, so the net is the
        # household's whatever is picked, and the twin is the base card
        # scaled by one number.
        statement_section = ("CASE WHEN section IN ('operating_in', 'operating_out')\n"
                             "              THEN 'operating' ELSE section END"
                             " AS statement_section,\n         ")
        flow("Cash flow statement by month", "combo",
            "The statement per month, each section and the net a share of the "
            "biggest month's gross movement.",
            _series_order(
                month_cte(statement_section) + "   GROUP BY 1, 2),\n"
                f"a AS (\n  SELECT {OCCURRED_MONTH},\n         {statement_section}{val} AS v\n"
                "    FROM web_cashflow" + where_nosec + "\n   GROUP BY 1, 2),\n" + gross_peak("a") +
                f"SELECT month, statement_section,\n       {PEAK_DIV} AS share_pct\n"
                f"  FROM m\nUNION ALL\nSELECT month, '{NET_CASH_FLOW}',\n       sum(v) / {PEAK} * 100\n"
                "  FROM a\n GROUP BY 1", "month", "statement_section", "share_pct", "total",
                legend_last=NET_CASH_FLOW),
            {"graph.dimensions": ["month", "statement_section"],
             "graph.metrics": ["share_pct"], **STACKED_VIZ, **STATEMENT_SERIES})
        # The grouping expression and the display ride in the tuple
        # rather than being inferred from the name or the filter text:
        # the investing row is the one that folds its classes under the
        # Investing picker, and a substring test against the filter
        # would break the day another row's filter names a section list
        # containing it.
        investing_grp = ("CASE WHEN {{investing}} = 'whole' THEN 'Investments'\n"
                         "              ELSE class_node END")
        for name, display, filt, neg, grp, blurb in (
                ("Inflows by class by month", "area", "\n   AND section = 'operating_in'", False,
                 "class_node", "What came in each month by class"),
                ("Outflows by class by month", "area", "\n   AND section = 'operating_out'", True,
                 "class_node", "What went out each month by class"),
                ("Investing by month", "bar", "\n   AND section = 'investing'", False,
                 investing_grp, "Investing per month, signed"),
                ("Financing and vehicles by month", "bar",
                 "\n   AND section IN ('financing', 'vehicles')", False,
                 "class_node", "Debt and the earmarked pools per month, signed")):
            sql = (month_cte(f"{grp} AS class,\n         ", filt, sign="-" if neg else "")
                   + "   GROUP BY 1, 2),\n" + gross_peak() +
                   f"SELECT month, class,\n       {PEAK_DIV} AS share_pct\n  FROM m")
            flow(name, display,
                f"{blurb}, as shares of the biggest month's gross movement.",
                _series_order(sql, "month", "class", "share_pct", "total"),
                {"graph.dimensions": ["month", "class"],
                 "graph.metrics": ["share_pct"], **STACKED_VIZ})
        # The one card that redacts rather than normalising: the base
        # version names the account a line moved through and the payer
        # or merchant behind it, and the fix for a leaking column is to
        # drop it. Ranked, so the shape of "how concentrated was this
        # window" survives without naming anyone.
        flow("Largest flows", "table",
            "The fifty largest lines, ranked, each a share (%) of the hub, with "
            "no name or account.",
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
        cashflow_plan_cards(f)
        bridge_cards(f)
        return
    whole("Net cash flow", "scalar",
        "The cash pool's net change over the window, the diagram's Cash "
        "node negated.",
        f"SELECT {val} AS net_cash_flow\n  FROM web_cashflow" + where_nosec,
        {}, home="Cash Flow")
    # Named for the SECTION, not for "cash": the residual section
    # is also called cash and is a different number, and the CLI's
    # columns behind these two tiles are operating_in / operating_out.
    whole("Operating in", "scalar",
        "Every receipt over the window. It differs from `wealthdb income` "
        "(docs/CASHFLOW.md §8).",
        f"SELECT {val} AS operating_in\n  FROM web_cashflow" + where_nosec +
        "\n   AND section = 'operating_in'",
        {})
    whole("Operating out", "scalar",
        "Consumption, fees, taxes and giving over the window, buying and "
        "selling excluded.",
        f"SELECT -({val}) AS operating_out\n  FROM web_cashflow" + where_nosec +
        "\n   AND section = 'operating_out'",
        {})
    whole("Savings rate", "scalar",
        "Operating cash flow as a share of receipts. Blank where nothing "
        "came in.",
        _savings_rate_sql(where_nosec, "savings_rate"),
        _percent_viz("savings_rate"), home="Cash Flow")
    whole("Yield share", "scalar",
        "The share of receipts from assets, such as dividends, interest "
        "and rent.",
        _yield_share_sql(where_nosec, "yield_share"),
        _percent_viz("yield_share"))
    # The diagram, and the centre of the dashboard. A native query in
    # the three columns the BI layer's Sankey visualisation reads,
    # over the LINE-grain serving view: a node's side is the sign of
    # its net over the filtered window, so the nets and the sides
    # have to be computed where the pickers apply.
    whole("Cash flow", "sankey",
        "Where the window's cash came from (left) and went (right).",
        _cashflow_sankey_sql(where_nosec, val), sankey_viz)
    # The Section picker narrows the bars. The net is the
    # household's whatever it picks, as the Net cash flow tile is.
    flow("Cash flow statement by month", "combo",
        "Operating, investing, financing and vehicles per month as signed "
        "bars, with net cash flow as a line.",
        _series_order(f"SELECT {OCCURRED_MONTH},\n"
                 "       CASE WHEN section IN ('operating_in', 'operating_out')\n"
                 "            THEN 'operating' ELSE section END AS statement_section,\n"
                 f"       {val} AS value\n"
                 "  FROM web_cashflow" + where + "\n GROUP BY 1, 2\n"
                 f"UNION ALL\nSELECT {OCCURRED_MONTH},\n       '{NET_CASH_FLOW}',\n"
                 f"       {val}\n"
                 "  FROM web_cashflow" + where_nosec + "\n GROUP BY 1",
                 "month", "statement_section", "value", "total", legend_last=NET_CASH_FLOW),
        {"graph.dimensions": ["month", "statement_section"],
         "graph.metrics": ["value"], **STACKED_VIZ, **STATEMENT_SERIES})
    flow("Inflows by class by month", "area",
        "Receipts per month by class, such as earnings, yield, pensions "
        "and benefits, and the uncategorized backlog.",
        _series_order(f"SELECT {OCCURRED_MONTH},\n       class_node AS class,\n"
                 f"       {val} AS value\n"
                 "  FROM web_cashflow" + where +
                 "\n   AND section = 'operating_in'\n GROUP BY 1, 2",
                 "month", "class", "value", "total"),
        {"graph.dimensions": ["month", "class"],
         "graph.metrics": ["value"], **STACKED_VIZ})
    flow("Outflows by class by month", "area",
        "Outflows per month by class, such as consumption, fees, taxes, "
        "giving, and the uncategorized backlog.",
        _series_order(f"SELECT {OCCURRED_MONTH},\n       class_node AS class,\n"
                 f"       -({val}) AS value\n"
                 "  FROM web_cashflow" + where +
                 "\n   AND section = 'operating_out'\n GROUP BY 1, 2",
                 "month", "class", "value", "total"),
        {"graph.dimensions": ["month", "class"],
         "graph.metrics": ["value"], **STACKED_VIZ})
    flow("Investing by month", "bar",
        "Investing per month, signed. Above the axis the portfolio fed "
        "the household, below it the reverse.",
        _series_order(f"SELECT {OCCURRED_MONTH},\n"
                 "       CASE WHEN {{investing}} = 'whole' THEN 'Investments'\n"
                 "            ELSE class_node END AS class,\n"
                 f"       {val} AS value\n"
                 "  FROM web_cashflow" + where +
                 "\n   AND section = 'investing'\n GROUP BY 1, 2",
                 "month", "class", "value", "total"),
        {"graph.dimensions": ["month", "class"],
         "graph.metrics": ["value"], **STACKED_VIZ})
    flow("Financing and vehicles by month", "bar",
        "Debt serviced or drawn, and earmarked pools funded or drawn on, "
        "per month and signed.",
        _series_order(f"SELECT {OCCURRED_MONTH},\n       class_node AS class,\n"
                 f"       {val} AS value\n"
                 "  FROM web_cashflow" + where +
                 "\n   AND section IN ('financing', 'vehicles')\n GROUP BY 1, 2",
                 "month", "class", "value", "total"),
        {"graph.dimensions": ["month", "class"],
         "graph.metrics": ["value"], **STACKED_VIZ})
    flow("Largest flows", "table",
        "The fifty largest lines of the window, with their node, account "
        "and instrument, payer or merchant.",
        "SELECT occurred_at,\n       section,\n       class_node AS class,\n"
        "       group_node AS \"group\",\n       name,\n"
        "       account_label AS account,\n"
        f"       {_ccy_case('value')} AS value\n"
        "  FROM web_cashflow" + where +
        f"\n ORDER BY abs({_ccy_case('value')}) DESC\n LIMIT 50",
        {})
    cashflow_plan_cards(f)
    bridge_cards(f)


def returns_cards(f):
    """The Returns dashboard's cards, or on the twin its privacy cards,
    over the materialized report_returns table. The three scalars are
    MBQL over the REDACTED model (grain 'global', granularity 'total'):
    the Currency and Start-year pickers land on their currency /
    window_from_year dimensions, and window_from_year selects the
    since-inception (0) or since-<year> summary, so the picker rescopes
    the whole figure exactly (the since-inception TWR is often null
    around the degenerate first months). twr/mwr are ratios rendered as
    percent ("max" is the name Metabase gives the aggregate). Opened
    standalone the scalars aggregate across currencies / windows, so set
    Currency and Start year first. They are privacy-exempt, and a
    scalar's "see these records" drill-through opens the underlying
    model, which therefore must not carry the absolute money columns.

    The charts and the by-source table are native: every chart splits
    by source with the global grain unioned in as a toggleable '(all
    sources)' line, and the Currency / Start-year / Source pickers map
    onto their {{currency}} / {{start_year}} / {{source_ff}} tags. Every
    figure is a ratio but the by-source table's money columns and the
    value against the benchmark, so those two alone have a privacy card:
    the table without the money columns, the value indexed to 100."""
    f = f.using(tail=RETURNS_PRIVACY_DESC if f.privacy else "")

    def chart(name, desc, sql, viz, **kw):
        f.card(name, "line", desc, sql, viz, tags=returns_tags(sql), **kw)

    def figure(name, desc, sql, label, excess=False):
        """A figure beside the returns scalars, a benchmark's: a percent,
        or with `excess` a difference in percentage points."""
        viz = ({"column_settings": {f'["name","{label}"]': {"suffix": " pp", "decimals": 2}}} if excess
               else _percent_viz(label))
        f.card(name, "scalar", desc, sql, {**viz, "scalar.field": label}, tags=returns_tags(sql),
               exempt=True, hide_empty=True)

    def scalar(name, desc, col):
        f.card(name, "scalar", desc,
               _mbql("report_returns_redacted",
                     {"filter": _returns_part("global", "total"),
                      "aggregation": [["max", _f(col, "type/Float")]]}),
               _percent_viz("max"), exempt=True)

    scalar("Return (TWR)",
           "Whole-portfolio time-weighted return over the chosen window, with "
           "the timing and size of external flows stripped out.", "twr")
    scalar("Return (MWR)",
           "Whole-portfolio money-weighted return (XIRR) over the chosen "
           "window, external flows included.", "mwr")
    scalar("Annualized return (TWR)",
           "The window's time-weighted return as a constant yearly rate, n/a "
           "for windows under a year.", "twr_annualized")
    chart("Cumulative return (TWR)",
          "Time-weighted return compounded year by year from the start year, "
          "per source and for '(all sources)'.",
          _series_order(returns_growth_sql(), "year", "source", "cumulative_return", "latest",
                        largest_first=True, rows_last=BENCHMARK_SERIES),
          {**_series_viz("year", "source", "cumulative_return", percent=True), **LINES_VIZ, **BENCHMARK_LINE},
          exempt=True)
    for name, granularity, period in (("Monthly returns (TWR)", "monthly", "month"),
                                      ("Quarterly returns (TWR)", "quarterly", "quarter"),
                                      ("Annual returns (TWR)", "annual", "calendar year")):
        chart(name,
              f"Time-weighted return per {period}, one line per source plus an '(all sources)' line.",
              returns_period_sql(granularity),
              {**_series_viz("period", "source", "twr", percent=True), **BENCHMARK_LINE}, exempt=True)
    sql = returns_by_source_sql(not f.privacy)
    f.card("Returns by source", "table",
           "TWR and MWR per source over the chosen window, with the flags "
           "behind each n/a. A benchmark adds a line over the months it covers."
           if f.privacy else
           "TWR and MWR per source over the chosen window, with values, net "
           "external flow and the flags behind each n/a. A benchmark adds a line "
           "over the months it covers.",
           sql, _percent_viz("twr", "twr_annualized", "mwr", "mwr_annualized"), tags=returns_tags(sql))
    figure("Benchmark return (TWR)",
           "The picked benchmark's time-weighted return over the months it covers of "
           "the chosen window, which Returns by source names.",
           benchmark_scalar_sql("twr", "benchmark_return"), "benchmark_return")
    figure("Benchmark return (MWR)",
           "What the household's own flows would have earned in the benchmark: the "
           "same money on the same days, over the months it covers.",
           benchmark_scalar_sql("mwr", "benchmark_mwr"), "benchmark_mwr")
    figure("Benchmark annualized return (TWR)",
           "The benchmark's return over its months as a constant yearly rate, n/a under a year.",
           benchmark_scalar_sql("twr_annualized", "benchmark_annualized"), "benchmark_annualized")
    figure("Excess return (TWR)",
           "The portfolio's time-weighted return less the benchmark's, in percentage "
           "points, over the same months.",
           benchmark_scalar_sql("100 * excess", "excess_return"), "excess_return", excess=True)
    figure("Excess return (MWR)",
           "The portfolio's money-weighted return less the same money's in the benchmark, "
           "in percentage points, over the same months.",
           benchmark_scalar_sql("100 * (portfolio_mwr - mwr)", "excess_mwr"), "excess_mwr", excess=True)
    figure("Excess annualized return (TWR)",
           "The portfolio's yearly rate less the benchmark's, in percentage points.",
           benchmark_scalar_sql("100 * excess_annualized", "excess_annualized"), "excess_annualized", excess=True)
    chart("Money-weighted return over time",
          "The money-weighted return from the window's start to each month end, for the "
          "portfolio and for the same money in the benchmark.",
          benchmark_line_sql("mwr"),
          # Ordinal, or the days between month ends read as missing too.
          {**_series_viz("day", "series", "mwr", percent=True), **LINES_VIZ, **BENCHMARK_LINE, "line.missing": "none",
           "graph.x_axis.scale": "ordinal"}, exempt=True, hide_empty=True)
    if f.privacy:
        chart("Value against the benchmark",
              "The portfolio's value and the same money in the benchmark, each at 100 at "
              "the window's start.",
              benchmark_line_sql("value", index=True),
              {**_series_viz("day", "series", "indexed_value"), **LINES_VIZ, **BENCHMARK_LINE}, hide_empty=True)
    else:
        chart("Value against the benchmark",
              "The portfolio's value, and what the same start value and flows would be "
              "worth in the benchmark.",
              benchmark_line_sql("value"),
              {**_series_viz("day", "series", "value"), **LINES_VIZ, **BENCHMARK_LINE}, hide_empty=True)


def freshness_cards(f):
    """The Data Freshness dashboard's cards, or on the twin its one
    privacy card. The dashboard takes no picker. Its scalars and its Mark
    age, which reads `wealthdb status --marks` (web_marks), show days and
    shares, so the twin shows them as they are. The freshness table's
    twin runs it over the _pct sources model: the dashboard is unfiltered
    by design, so the model's fixed %-of-latest scale IS the
    selected-sources scale, and the source rows total 100. It has a
    description of its own: the base card's promises a money column,
    which there holds shares."""
    f = f.using(tail=PRIVACY_DESC if f.privacy else "")
    f.card("Stalest source (days)", "scalar",
           "Days since the source read longest ago was last read. A source "
           "that marks quarterly counts from its latest read.",
           _mbql("report_sources_latest",
                 {"expressions": {"days_stale": DAYS_STALE},
                  "filter": FED_SOURCES,
                  "aggregation": [["max", ["expression", "days_stale"]]]}),
           {}, exempt=True, home="Data Freshness")
    f.card("Returns age (days)", "scalar",
           "Days since a `web refresh` last computed the returns.",
           # Every row of a run shares one computed_at, so max(age) is
           # the run's age. Runs over the REDACTED model although the
           # card itself shows no values: it is privacy-exempt (shared
           # by the Data Freshness twin), and a scalar's "see these
           # records" drill-through opens the underlying model — which
           # must therefore not carry absolute money columns.
           _mbql("report_returns_redacted",
                 {"expressions": {"age_days": ["datetime-diff",
                      _f("computed_at", "type/DateTime"), ["now"], "day"]},
                  "aggregation": [["max", ["expression", "age_days"]]]}),
           {}, exempt=True)
    f.card("Source freshness", "table",
           "Each source's last read and latest mark, the days since each, and its share of the latest total."
           if f.privacy else
           "Each source's last read and latest mark, the days since each, "
           f"and the value it holds in {DEFAULT_CURRENCY}.",
           _mbql("report_sources_latest_pct" if f.privacy else "report_sources_latest",
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
    f.card("Mark age", "bar",
           "The share of value marked within five days of each month end: how far that month's return can "
           "be trusted. Liabilities and cash are out.",
           "SELECT CAST(date_trunc('month', month_end) AS TIMESTAMP) AS month, marked_pct\n"
           "  FROM web_marks\n ORDER BY month",
           {"graph.dimensions": ["month"], "graph.metrics": ["marked_pct"],
            "graph.y_axis.auto_range": False, "graph.y_axis.min": 0, "graph.y_axis.max": 100,
            "graph.x_axis.title_text": "", "graph.y_axis.title_text": "",
            "series_settings": {"marked_pct": {"color": BENCHMARK_COLOR}},
            **_share_viz("marked_pct")},
           tags={}, exempt=True)


def gains_cards(f):
    """The Gains dashboard's cards: native SQL over web_gains, the
    monthly gains rows materialized per reporting currency. The view
    carries a row set per currency, so {{currency}} is a row filter
    here, where the other families pick a value column with it. A
    figure over the window sums the months in it; a figure at the
    window's end reads its last month. docs/GAINS.md defines the
    figures and the quality counters. It carries a row set per reading
    of a missing cost basis too, and {{missing_basis}} picks one the same
    way (docs/GAINS.md §8).

    On a twin, the privacy variants of the two headline gains the
    Wealth Overview's twin shows: each gain as a ratio of the cost basis
    it is a gain on. Gains has no twin of its own (NO_PRIVACY_TWIN)."""
    tags = {**native_tags("web_gains", GAINS_FILTERS), "missing_basis": missing_basis_tag()}
    native = f.using(tags=tags, tail=PRIVACY_DESC if f.privacy else "").card
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

    if f.privacy:
        native("Realized gain", "scalar",
            "The gain or loss on sales over the window, as a percent of the "
            "cost basis sold.",
            "SELECT sum(realized) FILTER (WHERE realized_cost_basis > 0)\n"
            "       / nullif(sum(realized_cost_basis) FILTER (WHERE realized IS NOT NULL\n"
            "                                                 AND realized_cost_basis > 0), 0)"
            " AS realized_pct\n"
            "  FROM web_gains" + where, _percent_viz("realized_pct"), home="Gains")
        native("Unrealized gain", "scalar",
            "The gain or loss held at the window's end, as a percent of the "
            "cost basis held.",
            end + "SELECT sum(unrealized_end) FILTER (WHERE cost_basis > 0)\n"
            "       / nullif(sum(cost_basis) FILTER (WHERE unrealized_end IS NOT NULL\n"
            "                                        AND cost_basis > 0), 0) AS unrealized_pct\n"
            "  FROM e", _percent_viz("unrealized_pct"), home="Gains")
        return

    def unrealized_table(name, desc, pred, order):
        """Ten positions held at the window's end, by unrealized gain or
        by its percent of the cost basis."""
        native(name, "table", desc,
                      end + f"SELECT {position} AS position,\n"
                      "       unrealized_end AS unrealized,\n"
                      "       unrealized_end / nullif(cost_basis, 0) AS unrealized_pct,\n"
                      "       cost_basis,\n       value,\n       account\n"
                      f"  FROM e\n WHERE {pred}\n ORDER BY {order}\n LIMIT 10",
                      _percent_viz("unrealized_pct"))

    def realized_table(name, desc, pred, order):
        """Ten positions by what their sales over the window realized,
        or by its percent of the cost basis sold."""
        native(name, "table", desc,
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
        native(name, "bar", desc,
                      f"SELECT {dim} AS {label},\n{cols}\n"
                      "  FROM web_gains" + where +
                      f"\n GROUP BY 1\nHAVING {some}\n ORDER BY {total} DESC",
                      {"graph.dimensions": [label],
                       "graph.metrics": [a for _, a in metrics],
                       "stackable.stack_type": "stacked"})

    term_split = [("realized_short", "short_term"),
                  ("realized_long", "long_term"),
                  ("realized_other", "term_unstated")]
    native("Realized gain", "scalar",
        "The gain or loss on what was sold over the window. The coverage "
        "table counts the sales it leaves out.",
        "SELECT sum(realized) AS realized\n  FROM web_gains" + where, {}, home="Gains")
    native("Unrealized gain", "scalar",
        "The gain or loss on what is held at the window's end, over the "
        "holdings with a cost basis.",
        end + "SELECT sum(unrealized_end) AS unrealized\n  FROM e", {}, home="Gains")
    native("Gain on positions sold", "scalar",
        "What the positions sold over the window gained in their month of "
        "sale, from the month's start or their purchase.",
        "SELECT sum(sold_gain) AS sold\n  FROM web_gains" + where, {})
    native("Gain on positions held", "scalar",
        "How far the unrealized gain of the positions kept moved over the "
        "window, exchange-rate moves included.",
        "SELECT sum(held_change) AS held\n  FROM web_gains" + where, {})
    native("Total gain", "scalar",
        "The gain on positions sold plus the gain on positions held. "
        "Income, fees and taxes are not in it.",
        f"SELECT {gain} AS gain\n  FROM web_gains" + where, {})
    native("Cost basis coverage", "scalar",
        "Share of the value held at the window's end that carries a cost "
        "basis. Unrealized figures leave out the rest.",
        end + "SELECT sum(abs(value)) FILTER (WHERE NOT end_without_basis)\n"
        "       / nullif(sum(abs(value)), 0) AS basis_coverage\n"
        "  FROM e\n WHERE end_applies",
        _percent_viz("basis_coverage"))
    native("Gains by month", "combo",
        "Each month's gain on positions sold and held as stacked bars, "
        "with their sum as a line.",
        "SELECT period AS month,\n" + ",\n".join(
            f"       sum({c}) AS {a}" for c, a in gain_split) +
        f",\n       {gain} AS gain\n"
        "  FROM web_gains" + where + "\n GROUP BY 1\n ORDER BY 1",
        {"graph.dimensions": ["month"],
         "graph.metrics": [a for _, a in gain_split] + ["gain"],
         "series_settings": {**{a: {"display": "bar"} for _, a in gain_split},
                             "gain": {"display": "line"}},
         "stackable.stack_type": "stacked"})
    native(
        "Realized gains by month, short vs long term", "bar",
        "Each month's realized gain as short term, long term, or with no "
        "term stated.",
        "SELECT period AS month,\n" + ",\n".join(
            f"       sum({c}) AS {a}" for c, a in term_split) +
        "\n  FROM web_gains" + where + "\n     AND realized IS NOT NULL"
        "\n GROUP BY 1\n ORDER BY 1",
        {"graph.dimensions": ["month"],
         "graph.metrics": [a for _, a in term_split],
         "stackable.stack_type": "stacked"})
    native("Unrealized gain over time", "line",
        "The unrealized gain at each month's end, per source and for "
        "'(all sources)', which the Source picker does not narrow.",
        _series_order(
            "SELECT period AS month, silver_source_id AS source,\n"
            "       sum(unrealized_end) AS unrealized\n"
            "  FROM web_gains" + where + "\n     AND at_end\n GROUP BY 1, 2\n"
            "UNION ALL\n"
            f"SELECT period, '{ALL_SOURCES_SERIES}', sum(unrealized_end)\n"
            "  FROM web_gains" + where_all + "\n     AND at_end\n GROUP BY 1",
            "month", "source", "unrealized", "latest", largest_first=True),
        {**_series_viz("month", "source", "unrealized"), **LINES_VIZ})
    unrealized_table("Largest unrealized gains",
        "The ten positions held at the window's end with the largest "
        "unrealized gain.",
        "unrealized_end > 0", "unrealized DESC")
    unrealized_table("Largest unrealized losses",
        "The ten positions held at the window's end with the largest "
        "unrealized loss.",
        "unrealized_end < 0", "unrealized")
    unrealized_table("Best unrealized %",
        "The ten positions held at the window's end with the largest "
        "unrealized gain as a share of their cost basis.",
        "unrealized_end > 0 AND cost_basis > 0", "unrealized_pct DESC")
    unrealized_table("Worst unrealized %",
        "The ten positions held at the window's end with the largest "
        "unrealized loss as a share of their cost basis.",
        "unrealized_end < 0 AND cost_basis > 0", "unrealized_pct")
    realized_table("Largest realized gains",
        "The ten positions whose sales over the window realized the "
        "largest gain. The cost basis is that of the lots sold.",
        "realized > 0", "realized DESC")
    realized_table("Largest realized losses",
        "The ten positions whose sales over the window realized the "
        "largest loss. The cost basis is that of the lots sold.",
        "realized < 0", "realized")
    realized_table("Best realized %",
        "The ten positions whose sales over the window realized the "
        "largest gain as a share of the cost basis sold.",
        "realized > 0 AND cost_basis > 0", "realized_pct DESC")
    realized_table("Worst realized %",
        "The ten positions whose sales over the window realized the "
        "largest loss as a share of the cost basis sold.",
        "realized < 0 AND cost_basis > 0", "realized_pct")
    split_bars("Gain by source",
        "Each source's gain on positions sold and held over the window, "
        "stacked.",
        "silver_source_id", "source", gain_split)
    split_bars("Gain by asset class",
        "Each asset class's gain on positions sold and held over the "
        "window, stacked. 'unknown' holds sales no snapshot classes.",
        "asset_class", "asset_class", gain_split)
    split_bars(
        "Realized gains by tax wrapper",
        "Realized gain over the window per tax wrapper, by holding "
        "period.",
        "tax_wrapper", "tax_wrapper", term_split)
    native("Realized gains by tax year", "table",
        "Per calendar year, realized gain by holding period, wash sales, "
        "proceeds, cost basis sold and uncovered sells. Not a tax return.",
        "SELECT CAST(year(period) AS VARCHAR) AS tax_year,\n"
        "       sum(realized) AS realized,\n" + ",\n".join(
            f"       sum({c}) AS {a}" for c, a in term_split) + ",\n"
        "       sum(wash_disallowed) AS wash_disallowed,\n"
        "       sum(proceeds) AS proceeds,\n"
        "       sum(realized_cost_basis) AS cost_basis,\n"
        "       sum(realized_lots) AS realized_lots,\n"
        "       sum(sells_without_documents) AS sells_without_documents\n"
        "  FROM web_gains" + where + "\n GROUP BY 1\n ORDER BY 1 DESC",
        {})
    native("Unrealized gain vs. percent",
        "scatter",
        "Each position held at the window's end, its unrealized gain "
        "against its percent of cost basis, sized by value.",
        end + f"SELECT {position} AS position,\n"
        "       asset_class,\n"
        "       unrealized_end / cost_basis AS unrealized_pct,\n"
        "       unrealized_end AS unrealized,\n"
        "       abs(value) AS value\n"
        "  FROM e\n WHERE unrealized_end IS NOT NULL AND cost_basis > 0",
        # The second dimension splits the dots into one series, and
        # so one colour, per asset class.
        {"graph.dimensions": ["unrealized_pct", "asset_class"],
         "graph.metrics": ["unrealized"],
         "scatter.bubble": "value",
         **_percent_viz("unrealized_pct")})
    native("Positions in the green", "scalar",
        "Positions held at the window's end with an unrealized gain.",
        end + "SELECT count(*) AS positions\n  FROM e\n WHERE unrealized_end > 0",
        {})
    native("Positions in the red", "scalar",
        "Positions held at the window's end with an unrealized loss.",
        end + "SELECT count(*) AS positions\n  FROM e\n WHERE unrealized_end < 0",
        {})
    native("Coverage by source", "table",
        "Per source, the value held at the window's end, the part with a "
        "cost basis, and counts of gains missing or misplaced.",
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
        _percent_viz("basis_coverage"))


# The dashboard pickers' ids (PICKERS). They are arbitrary but must be
# stable across runs so re-provisioning converges instead of
# accumulating parameters, and they must be distinct — a reused id
# would make two pickers one. The Returns dashboards carry a start-year
# picker instead of a time filter — the periods are precomputed buckets.
TIME_PARAM_ID = "aa5df100"
SOURCE_PARAM_ID = "aa5df101"
ASOF_PARAM_ID = "aa5df102"
ASSET_PARAM_ID = "aa5df103"
CURRENCY_PARAM_ID = "aa5df104"
START_YEAR_PARAM_ID = "aa5df105"
VEHICLE_PARAM_ID = "aa5df106"
# The Spending dashboards' own three pickers, beside the time range
# and the source. The currency picker is separate from the Returns one
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

# Spending cards that carry every reporting currency as its own column,
# and so must NOT be wired to the Currency picker: the long model has a
# row per (line, currency), so a row filter on `currency` would empty
# every column but the one the picker selects. Such a card also reads the
# same opened standalone, where no picker reaches it.
SPEND_ALL_CURRENCY_CARDS = {"Top 50 merchants"}

def dashboard_tile_titles(base, cards):
    """card name -> the title its tiles show on base dashboard `base` and
    its twin, of `cards` (card_defs). The twin's title takes the privacy
    form, unless the twin shows the card itself (an exempt card). A title is set for three
    kinds of card: a borrowed card whose name leans on its home
    dashboard's pickers, a card whose title is another card's name, and
    an Excess figure, which reads the benchmark's months and not the
    Return tiles' window."""
    titles = {"Wealth Overview": {"Income expected": f"Income expected, {HORIZONS[0]} months",
                                  "Asset class mix over time": "Asset classes over time"},
              "Returns": {**{c: f"{c}, same months" for c in EXCESS_CARDS},
                          "Excess annualized return (TWR)": "Excess annualized (TWR), same months"}}.get(base, {})
    return {**titles, **{privacy_name(c): privacy_name(t) for c, t in titles.items()
                         if not cards[c].exempt}}


class Card(NamedTuple):
    """One pre-defined card: what is sent (name, display, description,
    dataset_query, viz) and how its tiles treat it. `exempt`: it shows no
    money, so a twin shows it as it is. `hide_empty`: it reads a row
    only beside a picked benchmark, and its tile hides with none.
    A dashboard with no benchmark picked then reads as it does without
    such cards: Metabase closes the gap a hidden tile leaves, wherever
    the tile sits. `links`: what a click on a cell does (Link). `home`:
    the dashboard a Wealth Overview tile of it opens."""
    name: str
    display: str
    desc: str
    query: dict
    viz: dict
    exempt: bool = False
    hide_empty: bool = False
    links: tuple = ()
    home: str = None


class Link(NamedTuple):
    """A click on a cell of column `column` sets the picker of slug `to`
    to the value clicked. On a tile that opens its card's home, the
    picker is the home dashboard's; on any other tile, its own
    dashboard's (Metabase's crossfilter)."""
    column: str
    to: str


class Family:
    """The cards one dashboard defines, its base's or with `privacy` its
    twin's, into `cards`. `tags` and `tail` are what a card takes unless
    it names its own: its template tags and the words its description
    ends in."""

    def __init__(self, privacy, cards, tags=None, tail=""):
        self.privacy, self.cards, self.tags, self.tail = privacy, cards, tags, tail

    def using(self, tags=None, tail=None):
        """The same family whose cards take `tags` and `tail` instead."""
        return Family(self.privacy, self.cards, self.tags if tags is None else tags,
                      self.tail if tail is None else tail)

    def card(self, name, display, desc, sql, viz, *, tags=None, exempt=False, hide_empty=False,
             cell_clicks=(), home=None):
        """Define card `name`, or on a twin its privacy variant, none for
        an `exempt` card, which the twin shows as it is. `sql` is native
        SQL over `tags` (the family's unless given), or an MBQL card's
        dataset_query. A family's tags are all its cards', but the
        Benchmark lands only on a card that reads it."""
        if self.privacy and exempt:
            return
        if isinstance(sql, str):
            tags = self.tags if tags is None else tags
            if "{{benchmark}}" not in sql:
                tags = {k: v for k, v in tags.items() if k != "benchmark"}
            sql = _native(sql, tags)
        name = privacy_name(name) if self.privacy else name
        if name in self.cards:
            raise ValueError(f"provision: card {name!r} is defined twice")
        self.cards[name] = Card(name, display, desc + self.tail, sql, viz, exempt, hide_empty,
                                tuple(cell_clicks), home)


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


def _beside(label, metric, dimension, cols=(), weight="share_pct", where="", order=None, picked_only=False,
            keep_series=False):
    """The tail of a card that sets the household's rows, CTE `p` of
    `label`, `metric` and `cols`, beside the picked benchmark's: the rows
    of `dimension` of benchmark_exposure, each with `weight` as `metric`
    under `label`, and a `series` column the chart breaks out by. Under
    None the benchmark's rows are none and the household's stay as they
    are, and `series` goes, so the chart is the single series it is with
    no benchmark at all; with `picked_only` the household's rows go too,
    for a card that exists only beside a benchmark. The portfolio comes
    first, so its order sets the bars'. `order`, by default the metric,
    largest first, orders within a series. With `keep_series` the column
    stays under None, one series, and the benchmark's takes the fixed
    label, so a chart keeps its breakout and its colours."""
    picked = "\n WHERE {{benchmark}} <> '" + NO_BENCHMARK + "'" if picked_only else ""
    named = ", ".join((label, *cols, metric))
    columns = (", ".join((label, "series", *cols, metric)) if keep_series
               else f"COLUMNS(lambda c: c <> 'series' OR {{{{benchmark}}}} <> '{NO_BENCHMARK}')")
    name = f"'{BENCHMARK_SERIES}'" if keep_series else "{{benchmark}}"
    return (f"SELECT {columns}\n  FROM (\n"
            f"SELECT {named}, '{PORTFOLIO_SERIES}' AS series\n  FROM p{picked}\n"
            "UNION ALL BY NAME\n"
            f"SELECT bucket AS {label}, {name} AS series, {weight} AS {metric}\n"
            "  FROM benchmark_exposure\n"
            f" WHERE {PICKED_BENCHMARK} AND dimension = '{dimension}'{where})\n"
            f" ORDER BY series <> '{PORTFOLIO_SERIES}', {order or metric + ' DESC'}, 1")


def _beside_viz(label):
    """The dimensions of a chart a `series` column breaks out."""
    return {"graph.dimensions": [label, "series"]}


def _table_columns(*names):
    """A table's columns, named: a run that returns more than the saved
    question's columns shows only those, so a column the Benchmark picker
    brings is named here. One a run leaves out is passed over."""
    return {"table.columns": [{"name": n, "enabled": True} for n in names]}


def _columns_with_benchmark(sql, order, *cols):
    """`sql`'s columns, the benchmark's `cols` among them only with a
    benchmark picked, by `order`. A table or a Yield line chart then
    shows under None what it shows with no benchmark at all. DuckDB reads
    the picker's value when it binds the query, so the column list can
    follow it."""
    names = ", ".join(f"'{c}'" for c in cols)
    return (f"SELECT COLUMNS(lambda c: c <> '{order}' AND (c NOT IN ({names}) OR {{{{benchmark}}}} <> '{NO_BENCHMARK}'))\n"
            f"  FROM (\n{sql}\n)\n ORDER BY {order}")


def exposure_cards(f):
    """The Exposure dashboard's cards, or with `privacy` their privacy
    variants: name -> (display, description, dataset_query, viz). Native
    SQL over web_exposure, the look-through at every month end and
    today, materialized per reporting currency, so {{currency}} is a row
    filter as on Gains, and per source and tax wrapper. The pickers
    narrow those and the asset class. Every share is of the selection,
    its day's net worth with nothing picked, computed over the rows a
    card reads. Sizes and cycles plot a share of the public equity
    instead, and the debt cards one of the debt, or of the debt and
    cash. A breakdown reads the latest day in the window and plots each
    share: a bar carries its figure, a map colours each country by its
    share. Its base card keeps the value as a column and shows it in the
    tooltip; its privacy card has the share alone, a share being
    scale-free. A chart over time shows each month end's shares on both,
    so it is privacy-exempt and has no privacy variant. Currencies over
    time is the exception: its base card plots each month end's value in
    the picked currency, its privacy card the shares.

    The debt cards read web_exposure_debt, at the same instants and
    currencies. The Top securities card reads web_exposure_securities,
    each security's slices at those instants. docs/EXPOSURE.md defines
    every figure.

    A breakdown of the design's table sets the picked benchmark beside
    the selection (_beside): today's look-through of its funds, which no
    picker but the Benchmark narrows. A chart type is fixed, so those
    breakouts are bars whatever is picked, and the map has a table."""
    privacy = f.privacy
    tags = {**native_tags("web_exposure", EXPOSURE_FILTERS), "benchmark": BENCHMARK_TAG}
    tail = PRIVACY_DESC if privacy else ""
    # The selection, each row's share of its day's selected total, and
    # its rows at the window's end.
    last = _window_end("web_exposure", tags, share=True)[1]
    end = last + "\n"
    money = "" if privacy else ",\n       sum(value) AS value"
    amount = () if privacy else ("value",)
    card = f.using(tags=tags, tail=tail).card

    def bars(label, metric, tooltip=(), decimals=None, beside=False):
        """The viz of a breakdown's bars: the share, its figure on each
        bar, and in the tooltip the value on a base card, then the
        `tooltip` columns; with `beside` a bar per series."""
        viz = {"graph.dimensions": [label], "graph.metrics": [metric],
               "graph.show_values": True, **_share_viz(metric, decimals=decimals),
               **(_beside_viz(label) if beside else {})}
        if beside:
            viz["series_settings"] = {PORTFOLIO_SERIES: {"color": EXPOSURE_BAR_COLORS[metric]},
                                      BENCHMARK_SERIES: {"color": BENCHMARK_COLOR}}
        cols = ([] if privacy else ["value"]) + list(tooltip)
        if cols:
            viz["graph.tooltip_columns"] = cols
        return viz

    def at_end(name, display, desc, dim, label, dimension=None):
        """A breakdown's bars at the window's end, one row per value of
        `dim`. With `dimension`, the bars of benchmark_exposure's dimension
        beside them."""
        rows = f"SELECT {dim} AS {label}{money},\n       sum(share_pct) AS share_pct\n  FROM e\n GROUP BY ALL"
        if dimension:
            card(name, display, desc,
                 last + ",\np AS (\n" + rows + ")\n" + _beside(label, "share_pct", dimension, amount, keep_series=True),
                 bars(label, "share_pct", beside=True))
            return
        card(name, display, desc, end + rows + "\n ORDER BY sum(share_pct) DESC, 1", bars(label, "share_pct"))

    def over_time(name, desc, dim, label, fold=False, amounts=False):
        """Each month end's shares, stacked by the values of `dim`, the
        rows smallest first by the share at the window's end, as
        _series_order orders any stacked chart. With `fold`, a value
        under one percent long or short at the window's end, or gone by
        then, joins the `other` band, which tops the stack. A value kept
        ranks by the magnitude of its share. A currency sold forward thus
        keeps its own band, and Metabase stacks it below zero. A tie
        orders by the band's name. It ranks with an aggregate of its
        own: the DuckDB in Metabase's driver fails on that helper's
        window functions over w, whose share is a window function
        itself. With `amounts`, a base card plots each band's value in
        the picked currency instead, with the same bands and order, and
        its privacy card the shares."""
        kept = "abs(r.latest) >= 1"
        rank = f"CASE WHEN {kept} THEN abs(r.latest) ELSE -1 END" if fold else "COALESCE(r.latest, -1)"
        band = f"CASE WHEN {kept} THEN w.{dim} ELSE 'other' END" if fold else f"w.{dim}"
        metric = "value" if amounts and not privacy else "share_pct"
        card(name, "area", desc,
             last + f",\nr AS (SELECT {dim} AS k, sum(share_pct) AS latest FROM e GROUP BY 1)\n"
             f"SELECT w.as_of_day, {band} AS {label},\n       sum(w.{metric}) AS {metric}\n"
             f"  FROM w\n  LEFT JOIN r ON r.k = w.{dim}\n GROUP BY 1, 2\n"
             f" ORDER BY max({rank}), {'2, ' if fold else ''}1",
             {"graph.dimensions": ["as_of_day", label], "graph.metrics": [metric], **STACKED_VIZ,
              **({} if metric == "value" else _share_viz(metric))}, exempt=not amounts)

    at_end("Exposure by asset class", "bar",
           "Net worth by asset class after look-through, at the window's end.",
           "asset_class", "asset_class", dimension="asset_class")
    over_time("Asset classes over time",
              "Each month end's net worth by asset class after look-through, as "
              "shares stacking to 100.", "asset_class", "asset_class")
    at_end("Exposure by sector", "bar",
           "Net worth by sector at the window's end. Unknown is what no "
           "source describes.", "sector", "sector", dimension="sector")
    at_end("Exposure by region", "row",
           "Net worth by region of risk at the window's end. Metals and coins "
           "have no country.", "region", "region", dimension="region")
    over_time("Regions over time",
              "Each month end's net worth by region, as shares stacking to 100.",
              "region", "region")
    card("Largest country deviations", "table",
         "The twenty countries furthest from the picked benchmark's share, in "
         "percentage points. Metals, coins and unknowns are not placed.",
         last + ",\np AS (\n"
         "  SELECT country, max(country_name) AS country_name, sum(share_pct) AS share_pct\n"
         "    FROM e\n   WHERE country NOT IN ('(none)', 'unknown')\n   GROUP BY 1),\n"
         "b AS (\n"
         "  SELECT bucket AS country, share_pct AS benchmark_pct\n"
         "    FROM benchmark_exposure\n"
         f"   WHERE {PICKED_BENCHMARK} AND dimension = 'country' AND bucket NOT IN ('(none)', 'unknown'))\n"
         "SELECT COALESCE(p.country, b.country) AS country, p.country_name,\n"
         "       COALESCE(p.share_pct, 0) AS share_pct, COALESCE(b.benchmark_pct, 0) AS benchmark_pct,\n"
         "       COALESCE(p.share_pct, 0) - COALESCE(b.benchmark_pct, 0) AS difference_pct\n"
         "  FROM p FULL JOIN b ON b.country = p.country\n"
         " WHERE {{benchmark}} <> '" + NO_BENCHMARK + "'\n"
         " ORDER BY abs(difference_pct) DESC, 1\n LIMIT 20",
         _share_viz("share_pct", "benchmark_pct", "difference_pct"), exempt=True, hide_empty=True)
    card("Exposure by country", "map",
         "Net worth by country of risk at the window's end. Metals, coins "
         "and unknowns are not mapped.",
         end + f"SELECT country,\n       country_name{money},\n       sum(share_pct) AS share_pct\n"
         "  FROM e\n WHERE country NOT IN ('(none)', 'unknown')\n GROUP BY ALL\n"
         " ORDER BY sum(share_pct) DESC, 1",
         {"map.type": "region", "map.region": "world_countries", "map.dimension": "country",
          "map.metric": "share_pct", **_share_viz("share_pct")})
    card("Exposure by region and country",
         "table",
         "Each region's total, then its countries, at the window's end, "
         "largest first.",
         end + "SELECT * EXCLUDE (region_rank)\n"
         f"  FROM (SELECT region, country, country_name{money},\n"
         "               sum(share_pct) AS share_pct,\n"
         "               sum(sum(share_pct)) OVER (PARTITION BY region) AS region_rank\n"
         "          FROM e\n"
         "         GROUP BY GROUPING SETS ((region), (region, country, country_name)))\n"
         " ORDER BY region_rank DESC, region, country IS NOT NULL, share_pct DESC, country",
         _share_viz("share_pct"))
    at_end("Exposure by legal situs", "bar",
           "Net worth by legal situs at the window's end. No fund is looked "
           "through, so funds count whole.", "situs", "situs")
    # A currency under one percent on both sides joins `other`, as a ring's
    # small slices fold: a bar per currency would leave most too thin to read.
    card("Exposure by currency", "bar",
         "Net worth by the currency each value moves with at the window's end, those "
         "under one percent on both sides as other.",
         last + ",\n"
         f"b AS (SELECT bucket, share_pct FROM benchmark_exposure\n WHERE {PICKED_BENCHMARK} AND dimension = 'currency'),\n"
         "s AS (SELECT exposure_currency AS bucket, sum(share_pct) AS share_pct FROM e GROUP BY 1),\n"
         "kept AS (SELECT bucket FROM s WHERE abs(share_pct) >= 1\n"
         "         UNION SELECT bucket FROM b WHERE abs(share_pct) >= 1)\n"
         f"SELECT {', '.join(('currency', 'series', *amount, 'share_pct'))}\n  FROM (\n"
         "SELECT CASE WHEN exposure_currency IN (SELECT bucket FROM kept) THEN exposure_currency ELSE 'other' END"
         f" AS currency{money},\n       sum(share_pct) AS share_pct, '{PORTFOLIO_SERIES}' AS series\n"
         "  FROM e\n GROUP BY 1\n"
         "UNION ALL BY NAME\n"
         "SELECT CASE WHEN bucket IN (SELECT bucket FROM kept) THEN bucket ELSE 'other' END AS currency,\n"
         f"       sum(share_pct) AS share_pct, '{BENCHMARK_SERIES}' AS series\n"
         "  FROM b\n GROUP BY 1)\n"
         f" ORDER BY series <> '{PORTFOLIO_SERIES}', currency = 'other', share_pct DESC, 1",
         bars("currency", "share_pct", beside=True))
    over_time("Currencies over time",
              "Each month end's net worth by the currency it moves with, as "
              "shares stacking to 100. A currency sold forward can read below "
              "zero." if privacy else
              "Each month end's net worth by the currency it moves with, in "
              "the picked currency. A currency sold forward reads below zero.",
              "exposure_currency", "currency", fold=True, amounts=True)
    over_time("Held directly or through funds",
              "Each month end's net worth, held directly or through funds.",
              "held", "held")
    card("Public equity by size", "bar",
         "Public equity by company size at the window's end, large from 10bn "
         "USD market cap, mid from 2bn.",
         last + f",\np AS (\nSELECT size{money},\n"
         "       100 * sum(share_pct) / sum(sum(share_pct)) OVER () AS class_share_pct\n"
         "  FROM e\n WHERE asset_class = 'public_equity'\n GROUP BY 1)\n"
         + _beside("size", "class_share_pct", "size", amount, weight="100 * share_pct / sum(share_pct) OVER ()",
                   order="CASE size WHEN 'large' THEN 1 WHEN 'mid' THEN 2 WHEN 'small' THEN 3 ELSE 4 END",
                   keep_series=True),
         bars("size", "class_share_pct", beside=True))
    over_time("How the figures are classified",
              "Each month end's net worth by how its figures are classified. "
              "The lower the unknown band, the more the charts cover.", "how", "how")

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
    # holds them, the issuers and the securities, for today and the twelve
    # month ends before only, their look-through costing several times the
    # plain one.
    dtags = {**native_tags("web_exposure_debt", EXPOSURE_DEBT_FILTERS), "benchmark": BENCHMARK_TAG}
    # The debt view carries no asset class (EXPOSURE_DEBT_FILTERS).
    whole_class = " Its class is fixed, so it takes no Asset class."
    class_share = "100 * sum(value) / (SELECT CASE WHEN total > 0 THEN total END FROM c)"

    def class_select(dimension, label, cols):
        """The CTEs of a class_bars card over `dimension`, and its rows."""
        return (_window_end("web_exposure_debt", dtags, where=f"\n     AND dimension = '{dimension}'")[1] + ",\n"
                "c AS (SELECT sum(value) AS total FROM e WHERE bucket <> '(none)')",
                f"SELECT bucket AS {label}{cols}{money},\n       {class_share} AS class_share_pct\n"
                "  FROM e\n WHERE bucket <> '(none)'\n GROUP BY bucket\n"
                f"HAVING coalesce(abs({class_share}) >= {SMALLEST_BAR_PCT}, TRUE)")

    def class_bars(name, desc, dimension, label, buckets, extra=(), tooltip=(), beside=False):
        """A dimension's bars at the window's end, in the view's own
        bucket order: each bucket's share of its class, with the `extra`
        columns beside it and the `tooltip` ones in its tooltip. With
        `beside`, the picked benchmark's on the same reading."""
        head, select = class_select(dimension, label, "".join(f",\n       {c}" for c in extra))
        if beside:
            card(name, "bar", desc + whole_class,
                 head + ",\np AS (\n" + select + ")\n"
                 + _beside(label, "class_share_pct", dimension, (*tooltip, *amount),
                           weight="100 * share_pct / sum(share_pct) OVER ()", where="\n   AND bucket <> '(none)'",
                           order=f"list_position([{_sql_list(buckets)}], {label})", keep_series=True),
                 bars(label, "class_share_pct", tooltip, decimals=2, beside=True), tags=dtags)
            return
        card(name, "bar", desc + whole_class,
             head + "\n" + select + f"\n ORDER BY list_position([{_sql_list(buckets)}], bucket)",
             bars(label, "class_share_pct", tooltip, decimals=2), tags=dtags)

    grade = (f"CASE WHEN bucket IN ({_sql_list(INVESTMENT_GRADE)}) THEN 'IG'"
             f" WHEN bucket IN ({_sql_list(HIGH_YIELD)}) THEN 'HY' END AS grade")
    class_bars("Debt by credit rating, share of debt and cash",
               "Counts debt and cash by rating at the window's end. It covers the last twelve months.",
               "credit", "rating", CREDIT_BUCKETS,
               extra=(grade, "100 * sum(abs(value)) FILTER (WHERE unknown) / NULLIF(sum(abs(value)), 0)"
                             " AS unknown_pct"),
               tooltip=("grade", "unknown_pct"), beside=True)
    class_bars("Debt by maturity",
               "Counts debt and cash by term at the window's end. It covers the last twelve months.",
               "maturity", "maturity", MATURITY_BUCKETS, beside=True)
    class_bars("Debt by coupon kind",
               "Counts debt by coupon kind at the window's end. It covers the last twelve months.",
               "coupon", "coupon", COUPON_BUCKETS)
    class_bars("Public equity by cycle",
               "Counts public equity by cycle at the window's end. It covers the last twelve months.",
               "cycle", "cycle", CYCLE_BUCKETS)

    # The securities, each summed over its slices of the selection
    # (web_exposure_securities): the labels of its heaviest slice that
    # has them, the greater label on a tie, the funds counted once, as
    # `wealthdb exposure securities` gives them with nothing picked. The
    # share is of the selection, all of its securities counted. The twin
    # drops the values and hides a key that is a holding's own, which
    # can carry a contract's or an account's id. The name shows, less any
    # address.
    stags = {**native_tags("web_exposure_securities", EXPOSURE_FILTERS), "benchmark": BENCHMARK_TAG}
    hidden = f"security LIKE '%{HOLDING_KEY_PREFIX}%'"
    shown = ((f"CASE WHEN {hidden} THEN '(unnamed)' ELSE security END AS security,\n"
              f"       {twin_name('name')} AS name")
             if privacy else "security, name")
    values = ("100 * direct / t.total AS direct_pct,\n       100 * via / t.total AS via_funds_pct"
              if privacy else "amount AS value, direct AS direct_value, via AS via_funds_value")
    card("Top securities", "table",
         "The hundred largest securities at the window's end, held directly and "
         "through funds, added up. It covers the last twelve months. A benchmark "
         "adds its weight and the difference.",
         _columns_with_benchmark(
             _window_end("web_exposure_securities", stags)[1] + ",\n"
             "t AS (SELECT NULLIF(sum(value), 0) AS total FROM e),\n"
             "g AS (\n"
             "  SELECT security, max(name) AS name, max(ticker) AS ticker,\n"
             "         arg_max(asset_class, (class_weight, asset_class)) AS asset_class,\n"
             "         arg_max(sector, (sector_weight, sector)) AS sector,\n"
             "         arg_max(country, (country_weight, country)) AS country,\n"
             "         max(market_cap) AS market_cap, max(size) AS size, sum(value) AS amount,\n"
             "         sum(direct_value) AS direct, sum(via_funds_value) AS via,\n"
             "         len(list_distinct(flatten(list(funds)))) AS funds\n"
             "    FROM e\n   GROUP BY security)\n"
             f"SELECT {shown},\n       ticker, asset_class, sector, country, market_cap, size,\n"
             f"       {values}, funds,\n"
             "       100 * amount / t.total AS share_pct,\n"
             "       COALESCE(b.share_pct, 0) AS benchmark_pct,\n"
             "       100 * amount / t.total - COALESCE(b.share_pct, 0) AS difference_pct,\n"
             "       row_number() OVER (ORDER BY abs(amount) DESC, g.security) AS sort_rank\n"
             "  FROM g CROSS JOIN t\n"
             "  LEFT JOIN benchmark_exposure b ON b.benchmark = {{benchmark}} AND b.dimension = 'security'"
             " AND b.bucket = g.security\n ORDER BY sort_rank\n LIMIT 100",
             "sort_rank", "benchmark_pct", "difference_pct"),
         {**_share_viz("share_pct", "benchmark_pct", "difference_pct",
                       *(("direct_pct", "via_funds_pct") if privacy else ())),
          **_table_columns("security", "name", "ticker", "asset_class", "sector", "country", "market_cap", "size",
                           *(("direct_pct", "via_funds_pct") if privacy else
                             ("value", "direct_value", "via_funds_value")),
                           "funds", "share_pct", "benchmark_pct", "difference_pct")}, tags=stags)


def issuer_cards(f):
    """The Allocation dashboard's issuers card, or with `privacy` its
    privacy variant: name -> (display, description, dataset_query, viz).
    Native SQL over web_exposure_issuers: the largest issuers of the
    whole book, per reporting currency, today and at the twelve month
    ends before. The dashboard's as-of day reaches it through
    web_history_calendar (_history_day), and the card reads the latest
    instant on or before that history day. The issuers view carries no
    source, tax wrapper or asset class, so those pickers do not reach it.
    A security no source names reads its key, which the privacy card
    hides where it is a holding's own; the privacy card also
    drops the value."""
    privacy = f.privacy
    tags = {"currency": currency_tag(), "benchmark": BENCHMARK_TAG, **_calendar_tags()}
    tail = PRIVACY_DESC if privacy else ""
    card = f.using(tags=tags, tail=tail).card
    weight = ("(SELECT max(b.share_pct) FROM benchmark_exposure b\n"
              f"         WHERE b.{PICKED_BENCHMARK} AND b.dimension = 'issuer'\n"
              "           AND (b.bucket = 'lei:' || e.lei OR upper(b.label) = upper(e.issuer)))")
    issuer = (f"CASE WHEN issuer LIKE '{HOLDING_KEY_PREFIX}%' THEN '(unnamed)' ELSE issuer END"
              if privacy else "issuer")
    value = "" if privacy else ",\n       value"
    card("Top 100 issuers", "table",
         "The hundred largest issuers as of the day, a company's stock, bonds and deposits together. "
         "A day over a year back shows none.",
         _columns_with_benchmark(
             "WITH d AS (SELECT " + _history_day() + " AS day),\n"
             "w AS (\n  SELECT *\n    FROM web_exposure_issuers\n   WHERE currency = {{currency}}\n"
             "     AND as_of_day <= (SELECT day FROM d)),\n"
             "e AS (SELECT * FROM w WHERE as_of_day = (SELECT max(as_of_day) FROM w))\n"
             f"SELECT rank,\n       {issuer} AS issuer{value},\n"
             "       share_pct, cumulative_share_pct, equity_pct, debt_pct, cash_pct,\n"
             f"       COALESCE({weight}, 0) AS benchmark_pct,\n"
             f"       share_pct - COALESCE({weight}, 0) AS difference_pct,\n"
             "       rank AS sort_rank\n"
             "  FROM e\n WHERE issuer <> '(none)'\n ORDER BY rank\n LIMIT 100",
             "sort_rank", "benchmark_pct", "difference_pct"),
         {**_share_viz("share_pct", "cumulative_share_pct", "equity_pct", "debt_pct", "cash_pct",
                       "benchmark_pct", "difference_pct"),
          **_table_columns("rank", "issuer", *(() if privacy else ("value",)), "share_pct",
                           "benchmark_pct", "difference_pct",
                           "cumulative_share_pct", "equity_pct", "debt_pct", "cash_pct")})


# The key gold gives a holding no security key names
# (gold.HoldingKeyPrefix): it can carry an account's id.
HOLDING_KEY_PREFIX = "holding:"

# The exposure views' buckets in their order (the report_exposure_credit,
# _maturities, _coupons and _cycles macros), each card's bars drawn in it,
# and the grades of the credit buckets.
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
# yield_realized_pct). The days are not in the table, so the engine's
# thin rule for a row does not apply to a selection.
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


def yield_cards(f):
    """The Yield dashboard's cards, or with `privacy` their privacy
    variants: name -> (display, description, dataset_query, viz). Native
    SQL over the yield report's figures per holding (web_yield), a row set
    per reporting currency and instant (today and the last second of each
    of the twelve months before), so {{currency}} is a row filter, as on
    Exposure. A figure as of a day reads the latest day in the window, and
    a rate weighs the rows that state it as yield.summarize does (_rate),
    so a figure is `wealthdb yield global`'s over the same selection.

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
    privacy = f.privacy
    tags = {**native_tags("web_yield", YIELD_FILTERS), "benchmark": BENCHMARK_TAG}
    # Today's selected holdings, whatever the window.
    held_tags = native_tags("web_yield", {k: v for k, v in YIELD_FILTERS.items() if k != "time_range"})
    note = PRIVACY_DESC if privacy else ""
    proj_note = " It reads today's projection." + note
    card = f.using(tags=tags, tail=note).card
    w, end = _window_end("web_yield", tags)
    money = "" if privacy else ",\n       sum(value) AS value"
    # A group's share of the value that earns, so a liability's group
    # does not turn the assets' shares past 100.
    share = f",\n       100 * {EARNING_VALUE} / NULLIF(sum({EARNING_VALUE}) OVER (), 0) AS share_pct"

    def scalar(name, rate, alias, desc):
        """The selection's `rate` at the window's end, as `alias`."""
        card(name, "scalar", desc, end + f"\nSELECT {rate} AS {alias}\n  FROM e", _share_viz(alias), exempt=True)

    # ---- the projection: today's holdings and their parts
    proj_tags = {**held_tags, **PROJECTION_TAGS}
    proj = f.using(tags=proj_tags, tail=proj_note).card
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

    proj("Income yield", "scalar",
         "Today's holdings' expected yearly income over the horizon, over the value that "
         "earns it.",
         horizon + horizon_yield, _share_viz("income_yield_pct"), exempt=True, home="Yield")
    if privacy:
        # As a yield it would be the Income yield beside it, so it shows
        # how evenly the horizon pays instead.
        proj("Projected income", "scalar",
             "Expected monthly income as a share of its biggest month.",
             horizon.rstrip("\n") + ",\n" + _peak_cte("x", "income") +
             "SELECT 100 * (SELECT sum(income) FROM x) / " + HORIZON_MONTHS
             + f"\n       / {PEAK} AS monthly_average_pct",
             _share_viz("monthly_average_pct"))
    else:
        proj("Projected income", "scalar",
             "What today's holdings are expected to pay over the horizon after this month.",
             horizon + "SELECT sum(income) AS projected_income\n  FROM x", {})
    scalar("Implied yield", _rate("implied_pct"), "implied_yield_pct",
           "The yield the prices imply at the window's end, weighted by value over the "
           "holdings that have one.")
    scalar("Realized yield", REALIZED_YIELD, "realized_yield_pct",
           "Income received in the twelve months to the window's end, over the mean daily "
           "value. A day not held counts as zero.")

    card("Yield by currency", "bar",
         "Income yield beside implied yield per currency at the window's end."
         + (" The tooltip gives each currency's share." if privacy else ""),
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
    cash_desc = ("Each cash holding's rate against its benchmark at the window's end, worst "
                 "paid first.")
    if privacy:
        card("Cash against its benchmark", "table",
             cash_desc + " No account is named.",
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
         "Each bond and rate-sensitive fund at the window's end, duration across, implied "
         f"yield up, sized by {'share' if privacy else 'value'}.",
         end + "\nSELECT holding_currency AS currency, duration, implied_pct AS implied_yield_pct, symbol, "
         + (f"{twin_name('name')} AS name,\n       100 * value / NULLIF(sum(value) OVER (), 0) AS share_pct"
            if privacy else "name,\n       value") +
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
    if privacy:
        # A share of net worth per basis point: the selection's DV01 over
        # its net worth that day. The curve rows carry no holding, so they
        # are the ones whose source, wrapper and class the selected
        # holdings carry.
        card("DV01 along the curve", "bar",
             "What one basis point more at each tenor takes off net worth, as a share, per "
             "curve currency.",
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
             "What one basis point more at each tenor takes off the holdings' value, per "
             "curve currency. A liability's is negative.",
             _window_end("web_yield_curve", ctags)[1] + grid.format(dv01="g.dv01 AS dv01"),
             {**grouped, "graph.metrics": ["dv01"]}, tags=ctags)

    # The change in the horizon's income for rates one percent higher than
    # the picked path and shift: each part's income there less its income
    # here, as the engine's income per 100bp reads it. A floating rate
    # floors at zero, so below zero a point more earns less than a point.
    up = PART_INCOME.replace("({{shift}})", "({{shift}} + 100)")
    sens = (held + ",\ns AS (\n"
            f"  SELECT p.holding_currency, (({up}) - ({PART_INCOME})) * {_ccy_case('p.fx')} AS v\n" + PARTS_FROM +
            "\n" + HELD_JOIN + PARTS_AHEAD + ")")
    if privacy:
        proj("Income sensitivity to rates", "bar",
             "The yield change at rates one percent higher, per currency, in percentage points.",
             sens + ",\nv AS (SELECT holding_currency, " + EARNING_VALUE + " AS earning FROM h GROUP BY 1)\n"
             "SELECT s.holding_currency,\n"
             "       100 * sum(s.v) * 12 / " + HORIZON_MONTHS + " / NULLIF(any_value(v.earning), 0) AS yield_change_pct\n"
             "  FROM s\n  LEFT JOIN v ON v.holding_currency = s.holding_currency\n"
             " GROUP BY 1\n ORDER BY 2 DESC NULLS LAST, 1",
             {"graph.dimensions": ["holding_currency"], "graph.metrics": ["yield_change_pct"],
              "graph.show_values": True, **_share_viz("yield_change_pct")})
    else:
        proj("Income sensitivity to rates", "bar",
             "How much more today's holdings would pay over the horizon at rates one percent "
             "higher, per currency.",
             sens + "\nSELECT holding_currency, sum(v) AS income_change\n"
             "  FROM s\n GROUP BY 1\n ORDER BY 2 DESC NULLS LAST, 1",
             {"graph.dimensions": ["holding_currency"], "graph.metrics": ["income_change"],
              "graph.show_values": True})

    card("Dividend yield against payout", "scatter",
         "Each stock's payout ratio against its dividend yield at the window's end. A payout "
         "above 100% is not covered by earnings.",
         end + "\nSELECT DISTINCT symbol, name, payout_pct, dividend_yield_pct\n  FROM e\n"
         " WHERE kind = 'stock'\n   AND payout_pct IS NOT NULL AND dividend_yield_pct IS NOT NULL\n"
         " ORDER BY dividend_yield_pct DESC, symbol",
         {"graph.dimensions": ["payout_pct"], "graph.metrics": ["dividend_yield_pct"],
          "graph.tooltip_columns": ["symbol", "name"], **_share_viz("payout_pct", "dividend_yield_pct")}, exempt=True)

    # Two charts, not one: a yield and a duration differ in unit, and one
    # chart would need an axis for each.
    # A benchmark's figure is one more line, on the household's axis, so
    # the two read against one scale. No graph.metrics: the chart takes
    # the columns a run returns, so under None it has no such line.
    yields = ("income_yield_pct", "implied_yield_pct", "realized_yield_pct")
    picked = ("  LEFT JOIN (SELECT as_of, yield_pct AS benchmark_yield_pct, duration AS benchmark_duration\n"
              "               FROM benchmark_yield WHERE " + PICKED_BENCHMARK + ") b\n"
              "         ON CAST(epoch_ms(b.as_of * 1000) AS DATE) = w.as_of_day\n")
    card("Yields over time", "line",
         "The income, implied and realized yields at each month end in the window and "
         "today. A benchmark adds its funds' yield.",
         _columns_with_benchmark(
             w + f"\nSELECT as_of_day,\n       {_income_yield()} AS income_yield_pct,\n"
             f"       {_rate('implied_pct')} AS implied_yield_pct,\n"
             f"       {REALIZED_YIELD} AS realized_yield_pct,\n"
             "       any_value(benchmark_yield_pct) AS benchmark_yield_pct,\n"
             "       as_of_day AS sort_rank\n"
             "  FROM w\n" + picked + " GROUP BY 1",
             "sort_rank", "benchmark_yield_pct"),
         {"graph.dimensions": ["as_of_day"], **LINES_VIZ, **_share_viz(*yields, "benchmark_yield_pct"),
          "series_settings": {"benchmark_yield_pct": {"axis": "left"}}}, exempt=True)
    card("Duration over time", "line",
         "The holdings' value-weighted modified duration in years, at each month end in the "
         "window and today. A benchmark adds its funds' duration.",
         _columns_with_benchmark(
             w + f"\nSELECT as_of_day,\n       {_rate('duration')} AS duration,\n"
             "       any_value(benchmark_duration) AS benchmark_duration,\n"
             "       as_of_day AS sort_rank\n"
             "  FROM w\n" + picked + " GROUP BY 1",
             "sort_rank", "benchmark_duration"),
         {"graph.dimensions": ["as_of_day"], "series_settings": {"benchmark_duration": {"axis": "left"}}}, exempt=True)
    # A holding carries every tag that describes a figure of it, so the
    # shares overlap; each is of the gross value, a liability counted
    # whole (docs/YIELD.md §7).
    card("What the yields rest on", "row",
         "Gross value each quality tag covers at the window's end. Thin is a holding near zero most "
         "of the year.",
         end + ",\nt AS (SELECT unnest(tags) AS tag, abs(value) AS v FROM e)\n"
         "SELECT tag" + ("" if privacy else ",\n       sum(v) AS value") +
         ",\n       100 * sum(v) / NULLIF((SELECT sum(abs(value)) FROM e), 0) AS share_pct" +
         ("" if privacy else ",\n       count(*) AS holdings") +
         "\n  FROM t\n GROUP BY 1\n ORDER BY share_pct DESC, 1",
         {"graph.dimensions": ["tag"], "graph.metrics": ["share_pct"], "graph.show_values": True,
          **({} if privacy else {"graph.tooltip_columns": ["value", "holdings"]}), **_share_viz("share_pct")})


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
# Each type, by the income lines' detailed types it reads, as the
# engine's incomeTypeSQL maps a line: interest a bond pays is a coupon,
# a dividend a fund pays a distribution. So the Type picker, which lists
# the lines' labels, reaches the holdings' cards through this map, and a
# label of any other type selects nothing there.
INCOME_DETAILED_TYPES = {"interest": ("INCOME_INTEREST_EARNED",), "coupons": ("INCOME_INTEREST_EARNED",),
                         "dividends": ("INCOME_DIVIDENDS",),
                         "distributions": ("INCOME_DIVIDENDS", "INCOME_DISTRIBUTIONS"),
                         "rent": ("INCOME_RENTAL",), "pension": ("INCOME_RETIREMENT_PENSION",)}
# The detailed types the holdings' cards type, every other income line
# being one no holding pays.
PROJECTED_DETAILED = tuple(sorted({d for ds in INCOME_DETAILED_TYPES.values() for d in ds}))
# The cash flow statement's unresolved class, and its class of an
# income line no other class names.
UNCATEGORIZED_CLASS = "(uncategorized)"
OTHER_RECEIPTS = "other_receipts"
# The operating-in class of an income line no holding pays, as the cash
# flow statement classes it (cashflow_txn_nodes), and as the cash flow
# projection keeps its other income (0169): a line nothing placed,
# labelled UNCATEGORIZED, is the unresolved class, any other one other
# receipts. A test holds it to the macro's.
INCOME_LINE_CLASSES = {"earnings": ("INCOME_SALARY", "INCOME_GIG_ECONOMY", "INCOME_CONTRACTOR"),
                       "yield": ("INCOME_STAKING", "INCOME_ROYALTIES", "INCOME_ENERGY_FEED_IN"),
                       "benefits": ("INCOME_GOVERNMENT_BENEFITS", "INCOME_UNEMPLOYMENT",
                                    "INCOME_LONG_TERM_DISABILITY", "INCOME_MILITARY", "INCOME_CHILD_SUPPORT",
                                    "INCOME_ALIMONY")}
# Each class's band on the by-month chart, and its colour: received in
# it, its run-rate in a lighter shade. A greyer palette than the six
# types', so the holdings' bands stay the ones that read first.
INCOME_CLASS_BANDS = {"earnings": "salary and earnings", "yield": "other yield", "benefits": "benefits",
                      OTHER_RECEIPTS: "other receipts", UNCATEGORIZED_CLASS: "uncategorized"}
INCOME_CLASS_COLORS = {"earnings": "#6f4fd8", "yield": "#0b8aa8", "benefits": "#8c5a3c",
                       OTHER_RECEIPTS: "#6b6b6b", UNCATEGORIZED_CLASS: "#a3a3a3"}


def _line_class(col="income_detailed"):
    """The SQL of the INCOME_LINE_CLASSES class of an income line by its
    detailed type `col`."""
    return ("CASE " + " ".join(f"WHEN {col} IN ({_sql_list(ds)}) THEN '{c}'" for c, ds in INCOME_LINE_CLASSES.items())
            + f" WHEN {col} = '{UNCATEGORIZED}' THEN '{UNCATEGORIZED_CLASS}' ELSE '{OTHER_RECEIPTS}' END")


def _class_band(col):
    """The SQL of the band of class `col` (INCOME_CLASS_BANDS)."""
    return "CASE " + " ".join(f"WHEN {col} = '{c}' THEN '{b}'" for c, b in INCOME_CLASS_BANDS.items()) + " END"


# The keys the engine gives a cash balance (`cash:<ccy>`) and an
# account's income (`income:<type>:<ccy>`), which name no holding.
SHOWN_KEY_PREFIXES = ("cash:", "income:")
# A position's name on a privacy card: its own less any address
# (twin_name), else its key where that starts with one of
# SHOWN_KEY_PREFIXES. Any other key is a holding's own, which can carry a
# contract's or an account's id, so it reads (unnamed), as the issuers
# twin hides one.
TWIN_POSITION_NAME = (f"CASE WHEN name IS NOT NULL THEN {twin_name('name')} WHEN "
                      + " OR ".join(f"position_key LIKE '{k}%'" for k in SHOWN_KEY_PREFIXES)
                      + " THEN position_key ELSE '(unnamed)' END")


def _tint(color, share=0.55):
    """`color` mixed with white by `share`: a series' lighter shade."""
    rgb = [int(color[i:i + 2], 16) for i in (1, 3, 5)]
    return "#" + "".join(f"{round(c + (255 - c) * share):02x}" for c in rgb)


def income_projected_cards(f):
    """The Income dashboard's cards of received and expected income, or
    with `privacy` their privacy variants: name -> (display, description,
    dataset_query, viz). Native SQL over four sources, each a row set per
    reporting currency or a rate to it:

    - the six types the holdings receive (web_yield_received), each under
      its holding, else its account;
    - every other income line (web_income), under its account and banded
      by its operating-in class (INCOME_LINE_CLASSES);
    - what the holdings are expected to pay (web_yield_parts, under the
      projection's pickers);
    - the other income's run-rate (web_cashflow_plan's pool_income
      and outside_income), by class and account: the average of the
      income lines on every account a line arrives on.

    What was received is the Time range's: the picker reaches a card
    through the days web_income_days lists. What is expected is the
    Horizon's: the rest of the current month and the horizon's months
    after it (docs/INCOME.md §12). In the current month a run-rate expects
    its monthly figure less what its class and account received so far.
    The tax booked (web_income_withheld) names no income type, so the
    withholding and the income after it take no Type picker.

    A privacy card shows a figure as a share of the income it covers, or
    of its biggest month; the holdings' income, and a position's, as a
    yield over the value it is earned on; and what the withholding takes
    as a share. No account label, amount, quantity or price reaches it,
    and it has no account picker."""
    privacy = f.privacy
    selection = {k: v for k, v in INCOME_PROJECTED_SELECTION.items()
                 if not (privacy and k == "account")}
    sel_tags = view_tags("web_income", {"type": ("income_label", "string/=")})
    for k, (view, col) in selection.items():
        sel_tags.update(view_tags(view, {k: (col, "string/=")}))
    # The six types the labels picked map to (INCOME_DETAILED_TYPES).
    picked_types = ("SELECT m.type FROM (VALUES " + ", ".join(
        f"('{d}', '{t}')" for t, ds in INCOME_DETAILED_TYPES.items() for d in ds)
        + ") m(detailed, type)\n                    WHERE m.detailed IN"
        " (SELECT income_detailed FROM web_income WHERE {{type}})")
    projected = _sql_list(PROJECTED_DETAILED)
    # The classes the labels picked map to, a projected type none.
    picked_classes = (f"SELECT {_line_class()} FROM web_income\n                    WHERE income_detailed NOT IN"
                      f" ({projected}) AND {{{{type}}}}")

    def kept(alias="", typed=True, by_account=False):
        """The optional clauses keeping a row whose source, account, tax
        wrapper and type the pickers select. A row `typed` False has no
        type and keeps any. A row `by_account` has no tax wrapper of its
        own, and keeps its account's, defaulted as the views default
        it."""
        out = []
        for k, (view, col) in selection.items():
            pick = f"SELECT {col} FROM {view} WHERE {{{{{k}}}}}"
            if by_account and k == "tax_wrapper":
                out.append(f"({alias}silver_source_id, {alias}account_external_id) IN"
                           " (SELECT silver_source_id, account_external_id FROM accounts"
                           f" WHERE COALESCE(tax_wrapper, 'taxable_personal') IN ({pick}))")
            else:
                out.append(f"{alias}{col} IN ({pick})")
        if typed:
            out.append(f"{alias}type IN ({picked_types})")
        return "".join(f"\n     [[AND {c}]]" for c in out)

    # A card with a received figure takes the Time range, one with an
    # expected figure the projection's four, a twin's share of the whole
    # both; the calendar takes the Position picker too. The withholding
    # and the income after it take no Type (untyped): a tax row has
    # none.
    range_tags = view_tags("web_income_days", {"time_range": ("day", "date/all-options")})
    received_tags = {"currency": currency_tag(), **range_tags, **sel_tags}
    expected_tags = {"currency": currency_tag(), **PROJECTION_TAGS, **sel_tags}
    both_tags = {**received_tags, **PROJECTION_TAGS}
    untyped_tags = {k: v for k, v in both_tags.items() if k != "type"}
    calendar_tags = {**expected_tags, "position": POSITION_TAG}

    card = f.using(tags=both_tags, tail=PRIVACY_DESC if privacy else "").card

    # The days the Time range selects, every day web_income_days lists
    # with none.
    days_from = "FROM web_income_days [[WHERE {{time_range}}]]"
    days = "SELECT day " + days_from
    # A line of web_income on the days the Time range selects, every line
    # with none picked, as Net income counts them.
    line_days = "\n     [[AND CAST(occurred_at AS DATE) IN (SELECT day FROM web_income_days WHERE {{time_range}})]]"
    # The rest of the current month, as a share of its days.
    month_rest = ("(SELECT (day(last_day(max(day))) - day(max(day))) / day(last_day(max(day)))"
                  " FROM web_income_days)")
    # The current month's first day, and the lines no holding pays
    # received from it to today, by class and account, whatever the Time
    # range.
    this_month = "(SELECT CAST(date_trunc('month', max(day)) AS DATE) FROM web_income_days)"
    this_month_got = ("mg AS (\n"
                      f"  SELECT {_line_class()} AS class, silver_source_id, account_external_id,\n"
                      f"         sum({_ccy_case('value')}) AS got\n"
                      f"    FROM web_income\n   WHERE CAST(occurred_at AS DATE) >= {this_month}\n"
                      "     AND CAST(occurred_at AS DATE) <= (SELECT max(day) FROM web_income_days)\n"
                      f"     AND income_detailed NOT IN ({projected})\n"
                      "   GROUP BY ALL)")

    # Each part's income in its holding's currency and its rate to the
    # picked one, over the horizon; a payment of an estimated day reads
    # its month.
    def expected(typed=True):
        return ("ex AS (\n"
                "  SELECT p.type, p.month_start, p.month AS m, CASE WHEN NOT p.month_only THEN p.day END AS day,\n"
                f"         p.holding_currency, ({PART_INCOME}) AS amount,\n"
                f"         {_ccy_case('p.fx')} AS fx, p.withholding_pct,\n"
                "         p.basis, p.reinvested, p.silver_source_id, p.account_external_id, p.position_key,\n"
                "         p.account_label, p.vehicle, p.symbol, p.name\n" + PARTS_FROM +
                "\n   WHERE p.rule = {{maturing}}\n     AND p.month <= " + HORIZON_MONTHS + kept("p.", typed) + ")")

    def received(typed=True):
        return ("rx AS (\n  SELECT * FROM web_yield_received\n   WHERE currency = {{currency}}\n"
                f"     AND day IN ({days})" + kept(typed=typed) + ")")

    # Every other income line received, by its account and class.
    def other(typed=True):
        return ("ox AS (\n"
                f"  SELECT CAST(occurred_at AS DATE) AS day, {_line_class()} AS class,\n"
                f"         silver_source_id, account_external_id, account_label, {_ccy_case('value')} AS value\n"
                f"    FROM web_income\n   WHERE income_detailed NOT IN ({projected})" + line_days
                + kept(typed=False) + ("\n     [[AND {{type}}]]" if typed else "") + ")")

    # The other income's run-rate over the horizon's months. A salary
    # arrives on a pay day, so the current month expects the monthly
    # figure less what its class and account received so far, never
    # below 0, rather than a share of its days.
    def run_rate(typed=True):
        keep = kept(typed=False, by_account=True) + (
            f"\n     [[AND class IN ({picked_classes})]]" if typed else "")
        # An account whose wrapper changed in the look-back holds a row
        # of each part: one figure per class, account and month.
        return ("pl AS (\n"
                "  SELECT c.month, c.class, c.silver_source_id, c.account_external_id,\n"
                "         any_value(c.month_start) AS month_start, sum(c.value) AS value,\n"
                "         account_label(ac.display_name, c.account_external_id, c.silver_source_id, ac.account_kind)"
                " AS account_label\n"
                "    FROM web_cashflow_plan c\n"
                "    LEFT JOIN accounts ac ON ac.silver_source_id = c.silver_source_id"
                " AND ac.account_external_id = c.account_external_id\n"
                f"   WHERE c.part IN ({_sql_list(RUN_RATE_PARTS)})\n     AND c.currency = {{{{currency}}}}\n"
                "   GROUP BY ALL),\n"
                + this_month_got + ",\n"
                "px AS (\n"
                "  SELECT month AS m, month_start, class, silver_source_id, account_external_id, account_label, value\n"
                "    FROM pl\n   WHERE month" + AFTER_THIS_MONTH + keep + "\n"
                "  UNION ALL\n"
                f"  SELECT 0, {this_month}, class,\n"
                "         silver_source_id, account_external_id, account_label, GREATEST(0, value - coalesce(mg.got, 0))\n"
                "    FROM pl\n    LEFT JOIN mg USING (class, silver_source_id, account_external_id)\n"
                f"   WHERE month = 1\n     AND {this_month} IS NOT NULL" + keep + ")")

    keys = "silver_source_id, account_external_id, position_key, account_label, vehicle, symbol, name"
    lined = ("silver_source_id, account_external_id, 'class:' || class AS position_key, account_label,\n"
             f"           NULL AS vehicle, NULL AS symbol, {_class_band('class')} AS name")
    arms = {"expected": (expected,
                         "SELECT 'expected' AS status, type, month_start, m, day, holding_currency, amount,\n"
                         "           amount * fx AS value, amount * fx * withholding_pct / 100 AS withheld,\n"
                         f"           {keys}\n      FROM ex"),
            "received": (received,
                         "SELECT 'received' AS status, type, CAST(date_trunc('month', day) AS DATE) AS month_start,\n"
                         "           NULL AS m, day, NULL AS holding_currency, NULL AS amount, value,\n"
                         f"           NULL AS withheld, {keys}\n      FROM rx"),
            "other": (other,
                      "SELECT 'received' AS status, class AS type, CAST(date_trunc('month', day) AS DATE) AS month_start,\n"
                      "           NULL AS m, day, NULL AS holding_currency, NULL AS amount, value, NULL AS withheld,\n"
                      f"           {lined}\n      FROM ox"),
            "run_rate": (run_rate,
                         "SELECT 'expected' AS status, class AS type, month_start, m, NULL AS day,\n"
                         "           NULL AS holding_currency, NULL AS amount, value, NULL AS withheld,\n"
                         f"           {lined}\n      FROM px")}
    # The holdings' halves, every line's received, every line's expected,
    # and the four.
    holdings = ("expected", "received")
    got, due = ("received", "other"), ("expected", "run_rate")
    every = (*got, *due)

    def rows(parts, *extra, typed=True):
        """The CTEs a card reads, ending with `a`, then the `extra` ones:
        each amount of the `parts` (arms) in the picked currency, an
        expected one of a holding with what the withholding table
        withholds from it; with `typed` False, of every type."""
        ctes = [arms[x][0](typed) for x in parts]
        ctes.append("a AS (\n    " + "\n    UNION ALL\n    ".join(arms[x][1] for x in parts) + ")")
        return "WITH " + ",\n".join(ctes + list(extra)) + "\n"

    # The months the figures cover: each day received its share of its
    # month, and the horizon's months with the rest of this one.
    months = ("mo AS (\n"
              f"  SELECT (SELECT coalesce(sum(1 / day(last_day(day))), 0) {days_from}) AS received,\n"
              f"         {HORIZON_MONTHS} + coalesce({month_rest}, 0) AS expected)")
    # The tax a source withheld on the days received, which the
    # withholding table does not model.
    taxed = ("tx AS (\n  SELECT sum(" + _ccy_case("value") + ") AS withheld\n    FROM web_income_withheld\n"
             f"   WHERE day IN ({days})" + kept(typed=False, by_account=True) + ")")
    # Every income line received on those days, Net income's lines, of
    # any type: the income the tax booked was withheld from.
    every_line = ("li AS (\n  SELECT coalesce(sum(" + _ccy_case("value") + "), 0) AS income\n    FROM web_income\n"
                  "   WHERE TRUE" + line_days + kept(typed=False) + ")")
    # The biggest month, for the twins (PEAK).
    peak = _peak_cte("a", "value", "month_start").rstrip()

    # ---- the headline figures
    def tile(name, col, cond, desc, half=None, home=None):
        """The income of the rows `cond` selects: what was received over
        the Time range with `half` 'received', what is expected over the
        Horizon with 'expected', both without one. On the privacy card,
        its share of what was received, or of both."""
        pick = f" FILTER (WHERE {cond})" if cond else ""
        parts = {"received": got, "expected": due, None: every}[half]
        if privacy:
            parts, tags, of = ((got, received_tags, "all income received") if half == "received"
                               else (every, both_tags, "the income received and expected"))
            card(name, "scalar", desc + f" A share of {of}.",
                 rows(parts) + f"SELECT coalesce(100 * coalesce(sum(value){pick}, 0) / NULLIF(sum(value), 0), 0)"
                 f" AS {col}_pct\n  FROM a", _share_viz(f"{col}_pct"), tags=tags, home=home)
        else:
            tags = {"received": received_tags, "expected": expected_tags, None: both_tags}[half]
            card(name, "scalar", desc, rows(parts) + f"SELECT coalesce(sum(value){pick}, 0) AS {col}\n  FROM a",
                 {}, tags=tags, home=home)

    interest, dividends = (f"type IN ({_sql_list(t)})" for t in (INTEREST_TYPES, DIVIDEND_TYPES))
    expect = "status = 'expected'"
    if privacy:
        h = ("h AS (SELECT value, weighs FROM web_yield\n"
             "        WHERE today\n          AND currency = {{currency}}" + kept(typed=False) + ")")
        card("Holdings' income yield", "scalar",
             "The holdings' income over the Time range and the Horizon, as a yield on their value today.",
             rows(holdings, months, h) + "SELECT coalesce(100 * (SELECT sum(value) FROM a) * 12"
             " / (SELECT received + expected FROM mo)\n"
             f"       / NULLIF((SELECT {EARNING_VALUE} FROM h), 0), 0) AS income_yield_pct",
             _share_viz("income_yield_pct"))
        card("Monthly average income", "scalar", "The income per month, as a share of the biggest month.",
             rows(every, months, peak) +
             "SELECT coalesce(100 * (SELECT sum(value) FROM a) / (SELECT received + expected FROM mo)\n"
             f"       / {PEAK}, 0) AS monthly_average_pct",
             _share_viz("monthly_average_pct"))
    else:
        tile("Income in the window", "income", "",
             "Every income line received over the Time range, and the income expected over the Horizon.")
        card("Monthly average income", "scalar",
             "The income in the window over the months the Time range and the Horizon cover.",
             rows(every, months) + "SELECT coalesce(sum(value), 0) / (SELECT received + expected FROM mo)"
             " AS monthly_average\n  FROM a", {})
    tile("Income received", "received", "", "Every income line received over the Time range.", "received",
         home="Income")
    tile("Holdings' interest received", "received_interest", interest,
         "The interest and coupons the holdings received over the Time range.", "received")
    tile("Holdings' dividends received", "received_dividends", dividends,
         "The dividends and fund distributions the holdings received over the Time range.", "received")
    tile("Income expected", "expected", expect,
         "What the holdings are expected to pay over the Horizon, and the other income's run-rate.",
         "expected", home="Income")
    tile("Holdings' interest expected", "expected_interest", f"{expect} AND {interest}",
         "The interest and coupons the holdings are expected to pay over the Horizon.", "expected")
    tile("Holdings' dividends expected", "expected_dividends", f"{expect} AND {dividends}",
         "The dividends and fund distributions the holdings are expected to pay over the Horizon.", "expected")

    # What was withheld from the income received is the tax booked on the
    # income's accounts, every negative tax row there, as the CLI's
    # `withheld` memo counts it; what will be withheld from the income
    # expected is what the config's withholding table withholds at each
    # holding's situs and account wrapper, a model. A line the table names
    # no rate for withholds nothing, a run-rate neither, and with no table
    # the expected part is nothing. The tax booked is on any income line,
    # so the income it is set against is every line received (li) and
    # all income expected. Neither half takes the Type picker: a tax row
    # names no income type.
    withheld = "coalesce((SELECT withheld FROM tx), 0) + coalesce(sum(withheld), 0)"
    income = "(SELECT income FROM li) + coalesce(sum(value), 0)"
    untyped = rows(due, taxed, every_line, typed=False)
    no_type = " A tax line has no Type."
    withheld_desc = "Tax withheld, booked over the Time range and modelled over the Horizon." + no_type
    net_desc = "The income in the window less the withholding." + no_type
    if privacy:
        card("Withholding", "scalar", withheld_desc + " A share of the income.",
             untyped + f"SELECT coalesce(100 * ({withheld}) / NULLIF({income}, 0), 0) AS withheld_pct"
             "\n  FROM a", _share_viz("withheld_pct"), tags=untyped_tags)
        card("Income after withholding", "scalar", net_desc + " A share of the income.",
             untyped + f"SELECT coalesce(100 * ({income} - ({withheld})) / NULLIF({income}, 0), 0)"
             " AS net_pct\n  FROM a", _share_viz("net_pct"), tags=untyped_tags)
    else:
        card("Withholding", "scalar", withheld_desc,
             untyped + f"SELECT {withheld} AS withheld\n  FROM a", {}, tags=untyped_tags)
        card("Income after withholding", "scalar", net_desc,
             untyped + f"SELECT {income} - ({withheld}) AS net\n  FROM a", {}, tags=untyped_tags)

    # ---- received and expected by month, on one axis: a band per
    # holding's type and status, and per class of the other lines, the
    # expected one a run-rate.
    series = (f"CASE WHEN type IN ({_sql_list(INCOME_TYPE_COLORS)}) THEN type || ' (' || status || ')'\n"
              f"            ELSE {_class_band('type')} || CASE WHEN status = 'received' THEN ' (received)'"
              " ELSE ' (expected, run-rate)' END END AS series")
    colors = {**{f"{t} ({s})": {"color": c if s == "received" else _tint(c)}
                 for t, c in INCOME_TYPE_COLORS.items() for s in ("received", "expected")},
              **{f"{INCOME_CLASS_BANDS[k]} ({s})": {"color": c if s == "received" else _tint(c)}
                 for k, c in INCOME_CLASS_COLORS.items() for s in ("received", "expected, run-rate")}}
    month_desc = ("Income per month by type, received in full colour, expected lighter. A run-rate band "
                  "expects the rest of this month. It ignores the Rate path, Maturing and Shift.")
    if privacy:
        sql = (rows(every, peak) + f"SELECT CAST(month_start AS TIMESTAMP) AS month, {series},\n"
               f"       100 * sum(value) / {PEAK} AS income_pct\n  FROM a\n GROUP BY 1, 2")
        metric = "income_pct"
    else:
        sql = (rows(every) + f"SELECT CAST(month_start AS TIMESTAMP) AS month, {series},\n"
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
    account_desc = "Each account's income received, expected and in all, the largest first."
    if privacy:
        card("Received and expected by account", "table",
             account_desc + " Ranked, no account named, each a share of every account's total.",
             rows(every, acc) + "SELECT row_number() OVER (ORDER BY total DESC) AS rank,\n" +
             ",\n".join(f"       100 * {c} / NULLIF(sum(total) OVER (), 0) AS {c}_pct"
                        for c in ("received", "expected", "total")) +
             "\n  FROM acc\n ORDER BY 1",
             _share_viz("received_pct", "expected_pct", "total_pct"))
    else:
        card("Received and expected by account", "table", account_desc,
             rows(every, acc) + "SELECT account_label AS account, received, expected, total\n"
             "  FROM acc\n ORDER BY total DESC, account", {})

    # Every holding of today, the ones that pay nothing included, and any
    # held no longer that received income over the Time range; a line no
    # holding pays under its account, a row per class (`class:<class>`).
    # With a Type picked, only the holdings that receive or are expected
    # to pay it.
    paying = ("\n     [[AND (silver_source_id, account_external_id, position_key) IN"
              "\n          (SELECT silver_source_id, account_external_id, position_key FROM web_yield_parts"
              f" WHERE type IN ({picked_types})\n           UNION ALL"
              "\n           SELECT silver_source_id, account_external_id, position_key FROM web_yield_received"
              f" WHERE type IN ({picked_types}))]]")
    hold = ("hold AS (\n"
            f"  SELECT {keys}, quantity, price, value\n"
            "    FROM web_yield\n   WHERE today\n     AND currency = {{currency}}" + kept(typed=False) + paying + ")")
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
    position_desc = ("Each position's income received and expected, by vehicle. A class row holds an "
                     "account's other lines and has no payments; click a holding's symbol or name to list its.")
    # A click on a position's symbol or name picks the position the
    # calendar below lists, by the value its cell shows; the twin's
    # calendar also matches the name the twin shows (TWIN_POSITION_NAME).
    clicks = (Link("symbol", "position"), Link("name", "position"))
    # A position's income over the value it is earned on: none for a
    # position no longer held, or of no positive value.
    on_value = "NULLIF(GREATEST(value, 0), 0)"
    if privacy:
        per_year = {"received": "(SELECT received FROM mo)", "expected": "(SELECT expected FROM mo)",
                    "total": "(SELECT received + expected FROM mo)"}
        figure = {"received": "received", "expected": "expected", "total": "(received + expected)"}
        card("Received and expected by position", "table",
             "Each position's income as a yearly yield on its value today. A class row has no yield or "
             "payments; click a holding's symbol or name to list its.",
             rows(every, months, hold, inc, positions) + f"SELECT vehicle, symbol, {TWIN_POSITION_NAME} AS name,\n"
             "       100 * value / NULLIF(sum(GREATEST(value, 0)) OVER (), 0) AS share_pct,\n" +
             ",\n".join(f"       100 * {figure[c]} * 12 / NULLIF({per_year[c]}, 0) / {on_value} AS {c}_pct"
                        for c in figure) + position_order,
             _share_viz("share_pct", "received_pct", "expected_pct", "total_pct"), cell_clicks=clicks)
    else:
        card("Received and expected by position", "table", position_desc,
             rows(every, hold, inc, positions) +
             "SELECT vehicle, account_label AS account, symbol, COALESCE(name, position_key) AS name,\n"
             "       quantity, price, value, received, expected, received + expected AS total" + position_order, {}, cell_clicks=clicks)

    # Each payment the holdings are expected to pay over the Horizon, a
    # dated one on its day, an accrual or an estimated day in its month
    # after the month's dated ones, and each principal a maturity
    # returns. A run-rate is no payment, so the calendar has none. The
    # Position picker takes a symbol, a name or a key; on the twin also
    # the name the twin shows, so a click on (unnamed) lists every
    # unnamed position's payments.
    picked = "{{position}} IN (symbol, name, position_key" + (f", {TWIN_POSITION_NAME}" if privacy else "") + ")"
    cal = ("WITH " + expected() + ",\ncal AS (\n"
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
    cal_desc = ("Each payment and principal the holdings are expected to pay over the Horizon, by date. "
                "The Position picker, or a click above, lists one position's.")
    if privacy:
        card("Expected payments", "table", cal_desc,
             cal + ",\nhold AS (SELECT silver_source_id, account_external_id, position_key, value\n"
             "           FROM web_yield WHERE today AND currency = {{currency}})\n"
             f"SELECT {date}, symbol, {TWIN_POSITION_NAME} AS name, type, kind, basis,\n"
             "       100 * c.value / NULLIF(GREATEST(h.value, 0), 0) AS value_pct\n"
             "  FROM cal c\n  LEFT JOIN hold h USING (silver_source_id, account_external_id, position_key)\n"
             " ORDER BY month_start, day IS NULL, day, symbol, name, kind",
             _share_viz("value_pct"), tags=calendar_tags)
    else:
        card("Expected payments", "table", cal_desc,
             cal + f"SELECT {date}, account_label AS account, symbol, COALESCE(name, position_key) AS name,\n"
             "       type, kind, basis, holding_currency AS currency, amount, value\n"
             "  FROM cal\n ORDER BY month_start, day IS NULL, day, account, symbol, name, kind",
             {}, tags=calendar_tags)


# The parts of web_cashflow_plan the Income cards' run-rate reads: the
# income lines on a day their account sits in the cash pool and
# every other income line. So the run-rate reads the lines the
# received half reads. The cash flow projection reads the statement's
# other_income instead.
RUN_RATE_PARTS = ("pool_income", "outside_income")

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


def cashflow_plan_cards(f):
    """The Cash Flow dashboard's projection cards and the Wealth
    Overview's projected net, or with `privacy` their privacy variants:
    name -> (display, description, dataset_query, viz).
    `wealthdb cashflow projection` month by month over the Horizon
    (docs/CASHFLOW.md §12): the income projection's investment income and
    the principal its maturities return, read from today's projection
    (web_yield_parts, web_yield_rates, web_yield_events) under the four
    projection pickers as the Income cards read them, and split by the
    cash pool's accounts (web_cashflow_pool): the rest is the vehicles'
    income. They are set against the look-back's monthly averages a
    refresh stores (web_cashflow_plan). The cash flow projection starts
    with the month after this one, so month 0 never counts.

    The cards take the Currency and the projection's pickers and none of
    the dashboard's own: the look-back is fixed when a refresh writes
    it, so a Time range, Source, Section or Investing picker would move
    nothing.

    A privacy card shows each part as a share of all the income the
    projection expects over the Horizon, on every account; blank where it
    expects none."""
    privacy = f.privacy
    tags = {"currency": currency_tag(), **PROJECTION_TAGS}
    tail = (" Each part is a share of what the holdings are expected to pay." if privacy
            else " Its look-back averages ignore window, source, section and grain.")
    card = f.using(tags=tags, tail=tail).card

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
    card("Cash flow projection by month", "combo",
         "Expected income against past average outflows, month by month.",
         plan + "SELECT month,\n       " + ",\n       ".join(col(chart_expr[c], c) for c in chart_cols) +
         "\n  FROM proj\n ORDER BY month",
         {"graph.dimensions": ["month"], "graph.metrics": [name_of(c) for c in chart_cols], **STACKED_VIZ,
          "series_settings": {name_of(c): {"display": "line" if c in ("net", "vehicles_income") else "bar"}
                              for c in chart_cols},
          **(_share_viz(*(name_of(c) for c in chart_cols)) if privacy else {})})
    table_cols = ["investment_income", "principal_returned", "vehicles_income", *PLAN_LOOKBACK_PARTS, "net"]
    card("Cash flow projection", "table",
         "Each Horizon month's parts, net and quality.",
         plan + "SELECT month,\n       " + ",\n       ".join(col(c, c) for c in table_cols) +
         ",\n       quality\n  FROM proj\n ORDER BY month",
         _share_viz(*(name_of(c) for c in table_cols)) if privacy else {})
    # The Wealth Overview's figure of the projection: the net over the
    # Horizon, which no picker of that dashboard sets.
    card("Projected net cash flow", "scalar",
         "The Horizon's net addition to the cash pool.",
         plan + "SELECT " + col("sum(net)", "net") + "\n  FROM proj",
         _share_viz(name_of("net")) if privacy else {}, home="Cash Flow")


# The net worth bridge's colours: a rise green, a fall red, a total grey
# (BENCHMARK_COLOR).
BRIDGE_RISE, BRIDGE_FALL = "#1BAF7A", "#E0533D"
# The monthly bars by column, and the month's change as a line.
BRIDGE_MONTH_SERIES = {"net_saved": ("Net saved", "#2A78D6"), "tracking_changes": ("Tracking changes", "#A989C5"),
                       "investment_gain": ("Investment gain", BRIDGE_RISE)}
BRIDGE_FILTERS = {"time_range": ("month_end", "date/all-options")}


def bridge_cards(f):
    """The Cash Flow dashboard's bridge tiles, or with `privacy` their
    twins: name -> (display, description, dataset_query, viz). They read
    the months of `wealthdb returns bridge` (web_bridge), which sum to any
    window. A bridge exists for the whole household only, since returns do
    not add across grains, so the cards take the Currency and the Time
    range and decline Source, Section and Investing.

    The waterfall shows the window's changes alone, so its steps fill the
    chart, and two figures beside it state the net worth they lead from and
    to. Metabase's total bar is their sum and carries Metabase's own label.

    A twin card shows the same as shares of the starting net worth: the
    window's, or by month that month's."""
    privacy = f.privacy
    tags = native_tags("web_bridge", BRIDGE_FILTERS)
    tail = " It covers the whole household." + (" Shares of the start." if privacy else "")
    card = f.using(tags=tags, tail=tail).card
    window = ("WITH w AS (\n  SELECT * FROM web_bridge" + _native_where(tags)
              + "\n     AND currency = {{currency}})")
    start = "arg_min(start_net_worth, end_day)::DOUBLE"
    finish = "arg_max(end_net_worth, end_day)::DOUBLE"

    def share(x, of):
        return f"({x}) * 100 / NULLIF(abs({of}), 0)" if privacy else x

    def figure(name, desc, value):
        col = "net_worth_pct" if privacy else "net_worth"
        # Never compact: Metabase compacts a figure too wide for its tile,
        # and the two would then read differently.
        card(name, "scalar", desc, window + f"\nSELECT {value} AS {col}\n  FROM w",
             {"scalar.compact_primary_number": "never",
              **(_share_viz(col, decimals=1) if privacy
                 else {"column_settings": {f'["name","{col}"]': {"decimals": 0}}})})

    figure("Net worth at start", "Net worth where the Time range's first whole month starts.",
           share(start, start))
    figure("Net worth at end", "Net worth where the Time range's last whole month ends.",
           share(finish, start))

    amount = "amount_pct" if privacy else "amount"
    steps = (window + ",\ns AS (\n"
             f"  SELECT {start} AS start, sum(net_saved)::DOUBLE AS saved,\n"
             "         sum(onboarded)::DOUBLE AS tracked, sum(gain)::DOUBLE AS gained\n"
             "    FROM w)\n")
    arms = [("Net saved", "saved", ""), ("Tracking changes", "tracked", " WHERE tracked <> 0"),
            ("Investment gain", "gained", "")]
    card("Why net worth moved", "waterfall",
         "What moved net worth, by whole months: saving, tracking changes and gain.",
         steps + "\nUNION ALL\n".join(
             f"SELECT {i} AS i, '{label}' AS step, {col + ' / NULLIF(abs(start), 0)' if privacy else col}"
             f" AS {amount} FROM s{where}"
             for i, (label, col, where) in enumerate(arms)) + "\nORDER BY i",
         {"graph.dimensions": ["step"], "graph.metrics": [amount], "graph.x_axis.title_text": "",
          "graph.y_axis.title_text": "", "graph.show_values": True, "graph.label_value_formatting": "full",
          "waterfall.increase_color": BRIDGE_RISE, "waterfall.decrease_color": BRIDGE_FALL,
          "waterfall.total_color": BENCHMARK_COLOR, "waterfall.show_total": True,
          # Metabase's waterfall labels print a negative in parentheses,
          # with no setting. As a percent style the sign stays inside them.
          "column_settings": {f'["name","{amount}"]': {"number_style": "percent", "decimals": 1} if privacy
                              else {"decimals": 0}}})

    # One row a month, the change beside its parts. The twin divides each
    # by the month's own start.
    parts = {"net_saved": "net_saved", "tracking_changes": "onboarded", "investment_gain": "gain",
             "change": "end_net_worth - start_net_worth"}
    names = {c: f"{c}_pct" if privacy else c for c in parts}
    series = {names[c]: {"display": "bar", "color": color, "title": title}
              for c, (title, color) in BRIDGE_MONTH_SERIES.items()}
    series[names["change"]] = {"display": "line", "color": BENCHMARK_COLOR, "title": "Change in net worth"}
    card("Saved and gained by month", "combo",
         "Saving, tracking changes and gain per month. The line is net worth's change.",
         window + "\nSELECT CAST(date_trunc('month', month_end) AS TIMESTAMP) AS month,\n       " +
         ",\n       ".join(f"({share(e, 'start_net_worth')})::DOUBLE AS {names[c]}" for c, e in parts.items()) +
         "\n  FROM w\n ORDER BY month",
         {"graph.dimensions": ["month"], "graph.metrics": list(names.values()), **STACKED_VIZ,
          "graph.x_axis.title_text": "", "graph.y_axis.title_text": "", "series_settings": series,
          **(_share_viz(*names.values()) if privacy else {})})


# The fee tiles' detailed groups, in stacking order (gold's fee_group),
# and their colours.
FEE_GROUP_COLORS = {"investment": "#2A78D6", "bank": "#EB6834", "atm": "#1BAF7A", "other": "#8A99A8"}
FEE_GROUP_TITLES = {"investment": "Investment", "bank": "Bank", "atm": "ATM", "other": "Other"}


def fee_cards(f):
    """The Spending dashboard's two fee tiles, or with `privacy` their
    twins: name -> (display, description, dataset_query, viz). They read
    `wealthdb spending fees` over web_fees: the
    spending lines of primary BANK_FEES, so a vehicle's fees show, and the
    daily values, each entity a portfolio, else an account outside any.
    They take the card balance tile's pickers: fees are one category, so
    a Category picker has nothing to split.

    The return is the CLI's: the entity's monthly returns chained over
    the months that end inside the window. A picked account narrows the
    fees and the value, never the return.

    On a twin the rates and returns stay, the fees read as shares of the
    window's fees, and an entity is its source, its kind and its rank, as
    the Spend by account twin names a group, with no name of its own."""
    privacy = f.privacy
    tags = native_tags("web_fees", PRIVACY_FEE_FILTERS if privacy else FEE_FILTERS)
    tail = " Fees are one category." + (PRIVACY_DESC if privacy else "")
    card = f.using(tags=tags, tail=tail).card
    val = _ccy_case("value")
    rows = ("WITH f AS (\n  SELECT * FROM web_fees" + _native_where(tags) + "),\n"
            "e AS (\n  SELECT entity_key, any_value(silver_source_id) AS silver_source_id,\n"
            "         any_value(entity_kind) AS entity_kind, any_value(entity) AS entity,\n"
            f"         sum({val}) FILTER (WHERE line = 'fee')::DOUBLE AS fees,\n" +
            "".join(f"         coalesce(sum({val}) FILTER (WHERE fee_group = '{g}'), 0)::DOUBLE AS {g},\n"
                    for g in FEE_GROUP_COLORS) +
            f"         sum({val}) FILTER (WHERE line = 'value')::DOUBLE AS value_days,\n"
            "         count(DISTINCT day) FILTER (WHERE line = 'value') AS days\n"
            "    FROM f\n   GROUP BY 1\n  HAVING count(*) FILTER (WHERE line = 'fee') > 0),\n"
            "r AS (\n  SELECT entity_key, exp(sum(ln(1 + twr))) - 1 AS twr\n"
            "    FROM (SELECT DISTINCT entity_key, day, twr FROM f\n"
            "           WHERE line = 'return' AND currency = {{currency}} AND twr > -1)\n"
            "   GROUP BY 1),\n"
            "t AS (SELECT NULLIF(sum(fees), 0) AS total FROM e),\n"
            "n AS (\n  SELECT *, entity || ' (' || silver_source_id || ' ' || entity_kind || ')' AS entity_label,\n"
            "         silver_source_id || ' / ' || entity_kind || ' ' ||\n"
            "         row_number() OVER (PARTITION BY silver_source_id, entity_kind ORDER BY fees DESC) AS entity_group\n"
            "    FROM e)\n")
    # A name alone can repeat across sources, so the base names an
    # entity as the account tiles name an account: name (source kind).
    label = "entity_group" if privacy else "entity"
    pick = "entity_group" if privacy else "entity_label AS entity"
    group_cols = [f"{g}_pct" if privacy else g for g in FEE_GROUP_COLORS]
    groups = (", ".join(f"100 * {g} / (SELECT total FROM t) AS {g}_pct" for g in FEE_GROUP_COLORS) if privacy
              else ", ".join(FEE_GROUP_COLORS))
    card("Fees by portfolio", "row",
         "What each portfolio, or account outside one, paid in fees, net of refunds, by group. Trading "
         "and fund costs are not fee lines.",
         rows + f"SELECT {pick}, {groups}\n  FROM n\n ORDER BY fees DESC",
         {"graph.dimensions": [label], "graph.metrics": group_cols, "stackable.stack_type": "stacked",
          "graph.x_axis.title_text": "", "graph.y_axis.title_text": "",
          "series_settings": {c: {"color": FEE_GROUP_COLORS[g], "title": FEE_GROUP_TITLES[g]}
                              for c, g in zip(group_cols, FEE_GROUP_COLORS, strict=True)},
          **(_share_viz(*group_cols) if privacy else {})})
    rate = "CASE WHEN value_days > 0 THEN 100 * fees * 365 / value_days END AS fee_rate_pct"
    twr = "100 * r.twr AS twr_pct"
    money = ("100 * fees / (SELECT total FROM t) AS fees_pct" if privacy
             else "value_days / NULLIF(days, 0) AS mean_value")
    card("Fee rate by portfolio", "table",
         "The yearly fee rate on the value held, beside the return of the months ending in the window. "
         "Blank with no value or return.",
         rows + f"SELECT {pick}, {money},\n       {rate},\n       {twr}\n"
         "  FROM n LEFT JOIN r USING (entity_key)\n ORDER BY fees DESC",
         _share_viz("fees_pct", "fee_rate_pct", "twr_pct", decimals=2) if privacy
         else {"column_settings": {**_share_viz("fee_rate_pct", "twr_pct", decimals=2)["column_settings"],
                                   '["name","mean_value"]': {"decimals": 0}}})


def view_tags(table, spec):
    """Field-filter template tags for a native card over serving view
    `table`. spec maps tag name -> (column, widget-type). Each tag's
    dimension is the reference ("field", table, column), which bind
    resolves to the column's field id."""
    return {name: {"id": f"{table}.{name}", "name": name,
                   "display-name": name.replace("_", " ").title(),
                   "type": "dimension", "dimension": ("field", table, col),
                   "widget-type": widget, "default": None}
            for name, (col, widget) in spec.items()}


def _cl(name):
    """The optional filter clause for tag `name`."""
    return "\n     [[AND {{" + name + "}}]]"


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
# carry-forward of card balances, and web_fees the fee tiles' lines
# (fee_cards); neither has a category dimension. The privacy variants
# below are what the twin's cards actually take. The account filter binds
# account_label (PICKERS says why the readable column wins).
# The category filter binds spend_primary_label, not spend_primary: a
# field filter's widget is a dropdown of the values its column takes, and
# the Category picker is SHARED with the money dashboard, whose model
# column of the same name holds the display label. Binding the two to
# different vocabularies would leave the picker offering labels and the
# native cards matching them against vendored values — every tile empty.
SPEND_FILTERS = {**range_filters("occurred_at"),
                 "account": ("account_label", "string/="),
                 "category": ("spend_primary_label", "string/=")}
CARD_BALANCE_FILTERS = {**range_filters("as_of_day"),
                        "account": ("account_label", "string/=")}
FEE_FILTERS = {**range_filters("day"), "account": ("account_label", "string/=")}
# The same specs for the privacy twin, WITHOUT the account filter. A
# field filter's widget is a dropdown of the values its column takes, so
# an account filter on a privacy card offers account labels — which is
# the one thing the twin exists not to show. It is dropped rather than
# rebound: no column identifies an account without naming it, so a
# picker over some other column would be a different filter wearing the
# Account name (see PICKERS: the twin drops its Account picker for the
# same reason).
PRIVACY_SPEND_FILTERS = {k: v for k, v in SPEND_FILTERS.items()
                         if k != "account"}
PRIVACY_CARD_BALANCE_FILTERS = {k: v for k, v in CARD_BALANCE_FILTERS.items()
                                if k != "account"}
PRIVACY_FEE_FILTERS = {k: v for k, v in FEE_FILTERS.items() if k != "account"}
# The income dashboards' own picker ids. Distinct from the spending
# ones because a Metabase parameter id is per-dashboard-parameter, and
# the two dashboards carry different filter sets over different views.
INCOME_CURRENCY_PARAM_ID = "aa5df10a"
INCOME_ACCOUNT_PARAM_ID = "aa5df10b"
INCOME_TYPE_PARAM_ID = "aa5df10c"
INCOME_TAX_WRAPPER_PARAM_ID = "aa5df122"

# The Income filters. `type` binds to income_label rather than to the
# primary label its spending twin uses: the income taxonomy has ONE
# vendored primary, so a primary-level picker would collapse every
# earned type behind a single "Income" value and offer the deltas
# beside it — hiding every distinction a reader opens the dashboard
# for.
INCOME_FILTERS = {**range_filters("occurred_at"),
                  "account": ("account_label", "string/="),
                  "type": ("income_label", "string/="),
                  "tax_wrapper": ("tax_wrapper", "string/=")}
# The twin's, without the account filter, for the reason the spending
# twin drops its own: a field filter renders as a dropdown of its
# column's values, and account labels are the one thing the twin exists
# not to show.
PRIVACY_INCOME_FILTERS = {k: v for k, v in INCOME_FILTERS.items()
                          if k != "account"}

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
# line list. cashflow_cards draws that line, by withholding the tag
# from the cards that decline it. The twin's filters are the same: none
# of the three renders a dropdown of anything that identifies an
# account, which is the reason the other two twins drop theirs.
CASHFLOW_FILTERS = {**range_filters("occurred_at"),
                    "section": ("section", "string/=")}
# What a card that declines the Section picker says of it.
SECTION_DECLINED_DESC = " It needs every section, so it takes no Section."

# The Gains dashboard's own picker ids.
GAINS_CURRENCY_PARAM_ID = "aa5df111"
GAINS_TAX_WRAPPER_PARAM_ID = "aa5df112"
GAINS_MISSING_PARAM_ID = "aa5df113"
GAINS_ASSET_CLASS_PARAM_ID = "aa5df123"

# The Gains filters. The tax wrapper is the one of its own: a realized
# gain in a taxable account is a tax bill, in a retirement account it is
# not. The asset class is each position's, as Exposure and Yield narrow
# by it. A sale nothing classes reads `unknown`.
# No account filter: the tables name the accounts already.
GAINS_FILTERS = {**range_filters("period"),
                 "tax_wrapper": ("tax_wrapper", "string/="),
                 "asset_class": ("asset_class", "string/=")}

# The Exposure dashboard's picker ids and filters: the currency (a row
# filter, as on Gains), the time range over the month ends, the source,
# the tax wrapper and the asset class of each leaf.
EXPOSURE_CURRENCY_PARAM_ID = "aa5df114"
EXPOSURE_TAX_WRAPPER_PARAM_ID = "aa5df115"
EXPOSURE_ASSET_CLASS_PARAM_ID = "aa5df116"
EXPOSURE_FILTERS = {**range_filters("as_of_day"), "tax_wrapper": ("tax_wrapper", "string/="),
                    "asset_class": ("asset_class", "string/=")}
# Its debt cards. The debt view carries no asset class,
# so the class picker does not reach those cards.
EXPOSURE_DEBT_FILTERS = {k: v for k, v in EXPOSURE_FILTERS.items() if k != "asset_class"}


# The filters of the Wealth Overview's own tiles and of Allocation's (the
# Overview's pair is range_filters). Neither dashboard names an account
# anywhere, so the base and the twin share them. Every such tile reads a
# serving view natively and takes the Currency picker as the
# {{currency}} variable: the views carry the reporting currencies as
# columns, and a picker selects rows but never a column.
#
# Both narrow to a tax wrapper and an asset class: every holdings view
# carries each line's account wrapper and class, a cash balance being the
# class `cash`.
SELECTION_FILTERS = {"tax_wrapper": ("tax_wrapper", "string/="),
                     "asset_class": ("asset_class", "string/=")}
# The source, wrapper and class of a holdings card that takes no time
# filter. Allocation's As of day reaches its cards through asof_tags,
# which binds it to web_history_calendar. The Wealth Overview's three
# headline figures read each account at its latest snapshot whatever the
# time range, as `wealthdb holdings sources` does: a time filter on the
# snapshot would drop a source whose last snapshot falls before the
# range. The Stalest source tile beside them warns of a stale one.
HOLDINGS_FILTERS = {"source": ("silver_source_id", "string/="), **SELECTION_FILTERS}
POSITION_FILTERS = {**HOLDINGS_FILTERS, "vehicle": ("vehicle", "string/=")}
# The Top 100 positions cards show a stock's size beside it
# (web_position_sizes), joined after the ranking on the position's label, so
# the list and its values stay those of web_positions_history alone.
POSITION_SIZE_JOIN = ("\n  LEFT JOIN web_position_sizes z ON z.symbol IS NOT DISTINCT FROM p.symbol"
                      "\n       AND z.name IS NOT DISTINCT FROM p.name"
                      "\n       AND z.asset_class IS NOT DISTINCT FROM p.asset_class"
                      "\n       AND z.vehicle IS NOT DISTINCT FROM p.vehicle")
POSITION_SIZE_NOTE = " Size is a stock's market-cap bucket."


# The Wealth Overview's history tiles, the net worth chart and the asset
# classes over time: the range pair on the history day, the tax wrapper
# and the asset class.
HISTORY_FILTERS = {**range_filters("as_of_day"), **SELECTION_FILTERS}
# The note of the headline figures, which take no Time range.
LATEST_DESC = " It reads the latest snapshot whatever the Time range."

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
# Optional: the calendar lists every position's payments until one is
# picked, by its symbol, its name or its position key.
POSITION_TAG = {"id": "position-tag", "name": "position", "display-name": "Position",
                "type": "text", "required": False}

# The Yield dashboards' filters over web_yield and web_yield_curve: the
# Exposure set, the window's last day being the day a card reads.
YIELD_FILTERS = EXPOSURE_FILTERS

# The Income dashboards' cards of received and expected income read
# several views each, the income received and its other lines, the tax
# withheld, the parts expected and the run-rate, and a picker reaches
# one template tag of a card. So each of its field filters is bound to
# the view whose values its picker's dropdown lists, and every view the
# card reads keeps the rows whose source, account or tax wrapper that
# view's selected rows carry (`kept` in income_projected_cards). A
# source with no income line is still a source, so the source's view is
# the sources', not the income's; the same holds for a tax wrapper,
# whose picker lists every account's. The Type binds the income lines'
# label, which `kept` maps to the holdings' types (INCOME_DETAILED_TYPES)
# and the run-rate's classes (INCOME_LINE_CLASSES). The Time range
# reaches them the same way, through the days the income received
# covers.
INCOME_PROJECTED_SELECTION = {"source": ("web_sources_latest", "silver_source_id"),
                              "account": ("web_income", "account_label"),
                              "tax_wrapper": ("web_sources_latest", "tax_wrapper")}


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
        # It does NOT decide the diagram's layout. The renderer seeds
        # each column in the order the rows arrive and then RELAXES,
        # pulling every node toward the weighted mean of its neighbours
        # for a fixed number of iterations; the seed is forgotten, so
        # the rows in this order and in plain value order give the same
        # column. If the leaf column reads wrongly, the lever is
        # `sankey.node_align` (see the parameter list) and not this
        # ORDER BY.
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


def _field_clauses(tags, exclude=()):
    """One optional [[AND {{tag}}]] clause per FIELD FILTER on the card,
    but those named in `exclude`.

    A plain variable — {{currency}}, {{investing}} — picks a column or a
    grain rather than filtering rows, and an [[AND {{x}}]] clause around
    one is not valid SQL. The test is the tag's own type, so a variable
    added to a family later cannot silently produce a broken predicate.
    Excluding by name keeps "every filter but X" in step with a filter
    added later."""
    return "".join(_cl(n) for n in tags
                   if tags[n].get("type") == "dimension" and n not in exclude)


def _native_where(tags, indent="   ", exclude=()):
    """`WHERE TRUE` plus the card's field-filter clauses (_field_clauses)."""
    return f"\n{indent}WHERE TRUE" + _field_clauses(tags, exclude)


def _calendar_tags():
    """The As of day's template tag of an Allocation card. It binds
    web_history_calendar, so a day after the
    history, up to tomorrow, reaches its last day (_history_day)."""
    return view_tags("web_history_calendar", {"as_of_day": ("as_of_day", "date/single")})


def asof_tags(view, spec):
    """Template tags for an Allocation card over daily-history view
    `view`: native_tags for every filter of `spec`, plus the day's
    (_calendar_tags)."""
    return {**native_tags(view, spec), **_calendar_tags()}


def _history_day():
    """The history day an Allocation card reads: the last day of the
    holdings history on or before the As of day, the history's last day
    with none picked. A day the calendar does not hold, before the
    history or later than tomorrow, reads none."""
    return "(SELECT max(history_day) FROM web_history_calendar WHERE TRUE" + _cl("as_of_day") + ")"


def _asof_where(tags, exclude=()):
    """An Allocation card's WHERE over a daily-history view: its field
    filters but the day's and those in `exclude`, and the history day
    (_history_day) as its own."""
    return (_native_where(tags, exclude=("as_of_day", *exclude))
            + "\n     AND as_of_day = " + _history_day())


def _breakdown_sql(view, dim, col, tags, share=False, benchmark=None):
    """An Allocation breakdown's SQL: `col` summed by `dim` over serving
    view `view`'s rows on the history day (_asof_where) that the field
    filters in `tags` keep, largest first; with `share` each as % of the
    sum, the twin's (liability buckets read negative, and a zero sum
    blanks them). With `benchmark`, that dimension of the picked benchmark
    beside it: a share as a share, and on the base card as the same total
    held in the benchmark's mix, so both bars read in the picked currency."""
    where = _asof_where(tags)
    if benchmark and not share:
        return (f"WITH p AS (\n  SELECT {dim}, sum({_ccy_case(col)})::DOUBLE AS value\n"
                f"    FROM {view}" + where + "\n   GROUP BY 1)\n"
                + _beside(dim, "value", benchmark, weight="share_pct / 100 * (SELECT sum(value) FROM p)"))
    if benchmark:
        return ("WITH r AS (\n"
                f"  SELECT {dim}, sum({_ccy_case(col)})::DOUBLE AS v\n"
                f"    FROM {view}" + where + "\n   GROUP BY 1),\n"
                f"p AS (SELECT {dim}, v / (SELECT CASE WHEN sum(v) <> 0 THEN sum(v) END FROM r) * 100 AS value_pct\n"
                "        FROM r)\n" + _beside(dim, "value_pct", benchmark))
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


# Every family of cards, in the order their cards are saved.
FAMILIES = (overview_cards, allocation_cards, spending_cards, income_cards, cashflow_cards, returns_cards,
            freshness_cards, gains_cards, exposure_cards, yield_cards)


def card_defs():
    """card name -> Card for every card provisioning defines: each
    family's base cards, then each one's twin cards. A tile that sums
    money in the dashboard's chosen currency is native SQL over a gold
    serving view: a picker selects rows and never a column, so the
    required {{currency}} variable picks the value column with a CASE,
    and the other pickers land as field filters. The returns charts are
    native too, for their window functions and pseudo-source UNION. The
    rest are MBQL over the models: the returns scalars (report_returns
    carries a row set per currency, so there the Currency picker is a
    row filter), the freshness cards, and the spending tiles that need no
    currency variable.

    Every dashboard but those in NO_PRIVACY_TWIN has a privacy twin whose
    cards show shares (%) instead of money, each recomputing its
    denominator in-query with the dashboard's filters applied and naming
    it (PRIVACY_DESC), or redact the money columns where the figures are
    ratios already (Returns). A card that shows no monetary value
    (percentages, indices, days, source names) is exempt: the twin shows
    it as it is."""
    cards = {}
    for privacy in (False, True):
        for family in FAMILIES:
            family(Family(privacy, cards))
    return cards


# The cards that exist only beside a benchmark (Card.hide_empty), by
# base name: demo/check_dashboards.py expects them empty at the defaults.
BENCHMARK_CARDS = frozenset(n for n, c in card_defs().items() if c.hide_empty and not n.endswith(PRIVACY_SUFFIX))


# ---- dashboards ---------------------------------------------------------

class Column(NamedTuple):
    """A picker's dropdown source: the values `field` takes on
    pre-defined model `model`."""
    model: str
    field: str


class Picker(NamedTuple):
    """One dashboard picker, the payload dashboard_parameters builds from
    it. `dashboards` are the base dashboards whose picker of `slug` it
    is, None for any dashboard no other row names. `kind`: `list`, a
    multi-select text picker over a Column, no default = all values;
    `choice`, a required single value over a fixed tuple of values or a
    Column; `number`, a required number, its values from a Column if one
    is given; `day` and `range`, a date and a date range; `text`, a free
    text box. `default` is a value, or a function for one the
    configuration sets. `inline`: the picker renders on the tiles it
    reaches, not in the filter bar."""
    slug: str
    dashboards: tuple
    id: str
    label: str
    kind: str
    source: object = None
    default: object = None
    inline: bool = False


def _configured_currency():
    return DEFAULT_CURRENCY


def _configured_missing_basis():
    return DEFAULT_MISSING_BASIS


def _asof_default():
    return ASOF_DEFAULT


_WEALTH = ("Wealth Overview", "Allocation")

# Every picker, one row per (dashboards, slug). The ids are arbitrary
# but must be stable across runs so re-provisioning converges instead of
# accumulating parameters, and they must be distinct — a reused id would
# make two pickers one. A slug's id is per family of dashboards where
# the families' pickers differ (their values or the views they land on),
# shared where they do not.
PICKERS = (
    # The required reporting-currency picker every filtered dashboard
    # carries, defaulting to the configured currency (DEFAULT_CURRENCY).
    # A card running with the currency cleared would be wrong: a tile
    # over a model with one row per (line, reporting currency) would sum
    # every currency, and a native tile's {{currency}} variable needs a
    # value. A static list: the product's own vocabulary, not the
    # data's, so a card-backed list would re-scan the whole population
    # for a few known strings. Required with a default, because a
    # required parameter resets to its default rather than clearing.
    Picker("currency", _WEALTH, WEALTH_CURRENCY_PARAM_ID, "Currency", "choice",
           REPORTING_CURRENCIES, _configured_currency),
    # report_returns carries one row set per currency, so a card running
    # with the currency cleared would show every period once per
    # currency. The values come off the materialized table's own
    # currency column, like the start-year list.
    Picker("currency", ("Returns",), CURRENCY_PARAM_ID, "Currency", "choice",
           Column("report_returns", "currency"), _configured_currency),
    Picker("currency", ("Spending",), SPEND_CURRENCY_PARAM_ID, "Currency", "choice",
           REPORTING_CURRENCIES, _configured_currency),
    Picker("currency", ("Income",), INCOME_CURRENCY_PARAM_ID, "Currency", "choice",
           REPORTING_CURRENCIES, _configured_currency),
    Picker("currency", ("Cash Flow",), CASHFLOW_CURRENCY_PARAM_ID, "Currency", "choice",
           REPORTING_CURRENCIES, _configured_currency),
    Picker("currency", ("Gains",), GAINS_CURRENCY_PARAM_ID, "Currency", "choice",
           REPORTING_CURRENCIES, _configured_currency),
    Picker("currency", ("Exposure",), EXPOSURE_CURRENCY_PARAM_ID, "Currency", "choice",
           REPORTING_CURRENCIES, _configured_currency),
    Picker("currency", ("Yield",), YIELD_CURRENCY_PARAM_ID, "Currency", "choice",
           REPORTING_CURRENCIES, _configured_currency),
    # "past12months~": the trailing ~ means "include this month".
    # Without it Metabase takes the previous 12 COMPLETE months, which
    # silently drops every row of the current partial month.
    Picker("time_range", None, TIME_PARAM_ID, "Time range", "range", default="past12months~"),
    # Required, with today as the default (ASOF_DEFAULT): a required
    # parameter resets to its default instead of clearing, so a card
    # always reads the day picked. It reads the last history day on or
    # before it (_history_day): a day after the history, up to tomorrow,
    # reads its last day; a day before it, or later than tomorrow,
    # nothing.
    Picker("as_of_day", None, ASOF_PARAM_ID, "As of day", "day", default=_asof_default),
    # Start year: rescopes the summary scalars/table to the since-<year>
    # window (window_from_year) and clips the charts to periods ending
    # on/after that year. 0 = since inception. Required + default 0 so a
    # card never aggregates across windows; values come from the
    # materialized window_from_year set (auto-syncs to the data's span).
    Picker("start_year", None, START_YEAR_PARAM_ID, "Start year (0 = since inception)", "number",
           Column("report_returns", "window_from_year"), 0),
    # Not required / no default = all sources. On Returns it narrows the
    # by-source table and the charts, which keep the global '(all
    # sources)' reference line regardless.
    Picker("source", None, SOURCE_PARAM_ID, "Source", "list",
           Column("report_sources_latest", "silver_source_id")),
    # Required with None as its default: every card then reads one
    # benchmark or none, and the list comes off the config by the model.
    Picker("benchmark", None, BENCHMARK_PARAM_ID, "Benchmark", "choice",
           Column("report_benchmarks", "benchmark"), NO_BENCHMARK),
    # A tax wrapper picker. Its values come off the accounts model: every
    # view reads a line's tax wrapper off its account. Multi-select, and
    # a wrapper names no account, so a twin carries it too.
    *(Picker("tax_wrapper", dashboards, pid, "Tax wrapper", "list",
             Column("report_accounts_latest", "tax_wrapper"))
      for dashboards, pid in ((_WEALTH, WEALTH_TAX_WRAPPER_PARAM_ID),
                              (("Income",), INCOME_TAX_WRAPPER_PARAM_ID),
                              (("Gains",), GAINS_TAX_WRAPPER_PARAM_ID),
                              (("Exposure",), EXPOSURE_TAX_WRAPPER_PARAM_ID),
                              (("Yield",), YIELD_TAX_WRAPPER_PARAM_ID))),
    # An asset class picker. Its values come off the asset class history
    # by default, which reads cash as the class `cash`, the way every
    # holdings view does. That taxonomy is also the one every exposure
    # leaf's class is one of. Gains and Yield list their own rows'
    # classes (report_gains_classes, report_yield_classes), `unknown`
    # among them.
    *(Picker("asset_class", dashboards, pid, "Asset class", "list", Column(model, "asset_class"))
      for dashboards, pid, model in (
          (_WEALTH, ASSET_PARAM_ID, "report_asset_classes_history"),
          (("Gains",), GAINS_ASSET_CLASS_PARAM_ID, "report_gains_classes"),
          (("Exposure",), EXPOSURE_ASSET_CLASS_PARAM_ID, "report_asset_classes_history"),
          (("Yield",), YIELD_ASSET_CLASS_PARAM_ID, "report_yield_classes"))),
    # The vehicle filter reaches only the Top-positions tiles, which list
    # individual holdings, and renders on them: the breakdowns each group
    # by a taxonomy dimension, so filtering them by the vehicle would
    # mostly self-select. Its values are the positions' vehicles.
    Picker("vehicle", None, VEHICLE_PARAM_ID, "Vehicle", "list",
           Column("report_positions_history", "vehicle"), inline=True),
    # The account picker targets `display_name` — the model's name for
    # the account LABEL (account_label) — and NOT `account_external_id`:
    # a picker lists the raw values of the column it is bound to and
    # there is no field-remapping machinery here, so binding it to the id
    # would offer a list of opaque identifiers. The label carries the
    # source and the kind, so two accounts wearing the same generic
    # product name at two institutions are two distinct entries rather
    # than one indistinguishable pair; two named alike WITHIN one source
    # still select together, accepted on the same reasoning, and the
    # money tiles group by the same column.
    #
    # THE PRIVACY TWIN CARRIES NO ACCOUNT PICKER (twin_drops). A picker
    # cannot redact what it offers: its dropdown IS the list of values
    # the bound column takes, and every column that identifies an
    # account is a label — the account label, the display name it is
    # built from (a card's falls back to its masked last four digits),
    # or the external id. Rebinding to a column that identifies no
    # account would make it a different filter wearing the same name, so
    # the twin drops it, the way its cards drop the columns they cannot
    # show (spending_cards). The Source picker still narrows by
    # institution, and the account breakdown there regroups onto source
    # × account kind.
    Picker("account", ("Spending",), ACCOUNT_PARAM_ID, "Account", "list",
           Column("report_spending", "display_name")),
    Picker("account", ("Income",), INCOME_ACCOUNT_PARAM_ID, "Account", "list",
           Column("report_income", "display_name")),
    Picker("category", ("Spending",), CATEGORY_PARAM_ID, "Category", "list",
           Column("report_spending", "spend_primary")),
    # Bound to the DETAILED label: the income taxonomy has one vendored
    # primary, so a primary-level picker would offer a handful of values
    # and hide every distinction worth filtering by.
    #
    # `income_detailed` is the MODEL'S ALIAS for that label
    # (report_income projects `i.income_label AS income_detailed`), not
    # the view column of the same name. A picker's dropdown is the values
    # its value_field takes on the model, so naming a column the model
    # does not project leaves the dropdown empty — and the tiles then
    # match a filter nobody could set. The matching field filter is bound
    # to the VIEW's income_label (INCOME_FILTERS), so picker and
    # predicate offer the same vocabulary, which is the contract the
    # spending pair states.
    Picker("type", ("Income",), INCOME_TYPE_PARAM_ID, "Type", "list",
           Column("report_income", "income_detailed")),
    # The position the calendar lists, a text box the positions table's
    # cells also fill (its card's Link).
    Picker("position", ("Income",), POSITION_PARAM_ID, "Position", "text"),
    # The Investing grain: net the section as one movement — the question
    # a reader opens with, "did the portfolio feed the household this
    # year or the household feed the portfolio" — or per asset class,
    # which is the step in. Required with a `whole` default so a card
    # never runs with it cleared, and a static list because the two
    # grains are the feature's, not the data's.
    Picker("investing", ("Cash Flow",), CASHFLOW_INVESTING_PARAM_ID, "Investing", "choice",
           ("whole", "class"), "whole"),
    # A section picker offers the feature's own vocabulary, the sections
    # the serving view carries, so the twin carries it too. It binds only
    # to the cards that declare its tag; the headline figures and the
    # diagram decline it, identically on the base and the twin.
    Picker("section", ("Cash Flow",), CASHFLOW_SECTION_PARAM_ID, "Section", "list",
           Column("report_cashflow", "section")),
    # Missing cost basis is required, like the currency: report_gains
    # holds a row set per reading, and a tile with the picker cleared
    # would sum both.
    Picker("missing_basis", ("Gains",), GAINS_MISSING_PARAM_ID, "Missing cost basis", "choice",
           MISSING_BASIS_READINGS, _configured_missing_basis),
    # The projection's four: required, as each card reads one rate path,
    # one maturing rule and one horizon of the parts, with the CLI's
    # defaults. The shift is a number of basis points.
    Picker("rates", None, RATES_PARAM_ID, "Rate path", "choice", RATE_PATHS, RATE_PATHS[0]),
    Picker("maturing", None, MATURING_PARAM_ID, "Maturing", "choice", MATURING_RULES, MATURING_RULES[0]),
    Picker("shift", None, SHIFT_PARAM_ID, "Shift (bp)", "number", default=0),
    Picker("horizon", None, HORIZON_PARAM_ID, "Horizon (months)", "choice", HORIZONS, HORIZONS[0]),
)


def picker_payload(x):
    """The parameter of a dashboard Picker `x`, a Column's model unbound
    (("model_id", name), which bind resolves). `values_query_type` is
    what makes Metabase render a choice's dropdown instead of a
    free-text box."""
    out = {"id": x.id, "name": x.label, "slug": x.slug}
    default = x.default() if callable(x.default) else x.default
    if x.kind == "range":
        out.update({"type": "date/all-options", "sectionId": "date", "default": default})
    elif x.kind == "day":
        out.update({"type": "date/single", "sectionId": "date", "default": default, "required": True})
    elif x.kind == "number":
        out.update({"type": "number/=", "sectionId": "number", "isMultiSelect": False,
                    "default": [default], "required": True})
    else:
        out.update({"type": "string/=", "sectionId": "string", "isMultiSelect": x.kind == "list"})
        if x.kind == "choice":
            out.update({"default": [default], "required": True, "values_query_type": "list"})
    if isinstance(x.source, Column):
        btype = "type/Integer" if x.kind == "number" else "type/Text"
        out.update({"values_source_type": "card",
                    "values_source_config": {"card_id": ("model_id", x.source.model),
                                             "value_field": ["field", x.source.field, {"base-type": btype}]}})
    elif x.source is not None:
        out.update({"values_source_type": "static-list", "values_source_config": {"values": list(x.source)}})
    return out


class Heading(NamedTuple):
    """A full-width row of text, one high: a section's heading, or with
    `display` "text" a chart's subtitle."""
    text: str
    display: str = "heading"


class Stack(NamedTuple):
    """A column `width` wide of `cards` one above the other, sharing the
    height of the row it stands in."""
    width: int
    cards: tuple


class Row(NamedTuple):
    """Tiles side by side, `height` high, from the left: each a (card,
    width) pair, with a third element the card the twin shows there in
    its place, or a Stack."""
    height: int
    tiles: tuple


class Twin(NamedTuple):
    """Rows the base dashboard lays out, `base`, and the twin lays out in
    their place, `twin`."""
    base: tuple
    twin: tuple


class Dashboard(NamedTuple):
    """One pre-defined dashboard and its privacy twin. `twin_desc` is the
    twin's blurb, naming what its figures are shares of; None for a
    dashboard with no twin. `pickers` are the slugs of its pickers in
    their order (PICKERS), `twin_drops` those the twin leaves out.
    `rows` lay its tiles out (layout). `redacts`: the twin redacts money
    columns where the others show shares, and the switch link of both
    says so."""
    name: str
    desc: str
    twin_desc: str
    pickers: tuple
    twin_drops: tuple
    rows: tuple
    redacts: bool = False


def layout(rows, twin=False):
    """The tiles and headings of `rows` on Metabase's 24-column grid, on
    the twin with `twin`: (tiles, headings), a tile (card, row, col,
    size_x, size_y, twin card), a heading (row, text, display). Each row
    starts below the one before it, each tile right of the one before."""
    tiles, headings, top = [], [], 0
    for item in rows:
        if isinstance(item, Twin):
            sub_tiles, sub_headings = layout(item.twin if twin else item.base, twin)
            tiles += [(c, r + top, col, w, h, t) for c, r, col, w, h, t in sub_tiles]
            headings += [(r + top, text, d) for r, text, d in sub_headings]
            top += max([r + h for _c, r, _col, _w, h, _t in sub_tiles] +
                       [r + 1 for r, _text, _d in sub_headings], default=0)
            continue
        if isinstance(item, Heading):
            headings.append((top, item.text, item.display))
            top += 1
            continue
        col = 0
        for tile in item.tiles:
            if isinstance(tile, Stack):
                h = item.height // len(tile.cards)
                tiles += [(c, top + k * h, col, tile.width, h, c) for k, c in enumerate(tile.cards)]
                col += tile.width
            else:
                card, width, *shown = tile
                tiles.append((card, top, col, width, item.height, shown[0] if shown else card))
                col += width
        top += item.height
    return tiles, headings


# The sentence every pre-defined dashboard's blurb ends with.
PREDEFINED_NOTE = "Pre-defined; duplicate it before customizing."

# Every pre-defined dashboard, in the order they are sent. A tile's card
# takes each picker its template tags name (tile_mappings); an MBQL
# card's mappings are card_mappings'. Data Freshness is deliberately
# unfiltered — its job is to show every source, especially the stale
# ones a time filter would hide.
DASHBOARDS = (
    Dashboard(
        "Wealth Overview",
        "Net worth and its asset classes over time, then key figures from "
        "the other dashboards. Each figure opens the dashboard it comes from. "
        + PREDEFINED_NOTE,
        "Net worth figures are shares (%) of the selected sources' total at "
        "the window's end. Other figures are their dashboard's privacy "
        "figures, gains a percent of cost basis. " + PREDEFINED_NOTE,
        ("currency", "time_range", "source", "tax_wrapper", "asset_class"), (), (
            # Net worth = positions + cash by construction — the first
            # three tiles reconcile exactly; the trend tile is a monthly
            # AVERAGE, so it intentionally differs from today's value.
            # The stalest source sits beside them: the warning to read
            # before trusting any figure.
            Row(3, (("Net worth", 4), ("Positions value", 5), ("Cash balance", 5),
                    ("Net worth — monthly trend", 5), ("Stalest source (days)", 5))),
            # The net worth chart, then its mix by asset class.
            Row(8, (("Net worth over time", 24),)),
            Row(8, (("Asset class mix over time", 24),)),
            # Under two headings that say what sets each row of figures:
            # the figures over the time range, then those at its end,
            # today and ahead, three to a row so each title reads whole.
            Heading("Over the time range"),
            # On the twin, received income and net spend read 100 by
            # construction: each is the total its own twin divides by.
            # The twin leaves them out, and the other four figures of
            # their rows sit two to a row.
            Twin((Row(3, (("Income received", 8), ("Net spend", 8), ("Spend — monthly trend", 8))),
                  Row(3, (("Net cash flow", 8), ("Savings rate", 8), ("Realized gain", 8)))),
                 (Row(3, (("Spend — monthly trend", 12), ("Net cash flow", 12))),
                  Row(3, (("Savings rate", 12), ("Realized gain", 12))))),
            Heading("At the range's end, today and ahead"),
            Row(3, (("Unrealized gain", 8), ("Return year to date (TWR)", 8),
                    ("Return year to date (MWR)", 8))),
            Row(3, (("Income yield", 8), ("Income expected", 8), ("Projected net cash flow", 8))),
        )),
    Dashboard(
        "Allocation",
        "Where the value sits as of the chosen day, by asset class, vehicle, "
        "currency, tax wrapper, management style, position and issuer. Cash "
        "is a position in every breakdown. " + PREDEFINED_NOTE,
        "Every breakdown is a share (%) of the selected sources' total "
        "on the chosen day, summing to 100. Absolute amounts never show. " + PREDEFINED_NOTE,
        ("currency", "as_of_day", "source", "tax_wrapper", "asset_class", "vehicle", "benchmark"), (), (
            # The two taxonomy dimensions side by side on the top row.
            Row(8, (("Allocation by asset class", 12), ("Allocation by vehicle", 12))),
            Row(8, (("Allocation by currency", 12), ("Value by tax wrapper", 12))),
            Row(6, (("Value by management style", 24),)),
            Row(8, (("Top 100 positions", 24),)),
            Row(9, (("Top 100 issuers", 24),)),
        )),
    Dashboard(
        "Returns",
        "Time-weighted (TWR) and money-weighted (MWR) returns, per period "
        "and per source. Start year 0 means since inception, where TWR is "
        "often n/a. " + PREDEFINED_NOTE,
        "Returns are scale-free and show as they are. The money columns "
        "and the account and portfolio grains are redacted. " + PREDEFINED_NOTE,
        ("currency", "start_year", "source", "benchmark"), (), (
            # Scalars rescope to the since-<start year> window; charts split
            # by source with the global grain as a toggleable '(all sources)'
            # line. MWR is a summary figure only (the per-period buckets
            # carry TWR); that is engine behavior the scalars mirror.
            Row(3, (("Return (TWR)", 8), ("Return (MWR)", 8), ("Annualized return (TWR)", 8))),
            # Beside a benchmark alone (Card.hide_empty), the figures above them
            # again in their order.
            Row(3, (("Benchmark return (TWR)", 8), ("Benchmark return (MWR)", 8),
                    ("Benchmark annualized return (TWR)", 8))),
            Row(3, (("Excess return (TWR)", 8), ("Excess return (MWR)", 8),
                    ("Excess annualized return (TWR)", 8))),
            Row(8, (("Cumulative return (TWR)", 24),)),
            Row(6, (("Monthly returns (TWR)", 8), ("Quarterly returns (TWR)", 8),
                    ("Annual returns (TWR)", 8))),
            Row(8, (("Returns by source", 24),)),
            Row(8, (("Money-weighted return over time", 24),)),
            Row(8, (("Value against the benchmark", 24),)),
        ), redacts=True),
    Dashboard(
        "Spending",
        "Where the money goes over the window, by month, category, merchant "
        "and account, what each portfolio paid in fees, and what the cards "
        "owe. Own-account moves are not spend. " + PREDEFINED_NOTE,
        "Values are shares (%) of the window's net spend, of its biggest "
        "month or of its fees. Merchant and account labels are redacted. " + PREDEFINED_NOTE,
        ("currency", "time_range", "source", "account", "category"), ("account",), (
            # The three headline figures, then the shape of the window
            # (months x category), then the two breakdown levels side by
            # side — the primary tile drills through to the lines behind
            # a bar, the detailed tile holds the same window one level
            # down. Merchants and accounts answer "to whom" and "from
            # where", the fee tiles what each portfolio paid (web_fees);
            # the card-balances chart reads web_card_balances_history,
            # and the transaction list is the bottom of the drill-down.
            Row(3, (("Spend — monthly trend", 8), ("Net spend", 8), ("Uncategorized share", 8))),
            Row(9, (("Spending by month", 24),)),
            Row(8, (("Spending by category", 12), ("Spending by subcategory", 12))),
            Row(8, (("Top 50 merchants", 12), ("Spend by account", 12))),
            Row(8, (("Fees by portfolio", 10), ("Fee rate by portfolio", 14))),
            Row(6, (("Card balances over time", 24),)),
            Row(8, (("Largest transactions", 24),)),
        )),
    Dashboard(
        "Income",
        "Every income line received, wages included, and the income expected, the "
        "holdings' payments with a run-rate of the rest. " + PREDEFINED_NOTE,
        "Income as shares of the window's income, the holdings' income as a yield on their "
        "value. No payer or account is named. " + PREDEFINED_NOTE,
        # The Horizon sits beside the Time range: the one sets every
        # expected figure as the other sets every received one.
        ("currency", "time_range", "horizon", "source", "account", "type", "tax_wrapper", "position",
         "rates", "maturing", "shift"), ("account",), (
            # The headline figures first, then the charts, then the
            # tables: what was received and is expected, beside the
            # income lines' net income and backlog; the trend and the
            # withholding; both halves by month, then their interest and
            # dividends; the types beside the payers; then by account
            # and by position, the calendar a position's row picks, and
            # the largest receipts. The income in the window has no share
            # worth showing, so the twin shows the holdings' income as a
            # yield on their value there, and names it so.
            Row(3, (("Income in the window", 4, "Holdings' income yield"), ("Income received", 4),
                    ("Income expected", 4), ("Net income", 4), ("Monthly average income", 4),
                    ("Uncategorized income share", 4))),
            Row(3, (("Income — monthly trend", 8), ("Withholding", 8), ("Income after withholding", 8))),
            # Which bands of the chart below are a run-rate rather than a
            # payment.
            Heading("Every income line. A band marked run-rate is an account's average of past months; this "
                    "month it expects only what has not arrived.", "text"),
            Row(9, (("Received and expected by month", 24),)),
            Row(3, (("Holdings' interest received", 6), ("Holdings' dividends received", 6),
                    ("Holdings' interest expected", 6), ("Holdings' dividends expected", 6))),
            Row(8, (("Income by type", 12), ("Top 50 payers", 12))),
            Row(7, (("Received and expected by account", 24),)),
            Row(10, (("Received and expected by position", 24),)),
            Row(8, (("Expected payments", 24),)),
            Row(8, (("Largest receipts", 24),)),
        )),
    Dashboard(
        "Cash Flow",
        "Where the household's cash came from and went, as a statement and "
        "a Sankey, and why net worth moved. Buying and selling shows net. "
        "The projection sets expected income against average past outflows. "
        + PREDEFINED_NOTE,
        "Values are shares (%) of the diagram's hub or of the biggest month. "
        "The bridge shows shares of the starting net worth. The projection "
        "shows shares of what the holdings are expected to pay. " + PREDEFINED_NOTE,
        # BOTH dashboards carry the same pickers: unlike the spending and
        # income pairs, the twin drops nothing, because none of these
        # pickers renders a dropdown of anything that identifies an
        # account. There is no account picker to drop — docs/CASHFLOW.md
        # §9. The projection's four follow, the Horizon first as on
        # Income; they bind only to the projection cards
        # (cashflow_plan_cards), which take none of the others but the
        # currency.
        ("currency", "investing", "time_range", "source", "section", "horizon", "rates", "maturing",
         "shift"), (), (
            # Five headline figures, then the diagram at the centre, then
            # the shape of the window, why net worth moved, and the
            # projection of the months ahead, then the two sides of
            # operating, then the swing sections, then the lines.
            Row(3, (("Operating in", 5), ("Operating out", 5), ("Net cash flow", 5), ("Savings rate", 4),
                    ("Yield share", 5))),
            # Full width and tall: the diagram is the dashboard.
            Row(12, (("Cash flow", 24),)),
            Row(8, (("Cash flow statement by month", 24),)),
            # The net worth the walk leads from and to, in a column
            # beside the walk.
            Row(8, (Stack(6, ("Net worth at start", "Net worth at end")), ("Why net worth moved", 18))),
            Row(8, (("Saved and gained by month", 24),)),
            Row(8, (("Cash flow projection by month", 24),)),
            Row(9, (("Cash flow projection", 24),)),
            Row(7, (("Inflows by class by month", 12), ("Outflows by class by month", 12))),
            Row(7, (("Investing by month", 12), ("Financing and vehicles by month", 12))),
            Row(8, (("Largest flows", 24),)),
        )),
    # No privacy twin. Its tiles are gains on named positions; a twin
    # would normalise the money, and what would remain is the percent
    # columns the tables carry already.
    Dashboard(
        "Gains",
        "Realized and unrealized gains over the window, by month, source, "
        "class and position, with what the figures miss. " + PREDEFINED_NOTE, None,
        ("currency", "time_range", "source", "tax_wrapper", "asset_class", "missing_basis"), (), (
            # Coarse to fine, so scrolling down reveals detail: the
            # totals, the tax years, the months, where the gains came
            # from, the monthly splits, then the positions, and last
            # what the figures miss.
            # The held total and the taxable realized gain first; then
            # the gains on positions sold and held side by side, which
            # add up to the total gain beside them.
            Row(3, (("Unrealized gain", 4), ("Realized gain", 4), ("Gain on positions sold", 4),
                    ("Gain on positions held", 4), ("Total gain", 4), ("Cost basis coverage", 4))),
            Row(6, (("Realized gains by tax year", 24),)),
            Row(8, (("Gains by month", 24),)),
            Row(8, (("Gain by source", 8), ("Gain by asset class", 8), ("Realized gains by tax wrapper", 8))),
            Row(6, (("Realized gains by month, short vs long term", 12), ("Unrealized gain over time", 12))),
            Row(8, (("Unrealized gain vs. percent", 16), Stack(8, ("Positions in the green", "Positions in the red")))),
            # The winners and losers two to a row: a table narrower
            # than half the grid cannot show one name in full.
            Row(8, (("Largest unrealized gains", 12), ("Largest unrealized losses", 12))),
            Row(8, (("Best unrealized %", 12), ("Worst unrealized %", 12))),
            Row(8, (("Largest realized gains", 12), ("Largest realized losses", 12))),
            Row(8, (("Best realized %", 12), ("Worst realized %", 12))),
            Row(8, (("Coverage by source", 24),)),
        )),
    Dashboard(
        "Exposure",
        "What is really owned, funds looked through, by class, sector, size, "
        "region, country, currency and legal situs. The credit, maturity, "
        "coupon and cycle charts fix their class and take no Asset class. "
        + PREDEFINED_NOTE,
        "Every figure is a share (%) of the selection's net worth, or of the "
        "class a chart counts, at its month end. Value columns are dropped. " + PREDEFINED_NOTE,
        # The currency is required, as report_exposure holds a row set
        # per currency; the source, the tax wrapper and the asset class
        # narrow the look-through to a selection, whose shares the cards
        # compute.
        ("currency", "time_range", "source", "tax_wrapper", "asset_class", "benchmark"), (), (
            # The allocation first, the ring beside the sectors and the
            # classes over time full width; then where it sits: the map
            # full width, the regions beside every region's countries, the
            # regions over time, and where it legally sits, full width;
            # then what it moves with, the currency ring beside its
            # history; then public equity by size beside what funds hold;
            # then the debt by credit beside its maturities, its coupons
            # beside the equity's cycles, and the largest securities, full
            # width; last, how much the figures cover.
            Row(9, (("Exposure by asset class", 12), ("Exposure by sector", 12))),
            Row(8, (("Asset classes over time", 24),)),
            Row(10, (("Exposure by country", 24),)),
            Row(9, (("Exposure by region", 11), ("Exposure by region and country", 13))),
            Row(8, (("Regions over time", 24),)),
            Row(8, (("Exposure by legal situs", 24),)),
            Row(9, (("Exposure by currency", 12), ("Currencies over time", 12))),
            Row(7, (("Public equity by size", 12), ("Held directly or through funds", 12))),
            Row(8, (("Debt by credit rating, share of debt and cash", 12), ("Debt by maturity", 12))),
            Row(7, (("Debt by coupon kind", 12), ("Public equity by cycle", 12))),
            Row(9, (("Top securities", 24),)),
            Row(7, (("How the figures are classified", 24),)),
            # Beside a benchmark alone (Card.hide_empty), and the map's
            # comparison is a table.
            Row(8, (("Largest country deviations", 24),)),
        )),
    Dashboard(
        "Yield",
        "What the holdings earn and how their value moves with rates, over "
        "the last twelve months. Projected figures follow the Rate path, "
        "Maturing, Shift and Horizon pickers. " + PREDEFINED_NOTE,
        "Rates and shares show as they are. DV01 is a share of net worth per "
        "basis point, expected income a yield on its value. Account labels "
        "are redacted. " + PREDEFINED_NOTE,
        # Exposure's five, report_yield_positions holding a row set per
        # currency too, and the projection's four.
        ("currency", "time_range", "source", "tax_wrapper", "asset_class", "benchmark", "rates", "maturing",
         "shift", "horizon"), (), (
            # The four headline yields, then each currency's beside what a
            # move in rates does to its income; cash against its
            # benchmark; the debt's yield against its duration beside its
            # DV01 along the curve; the stocks' dividends beside what the
            # figures rest on; last, the yields and the duration over
            # time.
            Row(3, (("Income yield", 6), ("Projected income", 6), ("Implied yield", 6), ("Realized yield", 6))),
            Row(8, (("Yield by currency", 12), ("Income sensitivity to rates", 12))),
            Row(8, (("Cash against its benchmark", 24),)),
            Row(9, (("Yield against duration", 12), ("DV01 along the curve", 12))),
            Row(8, (("Dividend yield against payout", 12), ("What the yields rest on", 12))),
            Row(7, (("Yields over time", 12), ("Duration over time", 12))),
        )),
    Dashboard(
        "Data Freshness",
        "How long ago each source was last read, and how much of the value "
        "each month end rests on recent marks. It takes no pickers, so every "
        "stale source shows. " + PREDEFINED_NOTE,
        "Values are shares (%) of the latest total across all sources. "
        "Absolute amounts never show. " + PREDEFINED_NOTE,
        (), (), (
            Row(3, (("Stalest source (days)", 8), ("Returns age (days)", 8))),
            Row(10, (("Source freshness", 24),)),
            Row(8, (("Mark age", 24),)),
        )),
)
DASHBOARDS_BY_NAME = {d.name: d for d in DASHBOARDS}


def picker_of(dashboard, slug):
    """The PICKERS row of `slug` for base dashboard `dashboard`: its
    own, else the slug's shared one. A slug with neither is a
    definition out of step: ValueError."""
    own = [x for x in PICKERS if x.slug == slug and x.dashboards and dashboard in x.dashboards]
    shared = [x for x in PICKERS if x.slug == slug and x.dashboards is None]
    if not own and not shared:
        raise ValueError(f"provision: dashboard {dashboard!r} names picker {slug!r}, "
                         "which PICKERS has no row for")
    return (own or shared)[0]


def check_dashboards(dashboards):
    """Raise ValueError on the first of `dashboards` that names a picker
    PICKERS has no row for, or drops one from its twin it does not
    carry. Run on DASHBOARDS as they are defined, so a dashboard out of
    step stops provisioning before anything is sent."""
    for d in dashboards:
        for slug in d.pickers:
            picker_of(d.name, slug)
        extra = [s for s in d.twin_drops if s not in d.pickers]
        if extra:
            raise ValueError(f"provision: dashboard {d.name!r} drops {extra[0]!r} from its twin, "
                             "a picker it does not carry")


check_dashboards(DASHBOARDS)

# The dashboards with no privacy twin.
NO_PRIVACY_TWIN = frozenset(d.name for d in DASHBOARDS if d.twin_desc is None)
# The Income dashboards: demo/check_dashboards.py reads them.
INCOME_DASHBOARDS = {"Income", "Income" + PRIVACY_SUFFIX}


class Layout(NamedTuple):
    """One dashboard as dashboard_defs lays it out: its blurb, the slugs
    of its pickers, the name of its twin or base (None for a dashboard
    with no twin), its tiles (card, row, col, size_x, size_y) and its
    headings (row, text, display), and whether its twin redacts
    (Dashboard.redacts)."""
    desc: str
    pickers: tuple
    sibling: str
    tiles: list
    headings: list
    redacts: bool


def dashboard_defs(cards):
    """dashboard name -> Layout of every dashboard of DASHBOARDS and its
    privacy twin, each base before its twin. A twin has its base's
    layout with its cards swapped for their privacy variants, or the
    card itself where it is exempt in `cards` (card_defs). The sibling
    name links the two views — ensure_dashboards renders it as a switch
    link at the top of each dashboard."""
    out = {}
    for d in DASHBOARDS:
        pname = privacy_name(d.name) if d.twin_desc is not None else None
        tiles, headings = layout(d.rows)
        out[d.name] = Layout(d.desc, d.pickers, pname, [t[:5] for t in tiles], headings, d.redacts)
        if pname is None:
            continue
        tiles, headings = layout(d.rows, twin=True)
        out[pname] = Layout(d.twin_desc, tuple(s for s in d.pickers if s not in d.twin_drops), d.name,
                            [(c if cards[c].exempt else privacy_name(shown), *place)
                             for c, *place, shown in tiles], headings, d.redacts)
    return out


def dashboard_parameters(name):
    """The pickers dashboard `name` carries, a twin's its base's but the
    ones it drops, in their order (picker_of, picker_payload). A model a
    picker lists the values of is unbound, as bind takes it."""
    base = name.removesuffix(PRIVACY_SUFFIX)
    d = DASHBOARDS_BY_NAME[base]
    return [picker_payload(picker_of(base, s)) for s in d.pickers if name == base or s not in d.twin_drops]


# The pickers that render on the tiles they reach rather than in the bar.
INLINE_PICKERS = frozenset(x.id for x in PICKERS if x.inline)


def gold_metadata(client, db_id, tries=3, delay=2):
    """The synced gold tables metadata, with a short retry. A transient
    failure here must fail the run loudly rather than read as a gold with
    no columns, whose cards no field id could bind. Returns the tables
    list, or None when the metadata stays unreadable."""
    for _ in range(tries):
        st, meta = client.get(f"/api/database/{db_id}/metadata")
        if st == 200 and isinstance(meta, dict):
            return meta.get("tables", [])
        time.sleep(delay)
    return None


def card_queries():
    """card name -> its dataset_query, for every question and privacy
    card, its references unbound."""
    return {name: c.query for name, c in card_defs().items()}


def filter_field_columns():
    """table -> the columns of it the cards' field filters bind, for
    every table a card filters and every web_* serving view a card or a
    model reads, a view no filter binds with none; tables and columns
    sorted. It is read off the definitions: the field-id resolution and
    the startup probe read it, so a view or a filter a card gains is
    resolved and probed as it is defined."""
    native = [q["native"] for q in card_queries().values() if q["type"] == "native"]
    columns = {}
    for tag in (t for n in native for t in n["template-tags"].values()):
        if tag["type"] == "dimension":
            _field, table, col = tag["dimension"]
            columns.setdefault(table, set()).add(col)
    sql = "\n".join([n["query"] for n in native] + [query for query, _desc in report_models().values()])
    for view in re.findall(r"\b(?:FROM|JOIN) (web_\w+)\b", sql):
        columns.setdefault(view, set())
    return {table: tuple(sorted(columns[table])) for table in sorted(columns)}


def field_ids_from(tables):
    """{(table, column): field id} for every column named in
    filter_field_columns present in `tables` — the ids behind the native
    cards' field filters, which render the dashboard pickers as
    dropdowns / date widgets (a plain template variable would be a
    free-text box instead)."""
    wanted = filter_field_columns()
    ids = {}
    for t in tables:
        cols = wanted.get(t.get("name"))
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


def unfingerprint_lists(client, tables):
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
        st, body = client.put(f"/api/field/{fid}", {"visibility_type": "sensitive"})
        if st == 200:
            marked += 1
        else:
            print(f"provision: could not keep {name} from fingerprinting ({st}): {body.get('message')}",
                  file=sys.stderr)
    if marked:
        print(f"provision: {marked} list column(s) kept from fingerprinting")


def missing_filter_columns(tables):
    """The filter_field_columns tables not fully synced in `tables` —
    checked per COLUMN, not per table: Metabase's sync can expose a
    table before its fields, and a provision that ran in that gap would
    wire zero filters."""
    ids = field_ids_from(tables)
    return sorted({t for t, cols in filter_field_columns().items()
                   for c in cols if (t, c) not in ids})


def web_views_wanted():
    """The web_* serving views the cards and models read
    (filter_field_columns)."""
    return [t for t in filter_field_columns() if t.startswith("web_")]


def probe_rows(client, db_id, sql):
    """The rows native `sql` returns through the driver, or None when the
    probe does not answer. Metabase answers a query that fails with 202
    as well, its status `failed` and an error beside empty rows: that is
    no answer, not an answer of no rows."""
    st, res = client.post("/api/dataset", {"type": "native", "database": db_id,
                                           "native": {"query": sql, "template-tags": {}}})
    if st != 202 or res.get("status") == "failed" or "error" in res:
        return None
    return (res.get("data") or {}).get("rows")


def missing_web_views(client, db_id):
    """The wanted web_* serving views the mounted gold snapshot does not
    carry, probed through the driver itself. A snapshot predating the
    schema version that adds one cannot be fixed by a Metabase schema sync —
    only `wealthdb web refresh` re-materializes and re-snapshots gold.

    Probed BY NAME rather than by counting web_* views: a count says
    nothing about WHICH views are there, so a snapshot missing one would
    pass the moment gold grew any other web_* view.
    A probe that fails to answer reports everything missing, which halts
    provisioning — the conservative reading, since the alternative is a
    sync no snapshot without the view can complete."""
    want = web_views_wanted()
    rows = probe_rows(client, db_id,
                      "SELECT view_name FROM duckdb_views() "
                      f"WHERE NOT internal AND view_name IN ({_sql_list(want)})")
    if rows is None:
        return want
    have = {r[0] for r in rows if r}
    return [t for t in want if t not in have]


# The oldest gold schema the cards can read. A test holds it at or above
# the newest schema version that defines a view or table a card reads.
# DESIGN.md §6 says what each older snapshot lacks.
MIN_GOLD_SCHEMA = 179

# main()'s exit status for a snapshot older than MIN_GOLD_SCHEMA; every
# other failure exits 1. web/web tells the two apart, because only
# `wealthdb web refresh` mends this one and the browser wizard does not.
EXIT_STALE_SNAPSHOT = 3


def gold_schema_version(client, db_id):
    """The mounted gold snapshot's schema version, probed through the
    driver, or None when the probe does not answer."""
    rows = probe_rows(client, db_id, "SELECT max(gold_schema_version) FROM schema_meta")
    try:
        return int(rows[0][0])
    except (TypeError, IndexError, ValueError):
        return None


def check_gold_schema(client, db_id):
    """0 when the mounted gold snapshot is at MIN_GOLD_SCHEMA or newer,
    else the status main() exits with: EXIT_STALE_SNAPSHOT for an older
    snapshot, 1 when the version probe does not answer. Checked before
    anything is synced or converged: an older snapshot carries its views
    by name but not what the cards read from them, so every card would
    converge onto SQL that fails or reads them wrong."""
    version = gold_schema_version(client, db_id)
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


# WITHHOLDING_PROBE asks whether the snapshot holds any withholding rate,
# a wrapper's or an account's.
WITHHOLDING_PROBE = ("SELECT (SELECT count(*) FROM yield_withholding)"
                     " + (SELECT count(*) FROM yield_withholding_accounts) > 0")


def withholding_table(client, db_id):
    """Whether the mounted gold snapshot holds a withholding table with
    any rate in it (yield_withholding and yield_withholding_accounts, the
    config's yield.withholding), probed through the driver; None when the
    probe does not answer."""
    rows = probe_rows(client, db_id, WITHHOLDING_PROBE)
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


def ensure_synced(client, db_id, tries=20, delay=3):
    """Make every column behind filter_field_columns available: when
    some are missing from the synced metadata, either Metabase simply
    has not synced the new gold DDL yet (trigger a sync, wait bounded)
    or the snapshot predates a serving view the cards read — which no
    sync can fix. Returns the tables metadata to resolve field ids from,
    or None when provisioning must not proceed: metadata unreadable, a
    stale snapshot that would break the view-backed models, or a sync
    that was still incomplete when the budget ran out. All three end the
    same way, because a card whose filter has no field id cannot be
    bound (bind). main() has already refused a snapshot older than
    MIN_GOLD_SCHEMA (check_gold_schema)."""
    tables = gold_metadata(client, db_id)
    if tables is None:
        print("provision: cannot read the gold metadata — aborting before "
              "converging cards to a degraded shape", file=sys.stderr)
        return None
    gone = missing_filter_columns(tables)
    if not gone:
        # The columns are already there. A sync would not change them.
        print("provision: schema sync skipped")
        return tables
    absent = missing_web_views(client, db_id)
    if absent:
        print("provision: the gold snapshot is missing serving views the "
              f"cards read ({', '.join(absent)}) — run `wealthdb web "
              "refresh` to re-snapshot a migrated gold; leaving the "
              "existing cards untouched", file=sys.stderr)
        return None
    print(f"provision: syncing gold schema (missing: {', '.join(gone)})")
    client.post(f"/api/database/{db_id}/sync_schema", {})
    for _ in range(tries):
        time.sleep(delay)
        latest = gold_metadata(client, db_id)
        # A read that failed says nothing about the sync; only overwrite on a
        # successful one, so `tables` is always the newest metadata actually
        # read — at worst the pre-sync one. Coercing a failed last read to []
        # would read every filter column as missing.
        if latest is None:
            continue
        tables = latest
        if not missing_filter_columns(tables):
            return tables
    # The budget ran out with columns still missing. A card whose filter
    # has no field id cannot be bound (bind), so the run stops here, as
    # the two guards above stop it, before any card is sent.
    print("provision: gold schema sync did not complete in time (still "
          f"missing: {', '.join(missing_filter_columns(tables))}) — leaving "
          "the existing cards untouched; re-run `wealthdb web start` once "
          "Metabase has finished syncing, or `wealthdb web refresh` to "
          "re-snapshot a migrated gold", file=sys.stderr)
    return None


def ensure_database(client, db_name, gold_path):
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
    tile reads a serving table, not a report
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
    st, dbs = client.get("/api/database")
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
        st, body = client.post("/api/database", {
            "engine": "duckdb", "name": db_name, "details": details,
        })
        if st in (200, 201) and body.get("id"):
            print(f"provision: added DuckDB database '{db_name}' -> {gold_path}")
            return body["id"]
        print(f"provision: failed to add database ({st}): {body.get('message')}",
              file=sys.stderr)
        return None
    if any(cur.get(k) != v for k, v in details.items()):
        st, body = client.put(f"/api/database/{db_id}", {"details": {**cur, **details}})
        if st not in (200, 201):
            print(f"provision: could not update database settings ({st}): "
                  f"{body.get('message')}", file=sys.stderr)
            return None
        print(f"provision: database '{db_name}' present; connection "
              "settings converged (memory limit, spill dir)")
        return db_id
    print(f"provision: database '{db_name}' already present")
    return db_id


def ensure_collection(client, name):
    """The id of the collection named `name`, created if absent, or None
    on failure.

    There is no fallback to the root collection: card and dashboard
    matching is scoped to this collection, so provisioning into the root
    one would update or archive any user item there that shares a
    pre-defined name. A listing that fails aborts too, since reading it
    as empty would create a second collection of the same name."""
    try:
        found = client.collection(name)
    except metabase.Error as e:
        print(f"provision: could not list collections ({e.status})", file=sys.stderr)
        return None
    if found is not None:
        return found.get("id")
    st, body = client.post("/api/collection", {"name": name})
    if st in (200, 201) and body.get("id"):
        print(f"provision: created collection '{name}'")
        return body["id"]
    print(f"provision: could not create collection '{name}' ({st}): "
          f"{body.get('message')}", file=sys.stderr)
    return None


def pre_defined_cards(client, coll_id):
    """name -> card, for the non-archived cards in the pre-defined
    collection, or None when the card list is unreadable — read as
    empty, it would create every card a second time. Scoped to that
    collection so a user card that happens to share a name is never
    touched."""
    st, cards = client.get("/api/card")
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


def _get_all(client, paths):
    """key -> the body of GET `path`, for each key -> path in `paths`,
    read in parallel. A read that fails or answers anything but a JSON
    object is left out, so the caller sends that object."""
    def fetch(item):
        key, path = item
        try:
            st, body = client.get(path)
        except Exception:
            # A body that is no JSON, or a read cut short.
            return key, None
        return key, body if st == 200 and isinstance(body, dict) else None

    with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
        return {key: body for key, body in pool.map(fetch, paths.items()) if body}


def stored_cards(client, by_name, names):
    """name -> the server's card, for each of `names` already in
    `by_name`. The card list carries each query in MBQL 5, which no
    payload equals, so every card is read again in the legacy form.
    A card that cannot be read is left out, and so is sent."""
    return _get_all(client, {name: f"/api/card/{by_name[name]['id']}?legacy-mbql=true"
                                for name in names if name in by_name})


def upsert_card(client, by_name, name, payload, stored=None):
    """Create card `name` or update it in place. `stored` is the
    server's card (stored_cards); one equal to the payload is left in
    place. Returns (card id, 'created'|'updated'|'skipped') on success,
    (None, error text) on failure."""
    existing = by_name.get(name)
    if card_same(stored, payload):
        return stored["id"], "skipped"
    if existing:
        st, body = client.put(f"/api/card/{existing['id']}", payload)
        if st in (200, 201):
            return existing["id"], "updated"
    else:
        st, body = client.post("/api/card", payload)
        if st in (200, 201) and body.get("id"):
            return body["id"], "created"
    return None, f"({st}): {body.get('message')}"


def bind(payload, db_id, field_ids, model_ids):
    """Card `payload` with its references bound to one Metabase's ids:
    its dataset_query takes the database `db_id`, a ("field", table,
    column) becomes the field reference of that column's id in
    `field_ids`, and an MBQL source table ("model", name) the model's
    card, "card__<id>" by `model_ids`, and a ("model_id", name) the
    model's card id. A reference with no id raises
    ValueError. ensure_synced stops the run before any card is built when
    a column filter_field_columns names has no id, so a missing one is a
    definition out of step."""
    def ref(kind, key, ids):
        if key not in ids:
            raise ValueError(f"provision: no {kind} id for {key}")
        return ids[key]

    def walk(v, key=None):
        if isinstance(v, tuple) and v[:1] == ("field",):
            return ["field", ref("field", v[1:], field_ids), None]
        if isinstance(v, tuple) and v[:1] == ("model",):
            return f"card__{ref('model', v[1], model_ids)}"
        if isinstance(v, tuple) and v[:1] == ("model_id",):
            return ref("model", v[1], model_ids)
        if isinstance(v, dict):
            out = {k: walk(x, k) for k, x in v.items()}
            return {"type": out["type"], "database": db_id, **out} if key == "dataset_query" else out
        if isinstance(v, list):
            return [walk(x) for x in v]
        return v
    return walk(payload)


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


def upsert_cards(client, by_name, payloads, label):
    """Create or update-in-place every card in `payloads` (name -> full
    /api/card payload). An unchanged card is left in place. Returns
    (name -> card id, #created, #updated, #skipped), or None on the
    first failure."""
    ids, n = {}, {"created": 0, "updated": 0, "skipped": 0}
    stored = stored_cards(client, by_name, payloads)
    for name, payload in payloads.items():
        cid, how = upsert_card(client, by_name, name, payload, stored.get(name))
        if cid is None:
            print(f"provision: failed to save {label} '{name}' {how}",
                  file=sys.stderr)
            return None
        ids[name] = cid
        n[how] += 1
    return ids, n["created"], n["updated"], n["skipped"]


def archive_all(client, kind, ids):
    """Archive /api/<kind>/<id> for every id; returns how many succeeded."""
    n = 0
    for i in ids:
        st, _ = client.put(f"/api/{kind}/{i}", {"archived": True})
        n += st in (200, 201)
    return n


def model_payloads(db_id, coll_id):
    """name -> /api/card payload of every report model, bound to
    database `db_id`. Sends nothing."""
    return {
        name: bind(card_payload(coll_id, name, "model", "table", desc, _native(query, {}), {}),
                   db_id, {}, {})
        for name, (query, desc) in report_models().items()
    }


def card_payloads(db_id, coll_id, field_ids, model_ids, colors):
    """name -> /api/card payload of every question and privacy card,
    bound to database `db_id`, the field ids `field_ids` and the models
    in `model_ids`, a card that draws a series per source with `colors`
    (source id -> colour). Sends nothing."""
    return {name: bind(card_payload(coll_id, name, "question", c.display, c.desc, c.query,
                                    with_source_colors(c.viz, colors)), db_id, field_ids, model_ids)
            for name, c in card_defs().items()}


def ensure_models(client, db_id, coll_id, by_name):
    """Create/refresh the pre-defined report models in `coll_id`, and
    archive any retired (renamed-away) ones. An unchanged model is left
    in place. The models are content-free shims over the gold serving
    tables and views — no source data is baked in. Returns model name ->
    card id, or None on failure."""
    saved = upsert_cards(client, by_name, model_payloads(db_id, coll_id), "model")
    if saved is None:
        return None
    ids, created, updated, skipped = saved
    archived = archive_all(client, "card",
                           [by_name[n]["id"] for n in RETIRED_MODEL_NAMES
                            if n in by_name])
    print(f"provision: report models ({_ccy_slash()}) — {created} created, "
          f"{updated} updated, {skipped} unchanged, {archived} retired "
          f"(collection '{COLLECTION_NAME}')")
    return ids


def ensure_cards(client, db_id, coll_id, by_name, field_ids, model_ids, colors=None):
    """Create/refresh the pre-defined questions, bound to the field ids
    `field_ids` and the models in `model_ids`, and archive any retired
    (renamed-away) ones. A card that draws a series per source takes
    `colors` (source id -> colour). Returns card name -> id (the
    dashboards' tile lookup) and card name -> its description, or None on
    failure."""
    payloads = card_payloads(db_id, coll_id, field_ids, model_ids, colors or {})
    saved = upsert_cards(client, by_name, payloads, "card")
    if saved is None:
        return None
    ids, created, updated, skipped = saved
    archived = archive_all(client, "card",
                           [by_name[n]["id"] for n in RETIRED_CARD_NAMES
                            if n in by_name])
    print(f"provision: questions — {created} created, {updated} updated, "
          f"{skipped} unchanged, {archived} retired")
    return ids, {name: body["description"] for name, body in payloads.items()}


def text_dashcard(dc_id, text, row=0, display="text"):
    """A virtual tile (no backing card) spanning row `row`: the absolute
    <-> privacy switch link on the top row or a chart's subtitle as
    `text`, a section heading as `heading`."""
    return {"id": dc_id, "card_id": None, "row": row, "col": 0,
            "size_x": 24, "size_y": 1, "series": [], "parameter_mappings": [],
            "visualization_settings": {
                "virtual_card": {"name": None, "display": display,
                                 "visualization_settings": {},
                                 "dataset_query": {}, "archived": False},
                "text": text,
                "dashcard.background": False,
                "text.align_vertical": "middle"}}


# The template tag whose name is not the slug of the picker it answers:
# the returns cards' Source field filter.
TAG_SLUGS = {"source_ff": "source"}


def tile_mappings(query, card_id, parameters):
    """The picker mappings of the tile of native card `card_id`, whose
    dataset_query is `query`, on a dashboard with pickers `parameters`:
    each picker, in their order, whose slug names one of the card's
    template tags (TAG_SLUGS where the two differ). A field filter takes
    it as a dimension, a plain variable as a variable. A card declines a
    picker by declaring no tag of its slug."""
    tags = query["native"]["template-tags"]
    by_slug = {TAG_SLUGS.get(t, t): t for t in tags}
    return [{"parameter_id": x["id"], "card_id": card_id,
             "target": ["dimension" if tags[t].get("type") == "dimension" else "variable",
                        ["template-tag", t]]}
            for x in parameters if (t := by_slug.get(x["slug"])) is not None]


def card_mappings(card, dash, card_ids):
    """The picker mappings of MBQL card `card`'s tile on dashboard `dash`
    or its twin, before they are kept to the pickers the dashboard
    carries: dimensions of its model. Only Returns and Spending have
    MBQL tiles that take a picker."""
    dash = dash.removesuffix(PRIVACY_SUFFIX)
    if dash == "Returns":
        # The MBQL returns scalars + table take Currency + Start-year as
        # dimensions: currency,
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
    if dash != "Spending":
        return []
    # The Spending pickers land on the spending models' dimensions, the
    # Time range on a line's occurred_at. Currency is a row filter here
    # (the model carries one row per reporting currency) where a native
    # spending card takes it as a {{currency}} variable — the same split
    # the returns tiles already make.
    return [{"parameter_id": TIME_PARAM_ID, "card_id": card_ids[card],
             "target": ["dimension", _f("occurred_at", "type/DateTime")]},
            {"parameter_id": SOURCE_PARAM_ID, "card_id": card_ids[card],
             "target": ["dimension",
                        _f("silver_source_id", "type/Text")]},
            *({"parameter_id": pid, "card_id": card_ids[card],
               "target": ["dimension", _f(col, "type/Text")]}
              for pid, col in (
                  (SPEND_CURRENCY_PARAM_ID, "currency"),
                  (ACCOUNT_PARAM_ID, "display_name"),
                  (CATEGORY_PARAM_ID, "spend_primary"))
              if not (pid == SPEND_CURRENCY_PARAM_ID
                      and card in SPEND_ALL_CURRENCY_CARDS))]


def overview_home(card, dash):
    """The dashboard Card `card`'s tile on overview dashboard `dash`
    restates, its home: a dashboard it is borrowed from, or one whose
    data the overview's own card shows. A click on the tile opens it,
    on the twin that dashboard's twin; a dashboard with no twin (Gains)
    opens from the base alone, so a click never leads from the privacy
    view to money. An MBQL tile takes the pickers its home dashboard
    gives it. None for the net worth tiles, which open nothing, and for
    a twin tile whose home has no twin."""
    if card.home is None or not dash.endswith(PRIVACY_SUFFIX):
        return card.home
    return None if card.home in NO_PRIVACY_TWIN else card.home + PRIVACY_SUFFIX


def overview_mappings(card, dash, params, card_ids):
    """The picker mappings of MBQL Card `card`'s tile on overview
    dashboard `dash`: each mapping the card has on its home dashboard,
    moved onto the overview's picker of the same slug. A mapping with no
    such picker here drops."""
    slug_pid = {x["slug"]: x["id"] for x in params[dash]}
    home = overview_home(card, dash)
    if home is None:
        return []
    home_slug = {x["id"]: x["slug"] for x in params[home]}
    return [{**m, "parameter_id": slug_pid[home_slug[m["parameter_id"]]]}
            for m in card_mappings(card.name, home, card_ids)
            if home_slug.get(m["parameter_id"]) in slug_pid]


def overview_click(home, dash, params, dash_ids, links):
    """The click behaviour of a tile on overview dashboard `dash` that
    opens dashboard `home` (overview_home): pass each overview picker
    that dashboard carries by slug, and the column of each Link in
    `links` to the home picker it names."""
    target = {x["slug"]: x["id"] for x in params[home]}

    def link(tid, source):
        return {"id": tid, "source": source, "target": {"type": "parameter", "id": tid}}
    mapping = {target[x["slug"]]: link(target[x["slug"]], {"type": "parameter", "id": x["id"], "name": x["name"]})
               for x in params[dash] if x["slug"] in target}
    for col, to in links:
        if to in target:
            mapping[target[to]] = link(target[to], {"type": "column", "id": col, "name": col})
    return {"click_behavior": {"type": "link", "linkType": "dashboard",
                               "targetId": dash_ids[home], "parameterMapping": mapping}}


def unreached_note(mapped, params):
    """The sentence a tile's description ends in, naming the pickers of
    its dashboard (parameters `params`) that the parameter ids `mapped`
    leave out; empty where every picker reaches the tile. The Benchmark
    picker goes unnamed: most tiles have no benchmark to show, and a
    reader expects none there."""
    params = [x for x in params if x["id"] != BENCHMARK_PARAM_ID]
    names = [x["name"] for x in params if x["id"] not in mapped]
    if not names:
        return ""
    if len(names) == len(params):
        return " No picker reaches it."
    listed = names[0] if len(names) == 1 else ", ".join(names[:-1]) + " and " + names[-1]
    return (f" The {listed} picker{'s' if len(names) > 1 else ''}"
            f" do{'' if len(names) > 1 else 'es'} not reach it.")


def crossfilter_viz(links, parameters):
    """A tile's own viz settings: for each Link in `links` whose picker
    the dashboard carries (`parameters`), the click behaviour that sets
    that picker to the value of the cell clicked in its column. Empty
    for a tile with none."""
    slug_pid = {x["slug"]: x["id"] for x in parameters}
    settings = {f'["name","{c}"]': {"click_behavior": {
                    "type": "crossfilter",
                    "parameterMapping": {pid: {"id": pid, "source": {"type": "column", "id": c, "name": c},
                                               "target": {"type": "parameter", "id": pid}}}}}
                for c, to in links if (pid := slug_pid.get(to)) is not None}
    return {"column_settings": settings} if settings else {}


def dashboard_payloads(defs, cards, model_ids, card_ids, dash_ids, titles, descs):
    """name -> (description, parameters, tiles) of each dashboard in
    `defs` (dashboard_defs), in its order, of the Cards `cards`: the
    payload ensure_dashboards sends. Its parameters are
    dashboard_parameters bound to the models in `model_ids`, `dash_ids`
    is name -> its id, and `titles` and `descs` are as ensure_dashboards
    takes them. Sends nothing."""
    params = {name: bind(dashboard_parameters(name), None, {}, model_ids) for name in defs}
    out = {}
    for name, d in defs.items():
        desc, sibling = d.desc, d.sibling
        parameters = params[name]
        param_ids = {x["id"] for x in parameters}
        # The pickers of the filter bar: an inline picker renders on the
        # tiles it reaches instead (inline_parameters).
        bar = [x for x in parameters if x["id"] not in INLINE_PICKERS]
        # Both returns dashboards show the same scale-free ratios, so
        # their link says "redacted" where the others say "shares".
        privacy = name.endswith(PRIVACY_SUFFIX)
        if d.redacts:
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
        overview = base_name == "Wealth Overview"
        tile_titles_here = {**titles, **dashboard_tile_titles(base_name, cards)}

        # The switch link occupies row 0, so the tiles and headings shift
        # down one row; a dashboard without a twin has no link and no
        # shift. The vehicle filter renders on the Top-positions tile
        # itself (inline_parameters) rather than in the dashboard's
        # filter bar: it only applies to that one widget.
        dashcards = [text_dashcard(-99, link)] if link else []
        shift = 1 if link else 0
        dashcards += [text_dashcard(-(100 + k), text, row + shift, display)
                      for k, (row, text, display) in enumerate(d.headings)]
        for i, (card, row, col, sx, sy) in enumerate(d.tiles):
            # The pickers a tile answers to. A native card takes each
            # picker of the dashboard its tags name (tile_mappings). An
            # MBQL card's mappings are kept to the pickers the dashboard
            # carries: the Spending twin has no Account picker
            # (Dashboard.twin_drops), and a mapping naming a
            # parameter not on the dashboard is a target with no filter
            # behind it. On a Wealth Overview an MBQL tile takes the
            # overview's pickers by slug (overview_mappings).
            c = cards[card]
            home = overview_home(c, name) if overview else None
            if c.query["type"] == "native":
                maps = tile_mappings(c.query, card_ids[card], parameters)
            else:
                maps = [m for m in (overview_mappings(c, name, params, card_ids) if overview
                                    else card_mappings(card, name, card_ids))
                        if m["parameter_id"] in param_ids]
            viz = {**(crossfilter_viz(c.links, parameters) if home is None else {}),
                   **({"card.title": tile_titles_here[card]} if card in tile_titles_here else {}),
                   **({"card.hide_empty": True} if c.hide_empty else {})}
            # A tile's description names the pickers of the bar that do
            # not reach it, read off the mappings it is sent with, and a
            # Wealth Overview tile opens its home dashboard.
            note = unreached_note({m["parameter_id"] for m in maps}, bar)
            if note and card in descs:
                viz["card.description"] = descs[card] + note
            if home is not None:
                viz.update(overview_click(home, name, params, dash_ids, c.links))
            dashcards.append({"id": -(i + 1), "card_id": card_ids[card], "row": row + shift,
                              "col": col, "size_x": sx, "size_y": sy, "series": [],
                              "visualization_settings": viz,
                              "inline_parameters": [m["parameter_id"] for m in maps
                                                    if m["parameter_id"] in INLINE_PICKERS],
                              "parameter_mappings": maps})
        out[name] = (desc, parameters, dashcards)
    return out


def ensure_dashboards(client, coll_id, card_ids, model_ids, titles=None, descs=None):
    """Create/refresh the pre-defined dashboards (and their privacy twins)
    with their global filters (see dashboard_parameters) linked to every
    tile and, where there is a twin, a switch link to it on the top row,
    and archive any retired (renamed-away) ones. A card in `titles`
    (tile_titles) shows that title on its tiles. `descs`, card name ->
    description, lets a tile carry its card's description with the
    pickers of its dashboard that do not reach it named. A dashboard's
    headings are full-width rows of text, and a Wealth Overview tile
    opens its home dashboard (overview_click). An unchanged dashboard
    is left in place. One that differs has its parameter list and its
    tiles replaced wholesale, so the layout converges to spec and a tile
    added by hand does not survive. The dashboard description says to
    duplicate before customizing."""
    titles = titles or {}
    descs = descs or {}
    try:
        listed = client.dashboards(coll_id)
    except metabase.Error as e:
        print(f"provision: could not list dashboards ({e.status}): {e.body.get('message')}",
              file=sys.stderr)
        return 1
    existing = {d.get("name"): d.get("id") for d in listed}

    cards = card_defs()
    defs = dashboard_defs(cards)
    n = {"created": 0, "updated": 0, "skipped": 0}
    # First pass: make sure every dashboard exists, so the switch links
    # can point at the sibling's id.
    dash_ids = {}
    for name, d in defs.items():
        did = existing.get(name)
        if did is None:
            st, body = client.post("/api/dashboard", {"name": name, "description": d.desc,
                                                      "collection_id": coll_id})
            if st not in (200, 201) or not body.get("id"):
                print(f"provision: failed to create dashboard '{name}' ({st}): "
                      f"{body.get('message')}", file=sys.stderr)
                return 1
            did = body["id"]
            n["created"] += 1
        dash_ids[name] = did
    # The dashboards listed before this run, read for the comparison. One
    # created above has no tiles yet, so it is sent without a read.
    stored = _get_all(client, {name: f"/api/dashboard/{existing[name]}"
                                  for name in defs if existing.get(name) is not None})

    payloads = dashboard_payloads(defs, cards, model_ids, card_ids, dash_ids, titles, descs)
    for name, (desc, parameters, dashcards) in payloads.items():
        current = stored.get(name)
        if dashboard_unchanged(current, name, desc, parameters, dashcards):
            n["skipped"] += 1
            continue
        if name in existing:
            n["updated"] += 1
        # A change to the pickers alone (a new picker default) is sent
        # alone, so the tiles keep their ids.
        if dashboard_unchanged(current, name, desc, (current or {}).get("parameters"), dashcards):
            if not _put_parameters(client, dash_ids[name], name, parameters,
                                   "update the filters of"):
                return 1
            continue
        st, body = client.put(f"/api/dashboard/{dash_ids[name]}",
                              {"name": name, "description": desc,
                               "parameters": parameters,
                               "dashcards": dashcards})
        if st not in (200, 201):
            print(f"provision: failed to lay out dashboard '{name}' ({st}): "
                  f"{body.get('message')}", file=sys.stderr)
            return 1
        # Deleting a dashcard that carries an inline filter makes Metabase
        # drop that filter from the dashboard — and the wholesale dashcard
        # replacement above deletes every old tile, so an inline parameter
        # sent in the same PUT gets cleaned right back up. Re-assert the
        # parameter list now that the new tiles are in place.
        if parameters and not _put_parameters(client, dash_ids[name], name, parameters,
                                              "re-assert filters on"):
            return 1

    archived = archive_all(client, "dashboard",
                           [existing[name] for name in RETIRED_DASHBOARD_NAMES
                            if existing.get(name) is not None])
    print(f"provision: dashboards — {n['created']} created, {n['updated']} updated, "
          f"{n['skipped']} unchanged, {archived} retired")
    return 0


def _put_parameters(client, did, name, parameters, what):
    """PUT dashboard `did`'s parameter list alone. False when the server
    refuses it, with "failed to `what` `name`" printed."""
    st, body = client.put(f"/api/dashboard/{did}", {"parameters": parameters})
    if st in (200, 201):
        return True
    print(f"provision: failed to {what} '{name}' ({st}): {body.get('message')}",
          file=sys.stderr)
    return False


def _mapping_order(mappings):
    """A tile's picker mappings in a stable order, by parameter, card and
    target: Metabase gives their order no meaning."""
    if not isinstance(mappings, list):
        return mappings
    return sorted(mappings, key=lambda m: json.dumps(
        [m.get("parameter_id"), m.get("card_id"), m.get("target")], sort_keys=True)
        if isinstance(m, dict) else "")


def _dashcards_same(server, desired):
    """True when the stored tiles already match the payload. Tiles pair
    by their place on the grid, whatever order the server lists them
    in, and a tile's picker mappings match whatever their order. A tile
    is compared on the keys the payload sends; the rest of a stored tile
    is the server's own. A missing tile list is not a match."""
    if not isinstance(server, list) or len(server) != len(desired):
        return False
    if not all(isinstance(t, dict) for t in server):
        return False

    def place(tile):
        return (tile.get("row") or 0, tile.get("col") or 0)

    server, desired = sorted(server, key=place), sorted(desired, key=place)
    for got, want in zip(server, desired, strict=True):
        for key, value in want.items():
            mine = got.get(key)
            if key == "parameter_mappings":
                mine, value = _mapping_order(mine), _mapping_order(value)
            if key != "id" and not _same_value(mine, value):
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
