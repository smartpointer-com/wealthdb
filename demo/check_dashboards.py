#!/usr/bin/env python3
"""Run every dashboard card of a wealthdb Metabase and report failures.

    python3 demo/check_dashboards.py [--base URL] [--password-file PATH]
                                     [--email ADDR] [--first-year YYYY]
                                     [--workers N] [--save-rows FILE]
                                     [--compare FILE]

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
  few values each, the shift at -100 basis points, and the benchmark
  over each built-in name.

The account, category, income type, vehicle and position pickers stay at
their defaults.

A run fails when Metabase reports an error. A response cut off with no
error and no result is asked once more first. A card that returns no
rows at its dashboard's defaults fails too, but for the calendar of
expected payments and the tiles that exist only beside a benchmark, and
so does a figure of the Income dashboards that reads null there. A tile
whose click opens a dashboard that is not there, passes a picker either
side does not carry, or passes a column its card does not return, fails
too. A card states its columns only once it has run, so the column
sources of a card that had not run are counted, not checked. Empty runs
under any other picker value are listed, not failed: many card and
source pairs are empty by nature (card balances for a source with no
card). Exits 1 on any failure. Stdlib only.

--save-rows FILE writes the rows of every run, keyed by dashboard, card
and picker label, with the seconds its query took. The file holds the
book's values: it belongs outside the tree. --compare FILE runs the
same check and then lists each run whose status or rows differ from
FILE's, and fails on any. Two floats match within 1e-13 relative or
1e-9 absolute, the bar two gold builds are held to; any other value
matches exactly. It also lists each tile whose runs took, in all,
SLOWER_RATIO times their time in FILE and SLOWER_FLOOR seconds more.
Two runs compare on the same day: the full window's label holds today's
date, and the relative windows move with it.
"""

import argparse
import concurrent.futures
import datetime as dt
import json
import math
import pathlib
import subprocess
import sys
import time

HERE = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
sys.path.insert(1, str(HERE.parent / "web"))

import metabase  # noqa: E402
import provision  # noqa: E402
from demohouse import config, spec  # noqa: E402

# The time windows each card runs over, before the full history.
# past3years~ reaches before last year: the received figures must count
# days that far back.
WINDOWS = ["past7days~", "past30days~", "past3months~", "past12months~", "past3years~"]
# Cards that may be empty at their defaults: a book whose holdings pay
# nothing over the horizon expects no payment.
EMPTY_AT_DEFAULTS_OK = {"Expected payments", provision.privacy_name("Expected payments"),
                        *provision.BENCHMARK_CARDS, *map(provision.privacy_name, provision.BENCHMARK_CARDS)}
# The dashboards whose figures read 0 where nothing counts, never null.
NO_NULL_DASHBOARDS = provision.INCOME_DASHBOARDS
# --compare: the float bar, relative and absolute, and when a tile's runs
# count as slower: SLOWER_RATIO times their saved seconds and
# SLOWER_FLOOR seconds more, which keeps a fast tile's noise out.
FLOAT_REL, FLOAT_ABS = 1e-13, 1e-9
SLOWER_RATIO, SLOWER_FLOOR = 1.5, 0.25
# The seconds one API call may take, a card's query among them.
TIMEOUT = 300


def mint_key(client, email, password):
    """(the admin's client, the id of a new API key, a client under that
    key) for the Metabase of `client`, logged in as `email`."""
    st, sess = client.post("/api/session", {"username": email, "password": password})
    if st != 200:
        raise SystemExit(f"login failed ({st}): {sess}")
    admin = client.with_auth(session=sess["id"])
    _, groups = admin.get("/api/permissions/group")
    group = next(g["id"] for g in groups if g["name"] == "Administrators")
    name = f"dashboard-check-{dt.datetime.now(dt.timezone.utc):%Y%m%d%H%M%S}"
    st, key = admin.post("/api/api-key", {"name": name, "group_id": group})
    if st != 200:
        raise SystemExit(f"could not create an API key ({st}): {key}")
    return admin, key["id"], client.with_auth(api_key=key["unmasked_key"])


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
        "benchmark": [[n] for n in sorted(json.loads((HERE / "household.json").read_text())["config"]["benchmarks"])],
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


def run_record(dashboard, card, label, ok, rows, err, seconds):
    """One run as --save-rows keeps it."""
    return {"dashboard": dashboard, "card": card, "label": label, "ok": ok,
            "error": None if ok else str(err), "rows": rows, "seconds": round(seconds, 3)}


def save_runs(path, records):
    """Write `records` (run_record) to `path`, sorted by their key."""
    records = sorted(records, key=lambda r: (r["dashboard"], r["card"], r["label"]))
    pathlib.Path(path).write_text(json.dumps({"runs": records}, indent=1, ensure_ascii=False) + "\n",
                                  encoding="utf-8")


def load_runs(path):
    """(dashboard, card, label) -> record, from a --save-rows file."""
    runs = json.loads(pathlib.Path(path).read_text(encoding="utf-8"))["runs"]
    return {(r["dashboard"], r["card"], r["label"]): r for r in runs}


def same_value(a, b):
    """True when two cells match: floats within FLOAT_REL or FLOAT_ABS,
    lists cell by cell, anything else exactly."""
    if isinstance(a, list) and isinstance(b, list):
        return len(a) == len(b) and all(same_value(x, y) for x, y in zip(a, b, strict=True))
    numbers = (int, float)
    if isinstance(a, numbers) and isinstance(b, numbers) and not isinstance(a, bool) \
            and not isinstance(b, bool) and (isinstance(a, float) or isinstance(b, float)):
        return a == b or math.isclose(a, b, rel_tol=FLOAT_REL, abs_tol=FLOAT_ABS)
    return type(a) is type(b) and a == b


def compare_runs(old, new):
    """(dashboard, card, label, what) for each run whose status or rows
    differ between `old` and `new`, both load_runs maps, and each run on
    one side only. A row is named by its place, never by its values."""
    out = []
    for key in sorted(old.keys() | new.keys()):
        a, b = old.get(key), new.get(key)
        if a is None or b is None:
            out.append((*key, "only in the saved runs" if b is None else "not in the saved runs"))
        elif a["ok"] != b["ok"]:
            out.append((*key, "now fails" if a["ok"] else "now runs"))
        elif len(a["rows"]) != len(b["rows"]):
            out.append((*key, f"{len(a['rows'])} rows saved, {len(b['rows'])} now"))
        else:
            diff = [i for i, (x, y) in enumerate(zip(a["rows"], b["rows"], strict=True))
                    if not same_value(x, y)]
            if diff:
                out.append((*key, f"{len(diff)} of {len(a['rows'])} rows differ, the first is row {diff[0]}"))
    return out


def slower_tiles(old, new):
    """(dashboard, card, saved seconds, seconds) for each tile whose
    runs on both sides took SLOWER_RATIO times their saved time and
    SLOWER_FLOOR seconds more, in all."""
    totals = {}
    for key in old.keys() & new.keys():
        t = totals.setdefault(key[:2], [0.0, 0.0])
        t[0] += old[key]["seconds"]
        t[1] += new[key]["seconds"]
    return [(d, c, round(a, 3), round(b, 3)) for (d, c), (a, b) in sorted(totals.items())
            if b > a * SLOWER_RATIO and b - a > SLOWER_FLOOR]


def run_card(client, dash, dc, ptypes, values):
    mapped = {m["parameter_id"]: m for m in dc.get("parameter_mappings", [])}
    body = {"parameters": [{"id": pid, "type": ptypes[pid], "value": v, "target": mapped[pid]["target"]}
                           for pid, v in values.items() if pid in mapped]}
    # A response Metabase cuts off carries no error and no result: the
    # card is asked once more before it counts as failed.
    for _attempt in range(2):
        st, res = client.post(f"/api/dashboard/{dash['id']}/dashcard/{dc['id']}/card/{dc['card_id']}/query",
                              body)
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


def main(argv=None, client=None):
    """Check the Metabase --base names, or `client`, the tests' stand-in."""
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--base", default=f"http://127.0.0.1:{config.WEB_PORT}")
    ap.add_argument("--email", default="admin@wealthdb.local")
    ap.add_argument("--password-file")
    ap.add_argument("--first-year", type=int, default=spec.load().history_start.year)
    ap.add_argument("--workers", type=int, default=3)
    ap.add_argument("--save-rows", metavar="FILE", help="write every run's rows and seconds to FILE")
    ap.add_argument("--compare", metavar="FILE",
                    help="list each run whose rows differ from FILE's, and each tile that got slower")
    a = ap.parse_args(argv)
    saved = load_runs(a.compare) if a.compare else None
    password_file = pathlib.Path(a.password_file) if a.password_file else demo_root() / "web/admin-password.txt"
    password = password_file.read_text().strip()
    admin, key_id, keyed = mint_key(client or metabase.Client(a.base, timeout=TIMEOUT), a.email, password)
    today = dt.datetime.now(dt.timezone.utc).date()
    try:
        coll = keyed.collection(provision.COLLECTION_NAME)
        if coll is None:
            raise SystemExit(f"collection not found: {provision.COLLECTION_NAME}")
        dashboards = sorted(keyed.dashboards(coll["id"]), key=lambda d: d["name"])
        failures, empties, figures, runs, freshness, dashes = [], set(), [], 0, None, {}
        records = []
        for d in dashboards:
            _, dash = keyed.get(f"/api/dashboard/{d['id']}")
            dashes[d["id"]] = dash
            ptypes = {p["id"]: p["type"] for p in dash.get("parameters", [])}
            def offered(slug, dash=dash, d=d):
                """The values the dashboard's `slug` picker offers."""
                p = next((p for p in dash.get("parameters", []) if p["slug"] == slug), None)
                if not p:
                    return []
                _, vals = keyed.get(f"/api/dashboard/{d['id']}/params/{p['id']}/values")
                return sorted(v[0] for v in (vals or {}).get("values", []))

            sources, currencies = offered("source"), offered("currency")
            jobs = []
            for dc in dash.get("dashcards", []):
                if not dc.get("card_id"):
                    continue
                for label, values in combos(dash.get("parameters", []), sources, currencies, a.first_year, today):
                    jobs.append((dc, label, values))
            def run(job, dash=dash, ptypes=ptypes):
                start = time.monotonic()
                result = run_card(keyed, dash, job[0], ptypes, job[2])
                return job, result, time.monotonic() - start

            with concurrent.futures.ThreadPoolExecutor(a.workers) as pool:
                timed = list(pool.map(run, jobs))
            results = [(job, result) for job, result, _seconds in timed]
            for (dc, label, _values), (ok, rows, err, params), seconds in timed:
                runs += 1
                name = dc["card"]["name"]
                records.append(run_record(d["name"], name, label, ok, rows, err, seconds))
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
        if a.save_rows:
            save_runs(a.save_rows, records)
            print(f"rows of {len(records)} runs saved to {a.save_rows}")
        changed = []
        if saved is not None:
            new = {(r["dashboard"], r["card"], r["label"]): r for r in records}
            changed = compare_runs(saved, new)
            print(f"runs that differ from {a.compare}: {len(changed)}")
            for d, c, label, what in changed:
                print("  DIFF", d, "|", c, "|", label, "|", what)
            slower = slower_tiles(saved, new)
            print(f"tiles slower than in {a.compare}: {len(slower)}")
            for d, c, was, now in slower:
                print(f"  SLOWER {d} | {c} | {was}s -> {now}s")
        return 1 if failures or at_defaults or nulls or clicks or changed else 0
    finally:
        admin.delete(f"/api/api-key/{key_id}")


if __name__ == "__main__":
    sys.exit(main())
