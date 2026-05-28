# Notes for Claude / coding agents

Two ground rules apply when working on this repo. Both are
non-negotiable.

## 1. Do not run real SFTP downloads unless the user asks

`download.py` pulls per-order-type zips from UBS. UBS **deletes each
file server-side immediately on a successful download** — there is no
re-fetch. An unprompted download therefore permanently consumes that
day's data and may break downstream workflows.

Allowed without asking:

- Read the code, configs, and docs.
- Run `download.py --dry-run` (connects, authenticates, verifies the
  host key, exits without `stat`/`get` against `download/`). This is
  the right way to verify connectivity-related changes.
- Run unit tests / mocked-SFTP exercises.

Not allowed unless the user explicitly asks:

- Run `download.py` without `--dry-run`.
- Call any `sftp.get(...)` / equivalent against the live UBS server,
  whether from the script, an ad-hoc REPL, or a one-off shell command.
- Add or change scheduling (cron, launchd, systemd timer, GitHub
  Actions, etc.) that would cause downloads to fire automatically.

## 2. Do not leak private information into source

The repo is intended to be publishable. Do not write any of the
following into tracked files (source, configs, comments, commit
messages, test fixtures):

- Customer / SFTP login IDs. Use a placeholder like `CHxxxxxx` in
  examples.
- Real SSH keys or key paths beyond the documented
  `~/.secrets/ubs_psn_key` default.
- Personal data: names, addresses, phone numbers, email addresses,
  banking relationship numbers.
- Identifiers from customer-specific UBS PDFs (e.g. internal dialog
  user IDs, per-customer server names like `SFTPCHnn`).

The Switzerland endpoint hostname/port and the publicly documented host
key fingerprints in `host_fingerprints.txt` come from UBS's public
factsheet and are fine to commit.

When in doubt, ask the user before adding a value that looks
identifier-shaped.
