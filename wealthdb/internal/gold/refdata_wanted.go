package gold

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The wanted list: what gold asks the refdata collector for. It is the
// one way the collector reads gold, through the hidden read-only
// subcommand `refdata-wanted`, and its JSON is a versioned contract
// (docs/adapters/refdata.md §3). Gold names every fund reference and
// security key; the collector answers them, and the composition it
// stores under a reference joins back exactly.

// WantedContractVersion is the version of the wanted list's JSON. A
// change that removes or renames a field, or changes a field's meaning,
// takes a new version; a field added alone does not.
const WantedContractVersion = 1

// Wanted is the wanted list.
type Wanted struct {
	Contract    string           `json:"contract"`
	Version     int              `json:"version"`
	GeneratedAt string           `json:"generated_at"`
	AsOf        string           `json:"as_of"`
	Settings    WantedSettings   `json:"settings"`
	Funds       []WantedFund     `json:"funds"`
	Securities  []WantedSecurity `json:"securities"`
}

// WantedSettings are the gold settings a collector plans by.
type WantedSettings struct {
	MaxGapDays int `json:"max_gap_days"`
}

// WantedFund is one fund gold wants compositions for.
type WantedFund struct {
	Ref    string  `json:"ref"`
	ISIN   *string `json:"isin"`
	CUSIP  *string `json:"cusip"`
	Symbol *string `json:"symbol"`
	Name   *string `json:"name"`
	// Currency is the currency the holding is priced in.
	Currency *string `json:"currency"`
	// Roles says why the fund is wanted: held, proxy (it stands in for a
	// held fund) or reference (its lines widen the security master).
	Roles []string `json:"roles"`
	// HeldFrom and HeldTo bound the days a composition is wanted for;
	// HeldTo is null while the fund is still held.
	HeldFrom *string `json:"held_from"`
	HeldTo   *string `json:"held_to"`
	// Compositions are the days gold already has a composition for, on
	// any basis, ascending.
	Compositions []string `json:"compositions"`
}

// WantedSecurity is one security gold wants attributes for.
type WantedSecurity struct {
	Key      string  `json:"key"`
	ISIN     *string `json:"isin"`
	CUSIP    *string `json:"cusip"`
	Symbol   *string `json:"symbol"`
	Name     *string `json:"name"`
	Currency *string `json:"currency"`
	// Scope is direct: held directly, and always completed. A collector
	// skips a scope it does not know.
	Scope string `json:"scope"`
	// Missing are the fields no source states yet: sector, country and,
	// for a stock, market_cap.
	Missing []string `json:"missing"`
	// Lookups are the day each provider last answered for the security,
	// so a collector refreshes what is older than its max_age.
	Lookups map[string]string `json:"lookups"`
}

// The asset classes of a fund whose lines describe its exposure, other
// among them: an unclassified fund may well publish lines. A metal or
// coin fund holds what its asset class already says, and a private fund
// publishes no lines.
var lookThroughClasses = []string{"public_equity", "fixed_income", "multi_asset", "real_estate", "infrastructure", "cash", "other"}

// heldInstrument is one instrument as the positions hold it.
type heldInstrument struct {
	src, iid                       string
	assetClass, vehicle            string
	isin, cusip, symbol, name, ccy sql.NullString
	firstAt, lastAt                int64
	open                           bool
}

// RefdataWanted builds the wanted list as of now. A gold opened
// read-only before migration 0125 reached it has no reference or
// exposure tables yet; it is read as holding none, so the first fetch
// can run before the first load.
func RefdataWanted(ctx context.Context, db *sql.DB, now time.Time) (*Wanted, error) {
	held, err := heldInstruments(ctx, db, now)
	if err != nil {
		return nil, err
	}
	migrated, err := hasTable(ctx, db, "exposure_settings")
	if err != nil {
		return nil, err
	}
	settings := canonical.ExposureSettings{MaxGapDays: canonical.DefaultMaxGapDays}
	if migrated {
		if settings, err = exposureSettings(ctx, db); err != nil {
			return nil, err
		}
	}
	w := &Wanted{
		Contract: "refdata-wanted", Version: WantedContractVersion,
		GeneratedAt: now.UTC().Format(time.RFC3339), AsOf: now.UTC().Format(time.DateOnly),
		Settings: WantedSettings{MaxGapDays: settings.MaxGapDays},
		Funds:    []WantedFund{}, Securities: []WantedSecurity{},
	}

	funds := map[string]*WantedFund{}
	spans := map[string]*span{}
	securities := map[string]*WantedSecurity{}
	stocks := map[string]bool{}
	for _, h := range held {
		ref := canonical.SecurityKey(h.isin.String, h.cusip.String, "", h.symbol.String, "")
		if ref == "" {
			continue
		}
		switch {
		case (h.vehicle == "etf" || h.vehicle == "fund") && slices.Contains(lookThroughClasses, h.assetClass):
			f := funds[ref]
			if f == nil {
				f = &WantedFund{Ref: ref, Roles: []string{"held"}}
				funds[ref] = f
				spans[ref] = &span{}
			}
			fillIDs(&f.ISIN, &f.CUSIP, &f.Symbol, &f.Name, &f.Currency, h)
			spans[ref].add(span{from: h.firstAt, to: h.lastAt, open: h.open, set: true})
		case h.vehicle == "stock" || h.vehicle == "bond":
			s := securities[ref]
			if s == nil {
				s = &WantedSecurity{Key: ref, Scope: "direct", Lookups: map[string]string{}}
				securities[ref] = s
				stocks[ref] = h.vehicle == "stock"
			}
			fillIDs(&s.ISIN, &s.CUSIP, &s.Symbol, &s.Name, &s.Currency, h)
		}
	}
	stated := map[[2]string]bool{}
	if migrated {
		if err := addProxiesAndReferences(ctx, db, funds, spans, now); err != nil {
			return nil, err
		}
		if err := addCompositionDates(ctx, db, funds); err != nil {
			return nil, err
		}
		if stated, err = addSecurityState(ctx, db, securities); err != nil {
			return nil, err
		}
	}
	for ref, f := range funds {
		f.HeldFrom, f.HeldTo = spans[ref].dates()
		if f.Compositions == nil {
			f.Compositions = []string{}
		}
	}
	for key, s := range securities {
		s.Missing = []string{}
		for _, wf := range wantedFields {
			if (wf.stocksOnly && !stocks[key]) || stated[[2]string{key, wf.field}] {
				continue
			}
			s.Missing = append(s.Missing, wf.field)
		}
	}
	for _, ref := range slices.Sorted(maps.Keys(funds)) {
		f := funds[ref]
		fillFromKey(ref, &f.ISIN, &f.CUSIP, &f.Symbol)
		w.Funds = append(w.Funds, *f)
	}
	for _, key := range slices.Sorted(maps.Keys(securities)) {
		s := securities[key]
		fillFromKey(key, &s.ISIN, &s.CUSIP, &s.Symbol)
		w.Securities = append(w.Securities, *s)
	}
	return w, nil
}

// fillFromKey states the identifier a key is made of where nothing else
// stated it, such as a proxy's symbol.
func fillFromKey(key string, isin, cusip, symbol **string) {
	prefix, body, _ := strings.Cut(key, ":")
	var dst **string
	switch prefix + ":" {
	case canonical.KeyISIN:
		dst = isin
	case canonical.KeyCUSIP:
		dst = cusip
	case canonical.KeySymbol:
		dst = symbol
	default:
		return
	}
	if *dst == nil {
		*dst = &body
	}
}

// heldInstruments is every instrument a position names with a value or a
// quantity, the span of the snapshots it appears in, and whether it is
// still held. A position's key goes through holding_aliases and
// instrument_aliases() first, as the gains reports read it, so a holding
// a source keys two ways is one instrument. It is still held when its
// source's latest snapshot lists it and that source was observed within
// stillHeldDays of now; a source that stopped reporting ends it at its
// last snapshot.
func heldInstruments(ctx context.Context, db *sql.DB, now time.Time) ([]heldInstrument, error) {
	rows, err := db.QueryContext(ctx, `
WITH last_snap AS (
    SELECT p.silver_source_id, MAX(p.snapshot_at) AS s,
           GREATEST(MAX(p.snapshot_at), COALESCE(MAX(ss.observed_snapshots_through), -1)) AS seen
      FROM positions p LEFT JOIN silver_sources ss ON ss.silver_source_id = p.silver_source_id
     GROUP BY 1),
pos AS (
    SELECT p.*, COALESCE(ha.to_key, hd.to_key, ia.k, p.instrument_external_id) AS iid
      FROM positions p
      LEFT JOIN holding_aliases ha ON ha.silver_source_id = p.silver_source_id
           AND ha.account_external_id = p.account_external_id AND ha.from_key = holding_key(p.instrument_external_id, p.position_key)
      LEFT JOIN holding_aliases hd ON hd.silver_source_id = p.silver_source_id
           AND hd.account_external_id = '' AND hd.from_key = holding_key(p.instrument_external_id, p.position_key)
      LEFT JOIN instrument_aliases() ia ON ia.src = p.silver_source_id AND ia.alias = p.instrument_external_id
     WHERE p.instrument_external_id IS NOT NULL
       AND (COALESCE(p.market_value, 0) <> 0 OR COALESCE(p.quantity, 0) <> 0)),
held AS (
    SELECT p.silver_source_id AS src, p.iid,
           MIN(p.snapshot_at) AS first_at, MAX(p.snapshot_at) AS last_at,
           BOOL_OR(p.snapshot_at = l.s AND l.seen >= ?) AS open,
           arg_max(p.asset_class, p.snapshot_at) AS asset_class,
           arg_max(p.vehicle, p.snapshot_at) AS vehicle,
           arg_max(p.currency, p.snapshot_at) AS ccy
      FROM pos p JOIN last_snap l ON l.silver_source_id = p.silver_source_id
     GROUP BY 1, 2)
SELECT h.src, h.iid, h.asset_class, COALESCE(h.vehicle, ''), i.isin, i.cusip, il.symbol, i.name, h.ccy,
       h.first_at, h.last_at, h.open
  FROM held h
  LEFT JOIN instruments i ON i.silver_source_id = h.src AND i.instrument_external_id = h.iid
  LEFT JOIN instrument_labels() il ON il.silver_source_id = h.src AND il.instrument_external_id = h.iid
 ORDER BY h.src, h.iid`, now.Unix()-stillHeldDays*86400)
	if err != nil {
		return nil, fmt.Errorf("wanted: held instruments: %w", err)
	}
	defer rows.Close()
	var out []heldInstrument
	for rows.Next() {
		var h heldInstrument
		if err := rows.Scan(&h.src, &h.iid, &h.assetClass, &h.vehicle, &h.isin, &h.cusip, &h.symbol, &h.name, &h.ccy,
			&h.firstAt, &h.lastAt, &h.open); err != nil {
			return nil, fmt.Errorf("wanted: held instruments scan: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// stillHeldDays is how recently a source must have been observed for
// what its latest snapshot lists to count as still held.
const stillHeldDays = 90

// fillIDs keeps the first non-blank value of each identifier.
func fillIDs(isin, cusip, symbol, name, ccy **string, h heldInstrument) {
	for _, f := range []struct {
		dst **string
		v   sql.NullString
	}{{isin, h.isin}, {cusip, h.cusip}, {symbol, h.symbol}, {name, h.name}, {ccy, h.ccy}} {
		if *f.dst == nil && f.v.Valid && f.v.String != "" {
			v := f.v.String
			*f.dst = &v
		}
	}
}

// span is the days a fund is wanted for: from its first day to its
// last, open while it is still held.
type span struct {
	from, to  int64
	open, set bool
}

func (s *span) add(o span) {
	if !o.set {
		return
	}
	if !s.set {
		*s = o
		return
	}
	s.from, s.to, s.open = min(s.from, o.from), max(s.to, o.to), s.open || o.open
}

// dates is the span as YYYY-MM-DD bounds, the end nil while open.
func (s *span) dates() (from, to *string) {
	if s == nil || !s.set {
		return nil, nil
	}
	f := time.Unix(s.from, 0).UTC().Format(time.DateOnly)
	if s.open {
		return &f, nil
	}
	t := time.Unix(s.to, 0).UTC().Format(time.DateOnly)
	return &f, &t
}

// addProxiesAndReferences adds the funds the exposure config names: a
// proxy, wanted over the spans of the funds it stands in for, and a
// reference fund, wanted from today.
func addProxiesAndReferences(ctx context.Context, db *sql.DB, funds map[string]*WantedFund,
	spans map[string]*span, now time.Time) error {
	rows, err := db.QueryContext(ctx, `SELECT fund_ref, proxy_ref FROM exposure_proxies ORDER BY 1`)
	if err != nil {
		return fmt.Errorf("wanted: proxies: %w", err)
	}
	type proxy struct{ from, to string }
	var proxies []proxy
	for rows.Next() {
		var p proxy
		if err := rows.Scan(&p.from, &p.to); err != nil {
			rows.Close()
			return err
		}
		proxies = append(proxies, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	want := func(ref, role string) *WantedFund {
		f := funds[ref]
		if f == nil {
			f = &WantedFund{Ref: ref}
			funds[ref] = f
			spans[ref] = &span{}
		}
		addRole(f, role)
		return f
	}
	today := now.UTC().Truncate(24 * time.Hour).Unix()
	for _, p := range proxies {
		want(p.to, "proxy")
		if held := spans[p.from]; held != nil {
			spans[p.to].add(*held)
		}
	}
	// A proxy whose fund is not held is wanted from today, as a
	// reference fund is.
	for _, p := range proxies {
		if !spans[p.to].set {
			spans[p.to].add(span{from: today, to: today, open: true, set: true})
		}
	}
	refs, err := db.QueryContext(ctx, `SELECT fund_ref FROM exposure_reference_funds ORDER BY 1`)
	if err != nil {
		return fmt.Errorf("wanted: reference funds: %w", err)
	}
	defer refs.Close()
	for refs.Next() {
		var ref string
		if err := refs.Scan(&ref); err != nil {
			return err
		}
		want(ref, "reference")
		spans[ref].add(span{from: today, to: today, open: true, set: true})
	}
	return refs.Err()
}

func addRole(f *WantedFund, role string) {
	if !slices.Contains(f.Roles, role) {
		f.Roles = append(f.Roles, role)
	}
}

// addCompositionDates lists the days gold has a composition for, per
// wanted fund.
func addCompositionDates(ctx context.Context, db *sql.DB, funds map[string]*WantedFund) error {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT fund_ref, as_of FROM fund_compositions ORDER BY 1, 2`)
	if err != nil {
		return fmt.Errorf("wanted: composition dates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ref string
		var asOf int64
		if err := rows.Scan(&ref, &asOf); err != nil {
			return err
		}
		if f := funds[ref]; f != nil {
			f.Compositions = append(f.Compositions, time.Unix(asOf, 0).UTC().Format(time.DateOnly))
		}
	}
	return rows.Err()
}

// The fields a security wants, and the facts and fund-line columns that
// state each. A bond wants no market cap.
var wantedFields = []struct {
	field      string
	facts      []string
	lineColumn string
	stocksOnly bool
}{
	{"sector", []string{"sector", "sic"}, "sector", false},
	{"country", []string{"risk_country", "hq_country", "legal_country", "incorporation"}, "country", false},
	{"market_cap", []string{"market_cap", "shares_outstanding", "size"}, "", true},
}

// addSecurityState fills the day each provider last answered for each
// security, and returns which (key, field) pairs some source states.
func addSecurityState(ctx context.Context, db *sql.DB, securities map[string]*WantedSecurity) (map[[2]string]bool, error) {
	rows, err := db.QueryContext(ctx, `
SELECT security_key, provider, MAX(as_of) FROM (
    SELECT security_key, provider, as_of FROM security_facts
    UNION ALL SELECT security_key, provider, as_of FROM security_identities)
 GROUP BY 1, 2`)
	if err != nil {
		return nil, fmt.Errorf("wanted: lookups: %w", err)
	}
	for rows.Next() {
		var key, provider string
		var asOf int64
		if err := rows.Scan(&key, &provider, &asOf); err != nil {
			rows.Close()
			return nil, err
		}
		if s := securities[key]; s != nil {
			s.Lookups[provider] = time.Unix(asOf, 0).UTC().Format(time.DateOnly)
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	stated := map[[2]string]bool{}
	for _, wf := range wantedFields {
		q := `SELECT DISTINCT security_key FROM security_facts WHERE field IN (` + placeholders(len(wf.facts)) + `)`
		args := make([]any, len(wf.facts))
		for i, f := range wf.facts {
			args[i] = f
		}
		if wf.lineColumn != "" {
			q += ` UNION SELECT DISTINCT security_key FROM fund_lines WHERE ` + wf.lineColumn + ` IS NOT NULL AND security_key IS NOT NULL`
		}
		keys, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("wanted: %s: %w", wf.field, err)
		}
		for keys.Next() {
			var key string
			if err := keys.Scan(&key); err != nil {
				keys.Close()
				return nil, err
			}
			stated[[2]string{key, wf.field}] = true
		}
		if err := keys.Err(); err != nil {
			keys.Close()
			return nil, err
		}
		keys.Close()
	}
	return stated, nil
}

func placeholders(n int) string {
	out := make([]byte, 0, 3*n)
	for i := range n {
		if i > 0 {
			out = append(out, ", "...)
		}
		out = append(out, '?')
	}
	return string(out)
}

// hasTable reports whether gold holds a table of that name.
func hasTable(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_name = ?`, name).Scan(&n)
	return n > 0, err
}

// exposureSettings reads the settings row, the defaults where a gold
// never loaded since the exposure tables arrived has none.
func exposureSettings(ctx context.Context, db *sql.DB) (canonical.ExposureSettings, error) {
	s := canonical.ExposureSettings{MaxGapDays: canonical.DefaultMaxGapDays,
		AgedDays: canonical.DefaultAgedDays, PartialPct: canonical.DefaultPartialPct}
	err := db.QueryRowContext(ctx, `SELECT max_gap_days, aged_days, partial_pct FROM exposure_settings LIMIT 1`).
		Scan(&s.MaxGapDays, &s.AgedDays, &s.PartialPct)
	if err == sql.ErrNoRows {
		err = nil
	}
	return s, err
}
