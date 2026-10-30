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

# ---- the wait for Metabase --------------------------------------------

section("the wait for Metabase")
_answers = iter([(503, {}), (0, {}), (200, {})])
_real_req = p.req
try:
    p.req = lambda *_a, **_k: next(_answers)
    _err = io.StringIO()
    with contextlib.redirect_stderr(_err):
        _up = p.wait_health("b", limit=5, delay=0)
finally:
    p.req = _real_req
check("the wait polls until Metabase answers and says how long it took",
      _up is True and re.search(r"provision: timing: wait for Metabase [0-9.]+s", _err.getvalue()), _err.getvalue())
try:
    p.req = lambda *_a, **_k: (503, {})
    _up = p.wait_health("b", limit=0.05, delay=0.01)
finally:
    p.req = _real_req
check("...and gives up after its limit", _up is False)

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
# their descriptions say which categories it leaves out. The share rings
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
# The As of day defaults to today. A card reads the last history day on
# or before it, so the default is the same whatever the snapshot.
_asof = [x for x in p.dashboard_parameters(MID, "asof", "Allocation") if x["slug"] == "as_of_day"][0]
check("Allocation's As of day is required and defaults to today",
      _asof["required"] is True and _asof["default"] == "thisday", _asof)
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
             p.DEFAULT_CURRENCY, p.DEFAULT_MISSING_BASIS, p.FIELD_IDS)
    sys.argv = ["provision.py", "--base", "http://127.0.0.1:3000",
                "--email", "admin@wealthdb.local", "--gold-path", "/gold/wealthdb.db"]
    os.environ["WEALTHDB_WEB_ADMIN_PASSWORD"] = "Synthetic-pw-1!"
    try:
        with quiet():
            return p.main()
    finally:
        sys.argv, pw, p.DEFAULT_CURRENCY, p.DEFAULT_MISSING_BASIS, p.FIELD_IDS = saved
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
    api = p.req
    buf = io.StringIO()
    with contextlib.redirect_stdout(buf):
        proceeds = p.ensure_synced("b", "s", 1, tries=1, delay=0) is not None
    check("...and provisioning proceeds", proceeds)
    check("...without a schema sync, which it says it skipped",
          not [c for c in api.calls if "sync_schema" in c[1]]
          and "provision: schema sync skipped" in buf.getvalue(), str(api.calls))

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
    """The migrations that create or redefine view, table or macro `obj`."""
    pat = rf"CREATE (?:OR REPLACE )?(?:VIEW|TABLE|MACRO) (?:IF NOT EXISTS )?{obj}\b"
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
        rc = p.ensure_dashboards("b", "s", 7, card_ids, MID, None, {n: c[1] for n, c in CARDS.items()})
    check("ensure_dashboards lays out every dashboard", rc == 0)
finally:
    p.req = real_req

section("each provisioning pass prints its counts on one line")


class FakeSaveAPI:
    def __init__(self):
        self.next_id = 0

    def __call__(self, base, path, method="GET", data=None, session=None, timeout=30):
        if method == "POST" and path in ("/api/card", "/api/dashboard"):
            self.next_id += 1
            return 200, {"id": self.next_id}
        return 200, {}


buf = io.StringIO()
real_req = p.req
try:
    p.req = FakeSaveAPI()
    with contextlib.redirect_stdout(buf):
        model_ids = p.ensure_models("b", "s", 1, 7, {})
        check("models save", model_ids is not None)
        saved = p.ensure_cards("b", "s", 1, 7, {}, model_ids)
        check("cards save", saved is not None)
        rc = p.ensure_dashboards("b", "s", 7, saved[0], model_ids, None, saved[1])
        check("dashboards save", rc == 0)
finally:
    p.req = real_req
printed = buf.getvalue()
n_cards = len(p.question_defs(1, model_ids)) + len(p.privacy_card_defs(1, model_ids))
for want in (f"provision: report models ({p._ccy_slash()}) — {len(p.report_models())} created, "
             f"0 updated, 0 unchanged, 0 retired",
             f"provision: questions — {n_cards} created, 0 updated, 0 unchanged, 0 retired",
             f"provision: dashboards — {len(p.dashboard_defs())} created, 0 updated, "
             "0 unchanged, 0 retired"):
    check(f"prints '{want.split(' — ')[0]}' with its counts", want in printed, printed)
check("...and one line per pass", printed.count("provision: ") == 3, printed)

section("a card already on the server is not sent again")
TAG_REF = ["field", 41, None]
payload = {"name": "n", "type": "question", "description": "d",
           "collection_id": 7, "display": "bar",
           "visualization_settings": {"graph.dimensions": ["a"], "graph.show_values": True},
           "dataset_query": {"type": "native", "database": 1,
                             "native": {"query": "SELECT 1 [[AND {{source}}]]",
                                        "template-tags": {"source": {
                                            "id": "s", "name": "source", "type": "dimension",
                                            "dimension": TAG_REF, "default": None}}}}}


def server_card(body, card_id=9):
    """The card as Metabase v0.59 returns it in the legacy form: the
    template tag's field reference typed, an empty tag map left out."""
    full = json.loads(json.dumps(body))
    full["id"] = card_id
    native = full["dataset_query"].get("native") or {}
    for tag in (native.get("template-tags") or {}).values():
        if tag.get("dimension"):
            tag["dimension"][2] = {"base-type": "type/Text"}
    if native.get("template-tags") == {}:
        del native["template-tags"]
    return full


stored = server_card(payload)
check("an equal card matches", p.card_same(stored, payload))
changed = dict(stored, description="other")
check("a changed card does not match", not p.card_same(changed, payload))
check("a card without its query does not match", not p.card_same({"id": 9, "name": "n"}, payload))
dropped = json.loads(json.dumps(payload))
del dropped["visualization_settings"]["graph.show_values"]
check("a payload that drops a visualization setting does not match",
      not p.card_same(stored, dropped))
untagged = json.loads(json.dumps(payload))
untagged["dataset_query"]["native"]["template-tags"] = {}
check("a payload that drops a template tag does not match", not p.card_same(stored, untagged))
check("an empty tag map the server left out matches",
      p.card_same(server_card(untagged), untagged))
retyped = server_card(payload)
retyped["dataset_query"]["native"]["template-tags"]["source"]["dimension"] = ["field", 42, None]
check("a tag on another field does not match", not p.card_same(retyped, payload))
joined = server_card(payload)
joined["dataset_query"]["native"]["template-tags"]["source"]["dimension"] = [
    "field", 41, {"base-type": "type/Text", "join-alias": "j"}]
check("a typed tag that carries more than its base type does not match",
      not p.card_same(joined, payload))
tile = {"card_id": 1, "row": 0, "col": 0, "size_x": 1, "size_y": 1,
        "series": [], "parameter_mappings": [], "inline_parameters": None,
        "visualization_settings": {"card.title": "t"}}
check("equal tiles match", p._dashcards_same([dict(tile, id=9)], [tile]))
server_tile = dict(tile, id=9, entity_id="server-only", series=None, inline_parameters=[])
check("a server-shaped tile matches", p._dashcards_same([server_tile], [tile]))
moved = dict(tile, row=1)
check("a moved tile does not match", not p._dashcards_same([tile], [moved]))
beside = dict(tile, col=6, card_id=2)
check("tiles listed in another order match",
      p._dashcards_same([dict(beside, id=10), dict(tile, id=9)], [tile, beside]))
retitled = dict(tile, visualization_settings={})
check("a tile that drops a setting does not match", not p._dashcards_same([tile], [retitled]))


class FetchCard:
    """A card list in MBQL 5, and the legacy form only when asked for."""

    def __init__(self, body):
        self.body = body
        self.puts = 0

    def __call__(self, base, path, method="GET", data=None, session=None, timeout=30):
        if method == "GET" and path == "/api/card/9?legacy-mbql=true":
            return 200, server_card(self.body)
        if method == "GET" and path == "/api/card/9":
            return 200, dict(self.body, id=9, dataset_query={"lib/type": "mbql/query", "stages": []})
        if method == "PUT":
            self.puts += 1
            return 200, {"id": 9}
        return 200, {}


real_req = p.req
listed = {"n": {"id": 9, "name": "n", "dataset_query": {"lib/type": "mbql/query"}}}
try:
    fetch = p.req = FetchCard(dict(payload))
    with quiet():
        saved = p.upsert_cards("b", "s", listed, {"n": payload}, "card")
    check("a stored card read in the legacy form is not sent",
          saved == ({"n": 9}, 0, 0, 1) and fetch.puts == 0, saved)
    fetch.body["description"] = "other"
    with quiet():
        saved = p.upsert_cards("b", "s", listed, {"n": payload}, "card")
    check("a changed card is sent", saved == ({"n": 9}, 0, 1, 0) and fetch.puts == 1, saved)
    fetch.body["description"] = payload["description"]
    for label, fail in (("an error status", lambda: (500, {"message": "down"})),
                        ("a body that is no JSON", lambda: (_ for _ in ()).throw(ValueError("html")))):
        fetch.puts = 0

        def failing(base, path, method="GET", data=None, session=None, timeout=30, fail=fail):
            if method == "GET" and "legacy-mbql" in path:
                return fail()
            return fetch(base, path, method, data, session, timeout)

        p.req = failing
        with quiet():
            saved = p.upsert_cards("b", "s", listed, {"n": payload}, "card")
        check(f"a card read that fails with {label} is sent",
              saved == ({"n": 9}, 0, 1, 0) and fetch.puts == 1, saved)
finally:
    p.req = real_req

section("an unchanged dashboard is not sent again")


def server_dashboard(data):
    """What a GET returns after a PUT: tile ids and keys of the
    server's own, and an empty list where the payload sent null."""
    if not isinstance(data, dict) or "dashcards" not in data:
        return data
    out = dict(data)
    out["dashcards"] = [dict(dc, id=1000 + i, entity_id="server-only",
                             inline_parameters=dc.get("inline_parameters") or [])
                        for i, dc in enumerate(data.get("dashcards") or [])]
    return out


class RememberDashboards:
    """Keeps what each PUT laid out and answers a GET with it. A
    dashboard id in `failing` answers its GET with a 500, and one in
    `refused` its PUT. `puts` counts the PUTs and `put_ids` holds the
    ids they went to."""

    def __init__(self):
        self.next_id = 100
        self.names = {}
        self.layouts = {}
        self.puts = 0
        self.put_ids = set()
        self.failing = set()
        self.refused = set()

    def __call__(self, base, path, method="GET", data=None, session=None, timeout=30):
        if method == "GET" and path.startswith("/api/collection/"):
            return 200, {"data": [{"name": n, "id": i} for n, i in self.names.items()]}
        if method == "GET" and path.startswith("/api/dashboard/"):
            i = int(path.rsplit("/", 1)[-1])
            if i in self.failing:
                return 500, {"message": "down"}
            return 200, server_dashboard(self.layouts.get(i, {}))
        if method == "POST" and path == "/api/dashboard":
            self.next_id += 1
            self.names[data["name"]] = self.next_id
            return 200, {"id": self.next_id}
        if method == "PUT" and path.startswith("/api/dashboard/"):
            self.puts += 1
            i = int(path.rsplit("/", 1)[-1])
            self.put_ids.add(i)
            if i in self.refused:
                return 500, {"message": "refused"}
            if data.get("dashcards") is not None:
                self.layouts[i] = dict(data)
            elif i in self.layouts and "parameters" in data:
                self.layouts[i]["parameters"] = data["parameters"]
            return 200, {}
        return 200, {}


real_req = p.req
n_dash = len(p.dashboard_defs())
try:
    remembered = p.req = RememberDashboards()
    buf = io.StringIO()
    with contextlib.redirect_stdout(buf):
        first = p.ensure_dashboards("b", "s", 7, card_ids, MID)
    check("first dashboard pass lays them out", first == 0 and remembered.puts > 0)
    check("...counting each dashboard it made as created, not updated",
          f"— {n_dash} created, 0 updated, 0 unchanged," in buf.getvalue(), buf.getvalue())
    remembered.puts = 0
    buf = io.StringIO()
    with contextlib.redirect_stdout(buf):
        second = p.ensure_dashboards("b", "s", 7, card_ids, MID)
    check("an unchanged dashboard is not sent", second == 0 and remembered.puts == 0)
    check("...and the second run counts every dashboard unchanged",
          f"— 0 created, 0 updated, {n_dash} unchanged," in buf.getvalue(), buf.getvalue())
    one = next(iter(remembered.layouts))
    remembered.layouts[one]["description"] = "edited"
    remembered.puts = 0
    with quiet():
        third = p.ensure_dashboards("b", "s", 7, card_ids, MID)
    check("a changed dashboard is sent", third == 0 and remembered.puts > 0)
    remembered.failing, remembered.put_ids = {one}, set()
    with quiet():
        fourth = p.ensure_dashboards("b", "s", 7, card_ids, MID)
    check("a dashboard whose read fails is sent, and only that one",
          fourth == 0 and remembered.put_ids == {one}, remembered.put_ids)
    # A new picker default, here the As of day's, changes Allocation's
    # pickers alone: they go in one PUT of the parameters, and the tiles
    # stay.
    remembered.failing, remembered.put_ids, remembered.puts = set(), set(), 0
    _alloc = remembered.names["Allocation"]
    _tiles = remembered.layouts[_alloc]["dashcards"]
    _asof_default = p.ASOF_DEFAULT
    try:
        p.ASOF_DEFAULT = "2026-10-25"
        with quiet():
            fifth = p.ensure_dashboards("b", "s", 7, card_ids, MID)
    finally:
        p.ASOF_DEFAULT = _asof_default
    check("a new As of day default is sent as the pickers alone, to the two Allocation dashboards",
          fifth == 0 and remembered.puts == 2
          and remembered.put_ids == {_alloc, remembered.names["Allocation (privacy)"]}
          and remembered.layouts[_alloc]["dashcards"] is _tiles, (remembered.puts, remembered.put_ids))
    # A refused pickers-only PUT stops the run.
    remembered.refused = {_alloc}
    _err = io.StringIO()
    try:
        p.ASOF_DEFAULT = "2026-10-24"
        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(_err):
            sixth = p.ensure_dashboards("b", "s", 7, card_ids, MID)
    finally:
        p.ASOF_DEFAULT = _asof_default
    check("...and a refused one fails the run, naming the dashboard",
          sixth == 1 and "failed to update the filters of 'Allocation' (500): refused" in _err.getvalue(),
          (sixth, _err.getvalue()))
finally:
    p.req = real_req

layouts = [d for _path, d in api.puts if d.get("dashcards")]
api_puts_layout = [(path, d) for path, d in api.puts if d.get("dashcards")]

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
          and api.queries == ["SELECT (SELECT count(*) FROM yield_withholding)"
                              " + (SELECT count(*) FROM yield_withholding_accounts) > 0"], _probed)
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
    # Called without descriptions, no tile carries a description of its
    # own; every other dashboard is laid out as before.
    _changed = {d.get("name") for k, d in enumerate(x for _path, x in api.puts if x.get("dashcards"))
                if d != layouts[k]}
    _noted = {d.get("name") for d in layouts
              if any("card.description" in dc["visualization_settings"] for dc in d["dashcards"])}
    check("...which the tiles show in place of the cards' names, and no other tile changes",
          _titled == {card_ids[c]: t for c, t in _titles.items()}
          and _changed == {"Income", "Income (privacy)"} | _noted, (_titled, _changed))
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
# above all, since a tile it misses silently keeps showing USD. The
# Overview's tiles borrowed from the other dashboards take the pickers
# their cards have (the Wealth Overview section below); the
# Top-positions tiles take the vehicle as well.
_TOP = {card_ids[c] for c in p.POSITION_FILTERED_CARDS}
_BORROWED = {card_ids[c] for t in ("Wealth Overview", "Wealth Overview" + p.PRIVACY_SUFFIX)
             for c, *_ in DEFS[t][3] if c.removesuffix(p.PRIVACY_SUFFIX) in p.OVERVIEW_TILE_HOMES
             and c.removesuffix(p.PRIVACY_SUFFIX) != "Asset class mix over time"}
_SELECTION = {p.WEALTH_TAX_WRAPPER_PARAM_ID, p.ASSET_PARAM_ID}
# The debt view carries no asset class, the issuers view neither a source
# nor a tax wrapper: those tiles take what their views can narrow by.
_DEBT = {card_ids[c + s] for c in ("Debt by credit rating, share of debt and cash", "Debt by maturity",
                                   "Debt by coupon kind", "Public equity by cycle")
         for s in ("", p.PRIVACY_SUFFIX)}
_ISSUERS = {card_ids["Top 100 issuers" + s] for s in ("", p.PRIVACY_SUFFIX)}
# The overview's three headline figures read the latest snapshot
# whatever the time range.
_LATEST = {card_ids[c + s] for c in ("Net worth", "Positions value", "Cash balance")
           for s in ("", p.PRIVACY_SUFFIX)}
for _d, _want in (("Wealth Overview", {p.WEALTH_CURRENCY_PARAM_ID,
                                       p.TIME_PARAM_ID, p.SOURCE_PARAM_ID} | _SELECTION),
                  ("Allocation", {p.WEALTH_CURRENCY_PARAM_ID,
                                  p.ASOF_PARAM_ID, p.SOURCE_PARAM_ID} | _SELECTION),
                  ("Exposure", {p.EXPOSURE_CURRENCY_PARAM_ID, p.TIME_PARAM_ID, p.SOURCE_PARAM_ID,
                                p.EXPOSURE_TAX_WRAPPER_PARAM_ID, p.EXPOSURE_ASSET_CLASS_PARAM_ID})):
    for _name in (_d, _d + p.PRIVACY_SUFFIX):
        _body = [d for d in layouts if d.get("name") == _name][0]
        _tiles = [dc for dc in _body["dashcards"] if dc["card_id"] is not None and dc["card_id"] not in _BORROWED]

        def _expected(cid, want=_want):
            if cid in _TOP:
                return want | {p.VEHICLE_PARAM_ID}
            if cid in _DEBT:
                return want - {p.EXPOSURE_ASSET_CLASS_PARAM_ID}
            if cid in _ISSUERS:
                return {p.WEALTH_CURRENCY_PARAM_ID, p.ASOF_PARAM_ID}
            if cid in _LATEST:
                return want - {p.TIME_PARAM_ID}
            return want
        _off = [dc["card_id"] for dc in _tiles
                if {m["parameter_id"] for m in dc["parameter_mappings"]} != _expected(dc["card_id"])]
        check(f"every '{_name}' tile answers the pickers its views can narrow by",
              bool(_tiles) and not _off, _off)
        check(f"on '{_name}', only the Top-positions tile carries a filter of its own, the vehicle",
              all(dc.get("inline_parameters") == ([p.VEHICLE_PARAM_ID] if dc["card_id"] in _TOP else [])
                  for dc in _tiles),
              [(dc["card_id"], dc.get("inline_parameters")) for dc in _tiles if dc.get("inline_parameters")])

# Each tile's SQL holds the clauses its pickers land on: a picker
# mapped onto a tag the SQL never references filters nothing.
_WA = {n for d in ("Wealth Overview", "Allocation") for s in ("", p.PRIVACY_SUFFIX)
       for n, *_ in DEFS[d + s][3] if card_ids[n] not in _BORROWED}
# The issuers card covers the whole book: its view carries no source,
# tax wrapper or asset class (above).
_wa_sql = {n: sql_of(CARDS[n][2]) or "" for n in _WA if not n.startswith("Top 100 issuers")}
check("every Wealth Overview and Allocation holdings tile narrows to the tax wrapper and the asset class",
      all("[[AND {{tax_wrapper}}]]" in q and "[[AND {{asset_class}}]]" in q for q in _wa_sql.values()),
      [n for n, q in _wa_sql.items() if "{{tax_wrapper}}" not in q or "{{asset_class}}" not in q])

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
                          "tax_wrapper", "position", "rates", "maturing", "shift"],
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
check("every Gains tile takes the six pickers, the asset class among them",
      all(dict(p.NATIVE_PARAM_TARGETS[n]).keys() ==
          {p.GAINS_CURRENCY_PARAM_ID, p.TIME_PARAM_ID, p.SOURCE_PARAM_ID,
           p.GAINS_TAX_WRAPPER_PARAM_ID, p.GAINS_ASSET_CLASS_PARAM_ID, p.GAINS_MISSING_PARAM_ID}
          for n in _gains_tiles)
      and all("[[AND {{asset_class}}]]" in _gn_sql[n] for n in _gains_tiles),
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
check("...and no card has a privacy variant but the two the Wealth Overview's twin shows",
      {n for n in _gains_tiles if p.privacy_name(n) in CARDS} == {"Realized gain", "Unrealized gain"})
_gn_pickers = [q["slug"] for q in p.dashboard_parameters(MID, "range", "Gains")]
check("the Gains dashboard carries six pickers",
      _gn_pickers == ["currency", "time_range", "source", "tax_wrapper", "asset_class", "missing_basis"],
      _gn_pickers)
# The Asset class picker lists the classes the gains rows carry, so it
# offers `unknown` where a snapshot holds a position nothing classes;
# the asset class history the other dashboards list may never hold it.
_gn_class = [q for q in p.dashboard_parameters(MID, "range", "Gains") if q["slug"] == "asset_class"][0]
check("...the Asset class picker's values those of the gains rows, `unknown` among them",
      _gn_class["values_source_config"]["card_id"] == MID["report_gains_classes"]
      and _gn_class["values_source_config"]["value_field"][1] == "asset_class"
      and MODELS["report_gains_classes"][0] == "SELECT DISTINCT asset_class FROM web_gains"
      and "COALESCE(g.asset_class, 'unknown') AS asset_class" in _MIG_TEXT[170]
      and all(q["values_source_config"]["card_id"] == MID["report_asset_classes_history"]
              for d in ("Wealth Overview", "Exposure", "Yield")
              for q in p.dashboard_parameters(MID, "range", d) if q["slug"] == "asset_class"),
      _gn_class["values_source_config"])
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

# A heading spans the grid's width on its row, so it counts as a tile.
# A twin carries its base's headings, and TWIN_TILES places some of its
# tiles anew.
for _d, (_desc, _mode, _sibling, _spec) in p.dashboard_defs().items():
    _base = _sibling if _d.endswith(p.PRIVACY_SUFFIX) else _d
    _spec = _spec + [(_text, _r, 0, 24, 1, None) for _r, _text, _disp in p.DASHBOARD_HEADINGS.get(_base, [])]
    check(f"'{_d}' sits on the 24-column grid without overlaps",
          all(c + w <= 24 for _n, _r, c, w, _h, _t in _spec) and not [
              (a[0], b[0]) for i, a in enumerate(_spec) for b in _spec[i + 1:]
              if a[2] < b[2] + b[3] and b[2] < a[2] + a[3] and a[1] < b[1] + b[4] and b[1] < a[1] + a[4]])

check("every TWIN_TILES card is a tile of its base",
      all(set(_t) <= {c for c, *_ in p.base_dashboards()[_d][2]} for _d, _t in p.TWIN_TILES.items()),
      {_d: sorted(set(_t) - {c for c, *_ in p.base_dashboards()[_d][2]}) for _d, _t in p.TWIN_TILES.items()})

section("the Wealth Overview")

_OV = "Wealth Overview"
_OVT = _OV + p.PRIVACY_SUFFIX
_ov_body = {n: [d for d in layouts if d.get("name") == n][0] for n in (_OV, _OVT)}
_ov_tiles = {n: {dc["card_id"]: dc for dc in b["dashcards"] if dc["card_id"] is not None}
             for n, b in _ov_body.items()}
_ov_params = {n: {x["id"]: x["slug"] for x in b["parameters"]} for n, b in _ov_body.items()}
_by_id = {v: k for k, v in card_ids.items()}

# The figures each picker reaches (the design's table): a borrowed card
# takes the overview's pickers its own tags or home dashboard name, and
# no other.
_ALL = {"currency", "time_range", "source", "tax_wrapper", "asset_class"}
_FLOW = {"currency", "time_range", "source"}
# The three headline figures read the latest snapshot whatever the time
# range.
_LATEST_REACH = _ALL - {"time_range"}
_REACH = {"Net worth": _LATEST_REACH, "Positions value": _LATEST_REACH, "Cash balance": _LATEST_REACH,
          "Net worth — monthly trend": _ALL,
          "Net worth over time": _ALL, "Asset class mix over time": _ALL, "Stalest source (days)": set(),
          "Income received": _FLOW | {"tax_wrapper"}, "Net spend": _FLOW, "Spend — monthly trend": _FLOW,
          "Net cash flow": _FLOW, "Savings rate": _FLOW,
          "Realized gain": _ALL, "Unrealized gain": _ALL,
          "Return year to date (TWR)": {"currency"}, "Return year to date (MWR)": {"currency"},
          "Income yield": {"currency", "source", "tax_wrapper", "asset_class"},
          "Income expected": {"currency", "source", "tax_wrapper"}, "Projected net cash flow": {"currency"}}
_reach = {_by_id[cid]: {_ov_params[_OV][m["parameter_id"]] for m in dc["parameter_mappings"]}
          for cid, dc in _ov_tiles[_OV].items()}
check("each Wealth Overview figure answers the pickers its card has, and no other",
      _reach == _REACH, {c: (_reach.get(c), w) for c, w in _REACH.items() if _reach.get(c) != w})
# On the twin a tile is the privacy card its home twin shows, whose tags
# can differ (the expected income's share follows the Time range there).
_twin_off = []
for _cid, _dc in _ov_tiles[_OVT].items():
    _c = _by_id[_cid]
    _tags = set((CARDS[_c][2].get("native") or {}).get("template-tags") or {})
    _got = {_ov_params[_OVT][m["parameter_id"]] for m in _dc["parameter_mappings"]}
    if sql_of(CARDS[_c][2]) is not None and _got != _tags & _ALL:
        _twin_off.append((_c, sorted(_got), sorted(_tags & _ALL)))
check("...and each twin figure the pickers its privacy card has", not _twin_off, _twin_off)
# The twin leaves out the two figures that read 100 by construction, and
# the four left in their rows sit two to a row (a row below the base's
# grid: the switch link takes the first).
_ovt_place = {_by_id[dc["card_id"]]: (dc["row"], dc["col"], dc["size_x"])
              for dc in _ov_body[_OVT]["dashcards"] if dc["card_id"] is not None}
check("the twin carries no received income and no net spend, whose shares read 100",
      not {"Income received (privacy)", "Net spend (privacy)"} & set(_ovt_place)
      and {"Income received", "Net spend"} <= set(_reach), sorted(_ovt_place))
check("...and its other four figures of the time range sit two to a row",
      {c: _ovt_place[c] for c in ("Spend — monthly trend (privacy)", "Net cash flow (privacy)",
                                  "Savings rate (privacy)", "Realized gain (privacy)")}
      == {"Spend — monthly trend (privacy)": (21, 0, 12), "Net cash flow (privacy)": (21, 12, 12),
          "Savings rate (privacy)": (24, 0, 12), "Realized gain (privacy)": (24, 12, 12)}, _ovt_place)
_params_all = {d.get("name"): d["parameters"] for d in layouts}
_mbql = p.overview_mappings("Net spend (privacy)", _OVT, DEFS, _params_all, card_ids)
check("an MBQL card takes the pickers its home dashboard maps, the twin's net spend among them",
      {_ov_params[_OVT][m["parameter_id"]] for m in _mbql} == _FLOW, _mbql)
check("...and both show the asset class mix, the same card, under the title of its kind",
      card_ids["Asset class mix over time"] in _ov_tiles[_OV] and card_ids["Asset class mix over time"] in _ov_tiles[_OVT]
      and all(_ov_tiles[n][card_ids["Asset class mix over time"]]["visualization_settings"].get("card.title")
              == "Asset classes over time" for n in (_OV, _OVT)))
check("...every mapping on both naming a picker the dashboard carries, its card's own id",
      all(m["parameter_id"] in _ov_params[n] and m["card_id"] == cid
          for n in (_OV, _OVT) for cid, dc in _ov_tiles[n].items() for m in dc["parameter_mappings"]))

# A tile's description names what does not reach it; one every picker
# reaches keeps its card's own.
_ov_desc = {_by_id[cid]: dc["visualization_settings"].get("card.description")
            for cid, dc in _ov_tiles[_OV].items()}
check("a figure no picker reaches says so",
      _ov_desc["Stalest source (days)"].endswith(" No picker reaches it."),
      _ov_desc["Stalest source (days)"])
check("...one the time range misses names that picker",
      _ov_desc["Net worth"] == CARDS["Net worth"][1]
      + " The Time range picker does not reach it."
      and "whatever the Time range" in CARDS["Net worth"][1], _ov_desc["Net worth"])
check("...one several miss names them all",
      _ov_desc["Projected net cash flow"].endswith(
          " The Time range, Source, Tax wrapper and Asset class pickers do not reach it."),
      _ov_desc["Projected net cash flow"])
check("...and one every picker reaches keeps its card's description",
      all(_ov_desc[c] is None for c, w in _REACH.items() if w == _ALL))

# A borrowed or family figure opens its dashboard with the pickers that
# dashboard shares; the twin's open the twins, and none opens money.
_dash_ids = {d.get("name"): int(path.rsplit("/", 1)[1]) for path, d in api_puts_layout}
_params_by_id = {d.get("name"): {x["id"]: x["slug"] for x in d["parameters"]} for d in layouts}
_click_off = []
for _n in (_OV, _OVT):
    for _cid, _dc in _ov_tiles[_n].items():
        _c = _by_id[_cid]
        _home = p.overview_home(_c, _n)
        _cb = _dc["visualization_settings"].get("click_behavior")
        if _home is None:
            if _cb is not None:
                _click_off.append((_n, _c, "opens something"))
            continue
        _target = _params_by_id.get(_home, {})
        _want = {t: s for t, s in _target.items() if s in set(_ov_params[_n].values())}
        if (not _cb or _cb["type"] != "link" or _cb["linkType"] != "dashboard"
                or _cb["targetId"] != _dash_ids[_home]
                or any(m["source"]["type"] == "parameter" and _target[t] != _ov_params[_n][m["source"]["id"]]
                       for t, m in _cb["parameterMapping"].items())
                or {t for t, m in _cb["parameterMapping"].items() if m["source"]["type"] == "parameter"} != set(_want)):
            _click_off.append((_n, _c, _home, _cb))
check("each figure from another dashboard opens it, passing the pickers both carry",
      not _click_off, _click_off)
check("...on the twin, a figure opens its home's twin, and the gains, whose dashboard has none, nothing",
      p.overview_home("Net cash flow (privacy)", _OVT) == "Cash Flow (privacy)"
      and p.overview_home("Stalest source (days)", _OVT) == "Data Freshness (privacy)"
      and p.overview_home("Realized gain (privacy)", _OVT) is None
      and p.overview_home("Net worth (privacy)", _OVT) is None)
_ytd = _ov_tiles[_OV][card_ids["Return year to date (TWR)"]]["visualization_settings"]["click_behavior"]
check("...a year-to-date return passing its year to the Returns Start year",
      _ytd["parameterMapping"].get(p.START_YEAR_PARAM_ID, {}).get("source")
      == {"type": "column", "id": "start_year", "name": "start_year"}, _ytd)
_mix = {n: _ov_tiles[n][card_ids["Asset class mix over time"]]["visualization_settings"]["click_behavior"]
        for n in (_OV, _OVT)}
check("...the asset class mix passing the day clicked to Allocation's As of day, on the overview and its twin",
      all(m["parameterMapping"].get(p.ASOF_PARAM_ID, {}).get("source")
          == {"type": "column", "id": "as_of_day", "name": "as_of_day"} for m in _mix.values()), _mix)
check("...every borrowed figure's home holds its card, and the ones built for the overview sit nowhere else",
      all((c in {t[0] for t in DEFS[h][3]}) != (c in p.OVERVIEW_BUILT_CARDS)
          for c, h in p.OVERVIEW_TILE_HOMES.items())
      and p.OVERVIEW_BUILT_CARDS <= set(p.OVERVIEW_TILE_HOMES),
      [c for c, h in p.OVERVIEW_TILE_HOMES.items()
       if (c in {t[0] for t in DEFS[h][3]}) == (c in p.OVERVIEW_BUILT_CARDS)])

# The two headings sit below the switch link's shift, full width.
_heads = {n: sorted((dc["row"], dc["visualization_settings"]["text"]) for dc in b["dashcards"]
                    if dc["card_id"] is None and dc["visualization_settings"]["virtual_card"]["display"] == "heading")
          for n, b in _ov_body.items()}
check("the overview and its twin carry the two row headings",
      _heads[_OV] == _heads[_OVT] == [(r + 1, t) for r, t, _d in p.DASHBOARD_HEADINGS[_OV]], _heads)
_exp = [dc["visualization_settings"].get("card.title") for n in (_OV, _OVT) for cid, dc in _ov_tiles[n].items()
        if _by_id[cid].startswith("Income expected")]
check("the expected income tile names its twelve months, on the twin in the privacy form",
      _exp == ["Income expected, 12 months", "Income expected, 12 months (privacy)"], _exp)
check("...and Income keeps the card's own name",
      not any(dc["visualization_settings"].get("card.title")
              for d in layouts if d.get("name") == "Income" for dc in d["dashcards"]))

# The retired tiles' cards are archived, their variants too.
_RETIRED = {c + s for c in ("Cash vs positions over time", "Investment income by month", "Fees & taxes by month",
                            "Asset class split")
            for s in ("", p.PRIVACY_SUFFIX)}
check("the overview's retired cards are archived",
      _RETIRED <= set(p.RETIRED_CARD_NAMES), sorted(_RETIRED - set(p.RETIRED_CARD_NAMES)))

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
_STACKED_BARS = {c + s for c in ("Cash flow statement by month", "Investing by month",
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
# The tiles over the look-through itself; the debt and securities tiles
# read views of their own (checked below).
_ex_look = {n: q for n, q in _ex_sql.items() if re.search(r"FROM web_exposure\b", q)}
_EX_VIEW = re.compile(r"FROM web_exposure(?:_debt|_issuers|_securities)?\b")
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
    shown = p.TWIN_CARDS.get(dash.removesuffix(p.PRIVACY_SUFFIX), {})
    for n in names:
        card = p.privacy_name(shown.get(n, n)) if twin and n not in p.PRIVACY_EXEMPT_CARDS else n
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


# A click passes a column of its card's rows on; Metabase states a card's
# columns only once it has run, so the column is checked against its SQL.
_col_off = {c: col for c, col in p.COLUMN_CLICKS.items() if col not in _output_columns(sql_of(CARDS[c][2]) or "")}
check("each column a click passes is one its card's SQL returns", not _col_off, _col_off)


# A privacy card's columns are shares and rates, `_pct`, or labels and
# years that name no account and carry no amount.
_LABELS = {"as_of_day", "holding_currency", "currency", "kind", "symbol", "name", "vehicle", "tag",
           "rating", "grade", "maturity", "coupon", "cycle", "rank", "issuer", "curve", "tenor",
           "month", "series", "date", "type", "basis", "duration"}

section("the Exposure dashboard's debt cards")

_ex_debt = {"Debt by credit rating, share of debt and cash": "credit", "Debt by maturity": "maturity",
            "Debt by coupon kind": "coupon", "Public equity by cycle": "cycle"}
_ex_debt_tiles = list(_ex_debt)
check("the four are on the Exposure dashboard and its twin, above the coverage chart",
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
_gold_go = _go_package("gold")
# The dimensions are the ones gold's materializer writes (its
# debtDimension constants): one respelt there would leave its card with
# no rows.
_debt_dims = set(re.findall(r'[Dd]ebtDimension\w+\s*=\s*"(\w+)"', _gold_go))
check("the debt cards' dimensions are the ones gold materializes",
      set(_ex_debt.values()) == _debt_dims, sorted(_debt_dims))
_holding_prefix = re.search(r'const HoldingKeyPrefix = "([^"]+)"', _gold_go)

section("the Exposure dashboard's securities card")

# The largest securities sit full width above the coverage chart, on the
# base and the twin. They read the slices the pickers narrow, at the
# latest instant in the window, in the picked currency; the twin shows
# shares alone and hides a key and a name that are a holding's own.
_ex_by_name = {t[0]: t for t in _ex_spec}
_sec = "Top securities"
check("the securities card sits full width just above the coverage chart",
      _ex_by_name[_sec][3] == 24 and _ex_by_name[_sec][1] + _ex_by_name[_sec][4]
      == _ex_by_name["How the figures are classified"][1] and p.privacy_name(_sec) in _ex_twin)
for _name in (_sec, p.privacy_name(_sec)):
    _q = sql_of(CARDS[_name][2])
    check(f"'{_name}' sums each security's slices of the selection at the window's end",
          "FROM web_exposure_securities" in _q and "AND currency = {{currency}}" in _q
          and "e AS (SELECT * FROM w WHERE as_of_day = (SELECT max(as_of_day) FROM w))" in _q
          and "GROUP BY security" in _q and "len(list_distinct(flatten(list(funds)))) AS funds" in _q
          and _q.rstrip().endswith("LIMIT 100")
          and set(CARDS[_name][2]["native"]["template-tags"])
          == {"currency", "time_range", "source", "tax_wrapper", "asset_class"}, _q)
_sec_cols = {n: _output_columns(sql_of(CARDS[n][2])) for n in (_sec, p.privacy_name(_sec))}
check("the base card gives the view's columns, the market cap and the size among them",
      _sec_cols[_sec] == ["security", "name", "ticker", "asset_class", "sector", "country", "market_cap", "size",
                          "value", "direct_value", "via_funds_value", "funds", "share_pct"], _sec_cols[_sec])
check("...and its twin shares in place of the values",
      _sec_cols[p.privacy_name(_sec)] == ["security", "name", "ticker", "asset_class", "sector", "country",
                                          "market_cap", "size", "direct_pct", "via_funds_pct", "funds", "share_pct"]
      and f"LIKE '%{p.HOLDING_KEY_PREFIX}%'" in sql_of(CARDS[p.privacy_name(_sec)][2]), _sec_cols)


def _aggregates(body):
    """The items of a SELECT list, each without its alias, in lower case
    with its whitespace collapsed."""
    depth, item, items = 0, "", []
    for ch in body + ",":
        depth += {"(": 1, ")": -1}.get(ch, 0)
        if ch == "," and depth == 0:
            items.append(re.sub(r"\s+as\s+\w+$", "", " ".join(item.lower().split())))
            item = ""
        else:
            item += ch
    return items


# A Go test sums each security's slices as the card does, to prove they
# add up to report_exposure_securities. Its copy reads the table and
# names the sums its own way, so the two must agree on the aggregates and
# the grouping alone: a change to the card's left out of the Go copy
# would leave that test proving a sum the card no longer takes.
_go_sec = re.search(r"const exposureSecuritiesCardSQL = `(.*?)`", open(os.path.join(
    _REPO, "wealthdb", "internal", "gold", "exposure_materialize_test.go"), encoding="utf-8").read(), re.S)
_go_g = _go_sec and re.search(r"WITH g AS \(\s*SELECT (.*?)\s+FROM report_exposure_security_slices\b"
                              r"[^)]*?GROUP BY security\)", _go_sec.group(1), re.S)
_card_g = re.search(r"\ng AS \(\n  SELECT (.*?)\n    FROM e\n   GROUP BY security\)", sql_of(CARDS[_sec][2]), re.S)
check("the Go test sums a security's slices with the card's aggregates",
      _go_g and _card_g and _aggregates(_go_g.group(1)) == _aggregates(_card_g.group(1)),
      "the shapes moved; this check went vacuous" if not (_go_g and _card_g)
      else (_aggregates(_go_g.group(1)), _aggregates(_card_g.group(1))))

section("the Allocation dashboard's day")

# Every Allocation card, on the base and the twin, reads the last day of
# the history on or before the As of day: the picker binds the calendar
# view (gold migration 0166), whose history day the card reads.
_al_cards = [n for n, *_ in DEFS["Allocation"][3]] + [n for n, *_ in DEFS["Allocation" + p.PRIVACY_SUFFIX][3]]
_history_day = ("(SELECT max(history_day) FROM web_history_calendar WHERE TRUE\n"
                "     [[AND {{as_of_day}}]])")
for _name in _al_cards:
    _q = sql_of(CARDS[_name][2]) or ""
    _tag = CARDS[_name][2]["native"]["template-tags"]["as_of_day"]
    _reads = "AND as_of_day = " + _history_day if "issuers" not in _name else "SELECT " + _history_day + " AS day"
    check(f"'{_name}' reads the last history day on or before the As of day",
          _reads in _q and _q.count("{{as_of_day}}") == _q.count("FROM web_history_calendar")
          and _tag["dimension"][1] == p.FIELD_IDS[("web_history_calendar", "as_of_day")], _q)

section("the Allocation dashboard's issuers card")

# It sits below the top positions, full width, on the base and the twin,
# and no longer on Exposure. It reads the latest instant on or before the
# as-of day, which reaches it through the daily history.
_al_spec = {t[0]: t for t in DEFS["Allocation"][3]}
_al_twin = {t[0] for t in DEFS["Allocation" + p.PRIVACY_SUFFIX][3]}
check("the issuers card sits below the top positions on Allocation and its twin, full width",
      "Top 100 issuers" in _al_spec and p.privacy_name("Top 100 issuers") in _al_twin
      and _al_spec["Top 100 issuers"][1] == _al_spec["Top 100 positions"][1] + _al_spec["Top 100 positions"][4]
      and _al_spec["Top 100 issuers"][3] == 24)
check("...and is gone from Exposure and its twin",
      "Top 100 issuers" not in _ex_tiles and p.privacy_name("Top 100 issuers") not in _ex_twin)
_iss_sql = {n: sql_of(CARDS[n][2]) or "" for n in ("Top 100 issuers", p.privacy_name("Top 100 issuers"))}
for _name, _q in _iss_sql.items():
    check(f"'{_name}' lists the top hundred issuers at the latest instant on or before the as-of day",
          "FROM web_exposure_issuers" in _q and "currency = {{currency}}" in _q
          and "FROM web_history_calendar" in _q and "[[AND {{as_of_day}}]]" in _q
          and "as_of_day <= (SELECT day FROM d)" in _q
          and "WHERE issuer <> '(none)'" in _q and _q.rstrip().endswith("LIMIT 100")
          and "lei" not in _output_columns(_q)
          and set(CARDS[_name][2]["native"]["template-tags"]) == {"currency", "as_of_day"}, _q[-300:])
check("the issuers twin hides an issuer named by a holding's own key, gold's, and shows no value",
      _holding_prefix is not None and p.HOLDING_KEY_PREFIX == _holding_prefix.group(1)
      and f"LIKE '{p.HOLDING_KEY_PREFIX}%'" in _iss_sql[p.privacy_name("Top 100 issuers")]
      and "value" not in _output_columns(_iss_sql[p.privacy_name("Top 100 issuers")]),
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
                   f" >= {p.SMALLEST_BAR_PCT}, TRUE)") in _q)
# Each description starts by saying what its card counts, which
# Metabase shows behind the info icon. The maturity card's title names
# the debt alone, so its description names the cash it counts too.
check("each debt card's description says what it counts",
      all(CARDS[n][1].startswith("Counts ") for n in _ex_debt)
      and all("debt and cash" in CARDS[n][1] for n in (_credit, "Debt by maturity")))
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
      and VIZ["DV01 along the curve (privacy)"]["graph.metrics"] == ["dv01_pct"])
# A group of bars per curve currency, a bar per tenor: every currency
# carries every tenor, rows tenor by tenor, the most rate risk first.
check("the DV01 cards group by curve currency, a bar per tenor, in curve order and by risk",
      all(VIZ[n]["graph.dimensions"] == ["curve", "tenor"] and "CROSS JOIN t" in _y_sql[n]
          and "ORDER BY t.tenor_order, k.risk DESC" in _y_sql[n] and "held_in" not in _y_sql[n]
          and not VIZ[n].get("stackable.stack_type")
          for n in ("DV01 along the curve", "DV01 along the curve (privacy)")))
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
_IN_ORDER = [[("Income in the window", 4), ("Income received", 4), ("Income expected", 4),
              ("Net income", 4), ("Monthly average income", 4), ("Uncategorized income share", 4)],
             [("Income — monthly trend", 8), ("Withholding", 8), ("Income after withholding", 8)],
             [("Received and expected by month", 24)],
             [("Holdings' interest received", 6), ("Holdings' dividends received", 6), ("Holdings' interest expected", 6),
              ("Holdings' dividends expected", 6)],
             [("Income by type", 12), ("Top 50 payers", 12)],
             [("Received and expected by account", 24)],
             [("Received and expected by position", 24)],
             [("Expected payments", 24)],
             [("Largest receipts", 24)]]
_in_at = {t[0]: t for t in _in_spec}
# A row of text sits between two rows of tiles: the chart's subtitle.
_in_text = {r for r, _t, _d in p.DASHBOARD_HEADINGS["Income"]}


def _next_row(name):
    end = _in_at[name][1] + _in_at[name][4]
    return end + (end in _in_text)


check("the Income tiles read small tiles first, then the charts, then the tables",
      [(t[0], t[3]) for t in _in_spec] == [x for row in _IN_ORDER for x in row]
      and all(len({_in_at[n][1] for n, _w in row}) == 1 for row in _IN_ORDER)
      and all(_next_row(_IN_ORDER[k - 1][0][0]) == _in_at[_IN_ORDER[k][0][0]][1]
              for k in range(1, len(_IN_ORDER))),
      [(t[0], t[1], t[3]) for t in _in_spec])
check("...the chart by month under a subtitle naming its run-rate bands",
      p.DASHBOARD_HEADINGS["Income"] == [(_in_at["Received and expected by month"][1] - 1,
                                          p.DASHBOARD_HEADINGS["Income"][0][1], "text")]
      and "run-rate" in p.DASHBOARD_HEADINGS["Income"][0][1], p.DASHBOARD_HEADINGS["Income"])


def _twin(n):
    """The card the Income twin shows in place of base card `n`."""
    return p.privacy_name(p.TWIN_CARDS["Income"].get(n, n))


check("...and the twin mirrors the order",
      [(t[0], t[1], t[2], t[3], t[4]) for t in DEFS["Income (privacy)"][3]]
      == [(n if n in p.PRIVACY_EXEMPT_CARDS else _twin(n), r, c, w, h) for n, r, c, w, h, _t in _in_spec])
_IN_PROJECTED = [t[0] for t in _in_spec if t[0] not in INCOME_CARD_NAMES]
_YIELD_TWIN = _twin("Income in the window")
check("...each card of received and expected income with a privacy twin, the income in the window's "
      "the holdings' yield",
      len(_IN_PROJECTED) == 14 and all(_twin(n) in CARDS for n in _IN_PROJECTED)
      and _YIELD_TWIN == "Holdings' income yield (privacy)"
      and "Income in the window (privacy)" not in CARDS, _IN_PROJECTED)
# The income lines' cards by month and by account would repeat the
# received and expected ones, so none is defined, and their names are
# retired.
_SUPERSEDED = ["Income by month", "Income by account"]
check("the income lines' cards by month and by account are not defined, and their names are retired",
      all(n + s not in CARDS and n + s in p.RETIRED_CARD_NAMES
          for n in _SUPERSEDED for s in ("", p.PRIVACY_SUFFIX))
      and not [t for d in DEFS.values() for t in d[3] if t[0].removesuffix(p.PRIVACY_SUFFIX) in _SUPERSEDED])
_in_all = [x for n in _IN_PROJECTED for x in (n, _twin(n))]
_in_sql = {n: sql_of(CARDS[n][2]) or "" for n in _in_all}
# What was received depends on no assumption, so those tiles read the
# income received alone and take no projection picker; what is expected
# reads the parts alone, but on the twin, whose share is of both.
_RECEIVED_ONLY = {n + s for n in ("Income received", "Holdings' interest received", "Holdings' dividends received")
                  for s in ("", p.PRIVACY_SUFFIX)}
_EXPECTED_ONLY = {"Income expected", "Holdings' interest expected", "Holdings' dividends expected",
                  "Expected payments", "Expected payments (privacy)"}
# The withholding and the income after it set the tax booked on any line
# against every line received, not the holdings' income received.
_EVERY_LINE = {n + s for n in ("Withholding", "Income after withholding") for s in ("", p.PRIVACY_SUFFIX)}
# The cards of the holdings alone: the calendar of their payments and
# the twin's yield on their value.
_HOLDINGS_ONLY = {"Expected payments", "Expected payments (privacy)", _YIELD_TWIN}
# The income received holds a row set per currency, a part its rate to
# each: the one is held to the picked currency, the other picks its rate.
check("every card reads the income received where it shows a received figure, every other line and "
      "the run-rate beside the holdings', and today's parts where it shows an expected one, each in "
      "the picked currency",
      all(("FROM web_yield_received\n   WHERE currency = {{currency}}" in q) == (n not in _EXPECTED_ONLY | _EVERY_LINE)
          and ("FROM web_yield_received" in q) == (n not in _EXPECTED_ONLY | _EVERY_LINE)
          and ("FROM web_income\n   WHERE income_detailed NOT IN" in q)
          == (n not in _EXPECTED_ONLY | _EVERY_LINE and n != _YIELD_TWIN)
          and ("FROM web_income\n   WHERE TRUE" in q) == (n in _EVERY_LINE)
          and ("FROM web_cashflow_plan c" in q) == (n not in _RECEIVED_ONLY | _HOLDINGS_ONLY)
          and ("FROM web_yield_parts" in q) == (n not in _RECEIVED_ONLY)
          and ("p.rule = {{maturing}}" in q) == (n not in _RECEIVED_ONLY)
          and ("CASE {{currency}} WHEN 'USD' THEN p.fx_usd" in q) == (n not in _RECEIVED_ONLY)
          for n, q in _in_sql.items()),
      [n for n, q in _in_sql.items() if ("FROM web_yield_parts" in q) == (n in _RECEIVED_ONLY)
       or ("FROM web_yield_received" in q) == (n in _EXPECTED_ONLY | _EVERY_LINE)])
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
    _tags = set(CARDS[_n][2]["native"]["template-tags"]) - {"currency", "source", "account", "position",
                                                             "tax_wrapper", "type"}
    if _n in _RECEIVED_ONLY:
        _want = {"time_range"}
    elif _n in _EXPECTED_ONLY:
        _want = {"horizon", "rates", "maturing", "shift"}
    else:
        _want = {"time_range", "horizon", "rates", "maturing", "shift"}
    check(f"'{_n}' takes the Time range where it shows a received figure, the Horizon and the "
          "assumptions where it shows an expected one",
          _tags == _want, sorted(_tags))
# A twin ends by saying its figures are shares.
check("an Income twin's description ends in the privacy note",
      all(CARDS[n][1].endswith(p.PRIVACY_DESC) for n in _in_all if n.endswith(p.PRIVACY_SUFFIX)),
      [n for n in _in_all if n.endswith(p.PRIVACY_SUFFIX) and not CARDS[n][1].endswith(p.PRIVACY_DESC)])
check("...where no shift is the Shift variable's default",
      p.PROJECTION_TAGS["shift"]["default"] == "0", p.PROJECTION_TAGS["shift"])
check("only the calendar takes the Position picker, and it is optional",
      [n for n in _in_all if "position" in CARDS[n][2]["native"]["template-tags"]]
      == ["Expected payments", "Expected payments (privacy)"]
      and p.POSITION_TAG["required"] is False)
_in_params = [q["slug"] for q in p.dashboard_parameters(MID, "range", "Income (privacy)")]
check("the Income twin carries the same pickers, but the account",
      _in_params == ["currency", "time_range", "horizon", "source", "type", "tax_wrapper", "position",
                     "rates", "maturing", "shift"], _in_params)
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
check("...set against every income line received and the income expected, of any type",
      all("FROM web_income\n   WHERE TRUE\n     [[AND CAST(occurred_at AS DATE) IN"
          " (SELECT day FROM web_income_days WHERE {{time_range}})]]" in _in_sql[n]
          and "FROM web_yield_received" not in _in_sql[n] and "{{type}}" not in _in_sql[n]
          and "type" not in CARDS[n][2]["native"]["template-tags"]
          and CARDS[n][1].count("A tax line has no Type.") == 1 for n in _EVERY_LINE),
      {n: _in_sql[n] for n in _EVERY_LINE})
_in_priv = [n for n in _in_all if n.endswith(p.PRIVACY_SUFFIX)]
_in_money = {n: [c for c in _output_columns(_in_sql[n]) if not c.endswith("_pct") and c not in _LABELS]
             for n in _in_priv}
check("no projected privacy card carries an amount: shares, yields and labels alone",
      not any(_in_money.values()), {n: c for n, c in _in_money.items() if c})
check("the twin's income is a yield on the value it is earned on",
      "FROM web_yield\n" in _in_sql[_YIELD_TWIN]
      and "* 12 / (SELECT received + expected FROM mo)" in _in_sql[_YIELD_TWIN])
_month_viz = VIZ["Received and expected by month"]
check("the by-month chart stacks a series per type and status, and per class of the other lines, "
      "the expected one lighter",
      _month_viz["graph.dimensions"] == ["month", "series"]
      and set(_month_viz["series_settings"]) == {f"{t} ({s})" for t in p.INCOME_TYPE_COLORS
                                                for s in ("received", "expected")}
      | {f"{p.INCOME_CLASS_BANDS[c]} ({s})" for c in p.INCOME_CLASS_COLORS
         for s in ("received", "expected, run-rate")}
      and set(p.INCOME_CLASS_COLORS) == set(p.INCOME_CLASS_BANDS)
      and all(_month_viz["series_settings"][f"{p.INCOME_CLASS_BANDS[c]} (expected, run-rate)"]["color"]
              == p._tint(v) for c, v in p.INCOME_CLASS_COLORS.items())
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
check("...and no other tile carries settings of its own but the pickers that miss it",
      all(set(dc["visualization_settings"]) <= {"card.description"} for dc in _in_body["dashcards"]
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
      all("click" in CARDS[n][1].lower()
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

section("the Income pickers reach every card")

# Every Income card, the income lines' and the holdings', takes the Tax
# wrapper; every one but the withholding and the income after it the
# Type, on the base and the twin.
_in_cards = sorted({c for c, *_r in DEFS["Income"][3]})
_in_off = {}
for _n in [x for n in _in_cards for x in (n, n if n in p.PRIVACY_EXEMPT_CARDS else _twin(n))]:
    _tg = set(CARDS[_n][2]["native"]["template-tags"])
    _reg = {pid for pid, _t in p.NATIVE_PARAM_TARGETS[_n]}
    _typed = _n not in _EVERY_LINE
    if not ("tax_wrapper" in _tg and p.INCOME_TAX_WRAPPER_PARAM_ID in _reg
            and ("type" in _tg) == _typed and (p.INCOME_TYPE_PARAM_ID in _reg) == _typed):
        _in_off[_n] = sorted(_tg)
check("every Income card carries the Tax wrapper tag, and every one but the withholding's two the Type",
      len(_in_cards) == 20 and not _in_off, _in_off)
_in_tw = {n: CARDS[n][2]["native"]["template-tags"]["tax_wrapper"]["dimension"][1] for n in _in_all}
check("...the holdings' cards binding it, as the Source, to the view of every account's",
      set(_in_tw.values()) == {p.FIELD_IDS[("web_sources_latest", "tax_wrapper")]}, _in_tw)
check("...and the tax booked reaching it through its account",
      all("(silver_source_id, account_external_id) IN (SELECT silver_source_id, account_external_id FROM accounts"
          " WHERE COALESCE(tax_wrapper, 'taxable_personal') IN (SELECT tax_wrapper FROM web_sources_latest"
          " WHERE {{tax_wrapper}}))" in _in_sql[n]
          for n in _EVERY_LINE))
# The Type picker lists the lines' labels; a holdings' card keeps a row
# whose type a picked label's detailed type maps to, the map the engine
# types the lines by.
_type_sql = re.search(r"var incomeTypeSQL = fmt\.Sprintf\(`(.*?)`,(.*?)\)\n", _gold_go, re.S)
_args = [a.strip().removeprefix("yield.Type").removeprefix("canonical.") for a in _type_sql.group(2).split(",")]
_go_map = {}
for _d, _i in re.findall(r"income_detailed = '(\w+)'.*?THEN '%\[(\d)\]s'", _type_sql.group(1), re.S):
    _go_map.setdefault(_d, set()).add(_args[int(_i) - 1].lower())
_py_map = {}
for _t, _ds in p.INCOME_DETAILED_TYPES.items():
    for _d in _ds:
        _py_map.setdefault(_d, set()).add(_t)
check("the label-to-type map is the engine's incomeTypeSQL",
      _py_map == _go_map, (_py_map, _go_map))
check("...each of the six types reached by a label, and only those the cash flow projection projects",
      set(p.INCOME_DETAILED_TYPES) == set(p.INCOME_TYPE_COLORS)
      and all(p.INCOME_DETAILED_TYPES.values())
      and set(_py_map) == set(re.findall(r'"(INCOME_\w+)"', re.search(
          r"var projectedIncomeDetailed = \[\]string\{(.*?)\}", open(os.path.join(
              _REPO, "wealthdb", "internal", "gold", "cashflow_plan.go"), encoding="utf-8").read(), re.S).group(1))),
      _py_map)
check("...and a label of any other type mapping to none",
      "INCOME_SALARY" not in _py_map and "INCOME_OTHER" not in _py_map)

# The classes the other lines band by are the cash flow statement's
# operating-in classes (cashflow_txn_nodes, as its newest migration
# defines it), as gold keeps the plan's other income by them: a class per
# detailed type its CASE names, the projected types left to the
# holdings' cards.
_in_case = re.search(r"WHEN 'in' THEN\s*CASE(.*?)\bEND\b",
                     _MIG_TEXT[max(_defined_in("cashflow_txn_nodes"))], re.S).group(1)
_sql_classes = {}
for _ds, _c in re.findall(r"WHEN p\.verdict IN \(([^)]*)\)\s*(?:--[^\n]*\n\s*)*THEN '(\w+)'", _in_case):
    for _d in re.findall(r"'(\w+)'", _ds):
        _sql_classes[_d] = _c
_py_classes = {d: c for c, ds in p.INCOME_LINE_CLASSES.items() for d in ds}
check("the other lines' classes are the statement's operating-in classes, the projected types aside",
      _py_classes == {d: c for d, c in _sql_classes.items() if d not in p.PROJECTED_DETAILED}
      and set(p.PROJECTED_DETAILED) <= set(_sql_classes)
      and f"ELSE '{p.OTHER_RECEIPTS}'" in _in_case and f"THEN '{p.UNCATEGORIZED_CLASS}'" in _in_case
      and set(p.INCOME_CLASS_BANDS) == {*p.INCOME_LINE_CLASSES, p.OTHER_RECEIPTS, p.UNCATEGORIZED_CLASS},
      (_py_classes, _sql_classes))

section("every tile names the pickers that miss it")

# A tile's description ends in the sentence naming the filter bar's
# pickers its mappings leave out, on every dashboard, and a tile every
# one reaches keeps its card's own.
_bar_names = {d.get("name"): {x["id"]: x["name"] for x in d["parameters"]
                              if x["id"] != p.VEHICLE_PARAM_ID} for d in layouts}
_note_off = []
for _d in layouts:
    for _dc in _d["dashcards"]:
        if _dc["card_id"] is None:
            continue
        _c = _by_id[_dc["card_id"]]
        _want = p.unreached_note({m["parameter_id"] for m in _dc["parameter_mappings"]},
                                 [x for x in _d["parameters"] if x["id"] in _bar_names[_d["name"]]])
        _got = _dc["visualization_settings"].get("card.description")
        if _got != (CARDS[_c][1] + _want if _want else None):
            _note_off.append((_d["name"], _c, _got))
check("every tile that misses a picker of its dashboard says which, in one sentence", not _note_off,
      _note_off[:5])


def _note(dash, card):
    dc = [x for x in _tile_layout(dash)[1]["dashcards"] if x["card_id"] == card_ids[card]][0]
    return dc["visualization_settings"].get("card.description") or ""


check("...the debt cards the Asset class, their class being fixed",
      all(_note(d, c + (s if d.endswith(s) else "")).endswith(
          " The Asset class picker does not reach it.")
          and "Its class is fixed, so it takes no Asset class." in CARDS[c][1]
          for d, s in (("Exposure", p.PRIVACY_SUFFIX), ("Exposure (privacy)", p.PRIVACY_SUFFIX))
          for c in ("Debt by credit rating, share of debt and cash", "Debt by maturity",
                    "Debt by coupon kind", "Public equity by cycle")))
check("...the Cash Flow figures and diagram the Section, saying why",
      all(" The Section, " in _note(d, c + s)
          and p.SECTION_DECLINED_DESC in CARDS[c + s][1]
          for d, s in (("Cash Flow", ""), ("Cash Flow (privacy)", p.PRIVACY_SUFFIX))
          for c in ("Operating in", "Operating out", "Net cash flow", "Savings rate", "Yield share", "Cash flow")))
check("...the withholding the Type",
      all(_note(d, "Withholding" + s).endswith(" The Type and Position pickers do not reach it.")
          for d, s in (("Income", ""), ("Income (privacy)", p.PRIVACY_SUFFIX))),
      _note("Income", "Withholding"))
check("...the Allocation tiles not the Vehicle picker, which sits on its one tile",
      all("Vehicle" not in (dc["visualization_settings"].get("card.description") or "")
          for dc in _tile_layout("Allocation")[1]["dashcards"]))

section("every description is short")

# A pop-up reads in one glance: a card says what it shows and at most
# one caveat, and with the pickers that miss its tile it stays under 40
# words; so does every dashboard's blurb. None names the defaults a
# card runs over opened on its own.
_LIMIT = 40
_long_cards = {n: len(CARDS[n][1].split()) for n in CARDS if len(CARDS[n][1].split()) >= _LIMIT}
_long_tiles = {(d["name"], _by_id[dc["card_id"]]): len(
    (dc["visualization_settings"].get("card.description") or CARDS[_by_id[dc["card_id"]]][1]).split())
    for d in layouts for dc in d["dashcards"] if dc["card_id"] is not None}
_long_tiles = {k: v for k, v in _long_tiles.items() if v >= _LIMIT}
_long_blurbs = {n: len(d[0].split()) for n, d in DEFS.items() if len(d[0].split()) >= _LIMIT}
check(f"every card description, with the pickers that miss its tile, and every blurb is under {_LIMIT} words",
      not _long_cards and not _long_tiles and not _long_blurbs, (_long_cards, _long_tiles, _long_blurbs))
check("...and none names what a card runs over opened on its own",
      not [n for n in CARDS if "standalone" in CARDS[n][1]]
      and not [n for n, d in DEFS.items() if "standalone" in d[0]])

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
    check("...and says why, or on the twin what its shares are of",
          CARDS[_n][1].endswith(" Each part is a share of what the holdings are expected to pay.")
          if _n.endswith(p.PRIVACY_SUFFIX)
          else "look-back averages ignore window, source, section and grain" in CARDS[_n][1], CARDS[_n][1])
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
# reporting-currency column outside the CASE the variable drives. The
# Wealth Overview's borrowed tiles are their home dashboards' cards,
# checked there; the overview's mapping of the Currency picker onto
# them is checked in the Wealth Overview's section.
_CCY_CASE = re.compile(r"CASE \{\{currency\}\}(?: WHEN '[A-Z]{3}' THEN -?\w+)+ END")
_WEALTH_DASHES = ("Wealth Overview", "Allocation")
for _d in _WEALTH_DASHES:
    for _name in (_d, _d + p.PRIVACY_SUFFIX):
        for _c, *_ in (t for t in DEFS[_name][3] if card_ids[t[0]] not in _BORROWED):
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
# `wealthdb holdings sources` prints, whatever the time range; the three
# share their tags, so net worth is positions plus cash.
for _c in ("Net worth", "Positions value", "Cash balance",
           "Net worth (privacy)", "Positions value (privacy)",
           "Cash balance (privacy)"):
    _q = CARDS[_c][2]
    check(f"'{_c}' reads the latest holdings, with no time tag",
          "FROM web_sources_latest" in (sql_of(_q) or "")
          and set(_q["native"]["template-tags"]) == {"currency", "source", "tax_wrapper", "asset_class"}
          and "{{time_range}}" not in sql_of(_q) and "whatever the Time range" in CARDS[_c][1])
check("...and no field filter binds the snapshot",
      ("web_sources_latest", "snapshot_at") not in p.FIELD_IDS
      and "snapshot_at" not in p.FILTER_FIELD_COLUMNS["web_sources_latest"])

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
# every reporting currency beside it; its rates table, the days the
# income received covers and the stock positions' sizes hold no money.
_LONG_VIEWS = {"web_gains", "web_exposure", "web_exposure_debt", "web_exposure_issuers", "web_exposure_securities",
               "web_yield", "web_yield_curve", "web_yield_received", "web_cashflow_plan"}
_FX_TABLES = {"web_yield_parts": "report_yield_parts", "web_yield_events": "report_yield_events"}
_missing = {v: [c for c in p.REPORTING_CURRENCIES
                if not re.search(rf"_{c.lower()}\b", _latest_definition(v))]
            for v in p.web_views_wanted()
            if v not in _LONG_VIEWS | set(_FX_TABLES) | {"web_yield_rates", "web_income_days", "web_cashflow_pool",
                                                          "web_position_sizes", "web_history_calendar"}}
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
    check("...and the merchant ranking's description names it",
          "ranked in GBP" in _gbp_cards["Top 50 merchants"][1])
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

section("the gold's storage format is one the driver reads")

# The driver bundles the DuckDB of its own version (web/Dockerfile), and
# the engine creates gold files in that version's format
# (gold.StorageVersion): a driver bump must move both.
_driver = re.search(r"metabase_duckdb_driver/releases/download/(\d+)\.(\d+)\.",
                    open(os.path.join(_REPO, "web", "Dockerfile"), encoding="utf-8").read())
_storage = re.search(r'StorageVersion = "v(\d+)\.(\d+)\.',
                     open(os.path.join(_REPO, "wealthdb", "internal", "gold", "open.go"), encoding="utf-8").read())
check("the engine writes the storage format of the DuckDB the driver bundles",
      _driver and _storage and _driver.groups() == _storage.groups(),
      f"{_driver and _driver.groups()} vs {_storage and _storage.groups()}")

section("Metabase never fingerprints a list column")


class FieldAPI:
    """A stand-in for provision.req that records each field write and
    answers it with `status`."""

    def __init__(self, status=200):
        self.status, self.puts = status, []

    def __call__(self, base, path, method="GET", data=None, session=None, timeout=30):
        self.puts.append((method, path, data))
        return self.status, {"message": "refused"}


_tables = [{"name": "web_yield", "fields": [
    {"id": 1, "name": "tags", "database_type": "VARCHAR[]", "visibility_type": "normal"},
    {"id": 2, "name": "symbol", "database_type": "VARCHAR", "visibility_type": "normal"}]},
    {"name": "report_yield_positions", "fields": [
        {"id": 3, "name": "tags", "database_type": "VARCHAR[]", "visibility_type": "sensitive"},
        {"id": 4, "name": "weights", "database_type": "DOUBLE[]"}]}]
real_req = p.req
try:
    api = p.req = FieldAPI()
    with quiet():
        p.unfingerprint_lists("b", "s", _tables)
    check("each list column not yet kept from fingerprinting is marked sensitive",
          api.puts == [("PUT", "/api/field/1", {"visibility_type": "sensitive"}),
                       ("PUT", "/api/field/4", {"visibility_type": "sensitive"})], api.puts)
    # A marked field leaves the metadata a later run reads.
    api = p.req = FieldAPI()
    p.unfingerprint_lists("b", "s", [{"name": t["name"], "fields": [f for f in t["fields"] if f["id"] not in (1, 4)]}
                                     for t in _tables])
    check("...so a later run marks nothing", api.puts == [], api.puts)
    api = p.req = FieldAPI(403)
    _err = io.StringIO()
    with quiet(), contextlib.redirect_stderr(_err):
        p.unfingerprint_lists("b", "s", _tables)
    check("...and a field that cannot be marked is named, and does not stop provisioning",
          len(api.puts) == 2 and "web_yield.tags" in _err.getvalue(), (api.puts, _err.getvalue()))
finally:
    p.req = real_req
# The one card that reads a list column reads it in native SQL, which a
# sensitive field leaves as it is: no picker and no model may bind one.
_list_cols = {m.group(1) for f in sorted(os.listdir(os.path.join(_REPO, "wealthdb", "internal", "gold", "migrations")))
              if f.endswith(".sql")
              for m in re.finditer(r"^\s+(\w+)\s+\w+(?:\(\d+(?:,\s*\d+)?\))?\[\]",
                                   open(os.path.join(_REPO, "wealthdb", "internal", "gold", "migrations", f),
                                        encoding="utf-8").read(), re.M)}
check("the gold's list columns are found", "tags" in _list_cols, _list_cols)
check("no picker binds a list column",
      not _list_cols & {c for cs in p.FILTER_FIELD_COLUMNS.values() for c in cs}, _list_cols)
check("no model reads a list column",
      not [m for m, (sql, _d) in p.report_models().items()
           if any(re.search(rf"\b{c}\b", sql) for c in _list_cols)], _list_cols)

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
      which is no tax;
    - the income lines: each line received, and wages of 1,000 on 25
      September 2026 into src-1, which no holding pays;
    - src-1 is taxable, src-2 a joint taxable account;
    - the cash flow projection's run-rate: 200 a month of earnings into
      src-1, 10 of other receipts into src-2.

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
               "CREATE TABLE web_sources_latest (silver_source_id TEXT, tax_wrapper TEXT);"
               "CREATE TABLE web_income (occurred_at TIMESTAMP, silver_source_id TEXT, account_external_id TEXT,"
               " account_label TEXT, tax_wrapper TEXT, income_detailed TEXT, income_label TEXT,"
               " value_usd DOUBLE, value_chf DOUBLE, value_eur DOUBLE, value_gbp DOUBLE);")
    db.execute(_MIG_TEXT[138])
    db.execute(_MIG_TEXT[146])
    # The cash flow projection's table and view (0149, 0169); the pool's
    # accounts the Cash Flow checks below define for themselves.
    db.execute("CREATE MACRO cashflow_pool_accounts() AS TABLE"
               " SELECT silver_source_id, account_external_id FROM accounts")
    db.execute(_MIG_TEXT[149])
    db.execute(_MIG_TEXT[169])

    def insert(table, rows):
        for r in rows:
            db.execute(f"INSERT INTO {table} ({', '.join(r)}) VALUES ({', '.join('?' * len(r))})",
                       list(r.values()))

    insert("accounts", [{"silver_source_id": "src-1", "account_external_id": "acct-1",
                         "display_name": "Brokerage", "account_kind": "brokerage",
                         "tax_wrapper": "taxable_personal"},
                        {"silver_source_id": "src-2", "account_external_id": "acct-2",
                         "display_name": "Savings", "account_kind": "deposit", "tax_wrapper": "taxable_joint"}])
    wrapper = {"src-1": "taxable_personal", "src-2": "taxable_joint"}
    insert("web_sources_latest", [{"silver_source_id": s, "tax_wrapper": w} for s, w in wrapper.items()])
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
         "tax_wrapper": wrapper[src],
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
    # The income lines: those received, and wages, which no holding pays.
    detailed = {"interest": ("INCOME_INTEREST_EARNED", "Interest"), "coupons": ("INCOME_INTEREST_EARNED", "Interest"),
                "dividends": ("INCOME_DIVIDENDS", "Dividends"), "rent": ("INCOME_RENTAL", "Rent"),
                "salary": ("INCOME_SALARY", "Salary")}
    label = {"src-1": "Brokerage (src-1 brokerage)", "src-2": "Savings (src-2 deposit)"}
    insert("web_income", [
        {"occurred_at": dt.datetime(day.year, day.month, day.day, 9), "silver_source_id": src,
         "account_external_id": acct, "account_label": label[src], "tax_wrapper": wrapper[src],
         "income_detailed": detailed[typ][0], "income_label": detailed[typ][1],
         **{f"value_{c.lower()}": v * per_usd.get(c, 1.0) for c in p.REPORTING_CURRENCIES}}
        for src, acct, _key, day, typ, v, _w in
        [*received, ("src-1", "acct-1", None, dt.date(2026, 9, 25), "salary", 1000.0, None)]])
    # The other income's run-rate the cash flow projection stores: 200 a
    # month of earnings into src-1 and 10 of other receipts into src-2.
    insert("report_cashflow_plan", [
        {"computed_at": 0, "currency": ccy, "month": m, "month_start": _month_start(m), "part": "other_income",
         "class": cls, "silver_source_id": src, "account_external_id": acct, "value": v * k, "quality": "trailing"}
        for m in range(1, 37) for ccy, k in per_usd.items()
        for cls, src, acct, v in (("earnings", "src-1", "acct-1", 200.0), ("other_receipts", "src-2", "acct-2", 10.0))])

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
    # Every other line: the wages of 1,000 in September 2026. The
    # run-rate: 210 a month over the horizon's months and 10/31 of it for
    # the rest of October.
    _received = 80 + 350 + 725.0
    _lines = _received + 1000.0
    _exp12, _exp24 = 260 + 800 + 162.5 + 1500, 500 + 1600 + 312.5 + 3000
    _rest = 10 / 31
    _rr12, _rr24 = 210 * (12 + _rest), 210 * (24 + _rest)
    _e12, _e24 = _exp12 + _rr12, _exp24 + _rr24
    for _args, _want in (({}, _lines + _e12), ({"time_range": _y24}, 80 + _e12),
                         ({"time_range": _y25}, 350 + _e12), ({"time_range": _y26}, 1725 + _e12),
                         ({"time_range": _y27}, _e12), ({"time_range": _y26, "horizon": "24"}, 1725 + _e24)):
        _got = _one("Income in the window", **_args)
        check(f"the income under {_args or 'the defaults'} is every line the Time range received and what "
              "the Horizon expects, the run-rate among it", _near(_got, _want), (_got, _want))
    check("...which is Net income and the income expected",
          _near(_one("Income in the window"), _one("Net income") + _one("Income expected"))
          and _near(_one("Income received"), _one("Net income")),
          (_one("Income in the window"), _one("Net income"), _one("Income expected")))
    check("the received tiles count every line the Time range received, before last year too",
          _near(_one("Income received"), _lines) and _near(_one("Income received", time_range=_y26), 1725.0)
          and _near(_one("Holdings' dividends received", time_range=_y26), 200.0)
          and _near(_one("Holdings' interest received", time_range=_y25), 50.0)
          and _near(_one("Holdings' dividends received", time_range=_y24), 80.0)
          and _near(_one("Income received", time_range=_y27), 0.0))
    check("the expected tiles count what the Horizon expects, the holdings' and the run-rate",
          _near(_one("Income expected"), _e12) and _near(_one("Income expected", horizon="24"), _e24)
          and _near(_one("Holdings' interest expected"), 260 + 800 + 162.5)
          and _near(_one("Holdings' dividends expected"), 0.0))
    # The days received run from the first, 15 March 2024, to 21 October
    # 2026: 17/31 of March, 30 months, and 21/31 of October. The horizon
    # adds 12 and the 10/31 of October left: 43 and 17/31 months. Over
    # 2026 the days received count 9 and 21/31: 22 months.
    _all_months = 43 + 17 / 31
    check("the monthly average divides by the months the days received and the horizon cover",
          _near(_one("Monthly average income"), (_lines + _e12) / _all_months)
          and _near(_one("Monthly average income", time_range=_y26), (1725 + _e12) / 22)
          and _near(_one("Monthly average income", time_range=_y26, horizon="24"), (1725 + _e24) / 34))
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
    # Every income line received, the wages of 1,000 among them, and the
    # income expected, less the tax: the income in the window less what
    # the Withholding tile takes.
    check("...and the income after it every line received and the income expected, less that tax",
          _near(_one("Income after withholding"), _lines + _e12 - 202.0)
          and _near(_one("Income after withholding"), _one("Income in the window") - _one("Withholding"))
          and _near(_one("Income after withholding", time_range=_y26), 1725 + _e12 - 145.0)
          and _near(_one("Income after withholding (privacy)"), 100 * (_lines + _e12 - 202) / (_lines + _e12))
          and _near(_one("Withholding (privacy)"), 100 * 202 / (_lines + _e12)),
          (_one("Income after withholding"), _one("Withholding (privacy)")))
    # The Type picker lists the income lines' labels; the holdings' cards
    # read the types a label maps to, and wages none.
    _ty = {"Salary": ["Salary"], "Interest": ["Interest"], "Dividends": ["Dividends"]}
    check("the Type picker reaches the holdings' cards through the types its labels map to",
          _one("Income received", type=_ty["Interest"]) == 450
          and _near(_one("Income expected", type=_ty["Interest"]), 260 + 800 + 162.5)
          and _one("Income received", type=_ty["Dividends"]) == 580
          and _one("Income expected", type=_ty["Dividends"]) == 0
          and _near(_one("Income received", type=_ty["Interest"] + _ty["Dividends"]), 1030.0),
          [_one("Income received", type=v) for v in _ty.values()])
    _salary_bands = {s for _m, s, _v in _run("Received and expected by month", type=_ty["Salary"])}
    check("...a label of the other lines selecting those lines and their class's run-rate alone, and the "
          "withholding not narrowed",
          _near(_one("Income received", type=_ty["Salary"]), 1000.0)
          and _near(_one("Income expected", type=_ty["Salary"]), 200 * (12 + _rest))
          and _near(_one("Income in the window", type=_ty["Salary"]), 1000 + 200 * (12 + _rest))
          and all(_one(n, type=_ty["Salary"]) == 0 for n in ("Holdings' interest received",
                                                              "Holdings' dividends expected"))
          and _salary_bands == {"salary and earnings (received)", "salary and earnings (expected, run-rate)"}
          and _run("Expected payments", type=_ty["Salary"]) == []
          and _near(_one("Withholding", type=_ty["Salary"]), 202.0),
          _salary_bands)
    # src-1 is taxable, src-2 a joint taxable account.
    _tw = {w: [w] for w in ("taxable_personal", "taxable_joint")}
    check("the Tax wrapper picker keeps an account's rows on every view a card reads",
          _near(_one("Income in the window", tax_wrapper=_tw["taxable_joint"]), 1787.5 + 10 * (12 + _rest))
          and _near(_one("Income in the window", tax_wrapper=_tw["taxable_personal"]),
                    1030 + 1000 + 260 + 800 + 200 * (12 + _rest))
          and _one("Withholding", tax_wrapper=_tw["taxable_joint"]) == 0
          and _near(_one("Withholding", tax_wrapper=_tw["taxable_personal"]), 202.0)
          and _near(_one("Income after withholding", tax_wrapper=_tw["taxable_joint"]), 1787.5 + 10 * (12 + _rest))
          and _near(_one("Income after withholding", tax_wrapper=_tw["taxable_personal"]),
                    1030 + 1000 + 260 + 800 + 200 * (12 + _rest) - 202.0)
          and len(_run("Received and expected by position", tax_wrapper=_tw["taxable_joint"])) == 4,
          [_one("Income in the window", tax_wrapper=v) for v in _tw.values()])
    _salary_rows = _run("Received and expected by position", type=_ty["Salary"])
    check("...and with a Type picked the positions table lists the holdings that pay it, a wage under its "
          "account by its class",
          [(r[1], r[3]) for r in _salary_rows] == [("Brokerage (src-1 brokerage)", "salary and earnings")]
          and [r[2] for r in _run("Received and expected by position (privacy)", type=_ty["Salary"])]
          == ["salary and earnings"]
          and sorted(r[3] for r in _run("Received and expected by position", type=_ty["Interest"]))
          == ["cash:CHF", "cash:USD", "isin:XX01"], _run("Received and expected by position", type=_ty["Interest"]))
    # A wage line older than every day the received figures cover, and
    # tax booked on an account with no income line: with no Time range
    # the one counts as Net income counts it, and a tax wrapper reaches
    # the other through its account.
    _db.execute("BEGIN")
    _db.execute("INSERT INTO web_income (occurred_at, silver_source_id, account_external_id, account_label, tax_wrapper,"
                " income_detailed, income_label, value_usd, value_chf) VALUES (TIMESTAMP '2020-06-01 09:00', 'src-1',"
                " 'acct-1', 'Brokerage (src-1 brokerage)', 'taxable_personal', 'INCOME_SALARY', 'Salary', 500, 400)")
    _db.execute("INSERT INTO accounts (silver_source_id, account_external_id, display_name, account_kind, tax_wrapper)"
                " VALUES ('src-2', 'acct-3', 'Current', 'deposit', 'taxable_joint')")
    _db.execute("INSERT INTO serving_transactions (silver_source_id, account_external_id, occurred_at, kind,"
                " net_amount, value_usd, value_chf, value_eur, value_gbp) VALUES ('src-2', 'acct-3', "
                f"{int(dt.datetime(2026, 4, 1, 9, tzinfo=dt.timezone.utc).timestamp())}, 'tax', -9, -9, -7.2, -9, -9)")
    _old = (_one("Income after withholding", time_range=None), _one("Withholding", tax_wrapper=_tw["taxable_joint"]),
            _one("Withholding", tax_wrapper=list(_tw)), _one("Withholding"))
    _db.execute("ROLLBACK")
    check("with no Time range the income after withholding counts every line, as Net income does, and the tax "
          "booked on an account with no income line follows its account's wrapper",
          _near(_old[0], _lines + 500 + _e12 - 211.0) and _near(_old[1], 9.0)
          and _near(_old[2], _old[3]) and _near(_old[3], 211.0), _old)
    # An older wage, and a refund of tax: a range over the wage's year
    # reaches it, as the days start at the first income line, June 2020,
    # so the year counts seven months; the
    # refund's label keeps its class's run-rate, other receipts; the
    # Account picker keeps an account's run-rate.
    _db.execute("BEGIN")
    _db.execute("INSERT INTO web_income (occurred_at, silver_source_id, account_external_id, account_label, tax_wrapper,"
                " income_detailed, income_label, value_usd, value_chf) VALUES"
                " (TIMESTAMP '2020-06-01 09:00', 'src-1', 'acct-1', 'Brokerage (src-1 brokerage)', 'taxable_personal',"
                " 'INCOME_SALARY', 'Salary', 500, 400),"
                " (TIMESTAMP '2026-04-01 09:00', 'src-2', 'acct-2', 'Savings (src-2 deposit)', 'taxable_joint',"
                " 'INCOME_TAX_REFUND', 'Tax refund', 40, 32)")
    _far = (_one("Income received", time_range=("2020-01-01", "2020-12-31")),
            _one("Monthly average income", time_range=("2020-01-01", "2020-12-31"), horizon="12"),
            _one("Income expected", type=["Tax refund"]), _one("Income received", type=["Tax refund"]))
    _db.execute("ROLLBACK")
    check("a range before the first holding's income reaches the older lines, and a label of the other "
          "receipts keeps their run-rate alone",
          _near(_far[0], 500.0) and _near(_far[1], (500 + _e12) / (7 + 12 + _rest))
          and _near(_far[2], 10 * (12 + _rest)) and _near(_far[3], 40.0), _far)
    check("...and the Account picker keeps an account's run-rate",
          _near(_one("Income expected", account=["Savings (src-2 deposit)"]), 162.5 + 1500 + 10 * (12 + _rest))
          and _near(_one("Income expected", account=["Brokerage (src-1 brokerage)"]),
                    260 + 800 + 200 * (12 + _rest)),
          [_one("Income expected", account=[a]) for a in ("Savings (src-2 deposit)", "Brokerage (src-1 brokerage)")])
    check("where nothing is withheld, the withholding tiles read 0, not null",
          _one("Withholding", source=["src-2"]) == 0 and _one("Withholding (privacy)", source=["src-2"]) == 0
          and _near(_one("Income after withholding", source=["src-2"]), 1787.5 + 10 * (12 + _rest))
          and _near(_one("Income after withholding (privacy)", source=["src-2"]), 100.0))
    # A twin's share of nothing reads 0, as the base figure does.
    _scalars = [n for n in _in_all if CARDS[n][0] == "scalar"]
    _nulls = [(n, a) for n in _scalars for a in ({}, {"source": ["src-2"]}, {"time_range": _y27})
              if _one(n, **a) is None]
    check("no headline figure reads null, at the defaults, for one source, or over days not yet come",
          len(_scalars) == 20 and not _nulls, _nulls)
    check("...the twin's received figures reading 0 where nothing was received",
          all(_one(n + p.PRIVACY_SUFFIX, time_range=_y27) == 0
              for n in ("Income received", "Holdings' interest received", "Holdings' dividends received")))
    # With no yield report there is no day received: the holdings'
    # received figures read 0, every other line counts with no Time range
    # picked, the run-rate has no rest of a month, and the monthly average
    # divides by the horizon alone.
    _db.execute("BEGIN")
    _db.execute("DELETE FROM report_yield_positions")
    _empty = {n: _one(n) for n in _scalars}
    _db.execute("ROLLBACK")
    check("with no yield report no figure reads null, and the average is the horizon's",
          not [n for n, v in _empty.items() if v is None] and _empty["Holdings' interest received"] == 0
          and _near(_empty["Income received"], 1000.0)
          and _near(_empty["Income in the window"], 1000 + _exp12 + 210 * 12)
          and _near(_empty["Monthly average income"], (1000 + _exp12 + 210 * 12) / 12), _empty)
    # The second source's: its rent received, its cash's 162.50 and
    # twelve months of rent expected.
    check("the Source picker keeps a source's received and expected rows",
          _near(_one("Income in the window", source=["src-2"]), 1787.5 + 10 * (12 + _rest)),
          _one("Income in the window", source=["src-2"]))
    _accounts = _run("Received and expected by account")
    check("the accounts table splits each account's received and expected income",
          [(r[0], round(r[1], 6), round(r[2], 6), round(r[3], 6)) for r in _accounts]
          == [("Brokerage (src-1 brokerage)", 2030.0, round(1060 + 200 * (12 + _rest), 6),
               round(3090 + 200 * (12 + _rest), 6)),
              ("Savings (src-2 deposit)", 125.0, round(1662.5 + 10 * (12 + _rest), 6),
               round(1787.5 + 10 * (12 + _rest), 6))], _accounts)
    _months = {(m.date().isoformat(), s): v for m, s, v in _run("Received and expected by month")}
    check("the by-month chart's bands are the Time range's received and the Horizon's expected",
          _near(sum(_months.values()), _lines + _e12)
          and _near(_months.get(("2026-09-01", "salary and earnings (received)")), 1000.0)
          and _near(_months.get(("2026-10-01", "salary and earnings (expected, run-rate)")), 200 * _rest)
          and _near(_months.get(("2026-11-01", "other receipts (expected, run-rate)")), 10.0)
          and _near(_months.get(("2026-12-01", "coupons (expected)")), 400.0)
          and _near(_months.get(("2026-02-01", "coupons (received)")), 400.0)
          and _near(_months.get(("2024-03-01", "dividends (received)")), 80.0)
          and _near(_months.get(("2026-10-01", "interest (expected)")), 32.5)
          and ("2027-11-01", "interest (expected)") not in _months, _months)
    _months26 = {(m.date().isoformat(), s) for m, s, _v in _run("Received and expected by month", time_range=_y26)}
    check("...the received ones moving with the Time range",
          ("2025-06-01", "dividends (received)") not in _months26
          and ("2026-02-01", "coupons (received)") in _months26)
    # The twins divide by the biggest month: September 2026's wages of
    # 1,000. (Each of the horizon's months pays 32.50 and the run-rate's
    # 210, months 2 and 8 the coupon of 400 too, each month but October
    # the rent of 125: December and June 767.50.) The projected income's
    # biggest month is the holdings' own.
    check("the twins divide by the biggest month",
          _near(_one("Projected income (privacy)"), 100 * 1190 / 12 / 432.5)
          and _near(_one("Monthly average income (privacy)"), 100 * (_lines + _e12) / _all_months / 1000)
          and all(_near(v, 100 * _months[(m.date().isoformat(), s)] / 1000)
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

    section("the Allocation issuers card's SQL, run on an invented book")

    # Issuer ranks at two month ends and at a refresh this month; the
    # daily history holds every day from August on. The card reads the
    # latest instant on or before the as-of day: the September month end
    # for a day inside October before the refresh, the refresh from its
    # day on, a day after the history included, and nothing for a day
    # before the first instant. The day reaches it through the calendar
    # over the history (migration 0166).
    _db.execute("CREATE TABLE serving_global_history (as_of_day BIGINT)")
    _db.execute("INSERT INTO serving_global_history SELECT epoch(d)::BIGINT"
                " FROM range(TIMESTAMP '2026-08-01', TIMESTAMP '2026-10-22', INTERVAL 1 DAY) t(d)")
    _db.execute(_MIG_TEXT[166])
    _db.execute("DELETE FROM report_exposure_issuer_ranks")
    for _at, _who, _v in (("2026-08-31 23:59:59", "Example Alpha", 500.0),
                          ("2026-09-30 23:59:59", "Example Beta", 600.0),
                          ("2026-10-21 12:00:00", "Example Gamma", 700.0)):
        _epoch = int(dt.datetime.fromisoformat(_at).replace(tzinfo=dt.timezone.utc).timestamp())
        for _rank, _name, _value in ((1, _who, _v), (2, "(none)", 100.0)):
            _db.execute("INSERT INTO report_exposure_issuer_ranks VALUES"
                        " (0, 'USD', ?, ?, ?, 'LEI0000000000000000X', ?, 50, 50, 100, 0, 0)",
                        [_epoch, _rank, _name, _value])
    _iss = "Top 100 issuers"

    def _issuers(day, name=_iss):
        return _run(name, as_of_day=(day, day) if day else None)

    check("on a day inside this month before the refresh, the issuers card reads the last month end",
          [r[:3] for r in _issuers("2026-10-15")] == [(1, "Example Beta", 600.0)], _issuers("2026-10-15"))
    check("...on the refresh's day, the refresh",
          [r[1] for r in _issuers("2026-10-21")] == ["Example Gamma"], _issuers("2026-10-21"))
    check("...and on a day after the history, the refresh too",
          [r[1] for r in _issuers("2026-10-25")] == ["Example Gamma"], _issuers("2026-10-25"))
    check("...before the first instant, nothing", _issuers("2026-08-15") == [], _issuers("2026-08-15"))
    check("...opened standalone, the latest day",
          [r[1] for r in _issuers(None)] == ["Example Gamma"], _issuers(None))
    check("...and its twin the same rows, with no value",
          [r[:2] for r in _issuers("2026-10-15", p.privacy_name(_iss))] == [(1, "Example Beta")]
          and len(_issuers("2026-10-15", p.privacy_name(_iss))[0]) == 7)

    section("the Exposure securities card's SQL, run on an invented book")

    # Today a stock in three slices: held directly in src-1 (300, sector
    # and country stated), through one fund in src-2 (200, another
    # country) and through two funds, one of them that one, in src-1
    # (100, no sector); a private holding of 500, named by its own key;
    # and cash of 400. A month end before, a stock of 50 alone. The card
    # sums the stock to one row with its heaviest slice's labels and its
    # two funds, each a share of the 1,500.
    _db.execute(_MIG_TEXT[165])
    _sec_now = int(dt.datetime(2026, 10, 21, 23, 59, 59, tzinfo=dt.timezone.utc).timestamp())
    _sec_sep = int(dt.datetime(2026, 9, 30, 23, 59, 59, tzinfo=dt.timezone.utc).timestamp())
    for _at, _src, _w, _cls, _key, _name, _sector, _country, _v, _direct, _funds in (
            (_sec_now, "src-1", "taxable_personal", "public_equity", "symbol:QSTK", "Example Stock", "technology",
             "US", 300.0, 300.0, []),
            (_sec_now, "src-2", "taxable_joint", "public_equity", "symbol:QSTK", "Example Stock", "technology", "IE",
             200.0, 0.0, ["symbol:QF1"]),
            (_sec_now, "src-1", "taxable_personal", "public_equity", "symbol:QSTK", "Example Stock", None, "US",
             100.0, 0.0, ["symbol:QF1", "symbol:QF2"]),
            (_sec_now, "src-1", "taxable_personal", "private_equity", "holding:src-1:QPRV", "Example Private",
             None, None, 500.0, 500.0, []),
            (_sec_now, "src-1", "taxable_personal", "cash", "cash:USD", None, "cash", None, 400.0, 400.0, []),
            (_sec_sep, "src-1", "taxable_personal", "public_equity", "symbol:QOLD", "Example Old", None, None,
             50.0, 50.0, [])):
        _db.execute("INSERT INTO report_exposure_security_slices VALUES (0, 'USD', ?, ?, ?, ?, ?, ?, NULL, ?, ?,"
                    " ?, ?, ?, 5e10, 'large', ?, ?, ?, ?)",
                    [_at, _src, _w, _cls, _key, _name, _sector, _country, _v, None if _sector is None else _v,
                     None if _country is None else _v, _v, _direct, _v - _direct, _funds])
    _secs = _run(_sec)
    check("the securities card sums a stock's slices to one row, its heaviest slice's labels, its funds once",
          [(r[0], r[4], r[5], r[8], r[9], r[10], r[11], round(r[12], 6)) for r in _secs]
          == [("symbol:QSTK", "technology", "US", 600.0, 300.0, 300.0, 2, 40.0),
              ("holding:src-1:QPRV", None, None, 500.0, 500.0, 0.0, 0, round(100 * 500 / 1500, 6)),
              ("cash:USD", "cash", None, 400.0, 400.0, 0.0, 0, round(100 * 400 / 1500, 6))], _secs)
    _secs2 = _run(_sec, source=["src-2"])
    check("...a source picked, its slice alone, the whole of that selection",
          [(r[0], r[5], r[8], r[12]) for r in _secs2] == [("symbol:QSTK", "IE", 200.0, 100.0)], _secs2)
    _secs_sep = _run(_sec, time_range=("2026-09-01", "2026-09-30"))
    check("...a window that ends before today, its last instant",
          [r[0] for r in _secs_sep] == ["symbol:QOLD"], _secs_sep)
    _twin_secs = _run(p.privacy_name(_sec))
    check("...and its twin the same rows as shares, a holding's own key and name hidden",
          [(r[0], r[1], round(r[8], 6), round(r[11], 6)) for r in _twin_secs]
          == [("symbol:QSTK", "Example Stock", 20.0, 40.0), ("(unnamed)", None, round(100 * 500 / 1500, 6),
                                                            round(100 * 500 / 1500, 6)),
              ("cash:USD", None, round(100 * 400 / 1500, 6), round(100 * 400 / 1500, 6))], _twin_secs)

    section("the Top 100 positions cards' size column, run on an invented book")

    # One day's positions: a stock on two accounts and one on one, both
    # sized, a stock nothing sizes, a fund, and two accounts' cash in
    # francs (migration 0171). The size joins after the ranking, so a
    # card keeps its rows, order and values; a stock on two lines stays
    # one row, and each account's cash is a row of its own.
    _db.execute("CREATE TABLE web_positions_history (as_of_day DATE, silver_source_id TEXT, tax_wrapper TEXT,"
                " line_key TEXT, symbol TEXT, name TEXT, asset_class TEXT, vehicle TEXT, currency TEXT,"
                " value_usd DOUBLE, value_chf DOUBLE, value_eur DOUBLE, value_gbp DOUBLE)")
    _db.execute("CREATE TABLE web_position_sizes (symbol TEXT, name TEXT, asset_class TEXT, vehicle TEXT,"
                " size TEXT)")
    for _sym, _veh, _v in (("QBIG", "stock", 400.0), ("QBIG", "stock", 200.0), ("QMID", "stock", 500.0),
                           ("QNOS", "stock", 300.0), ("QFND", "fund", 450.0)):
        _db.execute("INSERT INTO web_positions_history VALUES (DATE '2026-10-21', 'src-1', 'taxable_personal',"
                    " NULL, ?, ?, 'public_equity', ?, 'USD', ?, ?, ?, ?)", [_sym, _sym + " Example", _veh, _v, _v, _v, _v])
    for _key, _v in (("src-1/acct-1", 250.0), ("src-1/acct-2", 100.0)):
        _db.execute("INSERT INTO web_positions_history VALUES (DATE '2026-10-21', 'src-1', 'taxable_personal',"
                    " ?, NULL, 'Cash CHF', 'cash', 'demand_deposit', 'CHF', ?, ?, ?, ?)", [_key, _v, _v, _v, _v])
    _db.execute("INSERT INTO web_position_sizes VALUES ('QBIG', 'QBIG Example', 'public_equity', 'stock', 'large'),"
                " ('QMID', 'QMID Example', 'public_equity', 'stock', 'mid'),"
                " ('QNOS', 'QNOS Example', 'public_equity', 'stock', NULL)")
    _top = [(r[0], r[4], r[5]) for r in _run("Top 100 positions")]
    check("the Top 100 positions card gives a stock its size, a fund none, its ranking and values unchanged",
          _top == [("QBIG", "large", 600.0), ("QMID", "mid", 500.0), ("QFND", None, 450.0), ("QNOS", None, 300.0),
                   (None, None, 250.0), (None, None, 100.0)], _top)
    check("...each account's cash in francs a row of its own, named and classed as cash",
          [r[1:4] for r in _run("Top 100 positions") if r[0] is None]
          == [("Cash CHF", "cash", "demand_deposit")] * 2)
    _top_pct = [(r[0], r[4], round(r[5], 6)) for r in _run(p.privacy_name("Top 100 positions"), vehicle=["stock"])]
    check("...and its twin the shares, the vehicle narrowing the list but not the denominator",
          _top_pct == [("QBIG", "large", round(60000 / 2200, 6)), ("QMID", "mid", round(50000 / 2200, 6)),
                       ("QNOS", None, round(30000 / 2200, 6))], _top_pct)
    # The history ends on 2026-10-21 (the issuers' calendar above). A day
    # after it reads that day's positions, on both cards; a day before
    # the history reads none.
    _late = [(r[0], r[5]) for r in _run("Top 100 positions", as_of_day=("2026-10-25", "2026-10-25"))]
    check("a day after the history reads the positions of its last day",
          _late == [(r[0], r[2]) for r in _top]
          and len(_run(p.privacy_name("Top 100 positions"), as_of_day=("2026-10-25", "2026-10-25"))) == 6, _late)
    check("...and a day before the history none",
          _run("Top 100 positions", as_of_day=("2026-07-15", "2026-07-15")) == [])

    section("the Allocation by currency cards' SQL, run on an invented book")

    # One day's lines by currency (migration 0171): positions and cash in
    # francs and dollars on one source, euros on another. The cards sum
    # every bucket, cash included, largest first; the twin's shares total
    # 100; the Source picker narrows both.
    _db.execute("CREATE TABLE web_currencies_history (as_of_day DATE, silver_source_id TEXT, tax_wrapper TEXT,"
                " asset_class TEXT, currency TEXT, value_usd DOUBLE, value_chf DOUBLE, value_eur DOUBLE,"
                " value_gbp DOUBLE)")
    for _src, _cls, _cc, _v in (("src-1", "public_equity", "USD", 500.0), ("src-1", "cash", "USD", 100.0),
                                ("src-1", "cash", "CHF", 300.0), ("src-1", "fixed_income", "CHF", 200.0),
                                ("src-2", "cash", "EUR", 150.0)):
        _db.execute("INSERT INTO web_currencies_history VALUES (DATE '2026-10-21', ?, 'taxable_personal', ?, ?,"
                    " ?, ?, ?, ?)", [_src, _cls, _cc, _v, _v, _v, _v])
    _ccy = _run("Allocation by currency")
    check("the currency card sums every line by currency, cash included, largest first",
          _ccy == [("USD", 600.0), ("CHF", 500.0), ("EUR", 150.0)], _ccy)
    _ccy_cash = _run("Allocation by currency", asset_class=["cash"])
    check("...its cash alone under the Asset class picker",
          _ccy_cash == [("CHF", 300.0), ("EUR", 150.0), ("USD", 100.0)], _ccy_cash)
    check("...and the Source picker narrows it",
          _run("Allocation by currency", source=["src-2"]) == [("EUR", 150.0)])
    _ccy_pct = _run(p.privacy_name("Allocation by currency"))
    check("...its twin the same buckets as shares that total 100",
          [r[0] for r in _ccy_pct] == ["USD", "CHF", "EUR"] and _near(sum(r[1] for r in _ccy_pct), 100.0),
          _ccy_pct)

    section("the DV01 card's SQL, run on an invented book")

    # Two curves on one day: the USD curve at 3M and 5Y, the CHF curve at
    # 5Y alone, a liability's negative DV01 netting into it. The rows come
    # tenor by tenor, the USD curve first (the most rate risk), the CHF
    # curve's 3M bar empty.
    _db.execute("DELETE FROM report_yield_buckets")
    _at = int(dt.datetime(2030, 1, 15, tzinfo=dt.timezone.utc).timestamp())
    for _cc, _tenor, _order, _held, _v in (("USD", "3M", 1, "cash", 2.0), ("USD", "5Y", 3, "bonds", 30.0),
                                           ("USD", "5Y", 3, "funds", 10.0), ("CHF", "5Y", 3, "bonds", 12.0),
                                           ("CHF", "5Y", 3, "liabilities", -4.0)):
        _db.execute("INSERT INTO report_yield_buckets VALUES (0, ?, 'USD', 'src-1', 'acct-1', 'k', ?, ?, ?, ?, ?)",
                    [_at, _cc, _tenor, _order, _held, _v])
    _dv01 = _run("DV01 along the curve")
    check("the DV01 bars come tenor by tenor, the most rate risk first, an empty bar where a curve has no tenor",
          [(r[0], r[1], r[2]) for r in _dv01] == [("USD", "3M", 2.0), ("CHF", "3M", None),
                                                  ("USD", "5Y", 40.0), ("CHF", "5Y", 8.0)], _dv01)
    _db.execute("DELETE FROM report_yield_buckets")

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
    _db.execute(_MIG_TEXT[169])
    _db.execute(_MIG_TEXT[172])
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
    _db.execute("DELETE FROM report_cashflow_plan")
    _plan_rows = []
    for _ccy, _k in (("USD", 1.0), ("CHF", 0.8)):
        for _m in range(1, 37):
            # The other income's 50 a month, split by class and account
            # as gold 0169 stores it, and a wage on the vehicle's account
            # outside the pool (0172), which no Cash Flow card reads.
            _parts = [("other_income", "earnings", "src-1", "acct-1", 30.0),
                      ("other_income", "other_receipts", "src-2", "acct-2", 20.0),
                      ("outside_income", "earnings", "src-3", "acct-3", 900.0),
                      *(("operating_out", c, "", "", v) for c, v in _out.items()),
                      ("debt_service", "", "", "", 300.0 if _m <= 18 else 0.0), ("vehicles", "", "", "", -100.0)]
            _plan_rows += [(_ccy, _m, _month_start(_m), part, cls, src, acct, v * _k, "trailing terms")
                           for part, cls, src, acct, v in _parts]
    _db.executemany("INSERT INTO report_cashflow_plan (computed_at, currency, month, month_start, part, class,"
                    " silver_source_id, account_external_id, value, quality)"
                    " VALUES (0, ?, ?, ?, ?, ?, ?, ?, ?, ?)", _plan_rows)
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
    # The Wealth Overview's figure of the projection is the table's net,
    # summed over the Horizon; the twin's that net as a share of the
    # 3,410 the twelve months expect.
    _net = sum(r[8] for r in _t.values())
    check("the projected net cash flow is the projection's net over the Horizon",
          _near(_run("Projected net cash flow")[0][0], _net), (_run("Projected net cash flow"), _net))
    check("...under roll too",
          _near(_run("Projected net cash flow", maturing="roll")[0][0],
                sum(r[8] for r in _plan(maturing="roll").values())))
    check("...and its twin the share of the horizon's expected income",
          _near(_run("Projected net cash flow (privacy)")[0][0], 100 * _net / 3410))

    section("the Wealth Overview's new figures, run on an invented book")

    # The asset classes on two days: each day's classes as shares of
    # that day's total, the largest at the latest day first in the
    # stack's order, which puts it at the bottom.
    _db.execute("CREATE TABLE web_asset_classes_history (as_of_day DATE, silver_source_id TEXT, tax_wrapper TEXT,"
                " asset_class TEXT, value_usd DOUBLE, value_chf DOUBLE, value_eur DOUBLE, value_gbp DOUBLE)")
    for _day, _cls, _v in (("2026-09-30", "public_equity", 600.0), ("2026-09-30", "cash", 400.0),
                           ("2026-10-21", "public_equity", 900.0), ("2026-10-21", "cash", 300.0),
                           ("2026-10-21", "real_estate", -200.0)):
        _db.execute("INSERT INTO web_asset_classes_history VALUES (?, 'src-1', 'taxable_personal', ?, ?, ?, ?, ?)",
                    [_day, _cls, _v, _v * 0.8, _v, _v])
    _mix = [(str(d), c, round(v, 6)) for d, c, v in _run("Asset class mix over time")]
    check("the asset class mix is each day's classes as shares of its total, the smallest at the end first",
          _mix == [("2026-10-21", "real_estate", -20.0), ("2026-09-30", "cash", 40.0), ("2026-10-21", "cash", 30.0),
                   ("2026-09-30", "public_equity", 60.0), ("2026-10-21", "public_equity", 90.0)], _mix)
    _mix = [(str(d), c, round(v, 6)) for d, c, v in _run("Asset class mix over time", asset_class=["cash", "real_estate"])]
    check("...narrowed to two classes, their shares of the two, which sum to 100",
          _mix == [("2026-10-21", "real_estate", -200.0), ("2026-09-30", "cash", 100.0), ("2026-10-21", "cash", 300.0)],
          _mix)
    _mix = _run("Asset class mix over time", time_range=("2026-09-01", "2026-09-30"))
    check("...and the Time range keeps its days", {str(r[0]) for r in _mix} == {"2026-09-30"}, _mix)
    # A day whose classes cancel to float noise has no mix to show.
    for _cls, _v in (("public_equity", 0.1), ("cash", 0.2), ("real_estate", -0.3)):
        _db.execute("INSERT INTO web_asset_classes_history VALUES ('2026-08-31', 'src-1', 'taxable_personal', ?, ?, ?, ?, ?)",
                    [_cls, _v, _v, _v, _v])
    _mix = _run("Asset class mix over time")
    check("...and a day whose classes cancel shows nothing", "2026-08-31" not in {str(r[0]) for r in _mix}, _mix)
    # Nor does a day whose classes sum below zero: its shares would flip
    # every band's sign.
    for _cls, _v in (("public_equity", 100.0), ("real_estate", -400.0)):
        _db.execute("INSERT INTO web_asset_classes_history VALUES ('2026-07-31', 'src-1', 'taxable_personal', ?, ?, ?, ?, ?)",
                    [_cls, _v, _v, _v, _v])
    _mix = _run("Asset class mix over time")
    check("...nor a day whose classes sum below zero",
          {str(r[0]) for r in _mix} == {"2026-09-30", "2026-10-21"}, _mix)
    _db.execute("DELETE FROM web_asset_classes_history WHERE as_of_day IN (DATE '2026-08-31', DATE '2026-07-31')")
    check("...stacked, not normalized, so a negative band draws, with shares shown as percent",
          VIZ["Asset class mix over time"].get("stackable.stack_type") == "stacked"
          and VIZ["Asset class mix over time"].get("legend.is_reversed") is False
          and VIZ["Asset class mix over time"]["column_settings"]['["name","share_pct"]'] == {"suffix": "%"},
          VIZ["Asset class mix over time"])

    # The year-to-date returns read the summary since 1 January of this
    # year, in the chosen currency, and return the year a click passes on.
    _db.execute("CREATE TABLE report_returns (grain TEXT, granularity TEXT, currency TEXT,"
                " window_from_year INTEGER, twr DOUBLE, mwr DOUBLE)")
    _year = dt.date.today().year
    _db.executemany("INSERT INTO report_returns VALUES ('global', 'total', ?, ?, ?, ?)",
                    [("USD", _year, 0.05, 0.04), ("CHF", _year, 0.03, 0.02), ("USD", _year - 1, 0.20, 0.10)])
    _db.execute("INSERT INTO report_returns VALUES ('sources', 'total', 'USD', ?, 0.9, 0.9)", [_year])
    check("the year-to-date returns read this year's global summary in the chosen currency",
          _run("Return year to date (TWR)") == [(0.05, _year)] and _run("Return year to date (MWR)") == [(0.04, _year)]
          and _run("Return year to date (TWR)", currency="CHF") == [(0.03, _year)],
          (_run("Return year to date (TWR)"), _run("Return year to date (MWR)")))
    _db.execute("DELETE FROM report_returns WHERE window_from_year = ? AND currency = 'USD'", [_year])
    check("...and last year's where no refresh has read this one yet",
          _run("Return year to date (TWR)") == [(0.20, _year - 1)], _run("Return year to date (TWR)"))

    # The gains twins: each gain over the cost basis it is a gain on. A
    # sale with no cost basis, and a holding with none, count on neither
    # side; the unrealized one reads the window's last month alone.
    _db.execute("CREATE TABLE web_gains (period TIMESTAMP, silver_source_id TEXT, tax_wrapper TEXT, currency TEXT,"
                " missing_basis TEXT, realized DOUBLE, realized_cost_basis DOUBLE, unrealized_end DOUBLE,"
                " cost_basis DOUBLE, at_end BOOLEAN)")
    _db.executemany("INSERT INTO web_gains VALUES (?, 'src-1', 'taxable_personal', 'USD', 'ignore', ?, ?, ?, ?, ?)", [
        ("2026-08-01", 50.0, 200.0, 100.0, 400.0, True),
        ("2026-09-01", 30.0, 100.0, 300.0, 1000.0, True),
        ("2026-09-01", 70.0, None, None, None, True),
        ("2026-09-01", None, None, -20.0, 0.0, True)])
    check("the realized gain's twin is the gain over the cost basis sold, where the sale states both",
          _near(_run("Realized gain (privacy)")[0][0], 80 / 300), _run("Realized gain (privacy)"))
    check("...the unrealized gain's the gain at the window's end over the cost basis held there",
          _near(_run("Unrealized gain (privacy)")[0][0], 300 / 1000), _run("Unrealized gain (privacy)"))

if FAILS:
    print(f"provision tests: {FAILS} failed")
else:
    print("provision tests: all passed")
sys.exit(1 if FAILS else 0)
