// Package synthetic projects the one generic silver into canonical change
// records. See docs/adapters/synthetic.md for the mapping this implements.
//
// Every other kind reads a silver shaped by its source and decides what each
// row means. This one reads a silver shaped by the canonical model itself:
// one table per record type — portfolios, accounts, instruments, positions,
// open lots, cash balances, fx rates, transactions, realized lots, the
// reference records, the rate observations and the capital accounts — so a
// row is the change record it becomes, column for column
// (testdata/silver_schema.sql is the schema's one definition). No collector
// and no bronze stand behind it; a generator writes it, and nothing about
// any one generator is known here.
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
	"path/filepath"
	"slices"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

const kindName = "synthetic"

// init registers the kind (silver.Source).
func init() {
	silver.Register(silver.Source{
		Kind:    kindName,
		Open:    (&Adapter{}).Open,
		Returns: returnsPolicy(),
	})
}

type Adapter struct{}

func (*Adapter) Open(ctx context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, "synthetic silver")
	if err != nil {
		return nil, err
	}
	c := &Connection{db: db}
	note, err := silver.ProbeOptional(ctx, db, filepath.Base(spec.Path), c.schema.optional()...)
	if err != nil {
		db.Close()
		return nil, err
	}
	c.Note(note)
	if c.schema.hasReference() {
		// The reference tables' versions are the refdata collector's
		// schema versions, not this silver's, so the probe's note is
		// left out.
		if c.reference, _, err = silver.ProbeReferenceSchema(ctx, db, ""); err != nil {
			db.Close()
			return nil, err
		}
	}
	return c, nil
}

type Connection struct {
	db     *sql.DB
	schema schema
	// reference is which later parts of the reference schema the file
	// holds, where it holds every reference table (schema.reference).
	reference silver.ReferenceSchema
	silver.Advice
}

// schema is which optional parts of the silver schema a file holds,
// probed once at open. A file older than a table states none of its
// records.
type schema struct {
	reference    []bool // each of silver.ReferenceTables
	rates        bool   // silver.RatesTable
	positionLots bool   // the position_lots table
	realizedLots bool   // the realized_lots table
	capital      bool   // the capital_accounts table
}

func (s *schema) optional() []silver.Optional {
	s.reference = make([]bool, len(silver.ReferenceTables))
	out := []silver.Optional{
		{Table: silver.RatesTable, Has: &s.rates},
		{Table: "position_lots", Has: &s.positionLots},
		{Table: "realized_lots", Has: &s.realizedLots},
		{Table: "capital_accounts", Has: &s.capital},
	}
	for i, t := range silver.ReferenceTables {
		out = append(out, silver.Optional{Table: t, Has: &s.reference[i]})
	}
	return out
}

// hasReference reports whether the file holds every reference table.
func (s schema) hasReference() bool {
	return !slices.Contains(s.reference, false)
}

func (c *Connection) Close() error { return silver.CloseDB(&c.db) }

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
