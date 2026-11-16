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
	holdings       bool // the instrument and figures of positions (holdingRead)
}

func (s *apiSchema) optional() []silver.Optional {
	return []silver.Optional{
		{Table: "instruments", Version: 2, Has: &s.instruments},
		{Table: "accounts", Columns: []string{"nickname"}, Version: 3, Has: &s.nickname},
		{Table: "positions", Columns: []string{"unrealized_gain_loss"}, Version: 5, Has: &s.unrealizedGain},
		// An older silver states them in the payload, which is read in
		// their place: nothing loads empty, so nothing is advised.
		{Table: "positions", Columns: holdingColumns, Has: &s.holdings},
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
	txColumns           bool // transactions.amount .. action (silver migration 0014)
}

func (s *webSchema) optional() []silver.Optional {
	return []silver.Optional{
		{Table: "historical_position_snapshots", Version: 2, Has: &s.historicalPositions},
		{Table: "historical_cash_balances", Version: 2, Has: &s.historicalCash},
		{Table: "historical_cash_balances", Columns: []string{"interest_rate"}, Version: 11, Has: &s.historicalCashRate},
		{Table: "accounts", Columns: []string{"account_registration"}, Version: 3, Has: &s.registration},
		{Table: "open_lots", Version: 6, Has: &s.openLots},
		{Table: "closed_lots", Version: 7, Has: &s.closedLots},
		{Table: "transactions", Columns: []string{"amount", "quantity", "price", "description", "action"},
			Version: 14, Has: &s.txColumns},
	}
}

// schema is the file's optional parts, probed on the first call: Open
// makes that call, so a reader that opened has probed.
func (c *apiReader) schema(ctx context.Context) (apiSchema, error) {
	return c.parts.Get(func() (apiSchema, error) {
		var s apiSchema
		note, err := silver.ProbeOptional(ctx, c.db, c.file, s.optional()...)
		if err != nil {
			return apiSchema{}, err
		}
		c.note = note
		return s, nil
	})
}

// schema is the file's optional parts, probed on the first call: Open
// makes that call, so a reader that opened has probed.
func (r *webReader) schema(ctx context.Context) (webSchema, error) {
	return r.parts.Get(func() (webSchema, error) {
		var s webSchema
		note, err := silver.ProbeOptional(ctx, r.db, r.file, s.optional()...)
		if err != nil {
			return webSchema{}, err
		}
		r.note = note
		return s, nil
	})
}

// SchemaAdvice is the notes of the feeds whose silver is older than the
// adapter (silver.SchemaAdviser).
func (c *Connection) SchemaAdvice() []silver.SchemaNote {
	var a silver.Advice
	if c.api != nil {
		a.Note(c.api.note)
	}
	if c.web != nil {
		a.Note(c.web.note)
	}
	return a.SchemaAdvice()
}
