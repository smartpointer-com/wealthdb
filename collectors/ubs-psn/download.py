#!/usr/bin/env python3
"""
UBS PSN SFTP Pull downloader.

Connects to a UBS Private Standard Network SFTP server, pulls every
order-type zip currently waiting for the client, and stores the files
locally under <bronze-dir>/<UTC-timestamp>/<ORDERTYPE>.zip.

Per the UBS PSN SFTP factsheet, the per-order-type zip exists only when
new data is queued; UBS deletes the zip from its server after a
successful download. A missing remote file therefore just means "nothing
new for this order type" and is not an error.

Host authenticity is verified against the SHA-256 fingerprints UBS
publishes in the SFTP Pull factsheet. A connection to a host whose key
does not match is rejected before authentication.

Usage:
    download.py --client-id <login> --bronze-dir <dir> \\
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

from collectorkit import bronze, cli, debugcap, envfile

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
    p.add_argument("--env-file", type=Path, default=None,
                   help="KEY=VALUE credentials env file, sourced before the "
                        "client id is resolved (also honours the "
                        "UBS_PSN_ENV_FILE env var). The wrapper already "
                        "sources <secrets-dir>/ubs-psn.env, so this is for "
                        "running download.py directly.")
    p.add_argument("--client-id", default=None,
                   help="UBS PSN customer / SFTP login ID (e.g. CH123456). "
                        "Falls back to the UBS_PSN_CLIENT_ID env var "
                        "(sourced from <secrets>/ubs-psn.env by the wrapper).")
    p.add_argument("--bronze-dir", type=Path,
                   default=cli.default_data_root() / "ubs-psn",
                   help="Bronze tree root (default: %(default)s); "
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
    p.add_argument("--check", action="store_true",
                   help="Probe the configured credential and exit: load the "
                        "RSA key, open an SSH session (host-key checked) and "
                        "close it. Exit 0 when UBS accepts the key, non-zero "
                        "when it is missing, unloadable or rejected. Lists "
                        "nothing and consumes no files. This is what "
                        "`ubs-psn login --check` runs — PSN has no session to "
                        "mint, so the key IS the session.")
    p.add_argument("--dry-run", action="store_true",
                   help="Connect, authenticate and verify host key, then "
                        "exit without touching any files.")
    p.add_argument("--debug", action="store_true",
                   help="Capture debug artefacts into the bronze run dir "
                        "(default off): <run>/screenshots/sftp-listing.txt, "
                        "recording the accepted host-key fingerprint and "
                        "what each download/<ORDERTYPE>/ dir was actually "
                        "offering — which is what explains a pull that "
                        "brought back less than expected. Pure observation: "
                        "it lists, never fetches, and never changes what is "
                        "downloaded. A run that pulls nothing keeps its "
                        "shell (and the listing) for `prune` to reclaim "
                        "rather than discarding it unseen. `load` never "
                        "reads it. (For wire-level tracing use -v/--verbose, "
                        "which writes to stderr, not bronze.)")
    cli.add_standard_args(p, verb="download", full_history=True)
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


def _host_key_fingerprint(client: paramiko.SSHClient) -> str:
    """The SHA-256 fingerprint of the key the connected server presented —
    read back off the live transport, so it is by construction the key
    `FingerprintPolicy` accepted for this session."""
    transport = client.get_transport()
    key = transport.get_remote_server_key() if transport is not None else None
    return sha256_fingerprint(key) if key is not None else "unknown"


def _listing_line(sftp: paramiko.SFTPClient, remote: str) -> str:
    """One ``download/<ORDERTYPE>/`` dir as the server presents it.

    An absent dir is not an error — that is how an order type the customer
    is not provisioned for looks, and recording which ones are absent is
    part of the diagnostic."""
    try:
        entries = sftp.listdir_attr(remote)
    except FileNotFoundError:
        return f"{remote}/: absent (order type not provisioned)"
    except OSError as e:
        return f"{remote}/: listing failed: {e}"
    if not entries:
        return f"{remote}/: empty (nothing queued)"
    return f"{remote}/: " + ", ".join(
        f"{a.filename} ({a.st_size} bytes)"
        for a in sorted(entries, key=lambda a: a.filename))


def capture_remote_listing(sftp: paramiko.SFTPClient,
                           client: paramiko.SSHClient, run_dir: Path) -> None:
    """Record what the server was offering, under ``--debug``.

    `download_all` stats one exact path per order type, so a queued file
    under an unexpected name is indistinguishable from nothing queued. The
    listing is what tells those apart, which is why it is worth capturing
    at all.

    **Pure observation.** It lists and never reads a file: UBS deletes each
    zip server-side the moment it is downloaded and there is no re-fetch
    (CLAUDE.md), so a capture that fetched would silently consume the very
    data it was documenting. `download_all` never consults this listing —
    it still decides on its own `stat` — so a pull fetches exactly the same
    files with and without ``--debug``.

    Runs BEFORE the pull, for two reasons: the listing must show the zips
    while they still exist, and a capture that failed can then only cost a
    run, never strand a downloaded-but-unfinalised zip.
    """
    try:
        lines = [f"host-key: SHA256:{_host_key_fingerprint(client)}", ""]
        lines += [_listing_line(sftp, f"download/{ot}") for ot in ORDER_TYPES]
    except Exception as e:  # noqa: BLE001 - any paramiko error, never fatal
        # A diagnostic that breaks the run it is diagnosing is worse than no
        # diagnostic — and here that run is holding an irreplaceable pull.
        log.warning("--debug: SFTP listing capture failed: %s", e)
        return
    debugcap.capture_text(run_dir, "sftp-listing.txt",
                          "\n".join(lines) + "\n", log=log)


# FIXME: each order type is probed by stat-ing one EXACT expected filename
# (download/<OT>/<OT>.zip), so a file UBS queued under an unexpected name is
# indistinguishable from nothing being queued — both count as "empty" and the
# run reports success. Listing the dir and matching a pattern would tell the
# two apart. The --debug listing capture makes it diagnosable after the fact;
# it does not fix it. Worth doing if a silently-missed delivery ever bites:
# UBS deletes each file on a successful fetch, so a miss is not re-fetchable.
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


def _source_env_file(explicit: Path | None) -> None:
    """Source an explicit --env-file (or $UBS_PSN_ENV_FILE) before the client
    id is resolved, so a direct `download.py` run needs no wrapper. The
    wrapper already sources <secrets-dir>/ubs-psn.env; this one is sourced
    last, so its values win. A path that was asked for but is absent is an
    error, not a silent fallback to whatever the environment happened to
    hold."""
    path = explicit or (Path(os.environ["UBS_PSN_ENV_FILE"])
                        if os.environ.get("UBS_PSN_ENV_FILE") else None)
    if path is None:
        return
    if not path.is_file():
        raise SystemExit(f"--env-file does not exist: {path}")
    envfile.load_env_file(path, ("UBS_PSN_CLIENT_ID",), logger=log)


def _check_credential(args: argparse.Namespace) -> int:
    """Open and close one SSH session to prove UBS accepts the key.

    The fleet's `login --check` contract is "probe the stored session without
    minting a new one". PSN has no session — the RSA key is the credential —
    so the equivalent is a connect that authenticates and stops there: no
    SFTP channel, no listing, and above all no download (UBS deletes each zip
    on a successful fetch, so a probe must never touch one). `connect` raises
    SystemExit for a missing or unloadable key, which is the same failure.
    """
    try:
        client = connect(args)
    except paramiko.AuthenticationException:
        log.error("credential rejected by UBS — the key is not authorised "
                  "for client-id %s", args.client_id)
        return 1
    except (paramiko.SSHException, OSError) as e:
        log.error("probe could not reach the PSN endpoint: %s", e)
        return 1
    client.close()
    log.info("credential accepted — UBS authorised the key for client-id %s",
             args.client_id)
    return 0


def main() -> int:
    args = parse_args()
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(message)s",
    )
    # value-with-env-fallback (CLAUDE.md §3): --client-id VALUE else
    # UBS_PSN_CLIENT_ID, so a direct `download.py` with the env var exported
    # works, matching fred (F4).
    _source_env_file(args.env_file)
    args.client_id = envfile.resolve_credential(
        args.client_id, "UBS_PSN_CLIENT_ID", "--client-id")

    cli.warn_lookback_ignored(args.lookback, log,
                              what="whatever PSN data UBS currently has queued")

    if args.check:
        return _check_credential(args)

    # Validate the bronze root up front, before connecting. UBS deletes
    # each per-order-type zip immediately on a successful download, so a
    # local write failure after a download would silently destroy data.
    if not args.bronze_dir.is_dir():
        raise SystemExit(f"--bronze-dir does not exist: {args.bronze_dir}")
    if not os.access(args.bronze_dir, os.W_OK):
        raise SystemExit(f"--bronze-dir is not writable: {args.bronze_dir}")

    client = connect(args)
    try:
        sftp = client.open_sftp()
        log.info("SFTP session opened.")

        if args.dry_run:
            # Export nothing (root CLAUDE.md §2): no run dir is minted, so
            # --debug has nowhere to capture into. Resurrecting a shell just
            # to hold a listing would leave load/prune a dump to reason
            # about, which is a worse trade than capturing nothing.
            log.info("Dry run: skipping downloads.")
            return 0

        run_ts = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        run_dir = args.bronze_dir / run_ts
        run_dir.mkdir(parents=True, exist_ok=False)
        # Forward status marker (fleet convention): "in-progress" at run-dir
        # creation, atomically overwritten with a terminal status once the
        # pull finishes. This is additive metadata only — it never gates the
        # sftp.get data path, and prune's has-zip guard, not this field, is
        # what protects a crashed-but-non-empty dump from deletion.
        bronze.atomic_write_json(run_dir / "run.json", {"status": "in-progress"})

        if args.debug:
            capture_remote_listing(sftp, client, run_dir)

        downloaded, empty = download_all(sftp, run_dir, verbose=args.verbose)
        log.info("Done. %d zip(s) downloaded, %d order type(s) had nothing.",
                 downloaded, empty)
        if downloaded == 0 and not args.debug:
            # Nothing was queued and nothing was captured: the run dir holds
            # only the in-progress marker (no irreplaceable data), so discard
            # the whole shell. rmtree, not rmdir — the run.json makes the dir
            # non-empty.
            shutil.rmtree(run_dir)
        else:
            # status="empty" for a pull that found nothing but captured a
            # listing: the walk finished, so "in-progress" would be a lie,
            # yet a zip-less dump is not "complete" either. Being non-
            # complete is what lets prune reclaim the shell once quiescent,
            # taking the listing with it — the normal debug-artefact
            # lifecycle. Discarding it here instead would make the capture
            # unreachable in exactly the case it exists to explain.
            bronze.atomic_write_json(
                run_dir / "run.json",
                {"status": "complete" if downloaded else "empty",
                 "downloaded": downloaded, "empty": empty},
            )
        return 0
    finally:
        client.close()


if __name__ == "__main__":
    sys.exit(main())
