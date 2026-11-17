"""The Spending dashboard's cards and its twin's. Part of provision.py."""

from sqlkit import (_ccy_case, _donut, LINES_VIZ, _month_cte, native_tags, _native_where, OCCURRED_MONTH,
                    PEAK_CTE, PEAK_DIV, _percent_viz, PRIVACY_DESC, range_filters, _series_order,
                    _series_viz, _share_viz, STACKED_VIZ, UNCATEGORIZED, view_tags)


def spending_cards(f):
    """The Spending dashboard's cards, or on the twin their privacy
    variants. `value` carries gold's canonical sign (spend negative,
    refunds positive), so every card charts its negation, `net_spend`,
    and a month's outflow reads as a positive bar.

    The tiles read the serving view natively. The required {{currency}}
    variable picks the value column and defaults to the configured
    currency, so a tile reads the dashboard's choice there and the
    default anywhere else. The card-balances chart reads
    web_card_balances_history, a different grain (per account per day,
    carried forward), with tags of its own.

    The twin's figures are shares and no card renders a counterparty.
    Two denominators, both recomputed in-query with the dashboard's
    pickers applied, so a narrowed selection rescales to itself. The
    breakdowns, the merchant and account lists and the largest lines
    divide by the window's own total net spend (they sum to 100); the
    trend and the monthly bands divide by the window's peak month (the
    peak reads 100). The card-balances chart takes the same shape with
    the window's deepest total owed, so its peak reads 100. Guards blank
    a degenerate window (no positive month, or a total of zero) rather
    than render inf / sign-flipped shares.

    REDACTION on top of normalization, the way the Returns twin does it:
    the fix for a leaking column is to drop it, not to disguise it. No
    twin card projects `merchant_name`, `display_name` or
    `account_external_id` — the merchant list keeps its shape by ranking
    instead, so `merchant_name` appears only as a GROUP BY key and in
    the ranking's fixed WHERE predicate, never in a projection. Nor does
    any twin card FILTER on an account: a field filter renders as a
    dropdown of its column's values, so an account filter would print
    the labels the projections just dropped (PRIVACY_SPEND_FILTERS).
    `merchant_name` is safe as a GROUP BY key and as a fixed predicate
    because neither shows anything; a filter widget does."""
    twin = f.privacy
    tags = native_tags("web_spending", PRIVACY_SPEND_FILTERS if twin else SPEND_FILTERS)
    where = _native_where(tags)
    val = f"sum({_ccy_case('value', neg=True)})::DOUBLE"
    f = f.using(tags=tags, tail=PRIVACY_DESC if twin else "")
    spend = f.card

    def month_cte(extra=""):
        return _month_cte("web_spending", where, val, extra)

    # The window-total denominator of a twin card: every bucket over the
    # summed total of the same filtered rows, so the buckets total 100.
    # Signed, so a net-refunded bucket reads negative — and the <> 0
    # guard blanks a window whose spend and refunds cancel exactly.
    def total_cte(select, group):
        """The `r` CTE: one row per bucket, with the value the shares
        divide by."""
        return ("WITH r AS (\n"
                f"  SELECT {select}{val} AS v\n"
                "    FROM web_spending" + where
                + f"\n   GROUP BY {group})\n")
    share_of_total = ("v / (SELECT CASE WHEN sum(v) <> 0 THEN sum(v) END"
                      " FROM r) * 100")

    if twin:
        spend("Spend — monthly trend", "smartscalar",
            "Net spend per month as a share of the biggest month, with the "
            "change from the month before.",
            month_cte() + "   GROUP BY 1),\n" + PEAK_CTE +
            f"SELECT month, {PEAK_DIV} AS spend_pct\n  FROM m\n ORDER BY 1", {}, home="Spending")
        # Always 100 — the window total the other percentages are
        # relative to, blank where the window nets to nothing.
        spend("Net spend", "scalar",
            "Always 100, the anchor of every other share.",
            "WITH m AS (\n"
            f"  SELECT {val} AS v FROM web_spending" + where + ")\n"
            "SELECT v / nullif(v, 0) * 100 AS spend_pct FROM m",
            {}, home="Spending")
    else:
        spend("Spend — monthly trend",
            "smartscalar",
            "Net spend in the window's latest month, with the change from the "
            "month before.",
            f"SELECT {OCCURRED_MONTH},\n       {val} AS net_spend\n"
            "  FROM web_spending" + where + "\n GROUP BY 1\n ORDER BY 1",
            {}, home="Spending")
        spend("Net spend", "scalar",
            "Purchases minus refunds over the window, across the selected "
            "accounts and categories.",
            f"SELECT {val} AS net_spend\n"
            "  FROM web_spending" + where,
            {}, home="Spending")
    # The data-quality canary, a share of ROWS: scale-free and the same in
    # every currency, so it reads no {{currency}}. The figure needs no
    # twin, but a native card carries its own field-filter tags wherever
    # it is opened, and the base card's `account` filter renders as a
    # dropdown of account labels. So the twin's is its own, over
    # PRIVACY_SPEND_FILTERS, as the income share's is.
    share_tags = view_tags("web_spending", PRIVACY_SPEND_FILTERS if twin else SPEND_FILTERS)
    spend("Uncategorized share", "scalar",
        f"Share of the window's spending lines labelled '{UNCATEGORIZED}', "
        "the backlog `wealthdb categorize` works through.",
        "SELECT count(*) FILTER (WHERE spend_primary_label = "
        f"'{UNCATEGORIZED}')::DOUBLE\n       / nullif(count(*), 0) AS share\n"
        "  FROM web_spending" + _native_where(share_tags),
        _percent_viz("share"), tags=share_tags)
    # The axes are PINNED, and this is the card that most needs it.
    # Left to infer them, Metabase puts the dimension with the most
    # distinct values on the x-axis — and there are more categories
    # than months in any window worth charting, so the card drew its
    # own transpose: a bar per CATEGORY stacked by month, under a
    # band of rotated category names deep enough to leave the bars a
    # sliver. A card called "by month" charts months. On the base card
    # only the dimensions are pinned; the metric is left to default,
    # since naming it would be guessing at what Metabase calls an
    # aggregation column.
    #
    # Stacked areas rather than bars: months are a continuous axis,
    # so the categories read as bands whose thickness moves across
    # the window, and the envelope is the window's own shape. A
    # month whose refunds beat its purchases dips its band below the
    # line.
    if twin:
        spend("Spending by month", "area",
            "Net spend per month by primary category, as a share of the window's "
            "biggest month.",
            _series_order(month_cte("spend_primary_label AS category,\n         ")
                     + "   GROUP BY 1, 2),\n" + PEAK_CTE +
                     f"SELECT month, category, {PEAK_DIV} AS spend_pct\n"
                     "  FROM m", "month", "category", "spend_pct", "total"),
            {"graph.dimensions": ["month", "category"],
             "graph.metrics": ["spend_pct"], **STACKED_VIZ})
    else:
        spend("Spending by month", "area",
            "Net spend per month, stacked by primary category.",
            _series_order(f"SELECT {OCCURRED_MONTH},\n       spend_primary_label AS category,\n"
                     f"       {val} AS net_spend\n"
                     "  FROM web_spending" + where + "\n GROUP BY 1, 2",
                     "month", "category", "net_spend", "total"),
            {"graph.dimensions": ["month", "category"], **STACKED_VIZ})
    # The two breakdowns are donuts: the question they answer is how
    # the window DIVIDES, and a ring reads that as one shape where a
    # bar chart reads it as a ranking. The ring's hole carries the
    # window's own total, so the tile answers "how much" and "of
    # what" at once, and the legend carries each slice's share.
    #
    # `_donut` is where the two settings that make that work live:
    # every slice drawn (Metabase would otherwise fold the small
    # ones into a wedge it calls "Other", which is also the name of
    # a real category here), and the shares shown in the legend
    # rather than crowded onto the ring.
    #
    # A category can go net-negative over a window whose refunds
    # beat its purchases; a ring has no way to draw that, so
    # Metabase leaves such a slice out. The figure is in the
    # transaction list either way. The hole sums what was drawn,
    # so it carries the drawn categories rather than the window —
    # the two differ by the net-negative ones, and the
    # description says which figure it is.
    #
    # The twin's rings suit a share even better than an amount, since
    # the slices are already the percentages the ring would compute.
    # Neither carries a LIMIT: a limited ring renormalizes onto what it
    # drew and would print shares disagreeing with its own values, where
    # the threshold folds the tail into a wedge and keeps the whole
    # window on the ring. No figure in their hole. The shares are of a
    # SIGNED total, so a category whose refunds beat its purchases
    # carries a negative one and a ring cannot draw it — leaving the
    # hole to sum the rest and read just over 100. The values are right
    # and total exactly 100; it is the hole that cannot represent them,
    # and a hole reading ~100 on a breakdown already expressed as shares
    # says nothing the description does not.
    for name, col, threshold, what, twin_what in (
            ("Spending by category", "spend_primary_label", 0,
             "Net spend by primary category. A category whose refunds beat "
             "its purchases has no slice and is not in the total.",
             "Primary-category shares (%) of net spend. A category whose "
             "refunds beat its purchases has no slice."),
            ("Spending by subcategory", "spend_label", 1.5,
             "Net spend by detailed category, the long tail in one slice. A "
             "net-refunded category is not in the total.",
             "Detailed-category shares (%) of net spend, the long tail in one "
             "slice. Net-refunded categories have no slice.")):
        if twin:
            spend(name, "pie", twin_what,
                total_cte(f"{col} AS category,\n         ", "1") +
                f"SELECT category, {share_of_total} AS spend_pct\n"
                "  FROM r\n ORDER BY 2 DESC, 1",
                _donut(threshold=threshold, total=False))
        else:
            spend(name, "pie", what,
                f"SELECT {col} AS category,\n       {val} AS net_spend\n"
                "  FROM web_spending" + where + "\n GROUP BY 1\n ORDER BY 2 DESC, 1",
                _donut(threshold=threshold))
    # Ranks merchants only. A line resolved to a delta — a gift, a
    # bill on a card not itemised, cash out of an ATM — is not a
    # merchant transaction, and one kind of delta line carries a
    # name: a card bill labelled with the issuer it was paid to.
    # A blank merchant therefore does not mark a delta, so the
    # ranking excludes the delta CATEGORIES outright — a delta is
    # primary-level, so `spend_primary <> spend_detailed` reads the
    # dimension's own marker rather than restating a list of values
    # — and still needs a name to rank by. A line nothing resolved
    # is kept out by its two category columns being equal at
    # '(uncategorized)'. A line the merchant store never named shows
    # its own signature, so the rows are at the signature's grain —
    # a chain appears once per branch signature, and a fold that is
    # only the bank's own booking tag ranks under that tag, since
    # the gates that keep such a fold from the model fence the
    # merchant store and not this column. Only the ranking filters:
    # the transaction list below keeps every line, since a line is a
    # line.
    #
    # The twin's merchants are ranked and unnamed: merchant_name is a
    # GROUP BY key and a WHERE predicate only, so the counterparty
    # decides the rows without ever reaching a column. The list still
    # answers the question the money tile answers — how concentrated
    # the spending is. Its shares are of the window's whole net spend,
    # the anchor the twin's scalar reads as 100, so the total is summed
    # apart from the ranked rows.
    if twin:
        spend("Top 50 merchants", "table",
            "Top fifty merchants, unnamed, as shares of net spend. A card "
            "issuer's bill is outside the ranking.",
            "WITH r AS (\n"
            f"  SELECT {val} AS v\n"
            "    FROM web_spending" + where + "\n"
            "     AND merchant_name IS NOT NULL\n"
            "     AND spend_primary_label <> spend_label\n"
            "   GROUP BY merchant_name),\n"
            "t AS (\n"
            f"  SELECT CASE WHEN {val} <> 0 THEN {val} END AS total\n"
            "    FROM web_spending" + where + ")\n"
            "SELECT row_number() OVER (ORDER BY v DESC) AS merchant_rank,\n"
            "       v / (SELECT total FROM t) * 100 AS spend_pct\n"
            "  FROM r\n ORDER BY 1\n LIMIT 50", {})
    else:
        spend("Top 50 merchants", "table",
            "Top fifty merchants by net spend in the picked currency. Delta lines, a card "
            "issuer's bill among them, and lines nothing resolved are outside the ranking.",
            f"SELECT merchant_name,\n       {val} AS net_spend\n"
            "  FROM web_spending" + where +
            "\n   AND merchant_name IS NOT NULL\n   AND spend_primary_label <> spend_label"
            "\n GROUP BY 1\n ORDER BY 2 DESC, 1\n LIMIT 50",
            {})
    if twin:
        # The label is dropped, and the rows regroup onto the two
        # dimensions the privacy dashboards already show — the source and
        # the account kind — concatenated into one bar label.
        spend("Spend by account", "row",
            "Net spend shares (%) by source and account kind, with account labels "
            "redacted.",
            total_cte("silver_source_id || ' / '\n             || COALESCE("
                      "account_kind, 'unknown') AS account_group,\n         ", "1") +
            f"SELECT account_group, {share_of_total} AS spend_pct\n"
            "  FROM r\n ORDER BY 2 DESC, 1",
            {"graph.dimensions": ["account_group"],
             "graph.metrics": ["spend_pct"]})
    else:
        spend("Spend by account", "row",
            "Net spend by account over the window, each labelled with its "
            "name, source and kind.",
            f"SELECT account_label AS account,\n       {val} AS net_spend\n"
            "  FROM web_spending" + where + "\n GROUP BY 1\n ORDER BY 2 DESC, 1",
            {})
    # Read the way an issuer states a card: what is OWED, as a
    # positive figure. Gold stores the same balance negative — a card
    # is a liability, the margin-debit precedent — and keeps doing so
    # everywhere else, the CLI included; the sign is flipped in this
    # projection and nowhere else, so the chart matches the statement
    # a reader compares it against. The twin's has no category
    # dimension and a denominator of its own — the deepest total owed
    # within the window, so the peak reads 100 and a paid-off card
    # returns to 0 — and splits by source rather than by account.
    bal_tags = native_tags("web_card_balances_history",
                           PRIVACY_CARD_BALANCE_FILTERS if twin else CARD_BALANCE_FILTERS)
    if twin:
        spend("Card balances over time", "line",
            "What the cards owed each day by source, as a share of the window's "
            "deepest total owed.",
            _series_order(
                "WITH d AS (\n"
                "  SELECT as_of_day, silver_source_id,\n"
                f"         sum({_ccy_case('balance', neg=True)})::DOUBLE AS v\n"
                "    FROM web_card_balances_history" + _native_where(bal_tags) + "\n"
                "   GROUP BY 1, 2),\n"
                "p AS (SELECT CASE WHEN max(abs(t)) > 0 THEN max(abs(t)) END AS peak\n"
                "        FROM (SELECT sum(v) AS t FROM d GROUP BY as_of_day))\n"
                "SELECT as_of_day, silver_source_id,\n"
                "       v / (SELECT peak FROM p) * 100 AS balance_pct\n"
                "  FROM d", "as_of_day", "silver_source_id", "balance_pct", "latest",
                largest_first=True),
            {**_series_viz("as_of_day", "silver_source_id", "balance_pct"), **LINES_VIZ}, tags=bal_tags)
    else:
        spend("Card balances over time", "line",
            "What each credit card owed on every day of the window, carried "
            "forward between statements. A paid-off card returns to zero.",
            _series_order(
                "SELECT as_of_day, account_label,\n"
                f"       sum({_ccy_case('balance', neg=True)}) AS owed\n"
                "  FROM web_card_balances_history"
                + _native_where(bal_tags, " ") + "\n"
                " GROUP BY 1, 2", "as_of_day", "account_label", "owed", "latest",
                largest_first=True),
            {**_series_viz("as_of_day", "account_label", "owed"), **LINES_VIZ}, tags=bal_tags)
    # The one tile that lists LINES rather than grouping them. The twin
    # drops both counterparty columns: what is left is when, from which
    # source, and what it was categorised as — each as a share of the
    # window.
    if twin:
        spend("Largest transactions", "table",
            "The fifty largest spending lines as shares of net spend, with "
            "merchant and account redacted.",
            "WITH r AS (\n"
            "  SELECT occurred_at, silver_source_id, spend_primary_label, spend_label,\n"
            f"         ({_ccy_case('value', neg=True)})::DOUBLE AS v\n"
            "    FROM web_spending" + where + "),\n"
            "t AS (SELECT CASE WHEN sum(v) <> 0 THEN sum(v) END AS total FROM r)\n"
            "SELECT occurred_at, silver_source_id, spend_primary_label, spend_label,\n"
            "       v / (SELECT total FROM t) * 100 AS spend_pct\n"
            "  FROM r\n ORDER BY v DESC, occurred_at DESC, silver_source_id, spend_primary_label, spend_label\n LIMIT 50",
            {})
    else:
        spend("Largest transactions", "table",
            "The fifty largest spending lines of the window, with merchant, "
            "account and categories. A refund sorts last.",
            "SELECT occurred_at,\n       account_label AS account,\n"
            "       merchant_name,\n       spend_primary_label AS category,\n"
            "       spend_label AS subcategory,\n"
            f"       {_ccy_case('value', neg=True)} AS net_spend\n"
            "  FROM web_spending" + where
            + "\n ORDER BY net_spend DESC, occurred_at DESC, account, merchant_name, category, subcategory\n LIMIT 50",
            {})
    fee_cards(f)


# The fee tiles' detailed groups, in stacking order (gold's fee_group),
# and their colours.
FEE_GROUP_COLORS = {"investment": "#2A78D6", "bank": "#EB6834", "atm": "#1BAF7A", "other": "#8A99A8"}

FEE_GROUP_TITLES = {"investment": "Investment", "bank": "Bank", "atm": "ATM", "other": "Other"}


def fee_cards(f):
    """The Spending dashboard's two fee tiles, or on the twin their
    privacy variants. They read
    `wealthdb spending fees` over web_fees: the
    spending lines of primary BANK_FEES, so a vehicle's fees show, and the
    daily values, each entity a portfolio, else an account outside any.
    They take the card balance tile's pickers: fees are one category, so
    a Category picker has nothing to split.

    The return is the CLI's: the entity's monthly returns chained over
    the months that end inside the window. A picked account narrows the
    fees and the value, never the return.

    On a twin the rates and returns stay, the fees read as shares of the
    window's fees, and an entity is its source, its kind and its rank, as
    the Spend by account twin names a group, with no name of its own."""
    privacy = f.privacy
    tags = native_tags("web_fees", PRIVACY_FEE_FILTERS if privacy else FEE_FILTERS)
    tail = " Fees are one category." + (PRIVACY_DESC if privacy else "")
    card = f.using(tags=tags, tail=tail).card
    val = _ccy_case("value")
    rows = ("WITH f AS (\n  SELECT * FROM web_fees" + _native_where(tags) + "),\n"
            "e AS (\n  SELECT entity_key, any_value(silver_source_id) AS silver_source_id,\n"
            "         any_value(entity_kind) AS entity_kind, any_value(entity) AS entity,\n"
            f"         sum({val}) FILTER (WHERE line = 'fee')::DOUBLE AS fees,\n" +
            "".join(f"         coalesce(sum({val}) FILTER (WHERE fee_group = '{g}'), 0)::DOUBLE AS {g},\n"
                    for g in FEE_GROUP_COLORS) +
            f"         sum({val}) FILTER (WHERE line = 'value')::DOUBLE AS value_days,\n"
            "         count(DISTINCT day) FILTER (WHERE line = 'value') AS days\n"
            "    FROM f\n   GROUP BY 1\n  HAVING count(*) FILTER (WHERE line = 'fee') > 0),\n"
            "r AS (\n  SELECT entity_key, chain_returns(twr) AS twr\n"
            "    FROM (SELECT DISTINCT entity_key, day, twr FROM f\n"
            "           WHERE line = 'return' AND currency = {{currency}})\n"
            "   GROUP BY 1),\n"
            "t AS (SELECT NULLIF(sum(fees), 0) AS total FROM e),\n"
            "n AS (\n  SELECT *, entity || ' (' || silver_source_id || ' ' || entity_kind || ')' AS entity_label,\n"
            "         silver_source_id || ' / ' || entity_kind || ' ' ||\n"
            "         row_number() OVER (PARTITION BY silver_source_id, entity_kind ORDER BY fees DESC, entity_key)"
            " AS entity_group\n"
            "    FROM e)\n")
    # A name alone can repeat across sources, so the base names an
    # entity as the account tiles name an account: name (source kind).
    label = "entity_group" if privacy else "entity"
    pick = "entity_group" if privacy else "entity_label AS entity"
    group_cols = [f"{g}_pct" if privacy else g for g in FEE_GROUP_COLORS]
    groups = (", ".join(f"100 * {g} / (SELECT total FROM t) AS {g}_pct" for g in FEE_GROUP_COLORS) if privacy
              else ", ".join(FEE_GROUP_COLORS))
    card("Fees by portfolio", "row",
         "What each portfolio, or account outside one, paid in fees, net of refunds, by group. Trading "
         "and fund costs are not fee lines.",
         rows + f"SELECT {pick}, {groups}\n  FROM n\n ORDER BY fees DESC, entity_key",
         {"graph.dimensions": [label], "graph.metrics": group_cols, "stackable.stack_type": "stacked",
          "graph.x_axis.title_text": "", "graph.y_axis.title_text": "",
          "series_settings": {c: {"color": FEE_GROUP_COLORS[g], "title": FEE_GROUP_TITLES[g]}
                              for c, g in zip(group_cols, FEE_GROUP_COLORS, strict=True)},
          **(_share_viz(*group_cols) if privacy else {})})
    rate = "fee_rate_pct(fees, value_days) AS fee_rate_pct"
    twr = "100 * r.twr AS twr_pct"
    money = ("100 * fees / (SELECT total FROM t) AS fees_pct" if privacy
             else "value_days / NULLIF(days, 0) AS mean_value")
    card("Fee rate by portfolio", "table",
         "The yearly fee rate on the value held, beside the return of the months ending in the window. "
         "Blank with no value or return.",
         rows + f"SELECT {pick}, {money},\n       {rate},\n       {twr}\n"
         "  FROM n LEFT JOIN r USING (entity_key)\n ORDER BY fees DESC, entity_key",
         _share_viz("fees_pct", "fee_rate_pct", "twr_pct", decimals=2) if privacy
         else {"column_settings": {**_share_viz("fee_rate_pct", "twr_pct", decimals=2)["column_settings"],
                                   '["name","mean_value"]': {"decimals": 0}}})


# The field filters a native spending card may carry, and the pickers
# they answer to. web_spending is the transaction grain (a category
# filter applies); web_card_balances_history is the per-account daily
# carry-forward of card balances, and web_fees the fee tiles' lines
# (fee_cards); neither has a category dimension. The privacy variants
# below are what the twin's cards actually take. The account filter binds
# account_label (PICKERS says why the readable column wins).
# The category filter binds spend_primary_label, not spend_primary: the
# Category picker lists report_spending's spend_primary, which holds the
# display label, so a filter bound to the vendored value would match
# nothing the picker offers — every tile empty.
SPEND_FILTERS = {**range_filters("occurred_at"),
                 "account": ("account_label", "string/="),
                 "category": ("spend_primary_label", "string/=")}

CARD_BALANCE_FILTERS = {**range_filters("as_of_day"),
                        "account": ("account_label", "string/=")}

FEE_FILTERS = {**range_filters("day"), "account": ("account_label", "string/=")}

# The same specs for the privacy twin, WITHOUT the account filter. A
# field filter's widget is a dropdown of the values its column takes, so
# an account filter on a privacy card offers account labels — which is
# the one thing the twin exists not to show. It is dropped rather than
# rebound: no column identifies an account without naming it, so a
# picker over some other column would be a different filter wearing the
# Account name (see PICKERS: the twin drops its Account picker for the
# same reason).
PRIVACY_SPEND_FILTERS = {k: v for k, v in SPEND_FILTERS.items()
                         if k != "account"}

PRIVACY_CARD_BALANCE_FILTERS = {k: v for k, v in CARD_BALANCE_FILTERS.items()
                                if k != "account"}

PRIVACY_FEE_FILTERS = {k: v for k, v in FEE_FILTERS.items() if k != "account"}
