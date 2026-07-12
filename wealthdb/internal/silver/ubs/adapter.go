// Package ubs projects the ubs-psn silver SQLite into
// canonical change records. See docs/adapters/ubs.md for the
// mapping contract.
package ubs

import (
	"context"
	"fmt"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

const kindName = "ubs"

func init() {
	silver.Register(&Adapter{})
}

// Adapter implements silver.Adapter.
type Adapter struct{}

func (*Adapter) Kind() string { return kindName }

// Open attaches the configured UBS silvers.
//
// Single-file mode (spec.Path set): legacy form — treats the
// file as a PSN silver. Used by tests and by users who only have
// the PSN feed configured at the top level.
//
// Subsources mode: kind is "ubs-web" or "ubs-psn"; each entry is
// optional but at least one must be present. The returned
// Connection orchestrates a merged stream where web data covers
// pre-PSN-start dates and PSN covers from each banking
// relationship's go-live forward. See merge.go.
func (*Adapter) Open(_ context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	c := &Connection{relationships: spec.Relationships}

	// Single-path form (legacy + tests).
	if spec.Path != "" {
		db, err := silver.OpenReadOnlySQLite(spec.Path, "ubs (single path)")
		if err != nil {
			return nil, err
		}
		c.psn = &psnReader{db: db}
		return c, nil
	}

	for _, s := range spec.Subsources {
		db, err := silver.OpenReadOnlySQLite(s.Path, fmt.Sprintf("ubs subsource %q", s.Kind))
		if err != nil {
			_ = c.Close()
			return nil, err
		}
		switch s.Kind {
		case "ubs-psn":
			c.psn = &psnReader{db: db}
		case "ubs-web":
			c.web = &webReader{db: db}
		default:
			_ = db.Close()
			_ = c.Close()
			return nil, fmt.Errorf("ubs: unknown subsource kind %q (want ubs-web or ubs-psn)", s.Kind)
		}
	}
	if c.psn == nil && c.web == nil {
		return nil, fmt.Errorf("ubs: at least one of subsources[ubs-web], subsources[ubs-psn] must be configured")
	}
	return c, nil
}

// Connection orchestrates one or both UBS subsources. Each
// reader (psn / web) is non-nil only when its subsource is
// configured; the merge layer (merge.go) handles whichever
// combination is present.
type Connection struct {
	psn           *psnReader
	web           *webReader
	relationships []silver.RelationshipPair
}

func (c *Connection) Close() error {
	var firstErr error
	if c.psn != nil {
		if err := c.psn.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		c.psn = nil
	}
	if c.web != nil {
		if err := c.web.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		c.web = nil
	}
	return firstErr
}
