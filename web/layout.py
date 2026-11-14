"""A dashboard's declaration and its grid: rows, stacks, headings and a
twin's own rows, laid out on Metabase's 24-column grid. Part of
provision.py."""

from typing import NamedTuple


class Heading(NamedTuple):
    """A full-width row of text, one high: a section's heading, or with
    `display` "text" a chart's subtitle."""
    text: str
    display: str = "heading"


class Stack(NamedTuple):
    """A column `width` wide of `cards` one above the other, sharing the
    height of the row it stands in."""
    width: int
    cards: tuple


class Row(NamedTuple):
    """Tiles side by side, `height` high, from the left: each a (card,
    width) pair, with a third element the card the twin shows there in
    its place, or a Stack."""
    height: int
    tiles: tuple


class Twin(NamedTuple):
    """Rows the base dashboard lays out, `base`, and the twin lays out in
    their place, `twin`."""
    base: tuple
    twin: tuple


class Dashboard(NamedTuple):
    """One pre-defined dashboard and its privacy twin. `twin_desc` is the
    twin's blurb, naming what its figures are shares of; None for a
    dashboard with no twin. `pickers` are the slugs of its pickers in
    their order (PICKERS), `twin_drops` those the twin leaves out.
    `rows` lay its tiles out (layout). `redacts`: the twin redacts money
    columns where the others show shares, and the switch link of both
    says so."""
    name: str
    desc: str
    twin_desc: str
    pickers: tuple
    twin_drops: tuple
    rows: tuple
    redacts: bool = False


def layout(rows, twin=False):
    """The tiles and headings of `rows` on Metabase's 24-column grid, on
    the twin with `twin`: (tiles, headings), a tile (card, row, col,
    size_x, size_y, twin card), a heading (row, text, display). Each row
    starts below the one before it, each tile right of the one before."""
    tiles, headings, top = [], [], 0
    for item in rows:
        if isinstance(item, Twin):
            sub_tiles, sub_headings = layout(item.twin if twin else item.base, twin)
            tiles += [(c, r + top, col, w, h, t) for c, r, col, w, h, t in sub_tiles]
            headings += [(r + top, text, d) for r, text, d in sub_headings]
            top += max([r + h for _c, r, _col, _w, h, _t in sub_tiles] +
                       [r + 1 for r, _text, _d in sub_headings], default=0)
            continue
        if isinstance(item, Heading):
            headings.append((top, item.text, item.display))
            top += 1
            continue
        col = 0
        for tile in item.tiles:
            if isinstance(tile, Stack):
                h = item.height // len(tile.cards)
                tiles += [(c, top + k * h, col, tile.width, h, c) for k, c in enumerate(tile.cards)]
                col += tile.width
            else:
                card, width, *shown = tile
                tiles.append((card, top, col, width, item.height, shown[0] if shown else card))
                col += width
        top += item.height
    return tiles, headings


# The sentence every pre-defined dashboard's blurb ends with.
PREDEFINED_NOTE = "Pre-defined; duplicate it before customizing."


class Layout(NamedTuple):
    """One dashboard as dashboard_defs lays it out: its blurb, the slugs
    of its pickers, the name of its twin or base (None for a dashboard
    with no twin), its tiles (card, row, col, size_x, size_y) and its
    headings (row, text, display), and whether its twin redacts
    (Dashboard.redacts)."""
    desc: str
    pickers: tuple
    sibling: str
    tiles: list
    headings: list
    redacts: bool
