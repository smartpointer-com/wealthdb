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
  ends the first. This is another reason a re-link is never the remedy.
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
- **Consent is per product.** An Item can use only the products its link
  named. A product added later needs update mode.
- **A public token lives thirty minutes.** A session's result stays
  readable for six hours. A sign-in settled more than thirty minutes
  after it made its Item leaves an Item nothing can claim.

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

Rules that keep an Item from being lost:

- **Every public token is claimed.** That holds for a session that later
  ended in an exit, and for a second Item made in the same visit. A second
  Item is stored under `<name>-2`.
- **An item id that is stored already is left alone.** An earlier run
  claimed it.
- **A token that cannot be written is revoked.** `/item/remove` is
  called, so no access exists without a record. This is the only use of
  that route.
- **A fault that can pass keeps everything.** A server error or a rate
  limit never discards a pending sign-in.

What ends a wait without an Item:

- the newest visit ended in an exit. Plaid's own message is shown;
- time ran out. The pending file stays, so the next run resumes.

A closed browser tab does not end its session at Plaid. It cannot be
told apart from a visit that is still under way, so it is waited on.

`login --check` asks `/institutions/get` for one institution, which
proves the app keys. It then asks `/item/get` for each Item of the
environment. It exits 0 when the keys are accepted, one Item or more is
stored, and every Item is free of errors.

## 5. Observed

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

## 6. Open questions

1. Does `/item/get` show a renewed Item as healthy at once after update
   mode?
