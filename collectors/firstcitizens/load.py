#!/usr/bin/env python3
"""firstcitizens silver loader — Phase 3 (not built yet).

The Phase 2 scaffold ships `login` + `download` (bronze); parsing bronze
into the SQLite silver is Phase 3 (DESIGN.md §4.3). The shape is known and
recorded there: the `history/<id>.json` accountHistory payload already
carries a stable `transactionId` **and** a `runningBalance`, so — unlike
chase — a CSV↔QFX join is likely unnecessary; the exports under
`transactions/` are captured alongside for comparison before the parser is
chosen. This stub keeps the `load` verb wired (an orchestrator's
login→download→load never trips) and fails loudly rather than pretending to
build silver.
"""
from __future__ import annotations

import sys


def main(argv: list[str] | None = None) -> int:
    print("firstcitizens: `load` is not built yet — Phase 3 (bronze → SQLite "
          "silver). See DESIGN.md §4.3. `download` writes the bronze it will "
          "consume.", file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
