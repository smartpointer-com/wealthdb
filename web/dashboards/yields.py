"""The Yield dashboard's cards and its twin's. Part of provision.py."""

from sqlkit import (BENCHMARK_TAG, _ccy_case, _columns_with_benchmark, EARNING_VALUE, EXPOSURE_FILTERS,
                    HORIZON_MONTHS, LINES_VIZ, native_tags, _native_where, part_income, PARTS_AHEAD,
                    PARTS_FROM, PEAK, _peak_cte, PICKED_BENCHMARK, PRIVACY_DESC, PROJECTION_TAGS, _share_viz,
                    tooltip_viz, twin_name, _window_end)


def _rate(x):
    """The value-weighted average of rate column `x` over a group's rows
    that state it, weighed as yield.summarize weighs one (gold macro
    yield_rate)."""
    return f"yield_rate({x}, value, weighs)"


# A group's realized yield, pooled as yield.summarize pools a row's: its
# holdings' income of the twelve months over the sum of their mean daily
# values, over the assets, else over every row (gold macro
# yield_realized_pct). The days are not in the table, so the engine's
# thin rule for a row does not apply to a selection.
REALIZED_YIELD = "yield_realized_pct(received_12m, mean_value, weighs)"


def _income_yield(income="income_12m"):
    """A group's income yield: its income over the value that earns it."""
    return f"100 * sum({income}) / NULLIF({EARNING_VALUE}, 0)"


# A part of a holding web_yield's today rows select, as CTE `h`.
HELD_JOIN = ("    JOIN h ON h.silver_source_id = p.silver_source_id"
             " AND h.account_external_id = p.account_external_id\n"
             "         AND h.position_key = p.position_key")

def yield_cards(f):
    """The Yield dashboard's cards, or on the twin their privacy
    variants. Native
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
               f"  SELECT p.month, ({part_income()}) * {_ccy_case('p.fx')} AS income\n" + PARTS_FROM + "\n" + HELD_JOIN +
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
          "graph.show_values": True, **tooltip_viz("share_pct" if privacy else "value"),
          **_share_viz("income_yield_pct", "implied_yield_pct", *(["share_pct"] if privacy else []))})

    # The worst paid first: the balances no gap is known for, then the
    # largest shortfall below the benchmark; a debit balance after the
    # credit ones, as `wealthdb yield cash` lists them.
    cash_order = "value < 0, gap_pct NULLS FIRST, value DESC"
    cash_rows = (end + ",\nc AS (\n  SELECT account_label, kind, holding_currency, stated_pct, benchmark_pct, gap_pct, value\n"
                 "    FROM e\n   WHERE is_cash)\n")
    cash_desc = ("Each cash holding's rate against its benchmark at the window's end, worst "
                 "paid first.")
    if privacy:
        card("Cash against its benchmark", "table",
             cash_desc + " No account is named.",
             cash_rows + f"SELECT row_number() OVER (ORDER BY {cash_order}, kind, holding_currency, stated_pct, benchmark_pct) AS rank,\n"
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

    # A dot's tooltip names its holding and where it is held: the account
    # on the base card, the source alone on the twin, which names no
    # account.
    size = "share_pct" if privacy else "value"
    where = "source" if privacy else "account"
    card("Yield against duration", "scatter",
         "Each bond and rate-sensitive fund at the window's end, duration across, implied "
         f"yield up, sized by {'share' if privacy else 'value'}.",
         end + "\nSELECT holding_currency AS currency, duration, implied_pct AS implied_yield_pct, symbol, "
         + (f"{twin_name('name')} AS name, silver_source_id AS source,\n"
            "       100 * value / NULLIF(sum(value) OVER (), 0) AS share_pct"
            if privacy else "name, account_label AS account,\n       value") +
         "\n  FROM e\n WHERE rate_sensitive\n"
         "   AND duration IS NOT NULL AND implied_pct IS NOT NULL\n ORDER BY value DESC",
         {"graph.dimensions": ["duration", "currency"], "graph.metrics": ["implied_yield_pct"],
          "scatter.bubble": size, **tooltip_viz("symbol", "name", where, size),
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
    up = part_income("{{shift}} + 100")
    sens = (held + ",\ns AS (\n"
            f"  SELECT p.holding_currency, (({up}) - ({part_income()})) * {_ccy_case('p.fx')} AS v\n" + PARTS_FROM +
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

    held_in = "silver_source_id" if privacy else "account_label"
    card("Dividend yield against payout", "scatter",
         "Each stock's payout ratio against its dividend yield at the window's end. A payout "
         "above 100% is not covered by earnings.",
         end + f"\nSELECT symbol, {twin_name('name') + ' AS name' if privacy else 'name'}, payout_pct, dividend_yield_pct,\n"
         f"       string_agg(DISTINCT {held_in}, '; ' ORDER BY {held_in}) AS held_in\n  FROM e\n"
         " WHERE kind = 'stock'\n   AND payout_pct IS NOT NULL AND dividend_yield_pct IS NOT NULL\n"
         " GROUP BY ALL\n"
         " ORDER BY dividend_yield_pct DESC, symbol, name, payout_pct",
         {"graph.dimensions": ["payout_pct"], "graph.metrics": ["dividend_yield_pct"],
          **tooltip_viz("symbol", "name", "held_in"), **_share_viz("payout_pct", "dividend_yield_pct")})

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
          **({} if privacy else tooltip_viz("value", "holdings")), **_share_viz("share_pct")})


# The Yield dashboards' filters over web_yield and web_yield_curve: the
# Exposure set, the window's last day being the day a card reads.
YIELD_FILTERS = EXPOSURE_FILTERS
