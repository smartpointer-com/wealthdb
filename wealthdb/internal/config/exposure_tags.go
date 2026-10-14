package config

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The tags ledger states what no public source can know about a holding:
// a property, a loan, a private fund, a startup's shares. One row per
// holding, named by silver_source_id and instrument (its
// instrument_external_id, as `wealthdb exposure coverage` prints it):
//
//	silver_source_id,instrument,country,currency,sector,note
//	manual,house-1,CH,CHF,,the house
//	angellist,fund-7,US,USD,technology,
//
// The columns may come in any order; silver_source_id and instrument are
// required, and a row states at least one of country, currency and
// sector. note is free text. Any other column is refused, so a misspelt
// header cannot drop a field.

// tagColumns are the tags ledger's columns; the first two are required.
var (
	tagColumns         = []string{"silver_source_id", "instrument", "country", "currency", "sector", "note"}
	requiredTagColumns = []string{"silver_source_id", "instrument"}
)

// TagsPath is `exposure.tags`: the path to the tags ledger. Anything but
// a string in its place is refused with what the field takes.
type TagsPath string

func (p *TagsPath) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("exposure.tags is the path to the tags ledger, a CSV file (docs/EXPOSURE.md §9)")
	}
	*p = TagsPath(s)
	return nil
}

// readTagLedgerFile reads the tags ledger at path into gold's rows,
// sorted by source and instrument, each checked against the configured
// sources and the vocabularies. An empty path or a missing file holds no
// tags.
func readTagLedgerFile(path string, sources map[string]bool) ([]canonical.ExposureTag, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("exposure.tags: %w", err)
	}
	defer f.Close()
	tags, err := readTagLedger(f, sources)
	if err != nil {
		return nil, fmt.Errorf("exposure.tags: %s: %w", path, err)
	}
	return tags, nil
}

func readTagLedger(r io.Reader, sources map[string]bool) ([]canonical.ExposureTag, error) {
	cr := csv.NewReader(r)
	cr.TrimLeadingSpace = true
	cr.FieldsPerRecord = -1
	header, err := cr.Read()
	if err == io.EOF {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("header: %w", err)
	}
	col := map[string]int{}
	for i, h := range header {
		// A byte-order mark (a spreadsheet's UTF-8) and an empty trailing
		// cell are no columns.
		name := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")))
		if name == "" && i == len(header)-1 {
			continue
		}
		if !slices.Contains(tagColumns, name) {
			return nil, fmt.Errorf("header: unknown column %q; the columns are %s", h, strings.Join(tagColumns, ", "))
		}
		if _, dup := col[name]; dup {
			return nil, fmt.Errorf("header: column %q twice", name)
		}
		col[name] = i
	}
	for _, need := range requiredTagColumns {
		if _, ok := col[need]; !ok {
			return nil, fmt.Errorf("header: column %q is required", need)
		}
	}
	get := func(rec []string, name string) string {
		if i, ok := col[name]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	var out []canonical.ExposureTag
	seen := map[[2]string]int{}
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err // a csv.ParseError names its line
		}
		line, _ := cr.FieldPos(0)
		if strings.TrimSpace(strings.Join(rec, "")) == "" {
			continue
		}
		t := canonical.ExposureTag{
			SilverSourceID:       get(rec, "silver_source_id"),
			InstrumentExternalID: get(rec, "instrument"),
			Country:              get(rec, "country"),
			Currency:             get(rec, "currency"),
			Sector:               get(rec, "sector"),
		}
		if err := checkTag(t, sources); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		k := [2]string{t.SilverSourceID, t.InstrumentExternalID}
		if first, dup := seen[k]; dup {
			return nil, fmt.Errorf("line %d: %s %s is tagged on line %d already", line, k[0], k[1], first)
		}
		seen[k] = line
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b canonical.ExposureTag) int {
		if c := strings.Compare(a.SilverSourceID, b.SilverSourceID); c != 0 {
			return c
		}
		return strings.Compare(a.InstrumentExternalID, b.InstrumentExternalID)
	})
	return out, nil
}

// checkTag checks one row: a configured source, an instrument, and at
// least one field, each in its vocabulary.
func checkTag(t canonical.ExposureTag, sources map[string]bool) error {
	switch {
	case t.SilverSourceID == "" || t.InstrumentExternalID == "":
		return fmt.Errorf("silver_source_id and instrument are required")
	case !sources[t.SilverSourceID]:
		return fmt.Errorf("silver_source_id %q: no silver_sources[].id matches", t.SilverSourceID)
	case t.Sector == "" && t.Country == "" && t.Currency == "":
		return fmt.Errorf("at least one of country, currency and sector must be set")
	case t.Sector != "" && !canonical.Sector(t.Sector).Valid():
		return fmt.Errorf("sector %q is not one of %s", t.Sector, sectorNames())
	case t.Currency != "" && !IsLikelyISO4217(t.Currency):
		return fmt.Errorf("currency %q is not a 3-letter ISO 4217 code", t.Currency)
	}
	if t.Country != "" {
		if _, ok := canonical.CountryByCode(t.Country); !ok || t.Country != strings.ToUpper(t.Country) {
			return fmt.Errorf("country %q is not an ISO 3166 alpha-2 code", t.Country)
		}
	}
	return nil
}
