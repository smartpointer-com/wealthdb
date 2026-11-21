#!/usr/bin/env python3
"""Provision a fresh Metabase over its loopback REST API: create the
admin account (skipping the "tell us about yourself" setup wizard),
pre-add the gold DuckDB database, and create the pre-defined report
models, questions and dashboards. Idempotent — safe to run on
every `wealthdb web start`. Standard library only.

Called by web/web; not meant to be run by hand (but it can be). The
admin password arrives in WEALTHDB_WEB_ADMIN_PASSWORD, never on the
command line, where `ps` would show it.

Exit status: 0 when provisioned, EXIT_STALE_SNAPSHOT (3) when the
mounted gold snapshot is older than the cards need (MIN_GOLD_SCHEMA),
EXIT_UNVERIFIED (4) when setup was done before and the admin login
fails, so nothing is checked or changed, and 1 on any other failure.
Both 0 and 3 mean that the server reads the file --gold-path names.

With --dump FILE it opens no connection: it writes every model, card
and dashboard payload a provision into an empty collection sends, with
ids numbered in definition order, to FILE as JSON (payload_dump). Two
trees' dumps diff to what a change does to the payloads.
"""
import argparse
import concurrent.futures
import json
import os
import re
import sys
import time

import metabase
import sqlkit
from sqlkit import (_ccy_slash, default_currency, HORIZONS, MISSING_BASIS_READINGS, _native, privacy_name,
                    PRIVACY_SUFFIX, _sql_list)
from colors import (account_source_colors, load_source_colors, save_source_colors, with_source_colors)
from retired import (RETIRED_CARD_NAMES, RETIRED_DASHBOARD_NAMES, RETIRED_MODEL_NAMES)
from models import (report_models)
from pickers import (BENCHMARK_PARAM_ID, INLINE_PICKERS)
from dashboards import (card_defs, dashboard_defs, dashboard_parameters, NO_PRIVACY_TWIN)

# The names demo/check_dashboards.py reads here. A run setting
# (sqlkit.DEFAULT_CURRENCY and the like) is read and set in the module
# that owns it, never here.
from dashboards import (BENCHMARK_CARDS, INCOME_DASHBOARDS)  # noqa: F401


def wait_health(client, limit=270, delay=0.25):
    """Polls Metabase's health endpoint until it answers, for at most
    `limit` seconds; once it answers, prints how long the wait took. A
    short poll: a restarted server answers within seconds of coming up,
    and each poll is one local request."""
    start = time.monotonic()
    while time.monotonic() - start < limit:
        st, _ = client.get("/api/health", timeout=5)
        if st == 200:
            print(f"provision: timing: wait for Metabase {time.monotonic() - start:.3f}s", file=sys.stderr)
            return True
        time.sleep(delay)
    return False


def main(client=None):
    """Provision the Metabase --base names, or `client`, the tests' stand-in."""
    ap = argparse.ArgumentParser()
    ap.add_argument("--base")
    ap.add_argument("--email")
    ap.add_argument("--gold-path")
    ap.add_argument("--spill-dir", default="",
                    help="the directory DuckDB spills under, one subdirectory per --gold-path")
    ap.add_argument("--db-name", default="gold")
    ap.add_argument("--default-currency", default="USD")
    ap.add_argument("--missing-basis", default=MISSING_BASIS_READINGS[0], choices=MISSING_BASIS_READINGS)
    ap.add_argument("--source-colors", default="",
                    help="the file that keeps each source's colour (SOURCE_PALETTE)")
    ap.add_argument("--dump", metavar="FILE",
                    help="write the payloads to FILE as JSON and exit, with no connection;"
                         " --base, --email and --gold-path are then not needed")
    ap.add_argument("--withholding", default="yes", choices=DUMP_WITHHOLDING,
                    help="with --dump: whether the snapshot holds a withholding table"
                         " (unknown: the probe did not answer)")
    ap.add_argument("--sources", default="", metavar="A,B",
                    help="with --dump: the sources that hold an account, in load order")
    a = ap.parse_args()
    missing = [f for f, v in (("--base", a.base), ("--email", a.email), ("--gold-path", a.gold_path))
               if v is None]
    if missing and not a.dump:
        ap.error("the following arguments are required: " + ", ".join(missing))
    sqlkit.DEFAULT_CURRENCY = default_currency(a.default_currency)
    sqlkit.DEFAULT_MISSING_BASIS = a.missing_basis
    if a.dump:
        dump = payload_dump(DUMP_WITHHOLDING[a.withholding], [s for s in a.sources.split(",") if s])
        with open(a.dump, "w", encoding="utf-8") as f:
            f.write(json.dumps(dump, sort_keys=True, indent=1, ensure_ascii=False) + "\n")
        return 0
    password = os.environ.get("WEALTHDB_WEB_ADMIN_PASSWORD", "")
    if not password:
        print("provision: WEALTHDB_WEB_ADMIN_PASSWORD is not set", file=sys.stderr)
        return 1

    client = client or metabase.Client(a.base)
    if not wait_health(client):
        print("provision: Metabase did not become healthy in time", file=sys.stderr)
        return 1

    _, props = client.get("/api/session/properties")
    setup_done = bool(props.get("has-user-setup"))
    token = props.get("setup-token")

    if not setup_done and token:
        st, body = client.post("/api/setup", {
            "token": token,
            "user": {"first_name": "wealthdb", "last_name": "admin",
                     "email": a.email, "password": password},
            "prefs": {"site_name": "wealthdb", "allow_tracking": False},
        })
        if st not in (200, 201) or not body.get("id"):
            print(f"provision: setup failed ({st}): {body.get('message')}", file=sys.stderr)
            return 1
        print(f"provision: created admin {a.email}; setup wizard skipped")
    else:
        print("provision: already set up; skipping admin creation")

    # Log in to add the database. If setup was done earlier with a
    # different password (changed since in Metabase), don't fail the
    # start — the database was added on the first provision.
    _, body = client.post("/api/session", {"username": a.email, "password": password})
    sid = body.get("id")
    if not sid:
        if setup_done:
            print("provision: couldn't verify admin login (password changed?) — leaving as-is")
            return EXIT_UNVERIFIED
        print(f"provision: login failed: {body.get('message')}", file=sys.stderr)
        return 1

    client = client.with_auth(session=sid)
    db_id = ensure_database(client, a.db_name, a.gold_path, a.spill_dir)
    if db_id is None:
        return 1
    if not reads_gold_path(client, db_id, a.gold_path):
        return 1

    status = check_gold_schema(client, db_id)
    if status:
        return status
    tables = ensure_synced(client, db_id)
    if tables is None:
        return 1
    field_ids = field_ids_from(tables)
    unfingerprint_lists(client, tables)

    coll_id = ensure_collection(client, COLLECTION_NAME)
    if coll_id is None:
        return 1
    by_name = pre_defined_cards(client, coll_id)
    if by_name is None:
        return 1
    model_ids = ensure_models(client, db_id, coll_id, by_name)
    if model_ids is None:
        return 1
    colors = source_colors(client, db_id, a.source_colors)
    saved = ensure_cards(client, db_id, coll_id, by_name, field_ids, model_ids, colors)
    if saved is None:
        return 1
    card_ids, descs = saved
    titles = tile_titles(withholding_table(client, db_id))
    return ensure_dashboards(client, coll_id, card_ids, model_ids, titles, descs)


# --dump's --withholding: what the snapshot's withholding probe answers
# (withholding_table), unknown standing for a probe that did not answer.
DUMP_WITHHOLDING = {"yes": True, "no": False, "unknown": None}


def offline_field_ids():
    """(table, column) -> field id for every column of
    filter_field_columns, numbered from 1 in its order: the ids a sync
    would resolve, for a run with no Metabase."""
    cols = [(t, c) for t, names in filter_field_columns().items() for c in names]
    return {key: i for i, key in enumerate(cols, start=1)}


def payload_dump(withholding, sources):
    """{"models", "cards", "dashboards"}: name -> payload for every
    model, question and dashboard that a provision into an empty
    collection sends, built without a connection, and "ids": the id of
    each of them and of each field ("table.column"), which the payloads
    refer to. The database and the collection are 1, fields are numbered
    by offline_field_ids, models then cards from 1 in the order they are
    saved, dashboards from 1 in definition order. `withholding` is what
    withholding_table answers, `sources` the snapshot's sources that
    hold an account (source_colors with no kept file). A dashboard's
    payload is its full layout PUT."""
    field_ids = offline_field_ids()
    models = model_payloads(1, 1)
    model_ids = {name: i for i, name in enumerate(models, start=1)}
    cards = card_payloads(1, 1, field_ids, model_ids, account_source_colors({}, sources))
    card_ids = {name: i for i, name in enumerate(cards, start=len(models) + 1)}
    defs = dashboard_defs(card_defs())
    dash_ids = {name: i for i, name in enumerate(defs, start=1)}
    descs = {name: body["description"] for name, body in cards.items()}
    payloads = dashboard_payloads(defs, card_defs(), model_ids, card_ids, dash_ids,
                                  tile_titles(withholding), descs)
    dashboards = {name: {"name": name, "description": desc, "parameters": parameters,
                         "dashcards": dashcards}
                  for name, (desc, parameters, dashcards) in payloads.items()}
    return {"models": models, "cards": cards, "dashboards": dashboards,
            "ids": {"models": model_ids, "cards": card_ids, "dashboards": dash_ids,
                    "fields": {f"{t}.{c}": i for (t, c), i in field_ids.items()}}}


# The collection the pre-defined items live in, apart from hand-built ones.
COLLECTION_NAME = "wealthdb (pre-defined)"

EXCESS_CARDS = ("Excess return (TWR)", "Excess return (MWR)", "Excess annualized return (TWR)")


def source_colors(client, db_id, path):
    """Each source's colour: the kept assignment, with the snapshot's
    sources that hold an account added in the order gold first loaded
    them, then `declared`. A reference source, such as FX rates, holds
    no account and draws no series. A probe that does not answer adds
    none, and the cards keep the colours already kept."""
    kept = load_source_colors(path) if path else {}
    rows = probe_rows(client, db_id,
                      "SELECT silver_source_id FROM silver_sources"
                      " WHERE silver_source_id IN (SELECT silver_source_id FROM accounts)"
                      " ORDER BY first_loaded_at, silver_source_id")
    colors = account_source_colors(kept, None if rows is None else
                                   [r[0] for r in rows if r and r[0]])
    if path and colors != kept:
        save_source_colors(path, colors)
    return colors


def dashboard_tile_titles(base, cards):
    """card name -> the title its tiles show on base dashboard `base` and
    its twin, of `cards` (card_defs). The twin's title takes the privacy
    form, unless the twin shows the card itself (an exempt card). A title is set for three
    kinds of card: a borrowed card whose name leans on its home
    dashboard's pickers, a card whose title is another card's name, and
    an Excess figure, which reads the benchmark's months and not the
    Return tiles' window."""
    titles = {"Wealth Overview": {"Income expected": f"Income expected, {HORIZONS[0]} months",
                                  "Asset class mix over time": "Asset classes over time"},
              "Returns": {**{c: f"{c}, same months" for c in EXCESS_CARDS[:2]},
                          EXCESS_CARDS[2]: "Excess annualized (TWR), same months"}}.get(base, {})
    return {**titles, **{privacy_name(c): privacy_name(t) for c, t in titles.items()
                         if not cards[c].exempt}}


def gold_metadata(client, db_id, tries=3, delay=2):
    """The synced gold tables metadata, with a short retry. A transient
    failure here must fail the run loudly rather than read as a gold with
    no columns, whose cards no field id could bind. Returns the tables
    list, or None when the metadata stays unreadable."""
    for _ in range(tries):
        st, meta = client.get(f"/api/database/{db_id}/metadata")
        if st == 200 and isinstance(meta, dict):
            return meta.get("tables", [])
        time.sleep(delay)
    return None


def filter_field_columns():
    """table -> the columns of it the cards' field filters bind, for
    every table a card filters and every web_* serving view a card or a
    model reads, a view no filter binds with none; tables and columns
    sorted. It is read off the definitions: the field-id resolution and
    the startup probe read it, so a view or a filter a card gains is
    resolved and probed as it is defined."""
    native = [c.query["native"] for c in card_defs().values()]
    columns = {}
    for tag in (t for n in native for t in n["template-tags"].values()):
        if tag["type"] == "dimension":
            _field, table, col = tag["dimension"]
            columns.setdefault(table, set()).add(col)
    sql = "\n".join([n["query"] for n in native] + [query for query, _desc in report_models().values()])
    for view in re.findall(r"\b(?:FROM|JOIN) (web_\w+)\b", sql):
        columns.setdefault(view, set())
    return {table: tuple(sorted(columns[table])) for table in sorted(columns)}


def field_ids_from(tables):
    """{(table, column): field id} for every column named in
    filter_field_columns present in `tables` — the ids behind the native
    cards' field filters, which render the dashboard pickers as
    dropdowns / date widgets (a plain template variable would be a
    free-text box instead)."""
    wanted = filter_field_columns()
    ids = {}
    for t in tables:
        cols = wanted.get(t.get("name"))
        if not cols:
            continue
        for f in t.get("fields", []):
            if f.get("name") in cols:
                ids[(t["name"], f["name"])] = f.get("id")
    return ids


def list_fields(tables):
    """The id and name of every list column in `tables` that Metabase
    would still fingerprint: a DuckDB list, whose type ends in `[]`. A
    marked field leaves the metadata, so a later run finds nothing to
    mark, and no run here undoes a mark."""
    return [(f["id"], f"{t.get('name')}.{f.get('name')}") for t in tables for f in t.get("fields", [])
            if (f.get("database_type") or "").endswith("[]") and f.get("visibility_type") != "sensitive"]


def unfingerprint_lists(client, tables):
    """Keep Metabase's field sync off the gold's list columns. Its
    fingerprint reads a table in one query that cuts each text column to
    a prefix with substring(). The DuckDB driver reports a list as text,
    and DuckDB has no substring() for a list, so the whole table's
    fingerprint fails, every hour. Metabase never fingerprints a field
    marked `sensitive`, and such a field leaves its table browser and
    query builder. The native cards still read the column; no model
    or picker binds one. A field that cannot be marked leaves only the
    warning, so it is reported, not fatal."""
    marked = 0
    for fid, name in list_fields(tables):
        st, body = client.put(f"/api/field/{fid}", {"visibility_type": "sensitive"})
        if st == 200:
            marked += 1
        else:
            print(f"provision: could not keep {name} from fingerprinting ({st}): {body.get('message')}",
                  file=sys.stderr)
    if marked:
        print(f"provision: {marked} list column(s) kept from fingerprinting")


def missing_filter_columns(tables):
    """The filter_field_columns tables not fully synced in `tables` —
    checked per COLUMN, not per table: Metabase's sync can expose a
    table before its fields, and a provision that ran in that gap would
    wire zero filters."""
    ids = field_ids_from(tables)
    return sorted({t for t, cols in filter_field_columns().items()
                   for c in cols if (t, c) not in ids})


def web_views_wanted():
    """The web_* serving views the cards and models read
    (filter_field_columns)."""
    return [t for t in filter_field_columns() if t.startswith("web_")]


def probe_rows(client, db_id, sql):
    """The rows native `sql` returns through the driver, or None when the
    probe does not answer. Metabase answers a query that fails with 202
    as well, its status `failed` and an error beside empty rows: that is
    no answer, not an answer of no rows."""
    st, res = client.post("/api/dataset", {"type": "native", "database": db_id,
                                           "native": {"query": sql, "template-tags": {}}})
    if st != 202 or res.get("status") == "failed" or "error" in res:
        return None
    return (res.get("data") or {}).get("rows")


def missing_web_views(client, db_id):
    """The wanted web_* serving views the mounted gold snapshot does not
    carry, probed through the driver itself. A snapshot predating the
    schema version that adds one cannot be fixed by a Metabase schema sync —
    only `wealthdb web refresh` re-materializes and re-snapshots gold.

    Probed BY NAME rather than by counting web_* views: a count says
    nothing about WHICH views are there, so a snapshot missing one would
    pass the moment gold grew any other web_* view.
    A probe that fails to answer reports everything missing, which halts
    provisioning — the conservative reading, since the alternative is a
    sync no snapshot without the view can complete."""
    want = web_views_wanted()
    rows = probe_rows(client, db_id,
                      "SELECT view_name FROM duckdb_views() "
                      f"WHERE NOT internal AND view_name IN ({_sql_list(want)})")
    if rows is None:
        return want
    have = {r[0] for r in rows if r}
    return [t for t in want if t not in have]


# The oldest gold schema the cards can read. A test holds it at or above
# the newest schema version that defines a view, macro or table a card
# reads.
# DESIGN.md §6 says what each older snapshot lacks.
MIN_GOLD_SCHEMA = 227

# main()'s exit status for a snapshot older than MIN_GOLD_SCHEMA; every
# other failure exits 1. web/web tells the two apart, because only
# `wealthdb web refresh` mends this one and the browser wizard does not.
EXIT_STALE_SNAPSHOT = 3

# main()'s exit status when setup was done before and the admin login
# fails. It leaves everything as it was and confirms nothing, so web/web
# cannot take it for a server that reads the new snapshot.
EXIT_UNVERIFIED = 4

# SERVED_PATH_PROBE asks for the file the server's DuckDB has open.
SERVED_PATH_PROBE = "SELECT path FROM duckdb_databases() WHERE database_name = current_database()"


def reads_gold_path(client, db_id, gold_path):
    """Whether the server's connection reads `gold_path`, probed through
    the driver. The driver's DuckDB keeps one open database per path, and
    Metabase drops its connections when the database's settings change,
    so a new path is opened afresh. web/web gives each snapshot a path of
    its own; this is the check that the server took it up."""
    rows = probe_rows(client, db_id, SERVED_PATH_PROBE)
    served = rows[0][0] if rows and rows[0] else None
    if served == gold_path:
        return True
    reads = f"reads {served}, not {gold_path}" if served else "does not say which file it reads"
    print(f"provision: the gold database {reads}; leaving the existing cards untouched",
          file=sys.stderr)
    return False


def gold_schema_version(client, db_id):
    """The mounted gold snapshot's schema version, probed through the
    driver, or None when the probe does not answer."""
    rows = probe_rows(client, db_id, "SELECT max(gold_schema_version) FROM schema_meta")
    try:
        return int(rows[0][0])
    except (TypeError, IndexError, ValueError):
        return None


def check_gold_schema(client, db_id):
    """0 when the mounted gold snapshot is at MIN_GOLD_SCHEMA or newer,
    else the status main() exits with: EXIT_STALE_SNAPSHOT for an older
    snapshot, 1 when the version probe does not answer. Checked before
    anything is synced or converged: an older snapshot carries its views
    by name but not what the cards read from them, so every card would
    converge onto SQL that fails or reads them wrong."""
    version = gold_schema_version(client, db_id)
    if version is None:
        print("provision: cannot read the gold snapshot's schema version; "
              "leaving the existing cards untouched", file=sys.stderr)
        return 1
    if version < MIN_GOLD_SCHEMA:
        print(f"provision: the gold snapshot is at schema {version} and the "
              f"cards need {MIN_GOLD_SCHEMA} — run `wealthdb web refresh` to "
              "re-snapshot a migrated gold; leaving the existing cards "
              "untouched", file=sys.stderr)
        return EXIT_STALE_SNAPSHOT
    return 0


# WITHHOLDING_PROBE asks whether the snapshot holds any withholding rate,
# a wrapper's or an account's.
WITHHOLDING_PROBE = ("SELECT (SELECT count(*) FROM yield_withholding)"
                     " + (SELECT count(*) FROM yield_withholding_accounts) > 0")


def withholding_table(client, db_id):
    """Whether the mounted gold snapshot holds a withholding table with
    any rate in it (yield_withholding and yield_withholding_accounts, the
    config's yield.withholding), probed through the driver; None when the
    probe does not answer."""
    rows = probe_rows(client, db_id, WITHHOLDING_PROBE)
    try:
        return bool(rows[0][0])
    except (TypeError, IndexError):
        return None


# The Withholding tiles' titles where the snapshot holds no withholding
# table: the tiles then read the tax booked on the income's accounts
# alone, and nothing for the income expected.
NO_WITHHOLDING_TABLE_TITLES = {"Withholding": "Received withholding — no withholding table",
                               "Income after withholding": "Income after received withholding"}


def tile_titles(table):
    """card name -> the title its tiles show in place of the card's name,
    on the base dashboards and the twins: the Withholding tiles' where
    `table` is False, the snapshot holding no withholding table. A probe
    that did not answer, None, keeps the cards' names."""
    if table is not False:
        return {}
    titles = dict(NO_WITHHOLDING_TABLE_TITLES)
    titles.update({privacy_name(c): privacy_name(t) for c, t in NO_WITHHOLDING_TABLE_TITLES.items()})
    return titles


def ensure_synced(client, db_id, tries=20, delay=3):
    """Make every column behind filter_field_columns available: when
    some are missing from the synced metadata, either Metabase simply
    has not synced the new gold DDL yet (trigger a sync, wait bounded)
    or the snapshot predates a serving view the cards read — which no
    sync can fix. Returns the tables metadata to resolve field ids from,
    or None when provisioning must not proceed: metadata unreadable, a
    stale snapshot that would break the view-backed models, or a sync
    that was still incomplete when the budget ran out. All three end the
    same way, because a card whose filter has no field id cannot be
    bound (bind). main() has already refused a snapshot older than
    MIN_GOLD_SCHEMA (check_gold_schema)."""
    tables = gold_metadata(client, db_id)
    if tables is None:
        print("provision: cannot read the gold metadata — aborting before "
              "converging cards to a degraded shape", file=sys.stderr)
        return None
    gone = missing_filter_columns(tables)
    if not gone:
        # The columns are already there. A sync would not change them.
        print("provision: schema sync skipped")
        return tables
    absent = missing_web_views(client, db_id)
    if absent:
        print("provision: the gold snapshot is missing serving views the "
              f"cards read ({', '.join(absent)}) — run `wealthdb web "
              "refresh` to re-snapshot a migrated gold; leaving the "
              "existing cards untouched", file=sys.stderr)
        return None
    print(f"provision: syncing gold schema (missing: {', '.join(gone)})")
    client.post(f"/api/database/{db_id}/sync_schema", {})
    for _ in range(tries):
        time.sleep(delay)
        latest = gold_metadata(client, db_id)
        # A read that failed says nothing about the sync; only overwrite on a
        # successful one, so `tables` is always the newest metadata actually
        # read — at worst the pre-sync one. Coercing a failed last read to []
        # would read every filter column as missing.
        if latest is None:
            continue
        tables = latest
        if not missing_filter_columns(tables):
            return tables
    # The budget ran out with columns still missing. A card whose filter
    # has no field id cannot be bound (bind), so the run stops here, as
    # the two guards above stop it, before any card is sent.
    print("provision: gold schema sync did not complete in time (still "
          f"missing: {', '.join(missing_filter_columns(tables))}) — leaving "
          "the existing cards untouched; re-run `wealthdb web start` once "
          "Metabase has finished syncing, or `wealthdb web refresh` to "
          "re-snapshot a migrated gold", file=sys.stderr)
    return None


def ensure_database(client, db_name, gold_path, spill_dir=""):
    """Add the gold DuckDB connection, or converge an existing one's
    settings. Returns the database id, or None on failure.

    A new `gold_path` is a change of settings, and Metabase then closes
    the connections it holds, so the next query opens the new file.

    DuckDB runs in-process in the Metabase JVM; without a cap it helps
    itself to 80% of the machine's RAM and the kernel OOM-kills the JVM
    when several history-heavy dashboard tiles query concurrently. Both
    keys land as instance-level DuckDB config (the driver forwards
    unknown detail keys as JDBC properties); threads is capped because
    peak memory scales with per-query parallelism.

    The cap is INSTANCE-level, so every tile of a dashboard shares it,
    and a dashboard opens by firing all of its tiles at once — so it is
    sized against the busiest page rather than against one query. Each
    tile reads a serving table, not a report
    macro, so a tile's memory grows with the rows it keeps, not with the
    length of the history. Four GB leaves a page room to gain tiles; a
    pool too small surfaces as "There was a problem displaying this
    chart" on most of the page rather than as anything naming memory.

    Lowering `threads` instead does NOT trade off the same way — it
    makes matters worse, because a query that cannot parallelise holds
    its intermediates longer. Spill goes to a directory of its own
    under `spill_dir`, which web/web mounts writable, named after the
    file: the driver's own "<database_file>.tmp" sits on the read-only
    snapshot mount. The setting travels in the driver's additional
    options, which it forwards as instance-level JDBC config like the two
    keys above. One directory per file: while the old file's DuckDB
    closes, it deletes the spill files in its directory, and the new
    file's DuckDB may spill already. Without `spill_dir` the database
    keeps the spill path it has, the driver's own where it has none. Do NOT move these into init_sql: that runs per
    connection, and DuckDB refuses to re-SET a used temp_directory,
    which breaks every query after the first connection cycle (""
    converges the key away from older provisions)."""
    details = {"database_file": gold_path, "read_only": True,
               "memory_limit": "4GB", "threads": "8", "init_sql": ""}
    if spill_dir:
        spill = os.path.join(spill_dir, os.path.basename(gold_path) + ".tmp")
        details["additional-options"] = f"temp_directory={spill}"

    db_id, cur = None, None
    st, dbs = client.get("/api/database")
    if st != 200:
        # Read as empty, a failed listing would add the database twice.
        print(f"provision: could not list databases ({st}): {dbs.get('message')}",
              file=sys.stderr)
        return None
    for d in (dbs.get("data") or []):
        if d.get("name") == db_name and d.get("engine") == "duckdb":
            db_id, cur = d.get("id"), (d.get("details") or {})
            break

    if db_id is None:
        st, body = client.post("/api/database", {
            "engine": "duckdb", "name": db_name, "details": details,
        })
        if st in (200, 201) and body.get("id"):
            print(f"provision: added DuckDB database '{db_name}' -> {gold_path}")
            return body["id"]
        print(f"provision: failed to add database ({st}): {body.get('message')}",
              file=sys.stderr)
        return None
    if any(cur.get(k) != v for k, v in details.items()):
        st, body = client.put(f"/api/database/{db_id}", {"details": {**cur, **details}})
        if st not in (200, 201):
            print(f"provision: could not update database settings ({st}): "
                  f"{body.get('message')}", file=sys.stderr)
            return None
        if cur.get("database_file") != gold_path:
            print(f"provision: database '{db_name}' now reads {gold_path}")
        else:
            print(f"provision: database '{db_name}' present; connection "
                  "settings converged (memory limit, spill dir)")
        return db_id
    print(f"provision: database '{db_name}' already present")
    return db_id


def ensure_collection(client, name):
    """The id of the collection named `name`, created if absent, or None
    on failure.

    There is no fallback to the root collection: card and dashboard
    matching is scoped to this collection, so provisioning into the root
    one would update or archive any hand-built item there that shares a
    pre-defined name. A listing that fails aborts too, since reading it
    as empty would create a second collection of the same name."""
    try:
        found = client.collection(name)
    except metabase.Error as e:
        print(f"provision: could not list collections ({e.status})", file=sys.stderr)
        return None
    if found is not None:
        return found.get("id")
    st, body = client.post("/api/collection", {"name": name})
    if st in (200, 201) and body.get("id"):
        print(f"provision: created collection '{name}'")
        return body["id"]
    print(f"provision: could not create collection '{name}' ({st}): "
          f"{body.get('message')}", file=sys.stderr)
    return None


def pre_defined_cards(client, coll_id):
    """name -> card, for the non-archived cards in the pre-defined
    collection, or None when the card list is unreadable — read as
    empty, it would create every card a second time. Scoped to that
    collection so a hand-built card that shares a name is never
    touched."""
    st, cards = client.get("/api/card")
    if st != 200 or not isinstance(cards, list):
        print(f"provision: could not list cards ({st})", file=sys.stderr)
        return None
    return {c.get("name"): c for c in cards
            if c.get("collection_id") == coll_id and not c.get("archived")}


def _empty(value):
    return value is None or value == [] or value == {}


def _typed_field_ref(got, want):
    """True when `got` is the field reference `want` with the base type
    Metabase adds to a template tag's dimension on save."""
    return (isinstance(got, list) and isinstance(want, list)
            and len(got) == len(want) == 3 and want[0] == "field"
            and got[:2] == want[:2] and want[2] is None
            and isinstance(got[2], dict) and set(got[2]) == {"base-type"})


def _same_value(got, want):
    """True when the server's value equals the payload's. A key on one
    side only counts, so a setting the payload drops is sent. Metabase
    stores an empty list or map as null, or leaves it out, and adds a
    base type to a template tag's field reference: those match."""
    if _empty(got) and _empty(want):
        return True
    if isinstance(want, dict):
        if not isinstance(got, dict):
            return False
        return all(_same_value(got.get(key), want.get(key)) for key in got.keys() | want.keys())
    if isinstance(want, list):
        if _typed_field_ref(got, want):
            return True
        if not isinstance(got, list) or len(got) != len(want):
            return False
        return all(_same_value(item, value) for item, value in zip(got, want, strict=True))
    return got == want


def card_same(existing, payload):
    """True when the stored card already is the payload. `existing`
    must carry its query in the legacy form the payload uses
    (stored_cards). A missing field is not a match."""
    if not isinstance(existing, dict) or existing.get("id") is None:
        return False
    for key, want in payload.items():
        if key not in existing or not _same_value(existing[key], want):
            return False
    return True


def _get_all(client, paths):
    """key -> the body of GET `path`, for each key -> path in `paths`,
    read in parallel. A read that fails or answers anything but a JSON
    object is left out, so the caller sends that object."""
    def fetch(item):
        key, path = item
        try:
            st, body = client.get(path)
        except Exception:
            # A body that is no JSON, or a read cut short.
            return key, None
        return key, body if st == 200 and isinstance(body, dict) else None

    with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
        return {key: body for key, body in pool.map(fetch, paths.items()) if body}


def stored_cards(client, by_name, names):
    """name -> the server's card, for each of `names` already in
    `by_name`. The card list carries each query in MBQL 5, which no
    payload equals, so every card is read again in the legacy form.
    A card that cannot be read is left out, and so is sent."""
    return _get_all(client, {name: f"/api/card/{by_name[name]['id']}?legacy-mbql=true"
                                for name in names if name in by_name})


def upsert_card(client, by_name, name, payload, stored=None):
    """Create card `name` or update it in place. `stored` is the
    server's card (stored_cards); one equal to the payload is left in
    place. Returns (card id, 'created'|'updated'|'skipped') on success,
    (None, error text) on failure."""
    existing = by_name.get(name)
    if card_same(stored, payload):
        return stored["id"], "skipped"
    if existing:
        st, body = client.put(f"/api/card/{existing['id']}", payload)
        if st in (200, 201):
            return existing["id"], "updated"
    else:
        st, body = client.post("/api/card", payload)
        if st in (200, 201) and body.get("id"):
            return body["id"], "created"
    return None, f"({st}): {body.get('message')}"


def bind(payload, db_id, field_ids, model_ids):
    """Card `payload` with its references bound to one Metabase's ids:
    its dataset_query takes the database `db_id`, a ("field", table,
    column) becomes the field reference of that column's id in
    `field_ids`, and a ("model_id", name) the card id of that model by
    `model_ids`. A reference with no id raises
    ValueError. ensure_synced stops the run before any card is built when
    a column filter_field_columns names has no id, so a missing one is a
    definition out of step."""
    def ref(kind, key, ids):
        if key not in ids:
            raise ValueError(f"provision: no {kind} id for {key}")
        return ids[key]

    def walk(v, key=None):
        if isinstance(v, tuple) and v[:1] == ("field",):
            return ["field", ref("field", v[1:], field_ids), None]
        if isinstance(v, tuple) and v[:1] == ("model_id",):
            return ref("model", v[1], model_ids)
        if isinstance(v, dict):
            out = {k: walk(x, k) for k, x in v.items()}
            return {"type": out["type"], "database": db_id, **out} if key == "dataset_query" else out
        if isinstance(v, list):
            return [walk(x) for x in v]
        return v
    return walk(payload)


def card_payload(coll_id, name, ctype, display, desc, query, viz):
    """The full /api/card payload shared by models and questions."""
    return {
        "name": name,
        "type": ctype,
        "description": desc,
        "collection_id": coll_id,
        "display": display,
        "visualization_settings": viz,
        "dataset_query": query,
    }


def upsert_cards(client, by_name, payloads, label):
    """Create or update-in-place every card in `payloads` (name -> full
    /api/card payload). An unchanged card is left in place. Returns
    (name -> card id, #created, #updated, #skipped), or None on the
    first failure."""
    ids, n = {}, {"created": 0, "updated": 0, "skipped": 0}
    stored = stored_cards(client, by_name, payloads)
    for name, payload in payloads.items():
        cid, how = upsert_card(client, by_name, name, payload, stored.get(name))
        if cid is None:
            print(f"provision: failed to save {label} '{name}' {how}",
                  file=sys.stderr)
            return None
        ids[name] = cid
        n[how] += 1
    return ids, n["created"], n["updated"], n["skipped"]


def archive_all(client, kind, ids):
    """Archive /api/<kind>/<id> for every id; returns how many succeeded."""
    n = 0
    for i in ids:
        st, _ = client.put(f"/api/{kind}/{i}", {"archived": True})
        n += st in (200, 201)
    return n


def model_payloads(db_id, coll_id):
    """name -> /api/card payload of every report model, bound to
    database `db_id`. Sends nothing."""
    return {
        name: bind(card_payload(coll_id, name, "model", "table", desc, _native(query, {}), {}),
                   db_id, {}, {})
        for name, (query, desc) in report_models().items()
    }


def card_payloads(db_id, coll_id, field_ids, model_ids, colors):
    """name -> /api/card payload of every question and privacy card,
    bound to database `db_id`, the field ids `field_ids` and the models
    in `model_ids`, a card that draws a series per source with `colors`
    (source id -> colour). Sends nothing."""
    return {name: bind(card_payload(coll_id, name, "question", c.display, c.desc, c.query,
                                    with_source_colors(c.viz, colors)), db_id, field_ids, model_ids)
            for name, c in card_defs().items()}


def ensure_models(client, db_id, coll_id, by_name):
    """Create/refresh the pre-defined report models in `coll_id`, and
    archive any retired (renamed-away) ones. An unchanged model is left
    in place. The models are content-free shims over the gold serving
    tables and views — no source data is baked in. Returns model name ->
    card id, or None on failure."""
    saved = upsert_cards(client, by_name, model_payloads(db_id, coll_id), "model")
    if saved is None:
        return None
    ids, created, updated, skipped = saved
    archived = archive_all(client, "card",
                           [by_name[n]["id"] for n in RETIRED_MODEL_NAMES
                            if n in by_name])
    print(f"provision: report models ({_ccy_slash()}) — {created} created, "
          f"{updated} updated, {skipped} unchanged, {archived} retired "
          f"(collection '{COLLECTION_NAME}')")
    return ids


def ensure_cards(client, db_id, coll_id, by_name, field_ids, model_ids, colors=None):
    """Create/refresh the pre-defined questions, bound to the field ids
    `field_ids` and the models in `model_ids`, and archive any retired
    (renamed-away) ones. A card that draws a series per source takes
    `colors` (source id -> colour). Returns card name -> id (the
    dashboards' tile lookup) and card name -> its description, or None on
    failure."""
    payloads = card_payloads(db_id, coll_id, field_ids, model_ids, colors or {})
    saved = upsert_cards(client, by_name, payloads, "card")
    if saved is None:
        return None
    ids, created, updated, skipped = saved
    archived = archive_all(client, "card",
                           [by_name[n]["id"] for n in RETIRED_CARD_NAMES
                            if n in by_name])
    print(f"provision: questions — {created} created, {updated} updated, "
          f"{skipped} unchanged, {archived} retired")
    return ids, {name: body["description"] for name, body in payloads.items()}


def text_dashcard(dc_id, text, row=0, display="text"):
    """A virtual tile (no backing card) spanning row `row`: the absolute
    <-> privacy switch link on the top row or a chart's subtitle as
    `text`, a section heading as `heading`."""
    return {"id": dc_id, "card_id": None, "row": row, "col": 0,
            "size_x": 24, "size_y": 1, "series": [], "parameter_mappings": [],
            "visualization_settings": {
                "virtual_card": {"name": None, "display": display,
                                 "visualization_settings": {},
                                 "dataset_query": {}, "archived": False},
                "text": text,
                "dashcard.background": False,
                "text.align_vertical": "middle"}}


# The template tag whose name is not the slug of the picker it answers:
# the returns cards' Source field filter.
TAG_SLUGS = {"source_ff": "source"}


def tile_mappings(query, card_id, parameters):
    """The picker mappings of the tile of native card `card_id`, whose
    dataset_query is `query`, on a dashboard with pickers `parameters`:
    each picker, in their order, whose slug names one of the card's
    template tags (TAG_SLUGS where the two differ). A field filter takes
    it as a dimension, a plain variable as a variable. A card declines a
    picker by declaring no tag of its slug."""
    tags = query["native"]["template-tags"]
    by_slug = {TAG_SLUGS.get(t, t): t for t in tags}
    return [{"parameter_id": x["id"], "card_id": card_id,
             "target": ["dimension" if tags[t].get("type") == "dimension" else "variable",
                        ["template-tag", t]]}
            for x in parameters if (t := by_slug.get(x["slug"])) is not None]


def overview_home(card, dash):
    """The dashboard Card `card`'s tile on overview dashboard `dash`
    restates, its home: a dashboard it is borrowed from, or one whose
    data the overview's own card shows. A click on the tile opens it,
    on the twin that dashboard's twin; a dashboard with no twin (Gains)
    opens from the base alone, so a click never leads from the privacy
    view to money. None for the net worth tiles, which open nothing, and for
    a twin tile whose home has no twin."""
    if card.home is None or not dash.endswith(PRIVACY_SUFFIX):
        return card.home
    return None if card.home in NO_PRIVACY_TWIN else card.home + PRIVACY_SUFFIX


def overview_click(home, dash, params, dash_ids, links):
    """The click behaviour of a tile on overview dashboard `dash` that
    opens dashboard `home` (overview_home): pass each overview picker
    that dashboard carries by slug, and the column of each Link in
    `links` to the home picker it names."""
    target = {x["slug"]: x["id"] for x in params[home]}

    def link(tid, source):
        return {"id": tid, "source": source, "target": {"type": "parameter", "id": tid}}
    mapping = {target[x["slug"]]: link(target[x["slug"]], {"type": "parameter", "id": x["id"], "name": x["name"]})
               for x in params[dash] if x["slug"] in target}
    for col, to in links:
        if to in target:
            mapping[target[to]] = link(target[to], {"type": "column", "id": col, "name": col})
    return {"click_behavior": {"type": "link", "linkType": "dashboard",
                               "targetId": dash_ids[home], "parameterMapping": mapping}}


def unreached_note(mapped, params):
    """The sentence a tile's description ends in, naming the pickers of
    its dashboard (parameters `params`) that the parameter ids `mapped`
    leave out; empty where every picker reaches the tile. The Benchmark
    picker goes unnamed: most tiles have no benchmark to show, and a
    reader expects none there."""
    params = [x for x in params if x["id"] != BENCHMARK_PARAM_ID]
    names = [x["name"] for x in params if x["id"] not in mapped]
    if not names:
        return ""
    if len(names) == len(params):
        return " No picker reaches it."
    listed = names[0] if len(names) == 1 else ", ".join(names[:-1]) + " and " + names[-1]
    return (f" The {listed} picker{'s' if len(names) > 1 else ''}"
            f" do{'' if len(names) > 1 else 'es'} not reach it.")


def crossfilter_viz(links, parameters):
    """A tile's own viz settings: for each Link in `links` whose picker
    the dashboard carries (`parameters`), the click behaviour that sets
    that picker to the value of the cell clicked in its column. Empty
    for a tile with none."""
    slug_pid = {x["slug"]: x["id"] for x in parameters}
    settings = {f'["name","{c}"]': {"click_behavior": {
                    "type": "crossfilter",
                    "parameterMapping": {pid: {"id": pid, "source": {"type": "column", "id": c, "name": c},
                                               "target": {"type": "parameter", "id": pid}}}}}
                for c, to in links if (pid := slug_pid.get(to)) is not None}
    return {"column_settings": settings} if settings else {}


def dashboard_payloads(defs, cards, model_ids, card_ids, dash_ids, titles, descs):
    """name -> (description, parameters, tiles) of each dashboard in
    `defs` (dashboard_defs), in its order, of the Cards `cards`: the
    payload ensure_dashboards sends. Its parameters are
    dashboard_parameters bound to the models in `model_ids`, `dash_ids`
    is name -> its id, and `titles` and `descs` are as ensure_dashboards
    takes them. Sends nothing."""
    params = {name: bind(dashboard_parameters(name), None, {}, model_ids) for name in defs}
    out = {}
    for name, d in defs.items():
        desc, sibling = d.desc, d.sibling
        parameters = params[name]
        # The pickers of the filter bar: an inline picker renders on the
        # tiles it reaches instead (inline_parameters).
        bar = [x for x in parameters if x["id"] not in INLINE_PICKERS]
        # A dashboard whose twin redacts (Dashboard.redacts) says what
        # the twin does with the money where the others say "shares".
        privacy = name.endswith(PRIVACY_SUFFIX)
        if d.redacts:
            label = ("Switch to the full view — money columns included"
                     if privacy else
                     f"Switch to the privacy view — {d.redacts}")
        else:
            label = ("Switch to absolute values" if privacy else
                     "Switch to the privacy view — values as shares (%), "
                     "not amounts")
        icon = "🔓" if privacy else "🔒"
        link = (f"{icon} [{label}](/dashboard/{dash_ids[sibling]})"
                if sibling else None)
        base_name = sibling if privacy else name
        overview = base_name == "Wealth Overview"
        tile_titles_here = {**titles, **dashboard_tile_titles(base_name, cards)}

        # The switch link occupies row 0, so the tiles and headings shift
        # down one row; a dashboard without a twin has no link and no
        # shift.
        dashcards = [text_dashcard(-99, link)] if link else []
        shift = 1 if link else 0
        dashcards += [text_dashcard(-(100 + k), text, row + shift, display)
                      for k, (row, text, display) in enumerate(d.headings)]
        for i, (card, row, col, sx, sy) in enumerate(d.tiles):
            # The pickers a tile answers to: each picker of the
            # dashboard its card's tags name (tile_mappings).
            c = cards[card]
            home = overview_home(c, name) if overview else None
            maps = tile_mappings(c.query, card_ids[card], parameters)
            viz = {**(crossfilter_viz(c.links, parameters) if home is None else {}),
                   **({"card.title": tile_titles_here[card]} if card in tile_titles_here else {}),
                   **({"card.hide_empty": True} if c.hide_empty else {})}
            # A tile's description names the pickers of the bar that do
            # not reach it, read off the mappings it is sent with, and a
            # Wealth Overview tile opens its home dashboard.
            note = unreached_note({m["parameter_id"] for m in maps}, bar)
            if note and card in descs:
                viz["card.description"] = descs[card] + note
            if home is not None:
                viz.update(overview_click(home, name, params, dash_ids, c.links))
            dashcards.append({"id": -(i + 1), "card_id": card_ids[card], "row": row + shift,
                              "col": col, "size_x": sx, "size_y": sy, "series": [],
                              "visualization_settings": viz,
                              "inline_parameters": [m["parameter_id"] for m in maps
                                                    if m["parameter_id"] in INLINE_PICKERS],
                              "parameter_mappings": maps})
        out[name] = (desc, parameters, dashcards)
    return out


def ensure_dashboards(client, coll_id, card_ids, model_ids, titles=None, descs=None):
    """Create/refresh the pre-defined dashboards (and their privacy twins)
    with their pickers (dashboard_parameters) mapped onto the tiles
    whose tags name them (tile_mappings) and, where there is a twin, a switch link to it on the top row,
    and archive any retired (renamed-away) ones. A card in `titles`
    (tile_titles) shows that title on its tiles. `descs`, card name ->
    description, lets a tile carry its card's description with the
    pickers of its dashboard that do not reach it named. A dashboard's
    headings are full-width rows of text, and a Wealth Overview tile
    opens its home dashboard (overview_click). An unchanged dashboard
    is left in place. One that differs has its parameter list and its
    tiles replaced wholesale, so the layout converges to spec and a tile
    added by hand does not survive. The dashboard description says to
    duplicate before customizing."""
    titles = titles or {}
    descs = descs or {}
    try:
        listed = client.dashboards(coll_id)
    except metabase.Error as e:
        print(f"provision: could not list dashboards ({e.status}): {e.body.get('message')}",
              file=sys.stderr)
        return 1
    existing = {d.get("name"): d.get("id") for d in listed}

    cards = card_defs()
    defs = dashboard_defs(cards)
    n = {"created": 0, "updated": 0, "skipped": 0}
    # First pass: make sure every dashboard exists, so the switch links
    # can point at the sibling's id.
    dash_ids = {}
    for name, d in defs.items():
        did = existing.get(name)
        if did is None:
            st, body = client.post("/api/dashboard", {"name": name, "description": d.desc,
                                                      "collection_id": coll_id})
            if st not in (200, 201) or not body.get("id"):
                print(f"provision: failed to create dashboard '{name}' ({st}): "
                      f"{body.get('message')}", file=sys.stderr)
                return 1
            did = body["id"]
            n["created"] += 1
        dash_ids[name] = did
    # The dashboards listed before this run, read for the comparison. One
    # created above has no tiles yet, so it is sent without a read.
    stored = _get_all(client, {name: f"/api/dashboard/{existing[name]}"
                                  for name in defs if existing.get(name) is not None})

    payloads = dashboard_payloads(defs, cards, model_ids, card_ids, dash_ids, titles, descs)
    for name, (desc, parameters, dashcards) in payloads.items():
        current = stored.get(name)
        if dashboard_unchanged(current, name, desc, parameters, dashcards):
            n["skipped"] += 1
            continue
        if name in existing:
            n["updated"] += 1
        # A change to the pickers alone (a new picker default) is sent
        # alone, so the tiles keep their ids.
        if dashboard_unchanged(current, name, desc, (current or {}).get("parameters"), dashcards):
            if not _put_parameters(client, dash_ids[name], name, parameters,
                                   "update the filters of"):
                return 1
            continue
        st, body = client.put(f"/api/dashboard/{dash_ids[name]}",
                              {"name": name, "description": desc,
                               "parameters": parameters,
                               "dashcards": dashcards})
        if st not in (200, 201):
            print(f"provision: failed to lay out dashboard '{name}' ({st}): "
                  f"{body.get('message')}", file=sys.stderr)
            return 1
        # Deleting a dashcard that carries an inline filter makes Metabase
        # drop that filter from the dashboard — and the wholesale dashcard
        # replacement above deletes every old tile, so an inline parameter
        # sent in the same PUT gets cleaned right back up. Re-assert the
        # parameter list now that the new tiles are in place.
        if parameters and not _put_parameters(client, dash_ids[name], name, parameters,
                                              "re-assert filters on"):
            return 1

    archived = archive_all(client, "dashboard",
                           [existing[name] for name in RETIRED_DASHBOARD_NAMES
                            if existing.get(name) is not None])
    print(f"provision: dashboards — {n['created']} created, {n['updated']} updated, "
          f"{n['skipped']} unchanged, {archived} retired")
    return 0


def _put_parameters(client, did, name, parameters, what):
    """PUT dashboard `did`'s parameter list alone. False when the server
    refuses it, with "failed to `what` `name`" printed."""
    st, body = client.put(f"/api/dashboard/{did}", {"parameters": parameters})
    if st in (200, 201):
        return True
    print(f"provision: failed to {what} '{name}' ({st}): {body.get('message')}",
          file=sys.stderr)
    return False


def _mapping_order(mappings):
    """A tile's picker mappings in a stable order, by parameter, card and
    target: Metabase gives their order no meaning."""
    if not isinstance(mappings, list):
        return mappings
    return sorted(mappings, key=lambda m: json.dumps(
        [m.get("parameter_id"), m.get("card_id"), m.get("target")], sort_keys=True)
        if isinstance(m, dict) else "")


def _dashcards_same(server, desired):
    """True when the stored tiles already match the payload. Tiles pair
    by their place on the grid, whatever order the server lists them
    in, and a tile's picker mappings match whatever their order. A tile
    is compared on the keys the payload sends; the rest of a stored tile
    is the server's own. A missing tile list is not a match."""
    if not isinstance(server, list) or len(server) != len(desired):
        return False
    if not all(isinstance(t, dict) for t in server):
        return False

    def place(tile):
        return (tile.get("row") or 0, tile.get("col") or 0)

    server, desired = sorted(server, key=place), sorted(desired, key=place)
    for got, want in zip(server, desired, strict=True):
        for key, value in want.items():
            mine = got.get(key)
            if key == "parameter_mappings":
                mine, value = _mapping_order(mine), _mapping_order(value)
            if key != "id" and not _same_value(mine, value):
                return False
    return True


def dashboard_unchanged(current, name, desc, parameters, dashcards):
    """True when the stored dashboard already matches the payload."""
    return (isinstance(current, dict)
            and current.get("name") == name
            and current.get("description") == desc
            and _same_value(current.get("parameters"), parameters)
            and _dashcards_same(current.get("dashcards"), dashcards))


if __name__ == "__main__":
    sys.exit(main())
