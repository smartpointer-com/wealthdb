# plaid

## ⚠️ Security & liability disclaimer

> [!WARNING]
> **This collector holds credentials that read your financial accounts
> through a third party, Plaid. Read this disclaimer in full before
> configuring any credential.**

This collector holds the keys of a Plaid account and one **access token
per linked institution**. Together they return the account data of every
linked login — balances, holdings, transactions and liabilities — to
whoever holds them. The links this collector makes request data products
only, so its tokens cannot initiate a payment or a transfer; nothing but
this codebase's own discipline keeps it to those products. If malicious
code were ever introduced into this repository, its dependency chain, or
the environment it runs in, it could read and disclose that data in
full. Linking an institution also **shares your account data with Plaid
Inc.**, under Plaid's own terms and privacy policy.

**You are solely responsible for a thorough, independent security audit**
of this code, its dependency chain, and its runtime **before** entrusting
it with credentials, and again after every update. If you cannot perform
such an audit, do not hand this software real credentials. Access through
an aggregator may additionally breach an institution's terms of service;
verifying that your use is permitted, by Plaid and by each institution,
is likewise your responsibility.

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
affiliated with, endorsed by, or sponsored by Plaid Inc. or any financial
institution; nothing in this repository is financial, legal, or tax
advice.

A read-only collector for the institutions that
[Plaid](https://plaid.com) reaches. Plaid is a data aggregator. One Plaid
account can read many banks, brokers and card issuers.

Part of the **wealthdb** suite. See
[the architecture overview](../../DESIGN.md) for the bronze → silver →
gold model. See [collectors/README.md](../README.md) for the conventions
all collectors share.

## What it does

- **`link --item NAME`** links one institution. It prints the URL of a
  sign-in page that Plaid hosts. The sign-in at the institution happens
  on that page, in an ordinary browser. The collector waits, then stores
  the access token.
- **`link --item NAME`** on a name that is linked already renews that
  link. No second link is made.
- **`login`** settles the sign-ins that a stopped `link` left open. It
  never opens a sign-in page. With none left open, it does nothing.
- **`login --check`** reports whether Plaid accepts the keys, the state
  of every linked institution, and the sign-ins left open. It changes
  nothing.
- **`download`** reads what Plaid holds for every linked institution:
  accounts and balances, holdings, the bank and card ledger, investment
  transactions, and loan and card terms. It saves Plaid's answers as
  they are.
- **`load`** turns the runs of each linked institution into its own
  silver database: accounts and balances, holdings, both ledgers, and
  card and loan terms.
- **`prune`** deletes runs that did not complete, and the traces of
  `download --debug`.

One login at one institution is one Plaid **Item**. `NAME` is a local
short name for it, such as `bank` or `broker`.

## Examples

```sh
./plaid login --check                                # keys and links
./plaid link --item bank                             # link a bank or a card
./plaid link --item broker --require investments     # link a broker
./plaid link --item bank                             # later: renew the link
./plaid login                                        # settle a stopped link
./plaid download --lookback all                      # first run: all history
./plaid download                                     # later: the last ~90 days
./plaid download --item broker --dry-run             # what a run would read
./plaid load                                         # every Item into its silver
./plaid load --item broker                           # one Item
./plaid prune --dry-run                              # what prune would delete
```

`download` reads the bank and card ledger and the investment transactions
from the `--lookback` date to today. Plaid holds about two years of each
from the day of the link, and keeps every row it sees after that.
Balances, holdings and loan terms are read whole on every run.

A login that holds only brokerage accounts needs `--require investments`.

The Sandbox uses Plaid's test institutions and test data. It is free and
needs no real login. Its runs belong in a data dir of their own, never in
a deployment's:

```sh
./plaid link --item test-bank --sandbox              # sign in as user_good / pass_good
./plaid link --item test-bank --sandbox-institution ins_109508    # no browser
./plaid login --check --sandbox
./plaid download --sandbox --lookback 2y --data-dir ~/plaid-sandbox
```

`./plaid <verb> --help` lists the flags of a verb.

## Set up a Plaid account

Plaid has no shared application for tools like this one. Every
deployment uses its own Plaid account and its own keys. The account is
free.

### 1. Sign up

1. Open <https://dashboard.plaid.com/signup> and create an account.
2. Verify the email address through the link Plaid sends.

Plaid accepts individuals. A company is not required.

### 2. Start the Trial plan

The Trial plan is free and returns real data. Plaid offers it to
accounts in the US and Canada.

1. Open <https://dashboard.plaid.com/trial-plan>. The Dashboard home
   page has a button that leads to the same form.
2. Fill in the form. It is short. It asks for a name, an address and a
   use case. "Personal Finances" fits this collector.

The Trial plan covers Transactions, Investments and Liabilities. These
are the three products the collector reads.

### 3. Store the keys

1. Open <https://dashboard.plaid.com/developers/keys>. The page lists the
   client ID and two secrets, one per environment.
2. Copy the client ID, the Production secret and the Sandbox secret.
3. Write them to `~/.secrets/plaid.env`:

   ```sh
   export PLAID_CLIENT_ID='<client id>'
   export PLAID_SECRET='<production secret>'
   export PLAID_SANDBOX_SECRET='<sandbox secret>'
   ```

4. Restrict the file with `chmod 600 ~/.secrets/plaid.env`.

Keep the single quotes. The file is read as a shell script.

The Production secret reads real institutions. The Sandbox secret reads
only Plaid's test institutions.

### 4. Check the keys

```sh
./plaid login --check
```

It prints `production: Plaid accepts the app keys` when the Trial plan is
active. It then reports that nothing is linked yet.

### Limits to know before the first sign-in

- **Ten Items.** An Item is one login at one institution. The Trial
  plan allows ten.
- **The count never goes down.** Removing an Item does not free its
  slot. Link each institution once, and renew it under the same name.
  [When a link stops working](#when-a-link-stops-working) has the one
  exception.
- **Sandbox is free.** Items made with `--sandbox` do not count.
- **Some institutions take days.** Plaid enables some OAuth institutions
  on its own. This can take several business days.
- **Some institutions keep one link per login.** A second link there
  ends the first.

### What Plaid charges

The Trial plan charges nothing. A paid plan charges per Item, by product:

- **One subscription per product.** Every link asks for all three data
  products. The product that `--require` names is always added. The
  other two are added where a shared account supports them. `link`
  shows the products of each new link.
- **Investment transactions are one more subscription.** It starts with
  the first `download` of an Item that has the `investments` product.
- **Whole months, read or not.** Plaid bills each subscription for every
  calendar month the Item exists, in full. That holds while nothing
  reads the Item, and while it waits for a new sign-in.
- **The collector's calls are not billed one by one.** Each is free, or
  covered by the Item's subscriptions. `login --check` and
  `download --dry-run` cost nothing.
- **Trial subscriptions carry over.** An Item keeps its Trial
  subscriptions, and Plaid charges for them from the upgrade on. The
  upgrade request must name all three products. After an upgrade, Plaid
  keeps Production access only to the products the request named.
- **Only removing an Item ends its bills.** A subscription cannot be
  taken off an Item, and no `plaid` verb removes one. Removing the Item
  at <https://my.plaid.com>, or through Plaid support, ends its
  subscriptions.

### What the Dashboard shows

The Dashboard has no switch for a single institution. You pick the
institution on the sign-in page.

- **Institutions → OAuth institutions** reads "Automatic bank access.
  No action is needed to access banks through Plaid's free trial."
- An institution's own page can read "Not available". That page shows
  connection health. It is not an access setting.

## The sign-in page

`link` prints a URL and waits. The page stays valid for one hour. In
the Sandbox it shows these screens:

1. Plaid's consent screen.
2. The list of institutions, with a search box.
3. The institution's own sign-in.
4. The accounts to share.
5. A notice that the application is being tested. Choose "Share data".
6. An offer to save the login with Plaid. It can be skipped.

The collector stores the link as soon as the accounts are confirmed. The
terminal then shows the institution, its products and the date its
consent expires.

The wait can stop without a link: Ctrl-C, an exit on the page, or the
hour running out. `link` then asks Plaid once more how the sign-in went:

- an Item that the page made is stored;
- a page that has closed leaves nothing behind;
- a page that is still open can still make an Item. Plaid cannot close
  a page early, so the sign-in's record stays until the page closes.

`login` settles such a record. It stores an Item that the page made,
and removes the record once the page has closed. An orchestrator that
runs `login`, `download` and `load` in turn settles it on its next run.
`link` on the same name shows the same page again while it is open.

Plaid states the purpose of the data on its consent screen. When no
purpose is set for the account, `link` prints Plaid's error. A purpose
can be selected at <https://dashboard.plaid.com/link/data-transparency-v5>.

## Where things are stored

```
~/.secrets/
├── plaid.env                  the keys
├── plaid-token-<name>.json    one linked institution: its access token
├── plaid-link-<name>.json     a sign-in that is still open
└── plaid-link-<name>.lock     present while a run works on that sign-in

$XDG_DATA_HOME/wealthdb/plaid/
└── <name>/                    one tree per Item: one linked login
    ├── <name>.db              its silver database, written by load
    └── 20261001T120000Z/      one run of download, named by its UTC time
        ├── run.json           what the run read, product by product
        ├── item.json          the link: its products and update times
        ├── accounts.json      accounts and balances
        ├── holdings.json      investment holdings and securities
        ├── liabilities.json   card, mortgage and student-loan terms
        ├── transactions-0001.json              the ledger, page by page
        └── investment_transactions-0001.json   investment activity, page by page
```

Back up the token files. Plaid shows an access token once. A lost token
cannot be fetched again, and its Item still counts against the ten.

`--data-dir` names plaid's own dir, as shown above, never the data root
itself.

A run reads only the products the institution was linked with. A product
Plaid has no account for is noted in `run.json` and skipped.

Plaid fetches the history of a new link in steps, over minutes or hours.
A run made before it is done notes what was missing and exits with
status 1. Its log names the command that reads the same window again,
such as `./plaid download --item bank --lookback all`.

## Gold

Each linked login's silver database is a gold source of its own, of
kind `plaid`. Add one entry per Item to the gold config:

```json
{"id": "broker", "kind": "plaid", "path": "<data-dir>/broker/broker.db"}
```

Reports then name the source `broker`. The
[adapter](../../wealthdb/docs/adapters/plaid.md) says how each kind of
account appears in gold. Loans that Plaid reports and that are not on a
home stay in silver.

An account's tax wrapper follows the subtype Plaid reports. Where that
gives the wrong wrapper, `account_overrides` in the gold config sets it.
The `account_id` column of `wealthdb holdings accounts -C
account,account_id,tax_wrapper` shows each account's id:

```json
"account_overrides": {
    "broker": {
        "<account_id>": {"tax_wrapper": "<wrapper>"}
    }
}
```

The same entry takes `"exclude": true` instead, to leave an account out
of gold.

Add only an institution that no other collector reads into the same
gold. The [adapter](../../wealthdb/docs/adapters/plaid.md) says why.

## When a link stops working

`login --check` and `download` show what Plaid reports, and what to do:

- **A new sign-in is needed.** `link --item NAME` renews the link in
  place. It makes no new Item.
- **Plaid no longer has the Item** (`ITEM_NOT_FOUND`). It was removed at
  Plaid, for example at <https://my.plaid.com>. Nothing renews it. A new
  link under a new name makes a new Item, which counts against the ten.
  Every run reports the old Item until its token file leaves the
  secrets dir.
- **Plaid refuses the token** (`INVALID_ACCESS_TOKEN`). The app keys
  belong to another Plaid team, or the token file has changed. Use the
  keys that linked the Item, or restore the file from its backup. A new
  link does not help.

## Further reading

The design is in [DESIGN.md](DESIGN.md). The rules for coding agents are
in [AGENTS.md](AGENTS.md).
