"""The pre-defined dashboards: every family of cards, the dashboards
that show them, and each dashboard's pickers and layout. Part of
provision.py.

Like the report models, every card and dashboard is a content-free
definition — native SQL over the gold serving views, tables and macros;
no source data is baked in. Provisioning converges these to spec on
every start, so a user who wants to customize one should duplicate it
into another collection first."""

from sqlkit import (Family, privacy_name, PRIVACY_SUFFIX)
from pickers import (picker_payload, PICKERS)
from layout import (Dashboard, Heading, Layout, layout, PREDEFINED_NOTE, Row, Stack, Twin)
from dashboards.overview import (overview_cards)
from dashboards.allocation import (allocation_cards)
from dashboards.spending import (spending_cards)
from dashboards.income import (income_cards)
from dashboards.cashflow import (cashflow_cards)
from dashboards.returns import (returns_cards)
from dashboards.freshness import (freshness_cards)
from dashboards.gains import (gains_cards)
from dashboards.exposure import (exposure_cards)
from dashboards.yields import (yield_cards)


# Every family of cards, in the order their cards are saved.
FAMILIES = (overview_cards, allocation_cards, spending_cards, income_cards, cashflow_cards, returns_cards,
            freshness_cards, gains_cards, exposure_cards, yield_cards)


def card_defs():
    """card name -> Card for every card provisioning defines: each
    family's base cards, then each one's twin cards. A tile that sums
    money in the dashboard's chosen currency is native SQL over a gold
    serving view: a picker selects rows and never a column, so the
    required {{currency}} variable picks the value column with a CASE,
    and the other pickers land as field filters. The returns charts are
    native too, for their window functions and pseudo-source UNION; a
    card over report_returns, which carries a row set per currency,
    takes the Currency picker as a field filter on its rows.

    Every dashboard but those in NO_PRIVACY_TWIN has a privacy twin whose
    cards show shares (%) instead of money, each recomputing its
    denominator in-query with the dashboard's filters applied and naming
    it (PRIVACY_DESC), or redact the money columns where the figures are
    ratios already (Returns). A card that shows no monetary value
    (percentages, indices, days, source names) is exempt: the twin shows
    it as it is."""
    cards = {}
    for privacy in (False, True):
        for family in FAMILIES:
            family(Family(privacy, cards))
    return cards


# The cards that exist only beside a benchmark (Card.hide_empty), by
# base name: demo/check_dashboards.py expects them empty at the defaults.
BENCHMARK_CARDS = frozenset(n for n, c in card_defs().items() if c.hide_empty and not n.endswith(PRIVACY_SUFFIX))

# Every pre-defined dashboard, in the order they are sent. A tile's card
# takes each picker its template tags name (tile_mappings). Data Freshness is deliberately
# unfiltered — its job is to show every source, especially the stale
# ones a time filter would hide.
DASHBOARDS = (
    Dashboard(
        "Wealth Overview",
        "Net worth and its asset classes over time, then key figures from "
        "the other dashboards. Each figure opens the dashboard it comes from. "
        + PREDEFINED_NOTE,
        "Net worth figures are shares (%) of the selected sources' total at "
        "the window's end. Other figures are their dashboard's privacy "
        "figures, gains a percent of cost basis. " + PREDEFINED_NOTE,
        ("currency", "time_range", "source", "tax_wrapper", "asset_class"), (), (
            # Net worth = positions + cash by construction — the first
            # three tiles reconcile exactly; the trend tile is a monthly
            # AVERAGE, so it intentionally differs from today's value.
            # The stalest source sits beside them: the warning to read
            # before trusting any figure.
            Row(3, (("Net worth", 4), ("Positions value", 5), ("Cash balance", 5),
                    ("Net worth — monthly trend", 5), ("Stalest source (days)", 5))),
            # The net worth chart, then its mix by asset class.
            Row(8, (("Net worth over time", 24),)),
            Row(8, (("Asset class mix over time", 24),)),
            # Under two headings that say what sets each row of figures:
            # the figures over the time range, then those at its end,
            # today and ahead, three to a row so each title reads whole.
            Heading("Over the time range"),
            # On the twin, received income and net spend read 100 by
            # construction: each is the total its own twin divides by.
            # The twin leaves them out, and the other four figures of
            # their rows sit two to a row.
            Twin((Row(3, (("Income received", 8), ("Net spend", 8), ("Spend — monthly trend", 8))),
                  Row(3, (("Net cash flow", 8), ("Savings rate", 8), ("Realized gain", 8)))),
                 (Row(3, (("Spend — monthly trend", 12), ("Net cash flow", 12))),
                  Row(3, (("Savings rate", 12), ("Realized gain", 12))))),
            Heading("At the range's end, today and ahead"),
            Row(3, (("Unrealized gain", 8), ("Return year to date (TWR)", 8),
                    ("Return year to date (MWR)", 8))),
            Row(3, (("Income yield", 8), ("Income expected", 8), ("Projected net cash flow", 8))),
        )),
    Dashboard(
        "Allocation",
        "Where the value sits as of the chosen day, by asset class, vehicle, "
        "currency, tax wrapper, management style, position and issuer. Cash "
        "is a position in every breakdown. " + PREDEFINED_NOTE,
        "Every breakdown is a share (%) of the selected sources' total "
        "on the chosen day, summing to 100. Absolute amounts never show. " + PREDEFINED_NOTE,
        ("currency", "as_of_day", "source", "tax_wrapper", "asset_class", "vehicle", "benchmark"), (), (
            # The two taxonomy dimensions side by side on the top row.
            Row(8, (("Allocation by asset class", 12), ("Allocation by vehicle", 12))),
            Row(8, (("Allocation by currency", 12), ("Value by tax wrapper", 12))),
            Row(6, (("Value by management style", 24),)),
            Row(8, (("Top 100 positions", 24),)),
            Row(9, (("Top 100 issuers", 24),)),
        )),
    Dashboard(
        "Returns",
        "Time-weighted (TWR) and money-weighted (MWR) returns, per period "
        "and per source. Start year 0 means since inception, where TWR is "
        "often n/a. " + PREDEFINED_NOTE,
        "Returns are scale-free and show as they are. The money columns "
        "and the account and portfolio grains are redacted. " + PREDEFINED_NOTE,
        ("currency", "start_year", "source", "benchmark"), (), (
            # Scalars rescope to the since-<start year> window; charts split
            # by source with the global grain as a toggleable '(all sources)'
            # line. MWR is a summary figure only (the per-period buckets
            # carry TWR); that is engine behavior the scalars mirror.
            Row(3, (("Return (TWR)", 8), ("Return (MWR)", 8), ("Annualized return (TWR)", 8))),
            # Beside a benchmark alone (Card.hide_empty), the figures above them
            # again in their order.
            Row(3, (("Benchmark return (TWR)", 8), ("Benchmark return (MWR)", 8),
                    ("Benchmark annualized return (TWR)", 8))),
            Row(3, (("Excess return (TWR)", 8), ("Excess return (MWR)", 8),
                    ("Excess annualized return (TWR)", 8))),
            Row(8, (("Cumulative return (TWR)", 24),)),
            Row(6, (("Monthly returns (TWR)", 8), ("Quarterly returns (TWR)", 8),
                    ("Annual returns (TWR)", 8))),
            Row(8, (("Returns by source", 24),)),
            Row(8, (("Money-weighted return over time", 24),)),
            Row(8, (("Value against the benchmark", 24),)),
        ), redacts=True),
    Dashboard(
        "Spending",
        "Where the money goes over the window, by month, category, merchant "
        "and account, what each portfolio paid in fees, and what the cards "
        "owe. Own-account moves are not spend. " + PREDEFINED_NOTE,
        "Values are shares (%) of the window's net spend, of its biggest "
        "month or of its fees. Merchant and account labels are redacted. " + PREDEFINED_NOTE,
        ("currency", "time_range", "source", "account", "category"), ("account",), (
            # The three headline figures, then the shape of the window
            # (months x category), then the two breakdown levels side by
            # side, the detailed tile holding the same window one level
            # down. Merchants and accounts answer "to whom" and "from
            # where", the fee tiles what each portfolio paid (web_fees);
            # the card-balances chart reads web_card_balances_history,
            # and the transaction list is the bottom of the drill-down.
            Row(3, (("Spend — monthly trend", 8), ("Net spend", 8), ("Uncategorized share", 8))),
            Row(9, (("Spending by month", 24),)),
            Row(8, (("Spending by category", 12), ("Spending by subcategory", 12))),
            Row(8, (("Top 50 merchants", 12), ("Spend by account", 12))),
            Row(8, (("Fees by portfolio", 10), ("Fee rate by portfolio", 14))),
            Row(6, (("Card balances over time", 24),)),
            Row(8, (("Largest transactions", 24),)),
        )),
    Dashboard(
        "Income",
        "Every income line received, wages included, and the income expected, the "
        "holdings' payments with a run-rate of the rest. " + PREDEFINED_NOTE,
        "Income as shares of the window's income, the holdings' income as a yield on their "
        "value. No payer or account is named. " + PREDEFINED_NOTE,
        # The Horizon sits beside the Time range: the one sets every
        # expected figure as the other sets every received one.
        ("currency", "time_range", "horizon", "source", "account", "type", "tax_wrapper", "position",
         "rates", "maturing", "shift"), ("account",), (
            # The headline figures first, then the charts, then the
            # tables: what was received and is expected, beside the
            # income lines' net income and backlog; the trend and the
            # withholding; both halves by month, then their interest and
            # dividends; the types beside the payers; then by account
            # and by position, the calendar a position's row picks, and
            # the largest receipts. The income in the window has no share
            # worth showing, so the twin shows the holdings' income as a
            # yield on their value there, and names it so.
            Row(3, (("Income in the window", 4, "Holdings' income yield"), ("Income received", 4),
                    ("Income expected", 4), ("Net income", 4), ("Monthly average income", 4),
                    ("Uncategorized income share", 4))),
            Row(3, (("Income — monthly trend", 8), ("Withholding", 8), ("Income after withholding", 8))),
            # Which bands of the chart below are a run-rate rather than a
            # payment.
            Heading("Every income line. A band marked run-rate is an account's average of past months; this "
                    "month it expects only what has not arrived.", "text"),
            Row(9, (("Received and expected by month", 24),)),
            Row(3, (("Holdings' interest received", 6), ("Holdings' dividends received", 6),
                    ("Holdings' interest expected", 6), ("Holdings' dividends expected", 6))),
            Row(8, (("Income by type", 12), ("Top 50 payers", 12))),
            Row(7, (("Received and expected by account", 24),)),
            Row(10, (("Received and expected by position", 24),)),
            Row(8, (("Expected payments", 24),)),
            Row(8, (("Largest receipts", 24),)),
        )),
    Dashboard(
        "Cash Flow",
        "Where the household's cash came from and went, as a statement and "
        "a Sankey, and why net worth moved. Buying and selling shows net. "
        "The projection sets expected income against average past outflows. "
        + PREDEFINED_NOTE,
        "Values are shares (%) of the diagram's hub or of the biggest month. "
        "The bridge shows shares of the starting net worth. The projection "
        "shows shares of what the holdings are expected to pay. " + PREDEFINED_NOTE,
        # BOTH dashboards carry the same pickers: unlike the spending and
        # income pairs, the twin drops nothing, because none of these
        # pickers renders a dropdown of anything that identifies an
        # account. There is no account picker to drop — docs/CASHFLOW.md
        # §9. The projection's four follow, the Horizon first as on
        # Income; they bind only to the projection cards
        # (cashflow_plan_cards), which take none of the others but the
        # currency.
        ("currency", "investing", "time_range", "source", "section", "horizon", "rates", "maturing",
         "shift"), (), (
            # Five headline figures, then the diagram at the centre, then
            # the shape of the window, why net worth moved, and the
            # projection of the months ahead, then the two sides of
            # operating, then the swing sections, then the lines.
            Row(3, (("Operating in", 5), ("Operating out", 5), ("Net cash flow", 5), ("Savings rate", 4),
                    ("Yield share", 5))),
            # Full width and tall: the diagram is the dashboard.
            Row(12, (("Cash flow", 24),)),
            Row(8, (("Cash flow statement by month", 24),)),
            # The net worth the walk leads from and to, in a column
            # beside the walk.
            Row(8, (Stack(6, ("Net worth at start", "Net worth at end")), ("Why net worth moved", 18))),
            Row(8, (("Saved and gained by month", 24),)),
            Row(8, (("Cash flow projection by month", 24),)),
            Row(9, (("Cash flow projection", 24),)),
            Row(7, (("Inflows by class by month", 12), ("Outflows by class by month", 12))),
            Row(7, (("Investing by month", 12), ("Financing and vehicles by month", 12))),
            Row(8, (("Largest flows", 24),)),
        )),
    # No privacy twin. Its tiles are gains on named positions; a twin
    # would normalise the money, and what would remain is the percent
    # columns the tables carry already.
    Dashboard(
        "Gains",
        "Realized and unrealized gains over the window, by month, source, "
        "class and position, with what the figures miss. " + PREDEFINED_NOTE, None,
        ("currency", "time_range", "source", "tax_wrapper", "asset_class", "missing_basis"), (), (
            # Coarse to fine, so scrolling down reveals detail: the
            # totals, the tax years, the months, where the gains came
            # from, the monthly splits, then the positions, and last
            # what the figures miss.
            # The held total and the taxable realized gain first; then
            # the gains on positions sold and held side by side, which
            # add up to the total gain beside them.
            Row(3, (("Unrealized gain", 4), ("Realized gain", 4), ("Gain on positions sold", 4),
                    ("Gain on positions held", 4), ("Total gain", 4), ("Cost basis coverage", 4))),
            Row(6, (("Realized gains by tax year", 24),)),
            Row(8, (("Gains by month", 24),)),
            Row(8, (("Gain by source", 8), ("Gain by asset class", 8), ("Realized gains by tax wrapper", 8))),
            Row(6, (("Realized gains by month, short vs long term", 12), ("Unrealized gain over time", 12))),
            Row(8, (("Unrealized gain vs. percent", 16), Stack(8, ("Positions in the green", "Positions in the red")))),
            # The winners and losers two to a row: a table narrower
            # than half the grid cannot show one name in full.
            Row(8, (("Largest unrealized gains", 12), ("Largest unrealized losses", 12))),
            Row(8, (("Best unrealized %", 12), ("Worst unrealized %", 12))),
            Row(8, (("Largest realized gains", 12), ("Largest realized losses", 12))),
            Row(8, (("Best realized %", 12), ("Worst realized %", 12))),
            Row(8, (("Coverage by source", 24),)),
        )),
    Dashboard(
        "Exposure",
        "What is really owned, funds looked through, by class, sector, size, "
        "region, country, currency and legal situs. The credit, maturity, "
        "coupon and cycle charts fix their class and take no Asset class. "
        + PREDEFINED_NOTE,
        "Every figure is a share (%) of the selection's net worth, or of the "
        "class a chart counts, at its month end. Value columns are dropped. " + PREDEFINED_NOTE,
        # The currency is required, as report_exposure holds a row set
        # per currency; the source, the tax wrapper and the asset class
        # narrow the look-through to a selection, whose shares the cards
        # compute.
        ("currency", "time_range", "source", "tax_wrapper", "asset_class", "benchmark"), (), (
            # The allocation first, the ring beside the sectors and the
            # classes over time full width; then where it sits: the map
            # full width, the regions beside every region's countries, the
            # regions over time, and where it legally sits, full width;
            # then what it moves with, the currency ring beside its
            # history; then public equity by size beside what funds hold;
            # then the debt by credit beside its maturities, its coupons
            # beside the equity's cycles, and the largest securities, full
            # width; last, how much the figures cover.
            Row(9, (("Exposure by asset class", 12), ("Exposure by sector", 12))),
            Row(8, (("Asset classes over time", 24),)),
            Row(10, (("Exposure by country", 24),)),
            Row(9, (("Exposure by region", 11), ("Exposure by region and country", 13))),
            Row(8, (("Regions over time", 24),)),
            Row(8, (("Exposure by legal situs", 24),)),
            Row(9, (("Exposure by currency", 12), ("Currencies over time", 12))),
            Row(7, (("Public equity by size", 12), ("Held directly or through funds", 12))),
            Row(8, (("Debt by credit rating, share of debt and cash", 12), ("Debt by maturity", 12))),
            Row(7, (("Debt by coupon kind", 12), ("Public equity by cycle", 12))),
            Row(9, (("Top securities", 24),)),
            Row(7, (("How the figures are classified", 24),)),
            # Beside a benchmark alone (Card.hide_empty), and the map's
            # comparison is a table.
            Row(8, (("Largest country deviations", 24),)),
        )),
    Dashboard(
        "Yield",
        "What the holdings earn and how their value moves with rates, over "
        "the last twelve months. Projected figures follow the Rate path, "
        "Maturing, Shift and Horizon pickers. " + PREDEFINED_NOTE,
        "Rates and shares show as they are. DV01 is a share of net worth per "
        "basis point, expected income a yield on its value. Account labels "
        "are redacted. " + PREDEFINED_NOTE,
        # Exposure's five, report_yield_positions holding a row set per
        # currency too, and the projection's four.
        ("currency", "time_range", "source", "tax_wrapper", "asset_class", "benchmark", "rates", "maturing",
         "shift", "horizon"), (), (
            # The four headline yields, then each currency's beside what a
            # move in rates does to its income; cash against its
            # benchmark; the debt's yield against its duration beside its
            # DV01 along the curve; the stocks' dividends beside what the
            # figures rest on; last, the yields and the duration over
            # time.
            Row(3, (("Income yield", 6), ("Projected income", 6), ("Implied yield", 6), ("Realized yield", 6))),
            Row(8, (("Yield by currency", 12), ("Income sensitivity to rates", 12))),
            Row(8, (("Cash against its benchmark", 24),)),
            Row(9, (("Yield against duration", 12), ("DV01 along the curve", 12))),
            Row(8, (("Dividend yield against payout", 12), ("What the yields rest on", 12))),
            Row(7, (("Yields over time", 12), ("Duration over time", 12))),
        )),
    Dashboard(
        "Data Freshness",
        "How long ago each source was last read, and how much of the value "
        "each month end rests on recent marks. It takes no pickers, so every "
        "stale source shows. " + PREDEFINED_NOTE,
        "Values are shares (%) of the latest total across all sources. "
        "Absolute amounts never show. " + PREDEFINED_NOTE,
        (), (), (
            Row(3, (("Stalest source (days)", 8), ("Returns age (days)", 8))),
            Row(10, (("Source freshness", 24),)),
            Row(8, (("Mark age", 24),)),
        )),
)

DASHBOARDS_BY_NAME = {d.name: d for d in DASHBOARDS}


def picker_of(dashboard, slug):
    """The PICKERS row of `slug` for base dashboard `dashboard`: its
    own, else the slug's shared one. A slug with neither is a
    definition out of step: ValueError."""
    own = [x for x in PICKERS if x.slug == slug and x.dashboards and dashboard in x.dashboards]
    shared = [x for x in PICKERS if x.slug == slug and x.dashboards is None]
    if not own and not shared:
        raise ValueError(f"provision: dashboard {dashboard!r} names picker {slug!r}, "
                         "which PICKERS has no row for")
    return (own or shared)[0]


def check_dashboards(dashboards):
    """Raise ValueError on the first of `dashboards` that names a picker
    PICKERS has no row for, or drops one from its twin it does not
    carry. Run on DASHBOARDS as they are defined, so a dashboard out of
    step stops provisioning before anything is sent."""
    for d in dashboards:
        for slug in d.pickers:
            picker_of(d.name, slug)
        extra = [s for s in d.twin_drops if s not in d.pickers]
        if extra:
            raise ValueError(f"provision: dashboard {d.name!r} drops {extra[0]!r} from its twin, "
                             "a picker it does not carry")


check_dashboards(DASHBOARDS)

# The dashboards with no privacy twin.
NO_PRIVACY_TWIN = frozenset(d.name for d in DASHBOARDS if d.twin_desc is None)

# The Income dashboards: demo/check_dashboards.py reads them.
INCOME_DASHBOARDS = {"Income", "Income" + PRIVACY_SUFFIX}


def dashboard_defs(cards):
    """dashboard name -> Layout of every dashboard of DASHBOARDS and its
    privacy twin, each base before its twin. A twin has its base's
    layout with its cards swapped for their privacy variants, or the
    card itself where it is exempt in `cards` (card_defs). The sibling
    name links the two views — ensure_dashboards renders it as a switch
    link at the top of each dashboard."""
    out = {}
    for d in DASHBOARDS:
        pname = privacy_name(d.name) if d.twin_desc is not None else None
        tiles, headings = layout(d.rows)
        out[d.name] = Layout(d.desc, d.pickers, pname, [t[:5] for t in tiles], headings, d.redacts)
        if pname is None:
            continue
        tiles, headings = layout(d.rows, twin=True)
        out[pname] = Layout(d.twin_desc, tuple(s for s in d.pickers if s not in d.twin_drops), d.name,
                            [(c if cards[c].exempt else privacy_name(shown), *place)
                             for c, *place, shown in tiles], headings, d.redacts)
    return out


def dashboard_parameters(name):
    """The pickers dashboard `name` carries, a twin's its base's but the
    ones it drops, in their order (picker_of, picker_payload). A model a
    picker lists the values of is unbound, as bind takes it."""
    base = name.removesuffix(PRIVACY_SUFFIX)
    d = DASHBOARDS_BY_NAME[base]
    return [picker_payload(picker_of(base, s)) for s in d.pickers if name == base or s not in d.twin_drops]
