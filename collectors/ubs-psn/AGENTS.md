# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[AGENTS.md](../../AGENTS.md). The ubs-psn-specific surface below
applies on top of those shared rules.

- UBS **deletes each queue file (`download/<OT>/<OT>.zip`) server-side
  immediately on a successful download** — that fetch cannot be
  repeated. A dot-prefixed dated archive copy of each batch stays on
  the server for roughly two months and survives fetching
  (`download --recover` replays it); beyond that window the data is
  irreplaceable. An unprompted download still consumes the queued
  files and may break downstream workflows — never trigger one without
  being asked.
- There is no MFA and no session. The RSA key is the credential.
  `login --check` and `download --dry-run` each open one authenticated
  SSH session with that key and close it. Neither lists nor fetches a
  file.

## Authentication & private data

See the repo-root [AGENTS.md](../../AGENTS.md) §3 (authentication) and
§4 (no private information in source). They apply in full here.
