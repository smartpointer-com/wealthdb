#!/usr/bin/env python3
"""Unit tests for web/provision.py's pure definition helpers — no
Metabase and no Docker required. Run via `make test-web` (web/test_web.sh
invokes this), or directly.

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
import io
import json
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
        old_schema = p.ensure_synced("b", "s", 1, tries=1, delay=0) is None
    check("a snapshot older than the cards' schema aborts the run", old_schema)
    check("...before reading the metadata or attempting a sync",
          [c for c in api.calls if c[1] == "/api/dataset"] == api.calls, str(api.calls))
    p.req = FakeAPI(ALL_VIEWS, tables_for(ALL_VIEWS + ["report_returns"]))
    p.req.version = "unreadable"
    with quiet():
        unknown = p.ensure_synced("b", "s", 1, tries=1, delay=0) is None
    check("a schema version the probe cannot read aborts too", unknown)

    # The invariant that abort protects: a metadata read that fails says
    # nothing about the schema, and must never be coerced to an empty table
    # list — that would resolve no field ids at all.
    p.req = FakeAPI(ALL_VIEWS, None)
    check("a failed metadata read is never coerced to an empty table list",
          p.gold_metadata("b", "s", 1) is None)
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
    Go file."""
    text = open(os.path.join(_CANONICAL, file), encoding="utf-8").read()
    return set(re.findall(rf'\w+\s+{type_name}\s*=\s*"([^"]+)"', text))


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
          ("report_transactions", "serving_transactions"),
          ("report_transactions_pct", "serving_transactions"))))
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
                     "Uncategorized income share", "Income by month",
                     "Income by type", "Top 50 payers", "Income by account",
                     "Largest receipts"]

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
    SELECT and the FROM/ORDER/GROUP that ends it.

    Subqueries are dropped first so a scan cannot land inside one, and
    ordinary function calls are kept so a name wrapped in one is still
    visible. A query with no top-level SELECT left yields nothing, which
    is a shape no card here has and which the vacuity guard below
    catches."""
    flat = _strip_subqueries(_strip_sql_comments(q))
    out = []
    for m in re.finditer(r"\bSELECT\b", flat, re.I):
        tail = flat[m.end():]
        end = re.search(r"\b(FROM|ORDER\s+BY|GROUP\s+BY|LIMIT|UNION)\b", tail, re.I)
        out.append(tail[:end.start()] if end else tail)
    return out


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
    _projs = _projections(_q)
    _bad = [c for c in _REDACTED
            if any(re.search(rf"\b{c}\b", pr) for pr in _projs)]
    if any(_star_projection(pr) for pr in _projs):
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
check("...and it does not read a commented-out one",
      _projections("-- SELECT payer_name\nSELECT rank FROM m") == [" rank "],
      _projections("-- SELECT payer_name\nSELECT rank FROM m"))
# A name wrapped in a function call is still a name. Dropping every
# parenthesised group would delete the argument along with the parens
# and see an empty projection.
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
check("the Income dashboard carries five pickers",
      _income_pickers == ["currency", "time_range", "source", "account", "type"],
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
check("the Cash Flow dashboard carries five pickers and no account one",
      _cf_pickers == ["currency", "investing", "time_range", "source", "section"],
      _cf_pickers)
check("the Cash Flow twin carries the same five",
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
                if any(_star_projection(pr) or re.search(rf"\b{_col}\b", pr)
                       for pr in _projections(q))]
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

section("stacked area charts")

# Metabase stacks the band whose rows appear last at the bottom and, by
# default, reverses a stacked chart's legend. So every stacked area with
# bands ranks its rows smallest first, which puts the largest band on
# the axis, and switches the reversal off, which lists the largest first.
# The Exposure charts over time rank with an aggregate of their own (see
# over_time); the Exposure section checks them.
_stacked_areas = {n: sql_of(q) or "" for n, (d, _desc, q) in CARDS.items()
                  if d == "area" and VIZ[n].get("stackable.stack_type") == "stacked"
                  and len(VIZ[n].get("graph.dimensions", [])) == 2}
check("every stacked area with bands ranks its rows by band",
      bool(_stacked_areas) and all("series_rank" in q or "AS latest FROM e" in q for q in _stacked_areas.values()),
      [n for n, q in _stacked_areas.items() if "series_rank" not in q and "AS latest FROM e" not in q])
check("...and lists its legend in the bands' own order",
      all(VIZ[n].get("legend.is_reversed") is False for n in _stacked_areas),
      [n for n in _stacked_areas if VIZ[n].get("legend.is_reversed") is not False])
# A stack of two metrics is the mirror case: the first metric sits at
# the bottom, so positions, the larger, come first and the legend reads
# the same way.
check("the cash and positions stacks list positions first, legend unreversed",
      all(VIZ[n]["graph.metrics"][0].startswith("positions") and VIZ[n].get("legend.is_reversed") is False
          for n in ("Cash vs positions over time", "Cash vs positions over time (privacy)")))
check("...ending in that order and nothing after it",
      all(q.rstrip().endswith(("NULLS FIRST, month", "NULLS FIRST, as_of_day")) or "AS latest FROM e" in q
          for q in _stacked_areas.values()))

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
check("every Exposure tile is native over web_exposure",
      all("FROM web_exposure" in q for q in _ex_sql.values()),
      [n for n, q in _ex_sql.items() if "FROM web_exposure" not in q])
# A row set per currency: a read without the row filter would sum them.
check("...and every read of it is held to the picked currency",
      all(q.count("FROM web_exposure") == q.count("AND currency = {{currency}}")
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
          and "OVER (PARTITION BY as_of_day)" in q for q in _ex_sql.values()),
      [n for n, q in _ex_sql.items() if "{{asset_class}}" not in q])
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
      all(q.count("FROM web_exposure") == q.count("AND currency = {{currency}}") for q in _ex_twin.values()))
check("the base breakdowns show the value beside the share, the charts over time shares alone",
      all(("sum(value) AS value" in q) == (CARDS[n][0] != "area") for n, q in _ex_sql.items()))
# A reader wants the shares first: every bar plots a share, with its
# figure on the bar and a percent sign; the base card keeps the value for
# the tooltip, where the privacy card has none to show.
_ex_bars = {n: VIZ[n] for n in _ex_tiles if n in CARDS and CARDS[n][0] in ("bar", "row")}
check("every Exposure bar plots a share, labelled, with the value in its tooltip",
      bool(_ex_bars) and all(
          v.get("graph.metrics") in (["share_pct"], ["class_share_pct"])
          and v.get("graph.show_values") is True
          and v.get("graph.tooltip_columns") == ["value"]
          and any(c.get("suffix") == "%" for c in v.get("column_settings", {}).values())
          for v in _ex_bars.values()),
      [n for n, v in _ex_bars.items() if v.get("graph.tooltip_columns") != ["value"]])
check("...and its privacy card plots the share with no value to show",
      all(VIZ[n].get("graph.metrics") in (["share_pct"], ["class_share_pct"])
          and "graph.tooltip_columns" not in VIZ[n]
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

# The headline figures stay on the latest snapshot, the figure
# `wealthdb holdings sources` prints, with the time filter on it.
for _c in ("Net worth", "Positions value", "Cash balance",
           "Net worth (privacy)", "Positions value (privacy)",
           "Cash balance (privacy)"):
    _q = CARDS[_c][2]
    check(f"'{_c}' reads each source's latest snapshot",
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
_go = open(os.path.join(_REPO, "wealthdb", "internal", "gold",
                        "returns_materialize.go"), encoding="utf-8").read()
_go_ccys = re.search(r"var materializeCurrencies = \[\]string\{([^}]*)\}", _go)
check("the returns materializer writes exactly the reporting currencies",
      _go_ccys is not None and
      re.findall(r'"([A-Z]{3})"', _go_ccys.group(1)) == list(p.REPORTING_CURRENCIES),
      _go_ccys.group(1) if _go_ccys else "materializeCurrencies not found")
# The readings of a missing cost basis are one contract too: the Go
# vocabulary (lots.MissingBasisReadings) and the Gains picker here.
_lots = open(os.path.join(_REPO, "wealthdb", "internal", "lots", "types.go"), encoding="utf-8").read()
_go_readings = re.search(r"var MissingBasisReadings = \[\]MissingBasis\{([^}]*)\}", _lots)
_consts = dict(re.findall(r'(Missing\w+)\s+MissingBasis = "(\w+)"', _lots))
check("the Gains picker offers exactly the engine's readings of a missing cost basis",
      _go_readings is not None and
      [_consts.get(n.strip()) for n in _go_readings.group(1).split(",")] == list(p.MISSING_BASIS_READINGS),
      _go_readings.group(1) if _go_readings else "MissingBasisReadings not found")


def _latest_definition(name):
    """The text of the last migration statement that (re)defines `name`."""
    found = re.findall(rf"CREATE OR REPLACE (?:VIEW|MACRO) {name}\b.*?;\n",
                       MIGRATION_SQL, re.S)
    return found[-1] if found else ""


# web_gains and web_exposure are the long views: a row set per currency,
# written by their materializers from the same materializeCurrencies
# list.
_LONG_VIEWS = {"web_gains", "web_exposure"}
_missing = {v: [c for c in p.REPORTING_CURRENCIES
                if not re.search(rf"_{c.lower()}\b", _latest_definition(v))]
            for v in p.web_views_wanted() if v not in _LONG_VIEWS}
check("every wide serving view carries a value column per reporting currency",
      not any(_missing.values()), {v: m for v, m in _missing.items() if m})
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

if FAILS:
    print(f"provision tests: {FAILS} failed")
else:
    print("provision tests: all passed")
sys.exit(1 if FAILS else 0)
