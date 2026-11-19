"""The Private Markets dashboard's cards and its twin's. Part of provision.py."""

from sqlkit import (native_tags, STACKED_VIZ)


# The multiples the chart stacks, in stacking order, and their colours:
# what came back, then what is still marked, which add up to TVPI.
MULTIPLE_COLORS = {"dpi": "#2A78D6", "rvpi": "#8A99A8"}

MULTIPLE_TITLES = {"dpi": "Distributed (DPI)", "rvpi": "Still held (NAV / paid in)"}

PRIVATE_PRIVACY_DESC = " Money columns and account names are redacted."


def private_cards(f):
    """The Private Markets dashboard's cards, or on the twin the privacy
    variant of its table. They read `wealthdb returns private` over
    web_private: one row per private holding, each a fund or SPV a
    capital call names, then the total, one row set per reporting
    currency. The Currency picker lands on the {{currency}} variable, and
    the dashboard takes no other: the total is the CLI's, over every
    holding, so a Source picker would leave it out of step with the rows
    shown.

    The table's twin keeps the multiples and the marks' dates and drops
    the money and the account, as the Returns twin drops its money
    columns. A holding's name stays: privacy hides money and accounts,
    never what is held. The chart shows multiples only, so the twin shows
    it as it is."""
    tags = native_tags("web_private", {})
    base = ("SELECT * FROM web_private\n WHERE currency = {{currency}}")
    if f.privacy:
        desc = "Each private fund or SPV since its first call: its multiples and the date of its mark."
        cols = "holding, silver_source_id, first_call, marked_at, dpi, tvpi, quality"
    else:
        desc = "Each private fund or SPV since its first call: what it took, gave back and is marked at."
        cols = "holding, account_label, first_call, paid_in, distributed, nav, marked_at, dpi, tvpi, quality"
    card = f.using(tags=tags, tail=PRIVATE_PRIVACY_DESC if f.privacy else "").card
    card("Private holdings", "table",
         desc + " The total comes last. A call after the mark reads TVPI low.",
         f"WITH p AS (\n  {base})\nSELECT {cols}\n  FROM p\n ORDER BY is_total, first_call, silver_source_id, holding",
         {"column_settings": {'["name","dpi"]': {"decimals": 2}, '["name","tvpi"]': {"decimals": 2}}})
    card("Multiples by holding", "bar",
         "What each holding gave back (DPI) and what it is still marked at, per unit paid in. Together they "
         "are its TVPI.",
         f"WITH p AS (\n  {base}\n   AND NOT is_total)\n"
         f"SELECT holding || ' (' || silver_source_id || ')' AS holding, dpi,\n"
         "       tvpi - dpi AS rvpi\n  FROM p\n ORDER BY tvpi DESC NULLS LAST, holding",
         {"graph.dimensions": ["holding"], "graph.metrics": list(MULTIPLE_COLORS), **STACKED_VIZ,
          "graph.x_axis.title_text": "", "graph.y_axis.title_text": "",
          "series_settings": {c: {"color": MULTIPLE_COLORS[c], "title": MULTIPLE_TITLES[c]}
                              for c in MULTIPLE_COLORS}},
         exempt=True)
