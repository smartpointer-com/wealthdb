"""The Gains dashboard's cards and its twin's. Part of provision.py."""

import sqlkit
from sqlkit import (ALL_SOURCES_SERIES, LINES_VIZ, native_tags, _native_where,
                    _percent_viz, PRIVACY_DESC, range_filters, _series_order, _series_viz)


def missing_basis_tag():
    """The required {{missing_basis}} text variable of the Gains cards:
    ignore leaves a gain that needs a missing cost basis out, zero counts
    the missing cost as 0. Built on call, like currency_tag."""
    return {"id": "mb-tag", "name": "missing_basis", "display-name": "Missing cost basis",
            "type": "text", "default": sqlkit.DEFAULT_MISSING_BASIS, "required": True}


def gains_cards(f):
    """The Gains dashboard's cards: native SQL over web_gains, the
    monthly gains rows materialized per reporting currency. The view
    carries a row set per currency, so {{currency}} is a row filter
    here, where the other families pick a value column with it. A
    figure over the window sums the months in it; a figure at the
    window's end reads its last month. docs/GAINS.md defines the
    figures and the quality counters. It carries a row set per reading
    of a missing cost basis too, and {{missing_basis}} picks one the same
    way (docs/GAINS.md §8).

    On a twin, the privacy variants of the two headline gains the
    Wealth Overview's twin shows: each gain as a ratio of the cost basis
    it is a gain on. Gains has no twin of its own (NO_PRIVACY_TWIN)."""
    tags = {**native_tags("web_gains", GAINS_FILTERS), "missing_basis": missing_basis_tag()}
    native = f.using(tags=tags, tail=PRIVACY_DESC if f.privacy else "").card
    ccy = "\n     AND currency = {{currency}}\n     AND missing_basis = {{missing_basis}}"
    where = _native_where(tags) + ccy
    # The whole-portfolio line answers every picker but Source, so
    # narrowing the sources keeps the whole in view.
    where_all = _native_where(tags, exclude=("source",)) + ccy
    # The window's rows, and its end: the positions held at the last
    # month in range.
    end = ("WITH w AS (\n  SELECT * FROM web_gains" + where + "),\n"
           "e AS (SELECT * FROM w\n"
           "       WHERE at_end AND period = (SELECT max(period) FROM w))\n")
    # The window's gain as the price move of what was sold and of what
    # was kept, a sale counted from its month's start (web_gains); the
    # total is their sum, as it is realized plus unrealized change.
    gain_split = [("sold_gain", "sold"), ("held_change", "held")]
    gain = " + ".join(f"coalesce(sum({c}), 0)" for c, _ in gain_split)
    position = "coalesce(symbol, name, instrument_key)"

    if f.privacy:
        native("Realized gain", "scalar",
            "The gain or loss on sales over the window, as a percent of the "
            "cost basis sold.",
            "SELECT sum(realized) FILTER (WHERE realized_cost_basis > 0)\n"
            "       / nullif(sum(realized_cost_basis) FILTER (WHERE realized IS NOT NULL\n"
            "                                                 AND realized_cost_basis > 0), 0)"
            " AS realized_pct\n"
            "  FROM web_gains" + where, _percent_viz("realized_pct"), home="Gains")
        native("Unrealized gain", "scalar",
            "The gain or loss held at the window's end, as a percent of the "
            "cost basis held.",
            end + "SELECT sum(unrealized_end) FILTER (WHERE cost_basis > 0)\n"
            "       / nullif(sum(cost_basis) FILTER (WHERE unrealized_end IS NOT NULL\n"
            "                                        AND cost_basis > 0), 0) AS unrealized_pct\n"
            "  FROM e", _percent_viz("unrealized_pct"), home="Gains")
        return

    def unrealized_table(name, desc, pred, order):
        """Ten positions held at the window's end, by unrealized gain or
        by its percent of the cost basis."""
        native(name, "table", desc,
                      end + f"SELECT {position} AS position,\n"
                      "       unrealized_end AS unrealized,\n"
                      "       unrealized_end / nullif(cost_basis, 0) AS unrealized_pct,\n"
                      "       cost_basis,\n       value,\n       account\n"
                      f"  FROM e\n WHERE {pred}\n ORDER BY {order}, position, account\n LIMIT 10",
                      _percent_viz("unrealized_pct"))

    def realized_table(name, desc, pred, order):
        """Ten positions by what their sales over the window realized,
        or by its percent of the cost basis sold."""
        native(name, "table", desc,
                      "WITH r AS (\n"
                      f"  SELECT any_value({position}) AS position,\n"
                      "         any_value(account) AS account,\n"
                      "         sum(proceeds) AS proceeds,\n"
                      "         sum(realized_cost_basis) AS cost_basis,\n"
                      "         sum(realized) AS realized\n"
                      "    FROM web_gains" + where +
                      "\n     AND realized IS NOT NULL\n"
                      "   GROUP BY silver_source_id, account_external_id, instrument_key)\n"
                      "SELECT position, realized,\n"
                      "       realized / nullif(cost_basis, 0) AS realized_pct,\n"
                      "       proceeds, cost_basis, account\n"
                      f"  FROM r\n WHERE {pred}\n ORDER BY {order}, position, account\n LIMIT 10",
                      _percent_viz("realized_pct"))

    def split_bars(name, desc, dim, label, metrics):
        """Stacked bars per value of SQL expression `dim`, one series per
        metric, largest total first; a value none of the metrics has is
        left out."""
        cols = ",\n".join(f"       sum({c}) AS {a}" for c, a in metrics)
        total = " + ".join(f"coalesce(sum({c}), 0)" for c, _ in metrics)
        some = " OR ".join(f"count({c}) > 0" for c, _ in metrics)
        native(name, "bar", desc,
                      f"SELECT {dim} AS {label},\n{cols}\n"
                      "  FROM web_gains" + where +
                      f"\n GROUP BY 1\nHAVING {some}\n ORDER BY {total} DESC, 1",
                      {"graph.dimensions": [label],
                       "graph.metrics": [a for _, a in metrics],
                       "stackable.stack_type": "stacked"})

    term_split = [("realized_short", "short_term"),
                  ("realized_long", "long_term"),
                  ("realized_other", "term_unstated")]
    native("Realized gain", "scalar",
        "The gain or loss on what was sold over the window. The coverage "
        "table counts the sales it leaves out.",
        "SELECT sum(realized) AS realized\n  FROM web_gains" + where, {}, home="Gains")
    native("Unrealized gain", "scalar",
        "The gain or loss on what is held at the window's end, over the "
        "holdings with a cost basis.",
        end + "SELECT sum(unrealized_end) AS unrealized\n  FROM e", {}, home="Gains")
    native("Gain on positions sold", "scalar",
        "What the positions sold over the window gained in their month of "
        "sale, from the month's start or their purchase.",
        "SELECT sum(sold_gain) AS sold\n  FROM web_gains" + where, {})
    native("Gain on positions held", "scalar",
        "How far the unrealized gain of the positions kept moved over the "
        "window, exchange-rate moves included.",
        "SELECT sum(held_change) AS held\n  FROM web_gains" + where, {})
    native("Total gain", "scalar",
        "The gain on positions sold plus the gain on positions held. "
        "Income, fees and taxes are not in it.",
        f"SELECT {gain} AS gain\n  FROM web_gains" + where, {})
    native("Cost basis coverage", "scalar",
        "Share of the value held at the window's end that carries a cost "
        "basis. Unrealized figures leave out the rest.",
        end + "SELECT sum(abs(value)) FILTER (WHERE NOT end_without_basis)\n"
        "       / nullif(sum(abs(value)), 0) AS basis_coverage\n"
        "  FROM e\n WHERE end_applies",
        _percent_viz("basis_coverage"))
    native("Gains by month", "combo",
        "Each month's gain on positions sold and held as stacked bars, "
        "with their sum as a line.",
        "SELECT period AS month,\n" + ",\n".join(
            f"       sum({c}) AS {a}" for c, a in gain_split) +
        f",\n       {gain} AS gain\n"
        "  FROM web_gains" + where + "\n GROUP BY 1\n ORDER BY 1",
        {"graph.dimensions": ["month"],
         "graph.metrics": [a for _, a in gain_split] + ["gain"],
         "series_settings": {**{a: {"display": "bar"} for _, a in gain_split},
                             "gain": {"display": "line"}},
         "stackable.stack_type": "stacked"})
    native(
        "Realized gains by month, short vs long term", "bar",
        "Each month's realized gain as short term, long term, or with no "
        "term stated.",
        "SELECT period AS month,\n" + ",\n".join(
            f"       sum({c}) AS {a}" for c, a in term_split) +
        "\n  FROM web_gains" + where + "\n     AND realized IS NOT NULL"
        "\n GROUP BY 1\n ORDER BY 1",
        {"graph.dimensions": ["month"],
         "graph.metrics": [a for _, a in term_split],
         "stackable.stack_type": "stacked"})
    native("Unrealized gain over time", "line",
        "The unrealized gain at each month's end, per source and for "
        "'(all sources)', which the Source picker does not narrow.",
        _series_order(
            "SELECT period AS month, silver_source_id AS source,\n"
            "       sum(unrealized_end) AS unrealized\n"
            "  FROM web_gains" + where + "\n     AND at_end\n GROUP BY 1, 2\n"
            "UNION ALL\n"
            f"SELECT period, '{ALL_SOURCES_SERIES}', sum(unrealized_end)\n"
            "  FROM web_gains" + where_all + "\n     AND at_end\n GROUP BY 1",
            "month", "source", "unrealized", "latest", largest_first=True),
        {**_series_viz("month", "source", "unrealized"), **LINES_VIZ})
    unrealized_table("Largest unrealized gains",
        "The ten positions held at the window's end with the largest "
        "unrealized gain.",
        "unrealized_end > 0", "unrealized DESC")
    unrealized_table("Largest unrealized losses",
        "The ten positions held at the window's end with the largest "
        "unrealized loss.",
        "unrealized_end < 0", "unrealized")
    unrealized_table("Best unrealized %",
        "The ten positions held at the window's end with the largest "
        "unrealized gain as a share of their cost basis.",
        "unrealized_end > 0 AND cost_basis > 0", "unrealized_pct DESC")
    unrealized_table("Worst unrealized %",
        "The ten positions held at the window's end with the largest "
        "unrealized loss as a share of their cost basis.",
        "unrealized_end < 0 AND cost_basis > 0", "unrealized_pct")
    realized_table("Largest realized gains",
        "The ten positions whose sales over the window realized the "
        "largest gain. The cost basis is that of the lots sold.",
        "realized > 0", "realized DESC")
    realized_table("Largest realized losses",
        "The ten positions whose sales over the window realized the "
        "largest loss. The cost basis is that of the lots sold.",
        "realized < 0", "realized")
    realized_table("Best realized %",
        "The ten positions whose sales over the window realized the "
        "largest gain as a share of the cost basis sold.",
        "realized > 0 AND cost_basis > 0", "realized_pct DESC")
    realized_table("Worst realized %",
        "The ten positions whose sales over the window realized the "
        "largest loss as a share of the cost basis sold.",
        "realized < 0 AND cost_basis > 0", "realized_pct")
    split_bars("Gain by source",
        "Each source's gain on positions sold and held over the window, "
        "stacked.",
        "silver_source_id", "source", gain_split)
    split_bars("Gain by asset class",
        "Each asset class's gain on positions sold and held over the "
        "window, stacked. 'unknown' holds sales no snapshot classes.",
        "asset_class", "asset_class", gain_split)
    split_bars(
        "Realized gains by tax wrapper",
        "Realized gain over the window per tax wrapper, by holding "
        "period.",
        "tax_wrapper", "tax_wrapper", term_split)
    native("Realized gains by tax year", "table",
        "Per calendar year, realized gain by holding period, wash sales, "
        "proceeds, cost basis sold and uncovered sells. Not a tax return.",
        "SELECT CAST(year(period) AS VARCHAR) AS tax_year,\n"
        "       sum(realized) AS realized,\n" + ",\n".join(
            f"       sum({c}) AS {a}" for c, a in term_split) + ",\n"
        "       sum(wash_disallowed) AS wash_disallowed,\n"
        "       sum(proceeds) AS proceeds,\n"
        "       sum(realized_cost_basis) AS cost_basis,\n"
        "       sum(realized_lots) AS realized_lots,\n"
        "       sum(sells_without_documents) AS sells_without_documents\n"
        "  FROM web_gains" + where + "\n GROUP BY 1\n ORDER BY 1 DESC",
        {})
    native("Unrealized gain vs. percent",
        "scatter",
        "Each position held at the window's end, its unrealized gain "
        "against its percent of cost basis, sized by value.",
        end + f"SELECT {position} AS position,\n"
        "       asset_class,\n"
        "       unrealized_end / cost_basis AS unrealized_pct,\n"
        "       unrealized_end AS unrealized,\n"
        "       abs(value) AS value\n"
        "  FROM e\n WHERE unrealized_end IS NOT NULL AND cost_basis > 0",
        # The second dimension splits the dots into one series, and
        # so one colour, per asset class.
        {"graph.dimensions": ["unrealized_pct", "asset_class"],
         "graph.metrics": ["unrealized"],
         "scatter.bubble": "value",
         **_percent_viz("unrealized_pct")})
    native("Positions in the green", "scalar",
        "Positions held at the window's end with an unrealized gain.",
        end + "SELECT count(*) AS positions\n  FROM e\n WHERE unrealized_end > 0",
        {})
    native("Positions in the red", "scalar",
        "Positions held at the window's end with an unrealized loss.",
        end + "SELECT count(*) AS positions\n  FROM e\n WHERE unrealized_end < 0",
        {})
    native("Coverage by source", "table",
        "Per source, the value held at the window's end, the part with a "
        "cost basis, and counts of gains missing or misplaced.",
        end + ", t AS (\n"
        "  SELECT silver_source_id,\n"
        "         sum(sells) AS sells,\n"
        "         sum(sells_without_documents) AS sells_without_documents,\n"
        "         sum(sells_rebuilt) AS sells_rebuilt,\n"
        "         sum(realized_lots) AS realized_lots,\n"
        "         sum(lots_without_gain) AS lots_without_gain,\n"
        "         sum(in_kind_moves) AS in_kind_moves,\n"
        "         sum(corporate_actions) AS corporate_actions,\n"
        "         count(*) FILTER (WHERE basis_changed) AS basis_changed,\n"
        "         sum(seed_lots) AS seed_lots,\n"
        "         sum(basis_assumed_zero) AS basis_assumed_zero,\n"
        "         sum(fx_missing) AS fx_missing\n"
        "    FROM w GROUP BY 1),\n"
        "v AS (\n"
        "  SELECT silver_source_id,\n"
        "         sum(value) AS value,\n"
        "         sum(value) FILTER (WHERE NOT end_without_basis) AS value_with_basis,\n"
        "         sum(abs(value)) FILTER (WHERE NOT end_without_basis)\n"
        "             / nullif(sum(abs(value)), 0) AS basis_coverage\n"
        "    FROM e WHERE end_applies GROUP BY 1)\n"
        "SELECT silver_source_id AS source, v.value, v.value_with_basis,\n"
        "       v.basis_coverage, t.sells, t.sells_without_documents,\n"
        "       t.sells_rebuilt, t.realized_lots, t.lots_without_gain,\n"
        "       t.in_kind_moves, t.corporate_actions, t.basis_changed,\n"
        "       t.seed_lots, t.basis_assumed_zero, t.fx_missing\n"
        "  FROM t FULL JOIN v USING (silver_source_id)\n ORDER BY 1",
        _percent_viz("basis_coverage"))


# The Gains filters. The tax wrapper is the one of its own: a realized
# gain in a taxable account is a tax bill, in a retirement account it is
# not. The asset class is each position's, as Exposure and Yield narrow
# by it. A sale nothing classes reads `unknown`.
# No account filter: the tables name the accounts already.
GAINS_FILTERS = {**range_filters("period"),
                 "tax_wrapper": ("tax_wrapper", "string/="),
                 "asset_class": ("asset_class", "string/=")}
