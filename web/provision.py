#!/usr/bin/env python3
"""Provision a fresh Metabase over its loopback REST API: create the
admin account (skipping the "tell us about yourself" setup wizard) and
pre-add the gold DuckDB database. Idempotent — safe to run on every
`wealthdb web start`. Standard library only.

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
                    help="output currency the report models bind (report_x(..., CCY))")
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

    return ensure_models(a.base, sid, db_id, a.default_currency)


# MAX_BIGINT as the as-of epoch means "latest snapshot" (the macros'
# current mode), so the models stay current without re-provisioning;
# report_transactions spans all of time (filter in Metabase).
MAX_BIGINT = 9223372036854775807


def report_queries(ccy):
    """name -> native SQL, one per `wealthdb` report command. The
    report_* DuckDB macros (internal/gold/migrations/0021) are the
    single source of truth, so each model returns exactly what its CLI
    command prints for the same output currency."""
    return {
        "report_global":       f"SELECT * FROM report_global({MAX_BIGINT}, '{ccy}')",
        "report_portfolios":   f"SELECT * FROM report_portfolios({MAX_BIGINT}, '{ccy}')",
        "report_accounts":     f"SELECT * FROM report_accounts({MAX_BIGINT}, '{ccy}')",
        "report_positions":    f"SELECT * FROM report_positions({MAX_BIGINT}, '{ccy}')",
        "report_transactions": f"SELECT * FROM report_transactions(0, {MAX_BIGINT}, '{ccy}')",
    }


def ensure_models(base, sid, db_id, ccy):
    """Create the 5 report models as native-query Metabase models,
    idempotently (skip any whose name already exists). Content-free
    shims over the macros — no source data is baked in."""
    _, cards = req(base, "/api/card", session=sid)
    existing = {c.get("name") for c in cards} if isinstance(cards, list) else set()
    created = 0
    for name, query in report_queries(ccy).items():
        if name in existing:
            continue
        st, body = req(base, "/api/card", "POST", {
            "name": name,
            "type": "model",
            "display": "table",
            "visualization_settings": {},
            "dataset_query": {
                "type": "native",
                "database": db_id,
                "native": {"query": query, "template-tags": {}},
            },
        }, session=sid)
        if st in (200, 201) and body.get("id"):
            created += 1
        else:
            print(f"provision: failed to create model '{name}' ({st}): {body.get('message')}",
                  file=sys.stderr)
            return 1
    if created:
        print(f"provision: created {created} report model(s) in {ccy}")
    else:
        print("provision: report models already present")
    return 0


if __name__ == "__main__":
    sys.exit(main())
