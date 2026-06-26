#!/usr/bin/env python3
"""Provision a fresh Metabase over its loopback REST API: create the
admin account (skipping the "tell us about yourself" setup wizard),
pre-add the gold DuckDB database, and create the pre-defined report
models. Idempotent — safe to run on every `wealthdb web start`.
Standard library only.

Called by web/web; not meant to be run by hand (but it can be).
"""
import argparse
import json
import sys
import time
import urllib.error
import urllib.request


def req(base, path, method="GET", data=None, session=None, timeout=30):
    body = json.dumps(data).encode() if data is not None else None
    r = urllib.request.Request(base + path, data=body, method=method)
    r.add_header("Content-Type", "application/json")
    if session:
        r.add_header("X-Metabase-Session", session)
    try:
        with urllib.request.urlopen(r, timeout=timeout) as resp:
            raw = resp.read().decode()
            return resp.status, (json.loads(raw) if raw else {})
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            return e.code, json.loads(raw)
        except Exception:
            return e.code, {"message": raw}
    except (urllib.error.URLError, ConnectionError):
        return 0, {}


def wait_health(base, tries=90, delay=3):
    for _ in range(tries):
        st, _ = req(base, "/api/health", timeout=5)
        if st == 200:
            return True
        time.sleep(delay)
    return False


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", required=True)
    ap.add_argument("--email", required=True)
    ap.add_argument("--password", required=True)
    ap.add_argument("--gold-path", required=True)
    ap.add_argument("--db-name", default="gold")
    ap.add_argument("--default-currency", default="USD",
                    help="accepted for compatibility; no longer used — the report "
                         "models now expose USD/CHF/EUR columns via report_x_multi(...)")
    a = ap.parse_args()

    if not wait_health(a.base):
        print("provision: Metabase did not become healthy in time", file=sys.stderr)
        return 1

    _, props = req(a.base, "/api/session/properties")
    setup_done = bool(props.get("has-user-setup"))
    token = props.get("setup-token")

    if not setup_done and token:
        st, body = req(a.base, "/api/setup", "POST", {
            "token": token,
            "user": {"first_name": "wealthdb", "last_name": "admin",
                     "email": a.email, "password": a.password},
            "prefs": {"site_name": "wealthdb", "allow_tracking": False},
        })
        if st not in (200, 201) or not body.get("id"):
            print(f"provision: setup failed ({st}): {body.get('message')}", file=sys.stderr)
            return 1
        print(f"provision: created admin {a.email}; setup wizard skipped")
    else:
        print("provision: already set up; skipping admin creation")

    # Log in to add the database. If setup was done earlier with a
    # different password (user changed it), don't fail the start —
    # the database was added on the first provision.
    _, body = req(a.base, "/api/session", "POST",
                  {"username": a.email, "password": a.password})
    sid = body.get("id")
    if not sid:
        if setup_done:
            print("provision: couldn't verify admin login (password changed?) — leaving as-is")
            return 0
        print(f"provision: login failed: {body.get('message')}", file=sys.stderr)
        return 1

    db_id = None
    _, dbs = req(a.base, "/api/database", session=sid)
    for d in (dbs.get("data") or []):
        if d.get("name") == a.db_name and d.get("engine") == "duckdb":
            db_id = d.get("id")
            print(f"provision: database '{a.db_name}' already present")
            break

    if db_id is None:
        st, body = req(a.base, "/api/database", "POST", {
            "engine": "duckdb", "name": a.db_name,
            "details": {"database_file": a.gold_path, "read_only": True},
        }, session=sid)
        if st in (200, 201) and body.get("id"):
            db_id = body["id"]
            print(f"provision: added DuckDB database '{a.db_name}' -> {a.gold_path}")
        else:
            print(f"provision: failed to add database ({st}): {body.get('message')}", file=sys.stderr)
            return 1

    coll_id = ensure_collection(a.base, sid, COLLECTION_NAME)
    return ensure_models(a.base, sid, db_id, coll_id)


# MAX_BIGINT as the as-of epoch means "latest snapshot" (the macros'
# current mode), so the models stay current without re-provisioning;
# report_transactions spans all of time (filter in Metabase).
MAX_BIGINT = 9223372036854775807

# Collection the pre-defined models live in (kept apart from anything the
# user builds by hand).
COLLECTION_NAME = "wealthdb (pre-defined)"

# Un-suffixed model names retired when the snapshot reports gained the
# `_latest` suffix; archived on provision so a re-run cleans them up.
RETIRED_MODEL_NAMES = ["report_global", "report_portfolios",
                       "report_accounts", "report_positions"]


def report_models():
    """model name -> (native SQL, description). The report_*_multi DuckDB
    macros (migration 0024) are the single source of truth; each model only
    wraps a macro to bind the as-of and to render epoch columns as TIMESTAMP
    (`to_timestamp` -> naive-UTC) for Metabase. The macros already emit DECIMAL
    money/quantity columns and one value column set per reporting currency
    (USD/CHF/EUR), so no value casting is needed here. The `_latest` reports
    are as of each source's latest snapshot; the `_history` reports carry value
    forward per day."""
    def wrap(from_expr, ts_cols=()):
        parts = [f"CAST(to_timestamp({c}) AS TIMESTAMP) AS {c}" for c in ts_cols]
        return f"SELECT * REPLACE ({', '.join(parts)}) FROM {from_expr}"

    L = f"({MAX_BIGINT})"   # _multi _latest macro arg (as-of = latest snapshot)
    return {
        "report_global_latest": (
            wrap(f"report_global_multi{L}", ["min_snapshot_at", "max_snapshot_at"]),
            "Whole-portfolio rollup as of the latest snapshot: cash, positions and "
            "total value in USD, CHF and EUR (one column set per currency), with the "
            "min/max snapshot date span. Mirrors `wealthdb holdings global`."),
        "report_sources_latest": (
            wrap(f"report_sources_multi{L}", ["snapshot_at"]),
            "One row per silver source as of the latest snapshot: positions + cash "
            "totalled in the source's base currency and in USD/CHF/EUR, with rolled-up "
            "tax wrapper / management style. Mirrors `wealthdb holdings sources`."),
        "report_portfolios_latest": (
            wrap(f"report_portfolios_multi{L}", ["snapshot_at"]),
            "One row per portfolio as of the latest snapshot: positions + cash totalled "
            "in the portfolio's base currency and in USD/CHF/EUR, with rolled-up tax "
            "wrapper / management style. Mirrors `wealthdb holdings portfolios`."),
        "report_accounts_latest": (
            wrap(f"report_accounts_multi{L}", ["snapshot_at"]),
            "One row per account as of the latest snapshot: positions + cash totalled "
            "in the account's base currency and in USD/CHF/EUR, with kind, tax wrapper "
            "and management style. Mirrors `wealthdb holdings accounts`."),
        "report_positions_latest": (
            wrap(f"report_positions_multi{L}", ["snapshot_at"]),
            "One row per held position as of the latest snapshot, with market value in "
            "USD, CHF and EUR. Mirrors `wealthdb holdings positions`."),
        "report_transactions": (
            wrap(f"report_transactions_multi(0, {MAX_BIGINT})", ["occurred_at"]),
            "Every transaction over all time, with net amount in USD, CHF and EUR at the "
            "transaction date. Mirrors `wealthdb transactions` (filter the date range in "
            "Metabase)."),
        # History reports: one row per entity per UTC day, from the first snapshot to
        # today, value carried forward between snapshots. For time-series charts; filter
        # / aggregate by as_of_day. history@today equals the matching _latest report.
        "report_global_history": (
            wrap("report_global_history_multi()", ["as_of_day"]),
            "Whole-portfolio value for every day from the first snapshot to today "
            "(carried forward between snapshots), in USD, CHF and EUR. The net-worth-"
            "over-time series — chart total_value_usd (or _chf / _eur) against as_of_day."),
        "report_sources_history": (
            wrap("report_sources_history_multi()", ["as_of_day"]),
            "Per-silver-source value for every day (carried forward), in the source's "
            "base currency and in USD/CHF/EUR. Filter to a source and chart against "
            "as_of_day."),
        "report_accounts_history": (
            wrap("report_accounts_history_multi()", ["as_of_day"]),
            "Per-account value for every day (carried forward), in the account's base "
            "currency and in USD/CHF/EUR. Filter to an account and chart against as_of_day."),
        "report_portfolios_history": (
            wrap("report_portfolios_history_multi()", ["as_of_day"]),
            "Per-portfolio value for every day (carried forward), in the portfolio's base "
            "currency and in USD/CHF/EUR. Filter to a portfolio and chart against as_of_day."),
        "report_positions_history": (
            wrap("report_positions_history_multi()", ["as_of_day", "snapshot_at"]),
            "Per-position value for every day (carried forward), in USD, CHF and EUR. "
            "Large (days x held positions) — filter to a position / account / date range "
            "before charting."),
    }


def ensure_collection(base, sid, name):
    """Return the id of the collection named `name`, creating it if absent."""
    _, cols = req(base, "/api/collection", session=sid)
    for c in (cols if isinstance(cols, list) else []):
        if c.get("name") == name and not c.get("archived"):
            return c.get("id")
    st, body = req(base, "/api/collection", "POST", {"name": name}, session=sid)
    if st in (200, 201) and body.get("id"):
        print(f"provision: created collection '{name}'")
        return body["id"]
    # Non-fatal: fall back to the default (root) collection.
    print(f"provision: could not create collection '{name}' ({st}): "
          f"{body.get('message')}; using the default collection", file=sys.stderr)
    return None


def ensure_models(base, sid, db_id, coll_id):
    """Create/refresh the pre-defined report models in `coll_id`, and
    archive any retired (renamed-away) ones. Idempotent: an existing model
    of the same name is updated in place; re-running converges. The models
    are content-free shims over the gold macros — no source data is baked
    in."""
    _, cards = req(base, "/api/card", session=sid)
    by_name = {c.get("name"): c for c in cards} if isinstance(cards, list) else {}

    created = updated = 0
    for name, (query, desc) in report_models().items():
        payload = {
            "name": name,
            "type": "model",
            "description": desc,
            "collection_id": coll_id,
            "display": "table",
            "visualization_settings": {},
            "dataset_query": {
                "type": "native",
                "database": db_id,
                "native": {"query": query, "template-tags": {}},
            },
        }
        existing = by_name.get(name)
        if existing and not existing.get("archived"):
            st, body = req(base, f"/api/card/{existing['id']}", "PUT", payload, session=sid)
            if st in (200, 201):
                updated += 1
                continue
        else:
            st, body = req(base, "/api/card", "POST", payload, session=sid)
            if st in (200, 201) and body.get("id"):
                created += 1
                continue
        print(f"provision: failed to save model '{name}' ({st}): {body.get('message')}",
              file=sys.stderr)
        return 1

    archived = 0
    for name in RETIRED_MODEL_NAMES:
        c = by_name.get(name)
        if c and not c.get("archived"):
            st, _ = req(base, f"/api/card/{c['id']}", "PUT", {"archived": True}, session=sid)
            if st in (200, 201):
                archived += 1

    print(f"provision: report models (USD/CHF/EUR) — {created} created, {updated} updated, "
          f"{archived} retired (collection '{COLLECTION_NAME}')")
    return 0


if __name__ == "__main__":
    sys.exit(main())
