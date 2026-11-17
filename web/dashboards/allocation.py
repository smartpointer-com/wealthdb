"""The Allocation dashboard's cards and its twin's. Part of provision.py."""

from sqlkit import (BENCHMARK_TAG, _beside, _beside_viz, _ccy_case, _cl, _columns_with_benchmark,
                    currency_tag, HOLDING_KEY_PREFIX, HOLDINGS_FILTERS, native_tags, _native_where,
                    PICKED_BENCHMARK, PRIVACY_DESC, _share_viz, _table_columns, twin_name, view_tags)


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
               "  FROM p" + POSITION_SIZE_JOIN
               + "\n ORDER BY value_pct DESC, p.symbol, p.name, p.asset_class, p.vehicle\n LIMIT 100",
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
               "\n   GROUP BY 1, 2, 3, 4, line_key\n   ORDER BY 5 DESC, 1, 2, 3, 4, line_key\n   LIMIT 100)\n"
               "SELECT p.symbol, p.name, p.asset_class, p.vehicle, z.size, p.value\n"
               "  FROM p" + POSITION_SIZE_JOIN + "\n ORDER BY p.value DESC, p.symbol, p.name, p.asset_class, p.vehicle",
               {}, tags=tags)
    issuer_cards(f)


def issuer_cards(f):
    """The Allocation dashboard's issuers card, or on the twin its
    privacy variant. Native SQL over web_exposure_issuers: the largest issuers of the
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


POSITION_FILTERS = {**HOLDINGS_FILTERS, "vehicle": ("vehicle", "string/=")}

# The Top 100 positions cards show a stock's size beside it
# (web_position_sizes), joined after the ranking on the position's label, so
# the list and its values stay those of web_positions_history alone.
POSITION_SIZE_JOIN = ("\n  LEFT JOIN web_position_sizes z ON z.symbol IS NOT DISTINCT FROM p.symbol"
                      "\n       AND z.name IS NOT DISTINCT FROM p.name"
                      "\n       AND z.asset_class IS NOT DISTINCT FROM p.asset_class"
                      "\n       AND z.vehicle IS NOT DISTINCT FROM p.vehicle")

POSITION_SIZE_NOTE = " Size is a stock's market-cap bucket."


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
    held in the benchmark's mix, so both bars read in the picked currency.
    A benchmark is a whole allocation, so its bars stand only beside a
    whole one: none while the Asset class picker holds a value, and none
    when the household's total in view is not above zero, which no mix
    can be scaled to."""
    where = _asof_where(tags)
    whole = (f"\n   AND NOT EXISTS (SELECT 1 FROM {view} WHERE FALSE[[ OR {{{{asset_class}}}}]])"
             if "asset_class" in tags else "")
    if benchmark and not share:
        return (f"WITH p AS (\n  SELECT {dim}, sum({_ccy_case(col)})::DOUBLE AS value\n"
                f"    FROM {view}" + where + "\n   GROUP BY 1)\n"
                + _beside(dim, "value", benchmark, weight="share_pct / 100 * (SELECT sum(value) FROM p)",
                          where=whole + "\n   AND (SELECT sum(value) FROM p) > 0"))
    if benchmark:
        return ("WITH r AS (\n"
                f"  SELECT {dim}, sum({_ccy_case(col)})::DOUBLE AS v\n"
                f"    FROM {view}" + where + "\n   GROUP BY 1),\n"
                f"p AS (SELECT {dim}, v / (SELECT CASE WHEN sum(v) <> 0 THEN sum(v) END FROM r) * 100 AS value_pct\n"
                "        FROM r)\n" + _beside(dim, "value_pct", benchmark,
                                             where=whole + "\n   AND (SELECT sum(v) FROM r) > 0"))
    if not share:
        return (f"SELECT {dim}, sum({_ccy_case(col)})::DOUBLE AS value\n"
                f"  FROM {view}" + where + "\n GROUP BY 1\n ORDER BY 2 DESC, 1")
    return ("WITH r AS (\n"
            f"  SELECT {dim}, sum({_ccy_case(col)})::DOUBLE AS v\n"
            f"    FROM {view}" + where + "\n"
            "   GROUP BY 1)\n"
            f"SELECT {dim},\n"
            "       v / (SELECT CASE WHEN sum(v) <> 0 THEN sum(v) END FROM r)"
            " * 100 AS value_pct\n"
            "  FROM r\n ORDER BY 2 DESC, 1")
