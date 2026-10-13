# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[AGENTS.md](../../AGENTS.md). The refdata-specific surface below applies
on top of those shared rules.

`refdata` holds **no account data and no credential**. It reads public
reference data: fund compositions and facts about securities. It never
signs in anywhere.

## 1. Public reads only

- GET requests to the pages and files each provider documents in
  [README.md](README.md). A provider may add a read-only POST only where
  the source's lookup has no GET form.
- No login, no account, no form that changes anything at the source.
- Every request goes through `fetcher.Client`: one request a second per
  host, eight a second for the SEC (its limit is ten), and a User-Agent
  that names the tool. A source that refuses for its rate is not asked
  again in the run.
- A run fetches only what the wanted list names. Never crawl a product
  range or a site.

## 2. Live runs

A download needs no login and fires no challenge, so a live
`download`, `download --dry-run` and `login --check` may run in
development. Keep each run small (`--ref`, `--limit`).

## 3. Providers and their terms

A new provider quotes its source's terms in the README. Public-data
providers may be on by default. An issuer, report, archive or vendor
provider stays off until `refdata.cfg` names it: enabling one accepts
its terms, and that is the user's decision, not the code's.

## 4. What stays out of the repo

- The SEC contact address. It lives in `refdata.cfg`, outside the repo.
- Anything a provider returned. A composition is public, but which
  funds a real wanted list names is the household's data (root
  [AGENTS.md](../../AGENTS.md) §4). Test fixtures are invented files
  in the real layout.
- The wanted list of a real gold.
