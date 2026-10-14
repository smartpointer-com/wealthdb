package gold

import (
	"cmp"
	"context"
	"database/sql"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// A depositary receipt (an ADR, a GDR) stands for shares of a company
// listed elsewhere, and takes that share's sector, country and domicile
// where no source states its own. Where GLEIF maps the receipt's ISIN to
// an issuer and lists that issuer's other ISINs, the share is the equity
// line among them. Else, since no free source states which share an
// over-the-counter receipt stands for (the depositaries' directories
// forbid automated reading, and OpenFIGI states no ISIN), the exposure
// pass finds the share by the issuer's name among the non-US equity
// shares the fund lines describe.
//
// The match is strict, since a wrong one would pass unnoticed:
//
//   - Names are reduced to the issuer's words. The receipt's wording
//     (ADR, sponsored, each representing two shares), the legal form,
//     single letters and numbers drop out. A share keeps what its
//     parentheses say (NAME (M) is another company than NAME).
//   - Each name the receipt goes by is matched on its own. Equal words
//     match first. Else the receipt's words lead the share's, each the
//     start of the share's word in its place or the other way round,
//     the first equal or, at six letters or more, the start of the
//     share's first word. A share whose further words name a country
//     the receipt's names do not is no lead (NAME INDIA for NAME). A
//     name of one word leads only a share with one telling word (NAME
//     BANK, NAMECO for NAME). The first tier with any hit decides, even
//     where none of its shares fits.
//   - A legal form in a receipt's name tells where the company sits (AG,
//     SPA, PLC, BHD); a share outside those countries does not fit. A
//     receipt whose country a source states fits only a share of that
//     country.
//   - A name whose fitting hits are one issuer's (they share an ISIN or
//     a name) in one country votes for it; any other name abstains. The
//     votes must agree. Anything else stays unmatched.

// receiptSQL lists every security that lacks a sector or a risk country,
// with each name a lookup, a broker or a fund line gives it and whether
// a lookup calls it a depositary receipt.
const receiptSQL = `
WITH names AS (
    SELECT COALESCE(sa.security_key, i.security_key) AS k, i.name,
           i.security_type = 'Depositary Receipt' AS receipt
      FROM security_identities i LEFT JOIN security_aliases sa ON sa.alias = i.security_key
    UNION ALL
    SELECT COALESCE(sa.security_key, ik.security_key), ins.name, FALSE
      FROM instrument_keys ik
      JOIN instruments ins USING (silver_source_id, instrument_external_id)
      LEFT JOIN security_aliases sa ON sa.alias = ik.security_key
    UNION ALL
    SELECT DISTINCT COALESCE(sa.security_key, l.security_key), l.name, FALSE
      FROM fund_lines l LEFT JOIN security_aliases sa ON sa.alias = l.security_key
     WHERE l.security_key IS NOT NULL)
SELECT s.security_key,
       COALESCE(s.risk_country, CASE WHEN s.domicile_origin <> 'isin_prefix' THEN s.domicile END, ''),
       n.name, COALESCE(n.receipt, FALSE)
  FROM securities s JOIN names n ON n.k = s.security_key
 WHERE (s.sector IS NULL OR s.risk_country IS NULL) AND n.name IS NOT NULL AND trim(n.name) <> ''`

// shareSQL lists the non-US equity shares a receipt may stand for:
// each security a fund lists as an equity line under an ISIN from
// outside the US, with a sector, its country of risk and domicile, and
// every name the fund lines and the lookups give it.
const shareSQL = `
WITH eq AS (
    SELECT DISTINCT COALESCE(sa.security_key, l.security_key) AS k
      FROM fund_lines l
      JOIN fund_compositions c USING (silver_source_id, fund_ref, as_of, basis)
      JOIN fund_line_labels lab ON lab.provider = c.provider AND lab.field = 'asset_type' AND lab.raw = l.asset_type
      LEFT JOIN security_aliases sa ON sa.alias = l.security_key
     WHERE l.security_key IS NOT NULL AND lab.value = 'public_equity'),
names AS (
    SELECT COALESCE(sa.security_key, l.security_key) AS k, l.name
      FROM fund_lines l LEFT JOIN security_aliases sa ON sa.alias = l.security_key
     WHERE l.security_key IS NOT NULL AND l.name IS NOT NULL
    UNION SELECT security_key, name FROM securities WHERE name IS NOT NULL)
SELECT DISTINCT s.security_key, COALESCE(s.risk_country, ''), COALESCE(s.domicile, ''), n.name
  FROM securities s JOIN eq ON eq.k = s.security_key JOIN names n ON n.k = s.security_key
 WHERE s.security_key LIKE 'isin:%' AND s.security_key NOT LIKE 'isin:US%' AND s.sector IS NOT NULL`

// issuerSQL lists each receipt GLEIF ties to its issuer's other ISINs,
// with those ISINs, where the receipt lacks a sector or a risk country.
const issuerSQL = `
WITH f AS (
    SELECT DISTINCT COALESCE(sa.security_key, f.security_key) AS k, 'isin:' || upper(trim(i.isin)) AS share
      FROM security_facts f LEFT JOIN security_aliases sa ON sa.alias = f.security_key,
           unnest(string_split(f.value, ' ')) AS i(isin)
     WHERE f.field = 'issuer_isins' AND trim(i.isin) <> '')
SELECT f.k, f.share FROM f JOIN securities s ON s.security_key = f.k
 WHERE s.sector IS NULL OR s.risk_country IS NULL`

// receiptFillSQL completes each receipt from its share: a sector or a
// risk country no source states, and a domicile only an ISIN's prefix
// implies (a receipt's ISIN is the depositary's). The origin names the
// share and how it was found: lei:… by GLEIF, underlying:… by name.
const receiptFillSQL = `
UPDATE securities SET
    sector = COALESCE(securities.sector, u.sector),
    sector_origin = CASE WHEN securities.sector IS NULL AND u.sector IS NOT NULL
                         THEN x.how || u.security_key ELSE securities.sector_origin END,
    risk_country = COALESCE(securities.risk_country, u.risk_country),
    risk_country_origin = CASE WHEN securities.risk_country IS NULL AND u.risk_country IS NOT NULL
                               THEN x.how || u.security_key ELSE securities.risk_country_origin END,
    domicile = CASE WHEN COALESCE(securities.domicile_origin, 'isin_prefix') = 'isin_prefix' AND u.domicile IS NOT NULL
                    THEN u.domicile ELSE securities.domicile END,
    domicile_origin = CASE WHEN COALESCE(securities.domicile_origin, 'isin_prefix') = 'isin_prefix' AND u.domicile IS NOT NULL
                           THEN x.how || u.security_key ELSE securities.domicile_origin END
  FROM (SELECT *, CASE origin WHEN 'lei' THEN 'lei:' ELSE 'underlying:' END AS how FROM security_underlyings) x
  JOIN securities u ON u.security_key = x.underlying_key
 WHERE x.security_key = securities.security_key`

// writeReceipts rewrites security_underlyings and completes each
// receipt from the share it stands for.
func writeReceipts(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM security_underlyings`); err != nil {
		return err
	}
	receipts := map[string]*receiptNames{}
	if err := scanAll(ctx, tx, receiptSQL, func(rows *sql.Rows) error {
		var key, country, name string
		var receipt bool
		if err := rows.Scan(&key, &country, &name, &receipt); err != nil {
			return err
		}
		r := receipts[key]
		if r == nil {
			r = &receiptNames{country: country}
			receipts[key] = r
		}
		r.names = append(r.names, name)
		r.receipt = r.receipt || receipt || receiptWording.MatchString(strings.ToUpper(name))
		return nil
	}); err != nil {
		return err
	}
	var shares []shareName
	if err := scanAll(ctx, tx, shareSQL, func(rows *sql.Rows) error {
		var s shareName
		if err := rows.Scan(&s.key, &s.country, &s.domicile, &s.name); err != nil {
			return err
		}
		shares = append(shares, s)
		return nil
	}); err != nil {
		return err
	}
	isShare := map[string]bool{}
	for _, sh := range shares {
		isShare[sh.key] = true
	}
	// By GLEIF first: the first equity share among the issuer's ISINs.
	byLEI := map[string]string{}
	if err := scanAll(ctx, tx, issuerSQL, func(rows *sql.Rows) error {
		var k, share string
		if err := rows.Scan(&k, &share); err != nil {
			return err
		}
		if isShare[share] && (byLEI[k] == "" || share < byLEI[k]) {
			byLEI[k] = share
		}
		return nil
	}); err != nil {
		return err
	}
	for k := range byLEI {
		delete(receipts, k)
	}
	byName := linkReceipts(receipts, shares)
	type link struct{ key, share, origin string }
	var links []link
	for _, k := range slices.Sorted(maps.Keys(byLEI)) {
		links = append(links, link{k, byLEI[k], "lei"})
	}
	for _, k := range slices.Sorted(maps.Keys(byName)) {
		links = append(links, link{k, byName[k], "name"})
	}
	if err := InsertChunked(ctx, tx, "security_underlyings",
		`INSERT INTO security_underlyings (security_key, underlying_key, origin) VALUES `, `(?, ?, ?)`, len(links),
		func(i int, args []any) []any { return append(args, links[i].key, links[i].share, links[i].origin) }); err != nil {
		return err
	}
	return execAll(ctx, tx, receiptFillSQL)
}

func scanAll(ctx context.Context, tx *sql.Tx, q string, scan func(*sql.Rows) error) error {
	rows, err := tx.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// receiptNames is a security that may be a receipt: every name it goes
// by, the country a source states for it, and whether anything calls it
// a depositary receipt.
type receiptNames struct {
	names   []string
	country string
	receipt bool
}

// shareName is one name of a share a receipt may stand for.
type shareName struct {
	key, country, domicile, name string
}

// shareEntry is a share by one of its names, reduced to its words.
type shareEntry struct {
	words                  []string
	joined                 string
	key, country, domicile string
}

// shareIndex finds shares by their words.
type shareIndex struct {
	exact, byFirst map[string][]shareEntry
}

func newShareIndex(shares []shareName) shareIndex {
	idx := shareIndex{exact: map[string][]shareEntry{}, byFirst: map[string][]shareEntry{}}
	for _, s := range shares {
		w := issuerWords(s.name, false)
		if len(w) == 0 {
			continue
		}
		e := shareEntry{words: w, joined: strings.Join(w, " "), key: s.key, country: s.country, domicile: s.domicile}
		idx.exact[e.joined] = append(idx.exact[e.joined], e)
		idx.byFirst[w[0]] = append(idx.byFirst[w[0]], e)
	}
	return idx
}

// linkReceipts maps each receipt's key to the share it stands for.
func linkReceipts(receipts map[string]*receiptNames, shares []shareName) map[string]string {
	idx := newShareIndex(shares)
	out := map[string]string{}
	for key, r := range receipts {
		if !r.receipt {
			continue
		}
		hint := legalHint(r.names)
		said := map[string]bool{}
		for _, n := range r.names {
			for _, w := range nameTokens(n) {
				said[w] = true
			}
		}
		var votes []shareEntry
		for _, n := range r.names {
			if hits := idx.match(issuerWords(n, true), r.country, hint, said); len(hits) > 0 {
				if _, ok := oneShare(hits); ok {
					votes = append(votes, hits...)
				}
			}
		}
		if share, ok := oneShare(votes); ok && share != key {
			out[key] = share
		}
	}
	return out
}

// match is what one name of a receipt finds: the hits of the first tier
// that has any, those among them that fit the receipt's country and the
// hint its legal form gives. Within a tier, a name without its last word
// cut short is tried only where the whole name finds nothing. said holds
// every word the receipt's names state.
func (idx shareIndex) match(words []string, country string, hint, said map[string]bool) []shareEntry {
	fits := func(es []shareEntry) []shareEntry {
		var out []shareEntry
		for _, e := range es {
			if (country == "" || e.country == country || e.domicile == country) &&
				(hint == nil || hint[e.country] || hint[e.domicile]) {
				out = append(out, e)
			}
		}
		return out
	}
	tiers := []func([]string) []shareEntry{
		func(w []string) []shareEntry { return idx.exact[strings.Join(w, " ")] },
		func(w []string) []shareEntry {
			var out []shareEntry
			for _, e := range idx.candidates(w[0]) {
				if leads(w, e.words, said) {
					out = append(out, e)
				}
			}
			return out
		},
	}
	for _, tier := range tiers {
		for _, w := range receiptVariants(words) {
			if hits := tier(w); len(hits) > 0 {
				return fits(hits)
			}
		}
	}
	return nil
}

// leads reports whether a receipt's words lead a share's name. A share
// whose further words name a country the receipt never names is a
// subsidiary there, not the issuer. One word leads only a share with one
// telling word: that word, or that word cut short.
func leads(receipt, share []string, said map[string]bool) bool {
	if !leadingWords(receipt, share) {
		return false
	}
	if len(receipt) == 1 {
		var telling []string
		for _, w := range share {
			if !genericWords[w] {
				telling = append(telling, w)
			}
		}
		return len(telling) == 1 && telling[0] == share[0]
	}
	for _, w := range share[len(receipt):] {
		if countryWords[w] && !said[w] {
			return false
		}
	}
	return true
}

// candidates are the shares whose first word is first, or, for a first
// word of six letters or more, starts with it.
func (idx shareIndex) candidates(first string) []shareEntry {
	if len(first) < 6 {
		return idx.byFirst[first]
	}
	var out []shareEntry
	for w, es := range idx.byFirst {
		if strings.HasPrefix(w, first) {
			out = append(out, es...)
		}
	}
	return out
}

// oneShare is the share the hits name, when they are one issuer's in one
// country: linked through shared keys and shared names. Of an issuer's
// several shares, the first by key.
func oneShare(hits []shareEntry) (string, bool) {
	if len(hits) == 0 {
		return "", false
	}
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if parent[x] == "" || parent[x] == x {
			parent[x] = x
			return x
		}
		parent[x] = find(parent[x])
		return parent[x]
	}
	countries := map[string]bool{}
	best := hits[0].key
	for _, e := range hits {
		parent[find("k:"+e.key)] = find("n:" + e.joined)
		countries[cmp.Or(e.country, e.domicile)] = true
		best = min(best, e.key)
	}
	roots := map[string]bool{}
	for _, e := range hits {
		roots[find("k:"+e.key)] = true
	}
	return best, len(roots) == 1 && len(countries) == 1
}

// receiptVariants are the word lists a receipt's name is matched by, in
// order: its issuer's words, and the same without a last word that may
// be one cut short: one or two letters (HOLDINGS IN for HOLDINGS INC), or
// the start of a long legal form (COR for CORP), where two words remain.
// A list that names no issuer is left out.
func receiptVariants(words []string) [][]string {
	var out [][]string
	if distinctive(words) {
		out = append(out, words)
	}
	if n := len(words); n > 2 && cutShort(words[n-1]) && distinctive(words[:n-1]) {
		out = append(out, words[:n-1])
	}
	return out
}

// cutShort reports whether a last word may be a word cut short.
func cutShort(w string) bool {
	if len(w) <= 2 {
		return true
	}
	for _, form := range []string{"CORP", "CORPORATION", "LIMITED", "INCORPORATED", "COMPANY"} {
		if len(w) < len(form) && strings.HasPrefix(form, w) {
			return true
		}
	}
	return false
}

// receiptWording tells a receipt by its name.
var receiptWording = regexp.MustCompile(`\b(ADRS?|ADS|GDRS?)\b|DEPOSIT(ARY|ORY)?\s+RECEIPT`)

var (
	nameParens  = regexp.MustCompile(`\([^)]*\)`)
	nameSplit   = regexp.MustCompile(`[^A-Z0-9]+`)
	nameDigits  = regexp.MustCompile(`^[0-9]+$`)
	nameInitial = regexp.MustCompile(`\b([A-Z])\.([A-Z])\.(?:([A-Z])\.)?`)
)

// receiptWords are what a receipt's name says of the receipt rather than
// of its issuer, and the share-class words that do not tell two issuers
// apart.
var receiptWords = setOf("ADR", "ADRS", "ADS", "GDR", "GDRS", "DR", "DRS", "AMERICAN", "GLOBAL", "DEPOSITARY",
	"DEPOSITORY", "DEPOSIT", "RECEIPT", "RECEIPTS", "SHARE", "SHARES", "SHS", "SH", "ORD", "ORDS", "ORDINARY",
	"EACH", "EA", "REP", "REPR", "REPRESENTING", "REPRESENTS", "REG", "REGS", "LEVEL", "NPV", "UNITARY", "NEW", "CLASS",
	"CL", "PAR", "PREF", "PFD", "PREFERENCE", "BEARER", "REGISTERED", "UNS", "SP", "SPN", "UN", "EDR")

// legalWords are legal forms and filler, "and" in the languages issuers
// name themselves in among it.
var legalWords = setOf("AG", "SE", "SA", "SAU", "NV", "PLC", "LTD", "LIMITED", "CORP", "CORPORATION", "CO",
	"COMPANY", "INC", "INCORPORATED", "SPA", "AB", "PUBL", "ASA", "AS", "OYJ", "ABP", "KK", "BHD", "TBK", "PCL",
	"LTDA", "SAB", "DE", "CV", "GMBH", "KGAA", "THE", "AND", "UND", "ET", "LLC", "LP", "NL", "BV", "SAS", "SCA",
	"SGPS", "PJSC", "OJSC", "JSC")

// legalCountries are the countries a legal form says a company sits in.
// A form many countries use (LTD, INC, CORP) says nothing and is absent.
var legalCountries = func() map[string]map[string]bool {
	eea := "AT BE BG CY CZ DE DK EE ES FI FR GR HR HU IE IS IT LI LT LU LV MT NL NO PL PT RO SE SI SK"
	m := map[string]string{
		"AG": "DE CH AT LI LU", "SE": eea, "SA": "FR ES BE LU CH PT PL GR BR AR CL MX CO PE UY MA",
		"SAU": "ES", "SPA": "IT", "NV": "NL BE CW", "PLC": "GB IE JE GG IM CY MT", "AB": "SE", "ASA": "NO",
		"AS": "NO DK EE", "OYJ": "FI", "ABP": "FI", "KK": "JP", "BHD": "MY", "TBK": "ID", "PCL": "TH",
		"SAB": "MX", "KGAA": "DE", "GMBH": "DE AT CH", "SGPS": "PT",
	}
	out := map[string]map[string]bool{}
	for form, cs := range m {
		out[form] = setOf(strings.Fields(cs)...)
	}
	return out
}()

// abbreviations are the words a vendor's short name abbreviates other
// than by cutting them short.
var abbreviations = map[string]string{"INTL": "INTERNATIONAL", "HLDGS": "HOLDINGS", "HLDG": "HOLDING",
	"INDS": "INDUSTRIES", "MFG": "MANUFACTURING", "GRP": "GROUP", "BK": "BANK", "SVCS": "SERVICES",
	"FINL": "FINANCIAL", "NATL": "NATIONAL"}

// countryWords name a country or a region in a company's name: a share
// whose name adds one is a subsidiary there.
var countryWords = setOf("INDIA", "MALAYSIA", "THAILAND", "INDONESIA", "PHILIPPINES", "VIETNAM", "PAKISTAN",
	"BANGLADESH", "LANKA", "CHINA", "HONG", "KONG", "TAIWAN", "KOREA", "JAPAN", "SINGAPORE", "AUSTRALIA",
	"ZEALAND", "BRASIL", "BRAZIL", "MEXICO", "ARGENTINA", "CHILE", "PERU", "COLOMBIA", "CANADA", "AMERICA",
	"AMERICAS", "USA", "UK", "IRELAND", "DEUTSCHLAND", "GERMANY", "FRANCE", "ESPANA", "SPAIN", "ITALIA", "ITALY",
	"PORTUGAL", "NEDERLAND", "BELGIUM", "BELGIQUE", "SUISSE", "SCHWEIZ", "SWITZERLAND", "AUSTRIA", "OSTERREICH",
	"POLSKA", "POLAND", "CZECH", "HUNGARY", "ROMANIA", "GREECE", "HELLAS", "TURKIYE", "TURKEY", "ISRAEL", "EGYPT",
	"NIGERIA", "KENYA", "AFRICA", "SAUDI", "ARABIA", "EMIRATES", "QATAR", "KUWAIT", "NORGE", "NORWAY", "SVERIGE",
	"SWEDEN", "DANMARK", "DENMARK", "SUOMI", "FINLAND", "ASIA", "EUROPE", "EUROPA", "LATAM", "PACIFIC", "NORDIC",
	"IBERIA")

// genericWords say nothing of which issuer: a name of these alone is
// matched to nothing.
var genericWords = setOf("HOLDINGS", "HOLDING", "HLDGS", "HLDG", "GROUP", "GRUPPE", "GRP", "INTERNATIONAL", "INTL",
	"INDUSTRIES", "INDUSTRY", "BANK", "FINANCIAL", "OF")

func setOf(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

// nameTokens splits a name into upper-case words, an initialism (S.P.A.,
// N.V.) written as one word and A/S as AS.
func nameTokens(name string) []string {
	n := strings.ToUpper(name)
	n = strings.ReplaceAll(n, "'", "")
	n = strings.ReplaceAll(n, "A/S", "AS")
	n = nameInitial.ReplaceAllString(n, "$1$2$3")
	return nameSplit.Split(n, -1)
}

// issuerWords reduces a name to its issuer's words. A receipt's
// parentheses (REPR 2 SHS) drop; what a share's say (MALAYSIA, M) stays,
// a single letter too.
func issuerWords(name string, receipt bool) []string {
	var out []string
	add := func(text string, keepLetters bool) {
		for _, w := range nameTokens(text) {
			switch {
			case w == "" || nameDigits.MatchString(w) || (len(w) < 2 && !keepLetters),
				receiptWords[w], legalWords[w],
				strings.HasPrefix(w, "SPON"), strings.HasPrefix(w, "UNSP"), strings.HasPrefix(w, "AKTIENGES"):
				continue
			}
			if full, ok := abbreviations[w]; ok {
				w = full
			}
			out = append(out, w)
		}
	}
	if receipt {
		add(nameParens.ReplaceAllString(name, " "), false)
		return out
	}
	last := 0
	for _, m := range nameParens.FindAllStringIndex(name, -1) {
		add(name[last:m[0]], false)
		add(name[m[0]:m[1]], true)
		last = m[1]
	}
	add(name[last:], false)
	return out
}

// legalHint is the countries the legal forms in a receipt's names allow,
// nil where no name states a telling one.
func legalHint(names []string) map[string]bool {
	var hint map[string]bool
	for _, n := range names {
		for _, w := range nameTokens(nameParens.ReplaceAllString(n, " ")) {
			if cs := legalCountries[w]; cs != nil {
				if hint == nil {
					hint = map[string]bool{}
				}
				for c := range cs {
					hint[c] = true
				}
			}
		}
	}
	return hint
}

// distinctive reports whether the words name an issuer at all.
func distinctive(words []string) bool {
	for _, w := range words {
		if !genericWords[w] && len(w) >= 2 {
			return true
		}
	}
	return false
}

// leadingWords reports whether a receipt's words lead a share's name:
// the first word equal (or, at six letters or more, the start of the
// share's), each later one the start of the share's word in its place
// or the other way round.
func leadingWords(receipt, share []string) bool {
	if len(receipt) == 0 || len(receipt) > len(share) {
		return false
	}
	if receipt[0] != share[0] && (len(receipt[0]) < 6 || !strings.HasPrefix(share[0], receipt[0])) {
		return false
	}
	for i := 1; i < len(receipt); i++ {
		if !strings.HasPrefix(share[i], receipt[i]) && !strings.HasPrefix(receipt[i], share[i]) {
			return false
		}
	}
	return true
}
