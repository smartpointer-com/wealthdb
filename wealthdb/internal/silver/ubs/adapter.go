// Package ubs projects the ubs-web and ubs-psn silver SQLites into
// canonical change records. See docs/adapters/ubs.md for the
// mapping contract.
package ubs

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

const kindName = "ubs"

// collectors are the collectors whose silver the kind reads
// (silver.Collector), each a subsource of the one source.
var collectors = []silver.Collector{
	{
		Name:      "ubs-web",
		Display:   "UBS e-banking",
		Group:     silver.GroupSwitzerland,
		Covers:    "accounts, cards and portfolios from the e-banking website",
		Subsource: "ubs-web",
		Files:     []string{"ubs-web.db"},
	},
	{
		Name:      "ubs-psn",
		Display:   "UBS PSN (SFTP files)",
		Group:     silver.GroupSwitzerland,
		Covers:    "daily bank files by SFTP; needs an agreement with UBS",
		Subsource: "ubs-psn",
		Files:     []string{"ubs-psn.db"},
		FX:        true,
	},
}

// init registers the kind (silver.Source).
//
// Lots: shadow at the average method. The source states an average cost
// for its positions and no lots, so the replay fills nothing; `gains
// check` compares its ledger with the stated cost (docs/LOTS.md §6, §9).
func init() {
	lp := lots.ShadowPolicy()
	silver.Register(silver.Source{
		Kind:       kindName,
		Settings:   &silver.Settings{Key: "relationships", Parse: parseSettings},
		Open:       (&Adapter{}).Open,
		Subsources: []string{"ubs-web", "ubs-psn"},
		Collectors: collectors,
		Returns:    returnsPolicy(),
		Lots:       &lp,
	})
}

// Adapter implements silver.Adapter.
type Adapter struct{}

// Open attaches the configured UBS silvers.
//
// Single-file mode (spec.Path set): the file is read as a PSN
// silver, for a source configured with the PSN feed alone at the
// top level.
//
// Subsources mode: kind is "ubs-web" or "ubs-psn"; each entry is
// optional but at least one must be present. The returned
// Connection orchestrates a merged stream where web data covers
// pre-PSN-start dates and PSN covers from each banking
// relationship's go-live forward. See merge.go.
func (*Adapter) Open(ctx context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	rels, _ := spec.Settings.([]RelationshipPair)
	c := &Connection{relationships: rels, source: spec.ID}

	// Single-path form: a PSN silver.
	if spec.Path != "" {
		db, err := silver.OpenReadOnlySQLite(spec.Path, "ubs (single path)")
		if err != nil {
			return nil, err
		}
		c.psn = &psnReader{db: db, source: spec.ID, file: filepath.Base(spec.Path)}
		if _, err := c.psn.schema(ctx); err != nil {
			_ = c.Close()
			return nil, err
		}
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
			c.psn = &psnReader{db: db, source: spec.ID, file: filepath.Base(s.Path)}
		case "ubs-web":
			c.web = &webReader{db: db, file: filepath.Base(s.Path)}
		default:
			_ = db.Close()
			_ = c.Close()
			return nil, fmt.Errorf("ubs: unknown subsource kind %q (want ubs-web or ubs-psn)", s.Kind)
		}
	}
	if c.psn == nil && c.web == nil {
		return nil, fmt.Errorf("ubs: at least one of subsources[ubs-web], subsources[ubs-psn] must be configured")
	}
	if c.psn != nil {
		if _, err := c.psn.schema(ctx); err != nil {
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

// Connection orchestrates one or both UBS subsources. Each
// reader (psn / web) is non-nil only when its subsource is
// configured; the merge layer (merge.go) handles whichever
// combination is present.
type Connection struct {
	psn           *psnReader
	web           *webReader
	relationships []RelationshipPair
	// source is the source's id in gold, which a contract's key is
	// scoped by.
	source string
}

func (c *Connection) Close() error {
	var psnErr, webErr error
	if c.psn != nil {
		psnErr = c.psn.Close()
		c.psn = nil
	}
	if c.web != nil {
		webErr = c.web.Close()
		c.web = nil
	}
	return errors.Join(psnErr, webErr)
}
