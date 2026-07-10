#!/usr/bin/env python3
"""Provision a fresh Metabase over its loopback REST API: create the
admin account (skipping the "tell us about yourself" setup wizard),
pre-add the gold DuckDB database, and create the pre-defined report
models, metrics, questions and dashboards. Idempotent — safe to run on
every `wealthdb web start`. Standard library only.

Called by web/web; not meant to be run by hand (but it can be).
"""
import argparse
import json
import sys
import time
import urllib.error
import urllib.request


def req(base, path, method="GET", data=None, session=None, timeout=30):
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
    except (urllib.error.URLError, ConnectionError):
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
    ap.add_argument("--password", required=True)
    ap.add_argument("--gold-path", required=True)
    ap.add_argument("--db-name", default="gold")
    ap.add_argument("--default-currency", default="USD",
                    help="accepted for compatibility; no longer used — the report "
                         "models now expose USD/CHF/EUR columns via report_x_multi(...)")
    a = ap.parse_args()

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
                     "email": a.email, "password": a.password},
            "prefs": {"site_name": "wealthdb", "allow_tracking": False},
        })
        if st not in (200, 201) or not body.get("id"):
            print(f"provision: setup failed ({st}): {body.get('message')}", file=sys.stderr)
            return 1
        print(f"provision: created admin {a.email}; setup wizard skipped")
    else:
        print("provision: already set up; skipping admin creation")

    # Log in to add the database. If setup was done earlier with a
    # different password (user changed it), don't fail the start —
    # the database was added on the first provision.
    _, body = req(a.base, "/api/session", "POST",
                  {"username": a.email, "password": a.password})
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

    coll_id = ensure_collection(a.base, sid, COLLECTION_NAME)
    by_name = pre_defined_cards(a.base, sid, coll_id)
    model_ids = ensure_models(a.base, sid, db_id, coll_id, by_name)
    if model_ids is None:
        return 1
    card_ids = ensure_cards(a.base, sid, db_id, coll_id, by_name, model_ids)
    if card_ids is None:
        return 1
    return ensure_dashboards(a.base, sid, coll_id, card_ids, model_ids)


# MAX_BIGINT as the as-of epoch means "latest snapshot" (the macros'
# current mode), so the models stay current without re-provisioning;
# report_transactions spans all of time (filter in Metabase).
MAX_BIGINT = 9223372036854775807

# Collection the pre-defined models live in (kept apart from anything the
# user builds by hand).
COLLECTION_NAME = "wealthdb (pre-defined)"

# Un-suffixed model names retired when the snapshot reports gained the
# `_latest` suffix; archived on provision so a re-run cleans them up.
RETIRED_MODEL_NAMES = ["report_global", "report_portfolios",
                       "report_accounts", "report_positions"]


def report_models():
    """model name -> (native SQL, description). The report_*_multi DuckDB
    macros (migration 0024) are the single source of truth; each model only
    wraps a macro to bind the as-of and to render epoch columns as TIMESTAMP
    (`to_timestamp` -> naive-UTC) for Metabase. The macros already emit DECIMAL
    money/quantity columns and one value column set per reporting currency
    (USD/CHF/EUR), so no value casting is needed here. The `_latest` reports
    are as of each source's latest snapshot; the `_history` reports carry value
    forward per day; the `_pct` family are the privacy variants backing the
    privacy dashboards (values as % of peak, absolute-value columns dropped)."""
    def wrap(from_expr, ts_cols=()):
        parts = [f"CAST(to_timestamp({c}) AS TIMESTAMP) AS {c}" for c in ts_cols]
        return f"SELECT * REPLACE ({', '.join(parts)}) FROM {from_expr}"

    # Scalar subquery with the peak daily global net worth per currency —
    # the shared normalization constant of the privacy (_pct) models.
    NW_PEAK = ("(SELECT max(total_value_usd) AS peak_usd, "
               "max(total_value_chf) AS peak_chf, "
               "max(total_value_eur) AS peak_eur "
               "FROM report_global_history_multi()) AS peak")

    def pct_wrap(from_expr, value_cols, ts_cols=(), exclude=()):
        """Privacy wrapper: every monetary column becomes % of the peak
        daily global net worth in its currency (one constant scale per
        currency, so every aggregate keeps its shape), and columns that
        would leak absolute values are dropped."""
        repl = [f"CAST(to_timestamp({c}) AS TIMESTAMP) AS {c}" for c in ts_cols]
        repl += [f"{c} / peak.peak_{c.rsplit('_', 1)[1]} * 100 AS {c}"
                 for c in value_cols]
        excl = ", ".join(["peak_usd", "peak_chf", "peak_eur", *exclude])
        return (f"SELECT * EXCLUDE ({excl}) REPLACE ({', '.join(repl)}) "
                f"FROM {from_expr}, {NW_PEAK}")

    def flow_pct(kinds, sign=""):
        """Privacy wrapper for monthly transaction flows, normalized by the
        widget's own peak: aggregate to (month, kind, source), self-join the
        peak monthly total, and express each cell as % of that peak — so the
        tallest bar of the widget reads 100 regardless of the flows' size
        relative to net worth. `sign` negates debit kinds (fees, taxes) so
        costs read as positive bars."""
        ks = ", ".join(f"'{k}'" for k in kinds)
        sums = ", ".join(f"{sign}sum(value_{c}) AS value_{c}"
                         for c in ("usd", "chf", "eur"))
        tots = ", ".join(f"sum(value_{c}) AS value_{c}"
                         for c in ("usd", "chf", "eur"))
        peaks = ", ".join(f"max(value_{c}) AS peak_{c}"
                          for c in ("usd", "chf", "eur"))
        pcts = ", ".join(f"value_{c} / p.peak_{c} * 100 AS value_{c}"
                         for c in ("usd", "chf", "eur"))
        return (f"WITH m AS (SELECT date_trunc('month', to_timestamp(occurred_at)) "
                f"AS occurred_at, kind, silver_source_id, {sums} "
                f"FROM report_transactions_multi(0, {MAX_BIGINT}) "
                f"WHERE kind IN ({ks}) GROUP BY 1, 2, 3), "
                f"p AS (SELECT {peaks} FROM "
                f"(SELECT {tots} FROM m GROUP BY occurred_at)) "
                f"SELECT CAST(occurred_at AS TIMESTAMP) AS occurred_at, kind, "
                f"silver_source_id, {pcts} FROM m, p")

    def asset_classes(pct=False):
        """Positions grouped by asset class, unioned with each source's
        cash balance as a 'cash' class — so the class values sum exactly
        to net worth (liability classes stay negative). pct=True scales
        to % of the peak daily global net worth (per currency)."""
        vals = ", ".join(f"value_{c}" for c in ("usd", "chf", "eur"))
        if pct:
            vals = ", ".join(f"value_{c} / peak.peak_{c} * 100 AS value_{c}"
                             for c in ("usd", "chf", "eur"))
        peak = f", {NW_PEAK}" if pct else ""
        return (f"WITH u AS ("
                f"SELECT as_of_day, silver_source_id, asset_class, "
                f"sum(value_usd) AS value_usd, sum(value_chf) AS value_chf, "
                f"sum(value_eur) AS value_eur "
                f"FROM report_positions_history_multi() GROUP BY 1, 2, 3 "
                f"UNION ALL "
                f"SELECT as_of_day, silver_source_id, 'cash', "
                f"cash_balance_usd, cash_balance_chf, cash_balance_eur "
                f"FROM report_sources_history_multi()) "
                f"SELECT CAST(to_timestamp(as_of_day) AS TIMESTAMP) AS as_of_day, "
                f"silver_source_id, asset_class, {vals} FROM u{peak}")

    V3 = [f"{p}_{c}" for p in ("positions_value", "cash_balance", "total_value")
          for c in ("usd", "chf", "eur")]
    V1 = ["value_usd", "value_chf", "value_eur"]
    BASE3 = ("positions_value_base", "cash_balance_base", "total_value_base")

    L = f"({MAX_BIGINT})"   # _multi _latest macro arg (as-of = latest snapshot)
    return {
        "report_global_latest": (
            wrap(f"report_global_multi{L}", ["min_snapshot_at", "max_snapshot_at"]),
            "Whole-portfolio rollup as of the latest snapshot: cash, positions and "
            "total value in USD, CHF and EUR (one column set per currency), with the "
            "min/max snapshot date span. Mirrors `wealthdb holdings global`."),
        "report_sources_latest": (
            wrap(f"report_sources_multi{L}", ["snapshot_at"]),
            "One row per silver source as of the latest snapshot: positions + cash "
            "totalled in the source's base currency and in USD/CHF/EUR, with rolled-up "
            "tax wrapper / management style. Mirrors `wealthdb holdings sources`."),
        "report_portfolios_latest": (
            wrap(f"report_portfolios_multi{L}", ["snapshot_at"]),
            "One row per portfolio as of the latest snapshot: positions + cash totalled "
            "in the portfolio's base currency and in USD/CHF/EUR, with rolled-up tax "
            "wrapper / management style. Mirrors `wealthdb holdings portfolios`."),
        "report_accounts_latest": (
            wrap(f"report_accounts_multi{L}", ["snapshot_at"]),
            "One row per account as of the latest snapshot: positions + cash totalled "
            "in the account's base currency and in USD/CHF/EUR, with kind, tax wrapper "
            "and management style. Mirrors `wealthdb holdings accounts`."),
        "report_positions_latest": (
            wrap(f"report_positions_multi{L}", ["snapshot_at"]),
            "One row per held position as of the latest snapshot, with market value in "
            "USD, CHF and EUR. Mirrors `wealthdb holdings positions`."),
        "report_transactions": (
            wrap(f"report_transactions_multi(0, {MAX_BIGINT})", ["occurred_at"]),
            "Every transaction over all time, with net amount in USD, CHF and EUR at the "
            "transaction date. Mirrors `wealthdb transactions` (filter the date range in "
            "Metabase)."),
        # History reports: one row per entity per UTC day, from the first snapshot to
        # today, value carried forward between snapshots. For time-series charts; filter
        # / aggregate by as_of_day. history@today equals the matching _latest report.
        "report_global_history": (
            wrap("report_global_history_multi()", ["as_of_day"]),
            "Whole-portfolio value for every day from the first snapshot to today "
            "(carried forward between snapshots), in USD, CHF and EUR. The net-worth-"
            "over-time series — chart total_value_usd (or _chf / _eur) against as_of_day."),
        "report_sources_history": (
            wrap("report_sources_history_multi()", ["as_of_day"]),
            "Per-silver-source value for every day (carried forward), in the source's "
            "base currency and in USD/CHF/EUR. Filter to a source and chart against "
            "as_of_day."),
        "report_accounts_history": (
            wrap("report_accounts_history_multi()", ["as_of_day"]),
            "Per-account value for every day (carried forward), in the account's base "
            "currency and in USD/CHF/EUR. Filter to an account and chart against as_of_day."),
        "report_portfolios_history": (
            wrap("report_portfolios_history_multi()", ["as_of_day"]),
            "Per-portfolio value for every day (carried forward), in the portfolio's base "
            "currency and in USD/CHF/EUR. Filter to a portfolio and chart against as_of_day."),
        "report_positions_history": (
            wrap("report_positions_history_multi()", ["as_of_day", "snapshot_at"]),
            "Per-position value for every day (carried forward), in USD, CHF and EUR. "
            "Large (days x held positions) — filter to a position / account / date range "
            "before charting."),
        # Privacy (_pct) variants of the models the pre-defined cards are
        # built on: same columns and grain, but monetary values are % of
        # the peak daily global net worth (per currency) and columns that
        # would leak absolute values (base-currency totals, quantities,
        # amounts, prices) are dropped. The privacy dashboards use these.
        "report_sources_latest_pct": (
            pct_wrap(f"report_sources_multi{L}", V3, ["snapshot_at"], BASE3),
            "Privacy variant of report_sources_latest: totals as % of the peak "
            "daily global net worth (per currency); base-currency columns "
            "dropped."),
        "report_sources_history_pct": (
            pct_wrap("report_sources_history_multi()", V3, ["as_of_day"], BASE3),
            "Privacy variant of report_sources_history: totals as % of the peak "
            "daily global net worth (per currency); base-currency columns "
            "dropped."),
        "report_accounts_history_pct": (
            pct_wrap("report_accounts_history_multi()", V3, ["as_of_day"], BASE3),
            "Privacy variant of report_accounts_history: totals as % of the peak "
            "daily global net worth (per currency); base-currency columns "
            "dropped."),
        "report_positions_history_pct": (
            pct_wrap("report_positions_history_multi()", V1,
                     ["as_of_day", "snapshot_at"], ("quantity", "market_value")),
            "Privacy variant of report_positions_history: values as % of the "
            "peak daily global net worth (per currency); quantity and "
            "native-currency market value dropped."),
        "report_transactions_pct": (
            pct_wrap(f"report_transactions_multi(0, {MAX_BIGINT})", V1,
                     ["occurred_at"],
                     ("gross_amount", "net_amount", "quantity", "price")),
            "Privacy variant of report_transactions: values as % of the peak "
            "daily global net worth (per currency); native-currency amounts, "
            "quantity and price dropped."),
        # Flow widgets normalize by their own peak (max monthly total),
        # not by net worth — flows are orders of magnitude smaller, and
        # per-widget peaks keep the bars readable (tallest bar = 100).
        "report_income_monthly_pct": (
            flow_pct(INCOME_KINDS),
            "Monthly investment income by kind and source, as % of the peak "
            "month's total income (privacy view of the income widget)."),
        "report_costs_monthly_pct": (
            flow_pct(COST_KINDS, sign="-"),
            "Monthly fees and withheld taxes by kind and source (negated to "
            "positive), as % of the peak month's total costs (privacy view "
            "of the fees widget)."),
        # Asset classes incl. cash, so the asset-class widget sums exactly
        # to net worth: positions grouped by class, unioned with each
        # source's cash balance as a 'cash' class. Liability classes (e.g.
        # mortgages) stay negative — the widget must be a bar chart, not a
        # pie (pies silently drop negative slices).
        "report_asset_classes_history": (
            asset_classes(),
            "One row per asset class (incl. a 'cash' class) per source per "
            "day, carried forward, in USD/CHF/EUR. Sums to net worth by "
            "construction; liability classes are negative."),
        "report_asset_classes_history_pct": (
            asset_classes(pct=True),
            "Privacy variant of report_asset_classes_history: values as % of "
            "the peak daily global net worth (per currency)."),
    }


# ---- pre-defined metrics, questions and dashboards --------------------
# Like the report models, everything below is a content-free definition —
# MBQL over the models (referenced by card id) or native SQL over the gold
# macros; no source data is baked in. Provisioning converges these to spec
# on every start, so a user who wants to customize one should duplicate it
# into another collection first.

# Transaction kinds counted as investment income vs. carrying costs by the
# monthly charts (net-amount sign convention: income is a credit > 0, fees
# and withheld taxes are debits < 0).
INCOME_KINDS = ["coupon", "distribution", "dividend", "interest", "staking"]
COST_KINDS = ["fee", "tax"]

# Metric names retired when the pre-defined cards switched from
# identifier-style to prose names (dashboards and widgets read better as
# prose); archived on provision so a re-run cleans them up.
RETIRED_CARD_NAMES = ["net_worth_usd_current", "net_worth_chf_current",
                      "net_worth_eur_current", "positions_value_usd_current",
                      "cash_balance_usd_current", "net_worth_usd_daily",
                      # Top 10 -> Top 100 (with the inline asset-class filter)
                      "Top 10 positions (USD)", "Top 10 positions (% of peak)"]

# Dashboard names retired by renames ("Net Worth" undersold the income /
# cost flow tiles); archived on provision so a re-run cleans them up.
RETIRED_DASHBOARD_NAMES = ["Net Worth"]

# Every dashboard has a privacy twin whose cards run over the _pct models
# (monetary values as % of the peak daily global net worth). Cards listed
# here show no monetary values, so the twin reuses them as-is.
PRIVACY_EXEMPT_CARDS = {"Stalest source (days)"}

# Which _pct model replaces which model when building the privacy cards.
PCT_MODEL_MAP = {
    "report_sources_latest": "report_sources_latest_pct",
    "report_sources_history": "report_sources_history_pct",
    "report_accounts_history": "report_accounts_history_pct",
    "report_positions_history": "report_positions_history_pct",
    "report_transactions": "report_transactions_pct",
    "report_asset_classes_history": "report_asset_classes_history_pct",
}

PRIVACY_DESC = (" Privacy view: values are % of the peak daily global net "
                "worth, not absolute amounts.")


def privacy_name(name):
    """Card title for the %-of-peak variant of card `name`."""
    if " (USD)" in name:
        return name.replace(" (USD)", " (% of peak)")
    return f"{name} (% of peak)"


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


def metric_defs(db_id, mid):
    """metric name -> (display, description, dataset_query). Kept to a
    single aggregation so Metabase's metric editor can open them (charts
    needing breakouts live in question_defs). Built on the per-source
    reports rather than the global ones — summing across sources equals
    the global report by construction, and it gives the dashboards'
    source filter a silver_source_id dimension to land on."""
    m = {}
    for ccy in ("USD", "CHF", "EUR"):
        m[f"Net worth ({ccy})"] = ("scalar",
            f"Total net worth in {ccy} as of the latest snapshot "
            "(cash + positions across all sources).",
            _mbql(db_id, mid["report_sources_latest"],
                  {"aggregation": [["sum", _dec(f"total_value_{ccy.lower()}")]]}))
    m["Positions value (USD)"] = ("scalar",
        "Market value of all positions in USD as of the latest snapshot.",
        _mbql(db_id, mid["report_sources_latest"],
              {"aggregation": [["sum", _dec("positions_value_usd")]]}))
    m["Cash balance (USD)"] = ("scalar",
        "Total cash balance in USD as of the latest snapshot.",
        _mbql(db_id, mid["report_sources_latest"],
              {"aggregation": [["sum", _dec("cash_balance_usd")]]}))
    return m


def question_defs(db_id, mid):
    """question name -> (display, description, dataset_query, viz
    settings). All MBQL (no native SQL) so the dashboards' filters can
    map onto every card's dimensions."""
    def kind_in(kinds):
        return ["=", _f("kind", "type/Text")] + kinds

    month = _f("occurred_at", "type/DateTime", "month")
    days_stale = ["datetime-diff", _f("snapshot_at", "type/DateTime"),
                  ["now"], "day"]
    return {
        "Net worth — monthly trend (USD)": ("smartscalar",
            "Average daily net worth (USD) of the latest month, with the "
            "change vs the month before.",
            _mbql(db_id, mid["report_sources_history"],
                  {"aggregation": [["/",
                       ["sum", _dec("total_value_usd")],
                       ["distinct", _f("as_of_day", "type/DateTime")]]],
                   "breakout": [_f("as_of_day", "type/DateTime", "month")]}),
            {}),
        "Net worth over time (USD)": ("area",
            "Net worth in USD for every day since the first snapshot "
            "(value carried forward between snapshots), stacked by "
            "source; the envelope is total net worth.",
            _mbql(db_id, mid["report_sources_history"],
                  {"aggregation": [["sum", _dec("total_value_usd")]],
                   "breakout": [_f("as_of_day", "type/DateTime", "day"),
                                _f("silver_source_id", "type/Text")]}),
            {"stackable.stack_type": "stacked"}),
        "Cash vs positions over time (USD)": ("area",
            "Daily cash balance and positions value (USD), stacked; the "
            "envelope is total net worth.",
            _mbql(db_id, mid["report_sources_history"],
                  {"aggregation": [["sum", _dec("cash_balance_usd")],
                                   ["sum", _dec("positions_value_usd")]],
                   "breakout": [_f("as_of_day", "type/DateTime", "day")]}),
            {"stackable.stack_type": "stacked"}),
        "Income by month (USD)": ("bar",
            "Investment income (dividends, interest, distributions, "
            "coupons, staking) per month in USD, stacked by kind.",
            _mbql(db_id, mid["report_transactions"],
                  {"filter": kind_in(INCOME_KINDS),
                   "aggregation": [["sum", _dec("value_usd")]],
                   "breakout": [month, _f("kind", "type/Text")]}),
            {"stackable.stack_type": "stacked"}),
        "Fees & taxes by month (USD)": ("bar",
            "Fees and withheld taxes per month in USD, stacked by kind; "
            "debits are negated so costs read as positive bars.",
            _mbql(db_id, mid["report_transactions"],
                  {"filter": kind_in(COST_KINDS),
                   "expressions": {"cost_usd": ["*", _dec("value_usd"), -1]},
                   "aggregation": [["sum", ["expression", "cost_usd"]]],
                   "breakout": [month, _f("kind", "type/Text")]}),
            {"stackable.stack_type": "stacked"}),
        # The five allocation questions run over the daily-history models
        # so the Allocation dashboard can show holdings as of any chosen
        # day (history@today equals the _latest reports by construction).
        # The dashboard supplies the required as-of-day filter; opened
        # standalone they sum one row per entity per DAY, so add an
        # as_of_day filter first (the descriptions say so too).
        "Allocation by asset class (USD)": ("row",
            "Value (USD) by asset class as of a day, including a 'cash' "
            "class — the bars sum exactly to net worth; liability classes "
            "(e.g. mortgages) show as negative bars, which is why this is "
            "a bar chart and not a pie (pies silently drop negatives). "
            "Built for the Allocation dashboard, which supplies the as-of "
            "day; opened standalone, filter as_of_day to a single day "
            "first.",
            _mbql(db_id, mid["report_asset_classes_history"],
                  {"aggregation": [["sum", _dec("value_usd")]],
                   "breakout": [_f("asset_class", "type/Text")],
                   "order-by": [["desc", ["aggregation", 0]]]}),
            {}),
        "Allocation by currency (USD)": ("row",
            "Positions value (USD) by the position's native currency — the "
            "FX exposure of the invested part (cash not included) as of a "
            "day. Built for the Allocation dashboard, which supplies the "
            "as-of day; opened standalone, filter as_of_day to a single "
            "day first.",
            _mbql(db_id, mid["report_positions_history"],
                  {"aggregation": [["sum", _dec("value_usd")]],
                   "breakout": [_f("currency", "type/Text")],
                   "order-by": [["desc", ["aggregation", 0]]]}),
            {}),
        "Value by tax wrapper (USD)": ("pie",
            "Total account value (USD, incl. cash) by tax wrapper as of a "
            "day. Built for the Allocation dashboard, which supplies the "
            "as-of day; opened standalone, filter as_of_day to a single "
            "day first.",
            _mbql(db_id, mid["report_accounts_history"],
                  {"aggregation": [["sum", _dec("total_value_usd")]],
                   "breakout": [_f("tax_wrapper", "type/Text")]}),
            {}),
        "Value by management style (USD)": ("row",
            "Total account value (USD, incl. cash) by management style as "
            "of a day. Built for the Allocation dashboard, which supplies "
            "the as-of day; opened standalone, filter as_of_day to a "
            "single day first.",
            _mbql(db_id, mid["report_accounts_history"],
                  {"aggregation": [["sum", _dec("total_value_usd")]],
                   "breakout": [_f("management_style", "type/Text")],
                   "order-by": [["desc", ["aggregation", 0]]]}),
            {}),
        # A parameterized "Top K" was prototyped and rejected: an MBQL
        # limit cannot be driven by a dashboard filter, and the native-SQL
        # alternative needs the shared as-of filter mapped onto a text
        # variable that string-matches Metabase's literal 'thisday' token
        # — undocumented behavior, fragile across upgrades.
        "Top 100 positions (USD)": ("table",
            "The hundred largest positions by market value (USD) as of a "
            "day, aggregated across accounts; narrow with the widget's "
            "asset-class filter. Built for the Allocation dashboard, "
            "which supplies the as-of day; opened standalone, filter "
            "as_of_day to a single day first.",
            _mbql(db_id, mid["report_positions_history"],
                  {"aggregation": [["sum", _dec("value_usd")]],
                   "breakout": [_f("symbol", "type/Text"),
                                _f("name", "type/Text"),
                                _f("asset_class", "type/Text")],
                   "order-by": [["desc", ["aggregation", 0]]],
                   "limit": 100}),
            {}),
        "Stalest source (days)": ("scalar",
            "Days since the oldest source's latest snapshot — how out of "
            "date the worst feed is.",
            _mbql(db_id, mid["report_sources_latest"],
                  {"expressions": {"days_stale": days_stale},
                   "aggregation": [["max", ["expression", "days_stale"]]]}),
            {}),
        "Source freshness": ("table",
            "Per source: latest snapshot, its age in days, and the value "
            "riding on it (USD).",
            _mbql(db_id, mid["report_sources_latest"],
                  {"expressions": {"days_stale": days_stale},
                   "fields": [_f("silver_source_id", "type/Text"),
                              _f("snapshot_at", "type/DateTime"),
                              ["expression", "days_stale"],
                              _dec("total_value_usd")],
                   "order-by": [["desc", ["expression", "days_stale"]]]}),
            {}),
    }


# Dashboard filters: a silver-source picker (default: all values) plus
# either a time range over flows/history (default: past 12 months) or a
# single as-of day over point-in-time holdings (default: today), each
# linked to every tile — and a widget-scoped asset-class picker that
# renders inline on the Top-positions tile only. The parameter ids are
# arbitrary but must be stable across runs so re-provisioning converges
# instead of accumulating parameters.
TIME_PARAM_ID = "aa5df100"
SOURCE_PARAM_ID = "aa5df101"
ASOF_PARAM_ID = "aa5df102"
ASSET_PARAM_ID = "aa5df103"

# The asset-class filter is linked only to these tiles (the breakdown
# widgets each already show asset classes; filtering them by class would
# mostly self-select).
ASSET_FILTERED_CARDS = {"Top 100 positions (USD)", "Top 100 positions (% of peak)"}


def base_dashboards():
    """dashboard name -> (description, filter mode, tiles). A tile is
    (card name, row, col, size_x, size_y, time column) on Metabase's
    24-column grid. The filter mode picks the global filters (see
    dashboard_parameters): 'range' for flows/history dashboards, 'asof'
    for point-in-time holdings dashboards, None for no filters. The time
    filter lands on each card's time column (as_of_day for history cards,
    occurred_at for transactions, snapshot_at for latest-snapshot cards);
    the source filter always lands on silver_source_id. Data Freshness is
    deliberately unfiltered — its job is to show every source, especially
    the stale ones a time filter would hide."""
    note = ("Pre-defined by wealthdb and converged to spec on every `web "
            "start` — duplicate into another collection before customizing.")
    return {
        "Wealth Overview": (
            "The whole picture over time, in USD: net worth, cash vs "
            "positions, and income and cost flows. " + note, "range", [
            # Net worth = positions + cash by construction — the first
            # three tiles reconcile exactly; the trend tile is a monthly
            # AVERAGE, so it intentionally differs from today's value.
            ("Net worth (USD)", 0, 0, 6, 3, "snapshot_at"),
            ("Positions value (USD)", 0, 6, 6, 3, "snapshot_at"),
            ("Cash balance (USD)", 0, 12, 6, 3, "snapshot_at"),
            ("Net worth — monthly trend (USD)", 0, 18, 6, 3, "as_of_day"),
            ("Net worth over time (USD)", 3, 0, 24, 6, "as_of_day"),
            ("Cash vs positions over time (USD)", 9, 0, 24, 6, "as_of_day"),
            ("Income by month (USD)", 15, 0, 12, 6, "occurred_at"),
            ("Fees & taxes by month (USD)", 15, 12, 12, 6, "occurred_at"),
        ]),
        "Allocation": (
            "Where the value sits — asset class, currency, tax wrapper, "
            "management style and the largest positions — as of a chosen "
            "day (default: today). " + note, "asof", [
            ("Allocation by asset class (USD)", 0, 0, 12, 8, "as_of_day"),
            ("Allocation by currency (USD)", 0, 12, 12, 8, "as_of_day"),
            ("Value by tax wrapper (USD)", 8, 0, 12, 6, "as_of_day"),
            ("Value by management style (USD)", 8, 12, 12, 6, "as_of_day"),
            ("Top 100 positions (USD)", 14, 0, 24, 8, "as_of_day"),
        ]),
        "Data Freshness": (
            "Age of each source's latest snapshot — which feeds need a "
            "collector run. Unfiltered by design: it must show every "
            "source, especially stale ones. " + note, None, [
            ("Stalest source (days)", 0, 0, 8, 3, None),
            ("Source freshness", 3, 0, 24, 10, None),
        ]),
    }


def privacy_card_names():
    """Every card on a base dashboard that needs a %-of-peak variant."""
    return {t[0] for _, _, tiles in base_dashboards().values()
            for t in tiles} - PRIVACY_EXEMPT_CARDS


def privacy_card_defs(db_id, model_ids):
    """name -> (card type, display, description, dataset_query, viz
    settings) for the %-of-peak variants of every card the base
    dashboards show: the base defs re-run against the _pct models, plus
    per-widget-peak overrides for the flow charts (whose _pct models
    pre-filter kinds and pre-negate costs, so the cards are plain
    sums)."""
    wanted = privacy_card_names()
    pmid = {n: model_ids[PCT_MODEL_MAP.get(n, n)] for n in model_ids}
    out = {}
    for name, (display, desc, query) in metric_defs(db_id, pmid).items():
        if name in wanted:
            out[privacy_name(name)] = ("metric", display,
                                       desc + PRIVACY_DESC, query, {})
    for name, (display, desc, query, viz) in question_defs(db_id, pmid).items():
        if name in wanted:
            out[privacy_name(name)] = ("question", display,
                                       desc + PRIVACY_DESC, query, viz)
    for name, model in (
            ("Income by month (% of peak)", "report_income_monthly_pct"),
            ("Fees & taxes by month (% of peak)", "report_costs_monthly_pct")):
        out[name] = ("question", "bar", out[name][2],
                     _mbql(db_id, model_ids[model],
                           {"aggregation": [["sum", _dec("value_usd")]],
                            "breakout": [_f("occurred_at", "type/DateTime", "month"),
                                         _f("kind", "type/Text")]}),
                     {"stackable.stack_type": "stacked"})
    return out


def dashboard_defs():
    """dashboard name -> (description, filter mode, sibling dashboard
    name, tiles). Every base dashboard gets a privacy twin: same layout
    and filters, cards swapped for their %-of-peak variants. The sibling
    name links the two views — ensure_dashboards renders it as a switch
    link at the top of each dashboard."""
    out = {}
    for name, (desc, mode, tiles) in base_dashboards().items():
        pname = f"{name} (privacy)"
        out[name] = (desc, mode, pname, tiles)
        ptiles = [(c if c in PRIVACY_EXEMPT_CARDS else privacy_name(c),
                   r, col, sx, sy, t) for c, r, col, sx, sy, t in tiles]
        out[pname] = (
            "Privacy view: monetary values are % of peak (peak daily "
            "global net worth for holdings, the widget's peak month for "
            "flows), so shapes and shares show but absolute amounts do "
            "not. " + desc, mode, name, ptiles)
    return out


def dashboard_parameters(model_ids, mode):
    """The global filters a pre-defined dashboard carries, by mode:
    'range' pairs the source picker with a time range (flows / history
    dashboards), 'asof' pairs it with a single as-of day (point-in-time
    holdings dashboards), None means no filters. The source picker draws
    its dropdown values from the sources model."""
    if mode is None:
        return []
    source = {"id": SOURCE_PARAM_ID, "name": "Source", "slug": "source",
              "type": "string/=", "sectionId": "string", "isMultiSelect": True,
              "values_source_type": "card",
              "values_source_config": {
                  "card_id": model_ids["report_sources_latest"],
                  "value_field": ["field", "silver_source_id",
                                  {"base-type": "type/Text"}]}}
    if mode == "asof":
        # Required + dynamic "today" default: the as-of cards sum daily
        # history (one row per entity per day), so they must never run
        # with the day filter cleared — a required parameter resets to
        # its default instead of clearing. The asset-class picker (no
        # default = all values) is linked only to ASSET_FILTERED_CARDS.
        return [{"id": ASOF_PARAM_ID, "name": "As of day", "slug": "as_of_day",
                 "type": "date/single", "sectionId": "date",
                 "default": "thisday", "required": True},
                source,
                {"id": ASSET_PARAM_ID, "name": "Asset class",
                 "slug": "asset_class", "type": "string/=",
                 "sectionId": "string", "isMultiSelect": True,
                 "values_source_type": "card",
                 "values_source_config": {
                     "card_id": model_ids["report_positions_history"],
                     "value_field": ["field", "asset_class",
                                     {"base-type": "type/Text"}]}}]
    return [
        # "past12months~": the trailing ~ means "include this month".
        # Without it Metabase takes the previous 12 COMPLETE months, which
        # silently drops every row stamped in the current partial month —
        # for latest-snapshot cards that nulls out precisely the sources
        # that are freshest (their snapshot_at is this month).
        {"id": TIME_PARAM_ID, "name": "Time range", "slug": "time_range",
         "type": "date/all-options", "sectionId": "date",
         "default": "past12months~"},
        source,
    ]


def ensure_database(base, sid, db_name, gold_path):
    """Add the gold DuckDB connection, or converge an existing one's
    settings. Returns the database id, or None on failure.

    DuckDB runs in-process in the Metabase JVM; without a cap it helps
    itself to 80% of the machine's RAM and the kernel OOM-kills the JVM
    when several history-heavy dashboard tiles query concurrently. Both
    keys land as instance-level DuckDB config (the driver forwards
    unknown detail keys as JDBC properties); threads is capped because
    peak memory scales with per-query parallelism. Spill goes to the
    driver's hard-wired "<database_file>.tmp", which web/web mounts
    writable. Do NOT move these into init_sql: that runs per connection,
    and DuckDB refuses to re-SET a used temp_directory, which breaks
    every query after the first connection cycle ("" converges the key
    away from older provisions)."""
    details = {"database_file": gold_path, "read_only": True,
               "memory_limit": "2GB", "threads": "8", "init_sql": ""}

    db_id, cur = None, None
    _, dbs = req(base, "/api/database", session=sid)
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
    """Return the id of the collection named `name`, creating it if absent."""
    _, cols = req(base, "/api/collection", session=sid)
    for c in (cols if isinstance(cols, list) else []):
        if c.get("name") == name and not c.get("archived"):
            return c.get("id")
    st, body = req(base, "/api/collection", "POST", {"name": name}, session=sid)
    if st in (200, 201) and body.get("id"):
        print(f"provision: created collection '{name}'")
        return body["id"]
    # Non-fatal: fall back to the default (root) collection.
    print(f"provision: could not create collection '{name}' ({st}): "
          f"{body.get('message')}; using the default collection", file=sys.stderr)
    return None


def pre_defined_cards(base, sid, coll_id):
    """name -> card, for the non-archived cards in the pre-defined
    collection. Scoped to that collection so a user card that happens to
    share a name is never touched."""
    _, cards = req(base, "/api/card", session=sid)
    return {c.get("name"): c for c in (cards if isinstance(cards, list) else [])
            if c.get("collection_id") == coll_id and not c.get("archived")}


def upsert_card(base, sid, by_name, name, payload):
    """Create card `name` or update it in place. Returns (card id,
    'created'|'updated') on success, (None, error text) on failure."""
    existing = by_name.get(name)
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
    """The full /api/card payload shared by models, metrics and questions."""
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
    /api/card payload). Returns (name -> card id, #created, #updated), or
    None on the first failure."""
    ids, n = {}, {"created": 0, "updated": 0}
    for name, payload in payloads.items():
        cid, how = upsert_card(base, sid, by_name, name, payload)
        if cid is None:
            print(f"provision: failed to save {label} '{name}' {how}",
                  file=sys.stderr)
            return None
        ids[name] = cid
        n[how] += 1
    return ids, n["created"], n["updated"]


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
    archive any retired (renamed-away) ones. Idempotent: an existing model
    of the same name is updated in place; re-running converges. The models
    are content-free shims over the gold macros — no source data is baked
    in. Returns model name -> card id, or None on failure."""
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
    ids, created, updated = saved
    archived = archive_all(base, sid, "card",
                           [by_name[n]["id"] for n in RETIRED_MODEL_NAMES
                            if n in by_name])
    print(f"provision: report models (USD/CHF/EUR) — {created} created, "
          f"{updated} updated, {archived} retired (collection '{COLLECTION_NAME}')")
    return ids


def ensure_cards(base, sid, db_id, coll_id, by_name, model_ids):
    """Create/refresh the pre-defined metrics and questions over the models
    in `model_ids`, and archive any retired (renamed-away) ones. Returns
    card name -> id (the dashboards' tile lookup), or None on failure."""
    payloads = {}
    for name, (display, desc, query) in metric_defs(db_id, model_ids).items():
        payloads[name] = card_payload(coll_id, name, "metric", display,
                                      desc, query, {})
    for name, (display, desc, query, viz) in question_defs(db_id, model_ids).items():
        payloads[name] = card_payload(coll_id, name, "question", display,
                                      desc, query, viz)
    for name, (ctype, display, desc, query, viz) in privacy_card_defs(db_id, model_ids).items():
        payloads[name] = card_payload(coll_id, name, ctype, display,
                                      desc, query, viz)
    saved = upsert_cards(base, sid, by_name, payloads, "card")
    if saved is None:
        return None
    ids, created, updated = saved
    archived = archive_all(base, sid, "card",
                           [by_name[n]["id"] for n in RETIRED_CARD_NAMES
                            if n in by_name])
    print(f"provision: metrics + questions — {created} created, "
          f"{updated} updated, {archived} retired")
    return ids


def text_dashcard(dc_id, text):
    """A virtual text tile (no backing card), spanning the top row — used
    for the absolute <-> privacy switch link."""
    return {"id": dc_id, "card_id": None, "row": 0, "col": 0,
            "size_x": 24, "size_y": 1, "series": [], "parameter_mappings": [],
            "visualization_settings": {
                "virtual_card": {"name": None, "display": "text",
                                 "visualization_settings": {},
                                 "dataset_query": {}, "archived": False},
                "text": text,
                "dashcard.background": False,
                "text.align_vertical": "middle"}}


def ensure_dashboards(base, sid, coll_id, card_ids, model_ids):
    """Create/refresh the pre-defined dashboards (and their privacy twins)
    with their global filters (see dashboard_parameters) linked to every
    tile and a switch link to the sibling view on the top row, and archive
    any retired (renamed-away) ones. Parameters and the dashcard list are
    replaced wholesale on every run, so the layout converges to spec (a
    tile added by hand to a pre-defined dashboard does not survive — the
    dashboard description says to duplicate before customizing)."""
    coll = coll_id if coll_id is not None else "root"
    _, items = req(base, f"/api/collection/{coll}/items?models=dashboard", session=sid)
    existing = {d.get("name"): d.get("id") for d in (items.get("data") or [])}

    defs = dashboard_defs()
    n = {"created": 0, "updated": 0}
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
        else:
            n["updated"] += 1
        dash_ids[name] = did

    for name, (desc, mode, sibling, tiles) in defs.items():
        parameters = dashboard_parameters(model_ids, mode)
        tparam = ASOF_PARAM_ID if mode == "asof" else TIME_PARAM_ID
        link = (f"🔓 [Switch to absolute values](/dashboard/{dash_ids[sibling]})"
                if name.endswith(" (privacy)") else
                f"🔒 [Switch to the privacy view — values as % of peak]"
                f"(/dashboard/{dash_ids[sibling]})")

        def tile_mappings(card, tcol):
            if not mode:
                return []
            maps = [{"parameter_id": tparam, "card_id": card_ids[card],
                     "target": ["dimension", _f(tcol, "type/DateTime")]},
                    {"parameter_id": SOURCE_PARAM_ID, "card_id": card_ids[card],
                     "target": ["dimension",
                                _f("silver_source_id", "type/Text")]}]
            if mode == "asof" and card in ASSET_FILTERED_CARDS:
                maps.append({"parameter_id": ASSET_PARAM_ID,
                             "card_id": card_ids[card],
                             "target": ["dimension",
                                        _f("asset_class", "type/Text")]})
            return maps

        # The switch link occupies row 0, so the tiles shift down one row.
        # The asset-class filter renders on the Top-positions tile itself
        # (inline_parameters) rather than in the dashboard's filter bar —
        # it only applies to that one widget.
        dashcards = [text_dashcard(-99, link)]
        dashcards += [{"id": -(i + 1), "card_id": card_ids[card], "row": row + 1,
                       "col": col, "size_x": sx, "size_y": sy, "series": [],
                       "visualization_settings": {},
                       "inline_parameters":
                           [ASSET_PARAM_ID] if mode == "asof"
                           and card in ASSET_FILTERED_CARDS else [],
                       "parameter_mappings": tile_mappings(card, tcol)}
                      for i, (card, row, col, sx, sy, tcol) in enumerate(tiles)]
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
        if parameters:
            st, body = req(base, f"/api/dashboard/{dash_ids[name]}", "PUT",
                           {"parameters": parameters}, session=sid)
            if st not in (200, 201):
                print(f"provision: failed to re-assert filters on '{name}' "
                      f"({st}): {body.get('message')}", file=sys.stderr)
                return 1

    archived = archive_all(base, sid, "dashboard",
                           [existing[name] for name in RETIRED_DASHBOARD_NAMES
                            if existing.get(name) is not None])
    print(f"provision: dashboards — {n['created']} created, {n['updated']} updated, "
          f"{archived} retired")
    return 0


if __name__ == "__main__":
    sys.exit(main())
