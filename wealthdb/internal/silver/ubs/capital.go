package ubs

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// capitalAccounts are the private-markets funds' capital accounts dated
// in w, from their quarterly reports (private_market_reports): one per
// report, named by its document token. The investor's net commitment,
// the capital called and the valuation (the NAV) pass as printed, in
// the report's currency; a report that prints no currency is none. A
// report states no distributions and no multiple, and names the fund by
// its ISIN alone, which is the fund's instrument id: it names no
// portfolio, so a fund two portfolios hold can have two reports on one
// day, and each is kept.
func (r *webReader) capitalAccounts(ctx context.Context, w canonical.Window) ([]canonical.CapitalAccountChange, error) {
	if !w.HasChanges {
		return nil, nil
	}
	ok, err := r.hasTable(ctx, reportsTable)
	if err != nil || !ok {
		return nil, err
	}
	out, err := silver.Query(ctx, r.db, "", `
SELECT instrument_isin, as_of, source_doc_token, currency_iso, commitment, capital_called, valuation
  FROM `+reportsTable+`
 WHERE as_of BETWEEN ? AND ? AND currency_iso IS NOT NULL AND currency_iso <> ''
   AND (commitment IS NOT NULL OR capital_called IS NOT NULL OR valuation IS NOT NULL)
 ORDER BY instrument_isin, as_of, source_doc_token`, func(rows *sql.Rows) (canonical.CapitalAccountChange, error) {
		var (
			ca                        canonical.CapitalAccountChange
			commitment, called, value sql.NullFloat64
		)
		err := rows.Scan(&ca.InstrumentExternalID, &ca.AsOf, &ca.SourceRef, &ca.Currency, &commitment, &called,
			&value)
		ca.Commitment, ca.Called = silver.DecimalPtrFromNullFloat(commitment), silver.DecimalPtrFromNullFloat(called)
		ca.NAV = silver.DecimalPtrFromNullFloat(value)
		return ca, err
	}, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("ubs-web capital accounts: %w", err)
	}
	return out, nil
}
