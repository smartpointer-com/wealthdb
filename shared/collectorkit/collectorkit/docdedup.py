"""Shared download-avoidance engine for collector ``download.py``.

Where the shipped :mod:`collectorkit.dedup` reclaims duplicate disk *after*
the bytes are already on disk (a host-side sweep, run post-load),
``docdedup`` acts *before* the fetch, inside a live ``download`` walk, and can
avoid the fetch itself — the bandwidth, the authenticated request against a
live financial source (the bot-detection surface that matters most), and the
wall-clock inside a held-open MFA session. It generalises viac's proven
``find_existing_pdf`` + ``os.link`` pattern into a collector-agnostic helper.

Two modes, chosen **per document class** (not per collector):

* **link** (opt-in) — the same logical document lands at the same
  content-derived path every run, so on a hit hardlink the prior run's identical
  file into the new run dir and skip the fetch. On any error the hardlink falls
  through to a real fetch, so a document is never lost — only ever fetched. The
  linked file is a real (hard-linked) file physically inside the new run dir, so
  run dirs stay self-contained and **no loader change is needed**.
* **fetch-verify** (the default) — always fetch, then compare the fresh bytes
  against the prior run's copy for the same identity; on a byte-identical match
  discard the fetched bytes and hardlink the prior copy in (reclaims disk, still
  self-contained), on a difference or a genuinely new doc keep the fetched
  bytes. It reclaims exactly the same disk as link on an unchanged document (both
  collapse to one hardlinked copy) — it only pays the *fetch*. That fetch is what
  makes it the one mode correct against a *silent server-side re-issue* (a K-1 /
  corrected statement re-issued under a stable identity, where link-mode would
  serve a stale, superseded copy), and deduping a byte-identical copy is always
  safe, so it is also the fail-safe for any document whose class is unknown.

**Correctness rule (hard).** Mode is a property of the ``(collector,
document-class)`` pair, and **fetch-verify is the default**. A class is opted
into ``link`` only when serving a byte-identical prior copy in place of a fresh
fetch is known to be acceptable — i.e. the document is immutable under its key,
or the collector never parses it so a superseded copy cannot reach silver.
Everything else — parsed / restatement-prone documents, tax documents, and any
unclassified type — stays ``fetch-verify``: deduping a byte-identical copy is
always safe, but *skipping the fetch* of a document that might have changed is
not. The safe direction must be established before opting a class out of
fetch-verify, never the reverse.

The index of prior documents (:class:`SkipSet`) is **stateless** — rebuilt from
scratch each run from the COMPLETE prior runs' manifests/on-disk files via a
per-collector ``extract`` hook, with no persistent ledger. A key counts only if
its file still exists on disk, so a pruned bronze file simply drops out of the
index and is re-fetched — bronze self-heals. There is **no CLI verb**: unlike
``prune`` / ``dedup`` / ``recompress`` this engine runs inside ``download.py``,
so its whole surface is this Python API.

Hardlink only — never a clone: ``clonefile`` is unavailable inside the docker
collector runtimes; ``os.link`` works across the Docker-VM bind mounts
(proven by viac).
The engine only ever *adds* a file (a hardlink) or *declines to add* one; it
never deletes or rewrites a load input.
"""
from __future__ import annotations

import json
import logging
import os
from collections import deque
from dataclasses import dataclass
from datetime import date, timedelta
from pathlib import Path
from typing import Callable, Iterable

from collectorkit import bronze, prune

log = logging.getLogger("collectorkit.docdedup")

# ---------------------------------------------------------------------------
# Document classes and the class -> mode mapping (the correctness core)
# ---------------------------------------------------------------------------

CLASS_IMMUTABLE = "immutable"   # finalized, never re-issued under its key
CLASS_TAX = "tax"               # K-1 / 1099 / corrected — re-issued under a stable id
CLASS_MUTABLE = "mutable"       # alias for tax: any doc that can change under its key

MODE_LINK = "link"              # opt-in fetch-avoidance: hardlink a prior copy
MODE_FETCH_VERIFY = "fetch-verify"  # default: always fetch, dedup if byte-identical

# The default, correctness-safe mapping. Only ``immutable`` is opted into link;
# an unclassified document (a class not in this map, or ``None``) resolves to
# MODE_FETCH_VERIFY — always fetched, but a byte-identical copy is still
# deduped, so there is no reason to fetch a document without verifying it.
DEFAULT_CLASS_MODES: dict[str, str] = {
    CLASS_IMMUTABLE: MODE_LINK,
    CLASS_TAX: MODE_FETCH_VERIFY,
    CLASS_MUTABLE: MODE_FETCH_VERIFY,
}

# ---------------------------------------------------------------------------
# Per-document outcome codes (mirrors viac's {total, fetched, linked, skipped})
# ---------------------------------------------------------------------------

LINKED = "linked"              # link-mode: fetch avoided, prior file hardlinked in
FETCHED = "fetched"            # fresh bytes fetched and kept
VERIFIED = "verified"          # fetch-verify: byte-identical to prior, prior hardlinked
CHANGED = "changed"           # fetch-verify: prior existed but bytes differ (re-issue)
FETCH_FAILED = "fetch-failed"  # fetch() produced no file (error/empty)
# skip-mode (record "already have it", write no blob, load resolves across runs)
# is intentionally NOT implemented yet — only link / fetch-verify / fetch ship,
# all of which keep run dirs self-contained (no loader change). No outcome code
# is reserved for it until it exists, so nothing here looks live but unreachable.


# ---------------------------------------------------------------------------
# Manifest audit counters — the {total, per-outcome} block every adopter writes
# ---------------------------------------------------------------------------
#
# Every collector that adopts docdedup records the same per-document audit block
# in its run.json (mirrors viac's original {total, fetched, linked, skipped}).
# The outcome -> bucket mapping and the zero/increment helpers are identical
# across adopters, so they live here rather than being copied per collector.

# Maps a process() outcome to its manifest bucket. A collector extends this with
# its own PRE-fetch sentinels (e.g. a no-download-URL doc) via ``tally(extra=)``.
OUTCOME_BUCKET = {
    LINKED: "linked",       # link-mode: fetch avoided, prior copy hardlinked in
    FETCHED: "fetched",     # fresh bytes fetched and kept
    VERIFIED: "verified",   # fetch-verify: byte-identical to prior, hardlinked
    CHANGED: "changed",     # fetch-verify: re-issued under a stable id, fresh kept
    FETCH_FAILED: "errors",  # a real fetch failure
}
_BASE_BUCKETS = ("total", "fetched", "linked", "verified", "changed",
                 "errors", "other")


def empty_audit(*extra_buckets: str) -> dict:
    """A zeroed manifest audit block: ``total`` + one counter per outcome bucket,
    plus any collector-specific ``extra_buckets`` (e.g. ``"no_blob"`` for a
    document that carried no download URL)."""
    return {b: 0 for b in (*_BASE_BUCKETS, *extra_buckets)}


def tally(counts: dict, outcome: str, *, extra: dict | None = None) -> None:
    """Increment ``counts`` for one document ``outcome``.

    ``extra`` maps a collector's own pre-fetch sentinel codes to their bucket
    (e.g. ``{NO_BLOB: "no_blob"}``). An outcome the map does not know goes to
    ``"other"`` — never silently folded into ``"fetched"`` — with a warning,
    since an unmapped code is a programming error, not a fetched document.
    """
    buckets = OUTCOME_BUCKET if not extra else {**OUTCOME_BUCKET, **extra}
    bucket = buckets.get(outcome)
    if bucket is None:
        log.warning("unmapped docdedup outcome %r; counting as 'other'", outcome)
        bucket = "other"
    counts[bucket] = counts.get(bucket, 0) + 1


def audit_summary(counts: dict) -> str:
    """A stable ``key=value`` one-liner over an audit block, for a log line —
    the standard buckets first in a fixed order, then any integer collector
    extras. Non-integer entries (e.g. a collector's list of collected filenames)
    are skipped, so this is safe to call on a richer audit dict."""
    order = ["total", "fetched", "linked", "verified", "changed",
             "no_blob", "errors", "other"]
    def _num(k):
        return k in counts and isinstance(counts[k], int)
    keys = [k for k in order if _num(k)]
    keys += [k for k in counts if k not in order and _num(k)]  # collector extras
    return " ".join(f"{k}={counts[k]}" for k in keys)


def mode_for_class(doc_class: str | None,
                   class_modes: dict[str, str] | None = None) -> str:
    """Resolve a document class to its docdedup mode.

    ``immutable -> link``; ``tax``/``mutable`` and an unknown / ``None`` class
    all ``-> fetch-verify`` (the safe default — always fetch, dedup a
    byte-identical copy). ``class_modes`` overrides the defaults per collector
    but keeps the same fetch-verify fail-safe for unlisted classes.
    """
    modes = DEFAULT_CLASS_MODES if class_modes is None else class_modes
    return modes.get(doc_class, MODE_FETCH_VERIFY)


# Completeness classification reuses prune's shared lenient config
# (``prune.LENIENT_CLASSIFY_CFG``), exactly as dedup does, so docdedup only
# ever indexes COMPLETE prior runs and can never disagree with prune / dedup /
# recompress about what "complete" means.


# ---------------------------------------------------------------------------
# Core types
# ---------------------------------------------------------------------------

@dataclass(frozen=True)
class DocRef:
    """The identity of one document captured by a prior run.

    ``key`` is the logical identity used to match a current document to a prior
    file — a tuple so it can carry as many components as a collector needs
    (equityzen: ``(deal-slug, doc-slug)``; a label-keyed collector:
    ``(normalized-label, occurrence)``). Labels repeat across accounts/periods,
    so the index is a **multiset** (see :meth:`SkipSet.take`).

    ``doc_date`` is the document's issue date when known *before* the fetch
    (drives the freshness window); ``None`` when the collector has no reliable
    pre-fetch date (e.g. equityzen). ``relpath`` is the run-dir-relative path of
    the actual on-disk file. ``doc_class`` tags the document's class when the
    prior manifest carries it; it is informational for the index (mode is chosen
    from the *live* document at download time), so a disk-driven extract may
    leave it ``None``. ``size`` is the document's byte size when a collector can
    record it pre-fetch; it feeds :func:`link_or_fetch`'s optional
    ``expected_size`` collision guard for **label-keyed** callers (see there).
    ``None`` (the default) leaves that guard off — equityzen's keys are already
    collision-free (unique sha256 of the Relay id), so it provides no size.
    """
    key: tuple
    doc_date: date | None
    relpath: str
    doc_class: str | None = None
    size: int | None = None


class SkipSet:
    """Stateless multiset index of documents present in PRIOR complete runs.

    Rebuilt from scratch at the start of each documents phase — no persistent
    ledger, no new mutable state. Keyed ``key -> deque`` of prior absolute
    paths, oldest run first, so the canonical (archival) copy is handed out
    first, matching dedup's oldest-sorts-first rule.
    """

    def __init__(self, index: dict[tuple, "deque[Path]"]):
        self._index = index

    @classmethod
    def derive(cls, bronze_root: Path,
               extract: Callable[[Path, dict | None], Iterable[DocRef]],
               *, freshness_days: float | None = 35,
               now: date | None = None,
               exclude_run: Path | None = None) -> "SkipSet":
        """Build the skip index from the COMPLETE prior runs under
        ``bronze_root``.

        ``extract(run_dir, manifest) -> Iterable[DocRef]`` recovers each prior
        run's documents (disk-driven glob or manifest-driven — either is fine).

        Only COMPLETE runs (via ``prune.classify``) contribute, so an abandoned
        / in-progress / dry-run prior never seeds a phantom "already have it".
        ``exclude_run`` (normally the current in-progress run dir) is skipped
        outright; its in-progress status would exclude it anyway, but naming it
        is belt-and-braces.

        ``freshness_days`` defaults to ``35``, so a
        link-mode adopter that omits it still gets a freshness window: a document
        issued within that many days of ``now`` is left out of the index and
        re-fetched, since a just-issued doc may still be corrected under its id.
        Pass ``None`` to disable the window — appropriate when the collector has
        no reliable pre-fetch ``doc_date`` (e.g. equityzen), since a ``None``
        ``doc_date`` never falls inside the window anyway.

        A DocRef is dropped from the index — so its document is re-fetched —
        when either its file no longer exists on disk (pruned/deleted bronze
        self-heals) or ``freshness_days`` is set and its ``doc_date`` is within
        that window.
        """
        bronze_root = Path(bronze_root)
        now = now or date.today()
        exclude = Path(exclude_run).resolve() if exclude_run is not None else None
        fresh_delta = (timedelta(days=freshness_days)
                       if freshness_days is not None else None)

        index: dict[tuple, "deque[Path]"] = {}
        for run_dir in bronze.iter_run_dirs(bronze_root):
            if exclude is not None and run_dir.resolve() == exclude:
                continue
            if run_dir.is_symlink():
                continue
            state, _reason = prune.classify(run_dir, prune.LENIENT_CLASSIFY_CFG)
            if state != prune.COMPLETE:
                continue
            manifest = _read_manifest(run_dir)
            for ref in extract(run_dir, manifest):
                if (fresh_delta is not None and ref.doc_date is not None
                        and (now - ref.doc_date) < fresh_delta):
                    continue
                path = Path(ref.relpath)
                if not path.is_absolute():
                    path = run_dir / path
                # A key counts only if its file still exists (self-heal).
                try:
                    if not path.is_file() or path.is_symlink():
                        continue
                except OSError:
                    continue
                index.setdefault(ref.key, deque()).append(path)
        return cls(index)

    def take(self, key: tuple) -> Path | None:
        """Pop and return one prior file for ``key`` (absolute path), or None.

        Multiset-aware: N identical keys hand out N distinct prior files, once
        each; returns None once exhausted (or if a candidate vanished between
        derive and now — re-checked here). None always means "fetch".
        """
        dq = self._index.get(key)
        while dq:
            path = dq.popleft()
            try:
                if path.is_file() and not path.is_symlink():
                    return path
            except OSError:
                continue
        return None

    def __contains__(self, key: tuple) -> bool:
        return bool(self._index.get(key))

    def __len__(self) -> int:
        return sum(len(dq) for dq in self._index.values())


def _read_manifest(run_dir: Path, manifest_name: str = "run.json") -> dict | None:
    """Best-effort parse of a run's manifest for the ``extract`` hook; ``None``
    on any absence/read/parse error (the hook must tolerate that)."""
    path = run_dir / manifest_name
    try:
        meta = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    return meta if isinstance(meta, dict) else None


# ---------------------------------------------------------------------------
# The three modes
# ---------------------------------------------------------------------------

def _hardlink(src: Path, dst: Path) -> None:
    """Hardlink ``src`` -> ``dst`` inside the new run dir, creating parents.
    Raises OSError on any failure (cross-device, existing target, permissions),
    which every caller treats as "fall through to a real fetch"."""
    dst.parent.mkdir(parents=True, exist_ok=True)
    os.link(src, dst)


def _replace_with_hardlink(src: Path, target: Path) -> None:
    """Atomically replace an existing ``target`` file with a hardlink to
    ``src`` (a tmp sibling + ``os.replace``). On any failure ``target`` is left
    intact — the fetched bytes are never lost."""
    tmp = target.with_name(target.name + ".docdedup-tmp")
    try:
        tmp.unlink()
    except FileNotFoundError:
        pass
    try:
        os.link(src, tmp)
        os.replace(tmp, target)
    except BaseException:
        try:
            tmp.unlink()
        except FileNotFoundError:
            pass
        raise


def link_or_fetch(skipset: SkipSet, key: tuple, *, target_dir: Path, stem: str,
                  fetch: Callable[[], Path | None], force: bool = False,
                  expected_size: int | None = None,
                  usable: Callable[[Path], bool] | None = None) -> str:
    """link-mode: hardlink a prior identical file into the new run dir; fall
    through to a real fetch on ANY error (the viac invariant — degrade to a
    fetch, never to a missing document).

    ``fetch`` performs the collector's own download and returns the path it
    wrote (or ``None`` on failure). On a hit the linked file is named
    ``stem + prior.suffix`` — identical content yields an identical content-type
    and thus the prior's extension. Returns one of :data:`LINKED`,
    :data:`FETCHED`, :data:`FETCH_FAILED`.

    **link-mode correctness rests on collision-free keys** — a hit is trusted to
    be *the same logical document*, never merely the same label. equityzen's key
    is a unique ``sha256`` of the Relay id, so it holds by construction. For
    **label-keyed** adopters (whose human-readable key can repeat across
    accounts/periods) ``expected_size`` is the belt-and-braces: when set, the
    prior file's on-disk byte size must equal it before the link is trusted; a
    mismatch (or an un-stattable prior) is taken as a mis-keyed cross-account
    collision and falls through to a real fetch rather than silently linking the
    wrong file. ``None`` (equityzen) leaves the guard off.

    ``usable`` is the second guard, and it exists because link-mode is the one
    mode that can serve a file NO run ever validated: a fetch that once wrote
    the wrong bytes — an error page, an unfollowed url envelope — is hardlinked
    forward by every later run, and a mode that never re-fetches never notices.
    When given, the prior copy must satisfy it before the link is trusted; a
    copy that does not is treated exactly like a missing one and falls through
    to a real fetch. Cheap to satisfy (a magic-number read), and it turns a
    permanent poisoning into one wasted download.
    """
    if not force:
        prior = skipset.take(key)
        if (prior is not None and _size_ok(prior, expected_size)
                and _usable(prior, usable)):
            target = Path(target_dir) / (stem + prior.suffix)
            try:
                _hardlink(prior, target)
                return LINKED
            except OSError as e:
                log.debug("hardlink %s -> %s failed: %s; falling through to fetch.",
                          prior, target, e)
    return FETCHED if fetch() is not None else FETCH_FAILED


def is_pdf(path: Path) -> bool:
    """True if the file really starts with the PDF magic number.

    The ``usable`` predicate every adopter wants, because every adopter links
    PDFs: statements, notices, tax forms. It exists here rather than four times
    over because what it guards against is not a per-collector accident — a
    document endpoint that answers with an error page, or with a JSON envelope
    naming the real url, writes something that is not a PDF, and link-mode
    would hardlink that forward for ever.

    An unreadable file counts as unusable: the caller then fetches, which is
    the safe direction.
    """
    try:
        with path.open("rb") as f:
            return f.read(5) == b"%PDF-"
    except OSError:
        return False


def _usable(prior: Path, usable: Callable[[Path], bool] | None) -> bool:
    """True if the caller's usability predicate passes, or none was given. A
    predicate that raises counts as unusable -> the caller fetches, which is the
    safe direction and the same one :func:`_size_ok` takes on a stat error."""
    if usable is None:
        return True
    try:
        if usable(prior):
            return True
    except OSError:
        pass
    log.debug("usability guard: prior %s is not a usable document; "
              "fetching rather than linking it forward.", prior)
    return False


def _size_ok(prior: Path, expected_size: int | None) -> bool:
    """True if the size guard passes: no guard (``expected_size is None``) or the
    prior file's on-disk size equals ``expected_size``. A stat error counts as a
    mismatch → the caller fetches (the safe direction)."""
    if expected_size is None:
        return True
    try:
        if prior.stat().st_size == expected_size:
            return True
    except OSError:
        pass
    log.debug("size guard: prior %s does not match expected %r bytes; "
              "treating as a key collision and fetching.", prior, expected_size)
    return False


def fetch_verify_dedup(skipset: SkipSet, key: tuple, *,
                       fetch: Callable[[], Path | None],
                       digest_of: Callable[[Path], str] | None = None,
                       force: bool = False) -> str:
    """fetch-verify-dedup: ALWAYS fetch, then compare the fresh bytes to the
    prior run's copy for ``key``. On a byte-identical match discard the fetched
    bytes and hardlink the prior copy in (reclaims disk); on a difference keep
    the fetched bytes and report :data:`CHANGED` (the doc was re-issued); on a
    new doc keep the fetched bytes.

    The one mode correct against a silent server-side re-issue, so the mandatory
    default for tax/mutable documents. Any hiccup in the verify/relink keeps the
    freshly-fetched bytes (a load input is never lost). Returns one of
    :data:`VERIFIED`, :data:`CHANGED`, :data:`FETCHED`, :data:`FETCH_FAILED`.
    """
    fetched = fetch()
    if fetched is None:
        return FETCH_FAILED
    fetched = Path(fetched)
    if force:
        return FETCHED
    prior = skipset.take(key)
    if prior is None:
        return FETCHED
    digest = digest_of or (lambda p: bronze.sha256_file(p)[0])
    try:
        if digest(fetched) == digest(prior):
            _replace_with_hardlink(prior, fetched)
            return VERIFIED
        return CHANGED
    except OSError as e:
        log.debug("fetch-verify for %r hiccuped (%s); keeping fetched bytes.",
                  key, e)
        return FETCHED


def process(skipset: SkipSet, key: tuple, *, doc_class: str | None,
            target_dir: Path, stem: str, fetch: Callable[[], Path | None],
            digest_of: Callable[[Path], str] | None = None,
            force: bool = False, expected_size: int | None = None,
            class_modes: dict[str, str] | None = None,
            usable: Callable[[Path], bool] | None = None) -> str:
    """Dispatch one document to the mode its class selects (:func:`mode_for_class`).

    ``immutable -> link_or_fetch``; ``tax``/``mutable`` and any unknown class
    ``-> fetch_verify_dedup`` (always fetch, dedup a byte-identical copy).
    ``force`` bypasses the index (always fetch; for fetch-verify, keep the fresh
    bytes without deduping). ``expected_size`` is passed to link-mode's collision
    guard, and ``usable`` to its usability guard (both inert for fetch-verify,
    which re-reads the bytes anyway). Returns the per-document outcome code.
    """
    mode = mode_for_class(doc_class, class_modes)
    if mode == MODE_LINK:
        return link_or_fetch(skipset, key, target_dir=target_dir, stem=stem,
                             fetch=fetch, force=force,
                             expected_size=expected_size, usable=usable)
    # fetch-verify — the default for tax / mutable / unknown classes.
    return fetch_verify_dedup(skipset, key, fetch=fetch,
                              digest_of=digest_of, force=force)
