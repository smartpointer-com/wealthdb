# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[AGENTS.md](../../AGENTS.md). The plaid-specific surface below applies on
top of those shared rules.

`plaid` reads institutions through Plaid's REST API. It drives no browser
and scrapes no bank page. The sign-in happens on Plaid's own page, in the
user's own browser. One login at one institution is one Plaid **Item**,
with a local name and one token file.

## 0. Never run a real session unprompted

Root [AGENTS.md](../../AGENTS.md) §2 applies. Here a real session costs
more than an MFA prompt:

- `login --item NAME` without `--sandbox` signs in at a real institution
  and makes a Production Item. On a Trial plan an Item is one of ten **for
  the life of the Plaid account**. Removing an Item does not return its
  slot.
- Some institutions keep one Item per login. A second link there ends the
  first one.

Allowed without asking:

- reading code, docs and tests; `--help`; `make build-plaid` /
  `make test-plaid`;
- `login` and `download` with `--sandbox`, once `PLAID_SANDBOX_SECRET`
  is set. The Sandbox has test institutions and test data only, and its
  Items are free. A Sandbox download writes into a rehearsal data dir,
  never a deployment's;
- `login --check`. It asks Plaid two free questions and reaches no
  institution;
- `download --dry-run`. It reads each Item and its accounts from Plaid's
  copy, two free reads, and writes no run. No sign-in happens;
- `prune` with `--dry-run`. It reads local files only.

Not allowed unless explicitly asked: `login --item NAME` and `download`
without `--sandbox`. Hand the command to the user instead.

The data root of a deployment is not a scratch area. Every verb, dry
runs included, creates the empty data dir. Develop against a rehearsal
root: `WEALTHDB_DATA_ROOT` names the root, and `--data-dir` names
plaid's own dir in it (`<root>/plaid`), never the root itself. Never add
a Plaid source to a deployment's live config.

Re-linking is never a fix for an Item that Plaid still has. When an Item
stops answering, the remedy is `login --item NAME` on the **same name**,
which opens update mode on the same Item. Never suggest deleting a token
file and linking again.

Two answers are beyond update mode:

- `ITEM_NOT_FOUND`: Plaid no longer has the Item. The only way back is a
  new link under a new name, which uses one more Trial slot, so the
  choice is the user's. Moving the dead token file out of the secrets
  dir stops runs reporting the Item; that is also the user's step, never
  an agent's.
- `INVALID_ACCESS_TOKEN`: Plaid does not accept the stored token with
  these app keys. The keys of the Plaid team that linked the Item, or
  the token file's backup, fix it. A new link does not.

## 1. Read-only access — the whole surface

Two lists in [plaidapi.py](plaidapi.py) are the collector's whole surface
at Plaid. The client refuses anything outside them before it builds a
request.

**Routes** (`ENDPOINTS`, `SANDBOX_ENDPOINTS`):

- `/link/token/create`, `/link/token/get`: start a sign-in page and read
  its outcome.
- `/item/public_token/exchange`: turn a finished sign-in into the Item's
  access token.
- `/item/get`: the Item's state. Free.
- `/accounts/get`: accounts and the balances of Plaid's last update.
  Free.
- `/investments/holdings/get`, `/investments/transactions/get`,
  `/transactions/get`, `/liabilities/get`: the data. Each reads Plaid's
  copy and never reaches the institution.
- `/transactions/sync`: asked for one row, with no cursor, for one field:
  how much of the ledger's history Plaid holds. Only for an Item linked
  with transactions.
- `/institutions/get`, `/institutions/get_by_id`: institution metadata.
  Free; the first is the app-key probe.
- `/item/remove`: revoke an Item. Called from one place only: `login`,
  for an Item whose new token could not be written to disk. No verb and
  no flag reaches it.
- `/sandbox/public_token/create`: make a test Item. Sandbox host only.

**Products** (`DATA_PRODUCTS`): `transactions`, `investments`,
`liabilities`. A link requests these and nothing else. An Item can do
only what its link requested, so no Item of this collector can pay or
transfer.

`download` reads an Item only for the products in its `/item/get`
`products` list. Calling another product's route would add that product
to the Item, with its billing. The first `/investments/transactions/get`
on an Item starts Plaid's investment transactions subscription. A Trial
plan charges for neither. After an upgrade to a paid plan, Plaid bills
every subscription added during the Trial each month, until the Item is
removed.

Forbidden — do not call, wrap or add:

- any other product: `transfer`, `payment_initiation`, `auth`, `signal`,
  `identity`, `assets`, `income`, and the rest;
- any route under `/transfer`, `/payment_initiation`, `/processor`,
  `/bank_transfer` or `/signal`;
- `/accounts/balance/get`, `/transactions/refresh`,
  `/investments/refresh`: each forces a live pull at the institution and
  is billed per call on a paid plan;
- `/item/access_token/invalidate`, `/item/webhook/update`, or any other
  route that changes an Item;
- a webhook receiver or a redirect address. Hosted Link needs neither.

A new route or product is an edit to the lists in `plaidapi.py` **and**
to this file, and it needs the user's written opt-in first.

## 2. Protect the credentials

- **App keys.** `PLAID_CLIENT_ID`, `PLAID_SECRET` (Production) and
  `PLAID_SANDBOX_SECRET` come from `~/.secrets/plaid.env`, through the
  environment only. A secret never gets a flag. `--client-id` may, since
  the client id is an identifier.
- **Item tokens.** `~/.secrets/plaid-token-<name>.json`, mode 0600, one
  file per Item. Plaid shows an access token once. A lost token cannot
  be fetched again, and on a Trial plan its Item still counts. Never
  delete, move, rewrite or "tidy" a token file. That includes Sandbox
  files this session did not create.
- **Pending sign-ins.** `~/.secrets/plaid-link-<name>.json` holds a
  sign-in whose outcome is not stored yet. `login` removes it once the
  outcome is settled. Leave it alone otherwise.
- **Never print or log** a secret, an access token, a public token or a
  link token. An access token travels in the request body, so no request
  body is ever logged or written. Error text carries Plaid's code, its
  message and its request id only.
- A token file that exists but does not parse is an **error**, never
  "no such Item". Reading it as absent leads straight to a second link.

## Authentication & private data

See the repo-root [AGENTS.md](../../AGENTS.md) §3 (authentication) and
§4 (no private information in source). They apply in full here.

Plaid PII: account names, masks and numbers, balances, holdings,
transactions, liabilities, item ids, account ids — **and which
institutions a deployment has linked**, how many Items it has, and what
accounts each holds (root [AGENTS.md](../../AGENTS.md) §4). None of it
enters tracked files. Tests and docs use generic Item names (`bank`,
`broker`) and synthetic ids and tokens. Plaid's own Sandbox fixtures are
public and fine to name: its test institutions, the `user_good` login,
and their institution ids.
