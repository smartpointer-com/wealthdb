package schwab

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// holdingColumns are the columns of positions an apiHolding reads, in
// scan order: the instrument and the figures silver migration 0006
// promotes from the payload.
var holdingColumns = []string{
	"asset_type", "instrument_type", "cusip", "symbol", "description",
	"maturity_date", "variable_rate", "long_quantity", "short_quantity", "market_value",
}

// apiHolding is a positions row's instrument and figures.
type apiHolding struct {
	LongQuantity  canonical.Decimal
	ShortQuantity canonical.Decimal
	MarketValue   *canonical.Decimal
	Instrument    schwabInstrument
}

// holdingRead is how a silver's positions state their holdings: in the
// columns of migration 0006, or, in a silver older than it, in the
// payload alone. Both read the same holding.
type holdingRead struct{ columns bool }

func (c *apiReader) holdingRead(ctx context.Context) (holdingRead, error) {
	s, err := c.schema(ctx)
	return holdingRead{columns: s.holdings}, err
}

// selectList is what a query selects for the holding.
func (h holdingRead) selectList() string {
	if h.columns {
		return strings.Join(holdingColumns, ", ")
	}
	return "payload"
}

// scan is a scan target of selectList, for one row.
func (h holdingRead) scan() *holdingScan { return &holdingScan{columns: h.columns} }

// holdingScan is the scan target of a holdingRead.
type holdingScan struct {
	columns                                               bool
	payload                                               string
	assetType, instrumentType, cusip, symbol, description sql.NullString
	maturity                                              sql.NullInt64
	rate, long, short                                     sql.NullString
	value                                                 *canonical.Decimal
}

// dest are the scan targets, in selectList order.
func (s *holdingScan) dest() []any {
	if !s.columns {
		return []any{&s.payload}
	}
	return []any{&s.assetType, &s.instrumentType, &s.cusip, &s.symbol, &s.description,
		&s.maturity, &s.rate, &s.long, &s.short, silver.Dec(&s.value, silver.DecText)}
}

// holding is the row scanned.
func (s *holdingScan) holding() (apiHolding, error) {
	if !s.columns {
		return preV6Holding(s.payload)
	}
	return s.fromColumns()
}

// preV6Position is the payload of a positions row as a silver older
// than migration 0006 states it, with the fields its reader decodes.
type preV6Position struct {
	LongQuantity  canonical.Decimal  `json:"longQuantity"`
	ShortQuantity canonical.Decimal  `json:"shortQuantity"`
	AveragePrice  *canonical.Decimal `json:"averagePrice"`
	MarketValue   *canonical.Decimal `json:"marketValue"`
	Instrument    schwabInstrument   `json:"instrument"`
}

// preV6Holding reads a holding from the payload of a silver older than
// migration 0006, which states it there alone.
func preV6Holding(payload string) (apiHolding, error) {
	var p preV6Position
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return apiHolding{}, err
	}
	return apiHolding{LongQuantity: p.LongQuantity, ShortQuantity: p.ShortQuantity,
		MarketValue: p.MarketValue, Instrument: p.Instrument}, nil
}

// fromColumns is the holding the columns state. A quantity silver states
// none of is zero. The maturity is the day it names, in the form the API
// stamps a maturity, which apiMaturity reads.
func (s *holdingScan) fromColumns() (apiHolding, error) {
	long, err := silver.DecimalOrZero(s.long)
	if err != nil {
		return apiHolding{}, fmt.Errorf("long_quantity: %w", err)
	}
	short, err := silver.DecimalOrZero(s.short)
	if err != nil {
		return apiHolding{}, fmt.Errorf("short_quantity: %w", err)
	}
	var rate *float64
	if s.rate.Valid {
		f, err := strconv.ParseFloat(s.rate.String, 64)
		if err != nil {
			return apiHolding{}, fmt.Errorf("variable_rate: %w", err)
		}
		rate = &f
	}
	var maturity string
	if s.maturity.Valid {
		maturity = time.Unix(s.maturity.Int64, 0).UTC().Format(time.DateOnly)
	}
	return apiHolding{
		LongQuantity:  long,
		ShortQuantity: short,
		MarketValue:   s.value,
		Instrument: schwabInstrument{
			AssetType:    s.assetType.String,
			Type:         s.instrumentType.String,
			CUSIP:        s.cusip.String,
			Symbol:       s.symbol.String,
			Description:  s.description.String,
			MaturityDate: maturity,
			VariableRate: rate,
		},
	}, nil
}
