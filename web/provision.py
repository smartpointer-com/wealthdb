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

    tables = ensure_synced(a.base, sid, db_id)
    if tables is None:
        return 1
    global FIELD_IDS, CURRENCY_FIELD_ID, SOURCE_FIELD_ID
    FIELD_IDS = field_ids_from(tables)
    CURRENCY_FIELD_ID = FIELD_IDS.get(("report_returns", "currency"))
    SOURCE_FIELD_ID = FIELD_IDS.get(("report_returns", "silver_source_id"))

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
# `_latest` suffix, plus the per-widget flow models retired when the
# privacy flow charts moved to native SQL over web_transactions;
# archived on provision so a re-run cleans them up.
RETIRED_MODEL_NAMES = ["report_global", "report_portfolios",
                       "report_accounts", "report_positions",
                       "report_income_monthly_pct", "report_costs_monthly_pct"]


def report_models():
    """model name -> (native SQL, description). The report_*_multi DuckDB
    macros (migration 0024) are the single source of truth; each model only
    wraps a macro to bind the as-of and to render epoch columns as TIMESTAMP
    (`to_timestamp` -> naive-UTC) for Metabase — except the taxonomy models,
    which wrap the web_* serving views (migration 0032) that do that
    rendering and the cash fold-in themselves. The macros already emit DECIMAL
    money/quantity columns and one value column set per reporting currency
    (USD/CHF/EUR), so no value casting is needed here. The `_latest` reports
    are as of each source's latest snapshot; the `_history` reports carry value
    forward per day; the `_pct` family are the privacy variants for standalone
    privacy browsing (values as % of the latest global net worth,
    absolute-value columns dropped) — the privacy dashboards' charts are
    native SQL over the gold web_* serving views instead, recomputing their
    denominator per query so the dashboard pickers rescale them (see
    privacy_card_defs). The returns models wrap the materialized
    report_returns TABLE (migration 0026) instead of a macro, with the same
    epoch-to-TIMESTAMP rendering."""
    def wrap(from_expr, ts_cols=(), exclude=(), where=""):
        parts = [f"CAST(to_timestamp({c}) AS TIMESTAMP) AS {c}" for c in ts_cols]
        excl = f" EXCLUDE ({', '.join(exclude)})" if exclude else ""
        cond = f" WHERE {where}" if where else ""
        return f"SELECT *{excl} REPLACE ({', '.join(parts)}) FROM {from_expr}{cond}"

    # Scalar subquery with the latest global net worth per currency — the
    # shared normalization constant of the privacy (_pct) models. A fixed
    # constant keeps every aggregate's shape; the privacy dashboards'
    # charts recompute a selection-aware denominator per query instead
    # (privacy_card_defs), so this constant only backs standalone model
    # browsing and the privacy scalars' drill-through. Guarded to NULL
    # when the latest total is zero or negative (empty or under-water
    # gold): dividing would render inf/NaN resp. sign-flipped
    # percentages, where NULL just blanks the values.
    NW_LATEST = ("(SELECT " + ", ".join(
        f"CASE WHEN total_value_{c} > 0 THEN total_value_{c} END AS nw_{c}"
        for c in ("usd", "chf", "eur")) +
        f" FROM report_global_multi({MAX_BIGINT})) AS nw")

    def pct_wrap(from_expr, value_cols, ts_cols=(), exclude=()):
        """Privacy wrapper: every monetary column becomes % of the latest
        global net worth in its currency (one constant scale per
        currency, so every aggregate keeps its shape), and columns that
        would leak absolute values are dropped."""
        repl = [f"CAST(to_timestamp({c}) AS TIMESTAMP) AS {c}" for c in ts_cols]
        repl += [f"{c} / nw.nw_{c.rsplit('_', 1)[1]} * 100 AS {c}"
                 for c in value_cols]
        excl = ", ".join(["nw_usd", "nw_chf", "nw_eur", *exclude])
        return (f"SELECT * EXCLUDE ({excl}) REPLACE ({', '.join(repl)}) "
                f"FROM {from_expr}, {NW_LATEST}")

    def taxonomy_history(view, pct=False):
        """Breakdown-by-taxonomy models over the gold web_* serving views
        (migration 0032), which already render as_of_day as TIMESTAMP and
        fold each source's cash balance into a class/vehicle of its own so
        the rows sum exactly to net worth. pct=True rescales to % of the
        latest global net worth (per currency)."""
        if not pct:
            return f"SELECT * FROM {view}"
        vals = ", ".join(f"value_{c} / nw.nw_{c} * 100 AS value_{c}"
                         for c in ("usd", "chf", "eur"))
        return (f"SELECT * EXCLUDE (nw_usd, nw_chf, nw_eur) "
                f"REPLACE ({vals}) FROM {view}, {NW_LATEST}")

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
                 exclude=("start_value", "end_value", "net_flow"),
                 where="grain IN ('sources', 'global')"),
            "Privacy variant of report_returns: returns are scale-free ratios, "
            "so privacy is redaction rather than normalization — the absolute "
            "money columns are dropped and only the sources and global grains "
            "are kept (no account / portfolio labels)."),
        # Privacy (_pct) variants of the models the pre-defined cards are
        # built on: same columns and grain, but monetary values are % of
        # the latest global net worth (per currency) and columns that
        # would leak absolute values (base-currency totals, quantities,
        # amounts, prices) are dropped. These back standalone privacy
        # browsing and the privacy scalars' drill-through; the privacy
        # dashboards' charts are native SQL over the web_* serving views
        # (privacy_card_defs), whose denominators follow the pickers.
        "report_sources_latest_pct": (
            pct_wrap(f"report_sources_multi{L}", V3, ["snapshot_at"], BASE3),
            "Privacy variant of report_sources_latest: totals as % of the "
            "latest global net worth (per currency); base-currency columns "
            "dropped."),
        "report_sources_history_pct": (
            pct_wrap("report_sources_history_multi()", V3, ["as_of_day"], BASE3),
            "Privacy variant of report_sources_history: totals as % of the "
            "latest global net worth (per currency); base-currency columns "
            "dropped."),
        "report_accounts_history_pct": (
            pct_wrap("report_accounts_history_multi()", V3, ["as_of_day"], BASE3),
            "Privacy variant of report_accounts_history: totals as % of the "
            "latest global net worth (per currency); base-currency columns "
            "dropped."),
        "report_positions_history_pct": (
            pct_wrap("report_positions_history_multi()", V1,
                     ["as_of_day", "snapshot_at"], ("quantity", "market_value")),
            "Privacy variant of report_positions_history: values as % of the "
            "latest global net worth (per currency); quantity and "
            "native-currency market value dropped."),
        "report_transactions_pct": (
            pct_wrap(f"report_transactions_multi(0, {MAX_BIGINT})", V1,
                     ["occurred_at"],
                     ("gross_amount", "net_amount", "quantity", "price")),
            "Privacy variant of report_transactions: values as % of the "
            "latest global net worth (per currency); native-currency "
            "amounts, quantity and price dropped."),
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
            "day, carried forward, in USD/CHF/EUR. Sums to net worth by "
            "construction; liability classes are negative."),
        "report_asset_classes_history_pct": (
            taxonomy_history("web_asset_classes_history", pct=True),
            "Privacy variant of report_asset_classes_history: values as % of "
            "the latest global net worth (per currency)."),
        # Vehicle (wrapper) breakdown — the second taxonomy dimension.
        # Same construction as asset classes: sums to net worth, cash
        # balances counted as a 'demand_deposit' vehicle.
        "report_vehicles_history": (
            taxonomy_history("web_vehicles_history"),
            "One row per vehicle (wrapper, incl. a 'demand_deposit' vehicle "
            "for cash) per source per day, carried forward, in USD/CHF/EUR. "
            "Sums to net worth by construction."),
        "report_vehicles_history_pct": (
            taxonomy_history("web_vehicles_history", pct=True),
            "Privacy variant of report_vehicles_history: values as % of the "
            "latest global net worth (per currency)."),
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

# Account kinds fenced out of the investment flow charts. A credit card
# books `interest` (a finance charge) and `fee` (an annual fee) of its own
# — the same transaction kinds the charts select on — so without this
# fence a card would report spending costs as investment income and
# portfolio costs. Card flows are spending; they belong to the spending
# surface, not to the income / fees charts. Fenced on the account_kind
# column the transaction report macros carry (migration 0039), which is
# NULL for a transaction whose account is absent from `accounts`; NULL
# must be KEPT, so the fence is written as "not card, or unknown".
FLOW_CHART_EXCLUDED_ACCOUNT_KINDS = ["card"]

# Metric names retired when the pre-defined cards switched from
# identifier-style to prose names (dashboards and widgets read better as
# prose); archived on provision so a re-run cleans them up.
RETIRED_CARD_NAMES = ["net_worth_usd_current", "net_worth_chf_current",
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
                      "Source freshness (% of peak)"]

# Dashboard names retired by renames ("Net Worth" undersold the income /
# cost flow tiles); archived on provision so a re-run cleans them up.
RETIRED_DASHBOARD_NAMES = ["Net Worth"]

# Every dashboard has a privacy twin whose cards show shares (%) of the
# latest total across the selected sources instead of money. Cards listed
# here show no monetary values (percentages, indices, source names), so the
# twin reuses them as-is. The returns scalars and charts are all
# percentage/index-only; only the by-source table carries money.
PRIVACY_EXEMPT_CARDS = {"Stalest source (days)", "Returns age (days)",
                        "Return (TWR)", "Return (MWR)", "Annualized return (TWR)",
                        "Cumulative return (log scale)", "Monthly returns (TWR)",
                        "Quarterly returns (TWR)", "Annual returns (TWR)"}

# Denominator-neutral by design: each card's body text names its own
# denominator (latest total, chosen day's total, or peak month).
PRIVACY_DESC = " Privacy view: values are shares (%), not absolute amounts."

# The native-SQL returns charts: the Currency / Start-year pickers map onto
# their {{currency}} / {{start_year}} template variables (the MBQL returns
# cards get the same pickers on their currency / window_from_year dimensions).
RETURNS_NATIVE_CARDS = {"Cumulative return (log scale)", "Monthly returns (TWR)",
                        "Quarterly returns (TWR)", "Annual returns (TWR)"}

# The whole-portfolio scalars — global grain, so the Source picker doesn't
# apply (their silver_source_id is '').
RETURNS_GLOBAL_SCALARS = {"Return (TWR)", "Return (MWR)", "Annualized return (TWR)"}

RETURNS_PRIVACY_DESC = (" Privacy view: returns are scale-free ratios and "
                        "show unchanged; the absolute money columns are "
                        "redacted.")


def privacy_name(name):
    """Card title for the privacy variant of card `name`: a uniform
    '(privacy)' suffix, displacing a USD marker in the base name."""
    return f"{name.replace(' (USD)', '')} (privacy)"


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


# The native returns charts carry three template variables — currency, start
# year, and (a multi-value field filter) source. The Returns dashboard's
# Currency / Start-year / Source pickers map onto them; the same pickers map
# onto the MBQL cards' currency / window_from_year / silver_source_id
# dimensions. Currency and start year default so a card still runs standalone.
CURRENCY_TAG = {"id": "ccy-tag", "name": "currency", "display-name": "Currency",
                "type": "text", "default": "USD", "required": True}
START_YEAR_TAG = {"id": "year-tag", "name": "start_year",
                  "display-name": "Start year", "type": "number",
                  "default": "0", "required": True}

# The gold columns backing the native cards' field filters: the returns
# charts filter report_returns; the privacy cards filter the web_* serving
# views (gold migration 0032). Field ids are per-Metabase-instance
# (assigned when the DB syncs), so main() resolves them at provision time
# into FIELD_IDS — they can't be hard-coded. A missing id (fresh install
# before the first sync) leaves that filter off the affected cards; the
# next provision — post-sync — wires it up.
FILTER_FIELD_COLUMNS = {
    "report_returns": ("currency", "silver_source_id"),
    "web_sources_history": ("as_of_day", "silver_source_id"),
    "web_transactions": ("occurred_at", "silver_source_id"),
    "web_asset_classes_history": ("as_of_day", "silver_source_id"),
    "web_vehicles_history": ("as_of_day", "silver_source_id"),
    "web_accounts_history": ("as_of_day", "silver_source_id"),
    "web_positions_history": ("as_of_day", "silver_source_id",
                              "asset_class", "vehicle"),
}
FIELD_IDS = {}          # (table, column) -> field id, filled by main()
CURRENCY_FIELD_ID = None
SOURCE_FIELD_ID = None


def returns_tags():
    """Template tags for the native returns charts. Currency is a field filter
    (a dropdown, values from the currency column) when its field id is known,
    else a plain text variable (free-text fallback for a not-yet-synced DB).
    Source is a field filter when its id is known. Start year is always a
    plain number variable (it drives a `year(end_day) >= …` range, which a
    field filter can't express)."""
    if CURRENCY_FIELD_ID is not None:
        # A field-filter default is a LIST (string/= is multi-value-shaped),
        # unlike a plain text variable's bare-string default.
        currency = {"id": "ccy-ff", "name": "currency", "display-name": "Currency",
                    "type": "dimension", "dimension": ["field", CURRENCY_FIELD_ID, None],
                    "widget-type": "string/=", "default": ["USD"], "required": True}
    else:
        currency = CURRENCY_TAG
    tags = {"currency": currency, "start_year": START_YEAR_TAG}
    if SOURCE_FIELD_ID is not None:
        tags["source_ff"] = {"id": "src-ff", "name": "source_ff",
                             "display-name": "Source", "type": "dimension",
                             "dimension": ["field", SOURCE_FIELD_ID, None],
                             "widget-type": "string/=", "default": None}
    return tags


def _native(db_id, sql, tags):
    """A native-SQL dataset_query with template variables."""
    return {"type": "native", "database": db_id,
            "native": {"query": sql, "template-tags": tags}}


def _returns_source_union(granularity, value_col):
    """A UNION selecting one column from the per-source rows plus the global
    grain relabelled as the toggleable '(all sources)' pseudo-source, for one
    granularity's per-period buckets. Filtered to {{currency}} and to periods
    ending on/after {{start_year}} (0 = all). The Source field filter (an
    optional [[…]] clause) narrows the real sources but never the '(all
    sources)' line, so the global reference stays visible while a subset is
    selected. Shared by the period and growth charts."""
    # Currency: a field filter inside each subquery (dropdown) when its id is
    # known, else a plain-variable equality in the outer WHERE. Source: an
    # optional [[…]] field-filter clause (omitted when nothing is selected, and
    # only emitted when the tag exists — referencing an undefined {{tag}} would
    # make the query invalid).
    cf = CURRENCY_FIELD_ID is not None
    ccy_sub = "\n     AND {{currency}}" if cf else ""
    ccy_outer = "" if cf else "currency = {{currency}}\n   AND "
    src_clause = "\n     [[AND {{source_ff}}]]" if SOURCE_FIELD_ID is not None else ""
    return (
        "WITH s AS (\n"
        "  SELECT silver_source_id, currency, end_day, " + value_col + " AS v\n"
        "    FROM report_returns\n"
        "   WHERE grain = 'sources' AND granularity = '" + granularity + "'\n"
        "     AND NOT is_summary" + ccy_sub + src_clause + "\n"
        "  UNION ALL\n"
        "  SELECT '(all sources)', currency, end_day, " + value_col + "\n"
        "    FROM report_returns\n"
        "   WHERE grain = 'global' AND granularity = '" + granularity + "'\n"
        "     AND NOT is_summary" + ccy_sub + ")\n"
        "SELECT silver_source_id AS source, end_day, v\n"
        "  FROM s\n"
        " WHERE " + ccy_outer + "year(to_timestamp(end_day)) >= {{start_year}}")


def returns_period_sql(granularity):
    """Per-period TWR, one row per (source, period) plus the '(all sources)'
    global line — the pseudo-source that fixes the split-by-source
    inconsistency (every chart shows sources and the global line together)."""
    return ("SELECT source, to_timestamp(end_day) AS period, v AS twr\n"
            "  FROM (\n" + _returns_source_union(granularity, "twr") + "\n) u\n"
            " ORDER BY end_day")


def returns_growth_sql():
    """Cumulative growth index (base 100), per source and the '(all sources)'
    line, derived from the ENGINE's since-<year> windowed TWRs — NOT by
    chaining the per-period buckets. Chaining calendar-month/quarter Modified-
    Dietz returns is unsound here: a flow landing between two sparse snapshots
    poisons that bucket (a mid-month deposit with no fresh snapshot reads as a
    huge loss, then a huge gain next period), so a chained index can diverge by
    hundreds of points from the true TWR for sparse-snapshot sources — a real
    gainer chained all the way down to a spurious near-total loss. The windowed
    summaries use the engine's snapshot-
    aligned chain, so they are correct and — being the very figures the scalars
    and by-source table show — the chart agrees with them by construction.

    Since window_from_year=Y is the TWR from Jan 1 Y to today, the index at the
    start of year Y is G(Y) = base / (1 + TWR_since_Y); normalizing the earliest
    visible year to 100 gives G(Y) = 100 * (1 + TWR_since_Ymin) / (1 + TWR_Y).
    {{start_year}} sets Ymin (the index rebases to the chosen start); null
    windows (degenerate inception) drop out, so the line begins where the return
    is first defined. Annual granularity — one point per year — is the price of
    correctness here; a finer curve would need per-month windowed summaries."""
    cf = CURRENCY_FIELD_ID is not None
    ccy_sub = "\n     AND {{currency}}" if cf else ""
    ccy_f = "" if cf else "currency = {{currency}} AND "
    src = "\n     [[AND {{source_ff}}]]" if SOURCE_FIELD_ID is not None else ""
    return (
        "WITH w AS (\n"
        "  SELECT silver_source_id AS source, currency, window_from_year AS yr, twr\n"
        "    FROM report_returns\n"
        "   WHERE grain = 'sources' AND granularity = 'total' AND is_summary\n"
        "     AND window_from_year > 0" + ccy_sub + src + "\n"
        "  UNION ALL\n"
        "  SELECT '(all sources)', currency, window_from_year, twr\n"
        "    FROM report_returns\n"
        "   WHERE grain = 'global' AND granularity = 'total' AND is_summary\n"
        "     AND window_from_year > 0" + ccy_sub + "),\n"
        "f AS (\n"
        "  SELECT source, yr, twr FROM w\n"
        "   WHERE " + ccy_f + "yr >= {{start_year}} AND twr IS NOT NULL)\n"
        "SELECT source, make_date(yr, 1, 1) AS year,\n"
        "       100 * first_value(1 + twr) OVER (PARTITION BY source ORDER BY yr)\n"
        "           / (1 + twr) AS growth_index\n"
        "  FROM f\n"
        " ORDER BY yr")


def _series_viz(time_col, series_col, metric, *, log=False, percent=False):
    """Viz for a native time series split by a category: x = time_col,
    one line per series_col, y = metric. Native queries need the axes named
    explicitly (there is no MBQL breakout for Metabase to infer them from)."""
    viz = {"graph.dimensions": [time_col, series_col], "graph.metrics": [metric]}
    if log:
        viz["graph.y_axis.scale"] = "log"
    if percent:
        viz["column_settings"] = {f'["name","{metric}"]': {"number_style": "percent"}}
    return viz


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
    settings). Mostly MBQL over the models so the dashboards' filters map
    onto card dimensions; the returns charts (cumulative index, per-period
    split-by-source) are native SQL — window functions and the pseudo-source
    UNION need SQL — and take the Currency / Start-year pickers as template
    variables instead."""
    def kind_in(kinds):
        return ["=", _f("kind", "type/Text")] + kinds

    def flow_kinds(kinds):
        """Transaction-kind filter for the monthly flow charts, fenced to
        investment accounts: `!=` alone would silently drop the rows whose
        account_kind is NULL (a transaction with no matching accounts row),
        so the null branch is spelled out rather than left to Metabase's
        null handling."""
        ak = _f("account_kind", "type/Text")
        return ["and", kind_in(kinds),
                ["or", ["is-null", ak],
                 ["!=", ak] + FLOW_CHART_EXCLUDED_ACCOUNT_KINDS]]

    def part(grain, granularity):
        """Filter to one (grain, granularity) partition of report_returns
        (the returns scalars/table use the summary 'total' partition; the
        Start-year picker then selects the window_from_year within it)."""
        return ["and", ["=", _f("grain", "type/Text"), grain],
                ["=", _f("granularity", "type/Text"), granularity]]

    month = _f("occurred_at", "type/DateTime", "month")
    days_stale = ["datetime-diff", _f("snapshot_at", "type/DateTime"),
                  ["now"], "day"]
    twr = _f("twr", "type/Float")
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
            "coupons, staking) per month in USD, stacked by kind. Credit-"
            "card accounts are excluded — a card's interest is a finance "
            "charge on spending, not investment income.",
            _mbql(db_id, mid["report_transactions"],
                  {"filter": flow_kinds(INCOME_KINDS),
                   "aggregation": [["sum", _dec("value_usd")]],
                   "breakout": [month, _f("kind", "type/Text")]}),
            {"stackable.stack_type": "stacked"}),
        "Fees & taxes by month (USD)": ("bar",
            "Fees and withheld taxes per month in USD, stacked by kind; "
            "debits are negated so costs read as positive bars. Credit-"
            "card accounts are excluded — card fees are spending costs, "
            "not portfolio costs.",
            _mbql(db_id, mid["report_transactions"],
                  {"filter": flow_kinds(COST_KINDS),
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
        "Allocation by vehicle (USD)": ("row",
            "Value (USD) by vehicle (the wrapper an exposure is held "
            "through: stock, etf, fund, spv, bond, physical, …) as of a "
            "day, including a 'demand_deposit' vehicle for cash — the bars "
            "sum exactly to net worth. The wrapper-dimension companion to "
            "Allocation by asset class. Built for the Allocation dashboard, "
            "which supplies the as-of day; opened standalone, filter "
            "as_of_day to a single day first.",
            _mbql(db_id, mid["report_vehicles_history"],
                  {"aggregation": [["sum", _dec("value_usd")]],
                   "breakout": [_f("vehicle", "type/Text")],
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
            "asset-class and vehicle filters. Built for the Allocation "
            "dashboard, which supplies the as-of day; opened standalone, "
            "filter as_of_day to a single day first.",
            _mbql(db_id, mid["report_positions_history"],
                  {"aggregation": [["sum", _dec("value_usd")]],
                   "breakout": [_f("symbol", "type/Text"),
                                _f("name", "type/Text"),
                                _f("asset_class", "type/Text"),
                                _f("vehicle", "type/Text")],
                   "order-by": [["desc", ["aggregation", 0]]],
                   "limit": 100}),
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
                  {"filter": part("global", "total"),
                   "aggregation": [["max", twr]]}),
            _percent_viz("max")),
        "Return (MWR)": ("scalar",
            "Whole-portfolio money-weighted return (XIRR) over the chosen "
            "window — the return the invested cash experienced, external flows "
            "included. Built for the Returns dashboard (Currency + Start-year "
            "pickers); opened standalone, set those first.",
            _mbql(db_id, mid["report_returns_redacted"],
                  {"filter": part("global", "total"),
                   "aggregation": [["max", _f("mwr", "type/Float")]]}),
            _percent_viz("max")),
        "Annualized return (TWR)": ("scalar",
            "The window's time-weighted return restated as a constant "
            "per-year rate (n/a for windows under a year). Built for the "
            "Returns dashboard (Currency + Start-year pickers); opened "
            "standalone, set those first.",
            _mbql(db_id, mid["report_returns_redacted"],
                  {"filter": part("global", "total"),
                   "aggregation": [["max", _f("twr_annualized", "type/Float")]]}),
            _percent_viz("max")),
        # Native cumulative + per-period charts. All split by source with the
        # global grain unioned in as a toggleable '(all sources)' line; the
        # Currency / Start-year pickers map onto their {{currency}} /
        # {{start_year}} variables.
        "Cumulative return (log scale)": ("line",
            "Growth of 100, indexed from the chosen start year, per source and "
            "the '(all sources)' portfolio line — derived from the engine's "
            "since-<year> returns (so it matches the scalars and the by-source "
            "table exactly). Annual granularity. Log y-axis so a steady "
            "compounding rate reads as a straight line and every source is "
            "comparable regardless of size. Built for the Returns dashboard.",
            _native(db_id, returns_growth_sql(), returns_tags()),
            _series_viz("year", "source", "growth_index", log=True)),
        "Monthly returns (TWR)": ("line",
            "Time-weighted return per month, one line per source plus the "
            "'(all sources)' portfolio line. Built for the Returns dashboard "
            "(Currency + Start-year pickers).",
            _native(db_id, returns_period_sql("monthly"), returns_tags()),
            _series_viz("period", "source", "twr", percent=True)),
        "Quarterly returns (TWR)": ("line",
            "Time-weighted return per quarter, one line per source plus the "
            "'(all sources)' portfolio line. Built for the Returns dashboard "
            "(Currency + Start-year pickers).",
            _native(db_id, returns_period_sql("quarterly"), returns_tags()),
            _series_viz("period", "source", "twr", percent=True)),
        "Annual returns (TWR)": ("line",
            "Time-weighted return per calendar year, one line per source plus "
            "the '(all sources)' portfolio line. Built for the Returns "
            "dashboard (Currency + Start-year pickers).",
            _native(db_id, returns_period_sql("annual"), returns_tags()),
            _series_viz("period", "source", "twr", percent=True)),
        "Returns by source": ("table",
            "Returns per source over the chosen window: TWR and MWR, plain and "
            "annualized, with start/end values, net external flow and the "
            "quality flags that explain every n/a. Built for the Returns "
            "dashboard (Currency + Start-year pickers); opened standalone, set "
            "those first.",
            _mbql(db_id, mid["report_returns"],
                  {"filter": part("sources", "total"),
                   "fields": [_f("silver_source_id", "type/Text"),
                              _f("entity_label", "type/Text"),
                              twr,
                              _f("twr_annualized", "type/Float"),
                              _f("mwr", "type/Float"),
                              _f("mwr_annualized", "type/Float"),
                              _dec("start_value"), _dec("end_value"),
                              _dec("net_flow"),
                              _f("quality", "type/Text")],
                   "order-by": [["asc", _f("silver_source_id", "type/Text")]]}),
            _percent_viz("twr", "twr_annualized", "mwr", "mwr_annualized")),
        "Stalest source (days)": ("scalar",
            "Days since the oldest source's latest snapshot — how out of "
            "date the worst feed is.",
            _mbql(db_id, mid["report_sources_latest"],
                  {"expressions": {"days_stale": days_stale},
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
# renders inline on the Top-positions tile only. The Returns dashboards
# carry a required currency picker (static USD/CHF/EUR, default USD)
# instead of a time filter — the periods are precomputed buckets. The
# parameter ids are arbitrary but must be stable across runs so
# re-provisioning converges instead of accumulating parameters.
TIME_PARAM_ID = "aa5df100"
SOURCE_PARAM_ID = "aa5df101"
ASOF_PARAM_ID = "aa5df102"
ASSET_PARAM_ID = "aa5df103"
CURRENCY_PARAM_ID = "aa5df104"
START_YEAR_PARAM_ID = "aa5df105"
VEHICLE_PARAM_ID = "aa5df106"

# The asset-class and vehicle filters (the two taxonomy dimensions) are
# linked only to these tiles: the Top-positions widgets, which list
# individual holdings. The breakdown widgets each already group by one of
# the dimensions, so filtering them by it would mostly self-select.
POSITION_FILTERED_CARDS = {"Top 100 positions (USD)", "Top 100 positions (privacy)"}


def base_dashboards():
    """dashboard name -> (description, filter mode, tiles). A tile is
    (card name, row, col, size_x, size_y, time column) on Metabase's
    24-column grid. The filter mode picks the global filters (see
    dashboard_parameters): 'range' for flows/history dashboards, 'asof'
    for point-in-time holdings dashboards, 'returns' for the returns
    dashboards (a required currency picker; no time filter — the periods
    are precomputed buckets, and the summary rows ignore windows by
    construction), None for no filters. The time filter lands on each
    card's time column (as_of_day for history cards, occurred_at for
    transactions, snapshot_at for latest-snapshot cards); the source
    filter lands on silver_source_id — in 'returns' mode only on the
    by-source tiles (RETURNS_SOURCE_CARDS), since the other tiles show
    the global grain, whose silver_source_id is ''. Data Freshness is
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
            # The two taxonomy dimensions side by side on the top row.
            ("Allocation by asset class (USD)", 0, 0, 12, 8, "as_of_day"),
            ("Allocation by vehicle (USD)", 0, 12, 12, 8, "as_of_day"),
            ("Allocation by currency (USD)", 8, 0, 12, 8, "as_of_day"),
            ("Value by tax wrapper (USD)", 8, 12, 12, 8, "as_of_day"),
            ("Value by management style (USD)", 16, 0, 24, 6, "as_of_day"),
            ("Top 100 positions (USD)", 22, 0, 24, 8, "as_of_day"),
        ]),
        "Returns": (
            "How the portfolio performed — time-weighted (TWR) and "
            "money-weighted (MWR) returns, per period and per source, in a "
            "chosen currency (default USD). Use the Start-year picker to "
            "rescope past the noisy inception period (0 = since inception); "
            "the since-inception TWR is often n/a because the first months "
            "are degenerate. " + note,
            "returns", [
            # Scalars rescope to the since-<start year> window; charts split
            # by source with the global grain as a toggleable '(all sources)'
            # line. The cumulative chart is log-scaled (returns go negative,
            # so a growth index — always positive — is what a log axis can
            # show). MWR is a summary figure only (the per-period buckets
            # carry TWR); that is engine behavior the scalars mirror.
            ("Return (TWR)", 0, 0, 8, 3, None),
            ("Return (MWR)", 0, 8, 8, 3, None),
            ("Annualized return (TWR)", 0, 16, 8, 3, None),
            ("Cumulative return (log scale)", 3, 0, 24, 8, None),
            ("Monthly returns (TWR)", 11, 0, 8, 6, None),
            ("Quarterly returns (TWR)", 11, 8, 8, 6, None),
            ("Annual returns (TWR)", 11, 16, 8, 6, None),
            ("Returns by source", 17, 0, 24, 8, None),
        ]),
        "Data Freshness": (
            "Age of each source's latest snapshot — which feeds need a "
            "collector run. Unfiltered by design: it must show every "
            "source, especially stale ones. " + note, None, [
            ("Stalest source (days)", 0, 0, 8, 3, None),
            ("Returns age (days)", 0, 8, 8, 3, None),
            ("Source freshness", 3, 0, 24, 10, None),
        ]),
    }


def view_tags(table, spec):
    """Field-filter template tags for a native privacy card over serving
    view `table`. spec maps tag name -> (column, widget-type); a tag
    whose field id has not synced yet is omitted — the SQL builders then
    drop the matching [[AND {{tag}}]] clause (referencing an undefined
    tag would invalidate the query) and PRIVACY_PARAM_TARGETS skips its
    picker mapping until a later provision."""
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
    absent (field id not yet synced)."""
    return "\n     [[AND {{%s}}]]" % name if name in tags else ""


# Dashboard picker -> template tag wiring for the native privacy cards,
# rebuilt by privacy_card_defs (only tags whose field id resolved are
# included). ensure_dashboards reads it to map the privacy twins'
# pickers; cards absent here take the default MBQL dimension mappings.
PRIVACY_PARAM_TARGETS = {}


def privacy_card_defs(db_id, model_ids):
    """name -> (card type, display, description, dataset_query, viz
    settings) for the privacy variants of every card the base dashboards
    show. The three scalars are MBQL ratios over the _pct sources model:
    a ratio of sums is scale-free and both legs see the dashboard's
    filters, so they read as shares of the selected sources' latest
    total (net worth itself always 100). Every chart is native SQL over
    the gold web_* serving views, recomputing its normalization
    denominator in-query with the same filters applied: holdings charts
    divide by the latest total across the selected sources, the flow
    charts by their own peak month within the selected window and
    sources (the tallest bar always reads 100). The returns twin
    redacts instead — returns are already scale-free ratios."""
    PRIVACY_PARAM_TARGETS.clear()

    def register(name, tags, pairs):
        PRIVACY_PARAM_TARGETS[name] = [(pid, t) for pid, t in pairs
                                       if t in tags]

    out = {}

    # -- Wealth Overview scalars (MBQL ratios; filters land on the
    # snapshot_at / silver_source_id dimensions as usual).
    latest = model_ids["report_sources_latest_pct"]

    def share(num_col):
        return _mbql(db_id, latest,
                     {"aggregation": [["*", ["/",
                          ["sum", _dec(num_col)],
                          ["sum", _dec("total_value_usd")]], 100]]})

    out["Net worth (privacy)"] = ("question", "scalar",
        "Always 100 by construction — the selected sources' latest net "
        "worth as a share of itself, the anchor every other percentage "
        "on this dashboard is relative to." + PRIVACY_DESC,
        share("total_value_usd"), {})
    out["Positions value (privacy)"] = ("question", "scalar",
        "Market value of all positions as % of the selected sources' "
        "latest net worth; sums to 100 with the cash share." + PRIVACY_DESC,
        share("positions_value_usd"), {})
    out["Cash balance (privacy)"] = ("question", "scalar",
        "Cash as % of the selected sources' latest net worth; sums to "
        "100 with the positions share." + PRIVACY_DESC,
        share("cash_balance_usd"), {})

    # -- The holdings time series: % of the total at the END of the
    # selected window — the last charted day, which is today whenever
    # the window is open-ended. The nw CTE re-applies both pickers: the
    # time filter picks the anchor day, the source filter makes a subset
    # rescale to its own anchor total — so the envelope ends at 100 at
    # the window's last day and tops 100 wherever it previously peaked
    # higher. The > 0 guard blanks a selection whose anchor total is
    # zero or negative — dividing would render inf/NaN resp.
    # sign-flipped bands.
    sh_tags = view_tags("web_sources_history",
                        {"time_range": ("as_of_day", "date/all-options"),
                         "source": ("silver_source_id", "string/=")})
    nw_cte = (
        "WITH nw AS (\n"
        "  SELECT CASE WHEN sum(total_value_usd) > 0\n"
        "              THEN sum(total_value_usd)::DOUBLE END AS denom\n"
        "    FROM web_sources_history\n"
        "   WHERE as_of_day = (SELECT max(as_of_day) FROM web_sources_history\n"
        "                       WHERE TRUE" + _cl(sh_tags, "time_range") + ")"
        + _cl(sh_tags, "source") + ")\n")
    sh_where = ("\n WHERE TRUE" + _cl(sh_tags, "time_range")
                + _cl(sh_tags, "source"))
    out["Net worth — monthly trend (privacy)"] = ("question", "smartscalar",
        "Average daily net worth per month as % of the selected sources' "
        "net worth at the window's end, with the change vs the month "
        "before." + PRIVACY_DESC,
        _native(db_id, nw_cte +
            "SELECT CAST(date_trunc('month', as_of_day) AS TIMESTAMP) AS month,\n"
            "       sum(total_value_usd)::DOUBLE / count(DISTINCT as_of_day)\n"
            "           / (SELECT denom FROM nw) * 100 AS avg_pct\n"
            "  FROM web_sources_history" + sh_where + "\n"
            " GROUP BY 1\n ORDER BY 1", sh_tags),
        {})
    out["Net worth over time (privacy)"] = ("question", "area",
        "Daily net worth as % of the selected sources' total at the "
        "window's end, stacked by source: the envelope ends at 100 and "
        "exceeds 100 wherever earlier net worth topped the window-end "
        "total." + PRIVACY_DESC,
        _native(db_id, nw_cte +
            "SELECT as_of_day, silver_source_id,\n"
            "       sum(total_value_usd)::DOUBLE / (SELECT denom FROM nw)"
            " * 100 AS total_value_pct\n"
            "  FROM web_sources_history" + sh_where + "\n"
            " GROUP BY 1, 2\n ORDER BY 1", sh_tags),
        {**_series_viz("as_of_day", "silver_source_id", "total_value_pct"),
         "stackable.stack_type": "stacked"})
    out["Cash vs positions over time (privacy)"] = ("question", "area",
        "Daily cash and positions shares of the selected sources' total "
        "at the window's end, stacked; the two bands sum to 100 at the "
        "window's last day." + PRIVACY_DESC,
        _native(db_id, nw_cte +
            "SELECT as_of_day,\n"
            "       sum(cash_balance_usd)::DOUBLE / (SELECT denom FROM nw)"
            " * 100 AS cash_pct,\n"
            "       sum(positions_value_usd)::DOUBLE / (SELECT denom FROM nw)"
            " * 100 AS positions_pct\n"
            "  FROM web_sources_history" + sh_where + "\n"
            " GROUP BY 1\n ORDER BY 1", sh_tags),
        {"graph.dimensions": ["as_of_day"],
         "graph.metrics": ["cash_pct", "positions_pct"],
         "stackable.stack_type": "stacked"})
    for name in ("Net worth — monthly trend (privacy)",
                 "Net worth over time (privacy)",
                 "Cash vs positions over time (privacy)"):
        register(name, sh_tags, [(TIME_PARAM_ID, "time_range"),
                                 (SOURCE_PARAM_ID, "source")])

    # -- The flow charts: % of the peak month WITHIN the selected window
    # and sources, so the tallest bar always reads exactly 100. The peak
    # is the biggest month's POSITIVE-kind sum, not its net: stacked
    # bars render positives up and negatives down, so the visible bar
    # top is the positive sum — a net peak would push a mixed-sign
    # month's bar past 100 (and a big-income-but-net-negative window to
    # blank). The peak > 0 guard blanks a window with no positive flow
    # at all — better blank than sign-flipped bars.
    tx_tags = view_tags("web_transactions",
                        {"time_range": ("occurred_at", "date/all-options"),
                         "source": ("silver_source_id", "string/=")})

    def flow_sql(kinds, sign=""):
        ks = ", ".join(f"'{k}'" for k in kinds)
        # Same account-kind fence as the money twins (flow_kinds), in SQL:
        # the explicit IS NULL branch keeps the unknown-account rows that a
        # bare NOT IN would drop.
        aks = ", ".join(f"'{k}'" for k in FLOW_CHART_EXCLUDED_ACCOUNT_KINDS)
        return (
            "WITH m AS (\n"
            "  SELECT CAST(date_trunc('month', occurred_at) AS TIMESTAMP)"
            " AS month,\n"
            f"         kind, {sign}sum(value_usd)::DOUBLE AS v\n"
            "    FROM web_transactions\n"
            f"   WHERE kind IN ({ks})\n"
            f"     AND (account_kind IS NULL OR account_kind NOT IN ({aks}))"
            + _cl(tx_tags, "time_range") + _cl(tx_tags, "source") + "\n"
            "   GROUP BY 1, 2),\n"
            "p AS (SELECT max(t) AS peak FROM"
            " (SELECT sum(v) FILTER (WHERE v > 0) AS t FROM m GROUP BY month))\n"
            "SELECT month, kind,\n"
            "       v / (SELECT CASE WHEN peak > 0 THEN peak END FROM p)"
            " * 100 AS value_pct\n"
            "  FROM m\n ORDER BY 1")

    flow_viz = {"graph.dimensions": ["month", "kind"],
                "graph.metrics": ["value_pct"],
                "stackable.stack_type": "stacked"}
    out["Income by month (privacy)"] = ("question", "bar",
        "Investment income (dividends, interest, distributions, coupons, "
        "staking) per month, stacked by kind, as % of the biggest income "
        "month within the selected window and sources — the tallest bar "
        "reads 100. A kind can dip negative (e.g. margin interest); a "
        "window with no positive income shows blank." + PRIVACY_DESC,
        _native(db_id, flow_sql(INCOME_KINDS), tx_tags), flow_viz)
    out["Fees & taxes by month (privacy)"] = ("question", "bar",
        "Fees and withheld taxes per month (negated so costs read as "
        "positive bars), stacked by kind, as % of the costliest month "
        "within the selected window and sources — the tallest bar reads "
        "100." + PRIVACY_DESC,
        _native(db_id, flow_sql(COST_KINDS, sign="-"), tx_tags), flow_viz)
    for name in ("Income by month (privacy)", "Fees & taxes by month (privacy)"):
        register(name, tx_tags, [(TIME_PARAM_ID, "time_range"),
                                 (SOURCE_PARAM_ID, "source")])

    # -- The Allocation breakdowns: each bucket as % of the summed total
    # over the same filtered rows, so the buckets total 100 across the
    # selected sources as of the chosen day (liability buckets read
    # negative; the sum <> 0 guard blanks a degenerate zero day). Built
    # for the Allocation twin, which supplies the required as-of day;
    # run standalone they aggregate across all days, so filter As Of Day
    # to a single day first.
    day_src = {"as_of_day": ("as_of_day", "date/single"),
               "source": ("silver_source_id", "string/=")}
    standalone = (" Built for the Allocation dashboard, which supplies "
                  "the as-of day; opened standalone, set the As Of Day "
                  "filter to a single day first.")

    def breakdown_sql(view, dim, val, tags):
        return (
            "WITH r AS (\n"
            f"  SELECT {dim}, sum({val})::DOUBLE AS v\n"
            f"    FROM {view}\n"
            "   WHERE TRUE" + _cl(tags, "as_of_day") + _cl(tags, "source") + "\n"
            "   GROUP BY 1)\n"
            f"SELECT {dim},\n"
            "       v / (SELECT CASE WHEN sum(v) <> 0 THEN sum(v) END FROM r)"
            " * 100 AS value_pct\n"
            "  FROM r\n ORDER BY 2 DESC")

    def breakdown(name, view, dim, val, display, desc, viz=None):
        tags = view_tags(view, day_src)
        out[name] = ("question", display, desc + standalone + PRIVACY_DESC,
                     _native(db_id, breakdown_sql(view, dim, val, tags), tags),
                     viz if viz is not None else
                     {"graph.dimensions": [dim], "graph.metrics": ["value_pct"]})
        register(name, tags, [(ASOF_PARAM_ID, "as_of_day"),
                              (SOURCE_PARAM_ID, "source")])

    breakdown("Allocation by asset class (privacy)",
              "web_asset_classes_history", "asset_class", "value_usd", "row",
              "Asset-class shares (%) of the selected sources' net worth "
              "as of a day, including a 'cash' class — always sums to 100; "
              "liability classes (e.g. mortgages) read negative, which is "
              "why this is a bar chart and not a pie.")
    breakdown("Allocation by vehicle (privacy)",
              "web_vehicles_history", "vehicle", "value_usd", "row",
              "Vehicle shares (%) of the selected sources' net worth as of "
              "a day, including a 'demand_deposit' vehicle for cash — "
              "always sums to 100. The wrapper-dimension companion to "
              "Allocation by asset class.")
    breakdown("Allocation by currency (privacy)",
              "web_positions_history", "currency", "value_usd", "row",
              "Native-currency shares (%) of the selected sources' "
              "positions value (cash not included) as of a day — the FX "
              "exposure of the invested part; always sums to 100.")
    breakdown("Value by tax wrapper (privacy)",
              "web_accounts_history", "tax_wrapper", "total_value_usd", "pie",
              "Tax-wrapper shares (%) of the selected sources' total "
              "account value (incl. cash) as of a day; always sums to 100.",
              viz={"pie.dimension": "tax_wrapper", "pie.metric": "value_pct"})
    breakdown("Value by management style (privacy)",
              "web_accounts_history", "management_style", "total_value_usd",
              "row",
              "Management-style shares (%) of the selected sources' total "
              "account value (incl. cash) as of a day; always sums to 100.")

    # -- Top positions: each position's share of the selected sources'
    # TOTAL positions value at the day. The inline asset-class / vehicle
    # pickers narrow the list but not the denominator, so a position's
    # share reads the same however the list is narrowed.
    top_tags = view_tags("web_positions_history",
                         {**day_src,
                          "asset_class": ("asset_class", "string/="),
                          "vehicle": ("vehicle", "string/=")})
    out["Top 100 positions (privacy)"] = ("question", "table",
        "The hundred largest positions as of a day, each as % of the "
        "selected sources' total positions value; the widget's "
        "asset-class and vehicle filters narrow the list but not the "
        "denominator." + standalone + PRIVACY_DESC,
        _native(db_id,
            "WITH tot AS (\n"
            "  SELECT sum(value_usd)::DOUBLE AS t\n"
            "    FROM web_positions_history\n"
            "   WHERE TRUE" + _cl(top_tags, "as_of_day")
            + _cl(top_tags, "source") + "),\n"
            "p AS (\n"
            "  SELECT symbol, name, asset_class, vehicle,"
            " sum(value_usd)::DOUBLE AS v\n"
            "    FROM web_positions_history\n"
            "   WHERE TRUE" + _cl(top_tags, "as_of_day")
            + _cl(top_tags, "source") + _cl(top_tags, "asset_class")
            + _cl(top_tags, "vehicle") + "\n"
            "   GROUP BY 1, 2, 3, 4)\n"
            "SELECT symbol, name, asset_class, vehicle,\n"
            "       v / (SELECT CASE WHEN t <> 0 THEN t END FROM tot)"
            " * 100 AS value_pct\n"
            "  FROM p\n ORDER BY 5 DESC\n LIMIT 100", top_tags),
        {})
    register("Top 100 positions (privacy)", top_tags,
             [(ASOF_PARAM_ID, "as_of_day"), (SOURCE_PARAM_ID, "source"),
              (ASSET_PARAM_ID, "asset_class"), (VEHICLE_PARAM_ID, "vehicle")])

    # -- Data Freshness twin: the freshness table re-run over the _pct
    # sources model. The dashboard is unfiltered by design, so the
    # model's fixed %-of-latest scale IS the selected-sources scale, and
    # the source rows total 100. Own description: the base card's
    # promises a USD value column, which here holds shares.
    pmid = {**model_ids,
            "report_sources_latest": model_ids["report_sources_latest_pct"]}
    display, _desc, query, viz = question_defs(db_id, pmid)["Source freshness"]
    out["Source freshness (privacy)"] = ("question", display,
        "Per source: latest snapshot, its age in days, and the share (%) "
        "of the latest total riding on it." + PRIVACY_DESC, query, viz)

    # -- Returns twin: report_returns_redacted drops the money columns,
    # so the by-source table's privacy variant re-lists its fields
    # without them (the base card's start/end/net-flow refs would error
    # against it) and gets a description that doesn't promise the
    # dropped columns.
    out["Returns by source (privacy)"] = ("question", "table",
        "Returns per source over the chosen window: TWR and MWR, "
        "plain and annualized, with the quality flags that explain "
        "every n/a." + RETURNS_PRIVACY_DESC,
        _mbql(db_id, model_ids["report_returns_redacted"],
              {"filter": ["and",
                   ["=", _f("grain", "type/Text"), "sources"],
                   ["=", _f("granularity", "type/Text"), "total"]],
               "fields": [_f("silver_source_id", "type/Text"),
                          _f("entity_label", "type/Text"),
                          _f("twr", "type/Float"),
                          _f("twr_annualized", "type/Float"),
                          _f("mwr", "type/Float"),
                          _f("mwr_annualized", "type/Float"),
                          _f("quality", "type/Text")],
               "order-by": [["asc", _f("silver_source_id",
                                       "type/Text")]]}),
        _percent_viz("twr", "twr_annualized", "mwr", "mwr_annualized"))
    return out


def dashboard_defs():
    """dashboard name -> (description, filter mode, sibling dashboard
    name, tiles). Every base dashboard gets a privacy twin: same layout
    and filters, cards swapped for their share-normalized '(privacy)'
    variants. The sibling name links the two views — ensure_dashboards
    renders it as a switch link at the top of each dashboard."""
    # The twin blurb names each mode's denominator; absolute amounts
    # never show on any of them.
    pdesc = {
        "returns": (
            "Privacy view: returns are scale-free ratios and show "
            "unchanged; the absolute money columns and the account / "
            "portfolio grains are redacted. "),
        "range": (
            "Privacy view: values are shares (%) of the selected "
            "sources' total at the window's end — the holdings charts "
            "end at 100, the flow charts peak at 100 within the selected "
            "window; absolute amounts never show. "),
        "asof": (
            "Privacy view: every breakdown shows shares (%) of the "
            "selected sources' total as of the chosen day, summing to "
            "100; absolute amounts never show. "),
        None: (
            "Privacy view: values are shares (%) of the latest total "
            "across all sources, not absolute amounts. "),
    }
    out = {}
    for name, (desc, mode, tiles) in base_dashboards().items():
        pname = f"{name} (privacy)"
        out[name] = (desc, mode, pname, tiles)
        ptiles = [(c if c in PRIVACY_EXEMPT_CARDS else privacy_name(c),
                   r, col, sx, sy, t) for c, r, col, sx, sy, t in tiles]
        out[pname] = (pdesc[mode] + desc, mode, name, ptiles)
    return out


def dashboard_parameters(model_ids, mode):
    """The global filters a pre-defined dashboard carries, by mode:
    'range' pairs the source picker with a time range (flows / history
    dashboards), 'asof' pairs it with a single as-of day (point-in-time
    holdings dashboards), 'returns' pairs it with a required currency
    picker (the returns dashboards), None means no filters. The source
    picker draws its dropdown values from the sources model."""
    if mode is None:
        return []
    source = {"id": SOURCE_PARAM_ID, "name": "Source", "slug": "source",
              "type": "string/=", "sectionId": "string", "isMultiSelect": True,
              "values_source_type": "card",
              "values_source_config": {
                  "card_id": model_ids["report_sources_latest"],
                  "value_field": ["field", "silver_source_id",
                                  {"base-type": "type/Text"}]}}
    if mode == "returns":
        # Required + USD default: report_returns carries one row set per
        # currency, so a card must never run with the currency cleared —
        # every period would show all three currency rows (a required
        # parameter resets to its default instead of clearing). The value
        # list is static because the materializer's currency trio is
        # fixed, not data-dependent.
        currency = {"id": CURRENCY_PARAM_ID, "name": "Currency",
                    "slug": "currency", "type": "string/=",
                    "sectionId": "string", "isMultiSelect": False,
                    "default": ["USD"], "required": True,
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
        # Required + dynamic "today" default: the as-of cards sum daily
        # history (one row per entity per day), so they must never run
        # with the day filter cleared — a required parameter resets to
        # its default instead of clearing. The asset-class and vehicle
        # pickers (no default = all values) draw their dropdown values
        # from the positions model and are linked only to
        # POSITION_FILTERED_CARDS.
        def positions_picker(pid, name, slug, field):
            return {"id": pid, "name": name, "slug": slug, "type": "string/=",
                    "sectionId": "string", "isMultiSelect": True,
                    "values_source_type": "card",
                    "values_source_config": {
                        "card_id": model_ids["report_positions_history"],
                        "value_field": ["field", field,
                                        {"base-type": "type/Text"}]}}

        return [{"id": ASOF_PARAM_ID, "name": "As of day", "slug": "as_of_day",
                 "type": "date/single", "sectionId": "date",
                 "default": "thisday", "required": True},
                source,
                positions_picker(ASSET_PARAM_ID, "Asset class", "asset_class", "asset_class"),
                positions_picker(VEHICLE_PARAM_ID, "Vehicle", "vehicle", "vehicle")]
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


def gold_metadata(base, sid, db_id, tries=3, delay=2):
    """The synced gold tables metadata, with a short retry. A transient
    failure here must fail the run loudly rather than resolve zero field
    ids — an empty resolution would silently converge a working install
    down to the degraded no-filters card shape. Returns the tables list,
    or None when the metadata stays unreadable."""
    for i in range(tries):
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


def missing_filter_columns(tables):
    """The FILTER_FIELD_COLUMNS tables not fully synced in `tables` —
    checked per COLUMN, not per table: Metabase's sync can expose a
    table before its fields, and a provision that ran in that gap would
    wire zero filters."""
    ids = field_ids_from(tables)
    return sorted({t for t, cols in FILTER_FIELD_COLUMNS.items()
                   for c in cols if (t, c) not in ids})


def snapshot_has_web_views(base, sid, db_id):
    """Whether the mounted gold snapshot carries the web_* serving views
    (migration 0032), probed through the driver itself. A pre-0032
    snapshot cannot be fixed by a Metabase schema sync — only `wealthdb
    web refresh` re-materializes and re-snapshots gold."""
    want = sum(1 for t in FILTER_FIELD_COLUMNS if t.startswith("web_"))
    st, res = req(base, "/api/dataset", "POST",
                  {"type": "native", "database": db_id,
                   "native": {"query":
                              "SELECT count(*) FROM duckdb_views() "
                              "WHERE NOT internal AND view_name LIKE 'web!_%' "
                              "ESCAPE '!'",
                              "template-tags": {}}}, session=sid)
    rows = (res.get("data") or {}).get("rows") if st == 202 else None
    return bool(rows) and rows[0][0] >= want


def ensure_synced(base, sid, db_id, tries=20, delay=3):
    """Make every column behind FILTER_FIELD_COLUMNS available: when
    some are missing from the synced metadata, either Metabase simply
    has not synced the new gold DDL yet (trigger a sync, wait bounded)
    or the snapshot predates the web_* serving views — which no sync
    can fix. Returns the tables metadata to resolve field ids from, or
    None when provisioning must not proceed (metadata unreadable, or a
    stale snapshot that would break the view-backed models)."""
    tables = gold_metadata(base, sid, db_id)
    if tables is None:
        print("provision: cannot read the gold metadata — aborting before "
              "converging cards to a degraded shape", file=sys.stderr)
        return None
    gone = missing_filter_columns(tables)
    if not gone:
        return tables
    if not snapshot_has_web_views(base, sid, db_id):
        print("provision: the gold snapshot predates the web_* serving "
              "views (migration 0032) — run `wealthdb web refresh` to "
              "re-snapshot; leaving the existing cards untouched",
              file=sys.stderr)
        return None
    print(f"provision: syncing gold schema (missing: {', '.join(gone)})")
    req(base, f"/api/database/{db_id}/sync_schema", "POST", {}, session=sid)
    for _ in range(tries):
        time.sleep(delay)
        tables = gold_metadata(base, sid, db_id)
        if tables is not None and not missing_filter_columns(tables):
            return tables
    print("provision: gold schema sync incomplete — some dashboard filters "
          "will wire up on a later provision", file=sys.stderr)
    return tables or []


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
        if mode == "returns":
            # Both returns twins show the same scale-free ratios; the
            # privacy view redacts money columns instead of normalizing.
            link = (f"🔓 [Switch to the full view — money columns included]"
                    f"(/dashboard/{dash_ids[sibling]})"
                    if name.endswith(" (privacy)") else
                    f"🔒 [Switch to the privacy view — money columns redacted]"
                    f"(/dashboard/{dash_ids[sibling]})")
        else:
            link = (f"🔓 [Switch to absolute values](/dashboard/{dash_ids[sibling]})"
                    if name.endswith(" (privacy)") else
                    f"🔒 [Switch to the privacy view — values as shares (%), "
                    f"not amounts](/dashboard/{dash_ids[sibling]})")

        def tile_mappings(card, tcol):
            if not mode:
                return []
            # Native privacy cards take the pickers as field-filter
            # template tags (registered when their SQL was built; only
            # tags whose field id resolved are present).
            native = PRIVACY_PARAM_TARGETS.get(card)
            if native is not None:
                return [{"parameter_id": pid, "card_id": card_ids[card],
                         "target": ["dimension", ["template-tag", tag]]}
                        for pid, tag in native]
            if mode == "returns":
                # Currency + Start-year land on every returns tile. The
                # native charts take them as template variables ({{currency}}
                # / {{start_year}}); the MBQL scalars + table take them as
                # dimensions (currency, and window_from_year to pick the
                # since-<year> summary). The Source picker lands on the charts
                # (a field-filter variable, when its field id resolved) and
                # the by-source table (silver_source_id dimension) but NOT the
                # global scalars, whose silver_source_id is '' — a source
                # filter would blank them.
                if card in RETURNS_NATIVE_CARDS:
                    # Currency is a field-filter dimension (dropdown) when its
                    # id resolved, else a plain text variable.
                    ccy_target = (["dimension", ["template-tag", "currency"]]
                                  if CURRENCY_FIELD_ID is not None else
                                  ["variable", ["template-tag", "currency"]])
                    maps = [
                        {"parameter_id": CURRENCY_PARAM_ID, "card_id": card_ids[card],
                         "target": ccy_target},
                        {"parameter_id": START_YEAR_PARAM_ID, "card_id": card_ids[card],
                         "target": ["variable", ["template-tag", "start_year"]]},
                    ]
                    if SOURCE_FIELD_ID is not None:
                        maps.append(
                            {"parameter_id": SOURCE_PARAM_ID, "card_id": card_ids[card],
                             "target": ["dimension", ["template-tag", "source_ff"]]})
                    return maps
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
            maps = [{"parameter_id": tparam, "card_id": card_ids[card],
                     "target": ["dimension", _f(tcol, "type/DateTime")]},
                    {"parameter_id": SOURCE_PARAM_ID, "card_id": card_ids[card],
                     "target": ["dimension",
                                _f("silver_source_id", "type/Text")]}]
            if mode == "asof" and card in POSITION_FILTERED_CARDS:
                maps.append({"parameter_id": ASSET_PARAM_ID,
                             "card_id": card_ids[card],
                             "target": ["dimension",
                                        _f("asset_class", "type/Text")]})
                maps.append({"parameter_id": VEHICLE_PARAM_ID,
                             "card_id": card_ids[card],
                             "target": ["dimension",
                                        _f("vehicle", "type/Text")]})
            return maps

        # The switch link occupies row 0, so the tiles shift down one row.
        # The asset-class and vehicle filters render on the Top-positions
        # tile itself (inline_parameters) rather than in the dashboard's
        # filter bar — they only apply to that one widget.
        dashcards = [text_dashcard(-99, link)]
        dashcards += [{"id": -(i + 1), "card_id": card_ids[card], "row": row + 1,
                       "col": col, "size_x": sx, "size_y": sy, "series": [],
                       "visualization_settings": {},
                       "inline_parameters":
                           [ASSET_PARAM_ID, VEHICLE_PARAM_ID] if mode == "asof"
                           and card in POSITION_FILTERED_CARDS else [],
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
