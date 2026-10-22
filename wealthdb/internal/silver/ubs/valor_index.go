package ubs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// The valor index: a Swiss VALOR resolved to the instrument gold holds
// under it, so a trade that names its security in free text and carries
// no id — which is most of what a printed account statement says about
// a trade — reaches an identity rather than a guess.
//
// A valor identifies a security; a security NAME does not, and no road
// here reads one. Every road below is a correspondence a feed states
// outright or one arithmetic recovers exactly.

// valorIndex accumulates that correspondence from every feed stating
// one, under a single collision rule.
//
// A valor naming more than one instrument is dropped rather than
// resolved, and stays dropped however many later roads offer an answer
// for it. That should not happen — a valor identifies one security
// line, which is the whole reason this is an identity — so a collision
// means an assumption here is wrong, and the honest answer to a wrong
// assumption is no answer. The rule has to hold ACROSS feeds, not
// within each: two feeds disagreeing about what a valor names is the
// same wrong assumption, found the only way it can be.
type valorIndex struct {
	byValor   map[string]string
	ambiguous map[string]bool
}

func newValorIndex() *valorIndex {
	return &valorIndex{byValor: map[string]string{}, ambiguous: map[string]bool{}}
}

func (x *valorIndex) add(valor, isin string) {
	if valor == "" || isin == "" || x.ambiguous[valor] {
		return
	}
	if held, ok := x.byValor[valor]; ok && held != isin {
		delete(x.byValor, valor)
		x.ambiguous[valor] = true
		return
	}
	x.byValor[valor] = isin
}

// valorIndex is buildValorIndex over this reader and the connection's
// feed reader, psn. The first call reads it and later calls return the
// same map: a connection pairs one reader of each silver, so psn is the
// same on every call.
func (r *webReader) valorIndex(ctx context.Context, psn *psnReader) (map[string]string, error) {
	if r.valors != nil {
		return r.valors, nil
	}
	x, err := buildValorIndex(ctx, psn, r)
	if err != nil {
		return nil, err
	}
	r.valors = x
	return x, nil
}

// buildValorIndex reads every road both silvers offer.
//
// From the feed's instrument dimension, two — because it states the
// identifier only sometimes:
//
//   - `payload.InstrIdtfr.Valor`, where it carries the identifier
//     object at all.
//   - THE ISIN ITSELF, for a Swiss line. A `CH` ISIN is `CH` plus the
//     valor zero-padded to nine digits plus a check digit, so
//     `CH0012345678` IS valor 1234567. This reaches the instruments
//     the first road cannot.
//
// And two from the web silver, each printing the valor and the ISIN of
// one security together:
//
//   - The portfolio transaction list, in adjacent columns of the same
//     row. It reaches a security a managed portfolio traded but the
//     feed's instrument dimension never described, whose non-Swiss ISIN
//     no arithmetic recovers. Those are exactly the securities the
//     printed statements name by valor, so without it a statement-era
//     trade in one of them reaches gold identified by nothing.
//   - The statements of assets, on each position's `Valor … - ISIN …`
//     line. It reaches a security held but never traded through the
//     list, such as one whose dividends name it by valor alone.
//
// A nil or absent side contributes nothing rather than failing: an
// index that resolves less is the right answer for a silver that says
// less, and an empty one resolves nothing at all — which is also right.
func buildValorIndex(ctx context.Context, psn *psnReader, web *webReader) (map[string]string, error) {
	x := newValorIndex()
	if err := psn.addInstrumentValors(ctx, x); err != nil {
		return nil, err
	}
	if err := web.addPortfolioValors(ctx, x); err != nil {
		return nil, err
	}
	if err := web.addStatementValors(ctx, x); err != nil {
		return nil, err
	}
	return x.byValor, nil
}

// addInstrumentValors reads the feed's instrument dimension.
func (r *psnReader) addInstrumentValors(ctx context.Context, x *valorIndex) error {
	if r == nil || r.db == nil {
		return nil
	}
	rows, err := r.db.QueryContext(ctx, `SELECT isin, payload FROM instruments`)
	if err != nil {
		return fmt.Errorf("psn addInstrumentValors: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var isin, payload string
		if err := rows.Scan(&isin, &payload); err != nil {
			return fmt.Errorf("psn addInstrumentValors scan: %w", err)
		}
		var p struct {
			InstrIdtfr struct {
				Valor string `json:"Valor"`
			} `json:"InstrIdtfr"`
		}
		_ = json.Unmarshal([]byte(payload), &p)
		x.add(normalizeValor(p.InstrIdtfr.Valor), isin)
		x.add(valorFromSwissISIN(isin), isin)
	}
	return rows.Err()
}

// addPortfolioValors reads the portfolio transaction list, which names
// the security it traded both ways on the same row.
func (r *webReader) addPortfolioValors(ctx context.Context, x *valorIndex) error {
	if r == nil || r.db == nil {
		return nil
	}
	ok, err := r.hasTable(ctx, "portfolio_transactions")
	if err != nil || !ok {
		return err
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT valor, isin FROM portfolio_transactions
 WHERE valor IS NOT NULL AND isin IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("ubs-web addPortfolioValors: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var valor, isin string
		if err := rows.Scan(&valor, &isin); err != nil {
			return fmt.Errorf("ubs-web addPortfolioValors scan: %w", err)
		}
		x.add(normalizeValor(valor), strings.TrimSpace(isin))
	}
	return rows.Err()
}

// addStatementValors reads the statements of assets, which print each
// position's valor beside its ISIN.
func (r *webReader) addStatementValors(ctx context.Context, x *valorIndex) error {
	if r == nil || r.db == nil {
		return nil
	}
	ok, err := r.hasTable(ctx, "historical_position_snapshots")
	if err != nil || !ok {
		return err
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT DISTINCT json_extract(payload, '$.valor'), instrument_isin
  FROM historical_position_snapshots
 WHERE instrument_isin IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("ubs-web addStatementValors: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var valor sql.NullString
		var isin string
		if err := rows.Scan(&valor, &isin); err != nil {
			return fmt.Errorf("ubs-web addStatementValors scan: %w", err)
		}
		if isin = strings.TrimSpace(isin); looksLikeISIN(isin) {
			x.add(normalizeValor(valor.String), isin)
		}
	}
	return rows.Err()
}

// valorFromSwissISIN recovers the valor a Swiss ISIN is built from: the
// nine digits between the `CH` prefix and the trailing check digit,
// with leading zeros dropped. Empty for anything that is not a
// twelve-character CH ISIN of digits.
func valorFromSwissISIN(isin string) string {
	if len(isin) != 12 || !strings.HasPrefix(isin, "CH") {
		return ""
	}
	return normalizeValor(isin[2:11])
}

// normalizeValor drops leading zeros and refuses anything that is not
// all digits, so every road above agrees on one spelling of a number.
func normalizeValor(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	for _, c := range v {
		if c < '0' || c > '9' {
			return ""
		}
	}
	return strings.TrimLeft(v, "0")
}

// resolveValor looks a stated valor up in the index: the instrument gold
// holds under it or, for a well-formed valor naming none, the valor
// itself as the hint a config link closes. Both are empty when no valor
// is stated.
func resolveValor(index map[string]string, raw string) (id *string, hint string) {
	v := normalizeValor(raw)
	if v == "" {
		return nil, ""
	}
	if isin, ok := index[v]; ok {
		return &isin, ""
	}
	return nil, v
}
