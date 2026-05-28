# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[CLAUDE.md](../../CLAUDE.md). The ubs-psn-specific surface below
applies on top of those shared rules.

- UBS **deletes each file server-side immediately on a successful
  download** — there is no re-fetch. An unprompted download therefore
  permanently consumes that day's data and may break downstream
  workflows.

## Authentication & private data

See the repo-root [CLAUDE.md](../../CLAUDE.md) §3 (authentication) and
§4 (no private information in source). They apply in full here.
