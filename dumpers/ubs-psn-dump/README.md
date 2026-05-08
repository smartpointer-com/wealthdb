# ubs-psn-dump

A toolkit for ingesting UBS Private Standard Network (PSN) banking data:
fetching the raw per-order-type zips over SFTP Pull, then (in subsequent
scripts) decompressing, parsing, and reshaping them into formats that
downstream tools — e.g. local LLM-based agents — can consume directly.

## Tools

| Script | Status | Purpose |
| --- | --- | --- |
| [`download.py`](download.py) | implemented | Fetches all pending PSN data from UBS over SFTP Pull and stores the per-order-type zips locally, organised by UTC timestamp. |
| _future_ | planned | Unzip and parse SWIFT / XML / CSV payloads. |
| _future_ | planned | Project the parsed data into an agent-friendly shape (canonical JSON, normalised account/transaction views, etc.). |

The sections below document the only tool that currently exists.

## download.py

### How it works

Per the UBS PSN SFTP Pull factsheet, each authorised order type is exposed
at `download/<TYPE>/<TYPE>.zip`. UBS materialises a given zip only when
new data is queued for that type, and removes it from the server after a
successful download. `download.py` therefore:

1. Connects to the UBS SFTP server (defaults: Switzerland endpoint).
2. Verifies the server's host-key SHA-256 fingerprint against
   `host_fingerprints.txt`.
3. Authenticates with an RSA key (UBS only supports RSA).
4. For every known order type, `stat()`s the remote path and downloads
   the zip if present.

A missing remote zip is normal (no data queued) and is skipped silently.
The script intentionally never lists the `download/` directory — the SFTP
account is restricted from `READDIR` on it anyway.

### Prerequisites

- Python 3.11+
- An active UBS PSN agreement with the SFTP Pull channel selected.
- An RSA SSH key pair (>= 2048 bits) whose public key has been emailed
  to UBS at `sh-psn@ubs.com` and activated on UBS's side.

Generate a key pair with:

```sh
ssh-keygen -t rsa -b 4096 -f ~/.secrets/ubs_psn_key -N ""
```

then email `~/.secrets/ubs_psn_key.pub` to UBS as instructed in the PSN
SFTP factsheet.

### Setup

```sh
git clone <this repo>
cd ubs-psn-dump
python3 -m venv .venv
.venv/bin/pip install -r requirements.txt
```

### Configuration

`host_fingerprints.txt` lists the SHA-256 fingerprints (OpenSSH base64,
no `SHA256:` prefix or padding required) the SFTP server is allowed to
present. The values shipped come from the published UBS PSN SFTP Pull
factsheet (Switzerland). UBS may rotate these without re-issuing the
factsheet; if the fingerprint changes, confirm the new value out-of-band
with UBS before adding it to this file.

### Usage

Dry run — connects, verifies host key, authenticates, exits without
touching files:

```sh
.venv/bin/python download.py --client-id CHxxxxxx --dest ./data --dry-run
```

Real download:

```sh
.venv/bin/python download.py --client-id CHxxxxxx --dest ./data
```

Files land in `./data/<UTC-timestamp>/<ORDERTYPE>.zip`. If the run
downloaded nothing, the timestamped directory is removed.

#### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--host` | `sftp-keyport-ch.ubs.com` | UBS SFTP hostname or IP |
| `--port` | `26701` | UBS SFTP port |
| `--client-id` | _(required)_ | UBS customer / SFTP login ID |
| `--dest` | _(required)_ | Local destination directory |
| `--key` | `~/.secrets/ubs_psn_key` | Private RSA key path |
| `--ignore-fingerprint-mismatch` | off | Warn instead of abort on host-key mismatch |
| `--dry-run` | off | Skip downloads |
| `-v`, `--verbose` | off | DEBUG-level logging |

### Caveats

- **First successful download is one-shot.** Because UBS deletes the
  server-side zip on a successful download, you cannot re-download a
  given file via this script. Use `--dry-run` for connectivity tests.
- **Order types are the documented union.** A customer may not be
  provisioned for every order type listed; the script attempts each
  and silently skips those that yield `FileNotFoundError`.
- **No retry / resume / scheduling.** Run from cron, launchd, or your
  scheduler of choice.
