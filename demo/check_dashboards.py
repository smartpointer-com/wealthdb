#!/usr/bin/env python3
"""Run every dashboard card of a wealthdb Metabase and report failures.

    python3 demo/check_dashboards.py [--base URL] [--password-file PATH]
                                     [--email ADDR] [--first-year YYYY]
                                     [--workers N]

The defaults are the demo's: its port, its admin password file under
~/wealthdb-demo, and the year its history starts.

Logs in once to mint a temporary API key, runs every dashcard of every
dashboard in the 'wealthdb (pre-defined)' collection through the
dashboard query endpoint, then deletes the key. Queries run under the
key rather than the admin's session because Metabase remembers the last
filter values a user ran a dashboard with and shows them to that user
next time.

Each card runs at its dashboard's defaults, once per time window with no
source picked, and once per source over the full history. Six more
pickers are varied one at a time around their defaults: currency,
investing grain, section, start year, as-of day and the reading of a
missing cost basis. The account, category, income type, asset class and
vehicle pickers stay at their defaults.

A run fails when Metabase reports an error. A card that returns no rows
at its dashboard's defaults fails too. Empty runs under any other picker
value are listed, not failed: many card and source pairs are empty by
nature (card balances for a source with no card). Exits 1 on any
failure. Stdlib only.
"""

import argparse
import concurrent.futures
import datetime as dt
import json
import pathlib
import sys
import urllib.error
import urllib.request

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

from demohouse import config, spec  # noqa: E402

COLLECTION = "wealthdb (pre-defined)"
WINDOWS = ["past7days~", "past30days~", "past3months~", "past12months~"]


def call(base, path, method="GET", body=None, headers=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(base + path, data=data, method=method,
                                 headers={"Content-Type": "application/json", **(headers or {})})
    try:
        with urllib.request.urlopen(req, timeout=300) as r:
            raw = r.read()
            return r.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, json.loads(raw)
        except ValueError:
            return e.code, {"error": raw.decode(errors="replace")[:300]}


def mint_key(base, email, password):
    st, sess = call(base, "/api/session", "POST", {"username": email, "password": password})
    if st != 200:
        raise SystemExit(f"login failed ({st}): {sess}")
    auth = {"X-Metabase-Session": sess["id"]}
    _, groups = call(base, "/api/permissions/group", headers=auth)
    admin = next(g["id"] for g in groups if g["name"] == "Administrators")
    name = f"dashboard-check-{dt.datetime.now(dt.timezone.utc):%Y%m%d%H%M%S}"
    st, key = call(base, "/api/api-key", "POST", {"name": name, "group_id": admin}, auth)
    if st != 200:
        raise SystemExit(f"could not create an API key ({st}): {key}")
    return auth, key["id"], {"X-API-Key": key["unmasked_key"]}


def combos(params, sources, currencies, first_year, today):
    """(label, parameter values) for one dashboard: the defaults, each
    window with no source, full history with each source, then each other
    picker varied alone around the defaults. `currencies` is what the
    dashboard's own Currency picker offers."""
    by_slug = {p["slug"]: p for p in params}
    base = {}
    for p in params:
        if p.get("default") is not None:
            base[p["id"]] = p["default"]
    time = by_slug.get("time_range")
    source = by_slug.get("source")
    full = f"{first_year}-01-01~{today.isoformat()}"
    out = [("defaults", dict(base))]
    if time:
        for w in WINDOWS + [full]:
            out.append((f"window {w}", {**base, time["id"]: w}))
    if source:
        for s in sources:
            v = {**base, source["id"]: [s]}
            if time:
                v[time["id"]] = full
            out.append((f"source {s}", v))
    variations = {
        "currency": [[c] for c in currencies],
        "investing": [["whole"], ["class"]],
        "section": [["operating_in"], ["operating_out"], ["investing"], ["financing"], ["vehicles"]],
        "start_year": [[y] for y in range(first_year, today.year + 1)],
        "as_of_day": [f"{first_year + 1}-12-31", today.isoformat()],
        "missing_basis": [["ignore"], ["zero"]],
    }
    for slug, values in variations.items():
        p = by_slug.get(slug)
        if p:
            for v in values:
                out.append((f"{slug} {v}", {**base, p["id"]: v}))
    return out


def run_card(base, auth, dash, dc, ptypes, values):
    mapped = {m["parameter_id"]: m for m in dc.get("parameter_mappings", [])}
    body = {"parameters": [{"id": pid, "type": ptypes[pid], "value": v, "target": mapped[pid]["target"]}
                           for pid, v in values.items() if pid in mapped]}
    st, res = call(base, f"/api/dashboard/{dash['id']}/dashcard/{dc['id']}/card/{dc['card_id']}/query",
                   "POST", body, auth)
    ok = st in (200, 202) and isinstance(res, dict) and res.get("status") == "completed" and "error" not in res
    rows = (res.get("data") or {}).get("rows", []) if isinstance(res, dict) else []
    err = None if ok else (res.get("error") if isinstance(res, dict) else str(res))
    return ok, rows, err, body["parameters"]


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--base", default=f"http://127.0.0.1:{config.WEB_PORT}")
    ap.add_argument("--email", default="admin@wealthdb.local")
    ap.add_argument("--password-file", default=str(pathlib.Path.home() / "wealthdb-demo/web/admin-password.txt"))
    ap.add_argument("--first-year", type=int, default=spec.load().history_start.year)
    ap.add_argument("--workers", type=int, default=3)
    a = ap.parse_args(argv)
    password = pathlib.Path(a.password_file).read_text().strip()
    admin_auth, key_id, auth = mint_key(a.base, a.email, password)
    today = dt.datetime.now(dt.timezone.utc).date()
    try:
        _, cols = call(a.base, "/api/collection", headers=auth)
        coll = next(c for c in cols if c.get("name") == COLLECTION and not c.get("archived"))
        _, items = call(a.base, f"/api/collection/{coll['id']}/items?models=dashboard", headers=auth)
        dashboards = sorted(items["data"], key=lambda d: d["name"])
        failures, empties, runs, freshness = [], set(), 0, None
        for d in dashboards:
            _, dash = call(a.base, f"/api/dashboard/{d['id']}", headers=auth)
            ptypes = {p["id"]: p["type"] for p in dash.get("parameters", [])}
            def offered(slug, dash=dash, d=d):
                """The values the dashboard's `slug` picker offers."""
                p = next((p for p in dash.get("parameters", []) if p["slug"] == slug), None)
                if not p:
                    return []
                _, vals = call(a.base, f"/api/dashboard/{d['id']}/params/{p['id']}/values", headers=auth)
                return sorted(v[0] for v in (vals or {}).get("values", []))

            sources, currencies = offered("source"), offered("currency")
            jobs = []
            for dc in dash.get("dashcards", []):
                if not dc.get("card_id"):
                    continue
                for label, values in combos(dash.get("parameters", []), sources, currencies, a.first_year, today):
                    jobs.append((dc, label, values))
            def run(job, dash=dash, ptypes=ptypes):
                return job, run_card(a.base, auth, dash, job[0], ptypes, job[2])

            with concurrent.futures.ThreadPoolExecutor(a.workers) as pool:
                results = list(pool.map(run, jobs))
            for (dc, label, _values), (ok, rows, err, params) in results:
                runs += 1
                name = dc["card"]["name"]
                if not ok:
                    failures.append((d["name"], name, params, err))
                elif not rows:
                    empties.add((label, d["name"], name))
                if name == "Stalest source (days)" and ok and rows:
                    freshness = rows[0][0]
            print(f"{d['name']}: {len(jobs)} runs, "
                  f"{sum(1 for _job, (ok, *_rest) in results if not ok)} failed")
        print(f"\n{runs} card runs over {len(dashboards)} dashboards, {len(failures)} failed")
        for f in failures[:40]:
            print("FAIL", f[0], "|", f[1], "|", json.dumps(f[2])[:200], "|", str(f[3])[:300])
        at_defaults = sorted((d, c) for label, d, c in empties if label == "defaults")
        print(f"cards empty at their dashboard's defaults: {len(at_defaults)}")
        for d, c in at_defaults:
            print("  EMPTY", d, "|", c)
        by_label = {}
        for label, _d, _c in empties:
            by_label[label] = by_label.get(label, 0) + 1
        print("empty card runs by picker value (expected for narrow windows and single sources):")
        for label, n in sorted(by_label.items()):
            print(f"  {n:4}  {label}")
        print(f"Stalest source (days): {freshness}")
        return 1 if failures or at_defaults else 0
    finally:
        call(a.base, f"/api/api-key/{key_id}", "DELETE", headers=admin_auth)


if __name__ == "__main__":
    sys.exit(main())
