#!/usr/bin/env python3
"""
UBS PSN SFTP Pull downloader.

Connects to a UBS Private Standard Network SFTP server, pulls every
order-type zip currently waiting for the client, and stores the files
locally under <dest>/<UTC-timestamp>/<ORDERTYPE>.zip.

Per the UBS PSN SFTP factsheet, the per-order-type zip exists only when
new data is queued; UBS deletes the zip from its server after a
successful download. A missing remote file therefore just means "nothing
new for this order type" and is not an error.

Host authenticity is verified against the SHA-256 fingerprints UBS
publishes in the SFTP Pull factsheet. A connection to a host whose key
does not match is rejected before authentication.

Usage:
    download.py --client-id <login> --dest <dir> \\
                [--host <ip-or-hostname>] [--port <port>] [--key <path>] \\
                [--ignore-fingerprint-mismatch] [--dry-run]
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import logging
import os
import shutil
import sys
from datetime import datetime, timezone
from pathlib import Path

import paramiko

from collectorkit import bronze, cli

# Trusted host-key SHA-256 fingerprints are loaded from a sibling file
# rather than embedded in the source, so updates to UBS's published keys
# don't require a code change.
FINGERPRINTS_FILENAME = "host_fingerprints.txt"

# Order types known to be exposed on UBS PSN SFTP. Sources: the public
# UBS PSN SFTP factsheet (Switzerland), sections "UBS Data Files
# Description" and "UBS file types"; plus the EBICS administrative order
# types (HAC, PTK) that may be exposed alongside.
#
# A given customer is provisioned for a subset only; the script attempts
# all of them, and UBS returns the per-type zip only when new data is
# queued, so unprovisioned/empty types simply yield FileNotFoundError on
# stat() and are skipped.
ORDER_TYPES = (
    # EBICS administrative order types
    "HAC", "PTK",
    # Category 3: Treasury Markets - FX, Money Markets, Derivatives
    "ZAA", "ZAB", "ZAC", "ZAD", "ZAE",
    # Category 5: Securities Markets
    "ZM1", "ZM2", "ZM3", "ZAG", "ZM4", "ZAH", "ZMH", "ZM5",
    "ZAI", "ZAJ", "ZAK", "ZAL", "ZAM", "ZM6", "ZAN", "ZM7", "ZM8", "ZMI",
    # Category 6: Treasury Markets - Metals
    "ZAX", "ZM9", "ZMA",
    # Category 9: Cash Management and Customer Status
    "ZAO", "ZAP", "Z40", "ZAQ", "Z42", "ZAY", "ZMC",
    # UBS aggregate file types
    "ZMD", "ZME", "ZMG",
)

log = logging.getLogger("ubs-psn")


def sha256_fingerprint(key: paramiko.PKey) -> str:
    """OpenSSH-style base64 SHA-256 fingerprint (no SHA256: prefix, no padding)."""
    return base64.b64encode(hashlib.sha256(key.asbytes()).digest()) \
        .decode("ascii").rstrip("=")


class FingerprintPolicy(paramiko.MissingHostKeyPolicy):
    """Accept the server iff its host-key SHA-256 fingerprint is allow-listed.

    If `ignore_mismatch` is set, a non-matching fingerprint produces a
    warning on stderr and the connection proceeds — meant only as an
    emergency override during host-key rotations.
    """

    def __init__(self, allowed_sha256_b64: frozenset[str],
                 ignore_mismatch: bool = False) -> None:
        self.allowed = allowed_sha256_b64
        self.ignore_mismatch = ignore_mismatch

    def missing_host_key(self, client, hostname, key) -> None:
        fp = sha256_fingerprint(key)
        if fp in self.allowed:
            log.info("Host key OK (SHA256:%s, type=%s)", fp, key.get_name())
            return
        msg = (
            f"Untrusted host key for {hostname}: SHA256:{fp} "
            f"is not in the trusted fingerprint set "
            f"(allowed: {sorted(self.allowed)})."
        )
        if not self.ignore_mismatch:
            raise paramiko.SSHException(
                msg + " Re-run with --ignore-fingerprint-mismatch to bypass "
                "(only after confirming the value with UBS)."
            )
        print(
            f"WARNING: {msg} Proceeding because --ignore-fingerprint-mismatch "
            f"was given.",
            file=sys.stderr,
        )


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description="Download all pending UBS PSN data over SFTP.",
    )
    p.add_argument("--host", default="sftp-keyport-ch.ubs.com",
                   help="UBS SFTP server hostname or IP "
                        "(default: sftp-keyport-ch.ubs.com, per UBS PSN "
                        "SFTP factsheet, Switzerland).")
    p.add_argument("--port", type=int, default=26701,
                   help="UBS SFTP server port (default: 26701).")
    p.add_argument("--client-id", required=True,
                   help="UBS PSN customer / SFTP login ID (e.g. CH123456).")
    p.add_argument("--dest", type=Path,
                   default=cli.default_data_root() / "ubs-psn",
                   help="Local destination directory (default: %(default)s); "
                        "a UTC-timestamped subdirectory is created per run.")
    p.add_argument("--key", type=Path,
                   default=Path.home() / ".secrets" / "ubs_psn_key",
                   help="SSH private key path "
                        "(default: ~/.secrets/ubs_psn_key).")
    p.add_argument("--ignore-fingerprint-mismatch", action="store_true",
                   help="If the server's host-key SHA-256 fingerprint is "
                        "not in host_fingerprints.txt, emit a warning to "
                        "stderr and continue instead of aborting. Use only "
                        "during a verified UBS key rotation.")
    p.add_argument("--dry-run", action="store_true",
                   help="Connect, authenticate and verify host key, then "
                        "exit without touching any files.")
    p.add_argument("--debug", action="store_true",
                   help="Capture debug artefacts into the bronze run dir. "
                        "An SFTP pull produces none, so today this gates "
                        "nothing; the flag exists so the fleet's `--debug` "
                        "convention is uniform. (For wire-level tracing use "
                        "-v/--verbose, which writes to stderr, not bronze.)")
    p.add_argument("-v", "--verbose", action="store_true",
                   help="Enable DEBUG logging (incl. paramiko transport).")
    return p.parse_args()


def load_trusted_fingerprints() -> frozenset[str]:
    """Read SHA-256 fingerprints from host_fingerprints.txt next to this script."""
    path = Path(__file__).resolve().parent / FINGERPRINTS_FILENAME
    if not path.is_file():
        raise SystemExit(f"Trusted fingerprints file not found: {path}")
    out: set[str] = set()
    for raw in path.read_text(encoding="utf-8").splitlines():
        line = raw.split("#", 1)[0].strip()
        if not line:
            continue
        if line.upper().startswith("SHA256:"):
            line = line.split(":", 1)[1]
        out.add(line.rstrip("="))
    if not out:
        raise SystemExit(f"No fingerprints listed in {path}")
    return frozenset(out)


def connect(args: argparse.Namespace) -> paramiko.SSHClient:
    if not args.key.is_file():
        raise SystemExit(f"Private key not found: {args.key}")

    # UBS PSN SFTP only supports RSA keys (factsheet, "Supported Key Type").
    # Load explicitly so an auth failure surfaces as an auth error rather
    # than as paramiko's confusing "tried other key formats" exception.
    try:
        pkey = paramiko.RSAKey.from_private_key_file(str(args.key))
    except paramiko.SSHException as e:
        raise SystemExit(f"Could not load RSA private key {args.key}: {e}")

    allowed = load_trusted_fingerprints()
    client = paramiko.SSHClient()
    client.set_missing_host_key_policy(
        FingerprintPolicy(allowed, ignore_mismatch=args.ignore_fingerprint_mismatch)
    )
    log.info("Connecting to %s:%d as %s", args.host, args.port, args.client_id)
    client.connect(
        hostname=args.host,
        port=args.port,
        username=args.client_id,
        pkey=pkey,
        allow_agent=False,
        look_for_keys=False,
        timeout=30,
    )
    return client


def download_all(sftp: paramiko.SFTPClient, run_dir: Path,
                 verbose: bool = False) -> tuple[int, int]:
    downloaded = 0
    empty = 0
    for ot in ORDER_TYPES:
        remote = f"download/{ot}/{ot}.zip"
        local = run_dir / f"{ot}.zip"
        try:
            attrs = sftp.stat(remote)
        except FileNotFoundError:
            if verbose:
                print(f"no data queued for {ot} ({remote})",
                      file=sys.stderr, flush=True)
            empty += 1
            continue
        log.info("Downloading %s (%s bytes)", remote, attrs.st_size)
        sftp.get(remote, str(local))
        downloaded += 1
    return downloaded, empty


def main() -> int:
    args = parse_args()
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(message)s",
    )

    # Validate the destination up front, before connecting. UBS deletes
    # each per-order-type zip immediately on a successful download, so a
    # local write failure after a download would silently destroy data.
    if not args.dest.is_dir():
        raise SystemExit(f"Destination directory does not exist: {args.dest}")
    if not os.access(args.dest, os.W_OK):
        raise SystemExit(f"Destination directory is not writable: {args.dest}")

    client = connect(args)
    try:
        sftp = client.open_sftp()
        log.info("SFTP session opened.")

        if args.dry_run:
            log.info("Dry run: skipping downloads.")
            return 0

        run_ts = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        run_dir = args.dest / run_ts
        run_dir.mkdir(parents=True, exist_ok=False)
        # Forward status marker (fleet convention): "in-progress" at run-dir
        # creation, atomically overwritten with "complete" once the pull
        # finishes. This is additive metadata only — it never gates the
        # sftp.get data path, and prune's has-zip guard, not this field, is
        # what protects a crashed-but-non-empty dump from deletion.
        bronze.atomic_write_json(run_dir / "run.json", {"status": "in-progress"})

        downloaded, empty = download_all(sftp, run_dir, verbose=args.verbose)
        log.info("Done. %d zip(s) downloaded, %d order type(s) had nothing.",
                 downloaded, empty)
        if downloaded == 0:
            # Nothing was queued: the run dir holds only the in-progress
            # marker (no irreplaceable data), so discard the whole shell.
            # rmtree, not rmdir — the run.json makes the dir non-empty.
            shutil.rmtree(run_dir)
        else:
            bronze.atomic_write_json(
                run_dir / "run.json",
                {"status": "complete", "downloaded": downloaded, "empty": empty},
            )
        return 0
    finally:
        client.close()


if __name__ == "__main__":
    sys.exit(main())
