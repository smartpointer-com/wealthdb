"""The schwab-web collector's declaration (see collectorkit.spec)."""
from collectorkit.spec import Collector

SPEC = Collector(
    name="schwab-web",
    env_prefix="SCHWAB_WEB",
    session="profile",
    unattended=False,
    window="bounded",
    # Per-account landing-page HTML beside the tx-history exports. `load`
    # never reads it; this glob matches ONLY files named page-*.html two
    # levels deep, never a load-input sibling (all of which are
    # more-details.json or .csv/.json/.xml exports).
    debug_globs=("transactions/*/page-*.html",),
)
