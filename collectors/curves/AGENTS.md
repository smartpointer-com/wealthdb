# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[AGENTS.md](../../AGENTS.md). The curves-specific surface below applies
on top of those shared rules.

`curves` holds **no account data and no credential**. It reads public
interest-rate statistics: the ECB's euro area yield curve and the SNB's
spot rates of Swiss Confederation bonds. It never signs in anywhere.
Rates, dates and series ids are public and fine to commit, as in the
tests.

## 1. Public reads only

- GET requests to the two APIs [README.md](README.md) names
  (download.py's `ECB_URL` and `SNB_URL`): the ECB Data Portal's data
  API and the SNB data portal's warehouse API, the latter's
  latest-update endpoint included.
- No login, no account, no form, no POST.
- A run asks each portal once for its data. Keep it that way: the SNB
  asks API users not to poll its data endpoints, so a run checks its
  latest update first, and `login --check` asks only that
  ([README.md, Terms](README.md#terms)).

## 2. Live runs

A download needs no login and fires no challenge. So a live
`login --check`, `download` and `download --dry-run` may run in
development. Keep each run small (`--series`, a short `--lookback`) and
a few requests a minute. A whole-history download is one large answer
per portal: run it only when it is needed.

## 3. Terms

The portals' terms are quoted in the README, and only there. A new
portal or series quotes its source's terms there first. Stop and ask
before adding a source whose terms forbid automated access or the
redistribution of figures derived from it.
