#!/usr/bin/env python3
"""Screenshot the demo's dashboards for the project page.

    python3 site/capture.py --out DIR --as-of YYYY-MM-DD [--base URL]
                            [--email ADDR] [--password-file PATH]

Runs in the repo's Camoufox image (`make site-shots` starts it there)
against the demo's Metabase. Writes one PNG per dashboard and colour
scheme into DIR: overview.png, overview-dark.png and so on.

Each dashboard opens with every picker set to its default in the URL.
Metabase otherwise shows the last values the admin ran it with. A
default relative to today, such as "previous 12 months", is pinned to
the same window ending at --as-of, the demo's last day, so a reshoot on
a later day shows the same history. The
admin's colour scheme is switched to dark for the second pass and set
back to what it was afterwards, also when a shot fails.
"""

import argparse
import datetime as dt
import json
import re
import pathlib
import sys
import urllib.parse
import urllib.request

REPO = pathlib.Path(__file__).resolve().parent.parent
sys.path.insert(0, str(REPO / "demo"))
sys.path.insert(1, str(REPO / "web"))
from demohouse import config  # noqa: E402
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
    plus this one; without the ~, this month is left out. "thisday" is
    the day itself. A value that is not relative passes unchanged. A
    relative value of another kind stops the capture, so a new picker
    is pinned by hand rather than shot against today.
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
        start = as_of - dt.timedelta(days=n - (1 if current else 0))
        end = as_of if current else as_of - dt.timedelta(days=1)
    else:
        start = month_start(as_of, n)
        end = as_of if current else month_start(as_of) - dt.timedelta(days=1)
    return f"{start.isoformat()}~{end.isoformat()}"


def defaults_query(parameters, as_of):
    """The query string that opens a dashboard at its pickers' defaults.

    A picker with no default gets an empty value, which clears whatever
    value Metabase remembered for it. A date picker's default is pinned
    to as_of (pin_date).
    """
    pairs = []
    for p in parameters:
        default = p.get("default")
        if default is None:
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


def call(base, path, method="GET", body=None, session=None):
    """One Metabase API call; returns the decoded JSON body, or None."""
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(base + path, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if session:
        req.add_header("X-Metabase-Session", session)
    with urllib.request.urlopen(req, timeout=60) as r:
        raw = r.read()
    return json.loads(raw) if raw else None


def find_dashboards(base, session):
    """{name: dashboard} for the page's dashboards in the pre-defined collection."""
    cols = call(base, "/api/collection", session=session)
    coll = next(c for c in cols if c.get("name") == provision.COLLECTION_NAME and not c.get("archived"))
    items = call(base, f"/api/collection/{coll['id']}/items?models=dashboard", session=session)
    found = {d["name"]: d["id"] for d in items["data"] if d["name"] in DASHBOARDS}
    missing = sorted(set(DASHBOARDS) - set(found))
    if missing:
        raise SystemExit(f"capture: dashboards not found: {', '.join(missing)}")
    return {name: call(base, f"/api/dashboard/{did}", session=session) for name, did in found.items()}


def capture(base, session, dashboards, out, as_of):
    """Shoot every dashboard in both colour schemes into out."""
    from camoufox.sync_api import Camoufox

    host = urllib.parse.urlparse(base).hostname
    previous = call(base, "/api/session/properties", session=session).get("color-scheme")
    try:
        with Camoufox(headless=True) as browser:
            for scheme in SCHEMES:
                call(base, "/api/setting/color-scheme", "PUT", {"value": scheme}, session)
                ctx = browser.new_context(viewport=VIEWPORT, device_scale_factor=2)
                ctx.add_cookies([{"name": "metabase.SESSION", "value": session, "domain": host, "path": "/"}])
                page = ctx.new_page()
                for name, dash in dashboards.items():
                    url = f"{base}/dashboard/{dash['id']}?{defaults_query(dash.get('parameters', []), as_of)}"
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
        call(base, "/api/setting/color-scheme", "PUT", {"value": previous or "auto"}, session)


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
    session = call(a.base, "/api/session", "POST", {"username": a.email, "password": password})["id"]
    try:
        capture(a.base, session, find_dashboards(a.base, session), a.out, a.as_of)
    finally:
        call(a.base, "/api/session", "DELETE", session=session)


if __name__ == "__main__":
    main()
