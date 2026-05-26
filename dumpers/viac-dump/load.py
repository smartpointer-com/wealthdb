#!/usr/bin/env python3
"""
viac-dump Phase 4: silver loader (STUB).

This file is a placeholder so the Dockerfile build succeeds before
the Phase 4 implementation lands. The real load.py will:

  1. Apply pending schema migrations under migrations/ to the
     silver SQLite DB.
  2. Walk <bronze-dir> for subdirectories matching the
     YYYYMMDDTHHMMSSZ dump-timestamp format; skip those already
     recorded in dump_runs.
  3. Per dump: parse positions / transactions / documents JSON
     (and HTML fallbacks), upsert into the silver tables, record
     in dump_runs. Wrapped in a single transaction per dump for
     atomicity.
  4. Walk <bronze-dir>/manual/ on every run, dedupe by sha256,
     index user-uploaded artefacts.

See DESIGN.md \xa78 for the silver-schema sketch (under-specified
until Phase 1 reveals the actual JSON shape).
"""

from __future__ import annotations

import sys


def main() -> int:
    sys.stderr.write(
        "load.py: Phase 4 (silver loader) is not yet implemented. "
        "See DESIGN.md \xa78 for the schema sketch.\n"
    )
    return 2


if __name__ == "__main__":
    sys.exit(main())
