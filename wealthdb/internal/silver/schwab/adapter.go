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
// Single-file mode (spec.Path set): legacy form — treats the
// file as a schwab-api silver. Used by tests and by users who
// only have the api feed configured at the top level.
//
// Subsources mode: kind is "schwab-api" or "schwab-web"; each
// entry is optional but at least one must be present. The
// returned Connection orchestrates a merged stream where web
// data backfills pre-api-coverage transactions and contributes
// historical position / cash snapshots that api doesn't
// surface. See merge.go.
func (*Adapter) Open(_ context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	c := &Connection{}

	// Single-path form (legacy + tests).
	if spec.Path != "" {
		db, err := silver.OpenReadOnlySQLite(spec.Path, "schwab (single path)")
		if err != nil {
			return nil, err
		}
		c.api = &apiReader{db: db}
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
			c.api = &apiReader{db: db}
		case "schwab-web":
			c.web = &webReader{db: db}
		default:
			_ = db.Close()
			_ = c.Close()
			return nil, fmt.Errorf("schwab: unknown subsource kind %q (want schwab-api or schwab-web)", s.Kind)
		}
	}
	if c.api == nil && c.web == nil {
		return nil, fmt.Errorf("schwab: at least one of subsources[schwab-api], subsources[schwab-web] must be configured")
	}
	return c, nil
}
