#!/usr/bin/env python3
"""Unit tests for web/provision.py's pure definition helpers — no
Metabase and no Docker required. Run via `make test-web` (web/test_web.sh
invokes this), or directly. The SQL of the projection cards and of the
Exposure debt and cycle cards also runs on an invented book under
DuckDB, which needs the duckdb Python module: `make test-web` runs this
in web/.venv, which holds it, and without it those sections fail.

Everything provision.py builds before it talks to the API is a pure
function of module constants, so the card, filter and dashboard
definitions can be asserted statically: names resolve, parameter ids stay
unique, the privacy twin redacts what it promises to, and metadata that
does not describe a fully migrated gold aborts the run. Where a constant
here is one half of a contract gold's SQL states in the other, the
migration files are read off disk and the two halves compared — no
engine, no Docker. What cannot be checked here is whether Metabase
accepts a payload — that needs a live instance.

Prints the same "  ok / FAIL" lines web/test_web.sh does and exits with
the number of failures.
"""
import contextlib
import datetime as dt
import importlib.util
import io
import json
import math
import os
import re
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import provision as p                                   # noqa: E402

FAILS = 0


@contextlib.contextmanager
def quiet():
    """Swallow the progress and abort messages provisioning prints, so
    they don't read as test output."""
    buf = io.StringIO()
    with contextlib.redirect_stdout(buf), contextlib.redirect_stderr(buf):
        yield


def section(title):
    print(f"== {title} ==")


def ok(desc):
    print(f"  ok   {desc}")


def bad(desc, detail=""):
    global FAILS
    FAILS += 1
    print(f"  FAIL {desc}")
    if detail:
        print(f"       {detail}")


def check(desc, cond, detail=""):
    ok(desc) if cond else bad(desc, detail)


def resolve_field_ids():
    """Stand in for a synced Metabase: every column in the registry gets
    a distinct field id, the way main() resolves them post-sync."""
    p.FIELD_IDS = {(t, c): 1000 + i * 10 + j
                   for i, (t, cols) in enumerate(p.FILTER_FIELD_COLUMNS.items())
                   for j, c in enumerate(cols)}


def model_ids():
    return {name: i for i, name in enumerate(p.report_models(), start=1)}


def cards_of(qdefs, pdefs):
    """name -> (display, description, dataset_query) for the base and
    privacy definitions `qdefs` and `pdefs`, in the shape ensure_cards
    assembles them."""
    cards = {name: (display, desc, query)
             for name, (display, desc, query, _viz) in qdefs.items()}
    cards.update({name: (display, desc, query)
                  for name, (_t, display, desc, query, _viz) in pdefs.items()})
    return cards


def all_cards(db_id, mid):
    """Every card provisioning defines (cards_of), built afresh — which
    also rebuilds the picker registry, NATIVE_PARAM_TARGETS."""
    return cards_of(p.question_defs(db_id, mid), p.privacy_card_defs(db_id, mid))


def sql_of(query):
    """The native SQL of a card query, or None for an MBQL one."""
    return query["native"]["query"] if query.get("type") == "native" else None


# ---- the filter-field registry ----------------------------------------

section("filter-field registry (the two spending views)")
# The category field is the LABEL column: the Category picker is shared
# with the money dashboard, whose model column of the same name holds the
# label, so both sides of the filter must speak one vocabulary.
check("web_spending columns: timestamp, source, account label, primary category",
      set(p.FILTER_FIELD_COLUMNS.get("web_spending", ())) ==
      {"occurred_at", "silver_source_id", "account_label", "spend_primary_label"},
      str(p.FILTER_FIELD_COLUMNS.get("web_spending")))
check("web_card_balances_history columns: day, source, account label",
      set(p.FILTER_FIELD_COLUMNS.get("web_card_balances_history", ())) ==
      {"as_of_day", "silver_source_id", "account_label"},
      str(p.FILTER_FIELD_COLUMNS.get("web_card_balances_history")))
check("the account filter binds account_label, never the external id",
      not any("account_external_id" in cols
              for cols in p.FILTER_FIELD_COLUMNS.values()))

# ---- parameter ids ----------------------------------------------------

section("dashboard parameter ids")
PARAM_IDS = {n: v for n, v in vars(p).items() if n.endswith("_PARAM_ID")}
check("every *_PARAM_ID constant is distinct",
      len(set(PARAM_IDS.values())) == len(PARAM_IDS),
      json.dumps(PARAM_IDS))

# ---- the models -------------------------------------------------------

# A timestamp column is rendered with epoch_ms, never to_timestamp. The two
# differ by type, and so by what a reader sees: to_timestamp yields
# TIMESTAMPTZ, which Metabase renders in the reading session's zone, so a
# day-grain figure lands on the wrong day for anyone east or west of UTC.
# epoch_ms yields a zone-free TIMESTAMP. A regression here is silent —
# every chart still renders, just shifted — so it is asserted rather than
# left to the eye.
_MODEL_SQL = "\n".join(sql for sql, _desc in p.report_models().values())
check("no model renders a timestamp with to_timestamp",
      "to_timestamp" not in _MODEL_SQL,
      "to_timestamp is TIMESTAMPTZ and renders in the session zone")
check("timestamp columns are rendered with epoch_ms",
      "epoch_ms(" in _MODEL_SQL)


resolve_field_ids()
MID = model_ids()
MODELS = p.report_models()

section("spending models")
check("report_spending defined", "report_spending" in MODELS)
check("report_spending_pct defined", "report_spending_pct" in MODELS)
SPEND_SQL = MODELS["report_spending"][0]
PCT_SQL = MODELS["report_spending_pct"][0]
check("the model reads the web_spending serving view",
      "FROM web_spending s" in SPEND_SQL)
check("one row per reporting currency (a currency picker filters rows)",
      p._ccy_values() in SPEND_SQL and
      "AS value" in SPEND_SQL)
check("the money model keeps the merchant column",
      "merchant_name" in SPEND_SQL)
check("the privacy model drops the merchant column",
      "merchant_name" not in PCT_SQL)
check("the privacy model keeps display_name (the Account picker needs it)",
      "display_name" in PCT_SQL)
check("both models label an account with its source and kind, not its "
      "bare name",
      all("s.account_label AS display_name" in q for q in (SPEND_SQL, PCT_SQL)))
check("the privacy model scales by the latest net worth, per currency",
      all(f"nw.nw_{c}" in PCT_SQL for c in p._ccy_lower()))

# ---- the cards --------------------------------------------------------

# Built once, which also fills the picker registry. A test that changes
# DEFAULT_CURRENCY or FIELD_IDS rebuilds what it reads and restores the
# registry after.
_QDEFS = p.question_defs(1, MID)
_PDEFS = p.privacy_card_defs(1, MID)
CARDS = cards_of(_QDEFS, _PDEFS)
VIZ = {n: tup[-1] for defs in (_QDEFS, _PDEFS) for n, tup in defs.items()}
DEFS = p.dashboard_defs()

section("retired models")
check("no retired model is still defined",
      not set(p.RETIRED_MODEL_NAMES) & set(MODELS),
      sorted(set(p.RETIRED_MODEL_NAMES) & set(MODELS)))
check("the retired names are distinct",
      len(set(p.RETIRED_MODEL_NAMES)) == len(p.RETIRED_MODEL_NAMES))
# A privacy (_pct) model exists for the MBQL twin cards that run over it:
# the privacy charts are native over the serving views, so a variant no
# card reads is retired rather than kept for browsing.
_CARD_SOURCES = {int(i) for i in re.findall(
    r'"card__(\d+)"', json.dumps([q for _d, _desc, q in CARDS.values()]))}
_PCT_MODELS = sorted(n for n in MODELS if n.endswith("_pct"))
check("every _pct model backs a card",
      bool(_PCT_MODELS) and all(MID[n] in _CARD_SOURCES for n in _PCT_MODELS),
      [n for n in _PCT_MODELS if MID[n] not in _CARD_SOURCES])

section("spending cards")
SPEND_TILES = [t[0] for t in DEFS["Spending"][3]]
for want in ("Spend — monthly trend", "Net spend", "Uncategorized share",
             "Spending by month", "Spending by category",
             "Spending by subcategory", "Top 50 merchants",
             "Spend by account", "Card balances over time",
             "Largest transactions"):
    check(f"tile '{want}' is defined and pinned",
          want in CARDS and want in SPEND_TILES)
check("the uncategorized-share card counts the '(uncategorized)' label",
      p.UNCATEGORIZED in json.dumps(CARDS["Uncategorized share"][2]))
check("the uncategorized-share card runs over the _pct model "
      "(it is privacy-exempt, so its drill-through must be leak-free)",
      CARDS["Uncategorized share"][2]["query"]["source-table"]
      == f"card__{MID['report_spending_pct']}")
check("card balances come off web_card_balances_history",
      "FROM web_card_balances_history"
      in (sql_of(CARDS["Card balances over time"][2]) or ""))
check("the money cards render merchant labels",
      all("merchant_name" in json.dumps(CARDS[c][2])
          for c in ("Top 50 merchants", "Largest transactions")))
check("the money cards render account labels",
      all("account_label" in json.dumps(CARDS[c][2])
          for c in ("Spend by account", "Largest transactions")))

# The two breakdowns are rings, on both dashboards. Every slice is drawn
# on the primary ring — Metabase's own small-slice bucket is called
# "Other", and so is a real category here — and neither ring carries a
# LIMIT: a limited ring renormalizes onto what it drew, so its printed
# shares would disagree with its own values.
_RINGS = {n: (_QDEFS[n][0], _QDEFS[n][3]) for n in ("Spending by category",
                                                    "Spending by subcategory")}
_RINGS.update({n: (_PDEFS[n][1], _PDEFS[n][4])
               for n in ("Spending by category (privacy)",
                         "Spending by subcategory (privacy)")})
for _name, (_display, _viz) in _RINGS.items():
    check(f"'{_name}' is a ring", _display == "pie", _display)
    check(f"'{_name}' shows its shares in the legend",
          _viz.get("pie.percent_visibility") == "legend")
    check(f"'{_name}' draws no LIMIT'd ring",
          "limit" not in json.dumps(CARDS[_name][2])
          and "LIMIT" not in (sql_of(CARDS[_name][2]) or ""))
check("the primary ring folds nothing into an 'Other' wedge (a real "
      "category is called that)",
      all(_RINGS[n][1].get("pie.slice_threshold") == 0
          for n in ("Spending by category", "Spending by category (privacy)")))
check("the detailed rings do fold their tail (nearly a hundred values)",
      all(_RINGS[n][1].get("pie.slice_threshold", 0) > 0
          for n in ("Spending by subcategory",
                    "Spending by subcategory (privacy)")))
# The figure in the hole sums the slices Metabase DREW, and a ring
# cannot draw a negative one — so where a bucket can go net-negative
# (refunds beating purchases) the hole runs over by what it left out.
# The money rings keep it: it is the figure they are read for, and
# their descriptions say it is the drawn categories'. The share rings
# drop it, since there it would carry that error while restating a
# figure their own description gives as 100.
check("the share rings carry no figure in the hole",
      all(_RINGS[n][1].get("pie.show_total") is False
          for n in ("Spending by category (privacy)",
                    "Spending by subcategory (privacy)")))
check("the money rings keep their total in the hole",
      all(_RINGS[n][1].get("pie.show_total") is True
          for n in ("Spending by category", "Spending by subcategory")))
check("a ring promising a total in the hole says whose total it is",
      all("drawn categories" in CARDS[n][1] or "in the total" in CARDS[n][1]
          for n in ("Spending by category", "Spending by subcategory")),
      [CARDS[n][1] for n in ("Spending by category", "Spending by subcategory")])

# Card balances read the way an issuer states them: owed, positive. The
# flip is in the projection only — gold stores the liability negative.
for _name in ("Card balances over time", "Card balances over time (privacy)"):
    _sql = sql_of(CARDS[_name][2]) or ""
    check(f"'{_name}' states what is owed as a positive figure",
          all(f"-balance_{c}" in _sql for c in p._ccy_lower()), _sql)
check("the money card-balances chart is split by the account label",
      "account_label" in (sql_of(CARDS["Card balances over time"][2]) or ""))

# A card called "by month" charts months. Metabase infers the x-axis
# from cardinality, and there are more categories than months in any
# window worth charting, so both monthly cards pin their dimensions —
# over their own SQL aliases, both reading the view natively. Without
# the pin the card draws its own transpose.
check("the monthly cards put the month on the x-axis, categories in the "
      "stack",
      all(d[i]["graph.dimensions"] == ["month", "category"]
          for d, i in ((_QDEFS["Spending by month"], 3),
                       (_PDEFS["Spending by month (privacy)"], 4))))
check("...as stacked bands over a continuous axis",
      all(d == "area" and v.get("stackable.stack_type") == "stacked"
          for d, v in ((_QDEFS["Spending by month"][0],
                        _QDEFS["Spending by month"][3]),
                       (_PDEFS["Spending by month (privacy)"][1],
                        _PDEFS["Spending by month (privacy)"][4]))))

# The merchant rankings rank merchants. A line resolved to a delta — a
# gift, a card bill, cash out of an ATM — is not a merchant
# transaction, and a card bill carries the issuer it was paid to as its
# merchant (migration 0052), so a blank merchant does not stand for
# "not a delta". Both rankings exclude the delta CATEGORIES — a
# delta is primary-level, so the two category columns are equal on one
# — and still require a name to rank by. Only the rankings filter: a
# transaction list shows every line.
MERCHANT_RANKINGS = {"Top 50 merchants", "Top 50 merchants (privacy)"}


def filters_on_merchant(query):
    """Whether a card keeps only lines with a merchant: a native WHERE
    predicate on the column, or an MBQL filter clause naming it."""
    sql = sql_of(query)
    if sql is not None:
        return "merchant_name IS NOT NULL" in sql
    return "merchant_name" in json.dumps(query["query"].get("filter", []))


check("the money merchant ranking keeps only named, non-delta lines",
      CARDS["Top 50 merchants"][2]["query"].get("filter")
      == ["and",
          ["not-null", ["field", "merchant_name", {"base-type": "type/Text"}]],
          ["!=", ["field", "spend_primary", {"base-type": "type/Text"}],
                 ["field", "spend_detailed", {"base-type": "type/Text"}]]])
# Split on the CTE boundary with partition, which returns empties rather
# than raising when the shape moved: a regression must read as one FAIL
# line, not as a traceback that takes the rest of the file with it.
RANK_SQL = sql_of(CARDS["Top 50 merchants (privacy)"][2]) or ""
R_BODY, CTE_BOUNDARY, REST = RANK_SQL.partition("),\nt AS (\n")
T_BODY = REST.partition("\nSELECT row_number")[0]
check("the privacy merchant ranking is r (ranked rows) then t (the total)",
      bool(CTE_BOUNDARY) and "FROM web_spending" in T_BODY)
check("the privacy merchant ranking keeps only named, non-delta lines",
      "AND merchant_name IS NOT NULL" in R_BODY and
      "AND spend_primary_label <> spend_label" in R_BODY and
      "GROUP BY merchant_name" in R_BODY)
check("...and its shares stay of the window's whole net spend: the total "
      "is summed apart from the ranked rows, without either filter",
      RANK_SQL.count("FROM web_spending") == 2 and
      "merchant_name" not in T_BODY and
      "spend_primary_label <> spend_label" not in T_BODY)
check("both rankings say delta lines are outside them",
      all("outside the ranking" in CARDS[c][1] and
          "issuer" in CARDS[c][1] for c in MERCHANT_RANKINGS))
check("exactly the two rankings filter on the merchant column",
      {c for d in ("Spending", "Spending (privacy)")
       for c, *_ in DEFS[d][3] if filters_on_merchant(CARDS[c][2])}
      == MERCHANT_RANKINGS)
check("the largest-lines card keeps blank merchants (a line is a line)",
      "merchant_name IS NOT NULL" not in (sql_of(CARDS["Largest transactions"][2]) or ""))
check("every tile of every dashboard resolves to a defined card",
      not [(d, c) for d, (_, _, _, tiles) in DEFS.items()
           for c, *_ in tiles if c not in CARDS],
      str([(d, c) for d, (_, _, _, tiles) in DEFS.items()
           for c, *_ in tiles if c not in CARDS]))

# ---- the dashboard and its twin ---------------------------------------

section("Spending dashboard + privacy twin")
desc, mode, sibling, tiles = DEFS["Spending"]
check("filter mode is the range pair", mode == "range")
check("sibling is the privacy twin", sibling == "Spending (privacy)")
check("the twin is derived tile for tile",
      [t[0] for t in DEFS["Spending (privacy)"][3]] ==
      [c if c in p.PRIVACY_EXEMPT_CARDS else p.privacy_name(c)
       for c, *_ in tiles])
check("the twin's blurb names its own denominators, not the holdings one",
      "biggest month" in DEFS["Spending (privacy)"][0] and
      "redacted" in DEFS["Spending (privacy)"][0])
check("the twin keeps the layout", [t[1:] for t in DEFS["Spending (privacy)"][3]]
      == [t[1:] for t in tiles])

PARAMS = {x["name"]: x for x in p.dashboard_parameters(MID, "range", "Spending")}
check("the range pair is still there",
      {"Time range", "Source"} <= set(PARAMS))
check("currency picker is required and defaults to USD",
      PARAMS["Currency"]["required"] is True and
      PARAMS["Currency"]["default"] == ["USD"] and
      PARAMS["Currency"]["id"] == p.SPEND_CURRENCY_PARAM_ID)
check("currency offers every reporting currency",
      PARAMS["Currency"]["values_source_config"]["values"] == list(p.REPORTING_CURRENCIES))
check("the account picker draws display_name values",
      PARAMS["Account"]["values_source_config"]["value_field"][1] == "display_name")
# The spending model renders the taxonomy in words and keeps the values
# beside them. A dashboard picker's dropdown IS the list of labels it
# filters by, so the label is what a reader sees and selects; the value
# is there for a filter that must survive a rewording. The issuer's own
# classification travels too, and no tile may aggregate it.
# The Category picker is shared by both dashboards: its dropdown comes
# from the money model's `spend_primary`, and the privacy twin's native
# cards field-filter web_spending through FILTER_FIELD_COLUMNS. If those
# two named different vocabularies the picker would offer labels and the
# native cards would match them against vendored values — every privacy
# tile silently empty, with no error anywhere.
check("both sides of the shared Category filter speak one vocabulary",
      p.SPEND_FILTERS["category"][0] in p.FILTER_FIELD_COLUMNS["web_spending"] and
      p.SPEND_FILTERS["category"][0].endswith("_label"))
_PRIV_SQL = "\n".join(sql_of(c[2]) or "" for n, c in CARDS.items()
                      if n.endswith(p.PRIVACY_SUFFIX) and len(c) > 2)
check("...and the privacy cards project the same labels they filter on",
      "spend_primary_label" in _PRIV_SQL and "spend_primary," not in _PRIV_SQL)

check("the spending model renders labels as the category columns",
      "spend_primary_label AS spend_primary" in SPEND_SQL and
      "spend_label         AS spend_detailed" in SPEND_SQL)
check("...and keeps the vendored values beside them",
      "AS spend_primary_id" in SPEND_SQL and
      "AS spend_detailed_id" in SPEND_SQL)
check("...and carries the issuer's own view",
      "AS provider_category" in SPEND_SQL)
check("the privacy twin keeps the same category columns",
      "spend_primary_label AS spend_primary" in PCT_SQL and
      "AS provider_category" in PCT_SQL)

check("the category picker is a multi-select on spend_primary",
      PARAMS["Category"]["isMultiSelect"] is True and
      PARAMS["Category"]["values_source_config"]["value_field"][1] == "spend_primary")
TWIN_PARAMS = p.dashboard_parameters(MID, "range", "Spending (privacy)")
check("the twin carries every picker but the account one — a picker's "
      "dropdown IS the list of account labels it would filter by",
      [x["id"] for x in TWIN_PARAMS] ==
      [p.SPEND_CURRENCY_PARAM_ID, p.TIME_PARAM_ID, p.SOURCE_PARAM_ID,
       p.CATEGORY_PARAM_ID])
check("no picker on the twin is bound to an account column",
      not [x for x in TWIN_PARAMS
           if "display_name" in json.dumps(x.get("values_source_config", {}))
           or "account_external_id" in json.dumps(x.get("values_source_config", {}))],
      json.dumps(TWIN_PARAMS))
for _d in ("Wealth Overview", "Wealth Overview (privacy)"):
    check(f"'{_d}' carries the currency picker, the range pair, the tax wrapper and the asset class",
          [x["id"] for x in p.dashboard_parameters(MID, "range", _d)]
          == [p.WEALTH_CURRENCY_PARAM_ID, p.TIME_PARAM_ID, p.SOURCE_PARAM_ID,
              p.WEALTH_TAX_WRAPPER_PARAM_ID, p.ASSET_PARAM_ID])
for _d in ("Allocation", "Allocation (privacy)"):
    check(f"'{_d}' carries the currency picker, the as-of pickers, the tax wrapper and the asset class",
          [x["id"] for x in p.dashboard_parameters(MID, "asof", _d)]
          == [p.WEALTH_CURRENCY_PARAM_ID, p.ASOF_PARAM_ID, p.SOURCE_PARAM_ID,
              p.WEALTH_TAX_WRAPPER_PARAM_ID, p.ASSET_PARAM_ID, p.VEHICLE_PARAM_ID])
# The class picker lists the asset class history's classes, cash among
# them: a cash balance is the class `cash` in every holdings view.
for _mode, _d in (("range", "Wealth Overview"), ("asof", "Allocation"), ("range", "Exposure")):
    _ac = [x for x in p.dashboard_parameters(MID, _mode, _d) if x["slug"] == "asset_class"][0]
    check(f"'{_d}' lists its asset classes off the asset class history",
          _ac["values_source_config"]["card_id"] == MID["report_asset_classes_history"], _ac)
_wccy = p.dashboard_parameters(MID, "range", "Wealth Overview")[0]
check("that currency picker is required, defaults to USD and offers every "
      "reporting currency",
      _wccy["required"] is True and _wccy["default"] == ["USD"]
      and _wccy["values_source_config"]["values"] == list(p.REPORTING_CURRENCIES),
      _wccy)

# ---- the twin never renders a counterparty ----------------------------

section("privacy twin: redaction")
REDACTED = ("account_label", "display_name", "account_external_id")
for card, *_ in DEFS["Spending (privacy)"][3]:
    query = CARDS[card][2]
    sql = sql_of(query)
    blob = sql if sql is not None else json.dumps(query)
    leaks = [c for c in REDACTED if c in blob]
    check(f"'{card}' renders no account label", not leaks, ", ".join(leaks))
    # merchant_name may decide the ROWS (the merchant list groups by it,
    # and ranks only lines that carry one) but must never reach a
    # projection: strip the two sanctioned forms and nothing may remain,
    # wherever the line breaks happen to fall. A new use has to widen the
    # allowlist deliberately.
    rest = re.sub(r"GROUP BY merchant_name\b|merchant_name IS NOT NULL",
                  "", blob)
    i = rest.find("merchant_name")
    check(f"'{card}' renders no merchant name", i < 0,
          rest[max(0, i - 60):i + 60].strip() if i >= 0 else "")

check("the twin's denominators are computed in-query",
      all(kw in sql_of(CARDS[c][2])
          for c, kw in (("Spending by category (privacy)", "SELECT CASE WHEN sum(v)"),
                        ("Spending by month (privacy)", "peak"),
                        ("Spend — monthly trend (privacy)", "peak"))))
check("the twin's scalar is a scale-free ratio over the _pct model",
      CARDS["Net spend (privacy)"][2]["query"]["source-table"]
      == f"card__{MID['report_spending_pct']}")

# ---- native picker wiring ---------------------------------------------

section("native card picker wiring")
targets = p.NATIVE_PARAM_TARGETS
check("the currency picker reaches a native card as a VARIABLE "
      "(a template variable names a value, never a column)",
      (p.SPEND_CURRENCY_PARAM_ID, ["variable", ["template-tag", "currency"]])
      in targets["Top 50 merchants (privacy)"])
check("the field filters reach it as dimensions",
      all((pid, ["dimension", ["template-tag", tag]])
          in targets["Top 50 merchants (privacy)"]
          for pid, tag in ((p.TIME_PARAM_ID, "time_range"),
                           (p.SOURCE_PARAM_ID, "source"),
                           (p.CATEGORY_PARAM_ID, "category"))))
check("no privacy card declares an account field filter (its widget "
      "would list the labels the projections drop)",
      not [c for c, ts in targets.items() if c.endswith(p.PRIVACY_SUFFIX)
           and p.ACCOUNT_PARAM_ID in [pid for pid, _ in ts]],
      str([c for c, ts in targets.items() if c.endswith(p.PRIVACY_SUFFIX)
           and p.ACCOUNT_PARAM_ID in [pid for pid, _ in ts]]))
check("the money card-balances card keeps its account filter",
      p.ACCOUNT_PARAM_ID in [pid for pid, _ in targets["Card balances over time"]])
check("the card-balances tiles take no category picker (no such dimension)",
      all(p.CATEGORY_PARAM_ID not in [pid for pid, _ in targets[c]]
          for c in ("Card balances over time",
                    "Card balances over time (privacy)")))
check("a privacy chart maps its field filters as dimensions and the "
      "currency as a variable",
      sorted(targets["Net worth over time (privacy)"]) == sorted([
          (p.WEALTH_CURRENCY_PARAM_ID, ["variable", ["template-tag", "currency"]]),
          (p.TIME_PARAM_ID, ["dimension", ["template-tag", "time_range"]]),
          (p.SOURCE_PARAM_ID, ["dimension", ["template-tag", "source"]]),
          (p.WEALTH_TAX_WRAPPER_PARAM_ID, ["dimension", ["template-tag", "tax_wrapper"]]),
          (p.ASSET_PARAM_ID, ["dimension", ["template-tag", "asset_class"]])]),
      targets["Net worth over time (privacy)"])

# An unsynced column drops its filter rather than emitting a {{tag}} the
# query never declares.
saved = dict(p.FIELD_IDS)
p.FIELD_IDS = {k: v for k, v in saved.items() if k != ("web_spending", "spend_primary_label")}
degraded = p.spending_privacy_defs(1, MID)
check("an unsynced filter column leaves its clause out of the SQL",
      "{{category}}" not in sql_of(degraded["Top 50 merchants (privacy)"][3]))
check("...and leaves its picker unmapped",
      p.CATEGORY_PARAM_ID not in
      [pid for pid, _ in p.NATIVE_PARAM_TARGETS["Top 50 merchants (privacy)"]])
p.FIELD_IDS = saved
all_cards(1, MID)                           # restore the registry

# ---- a snapshot that predates migration 0043 --------------------------

section("missing serving views abort the run")


class FakeAPI:
    """A stand-in for provision.req: answers the metadata and view
    probes, and records every call so the test can assert what did NOT
    happen (a sync must not be attempted for a stale snapshot)."""

    def __init__(self, views, tables, version=None):
        self.views, self.tables, self.calls = views, tables, []
        self.version = p.MIN_GOLD_SCHEMA if version is None else version

    def __call__(self, base, path, method="GET", data=None, session=None,
                 timeout=30):
        self.calls.append((method, path))
        if path.endswith("/metadata"):
            return 200, {"tables": self.tables}
        if path == "/api/dataset":
            if self.views is None:                      # probe unanswerable
                return 500, {}
            if "schema_meta" in data["native"]["query"]:
                return 202, {"data": {"rows": [[self.version]]}}
            return 202, {"data": {"rows": [[v] for v in self.views]}}
        return 200, {}


class FlakyMetadataAPI(FakeAPI):
    '''FakeAPI whose metadata answers once and then goes unreadable —
    the transient failure ensure_synced must not read as "gold has no
    columns".'''

    def __call__(self, base, path, method="GET", data=None, session=None,
                 timeout=30):
        if path.endswith("/metadata"):
            self.calls.append((method, path))
            if self.tables is None:
                return 500, {}
            answered, self.tables = self.tables, None
            return 200, {"tables": answered}
        return super().__call__(base, path, method, data, session, timeout)


class FakeMetabase(FakeAPI):
    """FakeAPI that also answers the steps main() takes before the schema
    check: health, an admin already set up, its login and the gold
    database."""

    def __call__(self, base, path, method="GET", data=None, session=None,
                 timeout=30):
        if path == "/api/session/properties":
            self.calls.append((method, path))
            return 200, {"has-user-setup": True}
        if path == "/api/session":
            self.calls.append((method, path))
            return 200, {"id": "sid"}
        if path == "/api/database" and method == "GET":
            self.calls.append((method, path))
            return 200, {"data": [{"id": 1, "name": "gold", "engine": "duckdb",
                                   "details": {}}]}
        return super().__call__(base, path, method, data, session, timeout)


def run_main():
    """main()'s exit status against the stand-in in p.req, with the
    arguments web/web passes; the globals main() sets are restored."""
    saved = (sys.argv, os.environ.get("WEALTHDB_WEB_ADMIN_PASSWORD"),
             p.DEFAULT_CURRENCY, p.DEFAULT_MISSING_BASIS)
    sys.argv = ["provision.py", "--base", "http://127.0.0.1:3000",
                "--email", "admin@wealthdb.local", "--gold-path", "/gold/wealthdb.db"]
    os.environ["WEALTHDB_WEB_ADMIN_PASSWORD"] = "Synthetic-pw-1!"
    try:
        with quiet():
            return p.main()
    finally:
        sys.argv, pw, p.DEFAULT_CURRENCY, p.DEFAULT_MISSING_BASIS = saved
        if pw is None:
            os.environ.pop("WEALTHDB_WEB_ADMIN_PASSWORD", None)
        else:
            os.environ["WEALTHDB_WEB_ADMIN_PASSWORD"] = pw


def tables_for(views):
    """Synced metadata carrying every registry column of `views`."""
    return [{"name": t, "fields": [{"name": c, "id": 900 + i}
                                   for i, c in enumerate(cols)]}
            for t, cols in p.FILTER_FIELD_COLUMNS.items() if t in views]


ALL_VIEWS = p.web_views_wanted()
PRE_0043 = [v for v in ALL_VIEWS if v not in
            ("web_spending", "web_card_balances_history")]
real_req = p.req
try:
    p.req = FakeAPI(PRE_0043, tables_for(PRE_0043 + ["report_returns"]))
    check("a pre-0043 snapshot reports both spending views missing",
          p.missing_web_views("b", "s", 1) ==
          ["web_card_balances_history", "web_spending"])
    api = p.req = FakeAPI(PRE_0043, tables_for(PRE_0043 + ["report_returns"]))
    with quiet():
        aborted = p.ensure_synced("b", "s", 1, tries=1, delay=0) is None
    check("ensure_synced aborts rather than half-provisioning", aborted)
    check("...without triggering a schema sync it cannot be fixed by",
          not [c for c in api.calls if "sync_schema" in c[1]],
          str(api.calls))

    p.req = FakeAPI(None, tables_for(PRE_0043 + ["report_returns"]))
    check("an unanswerable probe is read as 'missing everything'",
          p.missing_web_views("b", "s", 1) == ALL_VIEWS)

    p.req = FakeAPI(ALL_VIEWS + ["web_something_else"],
                    tables_for(ALL_VIEWS + ["report_returns"]))
    check("a fully migrated snapshot reports nothing missing",
          p.missing_web_views("b", "s", 1) == [])
    with quiet():
        proceeds = p.ensure_synced("b", "s", 1, tries=1, delay=0) is not None
    check("...and provisioning proceeds", proceeds)

    # The probe must name the views, not count them: extra web_* views
    # in gold cannot stand in for a missing one.
    p.req = FakeAPI(PRE_0043 + ["web_extra_one", "web_extra_two"],
                    tables_for(PRE_0043 + ["report_returns"]))
    check("unrelated web_* views cannot pad the count",
          p.missing_web_views("b", "s", 1) ==
          ["web_card_balances_history", "web_spending"])

    # A sync that has not landed when the budget runs out leaves columns
    # missing, and a card built from that resolves no field id for them —
    # dropping every [[AND {{tag}}]] clause and PUTting the filter-less
    # definition over one that works. Same outcome as the two guards above,
    # so it ends the same way: abort. Distinguished from the stale-snapshot
    # abort by having actually attempted the sync.
    api = p.req = FlakyMetadataAPI(ALL_VIEWS,
                                   tables_for(PRE_0043 + ["report_returns"]))
    real_sleep, p.time.sleep = p.time.sleep, lambda *_: None
    try:
        with quiet():
            exhausted = p.ensure_synced("b", "s", 1, tries=1, delay=0)
    finally:
        p.time.sleep = real_sleep
    check("a sync still incomplete at the end of the budget aborts rather "
          "than provisioning cards with no filters", exhausted is None)
    check("...having actually tried the sync, unlike the stale-snapshot abort",
          bool([c for c in api.calls if "sync_schema" in c[1]]),
          str(api.calls))

    # A snapshot older than the cards' columns carries every view by name,
    # so the view probe passes it; only the schema version tells it apart.
    # Provisioning onto it would rewrite every money card to read columns
    # the snapshot lacks.
    api = p.req = FakeAPI(ALL_VIEWS, tables_for(ALL_VIEWS + ["report_returns"]),
                          version=p.MIN_GOLD_SCHEMA - 1)
    with quiet():
        old_schema = p.check_gold_schema("b", "s", 1)
    check("a snapshot older than the cards' schema fails with a status of its own",
          old_schema == p.EXIT_STALE_SNAPSHOT, old_schema)
    check("...which no other failure (1) or a usage error (2) exits with",
          p.EXIT_STALE_SNAPSHOT not in (0, 1, 2), p.EXIT_STALE_SNAPSHOT)
    check("...found before reading the metadata or attempting a sync",
          [c for c in api.calls if c[1] == "/api/dataset"] == api.calls, str(api.calls))
    p.req = FakeAPI(ALL_VIEWS, tables_for(ALL_VIEWS + ["report_returns"]))
    check("a snapshot at the cards' schema passes", p.check_gold_schema("b", "s", 1) == 0)
    p.req.version = "unreadable"
    with quiet():
        unknown = p.check_gold_schema("b", "s", 1)
    check("a schema version the probe cannot read is an ordinary failure",
          unknown == 1, unknown)

    # The invariant that abort protects: a metadata read that fails says
    # nothing about the schema, and must never be coerced to an empty table
    # list — that would resolve no field ids at all.
    p.req = FakeAPI(ALL_VIEWS, None)
    check("a failed metadata read is never coerced to an empty table list",
          p.gold_metadata("b", "s", 1) is None)

    # web/web reads main()'s exit status: the stale one names `wealthdb
    # web refresh`, any other the browser wizard.
    api = p.req = FakeMetabase(ALL_VIEWS, tables_for(ALL_VIEWS + ["report_returns"]),
                               version=p.MIN_GOLD_SCHEMA - 1)
    check("provisioning onto an older snapshot exits with the stale status",
          run_main() == p.EXIT_STALE_SNAPSHOT)
    check("...having synced and written nothing",
          not [c for c in api.calls
               if "sync_schema" in c[1] or "/metadata" in c[1] or c[1].startswith("/api/card")
               or c[1].startswith("/api/collection")], str(api.calls))
    api = p.req = FakeMetabase(ALL_VIEWS, tables_for(ALL_VIEWS + ["report_returns"]))
    api.version = "unreadable"
    check("...and onto one whose version cannot be read with 1, at the probe",
          run_main() == 1 and ("POST", "/api/dataset") in api.calls, str(api.calls))
finally:
    p.req = real_req

# ---- the (uncategorized) label is a cross-repo contract -----------------

section("gold's spending label and the provisioner's constant")

# The provisioner does not merely DISPLAY this label, it FILTERS on it
# (the "Uncategorized share" card). Re-spelling either side would make
# that card return zero and read as "nothing is uncategorised" — the most
# reassuring possible way to be wrong. Scanning every migration rather
# than one by number keeps this correct when a later migration re-issues
# the view; set equality fails from either direction, and from a third
# spelling appearing.
_REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
GOLD_MIGRATIONS = os.path.join(_REPO, "wealthdb", "internal", "gold", "migrations")
MIGRATION_SQL = "\n".join(
    open(os.path.join(GOLD_MIGRATIONS, f), encoding="utf-8").read()
    for f in sorted(os.listdir(GOLD_MIGRATIONS)) if f.endswith(".sql"))
COALESCED = set(re.findall(
    r"COALESCE\(\s*(?:CASE\b[^()]*?END|spend_(?:primary|detailed))\s*,\s*"
    r"'([^']*)'\s*\)", MIGRATION_SQL, re.S))
check("the migrations really do render a label for an unresolved category",
      bool(COALESCED), "the COALESCE shape moved; this check went vacuous")
check("gold's spending views render exactly the label the "
      "uncategorized-share card filters on",
      COALESCED == {p.UNCATEGORIZED},
      f"{COALESCED} != {{{p.UNCATEGORIZED!r}}}")

# The same duplicate-with-no-test shape: the flow charts fence account
# kinds by a Python copy of gold's account_kind vocabulary, and a kind
# spelled differently there fences nothing at all — silently.
# The table is recreated by several migrations (DuckDB cannot alter a
# CHECK in place), so the LAST one in migration order is the vocabulary
# in force.
KIND_VOCAB = set(re.findall(r"'([a-z_]+)'", re.findall(
    r"account_kind\s+TEXT\s+NOT NULL CHECK \(account_kind IN \(([^)]*)\)",
    MIGRATION_SQL, re.S)[-1]))
check("every account kind the flow charts fence out is one gold can store",
      set(p.FLOW_CHART_EXCLUDED_ACCOUNT_KINDS) <= KIND_VOCAB,
      f"{p.FLOW_CHART_EXCLUDED_ACCOUNT_KINDS} not all in {sorted(KIND_VOCAB)}")

# The same for the transaction kinds the fees-and-taxes charts select: a
# kind renamed in the engine would empty the tile with no error.
_CANONICAL = os.path.join(_REPO, "wealthdb", "internal", "canonical")


def _go_consts(file, type_name):
    """The values of the `Name Type = "value"` constants in a canonical
    Go file (goref.go_consts)."""
    text = open(os.path.join(_CANONICAL, file), encoding="utf-8").read()
    return set(goref.go_consts(text, type_name).values())


def _go_package(pkg):
    """The source of Go package `pkg` under wealthdb/internal, its tests
    left out, so a declaration is found in whichever file holds it."""
    d = os.path.join(_REPO, "wealthdb", "internal", pkg)
    return "\n".join(open(os.path.join(d, f), encoding="utf-8").read()
                     for f in sorted(os.listdir(d)) if f.endswith(".go") and not f.endswith("_test.go"))


# The demo's reader of the engine's Go source, which parses a slice and
# its constants.
_goref_spec = importlib.util.spec_from_file_location("goref", os.path.join(_REPO, "demo", "tests", "goref.py"))
goref = importlib.util.module_from_spec(_goref_spec)
_goref_spec.loader.exec_module(goref)


def _go_list(text, var, type_name=None):
    """The values of Go slice `var` in `text` (goref.go_slice), its names
    read through the `type_name` constants; None where `text` declares
    no such slice."""
    try:
        return goref.go_slice(text, var, type_name and goref.go_consts(text, type_name))
    except (AttributeError, KeyError):
        return None


_TX_KINDS = _go_consts("enums.go", "TxKind")
check("every cost kind the fees-and-taxes charts select is an engine transaction kind",
      bool(_TX_KINDS) and set(p.COST_KINDS) <= _TX_KINDS,
      f"{p.COST_KINDS} not all in {sorted(_TX_KINDS)}")

# The fence is written the long way ON PURPOSE, in both the MBQL and the
# native form: a bare `!=` drops the rows whose account_kind is NULL — a
# transaction with no matching accounts row — and those are flows the chart
# is supposed to show. The NULL-keeping branch is the whole reason it is not
# a one-liner, so a future simplification has to fail here.
for _n in ("Fees & taxes by month", "Fees & taxes by month (privacy)"):
    check(f"'{_n}' keeps rows with a NULL account_kind",
          "account_kind IS NULL OR account_kind NOT IN"
          in (sql_of(CARDS[_n][2]) or ""),
          "the fence dropped its null branch; a bare NOT IN drops unmatched rows")

# The guard's version is a migration that exists, and no older than the
# newest one that (re)defines an object the cards read: a web_* view in
# the registry or a serving_* table a model names. A snapshot older than
# that can carry every view by name; only the version tells it apart.
_MIG_FILES = sorted(f for f in os.listdir(GOLD_MIGRATIONS) if f.endswith(".sql"))
check("the cards' minimum gold schema is a migration on disk",
      any(f.startswith(f"{p.MIN_GOLD_SCHEMA:04d}_") for f in _MIG_FILES),
      p.MIN_GOLD_SCHEMA)
_MIG_TEXT = {int(f[:4]): open(os.path.join(GOLD_MIGRATIONS, f), encoding="utf-8").read()
             for f in _MIG_FILES}
_SERVED = sorted(set(re.findall(r"\bserving_\w+", "\n".join(
    sql for sql, _d in MODELS.values()))))


def _defined_in(obj):
    """The migrations that create or redefine view or table `obj`."""
    pat = rf"CREATE (?:OR REPLACE )?(?:VIEW|TABLE) (?:IF NOT EXISTS )?{obj}\b"
    return [n for n, text in _MIG_TEXT.items() if re.search(pat, text)]


_NEWEST = {o: max(_defined_in(o), default=None) for o in p.web_views_wanted() + _SERVED}
check("the cards' minimum gold schema is no older than any view or table they read",
      bool(_SERVED) and all(n is not None and n <= p.MIN_GOLD_SCHEMA for n in _NEWEST.values()),
      {o: n for o, n in _NEWEST.items() if n is None or n > p.MIN_GOLD_SCHEMA})
# The startup probe and that check read the registry, so a view a card
# reads and the registry leaves out would go unprobed.
_READ_SQL = [sql_of(q) or "" for _d, _desc, q in CARDS.values()] + [sql for sql, _d in MODELS.values()]
_READ_VIEWS = {v for q in _READ_SQL for v in re.findall(r"\b(?:FROM|JOIN) (web_\w+)\b", q)}
check("the registry names exactly the web_* views the cards and models read",
      _READ_VIEWS == set(p.web_views_wanted()), sorted(_READ_VIEWS ^ set(p.web_views_wanted())))


def _view_body(view):
    """The newest migration's CREATE statement for `view`, up to its `;`."""
    text = _MIG_TEXT.get(_NEWEST.get(view), "")
    m = re.search(rf"CREATE (?:OR REPLACE )?VIEW {view} AS(.*?);", text, re.S)
    return m.group(1) if m else ""


# The gains and exposure views read a missing tax wrapper as
# taxable_personal, as the holdings views do, so a card reads the column
# as it stands and the Tax wrapper picker offers one value for it.
_FILLED = ("web_gains", "web_exposure")
check("the gains and exposure views fill a missing tax wrapper in",
      all(re.search(r"COALESCE\(\w+\.tax_wrapper, 'taxable_personal'\) AS tax_wrapper",
                    _view_body(v)) for v in _FILLED),
      [v for v in _FILLED if "taxable_personal" not in _view_body(v)])
_over_filled = {n: sql_of(q) for n, (_d, _desc, q) in CARDS.items()
                if sql_of(q) and any(f"FROM {v}" in sql_of(q) for v in _FILLED)}
check("...so no card over them fills it in again",
      bool(_over_filled) and not [n for n, q in _over_filled.items()
                                  if re.search(r"coalesce\(\s*tax_wrapper", q, re.I)],
      [n for n, q in _over_filled.items() if re.search(r"coalesce\(\s*tax_wrapper", q, re.I)])

# ---- the dashboard PUT payloads ---------------------------------------

section("dashboard layout payload")


class FakeDashboardAPI:
    def __init__(self):
        self.puts, self.next_id = [], 100

    def __call__(self, base, path, method="GET", data=None, session=None,
                 timeout=30):
        if method == "GET":
            return 200, {"data": []}
        if method == "POST" and path == "/api/dashboard":
            self.next_id += 1
            return 200, {"id": self.next_id}
        if method == "PUT":
            self.puts.append((path, data))
        return 200, {}


card_ids = {name: 500 + i for i, name in enumerate(sorted(CARDS))}
real_req = p.req
try:
    api = p.req = FakeDashboardAPI()
    with quiet():
        rc = p.ensure_dashboards("b", "s", 7, card_ids, MID)
    check("ensure_dashboards lays out every dashboard", rc == 0)
finally:
    p.req = real_req

layouts = [d for _path, d in api.puts if d.get("dashcards")]

# Where the snapshot holds no withholding table, the Withholding tiles
# read the tax booked alone, and their titles say so; a probe that does
# not answer keeps the cards' names.


class FakeProbeAPI:
    def __init__(self, status, rows=None):
        self.status, self.rows, self.queries = status, rows, []

    def __call__(self, base, path, method="GET", data=None, session=None, timeout=30):
        self.queries.append(data["native"]["query"])
        return self.status, {"data": {"rows": self.rows}}


real_req = p.req
try:
    _probed = {}
    for _label, _status, _rows in (("a rate", 202, [[True]]), ("no rate", 202, [[False]]),
                                   ("no answer", 500, None)):
        api = p.req = FakeProbeAPI(_status, _rows)
        _probed[_label] = p.withholding_table("b", "s", 1)
    check("the withholding probe reads whether the withholding table holds any rate",
          _probed == {"a rate": True, "no rate": False, "no answer": None}
          and api.queries == ["SELECT count(*) > 0 FROM yield_withholding"], _probed)
    _titles = p.tile_titles(False)
    check("...and without a table the Withholding tiles' titles say they read what was received",
          _titles == {"Withholding": "Received withholding — no withholding table",
                      "Income after withholding": "Income after received withholding",
                      "Withholding (privacy)": "Received withholding — no withholding table (privacy)",
                      "Income after withholding (privacy)": "Income after received withholding (privacy)"}
          and p.tile_titles(True) == {} and p.tile_titles(None) == {}, _titles)
    api = p.req = FakeDashboardAPI()
    with quiet():
        p.ensure_dashboards("b", "s", 7, card_ids, MID, _titles)
    _titled = {dc["card_id"]: dc["visualization_settings"].get("card.title")
               for _path, d in api.puts if d.get("name") in ("Income", "Income (privacy)")
               for dc in d.get("dashcards", []) if dc["visualization_settings"].get("card.title")}
    check("...which the tiles show in place of the cards' names, and no other tile changes",
          _titled == {card_ids[c]: t for c, t in _titles.items()}
          and [d for _path, d in api.puts if d.get("dashcards")] != layouts
          and all(d == layouts[k] or d.get("name") in ("Income", "Income (privacy)")
                  for k, d in enumerate(x for _path, x in api.puts if x.get("dashcards"))), _titled)
finally:
    p.req = real_req

spending = [d for d in layouts if d.get("name") == "Spending"]
check("the Spending dashboard was laid out", len(spending) == 1)
if spending:
    body = spending[0]
    check("its five pickers are attached",
          [x["id"] for x in body["parameters"]] ==
          [p.SPEND_CURRENCY_PARAM_ID, p.TIME_PARAM_ID, p.SOURCE_PARAM_ID,
           p.ACCOUNT_PARAM_ID, p.CATEGORY_PARAM_ID])
    tiles = [dc for dc in body["dashcards"] if dc["card_id"] is not None]
    check("every tile plus the switch link is present",
          len(tiles) == 10 and len(body["dashcards"]) == 11)
    ALL_CCY_IDS = {card_ids[c] for c in p.SPEND_ALL_CURRENCY_CARDS}
    check("every picker lands on every tile",
          all({m["parameter_id"] for m in dc["parameter_mappings"]} ==
              {p.SPEND_CURRENCY_PARAM_ID, p.TIME_PARAM_ID, p.SOURCE_PARAM_ID,
               p.ACCOUNT_PARAM_ID, p.CATEGORY_PARAM_ID}
              for dc in tiles
              if dc["card_id"] != card_ids["Card balances over time"]
              and dc["card_id"] not in ALL_CCY_IDS),
          json.dumps([[m["parameter_id"] for m in dc["parameter_mappings"]]
                      for dc in tiles]))
    # A tile carrying every currency as its own column is the exception:
    # the Currency picker is a row filter on the long model, so wiring it
    # would empty every column but the one the picker selects.
    for dc in (dc for dc in tiles if dc["card_id"] in ALL_CCY_IDS):
        check("an all-currency tile takes every picker but Currency",
              {m["parameter_id"] for m in dc["parameter_mappings"]} ==
              {p.TIME_PARAM_ID, p.SOURCE_PARAM_ID,
               p.ACCOUNT_PARAM_ID, p.CATEGORY_PARAM_ID})
    balances = [dc for dc in tiles
                if dc["card_id"] == card_ids["Card balances over time"]][0]
    check("the card-balances tile takes every picker but the category one",
          {m["parameter_id"] for m in balances["parameter_mappings"]} ==
          {p.SPEND_CURRENCY_PARAM_ID, p.TIME_PARAM_ID, p.SOURCE_PARAM_ID,
           p.ACCOUNT_PARAM_ID})

twin = [d for d in layouts if d.get("name") == "Spending (privacy)"]
check("the Spending twin was laid out", len(twin) == 1)
if twin:
    body = twin[0]
    check("the twin's filter bar carries no account picker",
          p.ACCOUNT_PARAM_ID not in [x["id"] for x in body["parameters"]])
    check("...and no tile maps one — including the privacy-exempt tile, "
          "whose MBQL mapping would otherwise name a filter that is gone",
          not [dc for dc in body["dashcards"]
               if any(m["parameter_id"] == p.ACCOUNT_PARAM_ID
                      for m in dc["parameter_mappings"])])
    check("every mapping on the twin names a picker it actually carries",
          all(m["parameter_id"] in {x["id"] for x in body["parameters"]}
              for dc in body["dashcards"] for m in dc["parameter_mappings"]))

# Every Wealth Overview, Allocation and Exposure tile, on the base and on
# the twin, answers every picker its dashboard carries — the currency one
# above all, since a tile it misses silently keeps showing USD.
# The Overview's flow tiles take the tax wrapper but not the asset
# class (a fee or an interest line often names no instrument); the
# Top-positions tiles take the vehicle as well.
_TOP = {card_ids[c] for c in p.POSITION_FILTERED_CARDS}
_FLOWS = {card_ids[c + s] for c in ("Investment income by month", "Fees & taxes by month")
          for s in ("", p.PRIVACY_SUFFIX)}
_SELECTION = {p.WEALTH_TAX_WRAPPER_PARAM_ID, p.ASSET_PARAM_ID}
# The debt view carries no asset class, the issuers view neither a source
# nor a tax wrapper: those tiles take what their views can narrow by.
_DEBT = {card_ids[c + s] for c in ("Debt by credit rating, share of debt and cash", "Debt by maturity",
                                   "Debt by coupon kind", "Public equity by cycle")
         for s in ("", p.PRIVACY_SUFFIX)}
_ISSUERS = {card_ids["Largest issuers" + s] for s in ("", p.PRIVACY_SUFFIX)}
for _d, _want in (("Wealth Overview", {p.WEALTH_CURRENCY_PARAM_ID,
                                       p.TIME_PARAM_ID, p.SOURCE_PARAM_ID} | _SELECTION),
                  ("Allocation", {p.WEALTH_CURRENCY_PARAM_ID,
                                  p.ASOF_PARAM_ID, p.SOURCE_PARAM_ID} | _SELECTION),
                  ("Exposure", {p.EXPOSURE_CURRENCY_PARAM_ID, p.TIME_PARAM_ID, p.SOURCE_PARAM_ID,
                                p.EXPOSURE_TAX_WRAPPER_PARAM_ID, p.EXPOSURE_ASSET_CLASS_PARAM_ID})):
    for _name in (_d, _d + p.PRIVACY_SUFFIX):
        _body = [d for d in layouts if d.get("name") == _name][0]
        _tiles = [dc for dc in _body["dashcards"] if dc["card_id"] is not None]

        def _expected(cid, want=_want):
            if cid in _TOP:
                return want | {p.VEHICLE_PARAM_ID}
            if cid in _FLOWS:
                return want - {p.ASSET_PARAM_ID}
            if cid in _DEBT:
                return want - {p.EXPOSURE_ASSET_CLASS_PARAM_ID}
            if cid in _ISSUERS:
                return {p.EXPOSURE_CURRENCY_PARAM_ID, p.TIME_PARAM_ID}
            return want
        _off = [dc["card_id"] for dc in _tiles
                if {m["parameter_id"] for m in dc["parameter_mappings"]} != _expected(dc["card_id"])]
        check(f"every '{_name}' tile answers every picker the dashboard carries",
              bool(_tiles) and not _off, _off)
        check(f"on '{_name}', only the Top-positions tile carries a filter of its own, the vehicle",
              all(dc.get("inline_parameters") == ([p.VEHICLE_PARAM_ID] if dc["card_id"] in _TOP else [])
                  for dc in _tiles),
              [(dc["card_id"], dc.get("inline_parameters")) for dc in _tiles if dc.get("inline_parameters")])

# Each tile's SQL holds the clauses its pickers land on: a picker
# mapped onto a tag the SQL never references filters nothing.
_WA = {n for d in ("Wealth Overview", "Allocation") for s in ("", p.PRIVACY_SUFFIX)
       for n, *_ in DEFS[d + s][3]}
_FLOW_NAMES = {c + s for c in ("Investment income by month", "Fees & taxes by month")
               for s in ("", p.PRIVACY_SUFFIX)}
_wa_sql = {n: sql_of(CARDS[n][2]) or "" for n in _WA}
check("every Wealth Overview and Allocation tile narrows to the tax wrapper",
      all("[[AND {{tax_wrapper}}]]" in q for q in _wa_sql.values()),
      [n for n, q in _wa_sql.items() if "{{tax_wrapper}}" not in q])
check("every holdings tile narrows to the asset class, and no flow tile does",
      all(("[[AND {{asset_class}}]]" in q) != (n in _FLOW_NAMES) for n, q in _wa_sql.items()),
      [n for n, q in _wa_sql.items() if ("{{asset_class}}" in q) == (n in _FLOW_NAMES)])

# The history and latest models read the tables `web refresh` fills, not
# the macros: a macro computes every day of history on each read.
_MODELS_SQL = {n: sql for n, (sql, _d) in p.report_models().items()}
check("no report model reads a history macro",
      not any("_history_multi()" in q for q in _MODELS_SQL.values()),
      [n for n, q in _MODELS_SQL.items() if "_history_multi()" in q])
check("the accounts, positions and sources latest models and the transactions ones read their tables",
      all(f"FROM {t}" in _MODELS_SQL[m] for m, t in (
          ("report_accounts_latest", "serving_accounts_latest"),
          ("report_positions_latest", "serving_positions_latest"),
          ("report_sources_latest", "serving_sources_latest"),
          ("report_global_latest", "serving_global_latest"),
          ("report_portfolios_latest", "serving_portfolios_latest"),
          ("report_transactions", "serving_transactions"))))
# The tables are a cross-language contract: the models name them here,
# and web-materialize rewrites the ones serving_materialize.go lists. A
# table renamed on one side would break every model that reads it at
# runtime only.
_sm = open(os.path.join(_REPO, "wealthdb", "internal", "gold", "serving_materialize.go"),
           encoding="utf-8").read()
_sm_list = re.search(r"var servingTables = .*?\n\}", _sm, re.S)
_GO_SERVING = set(re.findall(r'\{"(serving_\w+)",', _sm_list.group(0))) if _sm_list else set()
check("every serving table a model reads is one web-materialize rewrites",
      bool(_SERVED) and set(_SERVED) <= _GO_SERVING,
      sorted(set(_SERVED) - _GO_SERVING) if _GO_SERVING else "servingTables not found")
check("...and one a migration creates",
      all(_defined_in(t) for t in _SERVED), [t for t in _SERVED if not _defined_in(t)])


# ---- Income dashboards ----------------------------------------------
section("Income dashboards")

INCOME_CARD_NAMES = ["Income — monthly trend", "Net income",
                     "Uncategorized income share", "Income by type",
                     "Top 50 payers", "Largest receipts"]

check("every Income tile is defined",
      all(n in CARDS for n in INCOME_CARD_NAMES),
      [n for n in INCOME_CARD_NAMES if n not in CARDS])

# Every income tile reads the serving view and takes the currency
# variable: a tile that summed every reporting currency would be
# silently and plausibly wrong.
_income_sql = {n: (sql_of(CARDS[n][2]) or "") for n in INCOME_CARD_NAMES if n in CARDS}
check("every Income tile is native over web_income",
      all("web_income" in q for q in _income_sql.values()),
      [n for n, q in _income_sql.items() if "web_income" not in q])
# Every MONEY tile reads the currency variable; the uncategorised
# share is a share of rows and is the same in every currency, so it
# declares no such variable rather than a required one it ignores.
check("every Income money tile reads the currency variable",
      all("{{currency}}" in q for n, q in _income_sql.items()
          if n != "Uncategorized income share"),
      [n for n, q in _income_sql.items()
       if n != "Uncategorized income share" and "{{currency}}" not in q])
check("the uncategorised-share tile declares no currency variable",
      "{{currency}}" not in _income_sql["Uncategorized income share"])
# ...and none of them negates: gold stores a receipt positive, and the
# negation is the spending family's alone.
_negating = [n for n, q in _income_sql.items()
             if any(f"-value_{c}" in q for c in p._ccy_lower())]
check("no Income tile negates the value", not _negating, _negating)

# The payer ranking excludes the lines that have no payer rather than
# grouping them under a blank.
check("Top 50 payers excludes lines with no payer",
      "payer_name IS NOT NULL" in _income_sql.get("Top 50 payers", ""))

# The rings: the money one keeps its total in the hole, the share one
# has none to keep.
check("the Income ring shows its total",
      _QDEFS["Income by type"][3].get("pie.show_total") is True,
      _QDEFS["Income by type"][3])
check("the Income share ring shows no total",
      _PDEFS["Income by type (privacy)"][4].get("pie.show_total") is False,
      _PDEFS["Income by type (privacy)"][4])

# The twin redacts by dropping, not disguising.
#
# Every NATIVE twin card, not just the income ones: a twin's SQL is
# where a column is dropped, and the check is the same question on both
# dashboards. MBQL twins are excluded because they have no SQL to read —
# they are redacted by running over a `_pct` model instead.
_twin_sql = {n: sql_of(CARDS[n][2]) for n in CARDS if n.endswith(p.PRIVACY_SUFFIX)}
_twin_sql = {n: q for n, q in _twin_sql.items() if q}
# Redaction is about the OUTPUT: a name may be a GROUP BY key inside a
# CTE — that is how the ranking keeps its shape — but no projection the
# card renders may carry one.
#
# Four things this reader has to get right:
#   - COMMENTS are stripped first, so `-- SELECT payer_name` neither
#     hides a projection nor invents one.
#   - SUBQUERIES are stripped, so a scan cannot land inside
#     `(SELECT total FROM t)` and read the projection as empty.
#   - EVERY top-level SELECT arm is read, not just the last: a UNION's
#     other arm renders too.
#   - `*` is REFUSED outright. `SELECT * FROM m` over a CTE grouped by
#     payer_name projects the name while naming no column, so no
#     column-name test can see it.


def _strip_sql_comments(q):
    """`q` with `--` line comments and `/* */` blocks removed."""
    q = re.sub(r"/\*.*?\*/", " ", q, flags=re.S)
    return "\n".join(line.split("--")[0] for line in q.splitlines())


def _strip_parens(q):
    """`q` with every parenthesised group removed, so a scan for a
    top-level SELECT cannot land inside a subquery."""
    out, depth = [], 0
    for ch in q:
        if ch == "(":
            depth += 1
        elif ch == ")":
            depth = max(0, depth - 1)
        elif depth == 0:
            out.append(ch)
    return "".join(out)


def _strip_subqueries(q):
    """`q` with only the parenthesised groups that CONTAIN a SELECT
    removed.

    _strip_parens is too blunt for reading a projection: it also deletes
    `upper(payer_name)` down to `upper`, and a column-name test over
    that sees nothing. A redacting twin that wrapped a name in any
    function would have passed. Ordinary calls are therefore kept and
    only real subqueries dropped."""
    out, buf, depth = [], [], 0
    for ch in q:
        if ch == "(":
            depth += 1
            buf.append(ch)
        elif ch == ")":
            buf.append(ch)
            depth -= 1
            if depth == 0:
                group = "".join(buf)
                # A subquery is dropped; any other call is kept whole.
                out.append(" " if re.search(r"\bSELECT\b", group, re.I) else group)
                buf = []
        elif depth > 0:
            buf.append(ch)
        else:
            out.append(ch)
    return "".join(out) + "".join(buf)


def _projections(q):
    """Every top-level SELECT's projection list — the text between each
    SELECT and the FROM/ORDER/GROUP that ends it, outside parentheses:
    a window's `OVER (ORDER BY ...)` ends nothing.

    Subqueries are dropped first so a scan cannot land inside one, and
    ordinary function calls are kept so a name wrapped in one is still
    visible. A query with no top-level SELECT left yields nothing, which
    is a shape no card here has and which the vacuity guard below
    catches."""
    flat = _strip_subqueries(_strip_sql_comments(q))
    out = []
    for m in re.finditer(r"\bSELECT\b", flat, re.I):
        tail = flat[m.end():]
        end, depth = len(tail), 0
        for k in re.finditer(r"[()]|\b(?:FROM|ORDER\s+BY|GROUP\s+BY|LIMIT|UNION)\b", tail, re.I):
            depth += {"(": 1, ")": -1}.get(k.group(0), 0)
            if depth == 0 and k.group(0) not in "()":
                end = k.start()
                break
        out.append(tail[:end])
    return out


def _ctes(q):
    """The top-level CTEs of `q`, name to body: `WITH a AS (...), b AS
    (...)` before the query that reads them."""
    flat = _strip_sql_comments(q)
    m = re.match(r"\s*WITH\b", flat, re.I)
    out, i = {}, m.end() if m else None
    while i is not None:
        head = re.compile(r"\s*([A-Za-z_]\w*)\s+AS\s*\(", re.I).match(flat, i)
        if not head:
            break
        depth, j = 1, head.end()
        while depth and j < len(flat):
            depth += {"(": 1, ")": -1}.get(flat[j], 0)
            j += 1
        out[head.group(1)] = flat[head.end():j - 1]
        comma = re.compile(r"\s*,").match(flat, j)
        i = comma.end() if comma else None
    return out


_CTE_STAR = re.compile(r"\s*([A-Za-z_]\w*)\s*\.\s*\*\s*")


def _output_projections(q):
    """The projections whose columns reach a query's rows: its top-level
    ones, with a `cte.*` item read through to that CTE's own, so a
    wrapper that only orders a card's rows hides nothing."""
    ctes, out = _ctes(q), []
    for pr in _projections(q):
        out.append(pr)
        for item in pr.split(","):
            m = _CTE_STAR.fullmatch(item)
            if m and m.group(1) in ctes:
                out += _output_projections(ctes[m.group(1)])
    return out


def _blind_star(q):
    """True when a query's rows take columns it never names: a `*`, or
    an `alias.*` that is not one of its own CTEs read through."""
    ctes = _ctes(q)
    for pr in _projections(q):
        for item in pr.split(","):
            m = _CTE_STAR.fullmatch(item)
            if m and m.group(1) in ctes:
                if _blind_star(ctes[m.group(1)]):
                    return True
            elif _star_projection(item):
                return True
    return False


_REDACTED = ("payer_name", "account_label", "display_name", "account_external_id",
             "merchant_name", "merchant_signature", "payer_signature", "counterparty")


def _star_projection(proj):
    """True when a projection item is a bare `*` or `alias.*`.

    Read per ITEM, because `*` is also multiplication and every
    percentage card here divides by a total and multiplies by 100. A
    leading DISTINCT / ALL is stripped first: `SELECT DISTINCT *` over a
    CTE grouped by a name projects the name while naming no column,
    which is the whole reason `*` is refused."""
    for item in proj.split(","):
        bare = re.sub(r"^\s*(DISTINCT|ALL)\b", "", item, flags=re.I).strip()
        if bare == "*" or re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*\s*\.\s*\*", bare):
            return True
    return False


_leaks = {}
for _n, _q in _twin_sql.items():
    _projs = _output_projections(_q)
    _bad = [c for c in _REDACTED
            if any(re.search(rf"\b{c}\b", pr) for pr in _projs)]
    if _blind_star(_q):
        _bad.append("* (projects whatever the source carries)")
    if _bad:
        _leaks[_n] = _bad
check("no privacy twin card projects an identifying column", not _leaks, _leaks)

# The reader must actually be reading something. A projection extractor
# that silently returned [] would pass the check above on every card.
check("the redaction reader found a projection in every twin card",
      all(_projections(q) for q in _twin_sql.values()),
      [n for n, q in _twin_sql.items() if not _projections(q)])
check("...and it reads more than one arm where there is more than one",
      len(_projections("SELECT a FROM t UNION ALL SELECT b FROM u")) == 2,
      _projections("SELECT a FROM t UNION ALL SELECT b FROM u"))
check("...and reads past a window's own ORDER BY",
      any("v_pct" in pr for pr in _projections(
          "SELECT row_number() OVER (ORDER BY v DESC) AS rank, v AS v_pct FROM m")))
check("...and it does not read a commented-out one",
      _projections("-- SELECT payer_name\nSELECT rank FROM m") == [" rank "],
      _projections("-- SELECT payer_name\nSELECT rank FROM m"))
# A name wrapped in a function call is still a name. Dropping every
# parenthesised group would delete the argument along with the parens
# and see an empty projection.
check("...and it reads through a CTE whose rows the query takes with `cte.*`",
      any("payer_name" in pr for pr in _output_projections(
          "WITH q AS (SELECT payer_name, v FROM t), r AS (SELECT 1 AS k)\nSELECT q.* FROM q")),
      _output_projections("WITH q AS (SELECT payer_name, v FROM t), r AS (SELECT 1 AS k)\nSELECT q.* FROM q"))
check("...but not into a CTE it only joins or groups",
      not any("payer_name" in pr for pr in _output_projections(
          "WITH g AS (SELECT payer_name, sum(v) AS v FROM t GROUP BY 1)\nSELECT rank FROM g")))
check("...and a star over anything but its own CTE is blind",
      _blind_star("SELECT x.* FROM t x") and _blind_star("SELECT * FROM t")
      and not _blind_star("WITH q AS (SELECT a FROM t)\nSELECT q.* FROM q")
      and _blind_star("WITH q AS (SELECT * FROM t)\nSELECT q.* FROM q"))
check("...and it sees a column wrapped in a function call",
      any("payer_name" in pr for pr in _projections("SELECT upper(payer_name) AS p FROM m")),
      _projections("SELECT upper(payer_name) AS p FROM m"))
check("...and a scalar subquery is still dropped",
      not any("payer_name" in pr
              for pr in _projections("SELECT (SELECT max(payer_name) FROM m) AS x FROM m")),
      _projections("SELECT (SELECT max(payer_name) FROM m) AS x FROM m"))
for _star in ("SELECT * FROM m", "SELECT DISTINCT * FROM m", "SELECT m.* FROM m"):
    check(f"...and {_star!r} counts as a star projection",
          any(_star_projection(pr) for pr in _projections(_star)), _projections(_star))
check("...and arithmetic is not mistaken for one",
      not any(_star_projection(pr) for pr in _projections("SELECT v / t * 100 AS pct FROM m")),
      _projections("SELECT v / t * 100 AS pct FROM m"))
check("the Income twin has a card for every base tile",
      all(p.privacy_name(n) in CARDS for n in INCOME_CARD_NAMES
          if n not in p.PRIVACY_EXEMPT_CARDS),
      [n for n in INCOME_CARD_NAMES
       if n not in p.PRIVACY_EXEMPT_CARDS and p.privacy_name(n) not in CARDS])

# The dashboards and their pickers.
check("the Income dashboard and its twin are laid out",
      "Income" in DEFS and "Income (privacy)" in DEFS)
_income_pickers = [q["slug"] for q in p.dashboard_parameters(MID, "range", "Income")]
# One past control and one future control: the Time range sets every
# received figure, the Horizon every expected one.
check("the Income dashboard carries the currency, the time range and the horizon, then its "
      "selection and the projection's assumptions",
      _income_pickers == ["currency", "time_range", "horizon", "source", "account", "type",
                          "position", "rates", "maturing", "shift"],
      _income_pickers)
_twin_pickers = [q["slug"] for q in p.dashboard_parameters(MID, "range", "Income (privacy)")]
check("the Income twin carries no account picker",
      "account" not in _twin_pickers, _twin_pickers)
# The type picker binds to the DETAILED label: the income taxonomy has
# one vendored primary, and a primary-level dropdown would offer four
# values.
#
# A picker's dropdown is the values its value_field takes ON THE MODEL,
# so the binding has to name a column the model's SELECT actually
# projects. A substring test over the parameter dict would pass on any
# plausible-looking name — including one the model does not have, which
# leaves the dropdown silently empty — so the model's aliases are parsed
# and the binding checked against them.


def _model_columns(model):
    """Every column name model `model`'s native SELECT exposes: the
    trailing identifier of each projected expression, which for an
    aliased one is the alias."""
    head = _strip_parens(MODELS[model][0])
    head = head[head.index("SELECT") + len("SELECT"):]
    for kw in ("\n  FROM", "\nFROM", " FROM "):
        if kw in head:
            head = head[:head.index(kw)]
            break
    cols = []
    for part in head.split(","):
        toks = re.findall(r"[A-Za-z_][A-Za-z0-9_]*", part)
        if toks:
            cols.append(toks[-1])
    return cols


for _model, _slug, _dash_name in (("report_income", "type", "Income"),
                                  ("report_spending", "category", "Spending")):
    _picker = [q for q in p.dashboard_parameters(MID, "range", _dash_name)
               if q["slug"] == _slug][0]
    _field = _picker["values_source_config"]["value_field"][1]
    check(f"the {_dash_name} {_slug} picker binds to a column {_model} projects",
          _field in _model_columns(_model),
          (_field, _model_columns(_model)))

check("the Income type picker binds to the detailed label",
      [q for q in p.dashboard_parameters(MID, "range", "Income")
       if q["slug"] == "type"][0]["values_source_config"]["value_field"][1]
      == "income_detailed")

# Provisioning matches cards by name. ensure_cards merges the base and
# the privacy definitions into one name-keyed payload map, so a name both
# define leaves one card standing in for two — whichever was merged last
# — and a dashboard renders the wrong chart. The models share the
# collection's name map, so a card named like a model would overwrite it.
_base_names = set(_QDEFS)
_twin_names = set(_PDEFS)
_clashes = sorted((_base_names & _twin_names)
                  | ((_base_names | _twin_names) & set(MODELS)))
check("no card name is defined twice, nor shared with a model",
      bool(_base_names) and bool(_twin_names) and not _clashes, _clashes)

# No twin card may carry an account field filter. A native card's
# template tags render as widgets wherever it is opened, so a tag is a
# dropdown of its column's values — and the account labels are what the
# twin exists not to show. The picker mapping being filtered out at the
# dashboard is not enough: the card carries the tag either way.
# The population is the privacy dashboards' OWN TILE LISTS, not every
# card whose name ends in " (privacy)". The two differ: a
# PRIVACY-EXEMPT card keeps its base name and is placed on the twin
# unchanged (dashboard_defs maps a tile through privacy_name only when
# the name is not exempt), so a suffix-selected population would not
# contain it, and a native exempt card carrying an `account` field
# filter would reach a twin unseen.
_privacy_tiles = {}
for _dname, (_desc, _mode, _sib, _tiles) in p.dashboard_defs().items():
    if not _dname.endswith(p.PRIVACY_SUFFIX):
        continue
    for _c, *_ in _tiles:
        _privacy_tiles.setdefault(_c, set()).add(_dname)
check("every privacy tile names a card that exists",
      all(c in CARDS for c in _privacy_tiles),
      [c for c in _privacy_tiles if c not in CARDS])
_with_account = sorted(
    f"{c} (on {', '.join(sorted(_privacy_tiles[c]))})"
    for c in _privacy_tiles if c in CARDS
    and "account" in ((CARDS[c][2].get("native") or {}).get("template-tags") or {}))
check("no card on a privacy dashboard declares an account template tag",
      not _with_account, _with_account)
# ...and the population really does include the exempt cards, or the
# check above is back to reading only the suffixed ones.
check("the privacy tile population includes the exempt cards",
      any(c in p.PRIVACY_EXEMPT_CARDS for c in _privacy_tiles),
      sorted(_privacy_tiles))

# The Wealth Overview's income card reads the income base.
_wo = sql_of(CARDS["Investment income by month"][2]) or ""
check("the Wealth Overview income card reads web_income",
      "web_income" in _wo, _wo[:120])
check("...and names the four investment income types",
      all(t in _wo for t in p.INVESTMENT_INCOME_TYPES),
      [t for t in p.INVESTMENT_INCOME_TYPES if t not in _wo])
check("...and fences no card accounts",
      "account_kind" not in _wo, _wo[:200])
# ...each of them an income type the engine's taxonomy holds: a type
# renamed there would drop out of the chart with no error. The rows are
# read the way demo/tests/goref.py reads them.
_tax = open(os.path.join(_CANONICAL, "spendtaxonomy.go"), encoding="utf-8").read()
_tax_consts = dict(re.findall(r'(\w+)\s*=\s*"([^"]+)"', _tax))
_INCOME_TYPES = {
    d.strip('"') if d.startswith('"') else _tax_consts.get(d, d)
    for d, fam in re.findall(r'\{\s*(?:"[A-Za-z_]+"|\w+),\s*("[A-Za-z_]+"|\w+),'
                             r'\s*"(?:[^"\\]|\\.)*",\s*(Family\w+)\}', _tax, re.S)
    if fam in ("FamilyIncome", "FamilyBoth")}
check("...and each of them is an income type of the engine's taxonomy",
      bool(_INCOME_TYPES) and set(p.INVESTMENT_INCOME_TYPES) <= _INCOME_TYPES,
      sorted(set(p.INVESTMENT_INCOME_TYPES) - _INCOME_TYPES))

section("the merchant ranking carries every currency")
MERCH = CARDS["Top 50 merchants"][2]["query"]
AGGS = MERCH.get("aggregation", [])
check("it sums one column per reporting currency",
      [a[2].get("display-name") for a in AGGS if a[0] == "aggregation-options"]
      == list(p.REPORTING_CURRENCIES))
check("each column carries its own currency predicate, so no row filter is needed",
      all(a[1][0] == "sum-where"
          and a[1][2] == ["=", ["field", "currency", {"base-type": "type/Text"}], ccy]
          for a, ccy in zip(AGGS, p.REPORTING_CURRENCIES, strict=True)))
check("the ranking is by the default currency's column (USD, aggregation 0)",
      MERCH.get("order-by") == [["desc", ["aggregation", 0]]]
      and AGGS[0][2]["display-name"] == "USD")
check("it carries no standalone-currency caveat",
      "filter currency to a single" not in CARDS["Top 50 merchants"][1])
check("the Currency picker is not wired to it — a row filter would empty "
      "every column but the one it selects",
      "Top 50 merchants" in p.SPEND_ALL_CURRENCY_CARDS)

section("every spending tile reads one currency")
# The long-format model is right only under a row filter on `currency`,
# which the dashboard supplies and nothing else does — so a tile over it
# would sum every currency whenever it was opened on its own. Every
# money spending tile settles that for itself: natively through the
# {{currency}} variable the picker substitutes (and which defaults to
# the configured currency), or, on the merchant ranking, by carrying
# each currency as its own column. Nothing on the dashboard may read the
# model unguarded.
SPEND_MODEL_CARD = f"card__{MID['report_spending']}"
for _c, *_ in DEFS["Spending"][3]:
    _q = CARDS[_c][2]
    _sql = sql_of(_q)
    if _sql is not None:
        check(f"'{_c}' names its currency through the variable",
              "{{currency}}" in _sql,
              _sql[:160])
        continue
    if _q.get("query", {}).get("source-table") != SPEND_MODEL_CARD:
        continue          # the _pct model's share is a row count: invariant
    check(f"'{_c}' carries a currency column per reporting currency",
          _c in p.SPEND_ALL_CURRENCY_CARDS
          and [a[2].get("display-name")
               for a in _q["query"]["aggregation"]] == list(p.REPORTING_CURRENCIES))

# ---- the Cash Flow dashboard ------------------------------------------

section("the Cash Flow dashboard")

CASHFLOW_CARD_NAMES = ["Operating in", "Operating out", "Net cash flow", "Savings rate",
                       "Yield share", "Cash flow",
                       "Cash flow statement by month",
                       "Inflows by class by month", "Outflows by class by month",
                       "Investing by month", "Financing and vehicles by month",
                       "Largest flows"]
check("every Cash Flow tile is defined",
      all(n in CARDS for n in CASHFLOW_CARD_NAMES),
      [n for n in CASHFLOW_CARD_NAMES if n not in CARDS])

_cf_sql = {n: (sql_of(CARDS[n][2]) or "") for n in CASHFLOW_CARD_NAMES if n in CARDS}
check("every Cash Flow tile is native over web_cashflow",
      all("web_cashflow" in q for q in _cf_sql.values()),
      [n for n, q in _cf_sql.items() if "web_cashflow" not in q])
check("...and names its currency through the variable",
      all("{{currency}}" in q for q in _cf_sql.values()),
      [n for n, q in _cf_sql.items() if "{{currency}}" not in q])
# The Investing grain is a VARIABLE, not a field filter: it changes how
# the investing section is GROUPED, and a field filter can only narrow a
# population. The tiles that draw investing must read it.
for _n in ("Cash flow", "Investing by month"):
    check(f"'{_n}' honours the Investing picker",
          "{{investing}}" in _cf_sql.get(_n, ""), _cf_sql.get(_n, "")[:200])

# The diagram is what the dashboard is for, and a Sankey visualisation
# reads three named columns.
_sankey_viz = _QDEFS["Cash flow"][3]
check("the diagram is wired as a Sankey over source/target/value",
      _sankey_viz.get("sankey.source") == "source"
      and _sankey_viz.get("sankey.target") == "target"
      and _sankey_viz.get("sankey.value") == "value", _sankey_viz)
check("...and it computes the hub where the pickers apply",
      "hub AS" in _cf_sql["Cash flow"] and "Household" in _cf_sql["Cash flow"])
# Metabase lays a node out at the depth its INCOMING edge puts it at, so
# a class with no leaves would sit on the middle level as a dead end
# beside the classes that have them, and read as though it were one.
_twin_sankey_viz = _PDEFS[p.privacy_name("Cash flow")][4]
for _n, _viz in (("Cash flow", _sankey_viz),
                 ("Cash flow (privacy)", _twin_sankey_viz)):
    check(f"'{_n}' draws every ending on the last level",
          _viz.get("sankey.node_align") == "justify", _viz)

# No account picker, and no tile that groups by one. The household
# boundary is what separates the household's cash flow from its
# vehicles', and a picker that moved accounts in and out of the pool
# would turn every crossing it split into an unexplained disappearance.
_cf_pickers = [q["slug"] for q in p.dashboard_parameters(MID, "range", "Cash Flow")]
check("the Cash Flow dashboard carries its five pickers, the projection's four and no account one",
      _cf_pickers == ["currency", "investing", "time_range", "source", "section",
                      "horizon", "rates", "maturing", "shift"],
      _cf_pickers)
check("the Cash Flow twin carries the same nine",
      [q["slug"] for q in p.dashboard_parameters(MID, "range", "Cash Flow (privacy)")]
      == _cf_pickers)
_cf_picker = [q for q in p.dashboard_parameters(MID, "range", "Cash Flow")
              if q["slug"] == "section"][0]
check("the Section picker binds to a column report_cashflow projects",
      _cf_picker["values_source_config"]["value_field"][1] in _model_columns("report_cashflow"),
      _model_columns("report_cashflow"))
check("the Cash Flow dashboard and its twin are laid out",
      "Cash Flow" in DEFS and "Cash Flow (privacy)" in DEFS)
check("the Cash Flow twin has a card for every base tile",
      all(p.privacy_name(n) in CARDS for n in CASHFLOW_CARD_NAMES),
      [n for n in CASHFLOW_CARD_NAMES if p.privacy_name(n) not in CARDS])

# THE TWIN PROJECTS NO LABEL AT ANY GRAIN. The diagram's nodes are
# vocabulary — no node is ever a merchant, a payer, an account or an
# instrument — so the twin needs normalisation rather than redaction;
# the one card that DID name an account and a counterparty drops both
# columns and ranks instead. This is the assertion that keeps it true.
_cf_twin_sql = {p.privacy_name(n): (sql_of(CARDS[p.privacy_name(n)][2]) or "")
                for n in CASHFLOW_CARD_NAMES if p.privacy_name(n) in CARDS}
for _col in ("account_label", "display_name", "account_external_id", "name"):
    _leaking = [n for n, q in _cf_twin_sql.items()
                if _blind_star(q) or any(re.search(rf"\b{_col}\b", pr)
                                         for pr in _output_projections(q))]
    check(f"no Cash Flow twin card projects {_col}", not _leaking, _leaking)
check("the twin's largest-flows card ranks instead of naming",
      "row_number() OVER" in _cf_twin_sql["Largest flows (privacy)"],
      _cf_twin_sql["Largest flows (privacy)"][:200])
# Every twin figure is a share of the hub or of the peak month, and the
# hub moves with the Investing picker exactly as the diagram does.
check("the twin's scalars divide by the hub",
      all("hub" in _cf_twin_sql[p.privacy_name(n)]
          for n in ("Operating in", "Operating out", "Net cash flow")),
      [n for n in ("Operating in", "Operating out", "Net cash flow")
       if "hub" not in _cf_twin_sql[p.privacy_name(n)]])

# The rate tiles' arithmetic, pinned. web_cashflow's values are already
# SIGNED — an outflow is negative — so operating cash flow is the plain
# sum over the two halves. Multiplying the outflow half by -1 and summing
# computes income PLUS spending, which is the inverse of what the card's
# own description promises and of what the summary macro returns, and no
# shape test would catch it.
for _n in ("Savings rate", "Savings rate (privacy)"):
    _q = _cf_sql.get(_n) or _cf_twin_sql.get(_n, "")
    check(f"'{_n}' sums the two operating halves signed",
          "THEN 1 ELSE -1 END" not in _q, _q[:240])
    check("...and divides by what came in",
          "section = 'operating_in'" in _q and "nullif" in _q, _q[:240])
# The twin's scalars must read the same way round as the base tiles they
# stand in for: gold stores an outflow negative and the base
# "Operating out" prints a positive magnitude, so its share has to be
# one too.
check("'Operating out (privacy)' reports a positive magnitude, as its base tile does",
      all(f"-value_{c}" in _cf_twin_sql["Operating out (privacy)"] for c in p._ccy_lower()),
      _cf_twin_sql["Operating out (privacy)"][:240])
check("'Operating in (privacy)' does not negate",
      not any(f"-value_{c}" in _cf_twin_sql["Operating in (privacy)"] for c in p._ccy_lower()),
      _cf_twin_sql["Operating in (privacy)"][:240])
# One hub. It is the sum of the positive nets AT THE LEVEL DRAWN, so a
# class-level sum is a different number from the diagram's whenever a
# leaf nets against its class — and a reader comparing a scalar against
# the diagram beside it would find they did not agree.
for _n in ("Operating in (privacy)", "Operating out (privacy)",
           "Net cash flow (privacy)", "Largest flows (privacy)",
           "Cash flow (privacy)"):
    check(f"'{_n}' divides by the diagram's own hub",
          "atoms AS" in _cf_twin_sql[_n] and "hub AS" in _cf_twin_sql[_n],
          _cf_twin_sql[_n][:200])

# A Sankey cannot render an edge whose two ends are the same node, and
# several classes ARE their own leaf — the backlog, and every vehicle
# class. The card keeps them out of the leaf stage and attaches them to
# the hub directly.
#
# The rule is asserted as the rule, not as a list of section and class
# names, which would have to be kept in step with the taxonomy by hand.
# Financing takes a leaf stage, since a mortgage instalment splits into
# interest and amortisation.
for _n, _q in (("Cash flow", _cf_sql["Cash flow"]),
               ("Cash flow (privacy)", _cf_twin_sql["Cash flow (privacy)"])):
    check(f"'{_n}' keeps a class that is its own leaf out of the leaf stage",
          "AND class <> grp" in _q, _q[:400])
    check("...and attaches such a class to the hub instead",
          "NOT EXISTS" in _q, _q[:400])
    check("...and gives financing a leaf stage, mortgage having two",
          "'operating_in', 'operating_out', 'financing'" in _q, _q[:400])

# A picker whose values are known must OFFER them. Metabase derives the
# widget from `values_query_type`, and a parameter that declares a value
# source but not that key renders as a free-text box: the reader has to
# know how the currencies are spelled and that the investing
# grains are `whole`/`class`, and a typo silently filters everything away.
_LISTED = {"Currency", "Investing"}
_seen_listed = 0
for _dash, _def in p.dashboard_defs().items():
    for _param in p.dashboard_parameters(MID, _def[1], _dash):
        if _param.get("name") in _LISTED:
            _seen_listed += 1
            check(f"'{_dash}' offers its {_param['name']} values as a list",
                  _param.get("values_query_type") == "list", _param)
check("...and that check read every dashboard that has one",
      _seen_listed >= 8, _seen_listed)

# The Section picker is not a picker the design asked for, so its scope
# is pinned rather than left to whichever card was written last. It
# reaches the by-month charts and the line list; the headline figures
# and the diagram decline it, on the base and on the twin alike. A card
# that does not declare the {{section}} tag cannot be bound to the
# picker, so the tag IS the scope.
_CF_TAKES_SECTION = {"Cash flow statement by month", "Inflows by class by month",
                     "Outflows by class by month", "Investing by month",
                     "Financing and vehicles by month", "Largest flows"}
for _n in CASHFLOW_CARD_NAMES:
    for _name, _q in ((_n, _cf_sql.get(_n, "")),
                      (p.privacy_name(_n), _cf_twin_sql.get(p.privacy_name(_n), ""))):
        if not _q:
            continue
        _want = _n in _CF_TAKES_SECTION
        check(f"'{_name}' {'takes' if _want else 'declines'} the Section picker",
              ("{{section}}" in _q) == _want, _q[:240])

# A predicate keyed on a DISPLAY LABEL turns its tile to zero the day
# the label is reworded, with every shape test above still passing. The
# serving view carries the id beside every label for exactly this
# reason, so no card may filter on one of the four label columns.
for _name, _q in list(_cf_sql.items()) + list(_cf_twin_sql.items()):
    _bad = [c for c in ("class_label", "class_node", "group_label", "group_node")
            if f"{c} =" in _q or f"{c} IN" in _q]
    check(f"'{_name}' keys its predicates on ids, not labels", not _bad, _bad)

section("the Gains dashboard")

_gains_tiles = [t[0] for t in p.base_dashboards()["Gains"][2]]
check("every Gains tile is defined",
      bool(_gains_tiles) and all(n in CARDS for n in _gains_tiles),
      [n for n in _gains_tiles if n not in CARDS])
check("...and is placed once", len(set(_gains_tiles)) == len(_gains_tiles))
_gn_sql = {n: (sql_of(CARDS[n][2]) or "") for n in _gains_tiles if n in CARDS}
check("every Gains tile is native over web_gains",
      all("FROM web_gains" in q for q in _gn_sql.values()),
      [n for n, q in _gn_sql.items() if "FROM web_gains" not in q])
# The view carries a row set per currency, so a tile that skipped the
# row filter would sum every currency at once.
check("...and every read of it is held to the picked currency",
      all(q.count("FROM web_gains") == q.count("AND currency = {{currency}}")
          for q in _gn_sql.values()),
      [n for n, q in _gn_sql.items()
       if q.count("FROM web_gains") != q.count("AND currency = {{currency}}")])
check("...and to the picked reading of a missing cost basis",
      all(q.count("FROM web_gains") == q.count("AND missing_basis = {{missing_basis}}")
          for q in _gn_sql.values()),
      [n for n, q in _gn_sql.items()
       if q.count("FROM web_gains") != q.count("AND missing_basis = {{missing_basis}}")])
check("every Gains tile takes the five pickers",
      all(dict(p.NATIVE_PARAM_TARGETS[n]).keys() ==
          {p.GAINS_CURRENCY_PARAM_ID, p.TIME_PARAM_ID, p.SOURCE_PARAM_ID,
           p.GAINS_TAX_WRAPPER_PARAM_ID, p.GAINS_MISSING_PARAM_ID} for n in _gains_tiles),
      {n: [pid for pid, _t in p.NATIVE_PARAM_TARGETS.get(n, [])]
       for n in _gains_tiles})
# The '(all sources)' line is the whole the Source picker narrows away
# from, so its arm of the union carries every filter but that one.
_over_time = _gn_sql["Unrealized gain over time"]
_arms = _over_time.split("UNION ALL")
check("the over-time chart's whole-portfolio line ignores the Source picker",
      len(_arms) == 2 and "{{source}}" in _arms[0]
      and "{{source}}" not in _arms[1] and "{{tax_wrapper}}" in _arms[1],
      _over_time)
check("the winners and losers tables show ten rows each",
      all(_gn_sql[n].rstrip().endswith("LIMIT 10")
          for n in _gains_tiles if n.startswith(("Largest", "Best", "Worst"))))

check("the Gains dashboard has no privacy twin",
      "Gains" in DEFS and DEFS["Gains"][2] is None
      and "Gains" + p.PRIVACY_SUFFIX not in DEFS)
check("...and no card has a privacy variant",
      not [n for n in _gains_tiles if p.privacy_name(n) in CARDS])
_gn_pickers = [q["slug"] for q in p.dashboard_parameters(MID, "range", "Gains")]
check("the Gains dashboard carries five pickers",
      _gn_pickers == ["currency", "time_range", "source", "tax_wrapper", "missing_basis"],
      _gn_pickers)
_gn_missing = [q for q in p.dashboard_parameters(MID, "range", "Gains") if q["slug"] == "missing_basis"]
check("...Missing cost basis is required, defaulting to ignore, and offers both readings",
      _gn_missing and _gn_missing[0]["required"] and _gn_missing[0]["default"] == ["ignore"]
      and _gn_missing[0]["values_source_config"]["values"] == ["ignore", "zero"], _gn_missing)
_gbody = [d for d in layouts if d.get("name") == "Gains"]
check("the Gains dashboard was laid out", len(_gbody) == 1)
if _gbody:
    _gdc = _gbody[0]["dashcards"]
    check("...with no switch link, and its tiles from the top row",
          all(dc["card_id"] is not None for dc in _gdc)
          and min(dc["row"] for dc in _gdc) == 0, [dc["row"] for dc in _gdc])
    check("...and every picker on every tile",
          all({m["parameter_id"] for m in dc["parameter_mappings"]} ==
              {x["id"] for x in _gbody[0]["parameters"]} for dc in _gdc))

section("every dashboard's grid")

for _d, (_desc, _mode, _spec) in p.base_dashboards().items():
    check(f"'{_d}' sits on the 24-column grid without overlaps",
          all(c + w <= 24 for _n, _r, c, w, _h, _t in _spec) and not [
              (a[0], b[0]) for i, a in enumerate(_spec) for b in _spec[i + 1:]
              if a[2] < b[2] + b[3] and b[2] < a[2] + a[3] and a[1] < b[1] + b[4] and b[1] < a[1] + a[4]])

section("stacked charts with a band per series")

# Metabase stacks the band whose rows appear last at the bottom and, by
# default, reverses a stacked chart's legend. So every stacked chart with
# bands, area or bar, ranks its rows smallest first, which puts the
# largest band on the axis, and switches the reversal off, which lists
# the largest first. The Exposure charts over time rank with an
# aggregate of their own (see over_time); the Exposure section checks
# them.
_stacked_bands = {n: sql_of(q) or "" for n, (d, _desc, q) in CARDS.items()
                  if d in ("area", "bar", "combo")
                  and VIZ[n].get("stackable.stack_type") == "stacked"
                  and len(VIZ[n].get("graph.dimensions", [])) == 2}
_STACKED_BARS = {c + s for c in ("Investment income by month", "Fees & taxes by month",
                                 "Cash flow statement by month", "Investing by month",
                                 "Financing and vehicles by month")
                 for s in ("", p.PRIVACY_SUFFIX)}
check("the population holds the stacked bars as well as the areas",
      _STACKED_BARS <= set(_stacked_bands)
      and {CARDS[n][0] for n in _stacked_bands} == {"area", "bar", "combo"},
      sorted(_STACKED_BARS - set(_stacked_bands)))
check("every stacked chart with bands ranks its rows by band",
      bool(_stacked_bands) and all("series_rank" in q or "AS latest FROM e" in q for q in _stacked_bands.values()),
      [n for n, q in _stacked_bands.items() if "series_rank" not in q and "AS latest FROM e" not in q])
check("...the bars, flows each, by the band's total over the window",
      all(f"{VIZ[n]['graph.dimensions'][1]} AS series_key, sum({VIZ[n]['graph.metrics'][0]}) AS series_rank"
          in _stacked_bands.get(n, "") for n in _STACKED_BARS),
      [n for n in sorted(_STACKED_BARS)
       if f"sum({VIZ[n]['graph.metrics'][0]}) AS series_rank" not in _stacked_bands.get(n, "")])
# The driver's DuckDB fails on a window partitioned by a constant over
# no rows: a card whose series is a constant (Investing, whole grain)
# meets one when a picker contradicts its own filter.
check("a series rank is an aggregate joined back, never a window",
      all("OVER" not in p._series_order("SELECT 1 AS m, 'k' AS s, 2 AS v", "m", "s", "v", by)
          for by in ("total", "latest")))
check("...and lists its legend in the bands' own order",
      all(VIZ[n].get("legend.is_reversed") is False for n in _stacked_bands),
      [n for n in _stacked_bands if VIZ[n].get("legend.is_reversed") is not False])
# A stack of two metrics is the mirror case: the first metric sits at
# the bottom, so positions, the larger, come first and the legend reads
# the same way.
check("the cash and positions stacks list positions first, legend unreversed",
      all(VIZ[n]["graph.metrics"][0].startswith("positions") and VIZ[n].get("legend.is_reversed") is False
          for n in ("Cash vs positions over time", "Cash vs positions over time (privacy)")))
check("...ending in that order and nothing after it",
      all(q.rstrip().endswith(("NULLS FIRST, month", "NULLS FIRST, as_of_day")) or "AS latest FROM e" in q
          for q in _stacked_bands.values()))

section("line charts list their legend top line first")

# A line chart takes its series in the order their rows first appear,
# so a chart with one line per series ranks its rows by the series'
# latest value, largest first, and keeps the legend in that order.
_LINE_RANKED = ("Unrealized gain over time", "Card balances over time",
                "Card balances over time (privacy)", "Cumulative return (TWR)")
check("the ranked line charts order their rows largest series first",
      all("series_rank DESC NULLS LAST" in (sql_of(CARDS[n][2]) or "") for n in _LINE_RANKED),
      [n for n in _LINE_RANKED if "series_rank DESC" not in (sql_of(CARDS[n][2]) or "")])
check("...and keep the legend in that order",
      all(VIZ[n].get("legend.is_reversed") is False for n in _LINE_RANKED))

section("the Exposure dashboard")

_ex_spec = p.base_dashboards()["Exposure"][2]
_ex_tiles = [t[0] for t in _ex_spec]
check("every Exposure tile is defined",
      bool(_ex_tiles) and all(n in CARDS for n in _ex_tiles),
      [n for n in _ex_tiles if n not in CARDS])
check("...and is placed once", len(set(_ex_tiles)) == len(_ex_tiles))
# The description promises the coverage chart last.
check("...the coverage chart last",
      max(_ex_spec, key=lambda t: t[1])[0] == "How the figures are classified")
_ex_sql = {n: (sql_of(CARDS[n][2]) or "") for n in _ex_tiles if n in CARDS}
# The tiles over the look-through itself; the debt and issuer tiles read
# views of their own (checked below).
_ex_look = {n: q for n, q in _ex_sql.items() if re.search(r"FROM web_exposure\b", q)}
_EX_VIEW = re.compile(r"FROM web_exposure(?:_debt|_issuers)?\b")
check("every Exposure tile is native over an exposure view",
      all(_EX_VIEW.search(q) for q in _ex_sql.values()),
      [n for n, q in _ex_sql.items() if not _EX_VIEW.search(q)])
# A row set per currency: a read without the row filter would sum them.
check("...and every read of one is held to the picked currency",
      all(len(_EX_VIEW.findall(q)) == q.count("AND currency = {{currency}}")
          for q in _ex_sql.values()))
_ex_pickers = [q["slug"] for q in p.dashboard_parameters(MID, "range", "Exposure")]
check("the Exposure dashboard carries the currency, the time range, the source, the tax wrapper "
      "and the asset class",
      _ex_pickers == ["currency", "time_range", "source", "tax_wrapper", "asset_class"], _ex_pickers)
# A share of what the pickers select is each card's own, over the rows
# it reads; nothing picked, it is the share of net worth.
check("every Exposure tile reads the source, the tax wrapper and the asset class "
      "and computes its shares",
      all("[[AND {{source}}]]" in q and "[[AND {{tax_wrapper}}]]" in q
          and "[[AND {{asset_class}}]]" in q
          and "OVER (PARTITION BY as_of_day)" in q for q in _ex_look.values()),
      [n for n, q in _ex_look.items() if "{{asset_class}}" not in q])
check("the Exposure dashboard has a privacy twin",
      DEFS["Exposure"][2] == "Exposure" + p.PRIVACY_SUFFIX
      and "Exposure" + p.PRIVACY_SUFFIX in DEFS)
_ex_twin = {n: sql_of(CARDS[n][2]) or "" for n, *_ in DEFS["Exposure" + p.PRIVACY_SUFFIX][3] if n in CARDS}
check("...whose every tile is a privacy card or an exempt one",
      list(_ex_twin) == [n if n in p.PRIVACY_EXEMPT_CARDS else p.privacy_name(n) for n in _ex_tiles],
      list(_ex_twin))
check("...that never shows a value, only shares of the selection",
      all(not re.search(r"\bAS value\b", q) and "share_pct" in q for q in _ex_twin.values()),
      [n for n, q in _ex_twin.items() if re.search(r"\bAS value\b", q)])
check("...and reads the picked currency too",
      all(len(_EX_VIEW.findall(q)) == q.count("AND currency = {{currency}}") for q in _ex_twin.values()))
check("the base breakdowns show the value beside the share, the charts over time shares alone",
      all(("sum(value) AS value" in q) == (CARDS[n][0] != "area") for n, q in _ex_look.items()))
# A reader wants the shares first: every bar plots a share, with its
# figure on the bar and a percent sign; the base card keeps the value for
# the tooltip, where the privacy card has none to show.
_ex_bars = {n: VIZ[n] for n in _ex_tiles if n in CARDS and CARDS[n][0] in ("bar", "row")}
check("every Exposure bar plots a share, labelled, with the value in its tooltip",
      bool(_ex_bars) and all(
          v.get("graph.metrics") in (["share_pct"], ["class_share_pct"])
          and v.get("graph.show_values") is True
          and (v.get("graph.tooltip_columns") or [])[:1] == ["value"]
          and any(c.get("suffix") == "%" for c in v.get("column_settings", {}).values())
          for v in _ex_bars.values()),
      [n for n, v in _ex_bars.items() if (v.get("graph.tooltip_columns") or [])[:1] != ["value"]])
check("...and its privacy card plots the share with no value to show",
      all(VIZ[n].get("graph.metrics") in (["share_pct"], ["class_share_pct"])
          and "value" not in VIZ[n].get("graph.tooltip_columns", [])
          for n in _ex_twin if n in CARDS and CARDS[n][0] in ("bar", "row")))
# A loan can make a class or a currency negative, which a ring cannot
# draw: Metabase leaves the slice out with a warning, so only the two
# rings whose descriptions say so are rings; the sector and region
# charts stay bars.
_ex_rings = {"Exposure by asset class", "Exposure by currency"}
_ex_pies = [n for n in _ex_tiles + list(_ex_twin) if n in CARDS and CARDS[n][0] == "pie"]
check("the asset class and currency tiles are rings, and their twins",
      set(_ex_pies) == _ex_rings | {p.privacy_name(n) for n in _ex_rings}, _ex_pies)
check("...whose descriptions say a negative slice is left out",
      all("left out" in CARDS[n][1] for n in _ex_pies))
check("...drawn from the value on the base card, with its total in the hole, the share on the twin",
      all(VIZ[n].get("pie.metric") == ("share_pct" if "(privacy)" in n else "value")
          and VIZ[n].get("pie.show_total") is ("(privacy)" not in n)
          and VIZ[n].get("pie.percent_visibility") == "legend" for n in _ex_pies))
# The countries on a world map, keyed by their ISO alpha-2 code as the
# exposure view carries it, beside every region and its countries.
check("the country tile is a world map of the shares",
      CARDS["Exposure by country"][0] == "map"
      and VIZ["Exposure by country"].get("map.region") == "world_countries"
      and VIZ["Exposure by country"].get("map.dimension") == "country"
      and VIZ["Exposure by country"].get("map.metric") == "share_pct")
check("...that leaves out what has no country",
      "WHERE country NOT IN ('(none)', 'unknown')" in _ex_sql["Exposure by country"])
# The charts over time rank their bands smallest first by the share at
# the window's end, as every stacked area does (the section above), with
# an aggregate of their own; the currencies, a long tail, fold the small
# ones.
_ex_areas = {n: q for n, q in _ex_sql.items() if CARDS[n][0] == "area"}
check("every chart over time ranks its bands by the latest share, smallest first",
      bool(_ex_areas) and all("ORDER BY max(" in q and "AS latest FROM e" in q
                              and "DESC" not in q.split("ORDER BY")[-1] for q in _ex_areas.values()))
check("...with the legend reading largest first",
      all(VIZ[n].get("legend.is_reversed") is False for n in _ex_areas))
check("...and only the currencies fold their tail into an other band",
      all(("ELSE 'other'" in q) == (n == "Currencies over time") for n, q in _ex_areas.items()))
# The legal situs reads no fund through: a bar of each situs's share, the
# value in the tooltip, under the country map and the region table, with
# its twin, and named in the dashboard's description.
_situs = "Exposure by legal situs"
_ex_at = {t[0]: t for t in _ex_spec}
check("the legal situs tile is a bar of the shares by situs, the value in its tooltip",
      CARDS[_situs][0] == "bar" and VIZ[_situs].get("graph.dimensions") == ["situs"]
      and VIZ[_situs].get("graph.metrics") == ["share_pct"]
      and VIZ[_situs].get("graph.tooltip_columns") == ["value"]
      and "GROUP BY ALL" in _ex_sql[_situs] and "SELECT situs AS situs" in _ex_sql[_situs])
check("...placed under the country map and the region table",
      all(_ex_at[_situs][1] >= _ex_at[n][1] + _ex_at[n][4]
          for n in ("Exposure by country", "Exposure by region and country")))
check("...with its privacy twin", p.privacy_name(_situs) in _ex_twin
      and VIZ[p.privacy_name(_situs)].get("graph.metrics") == ["share_pct"])
check("...and its description says no fund is looked through", "No fund is looked through" in CARDS[_situs][1])
check("the Exposure dashboard's description names the legal situs", "legal situs" in DEFS["Exposure"][0])
check("the region and country table carries a row per region and one per country",
      CARDS["Exposure by region and country"][0] == "table"
      and "GROUPING SETS ((region), (region, country, country_name))" in _ex_sql["Exposure by region and country"])

def _output_columns(q):
    """The names of the columns a query's rows carry: the alias, else the
    trailing identifier, of each item of every top-level projection, a
    `cte.*` read through to that CTE's own."""
    ctes, out = _ctes(q), []
    for pr in _projections(q):
        depth, item, items = 0, "", []
        for ch in pr:
            depth += {"(": 1, ")": -1}.get(ch, 0)
            if ch == "," and depth == 0:
                items.append(item)
                item = ""
            else:
                item += ch
        items.append(item)
        for it in items:
            star = _CTE_STAR.fullmatch(it)
            names = re.findall(r"[A-Za-z_]\w*", _strip_parens(it))
            if star and star.group(1) in ctes:
                out += _output_columns(ctes[star.group(1)])
            elif names:
                out.append(names[-1])
    return out


def _tile_layout(name):
    """dashboard `name`'s laid-out tiles, card id -> the picker ids mapped
    onto it, and its PUT body."""
    body = [d for d in layouts if d.get("name") == name][0]
    return {dc["card_id"]: {m["parameter_id"] for m in dc["parameter_mappings"]}
            for dc in body["dashcards"] if dc["card_id"] is not None}, body


def _tiles_off_their_pickers(dash, names):
    """The tiles of dashboard `dash` among `names`, by their base names,
    that answer other pickers than their card registers, or that register
    a picker `dash` lacks. A twin's tile is the privacy card, or the base
    card where that is exempt."""
    layout, _ = _tile_layout(dash)
    params = {q["id"] for q in p.dashboard_parameters(MID, "range", dash)}
    twin = dash.endswith(p.PRIVACY_SUFFIX)
    off = []
    for n in names:
        card = p.privacy_name(n) if twin and n not in p.PRIVACY_EXEMPT_CARDS else n
        want = {pid for pid, _t in p.NATIVE_PARAM_TARGETS[card]}
        if layout[card_ids[card]] != want or not want <= params:
            off.append(card)
    return off


def _picker_targets_resolve(names):
    """The cards among `names` whose pickers map onto a tag the card does
    not declare, or whose declared tag its SQL never reads."""
    off = []
    for n in names:
        q = CARDS[n][2]
        tags = q["native"]["template-tags"]
        mapped = {t[1][1] for _pid, t in p.NATIVE_PARAM_TARGETS.get(n, [])}
        if not mapped <= set(tags) or any("{{" + t + "}}" not in sql_of(q) for t in tags):
            off.append(n)
    return off


# A privacy card's columns are shares and rates, `_pct`, or labels and
# years that name no account and carry no amount.
_LABELS = {"as_of_day", "holding_currency", "currency", "kind", "symbol", "name", "vehicle", "tag",
           "rating", "grade", "maturity", "coupon", "cycle", "rank", "issuer", "lei", "point",
           "month", "series", "date", "type", "basis", "duration"}

section("the Exposure dashboard's debt and issuer cards")

_ex_debt = {"Debt by credit rating, share of debt and cash": "credit", "Debt by maturity": "maturity",
            "Debt by coupon kind": "coupon", "Public equity by cycle": "cycle"}
_ex_debt_tiles = [*_ex_debt, "Largest issuers"]
check("the five are on the Exposure dashboard and its twin, above the coverage chart",
      all(n in _ex_tiles and p.privacy_name(n) in _ex_twin for n in _ex_debt_tiles)
      and all(_ex_at[n][1] < _ex_at["How the figures are classified"][1] for n in _ex_debt_tiles))
_ex_debt_sql = {n + s: sql_of(CARDS[n + s][2]) or "" for n in _ex_debt_tiles for s in ("", p.PRIVACY_SUFFIX)}
for _n, _dim in _ex_debt.items():
    for _name in (_n, p.privacy_name(_n)):
        _q = _ex_debt_sql[_name]
        check(f"'{_name}' reads its dimension of web_exposure_debt in the picked currency, (none) left out",
              _q.count("FROM web_exposure_debt") == 1 and "AND currency = {{currency}}" in _q
              and f"AND dimension = '{_dim}'" in _q and "WHERE bucket <> '(none)'" in _q, _q[:300])
        _tags = CARDS[_name][2]["native"]["template-tags"]
        check(f"'{_name}' takes the window, the source and the tax wrapper, but no asset class "
              "and no projection picker",
              {"currency", "time_range", "source", "tax_wrapper"} == set(_tags), sorted(_tags))
for _name in ("Largest issuers", "Largest issuers (privacy)"):
    _q = _ex_debt_sql[_name]
    check(f"'{_name}' lists the top twenty issuers at the window's end, the currency's and the window's alone",
          "FROM web_exposure_issuers" in _q and "AND currency = {{currency}}" in _q
          and "WHERE issuer <> '(none)'" in _q and _q.rstrip().endswith("LIMIT 20")
          and set(CARDS[_name][2]["native"]["template-tags"]) == {"currency", "time_range"}, _q[-200:])
_gold_go = _go_package("gold")
# The dimensions are the ones gold's materializer writes (its
# debtDimension constants): one respelt there would leave its card with
# no rows.
_debt_dims = set(re.findall(r'[Dd]ebtDimension\w+\s*=\s*"(\w+)"', _gold_go))
check("the debt cards' dimensions are the ones gold materializes",
      set(_ex_debt.values()) == _debt_dims, sorted(_debt_dims))
_holding_prefix = re.search(r'const HoldingKeyPrefix = "([^"]+)"', _gold_go)
check("the issuers twin hides an issuer named by a holding's own key, gold's, and shows no value",
      _holding_prefix is not None and p.HOLDING_KEY_PREFIX == _holding_prefix.group(1)
      and f"LIKE '{p.HOLDING_KEY_PREFIX}%'" in _ex_debt_sql["Largest issuers (privacy)"]
      and "value" not in _output_columns(_ex_debt_sql["Largest issuers (privacy)"]),
      _holding_prefix and _holding_prefix.group(1))
_credit = "Debt by credit rating, share of debt and cash"
check("the credit bars carry the grade and the unknown share in the tooltip",
      VIZ[_credit]["graph.tooltip_columns"] == ["value", "grade", "unknown_pct"]
      and VIZ[p.privacy_name(_credit)]["graph.tooltip_columns"] == ["grade", "unknown_pct"])
# Each bucket is a share of its class, every bucket but (none), as the
# CLI's class_share is: the bars sum to 100.
for _n in _ex_debt:
    for _name in (_n, p.privacy_name(_n)):
        _q = _ex_debt_sql[_name]
        check(f"'{_name}' plots each bucket's share of every bucket but (none)",
              VIZ[_name]["graph.metrics"] == ["class_share_pct"]
              and "c AS (SELECT sum(value) AS total FROM e WHERE bucket <> '(none)')" in _q
              and "100 * sum(value) / (SELECT CASE WHEN total > 0 THEN total END FROM c) AS class_share_pct" in _q
              and "share_pct AS share_pct" not in _q and "OVER" not in _q, _q[-500:])
        check(f"...its label held to two decimals, a bucket under {p.SMALLEST_BAR_PCT}% left out of the bars",
              VIZ[_name]["column_settings"]['["name","class_share_pct"]'] == {"suffix": "%", "decimals": 2}
              and ("HAVING coalesce(abs(100 * sum(value) / (SELECT CASE WHEN total > 0 THEN total END FROM c))"
                   f" >= {p.SMALLEST_BAR_PCT}, TRUE)") in _q
              and f"under {p.SMALLEST_BAR_PCT}% of it is left out" in CARDS[_name][1])
# Each description starts by saying what its card counts, which
# Metabase shows behind the info icon. The maturity card's title names
# the debt alone, so its description names the cash it counts too.
check("each debt card's description says what it counts",
      all(CARDS[n][1].startswith("Counts ") for n in _ex_debt)
      and "Equities are never rated here" in CARDS[_credit][1]
      and "and cash, by their remaining term" in CARDS["Debt by maturity"][1]
      and "not its next reset" in CARDS["Debt by maturity"][1])
check("...and the names they replace are retired",
      all(n + s in p.RETIRED_CARD_NAMES and n + s not in CARDS
          for n in ("Exposure by credit rating", "Exposure by maturity", "Exposure by coupon")
          for s in ("", p.PRIVACY_SUFFIX)))
check("every picker these tiles map lands on a tag they declare and read",
      not _picker_targets_resolve(list(_ex_debt_sql)), _picker_targets_resolve(list(_ex_debt_sql)))
# The bars follow the views' own bucket orders (the CLI's), so a bucket
# respelt in the engine would drop out of its bar silently.
_orders = [re.findall(r"'([^']+)'", m)
           for m in re.findall(r"list_position\(\[([^\]]*)\]", _MIG_TEXT.get(137, ""), re.S)]
check("each card's buckets are the engine's, in its order, (none) apart",
      all([*b, "(none)"] in _orders for b in (p.CREDIT_BUCKETS, p.MATURITY_BUCKETS,
                                               p.COUPON_BUCKETS, p.CYCLE_BUCKETS)), _orders)
# The grades split canonical's rating buckets, and the credit view grades
# a bucket by the same split.
_canonical_go = _go_package("canonical")
_ratings = _go_list(_canonical_go, "ratingBuckets", "RatingBucket")
check("the grades split canonical's rating buckets, best first, as the credit view grades them",
      [*p.INVESTMENT_GRADE, *p.HIGH_YIELD] == _ratings
      and (f"WHEN bucket IN ({p._sql_list(p.INVESTMENT_GRADE)}) THEN 'IG' "
           f"WHEN bucket IN ({p._sql_list(p.HIGH_YIELD)}) THEN 'HY'") in _MIG_TEXT.get(137, ""), _ratings)
check("their descriptions say they cover the last twelve months",
      all("last twelve months" in CARDS[n][1] for n in _ex_debt_sql))

section("the Yield dashboard")

_yield_go = _go_package("yield")

_y_spec = p.base_dashboards()["Yield"][2]
_y_tiles = [t[0] for t in _y_spec]
_Y_TILES = ["Income yield", "Projected income", "Implied yield", "Realized yield", "Yield by currency",
            "Income sensitivity to rates", "Cash against its benchmark", "Yield against duration",
            "DV01 along the curve", "Dividend yield against payout", "What the yields rest on",
            "Yields over time", "Duration over time"]
check("the Yield dashboard lays out its thirteen tiles once each",
      _y_tiles == _Y_TILES and all(n in CARDS for n in _y_tiles), _y_tiles)
check("...and has a privacy twin of the same layout",
      DEFS["Yield"][2] == "Yield (privacy)"
      and [t[0] for t in DEFS["Yield (privacy)"][3]] ==
      [n if n in p.PRIVACY_EXEMPT_CARDS else p.privacy_name(n) for n in _y_tiles])
_y_pickers = [q["slug"] for q in p.dashboard_parameters(MID, "range", "Yield")]
check("the Yield dashboard carries Exposure's five pickers and the projection's four",
      _y_pickers == ["currency", "time_range", "source", "tax_wrapper", "asset_class",
                     "rates", "maturing", "shift", "horizon"]
      and [q["slug"] for q in p.dashboard_parameters(MID, "range", "Yield (privacy)")] == _y_pickers,
      _y_pickers)
_y_params = {q["slug"]: q for q in p.dashboard_parameters(MID, "range", "Yield")}
check("...the projection's pickers required, defaulting to the CLI's assumptions",
      all(_y_params[k]["required"] for k in ("rates", "maturing", "shift", "horizon"))
      and _y_params["rates"]["default"] == ["constant"] and _y_params["maturing"]["default"] == ["cash"]
      and _y_params["shift"]["default"] == [0] and _y_params["shift"]["type"] == "number/="
      and _y_params["horizon"]["default"] == ["12"]
      and _y_params["horizon"]["values_source_config"]["values"] == ["12", "24", "36"]
      and _y_params["maturing"]["values_source_config"]["values"] == ["cash", "idle", "roll"],
      _y_params)
_y_all = [n for t in _y_tiles for n in {t, p.privacy_name(t)} if n in CARDS]
_y_sql = {n: sql_of(CARDS[n][2]) or "" for n in _y_all}
check("every Yield tile and twin is native over the yield views",
      all("FROM web_yield" in q for q in _y_sql.values()), [n for n, q in _y_sql.items() if not q])
# A row set per currency and instant: a read without the row filter would
# sum every currency.
_y_reads = {n: len(re.findall(r"FROM web_yield(?:_curve)?\b", q)) for n, q in _y_sql.items()}
check("...and every read of web_yield or web_yield_curve is held to the picked currency",
      all(_y_reads[n] == q.count("currency = {{currency}}") for n, q in _y_sql.items()),
      {n: (_y_reads[n], q.count("currency = {{currency}}")) for n, q in _y_sql.items()})
check("every picker a Yield tile maps lands on a tag it declares and reads",
      not _picker_targets_resolve(_y_all), _picker_targets_resolve(_y_all))
# The cards with an expected figure read today's projection whatever
# the window, so the Time range does not reach them, and the four
# assumptions do: a floating rate floors at zero, so the path and the
# shift move the sensitivity too.
_proj = {"Income yield": {"rates", "maturing", "shift", "horizon"},
         "Projected income": {"rates", "maturing", "shift", "horizon"},
         "Income sensitivity to rates": {"rates", "maturing", "shift", "horizon"}}
for _n in _y_all:
    _tags = set(CARDS[_n][2]["native"]["template-tags"])
    _want = _proj.get(_n.removesuffix(p.PRIVACY_SUFFIX))
    if _want:
        check(f"'{_n}' reads today's parts under {sorted(_want)}, whatever the window",
              "FROM web_yield_parts" in _y_sql[_n] and "AND today" in _y_sql[_n]
              and "time_range" not in _tags and _tags & {"rates", "maturing", "shift", "horizon"} == _want,
              sorted(_tags))
    else:
        check(f"'{_n}' reads the window's last day and takes no projection picker",
              "time_range" in _tags and not _tags & {"rates", "maturing", "shift", "horizon"}, sorted(_tags))
check("a part's income is yield.Part.Income's, each floating rate floored at zero",
      all(s in p.PART_INCOME for s in ("p.fixed", "p.roll_forward", "p.roll_constant",
                                       "p.principal * GREATEST(0, COALESCE(rt.rate_pct, 0) + p.spread"
                                       " + ({{shift}}) / 100.0) / 1200",
                                       "WHEN p.roll_principal = 0 THEN",
                                       "ELSE p.roll_principal * GREATEST(0, ("))
      and "rt.path = {{rates}}" in p.PARTS_FROM, p.PART_INCOME)
check("a rate weighs the assets, else every row, as yield.summarize does",
      "CASE WHEN bool_or(weighs) THEN sum(value * x) FILTER (WHERE weighs AND x IS NOT NULL)" in p._rate("x")
      and "ELSE sum(value * x) FILTER (WHERE x IS NOT NULL)" in p._rate("x"))
check("the cash card lists the worst paid first, the debit balances last",
      "ORDER BY value < 0, gap_pct NULLS FIRST" in _y_sql["Cash against its benchmark"])
# A kind respelt in the engine would empty a tile with no error.
_kinds = set(re.findall(r'Kind\w+\s+Kind = "(\w+)"', _yield_go))
check("the cash, duration and dividend cards select the yield report's kinds",
      {*p.CASH_KINDS, *p.DURATION_KINDS, "stock"} <= _kinds and "multi_asset" in p.DURATION_KINDS
      and f"kind IN ({p._sql_list(p.CASH_KINDS)})" in _y_sql["Cash against its benchmark"]
      and f"kind IN ({p._sql_list(p.DURATION_KINDS)})" in _y_sql["Yield against duration"]
      and "WHERE kind = 'stock'" in _y_sql["Dividend yield against payout"], sorted(_kinds))
check("the DV01 twin is a share of the day's net worth per basis point",
      "nw AS (SELECT sum(value) AS nw FROM w" in _y_sql["DV01 along the curve (privacy)"]
      and VIZ["DV01 along the curve (privacy)"]["graph.metrics"] ==
      [f"{x}_pct" for x in p.CURVE_HOLDERS])
# The holders are the yield report's labels (yield.HeldIn), which the
# materializer writes into held_in.
_held_in = set(re.findall(r'HeldIn\w+\s*=\s*"(\w+)"', _yield_go))
check("the curve's holders are the yield report's, and the DV01 cards split by each of them",
      set(p.CURVE_HOLDERS) == _held_in
      and all(f"held_in = '{x}'" in _y_sql[n] for x in p.CURVE_HOLDERS
              for n in ("DV01 along the curve", "DV01 along the curve (privacy)")), sorted(_held_in))
check("the yields and the duration are two charts, each on one axis",
      VIZ["Yields over time"]["graph.metrics"] == ["income_yield_pct", "implied_yield_pct",
                                                   "realized_yield_pct"]
      and VIZ["Duration over time"]["graph.metrics"] == ["duration"])
check("the column reader reads a card's columns, through a CTE's star",
      _output_columns(_y_sql["Cash against its benchmark (privacy)"]) ==
      ["rank", "kind", "currency", "rate_pct", "benchmark_pct", "gap_pct", "share_pct"]
      and _output_columns("WITH q AS (SELECT a, b AS c_pct FROM t)\nSELECT q.* FROM q") == ["a", "c_pct"],
      _output_columns(_y_sql["Cash against its benchmark (privacy)"]))
_y_exempt = [n for n in _y_tiles if n in p.PRIVACY_EXEMPT_CARDS]
_y_priv = [p.privacy_name(n) for n in _y_tiles if n not in p.PRIVACY_EXEMPT_CARDS] + _y_exempt
_y_money = {n: [c for c in _output_columns(_y_sql[n]) if not c.endswith("_pct") and c not in _LABELS]
            for n in _y_priv}
check("no Yield privacy or exempt card carries an amount: shares, rates, years and labels alone",
      bool(_y_exempt) and not any(_y_money.values()), {n: c for n, c in _y_money.items() if c})
# The twin's tiles too: a mapping onto a picker the twin lacks is a
# target with no filter behind it.
for _d in ("Yield", "Yield (privacy)"):
    check(f"every {_d} tile answers the pickers its card maps, and only those on the dashboard",
          not _tiles_off_their_pickers(_d, _y_tiles), _tiles_off_their_pickers(_d, _y_tiles))
_horizons = {k: int(v) for k, v in re.findall(r"(Default|Max)HorizonMonths\s*=\s*(\d+)",
                                              _canonical_go + _go_package("config"))}
check("the Yield and the projection's pickers are the engine's rate paths, maturing rules and horizons",
      _go_list(_yield_go, "Paths", "Path") == list(p.RATE_PATHS)
      and _go_list(_yield_go, "Rules", "Rule") == list(p.MATURING_RULES)
      and int(p.HORIZONS[0]) == _horizons.get("Default") and int(p.HORIZONS[-1]) == _horizons.get("Max"),
      (p.RATE_PATHS, p.MATURING_RULES, p.HORIZONS, _horizons))

section("the Income dashboard's cards of the holdings' income")

_in_spec = p.base_dashboards()["Income"][2]
# The small tiles first, the charts after, the tables last, each with its
# width.
_IN_ORDER = [[("Income in the window", 4), ("Received income", 4), ("Expected remaining", 4),
              ("Net income", 4), ("Monthly average income", 4), ("Uncategorized income share", 4)],
             [("Income — monthly trend", 8), ("Withholding", 8), ("Income after withholding", 8)],
             [("Received and expected by month", 24)],
             [("Received interest", 6), ("Received dividends", 6), ("Expected interest remaining", 6),
              ("Expected dividends remaining", 6)],
             [("Income by type", 12), ("Top 50 payers", 12)],
             [("Received and expected by account", 24)],
             [("Received and expected by position", 24)],
             [("Expected payments", 24)],
             [("Largest receipts", 24)]]
_in_at = {t[0]: t for t in _in_spec}
check("the Income tiles read small tiles first, then the charts, then the tables",
      [(t[0], t[3]) for t in _in_spec] == [x for row in _IN_ORDER for x in row]
      and all(len({_in_at[n][1] for n, _w in row}) == 1 for row in _IN_ORDER)
      and all(_in_at[_IN_ORDER[k - 1][0][0]][1] + _in_at[_IN_ORDER[k - 1][0][0]][4] == _in_at[_IN_ORDER[k][0][0]][1]
              for k in range(1, len(_IN_ORDER))),
      [(t[0], t[1], t[3]) for t in _in_spec])
check("...and the twin mirrors the order",
      [(t[0], t[1], t[2], t[3], t[4]) for t in DEFS["Income (privacy)"][3]]
      == [(n if n in p.PRIVACY_EXEMPT_CARDS else p.privacy_name(n), r, c, w, h) for n, r, c, w, h, _t in _in_spec])
_IN_PROJECTED = [t[0] for t in _in_spec if t[0] not in INCOME_CARD_NAMES]
check("...each card of the holdings' income with a privacy twin",
      len(_IN_PROJECTED) == 14 and all(p.privacy_name(n) in CARDS for n in _IN_PROJECTED), _IN_PROJECTED)
# The income lines' cards by month and by account would repeat the
# received and expected ones, so none is defined, and their names are
# retired.
_SUPERSEDED = ["Income by month", "Income by account"]
check("the income lines' cards by month and by account are not defined, and their names are retired",
      all(n + s not in CARDS and n + s in p.RETIRED_CARD_NAMES
          for n in _SUPERSEDED for s in ("", p.PRIVACY_SUFFIX))
      and not [t for d in DEFS.values() for t in d[3] if t[0].removesuffix(p.PRIVACY_SUFFIX) in _SUPERSEDED])
_in_all = [n + s for n in _IN_PROJECTED for s in ("", p.PRIVACY_SUFFIX)]
_in_sql = {n: sql_of(CARDS[n][2]) or "" for n in _in_all}
# What was received depends on no assumption, so those tiles read the
# income received alone and take no projection picker; what is expected
# reads the parts alone, but on the twin, whose share is of both.
_RECEIVED_ONLY = {n + s for n in ("Received income", "Received interest", "Received dividends")
                  for s in ("", p.PRIVACY_SUFFIX)}
_EXPECTED_ONLY = {"Expected remaining", "Expected interest remaining", "Expected dividends remaining",
                  "Expected payments", "Expected payments (privacy)"}
# The income received holds a row set per currency, a part its rate to
# each: the one is held to the picked currency, the other picks its rate.
check("every card reads the income received where it shows a received figure, and today's parts "
      "where it shows an expected one, each in the picked currency",
      all(("FROM web_yield_received\n   WHERE currency = {{currency}}" in q) == (n not in _EXPECTED_ONLY)
          and ("FROM web_yield_received" in q) == (n not in _EXPECTED_ONLY)
          and ("FROM web_yield_parts" in q) == (n not in _RECEIVED_ONLY)
          and ("p.rule = {{maturing}}" in q) == (n not in _RECEIVED_ONLY)
          and ("CASE {{currency}} WHEN 'USD' THEN p.fx_usd" in q) == (n not in _RECEIVED_ONLY)
          for n, q in _in_sql.items()),
      [n for n, q in _in_sql.items() if ("FROM web_yield_parts" in q) == (n in _RECEIVED_ONLY)
       or ("FROM web_yield_received" in q) == (n in _EXPECTED_ONLY)])
_KEPT_DAYS = "day IN (SELECT day FROM web_income_days [[WHERE {{time_range}}]])"
check("...the received rows those of the days the Time range selects, the parts those of the horizon",
      all((_KEPT_DAYS in q) == (n not in _EXPECTED_ONLY)
          and ("AND p.month <= CAST({{horizon}} AS INTEGER)" in q) == (n not in _RECEIVED_ONLY)
          for n, q in _in_sql.items()),
      [n for n, q in _in_sql.items() if (_KEPT_DAYS in q) == (n in _EXPECTED_ONLY)])
_tr = [CARDS[n][2]["native"]["template-tags"].get("time_range") for n in _in_all if n not in _EXPECTED_ONLY]
check("...through a Time range field filter on the days the income received covers",
      bool(_tr) and all(t and t["dimension"][1] == p.FIELD_IDS[("web_income_days", "day")]
                        and t["widget-type"] == "date/all-options" for t in _tr))
check("every picker a card maps lands on a tag it declares and reads",
      not _picker_targets_resolve(_in_all), _picker_targets_resolve(_in_all))
# One picker reaches one tag of a card, and a card reads two views, so
# the source and account filters are bound to the views their dropdowns
# list and keep each view's rows through them.
_src = [CARDS[n][2]["native"]["template-tags"].get("source") for n in _in_all]
_acc = [CARDS[n][2]["native"]["template-tags"].get("account") for n in _IN_PROJECTED]
check("the Source and Account pickers reach every card through the views they list",
      all(t and t["dimension"][1] == p.FIELD_IDS[("web_sources_latest", "silver_source_id")] for t in _src)
      and all(t and t["dimension"][1] == p.FIELD_IDS[("web_income", "account_label")] for t in _acc)
      and all(re.search(r"\[\[AND (p\.)?silver_source_id IN \(SELECT silver_source_id FROM web_sources_latest "
                        r"WHERE \{\{source\}\}\)\]\]", q) for q in _in_sql.values()))
check("...and the twin carries no account tag",
      not [n for n in _in_all if n.endswith(p.PRIVACY_SUFFIX) and "account" in CARDS[n][2]["native"]["template-tags"]])
for _n in _in_all:
    _tags = set(CARDS[_n][2]["native"]["template-tags"]) - {"currency", "source", "account", "position"}
    if _n in _RECEIVED_ONLY:
        _want = {"time_range"}
    elif _n in _EXPECTED_ONLY:
        _want = {"horizon", "rates", "maturing", "shift"}
    else:
        _want = {"time_range", "horizon", "rates", "maturing", "shift"}
    check(f"'{_n}' takes the Time range where it shows a received figure, the Horizon and the "
          "assumptions where it shows an expected one",
          _tags == _want, sorted(_tags))
# Opened on its own, a card runs over its variables' defaults, and its
# description ends by naming them: the days for a received figure, the
# horizon and the assumptions for an expected one.
_STANDALONE = (" Built for the Income dashboard; opened standalone it runs in "
               f"{p.DEFAULT_CURRENCY} over the variables' defaults: ")
_DEFAULTS = {"time_range": "every day to today from the first income received, the first tax withheld "
                           "or 1 January of last year, whichever is earliest",
             "horizon": f"the rest of this month and the {p.PROJECTION_TAGS['horizon']['default']} months "
                        f"after it, under {p.PROJECTION_TAGS['rates']['default']} rates, maturing principal "
                        f"to {p.PROJECTION_TAGS['maturing']['default']} and no shift"}
for _n in _in_all:
    _tags = CARDS[_n][2]["native"]["template-tags"]
    _want = (_STANDALONE + "; ".join(v for k, v in _DEFAULTS.items() if k in _tags) + "."
             + (p.PRIVACY_DESC if _n.endswith(p.PRIVACY_SUFFIX) else ""))
    check(f"'{_n}' ends its description with the defaults it runs over opened on its own",
          CARDS[_n][1].endswith(_want), CARDS[_n][1][-300:])
check("...where no shift is the Shift variable's default",
      p.PROJECTION_TAGS["shift"]["default"] == "0", p.PROJECTION_TAGS["shift"])
check("only the calendar takes the Position picker, and it is optional",
      [n for n in _in_all if "position" in CARDS[n][2]["native"]["template-tags"]]
      == ["Expected payments", "Expected payments (privacy)"]
      and p.POSITION_TAG["required"] is False)
_in_params = [q["slug"] for q in p.dashboard_parameters(MID, "range", "Income (privacy)")]
check("the Income twin carries the same pickers, but the account",
      _in_params == ["currency", "time_range", "horizon", "source", "type", "position", "rates",
                     "maturing", "shift"], _in_params)
check("no card declares a window variable: on Income the Time range and the Horizon set every figure",
      not [n for n in CARDS if "window" in ((CARDS[n][2].get("native") or {}).get("template-tags") or {})])
# A day received counts its share of its month, and the horizon its
# months and the rest of the current one.
check("the monthly average divides by the months the received days and the horizon cover",
      "SELECT (SELECT coalesce(sum(1 / day(last_day(day))), 0) FROM web_income_days [[WHERE {{time_range}}]])"
      " AS received" in _in_sql["Monthly average income"]
      and "CAST({{horizon}} AS INTEGER) + coalesce((SELECT (day(last_day(max(day))) - day(max(day)))"
      in _in_sql["Monthly average income"]
      and "/ (SELECT received + expected FROM mo)" in _in_sql["Monthly average income"])
# The withholding of the income received is the tax a source booked, of
# the income expected the table's model; with neither a tile reads 0.
for _n in ("Withholding", "Income after withholding"):
    for _name in (_n, p.privacy_name(_n)):
        check(f"'{_name}' reads the tax a source withheld over the Time range and the table's over the "
              "Horizon, 0 where neither withholds",
              "FROM web_income_withheld" in _in_sql[_name] and _KEPT_DAYS in _in_sql[_name]
              and "coalesce((SELECT withheld FROM tx), 0) + coalesce(sum(withheld), 0)" in _in_sql[_name]
              and "amount * fx * withholding_pct / 100 AS withheld" in _in_sql[_name])
check("...and the received rows carry none of the table's",
      "NULL AS withheld" in _in_sql["Withholding"])
_in_priv = [n for n in _in_all if n.endswith(p.PRIVACY_SUFFIX)]
_in_money = {n: [c for c in _output_columns(_in_sql[n]) if not c.endswith("_pct") and c not in _LABELS]
             for n in _in_priv}
check("no projected privacy card carries an amount: shares, yields and labels alone",
      not any(_in_money.values()), {n: c for n, c in _in_money.items() if c})
check("the twin's income is a yield on the value it is earned on",
      "FROM web_yield\n" in _in_sql["Income in the window (privacy)"]
      and "* 12 / (SELECT received + expected FROM mo)" in _in_sql["Income in the window (privacy)"])
_month_viz = VIZ["Received and expected by month"]
check("the by-month chart stacks a series per type and status, the expected one lighter",
      _month_viz["graph.dimensions"] == ["month", "series"]
      and set(_month_viz["series_settings"]) == {f"{t} ({s})" for t in p.INCOME_TYPE_COLORS
                                                for s in ("received", "expected")}
      and all(_month_viz["series_settings"][f"{t} (expected)"]["color"] == p._tint(c) != c
              and _month_viz["series_settings"][f"{t} (received)"]["color"] == c
              for t, c in p.INCOME_TYPE_COLORS.items()))
check("...whose types are the projection's", set(p.INCOME_TYPE_COLORS) == set(
    re.findall(r'Type\w+\s+IncomeType = "(\w+)"', _yield_go)), sorted(p.INCOME_TYPE_COLORS))
check("...and the headline figures' interest and dividends are among them",
      {*p.INTEREST_TYPES, *p.DIVIDEND_TYPES} <= set(p.INCOME_TYPE_COLORS))
check("a lighter shade is the colour mixed toward white", p._tint("#000000", 0.5) == "#808080")
_in_layout, _in_body = _tile_layout("Income")
_pos_dc = [dc for dc in _in_body["dashcards"] if dc["card_id"] == card_ids["Received and expected by position"]][0]
_clicks = _pos_dc["visualization_settings"].get("column_settings", {})
check("a click on a position's symbol or name sets the Position picker the calendar reads",
      set(_clicks) == {'["name","symbol"]', '["name","name"]'}
      and all(c["click_behavior"]["type"] == "crossfilter"
              and c["click_behavior"]["parameterMapping"][p.POSITION_PARAM_ID]["target"]
              == {"type": "parameter", "id": p.POSITION_PARAM_ID} for c in _clicks.values())
      and p.POSITION_PARAM_ID in {q["id"] for q in _in_body["parameters"]}, _clicks)
check("...and no other tile carries settings of its own",
      all(dc["visualization_settings"] == {} for dc in _in_body["dashcards"]
          if dc["card_id"] not in (None, card_ids["Received and expected by position"])))
# The twin's name column shows the name the twin renders, `(unnamed)`
# for a holding's own key, so the twin's calendar matches that name too.
_tw_body = _tile_layout("Income (privacy)")[1]
_tw_pos = [dc for dc in _tw_body["dashcards"]
           if dc["card_id"] == card_ids["Received and expected by position (privacy)"]][0]
check("...and on the twin the same, its calendar matching the name the twin shows",
      set(_tw_pos["visualization_settings"].get("column_settings", {})) == set(_clicks)
      and f"position_key, {p.TWIN_POSITION_NAME})]]" in _in_sql["Expected payments (privacy)"]
      and p.TWIN_POSITION_NAME not in _in_sql["Expected payments"], _tw_pos["visualization_settings"])
check("...as each card's description says",
      all("symbol or name" in CARDS[n][1]
          for n in ("Received and expected by position", "Expected payments",
                    "Received and expected by position (privacy)", "Expected payments (privacy)")))
for _d in ("Income", "Income (privacy)"):
    check(f"every projected tile of {_d} answers the pickers its card maps, and only those on the dashboard",
          not _tiles_off_their_pickers(_d, _IN_PROJECTED), _tiles_off_their_pickers(_d, _IN_PROJECTED))
# The twin shows the key of a cash balance and of an account's income,
# which the engine builds from these prefixes.
_key_go = _gold_go + _yield_go
check("the keys the positions twin shows are the ones the engine builds",
      all(f'"{k}" +' in _key_go for k in p.SHOWN_KEY_PREFIXES), p.SHOWN_KEY_PREFIXES)

section("the Cash Flow dashboard's projection")

_PLAN = ["Cash flow projection by month", "Cash flow projection"]
_plan_all = [n + s for n in _PLAN for s in ("", p.PRIVACY_SUFFIX)]
check("the projection cards and their twins are defined",
      all(n in CARDS for n in _plan_all), [n for n in _plan_all if n not in CARDS])
# The projection sits right below the statement, full width, on the base and
# the twin alike.
_cf_spec = [t[0] for t in p.base_dashboards()["Cash Flow"][2]]
_cf_at = _cf_spec.index("Cash flow statement by month")
check("the projection follows the statement on the Cash Flow dashboard, its twin's on the twin",
      _cf_spec[_cf_at + 1:_cf_at + 3] == _PLAN
      and [t[0] for t in DEFS["Cash Flow (privacy)"][3]][_cf_at + 1:_cf_at + 3]
      == [p.privacy_name(n) for n in _PLAN], _cf_spec)
_plan_sql = {n: sql_of(CARDS[n][2]) or "" for n in _plan_all}
# The investment income is the Income cards' expected income, part by
# part, over the months after this one; the principal is the events' of
# the rule; the rest is the look-back's, in the picked currency.
check("every projection card sums the income projection's parts under the pickers, months 1 to the Horizon",
      all(f"sum(({p.PART_INCOME}) * " + p._ccy_case("p.fx") + ")" in q and p.PARTS_FROM in q
          and "p.rule = {{maturing}}\n     AND p.month BETWEEN 1 AND CAST({{horizon}} AS INTEGER)" in q
          for q in _plan_sql.values()), [n for n, q in _plan_sql.items() if p.PARTS_FROM not in q])
check("...the principal maturities return under the rule, in the picked currency",
      all("FROM web_yield_events e" in q and "WHERE e.rule = {{maturing}}\n     AND e.month BETWEEN 1 AND" in q
          and "sum(e.amount * " + p._ccy_case("e.fx") + ")" in q for q in _plan_sql.values()))
check("...both split by the cash pool's accounts, the rest the vehicles' income",
      all(p._pool_join("p") in q and p._pool_join("e") in q and "GROUP BY 1, 2" in q
          and "coalesce(xv.v, 0) + coalesce(rv.v, 0) AS vehicles_income" in q for q in _plan_sql.values()))
_net_of = {n: re.search(r"SELECT \*, (.*?) AS net\n", q, re.S) for n, q in _plan_sql.items()}
check("...and the net is the pool's: it leaves the vehicles' income out",
      all(m and "+ vehicles" in m[1] and "vehicles_income" not in m[1] and "expected" not in m[1]
          for m in _net_of.values()), {n: m and m[1] for n, m in _net_of.items()})
check("...and the look-back's parts, held to the picked currency",
      all("FROM web_cashflow_plan\n   WHERE currency = {{currency}}" in q
          and all(f"part = '{x}'" in q for x in p.PLAN_LOOKBACK_PARTS) for q in _plan_sql.values()))
# The projection cards take the currency and the projection's four, and none
# of the dashboard's own pickers: the look-back is fixed at the refresh.
for _n in _plan_all:
    check(f"'{_n}' declares the currency and the projection's four tags alone",
          set(CARDS[_n][2]["native"]["template-tags"]) == {"currency", "rates", "maturing", "shift", "horizon"},
          sorted(CARDS[_n][2]["native"]["template-tags"]))
    check("...and says so, and what it reads standalone",
          "the Time range, Source, Section and Investing pickers do not reach this card" in CARDS[_n][1]
          and "Built for the Cash Flow dashboard; opened standalone it runs in" in CARDS[_n][1]
          and CARDS[_n][1].endswith(p.PRIVACY_DESC) == _n.endswith(p.PRIVACY_SUFFIX), CARDS[_n][1])
check("every picker a projection card maps lands on a tag it declares and reads",
      not _picker_targets_resolve(_plan_all), _picker_targets_resolve(_plan_all))
_PROJ_IDS = {pid for pid, _t in p.PROJECTION_PICKERS}
for _name in ("Cash Flow", "Cash Flow (privacy)"):
    _body = [d for d in layouts if d.get("name") == _name][0]
    _maps = {dc["card_id"]: {m["parameter_id"] for m in dc["parameter_mappings"]}
             for dc in _body["dashcards"] if dc["card_id"] is not None}
    _plan_ids = {card_ids[n] for n in _plan_all if n.endswith(p.PRIVACY_SUFFIX) == _name.endswith(p.PRIVACY_SUFFIX)}
    check(f"on '{_name}', the projection tiles answer the currency and the projection's four alone",
          len(_plan_ids) == 2 and all(_maps.get(c) == {p.CASHFLOW_CURRENCY_PARAM_ID} | _PROJ_IDS for c in _plan_ids),
          {c: _maps.get(c) for c in _plan_ids})
    check("...and no other tile answers a projection picker",
          not [c for c, m in _maps.items() if c not in _plan_ids and m & _PROJ_IDS],
          [c for c, m in _maps.items() if c not in _plan_ids and m & _PROJ_IDS])
    check("...every one of which the dashboard carries",
          _PROJ_IDS <= {x["id"] for x in _body["parameters"]}, [x["id"] for x in _body["parameters"]])
# The chart stacks what comes in above the axis and what goes out below
# it, with the net as a line over them.
for _n in _PLAN:
    for _name in (_n, p.privacy_name(_n)):
        _cols = _output_columns(_plan_sql[_name])
        _sfx = "_pct" if _name.endswith(p.PRIVACY_SUFFIX) else ""
        _want = ["month", *(c + _sfx for c in (
            [*p.PLAN_INCOME_PARTS, *p.PLAN_OUTFLOW_PARTS, "net", "vehicles_income"] if _n == _PLAN[0] else
            ["investment_income", "principal_returned", "vehicles_income", *p.PLAN_LOOKBACK_PARTS, "net"]))]
        check(f"'{_name}' carries the CLI's parts and the net, by month",
              _cols == _want + ([] if _n == _PLAN[0] else ["quality"]), _cols)
for _name in (_PLAN[0], p.privacy_name(_PLAN[0])):
    _viz = VIZ[_name]
    _metrics = _viz["graph.metrics"]
    check(f"'{_name}' stacks the parts as bars and draws the net and the vehicles' income as lines",
          CARDS[_name][0] == "combo" and _viz["graph.dimensions"] == ["month"]
          and _viz.get("stackable.stack_type") == "stacked"
          and [_viz["series_settings"][m]["display"] for m in _metrics] == ["bar"] * (len(_metrics) - 2) + ["line"] * 2
          and _metrics[-2].startswith("net") and _metrics[-1].startswith("vehicles_income"), _viz)
check("...the outflows below the axis, the principal where it counts",
      all(f"-{x} AS {x}" in _plan_sql[_PLAN[0]] for x in p.PLAN_OUTFLOW_PARTS)
      and "principal_counted AS principal_returned" in _plan_sql[_PLAN[0]])
# The twin shows each part as a share of all the income the projection
# expects over the horizon, on every account. It is blank where the
# horizon expects none, and shows no amount at all.
_plan_money = {n: [c for c in _output_columns(_plan_sql[n]) if not c.endswith("_pct") and c not in
                   {"month", "quality"}] for n in _plan_all if n.endswith(p.PRIVACY_SUFFIX)}
check("no projection twin carries an amount: shares, the month and the quality alone",
      not any(_plan_money.values()), _plan_money)
check("...each a share of the horizon's expected income, blank where there is none",
      all("CASE WHEN sum(expected) > 0 THEN sum(expected) END AS total" in _plan_sql[n]
          and "coalesce(ex.v, 0) + coalesce(xv.v, 0) AS expected" in _plan_sql[n]
          and "/ (SELECT total FROM d)" in _plan_sql[n] for n in _plan_money))

section("the Wealth Overview and Allocation read the chosen currency")

# A tile that hard-codes one reporting currency keeps showing it whatever
# the picker says, plausibly and without an error. So every money tile on
# these four dashboards is native, reads {{currency}}, and names no
# reporting-currency column outside the CASE the variable drives.
_CCY_CASE = re.compile(r"CASE \{\{currency\}\}(?: WHEN '[A-Z]{3}' THEN -?\w+)+ END")
_WEALTH_DASHES = ("Wealth Overview", "Allocation")
for _d in _WEALTH_DASHES:
    for _name in (_d, _d + p.PRIVACY_SUFFIX):
        for _c, *_ in DEFS[_name][3]:
            _q = CARDS[_c][2]
            _sql = sql_of(_q)
            check(f"'{_c}' is native", _sql is not None)
            if _sql is None:
                continue
            check(f"'{_c}' declares the currency variable",
                  _q["native"]["template-tags"].get("currency") == p.currency_tag())
            check("...and reads it", "{{currency}}" in _sql, _sql[:200])
            _rest = _CCY_CASE.sub("", _sql)
            _fixed = re.findall(r"\b\w+_(?:" + "|".join(p._ccy_lower()) + r")\b", _rest)
            check("...and fixes no reporting currency outside the CASE",
                  not _fixed, _fixed)
            check(f"'{_c}' carries no currency marker in its name",
                  not re.search(r"\((?:" + "|".join(p.REPORTING_CURRENCIES) + r")\)", _c))

# The headline figures stay on the latest holdings, the figure
# `wealthdb holdings sources` prints, with the time filter on it.
for _c in ("Net worth", "Positions value", "Cash balance",
           "Net worth (privacy)", "Positions value (privacy)",
           "Cash balance (privacy)"):
    _q = CARDS[_c][2]
    check(f"'{_c}' reads the latest holdings",
          "FROM web_sources_latest" in (sql_of(_q) or "")
          and _q["native"]["template-tags"]["time_range"]["dimension"][1]
          == p.FIELD_IDS[("web_sources_latest", "snapshot_at")])

# ensure_cards archives every retired name it finds AFTER saving the live
# cards, so a name on both lists would be saved and then archived — a
# dashboard tile pointing at the archive.
_retired_live = sorted(set(p.RETIRED_CARD_NAMES) & set(CARDS))
check("no retired card name is still defined", not _retired_live, _retired_live)

section("the reporting currencies")

# The currency list is one contract in three places: the value columns
# gold's `_multi` macros and serving views carry, the partitions the
# returns materializer writes, and every picker, CASE and unpivot here.
# A currency missing from one of them renders blank or not at all, so
# the three are read off disk and compared.
_go_ccys = _go_list(_gold_go, "materializeCurrencies")
check("the returns materializer writes exactly the reporting currencies",
      _go_ccys == list(p.REPORTING_CURRENCIES), _go_ccys)
# The readings of a missing cost basis are one contract too: the Go
# vocabulary (lots.MissingBasisReadings) and the Gains picker here.
_go_readings = _go_list(_go_package("lots"), "MissingBasisReadings", "MissingBasis")
check("the Gains picker offers exactly the engine's readings of a missing cost basis",
      _go_readings == list(p.MISSING_BASIS_READINGS), _go_readings)


def _latest_definition(name):
    """The text of the last migration statement that (re)defines `name`."""
    found = re.findall(rf"CREATE OR REPLACE (?:VIEW|MACRO) {name}\b.*?;\n",
                       MIGRATION_SQL, re.S)
    return found[-1] if found else ""


# The long views carry a row set per currency, written by their
# materializers from the same materializeCurrencies list, the cash flow
# projection's look-back parts among them. The projection's
# parts and principals are in each holding's currency, with its rate to
# every reporting currency beside it; its rates table and the days the
# income received covers hold no money.
_LONG_VIEWS = {"web_gains", "web_exposure", "web_exposure_debt", "web_exposure_issuers",
               "web_yield", "web_yield_curve", "web_yield_received", "web_cashflow_plan"}
_FX_TABLES = {"web_yield_parts": "report_yield_parts", "web_yield_events": "report_yield_events"}
_missing = {v: [c for c in p.REPORTING_CURRENCIES
                if not re.search(rf"_{c.lower()}\b", _latest_definition(v))]
            for v in p.web_views_wanted()
            if v not in _LONG_VIEWS | set(_FX_TABLES) | {"web_yield_rates", "web_income_days", "web_cashflow_pool"}}
check("every wide serving view carries a value column per reporting currency",
      not any(_missing.values()), {v: m for v, m in _missing.items() if m})
_ym = open(os.path.join(_REPO, "wealthdb", "internal", "gold", "yield_materialize.go"),
           encoding="utf-8").read()
_fx_missing = {v: [c for c in p.REPORTING_CURRENCIES if not re.search(
    rf"CREATE TABLE IF NOT EXISTS {t} \([^;]*\bfx_{c}\b", _MIG_TEXT.get(138, ""))]
    for v, t in _FX_TABLES.items()}
check("the projection's parts and principals carry a rate to every reporting currency",
      not any(_fx_missing.values()) and "range materializeCurrencies" in _ym, _fx_missing)
# The projection cards filter the look-back parts by name and re-add the tag
# the engine strips from their quality. A name respelt on either side
# would read 0 with no error.
_cfp = open(os.path.join(_REPO, "wealthdb", "internal", "gold", "cashflow_plan.go"), encoding="utf-8").read()
_plan_parts = _go_list(_cfp, "PlanParts")
check("the projection cards read the look-back parts the engine writes, in its order",
      _plan_parts == list(p.PLAN_LOOKBACK_PARTS), _plan_parts)
_projected = re.search(r'\bPlanProjected\s*=\s*"([^"]*)"', _cfp)
check("...and re-add the tag it strips from the quality",
      _projected is not None and re.search(r"TrimPrefix\(r\.Quality, PlanProjected\)", _ym) is not None
      and all(f"trim('{_projected.group(1)} ' || lb.quality) AS quality" in q for q in _plan_sql.values()),
      _projected and _projected.group(0))
# The Exposure cards compare against labels web_exposure renders and
# vocabularies the engine defines. A label respelt on either side would
# empty or overfill a tile with no error.
_wx = _latest_definition("web_exposure")
check("web_exposure renders the '(none)' and 'unknown' labels the country map leaves out",
      "THEN '(none)' ELSE 'unknown' END AS country" in _wx, _wx[:300])
check("the size tile's class and sizes are the engine's",
      "public_equity" in _go_consts("taxonomy.go", "AssetClass")
      and {"large", "mid", "small"} <= _go_consts("size.go", "Size"),
      sorted(_go_consts("size.go", "Size")))
_gm = open(os.path.join(_REPO, "wealthdb", "internal", "gold",
                        "gains_materialize.go"), encoding="utf-8").read()
check("the gains materializer writes the same currency list",
      "gainsInsertSQL, computedAt, toEpoch, materializeCurrencies" in _gm)
# The gain's split by what was sold and what was kept: the materializer
# writes it, web_gains carries it, and the tiles that add up to the
# total gain read it.
_wg = _latest_definition("web_gains")
_split_sql = {n: sql_of(CARDS[n][2]) or "" for n in
              ("Gain on positions sold", "Gain on positions held", "Gains by month")}
check("the Gains split is materialized, served and read",
      "held_change_x, sold_gain_x" in _gm
      and "AS held_change" in _wg and "AS sold_gain" in _wg
      and "sum(sold_gain)" in _split_sql["Gain on positions sold"]
      and "sum(held_change)" in _split_sql["Gain on positions held"]
      and all(f"sum({c})" in _split_sql["Gains by month"] for c in ("sold_gain", "held_change")),
      _split_sql)
_fxv = _latest_definition("fx_reporting_value")
check("the conversion helper names every reporting currency",
      all(f"'{c}'" in _fxv for c in p.REPORTING_CURRENCIES), _fxv[:200])

# A currency the CASE does not name must render blank, not as another
# currency's figures: an arm per currency, and no ELSE to fall into.
_case = p._ccy_case("value")
check("the currency CASE has an arm per reporting currency and no ELSE",
      all(f"WHEN '{c}' THEN value_{c.lower()}" in _case for c in p.REPORTING_CURRENCIES)
      and "ELSE" not in _case, _case)
check("the long models unpivot every reporting currency",
      all(f"WHEN '{c}' THEN s.value_{c.lower()}" in SPEND_SQL for c in p.REPORTING_CURRENCIES)
      and "ELSE" not in SPEND_SQL.split("AS value")[0].rsplit("CASE", 1)[-1], SPEND_SQL[-400:])

# The configured default currency is what every dashboard opens on.
check("a reporting currency is taken as the default as configured",
      p.default_currency("gbp") == "GBP")
with quiet():
    _fallback = p.default_currency("JPY")
check("any other currency falls back to USD", _fallback == "USD")
_saved = p.DEFAULT_CURRENCY
p.DEFAULT_CURRENCY = "GBP"
try:
    _params = [x for d, df in p.dashboard_defs().items()
               for x in p.dashboard_parameters(MID, df[1], d) if x["slug"] == "currency"]
    check("with a GBP default, every Currency picker opens on GBP",
          bool(_params) and all(x["default"] == ["GBP"] for x in _params),
          [x["default"] for x in _params])
    _gbp_cards = all_cards(1, MID)
    _tags = [q["native"]["template-tags"]["currency"]
             for _d, _desc, q in _gbp_cards.values()
             if q.get("type") == "native" and "currency" in q["native"]["template-tags"]]
    check("...and every native card's currency variable defaults to it",
          bool(_tags) and all(t["default"] in ("GBP", ["GBP"]) for t in _tags),
          sorted({str(t["default"]) for t in _tags}))
    _m = _gbp_cards["Top 50 merchants"][2]["query"]
    check("...and the merchant ranking orders by the GBP column",
          _m["order-by"] == [["desc", ["aggregation", p.REPORTING_CURRENCIES.index("GBP")]]])
    check("...and the standalone notes name it",
          "runs in GBP" in _gbp_cards["Net worth"][1])
finally:
    p.DEFAULT_CURRENCY = _saved
    all_cards(1, MID)               # rebuild the picker registry at the default

section("a percent-styled column is a fraction, not a percentage")

# _percent_viz's contract is "ratio columns (0.07 -> 7%)": Metabase
# multiplies by 100 itself. A card that also multiplies renders 51% as
# 5.1k%, and no shape test would see it — the column exists, the viz
# setting is attached, and only the figure is wrong.
_PCT_CARDS = {}
for _defs in (_QDEFS, _PDEFS):
    for _name, _tup in _defs.items():
        _query, _viz = _tup[-2], _tup[-1]
        _sql = sql_of(_query)
        if not _sql or not isinstance(_viz, dict):
            continue
        for _key, _setting in (_viz.get("column_settings") or {}).items():
            if _setting.get("number_style") == "percent":
                _PCT_CARDS.setdefault(_name, (_sql, []))[1].append(
                    json.loads(_key)[1])

check("some card declares a percent column", bool(_PCT_CARDS), list(_PCT_CARDS))
for _name, (_sql, _cols) in sorted(_PCT_CARDS.items()):
    for _col in _cols:
        # `* 100` and not `* 1000`: `end_day * 1000` converts an epoch
        # day to milliseconds and is not a scaling of anything.
        _scaled = [m.start() for m in re.finditer(r"\bAS\s+" + re.escape(_col) + r"\b", _sql)
                   if re.search(r"\*\s*100(?!\d)",
                                _sql[max(0, m.start() - 60):m.start()])]
        check(f"'{_name}'.{_col} is a fraction, not already a percentage",
              not _scaled, _sql[:300])

# The declared accounts' reserved source has no feed and no snapshot, so
# its latest snapshot reads as the epoch. Every freshness card leaves it
# out, or the stalest source would be that non-feed every day. Checked
# against the engine's own constant, read off disk.
_types_go = open(os.path.join(_REPO, "wealthdb", "internal", "canonical", "types.go"),
                encoding="utf-8").read()
check("DECLARED_SOURCE matches canonical.DeclaredSourceID",
      f'const DeclaredSourceID = "{p.DECLARED_SOURCE}"' in _types_go)
_declared_out = ["!=", ["field", "silver_source_id", {"base-type": "type/Text"}], p.DECLARED_SOURCE]
_FRESH = {**{n: _QDEFS[n] for n in ("Stalest source (days)", "Source freshness")},
          "Source freshness (privacy)": _PDEFS["Source freshness (privacy)"]}
for _name, _tup in _FRESH.items():
    _q = _tup[-2]["query"]
    check(f"'{_name}' leaves the declared source out", _q.get("filter") == _declared_out, _q.get("filter"))

# Freshness is the last read, not the latest snapshot: a source that
# marks quarterly and was read today is current. Both sources models
# carry the read (gold source_observed), and every freshness card
# counts its days from it.
check("both sources models carry when a run last read each source",
      all("source_observed" in MODELS[m][0] and "observed_at" in MODELS[m][0]
          for m in ("report_sources_latest", "report_sources_latest_pct")),
      [MODELS[m][0][:120] for m in ("report_sources_latest", "report_sources_latest_pct")])
_observed = ["field", "observed_at", {"base-type": "type/DateTime"}]
for _name, _tup in _FRESH.items():
    _q = _tup[-2]["query"]
    check(f"'{_name}' counts its days from the last read",
          _q["expressions"]["days_stale"][1] == _observed, _q["expressions"].get("days_stale"))
_valued = ["field", "valued_at", {"base-type": "type/DateTime"}]
_fields = _QDEFS["Source freshness"][-2]["query"]["fields"]
check("'Source freshness' shows the read, the latest mark and the days quiet",
      _observed in _fields and _valued in _fields and ["expression", "days_quiet"] in _fields, _fields)
_quiet = _QDEFS["Source freshness"][-2]["query"]["expressions"]["days_quiet"]
check("the days quiet run from the latest mark, not the snapshot a read dated",
      _quiet[1] == _valued and _quiet[2] == _observed, _quiet)
check("both sources models render valued_at as a timestamp",
      all("epoch_ms(valued_at * 1000) AS valued_at" in MODELS[m][0]
          for m in ("report_sources_latest", "report_sources_latest_pct")),
      [MODELS[m][0][:200] for m in ("report_sources_latest", "report_sources_latest_pct")])
_mig = open(os.path.join(GOLD_MIGRATIONS, "0122_observed_through.sql"), encoding="utf-8").read()
check("source_observed is a gold view", "CREATE OR REPLACE VIEW source_observed" in _mig)
_mig = open(os.path.join(GOLD_MIGRATIONS, "0123_valued_at.sql"), encoding="utf-8").read()
check("the sources reports carry valued_at", "AS valued_at" in _mig and "report_sources_multi" in _mig)

section("an API call that fails is never read as an empty answer")


class FailingListAPI:
    """A stand-in for provision.req whose GETs fail with `status` (0 is
    no HTTP answer at all) and whose writes succeed, recording every
    call so the test can assert that nothing was created."""

    def __init__(self, status, list_ok=False):
        self.status, self.list_ok, self.calls = status, list_ok, []

    def __call__(self, base, path, method="GET", data=None, session=None,
                 timeout=30):
        self.calls.append((method, path))
        if method == "GET":
            if self.list_ok:
                return 200, []
            return self.status, ({} if self.status == 0 else {"message": "unavailable"})
        if self.list_ok:
            return self.status, {"message": "forbidden"}
        return 200, {"id": 1}


real_req = p.req
try:
    # Each of these lists what exists and creates what it did not find.
    # Read as empty, a listing that failed would create a second
    # collection, database, card set or dashboard set of the same name.
    for _st in (0, 500):
        api = p.req = FailingListAPI(_st)
        with quiet():
            _got = (p.ensure_collection("b", "s", p.COLLECTION_NAME),
                    p.pre_defined_cards("b", "s", 7),
                    p.ensure_database("b", "s", "gold", "/data/gold.duckdb"),
                    p.ensure_dashboards("b", "s", 7, card_ids, MID))
        check(f"a failed listing ({_st}) aborts the collection, cards, database "
              "and dashboards", _got == (None, None, None, 1), _got)
        check("...without creating anything",
              not [c for c in api.calls if c[0] != "GET"], api.calls)
    # A collection that cannot be created aborts the run. Falling back to
    # the root collection would scope card matching to it, and update or
    # archive any item there that shares a pre-defined name.
    p.req = FailingListAPI(403, list_ok=True)
    with quiet():
        _coll = p.ensure_collection("b", "s", p.COLLECTION_NAME)
    check("a collection that cannot be created aborts rather than using the root one",
          _coll is None, _coll)
finally:
    p.req = real_req

# A read timeout surfaces from urlopen as TimeoutError, not as URLError,
# and must read as no answer like a refused connection does.
_real_urlopen = p.urllib.request.urlopen


def _timed_out(*_a, **_k):
    raise TimeoutError("read timed out")


p.urllib.request.urlopen = _timed_out
try:
    _ans = p.req("http://b", "/api/health")
finally:
    p.urllib.request.urlopen = _real_urlopen
check("a call that times out reads as no answer", _ans == (0, {}), _ans)

section("the projection cards' SQL, run on an invented book")

# The cards' SQL, rendered the way Metabase fills its template tags and
# run on DuckDB over migration 0138's tables. The book is small and
# invented, and each figure below is worked out by hand in its comment,
# so a wrong join, horizon or weighing changes a number. These are the
# only checks that run the projection's SQL, so a host without the
# duckdb module fails here rather than passing without them.
try:
    import duckdb
except ImportError:
    duckdb = None
check("the duckdb module is installed", duckdb is not None, "make test-web runs these tests with it")

_FIELD = {v: k for k, v in p.FIELD_IDS.items()}


def _render(query, values):
    """Card `query`'s SQL with its tags filled as Metabase fills them: a
    variable takes its value, else its default; a field filter given
    values becomes a predicate on its column, a date one given its first
    and last day a range; an optional clause whose tag has no value drops
    out."""
    sql, tags = query["native"]["query"], query["native"]["template-tags"]

    def value(name):
        return values.get(name, tags[name].get("default"))

    def fill(name):
        tag, v = tags[name], value(name)
        if tag["type"] == "dimension":
            table, col = _FIELD[tag["dimension"][1]]
            if tag["widget-type"].startswith("date"):
                return f'"main"."{table}"."{col}" BETWEEN DATE \'{v[0]}\' AND DATE \'{v[1]}\''
            return f'"main"."{table}"."{col}" IN (' + ", ".join(f"'{x}'" for x in v) + ")"
        return str(v) if tag["type"] == "number" else f"'{v}'"

    def clause(m):
        names = re.findall(r"\{\{(\w+)\}\}", m.group(1))
        if any(value(n) is None for n in names):
            return ""
        return re.sub(r"\{\{(\w+)\}\}", lambda x: fill(x.group(1)), m.group(1))
    sql = re.sub(r"\[\[(.*?)\]\]", clause, sql, flags=re.S)
    return re.sub(r"\{\{(\w+)\}\}", lambda x: fill(x.group(1)), sql)


def _month_start(m):
    """The first day of month m of the invented book, month 0 being
    October 2026."""
    return dt.date(2026 + (9 + m) // 12, (9 + m) % 12 + 1, 1)


def _book():
    """A DuckDB holding migrations 0138's and 0146's tables and views, the
    few gold objects their views read, and the invented book:

    - src-1/acct-1 (USD): cash of 12,000 at the USD benchmark less 1.00,
      and a bond of 8,000 paying 400 in months 2, 8, 14, 20, 26 and 32,
      15% withheld;
    - src-2/acct-2 (CHF, 1 CHF = 1.25 USD): cash of 8,000 CHF (10,000
      USD) at the CHF benchmark, a mortgage of -20,000 USD, and rent of
      100 CHF a month from month 1;
    - over the year to today, in USD: the USD cash received 240 on a
      mean daily value of 11,000, the bond 400 on 8,000, and the CHF
      cash, thin, 150 on 1,000;
    - the USD benchmark 3.00 constant, 3.00 + 0.10 x month forward; the
      CHF one 1.50 on both paths;
    - under the idle rule the CHF cash earns only in months 0 to 5;
    - received: the sold holding's dividend 80 on 15 March 2024; 2025
      interest 50 and the sold holding's dividend 300; 2026 a coupon 400,
      rent 125 and the sold holding's dividend 200, which the withholding
      table would withhold 30 from;
    - the tax src-1 booked as withheld: 12 in 2024, 45 in 2025 and 25 in
      2026, beside a refund of 5, which is no withholding, and a fee of 7,
      which is no tax.

    Today is 2026-10-21, so month 0 is October 2026."""
    db = duckdb.connect()
    db.execute("CREATE TABLE schema_meta (gold_schema_version INTEGER, applied_at BIGINT);"
               "CREATE TABLE accounts (silver_source_id TEXT, account_external_id TEXT, display_name TEXT,"
               " account_kind TEXT, tax_wrapper TEXT, nickname TEXT, account_category TEXT,"
               " portfolio_external_id TEXT);"
               "CREATE TABLE income_account_scope (silver_source_id TEXT, account_external_id TEXT, mode TEXT);"
               "CREATE TABLE serving_transactions (silver_source_id TEXT, account_external_id TEXT,"
               " occurred_at BIGINT, kind TEXT, net_amount DECIMAL(28,4), value_usd DECIMAL(28,4),"
               " value_chf DECIMAL(28,4), value_eur DECIMAL(28,4), value_gbp DECIMAL(28,4));"
               + _latest_definition("account_display_name") + _latest_definition("account_label")
               + _latest_definition("income_scoped_accounts") +
               "CREATE TABLE web_sources_latest (silver_source_id TEXT);"
               "CREATE TABLE web_income (account_label TEXT);")
    db.execute(_MIG_TEXT[138])
    db.execute(_MIG_TEXT[146])

    def insert(table, rows):
        for r in rows:
            db.execute(f"INSERT INTO {table} ({', '.join(r)}) VALUES ({', '.join('?' * len(r))})",
                       list(r.values()))

    insert("accounts", [{"silver_source_id": "src-1", "account_external_id": "acct-1",
                         "display_name": "Brokerage", "account_kind": "brokerage",
                         "tax_wrapper": "taxable_personal"},
                        {"silver_source_id": "src-2", "account_external_id": "acct-2",
                         "display_name": "Savings", "account_kind": "deposit"}])
    insert("web_sources_latest", [{"silver_source_id": s} for s in ("src-1", "src-2")])
    insert("web_income", [{"account_label": "Brokerage (src-1 brokerage)"},
                          {"account_label": "Savings (src-2 deposit)"}])
    as_of = int(dt.datetime(2026, 10, 21, 23, 59, 59, tzinfo=dt.timezone.utc).timestamp())
    # A dollar in each reporting currency.
    per_usd = {"USD": 1.0, "CHF": 0.8}
    held = [("src-1", "acct-1", "cash:USD", "deposit", "cash", "USD", 12000, True, None, 240, 240, 11000, []),
            ("src-1", "acct-1", "isin:XX01", "bond", "fixed_income", "USD", 8000, True, 5.0, 800, 400, 8000, []),
            ("src-2", "acct-2", "cash:CHF", "deposit", "cash", "CHF", 10000, True, None, 150, 150, 1000, ["thin"]),
            ("src-2", "acct-2", "loan:1", "liability", "real_estate", "CHF", -20000, False, 2.0, None, 0, -20000, [])]
    insert("report_yield_positions", [
        {"computed_at": 0, "as_of": as_of, "currency": ccy, "silver_source_id": src,
         "account_external_id": acct, "position_key": key, "kind": kind, "asset_class": cls,
         "holding_currency": hc, "value": v * k, "weighs": w, "implied_pct": imp,
         "income_12m": None if inc is None else inc * k, "received_12m": rec * k, "mean_value": mean * k,
         "tags": ["stated", *tags]}
        for src, acct, key, kind, cls, hc, v, w, imp, inc, rec, mean, tags in held for ccy, k in per_usd.items()])
    fx = {"USD": {"fx_USD": 1.0, "fx_CHF": 0.8}, "CHF": {"fx_USD": 1.25, "fx_CHF": 1.0}}
    parts = []
    for rule in ("cash", "idle"):
        for m in range(37):
            base = {"computed_at": 0, "rule": rule, "month": m, "month_start": _month_start(m), "month_only": False,
                    "basis": "stated", "fixed": 0.0, "principal": 0.0, "spread": 0.0, "roll_constant": 0.0,
                    "roll_forward": 0.0, "roll_principal": 0.0}
            parts.append({**base, **fx["USD"], "silver_source_id": "src-1", "account_external_id": "acct-1",
                          "position_key": "cash:USD", "holding_currency": "USD", "type": "interest",
                          "principal": 12000.0, "spread": -1.0})
            if m % 6 == 2:
                parts.append({**base, **fx["USD"], "silver_source_id": "src-1", "account_external_id": "acct-1",
                              "position_key": "isin:XX01", "holding_currency": "USD", "type": "coupons",
                              "basis": "declared", "fixed": 400.0, "day": _month_start(m) + dt.timedelta(days=14),
                              "withholding_pct": 15.0})
            if rule == "cash" or m <= 5:
                parts.append({**base, **fx["CHF"], "silver_source_id": "src-2", "account_external_id": "acct-2",
                              "position_key": "cash:CHF", "holding_currency": "CHF", "type": "interest",
                              "principal": 8000.0})
            if m >= 1:
                parts.append({**base, **fx["CHF"], "silver_source_id": "src-2", "account_external_id": "acct-2",
                              "position_key": "income:rent", "holding_currency": "CHF", "type": "rent",
                              "basis": "realized", "fixed": 100.0, "day": _month_start(m)})
    insert("report_yield_parts", parts)
    insert("report_yield_rates", [{"computed_at": 0, "currency": c, "month": m, "path": path, "rate_pct": r}
                                  for m in range(37)
                                  for c, path, r in (("USD", "constant", 3.0), ("USD", "forward", 3.0 + 0.1 * m),
                                                     ("CHF", "constant", 1.5), ("CHF", "forward", 1.5))])
    received = [("src-1", "acct-1", "cash:USD", dt.date(2025, 3, 15), "interest", 50.0, None),
                ("src-1", "acct-1", "isin:SOLD", dt.date(2025, 6, 15), "dividends", 300.0, None),
                ("src-1", "acct-1", "isin:XX01", dt.date(2026, 2, 15), "coupons", 400.0, None),
                ("src-2", "acct-2", "income:rent", dt.date(2026, 5, 1), "rent", 125.0, None),
                ("src-1", "acct-1", "isin:SOLD", dt.date(2026, 7, 15), "dividends", 200.0, 30.0),
                ("src-1", "acct-1", "isin:SOLD", dt.date(2024, 3, 15), "dividends", 80.0, None)]
    insert("report_yield_received", [
        {"computed_at": 0, "currency": ccy, "silver_source_id": src, "account_external_id": acct,
         "position_key": key, "day": day, "type": typ, "value": v * k,
         "withheld": None if w is None else w * k}
        for src, acct, key, day, typ, v, w in received for ccy, k in per_usd.items()])

    def at(day):
        return int(dt.datetime(day.year, day.month, day.day, 9, tzinfo=dt.timezone.utc).timestamp())
    insert("serving_transactions", [
        {"silver_source_id": "src-1", "account_external_id": "acct-1", "occurred_at": at(day), "kind": kind,
         "net_amount": v, **{f"value_{c.lower()}": v * per_usd.get(c, 1.0) for c in p.REPORTING_CURRENCIES}}
        for day, kind, v in ((dt.date(2024, 3, 15), "tax", -12.0), (dt.date(2025, 6, 15), "tax", -45.0),
                             (dt.date(2026, 7, 15), "tax", -25.0),
                             (dt.date(2026, 8, 1), "tax", 5.0), (dt.date(2026, 8, 1), "fee", -7.0))])
    return db


if duckdb is not None:
    _db = _book()

    def _run(name, **values):
        """Card `name`'s rows on the book, its tags at `values`."""
        return _db.execute(_render(CARDS[name][2], values)).fetchall()

    def _one(name, **values):
        return _run(name, **values)[0][0]

    def _near(a, b):
        return a is not None and abs(a - b) < 1e-9

    # The income yield: months 1..12 pay 12 x 20 on the USD cash (12,000
    # x (3.00 - 1.00) / 1200), two coupons of 400 and 12 x 12.50 on the
    # CHF cash (8,000 x 1.50 / 1200 CHF at 1.25): 1,190 over the 30,000
    # the assets are worth, the mortgage left out.
    for _args, _want in (({}, 1190.0 / 300),
                         # Forward over 24 months: the USD cash earns
                         # 20 + m in month m, 780 in all; four coupons
                         # 1,600; the CHF cash 300: 2,680 x 12 / 24.
                         ({"rates": "forward", "horizon": "24"}, 2680.0 * 12 / 24 / 300)):
        _got = _one("Income yield", **_args)
        check(f"the income yield under {_args or 'the defaults'} is the book's", _near(_got, _want), (_got, _want))
    for _args, _want in (({}, 1190.0),
                         # 100bp lower: the USD cash earns 10 a month,
                         # the CHF cash 8,000 x 0.50 / 1200 x 1.25.
                         ({"shift": -100}, 120 + 800 + 50.0),
                         # Idle: the CHF cash earns in months 1 to 5 only.
                         ({"maturing": "idle"}, 240 + 800 + 62.5),
                         # In francs: the dollar parts at 0.8.
                         ({"currency": "CHF"}, 192 + 640 + 120.0)):
        _got = _one("Projected income", **_args)
        check(f"the projected income under {_args or 'the defaults'} is the book's", _near(_got, _want),
              (_got, _want))
    # One percent more on the floating principal over 24 months: 12,000
    # / 1200 x 24 in dollars, 8,000 / 1200 x 24 x 1.25 in francs.
    _sens = dict(_run("Income sensitivity to rates", horizon="24"))
    check("the sensitivity over 24 months is each currency's floating principal at one percent",
          _near(_sens.get("USD"), 240.0) and _near(_sens.get("CHF"), 200.0), _sens)
    # 200bp lower, the franc cash's 1.50 would earn -0.50: it floors at
    # zero. The dollar cash earns 3.00 - 1.00 - 2.00 = 0. So the twelve
    # months pay the two coupons alone, and a point more lifts the franc
    # cash only half a point above the floor: 8,000 x 0.50 / 1200 x 12 x
    # 1.25, the dollar cash a full point: 12,000 / 1200 x 12.
    _got = _one("Projected income", shift=-200)
    check("200bp lower, the franc cash earns zero, not a charge", _near(_got, 800.0), (_got, 800.0))
    _sens = dict(_run("Income sensitivity to rates", shift=-200))
    check("...and a point more lifts it half a point above the floor",
          _near(_sens.get("USD"), 120.0) and _near(_sens.get("CHF"), 50.0), _sens)
    # A part's income on its own, as yield.Part.Income's test reads it: a
    # roll at a quarter point (half a point forward), a loan at half a
    # point, a fixed coupon; the month's benchmark is 0.50.
    def _part(rates, shift, **fields):
        cols = {"fixed": 0, "principal": 0, "spread": 0, "roll_constant": 0, "roll_forward": 0,
                "roll_principal": 0, **fields}
        sql = p.PART_INCOME.replace("{{rates}}", f"'{rates}'").replace("{{shift}}", str(shift))
        return _db.execute("SELECT " + sql + " FROM (SELECT " +
                           ", ".join(f"{v}::DOUBLE AS {k}" for k, v in cols.items()) +
                           ") p, (SELECT 0.5::DOUBLE AS rate_pct) rt").fetchone()[0]
    _roll = {"roll_constant": 100000 * 0.25 / 1200, "roll_forward": 100000 * 0.5 / 1200, "roll_principal": 100000}
    for _what, _args, _fields, _want in (
            ("a roll at its rate", ("constant", 0), _roll, 100000 * 0.25 / 1200),
            ("a roll 100bp higher", ("constant", 100), _roll, 100000 * 1.25 / 1200),
            ("a roll 100bp lower", ("constant", -100), _roll, 0.0),
            ("a roll on the forward path 40bp lower", ("forward", -40), _roll, 100000 * 0.1 / 1200),
            ("a loan 100bp lower", ("constant", -100), {"principal": -100000}, 0.0),
            ("a loan at its rate", ("constant", 0), {"principal": -100000}, -100000 * 0.5 / 1200),
            ("a fixed coupon 500bp lower", ("constant", -500), {"fixed": 50}, 50.0)):
        _got = _part(*_args, **_fields)
        check(f"{_what} earns as yield.Part.Income gives it", _near(_got, _want), (_got, _want))
    # The implied yield weighs the assets: the bond's 5.00, not the
    # mortgage's 2.00.
    check("the implied yield weighs the assets alone", _near(_one("Implied yield"), 5.0), _one("Implied yield"))
    # The realized yield pools the assets: 240 + 400 + 150 over 11,000 +
    # 8,000 + 1,000, in either currency. The cash alone is 390 over
    # 12,000. The second source's one asset is thin, and the card, which
    # sees no days, still pools it: 150 over 1,000. The mortgage alone is
    # no asset: nothing received over its debt reads zero, not -0.
    for _args, _want in (({}, 3.95), ({"currency": "CHF"}, 3.95), ({"asset_class": ["cash"]}, 3.25),
                         ({"source": ["src-2"]}, 15.0), ({"asset_class": ["real_estate"]}, 0.0)):
        _got = _one("Realized yield", **_args)
        check(f"the realized yield under {_args or 'the defaults'} pools the holdings",
              _near(_got, _want) and math.copysign(1, _got) > 0, (_got, _want))
    check("...as the yields over time read it",
          [r[3] for r in _run("Yields over time")] == [_one("Realized yield")], _run("Yields over time"))
    # What was received over the Time range, every day received without
    # one: 80 in 2024, 50 + 300 in 2025, 400 + 125 + 200 in 2026. What is
    # expected over the Horizon, the rest of October and the twelve
    # months after it: 13 x 20 on the dollar cash, the coupons of December
    # and June, 13 x 12.50 on the franc cash and twelve months of rent at
    # 125; over 24 months 25 x 20, four coupons, 25 x 12.50 and 24 x 125.
    _y24, _y25, _y26, _y27 = (("2024-01-01", "2024-12-31"), ("2025-01-01", "2025-12-31"),
                              ("2026-01-01", "2026-12-31"), ("2027-01-01", "2027-12-31"))
    _received = 80 + 350 + 725.0
    _exp12, _exp24 = 260 + 800 + 162.5 + 1500, 500 + 1600 + 312.5 + 3000
    for _args, _want in (({}, _received + _exp12), ({"time_range": _y24}, 80 + _exp12),
                         ({"time_range": _y25}, 350 + _exp12), ({"time_range": _y26}, 725 + _exp12),
                         ({"time_range": _y27}, _exp12), ({"time_range": _y26, "horizon": "24"}, 725 + _exp24)):
        _got = _one("Income in the window", **_args)
        check(f"the income under {_args or 'the defaults'} is what the Time range received and the Horizon "
              "expects", _near(_got, _want), (_got, _want))
    check("the received tiles count what the Time range received, before last year too",
          _near(_one("Received income"), _received) and _near(_one("Received income", time_range=_y26), 725.0)
          and _near(_one("Received dividends", time_range=_y26), 200.0)
          and _near(_one("Received interest", time_range=_y25), 50.0)
          and _near(_one("Received dividends", time_range=_y24), 80.0)
          and _near(_one("Received income", time_range=_y27), 0.0))
    check("the expected tiles count what the Horizon expects",
          _near(_one("Expected remaining"), _exp12) and _near(_one("Expected remaining", horizon="24"), _exp24)
          and _near(_one("Expected interest remaining"), 260 + 800 + 162.5)
          and _near(_one("Expected dividends remaining"), 0.0))
    # The days received run from the first, 15 March 2024, to 21 October
    # 2026: 17/31 of March, 30 months, and 21/31 of October. The horizon
    # adds 12 and the 10/31 of October left: 43 and 17/31 months. Over
    # 2026 the days received count 9 and 21/31: 22 months.
    _all_months = 43 + 17 / 31
    check("the monthly average divides by the months the days received and the horizon cover",
          _near(_one("Monthly average income"), (_received + _exp12) / _all_months)
          and _near(_one("Monthly average income", time_range=_y26), (725 + _exp12) / 22)
          and _near(_one("Monthly average income", time_range=_y26, horizon="24"), (725 + _exp24) / 34))
    # Withheld: the tax src-1 booked, 12, 45 and 25, not the 30 the table
    # would withhold from the dividend received; and 15% of the two
    # coupons expected. The second source withholds nothing at all.
    check("the withholding counts the tax booked over the Time range and the table's over the Horizon",
          _near(_one("Withholding"), 82 + 120.0) and _near(_one("Withholding", time_range=_y26), 25 + 120.0)
          and _near(_one("Withholding", time_range=_y24), 12 + 120.0)
          and _near(_one("Withholding", time_range=_y27, horizon="24"), 240.0), _one("Withholding"))
    check("...the Account picker keeping an account's tax, the Currency picker its value",
          _near(_one("Withholding", account=["Brokerage (src-1 brokerage)"]), 202.0)
          and _one("Withholding", account=["Savings (src-2 deposit)"]) == 0
          and _near(_one("Withholding", currency="CHF"), 0.8 * 202.0),
          (_one("Withholding", account=["Savings (src-2 deposit)"]), _one("Withholding", currency="CHF")))
    check("...and the income after it the rest",
          _near(_one("Income after withholding"), _received + _exp12 - 202.0), _one("Income after withholding"))
    check("where nothing is withheld, the withholding tiles read 0, not null",
          _one("Withholding", source=["src-2"]) == 0 and _one("Withholding (privacy)", source=["src-2"]) == 0
          and _near(_one("Income after withholding", source=["src-2"]), 125 + 162.5 + 1500.0)
          and _near(_one("Income after withholding (privacy)", source=["src-2"]), 100.0))
    # A twin's share of nothing reads 0, as the base figure does.
    _scalars = [n for n in _in_all if CARDS[n][0] == "scalar"]
    _nulls = [(n, a) for n in _scalars for a in ({}, {"source": ["src-2"]}, {"time_range": _y27})
              if _one(n, **a) is None]
    check("no headline figure reads null, at the defaults, for one source, or over days not yet come",
          len(_scalars) == 20 and not _nulls, _nulls)
    check("...the twin's received figures reading 0 where nothing was received",
          all(_one(n + p.PRIVACY_SUFFIX, time_range=_y27) == 0
              for n in ("Received income", "Received interest", "Received dividends")))
    # With no yield report there is no day received: the received figures
    # read 0 and the monthly average divides by the horizon alone.
    _db.execute("BEGIN")
    _db.execute("DELETE FROM report_yield_positions")
    _empty = {n: _one(n) for n in _scalars}
    _db.execute("ROLLBACK")
    check("with no yield report no figure reads null, and the average is the horizon's",
          not [n for n, v in _empty.items() if v is None] and _empty["Received income"] == 0
          and _near(_empty["Income in the window"], _exp12)
          and _near(_empty["Monthly average income"], _exp12 / 12), _empty)
    # The second source's: its rent received, its cash's 162.50 and
    # twelve months of rent expected.
    check("the Source picker keeps a source's received and expected rows",
          _near(_one("Income in the window", source=["src-2"]), 125 + 162.5 + 1500.0),
          _one("Income in the window", source=["src-2"]))
    _accounts = _run("Received and expected by account")
    check("the accounts table splits each account's received and expected income",
          [(r[0], round(r[1], 6), round(r[2], 6), round(r[3], 6)) for r in _accounts]
          == [("Brokerage (src-1 brokerage)", 1030.0, 1060.0, 2090.0),
              ("Savings (src-2 deposit)", 125.0, 1662.5, 1787.5)], _accounts)
    _months = {(m.date().isoformat(), s): v for m, s, v in _run("Received and expected by month")}
    check("the by-month chart's bands are the Time range's received and the Horizon's expected",
          _near(sum(_months.values()), _received + _exp12)
          and _near(_months.get(("2026-12-01", "coupons (expected)")), 400.0)
          and _near(_months.get(("2026-02-01", "coupons (received)")), 400.0)
          and _near(_months.get(("2024-03-01", "dividends (received)")), 80.0)
          and _near(_months.get(("2026-10-01", "interest (expected)")), 32.5)
          and ("2027-11-01", "interest (expected)") not in _months, _months)
    _months26 = {(m.date().isoformat(), s) for m, s, _v in _run("Received and expected by month", time_range=_y26)}
    check("...the received ones moving with the Time range",
          ("2025-06-01", "dividends (received)") not in _months26
          and ("2026-02-01", "coupons (received)") in _months26)
    # The twins divide by the biggest month. Each of the horizon's months
    # pays 32.50, 20 on the dollar cash and 12.50 on the franc cash, and
    # months 2 and 8 the coupon of 400 too, each month but October the
    # rent of 125: December and June hold most, 557.50.
    check("the twins divide by the biggest month",
          _near(_one("Projected income (privacy)"), 100 * 1190 / 12 / 432.5)
          and _near(_one("Monthly average income (privacy)"), 100 * (_received + _exp12) / _all_months / 557.5)
          and all(_near(v, 100 * _months[(m.date().isoformat(), s)] / 557.5)
                  for m, s, v in _run("Received and expected by month (privacy)")),
          (_one("Projected income (privacy)"), _one("Monthly average income (privacy)")))
    # The bond of 8,000 received 400 over the 31 months and 7/31 of days
    # received and expects 800 over the horizon's 12 and 10/31: each a
    # yield per year on its value.
    _bond = [r for r in _run("Received and expected by position (privacy)") if r[1] is None and r[2] == "(unnamed)"
             and r[3] is not None and _near(r[3], 100 * 8000 / 30000)]
    check("the positions twin gives each position's income as a yield on its value",
          len(_bond) == 1 and _near(_bond[0][4], 100 * 400 * 12 / (_all_months - 12 - 10 / 31) / 8000)
          and _near(_bond[0][5], 100 * 800 * 12 / (12 + 10 / 31) / 8000)
          and _near(_bond[0][6], 100 * 1200 * 12 / _all_months / 8000), _bond)
    _names = {r[2] for r in _run("Received and expected by position (privacy)")}
    check("the positions twin keeps a cash or income key and hides a holding's own",
          {"cash:USD", "cash:CHF", "income:rent", "(unnamed)"} <= _names and "isin:SOLD" not in _names, _names)
    check("...where the money card reads the key",
          "isin:SOLD" in {r[3] for r in _run("Received and expected by position")})
    # A click on a twin cell sets the Position picker to the name shown:
    # a cash balance's key, or (unnamed) for the bond, whose key is its own.
    _picked = {k: {r[2] for r in _run("Expected payments (privacy)", position=k)}
               for k in ("cash:USD", "(unnamed)")}
    check("the twin's calendar lists the payments of the name a click picks",
          _picked == {"cash:USD": {"cash:USD"}, "(unnamed)": {"(unnamed)"}}, _picked)
    check("...where the money card's calendar knows no (unnamed)",
          not _run("Expected payments", position="(unnamed)") and _run("Expected payments", position="isin:XX01"))

    section("the Exposure bucket cards' SQL, run on an invented book")

    # The credit rating card on a book of 4,000 debt and cash beside 800
    # of stocks, today, in each of two sources: AAA 600, BBB 2,000,
    # unrated 199.9999 and cash 1,000 in the one, a B line of 0.0001 in
    # the other. Each bucket is a share of the 3,800 of debt and cash, so
    # the bars sum to 100; the B line, under a hundredth of a percent of
    # it, is left out of them.
    _db.execute("DELETE FROM report_exposure_debt")
    _today = _db.execute("SELECT max(as_of) FROM report_yield_positions").fetchone()[0]
    for _src, _bucket, _v in (("src-1", "AAA", 600.0), ("src-1", "BBB", 2000.0), ("src-1", "unrated", 199.9999),
                              ("src-1", "cash", 1000.0), ("src-2", "B", 0.0001), ("src-1", "(none)", 800.0)):
        _db.execute("INSERT INTO report_exposure_debt VALUES (0, 'USD', ?, ?, NULL, 'credit', ?, FALSE, ?)",
                    [_today, _src, _bucket, _v])
    _credit_rows = _run(_credit)
    _shares = {r[0]: r[-1] for r in _credit_rows}
    check("the credit bars are each bucket's share of the debt and cash, summing to 100",
          list(_shares) == ["AAA", "BBB", "unrated", "cash"]
          and _near(_shares["BBB"], 100 * 2000 / 3800) and abs(sum(_shares.values()) - 100) < p.SMALLEST_BAR_PCT
          and [r[-1] for r in _run(p.privacy_name(_credit))] == list(_shares.values()), _credit_rows)
    check("...and a source's own, whose one bucket is the whole of its class",
          [(r[0], r[-1]) for r in _run(_credit, source=["src-2"])] == [("B", 100.0)],
          _run(_credit, source=["src-2"]))
    # An overdraft that outweighs the debt leaves no share of the class,
    # as the CLI's class_share has none: the buckets stay, blank.
    for _bucket, _v in (("AAA", 1000.0), ("cash", -2000.0)):
        _db.execute("INSERT INTO report_exposure_debt VALUES (0, 'USD', ?, 'src-3', NULL, 'credit', ?, FALSE, ?)",
                    [_today, _bucket, _v])
    _over = [(r[0], r[-1]) for r in _run(_credit, source=["src-3"])]
    check("a class that sums to less than nothing gives no bucket a share",
          _over == [("AAA", None), ("cash", None)], _over)
    # The four cards over the book the gold test holds `wealthdb exposure
    # <view>` to (testdata/class_share_book.json): each bar is the class
    # share the command prints, and the bars sum to 100.
    with open(os.path.join(_REPO, "wealthdb", "internal", "gold", "testdata", "class_share_book.json"),
              encoding="utf-8") as _f:
        _book = json.load(_f)
    _cards = {"credit": (_credit, "credit"), "maturities": ("Debt by maturity", "maturity"),
              "coupons": ("Debt by coupon kind", "coupon"), "cycles": ("Public equity by cycle", "cycle")}
    check("the class share book holds the four bucket views", set(_book) == set(_cards), sorted(_book))
    for _view, _rows in _book.items():
        for _row in _rows:
            _db.execute("INSERT INTO report_exposure_debt VALUES (0, 'USD', ?, 'src-9', NULL, ?, ?, FALSE, ?)",
                        [_today, _cards[_view][1], _row["bucket"], float(_row["value"])])
    for _view, (_name, _dim) in _cards.items():
        _want = [(r["bucket"], r["class_share"]) for r in _book.get(_view, []) if r["bucket"] != "(none)"]
        for _n in (_name, p.privacy_name(_name)):
            _bars = _run(_n, source=["src-9"])
            _got = [(r[0], f"{float(r[-1]):.2f}") for r in _bars]
            check(f"'{_n}' draws the class shares `wealthdb exposure {_view}` prints, summing to 100",
                  bool(_want) and _got == _want and abs(sum(r[-1] for r in _bars) - 100) < 1e-9, (_got, _want))

    section("the Cash Flow projection cards' SQL, run on an invented book")

    # The book above, and migration 0149's look-back parts: each month in
    # dollars 50 of other income, 910 out (700 consumption, 10 fees, 200
    # taxes), a debt service of 300 that ends after month 18, and 100 paid
    # into a vehicle; in francs the same at 0.8. The bond returns 5,000 of
    # principal in month 3 under every rule. A retirement plan, outside
    # the pool, expects a dividend of 60 dollars each month from the first
    # and returns 1,000 of principal in month 5 under every rule. The parts
    # are of the cash and idle rules alone, so under roll nothing is
    # expected.
    _db.execute("CREATE TABLE cashflow_account_scope (silver_source_id TEXT, account_external_id TEXT, mode TEXT);"
                "CREATE TABLE cashflow_wrapper_sides (tax_wrapper TEXT, side TEXT, class TEXT);"
                "INSERT INTO cashflow_wrapper_sides VALUES ('roth_ira', 'vehicle', 'retirement');"
                "INSERT INTO accounts (silver_source_id, account_external_id, display_name, account_kind, tax_wrapper)"
                " VALUES ('src-3', 'acct-3', 'Plan', 'brokerage', 'roth_ira');"
                + _latest_definition("cashflow_pool_accounts"))
    _db.execute(_MIG_TEXT[149])
    _db.executemany("INSERT INTO report_yield_parts (computed_at, silver_source_id, account_external_id, "
                    "position_key, holding_currency, rule, month, month_start, month_only, type, basis, fixed, "
                    "principal, spread, roll_constant, roll_forward, roll_principal, fx_USD, fx_CHF) "
                    "VALUES (0, 'src-3', 'acct-3', 'isin:XX02', 'USD', ?, ?, ?, FALSE, 'dividends', 'stated', 60, "
                    "0, 0, 0, 0, 0, 1.0, 0.8)",
                    [(r, m, _month_start(m)) for r in ("cash", "idle") for m in range(1, 37)])
    _db.executemany("INSERT INTO report_yield_events (computed_at, silver_source_id, account_external_id, "
                    "position_key, holding_currency, rule, month, day, type, amount, fx_USD, fx_CHF) "
                    "VALUES (0, 'src-3', 'acct-3', 'isin:XX03', 'USD', ?, 5, ?, 'maturity', 1000, 1.0, 0.8)",
                    [(r, _month_start(5) + dt.timedelta(days=9)) for r in ("cash", "idle", "roll")])
    _out = {"consumption": 700.0, "fees": 10.0, "taxes": 200.0, "giving": 0.0, "(uncategorized)": 0.0}
    _plan_rows = []
    for _ccy, _k in (("USD", 1.0), ("CHF", 0.8)):
        for _m in range(1, 37):
            _parts = [("other_income", "", 50.0), *(("operating_out", c, v) for c, v in _out.items()),
                      ("debt_service", "", 300.0 if _m <= 18 else 0.0), ("vehicles", "", -100.0)]
            _plan_rows += [(_ccy, _m, _month_start(_m), part, cls, v * _k, "trailing terms") for part, cls, v in _parts]
    _db.executemany("INSERT INTO report_cashflow_plan VALUES (0, ?, ?, ?, ?, ?, ?, ?)", _plan_rows)
    _db.executemany("INSERT INTO report_yield_events (computed_at, silver_source_id, account_external_id, "
                    "position_key, holding_currency, rule, month, day, type, amount, fx_USD, fx_CHF) "
                    "VALUES (0, 'src-1', 'acct-1', 'isin:XX01', 'USD', ?, 3, ?, 'maturity', 5000, 1.0, 0.8)",
                    [(r, _month_start(3) + dt.timedelta(days=14)) for r in ("cash", "idle", "roll")])

    def _plan(name="Cash flow projection", **values):
        """The projection table's rows on the book, by month index."""
        return {k + 1: r for k, r in enumerate(_run(name, **values))}

    # Each month the pool expects 20 on the dollar cash, 12.50 on the
    # franc cash and 125 of rent, months 2 and 8 the coupon of 400 too:
    # 2,690 over twelve months, and the projected dividends 720 more. Month
    # 3's net is 157.50 + 5,000 + 50 - 910 - 300 - 100.
    _t = _plan()
    check("the projection lists the Horizon's months from the one after this",
          len(_t) == 12 and _t[1][0].date() == _month_start(1) and _t[12][0].date() == _month_start(12), _t.get(1))
    check("...each part as `wealthdb cashflow projection` reads it, and the quality",
          all(_near(a, b) for a, b in zip(_t[3][1:9], (157.5, 5000, 60, 50, 910, 300, -100, 3897.5), strict=True))
          and _near(_t[2][1], 557.5) and _near(_t[2][8], -702.5) and _t[3][9] == "projected trailing terms",
          (_t[2], _t[3]))
    check("...the vehicles' income and principal apart from the pool's, out of the net",
          _near(_t[5][3], 1060) and _t[5][2] == 0
          and _near(_t[5][8], _t[5][1] + 50 - 910 - 300 - 100), _t[5])
    # The pickers move the figures: the Horizon's months, the rates, the
    # shift, the rule and the currency.
    _t24 = _plan(horizon="24")
    check("the Horizon sets the months, the debt service ending where its terms do",
          len(_t24) == 24 and _near(_t24[18][6], 300) and _t24[19][6] == 0, (_t24[18], _t24[19]))
    check("the Rate path and the Shift move the investment income",
          _near(_plan(rates="forward")[1][1], 21 + 12.5 + 125)
          and _near(_plan(shift=-100)[1][1], 10 + 8000 * 0.5 / 1200 * 1.25 + 125))
    _troll = _plan(maturing="roll")
    check("under roll the principal shows but buys its successor, out of the net",
          _troll[3][1] == 0 and _near(_troll[3][2], 5000) and _near(_troll[3][8], 50 - 910 - 300 - 100)
          and _troll[1][3] == 0 and _near(_troll[5][3], 1000), (_troll[3], _troll[5]))
    check("the Currency picks the parts' rate and the look-back's own rows",
          all(_near(a, 0.8 * b) for a, b in zip(_plan(currency="CHF")[3][1:9], _t[3][1:9], strict=True)))
    # The chart's bars sum to its net line, the outflows below the axis;
    # the vehicles' income is a line of its own.
    _bars = _run("Cash flow projection by month")
    check("the chart's bars stack to the net, the outflows negative",
          len(_bars) == 12 and all(_near(sum(r[1:7]), r[7]) for r in _bars)
          and _near(_bars[2][5], -910) and _near(_bars[2][6], -300) and _near(_bars[2][3], 5000)
          and _near(_bars[4][8], 1060), (_bars[2], _bars[4]))
    check("...and under roll draw no principal",
          all(r[3] == 0 and _near(sum(r[1:7]), r[7]) for r in _run("Cash flow projection by month", maturing="roll")))
    # The twins divide by the 3,410 the twelve months expect, the projected
    # dividends included, and blank where nothing is expected.
    _tw = _plan("Cash flow projection (privacy)")
    check("the twin's figures are shares of the horizon's expected income",
          _near(_tw[3][5], 100 * 910 / 3410) and _near(_tw[3][8], 100 * 3897.5 / 3410)
          and _near(sum(r[1] + r[3] for r in _tw.values()), 100 * (3410 + 1000) / 3410)
          and _tw[3][9] == "projected trailing terms", _tw[3])
    check("...the chart's alike",
          all(_near(a, 100 * b / 3410) for ra, rb in zip(_run("Cash flow projection by month (privacy)"), _bars, strict=True)
              for a, b in zip(ra[1:], rb[1:], strict=True)))
    check("...and blank where the horizon expects no income",
          all(v is None for r in _plan("Cash flow projection (privacy)", maturing="roll").values() for v in r[1:9]))

if FAILS:
    print(f"provision tests: {FAILS} failed")
else:
    print("provision tests: all passed")
sys.exit(1 if FAILS else 0)
