package config

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The declarations ledger states what no public source can know about a
// holding, one NDJSON record per line, for one holding and date:
//
//	{"silver_source_id":"manual","instrument":"house-1","country":{"CH":100},"currency":{"CHF":100}}
//
// docs/EXPOSURE.md §9 defines the record and its rules; this file reads
// and checks it, and crosses its dimensions into the parts gold stores.

// declarationKeys are the keys a record may carry besides its dimensions;
// all but source take a string.
var declarationKeys = []string{"silver_source_id", "instrument", "as_of", "source", "note"}

// hundred is 100 percent.
var hundred = big.NewRat(100, 1)

// declarationTolerance is how far a dimension's shares may sum from 100,
// in percent, for rounding.
var declarationTolerance = big.NewRat(1, 2)

// regionRest is the key that names the rest of the world as one region.
const regionRest = "rest"

// DeclarationsPath is `exposure.declarations`: the path to the
// declarations ledger. Anything but a string in its place is refused
// with what the field takes.
type DeclarationsPath string

func (p *DeclarationsPath) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("exposure.declarations is the path to the declarations ledger, an NDJSON file (docs/EXPOSURE.md §9)")
	}
	*p = DeclarationsPath(s)
	return nil
}

// retiredTags is `exposure.tags`, which the declarations ledger replaces:
// any value is refused with where it went.
type retiredTags struct{}

func (retiredTags) UnmarshalJSON([]byte) error {
	return fmt.Errorf("exposure.tags: the declarations ledger replaces the tags ledger; name it in exposure.declarations, " +
		"one record per holding (docs/EXPOSURE.md §9)")
}

// readDeclarationsFile reads the declarations ledger at path, each record
// checked against the configured sources and the vocabularies. An empty
// path or a missing file holds no declarations.
func readDeclarationsFile(path string, sources map[string]bool) ([]canonical.ExposureDeclaration, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("exposure.declarations: %w", err)
	}
	defer f.Close()
	out, err := readDeclarations(f, sources)
	if err != nil {
		return nil, fmt.Errorf("exposure.declarations: %s: %w", path, err)
	}
	return out, nil
}

func readDeclarations(r io.Reader, sources map[string]bool) ([]canonical.ExposureDeclaration, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var out []canonical.ExposureDeclaration
	// A holding has one record per date for its exposure, and one per date
	// that states its situs: the two have precedences of their own
	// (docs/EXPOSURE.md §9). A record that states both counts in both.
	type holdingDate struct {
		src, iid, date string // date "" for a record without one
		situs          bool
	}
	seen := map[holdingDate]int{}
	for line := 1; sc.Scan(); line++ {
		text := bytes.TrimSpace(sc.Bytes())
		if line == 1 {
			// A byte-order mark (an editor's UTF-8) is no content.
			text = bytes.TrimPrefix(text, []byte("\ufeff"))
		}
		if len(text) == 0 {
			continue
		}
		d, err := parseDeclaration(text, sources)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		d.Line = line
		k := holdingDate{src: d.SilverSourceID, iid: d.InstrumentExternalID}
		if d.AsOf != nil {
			k.date = time.Unix(*d.AsOf, 0).UTC().Format(time.DateOnly)
		}
		when := "without a date"
		if k.date != "" {
			when = "as of " + k.date
		}
		for _, kind := range []struct {
			situs  bool
			states bool
			what   string
		}{{false, len(d.Dimensions) > 0, "declared"}, {true, d.Situs != "", "given a situs"}} {
			if !kind.states {
				continue
			}
			k.situs = kind.situs
			if first, dup := seen[k]; dup {
				return nil, fmt.Errorf("line %d: %s %s is %s %s on line %d already", line, k.src, k.iid, kind.what,
					when, first)
			}
			seen[k] = line
		}
		out = append(out, d)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// parseDeclaration reads one record: its keys checked, each dimension's
// shares summed and padded to 100, the dimensions crossed into leaves,
// and its situs read.
func parseDeclaration(text []byte, sources map[string]bool) (canonical.ExposureDeclaration, error) {
	var d canonical.ExposureDeclaration
	fields, err := jsonObject(text)
	if err != nil {
		return d, fmt.Errorf("%w; a record is one JSON object on one line", err)
	}
	dims := map[string][]share{}
	for _, f := range fields {
		switch {
		case slices.Contains(canonical.DeclarationDimensions, f.key):
			if dims[f.key], err = parseDimension(f.key, f.value); err != nil {
				return d, err
			}
		case f.key == canonical.DeclarationSitus:
			if d.Situs, err = parseSitus(f.value); err != nil {
				return d, err
			}
		case f.key == "source":
			var compact bytes.Buffer
			if err := json.Compact(&compact, f.value); err != nil {
				return d, fmt.Errorf("source: %w", err)
			}
			d.Source = compact.String()
		case slices.Contains(declarationKeys, f.key):
			var s string
			if err := json.Unmarshal(f.value, &s); err != nil {
				return d, fmt.Errorf("%s is a string", f.key)
			}
			switch f.key {
			case "silver_source_id":
				d.SilverSourceID = strings.TrimSpace(s)
			case "instrument":
				d.InstrumentExternalID = strings.TrimSpace(s)
			case "note":
				d.Note = s
			default:
				t, err := time.Parse(time.DateOnly, s)
				if err != nil {
					return d, fmt.Errorf("as_of %q is not a date (YYYY-MM-DD)", s)
				}
				at := t.Unix()
				d.AsOf = &at
			}
		default:
			return d, fmt.Errorf("unknown key %q; a record takes %s, the dimensions %s, and %s", f.key,
				strings.Join(declarationKeys, ", "), strings.Join(canonical.DeclarationDimensions, ", "),
				canonical.DeclarationSitus)
		}
	}
	switch {
	case d.SilverSourceID == "" || d.InstrumentExternalID == "":
		return d, fmt.Errorf("silver_source_id and instrument are required")
	case !sources[d.SilverSourceID]:
		return d, fmt.Errorf("silver_source_id %q: no silver_sources[].id matches", d.SilverSourceID)
	case len(dims) == 0 && d.Situs == "":
		return d, fmt.Errorf("a record declares at least one of %s or %s",
			strings.Join(canonical.DeclarationDimensions, ", "), canonical.DeclarationSitus)
	case dims[canonical.DimensionRegion] != nil && dims[canonical.DimensionCountry] != nil:
		return d, fmt.Errorf("region and country are one dimension: a country implies its region, so declare one of them")
	}
	for _, dim := range canonical.DeclarationDimensions {
		if dims[dim] != nil {
			d.Dimensions = append(d.Dimensions, dim)
		}
	}
	if len(d.Dimensions) > 0 {
		d.Leaves = crossDimensions(d.Dimensions, dims)
	}
	return d, nil
}

// parseSitus reads the situs: one country at 100, since a holding sits
// under one country's law.
func parseSitus(raw json.RawMessage) (string, error) {
	fields, err := jsonObject(raw)
	if err != nil {
		return "", fmt.Errorf("%s: %w", canonical.DeclarationSitus, err)
	}
	if len(fields) != 1 {
		return "", fmt.Errorf("%s: %d countries; a holding has one situs, stated as one country at 100",
			canonical.DeclarationSitus, len(fields))
	}
	f := fields[0]
	switch {
	case f.key == canonical.Unclassified:
		return "", fmt.Errorf("%s: %s states nothing; leave %s out", canonical.DeclarationSitus, f.key,
			canonical.DeclarationSitus)
	case f.key == regionRest || slices.Contains(canonical.Regions(), canonical.Region(f.key)):
		return "", fmt.Errorf("%s: %q is a region; a situs is a country", canonical.DeclarationSitus, f.key)
	case f.key == canonical.EuropeanUnion:
		return "", fmt.Errorf("%s: %s is the European Union, no one country's law", canonical.DeclarationSitus, f.key)
	case !isCountryCode(f.key):
		return "", fmt.Errorf("%s: %q is not an ISO 3166 alpha-2 code", canonical.DeclarationSitus, f.key)
	}
	pct, err := percent(f.value)
	if err != nil {
		return "", fmt.Errorf("%s: %s: %w", canonical.DeclarationSitus, f.key, err)
	}
	if pct.Cmp(hundred) != 0 {
		return "", fmt.Errorf("%s: %s at %s%%; a situs is one country at 100", canonical.DeclarationSitus, f.key,
			percentText(pct))
	}
	return f.key, nil
}

// share is one value of a dimension and its share of the holding, as a
// fraction.
type share struct {
	value string
	frac  *big.Rat
}

// parseDimension reads one dimension's object: each key a value of its
// vocabulary, each share a number from 0 to 100 percent, the shares
// summing to 100 within declarationTolerance. What they leave of 100, or
// take beyond it, goes to the dimension's unclassified part, so the
// shares sum to exactly one. A share of zero states nothing.
func parseDimension(dim string, raw json.RawMessage) ([]share, error) {
	fields, err := jsonObject(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", dim, err)
	}
	sum := new(big.Rat)
	unclassified := new(big.Rat)
	var out []share
	for _, f := range fields {
		value, err := declaredValue(dim, f.key)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", dim, err)
		}
		pct, err := percent(f.value)
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %w", dim, f.key, err)
		}
		sum.Add(sum, pct)
		frac := new(big.Rat).Quo(pct, hundred)
		switch {
		case value == canonical.Unclassified:
			unclassified.Add(unclassified, frac)
		case frac.Sign() > 0:
			out = append(out, share{value: value, frac: frac})
		}
	}
	gap := new(big.Rat).Sub(hundred, sum)
	if new(big.Rat).Abs(gap).Cmp(declarationTolerance) > 0 {
		return nil, fmt.Errorf("%s: the shares sum to %s%%, want 100 within half a point; state the rest as unclassified",
			dim, percentText(sum))
	}
	unclassified.Add(unclassified, gap.Quo(gap, hundred))
	if unclassified.Sign() != 0 {
		out = append(out, share{value: canonical.Unclassified, frac: unclassified})
	}
	return out, nil
}

// declaredValue checks one key of a dimension against its vocabulary
// and returns the value gold stores: `rest` reads as the region other.
func declaredValue(dim, key string) (string, error) {
	if key == canonical.Unclassified {
		return key, nil
	}
	switch dim {
	case canonical.DimensionRegion:
		if key == regionRest {
			return string(canonical.RegionOther), nil
		}
		if !slices.Contains(canonical.Regions(), canonical.Region(key)) {
			var names []string
			for _, r := range canonical.Regions() {
				names = append(names, string(r))
			}
			return "", fmt.Errorf("%q is not a region; the regions are %s, %s (the rest of the world) and %s",
				key, strings.Join(names, ", "), regionRest, canonical.Unclassified)
		}
	case canonical.DimensionCountry:
		if !isCountryCode(key) {
			return "", fmt.Errorf("%q is not an ISO 3166 alpha-2 code", key)
		}
	case canonical.DimensionCurrency:
		if !IsLikelyISO4217(key) {
			return "", fmt.Errorf("%q is not a 3-letter ISO 4217 code", key)
		}
	case canonical.DimensionSector:
		if !canonical.Sector(key).Valid() {
			return "", fmt.Errorf("%q is not one of %s", key, sectorNames())
		}
	case canonical.DimensionAssetClass:
		if !canonical.AssetClass(key).Valid() {
			var names []string
			for _, a := range canonical.AssetClasses() {
				names = append(names, string(a))
			}
			return "", fmt.Errorf("%q is not one of %s (docs/TAXONOMY.md §2)", key, strings.Join(names, ", "))
		}
	}
	return key, nil
}

// percentText is a share as a short decimal, rounded only past the sixth
// place, so a refused sum never prints as one that would pass.
func percentText(r *big.Rat) string {
	t := strings.TrimRight(r.FloatString(6), "0")
	return strings.TrimSuffix(t, ".")
}

// percent reads a share: a JSON number from 0 to 100, exact.
func percent(raw json.RawMessage) (*big.Rat, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	n, ok := v.(json.Number)
	if !ok {
		return nil, fmt.Errorf("a share is a number in percent, not %s; a range goes into note", raw)
	}
	r, ok := new(big.Rat).SetString(n.String())
	if !ok || r.Sign() < 0 || r.Cmp(hundred) > 0 {
		return nil, fmt.Errorf("a share is a number from 0 to 100, not %s", n)
	}
	return r, nil
}

// crossDimensions is the cross product of the declared dimensions: one
// leaf per combination of their values, its share the product of theirs.
// Each dimension's shares sum to exactly one, so the leaves' do too.
func crossDimensions(order []string, dims map[string][]share) []canonical.ExposureDeclaredLeaf {
	type partial struct {
		leaf canonical.ExposureDeclaredLeaf
		frac *big.Rat
	}
	acc := []partial{{frac: big.NewRat(1, 1)}}
	for _, dim := range order {
		var next []partial
		for _, p := range acc {
			for _, s := range dims[dim] {
				l := p.leaf
				switch dim {
				case canonical.DimensionRegion:
					l.Region = s.value
				case canonical.DimensionCountry:
					l.Country = s.value
				case canonical.DimensionCurrency:
					l.Currency = s.value
				case canonical.DimensionSector:
					l.Sector = s.value
				case canonical.DimensionAssetClass:
					l.AssetClass = s.value
				}
				next = append(next, partial{leaf: l, frac: new(big.Rat).Mul(p.frac, s.frac)})
			}
		}
		acc = next
	}
	out := make([]canonical.ExposureDeclaredLeaf, 0, len(acc))
	for _, p := range acc {
		p.leaf.Share, _ = p.frac.Float64()
		out = append(out, p.leaf)
	}
	return out
}

// jsonField is one key of a JSON object and its raw value.
type jsonField struct {
	key   string
	value json.RawMessage
}

// jsonObject reads b as one JSON object and nothing after it, its keys
// in order. A key stated twice is refused: a decoder would keep the last
// and drop the first without a word.
func jsonObject(b []byte) ([]jsonField, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("not JSON: %w", err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, fmt.Errorf("want a JSON object")
	}
	var out []jsonField
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("not JSON: %w", err)
		}
		key := tok.(string) // inside an object, the decoder hands out a key here
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("not JSON: %w", err)
		}
		if seen[key] {
			return nil, fmt.Errorf("key %q twice", key)
		}
		seen[key] = true
		out = append(out, jsonField{key: key, value: v})
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("not JSON: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("something follows the JSON object")
	}
	return out, nil
}
