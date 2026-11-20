"""Every dashboard picker: its id, label, kind, values and default
(PICKERS), and the parameter payload built from it. Part of
provision.py."""

from typing import NamedTuple

import sqlkit
from sqlkit import (HORIZONS, MATURING_RULES,
                    MISSING_BASIS_READINGS, NO_BENCHMARK, RATE_PATHS, REPORTING_CURRENCIES)


BENCHMARK_PARAM_ID = "aa5df124"

# The dashboard pickers' ids (PICKERS).
TIME_PARAM_ID = "aa5df100"

SOURCE_PARAM_ID = "aa5df101"

ASOF_PARAM_ID = "aa5df102"

ASSET_PARAM_ID = "aa5df103"

CURRENCY_PARAM_ID = "aa5df104"

START_YEAR_PARAM_ID = "aa5df105"

VEHICLE_PARAM_ID = "aa5df106"

# The Spending dashboards' own three pickers, beside the time range
# and the source. The currency picker is separate from the Returns one
# (CURRENCY_PARAM_ID): it draws a static value list and lands on the
# spending cards' {{currency}} variable.
SPEND_CURRENCY_PARAM_ID = "aa5df107"

ACCOUNT_PARAM_ID = "aa5df108"

CATEGORY_PARAM_ID = "aa5df109"

# The currency picker of the Wealth Overview and Allocation dashboards
# and their twins: the same static list as the Spending one, landing on
# every tile's {{currency}} variable.
WEALTH_CURRENCY_PARAM_ID = "aa5df110"

# Their tax wrapper picker. Their asset class picker is ASSET_PARAM_ID.
WEALTH_TAX_WRAPPER_PARAM_ID = "aa5df118"

# The Yield dashboards' currency, tax wrapper and asset class pickers.
YIELD_CURRENCY_PARAM_ID = "aa5df119"

YIELD_TAX_WRAPPER_PARAM_ID = "aa5df11a"

YIELD_ASSET_CLASS_PARAM_ID = "aa5df11b"

# The projection's four pickers, which the Yield dashboards and the
# Income dashboards' cards of the holdings' income share: the rate path,
# what a maturing principal does, the shift in basis points and the
# horizon.
RATES_PARAM_ID = "aa5df11c"

MATURING_PARAM_ID = "aa5df11d"

SHIFT_PARAM_ID = "aa5df11e"

HORIZON_PARAM_ID = "aa5df11f"

# The position the Income dashboards' calendar lists.
POSITION_PARAM_ID = "aa5df121"

# The income dashboards' own picker ids. Distinct from the spending
# ones because a Metabase parameter id is per-dashboard-parameter, and
# the two dashboards carry different filter sets over different views.
INCOME_CURRENCY_PARAM_ID = "aa5df10a"

INCOME_ACCOUNT_PARAM_ID = "aa5df10b"

INCOME_TYPE_PARAM_ID = "aa5df10c"

INCOME_TAX_WRAPPER_PARAM_ID = "aa5df122"

# The Cash Flow dashboards' own picker ids.
CASHFLOW_CURRENCY_PARAM_ID = "aa5df10d"

CASHFLOW_INVESTING_PARAM_ID = "aa5df10e"

CASHFLOW_SECTION_PARAM_ID = "aa5df10f"

# The Gains dashboard's own picker ids.
GAINS_CURRENCY_PARAM_ID = "aa5df111"

GAINS_TAX_WRAPPER_PARAM_ID = "aa5df112"

GAINS_MISSING_PARAM_ID = "aa5df113"

GAINS_ASSET_CLASS_PARAM_ID = "aa5df123"

# The Private Markets dashboards' currency picker: a static list, landing
# on the cards' {{currency}} variable. Their asset class, vehicle and
# holding pickers list the holdings' own values (report_private_holdings).
PRIVATE_CURRENCY_PARAM_ID = "aa5df125"

PRIVATE_ASSET_CLASS_PARAM_ID = "aa5df126"

PRIVATE_VEHICLE_PARAM_ID = "aa5df127"

PRIVATE_HOLDING_PARAM_ID = "aa5df128"

# The Exposure dashboard's picker ids and filters: the currency (a row
# filter, as on Gains), the time range over the month ends, the source,
# the tax wrapper and the asset class of each leaf.
EXPOSURE_CURRENCY_PARAM_ID = "aa5df114"

EXPOSURE_TAX_WRAPPER_PARAM_ID = "aa5df115"

EXPOSURE_ASSET_CLASS_PARAM_ID = "aa5df116"


class Column(NamedTuple):
    """A picker's dropdown source: the values `field` takes on
    pre-defined model `model`."""
    model: str
    field: str


class Picker(NamedTuple):
    """One dashboard picker, the payload dashboard_parameters builds from
    it. `dashboards` are the base dashboards whose picker of `slug` it
    is, None for any dashboard no other row names. `kind`: `list`, a
    multi-select text picker over a Column, no default = all values;
    `exclude`, the same that keeps the values it does not hold;
    `choice`, a required single value over a fixed tuple of values or a
    Column; `number`, a required number, its values from a Column if one
    is given; `day` and `range`, a date and a date range; `text`, a free
    text box. `default` is a value, or a function for one the
    configuration sets. `inline`: the picker renders on the tiles it
    reaches, not in the filter bar."""
    slug: str
    dashboards: tuple
    id: str
    label: str
    kind: str
    source: object = None
    default: object = None
    inline: bool = False


def _configured_currency():
    return sqlkit.DEFAULT_CURRENCY


def _configured_missing_basis():
    return sqlkit.DEFAULT_MISSING_BASIS


def _asof_default():
    return sqlkit.ASOF_DEFAULT


_WEALTH = ("Wealth Overview", "Allocation")

# Every picker, one row per (dashboards, slug). The ids are arbitrary
# but must be stable across runs so re-provisioning converges instead of
# accumulating parameters, and they must be distinct — a reused id would
# make two pickers one. A slug's id is per family of dashboards where
# the families' pickers differ (their values or the views they land on),
# shared where they do not.
PICKERS = (
    # The required reporting-currency picker every filtered dashboard
    # carries, defaulting to the configured currency (DEFAULT_CURRENCY).
    # A card running with the currency cleared would be wrong: a native
    # tile's {{currency}} variable needs a value. A static list: the product's own vocabulary, not the
    # data's, so a card-backed list would re-scan the whole population
    # for a few known strings. Required with a default, because a
    # required parameter resets to its default rather than clearing.
    Picker("currency", _WEALTH, WEALTH_CURRENCY_PARAM_ID, "Currency", "choice",
           REPORTING_CURRENCIES, _configured_currency),
    # report_returns carries one row set per currency, so a card running
    # with the currency cleared would show every period once per
    # currency. The values come off the materialized table's own
    # currency column, like the start-year list.
    Picker("currency", ("Returns",), CURRENCY_PARAM_ID, "Currency", "choice",
           Column("report_returns", "currency"), _configured_currency),
    Picker("currency", ("Spending",), SPEND_CURRENCY_PARAM_ID, "Currency", "choice",
           REPORTING_CURRENCIES, _configured_currency),
    Picker("currency", ("Income",), INCOME_CURRENCY_PARAM_ID, "Currency", "choice",
           REPORTING_CURRENCIES, _configured_currency),
    Picker("currency", ("Cash Flow",), CASHFLOW_CURRENCY_PARAM_ID, "Currency", "choice",
           REPORTING_CURRENCIES, _configured_currency),
    Picker("currency", ("Gains",), GAINS_CURRENCY_PARAM_ID, "Currency", "choice",
           REPORTING_CURRENCIES, _configured_currency),
    Picker("currency", ("Exposure",), EXPOSURE_CURRENCY_PARAM_ID, "Currency", "choice",
           REPORTING_CURRENCIES, _configured_currency),
    Picker("currency", ("Yield",), YIELD_CURRENCY_PARAM_ID, "Currency", "choice",
           REPORTING_CURRENCIES, _configured_currency),
    Picker("currency", ("Private Markets",), PRIVATE_CURRENCY_PARAM_ID, "Currency", "choice",
           REPORTING_CURRENCIES, _configured_currency),
    # "past12months~": the trailing ~ means "include this month".
    # Without it Metabase takes the previous 12 COMPLETE months, which
    # silently drops every row of the current partial month.
    Picker("time_range", None, TIME_PARAM_ID, "Time range", "range", default="past12months~"),
    # Required, with today as the default (ASOF_DEFAULT): a required
    # parameter resets to its default instead of clearing, so a card
    # always reads the day picked. It reads the last history day on or
    # before it (_history_day): a day after the history, up to tomorrow,
    # reads its last day; a day before it, or later than tomorrow,
    # nothing.
    Picker("as_of_day", None, ASOF_PARAM_ID, "As of day", "day", default=_asof_default),
    # Start year, the Returns dashboards' picker in place of a time
    # filter, since their periods are precomputed buckets. It rescopes
    # the summary scalars/table to the since-<year> window
    # (window_from_year) and clips the charts to periods ending on/after
    # that year. 0 = since inception. Required + default 0 so a
    # card never aggregates across windows; values come from the
    # materialized window_from_year set (auto-syncs to the data's span).
    Picker("start_year", None, START_YEAR_PARAM_ID, "Start year (0 = since inception)", "number",
           Column("report_returns", "window_from_year"), 0),
    # Not required / no default = all sources. On Returns it narrows the
    # by-source table and the charts, which keep the global '(all
    # sources)' reference line regardless.
    Picker("source", None, SOURCE_PARAM_ID, "Source", "list",
           Column("report_sources_latest", "silver_source_id")),
    # Required with None as its default: every card then reads one
    # benchmark or none, and the list comes off the config by the model.
    Picker("benchmark", None, BENCHMARK_PARAM_ID, "Benchmark", "choice",
           Column("report_benchmarks", "benchmark"), NO_BENCHMARK),
    # A tax wrapper picker. Its values come off the accounts model: every
    # view reads a line's tax wrapper off its account. Multi-select, and
    # a wrapper names no account, so a twin carries it too.
    *(Picker("tax_wrapper", dashboards, pid, "Tax wrapper", "list",
             Column("report_accounts_latest", "tax_wrapper"))
      for dashboards, pid in ((_WEALTH, WEALTH_TAX_WRAPPER_PARAM_ID),
                              (("Income",), INCOME_TAX_WRAPPER_PARAM_ID),
                              (("Gains",), GAINS_TAX_WRAPPER_PARAM_ID),
                              (("Exposure",), EXPOSURE_TAX_WRAPPER_PARAM_ID),
                              (("Yield",), YIELD_TAX_WRAPPER_PARAM_ID))),
    # An asset class picker. Its values come off the asset class history
    # by default, which reads cash as the class `cash`, the way every
    # holdings view does. That taxonomy is also the one every exposure
    # leaf's class is one of. Gains and Yield list their own rows'
    # classes (report_gains_classes, report_yield_classes), `unknown`
    # among them.
    *(Picker("asset_class", dashboards, pid, "Asset class", "list", Column(model, "asset_class"))
      for dashboards, pid, model in (
          (_WEALTH, ASSET_PARAM_ID, "report_asset_classes_history"),
          (("Gains",), GAINS_ASSET_CLASS_PARAM_ID, "report_gains_classes"),
          (("Exposure",), EXPOSURE_ASSET_CLASS_PARAM_ID, "report_asset_classes_history"),
          (("Yield",), YIELD_ASSET_CLASS_PARAM_ID, "report_yield_classes"),
          (("Private Markets",), PRIVATE_ASSET_CLASS_PARAM_ID, "report_private_holdings"))),
    # The vehicle filter reaches only the Top-positions tiles, which list
    # individual holdings, and renders on them: the breakdowns each group
    # by a taxonomy dimension, so filtering them by the vehicle would
    # mostly self-select. Its values are the positions' vehicles.
    Picker("vehicle", None, VEHICLE_PARAM_ID, "Vehicle", "list",
           Column("report_positions_history", "vehicle"), inline=True),
    # On Private Markets every card lists or adds up holdings, so the
    # vehicle reaches them all, from the bar, over the holdings' own
    # vehicles. Its Holding picker leaves the holdings it holds out:
    # the question it answers is the figures without one or two
    # holdings, the one that dwarfs the others or one with no cost. A
    # holding's name is no account's, so the twin carries both.
    Picker("vehicle", ("Private Markets",), PRIVATE_VEHICLE_PARAM_ID, "Vehicle", "list",
           Column("report_private_holdings", "vehicle")),
    Picker("holding", ("Private Markets",), PRIVATE_HOLDING_PARAM_ID, "Holding is not", "exclude",
           Column("report_private_holdings", "holding")),
    # The account picker targets `display_name` — the model's name for
    # the account LABEL (account_label) — and NOT `account_external_id`:
    # a picker lists the raw values of the column it is bound to and
    # there is no field-remapping machinery here, so binding it to the id
    # would offer a list of opaque identifiers. The label carries the
    # source and the kind, so two accounts wearing the same generic
    # product name at two institutions are two distinct entries rather
    # than one indistinguishable pair; two named alike WITHIN one source
    # still select together, accepted on the same reasoning, and the
    # money tiles group by the same column.
    #
    # THE PRIVACY TWIN CARRIES NO ACCOUNT PICKER (twin_drops). A picker
    # cannot redact what it offers: its dropdown IS the list of values
    # the bound column takes, and every column that identifies an
    # account is a label — the account label, the display name it is
    # built from (a card's falls back to its masked last four digits),
    # or the external id. Rebinding to a column that identifies no
    # account would make it a different filter wearing the same name, so
    # the twin drops it, the way its cards drop the columns they cannot
    # show (spending_cards). The Source picker still narrows by
    # institution, and the account breakdown there regroups onto source
    # × account kind.
    Picker("account", ("Spending",), ACCOUNT_PARAM_ID, "Account", "list",
           Column("report_spending", "display_name")),
    Picker("account", ("Income",), INCOME_ACCOUNT_PARAM_ID, "Account", "list",
           Column("report_income", "display_name")),
    Picker("category", ("Spending",), CATEGORY_PARAM_ID, "Category", "list",
           Column("report_spending", "spend_primary")),
    # Bound to the DETAILED label: the income taxonomy has one vendored
    # primary, so a primary-level picker would offer a handful of values
    # and hide every distinction worth filtering by.
    #
    # `income_detailed` is the MODEL'S ALIAS for that label
    # (report_income projects `i.income_label AS income_detailed`), not
    # the view column of the same name. A picker's dropdown is the values
    # its value_field takes on the model, so naming a column the model
    # does not project leaves the dropdown empty — and the tiles then
    # match a filter nobody could set. The matching field filter is bound
    # to the VIEW's income_label (INCOME_FILTERS), so picker and
    # predicate offer the same vocabulary, which is the contract the
    # spending pair states.
    Picker("type", ("Income",), INCOME_TYPE_PARAM_ID, "Type", "list",
           Column("report_income", "income_detailed")),
    # The position the calendar lists, a text box the positions table's
    # cells also fill (its card's Link).
    Picker("position", ("Income",), POSITION_PARAM_ID, "Position", "text"),
    # The Investing grain: net the section as one movement — the question
    # a reader opens with, "did the portfolio feed the household this
    # year or the household feed the portfolio" — or per asset class,
    # which is the step in. Required with a `whole` default so a card
    # never runs with it cleared, and a static list because the two
    # grains are the feature's, not the data's.
    Picker("investing", ("Cash Flow",), CASHFLOW_INVESTING_PARAM_ID, "Investing", "choice",
           ("whole", "class"), "whole"),
    # A section picker offers the feature's own vocabulary, the sections
    # the serving view carries, so the twin carries it too. It binds only
    # to the cards that declare its tag; the headline figures and the
    # diagram decline it, identically on the base and the twin.
    Picker("section", ("Cash Flow",), CASHFLOW_SECTION_PARAM_ID, "Section", "list",
           Column("report_cashflow", "section")),
    # Missing cost basis is required, like the currency: report_gains
    # holds a row set per reading, and a tile with the picker cleared
    # would sum both.
    Picker("missing_basis", ("Gains",), GAINS_MISSING_PARAM_ID, "Missing cost basis", "choice",
           MISSING_BASIS_READINGS, _configured_missing_basis),
    # The projection's four: required, as each card reads one rate path,
    # one maturing rule and one horizon of the parts, with the CLI's
    # defaults. The shift is a number of basis points.
    Picker("rates", None, RATES_PARAM_ID, "Rate path", "choice", RATE_PATHS, RATE_PATHS[0]),
    Picker("maturing", None, MATURING_PARAM_ID, "Maturing", "choice", MATURING_RULES, MATURING_RULES[0]),
    Picker("shift", None, SHIFT_PARAM_ID, "Shift (bp)", "number", default=0),
    Picker("horizon", None, HORIZON_PARAM_ID, "Horizon (months)", "choice", HORIZONS, HORIZONS[0]),
)


def picker_payload(x):
    """The parameter of a dashboard Picker `x`, a Column's model unbound
    (("model_id", name), which bind resolves). `values_query_type` is
    what makes Metabase render a choice's dropdown instead of a
    free-text box."""
    out = {"id": x.id, "name": x.label, "slug": x.slug}
    default = x.default() if callable(x.default) else x.default
    if x.kind == "range":
        out.update({"type": "date/all-options", "sectionId": "date", "default": default})
    elif x.kind == "day":
        out.update({"type": "date/single", "sectionId": "date", "default": default, "required": True})
    elif x.kind == "number":
        out.update({"type": "number/=", "sectionId": "number", "isMultiSelect": False,
                    "default": [default], "required": True})
    else:
        out.update({"type": "string/!=" if x.kind == "exclude" else "string/=", "sectionId": "string",
                    "isMultiSelect": x.kind in ("list", "exclude")})
        if x.kind == "choice":
            out.update({"default": [default], "required": True, "values_query_type": "list"})
    if isinstance(x.source, Column):
        btype = "type/Integer" if x.kind == "number" else "type/Text"
        out.update({"values_source_type": "card",
                    "values_source_config": {"card_id": ("model_id", x.source.model),
                                             "value_field": ["field", x.source.field, {"base-type": btype}]}})
    elif x.source is not None:
        out.update({"values_source_type": "static-list", "values_source_config": {"values": list(x.source)}})
    return out


# The pickers that render on the tiles they reach rather than in the bar.
INLINE_PICKERS = frozenset(x.id for x in PICKERS if x.inline)
