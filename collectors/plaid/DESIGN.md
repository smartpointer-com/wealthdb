# plaid — design notes

How the collector is built, what Plaid's rules force on it, and what was
measured.

## 1. Shape

- **One collector, many institutions.** Plaid is an aggregator. One Plaid
  account reaches every institution Plaid covers.
- **The unit is the Item.** An Item is one login at one institution. It
  has a local name, chosen at `login`, and its own token file.
- **Two environments.** Production reaches real institutions. The Sandbox
  reaches Plaid's test institutions. An Item belongs to the environment
  it was made in. `--sandbox` selects the Sandbox, with its own secret and
  its own Items. No command mixes the two.
- **No browser, no server.** The data calls are plain REST. The sign-in
  runs on a page Plaid hosts.

## 2. What Plaid's rules force

Each rule below was read in Plaid's documentation on 2026-10-01.

- **A Trial plan allows ten Production Items for its whole life.**
  Removing an Item does not return its slot. Three choices follow:
  - a name that is linked already opens update mode on the same Item;
  - a sign-in that was started is settled before a new one starts;
  - development runs in the Sandbox, whose Items are free.
- **An access token is shown once.** Plaid tells Trial users to keep
  their tokens, and offers no call that returns one again. Each token
  file is therefore written the moment the token exists, and is never
  rewritten by a later link.
- **Some institutions keep one Item per login.** A second link there
  ends the first. This is another reason a re-link is never the remedy
  for an Item Plaid still has.
- **A removed Item cannot be renewed.** Plaid answers `ITEM_NOT_FOUND`
  for it, and update mode needs a token Plaid accepts. The only way back
  is a new link: a new Item, under a new name.
- **Consent can expire.** Several large US institutions require a new
  sign-in every twelve months. Update mode renews the consent. `/item/get`
  reports the date as `consent_expiration_time`.
- **`products` is exclusive.** Plaid offers only institutions that
  support every product in `products`, and refuses a login with no
  account that fits one. So a link names one required product there. The
  other data products go in `optional_products`. Plaid then collects
  consent for them and fetches them where it can, and their absence does
  not stop the Item.
- **History depth is fixed at link time.** `transactions.days_requested`
  cannot change later. Every link asks for 730 days, the maximum.
- **Consent is per product.** A product's route adds its product to an
  Item only where the Item's consent covers it. Otherwise the call fails,
  and update mode collects the consent.
- **A public token lives thirty minutes.** A session's result stays
  readable for six hours. A sign-in settled more than thirty minutes
  after it made its Item leaves an Item nothing can claim.
- **Ledger history arrives in two steps.** For a new Item, and for an
  account added in update mode, Plaid fetches the newest 30 days first
  and the rest later. Only `/transactions/sync` reports the step, as
  `transactions_update_status`.
- **The first read of investment transactions adds a subscription** to
  the Item. A Trial plan does not charge for it. After an upgrade to a
  paid plan, Plaid bills every subscription added during the Trial each
  month, until the Item is removed.

## 3. Files

```
~/.secrets/
├── plaid.env                  app keys, managed by hand
├── plaid-token-<name>.json    one Item: access token, ids, environment
└── plaid-link-<name>.json     a sign-in that is not settled yet
```

- All three are mode 0600.
- One token file per Item. Linking one Item never rewrites another's
  token.
- A token file that exists and does not parse is an error. It is never
  read as "no such Item", because the next step would be a second link.
- The token's own text names its environment
  (`access-<environment>-<uuid>`). A file whose record and token disagree
  is refused.

Each Item has its own tree under the data dir, laid out like the data dir
of a one-source collector, so the shared prune engine runs on it as it
stands:

```
<data-dir>/
└── <name>/              one tree per Item
    ├── <name>.db        its silver database (§6)
    └── <UTC-ts>/        one download run (§5)
```

A tree holds the runs of one Item (§5). `prune` checks every tree
before it removes anything. A tree that holds a run that is not plaid's
run of that tree means the data dir is a wrong one, such as the data
root itself, and then nothing is removed.

## 4. The `login` verb

`login --item NAME` does one of three things.

**A new Item.** Nothing is stored under the name.

1. `/link/token/create` returns a link token and the page's URL. The page
   lives as long as the wait (`--mfa-timeout`, one hour by default).
2. The link token is written to `plaid-link-<name>.json`. This happens
   before the URL is shown. From then on the page can make an Item
   without this process hearing of it, and the link token is the one
   handle that can ask Plaid what happened.
3. The URL is printed. The sign-in happens in a browser.
4. `/link/token/get` is polled every three seconds. Each new event of the
   page is logged by name, so a pasted log shows how far a sign-in got.
5. A public token ends the wait. It is exchanged, and the token file is
   written at once. Then the pending file is removed.
6. `/item/get` proves the stored token works.

**An Item that exists.** Link opens in update mode with the Item's
access token. The Item, its id and its token stay the same. The wait
ends when a visit runs to its end. `/item/get` then reports the Item's
state.

**A sign-in that was never settled.** A pending file exists.

- A session that made an Item is claimed, even when the page has expired.
- A page that is still valid is shown again and waited on.
- A page that has expired, with nothing to claim, is dropped. A new one
  starts.
- A page's expiry is the one the pending file records, else the
  `expiration` Plaid states for its link token. It decides only between
  showing the page again and dropping it. Without either, `login` stops
  at that point and changes no file.

Rules that keep an Item from being lost:

- **Every public token is claimed.** That holds for a session that later
  ended in an exit, and for a second Item made in the same visit. A second
  Item is stored under `<name>-2`.
- **An item id that is stored already is left alone.** An earlier run
  claimed it.
- **A token that cannot be written is revoked.** `/item/remove` is
  called, so no access exists without a record. This is the only use of
  that route.
- **Only Plaid's word that the link token is gone discards a pending
  sign-in.** That word is `INVALID_LINK_TOKEN`. Any other error, a
  passing fault or a refusal, stops `login` and keeps the file.

What ends a wait without an Item:

- the newest visit ended in an exit. Plaid's own message is shown;
- time ran out. The pending file stays, so the next run resumes.

A closed browser tab does not end its session at Plaid. It cannot be
told apart from a visit that is still under way, so it is waited on.

`login --check` asks `/institutions/get` for one institution, which
proves the app keys. It then asks `/item/get` for each Item of the
environment. It exits 0 when the keys are accepted, one Item or more is
stored, and every Item is free of errors.

## 5. The `download` verb

`download` reads every Item of one environment, or the Items `--item`
names. Each Item gets a new run under `<bronze-dir>/<item>/<UTC-ts>/`.
A token file that cannot be read is reported and fails the run, and the
other Items are still read. A run that names its Items reads only their
files. `login --check` treats the files the same way.

**Run names.** A run is named by its UTC start, to the second. Two Items
read by one `download` can start in the same second, so a run is
identified by its Item and its name together.

**A tree holds one Item.** Every `run.json` names its tree (`item`) and
carries the Item's `item_id` and `environment` from its first write. A
run is written into a tree only when every run there is a run of the
same Item. A tree that holds anything else is refused and left as it is.
That is a data dir pointed at the wrong place, or a name used again for
another Item.

**What a run reads, in order:**

1. `/item/get` → `item.json`. The Item's products and the times Plaid
   last updated it. An `error` on the Item fails the run. Plaid answers
   every read of such an Item with that error, and update mode
   (`login --item NAME`) clears it.
2. `/accounts/get` → `accounts.json`. Free. It fails the run too when it
   cannot be read: nothing else means anything without the accounts.
3. Each product the Item was linked with, and no other:
   `/investments/holdings/get`, `/investments/transactions/get`,
   `/transactions/get`, `/liabilities/get`. Asking for a product the
   Item lacks would add the product to the Item.

The two ledgers are read over the `--lookback` window and paged at 500
rows. Each page is saved as Plaid answered it. Balances, holdings and
liabilities are read whole.

**The ledger's history.** Plaid fetches a new ledger's history in two
steps (§2). Before the first page of the bank and card ledger, a run asks
`/transactions/sync` for one row and records its
`transactions_update_status`. Until that is `HISTORICAL_UPDATE_COMPLETE`
the ledger is `partial`: the rows are what Plaid holds, and older ones
may still be missing. The log names the command that reads the window
again. The status is asked first, so a history that Plaid completes
mid-read is never claimed for rows read before it.

**What `run.json` records.** A run starts as `in-progress` and ends as
`complete`, or as `failed` with a `reason` when the Item could not be
read. A run stopped by a write error or by Ctrl-C stays `in-progress`,
and so does a run whose files vanished while it was written, as when a
prune with no age guard removed it.
Each product has an entry with one status:

| Status | Meaning |
| --- | --- |
| `fetched` | read in full; its files are listed, with the row count |
| `partial` | the bank and card ledger, while Plaid holds part of its history |
| `not_linked` | the Item was not linked with the product; not asked |
| `absent` | Plaid says the Item has no account the product fits |
| `not_ready` | Plaid was still assembling it, five minutes on |
| `failed` | asked, and the read did not succeed |

A ledger's entry also records its window, `since` and `until`. The bank
and card ledger's entry records `history`, the status Plaid gave. A
product's files are written only once all its pages are in, so a product
that failed today never reads as "now empty". Only a `complete` run is
a load input.

**Waits.** A fault on Plaid's side or a rate limit is asked again twice,
after 10 s and 30 s. `PRODUCT_NOT_READY` is asked again every 20 s for
up to five minutes. A refused request is not asked again.

**Paging.** Plaid pages by offset, so a row that arrives or leaves
mid-read shifts the pages after it. A read starts again when a page
states another total, when a row comes back a second time, or when the
pages do not add up to the total. A ledger is read at most three times.
One change shows in none of these: a row that leaves a page already
read while another arrives in a page not read yet. The next run reads
the window again and makes up for it.

**Exit status.** 0 when every Item was read and each of its products is
`fetched`, `absent` or `not_linked`. 1 when an Item failed, or a product
is `failed`, `not_ready` or `partial`. 130 when the run was stopped.

**`--dry-run`** reads the Item and its accounts, both free, says what a
run would read, and writes nothing. It checks the Item's tree as a run
would.

**`--debug`** records each HTTP exchange in the run, under
`screenshots/http-trace.jsonl`: URL, status, time and size. The client
reports exchanges through a hook that never sees a request, so no key
or token can reach the trace.

## 6. The `load` verb

`load` builds one silver database per Item, `<data-dir>/<item>/<item>.db`.
So each Item can be a gold source of its own. The schema is
[migrations/0001_initial.sql](migrations/0001_initial.sql). Its comments
describe each table, and each column whose name does not say enough.

**Which runs.** A run is loaded once, oldest first, and only when its
`run.json` says `complete`. Each run loads in one transaction. A run
older than the newest one already loaded cannot be replayed on top of
the newer windows, so the Item's database is then rebuilt from all its
runs. `--force` does the same on request. A rebuild writes a fresh
database beside the old one, and puts it in place only once every run
is in. It keeps the newest run's start as gold's change number, so gold
takes it in on `wealthdb reload <source>`.

A complete run that lacks a file it lists, or holds a file not in the
shape `download` writes, stops its tree. So does a run whose run.json
cannot be read. An update keeps the runs before it loaded. A rebuild
leaves the silver as it was. It and the runs after it wait until it is
moved out of the tree.

**Which Item.** A tree that holds a run that is not plaid's means the
data dir is a wrong one, such as the data root itself. `load` then
loads nothing at all. Beyond that, `load` refuses, and leaves as it is:

- a tree whose runs name two Items;
- a database that holds the silver of another Item;
- a database that is not a plaid silver at all.

A tree with no run yet gets no database. One refused tree does not stop
the others, and the exit status is then 1.

**One instant per run.** Every snapshot a run stores carries the run's
start, `dump_runs.snapshot_at`. That covers the accounts with their
balances, the holdings and the liabilities. Gold reads a source's
current state from its single latest snapshot time. So every fact of
the newest run must carry that time. Each run restates every account,
and each run that read the holdings restates every holding, quiet or
not. Plaid's own update times are kept beside it, in `item_states`.

**Which products.** A product is loaded as far as its run read it:

| Status | What `load` does |
| --- | --- |
| `fetched` | stores the snapshot; a ledger replaces its window (below) |
| `partial` | adds and updates the bank and card ledger's rows; removes only stale pending rows |
| `not_linked`, `absent`, `not_ready`, `failed` | nothing; earlier rows stay |

Every product's entry is kept in `run_products`, with its window and the
history status Plaid gave. A reader tells "read, and empty" from "not
read" there.

**The ledgers.** For each account a fetched ledger covers, a run deletes
the rows dated within its window and inserts what Plaid listed. A row
Plaid no longer lists leaves silver with it. A pending charge that posts
under a new id is the common case. An account the Item no longer lists
keeps its rows: absence from one answer proves nothing about history. A
pending row is provisional, so every read of the bank and card ledger
replaces the covered accounts' pending rows wholesale, wherever they are
dated. A partial read does so too: the newest rows are the ones Plaid
holds first. Beyond that a partial ledger deletes nothing, since its
absent rows may only be missing so far.

**Values.** Money, quantities and prices are stored as decimal strings:
the shortest decimal that reads back as the number Plaid sent. That is
Plaid's own figure, up to 15 significant digits, without trailing zeros.
Dates are 00:00 UTC of the day Plaid states.
Both ledgers are negated into the fleet sign: positive is money into the
account. Balances keep Plaid's sign: a card or a loan states what is
owed, positive. `payload` keeps each object as Plaid sent it.

## 7. Observed

### Sandbox, Hosted Link (2026-10-01)

Measured against `sandbox.plaid.com` with a scripted browser and Plaid's
test institution.

- **The page lifetime is honoured.** The token's `expiration` equals its
  creation time plus `url_lifetime_seconds`.
- **One session per visit.** Each opening of the URL adds a session to
  the same link token. Plaid lists them in no fixed order. `started_at`
  has nanosecond digits and sorts as text.
- **A closed tab stays open at Plaid.** Its session never gets a
  `finished_at`.
- **A confirmed exit** sets `finished_at` and an `exit` object. The
  object holds `error` (null for a plain exit) and `metadata`.
- **The Item exists before the page ends.** `results.item_add_results`
  appears when the account selection is confirmed. The page then shows
  two more screens. The session has no `finished_at` yet.
- **Cancelling after that keeps the Item.** The session ends with an
  `exit`, and its public token still exchanges for a working Item with
  all three products.
- **A completed update** sets `finished_at` with no `exit`, and ends with
  a `HANDOFF` event. It also reports a public token. That token exchanges
  to the Item that exists already.
- **Update mode asked for no password** on a healthy Item at a
  non-OAuth institution. It showed the account selection.
- **Events** are listed newest first, stamped to the second. Names seen:
  `OPEN`, `TRANSITION_VIEW` (with a `view_name`), `SKIP_SUBMIT_PHONE`,
  `SEARCH_INSTITUTION`, `SELECT_BRAND`, `SELECT_INSTITUTION`,
  `SUBMIT_CREDENTIALS`, `HANDOFF`, `EXIT`.
- **The screens of a new link**, in order: consent, institution list,
  the institution's sign-in, account selection, a notice that the
  application is being tested ("Share data" or "Cancel"), an offer to
  save the login with Plaid, success.
- **The consent text names "contact details"** among the data shared,
  though no identity product is requested.
- **A removed Item** answers `ITEM_NOT_FOUND` afterwards.
- **`optional_products` all arrived.** The test institution supports all
  three data products, and the Item carried all three.

### Sandbox, data reads (2026-10-02)

Read from the `user_good` test Item at First Platypus Bank.

- **Windows.** A start thirty years back is accepted and returns all
  Plaid holds. A start equal to the end is accepted and returns that
  day's rows. An end date in the future is accepted. An end before the
  start is refused with `INVALID_FIELD`. An offset past the end returns
  an empty page.
- **`/transactions/sync`**, asked for one row with no cursor, answers
  `transactions_update_status` (`HISTORICAL_UPDATE_COMPLETE` for the
  test Item), beside `added`, `modified`, `removed`, `has_more`,
  `next_cursor` and `accounts`.
- **Every page repeats** `accounts` and `item`. The investment pages also
  repeat `securities`.
- **`/item/get`** lists the products as `products` and `billed_products`,
  and a `last_successful_update` for `transactions` and `investments`.
- **Accounts** carry `type`, `subtype`, `mask`, `name`, `official_name`,
  and `balances` with `current`, `available`, `limit` and the currency.
  An investment account's `current` is its total value.
- **Holdings** carry quantity, price, value, cost basis, the price date,
  and `tax_lots`. A lot can be short, with a negative quantity.
- **Securities.** `isin`, `cusip` and `subtype` were null on every
  Sandbox security. `type` and `name` were always set. `ticker_symbol`
  was null on three: the cash security, the fixed-income security and
  one mutual fund. Cash is the security of type `cash`. Plaid's own
  documented example gives it the ticker `USD`, so a missing ticker does
  not mark cash. Bitcoin is a `cryptocurrency` with `is_cash_equivalent`
  true, so that flag does not mark cash either.
- **Investment transactions** follow the documented sign. A buy and a
  fee are positive. A sell, a contribution, a dividend and interest are
  negative. The rows that are not trades take three shapes:
  - a contribution or an account fee points at the cash security, with
    a quantity equal to its amount, sign included;
  - interest points at the cash security, with quantity 0 and price 0;
  - a dividend points at the security that paid it, with quantity 0 and
    price 0.
- **The ledger** carries `personal_finance_category` with
  `version: v2`, `original_description`, `counterparties` and
  `merchant_name`. Card spend is positive, a deposit negative.

## 8. Open questions

1. Does Plaid keep an account's id from one run to the next? A new id
   would read in gold as one account closed and another opened.
2. Does `/item/get` show a renewed Item as healthy at once after update
   mode?
3. How long after a link does `/transactions/sync` report
   `HISTORICAL_UPDATE_COMPLETE`?
