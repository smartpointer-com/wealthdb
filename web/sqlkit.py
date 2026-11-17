"""The vocabulary every card is written in: the reporting currencies and
the configured defaults, the template tags and their WHERE clauses, the
series order and the viz helpers, the privacy naming, and the card
maker (Card, Link, Family). Part of provision.py."""

import sys
from typing import NamedTuple


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


def _ccy_pick(alias, col):
    """`<alias>.<col>_<ccy>` for the row's own reporting currency `c.currency`
    — the unpivot of a wide value set into the long model shape."""
    return _ccy_switch("c.currency", lambda c: f"{alias}.{col}_{c}")


def _ccy_case(col, neg=False):
    """The `col`_<ccy> column set reduced to the one the required
    {{currency}} variable names. A template variable interpolates a
    VALUE, never an identifier, so a native card picks its column with a
    CASE rather than by splicing a column name in. `neg` negates it, so
    a figure gold stores negative by convention — a spending outflow, a
    card's owed balance — reads as a positive one."""
    s = "-" if neg else ""
    return _ccy_switch("{{currency}}", lambda c: f"{s}{col}_{c}")


# The label gold's spending views carry for a line whose category the
# enrichment pass could not resolve. Rendered at the view rather than left
# NULL, so a breakdown shows the backlog as a bucket instead of an
# unlabelled slice; the uncategorized-share card counts rows carrying it.
UNCATEGORIZED = "(uncategorized)"

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

# A mid-tone in no palette: it has to read on a light and on a dark theme.
BENCHMARK_COLOR = "#8A99A8"


def _text_tag(name, label, default):
    """A required text template variable with a default, so a card still
    runs opened on its own."""
    return {"id": f"{name}-tag", "name": name, "display-name": label,
            "type": "text", "default": default, "required": True}


BENCHMARK_TAG = _text_tag("benchmark", "Benchmark", NO_BENCHMARK)

# The global line beside the sources, solid where the benchmark is dashed.
ALL_SOURCES_SERIES = "(all sources)"

# Denominator-neutral by design: a twin card's own text says what its
# shares are of (latest total, chosen day's total, peak month, the
# window's own net spend) where its title does not.
PRIVACY_DESC = " Absolute amounts never show."

# The suffix that marks a privacy variant — of a card, and of the
# dashboard it sits on. One home, because it is also how a dashboard
# tells itself apart from its twin (dashboard_parameters,
# dashboard_payloads, overview_home).
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


def _native(sql, tags):
    """A native-SQL dataset_query with template variables."""
    return {"type": "native", "native": {"query": sql, "template-tags": tags}}


PICKED_BENCHMARK = "benchmark = {{benchmark}}"


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
        SQL over `tags` (the family's unless given). A family's tags are
        all its cards', but the Benchmark lands only on a card that reads
        it."""
        if self.privacy and exempt:
            return
        tags = self.tags if tags is None else tags
        if "{{benchmark}}" not in sql:
            tags = {k: v for k, v in tags.items() if k != "benchmark"}
        name = privacy_name(name) if self.privacy else name
        if name in self.cards:
            raise ValueError(f"provision: card {name!r} is defined twice")
        self.cards[name] = Card(name, display, desc + self.tail, _native(sql, tags), viz, exempt, hide_empty,
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


# The key gold gives a holding no security key names
# (gold.HoldingKeyPrefix): it can carry an account's id.
HOLDING_KEY_PREFIX = "holding:"

# The value a group's income yield divides by, weighed as its rates are
# (gold macro yield_earning_value).
EARNING_VALUE = "yield_earning_value(value, weighs)"

# A projection part's income under the picked rate path and shift, in its
# holding's currency (gold macro yield_part_income). PARTS_FROM joins
# each part to its rate as `rt`; a currency the rates lack reads zero, as
# the engine's does.
PART_INCOME = ("yield_part_income(p.fixed, p.principal, p.spread, COALESCE(rt.rate_pct, 0), p.roll_constant,\n"
               "                               p.roll_forward, p.roll_principal, {{rates}}, {{shift}})")

PARTS_FROM = ("    FROM web_yield_parts p\n"
              "    LEFT JOIN web_yield_rates rt ON rt.currency = p.holding_currency AND rt.month = p.month\n"
              "         AND rt.path = {{rates}}")

HORIZON_MONTHS = "CAST({{horizon}} AS INTEGER)"

# The months of the horizon after the current one: month 0 never counts.
AFTER_THIS_MONTH = " BETWEEN 1 AND " + HORIZON_MONTHS

# The parts under the picked Maturing rule over those months.
PARTS_AHEAD = "\n   WHERE p.rule = {{maturing}}\n     AND p.month" + AFTER_THIS_MONTH


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


EXPOSURE_FILTERS = {**range_filters("as_of_day"), "tax_wrapper": ("tax_wrapper", "string/="),
                    "asset_class": ("asset_class", "string/=")}

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

# The projection's assumptions (docs/INCOME.md §12), each list's default
# first: the rate path (yield.Paths), what a maturing principal earns
# (yield.Rules), and the horizon in months, up to the 36 the projection
# covers. The pickers and the template variables read them, and a test
# holds them to the engine's.
RATE_PATHS = ("constant", "forward")

MATURING_RULES = ("cash", "idle", "roll")

HORIZONS = ("12", "24", "36")


# The four variables every card with an expected figure reads. The shift
# is a number of basis points, below zero to lower the rates. The horizon
# is text, read as a number in the SQL: its picker offers the three
# horizons as a list, which Metabase draws for a text picker.
PROJECTION_TAGS = {"rates": _text_tag("rates", "Rate path", RATE_PATHS[0]),
                   "maturing": _text_tag("maturing", "Maturing", MATURING_RULES[0]),
                   "shift": {"id": "shift-tag", "name": "shift", "display-name": "Shift (bp)",
                             "type": "number", "default": "0", "required": True},
                   "horizon": _text_tag("horizon", "Horizon (months)", HORIZONS[0])}


def native_tags(table, spec):
    """Template tags for a native money card over serving view `table`:
    the required {{currency}} text variable, plus a field filter per
    column in `spec` (view_tags)."""
    return {"currency": currency_tag(), **view_tags(table, spec)}


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
