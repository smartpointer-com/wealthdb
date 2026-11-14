"""The Wealth Overview dashboard's cards and its twin's. Part of provision.py."""

from sqlkit import (_ccy_case, _cl, currency_tag, _field_clauses, HOLDINGS_FILTERS, Link, native_tags,
                    _native_where, _percent_viz, PRIVACY_DESC, range_filters, SELECTION_FILTERS,
                    _series_order, _series_viz, _share_viz, STACKED_VIZ)


def overview_cards(f):
    """The Wealth Overview's own cards, or on the twin their privacy
    variants. Its other tiles are other dashboards' cards, each with
    that dashboard as its home. The cards read the serving views
    natively, for the reason the spending ones do: the Currency picker
    has to choose a value COLUMN, which only a template variable can.
    Each reads the view at the grain it charts. The year-to-date returns
    read report_returns, a row set per currency, where {{currency}}
    filters rows.

    On the twin, a ratio of sums is scale-free, and both legs see the
    dashboard's filters, so each headline figure reads as a share of the
    selected sources' latest total. The > 0 guard blanks an empty or
    under-water selection rather than rendering inf or a sign-flipped
    share. The holdings time series read as % of the total at the END of
    the selected window — the last charted day, which is today whenever
    the window is open-ended. The nw CTE re-applies both pickers: the
    time filter picks the anchor day, the source filter makes a subset
    rescale to its own anchor total — so the envelope ends at 100 at the
    window's last day and tops 100 wherever it previously peaked higher.
    The anchor day is the latest day of the view the Time range keeps,
    not of the selection; the other pickers then filter that day's rows.
    The > 0 guard blanks a selection whose anchor total is zero or
    negative — dividing would render inf/NaN resp. sign-flipped bands."""
    lat_tags = native_tags("web_sources_latest", HOLDINGS_FILTERS)
    hist_tags = native_tags("web_sources_history", HISTORY_FILTERS)
    cls_tags = native_tags("web_asset_classes_history", HISTORY_FILTERS)
    f = f.using(tail=PRIVACY_DESC if f.privacy else "")

    def latest_scalar(name, col, desc):
        """A headline figure: one value column summed over the selected
        sources' latest holdings, on the twin its share of their total."""
        f.card(name, "scalar", desc,
               f"SELECT sum({_ccy_case(col)})::DOUBLE\n"
               f"       / CASE WHEN sum({_ccy_case('total_value')}) > 0\n"
               f"              THEN sum({_ccy_case('total_value')})::DOUBLE END"
               " * 100 AS share_pct\n"
               "  FROM web_sources_latest" + _native_where(lat_tags) if f.privacy else
               f"SELECT sum({_ccy_case(col)})::DOUBLE AS {col}\n"
               "  FROM web_sources_latest" + _native_where(lat_tags), {}, tags=lat_tags)

    # Net worth = positions + cash by construction, so the three
    # headline figures reconcile exactly. They read the latest
    # holdings, like `wealthdb holdings sources`; the charts below read
    # the daily history, which agrees with them on each day.
    latest_scalar("Net worth", "total_value",
                  "Always 100, the anchor of every other share." + LATEST_DESC if f.privacy else
                  "Cash plus positions across the selected sources." + LATEST_DESC)
    latest_scalar("Positions value", "positions_value",
                  "Positions as a share of net worth." + LATEST_DESC if f.privacy else
                  "Market value of all positions." + LATEST_DESC)
    latest_scalar("Cash balance", "cash_balance",
                  "Cash as a share of net worth." + LATEST_DESC if f.privacy else
                  "Total cash balance." + LATEST_DESC)
    total = _ccy_case("total_value")
    nw_cte = (
        "WITH nw AS (\n"
        f"  SELECT CASE WHEN sum({total}) > 0\n"
        f"              THEN sum({total})::DOUBLE END AS denom\n"
        "    FROM web_sources_history\n"
        "   WHERE as_of_day = (SELECT max(as_of_day) FROM web_sources_history\n"
        "                       WHERE TRUE" + _cl("time_range") + ")"
        + _field_clauses(hist_tags, exclude=("time_range",)) + ")\n")
    if f.privacy:
        sh_where = _native_where(hist_tags, " ")
        f.card("Net worth — monthly trend", "smartscalar",
               "Average daily net worth per month as a share of net worth at the "
               "window's end.",
               nw_cte +
               "SELECT CAST(date_trunc('month', as_of_day) AS TIMESTAMP) AS month,\n"
               f"       sum({total})::DOUBLE / count(DISTINCT as_of_day)\n"
               "           / (SELECT denom FROM nw) * 100 AS avg_pct\n"
               "  FROM web_sources_history" + sh_where + "\n"
               " GROUP BY 1\n ORDER BY 1", {}, tags=hist_tags)
        f.card("Net worth over time", "area",
               "Daily net worth by source, as a share of the total at the window's "
               "end.",
               _series_order(nw_cte +
                             "SELECT as_of_day, silver_source_id,\n"
                             f"       sum({total})::DOUBLE / (SELECT denom FROM nw)"
                             " * 100 AS total_value_pct\n"
                             "  FROM web_sources_history" + sh_where + "\n"
                             " GROUP BY 1, 2", "as_of_day", "silver_source_id", "total_value_pct", "latest"),
               {**_series_viz("as_of_day", "silver_source_id", "total_value_pct"), **STACKED_VIZ},
               tags=hist_tags)
    else:
        f.card("Net worth — monthly trend", "smartscalar",
               "Average daily net worth of the latest month, with the change "
               "from the month before.",
               "SELECT CAST(date_trunc('month', as_of_day) AS TIMESTAMP) AS month,\n"
               f"       sum({_ccy_case('total_value')})::DOUBLE"
               " / count(DISTINCT as_of_day) AS net_worth\n"
               "  FROM web_sources_history" + _native_where(hist_tags) +
               "\n GROUP BY 1\n ORDER BY 1",
               {}, tags=hist_tags)
        f.card("Net worth over time", "area",
               "Daily net worth since the first snapshot, stacked by source. "
               "Values carry forward between snapshots.",
               _series_order("SELECT as_of_day, silver_source_id,\n"
                             f"       sum({_ccy_case('total_value')})::DOUBLE AS net_worth\n"
                             "  FROM web_sources_history" + _native_where(hist_tags) +
                             "\n GROUP BY 1, 2", "as_of_day", "silver_source_id", "net_worth", "latest"),
               {**_series_viz("as_of_day", "silver_source_id", "net_worth"), **STACKED_VIZ}, tags=hist_tags)
    # Shares, not money, so the twin shows the card as it is. A click
    # opens Allocation as of the day clicked.
    f.card("Asset class mix over time", "area",
           "Each day's net worth by asset class, as shares that stack to "
           "100. A class in net debt reads below zero.",
           _series_order(
               "WITH v AS (\n"
               f"  SELECT as_of_day, asset_class, sum({_ccy_case('value')})::DOUBLE AS value\n"
               "    FROM web_asset_classes_history" + _native_where(cls_tags) + "\n"
               "   GROUP BY 1, 2),\n"
               "t AS (SELECT as_of_day, sum(value) AS total FROM v GROUP BY 1)\n"
               "SELECT v.as_of_day, v.asset_class, v.value / t.total * 100 AS share_pct\n"
               "  FROM v JOIN t ON t.as_of_day = v.as_of_day\n"
               " WHERE t.total > 1e-9", "as_of_day", "asset_class", "share_pct", "latest"),
           # Stacked, not normalized: Metabase pins a normalized stack's
           # axis to 0..100%, which drops a negative band and clips the
           # bands above it. The shares already sum to 100.
           {**_series_viz("as_of_day", "asset_class", "share_pct"), **STACKED_VIZ,
            **_share_viz("share_pct")},
           tags=cls_tags, exempt=True, home="Allocation", cell_clicks=(Link("as_of_day", "as_of_day"),))
    # The returns of the current year. A click opens the Returns at the
    # start year of the figure.
    for name, method, col in (("Return year to date (TWR)", "time-weighted", "twr"),
                              ("Return year to date (MWR)", "money-weighted", "mwr")):
        f.card(name, "scalar",
               f"Whole-portfolio {method} return since 1 January of the latest year "
               "the returns cover.",
               "WITH g AS (\n"
               f"  SELECT window_from_year, {col}\n"
               "    FROM report_returns\n"
               "   WHERE grain = 'global' AND granularity = 'total'\n"
               "     AND currency = {{currency}})\n"
               f"SELECT {col}, window_from_year AS start_year\n"
               "  FROM g\n"
               " WHERE window_from_year = (SELECT max(window_from_year) FROM g)",
               {**_percent_viz(col), "scalar.field": col},
               tags={"currency": currency_tag()}, exempt=True, home="Returns",
               cell_clicks=(Link("start_year", "start_year"),))


# The Wealth Overview's history tiles, the net worth chart and the asset
# classes over time: the range pair on the history day, the tax wrapper
# and the asset class.
HISTORY_FILTERS = {**range_filters("as_of_day"), **SELECTION_FILTERS}

# The note of the headline figures, which take no Time range.
LATEST_DESC = " It reads the latest snapshot whatever the Time range."
