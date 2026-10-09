// Package synthetic projects the one generic silver into canonical change
// records. See docs/adapters/synthetic.md for the mapping this implements.
//
// Every other kind reads a silver shaped by its source and decides what each
// row means. This one reads a silver shaped by the canonical model itself:
// one table per record type — portfolios, accounts, instruments, positions,
// open lots, cash balances, fx rates, transactions, realized lots — so a row
// is the change record it becomes, column for column (testdata/silver_schema.sql
// is the schema's one definition). No collector and no bronze stand behind
// it; a generator writes it, and nothing about any one generator is known
// here.
//
// The projection is therefore a pass-through with the cross-cutting guards
// every adapter applies (docs/DESIGN.md §6.8): a value outside a canonical
// vocabulary falls back to the vocabulary's catch-all with the raw value kept
// in the payload, and a transaction's amounts are re-signed the canonical
// way.
//
// Unlike the sources whose dumps re-read everything, this silver is
// append-only, and its change window is INCREMENTAL: a load takes only the
// runs appended since the watermark (status.go).
package synthetic

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

const kindName = "synthetic"

func init() {
	silver.Register(&Adapter{})
}

type Adapter struct{}

func (*Adapter) Kind() string { return kindName }

func (*Adapter) Open(_ context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, "synthetic silver")
	if err != nil {
		return nil, err
	}
	return &Connection{db: db}, nil
}

type Connection struct {
	db *sql.DB
}

func (c *Connection) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	err := c.db.Close()
	c.db = nil
	return err
}

// annotations collects the raw values a fallback replaced, keyed
// `source_<column>`, so the payload keeps what the row said. The zero value
// is empty and allocates nothing until a fallback fires.
type annotations map[string]any

func (a *annotations) keep(column, raw string) {
	if *a == nil {
		*a = annotations{}
	}
	(*a)["source_"+column] = raw
}

// payloadWith passes a silver payload through, with the annotations merged
// in when there are any. An empty payload is absent.
func payloadWith(payload string, extra annotations) json.RawMessage {
	if len(extra) == 0 {
		return silver.JSONOrNil(sql.NullString{String: payload, Valid: true})
	}
	return silver.PayloadWith(payload, extra)
}

// taxonomyPair is the (exposure, vehicle) pair of a position or an
// instrument: the row's own where the taxonomy admits it, else
// (other, other) with the raw pair kept.
func taxonomyPair(assetClass, vehicle string, extra *annotations) (canonical.AssetClass, canonical.Vehicle) {
	a, v := canonical.AssetClass(assetClass), canonical.Vehicle(vehicle)
	if canonical.ValidTaxonomyPair(a, v) {
		return a, v
	}
	extra.keep("asset_class", assetClass)
	extra.keep("vehicle", vehicle)
	return canonical.AssetClassOther, canonical.VehicleOther
}

// tradedPair is taxonomyPair for a transaction, where either half may be
// absent. Both absent is the ordinary case, and one alone passes where it
// belongs to its own vocabulary — the gate the gold writer applies. A pair
// that fails it leaves both halves empty, so the instrument's own pair
// answers, with the raw values kept.
func tradedPair(assetClass, vehicle string, extra *annotations) (canonical.AssetClass, canonical.Vehicle) {
	a, v := canonical.AssetClass(assetClass), canonical.Vehicle(vehicle)
	var ok bool
	switch {
	case a != "" && v != "":
		ok = canonical.ValidTaxonomyPair(a, v)
	case a != "":
		ok = a.Valid()
	case v != "":
		ok = v.Valid()
	default:
		return "", ""
	}
	if ok {
		return a, v
	}
	if a != "" {
		extra.keep("asset_class", assetClass)
	}
	if v != "" {
		extra.keep("vehicle", vehicle)
	}
	return "", ""
}

// txKind is the row's transaction kind, or `other` with the raw value kept.
func txKind(raw string, extra *annotations) canonical.TxKind {
	k := canonical.TxKind(raw)
	if k.Valid() {
		return k
	}
	extra.keep("kind", raw)
	return canonical.TxKindOther
}
