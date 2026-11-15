package schwab

import (
	"context"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// apiSchema is which optional parts of the schwab-api silver schema a
// file holds, probed once (apiReader.schema).
type apiSchema struct {
	instruments    bool // the instruments table (--with-instruments)
	nickname       bool // accounts.nickname
	unrealizedGain bool // positions.unrealized_gain_loss
}

func (s *apiSchema) optional() []silver.Optional {
	return []silver.Optional{
		{Table: "instruments", Has: &s.instruments},
		{Table: "accounts", Columns: []string{"nickname"}, Has: &s.nickname},
		{Table: "positions", Columns: []string{"unrealized_gain_loss"}, Has: &s.unrealizedGain},
	}
}

// webSchema is which optional parts of the schwab-web silver schema a
// file holds, probed once (webReader.schema).
type webSchema struct {
	historicalPositions bool // historical_position_snapshots (silver migration 0002)
	historicalCash      bool // historical_cash_balances (silver migration 0002)
	historicalCashRate  bool // historical_cash_balances.interest_rate
	registration        bool // accounts.account_registration (silver migration 0003)
	openLots            bool // the open_lots table
	closedLots          bool // the closed_lots table
}

func (s *webSchema) optional() []silver.Optional {
	return []silver.Optional{
		{Table: "historical_position_snapshots", Has: &s.historicalPositions},
		{Table: "historical_cash_balances", Has: &s.historicalCash},
		{Table: "historical_cash_balances", Columns: []string{"interest_rate"}, Has: &s.historicalCashRate},
		{Table: "accounts", Columns: []string{"account_registration"}, Has: &s.registration},
		{Table: "open_lots", Has: &s.openLots},
		{Table: "closed_lots", Has: &s.closedLots},
	}
}

// schema is the file's optional parts, probed on the first call: Open
// makes that call, so a reader that opened has probed.
func (c *apiReader) schema(ctx context.Context) (apiSchema, error) {
	if !c.probed {
		if err := silver.ProbeOptional(ctx, c.db, c.parts.optional()...); err != nil {
			return apiSchema{}, err
		}
		c.probed = true
	}
	return c.parts, nil
}

// schema is the file's optional parts, probed on the first call: Open
// makes that call, so a reader that opened has probed.
func (r *webReader) schema(ctx context.Context) (webSchema, error) {
	if !r.probed {
		if err := silver.ProbeOptional(ctx, r.db, r.parts.optional()...); err != nil {
			return webSchema{}, err
		}
		r.probed = true
	}
	return r.parts, nil
}
