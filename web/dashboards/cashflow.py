"""The Cash Flow dashboard's cards and its twin's. Part of provision.py."""

from sqlkit import (BENCHMARK_COLOR, _ccy_case, currency_tag, _month_cte, native_tags, _native_where,
                    OCCURRED_MONTH, PEAK, PEAK_DIV, _percent_viz, PRIVACY_DESC, PROJECTION_ARGS, PROJECTION_TAGS,
                    range_filters, _series_order, _share_viz, STACKED_VIZ)


# The Cash Flow dashboard's investing grain. Required with a `whole`
# default, so a card opened away from the dashboard draws the section as
# one movement — the reading a household opens the statement with —
# rather than running with the variable cleared.
INVESTING_TAG = {"id": "inv-tag", "name": "investing",
                 "display-name": "Investing", "type": "text",
                 "default": "whole", "required": True}

# The Cash Flow statement's series: a combo draws its last series as a line
# unless each says what it is, so the sections are bars and the net a line.
NET_CASH_FLOW = "net cash flow"

STATEMENT_SERIES = {"series_settings": {**{s: {"display": "bar"}
                                           for s in ("operating", "investing", "financing", "vehicles")},
                                        NET_CASH_FLOW: {"display": "line", "color": BENCHMARK_COLOR}}}


def cashflow_cards(f):
    """The Cash Flow dashboard's cards, or on the twin their privacy
    variants. Every tile of the statement is native over web_cashflow,
    and a required {{currency}} variable picks the value column. One
    more variable is its own: {{investing}}, the display grain of the
    investing section. It is a VARIABLE and not a field filter because it changes how rows are
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
            "SELECT row_number() OVER (ORDER BY abs(v) DESC, occurred_at DESC, section, class, \"group\") AS rank,\n"
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
        f"\n ORDER BY abs({_ccy_case('value')}) DESC, occurred_at DESC, section, class, \"group\", name, account\n LIMIT 50",
        {})
    cashflow_plan_cards(f)
    bridge_cards(f)


# The cash flow projection's look-back parts (web_cashflow_plan) and the
# order its chart stacks them: what comes in, then what goes out.
PLAN_LOOKBACK_PARTS = ("other_income", "operating_out", "debt_service", "vehicles")

PLAN_INCOME_PARTS = ("investment_income", "other_income", "principal_returned", "vehicles")

PLAN_OUTFLOW_PARTS = ("operating_out", "debt_service")

def cashflow_plan_cards(f):
    """The Cash Flow dashboard's projection cards and the Wealth
    Overview's projected net, or on the twin their privacy variants.
    `wealthdb cashflow projection` month by month over the Horizon
    (docs/CASHFLOW.md §12), as gold's cashflow_projection gives it: the
    income projection's investment income and
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

    # Each month of the horizon: the projection's income and principal
    # beside the look-back's parts (gold macro cashflow_projection).
    plan = "WITH proj AS (SELECT * FROM cashflow_projection(" + PROJECTION_ARGS + "))"
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
    """The Cash Flow dashboard's bridge tiles, or on the twin their
    privacy variants. They read
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
        "  FROM e ORDER BY e.stage, e.kin DESC, e.value DESC, e.source, e.target")
