"""The Private Markets dashboard's cards and its twin's. Part of provision.py."""

from sqlkit import (_cl, _field_clauses, LINES_VIZ, native_tags, PRIVACY_DESC, _share_viz, STACKED_VIZ, tooltip_viz,
                    view_tags)


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

# The pickers every card takes, beside the currency and the as-of day,
# as field filters on the holdings: the source, the asset class, the
# vehicle and the holdings left out. A holding's name is no account's,
# so the twin takes them all.
PRIVATE_FILTERS = {"source": ("silver_source_id", "string/="), "asset_class": ("asset_class", "string/="),
                   "vehicle": ("vehicle", "string/="), "holding": ("holding", "string/!=")}

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

    The table's twin keeps the multiples, the IRR and the marks' dates
    and drops the money and the account, as the Returns twin drops its money
    columns. A holding's name stays: privacy hides money and accounts,
    never what is held. The multiples chart shows multiples only, so the
    twin shows it as it is."""
    tags = private_tags("web_private")
    if f.privacy:
        desc = "Each private holding since its first call: its status, multiples, IRR and mark."
        cols = ("holding, silver_source_id, asset_class, vehicle, status, first_call, marked_at, dpi, tvpi, irr_pct, "
                "quality")
    else:
        desc = "Each private holding since its first call: what it took, gave back and is marked at."
        cols = ("holding, account_label, asset_class, vehicle, status, first_call, paid_in, distributed, nav, "
                "marked_at, dpi, tvpi, irr_pct, quality")
    card = f.using(tags=tags, tail=PRIVATE_PRIVACY_DESC if f.privacy else "").card
    card("Private holdings", "table",
         desc + " The total comes last. With holdings left out, it adds up the rows shown and has no IRR.",
         private_rows(tags) + ",\n"
         "t AS (\n  SELECT * FROM a WHERE is_total AND " + WHOLE + "\n"
         "  UNION ALL BY NAME\n"
         "  SELECT TRUE AS is_total, 'Total' AS holding, min(first_call) AS first_call, sum(paid_in) AS paid_in,\n"
         "         sum(distributed) AS distributed, sum(nav) AS nav, min(marked_at) AS marked_at,\n"
         "         (sum(distributed) FILTER (WHERE paid_in IS NOT NULL)\n"
         "          / nullif(sum(paid_in), 0))::DOUBLE AS dpi,\n"
         "         (sum(distributed + nav) FILTER (WHERE paid_in IS NOT NULL)\n"
         "          / nullif(sum(paid_in) FILTER (WHERE nav IS NOT NULL), 0))::DOUBLE AS tvpi,\n"
         "         'filtered' AS quality\n"
         "    FROM p\n  HAVING count(*) > 0 AND NOT " + WHOLE + ")\n"
         f"SELECT {cols}\n  FROM (SELECT * FROM p UNION ALL BY NAME SELECT * FROM t)\n"
         " ORDER BY is_total, first_call, silver_source_id, holding",
         {"column_settings": {'["name","dpi"]': {"decimals": 2}, '["name","tvpi"]': {"decimals": 2},
                              **_share_viz("irr_pct", decimals=2)["column_settings"]}})
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
    """The Private Markets charts of the gain, the cash flows and the
    vintages. They read web_private, `wealthdb returns private`'s rows
    at the picked as-of instant, and web_private_flows, the legs its
    total counts per holding and quarter, up to the instant: a figure
    for many holdings adds their money and pools their multiples. Every
    picker reaches each one. An exit in kind that names no holding is no
    holding's, so only the cash flows count it, and only while the
    pickers keep every holding, as the total does.

    A twin card shows each sum of money as a share of the total paid in:
    the kept holdings', or the calls to date at the last quarter. A
    multiple, a count and a share of the gain show as they are, and a
    holding's source stands in for its account."""
    tags = private_tags("web_private")
    card = f.using(tags=tags, tail=PRIVACY_DESC if f.privacy else "").card
    held = private_rows(tags) + (",\nt AS (\n  SELECT sum(paid_in) AS total FROM p)" if f.privacy else "")

    def money(expr, name, total="t.total"):
        """Money `expr` as column `name`, on the twin as `name`_pct, a
        percent of the total paid in."""
        if f.privacy:
            return f"100 * {expr} / nullif({total}, 0) AS {name}_pct"
        return expr if expr == name else f"{expr} AS {name}"
    name = (lambda c: f"{c}_pct") if f.privacy else (lambda c: c)
    tail_from = "\n  FROM p" + (" CROSS JOIN t" if f.privacy else "")

    card("Gain against multiple", "scatter",
         "Each holding's gain against its TVPI on a log scale, sized by what it paid in" +
         (", both as shares of all paid in" if f.privacy else "") +
         ". No mark, no cost or a TVPI of 0 leaves it out.",
         held + "\nSELECT holding, " + ("silver_source_id AS source" if f.privacy else "account_label AS account") +
         ", status,\n       tvpi, " + money("gain", "gain") + ",\n       " + money("paid_in", "paid_in") +
         tail_from + "\n WHERE gain IS NOT NULL AND tvpi > 0\n"
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
         "       count(*) AS holdings" + tail_from + "\n GROUP BY 1\n ORDER BY 1",
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
         (",\nt AS (\n  SELECT arg_max(called_to_date, quarter) AS total FROM s)" if f.privacy else "") +
         "\nSELECT quarter,\n" + ",\n".join(
             f"       {money(c + '_to_date', c)}" for c in FLOW_COLORS) +
         "\n  FROM s" + (" CROSS JOIN t" if f.privacy else "") + "\n ORDER BY quarter",
         {"graph.dimensions": ["quarter"], "graph.metrics": [name(c) for c in FLOW_COLORS], **LINES_VIZ,
          "graph.x_axis.title_text": "", "graph.y_axis.title_text": "",
          "series_settings": {name(c): {"color": FLOW_COLORS[c], "title": FLOW_TITLES[c]} for c in FLOW_COLORS},
          **(_share_viz(*(name(c) for c in FLOW_COLORS), decimals=1) if f.privacy else {})},
         tags=flow_tags)

    # A holding's share of the gain adds up, the largest first, to the
    # holdings' whole: how few holdings make most of it. None where the
    # holdings' gain is not positive, as a share of it means nothing
    # then.
    card("Gain by holding", "combo",
         "Each holding's gain, the largest first" + (" as a share of the total paid in" if f.privacy else "") +
         ", and the share of the holdings' gain the holdings up to it make. No mark or cost leaves a holding "
         "out.",
         held + ",\n" + HOLDING_NAMES + ",\nr AS (\n  SELECT *, sum(gain) OVER (ORDER BY gain DESC, holding, "
         "silver_source_id, instrument_external_id\n                                ROWS UNBOUNDED PRECEDING) AS gain_to,\n"
         "         sum(gain) OVER () AS gain_all\n    FROM n\n   WHERE gain IS NOT NULL)\n"
         f"SELECT {HOLDING_LABEL},\n       " + money("gain", "gain") + ",\n"
         "       CASE WHEN gain_all > 0 THEN 100 * gain_to / gain_all END AS cumulative_share_pct\n"
         "  FROM r" + (" CROSS JOIN t" if f.privacy else "") +
         "\n ORDER BY r.gain DESC, r.holding, silver_source_id, instrument_external_id",
         {"graph.dimensions": ["holding"], "graph.metrics": [name("gain"), "cumulative_share_pct"],
          "graph.x_axis.title_text": "", "graph.y_axis.title_text": "",
          "series_settings": {name("gain"): {"display": "bar", "color": MULTIPLE_COLORS["dpi"], "title": "Gain"},
                              "cumulative_share_pct": {"display": "line", "axis": "right",
                                                       "color": FLOW_COLORS["called"],
                                                       "title": "Share of the holdings' gain"}},
          **_share_viz(*([name("gain")] if f.privacy else []), "cumulative_share_pct", decimals=1)})
