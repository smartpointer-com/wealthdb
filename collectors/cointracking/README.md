# cointracking

A read-only scraper for [cointracking.info](https://cointracking.info)
— the aggregator portal that tracks the user's full crypto portfolio
and transaction history across centralised exchanges + on-chain
wallets. **No first-order crypto integration** lives in wealthdb;
cointracking.info is the single source of truth for crypto, the
same way the per-bank collectors are for fiat.

cointracking.info has no public API. The collector replays the
browser flow (TOTP 2FA on first login; the device-trust cookie
persists for years afterwards) by driving headless Firefox via
Playwright.

Part of the **wealthdb** suite — see
[the architecture overview](../../ARCHITECTURE.md) for the bronze →
silver → gold model and [collectors/README.md](../README.md) for
shared collector conventions.

## Status

| Verb | Status | Notes |
| --- | --- | --- |
| `login`    | implemented | Headless Playwright Firefox, CLI-MFA on stdin, persistent profile. |
| `download` | implemented | Per-portfolio SPA loop, 19-column trade CSV + balance CSV per portfolio. |
| `load`     | implemented | DuckDB silver, aggregate-then-window holdings replay with incremental upsert + balance reconciliation. |
| `explore`  | implemented | Discovery harness (Camoufox + VNC + HAR + trace + click log). Kept around for re-discovery if cointracking changes their UI. |

Per the user's note: the device-trust cookie is multi-year, so once
`login` has been run once the collector slots into
`wealthdb-nightly` like the rest — one MFA prompt every few years,
otherwise unattended.

## Operational quick start

```sh
# 1. Build the image (~3-5 min first time; the base-camoufox image
#    holds the pre-fetched Firefox so this step is fast on a warm cache).
./cointracking build

# 2. Drop credentials into the env file. chmod 0600 enforced by login.py.
#    cat > ~/.secrets/cointracking.env <<'EOF'
#    COINTRACKING_USERNAME=your-cointracking-email
#    COINTRACKING_PASSWORD=your-cointracking-password
#    EOF

# 3. Mint the session. Headless Firefox; prompts for the 6-digit TOTP
#    code on stdin once, ticks "Don't ask again" for the multi-year
#    device-trust cookie. Subsequent runs short-circuit without firing
#    a 2FA push.
./cointracking login

# 4. Pull a fresh bronze dump. ~15 s for 5 portfolios.
./cointracking download

# 5. Ingest into the DuckDB silver. Computes daily holdings from the
#    full transaction history via the aggregate-then-window replay;
#    only rewrites positions_daily from the first deviation day
#    onwards. Reconciles final balances vs the balance CSV per
#    portfolio — warnings (not failures) for any discrepancies above
#    the 8-decimal export precision.
./cointracking load

# 6. Cron / launchd: a nightly `./cointracking download && ./cointracking load`
#    is unattended for the multi-year lifetime of the device-trust cookie.
```

Probe the session without firing a 2FA push:

```sh
./cointracking login --check    # exits 0 if session is valid, 1 if not
./cointracking download --dry-run    # walks navigation, skips exports
./cointracking load --replay-only    # re-runs the holdings replay only
./cointracking load --force          # re-ingest snapshots already in dump_runs
```

## Re-discovery: when cointracking changes their UI

The `explore` subcommand is kept around for the next time
cointracking changes a selector that the headless flow depends on
(e.g. the "Extended with additional columns" mode dropdown, or the
"Don't ask again" checkbox). It launches Camoufox in the
container's Xvfb display and exposes a VNC port so the operator
can drive the live browser; meanwhile it records HAR, Playwright
trace, click log, and any blob downloads:

```sh
./cointracking explore --fresh      # --fresh wipes the profile so 2FA is forced
#  explore: VNC ready on 127.0.0.1:<port>
#  explore: password (single-use):  <16 hex chars>
#  explore: tunnel from your laptop with
#    ssh -L <port>:127.0.0.1:<port> <host>
#  explore: then on the laptop:
#    open vnc://localhost:<port>
```

Artefacts land under `$HOME/.cache/cointracking-debug/<UTC-ts>/`
(network.jsonl + trace-chunks/ + clicks.jsonl + downloads/).
Close the browser window OR Ctrl-C the container — either path
flushes everything to disk.

Override host mounts via env: `COINTRACKING_SECRETS_DIR`,
`COINTRACKING_DATA_DIR`, `COINTRACKING_DEBUG_DIR`.

## Read-only

See [CLAUDE.md](CLAUDE.md). cointracking.info exposes mutation
surfaces (add transactions by hand, edit address book entries,
delete imports, change account settings). This toolkit is
**read-only** — it only navigates, filters, and exports.
