// Package cointracking projects the cointracking silver DuckDB — the
// aggregator-of-record for crypto holdings across exchanges and on-chain
// wallets, its trade history replayed into daily holdings per portfolio —
// into canonical change records. docs/adapters/cointracking.md documents the
// identifiers, the account taxonomy, the position and FX mapping and the
// transaction rules. init registers the kind (silver.Source), its returns
// policy from policy.go and its lot policy from lotpolicy.go.
package cointracking

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/duckconn"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

const kindName = "cointracking"

// collectors are the collectors whose silver the kind reads
// (silver.Collector).
var collectors = []silver.Collector{
	{
		Name:    "cointracking",
		Display: "CoinTracking",
		Group:   silver.GroupWorldwide,
		Covers:  "crypto wallets and exchanges tracked in CoinTracking",
		Files:   []string{"cointracking.duckdb"},
	},
}

// init registers the kind (silver.Source).
//
// A wallet label is the holder's own words.
func init() {
	silver.Register(silver.Source{
		Kind:       kindName,
		Open:       (&Adapter{}).Open,
		Collectors: collectors,
		Returns:    returnsPolicy(),
		Lots:       lotPolicy(),
		Labels:     silver.LabelTraits{TypedAccountLabels: true},
	})
}

type Adapter struct{}

func (*Adapter) Open(_ context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	// DuckDB takes options as `?key=value` query params on the DSN
	// (cf. internal/gold/open.go).
	dsn := spec.Path + "?access_mode=" + url.QueryEscape("read_only")
	db, err := duckconn.Open(dsn)
	if err != nil {
		return nil, fmt.Errorf("open cointracking silver %q: %w", spec.Path, err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping cointracking silver %q: %w", spec.Path, err)
	}
	return &Connection{db: db}, nil
}

type Connection struct {
	db *sql.DB
}

func (c *Connection) Close() error { return silver.CloseDB(&c.db) }
