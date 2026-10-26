package gold

import (
	"cmp"
	"context"
	"database/sql"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// A depositary receipt (an ADR, a GDR) stands for shares of a company
// listed elsewhere, and takes that share's sector, country and domicile
// where no source states its own. GLEIF names the share where it maps
// the receipt to an issuer with other ISINs. Else, since no free source
// states which share an over-the-counter receipt stands for, the pass
// finds it by the issuer's name among the non-US equity shares the fund
// lines describe. A line a report names by name alone finds its share
// the same way, first among the lines of its fund's proxy. The match is
// strict, since a wrong one would pass unnoticed; docs/EXPOSURE.md §3
// states its rules.

// receiptType is the security type a lookup (OpenFIGI) gives a
// depositary receipt.
const receiptType = "Depositary Receipt"

// securityNamesSQL is a CTE, names: each name a lookup, a broker or a
// fund line gives a security (k), and whether a lookup calls it a
// depositary receipt.
const securityNamesSQL = `names AS (
    SELECT exposure_key(security_key) AS k, name, security_type = '` + receiptType + `' AS receipt
      FROM security_identities
    UNION ALL
    SELECT exposure_key(ik.security_key), ins.name, FALSE
      FROM instrument_keys ik JOIN instruments ins USING (silver_source_id, instrument_external_id)
    UNION ALL
    SELECT DISTINCT exposure_key(security_key), name, FALSE FROM fund_lines WHERE security_key IS NOT NULL)`

// receiptKeysSQL is a CTE, receipt_keys: each security (key) gold reads
// as a depositary receipt, docs/EXPOSURE.md §13. A lookup's type says
// so, GLEIF ties it to its issuer's other ISINs, or a name a lookup, a
// broker or a fund line gives it reads as one (the parameter is
// receiptWording) under a US ISIN or none. It reads the CTEs names
// (securityNamesSQL) and isins (statedISINsSQL). The situs rule reads a
// holding of one as a receipt where the holding is no fund
// (situsHoldingsSQL); the wanted list so marks it (addReceipts).
const receiptKeysSQL = `receipt_keys AS (
    SELECT k AS key FROM names WHERE receipt
    UNION SELECT exposure_key(security_key) FROM security_facts WHERE field = 'issuer_isins'
    UNION SELECT n.k FROM names n LEFT JOIN isins i ON i.key = n.k
     WHERE regexp_matches(upper(n.name), ?) AND COALESCE(i.country, 'US') = 'US')`

// receiptSQL lists every security that lacks a sector or a risk country,
// and every one a lookup or a name calls a depositary receipt (the
// parameter is receiptWording), with each name a lookup, a broker or a
// fund line gives it and whether a lookup calls it a receipt. A receipt
// a source describes still finds its share, for the share's size.
const receiptSQL = `
WITH ` + securityNamesSQL + `
SELECT s.security_key,
       COALESCE(s.risk_country, CASE WHEN s.domicile_origin <> 'isin_prefix' THEN s.domicile END, ''),
       n.name, COALESCE(n.receipt, FALSE)
  FROM securities s JOIN names n ON n.k = s.security_key
 WHERE n.name IS NOT NULL AND trim(n.name) <> ''
   AND (s.sector IS NULL OR s.risk_country IS NULL
        OR s.security_key IN (SELECT k FROM names WHERE receipt OR regexp_matches(upper(name), ?)))`

// shareSQL lists the equity shares a receipt or a named line may stand
// for: each security a fund lists as an equity line under an ISIN, with
// a sector, its country of risk and domicile, and every name the fund
// lines and the lookups give it.
const shareSQL = `
WITH eq AS (
    SELECT DISTINCT exposure_key(l.security_key) AS k
      FROM fund_lines l
      JOIN fund_compositions c USING (silver_source_id, fund_ref, as_of, basis)
      JOIN fund_line_labels lab ON lab.provider = c.provider AND lab.field = 'asset_type' AND lab.raw = l.asset_type
     WHERE l.security_key IS NOT NULL AND lab.value = 'public_equity'),
names AS (
    SELECT exposure_key(security_key) AS k, name FROM fund_lines
     WHERE security_key IS NOT NULL AND name IS NOT NULL
    UNION SELECT security_key, name FROM securities WHERE name IS NOT NULL)
SELECT DISTINCT s.security_key, COALESCE(s.risk_country, ''), COALESCE(s.domicile, ''), n.name
  FROM securities s JOIN eq ON eq.k = s.security_key JOIN names n ON n.k = s.security_key
 WHERE s.security_key LIKE 'isin:%' AND s.sector IS NOT NULL`

// lineCompSQL lists the compositions each line named by name alone sits
// in.
const lineCompSQL = `
SELECT DISTINCT exposure_key(security_key), fund_ref || '@' || as_of || '@' || basis
  FROM fund_lines WHERE security_key LIKE 'name:%'`

// lineSQL lists the equity lines a fund names by name alone (a report's
// inventory), as securities that lack a sector or a risk country, with
// the country a source states for them. A size fund's line is listed
// whatever it states: it finds its share for the share's size
// (exposure_sizes.go), and one that lacks nothing is so marked.
const lineSQL = `
WITH sized AS (SELECT exposure_key(fund_ref) AS fund_ref FROM exposure_size_funds),
eq AS (
    SELECT exposure_key(l.security_key) AS k, l.name,
           BOOL_OR(exposure_key(l.fund_ref) IN (SELECT fund_ref FROM sized)) AS in_size_fund
      FROM fund_lines l
      JOIN fund_compositions c USING (silver_source_id, fund_ref, as_of, basis)
      JOIN fund_line_labels lab ON lab.provider = c.provider AND lab.field = 'asset_type' AND lab.raw = l.asset_type
     WHERE l.security_key LIKE 'name:%' AND lab.value = 'public_equity' AND trim(COALESCE(l.name, '')) <> ''
     GROUP BY 1, 2)
SELECT eq.k, COALESCE(s.risk_country, ''), eq.name, s.sector IS NOT NULL AND s.risk_country IS NOT NULL
  FROM eq JOIN securities s ON s.security_key = eq.k
 WHERE s.sector IS NULL OR s.risk_country IS NULL OR eq.in_size_fund`

// rosterSQL lists, for each composition that names a line by name alone,
// the equity shares its fund's proxy lists: from the proxy's composition
// nearest the report's day, each line an identifier names (an ISIN, a
// CUSIP, a ticker) with a sector, by the name the proxy gives it. A line
// without an asset type counts as equity, as in an equity fund's file.
const rosterSQL = `
WITH comps AS (
    SELECT DISTINCT fund_ref || '@' || as_of || '@' || basis AS comp, exposure_key(fund_ref) AS fund_ref, as_of
      FROM fund_lines WHERE security_key LIKE 'name:%'),
near AS (
    SELECT c.comp, f.silver_source_id, f.stored_ref, f.as_of, f.basis, f.provider
      FROM comps c JOIN exposure_proxy_refs() px USING (fund_ref) JOIN exposure_comps() f ON f.fund_ref = px.proxy_ref
   QUALIFY ROW_NUMBER() OVER (PARTITION BY c.comp ORDER BY abs(f.as_of - c.as_of), f.as_of DESC, f.silver_source_id) = 1)
SELECT DISTINCT n.comp, s.security_key, COALESCE(s.risk_country, ''), COALESCE(s.domicile, ''), l.name
  FROM near n
  JOIN (SELECT *, exposure_key(security_key) AS key FROM fund_lines) l
    ON l.silver_source_id = n.silver_source_id AND l.fund_ref = n.stored_ref AND l.as_of = n.as_of AND l.basis = n.basis
  LEFT JOIN fund_line_labels lab ON lab.provider = n.provider AND lab.field = 'asset_type' AND lab.raw = l.asset_type
  JOIN securities s ON s.security_key = l.key
 WHERE COALESCE(lab.value, 'public_equity') = 'public_equity' AND s.security_key NOT LIKE 'name:%'
   AND s.sector IS NOT NULL AND trim(COALESCE(l.name, '')) <> ''`

// issuerSQL lists each receipt GLEIF ties to its issuer's other ISINs,
// with those ISINs.
const issuerSQL = `
WITH f AS (
    SELECT DISTINCT exposure_key(f.security_key) AS k, 'isin:' || upper(trim(i.isin)) AS share
      FROM security_facts f, unnest(string_split(f.value, ' ')) AS i(isin)
     WHERE f.field = 'issuer_isins' AND trim(i.isin) <> '')
SELECT f.k, f.share FROM f JOIN securities s ON s.security_key = f.k`

// underlyingsHowSQL is security_underlyings with how, the start of the
// origin a fill from the share writes, which says how the share was
// found: lei: by GLEIF, roster: by name among a proxy's lines,
// underlying: by name among all shares.
const underlyingsHowSQL = `(SELECT *, CASE origin WHEN 'lei' THEN 'lei:' WHEN 'roster' THEN 'roster:' ELSE 'underlying:' END AS how
          FROM security_underlyings)`

// receiptFillSQL completes each receipt or named line that lacks a sector
// or a risk country from its share: a sector or a risk country no source
// states, and a domicile only an ISIN's prefix implies (a receipt's ISIN
// is the depositary's). The origin names the share and how it was found
// (underlyingsHowSQL).
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
  FROM ` + underlyingsHowSQL + ` x
  JOIN securities u ON u.security_key = x.underlying_key
 WHERE x.security_key = securities.security_key AND (securities.sector IS NULL OR securities.risk_country IS NULL)`

// writeReceipts rewrites security_underlyings and completes each
// receipt, and each line named by name alone, from the share it stands
// for.
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
	}, receiptWording.String()); err != nil {
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
	var abroad []shareName
	isShare := map[string]bool{}
	for _, sh := range shares {
		if !strings.HasPrefix(sh.key, "isin:US") {
			abroad = append(abroad, sh)
			isShare[sh.key] = true
		}
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
	byName := linkReceipts(receipts, abroad)
	lines := map[string]*receiptNames{}
	if err := scanAll(ctx, tx, lineSQL, func(rows *sql.Rows) error {
		var key, country, name string
		var sizeOnly bool
		if err := rows.Scan(&key, &country, &name, &sizeOnly); err != nil {
			return err
		}
		// A security has one link, so a fill from its share reads one
		// share. A key GLEIF or a receipt's name links already is not
		// linked again as a line.
		if _, linked := byName[key]; linked || byLEI[key] != "" {
			return nil
		}
		r := lines[key]
		if r == nil {
			r = &receiptNames{country: country, sizeOnly: sizeOnly}
			lines[key] = r
		}
		r.names = append(r.names, name)
		return nil
	}); err != nil {
		return err
	}
	inComp := map[string][]string{}
	if err := scanAll(ctx, tx, lineCompSQL, func(rows *sql.Rows) error {
		var key, comp string
		if err := rows.Scan(&key, &comp); err != nil {
			return err
		}
		if lines[key] != nil {
			inComp[key] = append(inComp[key], comp)
		}
		return nil
	}); err != nil {
		return err
	}
	rosters := map[string][]shareName{}
	if err := scanAll(ctx, tx, rosterSQL, func(rows *sql.Rows) error {
		var comp string
		var s shareName
		if err := rows.Scan(&comp, &s.key, &s.country, &s.domicile, &s.name); err != nil {
			return err
		}
		rosters[comp] = append(rosters[comp], s)
		return nil
	}); err != nil {
		return err
	}
	world := newShareIndex(shares, lineWords)
	country := map[string]string{}
	note := func(sh shareName) { country[sh.key] = cmp.Or(country[sh.key], sh.country, sh.domicile) }
	for _, sh := range shares {
		note(sh)
	}
	for _, roster := range rosters {
		for _, sh := range roster {
			note(sh)
		}
	}
	byRoster := linkRosters(lines, rosters, inComp, world)
	votes := map[string]string{}
	for k, share := range byRoster {
		if !lines[k].sizeOnly {
			votes[k] = share
		}
		delete(lines, k)
	}
	maps.Copy(byName, linkLines(lines, world, country, inComp, votes))
	type link struct{ key, share, origin string }
	var links []link
	for _, k := range slices.Sorted(maps.Keys(byLEI)) {
		links = append(links, link{k, byLEI[k], "lei"})
	}
	for _, k := range slices.Sorted(maps.Keys(byRoster)) {
		links = append(links, link{k, byRoster[k], "roster"})
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

// receiptNames is a security that may be a receipt, or a line named by
// name alone: every name it goes by, the country a source states for it,
// whether anything calls it a depositary receipt, and whether it is
// listed for its share's size alone.
type receiptNames struct {
	names   []string
	country string
	receipt bool
	// sizeOnly marks a size fund's line a source describes: it finds its
	// share for the share's size alone, and casts no vote for its
	// composition's market.
	sizeOnly bool
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

// shareIndex finds shares by their words. firsts holds byFirst's keys,
// sorted, for a lookup by prefix.
type shareIndex struct {
	exact, byFirst map[string][]shareEntry
	firsts         []string
}

func newShareIndex(shares []shareName, reduce func(string) []string) shareIndex {
	idx := shareIndex{exact: map[string][]shareEntry{}, byFirst: map[string][]shareEntry{}}
	for _, s := range shares {
		w := reduce(s.name)
		if len(w) == 0 {
			continue
		}
		e := shareEntry{words: w, joined: strings.Join(w, " "), key: s.key, country: s.country, domicile: s.domicile}
		idx.exact[e.joined] = append(idx.exact[e.joined], e)
		idx.byFirst[w[0]] = append(idx.byFirst[w[0]], e)
	}
	idx.firsts = slices.Sorted(maps.Keys(idx.byFirst))
	return idx
}

// linkReceipts maps each receipt's key to the share it stands for.
func linkReceipts(receipts map[string]*receiptNames, shares []shareName) map[string]string {
	idx := newShareIndex(shares, func(n string) []string { return issuerWords(n, false) })
	out := map[string]string{}
	for key, r := range receipts {
		if r.receipt {
			if share, ok := idx.vote(r, func(n string) []string { return issuerWords(n, true) }); ok && share != key {
				out[key] = share
			}
		}
	}
	return out
}

// linkRosters maps each line a fund names by name alone to the share its
// name names among its fund's proxy's lines, by the receipts' rules. A
// line several compositions name searches their rosters together; a line
// whose funds have no proxy is left to linkLines. A share the roster
// finds only by a leading name (NAME for NAME HOLDINGS) yields where the
// world holds another share of exactly that name: the line may be a
// holding off the index. rosters gives each composition's roster, inComp
// the compositions each line sits in.
func linkRosters(lines map[string]*receiptNames, rosters map[string][]shareName,
	inComp map[string][]string, world shareIndex) map[string]string {
	indexes := map[string]shareIndex{}
	out := map[string]string{}
	for key, r := range lines {
		var comps []string
		for _, c := range inComp[key] {
			if len(rosters[c]) > 0 && !slices.Contains(comps, c) {
				comps = append(comps, c)
			}
		}
		if len(comps) == 0 {
			continue
		}
		slices.Sort(comps)
		id := strings.Join(comps, "|")
		idx, ok := indexes[id]
		if !ok {
			var shares []shareName
			seen := map[shareName]bool{}
			for _, c := range comps {
				for _, sh := range rosters[c] {
					if !seen[sh] {
						seen[sh] = true
						shares = append(shares, sh)
					}
				}
			}
			idx = newShareIndex(shares, lineWords)
			indexes[id] = idx
		}
		share, ok := idx.vote(r, lineWords)
		if !ok || share == key {
			continue
		}
		if !idx.exactly(r.names)[share] && anyOther(world.exactly(r.names), share) {
			continue
		}
		out[key] = share
	}
	return out
}

// anyOther reports whether keys holds a key other than k.
func anyOther(keys map[string]bool, k string) bool {
	for other := range keys {
		if other != k {
			return true
		}
	}
	return false
}

// exactly is the shares whose name equals one of names, both reduced
// alike (lineWords), with or without a last word cut short.
func (idx shareIndex) exactly(names []string) map[string]bool {
	out := map[string]bool{}
	for _, n := range names {
		w := lineWords(n)
		if len(w) == 0 {
			continue
		}
		for _, v := range receiptVariants(w) {
			for _, e := range idx.exact[strings.Join(v, " ")] {
				out[e.key] = true
			}
		}
	}
	return out
}

// linkLines maps each line a fund names by name alone to the share its
// name names, by the receipts' rules, among the shares of every country
// (idx, its names reduced by lineWords). A name that finds shares in
// several countries (one company's in Australia and in Britain) takes
// the market of the compositions it sits in as its country, where they
// share one: a composition's market is the country of two in three of
// its lines that found a share, here or in a roster (linked); a line
// listed for its share's size alone has no vote. country gives each
// share's country, inComp the compositions each line sits in.
func linkLines(lines map[string]*receiptNames, idx shareIndex, country map[string]string,
	inComp map[string][]string, linked map[string]string) map[string]string {
	out := map[string]string{}
	for key, r := range lines {
		if share, ok := idx.vote(r, lineWords); ok && share != key {
			out[key] = share
		}
	}
	tally := map[string]map[string]int{}
	for _, links := range []map[string]string{out, linked} {
		for key, share := range links {
			if r := lines[key]; r != nil && r.sizeOnly {
				continue
			}
			for _, c := range inComp[key] {
				if tally[c] == nil {
					tally[c] = map[string]int{}
				}
				tally[c][country[share]]++
			}
		}
	}
	market := map[string]string{}
	for c, n := range tally {
		total := 0
		for _, k := range n {
			total += k
		}
		for ctry, k := range n {
			if ctry != "" && 3*k >= 2*total {
				market[c] = ctry
			}
		}
	}
	for key, r := range lines {
		if _, done := out[key]; done || r.country != "" {
			continue
		}
		hint := ""
		for _, c := range inComp[key] {
			if market[c] == "" || (hint != "" && market[c] != hint) {
				hint = ""
				break
			}
			hint = market[c]
		}
		if hint == "" {
			continue
		}
		local := *r
		local.country = hint
		if share, ok := idx.vote(&local, lineWords); ok && share != key {
			out[key] = share
		}
	}
	return out
}

// vote is the share a security's names agree on: each name that finds
// one issuer's shares votes for them; any other abstains.
func (idx shareIndex) vote(r *receiptNames, reduce func(string) []string) (string, bool) {
	hint := legalHint(r.names)
	said := map[string]bool{}
	for _, n := range r.names {
		for _, w := range nameTokens(n) {
			said[w] = true
		}
	}
	var votes []shareEntry
	for _, n := range r.names {
		if hits := idx.match(reduce(n), r.country, hint, said); len(hits) > 0 {
			if _, ok := oneShare(hits); ok {
				votes = append(votes, hits...)
			}
		}
	}
	return oneShare(votes)
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
	i, _ := slices.BinarySearch(idx.firsts, first)
	for _, w := range idx.firsts[i:] {
		if !strings.HasPrefix(w, first) {
			break
		}
		out = append(out, idx.byFirst[w]...)
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
var receiptWords = canonical.WordSet("ADR", "ADRS", "ADS", "GDR", "GDRS", "DR", "DRS", "AMERICAN", "GLOBAL", "DEPOSITARY",
	"DEPOSITORY", "DEPOSIT", "RECEIPT", "RECEIPTS", "SHARE", "SHARES", "SHS", "SH", "ORD", "ORDS", "ORDINARY",
	"EACH", "EA", "REP", "REPR", "REPRESENTING", "REPRESENTS", "REG", "REGS", "LEVEL", "NPV", "UNITARY", "NEW", "CLASS",
	"CL", "PAR", "PREF", "PFD", "PREFERENCE", "BEARER", "REGISTERED", "UNS", "SP", "SPN", "UN", "EDR")

// legalWords are legal forms and filler, "and" in the languages issuers
// name themselves in among it.
var legalWords = canonical.WordSet("AG", "SE", "SA", "SAU", "NV", "PLC", "LTD", "LIMITED", "CORP", "CORPORATION", "CO",
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
		out[form] = canonical.WordSet(strings.Fields(cs)...)
	}
	return out
}()

// abbreviations are the words a vendor's short name abbreviates other
// than by cutting them short.
var abbreviations = map[string]string{"INTL": "INTERNATIONAL", "HLDGS": "HOLDINGS", "HLDG": "HOLDING", "HLG": "HOLDING",
	"INDS": "INDUSTRIES", "MFG": "MANUFACTURING", "GRP": "GROUP", "BK": "BANK", "SVCS": "SERVICES",
	"FINL": "FINANCIAL", "NATL": "NATIONAL", "COS": "COMPANIES", "PVT": "PRIVATE"}

// countryWords name a country or a region in a company's name: a share
// whose name adds one is a subsidiary there.
var countryWords = canonical.WordSet("INDIA", "MALAYSIA", "THAILAND", "INDONESIA", "PHILIPPINES", "VIETNAM", "PAKISTAN",
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
var genericWords = canonical.WordSet("HOLDINGS", "HOLDING", "GROUP", "GRUPPE", "INTERNATIONAL", "INDUSTRIES", "INDUSTRY",
	"BANK", "FINANCIAL", "OF")

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

// classWords say which class of a share, or which form of it, a line
// holds, not whose; parValue is a par value printed with its currency
// (CHF0.10).
var (
	classWords = canonical.WordSet("SHARE", "SHARES", "SHS", "SH", "ORD", "ORDS", "ORDINARY", "REG", "REGS", "REGD", "NPV",
		"CLASS", "CL", "SER", "SERIES", "PAR", "PREF", "PFD", "PREFERENCE", "BEARER", "REGISTERED", "UNITARY", "PTG",
		"CERT", "CHF", "EUR", "USD", "GBP")
	parValue = regexp.MustCompile(`^(?:CHF|EUR|USD|GBP|SEK|NOK|DKK|JPY|CAD|AUD|HKD|SGD|NZD|ZAR|ILS|PLN|CZK|HUF)[0-9]+$`)
)

// lineWords reduces the name of a line a fund report states by name
// alone, and the names of the shares it may be, to the issuer's words.
// What parentheses say (REGD, US listed, voting rights), a share's class
// and par value, legal forms, numbers and single letters drop. Unlike a
// receipt's, the name keeps AMERICAN and GLOBAL: here they tell issuers
// apart (AMERICAN NAME is not NAME).
func lineWords(name string) []string {
	var out []string
	for _, w := range nameTokens(nameParens.ReplaceAllString(name, " ")) {
		switch {
		case w == "" || nameDigits.MatchString(w) || len(w) < 2, legalWords[w], classWords[w], parValue.MatchString(w):
			continue
		}
		if full, ok := abbreviations[w]; ok {
			w = full
		}
		out = append(out, w)
	}
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
