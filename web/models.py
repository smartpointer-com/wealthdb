"""The pre-defined report models: native SQL over the gold serving tables
and views, for the pickers' values and a reader's own questions. Part
of provision.py."""

from sqlkit import (_ccy_pick, _ccy_slash, _ccy_values, _ccy_words)


def report_models():
    """model name -> (native SQL, description). The report_*_multi DuckDB
    macros (gold derived/30_holdings.sql) are the single source of truth.

    Each model wraps the serving table `web refresh` fills with a macro's
    output (gold servingTables), a macro computing its whole output on
    every read. It renders epoch columns as TIMESTAMP for Metabase:
    `epoch_ms` gives zone-free UTC, where `to_timestamp` gives
    TIMESTAMPTZ, which renders in the reading session's zone. The macros
    already emit DECIMAL money/quantity columns and one value column set
    per reporting currency (REPORTING_CURRENCIES), so no value casting is
    needed here. The `_latest` reports are as of each account's latest
    snapshot; the `_history` reports carry value forward per day.

    The taxonomy, spending, income and cashflow models wrap the web_*
    serving views (gold derived/90_web.sql) instead, which do that
    rendering, the cash fold-in and the '(uncategorized)' labelling
    themselves. The gains- and yield-classes models list the classes of
    web_gains and of web_yield and web_yield_curve.

    No card reads a model: every card is native SQL over gold
    (card_defs), and a model backs a picker's values or a reader's own
    questions."""
    def wrap(from_expr, ts_cols=(), exclude=(), where=""):
        parts = [f"epoch_ms({c} * 1000) AS {c}" for c in ts_cols]
        excl = f" EXCLUDE ({', '.join(exclude)})" if exclude else ""
        cond = f" WHERE {where}" if where else ""
        return f"SELECT *{excl} REPLACE ({', '.join(parts)}) FROM {from_expr}{cond}"

    def taxonomy_history(view):
        """Breakdown-by-taxonomy models over the gold web_* serving views
        (derived/90_web.sql), which already render as_of_day as TIMESTAMP and
        fold each source's cash balance into a class/vehicle of its own so
        the rows sum exactly to net worth."""
        return f"SELECT * FROM {view}"

    def spending():
        """The spending model over the gold web_spending view, which
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
        view. It is never summed with the resolved category and no tile
        aggregates it — the two disagree by design.

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
        reporting currency), the long shape report_returns has, so a
        reader's own question picks a currency with a `currency`
        filter."""
        cols = ("s.occurred_at, s.silver_source_id, s.account_external_id,\n"
                "       s.account_label AS display_name, s.account_kind,\n"
                "       s.merchant_name,\n"
                "       s.spend_primary_label AS spend_primary,\n"
                "       s.spend_label         AS spend_detailed,\n"
                "       s.spend_primary AS spend_primary_id,\n"
                "       s.spend_detailed AS spend_detailed_id,\n"
                "       s.provider_spend_label AS provider_category, c.currency")
        return (f"SELECT {cols},\n"
                f"       {_ccy_pick('s', 'value')} AS value\n"
                "  FROM web_spending s,\n"
                f"       {_ccy_values()}")

    def income():
        """The income model over gold's web_income view.

        The spending model's shape with the income vocabulary, and the
        same reasoning behind every choice: the type columns carry the
        DISPLAY LABEL, with the vendored values beside them as `*_id`;
        `display_name` carries the account LABEL; and the wide value
        set is unpivoted to one row per (line, reporting currency).

        Every income tile is native (they all read the serving view
        directly). This model exists for the two dashboard PICKERS, which
        need a card-backed value list.

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

        Like the income model it backs a dashboard picker, the Section
        one, which needs a card-backed value list.

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

    # Each source's newest snapshot, the latest mark it rides on
    # (valued_at) and its last read (web_sources_observed): a quiet source
    # is observed through its latest download although its latest mark is
    # older.
    sources_latest = "(SELECT * EXCLUDE (is_feed) FROM web_sources_observed) s"
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
        # The returns model wraps the materialized report_returns TABLE
        # (written by the engine on `web refresh`) rather than a macro.
        "report_returns": (
            wrap("report_returns", ["computed_at", "start_day", "end_day"]),
            "Materialized TWR/MWR returns, refreshed on every `wealthdb web "
            "refresh`: each (grain, granularity, currency) slice is the verbatim "
            "output of `wealthdb returns <grain> --period <granularity> --method "
            "both -x <currency>`. twr/mwr are ratios (0.07 = 7%); NULL means not "
            "computable, with the reason in quality. Bucket rows carry TWR only; "
            "MWR lives on the is_summary (since-inception) rows."),
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
        # The holdings the private-markets rows list, with their source,
        # class and vehicle: the Private Markets dashboard's Asset
        # class, Vehicle and Holding pickers list them. It backs those
        # pickers alone.
        "report_private_holdings": (
            "SELECT DISTINCT silver_source_id, holding, asset_class, vehicle FROM web_private WHERE NOT is_total",
            "The private holdings, each with its source, asset class and vehicle. It backs the Private "
            "Markets dashboard's Asset class, Vehicle and Holding pickers."),
        "report_benchmarks": (
            "SELECT benchmark FROM web_benchmarks",
            "The benchmarks, built-in and configured, and None. It backs the "
            "Benchmark picker."),
        # The classes web_yield and web_yield_curve carry, `unknown` among
        # them where nothing classes a holding: the Yield dashboard's
        # Asset class picker lists them, as the asset class history may
        # never hold `unknown`. It backs that picker alone.
        "report_yield_classes": (
            "SELECT asset_class FROM web_yield\nUNION\nSELECT asset_class FROM web_yield_curve",
            "The asset classes the yield rows carry, `unknown` where nothing classes a "
            "holding. It backs the Yield dashboard's Asset class picker."),
    }
