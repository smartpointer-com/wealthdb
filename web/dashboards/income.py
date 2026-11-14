"""The Income dashboard's cards and its twin's. Part of provision.py."""

from sqlkit import (AFTER_THIS_MONTH, _ccy_case, currency_tag, _donut, EARNING_VALUE, HORIZON_MONTHS, Link,
                    _month_cte, native_tags, _native_where, OCCURRED_MONTH, PART_INCOME, PARTS_FROM, PEAK,
                    PEAK_CTE, _peak_cte, PEAK_DIV, _percent_viz, PRIVACY_DESC, PROJECTION_TAGS,
                    range_filters, _series_order, _share_viz, _sql_list, STACKED_VIZ, twin_name,
                    UNCATEGORIZED, view_tags)


# The uncategorised-income card, which the Income dashboard and its
# privacy twin draw identically: a share of ROWS is already
# privacy-safe, so the twin adds PRIVACY_DESC and changes nothing else.
# Both the words and the SQL live here so the two cannot drift apart.
IN_UNCATEGORIZED_DESC = (
    f"The share of the window's income lines nothing placed, labelled '{UNCATEGORIZED}', in any currency.")

IN_UNCATEGORIZED_SQL = (
    "SELECT count(*) FILTER (WHERE income_label = "
    f"'{UNCATEGORIZED}')::DOUBLE\n       / nullif(count(*), 0) AS uncategorized_share\n"
    "  FROM web_income")


def income_cards(f):
    """The Income dashboard's cards, or on the twin their privacy
    variants. Every income line's tile is native over web_income, for
    the reason the spending ones are: the serving view carries a row per
    (line, reporting currency), and a required {{currency}} variable
    picks the column rather than filtering rows. Two tiles the Spending
    dashboard has are deliberately absent — there is no second ring, the
    income taxonomy having one vendored primary and so no subcategory
    level worth one, and no balance-history chart, nothing on the income
    side being a liability. `neg` is False: gold stores a receipt
    positive. The cards of received and expected income follow
    (income_projected_cards).

    The twin's figures are shares, and no card renders a payer or an
    account. The denominators mirror the spending twin's, and for the
    same reason: the breakdowns and the lists divide by the window's own
    net income (they sum to 100), the trend by the window's peak month
    (the peak reads 100). A window with no positive month, or a total of
    zero, blanks rather than rendering inf or a sign-flipped share.
    REDACTION on top of normalization, as everywhere else in the twin:
    the fix for a leaking column is to drop it. No twin card projects
    `payer_name`, `display_name` or `account_external_id` — the payer
    list keeps its shape by RANKING instead, so `payer_name` is a GROUP
    BY key and never a projection. Nor does any twin card filter on an
    account, a field filter being a dropdown of its column's values
    (PRIVACY_INCOME_FILTERS)."""
    twin = f.privacy
    filters = PRIVACY_INCOME_FILTERS if twin else INCOME_FILTERS
    tags = native_tags("web_income", filters)
    where = _native_where(tags)
    val = f"sum({_ccy_case('value')})::DOUBLE"
    income = f.using(tags=tags, tail=PRIVACY_DESC if twin else "").card
    total_cte = ("t AS (SELECT nullif(sum(v), 0) AS total FROM m)\n")

    if twin:
        income("Income — monthly trend", "smartscalar",
            "The latest month's net income as a share of the window's biggest month, with the change.",
            _month_cte("web_income", where, val) + "   GROUP BY 1),\n" + PEAK_CTE +
            f"SELECT month, {PEAK_DIV} AS income_pct\n  FROM m\n ORDER BY 1", {})
        # The anchor every other percentage here is relative to.
        income("Net income", "scalar",
            "Always 100, the window's net income as a share of itself.",
            "WITH m AS (\n"
            f"  SELECT {val} AS v FROM web_income" + where + ")\n"
            "SELECT v / nullif(v, 0) * 100 AS income_pct FROM m",
            {})
    else:
        income("Income — monthly trend",
            "smartscalar",
            "Net income in the window's latest month, with the change from the month before.",
            f"SELECT {OCCURRED_MONTH},\n       {val} AS net_income\n"
            "  FROM web_income" + where + "\n GROUP BY 1\n ORDER BY 1",
            {})
        income("Net income", "scalar",
            "Every income line over the window, receipts less reversals, gross as booked.",
            f"SELECT {val} AS net_income\n"
            "  FROM web_income" + where,
            {})
    # The data-quality canary. A percentage, so the FIGURE needs no
    # twin — but this is a native card, and a native card carries its
    # own field-filter tags wherever it is opened, one of which is an
    # `account` filter, which renders as a dropdown of account labels.
    # So the twin builds its own over PRIVACY_INCOME_FILTERS like every
    # other card on its dashboard, and the card is not exempt.
    # The one income tile that does NOT read {{currency}}: it is a
    # share of ROWS, scale-free, and the same in every currency. It
    # therefore gets tags without the currency variable rather than
    # declaring a required one it never interpolates.
    share_tags = view_tags("web_income", filters)
    income("Uncategorized income share", "scalar",
        IN_UNCATEGORIZED_DESC,
        IN_UNCATEGORIZED_SQL + _native_where(share_tags),
        _percent_viz("uncategorized_share"), tags=share_tags)
    # No slice threshold: the income vocabulary is small enough to
    # name in full, unlike the spending one, so nothing is folded
    # into an "other" wedge a reader cannot open. The ring rule
    # still applies — a net-negative type has no slice. The twin's ring
    # carries no figure in its hole: a total of shares is 100 by
    # construction and says nothing, and the hole is where the base
    # ring puts the money the twin exists not to show.
    if twin:
        income("Income by type", "pie",
            "The window's net income by type, each as a share of it.",
            "WITH m AS (\n"
            f"  SELECT income_label AS type,\n         {val} AS v\n"
            "    FROM web_income" + where + "\n   GROUP BY 1),\n" + total_cte +
            "SELECT type, v / (SELECT total FROM t) * 100 AS income_pct\n"
            "  FROM m\n ORDER BY 2 DESC, 1",
            _donut(threshold=0, total=False))
    else:
        income("Income by type", "pie",
            "Net income by type over the window, the total in the middle. "
            "A type whose reversals beat its receipts has no slice.",
            f"SELECT income_label AS type,\n       {val} AS net_income\n"
            "  FROM web_income" + where + "\n GROUP BY 1\n ORDER BY 2 DESC, 1",
            _donut(threshold=0, total=True))
    # The only payer ranking there is: the CLI carries none, as it
    # carries no merchant ranking. Payers with no name — the delta
    # lines — are excluded rather than grouped into a blank slice. The
    # twin's are ranked and unnamed: the shape of "how concentrated is
    # this household's income" without naming anyone it comes from.
    if twin:
        income("Top 50 payers", "table",
            "The fifty payers the most income came from, ranked and unnamed, as shares of net income.",
            "WITH m AS (\n"
            f"  SELECT payer_name, {val} AS v\n"
            "    FROM web_income" + where +
            "\n     AND payer_name IS NOT NULL\n   GROUP BY 1),\n" + total_cte +
            "SELECT row_number() OVER (ORDER BY v DESC) AS rank,\n"
            "       v / (SELECT total FROM t) * 100 AS income_pct\n"
            "  FROM m\n ORDER BY 1\n LIMIT 50",
            {})
        income("Largest receipts", "table",
            "The fifty largest income lines as shares of net income, with the type but no payer or account.",
            "WITH m AS (\n"
            "  SELECT occurred_at, income_label AS type,\n"
            f"         {_ccy_case('value')} AS v\n"
            "    FROM web_income" + where + "),\n"
            "t AS (SELECT nullif(sum(v), 0) AS total FROM m)\n"
            "SELECT occurred_at, type,\n"
            "       v / (SELECT total FROM t) * 100 AS income_pct\n"
            "  FROM m\n ORDER BY v DESC, occurred_at DESC, type\n LIMIT 50",
            {})
    else:
        income("Top 50 payers", "table",
            "The fifty payers the most income came from over the window. "
            "Lines with no payer to name are left out.",
            f"SELECT payer_name,\n       {val} AS net_income\n"
            "  FROM web_income" + where +
            "\n   AND payer_name IS NOT NULL\n GROUP BY 1\n ORDER BY 2 DESC, 1\n LIMIT 50",
            {})
        income("Largest receipts", "table",
            "The fifty largest income lines of the window, with payer, account and type.",
            "SELECT occurred_at,\n       account_label AS account,\n"
            "       payer_name,\n       income_label AS type,\n"
            f"       {_ccy_case('value')} AS net_income\n"
            "  FROM web_income" + where + "\n ORDER BY net_income DESC, occurred_at DESC, account, payer_name, type\n LIMIT 50",
            {})
    income_projected_cards(f)


# The projection's income types (yield.IncomeTypes), each with its colour
# on the by-month chart: received in it, expected in a lighter shade. The
# hues are a categorical order checked for colour-blind separation of
# neighbours.
INCOME_TYPE_COLORS = {"interest": "#2a78d6", "coupons": "#eb6834", "dividends": "#1baf7a",
                      "distributions": "#eda100", "rent": "#e87ba4", "pension": "#008300"}

# The headline figures' interest, its coupons included, and dividends, a
# fund's distributions included.
INTEREST_TYPES = ("interest", "coupons")

DIVIDEND_TYPES = ("dividends", "distributions")

# Each type, by the income lines' detailed types it reads, as the
# engine's incomeTypeSQL maps a line: interest a bond pays is a coupon,
# a dividend a fund pays a distribution. So the Type picker, which lists
# the lines' labels, reaches the holdings' cards through this map, and a
# label of any other type selects nothing there.
INCOME_DETAILED_TYPES = {"interest": ("INCOME_INTEREST_EARNED",), "coupons": ("INCOME_INTEREST_EARNED",),
                         "dividends": ("INCOME_DIVIDENDS",),
                         "distributions": ("INCOME_DIVIDENDS", "INCOME_DISTRIBUTIONS"),
                         "rent": ("INCOME_RENTAL",), "pension": ("INCOME_RETIREMENT_PENSION",)}

# The detailed types the holdings' cards type, every other income line
# being one no holding pays.
PROJECTED_DETAILED = tuple(sorted({d for ds in INCOME_DETAILED_TYPES.values() for d in ds}))

# The cash flow statement's unresolved class, and its class of an
# income line no other class names.
UNCATEGORIZED_CLASS = "(uncategorized)"

OTHER_RECEIPTS = "other_receipts"

# The operating-in class of an income line no holding pays, as the cash
# flow statement classes it (cashflow_txn_nodes), and as the cash flow
# projection keeps its other income (0169): a line nothing placed,
# labelled UNCATEGORIZED, is the unresolved class, any other one other
# receipts. A test holds it to the macro's.
INCOME_LINE_CLASSES = {"earnings": ("INCOME_SALARY", "INCOME_GIG_ECONOMY", "INCOME_CONTRACTOR"),
                       "yield": ("INCOME_STAKING", "INCOME_ROYALTIES", "INCOME_ENERGY_FEED_IN"),
                       "benefits": ("INCOME_GOVERNMENT_BENEFITS", "INCOME_UNEMPLOYMENT",
                                    "INCOME_LONG_TERM_DISABILITY", "INCOME_MILITARY", "INCOME_CHILD_SUPPORT",
                                    "INCOME_ALIMONY")}

# Each class's band on the by-month chart, and its colour: received in
# it, its run-rate in a lighter shade. A greyer palette than the six
# types', so the holdings' bands stay the ones that read first.
INCOME_CLASS_BANDS = {"earnings": "salary and earnings", "yield": "other yield", "benefits": "benefits",
                      OTHER_RECEIPTS: "other receipts", UNCATEGORIZED_CLASS: "uncategorized"}

INCOME_CLASS_COLORS = {"earnings": "#6f4fd8", "yield": "#0b8aa8", "benefits": "#8c5a3c",
                       OTHER_RECEIPTS: "#6b6b6b", UNCATEGORIZED_CLASS: "#a3a3a3"}


def _line_class(col="income_detailed"):
    """The SQL of the INCOME_LINE_CLASSES class of an income line by its
    detailed type `col`."""
    return ("CASE " + " ".join(f"WHEN {col} IN ({_sql_list(ds)}) THEN '{c}'" for c, ds in INCOME_LINE_CLASSES.items())
            + f" WHEN {col} = '{UNCATEGORIZED}' THEN '{UNCATEGORIZED_CLASS}' ELSE '{OTHER_RECEIPTS}' END")


def _class_band(col):
    """The SQL of the band of class `col` (INCOME_CLASS_BANDS)."""
    return "CASE " + " ".join(f"WHEN {col} = '{c}' THEN '{b}'" for c, b in INCOME_CLASS_BANDS.items()) + " END"


# The keys the engine gives a cash balance (`cash:<ccy>`) and an
# account's income (`income:<type>:<ccy>`), which name no holding.
SHOWN_KEY_PREFIXES = ("cash:", "income:")

# A position's name on a privacy card: its own less any address
# (twin_name), else its key where that starts with one of
# SHOWN_KEY_PREFIXES. Any other key is a holding's own, which can carry a
# contract's or an account's id, so it reads (unnamed), as the issuers
# twin hides one.
TWIN_POSITION_NAME = (f"CASE WHEN name IS NOT NULL THEN {twin_name('name')} WHEN "
                      + " OR ".join(f"position_key LIKE '{k}%'" for k in SHOWN_KEY_PREFIXES)
                      + " THEN position_key ELSE '(unnamed)' END")


def _tint(color, share=0.55):
    """`color` mixed with white by `share`: a series' lighter shade."""
    rgb = [int(color[i:i + 2], 16) for i in (1, 3, 5)]
    return "#" + "".join(f"{round(c + (255 - c) * share):02x}" for c in rgb)


def income_projected_cards(f):
    """The Income dashboard's cards of received and expected income, or
    with `privacy` their privacy variants: name -> (display, description,
    dataset_query, viz). Native SQL over four sources, each a row set per
    reporting currency or a rate to it:

    - the six types the holdings receive (web_yield_received), each under
      its holding, else its account;
    - every other income line (web_income), under its account and banded
      by its operating-in class (INCOME_LINE_CLASSES);
    - what the holdings are expected to pay (web_yield_parts, under the
      projection's pickers);
    - the other income's run-rate (web_cashflow_plan's pool_income
      and outside_income), by class and account: the average of the
      income lines on every account a line arrives on.

    What was received is the Time range's: the picker reaches a card
    through the days web_income_days lists. What is expected is the
    Horizon's: the rest of the current month and the horizon's months
    after it (docs/INCOME.md §12). In the current month a run-rate expects
    its monthly figure less what its class and account received so far.
    The tax booked (web_income_withheld) names no income type, so the
    withholding and the income after it take no Type picker.

    A privacy card shows a figure as a share of the income it covers, or
    of its biggest month; the holdings' income, and a position's, as a
    yield over the value it is earned on; and what the withholding takes
    as a share. No account label, amount, quantity or price reaches it,
    and it has no account picker."""
    privacy = f.privacy
    selection = {k: v for k, v in INCOME_PROJECTED_SELECTION.items()
                 if not (privacy and k == "account")}
    sel_tags = view_tags("web_income", {"type": ("income_label", "string/=")})
    for k, (view, col) in selection.items():
        sel_tags.update(view_tags(view, {k: (col, "string/=")}))
    # The six types the labels picked map to (INCOME_DETAILED_TYPES).
    picked_types = ("SELECT m.type FROM (VALUES " + ", ".join(
        f"('{d}', '{t}')" for t, ds in INCOME_DETAILED_TYPES.items() for d in ds)
        + ") m(detailed, type)\n                    WHERE m.detailed IN"
        " (SELECT income_detailed FROM web_income WHERE {{type}})")
    projected = _sql_list(PROJECTED_DETAILED)
    # The classes the labels picked map to, a projected type none.
    picked_classes = (f"SELECT {_line_class()} FROM web_income\n                    WHERE income_detailed NOT IN"
                      f" ({projected}) AND {{{{type}}}}")

    def kept(alias="", typed=True, by_account=False):
        """The optional clauses keeping a row whose source, account, tax
        wrapper and type the pickers select. A row `typed` False has no
        type and keeps any. A row `by_account` has no tax wrapper of its
        own, and keeps its account's, defaulted as the views default
        it."""
        out = []
        for k, (view, col) in selection.items():
            pick = f"SELECT {col} FROM {view} WHERE {{{{{k}}}}}"
            if by_account and k == "tax_wrapper":
                out.append(f"({alias}silver_source_id, {alias}account_external_id) IN"
                           " (SELECT silver_source_id, account_external_id FROM accounts"
                           f" WHERE COALESCE(tax_wrapper, 'taxable_personal') IN ({pick}))")
            else:
                out.append(f"{alias}{col} IN ({pick})")
        if typed:
            out.append(f"{alias}type IN ({picked_types})")
        return "".join(f"\n     [[AND {c}]]" for c in out)

    # A card with a received figure takes the Time range, one with an
    # expected figure the projection's four, a twin's share of the whole
    # both; the calendar takes the Position picker too. The withholding
    # and the income after it take no Type (untyped): a tax row has
    # none.
    range_tags = view_tags("web_income_days", {"time_range": ("day", "date/all-options")})
    received_tags = {"currency": currency_tag(), **range_tags, **sel_tags}
    expected_tags = {"currency": currency_tag(), **PROJECTION_TAGS, **sel_tags}
    both_tags = {**received_tags, **PROJECTION_TAGS}
    untyped_tags = {k: v for k, v in both_tags.items() if k != "type"}
    calendar_tags = {**expected_tags, "position": POSITION_TAG}

    card = f.using(tags=both_tags, tail=PRIVACY_DESC if privacy else "").card

    # The days the Time range selects, every day web_income_days lists
    # with none.
    days_from = "FROM web_income_days [[WHERE {{time_range}}]]"
    days = "SELECT day " + days_from
    # A line of web_income on the days the Time range selects, every line
    # with none picked, as Net income counts them.
    line_days = "\n     [[AND CAST(occurred_at AS DATE) IN (SELECT day FROM web_income_days WHERE {{time_range}})]]"
    # The rest of the current month, as a share of its days.
    month_rest = ("(SELECT (day(last_day(max(day))) - day(max(day))) / day(last_day(max(day)))"
                  " FROM web_income_days)")
    # The current month's first day, and the lines no holding pays
    # received from it to today, by class and account, whatever the Time
    # range.
    this_month = "(SELECT CAST(date_trunc('month', max(day)) AS DATE) FROM web_income_days)"
    this_month_got = ("mg AS (\n"
                      f"  SELECT {_line_class()} AS class, silver_source_id, account_external_id,\n"
                      f"         sum({_ccy_case('value')}) AS got\n"
                      f"    FROM web_income\n   WHERE CAST(occurred_at AS DATE) >= {this_month}\n"
                      "     AND CAST(occurred_at AS DATE) <= (SELECT max(day) FROM web_income_days)\n"
                      f"     AND income_detailed NOT IN ({projected})\n"
                      "   GROUP BY ALL)")

    # Each part's income in its holding's currency and its rate to the
    # picked one, over the horizon; a payment of an estimated day reads
    # its month.
    def expected(typed=True):
        return ("ex AS (\n"
                "  SELECT p.type, p.month_start, p.month AS m, CASE WHEN NOT p.month_only THEN p.day END AS day,\n"
                f"         p.holding_currency, ({PART_INCOME}) AS amount,\n"
                f"         {_ccy_case('p.fx')} AS fx, p.withholding_pct,\n"
                "         p.basis, p.reinvested, p.silver_source_id, p.account_external_id, p.position_key,\n"
                "         p.account_label, p.vehicle, p.symbol, p.name\n" + PARTS_FROM +
                "\n   WHERE p.rule = {{maturing}}\n     AND p.month <= " + HORIZON_MONTHS + kept("p.", typed) + ")")

    def received(typed=True):
        return ("rx AS (\n  SELECT * FROM web_yield_received\n   WHERE currency = {{currency}}\n"
                f"     AND day IN ({days})" + kept(typed=typed) + ")")

    # Every other income line received, by its account and class.
    def other(typed=True):
        return ("ox AS (\n"
                f"  SELECT CAST(occurred_at AS DATE) AS day, {_line_class()} AS class,\n"
                f"         silver_source_id, account_external_id, account_label, {_ccy_case('value')} AS value\n"
                f"    FROM web_income\n   WHERE income_detailed NOT IN ({projected})" + line_days
                + kept(typed=False) + ("\n     [[AND {{type}}]]" if typed else "") + ")")

    # The other income's run-rate over the horizon's months. A salary
    # arrives on a pay day, so the current month expects the monthly
    # figure less what its class and account received so far, never
    # below 0, rather than a share of its days.
    def run_rate(typed=True):
        keep = kept(typed=False, by_account=True) + (
            f"\n     [[AND class IN ({picked_classes})]]" if typed else "")
        # An account whose wrapper changed in the look-back holds a row
        # of each part: one figure per class, account and month.
        return ("pl AS (\n"
                "  SELECT c.month, c.class, c.silver_source_id, c.account_external_id,\n"
                "         any_value(c.month_start) AS month_start, sum(c.value) AS value,\n"
                "         account_label(ac.display_name, c.account_external_id, c.silver_source_id, ac.account_kind)"
                " AS account_label\n"
                "    FROM web_cashflow_plan c\n"
                "    LEFT JOIN accounts ac ON ac.silver_source_id = c.silver_source_id"
                " AND ac.account_external_id = c.account_external_id\n"
                f"   WHERE c.part IN ({_sql_list(RUN_RATE_PARTS)})\n     AND c.currency = {{{{currency}}}}\n"
                "   GROUP BY ALL),\n"
                + this_month_got + ",\n"
                "px AS (\n"
                "  SELECT month AS m, month_start, class, silver_source_id, account_external_id, account_label, value\n"
                "    FROM pl\n   WHERE month" + AFTER_THIS_MONTH + keep + "\n"
                "  UNION ALL\n"
                f"  SELECT 0, {this_month}, class,\n"
                "         silver_source_id, account_external_id, account_label, GREATEST(0, value - coalesce(mg.got, 0))\n"
                "    FROM pl\n    LEFT JOIN mg USING (class, silver_source_id, account_external_id)\n"
                f"   WHERE month = 1\n     AND {this_month} IS NOT NULL" + keep + ")")

    keys = "silver_source_id, account_external_id, position_key, account_label, vehicle, symbol, name"
    lined = ("silver_source_id, account_external_id, 'class:' || class AS position_key, account_label,\n"
             f"           NULL AS vehicle, NULL AS symbol, {_class_band('class')} AS name")
    arms = {"expected": (expected,
                         "SELECT 'expected' AS status, type, month_start, m, day, holding_currency, amount,\n"
                         "           amount * fx AS value, amount * fx * withholding_pct / 100 AS withheld,\n"
                         f"           {keys}\n      FROM ex"),
            "received": (received,
                         "SELECT 'received' AS status, type, CAST(date_trunc('month', day) AS DATE) AS month_start,\n"
                         "           NULL AS m, day, NULL AS holding_currency, NULL AS amount, value,\n"
                         f"           NULL AS withheld, {keys}\n      FROM rx"),
            "other": (other,
                      "SELECT 'received' AS status, class AS type, CAST(date_trunc('month', day) AS DATE) AS month_start,\n"
                      "           NULL AS m, day, NULL AS holding_currency, NULL AS amount, value, NULL AS withheld,\n"
                      f"           {lined}\n      FROM ox"),
            "run_rate": (run_rate,
                         "SELECT 'expected' AS status, class AS type, month_start, m, NULL AS day,\n"
                         "           NULL AS holding_currency, NULL AS amount, value, NULL AS withheld,\n"
                         f"           {lined}\n      FROM px")}
    # The holdings' halves, every line's received, every line's expected,
    # and the four.
    holdings = ("expected", "received")
    got, due = ("received", "other"), ("expected", "run_rate")
    every = (*got, *due)

    def rows(parts, *extra, typed=True):
        """The CTEs a card reads, ending with `a`, then the `extra` ones:
        each amount of the `parts` (arms) in the picked currency, an
        expected one of a holding with what the withholding table
        withholds from it; with `typed` False, of every type."""
        ctes = [arms[x][0](typed) for x in parts]
        ctes.append("a AS (\n    " + "\n    UNION ALL\n    ".join(arms[x][1] for x in parts) + ")")
        return "WITH " + ",\n".join(ctes + list(extra)) + "\n"

    # The months the figures cover: each day received its share of its
    # month, and the horizon's months with the rest of this one.
    months = ("mo AS (\n"
              f"  SELECT (SELECT coalesce(sum(1 / day(last_day(day))), 0) {days_from}) AS received,\n"
              f"         {HORIZON_MONTHS} + coalesce({month_rest}, 0) AS expected)")
    # The tax a source withheld on the days received, which the
    # withholding table does not model.
    taxed = ("tx AS (\n  SELECT sum(" + _ccy_case("value") + ") AS withheld\n    FROM web_income_withheld\n"
             f"   WHERE day IN ({days})" + kept(typed=False, by_account=True) + ")")
    # Every income line received on those days, Net income's lines, of
    # any type: the income the tax booked was withheld from.
    every_line = ("li AS (\n  SELECT coalesce(sum(" + _ccy_case("value") + "), 0) AS income\n    FROM web_income\n"
                  "   WHERE TRUE" + line_days + kept(typed=False) + ")")
    # The biggest month, for the twins (PEAK).
    peak = _peak_cte("a", "value", "month_start").rstrip()

    # ---- the headline figures
    def tile(name, col, cond, desc, half=None, home=None):
        """The income of the rows `cond` selects: what was received over
        the Time range with `half` 'received', what is expected over the
        Horizon with 'expected', both without one. On the privacy card,
        its share of what was received, or of both."""
        pick = f" FILTER (WHERE {cond})" if cond else ""
        parts = {"received": got, "expected": due, None: every}[half]
        if privacy:
            parts, tags, of = ((got, received_tags, "all income received") if half == "received"
                               else (every, both_tags, "the income received and expected"))
            card(name, "scalar", desc + f" A share of {of}.",
                 rows(parts) + f"SELECT coalesce(100 * coalesce(sum(value){pick}, 0) / NULLIF(sum(value), 0), 0)"
                 f" AS {col}_pct\n  FROM a", _share_viz(f"{col}_pct"), tags=tags, home=home)
        else:
            tags = {"received": received_tags, "expected": expected_tags, None: both_tags}[half]
            card(name, "scalar", desc, rows(parts) + f"SELECT coalesce(sum(value){pick}, 0) AS {col}\n  FROM a",
                 {}, tags=tags, home=home)

    interest, dividends = (f"type IN ({_sql_list(t)})" for t in (INTEREST_TYPES, DIVIDEND_TYPES))
    expect = "status = 'expected'"
    if privacy:
        h = ("h AS (SELECT value, weighs FROM web_yield\n"
             "        WHERE today\n          AND currency = {{currency}}" + kept(typed=False) + ")")
        card("Holdings' income yield", "scalar",
             "The holdings' income over the Time range and the Horizon, as a yield on their value today.",
             rows(holdings, months, h) + "SELECT coalesce(100 * (SELECT sum(value) FROM a) * 12"
             " / (SELECT received + expected FROM mo)\n"
             f"       / NULLIF((SELECT {EARNING_VALUE} FROM h), 0), 0) AS income_yield_pct",
             _share_viz("income_yield_pct"))
        card("Monthly average income", "scalar", "The income per month, as a share of the biggest month.",
             rows(every, months, peak) +
             "SELECT coalesce(100 * (SELECT sum(value) FROM a) / (SELECT received + expected FROM mo)\n"
             f"       / {PEAK}, 0) AS monthly_average_pct",
             _share_viz("monthly_average_pct"))
    else:
        tile("Income in the window", "income", "",
             "Every income line received over the Time range, and the income expected over the Horizon.")
        card("Monthly average income", "scalar",
             "The income in the window over the months the Time range and the Horizon cover.",
             rows(every, months) + "SELECT coalesce(sum(value), 0) / (SELECT received + expected FROM mo)"
             " AS monthly_average\n  FROM a", {})
    tile("Income received", "received", "", "Every income line received over the Time range.", "received",
         home="Income")
    tile("Holdings' interest received", "received_interest", interest,
         "The interest and coupons the holdings received over the Time range.", "received")
    tile("Holdings' dividends received", "received_dividends", dividends,
         "The dividends and fund distributions the holdings received over the Time range.", "received")
    tile("Income expected", "expected", expect,
         "What the holdings are expected to pay over the Horizon, and the other income's run-rate.",
         "expected", home="Income")
    tile("Holdings' interest expected", "expected_interest", f"{expect} AND {interest}",
         "The interest and coupons the holdings are expected to pay over the Horizon.", "expected")
    tile("Holdings' dividends expected", "expected_dividends", f"{expect} AND {dividends}",
         "The dividends and fund distributions the holdings are expected to pay over the Horizon.", "expected")

    # What was withheld from the income received is the tax booked on the
    # income's accounts, every negative tax row there, as the CLI's
    # `withheld` memo counts it; what will be withheld from the income
    # expected is what the config's withholding table withholds at each
    # holding's situs and account wrapper, a model. A line the table names
    # no rate for withholds nothing, a run-rate neither, and with no table
    # the expected part is nothing. The tax booked is on any income line,
    # so the income it is set against is every line received (li) and
    # all income expected. Neither half takes the Type picker: a tax row
    # names no income type.
    withheld = "coalesce((SELECT withheld FROM tx), 0) + coalesce(sum(withheld), 0)"
    income = "(SELECT income FROM li) + coalesce(sum(value), 0)"
    untyped = rows(due, taxed, every_line, typed=False)
    no_type = " A tax line has no Type."
    withheld_desc = "Tax withheld, booked over the Time range and modelled over the Horizon." + no_type
    net_desc = "The income in the window less the withholding." + no_type
    if privacy:
        card("Withholding", "scalar", withheld_desc + " A share of the income.",
             untyped + f"SELECT coalesce(100 * ({withheld}) / NULLIF({income}, 0), 0) AS withheld_pct"
             "\n  FROM a", _share_viz("withheld_pct"), tags=untyped_tags)
        card("Income after withholding", "scalar", net_desc + " A share of the income.",
             untyped + f"SELECT coalesce(100 * ({income} - ({withheld})) / NULLIF({income}, 0), 0)"
             " AS net_pct\n  FROM a", _share_viz("net_pct"), tags=untyped_tags)
    else:
        card("Withholding", "scalar", withheld_desc,
             untyped + f"SELECT {withheld} AS withheld\n  FROM a", {}, tags=untyped_tags)
        card("Income after withholding", "scalar", net_desc,
             untyped + f"SELECT {income} - ({withheld}) AS net\n  FROM a", {}, tags=untyped_tags)

    # ---- received and expected by month, on one axis: a band per
    # holding's type and status, and per class of the other lines, the
    # expected one a run-rate.
    series = (f"CASE WHEN type IN ({_sql_list(INCOME_TYPE_COLORS)}) THEN type || ' (' || status || ')'\n"
              f"            ELSE {_class_band('type')} || CASE WHEN status = 'received' THEN ' (received)'"
              " ELSE ' (expected, run-rate)' END END AS series")
    colors = {**{f"{t} ({s})": {"color": c if s == "received" else _tint(c)}
                 for t, c in INCOME_TYPE_COLORS.items() for s in ("received", "expected")},
              **{f"{INCOME_CLASS_BANDS[k]} ({s})": {"color": c if s == "received" else _tint(c)}
                 for k, c in INCOME_CLASS_COLORS.items() for s in ("received", "expected, run-rate")}}
    month_desc = ("Income per month by type, received in full colour, expected lighter. A run-rate band "
                  "expects the rest of this month. It ignores the Rate path, Maturing and Shift.")
    if privacy:
        sql = (rows(every, peak) + f"SELECT CAST(month_start AS TIMESTAMP) AS month, {series},\n"
               f"       100 * sum(value) / {PEAK} AS income_pct\n  FROM a\n GROUP BY 1, 2")
        metric = "income_pct"
    else:
        sql = (rows(every) + f"SELECT CAST(month_start AS TIMESTAMP) AS month, {series},\n"
               "       sum(value) AS income\n  FROM a\n GROUP BY 1, 2")
        metric = "income"
    card("Received and expected by month", "bar", month_desc,
         _series_order(sql, "month", "series", metric, "total"),
         {"graph.dimensions": ["month", "series"], "graph.metrics": [metric], **STACKED_VIZ,
          "series_settings": colors})

    # ---- by account, by position, and the calendar
    acc = ("acc AS (\n"
           "  SELECT account_label,\n"
           "         coalesce(sum(value) FILTER (WHERE status = 'received'), 0) AS received,\n"
           "         coalesce(sum(value) FILTER (WHERE status = 'expected'), 0) AS expected,\n"
           "         coalesce(sum(value), 0) AS total\n"
           "    FROM a\n   GROUP BY 1\n  HAVING bool_or(value <> 0))")
    account_desc = "Each account's income received, expected and in all, the largest first."
    if privacy:
        card("Received and expected by account", "table",
             account_desc + " Ranked, no account named, each a share of every account's total.",
             rows(every, acc) + "SELECT row_number() OVER (ORDER BY total DESC, received DESC) AS rank,\n" +
             ",\n".join(f"       100 * {c} / NULLIF(sum(total) OVER (), 0) AS {c}_pct"
                        for c in ("received", "expected", "total")) +
             "\n  FROM acc\n ORDER BY 1",
             _share_viz("received_pct", "expected_pct", "total_pct"))
    else:
        card("Received and expected by account", "table", account_desc,
             rows(every, acc) + "SELECT account_label AS account, received, expected, total\n"
             "  FROM acc\n ORDER BY total DESC, account", {})

    # Every holding of today, the ones that pay nothing included, and any
    # held no longer that received income over the Time range; a line no
    # holding pays under its account, a row per class (`class:<class>`).
    # With a Type picked, only the holdings that receive or are expected
    # to pay it.
    paying = ("\n     [[AND (silver_source_id, account_external_id, position_key) IN"
              "\n          (SELECT silver_source_id, account_external_id, position_key FROM web_yield_parts"
              f" WHERE type IN ({picked_types})\n           UNION ALL"
              "\n           SELECT silver_source_id, account_external_id, position_key FROM web_yield_received"
              f" WHERE type IN ({picked_types}))]]")
    hold = ("hold AS (\n"
            f"  SELECT {keys}, quantity, price, value\n"
            "    FROM web_yield\n   WHERE today\n     AND currency = {{currency}}" + kept(typed=False) + paying + ")")
    inc = ("inc AS (\n  SELECT silver_source_id, account_external_id, position_key,\n"
           "         any_value(account_label) AS account_label, any_value(vehicle) AS vehicle,\n"
           "         any_value(symbol) AS symbol, any_value(name) AS name,\n"
           "         sum(value) FILTER (WHERE status = 'received') AS received,\n"
           "         sum(value) FILTER (WHERE status = 'expected') AS expected\n"
           "    FROM a\n   GROUP BY 1, 2, 3)")
    positions = ("r AS (\n"
                 "  SELECT COALESCE(h.vehicle, i.vehicle) AS vehicle,\n"
                 "         COALESCE(h.account_label, i.account_label) AS account_label,\n"
                 "         COALESCE(h.symbol, i.symbol) AS symbol, COALESCE(h.name, i.name) AS name, position_key,\n"
                 "         h.quantity, h.price, h.value,\n"
                 "         coalesce(i.received, 0) AS received, coalesce(i.expected, 0) AS expected\n"
                 "    FROM hold h\n"
                 "    FULL JOIN inc i USING (silver_source_id, account_external_id, position_key))")
    position_order = ("\n  FROM r\n ORDER BY vehicle NULLS LAST, received + expected DESC, symbol, name,"
                      " account_label, position_key")
    position_desc = ("Each position's income received and expected, by vehicle. A class row holds an "
                     "account's other lines and has no payments; click a holding's symbol or name to list its.")
    # A click on a position's symbol or name picks the position the
    # calendar below lists, by the value its cell shows; the twin's
    # calendar also matches the name the twin shows (TWIN_POSITION_NAME).
    clicks = (Link("symbol", "position"), Link("name", "position"))
    # A position's income over the value it is earned on: none for a
    # position no longer held, or of no positive value.
    on_value = "NULLIF(GREATEST(value, 0), 0)"
    if privacy:
        per_year = {"received": "(SELECT received FROM mo)", "expected": "(SELECT expected FROM mo)",
                    "total": "(SELECT received + expected FROM mo)"}
        figure = {"received": "received", "expected": "expected", "total": "(received + expected)"}
        card("Received and expected by position", "table",
             "Each position's income as a yearly yield on its value today. A class row has no yield or "
             "payments; click a holding's symbol or name to list its.",
             rows(every, months, hold, inc, positions) + f"SELECT vehicle, symbol, {TWIN_POSITION_NAME} AS name,\n"
             "       100 * value / NULLIF(sum(GREATEST(value, 0)) OVER (), 0) AS share_pct,\n" +
             ",\n".join(f"       100 * {figure[c]} * 12 / NULLIF({per_year[c]}, 0) / {on_value} AS {c}_pct"
                        for c in figure) + position_order,
             _share_viz("share_pct", "received_pct", "expected_pct", "total_pct"), cell_clicks=clicks)
    else:
        card("Received and expected by position", "table", position_desc,
             rows(every, hold, inc, positions) +
             "SELECT vehicle, account_label AS account, symbol, COALESCE(name, position_key) AS name,\n"
             "       quantity, price, value, received, expected, received + expected AS total" + position_order, {}, cell_clicks=clicks)

    # Each payment the holdings are expected to pay over the Horizon, a
    # dated one on its day, an accrual or an estimated day in its month
    # after the month's dated ones, and each principal a maturity
    # returns. A run-rate is no payment, so the calendar has none. The
    # Position picker takes a symbol, a name or a key; on the twin also
    # the name the twin shows, so a click on (unnamed) lists every
    # unnamed position's payments.
    picked = "{{position}} IN (symbol, name, position_key" + (f", {TWIN_POSITION_NAME}" if privacy else "") + ")"
    cal = ("WITH " + expected() + ",\ncal AS (\n"
           "  SELECT month_start, day, account_label, symbol, name, type, 'income' AS kind, basis, reinvested,\n"
           "         holding_currency, silver_source_id, account_external_id, position_key,\n"
           "         sum(amount) AS amount, sum(amount * fx) AS value\n"
           "    FROM ex\n   [[WHERE " + picked + "]]"
           "\n   GROUP BY ALL\n  HAVING sum(amount) <> 0\n"
           "  UNION ALL\n"
           "  SELECT CAST(date_trunc('month', day) AS DATE), day, account_label, symbol, name, type, 'principal',\n"
           "         NULL, NULL, holding_currency, silver_source_id, account_external_id, position_key,\n"
           f"         amount, amount * {_ccy_case('fx')}\n"
           "    FROM web_yield_events\n   WHERE rule = {{maturing}}\n"
           "     AND month <= " + HORIZON_MONTHS + "\n     [[AND " + picked + "]]" + kept() + ")\n")
    date = ("CASE WHEN day IS NULL THEN strftime(month_start, '%Y-%m') ELSE strftime(day, '%Y-%m-%d') END"
            " AS date")
    cal_desc = ("Each payment and principal the holdings are expected to pay over the Horizon, by date. "
                "The Position picker, or a click above, lists one position's.")
    if privacy:
        card("Expected payments", "table", cal_desc,
             cal + ",\nhold AS (SELECT silver_source_id, account_external_id, position_key, value\n"
             "           FROM web_yield WHERE today AND currency = {{currency}})\n"
             f"SELECT {date}, symbol, {TWIN_POSITION_NAME} AS name, type, kind, basis,\n"
             "       100 * c.value / NULLIF(GREATEST(h.value, 0), 0) AS value_pct\n"
             "  FROM cal c\n  LEFT JOIN hold h USING (silver_source_id, account_external_id, position_key)\n"
             " ORDER BY month_start, day IS NULL, day, symbol, name, kind, type, basis, account_external_id,"
             " position_key",
             _share_viz("value_pct"), tags=calendar_tags)
    else:
        card("Expected payments", "table", cal_desc,
             cal + f"SELECT {date}, account_label AS account, symbol, COALESCE(name, position_key) AS name,\n"
             "       type, kind, basis, holding_currency AS currency, amount, value\n"
             "  FROM cal\n ORDER BY month_start, day IS NULL, day, account, symbol, name, kind, type, basis, position_key",
             {}, tags=calendar_tags)


# The parts of web_cashflow_plan the Income cards' run-rate reads: the
# income lines on a day their account sits in the cash pool and
# every other income line. So the run-rate reads the lines the
# received half reads. The cash flow projection reads the statement's
# other_income instead.
RUN_RATE_PARTS = ("pool_income", "outside_income")

# The Income filters. `type` binds to income_label rather than to the
# primary label its spending twin uses: the income taxonomy has ONE
# vendored primary, so a primary-level picker would collapse every
# earned type behind a single "Income" value and offer the deltas
# beside it — hiding every distinction a reader opens the dashboard
# for.
INCOME_FILTERS = {**range_filters("occurred_at"),
                  "account": ("account_label", "string/="),
                  "type": ("income_label", "string/="),
                  "tax_wrapper": ("tax_wrapper", "string/=")}

# The twin's, without the account filter, for the reason the spending
# twin drops its own: a field filter renders as a dropdown of its
# column's values, and account labels are the one thing the twin exists
# not to show.
PRIVACY_INCOME_FILTERS = {k: v for k, v in INCOME_FILTERS.items()
                          if k != "account"}

# Optional: the calendar lists every position's payments until one is
# picked, by its symbol, its name or its position key.
POSITION_TAG = {"id": "position-tag", "name": "position", "display-name": "Position",
                "type": "text", "required": False}

# The Income dashboards' cards of received and expected income read
# several views each, the income received and its other lines, the tax
# withheld, the parts expected and the run-rate, and a picker reaches
# one template tag of a card. So each of its field filters is bound to
# the view whose values its picker's dropdown lists, and every view the
# card reads keeps the rows whose source, account or tax wrapper that
# view's selected rows carry (`kept` in income_projected_cards). A
# source with no income line is still a source, so the source's view is
# the sources', not the income's; the same holds for a tax wrapper,
# whose picker lists every account's. The Type binds the income lines'
# label, which `kept` maps to the holdings' types (INCOME_DETAILED_TYPES)
# and the run-rate's classes (INCOME_LINE_CLASSES). The Time range
# reaches them the same way, through the days the income received
# covers.
INCOME_PROJECTED_SELECTION = {"source": ("web_sources_latest", "silver_source_id"),
                              "account": ("web_income", "account_label"),
                              "tax_wrapper": ("web_sources_latest", "tax_wrapper")}
