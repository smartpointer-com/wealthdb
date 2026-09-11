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
    p.CURRENCY_FIELD_ID = p.FIELD_IDS[("report_returns", "currency")]
    p.SOURCE_FIELD_ID = p.FIELD_IDS[("report_returns", "silver_source_id")]


def model_ids():
    return {name: i for i, name in enumerate(p.report_models(), start=1)}


def all_cards(db_id, mid):
    """name -> (display, description, dataset_query) for every card
    provisioning defines, in the shape ensure_cards assembles them."""
    cards = {}
    for name, (display, desc, query) in p.metric_defs(db_id, mid).items():
        cards[name] = (display, desc, query)
    for name, (display, desc, query, _viz) in p.question_defs(db_id, mid).items():
        cards[name] = (display, desc, query)
    for name, (_t, display, desc, query, _viz) in p.privacy_card_defs(db_id, mid).items():
        cards[name] = (display, desc, query)
    return cards


def sql_of(query):
    """The native SQL of a card query, or None for an MBQL one."""
    return query["native"]["query"] if query.get("type") == "native" else None


# ---- the filter-field registry ----------------------------------------

section("filter-field registry (the two spending views)")
check("web_spending registered",
      "web_spending" in p.FILTER_FIELD_COLUMNS)
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
check("both spending views are in the wanted serving-view set",
      {"web_spending", "web_card_balances_history"} <= set(p.web_views_wanted()))

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
      "(VALUES ('USD'), ('CHF'), ('EUR')) AS c(currency)" in SPEND_SQL and
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
      all(f"nw.nw_{c}" in PCT_SQL for c in ("usd", "chf", "eur")))

# ---- the cards --------------------------------------------------------

CARDS = all_cards(1, MID)
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
      all("display_name" in json.dumps(CARDS[c][2])
          for c in ("Spend by account", "Largest transactions")))

# The two breakdowns are rings, on both dashboards. Every slice is drawn
# on the primary ring — Metabase's own small-slice bucket is called
# "Other", and so is a real category here — and neither ring carries a
# LIMIT: a limited ring renormalizes onto what it drew, so its printed
# shares would disagree with its own values.
_QDEFS = p.question_defs(1, MID)
_PDEFS = p.privacy_card_defs(1, MID)
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
check("the detailed rings do fold their tail (eighty-odd values)",
      all(_RINGS[n][1].get("pie.slice_threshold", 0) > 0
          for n in ("Spending by subcategory",
                    "Spending by subcategory (privacy)")))

# Card balances read the way an issuer states them: owed, positive. The
# flip is in the projection only — gold stores the liability negative.
for _name in ("Card balances over time", "Card balances over time (privacy)"):
    _sql = sql_of(CARDS[_name][2]) or ""
    check(f"'{_name}' states what is owed as a positive figure",
          "-balance_chf" in _sql and "-balance_eur" in _sql
          and "-balance_usd" in _sql, _sql)
check("the money card-balances chart is split by the account label",
      "account_label" in (sql_of(CARDS["Card balances over time"][2]) or ""))

# A card called "by month" charts months. Metabase infers the x-axis
# from cardinality, and there are more categories than months in any
# window worth charting, so both monthly cards pin their dimensions —
# the money one over the model's columns, the twin over its own SQL
# aliases. Without the pin the card draws its own transpose.
check("the monthly cards put the month on the x-axis, categories in the "
      "stack",
      _QDEFS["Spending by month"][3]["graph.dimensions"]
      == ["occurred_at", "spend_primary"]
      and _PDEFS["Spending by month (privacy)"][4]["graph.dimensions"]
      == ["month", "category"])
check("...as stacked bands over a continuous axis",
      all(d == "area" and v.get("stackable.stack_type") == "stacked"
          for d, v in ((_QDEFS["Spending by month"][0],
                        _QDEFS["Spending by month"][3]),
                       (_PDEFS["Spending by month (privacy)"][1],
                        _PDEFS["Spending by month (privacy)"][4]))))

# The merchant rankings rank merchants. A line resolved to a delta — a
# gift, a card bill, cash out of an ATM — is not a merchant
# transaction, and a card bill now carries the issuer it was paid to as
# its merchant (migration 0052), so a blank merchant no longer stands
# for "not a delta". Both rankings exclude the delta CATEGORIES — a
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
      "filter" not in CARDS["Largest transactions"][2]["query"])
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
check("currency offers the reporting trio",
      PARAMS["Currency"]["values_source_config"]["values"] == ["USD", "CHF", "EUR"])
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
check("other range dashboards keep just the range pair",
      [x["id"] for x in p.dashboard_parameters(MID, "range", "Wealth Overview")]
      == [p.TIME_PARAM_ID, p.SOURCE_PARAM_ID])

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
p.privacy_card_defs(1, MID)                 # rebuild the registry
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
check("the existing privacy charts still map as dimensions",
      all(t[0] == "dimension"
          for _pid, t in targets["Net worth over time (privacy)"]))

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
p.privacy_card_defs(1, MID)

# ---- a snapshot that predates migration 0043 --------------------------

section("missing serving views abort the run")


class FakeAPI:
    """A stand-in for provision.req: answers the metadata and view
    probes, and records every call so the test can assert what did NOT
    happen (a sync must not be attempted for a stale snapshot)."""

    def __init__(self, views, tables):
        self.views, self.tables, self.calls = views, tables, []

    def __call__(self, base, path, method="GET", data=None, session=None,
                 timeout=30):
        self.calls.append((method, path))
        if path.endswith("/metadata"):
            return 200, {"tables": self.tables}
        if path == "/api/dataset":
            if self.views is None:                      # probe unanswerable
                return 500, {}
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
GOLD_MIGRATIONS = (os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
                   + "/wealthdb/internal/gold/migrations")
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

# The fence is written the long way ON PURPOSE, in both the MBQL and the
# native form: a bare `!=` drops the rows whose account_kind is NULL — a
# transaction with no matching accounts row — and those are flows the chart
# is supposed to show. The NULL-keeping branch is the whole reason it is not
# a one-liner, so a future simplification has to fail here.
_CARD_QUERIES = "\n".join(json.dumps(q) for _d, _desc, q in CARDS.values())
check("the MBQL flow fence keeps rows with a NULL account_kind",
      '"is-null"' in _CARD_QUERIES,
      "no card spells the null branch; a bare != would drop unmatched rows")
check("the native flow fence keeps rows with a NULL account_kind",
      "account_kind IS NULL OR account_kind NOT IN" in _CARD_QUERIES,
      "the native form dropped its null branch")

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
    # would empty the two columns the picker does not select.
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

overview = [d for d in layouts if d.get("name") == "Wealth Overview"][0]
check("Wealth Overview is untouched by the spending pickers",
      all({m["parameter_id"] for m in dc["parameter_mappings"]} ==
          {p.TIME_PARAM_ID, p.SOURCE_PARAM_ID}
          for dc in overview["dashcards"] if dc["card_id"] is not None))

if FAILS:
    print(f"provision tests: {FAILS} failed")
else:
    print("provision tests: all passed")
sys.exit(1 if FAILS else 0)

section("the merchant ranking carries every currency")
MERCH = CARDS["Top 50 merchants"][2]["query"]
AGGS = MERCH.get("aggregation", [])
check("it sums one column per reporting currency",
      [a[2].get("display-name") for a in AGGS if a[0] == "aggregation-options"]
      == ["USD", "CHF", "EUR"])
check("each column carries its own currency predicate, so no row filter is needed",
      all(a[1][0] == "sum-where"
          and a[1][2] == ["=", ["field", "currency", {"base-type": "type/Text"}], ccy]
          for a, ccy in zip(AGGS, ("USD", "CHF", "EUR"))))
check("the ranking is by the USD column (aggregation 0)",
      MERCH.get("order-by") == [["desc", ["aggregation", 0]]]
      and AGGS[0][2]["display-name"] == "USD")
check("it no longer needs the standalone-currency caveat",
      "filter currency to a single" not in CARDS["Top 50 merchants"][1])
check("the Currency picker is not wired to it — a row filter would empty "
      "the two columns it does not select",
      "Top 50 merchants" in p.SPEND_ALL_CURRENCY_CARDS)
