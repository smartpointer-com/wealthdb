#!/usr/bin/env python3
"""
UBS PSN SFTP Pull downloader.

Connects to a UBS Private Standard Network SFTP server, lists every
``download/<ORDERTYPE>/`` dir, pulls the queued zips, and stores the
files locally under <bronze-dir>/<UTC-timestamp>/.

Per the UBS PSN SFTP factsheet, the per-order-type queue zip
(``download/<OT>/<OT>.zip``) exists only when new data is queued; UBS
deletes it from its server after a successful download. A missing queue
file therefore just means "nothing new for this order type" and is not
an error. Next to the queue file UBS retains dot-prefixed dated archive
copies (``.<OT>_<YYYYMMDD>.zip``) reaching back roughly two months; a
fetch does not consume those, so a delivery missed or corrupted inside
that window is recoverable with ``--recover``. Beyond the archive
window a batch is irreplaceable.

Every run records the full pre-pull listing — per-order-type filenames
and sizes, the accepted host-key fingerprint, a capture stamp — in
``<run>/listing.json`` before anything is fetched.

Host authenticity is verified against the SHA-256 fingerprints UBS
publishes in the SFTP Pull factsheet. A connection to a host whose key
does not match is rejected before authentication.

Usage:
    download.py --client-id <login> --bronze-dir <dir> \\
                [--host <ip-or-hostname>] [--port <port>] [--key <path>] \\
                [--ignore-fingerprint-mismatch] [--dry-run] [--recover]
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import logging
import os
import re
import sys
import zipfile
from datetime import datetime, timezone
from pathlib import Path

import paramiko

from collectorkit import bronze, cli, envfile

# Trusted host-key SHA-256 fingerprints are loaded from a sibling file
# rather than embedded in the source, so updates to UBS's published keys
# don't require a code change.
FINGERPRINTS_FILENAME = "host_fingerprints.txt"

# Order types known to be exposed on UBS PSN SFTP. Sources: the public
# UBS PSN SFTP factsheet (Switzerland), sections "UBS Data Files
# Description" and "UBS file types"; plus the EBICS administrative order
# types (HAC, PTK) that may be exposed alongside.
#
# A given customer is provisioned for a subset only; the script lists
# all of them. An unprovisioned type's dir is simply absent
# (FileNotFoundError on listdir_attr) and a provisioned-but-idle type's
# dir holds no queue file — both are recorded in listing.json and
# skipped.
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
    mode = p.add_mutually_exclusive_group()
    mode.add_argument("--check", action="store_true",
                      help="Probe the configured credential and exit: load "
                           "the RSA key, open an SSH session (host-key "
                           "checked) and close it. Exit 0 when UBS accepts "
                           "the key, non-zero when it is missing, unloadable "
                           "or rejected. Lists nothing and consumes no "
                           "files. This is what `ubs-psn login --check` "
                           "runs — PSN has no session to mint, so the key "
                           "IS the session.")
    mode.add_argument("--recover", action="store_true",
                      help="Fetch the dot-prefixed dated archive copies "
                           "(.<ORDERTYPE>_<YYYYMMDD>.zip) instead of the "
                           "queue files. UBS retains roughly two months of "
                           "these next to each queue file, and a fetch does "
                           "not consume them, so recovery is freely "
                           "re-runnable; --lookback bounds how far back the "
                           "replay reaches (default: the whole archive). "
                           "Each copy lands undotted as "
                           "<ORDERTYPE>_<YYYYMMDD>.zip in a normal "
                           "timestamped run dir; the undotted queue files "
                           "are never touched.")
    p.add_argument("--dry-run", action="store_true",
                   help="Connect, authenticate and verify host key, then "
                        "exit without touching any files.")
    p.add_argument("--debug", action="store_true",
                   help="Accepted for fleet uniformity. The diagnostics this "
                        "gate captures elsewhere are recorded here "
                        "unconditionally: every run writes the full "
                        "pre-pull SFTP listing (per-order-type filenames "
                        "and sizes, plus the accepted host-key fingerprint) "
                        "to <run>/listing.json, so there is nothing extra "
                        "for --debug to capture — the flag is logged and "
                        "otherwise ignored. (For wire-level tracing use "
                        "-v/--verbose, which writes to stderr, not bronze.)")
    cli.add_common_args(p)
    p.add_argument("--lookback", type=cli.lookback_value, default=None,
                   metavar=cli.LOOKBACK_METAVAR,
                   help="A normal pull takes whatever UBS has queued (a "
                        "superset of any window), so this is logged and "
                        "otherwise ignored. Under --recover it bounds the "
                        "archive replay: only dated copies from the "
                        "resolved start date onward are fetched (default: "
                        "the whole ~2-month archive).")
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


def list_remote(sftp: paramiko.SFTPClient) -> dict[str, dict]:
    """One ``listdir_attr`` per ``download/<ORDERTYPE>/`` dir, as the
    server presents it.

    Returns ``{order_type: info}`` where ``info["status"]`` is
    ``"listed"`` (with ``"entries"``: ``[{"name", "size"}, ...]`` sorted
    by name), ``"absent"`` (dir missing — how an unprovisioned order
    type looks), or ``"error"`` (listing failed; the message is kept).
    Pure observation: nothing is fetched, so nothing is consumed.
    """
    out: dict[str, dict] = {}
    for ot in ORDER_TYPES:
        try:
            entries = sftp.listdir_attr(f"download/{ot}")
        except FileNotFoundError:
            out[ot] = {"status": "absent"}
            continue
        except OSError as e:
            out[ot] = {"status": "error", "error": str(e)}
            continue
        out[ot] = {"status": "listed", "entries": [
            {"name": a.filename, "size": a.st_size}
            for a in sorted(entries, key=lambda a: a.filename)]}
    return out


def write_listing(run_dir: Path, listing: dict[str, dict],
                  host_fp: str) -> None:
    """Record the pre-pull listing as ``<run>/listing.json``.

    Written on every run, before the first fetch, so it shows each zip
    while it still exists server-side, and a failed write can only cost
    a run — never strand a consumed-but-unrecorded batch. Provenance,
    not a load input: `load` never reads it, and `prune` never deletes
    it from a dump that holds zips (only a zip-less shell is ever
    removed whole).
    """
    bronze.atomic_write_json(run_dir / "listing.json", {
        "captured_at": datetime.now(timezone.utc)
        .strftime("%Y-%m-%dT%H:%M:%SZ"),
        "host_key": f"SHA256:{host_fp}",
        "order_types": listing,
    })


def validate_zip(path: Path) -> None:
    """Fail the run loudly when a fetched zip is corrupt.

    Integrity is judged by content — the central directory plus a CRC
    pass over every entry — never by comparing byte counts against the
    listed ``st_size``: the stream the server serves for a dated archive
    copy can undercut its listed size while still being a complete,
    valid zip. On failure the run dir is kept as-is (a consumed queue
    copy is already gone server-side); the batch remains recoverable
    from its dated archive copy via ``--recover``, which is itself
    freely retryable.
    """
    try:
        with zipfile.ZipFile(path) as zf:
            bad = zf.testzip()
    except zipfile.BadZipFile as e:
        raise SystemExit(
            f"Corrupt zip fetched: {path} ({e}). The run dir is kept; "
            f"re-fetch the batch with --recover.")
    if bad is not None:
        raise SystemExit(
            f"Corrupt zip fetched: {path} (CRC mismatch in {bad!r}). The "
            f"run dir is kept; re-fetch the batch with --recover.")


def download_all(sftp: paramiko.SFTPClient, run_dir: Path,
                 listing: dict[str, dict],
                 verbose: bool = False) -> tuple[int, int]:
    """Fetch every queued (undotted) file named in `listing`.

    The expected queue file is ``<OT>.zip``; any other undotted entry is
    fetched too, under its server filename, with a warning — bronze
    keeps every zip, and a queue file left on the server may be
    consumed or superseded later, so fetching is the data-preserving
    choice. The server filename is trusted only as far as it lands
    safely: a listed name that is not a plain basename (path
    separators, an absolute path, ``..``) would escape the run dir, and
    one that collides with ``run.json`` / ``listing.json`` would
    clobber the run's own metadata — such entries are refused with a
    warning and left on the server (a skipped fetch consumes nothing).
    Dot-prefixed entries are the dated archive trail that ``--recover``
    replays; a normal pull never touches them (the undotted queue file
    is deleted server-side by its own fetch, the archive copies are
    not). Every fetched zip is content-validated before the run
    finalises.
    """
    downloaded = 0
    empty = 0
    for ot in ORDER_TYPES:
        info = listing.get(ot, {"status": "absent"})
        if info["status"] == "absent":
            if verbose:
                print(f"order type {ot} not provisioned (download/{ot}/ "
                      f"absent)", file=sys.stderr, flush=True)
            empty += 1
            continue
        if info["status"] == "error":
            # A queue file stays queued until fetched, so a failed listing
            # postpones its pickup to the next run rather than losing it.
            log.warning("Listing download/%s/ failed (%s) — skipping.",
                        ot, info.get("error"))
            empty += 1
            continue
        fetched = 0
        for entry in info["entries"]:
            name = entry["name"]
            if name.startswith("."):
                continue
            if name != f"{ot}.zip":
                if (not name or name != Path(name).name
                        or name in ("run.json", "listing.json")):
                    log.warning("Refusing unexpected file in download/%s/: "
                                "%r is not a safe run-dir basename — "
                                "leaving it on the server.", ot, name)
                    continue
                log.warning("Unexpected file queued in download/%s/: %s "
                            "(%s bytes) — fetching it under its server "
                            "name.", ot, name, entry["size"])
            remote = f"download/{ot}/{name}"
            local = run_dir / name
            log.info("Downloading %s (%s bytes)", remote, entry["size"])
            sftp.get(remote, str(local))
            validate_zip(local)
            downloaded += 1
            fetched += 1
        if not fetched:
            if verbose:
                print(f"no data queued for {ot} (download/{ot}/)",
                      file=sys.stderr, flush=True)
            empty += 1
    return downloaded, empty


def recover_all(sftp: paramiko.SFTPClient, run_dir: Path,
                listing: dict[str, dict], since=None,
                verbose: bool = False) -> tuple[int, int]:
    """Fetch the dated archive copies (``.<OT>_<YYYYMMDD>.zip``) named
    in `listing`.

    The undotted queue files are never touched — fetching one consumes
    it server-side, and recovery must not. The dated copies survive
    their own fetch (file, size and mtime unchanged), so a recovery run
    is freely re-runnable. Each copy lands undotted as
    ``<OT>_<YYYYMMDD>.zip``, the shape `load` routes to the order
    type's loaders; `since` drops copies dated before it, ``None``
    replays the whole archive. Every fetched zip is content-validated,
    as in `download_all`.
    """
    downloaded = 0
    empty = 0
    for ot in ORDER_TYPES:
        info = listing.get(ot, {"status": "absent"})
        if info["status"] == "absent":
            empty += 1
            continue
        if info["status"] == "error":
            log.warning("Listing download/%s/ failed (%s) — skipping.",
                        ot, info.get("error"))
            empty += 1
            continue
        fetched = 0
        for entry in info["entries"]:
            name = entry["name"]
            m = re.match(rf"^\.{ot}_(\d{{8}})\.zip$", name)
            if not m:
                continue
            try:
                stamp = datetime.strptime(m.group(1), "%Y%m%d").date()
            except ValueError:
                log.warning("Unparseable date in archive copy "
                            "download/%s/%s — skipping.", ot, name)
                continue
            if since is not None and stamp < since:
                log.debug("Skipping download/%s/%s (dated before %s).",
                          ot, name, since)
                continue
            remote = f"download/{ot}/{name}"
            local = run_dir / f"{ot}_{m.group(1)}.zip"
            log.info("Recovering %s (%s bytes)", remote, entry["size"])
            sftp.get(remote, str(local))
            validate_zip(local)
            downloaded += 1
            fetched += 1
        if not fetched:
            if verbose:
                print(f"no archive copies to recover for {ot}",
                      file=sys.stderr, flush=True)
            empty += 1
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
    SFTP channel, no listing, and above all no download (UBS deletes each
    queue zip on a successful fetch, so a probe must never touch one).
    `connect` raises SystemExit for a missing or unloadable key, which is the
    same failure.
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
    # works, matching fred.
    _source_env_file(args.env_file)
    args.client_id = envfile.resolve_credential(
        args.client_id, "UBS_PSN_CLIENT_ID", "--client-id")

    if not args.recover:
        cli.warn_lookback_ignored(
            args.lookback, log,
            what="whatever PSN data UBS currently has queued")
    if args.debug:
        log.info("--debug: nothing extra to capture — every run records the "
                 "pre-pull SFTP listing in listing.json; the flag has no "
                 "effect.")

    if args.check:
        return _check_credential(args)

    # Validate the bronze root up front, before connecting. UBS deletes
    # each queue zip immediately on a successful download, so a local
    # write failure after a download would silently destroy data.
    if not args.bronze_dir.is_dir():
        raise SystemExit(f"--bronze-dir does not exist: {args.bronze_dir}")
    if not os.access(args.bronze_dir, os.W_OK):
        raise SystemExit(f"--bronze-dir is not writable: {args.bronze_dir}")

    client = connect(args)
    try:
        sftp = client.open_sftp()
        log.info("SFTP session opened.")

        if args.dry_run:
            # Export nothing (root CLAUDE.md §2): no run dir is minted,
            # nothing is listed, nothing is fetched.
            log.info("Dry run: skipping downloads.")
            return 0

        mode = "recover" if args.recover else "download"
        run_ts = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        run_dir = args.bronze_dir / run_ts
        run_dir.mkdir(parents=True, exist_ok=False)
        # Forward status marker (fleet convention): "in-progress" at run-dir
        # creation, atomically overwritten with a terminal status once the
        # pull finishes. This is additive metadata only — it never gates the
        # sftp.get data path, and prune's has-zip guard, not this field, is
        # what protects a crashed-but-non-empty dump from deletion.
        bronze.atomic_write_json(run_dir / "run.json",
                                 {"status": "in-progress", "mode": mode})

        # The pre-pull listing both drives the fetch and is recorded first,
        # so listing.json shows every zip while it still exists server-side
        # and any later failure leaves the record of what was on offer.
        listing = list_remote(sftp)
        write_listing(run_dir, listing, _host_key_fingerprint(client))

        if args.recover:
            since = None
            if args.lookback:
                since = cli.lookback_start(
                    args.lookback, today=datetime.now(timezone.utc).date())
                log.info("Recovering archive copies dated %s or later.",
                         since)
            downloaded, empty = recover_all(sftp, run_dir, listing,
                                            since=since,
                                            verbose=args.verbose)
        else:
            downloaded, empty = download_all(sftp, run_dir, listing,
                                             verbose=args.verbose)
        log.info("Done. %d zip(s) downloaded, %d order type(s) had nothing.",
                 downloaded, empty)
        # status="empty" for a pull that found nothing: the walk finished,
        # so "in-progress" would be a lie, yet a zip-less dump is not
        # "complete" either. Being non-complete is what lets prune reclaim
        # the shell — listing.json and all — once quiescent, while the
        # listing stays inspectable in the meantime: it is the record that
        # explains a pull that brought back nothing.
        bronze.atomic_write_json(
            run_dir / "run.json",
            {"status": "complete" if downloaded else "empty",
             "mode": mode, "downloaded": downloaded, "empty": empty},
        )
        return 0
    finally:
        client.close()


if __name__ == "__main__":
    sys.exit(main())
