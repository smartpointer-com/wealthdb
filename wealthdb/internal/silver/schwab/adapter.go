// Package schwab projects the schwab-api silver SQLite (and
// optionally the schwab-web silver, when configured as a
// subsource) into canonical change records. See
// docs/adapters/schwab.md for the mapping contract.
package schwab

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// kindName is the silver-kind discriminator that appears in the
// wealthdb config file's silver_sources[].kind field. The
// umbrella name is "schwab"; subsource kinds are "schwab-api" and
// "schwab-web". Mirrors the UBS adapter's subsource pattern.
const kindName = "schwab"

// init registers the kind (silver.Source).
//
// Returns: flow-complete bank/pension, the standard bank external and
// transfer-like sets, forward knobs defaulted (no-op). The API states a
// corporate action's cash signed, and the adapter passes it through
// (canonical.CashSignedKinds). The class pass reads a lookup's type of a
// security over the adapter's class (RefineClass).
func init() {
	rp := returns.DefaultReturnsPolicy(returns.BankFlowPolicy())
	silver.Register(silver.Source{
		Kind:        kindName,
		Settings:    &silver.Settings{Key: "schwab", Parse: parseSettings},
		Open:        (&Adapter{}).Open,
		Returns:     &rp,
		Lots:        lotPolicy(),
		CashSigned:  []canonical.TxKind{canonical.TxKindCorporateAction},
		RefineClass: (&Adapter{}).RefineClass,
	})
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
// Subsources mode: kind is "schwab-api" or "schwab-web", either or
// both. With both, the web silver names an account by the last digits
// of its number and the api silver maps those to the account ids gold
// holds (buildAccountBridge); the returned Connection orchestrates a
// merged stream where web data backfills pre-api-coverage
// transactions and contributes historical position / cash snapshots
// that api doesn't surface. With web alone, an account's id is its
// full account number, which the web silver states for every account
// (webOnlyBridge). See merge.go.
func (*Adapter) Open(ctx context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	c := &Connection{}

	// Single-path form: a schwab-api silver.
	if spec.Path != "" {
		db, err := silver.OpenReadOnlySQLite(spec.Path, "schwab (single path)")
		if err != nil {
			return nil, err
		}
		c.api = &apiReader{db: db, marks: markGuardOf(spec), file: filepath.Base(spec.Path)}
		if _, err := c.api.schema(ctx); err != nil {
			_ = c.Close()
			return nil, err
		}
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
			c.api = &apiReader{db: db, marks: markGuardOf(spec), file: filepath.Base(s.Path)}
		case "schwab-web":
			c.web = &webReader{db: db, file: filepath.Base(s.Path)}
		default:
			_ = db.Close()
			_ = c.Close()
			return nil, fmt.Errorf("schwab: unknown subsource kind %q (want schwab-api or schwab-web)", s.Kind)
		}
	}
	if c.api == nil && c.web == nil {
		return nil, fmt.Errorf("schwab: configure subsources schwab-api, schwab-web or both")
	}
	if c.api != nil {
		if _, err := c.api.schema(ctx); err != nil {
			_ = c.Close()
			return nil, err
		}
	}
	if c.web != nil {
		if _, err := c.web.schema(ctx); err != nil {
			_ = c.Close()
			return nil, err
		}
	}
	return c, nil
}
