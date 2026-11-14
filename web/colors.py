"""Each source's colour on the charts that draw a series per source, and
the file that keeps the assignment. Part of provision.py."""

import contextlib
import json
import os
import sys

from sqlkit import (ALL_SOURCES_SERIES, DECLARED_SOURCE)


# A source's colour. Metabase colours a series by its place, and changes
# palette between eight series and nine, so a picked benchmark or a
# narrowed Source picker would recolour every line. A card that draws a
# series per source (SOURCE_SERIES_COLUMNS) therefore names each source's
# colour in its series_settings, which key on the series' label: the
# source id. The ids are gold's, so provisioning reads them from the
# snapshot, and the file --source-colors names keeps the assignment
# (source_colors). Twenty-four colours, so no two of a household's first
# 24 sources share one. Each reads at 3:1 or better on a light and on a
# dark theme. The order is farthest first: each slot is the colour
# farthest from every slot before it, under normal vision and under
# simulated red-green colour blindness, so a household with few sources
# gets the strongest contrast. Past 24 sources a slot is shared.
SOURCE_PALETTE = ("#89579e", "#fd1a06", "#1955f9", "#238c0a", "#987aff", "#cf2cc8",
                  "#bf6c5a", "#c8105c", "#197ece", "#289b7d", "#bf5ea4", "#2b7440",
                  "#fd1f80", "#6990e2", "#5d6ff5", "#a481c1", "#972de1", "#9c5132",
                  "#1089a0", "#b88848", "#5b65b9", "#a4476d", "#af55df", "#648746")

ALL_SOURCES_COLOR = "#6b7785"

# The breakout columns that hold a source id.
SOURCE_SERIES_COLUMNS = ("silver_source_id", "source")


def assign_source_colors(kept, sources):
    """Each of `sources` with a colour, first come, first served: a
    source in `kept` keeps its colour, and a new one takes the first slot
    of SOURCE_PALETTE no source holds, in the order `sources` lists them.
    So a source keeps its colour on every load, and a new source never
    recolours another. With every slot held, a new one takes the slot
    the fewest hold, the first of those. `kept` entries for sources gone
    from gold stay, so their slot stays theirs."""
    out = {k: v for k, v in kept.items() if v in SOURCE_PALETTE}
    for src in sources:
        if src in out:
            continue
        held = list(out.values())
        out[src] = min(SOURCE_PALETTE, key=lambda c: (held.count(c), SOURCE_PALETTE.index(c)))
    return out


def load_source_colors(path):
    """The kept assignment in `path`, or none when it is absent or not
    an object of strings."""
    try:
        with open(path) as f:
            kept = json.load(f)
    except (OSError, ValueError):
        return {}
    if not isinstance(kept, dict):
        return {}
    return {k: v for k, v in kept.items() if isinstance(k, str) and isinstance(v, str)}


def save_source_colors(path, colors):
    """Write the assignment to `path` whole, through a file beside it."""
    tmp = path + ".tmp"
    try:
        with open(tmp, "w") as f:
            json.dump(colors, f, indent=2, sort_keys=True)
            f.write("\n")
        os.replace(tmp, path)
    except OSError as e:
        with contextlib.suppress(OSError):
            os.remove(tmp)
        print(f"provision: could not save {path}: {e}", file=sys.stderr)


def account_source_colors(kept, sources):
    """assign_source_colors over `sources`, the snapshot's sources that
    hold an account, then DECLARED_SOURCE. None, a probe that did not
    answer, adds no source."""
    return assign_source_colors(kept, [] if sources is None else [*sources, DECLARED_SOURCE])


def with_source_colors(viz, colors):
    """`viz` with each source's colour, and the fixed colour of the
    '(all sources)' line, where its breakout is a source column. A
    setting the card names for a series already wins over the colour, so
    the benchmark keeps its own. Any other card comes back unchanged."""
    dims = viz.get("graph.dimensions") or []
    if len(dims) != 2 or dims[1] not in SOURCE_SERIES_COLUMNS:
        return viz
    settings = {src: {"color": c} for src, c in colors.items()}
    settings[ALL_SOURCES_SERIES] = {"color": ALL_SOURCES_COLOR}
    for key, value in (viz.get("series_settings") or {}).items():
        settings[key] = {**settings.get(key, {}), **value}
    return {**viz, "series_settings": settings}
