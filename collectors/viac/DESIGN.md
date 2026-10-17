# viac — design

Design notes for maintaining `login.py`, `download.py` and `load.py`
against the `app.viac.ch` SPA. The silver schema here is also the
contract with the `wealthdb` VIAC adapter. The [README](README.md) covers
the verbs, examples and bronze layout.

Part of the **wealthdb** suite — see [the architecture
overview](../../DESIGN.md) for the bronze → silver → gold
model and [collectors/README.md](../README.md) for shared collector
conventions. This document only covers what's VIAC-specific.

Scope: `download` fetches per-portfolio detail (strategy, positions,
fees) for Pillar-3a portfolios only. PVB and INV portfolios reach silver
`accounts` from the inventory alone (§9).

## 1. Context and non-goals

### 1.1 Why a scraper

VIAC has no retail-accessible read API. The provider supplies a
web SPA at `app.viac.ch` and a companion mobile app; both run on
the same internal backend, but neither's API surface is exposed
for personal aggregation:

- **OpenWealth** — B2B-only. VIAC's parent (WIR Group / Terzo)
  has no published retail OpenWealth participation.
- **PSD2** — Switzerland is outside the EU PSD2 regime.
- **Aggregators** — Plaid / TrueLayer / Tink / Powens / Akoya
  all decline Pillar-3a providers as too niche.
- **Email feeds** — "document available" notifications with no
  payload.

The remaining channel is the SPA's REST surface, which is plain
JSON over cookies (Airlock IAM session cookie + double-submit-
cookie CSRF). The toolkit replays it with httpx — no browser
needed for the operational path.

### 1.2 Account composition

VIAC's `/rest/web/wealth/portfolio-inventory` returns three
arrays:

- `p3a[]` — Pillar-3a portfolios. Per-portfolio strategy /
  positions / fees endpoints exist.
- `pvb[]` — Pillar-2 vested-benefits portfolios. Typically
  PASSIVE while the holder is employed; the per-portfolio
  endpoint surface is unmapped (see §9).
- `inv[]` — VIAC's non-retirement investment product line.
  Empty for a customer with retirement products only.

The first dotted segment of each portfolio number encodes the
product line: `3.*` is p3a, `2.*` is pvb, `1.*` is inv.

### 1.3 Non-goals

- **Real-time / near-real-time pull.** SMS mTAN gates every
  fresh login.
- **Strategy changes, contributions, beneficiary edits,
  withdrawals.** See [AGENTS.md §1](AGENTS.md) — the contract
  is read-only.
- **MFA automation.** Human-in-the-loop on every fresh login.
- **Cross-bank semantic alignment.** `wealthdb` gold's job.
- **The VIAC mobile app's signed-payload feed.**

## 2. Auth + REST surface

VIAC's portal is a JS SPA whose backend is JSON-over-cookies.
The login URL is a hash-route:

```
https://app.viac.ch/#/ext(modal:core/session/login)
```

— but the toolkit does not load it; the auth flow is a sequence
of REST calls.

### 2.1 Auth flow (replayed by `login.py`)

```
GET    /                                          (sets AL_SESS-S + CSRFT<N>-S cookies)
DELETE /external-login/public/authentication/flow/        (clear stale flow; 204)
POST   /external-login/public/authentication/password/check/
       {"username": "<E.164 phone>", "password": "<...>"}   (200; SMS sent)
POST   /external-login/public/authentication/mtan/otp/check/
       {"otp": "<code>"}                                    (200)
GET    /external-login/public/authentication/               (200; session confirmed)
POST   /rest/web/customer/loginHook                         (204; web-side activate)
```

`GET /rest/web/heartbeat` probes a stored session: 204 when it is
live. `login --check` sends only this call, and `download` sends it
before its walk.

After step 4 the `AL_SESS-S` cookie is server-side promoted
from anonymous to authenticated; the cookie value itself
doesn't rotate. `CSRFT<N>-S` (digit suffix is bundle-build-
bound) is echoed as the `x-csrft<N>` request header on
POST / PUT / PATCH / DELETE only. See `viac_client.py` for the
double-submit-cookie machinery.

### 2.2 Data surface (replayed by `download.py`)

| Endpoint | Returns |
| --- | --- |
| `/rest/web/customer/current/<N-N>` | customer profile |
| `/rest/web/wealth/portfolio-inventory` | p3a / pvb / inv arrays |
| `/rest/web/wealth/summary` | daily NAV time series (cross-portfolio) |
| `/rest/web/wealth/allocation` | cross-portfolio allocation breakdown |
| `/rest/web/p3a/portfolio/<num>/strategySummary` | strategy / risk / custody bank |
| `/rest/web/p3a/portfolio/<num>/assetsOverview` | per-fund holdings + cost basis |
| `/rest/web/p3a/portfolio/<num>/fees-<N>` | fee config |
| `/rest/web/p3a/portfolio/transactions` | all transactions, keyed by portfolio number |
| `/rest/web/document/<N-N>` | document index (one entry per document) |
| `/files/document/<docid>` | PDF binary |

Several endpoints carry build-bound `/N-N` version suffixes
(`customer/current/7-5`, `document/7-0`, `fees-70`). The values
appear stable per VIAC deploy. They are constants in
`download.py`. If one rotates, `download` fails fast with an
error that points here; the fix is a fresh capture of the SPA's
login flow.

No pagination needed — `transactions` and `document` return
the full set in one response. `download` retries transient
errors, including a dropped HTTP/2 stream.

## 3. Identity strategy

### 3.1 `account_external_id`

The dotted portfolio number, verbatim: `<product>.<customer-id>.<portfolio-index>`.

Examples (placeholder shape — synthetic):
- `3.NNN.NNN.NNN.NN` (Pillar-3a)
- `2.NNN.NNN.NNN.O` / `2.NNN.NNN.NNN.U` (PVB mandatory / extra-mandatory)
- `1.NNN.NNN.NNN.NN` (INV)

Silver promotes the first dotted segment to `product_code` and
the last to `portfolio_index` for the gold-layer `tax_wrapper`
mapping (see §7).

### 3.2 `instrument_external_id`

**ISIN.** VIAC carries an ISIN on every position; the silver
loader uses it directly as the per-instrument key. Mirrors
wealthdb gold's `instruments.isin` so cross-bank joins are
free.

### 3.3 `transaction_external_id`

Synthetic SHA-256 prefix over
`(account | type | value_date | amount_chf | document_number)`.
VIAC doesn't surface a stable per-event id on the wire — the
`documentNumber` is shared across some legs of corporate-
action pairs, so the id hashes a tuple. Deterministic → re-loading
the same bronze converges.

## 4. Anti-bot

VIAC's backend is plain JSON over cookies. No fingerprinting,
no JS challenge and no anti-replay guard at any of the
endpoints in §2. Vanilla httpx with a realistic Chrome
User-Agent + the SPA's standard headers (see
`viac_client.py:DEFAULT_HEADERS`) gets through every endpoint
the toolkit needs.

If VIAC adds an anti-bot layer (e.g. Akamai), the escalation
path mirrors the sibling collectors: stealth plugins, then
Camoufox-patched Firefox. Only the HTTP client would change.

## 5. Bronze layout

The run-dir tree is in the [README](README.md#output).

**PDF gating** (`download.py`). A date gate comes first: a
document whose `timestamp` falls outside the run's `--lookback`
window is skipped, and one with no readable timestamp is kept.
Then the type gate:

- **Default**: download everything — the non-TRANSACTION docs
  (statements, Bescheinigungen, contracts, investment profiles),
  the `SECURITY_FUSION` TRANSACTION docs (the only place the
  old→new ISIN mapping for fund mergers lives), and the per-event
  TRANSACTION PDFs (TRADE_REPORT, DIVIDEND, FEE_CHARGE, INTEREST,
  DIVIDEND_CANCELLATION).
- **`--no-transaction-documents`**: skip the per-event TRANSACTION
  PDFs, keeping the non-TRANSACTION docs and the `SECURITY_FUSION`
  TRANSACTION docs (the ISIN-mapping source is always fetched).

**Cross-run download-avoidance** — in-gate PDF fetches run
through the shared [`collectorkit.docdedup`](../../shared/collectorkit/collectorkit/docdedup.py)
engine, keyed by document number and chosen per document class so the
fetch-avoidance never serves a stale figure:

- **link** (fetch avoided) — the executed-once, immutable, unparsed
  documents (`PROVISION_CONTRACT`, `INVESTMENT_PROFILE`, `CONTRIBUTION_CREDIT_NOTE`,
  `GENERIC_COMMUNICATION`, and the per-event `TRANSACTION` receipts
  `TRADE_REPORT` / `FEE_CHARGE` / `INTEREST` / `DIVIDEND` /
  `DIVIDEND_CANCELLATION`) are hard-linked from a prior complete run when
  the document number matches, and the fetch is skipped. A hardlink error
  falls through to a real fetch — a document degrades to a fetch, never to
  a miss.
- **fetch-verify** (always fetched, then content-compared) — the
  `INVESTMENT_REPORTING` / `MANUAL_INVESTMENT_REPORTING` statements
  `load.py` parses (§5.1), every `TAX` document (the Pillar-3a
  Bescheinigungen), and the data-bearing `SECURITY_FUSION` PDF are always
  re-fetched and compared against the prior copy: a byte-identical one is
  hardlinked (disk reclaimed), a changed one keeps its fresh bytes. This is
  the one mode correct against a silent re-issue under a stable document
  number — a restated statement or corrected certificate is never served
  stale into silver. Any unrecognised type fetch-verifies too (the safe
  default). A `TAX` or `REPORT` document of an uncatalogued
  subtype fetch-verifies too, since the class keys on `type` as
  well as `subType`.

Every run dir stays self-contained (a hardlink is a real in-run file), so
`load.py` needs no cross-run fallback. `--documents-force` bypasses the
index entirely (fetch every in-gate PDF, no hardlink reuse) — the first-run
confidence check.

## 5.1 Historical positions from the Reporting PDFs

The REST API only exposes the *current* holdings snapshot
(`assetsOverview`), so live scraping alone gives gold a position
time series that starts at the first download. The document
archive closes that gap: the `REPORT` / `INVESTMENT_REPORTING`
PDFs (plus the on-demand `MANUAL_INVESTMENT_REPORTING`) are
period-end statements that list, per portfolio, every fund held
with its ISIN, units, prices and CHF market value — going back to
the contract's first year. Cadence varies (semi-annual or
annual). One PDF covers every portfolio.

[`pdf_parsers.py`](pdf_parsers.py) parses them (via **pypdfium2**
— the fastest lossless extractor benchmarked on these A4 reports;
see the module docstring). The "Securities overview" table row
grammar is

```
<sub-asset-class…> <FX> <qty> <name…> <ISIN> \
    <initial_price> <price> <return%> <share%> <market_value_chf>
```

anchored on the ISIN and the three-letter FX code, with asset
class taken from the section header (Liquidity / Equity / Bonds /
Real Estate / Commodities / Alternative Investments) that precedes
each block. The "3a Account" liquidity row routes to
`cash_balances`. Each report yields one position snapshot — plus
one cash balance per portfolio — keyed on the report's period-end
(as-of) date.

`load.py`'s `load_historical_reports_phase` writes these into the
same `positions` / `cash_balances` tables as the live scrapes,
tagged `source = 'report:<docid>'` and keyed on the as-of date
(which never collides with the live scrape timestamps). Re-parsing
the same stable report across successive dumps converges
(`INSERT OR REPLACE`). Reconstructed rows reconcile to the cent
against each report's printed "Balance in CHF". Instruments held
only historically (a fund merged away, for example) enter the
`instruments` catalogue through this path, and live instruments
gain an earlier `first_seen_at`. The statements are window-gated
like every document, so the history reaches back as far as
`--lookback` does.

## 5.2 Run status + reclaiming disk

`run.json` follows the
[fleet status lifecycle](../README.md#debug-artefacts-run-status-and-pruning-bronze):
`"in-progress"` when `download` creates the run dir, atomically
overwritten with `"complete"` when the walk returns. A `--dry-run`
creates no run dir. The viac specifics:

- `load` (`bronze.pending_run_dirs`) skips a run dir with no
  `run.json` and one whose status is neither `"complete"` nor absent.
  A statusless manifest stays loadable.
- `prune` treats a statusless manifest as complete, unless it
  carries `dry_run: true`. That legacy dry-run shell is non-complete,
  so `prune` reclaims it, while `load` still reads it.
- `prune`'s `debug_subdirs` is `screenshots/`, where
  `download --debug` writes its HTTP trace (`http-trace.jsonl`).
  viac drives no browser, so the trace is its whole debug surface.
- Document PDFs are hard-linked across dumps (§5). Deleting a
  non-complete dump that holds a link is safe: the inode survives
  while a complete dump still links it. So `documents/<docid>.pdf`,
  which the historical-reports phase reads across dumps, is never
  lost.
- The wrapper runs `prune` host-side. The entrypoint keeps a
  `prune` arm for a direct `docker run`.

## 6. Silver schema

Materialised in [`migrations/`](migrations/) (0001 to 0005);
read those files for column-level commentary, this section for
the overview.

| Table | PK | Purpose |
| --- | --- | --- |
| `schema_meta` | `silver_schema_version` | Migration version registry. |
| `dump_runs` | `snapshot_at` | One row per ingested bronze run; full `run.json` in `payload`. Promotes `dry_run`, `with_transaction_documents`, document-counter columns. |
| `accounts` | `(snapshot_at, account_external_id)` | One row per (snapshot, portfolio). Promotes `product_code` ('3' p3a / '2' pvb / '1' inv) and `portfolio_index` parsed from the dotted number; plus the inventory + strategy union. p3a portfolios populate `strategy_*` and `custody_bank`; pvb portfolios populate `foundation` + `portfolio_type`. `management_style` is `'automated'` for every VIAC account (see §7). |
| `cash_balances` | `(snapshot_at, account_external_id, currency, balance_kind)` | One row per (snapshot, account, currency, kind). `balance_kind` is always `'cash'`. `source` (schema v3) is `'live'` for `assetsOverview.cashAmount` rows, `'report:<docid>'` for the 3a-account liquidity reconstructed from a Reporting PDF (§5.1). |
| `positions` | `(snapshot_at, account_external_id, instrument_external_id)` | Holdings, `instrument_external_id` = ISIN. Promotes `quantity` (fund units; VIAC's confusingly-named `amount` JSON field), `market_value_chf` (CHF mark-to-market; VIAC's `ratioInChf`), `ratio` (fraction of portfolio), `acquisition_price`, `asset_price`, canonical `asset_class` + VIAC's raw `viac_asset_class` + `sub_asset_class`. `source` (schema v3) splits **live** rows from `assetsOverview` (snapshot_at = scrape time) and **historical** rows from the Reporting PDFs (`'report:<docid>'`, snapshot_at = the report's period-end date; §5.1). Both coexist; the gold adapter reads every snapshot_at uniformly, so a historical snapshot answers `wealthdb holdings positions --as-of <past date>`. |
| `instruments` | `instrument_external_id` | Slow-changing master data; ISIN-keyed. Upserts advance `last_seen_at`. |
| `transactions` | `transaction_external_id` | One row per event from `/p3a/portfolio/transactions`. `transaction_external_id` synthesised per §3.3. `kind` is the canonical mapping (`buy`, `sell`, `dividend`, `interest`, `fee`, `deposit`, `corporate_action`, `other`). `instrument_external_id` (migration 0005) is the ISIN of the one instrument whose `name` equals the transaction's `description`; NULL when no name or several match. |
| `wealth_history` | `(snapshot_at, value_date)` | Customer-level daily NAV from `/wealth/summary`. Zips `dailyWealth` + `dailyPerformance` + `dailyInvestedAmounts` by date. A scalar arrives either wrapped as `{"__type": "VIAC_DECIMAL", "__value": "<decimal string>"}` or as a bare JSON number; the loader reads both. Customer-level **by choice**: the same response carries a per-portfolio series under `p3aSummary` / `pvbSummary` → `portfolioWealthSummaries`, which the loader does not read. |
| `documents` | `content_sha256` | Content-deduped PDF index. Promotes `viac_doc_id`, `doc_type`, `doc_subtype`, `timestamp`, `product`. `bronze_path` is relative to bronze root. |
| `parser_generations` | `scope` | Which generation of the Reporting-PDF parsing logic produced the rows a pass is holding (`collectorkit.srcfp` fingerprint). A moved generation drops them before the pass re-derives, so a re-parse replaces rather than accumulates. |

The loader runs pending migrations on every invocation. An
applied migration is never rewritten; a change is a new file.

### 6.1 Load invariants

Per-portfolio reconstructed rows reconcile to the cent against
each report's printed Balance in CHF, ISIN-keyed instrument master
upserts dedupe, and content-dedup collapses identical PDFs across
snapshots.

Re-running `load` is a no-op (`dump_runs.snapshot_at` is the
idempotency anchor).

## 7. Silver columns for the gold bridge

How gold interprets these columns (the `tax_wrapper` /
`management_style` / `account_kind` mapping) is owned by the
wealthdb viac adapter — see [the canonical
model](../../DESIGN.md) and the adapter source
[`wealthdb/internal/silver/viac/`](../../wealthdb/internal/silver/viac/).
The silver-side facts the adapter reads:

- `accounts.product_code` — `'3'` (Pillar-3a), `'2'` (PVB),
  `'1'` (INV); parsed from the first dotted segment of the
  portfolio number (§3.1).
- `accounts.management_style` — carried in silver (migration
  0002); the loader sets it to `'automated'` for every account,
  and the adapter reads it. VIAC is robo-advisor-shaped — the holder
  picks a strategy from a menu (or builds one within VIAC's
  concentration limits), then rebalancing runs by rules. The
  custom-strategy capability looks self-directed but isn't:
  the holder can only pick from VIAC's listed fund universe,
  with concentration / risk-level guards. Same product shape
  as Relevate's FZ products. A non-robo product line would
  branch in the loader.
- `accounts.state` — VIAC's `ACTIVE` / `PASSIVE` portfolio
  state, as the inventory lists it.
- `instruments.isin` — always populated; VIAC ships ISIN on
  every position, so it is the cross-bank join key.
- `transactions.kind` — already the canonical mapping in
  silver via `VIAC_TX_KIND_MAP` in `load.py`.
- `transactions.instrument_external_id` — the traded fund's
  ISIN, resolved in silver by name (§6).

## 8. Out of scope

The non-goals of §1.3 hold. One more:

- **PDF body parsing for transaction documents** — silver records
  the per-event TRADE / DIVIDEND / SECURITY_FUSION PDFs only by
  sha256 + (type, subType) metadata; their bodies are not parsed
  (§9).

## 9. Open questions

Open items, none blocking:

1. **Session lifetime.** Airlock typically defaults to ~30 min
   idle / ~8 h absolute. `login --check` measures it.
2. **`/N-N` endpoint suffixes rotating.** They are constants.
   A rotation makes `download` fail fast with a pointer to
   §2.2 rather than a silent 404.
3. **PVB per-portfolio endpoints.** PVB portfolios are in the
   inventory, but `download.py` does not fetch their detail.
   An ACTIVE PVB portfolio would need the
   `/pvb/portfolio/<num>/...` surface mapped; symmetric to p3a
   is the likely shape.
4. **`INV` product line.** The `inv[]` array is in the
   inventory schema, but its per-portfolio surface is unmapped.
   Its gold mapping to `tax_wrapper='taxable_personal'` is
   unverified.
5. **Transaction collision rate.** Some source rows can
   collapse in silver on identical-to-16-decimals (account, type,
   date, amount, doc) keys — most likely duplicate reports of the
   same dividend in VIAC's API rather than legitimately
   distinct events. If such collisions grow, the synthesizer needs a
   row-index disambiguator.
6. **Transaction-document PDF body parsing.** The per-event TRADE /
   DIVIDEND / SECURITY_FUSION PDFs carry data not in the JSON
   (per-event ISIN, units, FX rate, old→new ISIN mapping for
   fusions). Projecting them into a structured silver column (they are
   indexed, not parsed; §8) is a mechanical parser
   pass, left until a downstream consumer needs it.
