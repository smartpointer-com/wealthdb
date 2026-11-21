"""The Exposure dashboard's cards and its twin's. Part of provision.py."""

from sqlkit import (BENCHMARK_COLOR, BENCHMARK_SERIES, BENCHMARK_TAG, _beside, _beside_viz,
                    _columns_with_benchmark, EXPOSURE_FILTERS, HOLDING_KEY_PREFIX, native_tags, NO_BENCHMARK,
                    PICKED_BENCHMARK, PORTFOLIO_SERIES, PRIVACY_DESC, _share_viz, STACKED_VIZ,
                    _table_columns, tooltip_viz, twin_name, _window_end)


# The colour Metabase gives each share column on its own, kept for the
# portfolio's series on a card that breaks out by series.
EXPOSURE_BAR_COLORS = {"share_pct": "#7172AD", "class_share_pct": "#A989C5"}


def exposure_cards(f):
    """The Exposure dashboard's cards, or on the twin their privacy
    variants. Native
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
            viz.update(tooltip_viz(*cols))
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
             f" ORDER BY max({rank}), 2, 1",
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
                   order="exposure_bucket_order('size', size)",
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
    class_share = "exposure_class_share(sum(value), any_value(c.total))"

    def class_select(dimension, label, cols):
        """The CTEs of a class_bars card over `dimension`, and its rows."""
        return (_window_end("web_exposure_debt", dtags, where=f"\n     AND dimension = '{dimension}'")[1] + ",\n"
                "c AS (SELECT sum(value) AS total FROM e WHERE bucket <> '(none)')",
                f"SELECT bucket AS {label}{cols}{money},\n       {class_share} AS class_share_pct\n"
                "  FROM e, c\n WHERE bucket <> '(none)'\n GROUP BY bucket, grade, bucket_order\n"
                f"HAVING coalesce(abs({class_share}) >= {SMALLEST_BAR_PCT}, TRUE)")

    def class_bars(name, desc, dimension, label, extra=(), tooltip=(), beside=False):
        """A dimension's bars at the window's end, in the view's own
        bucket order (bucket_order): each bucket's share of its class,
        with the `extra` columns beside it and the `tooltip` ones in its
        tooltip. With `beside`, the picked benchmark's on the same
        reading."""
        head, select = class_select(dimension, label, "".join(f",\n       {c}" for c in extra))
        if beside:
            card(name, "bar", desc + whole_class,
                 head + ",\np AS (\n" + select + ")\n"
                 + _beside(label, "class_share_pct", dimension, (*tooltip, *amount),
                           weight="100 * share_pct / sum(share_pct) OVER ()", where="\n   AND bucket <> '(none)'",
                           order=f"exposure_bucket_order('{dimension}', {label})", keep_series=True),
                 bars(label, "class_share_pct", tooltip, decimals=2, beside=True), tags=dtags)
            return
        card(name, "bar", desc + whole_class,
             head + "\n" + select + "\n ORDER BY bucket_order",
             bars(label, "class_share_pct", tooltip, decimals=2), tags=dtags)

    class_bars("Debt by credit rating, share of debt and cash",
               "Counts debt and cash by rating at the window's end. It covers the last twelve months.",
               "credit", "rating",
               extra=("grade", "100 * sum(abs(value)) FILTER (WHERE unknown) / NULLIF(sum(abs(value)), 0)"
                               " AS unknown_pct"),
               tooltip=("grade", "unknown_pct"), beside=True)
    class_bars("Debt by maturity",
               "Counts debt and cash by term at the window's end. It covers the last twelve months.",
               "maturity", "maturity", beside=True)
    class_bars("Debt by coupon kind",
               "Counts debt by coupon kind at the window's end. It covers the last twelve months.",
               "coupon", "coupon")
    class_bars("Public equity by cycle",
               "Counts public equity by cycle at the window's end. It covers the last twelve months.",
               "cycle", "cycle")

    # The securities, each summed over its slices of the selection
    # (web_exposure_securities): the labels of its heaviest slice that
    # has them, the greater label on a tie, the funds counted once, as
    # `wealthdb exposure securities` gives them with nothing picked. The
    # share is of the selection, all of its securities counted. The twin
    # drops the values and hides a key that is a holding's own, which
    # can carry a contract's or an account's id. The name shows, less any
    # address. The share and the benchmark's columns come right after the
    # name, as on Top 100 issuers, so a full-width tile shows them.
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
          **_table_columns("security", "name", *amount, "share_pct", "benchmark_pct", "difference_pct",
                           "ticker", "asset_class", "sector", "country", "market_cap", "size",
                           *(("direct_pct", "via_funds_pct") if privacy else ("direct_value", "via_funds_value")),
                           "funds")}, tags=stags)


# The smallest share, in percent, a debt card draws as a bar.
SMALLEST_BAR_PCT = 0.01

# Its debt cards. The debt view carries no asset class,
# so the class picker does not reach those cards.
EXPOSURE_DEBT_FILTERS = {k: v for k, v in EXPOSURE_FILTERS.items() if k != "asset_class"}
