# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[AGENTS.md](../../AGENTS.md). The fred-specific surface below applies on
top of those shared rules.

`fred` has **no private account data**. It reads **public** FX
reference rates (the US Federal Reserve H.10 release) and Treasury
yields (the H.15 release) via the FRED API. FX rates, dates and FRED
series IDs are public and fine to commit, as in the tests. Root
[AGENTS.md](../../AGENTS.md) §4 here protects the one secret, the **API
key**. Root §3 governs how that key reaches the tool.

## 1. Read-only, public data — never call write/other endpoints

The only FRED endpoint this collector touches is
`GET /fred/series/observations` (read-only time series). Do not add calls
to account-, billing-, or write-oriented endpoints (FRED has none that
matter here, but keep the surface to observations).

## 2. The API key is the only credential

- `FRED_API_KEY` reaches the tool via the environment only, sourced from
  `~/.secrets/fred.env`. It takes no command-line flag: a secret there
  leaks into `ps` and the shell history (root
  [AGENTS.md](../../AGENTS.md) §3). Never hard-code it, never commit it,
  and never paste it into a tracked file or a log.
- When debugging the live API, build request URLs so the key is not
  echoed to stdout/logs (it travels in the query string).
