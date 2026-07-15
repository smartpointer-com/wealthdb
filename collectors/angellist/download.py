#!/usr/bin/env python3
"""angellist bronze fetcher — browser-based, BYO-cookie authenticated.

Why a browser (not a plain HTTP client): AngelList's venture login is
bot-walled (invisible Turnstile/reCAPTCHA flags the automation stack) and
its GraphQL endpoint requires a per-request `x-al-gql` signature computed
by obfuscated venture-web JS (not a plain hash — see DESIGN.md). So we
can neither log in headlessly nor replay queries browserlessly. Instead
we inject the BYO session cookies that `login` / extract_cookies.py
lifted from a real Firefox, let the venture-web SPA fetch and *sign* its
own GraphQL requests, and capture the responses off the wire.

The injected session lands on the authenticated portal with no
re-challenge. We navigate the LP read-only surfaces and capture the
venture GraphQL the SPA fires:

  ViewerQuery               currentUser: identity + invest accounts
  PortfolioDashboardQuery   portfolio summary
  PositionsTableQuery       funded holdings per SPV / fund
  ActivityQuery             activity feed (unstructured VenturePosts)
  OpenInvestmentsQuery      unfunded commitments
  AccountDocumentsQuery     tax-document list (K-1 + financial-statement URLs)
  InvestmentEntityQuery     the dated funding-account cash ledger + balance
  (+ PositionFiltersQuery, InvestAccountInvestmentEntitiesQuery as fired)

URL shape (slugs derived from ViewerQuery, never hardcoded):
  /v/<userSlug>/i/<investAccountSlug>/portfolio/dashboard
  /v/<userSlug>/i/<investAccountSlug>/commitments
  /v/<userSlug>/i/<investAccountSlug>/taxes-and-documents
  /v/<userSlug>/i/<investAccountSlug>/funding-accounts
where userSlug = currentUser.slug and investAccountSlug =
currentUser.investAccounts[i].slugName.

Bronze layout (collectorkit.bronze conventions):
  $XDG_DATA_HOME/wealthdb/angellist/<UTC-ts>/
    viewer.json        currentUser (identity; PII stays out of the repo)
    captures.jsonl     one line per captured GraphQL exchange:
                       {"op","variables","data"}
    run.json           manifest (status, ops + counts), written LAST —
                       "in-progress" while artefacts serialise, atomically
                       overwritten with "complete" at the end. That
                       terminal status is the signal `prune` keys on to
                       tell a finished dump from a crashed one.

Read-only (CLAUDE.md): navigation + passive capture only. We never click
an invest/commit/fund/settings control, and stay off any lead/admin
surface. `--dry-run` walks the navigation but writes no bronze.
"""
from __future__ import annotations

import argparse
import json
import logging
import re
import sys
import time
from pathlib import Path

from collectorkit import bronze, cli, debugcap

log = logging.getLogger("angellist.download")

VENTURE = "https://venture.angellist.com"
GRAPHQL_RE = re.compile(r"/venture/graphql")

DEFAULT_COOKIES = Path("/secrets/angellist-cookies.json")
DEFAULT_BRONZE_DIR = Path("/data")
DEFAULT_DOCS = Path("/data/angellist-documents")

# Ops we want per invest account, and the route that fires them.
PORTFOLIO_OPS = {
    "PortfolioDashboardQuery", "PositionsTableQuery", "ActivityQuery",
    "PositionFiltersQuery", "InvestAccountInvestmentEntitiesQuery",
}
COMMITMENT_OPS = {"OpenInvestmentsQuery"}
DOCS_OPS = {"AccountDocumentsQuery"}
FUNDING_OPS = {"InvestmentEntityQuery"}

# The positions table is infinite-scroll paginated (~20/page). Scroll
# every scrollable element to the bottom to trigger the next page until
# we've captured all of totalCount.
SCROLL_JS = """() => {
  window.scrollTo(0, document.body.scrollHeight);
  document.querySelectorAll('*').forEach(el => {
    if (el.scrollHeight > el.clientHeight + 80) el.scrollTop = el.scrollHeight;
  });
}"""


def _abs_url(u):
    from urllib.parse import urljoin
    return urljoin(VENTURE, u) if u and u.startswith("/") else u


def _is_incomplete(taxdoc):
    """A tax year is incomplete — re-download every run until it settles —
    when AngelList hasn't marked it 'complete', or fewer K-1s have arrived
    than are expected (k1Count < totalK1Count). This runs through ~Aug of the
    following tax year, after which the year goes 'complete' and is skipped."""
    if taxdoc.get("documentType") != "complete":
        return True
    k1, total = taxdoc.get("k1Count"), taxdoc.get("totalK1Count")
    return isinstance(k1, int) and isinstance(total, int) and k1 < total


def download_documents(cookies, captures, docs_dir, dry_run):
    """Download the tax documents AccountDocumentsQuery lists — the K-1 PDF +
    structured CSV and the quarterly financial statements — into docs_dir,
    saved with the server's own filename (the same names `login`
    produces). Incomplete tax years (documentType != 'complete', or
    k1Count < totalK1Count) are re-fetched every run until they go complete
    (which runs through ~Aug of the following tax year); complete years
    already on disk are skipped.

    Browserless cookie GET — the file endpoints accept it but need a fresher
    session than GraphQL does; if the cookie is too stale they return the
    login page, which we detect and report without failing the run."""
    import gzip
    import urllib.error
    import urllib.request
    from urllib.parse import unquote

    ad = None
    for c in captures:
        if c.get("op") == "AccountDocumentsQuery":
            ad = ((c.get("data") or {}).get("invest") or {}).get("accountDocuments")
    if not ad:
        log.info("documents: no AccountDocumentsQuery captured — skipping")
        return

    targets = []  # (url, incomplete)
    for t in ad.get("taxDocuments", []):
        incomplete = _is_incomplete(t)
        for key in ("csvUrl", "pdfUrl"):
            if t.get(key):
                targets.append((_abs_url(t[key]), incomplete))
    for f in ad.get("financialDocuments", []):
        if f.get("pdfUrl"):
            targets.append((_abs_url(f["pdfUrl"]), False))

    if dry_run:
        log.info("--dry-run: %d document(s) listed (%d in incomplete years); "
                 "none downloaded", len(targets),
                 sum(1 for _, inc in targets if inc))
        return

    docs_dir = Path(docs_dir)
    docs_dir.mkdir(parents=True, exist_ok=True)
    headers = {
        "cookie": "; ".join(f"{c['name']}={c['value']}" for c in cookies),
        "accept": "*/*", "accept-encoding": "gzip",
        "user-agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:135.0) "
                      "Gecko/20100101 Firefox/135.0",
        "referer": f"{VENTURE}/v/",
    }
    fetched = skipped = 0
    for url, incomplete in targets:
        try:
            resp = urllib.request.urlopen(
                urllib.request.Request(url, headers=headers), timeout=120)
            raw = resp.read()
            if resp.headers.get("content-encoding") == "gzip":
                try:
                    raw = gzip.decompress(raw)
                except Exception:
                    pass
            ct = resp.headers.get("content-type", "")
        except urllib.error.HTTPError as e:
            log.warning("doc fetch failed (HTTP %s): %s", e.code, url)
            continue
        except Exception as e:
            log.warning("doc fetch failed (%r): %s", e, url)
            continue
        if "html" in ct.lower():
            log.warning("tax-doc download hit the login wall — the cookie is too "
                        "stale for the file endpoints (GraphQL still worked). Re-run "
                        "`./angellist login` to refresh, then download again. "
                        "(%d fetched before this.)", fetched)
            return
        cd = resp.headers.get("content-disposition", "")
        m = re.search(r"filename\*?=(?:UTF-8'')?\"?([^\";]+)\"?", cd)
        fname = (unquote(m.group(1)).strip() if m
                 else url.rstrip("/").rsplit("/", 1)[-1])
        dest = docs_dir / fname.replace("/", "_")
        if dest.exists() and not incomplete:
            skipped += 1
            continue
        dest.write_bytes(raw)
        fetched += 1
    log.info("documents: %d fetched, %d already-complete skipped -> %s",
             fetched, skipped, docs_dir)


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument("--cookies", type=Path, default=DEFAULT_COOKIES,
                   help="BYO cookie JSON from extract_cookies.py. Default: %(default)s.")
    p.add_argument("--bronze-dir", type=Path, default=DEFAULT_BRONZE_DIR,
                   help="A UTC-timestamped run dir is created here per invocation. "
                        "Default: %(default)s.")
    p.add_argument("--documents-dir", type=Path, default=DEFAULT_DOCS,
                   help="Where to save downloaded tax documents (K-1 CSV/PDF, "
                        "financial statements). Default: %(default)s.")
    p.add_argument("--settle", type=int, default=8,
                   help="Seconds to wait after each navigation for the SPA's "
                        "GraphQL to fire. Default: %(default)s.")
    p.add_argument("--dry-run", action="store_true",
                   help="Navigate + capture but write no bronze (smoke test).")
    p.add_argument("--debug", action="store_true",
                   help="Save opt-in debug captures INSIDE the bronze run dir "
                        "under <run>/screenshots/: the DOM + screenshot of the "
                        "bootstrap landing and of each LP route (portfolio, "
                        "commitments, taxes, funding) as it settles. These say "
                        "what the SPA rendered when an expected GraphQL op "
                        "never fired — captures.jsonl can only show the ops "
                        "that did. Off by default; never read by load, and "
                        "`prune` reclaims <run>/screenshots/. No-op under "
                        "--dry-run / --check, which write no bronze. Distinct "
                        "from the `explore` verb, whose HAR / trace / click log "
                        "land OUTSIDE bronze under /debug.")
    p.add_argument("--check", action="store_true",
                   help="Probe only whether the BYO session is still accepted by "
                        "the server (bootstrap identity, navigate nothing else, "
                        "write nothing); exit 0 if valid, non-zero if stale. "
                        "`login` uses this to decide whether a fresh sign-in is "
                        "needed — a cookie can be unexpired yet server-rejected.")
    p.add_argument(
        # The fleet-wide document opt-out. Skips the tax-document fetches,
        # which dominate the run, so one iteration on the captures needn't
        # pay for them.
        "--no-documents", dest="no_documents", action="store_true",
        help="Skip tax-document capture. The captures and run.json are "
             "still written.")
    cli.add_standard_args(p, verb="download", full_history=True)
    return p.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)

    if not args.check:
        cli.warn_lookback_ignored(args.lookback, log,
                                  what="the full AngelList portfolio snapshot")

    if not args.cookies.is_file():
        log.error("cookie jar not found: %s — run `./angellist login` "
                  "first to lift a session.", args.cookies)
        return 1
    cookies = json.loads(args.cookies.read_text(encoding="utf-8"))
    log.info("loaded %d BYO cookie(s) from %s", len(cookies), args.cookies)

    # The run dir's slug is fixed here rather than after the walk: --debug
    # captures are written while the browser is still open, and they must
    # land in the same dir the artefacts below eventually do.
    run_dir = bronze.run_dir(args.bronze_dir)

    # --dry-run and --check both write no bronze, so neither has a run dir
    # to capture into; --debug degrades to a warning rather than
    # materialising one for diagnostics alone. Otherwise the dir is created
    # up front: a run that captures nothing (a stale session) is exactly the
    # one worth having screenshots of, and it would never reach the write
    # below. Such a dir holds screenshots and no captures.jsonl, so `load`
    # finds nothing to ingest, and its missing terminal run.json makes it
    # non-complete for `prune` — the same lifecycle as a crash.
    debug_dir: Path | None = None
    if args.debug:
        if args.dry_run or args.check:
            log.warning("--debug: --dry-run / --check write no bronze; "
                        "no captures will be written")
        else:
            run_dir.mkdir(parents=True, exist_ok=True)
            debug_dir = run_dir

    import tempfile, shutil
    from camoufox.sync_api import Camoufox

    # Every captured venture/graphql exchange, in arrival order.
    captures: list[dict] = []
    latest_by_op: dict[str, dict] = {}

    def on_response(response) -> None:
        try:
            if not GRAPHQL_RE.search(response.url):
                return
            req = response.request
            if req.method != "POST":
                return
            pd = req.post_data
            if not pd:
                return
            body = json.loads(pd)
            for item in (body if isinstance(body, list) else [body]):
                op = item.get("operationName")
                if not op:
                    continue
                try:
                    payload = response.json()
                except Exception:
                    payload = json.loads(response.text())
                entry = {
                    "op": op,
                    "variables": item.get("variables", {}),
                    "data": payload.get("data") if isinstance(payload, dict) else None,
                    "errors": payload.get("errors") if isinstance(payload, dict) else None,
                }
                captures.append(entry)
                latest_by_op[op] = entry
        except Exception as exc:
            log.debug("on_response capture error: %r", exc)

    def wait_for_ops(page, expected: set[str], timeout: int) -> None:
        """Poll until the expected ops are captured or timeout elapses."""
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if expected <= set(latest_by_op):
                return
            page.wait_for_timeout(500)
        missing = expected - set(latest_by_op)
        if missing:
            log.warning("did not observe op(s) %s within %ds (continuing)",
                        sorted(missing), timeout)

    def capture(page, name: str) -> None:
        """Snapshot a route once it has settled. Named so the capture dir
        reads in walk order; a no-op unless --debug supplied a dir."""
        if debug_dir is not None:
            debugcap.capture_page(page, debug_dir, name, log=log)

    profile = Path(tempfile.mkdtemp(prefix="angellist-dl-"))
    try:
        with Camoufox(
            persistent_context=True,
            user_data_dir=str(profile),
            os="macos",
            headless=True,
            humanize=False,
            geoip=True,
            block_webrtc=True,
        ) as context:
            context.add_cookies(cookies)
            context.on("response", on_response)
            page = context.new_page()

            # Bootstrap: authenticated /v/login redirects to the venture
            # home and fires ViewerQuery. Derive the slugs from it.
            log.info("bootstrapping identity via %s/v/login", VENTURE)
            try:
                page.goto(f"{VENTURE}/v/login",
                          wait_until="domcontentloaded", timeout=45_000)
            except Exception as exc:
                log.warning("bootstrap goto failed: %s", exc)
            wait_for_ops(page, {"ViewerQuery"}, args.settle + 12)
            # The identity gate: an expired cookie or a bot wall renders
            # here, and every later route depends on the slugs this page
            # yields, so it is the first thing worth seeing.
            capture(page, "10-bootstrap")

            viewer = latest_by_op.get("ViewerQuery", {}).get("data")
            cu = (viewer or {}).get("currentUser") if viewer else None
            if not cu:
                # Fallback: the SPA may have redirected us straight onto a
                # slug URL even if ViewerQuery wasn't seen.
                m = re.search(r"/v/([^/]+)/i/([^/]+)", page.url)
                if not m:
                    log.error("could not establish identity (no ViewerQuery, "
                              "no slug URL — landed on %s). The session may be "
                              "stale; re-run `./angellist login`.", page.url)
                    return 1
                user_slug, accounts = m.group(1), [{"slugName": m.group(2)}]
            else:
                user_slug = cu.get("slug")
                accounts = cu.get("investAccounts") or []
            log.info("identity: userSlug=%s, %d invest account(s)",
                     user_slug, len(accounts))

            if args.check:
                # The server accepted the cookie (identity established) — the
                # session is live. Nothing to capture or write.
                log.info("session valid — the saved cookie is still accepted; "
                         "no fresh login needed")
                return 0

            # Capture names carry the account's ordinal, not its slug: the
            # walk order is what makes the dir legible, and the log lines
            # already tie each ordinal to a slug.
            for n, acct in enumerate(accounts, start=1):
                aslug = acct.get("slugName")
                if not aslug:
                    log.warning("invest account without slugName, skipping: %s",
                                {k: acct.get(k) for k in ("id", "name")})
                    continue
                base = f"{VENTURE}/v/{user_slug}/i/{aslug}"
                log.info("account %s: capturing portfolio", aslug)
                try:
                    page.goto(f"{base}/portfolio/dashboard",
                              wait_until="domcontentloaded", timeout=45_000)
                except Exception as exc:
                    log.warning("portfolio goto failed for %s: %s", aslug, exc)
                wait_for_ops(page, PORTFOLIO_OPS, args.settle + 14)

                # Positions are infinite-scroll paginated; scroll until every
                # page (up to totalCount) has been captured.
                def pos_state():
                    ids, total, more = set(), None, True
                    for c in captures:
                        if c["op"] != "PositionsTableQuery":
                            continue
                        conn = (((c.get("data") or {}).get("invest") or {})
                                .get("portfolio", {}) or {}).get("positions") or {}
                        total = conn.get("totalCount", total)
                        more = conn.get("pageInfo", {}).get("hasNextPage", more)
                        for e in conn.get("edges", []):
                            nid = (e.get("node") or {}).get("id")
                            if nid:
                                ids.add(nid)
                    return len(ids), total, more

                last, stale = 0, 0
                for _ in range(60):
                    got, total, more = pos_state()
                    if (total and got >= total) or (not more and got > 0):
                        break
                    if got == last:
                        stale += 1
                        if stale >= 5:
                            break
                    else:
                        stale, last = 0, got
                    try:
                        page.evaluate(SCROLL_JS)
                    except Exception:
                        pass
                    page.wait_for_timeout(1300)
                got, total, more = pos_state()
                log.info("account %s: positions captured %d/%s (hasNextPage=%s)",
                         aslug, got, total, more)
                # Taken after the scroll loop, so the DOM shows the table as
                # it finally settled — the evidence for a stall that stopped
                # short of totalCount.
                capture(page, f"20-acct{n}-portfolio")

                log.info("account %s: capturing commitments", aslug)
                try:
                    page.goto(f"{base}/commitments",
                              wait_until="domcontentloaded", timeout=45_000)
                except Exception as exc:
                    log.warning("commitments goto failed for %s: %s", aslug, exc)
                wait_for_ops(page, COMMITMENT_OPS, args.settle + 8)
                capture(page, f"30-acct{n}-commitments")

                log.info("account %s: capturing tax-document list", aslug)
                try:
                    page.goto(f"{base}/taxes-and-documents",
                              wait_until="domcontentloaded", timeout=45_000)
                except Exception as exc:
                    log.warning("taxes goto failed for %s: %s", aslug, exc)
                wait_for_ops(page, DOCS_OPS, args.settle + 8)
                capture(page, f"40-acct{n}-taxes")

                log.info("account %s: capturing funding ledger", aslug)
                try:
                    page.goto(f"{base}/funding-accounts",
                              wait_until="domcontentloaded", timeout=45_000)
                except Exception as exc:
                    log.warning("funding goto failed for %s: %s", aslug, exc)
                # The funding-accounts route loads the funding account detail
                # (InvestmentEntityQuery), whose `transactions` is the full
                # dated cash ledger (deposits / withdrawals / investments /
                # disbursements / refunds) — the source of dated cash flows.
                wait_for_ops(page, FUNDING_OPS, args.settle + 10)
                capture(page, f"50-acct{n}-funding")
    finally:
        shutil.rmtree(profile, ignore_errors=True)

    # Summary
    import collections
    counts = collections.Counter(c["op"] for c in captures)
    errs = [c for c in captures if c.get("errors")]
    log.info("captured %d GraphQL exchange(s): %s", len(captures), dict(counts))
    if errs:
        log.warning("%d exchange(s) returned GraphQL errors: %s",
                    len(errs), sorted({e["op"] for e in errs}))
    if not captures:
        log.error("nothing captured — session likely invalid. Re-run "
                  "`./angellist login`.")
        return 1

    if args.no_documents:
        log.info("--no-documents: skipping the tax-document fetches")
    else:
        download_documents(cookies, captures, args.documents_dir, args.dry_run)

    if args.dry_run:
        log.info("--dry-run: not writing bronze")
        return 0

    # run_dir's slug was fixed before the walk (--debug may already have
    # created it and written captures into it), so this only ensures it
    # exists.
    run_dir.mkdir(parents=True, exist_ok=True)
    # Drop an "in-progress" marker before serialising artefacts, to be
    # atomically overwritten with the terminal manifest below. All GraphQL
    # capture completes in-memory (the `captures` list) before this point,
    # so the in-progress window is brief — it guards a crash during the
    # captures.jsonl write loop: such a dir carries status="in-progress",
    # which `prune` reclaims once quiescent instead of `load` re-ingesting
    # a partial dump. (--dry-run returned above, so it never reaches here
    # and leaves no prunable shell — no status="dry-run" is needed.)
    bronze.atomic_write_json(run_dir / "run.json", {"status": "in-progress"})
    # viewer.json for identity convenience
    if "ViewerQuery" in latest_by_op:
        bronze.atomic_write_json(run_dir / "viewer.json",
                                 latest_by_op["ViewerQuery"]["data"])
    # captures.jsonl: every exchange, append-only
    with open(run_dir / "captures.jsonl", "w", encoding="utf-8") as fp:
        for c in captures:
            fp.write(bronze.canonical_json(c) + "\n")
    # Terminal manifest: written LAST, atomically overwriting the
    # in-progress marker in a single rename. status="complete" is the
    # forward signal `prune` keys on. `had_errors` does NOT gate
    # completeness — a dump with GraphQL errors still finished capturing
    # and is a complete dump.
    bronze.atomic_write_json(run_dir / "run.json", {
        "status": "complete",
        "source": "angellist",
        "venture_host": VENTURE,
        "captured_at": run_dir.name,
        "ops": dict(counts),
        "had_errors": bool(errs),
    })
    log.info("bronze written: %s", run_dir)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
