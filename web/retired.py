"""The names of the models, cards and dashboards provisioning archives:
retired by renames and removals. Part of provision.py."""


# Model names retired by renames and removals, each group noted where it
# is listed; archived on provision so a re-run cleans them up.
RETIRED_MODEL_NAMES = [
                       # Un-suffixed snapshot reports -> `_latest`
                       "report_global", "report_portfolios",
                       "report_accounts", "report_positions",
                       # Per-widget flow models: the privacy charts read
                       # the serving views natively.
                       "report_income_monthly_pct", "report_costs_monthly_pct",
                       # Privacy variants no card, picker or drill-through
                       # read: the privacy charts are native SQL over the
                       # web_* serving views.
                       "report_sources_history_pct", "report_accounts_history_pct",
                       "report_positions_history_pct", "report_transactions_pct",
                       "report_asset_classes_history_pct", "report_vehicles_history_pct",
                       # The privacy models of the MBQL cards: every card
                       # is native SQL over gold.
                       "report_returns_redacted", "report_sources_latest_pct", "report_spending_pct"]

# Card names retired by renames and removals, each group noted where it
# is listed; archived on provision so a re-run cleans them up.
RETIRED_CARD_NAMES = [
                      # Identifier-style metric names -> prose names
                      "net_worth_usd_current", "net_worth_chf_current",
                      "net_worth_eur_current", "positions_value_usd_current",
                      "cash_balance_usd_current", "net_worth_usd_daily",
                      # Top 10 -> Top 100 (with the inline asset-class filter)
                      "Top 10 positions (USD)", "Top 10 positions (% of peak)",
                      # Returns rework: start-year rescoping + growth chart +
                      # global-as-pseudo-source. Renamed/dropped cards and the
                      # privacy twins the percentage-only cards no longer need.
                      "Return since inception (TWR)", "Return since inception (MWR)",
                      "Returns by source since inception",
                      "Quarterly returns by source (TWR)",
                      "Return since inception (TWR) (privacy)",
                      "Return since inception (MWR) (privacy)",
                      "Annualized return (TWR) (privacy)",
                      "Quarterly returns (TWR) (privacy)",
                      "Monthly returns (TWR) (privacy)",
                      "Annual returns (TWR) (privacy)",
                      "Returns by source since inception (privacy)",
                      "Quarterly returns by source (TWR) (privacy)",
                      # "% of peak" -> "(privacy)": the privacy cards'
                      # normalization moved from a fixed %-of-all-time-peak
                      # to selection-aware shares of the latest total.
                      "Net worth (% of peak)", "Positions value (% of peak)",
                      "Cash balance (% of peak)",
                      "Net worth — monthly trend (% of peak)",
                      "Net worth over time (% of peak)",
                      "Cash vs positions over time (% of peak)",
                      "Income by month (% of peak)",
                      "Fees & taxes by month (% of peak)",
                      "Allocation by asset class (% of peak)",
                      "Allocation by vehicle (% of peak)",
                      "Allocation by currency (% of peak)",
                      "Value by tax wrapper (% of peak)",
                      "Value by management style (% of peak)",
                      "Top 100 positions (% of peak)",
                      "Source freshness (% of peak)",
                      # "Income by month (USD)" -> "Investment income by
                      # month (USD)": the Wealth Overview's tile is named
                      # for the four types it charts.
                      "Income by month (USD)",
                      # The log axis Metabase draws is a linear axis over
                      # log values, so its ticks fall between powers of ten.
                      "Cumulative return (log scale)",
                      # The Wealth Overview and Allocation tiles take a
                      # Currency picker, so their names drop the USD
                      # marker; the per-currency metrics go with them.
                      "Net worth (USD)", "Net worth (CHF)", "Net worth (EUR)",
                      "Positions value (USD)", "Cash balance (USD)",
                      "Net worth — monthly trend (USD)",
                      "Net worth over time (USD)",
                      "Cash vs positions over time (USD)",
                      "Investment income by month (USD)",
                      "Fees & taxes by month (USD)",
                      "Allocation by asset class (USD)",
                      "Allocation by vehicle (USD)",
                      "Allocation by currency (USD)",
                      "Value by tax wrapper (USD)",
                      "Value by management style (USD)",
                      "Top 100 positions (USD)",
                      # The Gains dashboard splits its gain by what was
                      # sold and what was kept.
                      "Unrealized change",
                      # The other Exposure charts over time show shares
                      # alone, so the twin reuses them.
                      "Asset classes over time (privacy)",
                      "Held directly or through funds (privacy)",
                      "How the figures are classified (privacy)",
                      "Regions over time (privacy)",
                      # The Income dashboard's received and expected cards
                      # by month and by account, which follow the Time
                      # range, supersede the income lines' two.
                      "Income by month", "Income by month (privacy)",
                      "Income by account", "Income by account (privacy)",
                      # The Exposure debt and cycle cards are named for
                      # the class they count. The maturity card's name
                      # says the debt alone: it counts the debt and the
                      # cash, each by its term.
                      "Exposure by credit rating", "Exposure by credit rating (privacy)",
                      "Exposure by maturity", "Exposure by maturity (privacy)",
                      "Exposure by coupon", "Exposure by coupon (privacy)",
                      # The cash flow projection's cards are named for
                      # what they show: a projection, not a plan.
                      "Cash flow plan by month", "Cash flow plan by month (privacy)",
                      "Cash flow plan", "Cash flow plan (privacy)",
                      # The issuers card lists a hundred on Allocation, as
                      # the positions card beside it does.
                      "Largest issuers", "Largest issuers (privacy)",
                      # The Wealth Overview shows a figure from each
                      # dashboard: the income Income's, the fees and
                      # taxes Cash Flow's operating outflows.
                      "Investment income by month", "Investment income by month (privacy)",
                      "Fees & taxes by month", "Fees & taxes by month (privacy)",
                      # The asset classes over time, below the net worth
                      # chart, show the mix the cash and positions chart
                      # and the one-day split showed.
                      "Cash vs positions over time", "Cash vs positions over time (privacy)",
                      "Asset class split", "Asset class split (privacy)",
                      # The Income dashboard's cards of received and
                      # expected income count every income line; the
                      # twin's yield is the holdings' and says so.
                      "Income in the window (privacy)",
                      "Received income", "Received income (privacy)",
                      "Expected remaining", "Expected remaining (privacy)",
                      "Holdings' income in the window", "Holdings' income in the window (privacy)",
                      "Holdings' income received", "Holdings' income received (privacy)",
                      "Holdings' income expected", "Holdings' income expected (privacy)",
                      "Holdings' monthly average income", "Holdings' monthly average income (privacy)",
                      "Received interest", "Received interest (privacy)",
                      "Received dividends", "Received dividends (privacy)",
                      "Expected interest remaining", "Expected interest remaining (privacy)",
                      "Expected dividends remaining", "Expected dividends remaining (privacy)"]

# Dashboard names retired by renames; archived on provision so a re-run
# cleans them up.
RETIRED_DASHBOARD_NAMES = ["Net Worth"]
