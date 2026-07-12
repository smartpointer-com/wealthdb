# viac — design

Design document for the `viac` toolkit. The audience is the
engineer (current author, future contributor) maintaining
`login.py`, `download.py`, and `load.py` against the live
`app.viac.ch` SPA. It is also the contract between this silver
and the `wealthdb` VIAC adapter.

Part of the **wealthdb** suite — see [the architecture
overview](../../DESIGN.md) for the bronze → silver → gold
model and [collectors/README.md](../README.md) for shared collector
conventions. This document only covers what's VIAC-specific.

## Status

| Verb | Status |
| --- | --- |
| `login.py` | implemented (pure httpx; Airlock-flow replay; cookies + CSRF metadata at chmod 0600) |
| `download.py` | implemented (pure httpx; `--with-transaction-documents` gate; `collectorkit.docdedup` download-avoidance — link the immutable/unparsed docs, fetch-verify the parsed/tax/fusion docs; `--documents-force` bypass; h2-stream-drop retry) |
| `load.py` + `migrations/0001_initial.sql` | implemented (idempotent on `dump_runs.snapshot_at`) |

What's NOT implemented: PVB (Pillar-2 vested-benefits) per-
portfolio endpoint surface — `accounts` carries the inventory
entry but positions / strategy / fees aren't fetched. See §9.

## 1. Context and non-goals

### 1.1 Why a scraper

VIAC has no retail-accessible read API. The provider supplies a
web SPA at `app.viac.ch` and a companion mobile app; both run on
the same internal backend, but neither's API surface is exposed
for personal aggregation:

- **OpenWealth** — B2B-only. VIAC's parent (WIR Group / Terzo)
  has no published retail OpenWealth participation.
- **PSD2** — Switzerland is outside the EU PSD2 regime.
- **Aggregators** — Plaid / TrueLayer / Tink / Powers / Akoya
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
  PASSIVE while still actively employed; per-portfolio
  endpoint surface not yet mapped (see §9).
- `inv[]` — VIAC's non-retirement investment product line.
  Empty for users who only hold retirement assets.

The first dotted segment of each portfolio number encodes the
product line: `3.*` is p3a, `2.*` is pvb, `1.*` is inv.

### 1.3 Non-goals

- **Real-time / near-real-time pull.** SMS mTAN gates every
  fresh login.
- **Strategy changes, contributions, beneficiary edits,
  withdrawals.** See [CLAUDE.md §1](CLAUDE.md) — the contract
  is read-only.
- **MFA automation.** Human-in-the-loop on every fresh login.
- **Cross-bank semantic alignment.** `wealthdb` gold's job.
- **The VIAC mobile app's signed-payload feed.** If reachable
  it lives in a separate `viac-mobile` repo.

## 2. Auth + REST surface

VIAC's portal is a JS SPA whose backend is JSON-over-cookies.
The login URL is a hash-route:

```
https://app.viac.ch/#/ext(modal:core/session/login)
```

— but the toolkit doesn't load it; the auth flow is a sequence
of REST calls. The SPA's hash-route is operationally irrelevant
once you know the wire shape.

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
(`customer/current/7-5`, `notification/6-2`, `document/7-0`,
`fees-70`). The values appear stable per VIAC deploy. They're
hardcoded as constants in `download.py`; if they rotate, the
loader fails fast with a clear error.

No pagination needed — `transactions` and `document` return
the full set in one response.

## 3. Identity strategy

### 3.1 `account_external_id`

The dotted portfolio number, verbatim: `<product>.<customer-id>.<portfolio-index>`.

Examples (placeholder shape — synthetic):
- `3.NNN.NNN.NNN.NN` (Pillar-3a)
- `2.NNN.NNN.NNN.O` / `2.NNN.NNN.NNN.U` (PVB mandatory / extra-mandatory)
- `1.NNN.NNN.NNN.NN` (INV, not yet observed)

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
action pairs, so we hash a tuple. Deterministic → re-loading
the same bronze converges.

## 4. Anti-bot — resolved

VIAC's backend is plain JSON over cookies. No fingerprinting,
no JS challenge, no anti-replay guard observed at any of the
endpoints in §2. Vanilla httpx with a realistic Chrome
User-Agent + the SPA's standard headers (see
`viac_client.py:DEFAULT_HEADERS`) gets through every endpoint
the toolkit needs.

If VIAC later adds an anti-bot layer (e.g. an Akamai upgrade),
the escalation path mirrors the sibling collectors: stealth
plugins, then Camoufox-patched Firefox. None of the existing
code would need to change beyond the underlying HTTP client.

## 5. Bronze layout

```
<bronze-dir>/                          e.g. $XDG_DATA_HOME/wealthdb/viac/
├── <YYYYMMDDTHHMMSSZ>/                one bronze dump per run
│   ├── run.json                       manifest (status, timestamp, flags, doc counts)
│   ├── customer.json                  /rest/web/customer/current/<N-N>
│   ├── wealth/
│   │   ├── portfolio-inventory.json   master list (p3a + pvb + inv)
│   │   ├── summary.json               daily NAV time series across all wealth
│   │   └── allocation.json            cross-portfolio allocation breakdown
│   ├── positions/<portfolio-num>/
│   │   ├── strategy.json
│   │   ├── assets.json                current holdings per fund
│   │   └── fees.json
│   ├── transactions/all.json          every tx keyed by portfolio
│   ├── documents/
│   │   ├── index.json                 document catalogue
│   │   └── <docid>.pdf                PDF binaries (see gating below)
│   └── (manual/ ... user-uploaded artefacts; same dedup path)
└── viac.db                            silver SQLite (default name)
```

**PDF gating** (`download.py`):

- **Default**: download non-TRANSACTION docs (statements,
  Bescheinigungen, contracts, investment profiles) plus
  `SECURITY_FUSION` TRANSACTION docs — the only place the
  old→new ISIN mapping for fund mergers lives.
- **`--with-transaction-documents`**: also download the per-
  event TRANSACTION PDFs (TRADE_REPORT, DIVIDEND, FEE_CHARGE,
  INTEREST, DIVIDEND_CANCELLATION).

**Cross-run download-avoidance** — in-gate PDF fetches run
through the shared [`collectorkit.docdedup`](../../shared/collectorkit/collectorkit/docdedup.py)
engine, keyed by document number and chosen per document class so the
fetch-avoidance never serves a stale figure:

- **link** (fetch avoided) — the executed-once, immutable, unparsed
  documents (`CONTRACT`, `INVESTMENT_PROFILE`, `CONTRIBUTION_CREDIT_NOTE`,
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
  default).

Every run dir stays self-contained (a hardlink is a real in-run file), so
`load.py` needs no cross-run fallback. `--documents-force` bypasses the
index entirely (fetch every in-gate PDF, no hardlink reuse) — the first-run
confidence check.

## 5.1 Historical positions from the Reporting PDFs

The REST API only exposes the *current* holdings snapshot
(`assetsOverview`), so live scraping alone gives gold a position
time series that starts the day scraping began. The document
archive closes that gap: the `REPORT` / `INVESTMENT_REPORTING`
PDFs (plus the on-demand `MANUAL_INVESTMENT_REPORTING`) are
period-end statements that list, per portfolio, every fund held
with its ISIN, units, prices and CHF market value — going back to
the contract's first year. Observed cadence: semi-annual through
2023, annual thereafter. One PDF covers every portfolio.

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
only historically (e.g. the pre-2024-fusion CS/iShares funds, no
longer in the live holdings) enter the `instruments` catalogue
through this path, and live instruments gain an earlier
`first_seen_at`.

## 5.2 Run status + reclaiming disk

`download.py` records a `status` in `run.json`: `"in-progress"` when
it creates the run dir (written via `bronze.atomic_write_json` right
after `mkdir`), then atomically overwritten with `"complete"` — or
`"dry-run"` for a `--dry-run` walk — once the walk returns. A crash
mid-walk therefore leaves `status = "in-progress"`, a stronger "this
dump is partial" signal than the older "no `run.json` = incomplete"
heuristic, which a partial manifest write could defeat.

`load.py` keys on it: `list_pending_dumps` skips a dump whose status is
`"in-progress"` or `"dry-run"`, so a crashed walk never leaks a partial
snapshot into silver. A statusless manifest predates the field and
stays loadable (the walk historically wrote `run.json` only at the end,
so its presence meant completion) — backward-compatible with existing
bronze.

`prune.py` — a thin wrapper over the shared, unit-tested
[`collectorkit.prune`](../../shared/collectorkit/collectorkit/prune.py)
engine — reclaims whole **non-complete** dumps (`status != "complete"`,
or a legacy statusless `dry_run: true` shell, or no `run.json`). Its
`PruneConfig.debug_subdirs` is empty: viac is REST-only and writes no
bronze-resident debug artefacts, so a complete dump has nothing inside
it to reclaim and is left untouched. The engine guarantees a `load`
input is never deleted, an unreadable/corrupt manifest is skipped as
UNKNOWN, symlinks and non-run-dir root entries (the silver `viac.db`)
are never touched, and an in-flight download is protected by a
newest-mtime age guard (`--min-age-hours`, default 1) plus a
recheck immediately before deletion. Document PDFs are hard-linked
across dumps (§5, cross-run dedup), so deleting a non-complete dump
that holds a link is safe — the inode survives while any complete dump
still links it, and `documents/<docid>.pdf` (a `load` input parsed
cross-dump by the historical-reports phase) is never lost. `prune`
runs in-container via the same `entrypoint.sh` dispatch as `load`.

## 6. Silver schema

Materialised in [`migrations/0001_initial.sql`](migrations/0001_initial.sql);
read that file for column-level commentary, this section for
the overview.

| Table | PK | Purpose |
| --- | --- | --- |
| `schema_meta` | `silver_schema_version` | Migration version registry. |
| `dump_runs` | `snapshot_at` | One row per ingested bronze run; full `run.json` in `payload`. Promotes `dry_run`, `with_transaction_documents`, document-counter columns. |
| `accounts` | `(snapshot_at, account_external_id)` | One row per (snapshot, portfolio). Promotes `product_code` ('3' p3a / '2' pvb / '1' inv) and `portfolio_index` parsed from the dotted number; plus the inventory + strategy union. p3a portfolios populate `strategy_*` and `custody_bank`; pvb portfolios populate `foundation` + `portfolio_type`. `management_style` is `'automated'` for every VIAC account today (see §7). |
| `cash_balances` | `(snapshot_at, account_external_id, currency, balance_kind)` | One row per (snapshot, account, currency, kind). Only `balance_kind='cash'` today. `source` (schema v3) is `'live'` for `assetsOverview.cashAmount` rows, `'report:<docid>'` for the 3a-account liquidity reconstructed from a Reporting PDF (§5.1). |
| `positions` | `(snapshot_at, account_external_id, instrument_external_id)` | Holdings, `instrument_external_id` = ISIN. Promotes `quantity` (fund units; VIAC's confusingly-named `amount` JSON field), `market_value_chf` (CHF mark-to-market; VIAC's `ratioInChf`), `ratio` (fraction of portfolio), `acquisition_price`, `asset_price`, canonical `asset_class` + VIAC's raw `viac_asset_class` + `sub_asset_class`. `source` (schema v3) splits **live** rows from `assetsOverview` (snapshot_at = scrape time) and **historical** rows from the Reporting PDFs (`'report:<docid>'`, snapshot_at = the report's period-end date; §5.1). Both coexist; the gold adapter reads every snapshot_at uniformly, so a historical snapshot answers `wealthdb holdings positions --as-of <past date>`. |
| `instruments` | `instrument_external_id` | Slow-changing master data; ISIN-keyed. Upserts advance `last_seen_at`. |
| `transactions` | `transaction_external_id` | One row per event from `/p3a/portfolio/transactions`. `transaction_external_id` synthesised per §3.3. `kind` is the canonical mapping (`buy`, `sell`, `dividend`, `interest`, `fee`, `deposit`, `corporate_action`, `other`). |
| `wealth_history` | `(snapshot_at, value_date)` | Customer-level daily NAV from `/wealth/summary`. Zips `dailyWealth` + `dailyPerformance` + `dailyInvestedAmounts` by date. NOT per-portfolio (VIAC's API doesn't expose per-portfolio history). |
| `documents` | `content_sha256` | Content-deduped PDF index. Promotes `viac_doc_id`, `doc_type`, `doc_subtype`, `timestamp`, `product`. `bronze_path` is relative to bronze root. |

Migrations land under `migrations/NNNN_<slug>.sql`. The loader
runs pending migrations on every invocation. Same discipline
as the sibling collectors — never rewrite an applied migration,
always add a new file.

### 6.1 Validation against the first real load

Two bronze dumps (one full, one re-fetch of a single missed PDF)
loaded cleanly into silver. Per-portfolio reconstructed rows
reconcile to the cent against each report's printed Balance in
CHF, ISIN-keyed instrument master upserts dedupe as expected, and
content-dedup collapses identical PDFs across snapshots.

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
  `'1'` (INV, not yet observed); parsed from the first dotted
  segment of the portfolio number (§3.1).
- `accounts.management_style` — carried in silver (added in
  migration 0002); the loader sets it to `'automated'` for
  every account. VIAC is robo-advisor-shaped — the holder
  picks a strategy from a menu (or builds one within VIAC's
  concentration limits), then rebalancing runs by rules. The
  custom-strategy capability looks self-directed but isn't:
  the holder can only pick from VIAC's listed fund universe,
  with concentration / risk-level guards. Same product shape
  as Relevate's FZ products. If VIAC ever ships a non-robo
  product line, the loader branches and the adapter reads the
  silver column directly.
- `accounts` state — ACTIVE p3a vs PASSIVE pvb (the latter
  until the pvb endpoint surface is mapped, §9).
- `instruments.isin` — always populated; VIAC ships ISIN on
  every position, so it is the cross-bank join key.
- `transactions.kind` — already the canonical mapping in
  silver via `VIAC_TX_KIND_MAP` in `load.py`.

## 8. What we do NOT do

- **Mutations** — no contributions, no strategy changes, no
  withdrawals, no beneficiary edits. See [CLAUDE.md §1](CLAUDE.md).
- **MFA automation** — human-in-the-loop on every fresh login.
- **Cron / launchd / GitHub-Actions scheduling** — see
  [CLAUDE.md §2](CLAUDE.md). Unattended runs can't pass the
  mTAN gate anyway.
- **Cross-bank semantic alignment** — gold's job.
- **PDF body parsing for transaction documents** — silver records
  the per-event TRADE / DIVIDEND / SECURITY_FUSION PDFs only by
  sha256 + (type, subType) metadata; parsing their bodies into
  structured events is deferred (§9).
- **The VIAC mobile app's payload** — separate `viac-mobile`
  repo if ever.

## 9. Open questions

Outstanding work — currently neither implemented nor blocking:

1. **Session lifetime.** Airlock typically defaults to ~30 min
   idle / ~8 h absolute. Determine empirically with periodic
   `login --check` runs.
2. **`/N-N` endpoint suffixes rotating.** Hardcoded as
   constants today. If VIAC ever rotates, the loader fails
   fast with a clear "endpoint changed" pointer (we don't
   silently 404).
3. **PVB per-portfolio endpoints.** PVB portfolios are present
   in the inventory but `download.py` doesn't fetch their
   detail. If a future VIAC user has ACTIVE PVB, mapping the
   `/pvb/portfolio/<num>/...` surface matters; symmetric to
   p3a is the likely shape.
4. **`INV` product line.** Not observed in any user; the
   `inv[]` array is in the inventory schema. Mapping to
   wealthdb `tax_wrapper='taxable_personal'` is speculative
   until we see one.
5. **Transaction collision rate.** A small number of source rows
   collapse in silver on identical-to-16-decimals (account, type,
   date, amount, doc) keys — most likely duplicate reports of the
   same dividend in VIAC's API rather than legitimately
   distinct events. Worth a follow-up if a future bronze dump
   shows a materially higher collision rate, which would suggest
   the synthesizer needs a row-index disambiguator.
6. **Transaction-document PDF body parsing.** The per-event TRADE /
   DIVIDEND / SECURITY_FUSION PDFs carry data not in the JSON
   (per-event ISIN, units, FX rate, old→new ISIN mapping for
   fusions). Projecting them into a structured silver column — today
   they are only indexed, not parsed (§8) — is a mechanical parser
   pass, deferred until a downstream consumer needs it.
