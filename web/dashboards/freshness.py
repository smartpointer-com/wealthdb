"""The Data Freshness dashboard's cards and its twin's. Part of provision.py."""

import sqlkit
from sqlkit import (BENCHMARK_COLOR, DECLARED_SOURCE, PRIVACY_DESC, _share_viz)


def freshness_cards(f):
    """The Data Freshness dashboard's cards, or on the twin its one
    privacy card. The dashboard takes no picker. Its scalars and its Mark
    age, which reads `wealthdb status --marks` (web_marks), show days and
    shares, so the twin shows them as they are. A source is as stale as
    its last read, not its latest mark: a portal that marks quarterly
    and was read today is current, and the days since its latest mark
    are days it has been quiet. The mark, not the snapshot: a read that
    found a holding gone dates a snapshot at the read while the marks
    keep their dates. The declared accounts (config `declared_accounts`)
    sit under their own reserved source, which has no feed and so no
    snapshot: counted, it would be the stalest source every day, so it is
    left out. The freshness table's twin shows each value as a share of
    the latest net worth: the dashboard is unfiltered by design, so that
    scale IS the selected-sources scale, and the source rows total 100.
    It has a description of its own: the base card's promises a money
    column, which there holds shares."""
    f = f.using(tail=PRIVACY_DESC if f.privacy else "")
    # Each source's last read (gold's source_observed), unknown reading
    # as its newest snapshot, as report_sources_latest carries it, and
    # the days since, counted in calendar days as Metabase's
    # datetime-diff counts them.
    f.card("Stalest source (days)", "scalar",
           "Days since the source read longest ago was last read. A source "
           "that marks quarterly counts from its latest read.",
           "WITH s AS (\n"
           "  SELECT datesub('day', CAST(observed_at AS DATE), CAST(now() AS DATE)) AS days_stale\n"
           "    FROM (SELECT r.silver_source_id,\n"
           "                 epoch_ms(GREATEST(COALESCE(o.observed_snapshots_through, r.snapshot_at),"
           " r.snapshot_at) * 1000) AS observed_at\n"
           "            FROM serving_sources_latest r\n"
           "            LEFT JOIN source_observed o USING (silver_source_id))\n"
           f"   WHERE silver_source_id <> '{DECLARED_SOURCE}')\n"
           "SELECT max(days_stale) AS max FROM s",
           {}, tags={}, exempt=True, home="Data Freshness")
    f.card("Returns age (days)", "scalar",
           "Days since a `web refresh` last computed the returns.",
           # Every row of a run shares one computed_at, so max(age) is
           # the run's age, in calendar days as Metabase's datetime-diff
           # counts them, over the sources and global grains.
           "SELECT max(datesub('day', CAST(epoch_ms(computed_at * 1000) AS DATE), CAST(now() AS DATE)))"
           " AS max\n  FROM report_returns\n WHERE grain IN ('sources', 'global')",
           {}, tags={}, exempt=True)
    if f.privacy:
        # The value as a share of the latest whole-portfolio net worth in
        # its currency, blank where that is zero or below: the dashboard
        # takes no picker, so the source rows total 100.
        share = f"total_value_{sqlkit.DEFAULT_CURRENCY.lower()}"
        f.card("Source freshness", "table",
               "Each source's last read and latest mark, the days since each, and its share of the latest total.",
               "WITH s AS (\n"
               "  SELECT r.silver_source_id,\n"
               "         epoch_ms(GREATEST(COALESCE(o.observed_snapshots_through, r.snapshot_at),"
               " r.snapshot_at) * 1000) AS observed_at,\n"
               f"         epoch_ms(r.valued_at * 1000) AS valued_at, r.{share} / nw.nw * 100 AS {share}\n"
               "    FROM serving_sources_latest r\n"
               "    LEFT JOIN source_observed o USING (silver_source_id),\n"
               f"         (SELECT CASE WHEN {share} > 0 THEN {share} END AS nw FROM serving_global_latest) nw\n"
               f"   WHERE r.silver_source_id <> '{DECLARED_SOURCE}')\n"
               "SELECT silver_source_id, observed_at,\n"
               "       datesub('day', CAST(observed_at AS DATE), CAST(now() AS DATE)) AS days_stale,\n"
               "       valued_at,\n"
               "       datesub('day', CAST(valued_at AS DATE), CAST(observed_at AS DATE)) AS days_quiet,\n"
               f"       {share}\n"
               "  FROM s\n ORDER BY days_stale DESC, silver_source_id",
               {}, tags={})
    else:
        # Each fed source's last read (gold's source_observed), unknown
        # reading as its newest snapshot, its latest mark, the days since
        # each in calendar days as Metabase's datetime-diff counts them,
        # and the value it holds, stalest first.
        value = f"total_value_{sqlkit.DEFAULT_CURRENCY.lower()}"
        f.card("Source freshness", "table",
               "Each source's last read and latest mark, the days since each, "
               f"and the value it holds in {sqlkit.DEFAULT_CURRENCY}.",
               "WITH s AS (\n"
               "  SELECT r.silver_source_id,\n"
               "         epoch_ms(GREATEST(COALESCE(o.observed_snapshots_through, r.snapshot_at),"
               " r.snapshot_at) * 1000) AS observed_at,\n"
               f"         epoch_ms(r.valued_at * 1000) AS valued_at, r.{value}\n"
               "    FROM serving_sources_latest r\n"
               "    LEFT JOIN source_observed o USING (silver_source_id)\n"
               f"   WHERE r.silver_source_id <> '{DECLARED_SOURCE}')\n"
               "SELECT silver_source_id, observed_at,\n"
               "       datesub('day', CAST(observed_at AS DATE), CAST(now() AS DATE)) AS days_stale,\n"
               "       valued_at,\n"
               "       datesub('day', CAST(valued_at AS DATE), CAST(observed_at AS DATE)) AS days_quiet,\n"
               f"       {value}\n"
               "  FROM s\n ORDER BY days_stale DESC, silver_source_id",
               {}, tags={})
    f.card("Mark age", "bar",
           "The share of value marked within five days of each month end: how far that month's return can "
           "be trusted. Liabilities and cash are out.",
           "SELECT CAST(date_trunc('month', month_end) AS TIMESTAMP) AS month, marked_pct\n"
           "  FROM web_marks\n ORDER BY month",
           {"graph.dimensions": ["month"], "graph.metrics": ["marked_pct"],
            "graph.y_axis.auto_range": False, "graph.y_axis.min": 0, "graph.y_axis.max": 100,
            "graph.x_axis.title_text": "", "graph.y_axis.title_text": "",
            "series_settings": {"marked_pct": {"color": BENCHMARK_COLOR}},
            **_share_viz("marked_pct")},
           tags={}, exempt=True)
