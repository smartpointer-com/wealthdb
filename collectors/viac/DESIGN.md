# viac-dump — design

Design document for the `viac-dump` toolkit. The audience is the
engineer (current author, future contributor) maintaining
`login.py`, `download.py`, and `load.py` against the live
`app.viac.ch` SPA. It is also the contract between this silver
and the `wealthdb` VIAC adapter.

Part of the **wealthdb** suite — see [the architecture
overview](../../ARCHITECTURE.md) for the bronze → silver → gold
model and [collectors/README.md](../README.md) for shared collector
conventions. This document only covers what's VIAC-specific.

## Status

| Verb | Status |
| --- | --- |
| `login.py` | implemented (pure httpx; Airlock-flow replay; cookies + CSRF metadata at chmod 0600) |
| `download.py` | implemented (pure httpx; `--with-transaction-documents` gate; hard-link dedup across prior bronze runs; h2-stream-drop retry) |
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
  it lives in a separate `viac-mobile-dump` repo.

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
| `/rest/web/document/<N-N>` | document index (~1000+ entries) |
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
<bronze-dir>/                          e.g. ~/wealthdb/viac/
├── <YYYYMMDDTHHMMSSZ>/                one bronze dump per run
│   ├── run.json                       manifest (timestamp, flags, doc counts)
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

**Cross-run dedup** — PDFs are hard-linked from prior bronze
runs when the document number matches, so a re-run only
fetches genuinely-new documents.

## 6. Silver schema

Materialised in [`migrations/0001_initial.sql`](migrations/0001_initial.sql);
read that file for column-level commentary, this section for
the overview.

| Table | PK | Purpose |
| --- | --- | --- |
| `schema_meta` | `silver_schema_version` | Migration version registry. |
| `dump_runs` | `snapshot_at` | One row per ingested bronze run; full `run.json` in `payload`. Promotes `dry_run`, `with_transaction_documents`, document-counter columns. |
| `accounts` | `(snapshot_at, account_external_id)` | One row per (snapshot, portfolio). Promotes `product_code` ('3' p3a / '2' pvb / '1' inv) and `portfolio_index` parsed from the dotted number; plus the inventory + strategy union. p3a portfolios populate `strategy_*` and `custody_bank`; pvb portfolios populate `foundation` + `portfolio_type`. `management_style` is `'automated'` for every VIAC account today (see §7). |
| `cash_balances` | `(snapshot_at, account_external_id, currency, balance_kind)` | One row per (snapshot, account, currency, kind). Currently only `balance_kind='cash'` (`assetsOverview.cashAmount`); schema extensible. |
| `positions` | `(snapshot_at, account_external_id, instrument_external_id)` | ACTUAL holdings from `assetsOverview` (not target allocation). `instrument_external_id` is the ISIN. Promotes `quantity` (fund units; VIAC's confusingly-named `amount` JSON field), `market_value_chf` (CHF mark-to-market; VIAC's `ratioInChf`), `ratio` (fraction of portfolio), `acquisition_price`, `asset_price`, both wealthdb-canonical `asset_class` and VIAC's raw `viac_asset_class` + `sub_asset_class` for forensics. |
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
loaded into silver:

| Table | Count |
| --- | --- |
| `accounts` | 14 (7 portfolios × 2 snapshots) |
| `cash_balances` | 10 |
| `positions` | 80 (8 funds × 5 p3a × 2 snapshots) |
| `instruments` | 16 (distinct ISINs) |
| `transactions` | 972 (4 collapsed; see §9 open Q5) |
| `wealth_history` | 3284 (1642 daily × 2 snapshots) |
| `documents` | 1019 (content-dedup'd; 1018 in both, 1 only in second) |

Re-running `load` is a no-op (`dump_runs.snapshot_at` is the
idempotency anchor).

## 7. Silver columns for the gold bridge

How gold interprets these columns (the `tax_wrapper` /
`management_style` / `account_kind` mapping) is owned by the
wealthdb viac adapter — see [the canonical
model](../../ARCHITECTURE.md) and the adapter source
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
- **PDF body parsing for transaction documents** — Phase 1
  established that the per-transaction PDFs carry rich data
  for TRADE / DIVIDEND / FUSION events (ISIN, units, FX rate,
  old→new ISIN map). Parsing them into structured silver
  events is a future migration; today silver records them
  only by sha256 + (type, subType) metadata.
- **The VIAC mobile app's payload** — separate `viac-mobile-dump`
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
5. **Transaction collision rate.** 4/976 source rows collapsed
   in silver on identical-to-16-decimals (account, type, date,
   amount, doc) keys — most likely duplicate reports of the
   same dividend in VIAC's API rather than legitimately
   distinct events. Worth a follow-up if a future bronze dump
   shows a higher collision rate, which would suggest the
   synthesizer needs a row-index disambiguator.
6. **Transaction-document PDF body parsing.** TRADE / DIVIDEND
   / SECURITY_FUSION PDFs carry data not in the JSON
   (per-event ISIN, units, FX rate, old→new ISIN mapping for
   fusions). A future loader pass could project these into a
   structured silver column. Phase 1 inspected the layouts;
   the work is mechanical pdfplumber given that.
