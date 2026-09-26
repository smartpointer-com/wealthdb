"""The standalone command line of a statement parser.

A parser module runs on its own over real documents — no load around
it — to show what it reads: it parses the PDFs named on the command line
and prints the results as one JSON array. Each parser supplies the
function it runs and a description; the arguments and the output are the
same everywhere.

The parser imports this module inside its ``__main__`` block, which still
puts it in the parser's fingerprint (``srcfp`` follows every import), so it
stays small and changes rarely.
"""
from __future__ import annotations

import argparse
import json
from collections.abc import Callable, Sequence
from typing import Any


def dump_json(
    argv: Sequence[str],
    parse: Callable[..., Any],
    *,
    description: str,
    add_arguments: Callable[[argparse.ArgumentParser], None] | None = None,
    parse_kwargs: Callable[[argparse.Namespace], dict] | None = None,
) -> int:
    """Parse each PDF in `argv` with `parse` and write the results as JSON.

    Positional arguments are the PDF paths; ``--json-out PATH`` writes the
    array to a file instead of stdout. `add_arguments` adds a parser's own
    options (listed after the paths), and `parse_kwargs` turns the parsed
    options into keyword arguments for `parse`. Returns the exit status.
    """
    p = argparse.ArgumentParser(description=description)
    p.add_argument("pdf", nargs="+", help="One or more PDF paths.")
    if add_arguments is not None:
        add_arguments(p)
    p.add_argument(
        "--json-out", default="-",
        help="Output path for the JSON array (default: stdout).",
    )
    args = p.parse_args(argv)
    kwargs = parse_kwargs(args) if parse_kwargs is not None else {}
    out = [parse(path, **kwargs) for path in args.pdf]
    blob = json.dumps(out, indent=2, ensure_ascii=False, default=str)
    if args.json_out == "-":
        print(blob)
    else:
        with open(args.json_out, "w", encoding="utf-8") as fh:
            fh.write(blob)
    return 0
