"""The venture SPA's GraphQL operations, by the names this collector keys
on. The portal spells an operation with or without a ``Query`` suffix
(``PositionsTableQuery`` and ``PositionsTable`` are the same query), so
every comparison goes through :func:`op_name`, which drops the suffix;
bronze keeps the name as captured.
"""
from __future__ import annotations

# Ops wanted per invest account, and the route that fires them.
PORTFOLIO_OPS = {
    "PortfolioDashboard", "PositionsTable", "Activity",
    "PositionFilters", "InvestAccountInvestmentEntities",
}
COMMITMENT_OPS = {"OpenInvestments"}
DOCS_OPS = {"AccountDocuments"}
FUNDING_OPS = {"InvestmentEntity"}
VIEWER_OP = "Viewer"


def op_name(raw) -> str:
    """The operation's name without the portal's optional ``Query`` suffix;
    empty for a missing name."""
    name = raw or ""
    return name[:-5] if name.endswith("Query") else name
