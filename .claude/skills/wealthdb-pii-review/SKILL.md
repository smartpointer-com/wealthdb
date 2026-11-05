---
name: wealthdb-pii-review
description: Privacy review of a wealthdb branch before it leaves the machine — the mechanical check, a watchlist built from the local data, and a full read of every added line and commit message for real identifiers, amounts, account rosters, financial narrative, names and deployment details. Use it before every push of a branch, before opening or updating a pull request, after any commit written with real data in view, and whenever someone asks whether a change is safe to share, even when they do not name the skill.
---

# wealthdb privacy review

wealthdb is built against real accounts. Its collectors sign in to real
logins, so code, fixtures, docs and commit messages get written with
real data in view. That is how private facts reach a repository: an
account number in a fixture, a balance in a commit message, a sentence
about what a login holds. A pushed commit stays public. A later rewrite
does not remove what a fork or a pull request already showed.

So the review runs before the first push, over every commit the branch
adds. The rule it enforces is section 4 of `AGENTS.md`. Read it first.

## Hard rules

1. Read and report. Change nothing until the user agrees to each fix.
   Never push.
2. Never copy a real value anywhere: not into the repository, a commit
   message, a pull request, an issue, or a brief for another agent. Name
   a finding by its location and its shape (`####-####-##`).
3. Never read `~/.secrets` or raw downloads. Read the local data only
   through `shared/pii/watchlist.py`.
4. Never bypass a hook (`--no-verify`). Never add a value to
   `shared/pii/allow.txt` to get past a finding.

## 1. Scope

The review covers every commit the branch adds to the main repository's
`main`:

```sh
git fetch upstream            # the remote of the main repository
BASE=$(git merge-base HEAD upstream/main)
git log --oneline "$BASE"..HEAD
```

In scope: every added line, every new or renamed file name, every
commit message, and the branch name. Commit work in progress first. The
review reads commits.

## 2. The watchlist

Skip this step on a machine without real data, and say so in the report.

A real account number in a fixture looks like a synthetic one. Only the
local data tells them apart. Build the watchlist from it:

```sh
python3 shared/pii/watchlist.py
```

It reads the ids and names of every account and portfolio the engine
reports. It writes them to `~/.config/wealthdb-pii/watchlist.txt`
(mode 0600) and prints only counts. Run it again after new accounts
appear.

Keep `~/.config/wealthdb-pii/curated.txt` by hand, one term per line.
Add what no account name carries:

- the people in the household, and anyone else the data names:
  beneficiaries, advisors, co-investors, landlords, employers;
- legal entities: trusts, companies, foundations, partnerships, and
  funds or vehicles held privately;
- property and places: street names, property nicknames, small towns;
- private holdings: startups, private funds, wallet labels, a single
  stock that would identify its owner;
- contact data and logins: e-mail addresses, phone numbers, user names.

Both files stay outside the repository. The hooks read them on every
commit.

## 3. The mechanical check

```sh
make pii PII_BASE="$BASE"             # the commits since BASE, and the index
make pii PII_BASE="$BASE" SHOW=1      # the same with the values: this terminal only
```

Treat each finding as a leak until it is shown otherwise. The check knows
shapes and checksums:

- IBANs, and card, AHV and social security numbers;
- credentials, home paths, host names, e-mail addresses, phone numbers;
- in docs and messages: account numbers and masked last-four digits;
- in messages: money amounts;
- every watchlist term.

It cannot read a sentence. A clean run proves little; step 4 is the
review. `make hooks` installs the same check for every commit.

## 4. Read every added line

Read the whole diff and every message, oldest commit first, in chunks of
at most about 1,500 lines:

```sh
git log -p --reverse --format='commit %h%n%B' "$BASE"..HEAD
```

Ask the questions below of every added line. A yes is a finding. So is a
doubt.

**A. Identifiers.** Account, card, contract, customer, portfolio, depot,
policy, mandate, relationship and document numbers, in full or in part
(the last four digits). IBANs, tax and social security numbers, birth
dates, postal addresses, phone numbers, e-mail addresses, login names,
device ids, wallet addresses, aggregator item and account ids.

- They hide in fixtures, in captured URLs and query strings, in log
  samples pasted into docs, in file names, and in commit messages ("fix
  the account ending ####").
- Obviously fake values pass: `0000…`, `1234567890`, `XXXX`,
  `<account-id>`, IBAN placeholders.

**B. Credentials.** Passwords, API keys, client ids and secrets, OAuth
and refresh tokens, cookies, session ids, MFA codes, TOTP seeds, private
keys, `.env` contents, URLs that carry a user and password. An expired or
rotated one still counts. A credential that was ever pushed is burned:
rotate it at once.

**C. Data a source returned.** Balances, positions, quantities, prices,
cost basis, fees, interest, dividends, transactions, allocations and
weights, returns, valuations, record counts, statement periods, document
lists. In any form: fixture rows, expected outputs, SQL seed rows,
example output in docs, inline test strings, commit messages. Apply the
provenance test: could someone have typed these values without a real
statement in view?

- Passes: round numbers, repeated placeholders, sequential ids, dates
  like `2024-01-01`, names like "Example Fund", values that a generator
  in the repository builds from fictional seed data.
- Fails: odd cents, realistic merchants, prices that match market
  history, quantities and totals that reconcile, runs of real-looking
  dates.
- Fails: a real fixture with only its account number changed. Its
  amounts, dates and descriptions are the financial history.
- Fails: a report's real output pasted into a doc, even with the
  identifier columns masked. The figures are the data.

**D. The account roster.** Any statement of what a real login holds:
which accounts, products, mandates, cards, loans or portfolios, how many,
their types or currencies, the absence of one, which one is the main
one, how many contact points are on file. This counts in aggregate, as
scope rationale, and in a test name (`test_the_<n>_savings_accounts`).
A capability or a condition passes. An observation fails.

- Fails: "The login holds <n> savings accounts and no card."
- Passes: "Deposit accounts (checking and savings) are in scope. Cards
  are out of scope."
- Fails: "Only one of the <n> portfolios has ever traded."
- Passes: "Trades are keyed by portfolio id. A portfolio without trades
  yields no rows."

The grammar is the tell. A sentence that says what the data is or was
("the roster lists…", "held paper only in <year>", "is always open")
describes a real login. Write what the source can express instead ("an
account need not hold securities"). Rationale follows from the schema,
never from the data just queried.

**E. Narrative about someone's finances.** What a person holds, bought,
sold, moved, closed, inherited or earns, and when. How one account's
money relates to another's. Tax status, residency, employment, pensions,
trusts, property, loans. It is private even when no number appears. A
commit message that quotes a figure, a count, a period or a
before-and-after belongs here too. Apply the stranger test: would the
sentence be equally true for a stranger who installs the software? If
not, it is about the author.

- Fails: "After the move abroad the old brokerage account went dormant."
- Passes: "An account with no activity in a period is marked dormant."
- Fails, as a commit message: "recover the <n> transactions missing
  from <month>."
- Passes: "parse multi-line transaction descriptions."

Words that often start such a sentence: "my", "our", "I", "we", "the
owner", "the user's", "real", "actual", "live", "observed", "in
practice", "it turns out", "so far", "currently", "as of", "never",
"only", "both", "all three", "since 20xx", "closed", "moved",
"transferred", "coverage", "backfill", "missing", "gap", "hole",
"residue", "reconcile", "pinned", "declared", "override".

**F. Other people.** Names, initials, roles and contact data of family,
partners, children, advisors, bankers, lawyers, trustees, beneficiaries,
co-investors, landlords, tenants, employers and colleagues. Words that
reveal a household ("the kids' accounts", "joint with my spouse").
Merchants that place a person: a named school, doctor or local shop.
Test: does the text say that a specific other person exists, or who
they are?

**G. Entities, property and holdings.** Names of trusts, companies,
foundations and funds that a person owns or controls. Addresses and
nicknames of property. Pension and insurance providers, custodians,
trust companies and administrators that have no collector. Private
companies and funds, SPVs, niche securities (a single stock, a municipal
bond by CUSIP), wallet labels. Apply the institution test: every
institution named is a collector's source or an obvious generic example.
Any other discloses a relationship. An institution reached through an
aggregator collector counts as a disclosure, although the aggregator
itself has a collector.

**H. Deployment details.** Absolute personal paths (home directories,
mounted volumes), user names, host names, LAN addresses, backup targets,
the real data root, real schedules, ports tied to one machine, links to
personal documents, sheets or dashboards, SSH aliases, editor configs
that name a machine. `$WEALTHDB_DATA_ROOT`, `~/.config/…`, container
paths, `localhost` and synthetic test paths (`/Users/alice/…`) pass.

**I. Dates and constants from the data.** A hard-coded inception or
closing date, a cut-over date, a date range chosen because that is when
the statements start, a threshold justified by "what the data shows", a
mapping table keyed by real account names. A documented config key with
a placeholder passes.

**J. Incidental.** Anything else a stranger should not learn: health or
family status, travel, residence beyond what the collectors imply,
"validated live on <date>" notes, a personal time zone as a default,
debug notes about a real run, TODOs that name people.

Do not report:

- the institutions the collectors name, their public URLs, endpoints,
  form fields, UI labels, error texts and document types;
- behaviour any customer of a provider sees: session lifetime, MFA
  factor types, the bot-protection vendor, export formats, pagination;
- account kinds as taxonomy (checking, savings, brokerage, IRA, 3a);
- widely held example tickers and indices, currency and country codes,
  IBAN placeholders such as `CH<chk><BBBB><RRRR>…`;
- the demo household in `demo/`, lockfile and content hashes, commit
  SHAs, migration numbers, software-default ports, container paths, XDG
  defaults, public host-key fingerprints;
- the author's git identity and the license's copyright line.

Hard cases:

- A sentence can be true, technical and still a leak. "The savings
  account was opened in <year> and has never seen a withdrawal" is about
  a customer, not about software.
- Synthetic ids beside real amounts: check every field, not the row.
  When one field of a fixture is flagged, re-check all the others.
- A real date alone fingerprints a real movement. Put a reproduction in
  an impossible year, such as 2098.
- "Out of scope" passes as a capability ("cards are out of scope") and
  fails as an observation ("the login has no card").
- A provider's documented behaviour is public. The author's span of
  history, accounts and run dates are not.

## 5. Fixtures

- Invent a fixture. Never adapt a real one: a renamed real line keeps
  its amounts, dates and descriptions.
- Rebuild a captured response from its shape, with made-up values
  throughout.
- A test that needs a valid checksum computes its value at run time, as
  `shared/pii/tests/test_pii_check.py` does. `allow.txt` holds published
  test values only.

## 6. Fix before the push

- Rewrite the commit that holds the leak. A later commit that deletes
  it leaves it in the history. With git 2.44 or newer:
  `git commit --fixup=<sha>` (or `--fixup=reword:<sha>` for a message),
  then `git rebase --autosquash "$BASE"`.
- Never write a commit message that names what it removes.
- Run steps 3 and 4 again over the rewritten branch.
- Already pushed: rewrite the branch and force-push it. Then tell the
  maintainers privately, through the repository's Security tab ("Report
  a vulnerability"), without naming the value. A pull request keeps its
  old commits reachable, and only the repository's owners and GitHub
  Support can remove them. Rotate any credential at once.

## 7. Report

For each finding, give the user:

- the commit, the file and the line;
- the group (A to J) and the value's shape, never the value;
- the fix.

Say which steps ran and which did not. "Clean" with a step skipped is
not clean.
