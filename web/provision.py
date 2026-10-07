#!/usr/bin/env python3
"""Provision a fresh Metabase over its loopback REST API: create the
admin account (skipping the "tell us about yourself" setup wizard),
pre-add the gold DuckDB database, and create the pre-defined report
models, questions and dashboards. Idempotent — safe to run on
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
    ap.add_argument("--default-currency", default="USD")
    a = ap.parse_args()
    global DEFAULT_CURRENCY
    DEFAULT_CURRENCY = default_currency(a.default_currency)

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

# The reporting currencies: the value column sets the `_multi` report
# macros and the web_* serving views carry (gold migration 0114), and the
# rows report_returns carries. Every currency list, CASE and picker below
# is generated from this one tuple.
REPORTING_CURRENCIES = ("USD", "CHF", "EUR", "GBP")

# The currency every picker and every native card's {{currency}}
# variable defaults to: wealthdb.cfg's default_currency when it is a
# reporting currency (main() sets it), USD otherwise.
DEFAULT_CURRENCY = "USD"


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
    (`epoch_ms` -> zone-free UTC, not `to_timestamp`, which is
    TIMESTAMPTZ and renders in the reading session's zone) for Metabase
    — except the taxonomy, spending and income models, which wrap the
    web_* serving views (migrations 0032, 0043, 0049 and 0072) that do
    that rendering, the cash fold-in and the
    '(uncategorized)' labelling themselves. The macros already emit DECIMAL
    money/quantity columns and one value column set per reporting currency
    (REPORTING_CURRENCIES), so no value casting is needed here. The `_latest` reports
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
        parts = [f"epoch_ms({c} * 1000) AS {c}" for c in ts_cols]
        excl = f" EXCLUDE ({', '.join(exclude)})" if exclude else ""
        cond = f" WHERE {where}" if where else ""
        return f"SELECT *{excl} REPLACE ({', '.join(parts)}) FROM {from_expr}{cond}"

    # Scalar subquery with the latest global net worth per currency — the
    # shared normalization constant of the privacy (_pct) models. A fixed
    # constant keeps every aggregate's shape; the privacy dashboards'
    # charts recompute a selection-aware denominator per query instead
    # (privacy_card_defs), so this constant only backs standalone model
    # browsing and the drill-through of the twins' MBQL cards. Guarded to NULL
    # when the latest total is zero or negative (empty or under-water
    # gold): dividing would render inf/NaN resp. sign-flipped
    # percentages, where NULL just blanks the values.
    NW_LATEST = ("(SELECT " + ", ".join(
        f"CASE WHEN total_value_{c} > 0 THEN total_value_{c} END AS nw_{c}"
        for c in _ccy_lower()) +
        f" FROM report_global_multi({MAX_BIGINT})) AS nw")
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

    def taxonomy_history(view, pct=False):
        """Breakdown-by-taxonomy models over the gold web_* serving views
        (migration 0032), which already render as_of_day as TIMESTAMP and
        fold each source's cash balance into a class/vehicle of its own so
        the rows sum exactly to net worth. pct=True rescales to % of the
        latest global net worth (per currency)."""
        if not pct:
            return f"SELECT * FROM {view}"
        vals = ", ".join(f"value_{c} / nw.nw_{c} * 100 AS value_{c}"
                         for c in _ccy_lower())
        return (f"SELECT * EXCLUDE ({', '.join(NW_COLS)}) "
                f"REPLACE ({vals}) FROM {view}, {NW_LATEST}")

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
    V1 = [f"value_{c}" for c in _ccy_lower()]
    BASE3 = ("positions_value_base", "cash_balance_base", "total_value_base")

    L = f"({MAX_BIGINT})"   # _multi _latest macro arg (as-of = latest snapshot)
    return {
        "report_global_latest": (
            wrap(f"report_global_multi{L}", ["min_snapshot_at", "max_snapshot_at"]),
            "Whole-portfolio rollup as of the latest snapshot: cash, positions and "
            f"total value in {_ccy_words()} (one column set per currency), with the "
            "min/max snapshot date span. Mirrors `wealthdb holdings global`."),
        "report_sources_latest": (
            wrap(f"report_sources_multi{L}", ["snapshot_at"]),
            "One row per silver source as of the latest snapshot: positions + cash "
            f"totalled in the source's base currency and in {_ccy_slash()}, with rolled-up "
            "tax wrapper / management style. Mirrors `wealthdb holdings sources`."),
        "report_portfolios_latest": (
            wrap(f"report_portfolios_multi{L}", ["snapshot_at"]),
            "One row per portfolio as of the latest snapshot: positions + cash totalled "
            f"in the portfolio's base currency and in {_ccy_slash()}, with rolled-up tax "
            "wrapper / management style. Mirrors `wealthdb holdings portfolios`."),
        "report_accounts_latest": (
            wrap(f"report_accounts_multi{L}", ["snapshot_at"]),
            "One row per account as of the latest snapshot: positions + cash totalled "
            f"in the account's base currency and in {_ccy_slash()}, with kind, tax wrapper "
            "and management style. Mirrors `wealthdb holdings accounts`."),
        "report_positions_latest": (
            wrap(f"report_positions_multi{L}", ["snapshot_at"]),
            "One row per held position as of the latest snapshot, with market value in "
            f"{_ccy_words()}. Mirrors `wealthdb holdings positions`."),
        "report_transactions": (
            wrap(f"report_transactions_multi(0, {MAX_BIGINT})", ["occurred_at"]),
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
        # active snapshot, so several values can share one source-day.
        "report_global_history": (
            wrap("report_global_history_multi()", ["as_of_day"]),
            "Whole-portfolio value for every day from the first snapshot to today "
            f"(carried forward between snapshots), in {_ccy_words()}. The net-worth-"
            "over-time series — chart total_value_usd (or another currency's "
            "column) against as_of_day."),
        "report_sources_history": (
            wrap("report_sources_history_multi()", ["as_of_day"]),
            "Per-silver-source value for every day (carried forward), in the source's "
            f"base currency and in {_ccy_slash()}. Filter to a source and chart against "
            "as_of_day."),
        "report_accounts_history": (
            wrap("report_accounts_history_multi()", ["as_of_day"]),
            "Per-account value for every day (carried forward), in the account's base "
            f"currency and in {_ccy_slash()}. Filter to an account and chart against as_of_day."),
        "report_portfolios_history": (
            wrap("report_portfolios_history_multi()", ["as_of_day"]),
            "Per-portfolio value for every day (carried forward), in the portfolio's base "
            f"currency and in {_ccy_slash()}. Filter to a portfolio and chart against as_of_day."),
        "report_positions_history": (
            wrap("report_positions_history_multi()", ["as_of_day", "snapshot_at"]),
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
        # browsing and the drill-through of the twins' few MBQL cards (the
        # freshness table, the spending scalars); every other privacy
        # card is native SQL over the web_* serving views
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
            f"day, carried forward, in {_ccy_slash()}. Sums to net worth by "
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
            f"for cash) per source per day, carried forward, in {_ccy_slash()}. "
            "Sums to net worth by construction."),
        "report_vehicles_history_pct": (
            taxonomy_history("web_vehicles_history", pct=True),
            "Privacy variant of report_vehicles_history: values as % of the "
            "latest global net worth (per currency)."),
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

# The investment income TYPES the Wealth Overview's income chart shows.
#
# Types, not transaction kinds: the chart reads web_income (migration
# 0072), whose base has already excluded own-account moves and returned
# capital and whose rows carry a RESOLVED income type. That is also why
# the chart needs no card fence — a card's finance charge is an outflow
# and is not in the base at all.
#
# A private fund's `distribution` floors to `capital_return` and is not
# income (docs/INCOME.md), so it is absent here by construction rather
# than by omission.
INVESTMENT_INCOME_TYPES = ["INCOME_DIVIDENDS", "INCOME_INTEREST_EARNED",
                           "INCOME_STAKING", "INCOME_DISTRIBUTIONS"]
WO_INCOME_TYPE_LIST = ", ".join(f"'{t}'" for t in INVESTMENT_INCOME_TYPES)
COST_KINDS = ["fee", "tax"]

# Account kinds fenced out of the FEES & TAXES flow charts. A credit card
# books `fee` (an annual fee) and `interest` (a finance charge) of its
# own — the same transaction kinds those charts select on — so without
# this fence a card would report spending costs as portfolio costs. Card
# flows are spending; they belong to the spending surface.
#
# The income charts no longer need it and no longer use it: they read
# `web_income` (migration 0072), whose base admits a card's finance
# charge nowhere at all — a negative `interest` is spending's by
# migration 0041, and a card fee is not an income kind. The fence
# shrank to the one question it still answers when the income tile was
# re-pointed at the income base.
#
# Fenced on the account_kind column the transaction report macros carry
# (migration 0039), which is NULL for a transaction whose account is
# absent from `accounts`; NULL must be KEPT, so the fence is written as
# "not card, or unknown".
FLOW_CHART_EXCLUDED_ACCOUNT_KINDS = ["card"]

# The label gold's spending views carry for a line whose category the
# enrichment pass could not resolve. Rendered at the view (migration
# 0043) rather than left NULL, so a breakdown shows the backlog as a
# bucket instead of an unlabelled slice; the uncategorized-share card
# counts rows carrying it.
UNCATEGORIZED = "(uncategorized)"

# Card names retired by renames and removals, each group noted where it
# is listed; archived on provision so a re-run cleans them up.
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
                      "Source freshness (% of peak)",
                      # "Income by month (USD)" -> "Investment income by
                      # month (USD)": the Wealth Overview's tile is named
                      # for the four types it charts, which also keeps its
                      # privacy twin from colliding with the Income
                      # dashboard's. The twin name is NOT retired — it is
                      # the Income dashboard's own, and always was the
                      # name the collision resolved to.
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
                      "Top 100 positions (USD)"]

# Dashboard names retired by renames ("Net Worth" undersold the income /
# cost flow tiles); archived on provision so a re-run cleans them up.
RETIRED_DASHBOARD_NAMES = ["Net Worth"]

# The reserved source id declared accounts carry in gold
# (canonical.DeclaredSourceID in the engine).
DECLARED_SOURCE = "declared"

# Every dashboard has a privacy twin whose cards show shares (%) instead
# of money (each names its own denominator — see PRIVACY_DESC). Cards
# listed here show no monetary values (percentages, indices, source
# names), so the twin reuses them as-is. The returns scalars and charts
# are all percentage/index-only; only the by-source table carries money.
PRIVACY_EXEMPT_CARDS = {"Stalest source (days)", "Returns age (days)",
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
                        "Uncategorized share"}

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


# The native returns charts carry three template variables — currency, start
# year, and (a multi-value field filter) source. The Returns dashboard's
# Currency / Start-year / Source pickers map onto them; the same pickers map
# onto the MBQL cards' currency / window_from_year / silver_source_id
# dimensions. Currency and start year default so a card still runs standalone.
START_YEAR_TAG = {"id": "year-tag", "name": "start_year",
                  "display-name": "Start year", "type": "number",
                  "default": "0", "required": True}


def currency_tag():
    """The required {{currency}} text variable: the one a native card
    picks its value column with (_ccy_case), and the returns charts'
    fallback before the currency field has synced. It defaults to
    DEFAULT_CURRENCY, which is the configured one only once main() has
    run, so it is built on call."""
    return {"id": "ccy-tag", "name": "currency", "display-name": "Currency",
            "type": "text", "default": DEFAULT_CURRENCY, "required": True}


# The Cash Flow dashboard's investing grain. Required with a `whole`
# default, so a card opened away from the dashboard draws the section as
# one movement — the reading a household opens the statement with —
# rather than running with the variable cleared.
INVESTING_TAG = {"id": "inv-tag", "name": "investing",
                 "display-name": "Investing", "type": "text",
                 "default": "whole", "required": True}

# The gold columns backing the native cards' field filters: the returns
# charts filter report_returns; every other native card filters the
# web_* serving views (gold migrations 0032, 0043, 0072, 0084 and
# 0113). Field
# ids are per-Metabase-instance (assigned when the DB syncs), so main()
# resolves them at provision time into FIELD_IDS — they can't be
# hard-coded. A missing id (fresh install before the first sync) leaves
# that filter off the affected cards; the next provision — post-sync —
# wires it up.
FILTER_FIELD_COLUMNS = {
    "report_returns": ("currency", "silver_source_id"),
    # The Wealth Overview's headline figures (migration 0113). Their time
    # filter lands on the snapshot, so a source whose latest snapshot
    # falls outside the window drops out of them.
    "web_sources_latest": ("snapshot_at", "silver_source_id"),
    "web_sources_history": ("as_of_day", "silver_source_id"),
    "web_transactions": ("occurred_at", "silver_source_id"),
    "web_asset_classes_history": ("as_of_day", "silver_source_id"),
    "web_vehicles_history": ("as_of_day", "silver_source_id"),
    "web_accounts_history": ("as_of_day", "silver_source_id"),
    "web_positions_history": ("as_of_day", "silver_source_id",
                              "asset_class", "vehicle"),
    # The spending views (migration 0043). On the money dashboard both
    # bind their account picker to account_label rather than
    # account_external_id — see dashboard_parameters for why the readable
    # column wins, and why the privacy twin carries no such picker.
    # spend_primary_label, not spend_primary: a field filter's widget is
    # a dropdown of the values its column takes, and the Category picker
    # is SHARED with the money dashboard, whose model column of the same
    # name now holds the display label. Binding the two to different
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
                   "income_label"),
    "web_card_balances_history": ("as_of_day", "silver_source_id",
                                  "account_label"),
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
                    "widget-type": "string/=", "default": [DEFAULT_CURRENCY],
                    "required": True}
    else:
        currency = currency_tag()
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
        " WHERE " + ccy_outer +
        "year(epoch_ms(end_day * 1000)) >= {{start_year}}")


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
        "       first_value(1 + twr) OVER (PARTITION BY source ORDER BY yr)\n"
        "           / (1 + twr) - 1 AS cumulative_return\n"
        "  FROM f\n"
        " ORDER BY yr")


def _series_viz(time_col, series_col, metric, *, percent=False):
    """Viz for a native time series split by a category: x = time_col,
    one line per series_col, y = metric. Native queries need the axes named
    explicitly (there is no MBQL breakout for Metabase to infer them from)."""
    viz = {"graph.dimensions": [time_col, series_col], "graph.metrics": [metric]}
    if percent:
        viz["column_settings"] = {f'["name","{metric}"]': {"number_style": "percent"}}
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
            "pie.slice_threshold": threshold}


def allocation_note():
    """The sentence an Allocation card's description ends with, on the
    base dashboard and the twin alike."""
    return (" Built for the Allocation dashboard, which supplies the as-of "
            f"day; opened standalone it runs in {DEFAULT_CURRENCY}, the "
            "currency variable's default, and sums every day, so set the As "
            "Of Day filter to a single day first.")


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
        Currency lands as a dimension when it is a field filter and as a
        variable otherwise, Start year as a variable, Source only when
        its field filter exists."""
        tags = returns_tags()
        register_native_targets(name, tags, [(CURRENCY_PARAM_ID, "currency"),
                                             (START_YEAR_PARAM_ID, "start_year"),
                                             (SOURCE_PARAM_ID, "source_ff")])
        return ("line", desc, _native(db_id, sql, tags), viz)

    def part(grain, granularity):
        """Filter to one (grain, granularity) partition of report_returns
        (the returns scalars/table use the summary 'total' partition; the
        Start-year picker then selects the window_from_year within it)."""
        return ["and", ["=", _f("grain", "type/Text"), grain],
                ["=", _f("granularity", "type/Text"), granularity]]

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
    bal_tags = spend_tags("web_card_balances_history", CARD_BALANCE_FILTERS)
    register_native_targets("Card balances over time", bal_tags,
                            CARD_BALANCE_PICKERS)

    # The spending tiles read the serving view natively, the way the
    # privacy twin's already do. The reason is the Currency picker: the
    # long-format model carries a row per (line, reporting currency), so
    # an MBQL tile over it is right only while a row filter holds it to
    # one — which the dashboard supplies and nothing else does. Opened on
    # its own, such a tile summed every currency, silently and
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

    sp_month = ("CAST(date_trunc('month', occurred_at) AS TIMESTAMP)"
                " AS month")

    # The income tiles are the same kit over the other serving view.
    # `neg` is the whole of the sign difference: gold stores an outflow
    # negative and a receipt positive, and both families report a
    # positive magnitude.
    in_where, in_val, income_native = _family_native_kit(
        db_id, "web_income", INCOME_FILTERS, INCOME_PICKERS, False, income_native_note)

    # The cashflow tiles are the same kit over the third serving view,
    # plus one template variable of its own: {{investing}}, the display
    # grain of the investing section. It is a VARIABLE and not a field
    # filter because it changes how rows are GROUPED rather than which
    # rows are selected — `whole` nets the section into one Investments
    # node, `class` opens it into one node per asset class — and a field
    # filter can only narrow a population.
    cf_tags = {**spend_tags("web_cashflow", CASHFLOW_FILTERS),
               "investing": INVESTING_TAG}
    # THE SECTION PICKER REACHES ONLY THE CARDS IT MEANS SOMETHING ON.
    # A card that does not declare the {{section}} tag cannot be bound
    # to the picker, so the scope is expressed once, here, rather than
    # as a rule a later card has to remember. The headline figures and
    # the diagram decline it: a statement whose sections have been
    # narrowed to one is not a statement, its savings rate and yield
    # share have lost a leg of their own ratio, and a one-section
    # diagram draws a Cash node that absorbs the whole section rather
    # than the residual it names (docs/CASHFLOW.md §9).
    cf_tags_nosec = {k: v for k, v in cf_tags.items() if k != "section"}
    cf_where = _spend_where(cf_tags)
    cf_where_nosec = _spend_where(cf_tags_nosec)
    cf_pickers_nosec = [t for t in CASHFLOW_PICKERS if t[1] != "section"]
    cf_val = f"sum({_ccy_case('value')})::DOUBLE"
    cf_note = (" Built for the Cash Flow dashboard; opened standalone it "
               f"runs in {DEFAULT_CURRENCY} with investing netted as a whole, the two "
               "variables' defaults.")

    def cashflow_native(name, display, desc, sql, viz, section=True):
        """A native Cash Flow card. `section` says whether the card
        takes the dashboard's Section picker; a card that declines it
        must have been built with `cf_where_nosec`, since a tag a card
        does not declare cannot appear in its SQL."""
        tags = cf_tags if section else cf_tags_nosec
        register_native_targets(
            name, tags, CASHFLOW_PICKERS if section else cf_pickers_nosec)
        return (display, desc + cf_note, _native(db_id, sql, tags), viz)

    # The uncategorised-share tile's tags: every filter but the
    # currency variable, which a share of rows has no use for.
    in_share_tags = view_tags("web_income", INCOME_FILTERS)
    register_native_targets("Uncategorized income share", in_share_tags,
                            [t for t in INCOME_PICKERS if t[1] != "currency"])

    # The Wealth Overview and Allocation tiles read the serving views
    # natively, for the reason the spending tiles do: the Currency picker
    # has to choose a value COLUMN, which only a template variable can.
    # Each card reads the view at the grain it charts. The Overview's
    # income tile reads the Income dashboard's view but answers only its
    # own dashboard's three pickers.
    wo_note = (" Built for the Wealth Overview; opened standalone it runs "
               f"in {DEFAULT_CURRENCY}, the currency variable's default.")
    al_note = allocation_note()
    lat_tags = spend_tags("web_sources_latest", range_filters("snapshot_at"))
    hist_tags = spend_tags("web_sources_history", range_filters("as_of_day"))
    wo_income_tags = spend_tags("web_income", range_filters("occurred_at"))
    tx_tags = spend_tags("web_transactions", range_filters("occurred_at"))
    pos_tags = spend_tags("web_positions_history", POSITION_FILTERS)

    def overview_native(name, tags, display, desc, sql, viz):
        register_native_targets(name, tags, OVERVIEW_PICKERS)
        return (display, desc + wo_note, _native(db_id, sql, tags), viz)

    def latest_scalar(name, col, desc):
        """A headline figure: one value column summed over the selected
        sources' latest snapshots."""
        return overview_native(name, lat_tags, "scalar", desc,
            f"SELECT sum({_ccy_case(col)})::DOUBLE AS {col}\n"
            "  FROM web_sources_latest" + _spend_where(lat_tags), {})

    def breakdown(name, view, dim, col, display, desc, viz=None):
        """An Allocation breakdown: `col` summed by `dim` over the as-of
        day's rows of serving view `view`, largest first."""
        tags = spend_tags(view, ASOF_FILTERS)
        register_native_targets(name, tags, ALLOCATION_PICKERS)
        return (display, desc + al_note,
                _native(db_id,
                    f"SELECT {dim}, sum({_ccy_case(col)})::DOUBLE AS value\n"
                    f"  FROM {view}" + _spend_where(tags) +
                    "\n GROUP BY 1\n ORDER BY 2 DESC", tags),
                viz if viz is not None else
                {"graph.dimensions": [dim], "graph.metrics": ["value"]})

    register_native_targets("Top 100 positions", pos_tags, POSITION_PICKERS)

    days_stale = ["datetime-diff", _f("snapshot_at", "type/DateTime"),
                  ["now"], "day"]
    # The declared accounts (config `declared_accounts`) sit under their
    # own reserved source, which has no feed and so no snapshot: its
    # latest snapshot reads as the epoch, and counted here it would be
    # the stalest source every day. Freshness is about feeds, so it is
    # left out.
    fed_sources = ["!=", _f("silver_source_id", "type/Text"), DECLARED_SOURCE]
    twr = _f("twr", "type/Float")
    return {
        # Net worth = positions + cash by construction, so the three
        # headline figures reconcile exactly. They read each source's
        # latest snapshot, like `wealthdb holdings sources`; the charts
        # below read the carried-forward history.
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
            "  FROM web_sources_history" + _spend_where(hist_tags) +
            "\n GROUP BY 1\n ORDER BY 1",
            {}),
        "Net worth over time": overview_native(
            "Net worth over time", hist_tags, "area",
            "Net worth for every day since the first snapshot (value "
            "carried forward between snapshots), stacked by source; the "
            "envelope is total net worth.",
            "SELECT as_of_day, silver_source_id,\n"
            f"       sum({_ccy_case('total_value')})::DOUBLE AS net_worth\n"
            "  FROM web_sources_history" + _spend_where(hist_tags) +
            "\n GROUP BY 1, 2\n ORDER BY 1",
            {**_series_viz("as_of_day", "silver_source_id", "net_worth"),
             "stackable.stack_type": "stacked"}),
        "Cash vs positions over time": overview_native(
            "Cash vs positions over time", hist_tags, "area",
            "Daily cash balance and positions value, stacked; the envelope "
            "is total net worth.",
            "SELECT as_of_day,\n"
            f"       sum({_ccy_case('cash_balance')})::DOUBLE AS cash_balance,\n"
            f"       sum({_ccy_case('positions_value')})::DOUBLE AS positions_value\n"
            "  FROM web_sources_history" + _spend_where(hist_tags) +
            "\n GROUP BY 1\n ORDER BY 1",
            {"graph.dimensions": ["as_of_day"],
             "graph.metrics": ["cash_balance", "positions_value"],
             "stackable.stack_type": "stacked"}),
        # Reads the income base (migration 0072), so this tile and the
        # Income dashboard agree to the cent.
        #
        # "Investment income", not "Income": it says what the card
        # charts — the four INVESTMENT_INCOME_TYPES, not the whole base —
        # and it keeps the name, and so the twin's, distinct from the
        # Income dashboard's own "Income by month".
        "Investment income by month": overview_native(
            "Investment income by month", wo_income_tags, "bar",
            "Investment income — dividends, interest earned, staking and "
            "fund distributions — per month, stacked by type. Reads the "
            "same income base as the Income dashboard, so the two agree; "
            "own-account moves and returned capital are already out of it.",
            f"SELECT {sp_month},\n"
            "       income_label AS type,\n"
            f"       sum({_ccy_case('value')})::DOUBLE AS value\n"
            "  FROM web_income" + _spend_where(wo_income_tags) + "\n"
            "   AND income_detailed IN (" + WO_INCOME_TYPE_LIST + ")\n"
            " GROUP BY 1, 2\n ORDER BY 1",
            {"graph.dimensions": ["month", "type"],
             "graph.metrics": ["value"],
             "stackable.stack_type": "stacked"}),
        "Fees & taxes by month": overview_native(
            "Fees & taxes by month", tx_tags, "bar",
            "Fees and withheld taxes per month, stacked by kind; debits are "
            "negated so costs read as positive bars. Credit-card accounts "
            "are excluded — card fees are spending costs, not portfolio "
            "costs.",
            f"SELECT {sp_month},\n       kind,\n"
            f"       sum({_ccy_case('value', neg=True)})::DOUBLE AS cost\n"
            "  FROM web_transactions" + _spend_where(tx_tags) +
            _flow_fence(COST_KINDS) + "\n GROUP BY 1, 2\n ORDER BY 1",
            {"graph.dimensions": ["month", "kind"],
             "graph.metrics": ["cost"],
             "stackable.stack_type": "stacked"}),
        # The Allocation tiles read the daily-history views so the
        # dashboard can show holdings as of any chosen day (history@today
        # equals the latest snapshot by construction). The dashboard
        # supplies the required as-of day.
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
        # A fixed hundred: the inline asset-class and vehicle filters
        # narrow the list rather than a K picker sizing it.
        "Top 100 positions": ("table",
            "The hundred largest positions by market value as of a day, "
            "aggregated across accounts; narrow with the widget's "
            "asset-class and vehicle filters." + al_note,
            _native(db_id,
                "SELECT symbol, name, asset_class, vehicle,\n"
                f"       sum({_ccy_case('value')})::DOUBLE AS value\n"
                "  FROM web_positions_history" + _spend_where(pos_tags) +
                "\n GROUP BY 1, 2, 3, 4\n ORDER BY 5 DESC\n LIMIT 100",
                pos_tags),
            {}),
        # The spending cards run over the long-format report_spending
        # model, whose `currency` dimension the dashboard's required
        # Currency picker selects. The one exception is the card-balances
        # chart: card balances are a different grain (per account per
        # day, carried forward) with no model of their own, so it reads
        # web_card_balances_history natively and takes the pickers as
        # template tags (registered above).
        "Spend — monthly trend": spend_native("Spend — monthly trend",
            "smartscalar",
            "Net spend in the window's latest month, with the change vs the "
            "month before. Net spend is purchases minus refunds.",
            f"SELECT {sp_month},\n       {sp_val} AS net_spend\n"
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
            f"SELECT {sp_month},\n       spend_primary_label AS category,\n"
            f"       {sp_val} AS net_spend\n"
            "  FROM web_spending" + sp_where + "\n GROUP BY 1, 2\n ORDER BY 1",
            {"graph.dimensions": ["month", "category"],
             "stackable.stack_type": "stacked"}),
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
        # (migration 0052). A blank merchant was the proxy for "not a
        # delta" (migration 0048) and no longer is, so the ranking
        # excludes the delta CATEGORIES outright — a delta is
        # primary-level, so `spend_primary <> spend_detailed` reads the
        # dimension's own marker rather than restating a list of values
        # — and still needs a name to rank by. That predicate is what
        # carries this card through migration 0054: a line the merchant
        # store never named now shows its own signature, and a line
        # nothing resolved is kept out by its two category columns
        # being equal at '(uncategorized)' rather than by a blank. What
        # may rank does not move; what ranks widens from the lines the
        # store named to every non-delta line carrying a signature, so
        # ranks move with it, and the rows are at the signature's grain
        # — a chain appears once per branch signature, and a fold that
        # is only the bank's own booking tag ranks under that tag,
        # since the gates that keep such a fold from the model fence
        # the merchant store and not this column. Only the ranking
        # filters: the transaction list below keeps every line, since a
        # line is a line.
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
            _native(db_id,
                "SELECT as_of_day, account_label,\n"
                f"       sum({_ccy_case('balance', neg=True)}) AS owed\n"
                "  FROM web_card_balances_history"
                + _spend_where(bal_tags, " ") + "\n"
                " GROUP BY 1, 2\n ORDER BY 1", bal_tags),
            _series_viz("as_of_day", "account_label", "owed")),
        # The one tile that lists LINES rather than grouping them, which
        # is why it reads the view natively like the charts do: the long
        # model would hand it each line once per reporting currency, and
        # no aggregation to fold them back.
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
            f"SELECT {sp_month},\n       {in_val} AS net_income\n"
            "  FROM web_income" + in_where + "\n GROUP BY 1\n ORDER BY 1",
            {}),
        "Net income": income_native("Net income", "scalar",
            "Total net income over the selected window: receipts minus "
            "reversals, across the selected accounts and types. Gross as "
            "booked — tax withheld at source is on the Spending side.",
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
                    IN_UNCATEGORIZED_SQL + _spend_where(in_share_tags),
                    in_share_tags),
            _percent_viz("uncategorized_share")),
        "Income by month": income_native("Income by month", "area",
            "Net income per month, stacked by type — the shape of the "
            "window. A type can dip negative in a month whose reversals "
            "beat its receipts.",
            f"SELECT {sp_month},\n       income_label AS type,\n"
            f"       {in_val} AS net_income\n"
            "  FROM web_income" + in_where + "\n GROUP BY 1, 2\n ORDER BY 1",
            {"graph.dimensions": ["month", "type"],
             "graph.metrics": ["net_income"],
             "stackable.stack_type": "stacked"}),
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
        "Income by account": income_native("Income by account", "row",
            "Net income by account over the window — which account the money "
            "arrived in. Each bar is labelled with the account's name, its "
            "source and its kind, since a name on its own says neither.",
            f"SELECT account_label AS account,\n       {in_val} AS net_income\n"
            "  FROM web_income" + in_where + "\n GROUP BY 1\n ORDER BY 2 DESC",
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
            # The view's values are already signed — an outflow is
            # negative — so operating is the plain SUM over the two
            # halves, not a difference between two magnitudes.
            f"SELECT {cf_val}\n"
            f"       / nullif(sum(CASE WHEN section = 'operating_in'"
            f" THEN {_ccy_case('value')} END), 0) AS savings_rate\n"
            "  FROM web_cashflow" + cf_where_nosec +
            "\n   AND section IN ('operating_in', 'operating_out')",
            _percent_viz("savings_rate"), section=False),
        "Yield share": cashflow_native("Yield share", "scalar",
            "What share of the household's receipts its assets produced "
            "without its labour — dividends, interest, fund distributions, "
            "staking, rent and royalties over everything that came in.",
            f"SELECT sum(CASE WHEN class = 'yield' THEN {_ccy_case('value')} END)::DOUBLE\n"
            f"       / nullif({cf_val}, 0) AS yield_share\n"
            "  FROM web_cashflow" + cf_where_nosec +
            "\n   AND section = 'operating_in'",
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
            f"SELECT {sp_month},\n"
            "       CASE WHEN section IN ('operating_in', 'operating_out')\n"
            "            THEN 'operating' ELSE section END AS statement_section,\n"
            f"       {cf_val} AS value\n"
            "  FROM web_cashflow" + cf_where + "\n GROUP BY 1, 2\n ORDER BY 1",
            {"graph.dimensions": ["month", "statement_section"],
             "graph.metrics": ["value"],
             "stackable.stack_type": "stacked"}),
        "Inflows by class by month": cashflow_native(
            "Inflows by class by month", "area",
            "What came in each month, stacked by class: earnings, yield, "
            "pensions and benefits, other receipts, and the backlog no "
            "tier could place.",
            f"SELECT {sp_month},\n       class_node AS class,\n"
            f"       {cf_val} AS value\n"
            "  FROM web_cashflow" + cf_where +
            "\n   AND section = 'operating_in'\n GROUP BY 1, 2\n ORDER BY 1",
            {"graph.dimensions": ["month", "class"],
             "graph.metrics": ["value"],
             "stackable.stack_type": "stacked"}),
        "Outflows by class by month": cashflow_native(
            "Outflows by class by month", "area",
            "What went out each month, stacked by class: consumption, "
            "fees, taxes, giving, and the backlog. Fees, taxes and giving "
            "are lifted out of consumption because each is worth a line "
            "of its own.",
            f"SELECT {sp_month},\n       class_node AS class,\n"
            f"       -({cf_val}) AS value\n"
            "  FROM web_cashflow" + cf_where +
            "\n   AND section = 'operating_out'\n GROUP BY 1, 2\n ORDER BY 1",
            {"graph.dimensions": ["month", "class"],
             "graph.metrics": ["value"],
             "stackable.stack_type": "stacked"}),
        "Investing by month": cashflow_native("Investing by month", "bar",
            "Investing per month, signed: above the axis the portfolio fed "
            "the household, below it the household fed the portfolio. One "
            "series or one per asset class, per the Investing picker — and "
            "the difference between the two is the reallocation the whole "
            "nets away.",
            f"SELECT {sp_month},\n"
            "       CASE WHEN {{investing}} = 'whole' THEN 'Investments'\n"
            "            ELSE class_node END AS class,\n"
            f"       {cf_val} AS value\n"
            "  FROM web_cashflow" + cf_where +
            "\n   AND section = 'investing'\n GROUP BY 1, 2\n ORDER BY 1",
            {"graph.dimensions": ["month", "class"],
             "graph.metrics": ["value"],
             "stackable.stack_type": "stacked"}),
        "Financing and vehicles by month": cashflow_native(
            "Financing and vehicles by month", "bar",
            "Debt serviced or drawn, and the earmarked pools funded or "
            "drawn on, per month and signed. A mortgage being repaid while "
            "a loan is drawn are two bars, and so are a retirement plan "
            "being funded while a trust pays out.",
            f"SELECT {sp_month},\n       class_node AS class,\n"
            f"       {cf_val} AS value\n"
            "  FROM web_cashflow" + cf_where +
            "\n   AND section IN ('financing', 'vehicles')\n GROUP BY 1, 2\n ORDER BY 1",
            {"graph.dimensions": ["month", "class"],
             "graph.metrics": ["value"],
             "stackable.stack_type": "stacked"}),
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
        # Currency / Start-year / Source pickers map onto their {{currency}}
        # / {{start_year}} / {{source_ff}} tags.
        "Cumulative return (TWR)": returns_native("Cumulative return (TWR)",
            "Time-weighted return compounded from the chosen start year, per "
            "source and the '(all sources)' portfolio line — derived from the "
            "engine's since-<year> returns (so it matches the scalars and the "
            "by-source table exactly). Annual granularity. Built for the "
            "Returns dashboard.",
            returns_growth_sql(),
            _series_viz("year", "source", "cumulative_return", percent=True)),
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
                   "filter": fed_sources,
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
            f"riding on it ({DEFAULT_CURRENCY}).",
            _mbql(db_id, mid["report_sources_latest"],
                  {"expressions": {"days_stale": days_stale},
                   "filter": fed_sources,
                   "fields": [_f("silver_source_id", "type/Text"),
                              _f("snapshot_at", "type/DateTime"),
                              ["expression", "days_stale"],
                              _dec(f"total_value_{DEFAULT_CURRENCY.lower()}")],
                   "order-by": [["desc", ["expression", "days_stale"]]]}),
            {}),
    }


# Dashboard filters: a required currency picker (default: the configured
# default currency) and a silver-source picker (default: all values) plus
# either a time range over flows/history (default: past 12 months) or a
# single as-of day over point-in-time holdings (default: today), each
# linked to every tile — plus widget-scoped asset-class and vehicle
# pickers that render inline on the Top-positions tile only. The Returns
# dashboards carry a start-year picker instead of a time filter — the
# periods are precomputed buckets; the Spending dashboards carry an
# account and a category picker on top of the time-range pair. The parameter ids are
# arbitrary but must be stable across runs so re-provisioning converges
# instead of accumulating parameters, and they must be distinct — a
# reused id would make two pickers one.
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

# The dashboards carrying the spending pickers (the base view and its
# privacy twin). Named rather than modelled as a filter mode of their
# own: the mode is 'range' like Wealth Overview — a time window plus a
# source picker — and these three pickers are additions to it.
SPENDING_DASHBOARDS = {"Spending", "Spending" + PRIVACY_SUFFIX}
# The same for the Income dashboards.
INCOME_DASHBOARDS = {"Income", "Income" + PRIVACY_SUFFIX}

# The same for the Cash Flow dashboards.
CASHFLOW_DASHBOARDS = {"Cash Flow", "Cash Flow" + PRIVACY_SUFFIX}

# The asset-class and vehicle filters (the two taxonomy dimensions) are
# linked only to these tiles: the Top-positions widgets, which list
# individual holdings. The breakdown widgets each already group by one of
# the dimensions, so filtering them by it would mostly self-select.
POSITION_FILTERED_CARDS = {"Top 100 positions", "Top 100 positions (privacy)"}

# Spending cards that carry every reporting currency as its own column,
# and so must NOT be wired to the Currency picker: the long model has a
# row per (line, currency), so a row filter on `currency` would empty the
# two columns the picker does not select. Such a card is also the only
# kind that reads correctly opened standalone, where no picker reaches it.
SPEND_ALL_CURRENCY_CARDS = {"Top 50 merchants"}



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
    the tile's time column below for an MBQL one. The source filter
    lands on silver_source_id — in 'returns' mode on every tile but the
    whole-portfolio scalars (RETURNS_GLOBAL_SCALARS), whose
    silver_source_id is ''. Data Freshness is deliberately unfiltered —
    its job is to show every source, especially the stale ones a time
    filter would hide."""
    note = ("Pre-defined by wealthdb and converged to spec on every `web "
            "start` — duplicate into another collection before customizing.")
    return {
        "Wealth Overview": (
            "The whole picture over time, in a chosen currency (default "
            f"{DEFAULT_CURRENCY}): net worth, cash vs positions, and income and cost "
            "flows. " + note, "range", [
            # Net worth = positions + cash by construction — the first
            # three tiles reconcile exactly; the trend tile is a monthly
            # AVERAGE, so it intentionally differs from today's value.
            ("Net worth", 0, 0, 6, 3, "snapshot_at"),
            ("Positions value", 0, 6, 6, 3, "snapshot_at"),
            ("Cash balance", 0, 12, 6, 3, "snapshot_at"),
            ("Net worth — monthly trend", 0, 18, 6, 3, "as_of_day"),
            ("Net worth over time", 3, 0, 24, 6, "as_of_day"),
            ("Cash vs positions over time", 9, 0, 24, 6, "as_of_day"),
            ("Investment income by month", 15, 0, 12, 6, "occurred_at"),
            ("Fees & taxes by month", 15, 12, 12, 6, "occurred_at"),
        ]),
        "Allocation": (
            "Where the value sits — asset class, currency, tax wrapper, "
            "management style and the largest positions — as of a chosen "
            f"day (default: today), in a chosen currency (default {DEFAULT_CURRENCY}). "
            + note, "asof", [
            # The two taxonomy dimensions side by side on the top row.
            ("Allocation by asset class", 0, 0, 12, 8, "as_of_day"),
            ("Allocation by vehicle", 0, 12, 12, 8, "as_of_day"),
            ("Allocation by currency", 8, 0, 12, 8, "as_of_day"),
            ("Value by tax wrapper", 8, 12, 12, 8, "as_of_day"),
            ("Value by management style", 16, 0, 24, 6, "as_of_day"),
            ("Top 100 positions", 22, 0, 24, 8, "as_of_day"),
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
            "What the tracked accounts received: wages, interest, "
            "dividends, staking rewards, rent, gifts — typed and "
            "attributed to a payer. Gross as booked; tax withheld at "
            "source is on the Spending dashboard. " + note,
            "range", [
            # The same reading order as Spending: three headline
            # figures, then the shape of the window, then the breakdown,
            # then who it came from and where it landed, then the lines.
            # There is no second ring (one vendored primary, so no
            # subcategory level) and no balance history (nothing on this
            # side is a liability).
            ("Income — monthly trend", 0, 0, 8, 3, "occurred_at"),
            ("Net income", 0, 8, 8, 3, "occurred_at"),
            ("Uncategorized income share", 0, 16, 8, 3, "occurred_at"),
            ("Income by month", 3, 0, 24, 9, "occurred_at"),
            ("Income by type", 12, 0, 12, 8, "occurred_at"),
            ("Top 50 payers", 12, 12, 12, 8, "occurred_at"),
            ("Income by account", 20, 0, 24, 8, "occurred_at"),
            ("Largest receipts", 28, 0, 24, 8, "occurred_at"),
        ]),
        "Cash Flow": (
            "Where the household's cash came from and where it went: the "
            "cash flow statement and the Sankey that draws it. The "
            "household is the accounts in its own tax wrappers; "
            "retirement plans, trusts and charitable vehicles are "
            "vehicles it pays into and draws on. Own-account moves are "
            "invisible, and buying and selling is shown NET. " + note,
            "range", [
            # Five headline figures, then the diagram at the centre, then
            # the shape of the window, then the two sides of operating,
            # then the swing sections, then the lines.
            ("Operating in", 0, 0, 5, 3, "occurred_at"),
            ("Operating out", 0, 5, 5, 3, "occurred_at"),
            ("Net cash flow", 0, 10, 5, 3, "occurred_at"),
            ("Savings rate", 0, 15, 4, 3, "occurred_at"),
            ("Yield share", 0, 19, 5, 3, "occurred_at"),
            # Full width and tall: the diagram is the dashboard.
            ("Cash flow", 3, 0, 24, 12, "occurred_at"),
            ("Cash flow statement by month", 15, 0, 24, 8, "occurred_at"),
            ("Inflows by class by month", 23, 0, 12, 7, "occurred_at"),
            ("Outflows by class by month", 23, 12, 12, 7, "occurred_at"),
            ("Investing by month", 30, 0, 12, 7, "occurred_at"),
            ("Financing and vehicles by month", 30, 12, 12, 7, "occurred_at"),
            ("Largest flows", 37, 0, 24, 8, "occurred_at"),
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
    """Field-filter template tags for a native card over serving view
    `table`. spec maps tag name -> (column, widget-type); a tag
    whose field id has not synced yet is omitted — the SQL builders then
    drop the matching [[AND {{tag}}]] clause (referencing an undefined
    tag would invalidate the query) and register_native_targets skips its
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
# line list. `cashflow_native` (question_defs) and `cashflow_card`
# (cashflow_privacy_defs) draw that line, by withholding the tag from
# the cards that decline it.
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

SPEND_PICKERS = [(SPEND_CURRENCY_PARAM_ID, "currency"),
                 (TIME_PARAM_ID, "time_range"), (SOURCE_PARAM_ID, "source"),
                 (ACCOUNT_PARAM_ID, "account"), (CATEGORY_PARAM_ID, "category")]
CARD_BALANCE_PICKERS = [t for t in SPEND_PICKERS if t[1] != "category"]

# The Wealth Overview's and Allocation's filters (the Overview's pair is
# range_filters). Neither dashboard names an account anywhere, so the
# base and the twin share them. Every tile reads a serving view natively
# and takes the Currency picker as the {{currency}} variable: the views
# carry the reporting currencies as columns, and a picker selects rows but
# never a column.
ASOF_FILTERS = {"as_of_day": ("as_of_day", "date/single"),
                "source": ("silver_source_id", "string/=")}
POSITION_FILTERS = {**ASOF_FILTERS,
                    "asset_class": ("asset_class", "string/="),
                    "vehicle": ("vehicle", "string/=")}
OVERVIEW_PICKERS = [(WEALTH_CURRENCY_PARAM_ID, "currency"),
                    (TIME_PARAM_ID, "time_range"), (SOURCE_PARAM_ID, "source")]
ALLOCATION_PICKERS = [(WEALTH_CURRENCY_PARAM_ID, "currency"),
                      (ASOF_PARAM_ID, "as_of_day"), (SOURCE_PARAM_ID, "source")]
POSITION_PICKERS = ALLOCATION_PICKERS + [(ASSET_PARAM_ID, "asset_class"),
                                         (VEHICLE_PARAM_ID, "vehicle")]


def _flow_fence(kinds):
    """The predicate of the fees-and-taxes charts: the transaction kinds
    charted, fenced to non-card accounts (FLOW_CHART_EXCLUDED_ACCOUNT_KINDS).
    The explicit IS NULL branch keeps the rows whose account is absent
    from `accounts`, which a bare NOT IN would drop."""
    ks = ", ".join(f"'{k}'" for k in kinds)
    aks = ", ".join(f"'{k}'" for k in FLOW_CHART_EXCLUDED_ACCOUNT_KINDS)
    return (f"\n     AND kind IN ({ks})"
            f"\n     AND (account_kind IS NULL OR account_kind NOT IN ({aks}))")


def spend_tags(table, spec):
    """Template tags for a native spending card over serving view
    `table`: the required {{currency}} text variable, plus a field filter
    per column in `spec` whose field id has synced."""
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
        # being named. The list had also excluded financing, which HAS
        # something finer to say now that a mortgage instalment splits
        # into interest and amortisation, and no amount of splitting it
        # would have drawn.
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
        # no leaves to enter through. Spelled as a section list this
        # stopped agreeing with the leaf rule the moment financing grew
        # leaves: the class then entered BOTH here and as the sum of its
        # leaves below, and the diagram drew the same edge twice.
        "atoms AS (SELECT c.class AS node, c.net FROM cls c\n"
        "           WHERE NOT EXISTS (SELECT 1 FROM att a\n"
        "                              WHERE a.section = c.section\n"
        "                                AND a.class = c.class)\n"
        "          UNION ALL SELECT grp, CASE WHEN stays THEN 0 ELSE net END FROM att\n"
        "          UNION ALL SELECT class, sum(CASE WHEN stays THEN net ELSE 0 END)\n"
        "                      FROM att GROUP BY 1),\n"
        "hub AS (SELECT nullif(sum(CASE WHEN net > 0 THEN net ELSE 0 END), 0)"
        " AS total FROM atoms)")


def _cashflow_sankey_sql(where, sum_expr, share=False):
    """The window's diagram as an edge list, computed where the pickers
    apply.

    A node's SIDE is the sign of its net over the FILTERED window, so a
    pre-netted table would be netted over the wrong window the moment a
    reader moved a picker. The stages, the hub and the swing rule are
    the `report_cashflow_sankey` macro's, in the three columns the BI
    layer's Sankey visualisation reads.

    A class gets a leaf stage when it has something finer to say than
    its own name — the operating sections, and financing since a
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
        # sort by value as before.
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


def _family_native_kit(db_id, view, filters, pickers, neg, note):
    """One family's native-card kit: the WHERE clause its serving
    view's template tags imply, the summed value expression in the
    picked currency, and a builder that registers a card's pickers as it
    defines it. The tags stay inside — every card that needs them goes
    through the builder.

    Shared because the two families differ only in what this takes —
    the view, the filter specs, the picker list, the SIGN and the note
    a card's description ends with — and a second copy of this would be
    those few constants and a drift risk. `neg` flips
    the sum so an outflow, which gold stores negative, reads as a
    positive magnitude; income is already positive and passes False.
    """
    tags = spend_tags(view, filters)
    where = _spend_where(tags)
    val = f"sum({_ccy_case('value', neg=neg)})::DOUBLE"

    def native(name, display, desc, sql, viz):
        register_native_targets(name, tags, pickers)
        return (display, desc + note, _native(db_id, sql, tags), viz)

    return where, val, native


def _spend_where(tags, indent="   "):
    """`WHERE TRUE` plus one optional [[AND {{tag}}]] clause per FIELD
    FILTER on the card.

    A plain variable — {{currency}}, {{investing}} — picks a column or a
    grain rather than filtering rows, and an [[AND {{x}}]] clause around
    one is not valid SQL. The test is the tag's own type, so a variable
    added to a family later cannot silently produce a broken predicate.
    A filter whose field id has not synced is absent from `tags` and left
    out entirely: referencing an undefined tag would invalidate the
    query."""
    return f"\n{indent}WHERE TRUE" + "".join(
        _cl(tags, n) for n in tags if tags[n].get("type") == "dimension")


# Dashboard picker -> template tag wiring for every native card, rebuilt
# whenever the card definitions are built: every entry is keyed by card
# name and every pass rewrites all of them.
# ensure_dashboards reads it to map those cards' pickers; cards absent
# here take the default MBQL dimension mappings.
NATIVE_PARAM_TARGETS = {}


def register_native_targets(card, tags, pairs):
    """Record card `card`'s picker -> template-tag mapping. `pairs` is
    (parameter id, tag name); a tag whose field id has not synced yet is
    absent from `tags`, and its picker stays unmapped until a later
    provision. A field-filter tag maps as a `dimension` target; a plain
    template variable ({{currency}}, {{start_year}}, {{investing}})
    maps as a `variable` one."""
    NATIVE_PARAM_TARGETS[card] = [
        (pid, ["dimension" if tags[t].get("type") == "dimension" else "variable",
               ["template-tag", t]])
        for pid, t in pairs if t in tags]


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
    render a merchant or account label — see spending_privacy_defs."""
    out = {}

    # -- Wealth Overview scalars: a ratio of sums is scale-free, and both
    # legs see the dashboard's filters, so each reads as a share of the
    # selected sources' latest total. The > 0 guard blanks an empty or
    # under-water selection rather than rendering inf or a sign-flipped
    # share.
    lat_tags = spend_tags("web_sources_latest", range_filters("snapshot_at"))

    def share(name, num_col, desc):
        out[name] = ("question", "scalar", desc + PRIVACY_DESC,
            _native(db_id,
                f"SELECT sum({_ccy_case(num_col)})::DOUBLE\n"
                f"       / CASE WHEN sum({_ccy_case('total_value')}) > 0\n"
                f"              THEN sum({_ccy_case('total_value')})::DOUBLE END"
                " * 100 AS share_pct\n"
                "  FROM web_sources_latest" + _spend_where(lat_tags), lat_tags),
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
    # higher. The > 0 guard blanks a selection whose anchor total is
    # zero or negative — dividing would render inf/NaN resp.
    # sign-flipped bands.
    sh_tags = spend_tags("web_sources_history", range_filters("as_of_day"))
    total = _ccy_case("total_value")
    nw_cte = (
        "WITH nw AS (\n"
        f"  SELECT CASE WHEN sum({total}) > 0\n"
        f"              THEN sum({total})::DOUBLE END AS denom\n"
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
        _native(db_id, nw_cte +
            "SELECT as_of_day, silver_source_id,\n"
            f"       sum({total})::DOUBLE / (SELECT denom FROM nw)"
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
            f"       sum({_ccy_case('cash_balance')})::DOUBLE"
            " / (SELECT denom FROM nw) * 100 AS cash_pct,\n"
            f"       sum({_ccy_case('positions_value')})::DOUBLE"
            " / (SELECT denom FROM nw) * 100 AS positions_pct\n"
            "  FROM web_sources_history" + sh_where + "\n"
            " GROUP BY 1\n ORDER BY 1", sh_tags),
        {"graph.dimensions": ["as_of_day"],
         "graph.metrics": ["cash_pct", "positions_pct"],
         "stackable.stack_type": "stacked"})
    for name in ("Net worth — monthly trend (privacy)",
                 "Net worth over time (privacy)",
                 "Cash vs positions over time (privacy)"):
        register_native_targets(name, sh_tags, OVERVIEW_PICKERS)

    # -- The flow charts: % of the peak month WITHIN the selected window
    # and sources, so the tallest bar always reads exactly 100. The peak
    # is the biggest month's POSITIVE-kind sum, not its net: stacked
    # bars render positives up and negatives down, so the visible bar
    # top is the positive sum — a net peak would push a mixed-sign
    # month's bar past 100 (and a big-income-but-net-negative window to
    # blank). The peak > 0 guard blanks a window with no positive flow
    # at all — better blank than sign-flipped bars.
    tx_tags = spend_tags("web_transactions", range_filters("occurred_at"))
    wo_tags = spend_tags("web_income", range_filters("occurred_at"))

    def peak_share(m_cte):
        """The month x kind rows of CTE body `m_cte`, each as % of the
        window's peak month."""
        return ("WITH m AS (\n" + m_cte + "\n   GROUP BY 1, 2),\n"
                "p AS (SELECT max(t) AS peak FROM"
                " (SELECT sum(v) FILTER (WHERE v > 0) AS t FROM m GROUP BY month))\n"
                "SELECT month, kind,\n"
                "       v / (SELECT CASE WHEN peak > 0 THEN peak END FROM p)"
                " * 100 AS value_pct\n"
                "  FROM m\n ORDER BY 1")

    month = "  SELECT CAST(date_trunc('month', occurred_at) AS TIMESTAMP) AS month,\n"
    flow_viz = {"graph.dimensions": ["month", "kind"],
                "graph.metrics": ["value_pct"],
                "stackable.stack_type": "stacked"}
    # The twin of the Wealth Overview's income tile, over the same income
    # base and the same four types.
    out["Investment income by month (privacy)"] = ("question", "bar",
        "Investment income — dividends, interest earned, staking and fund "
        "distributions — per month, stacked by type, as % of the biggest "
        "income month within the selected window and sources: the tallest "
        "bar reads 100. A type can dip negative in a month whose reversals "
        "beat its receipts; a window with no positive income shows blank."
        + PRIVACY_DESC,
        _native(db_id, peak_share(
            month + "         income_label AS kind,\n"
            f"         sum({_ccy_case('value')})::DOUBLE AS v\n"
            "    FROM web_income" + _spend_where(wo_tags) + "\n"
            "     AND income_detailed IN (" + WO_INCOME_TYPE_LIST + ")"),
            wo_tags), flow_viz)
    # Debits negated so costs read as positive bars, fenced the way the
    # base tile is (_flow_fence).
    out["Fees & taxes by month (privacy)"] = ("question", "bar",
        "Fees and withheld taxes per month (negated so costs read as "
        "positive bars), stacked by kind, as % of the costliest month "
        "within the selected window and sources — the tallest bar reads "
        "100." + PRIVACY_DESC,
        _native(db_id, peak_share(
            month + "         kind,\n"
            f"         sum({_ccy_case('value', neg=True)})::DOUBLE AS v\n"
            "    FROM web_transactions" + _spend_where(tx_tags)
            + _flow_fence(COST_KINDS)),
            tx_tags), flow_viz)
    register_native_targets("Investment income by month (privacy)", wo_tags,
                            OVERVIEW_PICKERS)
    register_native_targets("Fees & taxes by month (privacy)", tx_tags,
                            OVERVIEW_PICKERS)

    # -- The Allocation breakdowns: each bucket as % of the summed total
    # over the same filtered rows, so the buckets total 100 across the
    # selected sources as of the chosen day (liability buckets read
    # negative; the sum <> 0 guard blanks a degenerate zero day). Built
    # for the Allocation twin, which supplies the required as-of day;
    # run standalone they aggregate across all days, so filter As Of Day
    # to a single day first.
    standalone = allocation_note()

    def breakdown_sql(view, dim, col, tags):
        return (
            "WITH r AS (\n"
            f"  SELECT {dim}, sum({_ccy_case(col)})::DOUBLE AS v\n"
            f"    FROM {view}\n"
            "   WHERE TRUE" + _cl(tags, "as_of_day") + _cl(tags, "source") + "\n"
            "   GROUP BY 1)\n"
            f"SELECT {dim},\n"
            "       v / (SELECT CASE WHEN sum(v) <> 0 THEN sum(v) END FROM r)"
            " * 100 AS value_pct\n"
            "  FROM r\n ORDER BY 2 DESC")

    def breakdown(name, view, dim, col, display, desc, viz=None):
        tags = spend_tags(view, ASOF_FILTERS)
        out[name] = ("question", display, desc + standalone + PRIVACY_DESC,
                     _native(db_id, breakdown_sql(view, dim, col, tags), tags),
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

    # -- Top positions: each position's share of the selected sources'
    # TOTAL positions value at the day. The inline asset-class / vehicle
    # pickers narrow the list but not the denominator, so a position's
    # share reads the same however the list is narrowed.
    top_tags = spend_tags("web_positions_history", POSITION_FILTERS)
    value = _ccy_case("value")
    out["Top 100 positions (privacy)"] = ("question", "table",
        "The hundred largest positions as of a day, each as % of the "
        "selected sources' total positions value; the widget's "
        "asset-class and vehicle filters narrow the list but not the "
        "denominator." + standalone + PRIVACY_DESC,
        _native(db_id,
            "WITH tot AS (\n"
            f"  SELECT sum({value})::DOUBLE AS t\n"
            "    FROM web_positions_history\n"
            "   WHERE TRUE" + _cl(top_tags, "as_of_day")
            + _cl(top_tags, "source") + "),\n"
            "p AS (\n"
            "  SELECT symbol, name, asset_class, vehicle,"
            f" sum({value})::DOUBLE AS v\n"
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
    register_native_targets("Top 100 positions (privacy)", top_tags,
                            POSITION_PICKERS)

    # -- Data Freshness twin: the freshness table re-run over the _pct
    # sources model. The dashboard is unfiltered by design, so the
    # model's fixed %-of-latest scale IS the selected-sources scale, and
    # the source rows total 100. Own description: the base card's
    # promises a money column, which here holds shares.
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

    out.update(spending_privacy_defs(db_id, model_ids))
    out.update(income_privacy_defs(db_id))
    out.update(cashflow_privacy_defs(db_id))
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
    tags = {**spend_tags("web_cashflow", PRIVACY_CASHFLOW_FILTERS),
            "investing": INVESTING_TAG}
    # The Section picker's scope is the base dashboard's, card for card:
    # a twin tile stands in for a base tile and has to answer the same
    # pickers, or a reader comparing the two would find one narrowed and
    # the other not.
    tags_nosec = {k: v for k, v in tags.items() if k != "section"}
    where = _spend_where(tags)
    where_nosec = _spend_where(tags_nosec)
    pickers_nosec = [t for t in CASHFLOW_PICKERS if t[1] != "section"]
    val = f"sum({_ccy_case('value')})::DOUBLE"
    out = {}

    def cashflow_card(name, display, desc, sql, viz, section=True):
        card_tags = tags if section else tags_nosec
        out[name] = ("question", display, desc + PRIVACY_DESC,
                     _native(db_id, sql, card_tags), viz)
        register_native_targets(
            name, card_tags, CASHFLOW_PICKERS if section else pickers_nosec)

    # The hub, built by the same helper the diagram uses, so a scalar
    # and the diagram beside it cannot divide by two different numbers.
    hub_cte = _cashflow_atoms_cte(where, val) + "\n"
    # A tile that declines the Section picker must divide by a hub that
    # declines it too, or the numerator and the denominator would answer
    # different filters.
    hub_cte_nosec = _cashflow_atoms_cte(where_nosec, val) + "\n"
    peak_cte = ("p AS (SELECT max(t) AS peak FROM"
                " (SELECT sum(abs(v)) AS t FROM m GROUP BY month))\n")

    def month_cte(extra="", filt="", sign=""):
        # `sign` is built in rather than applied to the finished SQL: a
        # post-hoc string replacement would depend on this function's own
        # indentation, and a reflow here would silently turn a card's
        # bars negative with every shape test still green.
        v = f"{sign}({val})" if sign else val
        return ("WITH m AS (\n"
                "  SELECT CAST(date_trunc('month', occurred_at) AS TIMESTAMP)"
                f" AS month,\n         {extra}{v} AS v\n"
                "    FROM web_cashflow" + where + filt + "\n")

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
        month_cte("CASE WHEN section IN ('operating_in', 'operating_out')\n"
                  "              THEN 'operating' ELSE section END"
                  " AS statement_section,\n         ")
        + "   GROUP BY 1, 2),\n" + peak_cte +
        "SELECT month, statement_section,\n"
        "       v / (SELECT CASE WHEN peak > 0 THEN peak END FROM p) * 100 AS share_pct\n"
        "  FROM m\n ORDER BY 1",
        {"graph.dimensions": ["month", "statement_section"],
         "graph.metrics": ["share_pct"],
         "stackable.stack_type": "stacked"})

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
        cashflow_card(privacy_name(name), "area" if "class by month" in name else "bar",
            f"{blurb}, each as % of the window's biggest month by gross "
            "movement.",
            month_cte(f"{grp} AS class,\n         ", filt, sign="-" if neg else "")
            + "   GROUP BY 1, 2),\n" + peak_cte +
            "SELECT month, class,\n"
            "       v / (SELECT CASE WHEN peak > 0 THEN peak END FROM p) * 100 AS share_pct\n"
            "  FROM m\n ORDER BY 1",
            {"graph.dimensions": ["month", "class"],
             "graph.metrics": ["share_pct"],
             "stackable.stack_type": "stacked"})

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

    for name in ("Savings rate", "Yield share"):
        # A rate is a proportion already: the twin shows it as it is,
        # the way the returns percentages survive their own twin.
        base = name.lower()
        cashflow_card(privacy_name(name), "scalar",
            f"The {base} as it is: a rate is a proportion, so it needs no "
            "normalising and hides nothing.",
            "SELECT " + (
                f"{val}\n"
                f"       / nullif(sum(CASE WHEN section = 'operating_in'"
                f" THEN {_ccy_case('value')} END), 0) AS share_pct\n"
                "  FROM web_cashflow" + where_nosec +
                "\n   AND section IN ('operating_in', 'operating_out')"
                if name == "Savings rate" else
                f"sum(CASE WHEN class = 'yield' THEN {_ccy_case('value')} END)::DOUBLE\n"
                f"       / nullif({val}, 0) AS share_pct\n"
                "  FROM web_cashflow" + where_nosec +
                "\n   AND section = 'operating_in'"),
            _percent_viz("share_pct"), section=False)
    return out


def income_privacy_defs(db_id):
    """The Income twin's cards: the same tiles, every figure a share,
    and no card rendering a payer or an account.

    The denominators mirror the spending twin's, and for the same
    reason: the breakdowns and the lists divide by the window's own net
    income (they sum to 100), the trend and the monthly bands by the
    window's peak month (the peak reads 100). A window with no positive
    month, or a total of zero, blanks rather than rendering inf or a
    sign-flipped share.

    REDACTION on top of normalization, as everywhere else in the twin:
    the fix for a leaking column is to drop it. No card here projects
    `payer_name`, `display_name` or `account_external_id` — the payer
    list keeps its shape by RANKING instead, so `payer_name` is a GROUP
    BY key and never a projection. Nor does any card filter on an
    account, a field filter being a dropdown of its column's values
    (PRIVACY_INCOME_FILTERS)."""
    tags = spend_tags("web_income", PRIVACY_INCOME_FILTERS)
    where = _spend_where(tags)
    val = f"sum({_ccy_case('value')})::DOUBLE"
    out = {}

    def income_card(name, display, desc, sql, viz):
        out[name] = ("question", display, desc + PRIVACY_DESC,
                     _native(db_id, sql, tags), viz)
        register_native_targets(name, tags, INCOME_PICKERS)

    peak_cte = ("p AS (SELECT max(t) AS peak FROM"
                " (SELECT sum(v) FILTER (WHERE v > 0) AS t FROM m GROUP BY month))\n")
    peak_div = "v / (SELECT CASE WHEN peak > 0 THEN peak END FROM p) * 100"
    total_cte = ("t AS (SELECT nullif(sum(v), 0) AS total FROM m)\n")

    def month_cte(extra=""):
        return ("WITH m AS (\n"
                "  SELECT CAST(date_trunc('month', occurred_at) AS TIMESTAMP)"
                f" AS month,\n         {extra}{val} AS v\n"
                "    FROM web_income" + where + "\n")

    income_card("Income — monthly trend (privacy)", "smartscalar",
        "Net income per month as % of the window's biggest income month — "
        "the latest month with the change vs the one before it. A window "
        "with no positive month shows blank.",
        month_cte() + "   GROUP BY 1),\n" + peak_cte +
        f"SELECT month, {peak_div} AS income_pct\n  FROM m\n ORDER BY 1", {})
    income_card("Income by month (privacy)", "area",
        "Net income per month, stacked by type, as % of the window's "
        "biggest income month — the peak reads 100. A type can dip "
        "negative in a month whose reversals beat its receipts.",
        month_cte("income_label AS type,\n         ")
        + "   GROUP BY 1, 2),\n" + peak_cte +
        f"SELECT month, type, {peak_div} AS income_pct\n  FROM m\n ORDER BY 1",
        {"graph.dimensions": ["month", "type"],
         "graph.metrics": ["income_pct"],
         "stackable.stack_type": "stacked"}),
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
    income_card("Income by account (privacy)", "row",
        "Share (%) of the window's net income by account, ranked and "
        "unnamed.",
        "WITH m AS (\n"
        f"  SELECT account_label, {val} AS v\n"
        "    FROM web_income" + where + "\n   GROUP BY 1),\n" + total_cte +
        "SELECT row_number() OVER (ORDER BY v DESC) AS rank,\n"
        "       v / (SELECT total FROM t) * 100 AS income_pct\n"
        "  FROM m\n ORDER BY 2 DESC",
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
    # view_tags rather than the spend_tags the rest share.
    share_tags = view_tags("web_income", PRIVACY_INCOME_FILTERS)
    out["Uncategorized income share (privacy)"] = ("question", "scalar",
        IN_UNCATEGORIZED_DESC + PRIVACY_DESC,
        _native(db_id, IN_UNCATEGORIZED_SQL + _spend_where(share_tags),
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
    tags = spend_tags("web_spending", PRIVACY_SPEND_FILTERS)
    where = _spend_where(tags)
    val = f"sum({_ccy_case('value', neg=True)})::DOUBLE"
    out = {}

    def spend_card(name, display, desc, sql, viz):
        out[name] = ("question", display, desc + PRIVACY_DESC,
                     _native(db_id, sql, tags), viz)
        register_native_targets(name, tags, SPEND_PICKERS)

    # The peak-month denominator, in the shape the income / fee twins
    # already use: the biggest month's POSITIVE sum, so a mixed-sign
    # month's stacked bar cannot exceed 100, and blank when no month is
    # positive at all.
    peak_cte = ("p AS (SELECT max(t) AS peak FROM"
                " (SELECT sum(v) FILTER (WHERE v > 0) AS t FROM m GROUP BY month))\n")
    peak_div = "v / (SELECT CASE WHEN peak > 0 THEN peak END FROM p) * 100"

    def month_cte(extra=""):
        """The `m` CTE: one row per month (times `extra`'s dimension)."""
        return ("WITH m AS (\n"
                "  SELECT CAST(date_trunc('month', occurred_at) AS TIMESTAMP)"
                f" AS month,\n         {extra}{val} AS v\n"
                "    FROM web_spending" + where + "\n")

    spend_card("Spend — monthly trend (privacy)", "smartscalar",
        "Net spend per month as % of the window's biggest spending month — "
        "the latest month with the change vs the one before it. A window "
        "with no positive month shows blank.",
        month_cte() + "   GROUP BY 1),\n" + peak_cte +
        f"SELECT month, {peak_div} AS spend_pct\n  FROM m\n ORDER BY 1", {})
    spend_card("Spending by month (privacy)", "area",
        "Net spend per month, stacked by primary category, as % of the "
        "window's biggest spending month — the peak reads 100. A "
        "category can dip negative (a month whose refunds beat its "
        "purchases).",
        month_cte("spend_primary_label AS category,\n         ")
        + "   GROUP BY 1, 2),\n" + peak_cte +
        f"SELECT month, category, {peak_div} AS spend_pct\n"
        "  FROM m\n ORDER BY 1",
        {"graph.dimensions": ["month", "category"],
         "graph.metrics": ["spend_pct"], "stackable.stack_type": "stacked"})

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
    # percentages the ring would compute. The LIMIT the detailed card
    # used to carry is gone with it: a limited ring renormalizes onto
    # what it drew and would print shares disagreeing with its own
    # values, where the threshold folds the tail into a wedge and keeps
    # the whole window on the ring.
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
    # Migration 0054 gives a line the merchant store never named its own
    # signature, so the ranking covers every non-delta line carrying a
    # signature rather than only the lines the store named: more rows,
    # at the signature's grain, and the distribution this card is read
    # for — rank 1's share included — moves with them. The card itself
    # needs no change: it never printed a name, and the delta predicate
    # already decides what may rank.
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
    bal_tags = spend_tags("web_card_balances_history",
                          PRIVACY_CARD_BALANCE_FILTERS)
    bal_name = "Card balances over time (privacy)"
    out[bal_name] = ("question", "line",
        "What the cards owed for every day of the window as % of the "
        "window's deepest total owed: the peak reads 100, and a "
        "paid-off card returns to 0. Split by source; account labels are "
        "redacted." + PRIVACY_DESC,
        _native(db_id,
            "WITH d AS (\n"
            "  SELECT as_of_day, silver_source_id,\n"
            f"         sum({_ccy_case('balance', neg=True)})::DOUBLE AS v\n"
            "    FROM web_card_balances_history" + _spend_where(bal_tags) + "\n"
            "   GROUP BY 1, 2),\n"
            "p AS (SELECT CASE WHEN max(abs(t)) > 0 THEN max(abs(t)) END AS peak\n"
            "        FROM (SELECT sum(v) AS t FROM d GROUP BY as_of_day))\n"
            "SELECT as_of_day, silver_source_id,\n"
            "       v / (SELECT peak FROM p) * 100 AS balance_pct\n"
            "  FROM d\n ORDER BY 1", bal_tags),
        _series_viz("as_of_day", "silver_source_id", "balance_pct"))
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
    # Spending and Income are 'range' dashboards whose denominators are
    # their own window rather than a holdings total, and the two twins
    # that also redact, so each names both in a blurb of its own.
    own_pdesc = {"Spending": (
        "Privacy view: values are shares (%) of the window's own net "
        "spend, or of its biggest month; merchant and account labels are "
        "redacted and absolute amounts never show. "),
        "Income": (
        "Privacy view: values are shares (%) of the window's own net "
        "income, or of its biggest month; payer and account labels are "
        "redacted and absolute amounts never show. "),
        "Cash Flow": (
        "Privacy view: values are shares (%) of the window's own hub — "
        "the total the diagram flows through, which moves with the "
        "Investing picker — or of its biggest month. The diagram's nodes "
        "are vocabulary and never a merchant, a payer, an account or an "
        "instrument, so the twin needs normalisation rather than "
        "redaction; the one card that named an account drops the column. ")}
    out = {}
    for name, (desc, mode, tiles) in base_dashboards().items():
        pname = f"{name}{PRIVACY_SUFFIX}"
        out[name] = (desc, mode, pname, tiles)
        ptiles = [(c if c in PRIVACY_EXEMPT_CARDS else privacy_name(c),
                   r, col, sx, sy, t) for c, r, col, sx, sy, t in tiles]
        out[pname] = (own_pdesc.get(name, pdesc[mode]) + desc, mode, name, ptiles)
    return out


def dashboard_parameters(model_ids, mode, name=""):
    """The global filters a pre-defined dashboard carries, by mode:
    'range' pairs the source picker with a time range (flows / history
    dashboards), 'asof' pairs it with a single as-of day (point-in-time
    holdings dashboards), 'returns' pairs it with a start-year picker
    (the returns dashboards), None means no filters. Every mode but None
    adds a required currency picker. The source picker draws its
    dropdown values from the sources model. The Spending, Income and
    Cash Flow dashboards are 'range' plus pickers of their own, so they
    are matched by NAME rather than by mode — Cash Flow first, then
    Income, then Spending, with the Wealth Overview as the plain 'range'
    case. Spending and Income each carry one more picker than their
    twin, which has no account picker; Cash Flow's twin carries the same
    set, having no account picker to drop."""
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

    def currency_picker(pid):
        """The required reporting-currency picker every
        filtered dashboard but Returns carries.

        Required with a default, because a card running with the
        currency cleared would be wrong: a tile over a model with one
        row per (line, reporting currency) would sum every currency,
        and a native tile's {{currency}} variable needs a value. A
        required parameter resets to its default rather than clearing.
        The default is the configured one (DEFAULT_CURRENCY). The list
        is static because the reporting currencies are the product's,
        not the data's, and a card-backed list would re-scan the whole
        population for a few known strings. `values_query_type` is what
        makes Metabase render a dropdown instead of a free-text box."""
        return {"id": pid, "name": "Currency", "slug": "currency",
                "type": "string/=", "sectionId": "string",
                "isMultiSelect": False, "default": [DEFAULT_CURRENCY],
                "required": True,
                "values_query_type": "list",
                "values_source_type": "static-list",
                "values_source_config": {"values": list(REPORTING_CURRENCIES)}}

    source = card_picker(SOURCE_PARAM_ID, "Source", "source",
                         "report_sources_latest", "silver_source_id")
    if mode == "returns":
        # Required, defaulting to DEFAULT_CURRENCY: report_returns carries
        # one row set per currency, so a card must never run with the
        # currency cleared — every period would show a row per currency (a
        # required parameter resets to its default instead of clearing). The values
        # come off the materialized table's own currency column, like the
        # start-year list below.
        currency = {"id": CURRENCY_PARAM_ID, "name": "Currency",
                    "slug": "currency", "type": "string/=",
                    "sectionId": "string", "isMultiSelect": False,
                    "default": [DEFAULT_CURRENCY], "required": True,
                    "values_query_type": "list",
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
        # pickers draw their values from the positions model and are
        # linked only to POSITION_FILTERED_CARDS.
        return [currency_picker(WEALTH_CURRENCY_PARAM_ID),
                {"id": ASOF_PARAM_ID, "name": "As of day", "slug": "as_of_day",
                 "type": "date/single", "sectionId": "date",
                 "default": "thisday", "required": True},
                source,
                card_picker(ASSET_PARAM_ID, "Asset class", "asset_class",
                            "report_positions_history", "asset_class"),
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
    if name in CASHFLOW_DASHBOARDS:
        # The Investing grain: net the section as one movement — the
        # question a reader opens with, "did the portfolio feed the
        # household this year or the household feed the portfolio" — or
        # per asset class, which is the step in. Required with a `whole`
        # default so a card never runs with it cleared, and a static list
        # because the two grains are the feature's, not the data's.
        investing = {"id": CASHFLOW_INVESTING_PARAM_ID, "name": "Investing",
                     "slug": "investing", "type": "string/=",
                     "sectionId": "string", "isMultiSelect": False,
                     "default": ["whole"], "required": True,
                     "values_query_type": "list",
                     "values_source_type": "static-list",
                     "values_source_config": {"values": ["whole", "class"]}}
        # BOTH dashboards carry the same five: unlike the spending and
        # income pairs, the twin drops nothing, because none of these
        # pickers renders a dropdown of anything that identifies an
        # account. There is no account picker to drop — docs/CASHFLOW.md
        # §9 — and a section picker offers the feature's own vocabulary,
        # the sections the serving view carries. The Section picker is
        # declared here for both dashboards but BINDS only to the cards
        # that declare its tag; the headline figures and the diagram
        # decline it, identically on the base and the twin.
        return [currency_picker(CASHFLOW_CURRENCY_PARAM_ID), investing,
                time_range, source,
                card_picker(CASHFLOW_SECTION_PARAM_ID, "Section", "section",
                            "report_cashflow", "section")]
    if name in INCOME_DASHBOARDS:
        # The income pickers, in the shape the spending ones take and
        # for the same reasons — including no account picker on the
        # twin, a picker being unable to redact what its own dropdown
        # offers.
        pickers = [currency_picker(INCOME_CURRENCY_PARAM_ID), time_range, source]
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
        return pickers
    if name not in SPENDING_DASHBOARDS:
        return [currency_picker(WEALTH_CURRENCY_PARAM_ID), time_range, source]
    currency = currency_picker(SPEND_CURRENCY_PARAM_ID)
    # The account picker targets `display_name` — the model's name for
    # the account LABEL (migration 0063) — and NOT
    # `account_external_id`: a picker lists the raw values of the column
    # it is bound to and there is no field-remapping machinery here, so
    # binding it to the id would offer a list of opaque identifiers. The
    # label carries the source and the kind, so two accounts wearing the
    # same generic product name at two institutions are no longer one
    # indistinguishable pair of entries; two named alike WITHIN one
    # source still select together, accepted on the same reasoning, and
    # the money tiles group by the same column.
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


def missing_web_views(base, sid, db_id):
    """The wanted web_* serving views the mounted gold snapshot does not
    carry, probed through the driver itself. A snapshot predating one of
    them (0032 brought the first six, 0043 the two spending views) cannot
    be fixed by a Metabase schema sync — only `wealthdb web refresh`
    re-materializes and re-snapshots gold.

    Probed BY NAME rather than by counting web_* views: a count says
    nothing about WHICH views are there, so a snapshot missing one would
    pass the moment gold grew any other web_* view.
    A probe that fails to answer reports everything missing, which halts
    provisioning — the conservative reading, since the alternative is
    converging every card to a degraded shape."""
    want = web_views_wanted()
    names = ", ".join(f"'{t}'" for t in want)
    st, res = req(base, "/api/dataset", "POST",
                  {"type": "native", "database": db_id,
                   "native": {"query":
                              "SELECT view_name FROM duckdb_views() "
                              f"WHERE NOT internal AND view_name IN ({names})",
                              "template-tags": {}}}, session=sid)
    rows = (res.get("data") or {}).get("rows") if st == 202 else None
    if rows is None:
        return want
    have = {r[0] for r in rows if r}
    return [t for t in want if t not in have]


# The oldest gold schema the cards can read: the migration that gave the
# serving views and `_multi` models their GBP columns (0114). A snapshot
# taken before it carries every view by name, so the view probe below
# passes it; only the version tells it apart.
MIN_GOLD_SCHEMA = 114


def gold_schema_version(base, sid, db_id):
    """The mounted gold snapshot's schema version, probed through the
    driver, or None when the probe does not answer."""
    st, res = req(base, "/api/dataset", "POST",
                  {"type": "native", "database": db_id,
                   "native": {"query": "SELECT max(gold_schema_version) FROM schema_meta",
                              "template-tags": {}}}, session=sid)
    rows = (res.get("data") or {}).get("rows") if st == 202 else None
    try:
        return int(rows[0][0])
    except (TypeError, IndexError, ValueError):
        return None


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
    built from.

    A snapshot older than MIN_GOLD_SCHEMA is refused first: its views
    exist by name but lack columns the cards read, so every card would
    converge onto SQL that fails."""
    version = gold_schema_version(base, sid, db_id)
    if version is None or version < MIN_GOLD_SCHEMA:
        print(f"provision: the gold snapshot is at schema {version or 'unknown'} "
              f"and the cards need {MIN_GOLD_SCHEMA} — run `wealthdb web "
              "refresh` to re-snapshot a migrated gold; leaving the existing "
              "cards untouched", file=sys.stderr)
        return None
    tables = gold_metadata(base, sid, db_id)
    if tables is None:
        print("provision: cannot read the gold metadata — aborting before "
              "converging cards to a degraded shape", file=sys.stderr)
        return None
    gone = missing_filter_columns(tables)
    if not gone:
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
    # `_spend_where` the `[[AND {{tag}}]]` clause that reads it — and
    # `upsert_card` would then PUT that filter-less definition over a card
    # that already works. That is the same degradation the two guards above
    # abort to prevent, arrived at slowly; a first install reaches it too,
    # creating every card filter-less with nothing to say so.
    print("provision: gold schema sync did not complete in time (still "
          f"missing: {', '.join(missing_filter_columns(tables))}) — leaving "
          "the existing cards untouched; re-run `wealthdb web start` once "
          "Metabase has finished syncing", file=sys.stderr)
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
    sized against the busiest page rather than against one query. The
    busiest is Cash Flow, whose tiles each scan the statement over all
    history: measured at eight threads, a 2 GB pool serves six such
    tiles, 3 GB twelve, and 4 GB twenty. Four, because a dashboard that
    gains a tile should not take the page down; a pool too small
    surfaces as "There was a problem displaying this chart" on most of
    the page rather than as anything naming memory.

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
    print(f"provision: report models ({_ccy_slash()}) — {created} created, "
          f"{updated} updated, {archived} retired (collection '{COLLECTION_NAME}')")
    return ids


def ensure_cards(base, sid, db_id, coll_id, by_name, model_ids):
    """Create/refresh the pre-defined questions over the models in
    `model_ids`, and archive any retired (renamed-away) ones. Returns
    card name -> id (the dashboards' tile lookup), or None on failure."""
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
    ids, created, updated = saved
    archived = archive_all(base, sid, "card",
                           [by_name[n]["id"] for n in RETIRED_CARD_NAMES
                            if n in by_name])
    print(f"provision: questions — {created} created, "
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
        parameters = dashboard_parameters(model_ids, mode, name)
        param_ids = {x["id"] for x in parameters}
        tparam = ASOF_PARAM_ID if mode == "asof" else TIME_PARAM_ID
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
        link = f"{icon} [{label}](/dashboard/{dash_ids[sibling]})"

        def tile_mappings(card, tcol, param_ids=param_ids):
            """The pickers this dashboard's tiles answer to, restricted to
            the pickers the dashboard actually carries: the Spending twin
            has no Account picker (dashboard_parameters drops it), and a
            mapping naming a parameter that is not on the dashboard is a
            target with no filter behind it."""
            return [m for m in _tile_mappings(card, tcol)
                    if m["parameter_id"] in param_ids]

        def _tile_mappings(card, tcol, mode=mode, tparam=tparam, name=name):
            if not mode:
                return []
            # Native cards take the pickers as template tags, with the
            # target shape recorded when their SQL was built (a field
            # filter is a dimension, a plain variable is not; only tags
            # whose field id resolved are present).
            native = NATIVE_PARAM_TARGETS.get(card)
            if native is not None:
                return [{"parameter_id": pid, "card_id": card_ids[card],
                         "target": target} for pid, target in native]
            if mode == "returns":
                # The MBQL returns scalars + table (the native charts took
                # the path above) take Currency + Start-year as dimensions:
                # currency, and window_from_year to pick the since-<year>
                # summary. The Source picker lands on the by-source table
                # but NOT the global scalars, whose silver_source_id is ''
                # — a source filter would blank them.
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
            if name in SPENDING_DASHBOARDS:
                # The Spending pickers land on the spending models'
                # dimensions. Currency is a row filter here (the model
                # carries one row per reporting currency) where a native
                # spending card takes it as a {{currency}} variable —
                # the same split the returns tiles already make.
                maps += [{"parameter_id": pid, "card_id": card_ids[card],
                          "target": ["dimension", _f(col, "type/Text")]}
                         for pid, col in (
                             (SPEND_CURRENCY_PARAM_ID, "currency"),
                             (ACCOUNT_PARAM_ID, "display_name"),
                             (CATEGORY_PARAM_ID, "spend_primary"))
                         if not (pid == SPEND_CURRENCY_PARAM_ID
                                 and card in SPEND_ALL_CURRENCY_CARDS)]
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
