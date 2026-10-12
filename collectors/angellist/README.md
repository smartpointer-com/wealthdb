# angellist

## ⚠️ Security & liability disclaimer

> [!WARNING]
> **This collector impersonates a human browser user and holds fully
> privileged financial-account credentials. Read this disclaimer in full
> before configuring any credential.**

This collector **impersonates a human user**: it drives a real,
stealth-hardened browser session that signs in to AngelList with your
credentials and your multi-factor confirmations. The session it holds is
**fully privileged** — the same login a human uses to move money — and
AngelList offers no read-only sub-scope, so nothing but this codebase's
own discipline restricts the session to reading. If malicious code were
ever introduced into this repository, its dependency chain, or the
container images it runs, it could act on your accounts with your full
authority and cause **irreversible financial damage, up to the total loss
of the assets reachable from those credentials**.

**You are solely responsible for a thorough, independent security audit**
of this code, its dependency chain, and its runtime images **before**
entrusting it with credentials, and again after every update or rebuild.
If you cannot perform such an audit, do not hand this software real
credentials. Automated access may additionally breach AngelList's terms of
service; verifying that your use is permitted is likewise your
responsibility.

**No warranty; no liability.** This software is provided “AS IS”, without
warranty of any kind, express or implied, including but not limited to the
implied warranties of merchantability, fitness for a particular purpose,
title, and non-infringement. To the maximum extent permitted by applicable
law, **SmartPointer AG and the contributors accept no responsibility for,
and shall not be liable for, any claim, damages, or other liability** —
whether in an action of contract, tort, or otherwise — arising from, out
of, or in connection with this software or its use, including without
limitation unauthorized or erroneous transactions, loss of funds or other
assets, credential or data compromise, account suspension or termination,
and any direct, indirect, incidental, special, consequential, or punitive
damages. Your use is entirely at your own risk. See
[LICENSE](../../LICENSE) for the governing terms. This software is not
affiliated with, endorsed by, or sponsored by AngelList or any other
financial institution; nothing in this repository is financial, legal, or
tax advice.

A read-only collector for the [AngelList](https://angellist.com) venture
investor portal. It reads the **limited-partner** book of SPVs and fund
deals:

- per vehicle: commitment, capital called (contributed), invested,
  distributions (realized) and fair value;
- portfolio totals (IRR / TVPI / DPI) and unfunded commitments;
- the funding account's dated cash ledger;
- the tax documents (K-1 CSV/PDF, fund financial statements).

AngelList's public API (`docs.angellist.com`) serves fund admins and GPs,
not LPs. The LP web login sits behind an invisible Turnstile/reCAPTCHA
challenge that flags the Camoufox automation fingerprint. So the
collector uses a **bring-your-own-cookie** flow:

1. A by-hand login in a genuine Firefox over VNC clears the challenge.
2. The collector lifts that session cookie.
3. A headless Camoufox with the cookie drives the venture SPA and
   captures the GraphQL it fetches.

See [DESIGN.md](DESIGN.md) for the mechanism.

Part of the **wealthdb** suite. See
[the architecture overview](../../DESIGN.md) and
[collectors/README.md](../README.md).

## Verbs

| Verb | What it does |
| --- | --- |
| `login` | The auth path. If the saved Firefox profile holds an unexpired session cookie, a headless probe asks the server whether it still works. A live session is lifted to `~/.secrets/angellist-cookies.json` and `login` exits with no VNC. Otherwise it opens stock Mozilla Firefox under VNC for a by-hand login (+2FA). On a clean Firefox close, the cookie is lifted. The session lasts about 27 days. There is no unattended login. `--check` probes only; `--fresh` skips the probe and always opens Firefox. K-1 or financial documents downloaded by hand in that Firefox save to `angellist-documents/` in the data dir. |
| `download` | Headless Camoufox with the injected cookie drives the venture SPA and captures its GraphQL: positions, portfolio summary, commitments, the funding-account ledger and the document list. It then fetches the tax documents with the session cookie. It is browser-based because `/venture/graphql` needs a JS-signed `x-al-gql` header. `--no-documents` skips the document fetches, the run's dominant cost. |
| `load` | Parses bronze `captures.jsonl` and the K-1 CSVs and fund statements in `angellist-documents/` into the SQLite silver. |
| `explore` | Camoufox + VNC discovery harness, kept for re-discovery. It writes a redacted HAR and network/click logs under `/debug`. Flags: `--trace`, `--cookies`, `--dump-links`. |
| `prune` | Reclaims bronze disk (see [Reclaiming disk](#reclaiming-disk)). |

The silver tables and their columns are in
[DESIGN.md § Silver](DESIGN.md#silver). The gold adapter under
`wealthdb/internal/silver/angellist/` projects that silver into the
canonical model ([DESIGN.md § Gold mapping](DESIGN.md#gold-mapping)). A
run reads the source once `wealthdb.cfg` lists it under `silver_sources`.

## Quick start

```sh
# 1. Build the image (stock Firefox + Camoufox base; ~1-2 min on warm cache).
./angellist build

# 2. Lift a session. A still-valid saved session exits at once with the
#    cookie refreshed. Otherwise stock Firefox opens under VNC: connect,
#    log in (+2FA), check that the portfolio loads, then CLOSE Firefox.
#    The cookie is extracted on close. Repeat about monthly.
./angellist login
#   (already valid) login: existing AngelList session still valid — cookie lifted to
#   (fresh login)   login: VNC ready on 127.0.0.1:<port>
#                   login: password (single-use):  <password>

# 3. Pull a fresh bronze dump (headless; ~15s; read-only GraphQL).
./angellist download

# 4. Ingest into the SQLite silver.
./angellist load
```

Iterate without a fresh login:

```sh
./angellist download --dry-run    # navigate + capture with the saved cookie, write no bronze
./angellist load --force          # delete silver + rebuild from all bronze
```

`download --dry-run` uses the saved cookie and never signs in. It lists
the tax documents but downloads none.

The host mounts follow the
[shared directory overrides](../README.md#the-wrapper--the-wealthdb-collect-contract).
`ANGELLIST_DEBUG_DIR` moves the `/debug` mount.

`login`, `download` and `load` read no credential. Only `explore` reads
`ANGELLIST_USERNAME` and `ANGELLIST_PASSWORD` from
`~/.secrets/angellist.env`. It uses them to pre-fill the login form and
never submits it (`--no-prefill` turns that off).

Bronze lands under `$XDG_DATA_HOME/wealthdb/angellist/`:

- `<UTC-ts>/` — one run dir per `download` (`captures.jsonl`,
  `viewer.json`, `run.json`);
- `angellist-documents/` — the tax documents, shared by every run;
- `angellist.db` — the silver DB.

### Reclaiming disk

```sh
./angellist prune --dry-run   # print the deletion plan, delete nothing
./angellist prune             # delete it
```

`prune` follows the
[shared run-status lifecycle](../README.md#debug-artefacts-run-status-and-pruning-bronze):

- It deletes whole non-complete run dirs. A `download` that crashed
  leaves `status: "in-progress"` in `run.json`, or no `run.json` at all.
  `load` already skips both.
- From a complete dump it strips only `screenshots/`. That holds the
  per-route DOM and screenshot captures `download --debug` writes. `load`
  never reads them.

`load` takes a run dir whose `run.json` status is `complete` or absent.
It skips a run dir with no `run.json` and any other status. The
`explore` diagnostics land under `/debug`, outside bronze.
`angellist-documents/` and `angellist.db` are not run dirs, so `prune`
never touches them.

## Read-only & PII

See [AGENTS.md](AGENTS.md). The collector only navigates the venture LP
read surfaces and captures the GraphQL the SPA fetches. It never clicks
an invest, commit, fund, e-sign or settings control, and it stays off any
lead or admin surface. The data is highly sensitive: it includes
K-1 details and, on the commitments surface, bank and wire instructions.
It lives only under `$XDG_DATA_HOME/wealthdb/angellist/` and
`~/.secrets/`, never in the repo. Slugs and IDs are derived at runtime,
never hardcoded.
