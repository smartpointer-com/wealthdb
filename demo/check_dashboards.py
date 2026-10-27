#!/usr/bin/env python3
"""Run every dashboard card of a wealthdb Metabase and report failures.

    python3 demo/check_dashboards.py [--base URL] [--password-file PATH]
                                     [--email ADDR] [--first-year YYYY]
                                     [--workers N]

The defaults are the demo's: its port, its admin password file under
the checkout's demo root (shared/wrappers/demo-root.sh), and the year
its history starts.

Logs in once to mint a temporary API key, runs every dashcard of every
dashboard in the 'wealthdb (pre-defined)' collection through the
dashboard query endpoint, then deletes the key. Queries run under the
key rather than the admin's session because Metabase remembers the last
filter values a user ran a dashboard with and shows them to that user
next time.

Each card runs at its dashboard's defaults, once per time window with no
source picked, and once per source over the full history. The other
pickers are varied one at a time around their defaults:

- a picker with a fixed list of values, such as the currency, the
  investing grain, the reading of a missing cost basis, and the
  projection's rate path, maturing rule and horizon, over each value it
  offers but its default;
- a currency picker without such a list over each currency it offers;
- the section, start year, as-of day, asset class and tax wrapper over a
  few values each, and the shift at -100 basis points.

The account, category, income type, vehicle and position pickers stay at
their defaults.

A run fails when Metabase reports an error. A response cut off with no
error and no result is asked once more first. A card that returns no rows
at its dashboard's defaults fails too, but for the calendar of expected
payments, and so does a figure of the Income dashboards that reads null
there. A tile whose click opens a dashboard that is not there, passes
a picker either side does not carry, or passes a column its card does
not return, fails too. A card states its columns only once it has run,
so the column sources of a card that had not run are counted, not
checked. Empty runs under
any other picker value are listed, not failed: many card and source
pairs are empty by nature (card balances for a source with no card).
Exits 1 on any failure. Stdlib only.
"""

import argparse
import concurrent.futures
import datetime as dt
import json
import pathlib
import subprocess
import sys
import urllib.error
import urllib.request

HERE = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
sys.path.insert(1, str(HERE.parent / "web"))

import provision  # noqa: E402
from demohouse import config, spec  # noqa: E402

# The time windows each card runs over, before the full history.
# past3years~ reaches before last year: the received figures must count
# days that far back.
WINDOWS = ["past7days~", "past30days~", "past3months~", "past12months~", "past3years~"]
# Cards that may be empty at their defaults: a book whose holdings pay
# nothing over the horizon expects no payment.
EMPTY_AT_DEFAULTS_OK = {"Expected payments", provision.privacy_name("Expected payments")}
# The dashboards whose figures read 0 where nothing counts, never null.
NO_NULL_DASHBOARDS = provision.INCOME_DASHBOARDS


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
    picker varied alone around the defaults. A picker with a fixed list
    runs over its values but its default; `currencies` is what a Currency
    picker without one offers."""
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
            out.append((f"time_range {w}", {**base, time["id"]: w}))
    if source:
        for s in sources:
            v = {**base, source["id"]: [s]}
            if time:
                v[time["id"]] = full
            out.append((f"source {s}", v))
    variations = {
        "currency": [[c] for c in currencies],
        "section": [["operating_in"], ["operating_out"], ["investing"], ["financing"], ["vehicles"]],
        "start_year": [[y] for y in range(first_year, today.year + 1)],
        "as_of_day": [f"{first_year + 1}-12-31", today.isoformat()],
        "asset_class": [["public_equity"]],
        "tax_wrapper": [["taxable_personal"]],
        "shift": [[-100]],
    }
    for p in params:
        if p.get("values_source_type") == "static-list":
            values = [[v] for v in p["values_source_config"]["values"] if [v] != p.get("default")]
        else:
            values = variations.get(p["slug"], [])
        for v in values:
            out.append((f"{p['slug']} {v}", {**base, p["id"]: v}))
    return out


def empty_at_defaults(empties):
    """The (dashboard, card) pairs of `empties`, (label, dashboard, card)
    runs that returned no rows, that are empty at their dashboard's
    defaults and may not be."""
    return sorted((d, c) for label, d, c in empties if label == "defaults" and c not in EMPTY_AT_DEFAULTS_OK)


def null_at_defaults(figures):
    """The (dashboard, card) pairs of `figures`, (label, dashboard, card,
    rows) runs of a scalar card, that read null at their dashboard's
    defaults on one of NO_NULL_DASHBOARDS."""
    return sorted((d, c) for label, d, c, rows in figures
                  if label == "defaults" and d in NO_NULL_DASHBOARDS and rows and rows[0] and rows[0][0] is None)


def broken_clicks(dashes):
    """The (dashboard, card, reason) of each tile in `dashes`, dashboard
    id -> its JSON, whose click opens a dashboard that is not there,
    passes a picker either side does not carry, or passes a column the
    card does not return."""
    params = {i: {p["id"] for p in d.get("parameters", [])} for i, d in dashes.items()}
    out = []
    for i, d in sorted(dashes.items()):
        for dc in d.get("dashcards", []):
            cb = (dc.get("visualization_settings") or {}).get("click_behavior") or {}
            if cb.get("type") != "link" or cb.get("linkType") != "dashboard":
                continue
            name = (dc.get("card") or {}).get("name")
            target = cb.get("targetId")
            if target not in dashes:
                out.append((d["name"], name, f"opens dashboard {target}, which is not there"))
                continue
            for tid, m in sorted((cb.get("parameterMapping") or {}).items()):
                src = m.get("source") or {}
                if tid not in params[target]:
                    out.append((d["name"], name, f"sets {tid}, which {dashes[target]['name']} does not carry"))
                elif src.get("type") == "parameter" and src.get("id") not in params[i]:
                    out.append((d["name"], name, f"passes {src.get('id')}, which it does not carry"))
                elif src.get("type") == "column" and (dc.get("card") or {}).get("result_metadata") and \
                        src.get("id") not in {c.get("name") for c in dc["card"]["result_metadata"]}:
                    out.append((d["name"], name, f"passes column {src.get('id')}, which the card does not return"))
    return out


def unchecked_columns(dashes):
    """How many column sources of the clicks in `dashes`, dashboard id ->
    its JSON, broken_clicks cannot check. Metabase states a card's
    columns once it has run; before that there is nothing to check the
    column against."""
    return sum(1 for d in dashes.values() for dc in d.get("dashcards", [])
               if not (dc.get("card") or {}).get("result_metadata")
               for m in ((((dc.get("visualization_settings") or {}).get("click_behavior") or {})
                          .get("parameterMapping") or {}).values())
               if (m.get("source") or {}).get("type") == "column")


def run_card(base, auth, dash, dc, ptypes, values):
    mapped = {m["parameter_id"]: m for m in dc.get("parameter_mappings", [])}
    body = {"parameters": [{"id": pid, "type": ptypes[pid], "value": v, "target": mapped[pid]["target"]}
                           for pid, v in values.items() if pid in mapped]}
    # A response Metabase cuts off carries no error and no result: the
    # card is asked once more before it counts as failed.
    for _attempt in range(2):
        st, res = call(base, f"/api/dashboard/{dash['id']}/dashcard/{dc['id']}/card/{dc['card_id']}/query",
                       "POST", body, auth)
        ok = st in (200, 202) and isinstance(res, dict) and res.get("status") == "completed" and "error" not in res
        if ok or (isinstance(res, dict) and res.get("error")):
            break
    rows = (res.get("data") or {}).get("rows", []) if isinstance(res, dict) else []
    err = None if ok else (res.get("error") if isinstance(res, dict) else res) or "no result"
    return ok, rows, err, body["parameters"]


def demo_root() -> pathlib.Path:
    """The checkout's demo root, as the Makefile resolves it."""
    repo = pathlib.Path(__file__).resolve().parent.parent
    out = subprocess.run(["sh", "-c", '. "$1/shared/wrappers/demo-root.sh" && wealthdb_demo_root "$1"', "sh",
                          str(repo)], capture_output=True, text=True)
    if out.returncode != 0:
        sys.exit(out.stderr.strip() or "no demo root")
    return pathlib.Path(out.stdout.strip()).expanduser()


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--base", default=f"http://127.0.0.1:{config.WEB_PORT}")
    ap.add_argument("--email", default="admin@wealthdb.local")
    ap.add_argument("--password-file")
    ap.add_argument("--first-year", type=int, default=spec.load().history_start.year)
    ap.add_argument("--workers", type=int, default=3)
    a = ap.parse_args(argv)
    password_file = pathlib.Path(a.password_file) if a.password_file else demo_root() / "web/admin-password.txt"
    password = password_file.read_text().strip()
    admin_auth, key_id, auth = mint_key(a.base, a.email, password)
    today = dt.datetime.now(dt.timezone.utc).date()
    try:
        _, cols = call(a.base, "/api/collection", headers=auth)
        coll = next(c for c in cols if c.get("name") == provision.COLLECTION_NAME and not c.get("archived"))
        _, items = call(a.base, f"/api/collection/{coll['id']}/items?models=dashboard", headers=auth)
        dashboards = sorted(items["data"], key=lambda d: d["name"])
        failures, empties, figures, runs, freshness, dashes = [], set(), [], 0, None, {}
        for d in dashboards:
            _, dash = call(a.base, f"/api/dashboard/{d['id']}", headers=auth)
            dashes[d["id"]] = dash
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
                elif dc["card"].get("display") == "scalar":
                    figures.append((label, d["name"], name, rows))
                if name == "Stalest source (days)" and ok and rows:
                    freshness = rows[0][0]
            print(f"{d['name']}: {len(jobs)} runs, "
                  f"{sum(1 for _job, (ok, *_rest) in results if not ok)} failed")
        print(f"\n{runs} card runs over {len(dashboards)} dashboards, {len(failures)} failed")
        for f in failures[:40]:
            print("FAIL", f[0], "|", f[1], "|", json.dumps(f[2])[:200], "|", str(f[3])[:300])
        at_defaults = empty_at_defaults(empties)
        print(f"cards empty at their dashboard's defaults: {len(at_defaults)}")
        for d, c in at_defaults:
            print("  EMPTY", d, "|", c)
        nulls = null_at_defaults(figures)
        print(f"figures null at their dashboard's defaults: {len(nulls)}")
        for d, c in nulls:
            print("  NULL", d, "|", c)
        by_label = {}
        for label, _d, _c in empties:
            by_label[label] = by_label.get(label, 0) + 1
        print("empty card runs by picker value (expected for narrow windows and single sources):")
        for label, n in sorted(by_label.items()):
            print(f"  {n:4}  {label}")
        print(f"Stalest source (days): {freshness}")
        clicks = broken_clicks(dashes)
        print(f"clicks to a dashboard that do not land: {len(clicks)}")
        for d, c, why in clicks:
            print("  CLICK", d, "|", c, "|", why)
        print(f"clicks not checked against columns: {unchecked_columns(dashes)}")
        return 1 if failures or at_defaults or nulls or clicks else 0
    finally:
        call(a.base, f"/api/api-key/{key_id}", "DELETE", headers=admin_auth)


if __name__ == "__main__":
    sys.exit(main())
