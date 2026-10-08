# ubs-web — design notes

This document describes what the `ubs-web` silver carries and how it
lines up with the PSN-fed silver from the sibling
[ubs-psn](../ubs-psn/) collector — the source-specific facts a
reader needs to understand the two UBS feeds. It supplements the
schema comments in
[migrations/0001_initial.sql](migrations/0001_initial.sql); read
that file for column-level definitions, this file for the
feed-shape detail.

How gold actually merges the two UBS silvers (join keys, splice
rules, sign conventions, which feed wins) is owned by the wealthdb
UBS adapter — see [the adapter doc](../../wealthdb/docs/adapters/ubs.md).
The notes below describe the silver shapes that make those merge
decisions possible, not the merge policy itself.

The companion silvers, by path:

| Silver | Source | Coverage | Default path |
| --- | --- | --- | --- |
| `ubs-web` | Web scrape via Playwright + PDF reconstruction | Live-fetch tables: recent transactions window + on-demand positions + a PDF document index. Historical tables: quarterly position snapshots and monthly cash balances reconstructed from the bronze PDF archive, back to the earliest statement available | `$XDG_DATA_HOME/wealthdb/ubs-web/ubs-web.db` |
| `ubs-psn` | UBS PSN nightly SFTP feed | Forward-only daily snapshots + events, from agreement go-live date | `$XDG_DATA_HOME/wealthdb/ubs-psn/ubs-psn.db` |

## 1. The two UBS feeds

Both silvers expose the same logical entities (accounts, portfolios,
positions, transactions) plus a few feed-specific extras. The web
silver is the **historical source of truth** (it carries multi-year
data PSN can't); PSN takes over from its activation date forward.
The two feeds splice on a per-relationship cutover date: web before
it, PSN after it. Documents (PDFs) are web-only — PSN has no
equivalent.

How gold reconciles the overlap (which feed wins per entity, the
exact join keys and splice boundary) is owned by the wealthdb UBS
adapter — see [the adapter doc](../../wealthdb/docs/adapters/ubs.md).
The sections below document the silver-side facts that make the
splice possible.

## 2. Identifier conventions

The schema header in [migrations/0001_initial.sql](migrations/0001_initial.sql)
defines every identifier. Recap of the cross-silver join keys:

| Entity | Web silver | PSN silver | Join condition |
| --- | --- | --- | --- |
| Banking relationship | `banking_relationship_id` (UBS opaque token), plus `account_number_prefix` (e.g. `BBBB AAAAAAAA`) and `description` | `relationship_id` (`SFTPCH01` / `SFTPCH02`, the SFTP server name) | **Manual config required** — there is no shared key. See §3.1. |
| Portfolio | `portfolio_external_id` (e.g. `RNNN`) + `base_currency` (e.g. `CHF`, from positions.csv "Valued in:" footer) | `portfolio_external_id` (= UBS `PrtflId`) + `PrtflKey.PrtflCcyIsoCd` in payload | Equality on `portfolio_external_id` |
| Cash account | `account_external_id` = IBAN no-spaces uppercase (e.g. `CHKKBBBBRRRRAAAAAAAAC`); plus parallel `account_acct_id_psn_form` (e.g. `RRRR000000AAAAAAAA0000C`) computed by the web loader | `account_external_id` = IBAN per `psn/migrations/0001` comment; the PSN payload also has UBS `AcctId` in the 21-char form | Equality on `account_external_id` (IBAN) OR equality on `account_acct_id_psn_form` ↔ PSN payload `AcctId`. Either works. |
| Safekeeping account | Not currently surfaced by the web feed (would need future scraping) | `account_external_id` = UBS safekeeping code (e.g. `BBBB-AAAAAAAA.S1`) | PSN only for now. |
| Instrument | `instrument_isin` (ISO 6166 ISIN-12) on `positions` | `isin` on `instruments` / `holdings` | Equality on ISIN |
| Transaction | `(transaction_external_id, account_external_id)` compound PK — UBS reuses the Transaction no. for both debit + credit sides of an inter-account transfer | `event_external_id` (derived from the SWIFT message reference), unique per event | **DO NOT JOIN** — the two ID schemes do not overlap in practice. See §3.6 for the date-splice-only merge strategy. |

## 3. Per-entity silver shapes

This section documents the identifiers and entity shapes each feed
carries. How gold pairs them across the two silvers (config schema,
join keys, which feed wins) is owned by the wealthdb UBS adapter —
see [the adapter doc](../../wealthdb/docs/adapters/ubs.md).

### 3.1 Banking relationships

An e-banking login can hold 1+ banking relationships.
PSN sees each as a separate SFTP endpoint (`SFTPCH01`, `SFTPCH02`,
…). Web sees each as an opaque `bankingRelationId` URL token.

**There is no shared key**, only proxies the silver surfaces:

- `account_number_prefix` — the 12-char shared prefix of all
  account numbers under the relationship (`BBBB AAAAAAAA`).
  PSN's `AcctId` values can be sliced to derive the same prefix
  (positions 1–4 + reformat the middle).
- `description` — the web silver leaves
  `banking_relationships.description` empty by default, a slot for
  a human label pairing each web relationship with its PSN
  counterpart.

When `download.py` runs against a different relationship
(after it is switched in the UBS UI), a new
`banking_relationships` row appears with its own opaque token.

### 3.2 Portfolios

PSN's `portfolio_external_id` is UBS's `PrtflId` (4-char code
like `RNNN`). The web `positions.csv` "Portfolio" column carries
`<account_number_prefix> <PrtflId>` — e.g. `BBBB AAAAAAAA RNNN`.

Portfolios and accounts are **separate entities** in silver:
`portfolios` is keyed by `(snapshot_at, portfolio_external_id)`,
`accounts` carries a nullable `portfolio_external_id` foreign
key. A portfolio never owns positions / balances directly — they
hang off cash and safekeeping accounts.

**Three kinds of portfolio rows can appear in web silver:**

1. **Real customer-facing portfolios** that appear on the
   UBS homepage as separate tiles. The web loader pulls one
   `positions_<sha>.csv` per portfolio via the `portfolioUid`
   anchors enumerated from the homepage.
2. **A synthetic catch-all portfolio** that the consolidated
   default view (`positions.csv`, the `preselectFirstPortfolio
   =true` route) uses to file accounts that aren't attached to
   any real portfolio. The web loader processes per-portfolio
   CSVs first, then the consolidated CSV, and only inserts
   accounts + positions for `(account, isin)` pairs not yet seen.
   Net effect: real portfolio assignments win; the catch-all only
   ever owns the genuinely-unattached accounts.

**Web loader contract:** when parsing each positions.csv,
extract the trailing token of the "Portfolio" column (split on
whitespace, take the last word) as `portfolio_external_id`.
Store the full original string in `portfolio_full_id` for
traceability. Read the "Valued in: <CCY>" footer line and write
it as `portfolios.base_currency`. The catch-all is a real row in
silver, distinct from the customer-facing portfolios.

### 3.3 Cash accounts

Web shows the IBAN with spaces (`CHKK BBBB RRRR AAAA AAAA C`,
e.g. UBS Switzerland uses `BBBB = 0023`).
The web loader normalises to `account_external_id`:

```python
def iban_canonical(iban: str) -> str:
    return iban.replace(" ", "").upper()
```

PSN's loader stores the IBAN in `cash_accounts.account_external_id`
per the schema comment. The same canonical form on both sides
means equality joins.

#### IBAN ↔ PSN AcctId conversion

The PSN payload also carries `AcctId` in a 21-char UBS-internal
form (`RRRR000000AAAAAAAA0000C`). The relation between an IBAN and
the AcctId, for UBS Switzerland personal accounts:

```
IBAN structure (with spaces):
  CH<chk:2>  <bank:4>  <branch:4>  <base:8>   <chk:1>
  CHKK       BBBB      RRRR        AAAAAAAA   C

AcctId structure (21 chars, no spaces):
  <branch:4> <0000>    <00><base:8>  <0000>   <chk:1>
  RRRR       0000      00 AAAAAAAA   0000     C
```

So given a canonical IBAN `CH<chk:2><bank:4><branch:4><base:8><chk:1>`
of length 21, the AcctId is:

```
acct_id_psn_form = branch + "0000" + "00" + base + "0000" + chk
                 = iban[8:12] + "000000" + iban[12:20] + "0000" + iban[20]
```

The web loader computes `account_acct_id_psn_form` from the IBAN
and stores it as a parallel column, so both the IBAN and the
21-char AcctId form are present in both silvers.

#### Accounts PSN sees but web doesn't

PSN can report internal UBS booking accounts that the customer UI
does not expose. The web feed has no rows for them at all —
they exist only on the PSN side.

### 3.4 Safekeeping accounts (securities depots)

PSN exposes them directly (`safekeeping_accounts` table); web
does not surface them as first-class rows. The web `positions.csv`
flattens everything to portfolio + product, hiding the depot
layer. The web `positions.portfolio_external_id` does link to
`psn.holdings` (which carries both `safekeeping_external_id` and
`isin`), though not strictly 1-to-1 — multiple safekeepings can
exist under one portfolio.

**Future work:** per-safekeeping attribution from web data would
need scraping the "Investment positions (custody account)"
navigation tree, which exposes the depot hierarchy. Not done in v1.

### 3.5 Positions (snapshots)

Both silvers emit complete snapshots. Web is on-demand (one per
`download.py` run); PSN is daily.

The two `positions` shapes differ in fidelity. PSN's `holdings`
carry the full safekeeping / portfolio hierarchy and the
authoritative pricing snapshot per (safekeeping, instrument). The
web silver's `positions` table flattens that hierarchy: one
portfolio code per row, no safekeeping link, market value in
portfolio base currency only. What the web rows add on top is
`cost_price`, `lending_value`, `lending_value_ratio`,
`description`, etc. — fields PSN does not carry. The web
`positions` rows key on `portfolio_external_id` + `instrument_isin`.

**Cash positions** (the "Liquidity - Accounts" rows in
positions.csv) carry `account_external_id` as the canonical IBAN
per §2, the same form PSN's `cash_balances` use.

### 3.6 Transactions (the splice)

This is where the two UBS feeds meet. Rather than matching
individual transactions, the histories splice on a date boundary:
web before PSN's go-live, PSN after it. The wealthdb UBS adapter
owns the actual cut — see
[the adapter doc](../../wealthdb/docs/adapters/ubs.md). The
silver-side facts that make a clean date-splice possible:

- **Both silvers promote `value_date`.** PSN's `events.timestamp`
  is the MT940 booking/value date; web's `transactions.value_date`
  is the CSV "Value date" column. Identical by construction for
  cash movements in the overlap window, so the splice needs no
  per-row matching.
- **The cutover date is derivable from PSN silver:** PSN's feed
  go-live for a relationship is `MIN(snapshot_at)` in
  `psn.dump_runs`.
- **Inter-account transfers carry both sides.** UBS reuses the
  same `Transaction no.` for the debit row in the source account
  and the credit row in the destination account; the web silver
  uses a compound PK on `(transaction_external_id,
  account_external_id)` so both rows survive a per-account splice.
  The shared number is not only a key hazard: it is an identity
  the gold adapter re-exports as `payload.bank_ref`, and gold's
  internal-transfer matcher pairs the two rows on it without
  looking at their amounts.
- **One number, several movements — WITHIN one account.** The
  compound PK above answers the case UBS reuses a number ACROSS two
  accounts. It does not answer the case the bank reuses one within a
  single account, and the bank does, in two shapes. A deposit product
  (a call deposit, a fixed-term deposit) stamps the number derived from
  its own serial on every increase, decrease, repayment and monthly
  interest payment, so the product's whole life shares one number; and
  a cross-border payment carries the correspondent bank's third-party
  charge under the number of the payment it belongs to. Keyed on the
  bare number those rows overwrite each other, and because the write is
  an `ON CONFLICT` upsert it is indistinguishable from a re-load of the
  same row: nothing fails, nothing is logged, and the survivor looks
  like a complete account. `_assign_export_txn_ids` therefore leaves the
  bare number on exactly one member of the group and gives every other a
  suffix derived from its own content, the way the statement era has
  always suffixed the legs of a split movement. Which member that is is
  not re-decided per dump: UBS clamps its transactions UI and
  `--lookback` is a window, so a run routinely sees only PART of a
  group, and re-auctioning the number there would overwrite the row that
  held it and store the newcomer a second time — the silent loss this
  scheme exists to prevent, by the back door. So the row silver already
  holds the number under keeps it; a window that does not cover that row
  leaves the number untouched and suffixes every member it does carry;
  and only a group no row holds yet picks, where the largest movement
  takes it — the advice pass is keyed by the bare number, and an advice
  names the payment rather than the fee beside it. The holder is
  recognised by its movement even when the bank has since restated the
  security's name on it: the one member matching the holder's dates,
  amounts and kind in everything but Description1 keeps the number and
  takes the new name, rather than landing again beside itself. The suffix is
  content-derived rather than positional — bar the ordinal separating
  two rows identical in every movement field — so a dump covering a
  different slice of the same group converges on the same rows.
- **The two transaction-ID schemes do not overlap.** Web uses
  UBS's "Transaction no."; PSN derives event IDs from SWIFT
  message references (`mt515:…`). There is zero overlap between
  the two ID spaces, so per-row identity matching is not
  possible — the date-splice is the only safe merge.

**Note on Trade date vs Value date.** Web's CSV has four dates per
row: Trade date, Trade time, Booking date, Value date. PSN MT940
exposes booking + value date. Both silvers promote **Value date**
as the splice key for consistency.

**Three eras, not two.** The web silver's `transactions` table holds
two of them. Rows from the live CSV export carry UBS's "Transaction
no." as their `transaction_external_id`; rows reconstructed from the
Account-Statement PDF archive (migration 0002, §3.8) carry a
collector-minted content hash prefixed `stmt:`. The MT940 feed is the
third, in the PSN silver, with ids prefixed `mt940:`. The three id
spaces are disjoint by construction, so the prefix — or its absence —
is the era a row belongs to, readable without decoding a payload.

**The advice era: the leg the export never saw.** UBS does not issue
the CSV export for every account kind — a managed mandate's cash
sub-account has none — and the MT940 feed reaches back only as far as
its own go-live. For a transfer into such an account in the years
between, silver holds the debit leg and nothing receiving it: capital
leaving a payment account and arriving nowhere, so a consumer that
pairs legs to recognise an internal move sees money leave the
household that never left.

UBS does record that leg, in a document. It issues a Credit Advice to
the account a payment lands on and a Debit Advice to the account it
leaves — one PDF per side, each addressed to its own account, and
**both printing the same TRX-No.**, which is the "Transaction no." the
export puts in its id column. The loader parses those advices and
stores the movement under that bare number with the advice's OWN
account, which is not a new id scheme but exactly the shape the
compound primary key was built for (§3.6 above, migration 0001): the
missing leg lands beside the leg the export already holds, and a
consumer that pairs legs on a shared transaction number finds both.

Three consequences follow from sharing the export's id space rather
than minting one:

- **An advice never overwrites.** It states four facts — direction,
  amount, currency, and the booking and value dates — where the export
  and the feed state all of those plus the booking type, the
  counterparty and the trade date. Because the two feeds meet on the
  same key here, the insert is `ON CONFLICT DO NOTHING`: the advice
  fills a hole, it does not restate a row that already has an owner.
- **An advice never restates a movement the statement era already
  printed.** Against the statement the key argues the other way: a
  statement row is keyed by a minted `stmt:` hash, so the very booking
  an advice names can already sit on the same account under an id the
  primary key can never collide with, an advice having been issued for
  a payment the account's own statement went on to print in its ledger.
  Writing it again would not fill a hole, it would book the payment
  twice, which is the phantom flow this path exists to remove. So the
  movement is looked for by its CONTENT before an advice is written —
  same account, same currency, either date, same column, same
  magnitude — and an advice that finds it is dropped in favour of the
  ledger row, which carries a booking type and reconciled against the
  statement's printed running balance.
  Each existing row can excuse at most one advice, so two genuinely
  distinct same-day payments of one amount both still land; and the
  advices are written after every statement in the same pass, so the
  order the PDF workers happen to finish in cannot change the result.
- **The MT940 floor does not gate it.** That floor exists because the
  statement era mints ids of its own, so a statement and the feed
  recording one booking produce two rows nothing can recognise as one.
  An advice carries the feed's own id, so an overlap there collides on
  the key and the first rule resolves it in the richer row's favour —
  and the floor is keyed by account, while the whole value of an
  advice is the leg on the account the feed does not cover at all. What
  the floor would have caught against the statement era, the content
  check above catches on the movement itself.

The price is that the prefix rule above no longer identifies the rail
on its own: an advice row has no prefix and is not an export row. What
distinguishes it is its payload, which carries `document` =
`payment_advice_pdf`; that marker is also what the loader's
document-generation purge deletes on, so advice rows re-derive when the
parser moves without the delete reaching an export row.

Not every document UBS labels an advice is a payment — a mortgage
interest settlement and a safe-deposit-box rental bill wear the same
label — and neither prints a TRX-No. The absence of one is what rejects
them: a document that does not state the id its row would be keyed by
is not a movement the parser can place, and stays in the `documents`
catalog alone.

The two web eras also write the amount columns to different conventions,
because each records what its own source states. `amount_debit` /
`amount_credit` on an export row are the CSV's "Debit" / "Credit" cells
verbatim, already signed by the sheet; on a statement row they are the
figures the statement *prints*, and a statement prints a debit as a
positive figure in its debit column (a printed trailing minus is the only
thing that makes a stored figure negative). An advice row follows the
printed convention too: the advice states one unsigned total and says
which way it moved in its headline, so the figure goes in the column the
headline names. What the eras do state identically is *which* column
carries the figure. A consumer that needs a signed amount must therefore
take the direction from the column, not from the stored sign — which is
what the adapter's projection does.

The PDF archive reaches further back than the export window and the
feed, and its coverage runs forward into both. Silver keeps each era's
rows as it finds them: the statement loader dedups only *within* the
archive (the same booking printed on a monthly and an annual statement
hashes identically, so the upsert collapses it) and stops each account
at its MT940 floor, but nothing in silver reconciles a statement row
against the export's or the feed's record of the same booking — the
ids do not meet, and silver does not decide merge policy. The wealthdb
UBS adapter owns that reconciliation: see the era fold in
[the adapter doc](../../wealthdb/docs/adapters/ubs.md).

**A batch order becomes its payments.** The statement books a batch of
e-banking payments as ONE movement carrying the batch total, listing the
beneficiaries under it and closing the list with a `<N> times <rail>`
trailer. Left whole, that row is several unrelated payments added
together, and nothing downstream can take it apart: the narrative is
every beneficiary concatenated and the amount is a sum nobody was paid.
The loader stores one row per payment instead, so every consumer sees
plain single transactions.

The trailer is what makes the batch legible, and it is printed by
booking type rather than belonging to one — `MULTI E-BANKING ORDER` and
`MULTI PAYNET ORDER` both bundle, and the same trailer closes an
ordinary single order with `1 times`. The parser therefore matches the
shape, not the booking type: every bundle the statement can print is
split, including types not yet seen, and every single order is left
alone by construction.

A split is only taken when it can be proved, because a wrong one moves
money between beneficiaries: as many payments must be found as the
trailer counts, AND they must add up to the movement's printed total.
A movement that fails either check is stored whole, exactly as before.
Each payment keeps only its own narrative lines, and its own
counter-account and mandate markers are read from those lines rather
than inherited from whatever else the batch contained; the printed
running balance belongs to the last payment, since the balances between
them were never printed. The row ids are the movement's content hash
suffixed with the payment's position, so a batch re-read from the annual
statement still dedups against the monthly one, and identical amounts
inside one batch stay apart. `payload.multi_leg` records the position,
the count and the rail. A batch row written by a load that predates the
split is retired when its payments land, so the total is never counted
both whole and in parts.

### 3.7 Documents (PDFs)

Web-only — PSN has no document concept. The silver `documents`
table is one row per PDF, indexed by date and type. The web loader
populates `documents.account_external_id` /
`documents.portfolio_external_id` via best-effort label parsing,
when the listing-row label contains a discoverable IBAN / portfolio
code.

The PDF binaries live on disk under
`<bronze-root>/<dump-ts>/documents/`, named by their content hash
(`<sha256>.pdf`) — the silver `documents` table only indexes them.
Content-addressed naming means an unchanged document keeps the same
filename across runs (UBS otherwise serves it under a fresh per-session
token each time), so the tree stops accreting a new name per
re-download and each document gains a stable cross-run identity
(`content_sha256`, recorded in `run.json`) for later dedup tooling. The
loader is filename-agnostic — it catalogs whatever `*.pdf` the manifest
lists — so older token-named dumps keep loading unchanged.

A document the archive never listed has no manifest entry and so no
label; §3.7a covers how one is taken in.

### 3.7a Documents the bank delivers by hand

Not every document UBS produces reaches the e-banking archive. A
statement the bank runs on request — a Statement of assets for a
month-end it never published one for, say — is delivered directly and
appears in no listing, so `download` cannot reach it and no listing row
describes it.

Those PDFs are placed in `<bronze-dir>/supplied-documents/` and indexed
by the same `documents` table as the scraped archive, which is what
carries them into the historical walk (§3.8) with no second pipeline.
Four properties make that safe:

- **The directory is not a run dir.** Its name is not a UTC-timestamp
  slug, so `scan_bronze` never walks it as a dump and `prune` cannot
  delete it — the one deletion path that takes a whole directory accepts
  only `<bronze>/<run-slug>`. It sits INSIDE the bronze tree all the
  same, which is what keeps silver reproducible from bronze alone: a
  directory reachable only through a CLI flag would be dropped by every
  `--force` rebuild and every flag-less nightly load.
- **The document identifies itself.** Its type, its as-of date and its
  portfolio are read from the text it prints on its own first page
  (`Statement of assets` / `As of <D Month YYYY>` /
  `Portfolio BBB-AAAAAAAA-NN, valued in …`), never from a filename, which
  a hand-placed file makes no promises about. Measured across the whole
  archive, those anchors give the same answer as the listing label on
  every document that has both; the header's `As of` line is what the
  label states, not the `valued as of` note some year-end statements
  print a day or two earlier. A PDF whose body does not carry all three
  anchors is named in a warning and left out of `documents` entirely,
  rather than catalogued under no type where it would look ingested and
  never be parsed.
- **Its identity is its content.** `doc_token` is `supplied:<sha256>`,
  so re-running collides on the primary key and changes nothing.
- **It cannot double a document the archive already served.** The same
  bytes collide on `documents.content_sha256`, which is UNIQUE; the same
  statement re-rendered to different bytes still collapses on the
  historical tables' own key, which carries `(as_of_date, portfolio,
  account, ISIN)`.

Because the pass runs before the dump loop, a `--force` rebuild picks the
supplied documents up with no special handling. On a night when every
dump is already loaded, the archive walk would otherwise sit behind the
already-loaded skip and never read a newly placed document, so indexing
one is a second reason — beside a moved parser — for the loader to derive
the archive without a new dump.

### 3.7b Portfolio securities transactions

Web-only, and a different surface from §3.6's splice. The cash
transaction list reaches the accounts the homepage files as cash
tiles; a managed portfolio's own movements — its securities trades,
and the corporate actions against its holdings — are published on a
separate list under the legacy `/assetview/` path the SPA hosts in
child frames.

**It is driven, not requested.** The surface answers an export from
the list the page is showing, so a request that reconstructs that
state — however faithfully, and the parameter blob its own frame is
addressed by was reconstructed exactly — is answered with the rendered
page instead of a file. Three things follow, and between them they are
the whole shape of the pass:

- **The window is set through the filter panel**, by filling its date
  fields and submitting. The fields carrying it
  (`dateFromIndex[0]` / `dateToIndex[0]`) exist only in the panel,
  which the SPA builds into a frame of its own and which is absent
  from the document the endpoint answers with; there is no request
  that carries them. The surface then keeps that period **per scope,
  server-side, across sessions**, which is why an export never has to
  restate it — and why a walk leaves the period it asked for behind
  for the next person to open that page.
- **The file is taken by clicking** the list's own CSV control.
- **The answer states its period** in a footer line, which the walk
  checks against what it asked for: a window that was not applied
  comes back as the list's default and would otherwise be stored as
  though it were the answer. A start date *later* than asked for is
  accepted — that is the archive's floor (observed at 2024-01-01),
  and what it returns is real.

**Every portfolio is reached through the switcher in the SPA's
header**, which both lists them and moves between them
(`landmarks.PORTFOLIO_SWITCHER_BUTTON`). It has to be: the route takes
a `portfolioUid` and the homepage offers exactly one, so the route
alone reaches one portfolio. Two traps sit here.

The first is that the switcher is not the chooser the list renders
into its own HTML — that one is the custody-account filter *within* a
portfolio, and selecting a scope on it does not move the page. The
second is that re-navigating to the route after switching silently
undoes the switch, because the route still carries the one
`portfolioUid`; so the window is set on whatever list the page is
already showing rather than on a freshly-navigated one.

The switcher also lists the consolidated views beside the real
portfolios, and those answer for bookings a real portfolio already
gave. A portfolio already harvested is therefore not fetched twice,
keyed on what the export itself says it is for.

Silver keeps it in `portfolio_transactions` (migration 0012),
deliberately apart from `transactions`, because two things have to be
settled before one of these rows can be a ledger row and neither is
answerable from this feed:

- **Which account settled it.** The export names the custody account
  the securities moved in and the currency the cash moved in, but not
  the cash account that paid. A portfolio holds one cash account per
  currency, so portfolio + settlement currency determines it — via a
  roster this collector does not carry.
- **Whether another rail already has it.** From the day the sibling
  PSN feed's MT515 confirmations begin, the same trade arrives there
  too under an id this one cannot match.

Both are cross-source questions, so both belong to the adapter (§6),
and both are answered there now — see the gold adapter's own
[§11](../../wealthdb/docs/adapters/ubs.md).

**Keying a row.** Every actual trade carries UBS's own `External
reference`; corporate actions and FX legs are published without one
and are keyed by a content hash instead. Only columns that state the
BOOKING may enter that hash. The valuation is not one of them: each
scope values the same booking in its own reporting currency, so the
value and the currency beside it differ between a portfolio's own
export and a consolidated view's — and hashing them gave one booking
two ids, which defeated the dedupe below and let a trade reach the
ledger twice.

**The consolidated scopes.** UBS offers them in the same chooser as
the real portfolios, reporting their rows under a lettered id of their
own, so the same booking arrives twice. The loader keeps the copy that
names a numbered portfolio (`load.names_a_portfolio`), falling back to
the consolidated copy only when nothing else reported the booking.

**Two identifier spaces that look alike.** The export writes an
account as `<rel> <body>.<code>` and a portfolio as `<rel> <body>
<code>`, and the sibling feed keys them differently: an account's
middle group is zero-padded to ten digits, a portfolio's is not.
Padding the portfolio produced an id that joins to nothing
(`load.portfolio_id_canonical`).

**Units travel with the figures.** A quantity is counted in pieces
(`4'000 p`), a price is quoted per unit (`250.75 a`) or in percent
(`100%`), and the annotation is read and dropped — refusing it left a
real trade with no quantity at all. An FX leg states a PAIR in one
cell (`50'000 / -45'000.5`: bought the one, sold the other), and that
is not a figure: taking the first half would book one leg's amount
under the other leg's currency, so the pair reads as no value and both
numbers stay in the payload (`load.parse_grouped_decimal`).

### 3.8 Historical snapshots reconstructed from PDFs

The web silver also reconstructs **historical** position + cash
snapshots from the PDF document archive. Three dedicated tables (the
two document types that write `transactions` instead — the
Account-Statement movement ledger and the Credit/Debit Advices — are
in §3.6):

| Table | Source PDF type | Granularity |
| --- | --- | --- |
| `historical_position_snapshots` | "Statement of assets" PDFs (semi-annual, sometimes quarterly) | One row per `(as_of_date, portfolio, account, ISIN)`. Cash positions have `instrument_isin = NULL` and a populated `account_external_id` (IBAN); securities have `instrument_isin` set and `account_external_id = ''` (UBS doesn't surface the safekeeping account in the printed text in a way we can extract). |
| `historical_cash_balances` | "Account Statement" PDFs (monthly) | One row per `(period_end, account_external_id)` with opening / closing balance + turnover totals. UBS only issues an Account Statement for a given month when the account had activity in that month, so coverage is uneven; year-end months tend to cover the full account inventory. |
| `historical_mortgages` | "Maturity notice" PDFs (one per fixed-rate / SARON interest-roll period, typically quarterly) | One row per `(as_of_date, account_external_id)` with the outstanding principal (negated to liability sign), the product line → rate type, and the collateral description (migration 0005). |

(The live-fetch side of mortgages is the `mortgages` table —
migration 0004 — fed by positions.csv's "Pro memoria - Mortgages"
rows, which reuse the IBAN column for the fixed-rate term and carry
the UBS-internal mortgage number as `account_external_id`. Gold
projects both tables through the same mortgage account/instrument
path: one `AccountChange{Kind: mortgage}` + a negative-value
position.)

**Position-row shapes the Statement-of-assets walker handles.**
The securities walker anchors on each `Valor … - ISIN …` line and
reads the headline row just above it. It looks no further back than
the previous holding's own `Valor` line, so a holding whose headline
does not match is left out rather than given its neighbour's. The
headline comes in three flavours:

1. **Listed securities** — the `cost-price / market-price /
   market-gain%` triple. A one-letter price qualifier UBS sometimes
   prints after the market price (e.g. a structured product's
   `120.00 B 20.00%`) is tolerated.
2. **Private-markets / SPV holdings** (UBS-sponsored Private Markets
   funds and SPV interests) — these print a single price, or the
   literal `n.a.`, where listed rows print the triple. The price is
   the NAV per unit (units × price = market value), stored as
   `market_price`. The funded "Outstanding Shares" row carries the
   NAV; the `n.a.` Net/Unfunded Commitment rows are 0-valued and
   skipped (the same fund's commitment ISINs would otherwise add
   value-less rows).
3. **Overview-only asset classes** — UBS issues no Detailed-positions
   page for some portfolio types (e.g. precious-metals custody), so
   such a holding has no per-instrument row anywhere in the PDF. Its
   asset-class total is recovered from the relationship overview as a
   single synthetic position (`description = "Precious metals &
   commodities"`, a non-ISIN-shaped `instrument_isin` key of the form
   `PM-<portfolio>`). The overview prints the figure once per
   portfolio-currency PDF, so the walker emits it only from
   USD-valued PDFs (the reporting-currency baseline); the
   duplicate USD copies collapse on the silver PK. The gold adapter
   recognises the non-ISIN key and leaves the canonical ISIN null.

**Headlines whose figures run together.** The listed and
private-markets patterns find where one figure ends by the decimal
point a price prints. Some headlines print none, or leave a column
blank, and their figures run together:

- an integer cost price, such as `1 200 1 150 -4.17%`;
- a blank market gain, printed when the price has not moved, as in
  `43.50 43.50 4 350`;
- an integer private-markets price, such as `1 1 000`.

A block neither pattern reads is read from its figures alone. They are
split every way the digit grouping allows, and a reading must agree
with itself:

- a printed market gain equals the market price over the cost price,
  less one;
- a blank market gain means the two prices are the same figure;
- a private-markets row prints one price and no gain.

The headline is read only when exactly one reading agrees. Otherwise
the holding is left out.

**The cost side of a holding.** A holding prints up to four lines,
and the column header names what each carries on its right-hand
side:

| Line | Right-hand columns | Silver column |
| --- | --- | --- |
| 1 | cost price, market price, market gain, market value, % NA | `cost_price`, `market_price`, `market_value` |
| 2 | average buy exchange rate, current exchange rate, exchange gain, accrued interest | `acquisition_fx_rate`, `current_fx_rate`, `accrued_interest` |
| 3 | cost value, market-price date, unrealized P/L (a percentage) | `cost_basis`; a private-markets holding's `nav_date` |
| 4 | last purchase date | `last_purchase_date` |

- Line 2 prints the two rates only when the holding's currency
  (`currency_iso`) differs from the portfolio's
  (`market_value_currency`). Both rates convert the first into the
  second. A cash line prints one rate, its current one, in the same
  `current_fx_rate` column.
- The accrued interest follows the exchange gain on line 2, where the
  holding accrues any. It is printed in the market-value column, so it
  is in `market_value_currency`. It is read only behind the two rates:
  on a holding in the portfolio's currency, nothing on line 2 tells a
  figure in that column from one that ends the wrapped description.
- `cost_basis` is the statement's "cost value": the units at their
  average cost, at the average buy rate, in `market_value_currency`.
  The cost price, by contrast, is in `currency_iso`.
- The left-hand side of lines 2 to 4 holds the wrapped description, the
  sector and the distribution notes, so each figure is read from the
  line's right end. A distribution amount can sit one space before
  the cost value and read as part of it. The printed unrealized P/L
  (market value over cost value, less one) decides which reading is
  the cost value; when none agrees, `cost_basis` stays NULL.
- A holding whose market gain is blank leaves the P/L blank too, and
  its line 3 ends in the cost value alone. With the price unchanged,
  that figure equals the market value, and a line is taken as line 3
  only when it does.
- A distribution's ex-date (`Distribution: <date>`) is never read as
  the last purchase date.
- A private-markets holding prints no cost value. Its line 3 carries
  the NAV date (`nav_date`, ISO `YYYY-MM-DD`), and its line 4 the last
  purchase date.

**Why separate from the live-fetch `positions` / `accounts`
tables.** Two reasons:

1. **Identity model differs.** The PDFs use UBS's `NN` portfolio
   numbering (`01`, `02`, …), which the loader expands to the
   16-char PSN-aligned form: the 4-digit branch, the 8-digit base and
   the 4-digit portfolio number, all zero-padded. It joins directly
   against PSN's `PrtflId`. The live-fetch `portfolios` table uses
   4-char UBS-internal codes (e.g. `RNNN`, `NNNN`) which are a different
   surface. Putting them in one table would require either a
   mapping that doesn't exist in either source, or a `source`
   column that gold would still have to filter on every query.
2. **Cadence + authority differ.** PDFs are bank-of-record end-
   of-period snapshots, semi-annual at best for positions. Live
   fetches are intra-day customer-side snapshots. Keeping them in
   separate tables lets a consumer pick per use case (PDF snapshots
   for historical attribution; live fetch for "what does the
   customer see right now").

**Cross-feed join keys.** `historical_position_snapshots` carries
the portfolio identifier in PSN's `PrtflId`-aligned form (the
expansion above), so it joins PSN's `portfolios` directly, and PSN's
`holdings` through `safekeeping_accounts`. `historical_cash_balances`
carries `account_external_id` as the IBAN — the same canonical form PSN
uses. How gold splices the historical and PSN snapshots (which feed
wins per date) is owned by the wealthdb UBS adapter — see
[the adapter doc](../../wealthdb/docs/adapters/ubs.md).

**Known parser limitations.** The parsers in
[pdf_parsers.py](pdf_parsers.py) are best-effort:

- The "sector" field on securities sometimes captures the
  preceding `Distribution:` line instead of the actual sector
  label. Treat `sector` as informational, not authoritative.
- The cash-position "description" sometimes pulls in adjacent
  numeric noise (e.g. a balance-date that flowed into the
  description column on the printed page). The IBAN, currency,
  and market value are reliable.
- UBS only issues Account Statements for months with activity,
  so missing months don't imply missing balances — the closing
  balance from the most recent prior month is still valid until
  the next statement.
- Within a row, individual numeric columns may be NULL even when
  the row is present. The four columns come from two different
  PDF sections: opening / closing balance live on the in-table
  header / footer rows (present in essentially every statement),
  while `total_credits` / `total_debits` come from the "Your
  account at a glance" summary block (only emitted for statements
  with non-trivial activity, so it is present on only a subset of
  statements).
  Treat NULL as "the source PDF did not print that line" rather
  than as 0.0.

### 3.9 Securities advices: capital calls and contract notes

Two document kinds state what a holding was bought for where neither
the statement of assets nor the PSN MT535 feed does. The `advices`
table (migration 0013) holds one row per document, every figure as
printed.

| `kind` | Archive `doc_type` | What it states |
| --- | --- | --- |
| `capital_call` | `Private Market Letter` whose cover page is titled "Capital Call" | the called amount, the cash the call takes, the value date, the fund's ISIN |
| `contract_note` | `Contract note` | a purchase outside the exchange: units, price, market value, placement fee, stamp duty, the debit, any prepayment it settles against, the conversion rate |

- **A capital call states no units and no price.** The fund issues
  the units later, at a NAV the statement of assets reports. MT535
  carries no book cost for such units, so the calls are the record of
  what was paid in. `amount` is the called amount. `settlement_amount`
  is the breakdown's investor total: the called amount plus anything
  charged on top of it, such as equalisation interest on a late
  closing.
- **The notice is the fund administrator's prose.** It has no fixed
  layout, so each figure is read from the sentence that states it
  rather than from a fixed position.
- **Other Private Market Letters are not calls.** Quarterly reports and
  other letters share the label. The cover title selects the calls,
  and it is read from the first page alone, so a long report is not
  read in full.
- **A prepayment is a contract note of its own.** It states the
  amount and the conversion rate but no units. The subscription that
  settles against it prints "Minus your prepayment" (`prepayment`)
  and debits only the difference.
- **Charges are separate from the amount.** `placement_fee` and
  `stamp_duty` are as printed, beside the market value, never folded
  into it.

### 3.10 The transaction list a Statement of assets prints

A Statement of assets may close with a "Transaction list": every
securities booking in the statement's period. The `statement_trades`
table (migration 0014) holds one row per booking per statement, as
printed. For each booking the list states:

- the trade date and time, the value date and the booking text
  ('Purchase Spot', 'Sale Spot', 'Incoming from spin-off');
- the quantity, the security (name, valor, ISIN) and its currency;
- a purchase's price and purchase rate, or a sale's average cost, its
  average buy rate, its price and its transaction rate;
- the cost value, the transaction value and, for a sale, the realized
  P/L as a percentage of the cost value;
- the charges, one column per label: tax (`taxes`), "Various" (`fees`),
  brokerage (`commission`), stock exchange, third-party executions,
  and a foreign financial transaction tax;
- the settlement amount, the place of execution, the settlement and
  order numbers, the custody account and the cash account.

How it is read:

- **By word position.** The header stacks up to seven labels per
  column, one per printed row of a booking. A figure means what the
  label in its column and its row says, and the plain text cannot tell
  an empty cell from a missing one. So the parser places each word by
  its x-position against the header's column edges, and each row by its
  distance from the booking's first row in units of the header's row
  pitch.
- **A long booking text moves the rows below it.** When the booking
  text wraps past two rows, every cell below the first row prints that
  many rows lower. The trade time is the exception: it stays under the
  trade date. The parser reads each cell at its header row plus that
  shift. `payload` keeps every cell at the row it prints on.
- **A booking starts at a row with a date in the first column and a
  booking text.** The list's closing totals and the page footer end it.
  The rows of the description column vary with the description's length,
  so the settlement number, the Valor/ISIN line and the two accounts are
  told apart by their shape.
- **Currencies, as printed.** Prices and charges are in the trade's
  currency (`currency_iso`, `charges_currency_iso`). The cost value and
  the transaction value are in the statement's reporting currency. The
  settlement amount carries its own.
- **Signs, as printed.** A sale's quantity and transaction value are
  negative; so are a purchase's settlement amount and every charge.

**What it is for.** The list is the record of a portfolio's trades that
states quantities and costs for the years before the portfolio export
(§3.7b) reaches back. It is a reading of trades, not a rail of its own.
The same trade is also on the Account Statement's movement ledger, on
the export where that reaches, and on the PSN feed's MT515
confirmations. A booking also recurs in every statement whose period
covers it, so a month-end statement and the quarter-end one around it
both list it. `settlement_no` names it across statements.

## 4. Web loader implementation notes

- **Migration runner.** Applies pending migrations in numeric
  order, commit after each, record version in `schema_meta`.
  Mirror `ubs-psn/load.py`'s pattern.
- **Bronze scan.** Walk `<bronze-root>` for subdirectories
  matching `YYYYMMDDTHHMMSSZ` and skip those already in
  `dump_runs`.
- **Per dump:**
  1. Parse `run.json` to get the window bounds; write `dump_runs`.
  2. Parse `positions/*.csv` (per-portfolio CSVs first, then the
     consolidated default-view CSV) → upsert
     `banking_relationships`, `portfolios`, `accounts`,
     `positions`. Per-portfolio first means real portfolio
     assignments win and only truly-unassigned accounts land
     under the synthetic catch-all portfolio.
  3. Parse `transactions/cash_*.csv` (per-account, per-window) →
     upsert `transactions` keyed by `(transaction_external_id,
     account_external_id)`.
  4. Catalog `documents/*.pdf` files referenced in `run.json` →
     upsert `documents`, computing `content_sha256` per file.
  5. Walk the `documents` table, route every PDF whose label or
     `doc_type` names a type the parsers read to `pdf_parsers.py`, and
     upsert the parsed positions / cash balances into the
     `historical_*` tables. The Account-Statement movement walker
     and the Credit/Debit Advice parser write `transactions` from
     the same walk (§3.6), and the capital-call and contract-note
     parsers write `advices` (§3.9). A Statement of assets also
     writes its transaction list to `statement_trades` (§3.10).

- **PDF parsing isolation.** `pdfplumber` is bundled in the
  Docker image (`requirements.txt`). The parsers live in
  `pdf_parsers.py` and have no DB-side dependencies, so they
  can be unit-tested against a static PDF without spinning up
  silver.
- **Idempotency.** Re-running the loader on the same bronze dir
  is a no-op (PK collisions caught + content-dedup).
- **Atomicity.** One transaction per dump-run. Roll back on any
  parsing failure; re-run after fixing.

- **Run status + pruning.** `download` writes `run.json` twice: a
  `{"status": "in-progress"}` marker the moment it creates the run
  dir, then an atomic overwrite with the terminal manifest carrying
  `"status": "complete"` once the walk finishes (`--dry-run` writes
  nothing to bronze at all). This makes a crashed walk —
  which never reaches `write_run_json` — legible without leaving an
  empty run dir. `load` is unaffected: `scan_bronze` selects every
  timestamped subdir regardless of `run.json`, and `_read_run_json`
  tolerates a missing manifest, so no dump-selection guard is needed.
  `prune.py` (a thin wrapper over the shared `collectorkit.prune`
  engine) uses the status field to reclaim disk: it deletes whole run
  dirs that are non-complete — `in-progress` / `dry-run` / no
  `run.json` — and keeps every complete dump's load inputs untouched.
  `debug_subdirs` names `screenshots/`, the one thing a complete dump
  gives up: the landmark DOM + screenshot captures `download --debug`
  writes, which `load` never reads. ubs-web's other diagnostics —
  screenshots, Playwright traces and QR PNGs — go to the external
  `--screenshot-dir` / `--trace` / `--qr-png` outputs (the `/debug`
  mount), outside bronze. Because the only deletion path that can touch
  a load input is a whole non-complete dir, the classification that must
  be exact is the
  legacy (statusless) fallback: a pre-change `--dry-run` shell carries
  a full-looking manifest with `dry_run: true`, so completeness there
  is `manifest present AND not dry_run`, not the bare manifest-presence
  fidelity-web uses.

## 5. Cards — observed

Recorded from hand-driven `explore` sessions, 2026-09-06. The card
surface is a **REST API the SPA reads**, not a page to scrape: every
figure below comes from JSON the netbanking front end fetches for
itself, and the toolbar's CSV/PDF exports are a lossy rendering of it.

### 5.1 The endpoints

| Endpoint | Returns |
| --- | --- |
| `GET /api/v2/credit-card-accounts?valuationCurrency=<CCY>[&limitedData=true]` | the roster: every card account, the cards under each, balances, limits, and the link relations below |
| `GET /api/v1/credit-card-transactions?creditCardAccountIds=<id>[&creditCardIds=<id>][&timePeriodFrom=<date>&timePeriodTo=<date>]&transactionDateType=BOOKING\|PURCHASE[&transactionStatus=BOOKED]` | the ledger |
| `GET /api/v1/credit-card-transactions/extract?<same params>` | the toolbar's CSV / PDF export of that ledger |
| `GET /api/v1/credit-card-invoices?creditCardAccountIds=<id>[&latestInvoice=true]` | the billing periods |
| `GET /api/v1/credit-card-invoices/<id>` | one period's totals |
| `GET /api/v1/credit-card-invoices/<id>/extract` | that period's statement PDF / CSV |

The roster advertises the rest as HATEOAS links (`transactions`,
`invoices`, `invoiceConfiguration`, `interestStatements`,
`paymentToCard`, `self`), so a walk starts at the roster and follows
links rather than composing URLs.

**`transactionDateType` is a required choice, not a default.** The
window filters on either the purchase date or the booking date. The two
disagree for any row transacted near a period boundary, so the value a
fetch uses is part of what its window means.

### 5.2 The roster is the only authoritative enumeration

The homepage tiles an anchor per card account under
`#/cards?target=card-account-transactions&accountId=<token>`, the same
shape the cash anchors use. **It is not a complete enumeration** — the
tiled set is a subset of what the roster returns, and a collector keyed
on those anchors under-collects with no error to show for it. Enumerate
from `/api/v2/credit-card-accounts`.

A card *account* (`accountType=CREDIT_CARD_ACCOUNT`) holds one or more
*cards* (`cardNumber`, `productName`, `cardType`, `cardStatus`), and the
accounts nest (`structureType=COMPLEX_TLA`, with `topLevelAccount` /
`relatedCardAccounts` links). The ledger is addressed per account, and
optionally narrowed to one card with `creditCardIds`.

### 5.3 The ledger row

| Field | What it is |
| --- | --- |
| `_id` | **a per-session handle, not a key.** An opaque ~65-char token, re-minted at every login: two dumps a day apart shared not one id. Within a single session it is stable and distinguishes rows, which is all the paging needs; the silver row key is minted from row content instead (`card_parsers.py`) |
| `transactionNr` | **not an id** — a one- to three-digit sequence number that repeats heavily across rows. It reads as a position within a statement, not a key. Keying on it would collapse most of the ledger into a hundred rows |
| `transactionDate` / `valueDate` | purchase timestamp / booking date |
| `postingAmount` / `originalAmount` | `{amount, currency}` each — equal on a domestic row, different on a foreign-currency one |
| `details` | **the merchant descriptor** — the terminal string, the input a merchant signature is built from |
| `merchantName` | **the merchant CATEGORY in words**, an MCC description (`Grocery stores`, `Parking & Garages`) — *not* a merchant name |
| `merchantGroupCode` | a coarser UBS grouping, many MCC descriptions to one code, with a catch-all bucket |
| `cardNr` | which card under the account booked the row |
| `bookedAccountId` | **the CARD, not the account**, on any account holding more than one. See below |
| `transactionStatus` | `BOOKED` or `RESERVED` |
| `settledInInvoice` | whether the row has been billed |
| `exchangeRate`, `effectiveExchangeRate`, `exchangeRateDate`, `markup` | present only on a row converted from another currency |
| `parentTransactionNr` / `parentTransactionDate` / `parentTransactionDescription` | present on a reversal, naming the row it reverses |

**`bookedAccountId` names a card, not an account.** Every other card
table is keyed by the account id the roster enumerates, so a row stored
under this field verbatim joins to nothing — no account, no invoice, and
outside gold's spending scope, which selects from the accounts table. The
roster states the relation itself: each card node carries
`liableAccountId`, and the loader resolves through that map before
storing. On a single-card account the two ids coincide, which is why the
defect is invisible until an account holds a second card.

**`invoiceStatus` is an envelope**, `{"statusCode": …}`, not a string —
one of several single-valued fields UBS wraps. Bound straight into a TEXT
column it raises and takes the whole dump's load down with it.

**A `RESERVED` row is not yet a transaction.** The ledger returns
pending authorisations beside booked ones, and they carry none of
`_id`, `transactionNr`, `valueDate`, `postingAmount` or
`settledInInvoice` — nothing that could key them, and nothing that
would let a re-fetch recognise the same authorisation twice. They are
unposted activity that changes shape when it posts, so they belong in a
balance rather than in a ledger, which is the same place the chase
adapter puts a card's pending charges.

**The naming is a trap worth restating:** `merchantName` is the
category and `details` is the merchant. Reading them the other way round
would key every signature on a category and hand gold an MCC where it
expects a payee. The CSV export spells the same two columns `Sector`
and `Booking text`.

The CSV export is CP1252, not UTF-8, and carries no row id.

### 5.4 Invoices are the statement channel, and they reconcile in JSON

`credit-card-invoices` returns one row per billing period with
`periodFrom` / `periodTo`, `invoicingDate`, **`debitingDate`** (when the
cash account is debited for the bill), `dueOn`, `dueAmount`,
`minimalDueAmount`, `paymentMethod` (`LSV` — Swiss direct debit — or
`SWI`), `statementType` (`INVOICE` for a closed period, `STATEMENT` for
the open one, which has no `debitingDate` yet), and `_links` to the
period's `pdf`, `csv` and `transactions`.

The per-invoice detail adds the reconciliation identity: **`balanceForward`**
(the period's opening balance), `totalDebit`, `totalCredit`,
`transactionSubtotal`, `transactionCount`, and a
`transactionsPerCardSummary` breaking the period down per card.

This is the chase `statement_balances` shape delivered as structured
data. Chase needed a statement-PDF parser gated on a
`beginning + Σ == ending` reconciliation; here the same figures are
fields, so the gate can be arithmetic on JSON rather than a parse.

### 5.5 There is no deeper channel — no statement backfill

Both the ledger and the invoice archive reach back **about 24 months**,
matching the transactions UI's own "current month and last 24 months"
cap. They are the same window, not two eras.

The eDocuments archive is **not** a third channel: its categories are
account reporting, letters, mortgages, payment services, securities,
statements of assets and stock-exchange documents — **no card
category**, and no card statement type.

So the chase precedent does not transfer. There is no export seam and
no era below it, and **no statement-backfill phase** for cards: what the
API serves is the whole of what the source offers. Invoices still earn
their place in silver — as balance anchors and for `debitingDate` — but
never as a way to reach further back.

### 5.6 What the bill has to pair with

The settlement appears on the card's own ledger under the booking texts
`DIRECT DEBIT` (the `LSV` rail) and `TRANSFER FROM ACCOUNT`, and
`debitingDate` on the invoice is the day the cash account is debited.
Those two are what gold's internal-transfer matcher has to pair.

**A card's native currency need not be that of the account that settles
it.** The matcher's amount pass partitions candidates by native currency
and cannot pair across two, which is a property of that pass rather than
a setting. Its reference pass does cross the partition, but only on a
number one source stamped on both legs — and the card ledger mints its
own ids, so the invoice and the cash account's debit share none. Where a
card is billed in one currency and settled from an account in another,
both legs therefore stay unpaired however well the projection works, and
the built-in card-payment rule keeps placing `card_spend` over a card
that *is* collected. The gold work has to answer that case rather than
assume the matcher will.

### 5.7 The per-transaction detail view

A ledger row's title is an accordion
(`span[role="button"][aria-expanded]` inside
`div[data-name="transaction-title"]`), so the detail expands in place
rather than opening a page. Nothing in the card surface carried an
"order origination" field in any capture.

That field belongs to the **cash** ledger, where a debit-card
point-of-sale payment is what has an origination to state. The cash
API (`/api/v1/cash-transactions`) carries
`bankTransactionCode.proprietary.dealType` plus per-row links
`description` and `pdfExportTrxDetail`, and a running balance the CSV
and MT940 exports do not expose. Enriching the cash ledger from it is
adjacent to the card work rather than part of it, and is not in scope
here.

### 5.8 What the card UI looks like

**Nothing in the collector reads any of this** — the card surface is
fetched from the API (§5.1), and no card selector is pinned in
[landmarks.py](landmarks.py) because no code has needed one. It is
recorded because a capture is expensive: it dates what the UI looked
like, so a later diagnosis of drift, or a need the API cannot serve,
starts from a written record rather than from another live session.

- transactions toolbar: `button[data-testid="download-csv-button"]`,
  `button[data-testid="download-pdf-button"]` (cards render **both**;
  the `title="CSV"` handle the cash surface uses is not the one to key
  on here), and comboboxes labelled `Filter by period`, `Filter by
  amount`, `Filter by category`, `Filter by transaction status`.
- ledger rows: `article[data-name="panel-reserved"]` and the sibling
  `div[data-name="splitter-reserved"]` / `splitter-booked` /
  `splitter-settled` section markers; within a row,
  `[data-name="transaction-title"]`, `transaction-subtitle` (the MCC
  description), `transaction-badge`, `transaction-balance`.
- the card page renders **no** `[data-name="number-of-trx"]` counter —
  the cash surface's readiness signal does not exist here.
- the invoice archive: nav item `data-name="AccountsAndCardsCreditCardInvoices"`.

### 5.9 What silver keeps

Three tables plus a document index, added by
[migrations/0007_cards.sql](migrations/0007_cards.sql), kept apart from
the cash tables for the reason §3.8 gives for the historical ones — the
identity model differs. A cash account is keyed by IBAN and carries the
columns that join it to PSN; a card is keyed by an opaque token and has
no PSN twin at all.

| Table | Grain | Carries |
| --- | --- | --- |
| `card_accounts` | (snapshot, account) | balance, available, limit, and the magnitude of unposted activity |
| `card_transactions` | one booked row | both dates, both amounts, the merchant and the MCC description |
| `card_invoices` | (account, period end) | opening balance, turnover, the settlement date, and whether the figures reconcile |
| `card_statements` | one PDF | the statement, indexed against its period |

Four decisions of record:

- **The key is a content id, like chase's.** Not `transactionNr` (§5.3),
  and not the API's `_id` either: that is re-minted at every login, so
  keying on it made a re-download append a second copy of the ledger
  rather than UPSERT it (migration 0008). The key is `card:` plus a hash
  of the row's own facts — card, transaction and value dates, both
  amounts and currencies, merchant — with an occurrence index, so two
  identical purchases on one day stay two spends.
- **`RESERVED` rows are counted, not stored and not summed.** They carry
  nothing that could key them, so the ledger keeps only what has posted.
  Their magnitude comes from the roster — each card's
  `balanceIncludingReserved` less its `balance`, in the card's own
  currency — because a reserved row's only figure is the *merchant's*
  currency, and summing those mixes units. The count is currency-free
  and does come from the ledger.
- **An account's roster balance already includes its reserved spend.**
  It equals the sum of its cards' `balanceIncludingReserved`, not of
  their `balance`, so `reserved_amount` records a part of the balance
  and must never be added to it.
- **The reconciliation gate is arithmetic, not a parse.** Each period's
  `balance_forward + total_debit + total_credit` is checked against
  `due_amount` and the outcome stored per period. A period that fails is
  kept and marked rather than dropped: it is still the only evidence
  that period exists, and a consumer needing an anchor can ask for the
  ones that add up.
- **A partial run never zeroes what it did not cover.** Every write is
  an UPSERT keyed on an id the source owns; nothing is deleted and no
  window is cleared. `transactions_covered` is the one derived column,
  and it is recomputed across the whole table on every load rather than
  accumulated — coverage grows as more ledger lands, so a kept answer
  goes stale in the direction of claiming more than is there. A period
  counts as covered only when the account's loaded ledger reaches past
  *both* of its edges.
- **A cash row in `historical_position_snapshots` collapses on
  re-derivation.** That table's key ends in the ISIN and a cash line has
  none, so SQLite — which treats NULLs in a key as distinct — never let
  the upsert fire. Since the historical pass re-lists the whole document
  archive on every dump, the cash rows grew by one copy per dump without
  limit, every copy byte-identical (migration 0011 collapsed those already
  stored). The loader now deletes the row a cash line is about to replace,
  which is what the key would do if NULLs compared equal. They are kept
  rather than dropped even though the gold adapter reads cash from
  `historical_cash_balances`: most carry a quarter-end date that series has
  no row for, so those observations exist nowhere else.
- **No free text is inside a card row's identity.** The id hashes the
  card, the transaction and value dates, and the amounts and currencies
  — never the merchant, which UBS re-labels between fetches. Combined
  with the upsert-only rule above, text in the key meant a re-label
  minted a second id and nothing removed the first, so one purchase
  stood in the ledger twice (migration 0010; the same defect
  fidelity-web's migration 0005 paid for on its own feed). The key
  therefore collides where two same-day, same-amount purchases differ
  only by merchant, and the occurrence index separates them — sorted on
  the facts outside the key so the assignment does not follow the order
  the API happened to page them in. Do not resolve that collision by
  putting text back.

## 6. Feed-coverage gaps the adapter must reckon with

These are source-specific limits of what the silvers carry. How the
wealthdb UBS adapter resolves them is owned by
[the adapter doc](../../wealthdb/docs/adapters/ubs.md); they are
listed here because they are properties of the feeds, not of gold.

- **Multi-relationship sweep.** The web SPA only exposes the
  currently-selected relationship, so a session captures exactly
  one: covering a second one takes a relationship switch and
  another run. Could be automated in download.py later.
- **Cost basis comes in two shapes.** The web statement of assets
  carries a per-unit `cost_price` in the instrument currency, the
  holding's cost value in the portfolio currency (`cost_basis`) and,
  for a foreign-currency holding, the average buy FX rate (§3.8).
  PSN's MT535 carries the holding's total book cost (`:19A::BOOK//`).
  Its `:70C::SUBB//` narrative adds the average unit cost (`AVER`), the
  holding cost (`AHOD`) and, for a foreign-currency holding, the
  average acquisition FX rate (`AEXR`). A holding can come without a
  book cost; its narrative then has no `AVER` either. For a
  private-markets holding the paid-in amounts are in `advices` (§3.9).
- **FX coverage differs sharply.** PSN carries 980+ FX rates; web
  carries only the 8 CHF/* pairs in the `positions.csv` footer.
- **Documents are not indexed by instrument.** `documents` is indexed
  by type + date + account only. Capital calls and contract notes are
  parsed into `advices` (§3.9), which carry the ISIN. Trade
  confirmations and corporate-action notices also name ISINs in the
  PDF body, but no parser reads them.
- **A managed portfolio's trades arrive on three rails, none of
  which covers the whole timeline.** The Account Statement PDF
  reprints a year's movements, but is published annually — so it
  says nothing about the current year until the following January.
  The PSN MT515 confirmations start wherever that feed was first
  ingested and cover nothing before it. Between the two sits the
  `portfolio_transactions` export (§3.7b), the only rail that can be
  asked for a past window at all — though not an unbounded one: its
  archive begins where the surface's own floor does, observed at
  2024-01-01, so the deep history stays the statements'. Below that
  floor the Statement of assets' transaction list (§3.10) states each
  trade's quantity and cost; it restates trades the other rails carry,
  so it is a source to match against, never one to add. Both
  consequences for the adapter are settled there rather than here: a
  row from the export is matched against what the other rails already
  settled on that cash account and day before it enters the ledger, or
  the trade is counted twice; and its account is resolved from
  (portfolio, settlement currency) to the cash account that paid,
  which is a pairing only gold holds.
