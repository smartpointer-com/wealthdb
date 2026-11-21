"""The Private Markets dashboard's cards and its twin's. Part of provision.py."""

from sqlkit import (_cl, _field_clauses, FUNDED_CHOICES, LINES_VIZ, native_tags, PRIVACY_DESC, _share_viz, STACKED_VIZ,
                    _text_tag, tooltip_viz, view_tags)


# The multiples the chart stacks, in stacking order, and their colours:
# what came back, then what is still marked, which add up to TVPI.
MULTIPLE_COLORS = {"dpi": "#2A78D6", "rvpi": "#8A99A8"}

MULTIPLE_TITLES = {"dpi": "Distributed (DPI)", "rvpi": "Still held (NAV / paid in)"}

PRIVATE_PRIVACY_DESC = " Money columns and account names are redacted."

# A holding's label on a chart that draws one bar per holding: its name
# and source, and where the source holds two of that name, as one fund
# in two portfolios, a number by its account that tells them apart. The
# CTE `n` over `p` numbers them.
HOLDING_NAMES = ("n AS (\n  SELECT *, count(*) OVER (PARTITION BY silver_source_id, holding) AS same_name,\n"
                 "         row_number() OVER (PARTITION BY silver_source_id, holding\n"
                 "                            ORDER BY account_external_id, instrument_external_id) AS name_n\n"
                 "    FROM p)")
HOLDING_LABEL = ("holding || ' (' || silver_source_id || CASE WHEN same_name > 1 THEN ' ' || name_n ELSE '' END"
                 " || ')' AS holding")

# The cash flows to date, in their order: what was called, what came
# back, and the net, which dips while calls come in and climbs as money
# comes back (the J-curve).
FLOW_COLORS = {"called": "#EB6834", "distributed": MULTIPLE_COLORS["dpi"], "net": "#1BAF7A"}

FLOW_TITLES = {"called": "Called", "distributed": "Distributed", "net": "Net cash flow"}

# The Gain against multiple dots, one series per status: held, still
# marked, in the colour of what is; exited, all of it back, in DPI's;
# partly back, in the net cash flow's.
STATUS_COLORS = {"held": MULTIPLE_COLORS["rvpi"], "partially_exited": FLOW_COLORS["net"],
                 "exited": MULTIPLE_COLORS["dpi"]}

STATUS_TITLES = {"held": "Held", "partially_exited": "Partially exited", "exited": "Exited"}

# A commitment's parts, in stacking order: what was called of it, in
# the cash flows' colour of a call, then what it still holds open.
COMMITMENT_COLORS = {"called": FLOW_COLORS["called"], "unfunded": "#B8C2CC"}

COMMITMENT_TITLES = {"called": "Called", "unfunded": "Unfunded"}

# The pickers every card takes, beside the currency and the as-of day,
# as field filters on the holdings: the source, the asset class, the
# vehicle and the holdings left out. A holding's name is no account's,
# so the twin takes them all.
PRIVATE_FILTERS = {"source": ("silver_source_id", "string/="), "asset_class": ("asset_class", "string/="),
                   "vehicle": ("vehicle", "string/="), "holding": ("holding", "string/!=")}

# The Commitments chart's {{funded}} variable (the Fully funded picker)
# and the holdings it keeps: under Show every one, under Hide those with
# something still unfunded.
FUNDED_TAG = _text_tag("funded", "Fully funded", FUNDED_CHOICES[0])

FUNDED = f"({{{{funded}}}} = '{FUNDED_CHOICES[1]}' OR unfunded <> 0)"

# The holdings the Commitments chart draws, a bar each, and on the twin
# the total their shares are of. A commitment with no unfunded draws no
# bar, and under Hide neither does one with nothing unfunded.
DRAWN = f"commitment IS NOT NULL AND unfunded IS NOT NULL AND {FUNDED}"

# The instant a card reads: the latest stored as-of on or before the end
# of the As of day (web_private_calendar). A day before the first
# stored instant reads none.
AS_OF = "(SELECT max(as_of) FROM web_private_calendar WHERE TRUE" + _cl("as_of_day") + ")"

# Whether the pickers keep every holding of the instant (private_rows).
WHOLE = "(SELECT count(*) FROM p) = (SELECT count(*) FROM a WHERE NOT is_total)"


def private_tags(view):
    """The template tags of a Private Markets card over serving view
    `view`: the currency, the holdings' field filters (PRIVATE_FILTERS)
    and the As of day's, bound to web_private_calendar."""
    return {**native_tags(view, PRIVATE_FILTERS),
            **view_tags("web_private_calendar", {"as_of_day": ("as_of_day", "date/single")})}


def private_rows(tags):
    """The CTEs of a card over web_private at the picked as-of instant
    in the picked currency: `a`, every row, the total among them, and
    `p`, the holdings the pickers keep. Neither ends in a newline."""
    at = "\n   WHERE currency = {{currency}}\n     AND as_of = " + AS_OF
    return ("WITH a AS (\n  SELECT * FROM web_private" + at + "),\n"
            "p AS (\n  SELECT * FROM web_private" + at + "\n     AND NOT is_total"
            + _field_clauses(tags, exclude=("as_of_day",)) + ")")


def private_cards(f):
    """The Private Markets dashboard's table and multiples chart, or on
    the twin the privacy variant of its table, then its other charts
    (private_chart_cards). They read `wealthdb returns private` over
    web_private, the rows a refresh stores with their IRR at every
    quarter end and today: one row per private holding, a fund, an SPV,
    a private company's stock or a note, then the total, one row set per
    reporting currency. The Currency picker lands on the {{currency}}
    variable, the As of day on the instant read, and the Source, Asset
    class, Vehicle and Holding pickers on the holdings.

    The total is the CLI's where the pickers keep every holding. Where
    they leave some out, the table adds up what the rows it shows state
    and pools their DPI and TVPI as the CLI's total does, over the rows
    with a paid in, and for TVPI a NAV too. It leaves the IRR blank: the
    engine solves it for the whole set alone.

    The table's twin keeps the multiples, the IRR, the stated multiples
    and the marks' dates and drops the money and the account, as the
    Returns twin drops its money columns. It shows the unfunded as a
    share of the commitment, on the total of the commitments that state
    an unfunded. A holding's name stays: privacy hides money and
    accounts, never what is held. The multiples chart shows multiples
    only, so the twin shows it as it is."""
    tags = private_tags("web_private")
    stated = "stated_tvpi, stated_dpi, stated_as_of"
    if f.privacy:
        desc = "Each private holding since its first call: status, multiples, IRR, mark and unfunded share."
        cols = ("holding, silver_source_id, asset_class, vehicle, status, first_call,\n"
                "       CASE WHEN is_total THEN (SELECT 100 * sum(unfunded)\n"
                "                                  / nullif(sum(commitment) FILTER (WHERE unfunded IS NOT NULL), 0)\n"
                "                                  FROM p)\n"
                "            ELSE 100 * unfunded / nullif(commitment, 0) END AS unfunded_pct,\n"
                "       marked_at, dpi, tvpi, irr_pct,\n"
                f"       {stated}, quality")
    else:
        desc = "Each private holding since its first call: what it took and committed, gave back and is marked at."
        cols = ("holding, account_label, asset_class, vehicle, status, first_call, paid_in, commitment, unfunded,\n"
                f"       distributed, nav, marked_at, dpi, tvpi, irr_pct, {stated}, quality")
    card = f.using(tags=tags, tail=PRIVATE_PRIVACY_DESC if f.privacy else "").card
    card("Private holdings", "table",
         desc + " The total comes last. With holdings left out, it adds up the rows shown and has no IRR.",
         private_rows(tags) + ",\n"
         "t AS (\n  SELECT * FROM a WHERE is_total AND " + WHOLE + "\n"
         "  UNION ALL BY NAME\n"
         "  SELECT TRUE AS is_total, 'Total' AS holding, min(first_call) AS first_call, sum(paid_in) AS paid_in,\n"
         "         sum(commitment) AS commitment, sum(unfunded) AS unfunded,\n"
         "         sum(distributed) AS distributed, sum(nav) AS nav, min(marked_at) AS marked_at,\n"
         "         (sum(distributed) FILTER (WHERE paid_in IS NOT NULL)\n"
         "          / nullif(sum(paid_in), 0))::DOUBLE AS dpi,\n"
         "         (sum(distributed + nav) FILTER (WHERE paid_in IS NOT NULL)\n"
         "          / nullif(sum(paid_in) FILTER (WHERE nav IS NOT NULL), 0))::DOUBLE AS tvpi,\n"
         "         'filtered' AS quality\n"
         "    FROM p\n  HAVING count(*) > 0 AND NOT " + WHOLE + ")\n"
         f"SELECT {cols}\n  FROM (SELECT * FROM p UNION ALL BY NAME SELECT * FROM t)\n"
         " ORDER BY is_total, first_call, silver_source_id, holding",
         {"column_settings": {**{f'["name","{c}"]': {"decimals": 2}
                                 for c in ("dpi", "tvpi", "stated_tvpi", "stated_dpi")},
                              **_share_viz("irr_pct", *(("unfunded_pct",) if f.privacy else ()),
                                           decimals=2)["column_settings"]}})
    card("Multiples by holding", "bar",
         "What each holding gave back (DPI) and what it is still marked at, per unit paid in. Together they "
         "are its TVPI.",
         private_rows(tags) + ",\n" + HOLDING_NAMES + f"\nSELECT {HOLDING_LABEL}, dpi,\n"
         "       tvpi - dpi AS rvpi\n  FROM n\n ORDER BY tvpi DESC NULLS LAST, holding",
         {"graph.dimensions": ["holding"], "graph.metrics": list(MULTIPLE_COLORS), **STACKED_VIZ,
          "graph.x_axis.title_text": "", "graph.y_axis.title_text": "",
          "series_settings": {c: {"color": MULTIPLE_COLORS[c], "title": MULTIPLE_TITLES[c]}
                              for c in MULTIPLE_COLORS}},
         exempt=True)
    private_chart_cards(f)


def private_chart_cards(f):
    """The Private Markets charts of the gain, the vintages, the cash
    flows and the commitments. They read web_private, `wealthdb returns
    private`'s rows at the picked as-of instant, and web_private_flows,
    the legs its total counts per holding and quarter, up to the
    instant: a figure for many holdings adds their money and pools
    their multiples. Every picker but Fully funded reaches each one.
    Fully funded reaches the commitments alone. An exit in kind that names no holding is no
    holding's, so only the cash flows count it, and only while the
    pickers keep every holding, as the total does.

    A twin card shows each sum of money as a share of a total: the kept
    holdings' paid in, the calls to date at the last quarter, or the
    commitments the chart draws. A multiple and a
    count show as they are, and a holding's source stands in for its
    account."""
    tags = private_tags("web_private")
    card = f.using(tags=tags, tail=PRIVACY_DESC if f.privacy else "").card

    def total(agg, cte="p"):
        """On the twin the CTE `t`, the total a share is of: `agg` over
        `cte`."""
        return f",\nt AS (\n  SELECT {agg} AS total FROM {cte})" if f.privacy else ""

    def tail(cte="p"):
        """The FROM of a card's last SELECT, which joins `t` on the twin."""
        return f"\n  FROM {cte}" + (" CROSS JOIN t" if f.privacy else "")
    held = private_rows(tags) + total("sum(paid_in)")

    def money(expr, name, total="t.total"):
        """Money `expr` as column `name`, on the twin as `name`_pct, a
        percent of `total`: what was paid in, called or committed."""
        if f.privacy:
            return f"100 * {expr} / nullif({total}, 0) AS {name}_pct"
        return expr if expr == name else f"{expr} AS {name}"
    name = (lambda c: f"{c}_pct") if f.privacy else (lambda c: c)

    card("Gain against multiple", "scatter",
         "Each holding's gain against its TVPI on a log scale, sized by what it paid in" +
         (", both as shares of all paid in" if f.privacy else "") +
         ". No mark, no cost or a TVPI of 0 leaves it out.",
         held + "\nSELECT holding, " + ("silver_source_id AS source" if f.privacy else "account_label AS account") +
         ", status,\n       tvpi, " + money("gain", "gain") + ",\n       " + money("paid_in", "paid_in") +
         tail() + "\n WHERE gain IS NOT NULL AND tvpi > 0\n"
         " ORDER BY paid_in DESC, holding, silver_source_id, instrument_external_id",
         # The second dimension splits the dots into one series, and so
         # one colour, per status.
         {"graph.dimensions": ["tvpi", "status"], "graph.metrics": [name("gain")],
          "scatter.bubble": name("paid_in"), "graph.x_axis.scale": "log",
          **tooltip_viz("holding", "source" if f.privacy else "account", name("paid_in")),
          "series_settings": {k: {"color": c, "title": STATUS_TITLES[k]} for k, c in STATUS_COLORS.items()},
          "column_settings": {'["name","tvpi"]': {"decimals": 2},
                              **(_share_viz(name("gain"), name("paid_in"), decimals=1)["column_settings"]
                                 if f.privacy else {})}})

    card("By vintage year", "combo",
         "Per year of the first call: what came back and what is still marked, stacked, against what "
         "was paid in" + (", as shares of the total paid in." if f.privacy else "."),
         held + "\nSELECT CAST(year(first_call) AS VARCHAR) AS vintage,\n"
         "       " + money("sum(distributed)", "distributed", "any_value(t.total)") + ",\n"
         "       " + money("sum(nav)", "nav", "any_value(t.total)") + ",\n"
         "       " + money("sum(paid_in)", "paid_in", "any_value(t.total)") + ",\n"
         "       sum(distributed + nav) FILTER (WHERE paid_in IS NOT NULL)\n"
         "         / nullif(sum(paid_in) FILTER (WHERE nav IS NOT NULL), 0) AS tvpi,\n"
         "       count(*) AS holdings" + tail() + "\n GROUP BY 1\n ORDER BY 1",
         {"graph.dimensions": ["vintage"], "graph.metrics": [name(c) for c in ("distributed", "nav", "paid_in")],
          **STACKED_VIZ, "graph.x_axis.title_text": "", "graph.y_axis.title_text": "",
          **tooltip_viz("tvpi", "holdings"),
          "series_settings": {name("distributed"): {"display": "bar", "color": MULTIPLE_COLORS["dpi"],
                                                    "title": "Distributed"},
                              name("nav"): {"display": "bar", "color": MULTIPLE_COLORS["rvpi"], "title": "NAV"},
                              name("paid_in"): {"display": "line", "color": FLOW_COLORS["called"],
                                                "title": "Paid in"}},
          "column_settings": {'["name","tvpi"]': {"decimals": 2},
                              **(_share_viz(*(name(c) for c in ("distributed", "nav", "paid_in")),
                                            decimals=1)["column_settings"] if f.privacy else {})}})

    flow_tags = private_tags("web_private_flows")
    at = "\n   WHERE currency = {{currency}}\n     AND quarter <= " + AS_OF
    keys = "count(DISTINCT (silver_source_id, account_external_id, instrument_external_id))"
    card("Cash flows to date", "line",
         "Capital called, distributed and their net, added up by quarter to the as-of day. The net dips "
         "while calls come in and climbs as money comes back." +
         (" Each is a share of all capital called." if f.privacy else ""),
         "WITH a AS (\n  SELECT * FROM web_private_flows" + at + "),\n"
         "k AS (\n  SELECT * FROM web_private_flows" + at + "\n     AND NOT unattributed"
         + _field_clauses(flow_tags, exclude=("as_of_day",)) + "),\n"
         "q AS (\n  SELECT quarter, called, distributed FROM k\n  UNION ALL\n"
         "  SELECT quarter, called, distributed FROM a\n   WHERE unattributed\n"
         f"     AND (SELECT {keys} FROM k) = (SELECT {keys} FROM a WHERE NOT unattributed)),\n"
         "m AS (\n  SELECT quarter, sum(called) AS called, sum(distributed) AS distributed FROM q GROUP BY 1),\n"
         "s AS (\n  SELECT quarter, sum(called) OVER w AS called_to_date,\n"
         "         sum(distributed) OVER w AS distributed_to_date,\n"
         "         sum(distributed - called) OVER w AS net_to_date\n"
         "    FROM m\n  WINDOW w AS (ORDER BY quarter ROWS UNBOUNDED PRECEDING))" +
         total("arg_max(called_to_date, quarter)", "s") +
         "\nSELECT quarter,\n" + ",\n".join(
             f"       {money(c + '_to_date', c)}" for c in FLOW_COLORS) +
         tail("s") + "\n ORDER BY quarter",
         {"graph.dimensions": ["quarter"], "graph.metrics": [name(c) for c in FLOW_COLORS], **LINES_VIZ,
          "graph.x_axis.title_text": "", "graph.y_axis.title_text": "",
          "series_settings": {name(c): {"color": FLOW_COLORS[c], "title": FLOW_TITLES[c]} for c in FLOW_COLORS},
          **(_share_viz(*(name(c) for c in FLOW_COLORS), decimals=1) if f.privacy else {})},
         tags=flow_tags)

    # The bars (DRAWN) run by first call, a vintage year's together, and
    # an empty slot after each year but the last parts the years, so the
    # gaps and the years follow the bars that remain. The axis names the
    # year once, under its first bar; the tooltip names each bar's year
    # and holding, as a holding's name is too long for the axis. Every
    # other category is a run of zero-width spaces as long as its row
    # number, so no two merge.
    committed = private_rows(tags) + ",\n" + HOLDING_NAMES + total(
        f"sum(commitment) FILTER (WHERE {DRAWN})")
    card("Commitments", "bar",
         "Each holding's commitment and unfunded, by first call and vintage year: the part called "
         "under the part unfunded" + (", as shares of the commitments drawn." if f.privacy else ".") +
         " Fully funded shows or hides those with nothing unfunded.",
         committed + ",\nc AS (\n"
         f"  SELECT year(first_call) AS vintage, first_call, {HOLDING_LABEL},\n"
         "         " + money("(commitment - unfunded)", "called") + ",\n"
         "         " + money("unfunded", "unfunded") + "\n"
         "   " + tail("n").strip() + f"\n   WHERE {DRAWN}),\n"
         "r AS (\n"
         f"  SELECT vintage, 0 AS gap, first_call, holding, {name('called')}, {name('unfunded')} FROM c\n"
         "  UNION ALL\n"
         "  SELECT DISTINCT vintage, 1, NULL, NULL, NULL, NULL FROM c\n"
         "   WHERE vintage < (SELECT max(vintage) FROM c)),\n"
         "q AS (\n"
         "  SELECT *, row_number() OVER (PARTITION BY vintage, gap ORDER BY first_call, holding) AS k,\n"
         "         row_number() OVER (ORDER BY vintage, gap, first_call, holding) AS pos\n"
         "    FROM r)\n"
         "SELECT CASE WHEN gap = 0 AND k = 1 THEN CAST(vintage AS VARCHAR)\n"
         "            ELSE repeat(chr(8203), CAST(pos AS INTEGER)) END AS vintage,\n"
         "       CASE WHEN gap = 0 THEN CAST(q.vintage AS VARCHAR) END AS year,\n"
         f"       holding, {name('called')}, {name('unfunded')}\n"
         "  FROM q\n"
         " ORDER BY pos",
         {"graph.dimensions": ["vintage"], "graph.metrics": [name(c) for c in COMMITMENT_COLORS], **STACKED_VIZ,
          "graph.x_axis.title_text": "", "graph.y_axis.title_text": "", **tooltip_viz("year", "holding"),
          "series_settings": {name(c): {"color": COMMITMENT_COLORS[c], "title": COMMITMENT_TITLES[c]}
                              for c in COMMITMENT_COLORS},
          **(_share_viz(*(name(c) for c in COMMITMENT_COLORS), decimals=1) if f.privacy else {})},
         tags={**tags, "funded": FUNDED_TAG})

