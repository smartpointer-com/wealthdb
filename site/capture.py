#!/usr/bin/env python3
"""Screenshot the demo's dashboards for the project page.

    python3 site/capture.py --out DIR --as-of YYYY-MM-DD [--base URL]
                            [--email ADDR] [--password-file PATH]

Runs in the repo's Camoufox image (`make site-shots` starts it there)
against the demo's Metabase. Writes one PNG per dashboard and colour
scheme into DIR: overview.png, overview-dark.png and so on.

Each dashboard opens with every picker set to its default in the URL,
but for the pickers PICKS sets. Metabase otherwise shows the last values
the admin ran it with. A default relative to today, such as "previous 12
months", is pinned to the same window ending at --as-of, the demo's last
day, so a reshoot on a later day shows the same history. The admin's
colour scheme is switched to dark for the second pass and set back to
what it was afterwards, also when a shot fails.
"""

import argparse
import datetime as dt
import pathlib
import re
import sys
import urllib.parse

REPO = pathlib.Path(__file__).resolve().parent.parent
sys.path.insert(0, str(REPO / "demo"))
sys.path.insert(1, str(REPO / "web"))
from demohouse import config  # noqa: E402
import metabase  # noqa: E402
import provision  # noqa: E402

# The dashboards the page shows, by their name in Metabase, and the file
# stem each one's shots take.
DASHBOARDS = {
    "Wealth Overview": "overview",
    "Allocation": "allocation",
    "Returns": "returns",
    "Exposure": "exposure",
    "Yield": "yield",
    "Income": "income",
    "Spending": "spending",
    "Cash Flow": "cashflow",
}
SCHEMES = {"light": "", "dark": "-dark"}

# The pickers a dashboard's shots set away from their defaults, by slug:
# these two show a benchmark beside the household.
BENCHMARK_PICK = {"benchmark": "Global 60/40"}
PICKS = {"Returns": BENCHMARK_PICK, "Exposure": BENCHMARK_PICK}

# A 1440-pixel-wide window at twice the density, cut below Metabase's own
# header bar, which carries nothing the page needs.
VIEWPORT = {"width": 1440, "height": 1250}
HEADER = 72
SETTLE_MS = 8000


def month_start(day, back=0):
    """The first day of the month `back` months before day's."""
    n = day.year * 12 + day.month - 1 - back
    return dt.date(n // 12, n % 12 + 1, 1)


def pin_date(value, as_of):
    """A date picker's relative value as the same window ending at as_of.

    Metabase's "past12months~" is the 12 whole months before this one
    plus this one; without the ~, this month is left out. Days work the
    same way: "past7days~" is the 7 days before as_of plus as_of itself.
    "thisday" is the day itself. A value that is not relative passes
    unchanged. A relative value of another kind stops the capture, so a
    new picker is pinned by hand rather than shot against today.
    """
    if value == "thisday":
        return as_of.isoformat()
    m = re.fullmatch(r"past(\d+)(day|month)s(~?)", value)
    if not m:
        if re.fullmatch(r"(this|past|next|last)\w*~?", value):
            raise SystemExit(f"capture: cannot pin the date picker value {value!r}")
        return value
    n, unit, current = int(m.group(1)), m.group(2), m.group(3) == "~"
    if unit == "day":
        start = as_of - dt.timedelta(days=n)
        end = as_of if current else as_of - dt.timedelta(days=1)
    else:
        start = month_start(as_of, n)
        end = as_of if current else month_start(as_of) - dt.timedelta(days=1)
    return f"{start.isoformat()}~{end.isoformat()}"


def defaults_query(parameters, as_of, picks=None):
    """The query string that opens a dashboard at its pickers' defaults,
    or at the value `picks` names for a slug.

    A picker with no default gets an empty value, which clears whatever
    value Metabase remembered for it. A date picker's default is pinned
    to as_of (pin_date).
    """
    unknown = sorted(set(picks or ()) - {p["slug"] for p in parameters})
    if unknown:
        raise SystemExit(f"capture: no picker {', '.join(unknown)}")
    pairs = []
    for p in parameters:
        default = p.get("default")
        if p["slug"] in (picks or {}):
            pairs.append((p["slug"], picks[p["slug"]]))
        elif default is None:
            pairs.append((p["slug"], ""))
        elif isinstance(default, list):
            pairs.extend((p["slug"], str(v)) for v in default)
        elif p.get("type", "").startswith("date"):
            pairs.append((p["slug"], pin_date(str(default), as_of)))
        else:
            pairs.append((p["slug"], str(default)))
    return urllib.parse.urlencode(pairs)


def shot_name(dashboard, scheme):
    """The file name of a dashboard's shot in a colour scheme."""
    return f"{DASHBOARDS[dashboard]}{SCHEMES[scheme]}.png"


def answer(reply):
    """The body of a Metabase `reply`, (status, body). Any status but a
    success stops the capture, so no shot is taken over a failed call."""
    st, body = reply
    if not 200 <= st < 300:
        raise SystemExit(f"capture: Metabase answered {st}: {body}")
    return body


def find_dashboards(client):
    """{name: dashboard} for the page's dashboards in the pre-defined collection."""
    try:
        coll = client.collection(provision.COLLECTION_NAME)
        if coll is None:
            raise SystemExit(f"capture: collection not found: {provision.COLLECTION_NAME}")
        found = {d["name"]: d["id"] for d in client.dashboards(coll["id"]) if d["name"] in DASHBOARDS}
    except metabase.Error as e:
        raise SystemExit(f"capture: {e}: {e.body}") from None
    missing = sorted(set(DASHBOARDS) - set(found))
    if missing:
        raise SystemExit(f"capture: dashboards not found: {', '.join(missing)}")
    return {name: answer(client.get(f"/api/dashboard/{did}")) for name, did in found.items()}


def capture(client, dashboards, out, as_of):
    """Shoot every dashboard in both colour schemes into out."""
    from camoufox.sync_api import Camoufox

    base, session = client.base, client.session
    host = urllib.parse.urlparse(base).hostname
    previous = answer(client.get("/api/session/properties")).get("color-scheme")
    try:
        with Camoufox(headless=True) as browser:
            for scheme in SCHEMES:
                answer(client.put("/api/setting/color-scheme", {"value": scheme}))
                ctx = browser.new_context(viewport=VIEWPORT, device_scale_factor=2)
                ctx.add_cookies([{"name": "metabase.SESSION", "value": session, "domain": host, "path": "/"}])
                page = ctx.new_page()
                for name, dash in dashboards.items():
                    query = defaults_query(dash.get("parameters", []), as_of, PICKS.get(name))
                    url = f"{base}/dashboard/{dash['id']}?{query}"
                    page.goto(url, wait_until="networkidle", timeout=180000)
                    page.wait_for_timeout(SETTLE_MS)
                    welcome = page.get_by_role("button", name="Start exploring")
                    if welcome.count():
                        welcome.first.click()
                        page.wait_for_timeout(1500)
                    clip = {"x": 0, "y": HEADER, "width": VIEWPORT["width"], "height": VIEWPORT["height"] - HEADER}
                    page.screenshot(path=str(out / shot_name(name, scheme)), clip=clip)
                    print(f"capture: {shot_name(name, scheme)}", flush=True)
                ctx.close()
    finally:
        answer(client.put("/api/setting/color-scheme", {"value": previous or "auto"}))


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--out", required=True, type=pathlib.Path)
    ap.add_argument("--as-of", required=True, type=dt.date.fromisoformat,
                    help="the demo's last day; relative date pickers end on it")
    ap.add_argument("--base", default=f"http://host.docker.internal:{config.WEB_PORT}")
    ap.add_argument("--email", default="admin@wealthdb.local")
    ap.add_argument("--password-file", required=True, type=pathlib.Path)
    a = ap.parse_args(argv)
    password = a.password_file.read_text().strip()
    client = metabase.Client(a.base, timeout=60)
    session = answer(client.post("/api/session", {"username": a.email, "password": password}))["id"]
    client = client.with_auth(session=session)
    try:
        capture(client, find_dashboards(client), a.out, a.as_of)
    finally:
        answer(client.delete("/api/session"))


if __name__ == "__main__":
    main()
