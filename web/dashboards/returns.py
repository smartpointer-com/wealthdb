"""The Returns dashboard's cards and its twin's. Part of provision.py."""

import sqlkit
from sqlkit import (ALL_SOURCES_SERIES, BENCHMARK_COLOR, BENCHMARK_SERIES, BENCHMARK_TAG,
                    LINES_VIZ, _percent_viz, PICKED_BENCHMARK, PORTFOLIO_SERIES, _series_order, _series_viz)


# report_returns' absolute money columns: only the base by-source table
# lists them.
RETURNS_MONEY_COLUMNS = ("start_value", "end_value", "net_flow")

BENCHMARK_LINE = {"series_settings": {BENCHMARK_SERIES: {
    "color": BENCHMARK_COLOR, "line.style": "dashed", "line.size": "L", "line.marker_enabled": False}}}

RETURNS_PRIVACY_DESC = " Money columns are redacted."

START_YEAR_TAG = {"id": "year-tag", "name": "start_year",
                  "display-name": "Start year", "type": "number",
                  "default": "0", "required": True}


def returns_tags(sql=None):
    """Template tags for the native returns charts, four: the currency
    and source field filters on report_returns, the start year variable
    and the benchmark, which Family.card keeps only on a card that reads
    it. The Returns dashboard's pickers map onto them. Currency and Source
    are field filters (a dropdown, values from the column); currency and
    start year default so a card still runs standalone. Start year is a
    plain number variable (it drives a `year(end_day) >= …` range, which
    a field filter can't express). With `sql` the Source tag only where
    that query reads it, so a tile it misses says so."""
    # A field-filter default is a LIST (string/= is multi-value-shaped),
    # unlike a plain text variable's bare-string default.
    tags = {"currency": {"id": "ccy-ff", "name": "currency", "display-name": "Currency",
                         "type": "dimension",
                         "dimension": ("field", "report_returns", "currency"),
                         "widget-type": "string/=", "default": [sqlkit.DEFAULT_CURRENCY],
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


# The currency and source clauses of the native returns charts: the
# currency field filter inside each UNION arm, and the source one as an
# optional [[…]] clause, omitted when nothing is selected.
RETURNS_CCY_CLAUSE = "\n     AND {{currency}}"

RETURNS_SOURCE_CLAUSE = "\n     [[AND {{source_ff}}]]"

# A benchmark table's currency, by the {{currency}} field filter, which
# binds report_returns' column and so reaches another table through it.
BENCHMARK_CCY_CLAUSE = ("\n     AND currency IN (SELECT currency FROM report_returns WHERE TRUE"
                        + RETURNS_CCY_CLAUSE + ")")


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


def returns_by_source_sql(money):
    """The by-source returns table: each source's returns and quality
    flags, with `money` the absolute money columns (RETURNS_MONEY_COLUMNS)
    too, and the picked benchmark as one more line over the same window."""
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


def returns_cards(f):
    """The Returns dashboard's cards, or on the twin its privacy cards,
    over the materialized report_returns table. The three scalars read
    the global grain's summary (granularity 'total') of the window the
    Start year picks: 0 the since-inception one, else the since-<year>
    one, so the picker rescopes the whole figure exactly (the
    since-inception TWR is often null around the degenerate first
    months). twr/mwr are ratios rendered as percent. Opened standalone
    a scalar reads the default currency and start year. They show
    ratios alone, so the twin shows them as they are.

    Every chart splits
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

    # A whole-portfolio figure: the global grain's summary of the start
    # year's window, so the Source picker does not reach it.
    sql = ("SELECT max(twr) AS max\n  FROM report_returns\n"
           " WHERE grain = 'global' AND granularity = 'total'\n"
           "   AND window_from_year = {{start_year}}" + RETURNS_CCY_CLAUSE)
    f.card("Return (TWR)", "scalar",
           "Whole-portfolio time-weighted return over the chosen window, with "
           "the timing and size of external flows stripped out.",
           sql, _percent_viz("max"), tags=returns_tags(sql), exempt=True)
    # The global grain's summary of the start year's window, which the
    # Source picker does not reach.
    mwr_sql = ("SELECT max(mwr) AS max\n  FROM report_returns\n"
               " WHERE grain = 'global' AND granularity = 'total'\n"
               "   AND window_from_year = {{start_year}}" + RETURNS_CCY_CLAUSE)
    f.card("Return (MWR)", "scalar",
           "Whole-portfolio money-weighted return (XIRR) over the chosen "
           "window, external flows included.",
           mwr_sql, _percent_viz("max"), tags=returns_tags(mwr_sql), exempt=True)
    # The global grain's summary of the start year's window, which the
    # Source picker does not reach.
    annualized_sql = ("SELECT max(twr_annualized) AS max\n  FROM report_returns\n"
                      " WHERE grain = 'global' AND granularity = 'total'\n"
                      "   AND window_from_year = {{start_year}}" + RETURNS_CCY_CLAUSE)
    f.card("Annualized return (TWR)", "scalar",
           "The window's time-weighted return as a constant yearly rate, n/a "
           "for windows under a year.",
           annualized_sql, _percent_viz("max"), tags=returns_tags(annualized_sql), exempt=True)
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
