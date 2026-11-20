"""The Private Markets dashboard's cards and its twin's. Part of provision.py."""

from sqlkit import (LINES_VIZ, native_tags, PRIVACY_DESC, _share_viz, STACKED_VIZ, tooltip_viz)


# The multiples the chart stacks, in stacking order, and their colours:
# what came back, then what is still marked, which add up to TVPI.
MULTIPLE_COLORS = {"dpi": "#2A78D6", "rvpi": "#8A99A8"}

MULTIPLE_TITLES = {"dpi": "Distributed (DPI)", "rvpi": "Still held (NAV / paid in)"}

PRIVATE_PRIVACY_DESC = " Money columns and account names are redacted."

# A holding's label on a chart that draws one bar per holding.
HOLDING_LABEL = "holding || ' (' || silver_source_id || ')' AS holding"

# The Gain against multiple dots: exited, all of it back, in DPI's
# colour; held, still marked, in the colour of what is.
STATUS_COLORS = {"exited": MULTIPLE_COLORS["dpi"], "held": MULTIPLE_COLORS["rvpi"]}

# The cash flows to date, in their order: what was called, what came
# back, and the net, which dips while calls come in and climbs as money
# comes back (the J-curve).
FLOW_COLORS = {"called": "#EB6834", "distributed": MULTIPLE_COLORS["dpi"], "net": "#1BAF7A"}

FLOW_TITLES = {"called": "Called", "distributed": "Distributed", "net": "Net cash flow"}


def private_cards(f):
    """The Private Markets dashboard's table and multiples chart, or on
    the twin the privacy variant of its table, then its other charts
    (private_chart_cards). They read `wealthdb returns private` over
    web_private, the rows a refresh stores with their IRR: one row per
    private holding, each a fund or SPV a capital call names, then the
    total, one row set per reporting currency. The Currency picker lands on the {{currency}} variable, and
    the dashboard takes no other: the total is the CLI's, over every
    holding, so a Source picker would leave it out of step with the rows
    shown.

    The table's twin keeps the multiples, the IRR and the marks' dates
    and drops the money and the account, as the Returns twin drops its money
    columns. A holding's name stays: privacy hides money and accounts,
    never what is held. The multiples chart shows multiples only, so the
    twin shows it as it is."""
    tags = native_tags("web_private", {})
    base = "SELECT * FROM web_private\n WHERE currency = {{currency}}"
    if f.privacy:
        desc = "Each private fund or SPV since its first call: its multiples, its IRR and its mark's date."
        cols = "holding, silver_source_id, first_call, marked_at, dpi, tvpi, irr_pct, quality"
    else:
        desc = "Each private fund or SPV since its first call: what it took, gave back and is marked at."
        cols = ("holding, account_label, first_call, paid_in, distributed, nav, marked_at, dpi, tvpi, irr_pct, "
                "quality")
    card = f.using(tags=tags, tail=PRIVATE_PRIVACY_DESC if f.privacy else "").card
    card("Private holdings", "table",
         desc + " The total comes last. A call after the mark reads TVPI low.",
         f"WITH p AS (\n  {base})\nSELECT {cols}\n  FROM p\n ORDER BY is_total, first_call, silver_source_id, holding",
         {"column_settings": {'["name","dpi"]': {"decimals": 2}, '["name","tvpi"]': {"decimals": 2},
                              **_share_viz("irr_pct", decimals=2)["column_settings"]}})
    card("Multiples by holding", "bar",
         "What each holding gave back (DPI) and what it is still marked at, per unit paid in. Together they "
         "are its TVPI.",
         f"WITH p AS (\n  {base}\n   AND NOT is_total)\n"
         f"SELECT {HOLDING_LABEL}, dpi,\n"
         "       tvpi - dpi AS rvpi\n  FROM p\n ORDER BY tvpi DESC NULLS LAST, holding",
         {"graph.dimensions": ["holding"], "graph.metrics": list(MULTIPLE_COLORS), **STACKED_VIZ,
          "graph.x_axis.title_text": "", "graph.y_axis.title_text": "",
          "series_settings": {c: {"color": MULTIPLE_COLORS[c], "title": MULTIPLE_TITLES[c]}
                              for c in MULTIPLE_COLORS}},
         exempt=True)
    private_chart_cards(f)


def private_chart_cards(f):
    """The Private Markets charts of the gain, the cash flows and the
    vintages. They read web_private, `wealthdb returns private`'s rows,
    and web_private_flows, the legs its total counts per quarter: a
    figure for many holdings adds their money and pools their multiples.
    An exit in kind that names no holding is no holding's, so only the
    cash flows count it, as the total does. Each takes the {{currency}}
    variable alone.

    A twin card shows each sum of money as a share of the total paid in:
    the total row's, or the calls to date at the last quarter. A
    multiple, a count and a share of the gain show as they are, and a
    holding's source stands in for its account."""
    tags = native_tags("web_private", {})
    card = f.using(tags=tags, tail=PRIVACY_DESC if f.privacy else "").card
    rows = "web_private\n WHERE currency = {{currency}}"
    held = ("WITH p AS (\n  SELECT * FROM " + rows + "\n   AND NOT is_total)" +
            (",\nt AS (\n  SELECT paid_in AS total FROM " + rows + "\n   AND is_total)" if f.privacy else ""))

    def money(expr, name, total="t.total"):
        """Money `expr` as column `name`, on the twin as `name`_pct, a
        percent of the total paid in."""
        if f.privacy:
            return f"100 * {expr} / nullif({total}, 0) AS {name}_pct"
        return expr if expr == name else f"{expr} AS {name}"
    name = (lambda c: f"{c}_pct") if f.privacy else (lambda c: c)
    tail_from = "\n  FROM p" + (" CROSS JOIN t" if f.privacy else "")

    card("Gain against multiple", "scatter",
         "Each holding's gain against its TVPI, sized by what it paid in" +
         (", both as shares of the total paid in" if f.privacy else "") + ". A holding with no mark is left out.",
         held + "\nSELECT holding, " + ("silver_source_id AS source" if f.privacy else "account_label AS account") +
         ",\n       CASE WHEN list_contains(string_split(quality, ';'), 'exited') THEN 'exited' ELSE 'held' END"
         " AS status,\n       tvpi, " + money("gain", "gain") + ",\n       " + money("paid_in", "paid_in") +
         tail_from + "\n WHERE gain IS NOT NULL AND tvpi IS NOT NULL\n"
         " ORDER BY paid_in DESC, holding, silver_source_id, instrument_external_id",
         # The second dimension splits the dots into one series, and so
         # one colour, for the holdings exited and those still held.
         {"graph.dimensions": ["tvpi", "status"], "graph.metrics": [name("gain")],
          "scatter.bubble": name("paid_in"),
          **tooltip_viz("holding", "source" if f.privacy else "account", name("paid_in")),
          "series_settings": {k: {"color": c} for k, c in STATUS_COLORS.items()},
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
         "       CASE WHEN bool_or(nav IS NULL) THEN NULL\n"
         "            ELSE (sum(distributed) + sum(nav)) / nullif(sum(paid_in), 0) END AS tvpi,\n"
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

    card("Cash flows to date", "line",
         "Capital called, distributed and their net, added up by quarter. The net dips while calls come "
         "in and climbs as money comes back." + (" Each is a share of all the capital called." if f.privacy else ""),
         "WITH q AS (\n  SELECT * FROM web_private_flows\n   WHERE currency = {{currency}})" +
         (",\nt AS (\n  SELECT arg_max(called_to_date, quarter) AS total FROM q)" if f.privacy else "") +
         "\nSELECT quarter,\n" + ",\n".join(
             f"       {money(c + '_to_date', c)}" for c in FLOW_COLORS) +
         "\n  FROM q" + (" CROSS JOIN t" if f.privacy else "") + "\n ORDER BY quarter",
         {"graph.dimensions": ["quarter"], "graph.metrics": [name(c) for c in FLOW_COLORS], **LINES_VIZ,
          "graph.x_axis.title_text": "", "graph.y_axis.title_text": "",
          "series_settings": {name(c): {"color": FLOW_COLORS[c], "title": FLOW_TITLES[c]} for c in FLOW_COLORS},
          **(_share_viz(*(name(c) for c in FLOW_COLORS), decimals=1) if f.privacy else {})},
         tags=native_tags("web_private_flows", {}))

    # A holding's share of the gain adds up, the largest first, to the
    # holdings' whole: how few holdings make most of it. None where the
    # holdings' gain is not positive, as a share of it means nothing
    # then.
    card("Gain by holding", "combo",
         "Each holding's gain, the largest first" + (" as a share of the total paid in" if f.privacy else "") +
         ", and the share of the holdings' gain the holdings up to it make. A holding with no mark is left out.",
         held + ",\nr AS (\n  SELECT *, sum(gain) OVER (ORDER BY gain DESC, holding, silver_source_id, "
         "instrument_external_id\n                                ROWS UNBOUNDED PRECEDING) AS gain_to,\n"
         "         sum(gain) OVER () AS gain_all\n    FROM p\n   WHERE gain IS NOT NULL)\n"
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
