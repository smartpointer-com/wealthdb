# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[AGENTS.md](../../AGENTS.md). The ubs-web-specific surface below
applies on top of those shared rules.

## 1. Read-only UBS netbanking access — never trigger write actions

Root [AGENTS.md](../../AGENTS.md) §1 mandates read-only access. The
concrete surface for ubs-web:

Allowed UI surfaces — `download.py` may only navigate to or click
within, and the `explore` session may only be driven to (landmarks.py
pins the concrete URLs and selectors; the allow-list pattern below is
binding):

- The UBS login form and the MFA approval page that follows it.
- Read-only listing pages for: account overview, transactions,
  custody/portfolio holdings, eDocuments archive.
- The **portfolio securities-transaction list**, its filter panel and
  its CSV export button, plus the **portfolio switcher** in the SPA
  header used to move between portfolios. This surface is *driven*,
  not merely requested: the portfolio is chosen from the switcher, the
  period set by filling the panel's date fields and submitting it, and
  the file taken by clicking the list's export control (DESIGN.md
  §3.7b). These are the read-only navigation, filter-Apply and
  export-generation actions the list allows — they change what is
  shown and nothing else. Note that the surface remembers the
  submitted period per portfolio, so a walk leaves it on the window it
  asked for.
- The card area's **read** surfaces: the card roster, a card's
  transaction list and its exports, a single transaction's expanded
  detail, and the invoice archive with its statement downloads.
  Equivalently, the read endpoints behind them:
  `credit-card-accounts`, `credit-card-transactions`,
  `credit-card-invoices` and their `extract` links (DESIGN.md §5.1).
- The cash accounts' **interest conditions** (DESIGN.md §3.11), read
  from what the browser renders. The pass requests nothing itself.
  - Each homepage account's "Account information" route is navigated.
    The SPA's own response to its call
    `/api/v3/cash-accounts/<accountId>?include=INTEREST_RATES` is kept.
  - The legacy asset view. The SPA's portfolio overview is navigated,
    and its "Structure" menu and the "Account balances" item in it
    are clicked. That item's frame shows the entry `ebidispatcher.do`,
    which frames the contract list `getvaluatedcontractlist.do`.
  - For an account the API did not cover, the entry's frame is
    navigated to the row's "Account details" link `link.do`, as the
    item itself opens it. The browser follows its redirect to the
    dispatcher, which frames the `getpositiondetail.do` account page.
    "Account balances" is opened again before the next account.
    No token is built. Of a row's menu, only the "Account details"
    item is read: its payment and transfer items are the forbidden
    surface below.
  - While the pass runs, a route guard on the page aborts every
    request under `/assetview/` or `/api/v3/cash-accounts` that is not
    a GET, and every GET to a legacy page or an account outside the
    routes above. The SPA's and the legacy app's own requests are
    held to it too. A route handler sees only a redirect's first URL,
    so the page a redirect lands on is checked against the same
    allow-list once it has loaded.
- Date-range / period filter inputs and "Apply" / "Search" buttons
  on the above pages.
- Export / download buttons that produce CSV, XLS, or PDF copies
  of already-displayed data.
- Document download endpoints reachable via the session cookie
  (typically REST endpoints fetched via Playwright's request API
  rather than per-row link clicks).
- Logout (optional; not required between runs, but harmless).

Forbidden — do not navigate to, click, or scrape:

- Payment / transfer entry (`Payments`, `New Transfer`, `eBill`,
  IBAN entry, beneficiary management).
- Trade entry / order forms (`Trade`, `Buy/Sell`, `New Order`).
- **Card management, permanently and in every form** — block,
  unblock, replace, reissue, cancel, activate, request a card,
  change a limit, set or view a PIN. The card area hosts these
  beside the read surfaces above, so the allow-list is on the
  sub-surface, never on the area. The roster's own
  `register` / `unregister` / `reassign` / `orders` links and the
  `paymentToCard` link (which pays a card bill) are part of this: a
  link the API advertises is not thereby permitted.
- Settings pages that mutate account state (notification prefs,
  trading limits, MFA factor management, contact details).
- Any "confirm" or "submit" button outside the login form itself.
- Anything that performs a `POST` other than the login form, the
  read-only filter Apply actions, and explicit export-generation
  triggers.

### How each tool is held to that surface

The two tools are bound by different mechanisms, and the difference is
the point:

- **`download.py` navigates**, so its surface is fixed in code: it
  visits only the routes [landmarks.py](landmarks.py) names, and a new
  surface is reached by adding a landmark deliberately — never by
  following a route the SPA happened to offer. Its card pass reaches
  the API rather than the UI, and is held the same way:
  [cards.py](cards.py)'s `refuse_path` is an allow-list of read
  endpoints that every request passes through, the ledger's paging
  cursor included. The rate pass reads only what the browser renders,
  and is held by [rates.py](rates.py)'s route guard: while it runs, a
  write to the legacy app or the account API is aborted, and so is a
  read of either that `refuse_request`'s allow-list refuses. The links
  the pass itself opens pass the same allow-list first.
- **[`explore.py`](explore.py) does neither.** It opens the login entry
  point once and then only *records*; it issues no navigation and no
  click for the rest of the session. A recorder cannot stray onto a
  payment form or a card-management control, because it never interacts
  at all. The one thing it types is the contract number into the login
  form, and it never submits it.

So during an `explore` session the forbid list above binds **whoever is
driving the browser**. The harness will faithfully record a forbidden
surface if one is opened — recording is not permission, and the list is
what says where the session may go.

## 2. Sessions, challenges and the dry run

Root [AGENTS.md](../../AGENTS.md) §2 governs real sessions. For ubs-web:

- A full `login` raises a UBS Access App QR challenge.
- `login --check` and `download --dry-run` reuse the stored state
  file and never sign in. On a dead session they exit with an error
  and raise no challenge. Both are in the run-without-asking set.
- `explore` drives a live session and runs only when asked.
- The contract number comes from `UBS_CONTRACT_NUMBER`, sourced from
  `ubs-web.env`, or from the bank-level `ubs.env` when `ubs-web.env`
  does not exist.

## Authentication & private data

See the repo-root [AGENTS.md](../../AGENTS.md) §3 (authentication) and
§4 (no private information in source). They apply in full here.
