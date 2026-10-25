// Package schwab projects the schwab-api silver SQLite (and
// optionally the schwab-web silver, when configured as a
// subsource) into canonical change records. See
// docs/adapters/schwab.md for the mapping contract.
package schwab

import (
	"context"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// kindName is the silver-kind discriminator that appears in the
// wealthdb config file's silver_sources[].kind field. The
// umbrella name is "schwab"; subsource kinds are "schwab-api" and
// "schwab-web". Mirrors the UBS adapter's subsource pattern.
const kindName = "schwab"

func init() {
	silver.Register(&Adapter{})
}

// Adapter implements silver.Adapter.
type Adapter struct{}

func (*Adapter) Kind() string { return kindName }

// Open attaches the configured Schwab silvers.
//
// Single-file mode (spec.Path set): the file is a schwab-api
// silver, the form for a source configured with the api feed
// alone at the top level.
//
// Subsources mode: kind is "schwab-api" or "schwab-web". The api
// entry is required and the web entry optional: the web silver
// names an account by the last digits of its number, and only the
// api silver maps those to the account ids gold holds
// (buildAccountBridge). The returned Connection orchestrates a
// merged stream where web data backfills pre-api-coverage
// transactions and contributes historical position / cash
// snapshots that api doesn't surface. See merge.go.
func (*Adapter) Open(_ context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	c := &Connection{}

	// Single-path form: a schwab-api silver.
	if spec.Path != "" {
		db, err := silver.OpenReadOnlySQLite(spec.Path, "schwab (single path)")
		if err != nil {
			return nil, err
		}
		c.api = &apiReader{db: db, marks: spec.MarkGuard}
		return c, nil
	}

	for _, s := range spec.Subsources {
		db, err := silver.OpenReadOnlySQLite(s.Path, fmt.Sprintf("schwab subsource %q", s.Kind))
		if err != nil {
			_ = c.Close()
			return nil, err
		}
		switch s.Kind {
		case "schwab-api":
			c.api = &apiReader{db: db, marks: spec.MarkGuard}
		case "schwab-web":
			c.web = &webReader{db: db}
		default:
			_ = db.Close()
			_ = c.Close()
			return nil, fmt.Errorf("schwab: unknown subsource kind %q (want schwab-api or schwab-web)", s.Kind)
		}
	}
	if c.api == nil {
		_ = c.Close()
		return nil, fmt.Errorf("schwab: subsources[schwab-api] must be configured: " +
			"only the api silver maps the web silver's account numbers to account ids")
	}
	return c, nil
}
