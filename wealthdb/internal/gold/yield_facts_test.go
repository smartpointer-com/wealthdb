package gold

import (
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// YieldSecurities lists exposure's securities with the book's figures:
// a stock held only through a fund reads its line's price and its
// reported earnings over it; the reported figures are read for equities
// alone, so a bond line's stated earnings count for nothing. The book is
// invented.
func TestYieldSecurities(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	snap := ymd(2026, 6, 30).Unix()
	q := []string{
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
		    first_seen_at, last_seen_at) VALUES ('src', 'B', 'brokerage', 'Brokerage', 'USD', 0, 0)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, symbol, name, currency,
		    first_seen_at, last_seen_at) VALUES ('src', 'QMIX', 'multi_asset', 'etf', 'QMIX', 'Example Mix Fund', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		    asset_class, vehicle, currency, quantity, market_value)
		    VALUES ('src', ` + itoa(snap) + `, 'B', 'QMIX', 'QMIX', 'multi_asset', 'etf', 'USD', 10, 1000)`,
		`INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, line_count, currency)
		    VALUES ('ref', 'symbol:QMIX', ` + itoa(snap) + `, 'holdings', 'testprov', 2, 'USD')`,
		`INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, name, asset_type, weight,
		    currency, quantity, market_value, maturity, coupon) VALUES
		    ('ref', 'symbol:QMIX', ` + itoa(snap) + `, 'holdings', 1, 'symbol:QSTX', 'Example Stock Co', 'Equity', 60,
		     'USD', 10, 600, NULL, NULL),
		    ('ref', 'symbol:QMIX', ` + itoa(snap) + `, 'holdings', 2, 'cusip:00000000Q', 'EXAMPLE NOTE', 'Fixed Income', 40,
		     'USD', 400, 400, DATE '2030-01-15', 4)`,
	}
	for _, key := range []string{"symbol:QSTX", "cusip:00000000Q"} {
		for i, end := range []time.Time{ymd(2025, 9, 30), ymd(2025, 12, 31), ymd(2026, 3, 31), ymd(2026, 6, 30)} {
			q = append(q, `INSERT INTO security_period_facts (silver_source_id, security_key, field, as_of, period_start,
			    provider, value, unit, filed) VALUES ('ref', '`+key+`', 'eps_diluted', `+itoa(end.Unix())+`, '`+
				end.AddDate(0, -3, 1).Format(time.DateOnly)+`', 'sec', 0.75, 'USD/shares', '2026-08-0`+strconv.Itoa(i+1)+`')`)
		}
	}
	execEach(t, db, q...)
	rebuildExposureWithDefaults(t, db, ctx)
	in, err := YieldInputs(ctx, db, snap+86399, "USD")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := YieldSecurities(ctx, db, yield.Compute(in))
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]yield.SecurityRow{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	stock, note := byName["Example Stock Co"], byName["EXAMPLE NOTE"]
	if stock.AssetClass != "public_equity" || stock.Value != 600 || stock.Direct != 0 || stock.ViaFunds != 600 ||
		stock.Price == nil || *stock.Price != 60 || stock.EarningsYield == nil || math.Abs(*stock.EarningsYield-5) > 1e-9 {
		t.Errorf("the stock %+v", stock)
	}
	if note.AssetClass != "fixed_income" || note.EarningsYield != nil {
		t.Errorf("the note %+v", note)
	}
}
