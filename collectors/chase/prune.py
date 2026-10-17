#!/usr/bin/env python3
"""Prune non-complete dumps from the chase bronze tree.

Thin wrapper over :mod:`collectorkit.prune` (the shared, unit-tested prune
engine) with chase's configuration. It reclaims:

* whole run dirs that are not complete dumps — ``run.json`` is missing (a
  crashed walk) or its ``status`` is anything other than ``"complete"`` (an
  ``"in-progress"`` marker or a ``"dry-run"`` shell). ``load`` skips such
  dirs, so removing one never changes silver.
* the ``raw/svc/*.json`` bodies of a complete dump. With ``--debug``,
  ``download`` keeps every ``/svc/`` JSON body other than the roster's under
  ``raw/svc/``; ``load`` never reads ``raw/``.

A COMPLETE dump's load inputs (``accounts.json``, ``transactions/*.{csv,qfx}``,
``statements/*/*.pdf``) are structurally protected: the engine only ever
touches whole non-complete run dirs and the configured debug globs. The
roster bodies sit at the top of ``raw/`` (``raw/account-NN.json``), so the
glob never reaches them. The explore and login captures land in ``/debug``
(outside bronze), which ``prune --debug-dir`` reclaims.

Completeness signal: the ``run.json`` ``status`` field. A statusless but
readable manifest is COMPLETE, since a download that writes no status
writes its manifest only at the end; an unreadable / corrupt manifest is
UNKNOWN and never deleted. An in-flight guard skips non-complete dumps
written within ``--min-age-hours`` (default 1).

Usage:
    prune.py [--bronze-dir /data] [--debug-dir DIR] [--dry-run] [--min-age-hours N]
"""
from __future__ import annotations

import sys

from collectorkit import prune


CONFIG = prune.PruneConfig(
    # `download --debug` keeps every non-roster /svc/ body here; the roster
    # bodies are raw/account-NN.json, one level up, out of the glob's reach.
    debug_globs=("raw/svc/*.json",),
    is_complete=prune.lenient_classification,
)


def main(argv=None):
    return prune.main(CONFIG, argv, description=__doc__)


if __name__ == "__main__":
    sys.exit(main())
