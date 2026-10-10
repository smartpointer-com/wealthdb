# Lots

The lot engine replays a source's trades into lots. On a source it
fills (§1), it writes a cost basis on every position row, and a
realized lot for every sale, where the source states none. A rebuilt
figure says so: its basis stamp is
`rebuilt/<method>/<fees>` (DESIGN.md §7.4). A stated figure is never
replaced.

Related: DESIGN.md §7.4 (the stated cost basis), §7.5 (the engine's
place), §8.1 (where the pass runs), docs/GAINS.md (the readers), and
migration 0118.

---

## 1. What it covers

Each source kind registers a policy beside its adapter. The policy
says how the engine treats the source:

| kind | mode | what the engine does |
|---|---|---|
| cointracking | fill | every coin, pooled per portfolio (§6) |
| fidelity (also the svb collector's sources) | fill | every security the statements leave without a basis; the history before and between the stated lot sets |
| schwab | fill | the same, around the statement lots |
| ubs, swissquote, viac | shadow | the ledger only, at the average method; `gains check` compares it with the average cost the source states |
| every other kind | off | nothing; private markets and hand-kept holdings state a paid-in basis already |

The modes:

- **fill** writes the ledger, the cost basis of every position row
  that states none, and a realized lot per sale.
- **shadow** writes the ledger only.
- **off** replays nothing.

Config can change a source's mode and grain (§7).

## 2. Where it runs

The engine is a pass over gold, like the enrichment pass. `wealthdb
load` and `wealthdb reload` run it after the enrichment pass.
`wealthdb lots rebuild` runs it by hand.

- One pass replays every source that is not off, together. A move links
  two sources of one kind: securities that move between them carry
  their cost with them (§3.4).
- A pass replays nothing when no input changed since the last one: the
  rows the engine reads, the config, the engine's version. It still
  replays when a reload dropped what the last pass wrote.
  `wealthdb lots rebuild --force` replays anyway.
- One gold transaction writes the whole result. A reader sees the last
  pass or this one.

## 3. The replay

### 3.1 Keys and events

A key is an instrument held in one account of one source. A pooled
source keys on the portfolio instead of the account (§6). A key's open
lots are its book.

The feed reads gold into events, per key, in time order:

| event | from |
|---|---|
| acquire | a buy; income received in kind; a transfer in nothing pairs |
| dispose | a sell; a fee or spend in kind; a loss or gift; a transfer out nothing pairs; a cash merger, cash in lieu or expiry |
| depart, arrive | a transfer out and a transfer in of the same instrument and quantity (§3.4) |
| reorg | a merger, conversion, name change or reverse split: one instrument's lots become another's, or several others' |
| split | shares that arrive on an instrument already held |
| adjust | a return of capital |
| anchor | the lots a source states at a snapshot (§4) |
| observe | a snapshot's quantity |

Within one second a transfer moves first, then corporate actions,
then acquisitions, then disposals, then the snapshot. A same-second buy and
sell therefore never dips below zero.

A snapshot stamped at UTC midnight states its date's closing balance,
so it observes at the day's last second. A snapshot stamped at any
other time is a moment.

No key is made for cash, a mortgage or an FX forward (GAINS.md §1), nor
for an instrument the source ever holds short: the book is long only.

### 3.2 Methods

A method is the order a book is relieved in:

| method | relieves first | structure |
|---|---|---|
| `fifo` (default) | the earliest acquisition | a list ordered by acquisition date |
| `lifo` | the latest acquisition | the same list, from the back |
| `hifo` | the highest unit cost | a heap on unit cost |
| `lofo` | the lowest unit cost | a heap on unit cost |
| `average` | every lot alike, at the pool's average cost | a costed pool and an uncosted pool |

A lot without a cost sorts last under `hifo` and `lofo`. A lot without
an acquisition date (a seed, a receipt nothing pairs, a spin-off)
sorts first under `fifo`. A seed stands for holdings older than any
trade the history shows.

A disposal takes lots in that order and splits the last one it needs.
A partly relieved lot keeps its unit cost. Under `average` every lot
that enters the book merges into its pool, and the pool has no single
date.

### 3.3 Reconciliation

Each snapshot is compared with the book, within max(1e-8, 1e-6 ×
quantity):

- **The snapshot holds more.** A **seed** opens for the difference: a
  lot of unknown cost and unknown date.
- **The snapshot holds less.** An **implied disposal** relieves the
  difference, seeds first. It has no proceeds and realizes nothing.
- **A blip.** Quantity an implied disposal took that comes back within
  45 days is restored, with its cost and date. A seed an implied
  disposal takes back within 45 days was a trade the snapshot showed
  early. Either way the findings count one blip, not a seed and a
  disposal.

A disposal larger than the book first restores what an implied
disposal took within 45 days, then seeds the rest.

Which snapshots observe:

- A key held at a snapshot observes the quantity its rows sum to.
- A key the account held at its previous snapshot, or traded since,
  observes zero when the account appears without it.
- An account absent from a snapshot observes nothing. Sources leave
  accounts out of snapshots. A wallet of a pooled key that a snapshot
  leaves out holds what it held when it last appeared, moved by its
  trades since. One its trades emptied holds nothing: a source drops
  an empty wallet.
- A snapshot of a sending key taken between a receipt recorded first
  and its departure is not reconciled (§3.4).

### 3.4 Transfers

A transfer out and a transfer in of the same instrument pair when they
fall within 7 days of each other and their quantities within 5%:

- **Across keys** they are a move. The lots leave the giving key by its
  method at the departure, and open on the receiving key with their
  cost and acquisition date at the receipt. A sale on either side
  between the two relieves what that side held. A receipt recorded
  before its departure moves the lots at the receipt. The sender's
  snapshots until the departure still show them, so they are not
  reconciled.
- **Across sources** they pair only between sources of one kind, whose
  instrument ids mean the same thing.
- **Inside one key** (two wallets of one pooled portfolio) nothing
  moves.
- **What the receipt falls short by** leaves as a fee: a disposal of
  that quantity at its market value.
- **What the receipt exceeds the send by** opens a lot of unknown cost
  and unknown date.

A transfer in nothing pairs opens a lot of unknown cost and unknown
date. A row of the equity-transfer ledger (DESIGN.md §13.10) that
states a cost basis opens at that cost instead. A transfer out nothing
pairs relieves lots with no proceeds.

### 3.5 Corporate actions

A policy maps each corporate action to a rule:

| rule | the engine does |
|---|---|
| reorg | pairs the account's outgoing and incoming legs of the day (below) |
| return of capital | lowers the cost of the open lots, pro rata by quantity |
| cash in lieu | sells the shares for the amount |
| expiry | disposes at zero proceeds: the premium is the loss |
| ignore | nothing: a cash-only action |

The reorg legs of one account and day:

- The same leg stated twice (a source with two feeds) counts once.
- One leg out and several in: every lot is shared among the incoming
  instruments by the legs' stated values, else equally, and keeps its
  date. Several out and one in share the new quantity the same way.
  Several on each side pair by the closest stated value.
- Legs out and in of the same instrument (a reverse split stated as
  two legs) scale its lots in place.
- A leg out alone with an amount is a cash merger: a sale for the
  amount. Without an amount its lots leave.
- A leg in alone splits an instrument already held, else opens a
  spin-off: a lot of unknown cost.

### 3.6 Income in kind and fees

Income received in kind (staking, interest, rewards) opens a lot at its
market value on the day, from `fx_daily`. A day with no rate within
seven days leaves the lot without a cost.

A fee a policy marks as in a third currency is valued the same way.
It is added to the cost of a buy and taken off the proceeds of a sale.
A fee without a rate is a finding (`fee_unvalued`).

## 4. Anchors and splicing

A source that states lots at a snapshot anchors the book:

- **The check.** The book's lots are matched to the stated lots by
  acquisition date and quantity. The matched quantity and both costs go
  to `lot_anchors`.
- **Kept.** A book that equals the stated lots is left as it is.
- **Adopted.** Otherwise the book becomes the stated lots, and the
  replay continues from them.
- **Waiting.** A sending key's snapshot between a receipt recorded
  first and its departure still states what left. Its anchor waits for
  the next snapshot.

A seed that survives into an anchor is resolved from it. The stated
lots dated on or before the seed's opening, that no costed lot of the
book accounts for, are what the seed was made of. The seed takes their
cost and dates. So does every snapshot back to the seed's opening, or
to the last split, return of capital, move or reorg that reopened it:
a seed stays a seed through those.

A seed a sale relieves is resolved from the sale's stated realized
lots the same way (§5.3). A stated lot without a date resolves no seed.

What stays unresolved stays unknown. Nothing is guessed.

## 5. Output

### 5.1 The ledger

Every pass rewrites the first five tables whole and adds its rows to
`lot_runs`:

| table | one row per |
|---|---|
| `lots` | lot the engine opened: its key, opening, acquisition date, origin, quantity, cost and cost origin, method, and closing |
| `lot_disposals` | lot a disposal relieved: when, how much, its kind, proceeds and cost, and the realized lot it made |
| `lot_realized` | realized lot (§5.3), in the shape of `realized_lots`; the view `realized_lots_all` reads both |
| `lot_findings` | seed, implied disposal, blip, resolved seed, skipped snapshot, unpaired transfer, unvalued fee, and a loss sold within 30 days of a purchase of the same key |
| `lot_anchors` | anchor: the book against the stated lots |
| `lot_runs` | pass and source: mode, grain, methods, counts, whether trades are dated by settlement, and the input fingerprint |

A pooled source's lots belong to its portfolio. `lots_at(instants)`
gives the lots open at each instant with what is left of them.

### 5.2 Positions

On a fill source, every observation writes its book onto the position
rows of its snapshot that state no basis:

- `book_value` the complete cost basis, stamped `rebuilt/<method>/<fees>`;
  both stay NULL while any part is unknown;
- `book_value_known` the cost of the lots that have one;
- `quantity_without_basis` the quantity in lots that have none;
- `open_lots` the lot count.

The rows of a pooled key share its book pro rata by quantity, a wallet
the snapshot leaves out keeping its share (§3.3). The row holding most carries
the lot count, so the counts add up. A sending key's snapshot taken
between a receipt recorded first and its departure gets what the book
holds: the quantity that already left has no basis there.

### 5.3 Realized lots

A disposal that realizes (a sell, a fee or spend at market value, a
cash merger, cash in lieu, an expiry) with known proceeds writes one
`lot_realized` row per lot it relieved:

- `document_kind` `engine`, `realized_lot_external_id` the sale's id,
  `#`, and the lot's id;
- the quantity, proceeds, cost, acquisition date and term; the gain is
  proceeds less cost;
- `disposal` what it realized, NULL for a sale.

It is primary unless the source documents its transaction: a stated
primary lot of its tax year sits on its account, or on another account
of its portfolio (`documented_txns`). That is the test the gains
reports count a sale documented by (GAINS.md §6), so a sale is never
both. It does not ask for the instrument, because a document can name
a security by its CUSIP where the trades use its ticker. The engine's
row stays beside the stated one, for the check (§9).

The stated lots of the sale itself resolve a seed it relieves (§4):
those naming its instrument within two days of it, on its own account
first, then on the other accounts of its portfolio. A CUSIP counts as
the ticker where the source's instruments link the two and no key uses
the CUSIP itself.

Wash sales are not applied. A loss the engine realizes within 30 days
of a purchase of the same key is a `wash_sale_window` finding.

## 6. Per-source policies

A policy holds what a kind knows of its feed: its mode and grain, what
its fees mean, whether its trades are dated by settlement, the
instruments that are never keys, and how its transactions read
(`lots.Policy`).

- **cointracking.** Pooled per portfolio: coins sweep between wallets on
  arrival, so a wallet holds no lots of its own. Fees included: the
  amounts are net of a trade's fee in either of its own currencies, so
  the cost holds it and nothing is added. A fee in a third currency is
  valued and added. A fiat ticker is never a key. The adapter keeps
  CoinTracking's type in the payload, which tells a deposit from an
  airdrop:

  | CoinTracking type | the engine reads |
  |---|---|
  | Staking, Reward / Bonus, Income, Airdrop, Gift / Tip | income in kind |
  | Other Fee | a fee disposal |
  | Spend | a spend at market value |
  | Lost, Stolen, Gift, Donation | lots that leave with no proceeds |
  | `Income (non taxable)`, `Other Expense` with the comment "Dust Sweeping" | a dust sweep: the dust sells and the coin is bought at market value |

- **fidelity**, which also reads the svb collector's sources. Fees
  unknown: the statements do not say whether commission is in an
  amount. Dated by settlement: a row's date is its settlement or
  effective date, so a term near the one-year line can be off. A
  corporate action reads by the action its description opens with. The
  core money market fund is cash, and no key.
- **schwab.** A corporate action reads by its stated action, else its
  description; rights redemptions, litigation proceeds and option
  exchanges move no lot.
- **ubs, swissquote, viac.** Shadow, at the average method. They state
  an average cost and no lots, and a gap in a history would fill with
  seeds of unknown cost. `gains check` compares the ledger with the
  stated cost (§9).

## 7. Config

The `lots` block of wealthdb.cfg (DESIGN.md §5.1):

```json
"lots": {
  "method": "fifo",
  "missing_basis": "ignore",
  "sources":    { "<source>": { "method": "hifo", "mode": "fill", "grain": "portfolio" } },
  "portfolios": { "<source>": { "<portfolio-id>": { "method": "lifo" } } },
  "accounts":   { "<source>": { "<account-id>": { "method": "lifo" } } }
}
```

The method in force for a key is its account's, else its portfolio's,
else its source's, else the source kind's own (`average` for the shadow
kinds), else the global one. A pooled key has no account. A method
change replays on the next pass. The replay is deterministic: the same
gold and config give the same ledger, lot ids included.

`missing_basis` is the readers' default (§8). Every source id must name
a configured source.

## 8. A missing cost basis

The ledger holds NULL where a cost is unknown. GAINS.md §8 says how
the readers count it: `ignore` leaves a figure that needs it blank,
`zero` counts it as 0. `lots.missing_basis` sets their default.

## 9. The check

`wealthdb gains check [FROM [TO]]` compares the engine with what the
sources state, per account, in the output currency:

- **realized**, per account and tax year: the sales both the engine
  and a stated primary lot cover, on the instruments both name, where
  the two cover the same quantity and the engine knows every cost.
- **positions**: at each source's last snapshot in the window, the
  positions that state a cost basis where the engine's open lots hold
  the same quantity, each with a cost. A shadow source is checked here.
- **anchors**: the anchors the engine adopted, with the quantity its
  lots matched and both costs.
- **handover**: lots that moved in from another source, with the cost
  they carried against the receiving source's first stated cost basis,
  pro rata by quantity.

A delta is a finding, not a failure. Custodians relieve FIFO unless a
specific lot was chosen, so a realized delta says which lots the
custodian sold. A positions delta says where the source's own cost
and the trades disagree: a fee the amounts leave out, a corporate
action read differently, a seed the history cannot explain.

## 10. Cost

A replay is linear in its events for `fifo`, `lifo` and `average`, and
grows with the log of a key's open lots for `hifo` and `lofo`:

- Each lot is used up once, and each disposal leaves at most one lot
  partly used, so relief work over a run is bounded by lots plus
  disposals.
- Undated lots keep a queue of their own, so a seed or a receipt joins
  the book in O(1).
- Lots that arrive together (a move, a reorg, a restored blip) merge
  with the book in one pass. A restored blip reopens the lots that
  left, so it adds no rows.
- A split, a return of capital or an anchor closes and reopens the
  book in one sweep.
- A snapshot reads the book's running totals; one that holds less takes
  the latest seeds off a stack.
- The feed finds a sale's stated lots and a transfer's partner by
  searches bounded by their windows.

The ledger is the size of the history, not of the snapshots.

The test suite holds the engine to a million events, with a hundred
thousand lots open at the end, in under ten seconds and a gigabyte
(`TestMillionEventsBudget`). Every table goes into gold column by column
in one statement per hundred thousand rows, and no ledger table carries
an index to slow the rewrite.

## 11. What the engine does not do

- Short positions and written options: an instrument the source ever
  holds short is no key.
- Wash sale adjustments (§5.3).
- A spin-off's allocation of cost: the new shares open at unknown cost.
- Cash in lieu that states no quantity: the fraction leaves at the next
  snapshot as an implied disposal.
