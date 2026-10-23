# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[AGENTS.md](../../AGENTS.md). The refdata-specific surface below applies
on top of those shared rules.

`refdata` holds **no account data**. It reads public reference data:
fund compositions and facts about securities. No provider signs in to
an account; `finra` trades its API credential for a token.

It holds **one credential**: the user's FINRA API client, for the
`finra` provider. It is read only from the environment the wrapper
sources (`<secrets-dir>/finra.env`). The provider trades it for a token
with one POST to FINRA's token endpoint and keeps the token in memory
for the run. Neither is ever written anywhere, and no request goes to
FINRA without them ([README.md, finra](README.md#finra)).

## 1. Public reads only

- GET requests to the pages and files each provider documents in
  [README.md](README.md). A provider may add a read-only POST only where
  the source's lookup has no GET form.
- A provider may also accept a disclaimer a public site requires before
  it serves its documents: an investor category, no account. The
  session cookie the site sets lasts one run and is never stored.
- `finra` sends one more POST: its API credential to FINRA's token
  endpoint, which answers a bearer token for the run. The token is kept
  in memory only.
- No account login, no form that changes anything at the source.
- Every request goes through `fetcher.Client` and keeps its rules
  ([README.md, Pace](README.md#pace)). No provider opens a connection
  of its own.
- A run fetches only what the wanted list names. Never crawl a product
  range or a site.

## 2. Live runs

A download signs in to no account and fires no challenge, so a live
`download`, `download --dry-run` and `login --check` may run in
development. A dry run still makes the lookups that find each fund, and
reads JPX's index page.
Keep each run small (`--ref`, `--limit`).

A live run with `finra` enabled spends the user's FINRA credential and
its monthly budget. In development keep such a run small: `--provider
finra --ref otc-daily-list` makes only FINRA's request. That request
asks for every receipt whose calendar is due; a `--wanted FILE` that
names fewer receipts narrows it. A `--ref` that names a security plans
no FINRA request. `login --check` takes only a token from FINRA and
asks for no data.

## 3. Providers and their terms

A new provider quotes its source's terms in the README, and only
there. Its module points to that section (`Terms: README.md,
<section>.`). Public-data providers may be on by default. An issuer,
report, archive or vendor provider stays off until `refdata.cfg` names
it: enabling one accepts its terms, and that is the user's decision,
not the code's.

## 4. What stays out of the repo

- The SEC contact address. It lives in `refdata.cfg`, outside the repo.
- Anything a provider returned. A composition is public, but which
  funds a real wanted list names is the household's data (root
  [AGENTS.md](../../AGENTS.md) §4). Test fixtures are invented files
  in the real layout.
- The wanted list of a real gold.
