package gold

import (
	"database/sql"
	"maps"
	"slices"
	"strings"
	"testing"
)

func TestIssuerWords(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		receipt bool
		want    []string
	}{
		{"QORVANT AG-SPONS ADR", true, []string{"QORVANT"}},
		{"QORVANT AG SPON ADR EACH REP 0.5 ORD SHS", true, []string{"QORVANT"}},
		{"Sponsored American Deposit Receipt Qeldrix Technology Holding Co. Ltd (Repr 2 Shs)", true,
			[]string{"QELDRIX", "TECHNOLOGY", "HOLDING"}},
		{"QELDRIX TECHNOLOGY HOLDING CO LTD", false, []string{"QELDRIX", "TECHNOLOGY", "HOLDING"}},
		{"QORVANT N AG", false, []string{"QORVANT"}},
		{"Q'UMBRA-UNSPONSORED ADR", true, []string{"QUMBRA"}},
		{"QORVANT AKTIENGESELLSCHAF-SP ADR", true, []string{"QORVANT"}},
		{"QORVANT AG-UNSP ADR PREF", true, []string{"QORVANT"}},
		{"QELMORA S.P.A.-UNSP ADR", true, []string{"QELMORA"}},
		{"QELMORA INTL HLDGS", true, []string{"QELMORA", "INTERNATIONAL", "HOLDINGS"}},
		// a share keeps what its parentheses say, a single letter too
		{"QORVANT (MALAYSIA) BHD", false, []string{"QORVANT", "MALAYSIA"}},
		{"QORVANT CO (M) BHD", false, []string{"QORVANT", "M"}},
		{"QORVANT (MALAYSIA) BHD", true, []string{"QORVANT"}},
	} {
		if got := issuerWords(c.name, c.receipt); !slices.Equal(got, c.want) {
			t.Errorf("issuerWords(%q, %v) = %v, want %v", c.name, c.receipt, got, c.want)
		}
	}
	// A last word cut short is tried without, where two words remain; a
	// name of generic words alone names no issuer; a short telling word
	// does.
	got := receiptVariants(issuerWords("QELDRIX TORVA HOLDINGS IN-ADR", true))
	if len(got) != 2 || !slices.Equal(got[1], []string{"QELDRIX", "TORVA", "HOLDINGS"}) {
		t.Errorf("variants = %v", got)
	}
	if v := receiptVariants(issuerWords("QELDRIX QX-ADR", true)); len(v) != 1 {
		t.Errorf("two words cut to one: %v", v)
	}
	if v := receiptVariants(issuerWords("Q HOLDINGS CORP-UNSPON ADR", true)); len(v) != 0 {
		t.Errorf("a name of generic words alone counts as an issuer: %v", v)
	}
	if v := receiptVariants(issuerWords("7Q GROUP PLC-UNSPON ADR", true)); len(v) != 1 {
		t.Errorf("a short issuer word counts: %v", v)
	}
	// A legal form tells the countries; a form many use tells none.
	if h := legalHint([]string{"QELMORA S.P.A.-UNSP ADR"}); !h["IT"] || h["DE"] {
		t.Errorf("S.P.A. hint = %v", h)
	}
	if h := legalHint([]string{"QELMORA LTD-ADR", "QELMORA INC"}); h != nil {
		t.Errorf("LTD/INC hint = %v, want none", h)
	}
}

func TestLinkReceipts(t *testing.T) {
	t.Parallel()
	shares := []shareName{
		{"isin:XD0000000701", "DE", "DE", "QORVANT N AG"},
		{"isin:XD0000000702", "DE", "DE", "QORVANT ENERGY N AG"},
		{"isin:XJ0000000703", "JP", "JP", "TOVRIN ELECTRIC CORP"},
		{"isin:XJ0000000704", "JP", "JP", "TOVRIN HEAVY MACHINES"},
		{"isin:XI0000000727", "IN", "IN", "TOVRIN ELECTRIC INDIA"},
		{"isin:XJ0000000705", "JP", "JP", "VELDAN CORP"},
		{"isin:XJ0000000732", "JP", "JP", "VELDAN TRADING"},
		{"isin:XC0000000706", "CH", "CH", "QUMBRA HOLDING PAR AG"},
		{"isin:XC0000000707", "CH", "CH", "QUMBRA HOLDING AG"},
		{"isin:XI0000000708", "IT", "IT", "QELMORA VERDE RETI"},
		{"isin:XN0000000709", "NL", "NL", "BRISKOL NV"},
		{"isin:XN0000000710", "NL", "NL", "BRISKOL HOLDING NV"},
		{"isin:XD0000000711", "DE", "DE", "KELMORAN AG"},
		{"isin:XD0000000711", "DE", "DE", "TRAVENDIL AG"},
		{"isin:XC0000000712", "CH", "CH", "ZANDOVIR QX AG"},
		{"isin:XC0000000713", "CH", "CH", "ZANDOVIR LUMEN AG"},
		{"isin:XC0000000714", "CH", "CH", "BERGWALD UND SOHN INTERNATIONAL AG"},
		{"isin:XF0000000715", "FR", "FR", "SILVORANTICA SA"},
		{"isin:XE0000000716", "ES", "ES", "QELTRA SYSTEMS"},
		{"isin:XS0000000720", "SE", "SE", "VASKORO MOTION AB"},
		{"isin:XM0000000721", "MY", "MY", "ORDALIS (MALAYSIA) BHD"},
		{"isin:XM0000000728", "MY", "MY", "QAELON CO (M) BHD"},
		{"isin:XI0000000722", "IN", "IN", "PELMARIN LTD"},
		{"isin:XD0000000723", "DE", "DE", "PELMARIN N AG"},
		{"isin:XI0000000724", "IN", "IN", "ACMORA LTD"},
		{"isin:XB0000000725", "BR", "BR", "ACMORA SA"},
		{"isin:XI0000000729", "IN", "IN", "QARVEN CHEMICAL INDIA LTD"},
		{"isin:XD0000000730", "DE", "DE", "LORVEN ENERGY N AG"},
		{"isin:XC0000000731", "CH", "CH", "LORVEN ENERGY SOLUTIONS AG"},
		// a share whose risk sits elsewhere than its domicile
		{"isin:XC0000000726", "GB", "CH", "QARVEX AG"},
		{"isin:XN0000000733", "NO", "NO", "QDN BANK ASA"},
	}
	receipts := map[string]*receiptNames{
		// exact, through the broker's wording
		"symbol:QQRVY": {names: []string{"QORVANT AG SPON ADR EACH REP 0.5 ORD SHS"}, receipt: true},
		// a shortened word leads the share's name, not its subsidiary abroad
		"symbol:QTVEY": {names: []string{"TOVRIN ELECT-UNSP ADR"}, receipt: true},
		// two share classes of one issuer read as one
		"symbol:QUMBY": {names: []string{"QUMBRA HOLDINGS LTD-SPONS ADR"}, receipt: true},
		// an exact match wins over the holding company it leads
		"symbol:QBRSY": {names: []string{"BRISKOL NV-SPN ADR"}, receipt: true},
		// a holding company is not the operating company of its name
		"symbol:QVLDY": {names: []string{"VELDAN HOLDINGS CO LTD- UNSP"}, receipt: true},
		// several shares lead with the words: ambiguous
		"symbol:QTVNY": {names: []string{"TOVRIN KK-ADR"}, receipt: true},
		// a stated country rules out a share elsewhere, and the tier that
		// finds the issuer elsewhere decides: no namesake below it
		"isin:US0000000731": {names: []string{"QORVANT AG-SPONS ADR"}, country: "AT", receipt: true},
		"symbol:QLRVY":      {names: []string{"LORVEN ENERGY AG ADR"}, country: "CH", receipt: true},
		// not a receipt: no lookup or name calls it one
		"symbol:QQRV": {names: []string{"QORVANT AG"}},
		// two names that find two issuers
		"symbol:QMIXY": {names: []string{"BRISKOL ADR", "VELDAN ADR"}, receipt: true},
		// a name whose own hits are ambiguous abstains; the other decides
		"symbol:QTVAY": {names: []string{"TOVRIN ELECTRIC CORP ADR", "TOVRIN KK-ADR"}, receipt: true},
		// two names of one issuer
		"symbol:QKLMY": {names: []string{"KELMORAN ADR", "TRAVENDIL AG - ADR"}, receipt: true},
		// a short last word is a word, and two words are never cut to one
		"symbol:QZDRY": {names: []string{"ZANDOVIR QX LTD-SPN ADR"}, receipt: true},
		"symbol:QZDQY": {names: []string{"ZANDOVIR QY AG-SPN ADR"}, receipt: true},
		// a last word cut short from a legal form, and one whose whole
		// name decides before the cut one is tried
		"symbol:QVLCY": {names: []string{"VELDAN TRADING COR-UNSPON ADR"}, receipt: true},
		"symbol:QTVHY": {names: []string{"TOVRIN HE-SPON ADR"}, receipt: true},
		// "&" for "und", an abbreviation
		"symbol:QBGSY": {names: []string{"BERGWALD & SOHN INTL-UNSP ADR"}, receipt: true},
		// one word cut short leads a share named by that word alone
		"symbol:QSLVY": {names: []string{"SILVORANT-UNSPON ADR"}, receipt: true},
		"symbol:QCLVY": {names: []string{"QELTRA SYSTEMS SAU-UNSP ADR"}, receipt: true},
		// one word never leads a share of more telling words, a legal form or not
		"symbol:QQLMY": {names: []string{"QELMORA SPA-UNSPONSORED ADR"}, receipt: true},
		"symbol:QVSKY": {names: []string{"VASKORO-UNSPON ADR"}, receipt: true},
		// a namesake in another country than the legal form tells
		"symbol:QORDY": {names: []string{"ORDALIS SA-SPONS ADR"}, receipt: true},
		"symbol:QPLMY": {names: []string{"PELMARIN AG-SPON ADR"}, receipt: true},
		// a share's (M) is a word of its name
		"symbol:QAELY": {names: []string{"QAELON CO LTD ADR"}, receipt: true},
		// a subsidiary abroad is no lead
		"symbol:QQRCY": {names: []string{"QARVEN CHEMICAL CO LTD ADR"}, receipt: true},
		// one name, two countries, nothing to tell them apart
		"symbol:QACMY": {names: []string{"ACMORA ADR"}, receipt: true},
		// the legal form's country is the share's domicile
		"symbol:QQVXY": {names: []string{"QARVEX AG-UNSP ADR"}, receipt: true},
		// one word leads a share whose other words are generic; EA is
		// the receipt's "each"
		"symbol:QDNBY": {names: []string{"QDN ASA SPON ADR EA REP 1 ORD"}, receipt: true},
	}
	got := linkReceipts(receipts, shares)
	want := map[string]string{
		"symbol:QQRVY": "isin:XD0000000701",
		"symbol:QTVEY": "isin:XJ0000000703",
		"symbol:QUMBY": "isin:XC0000000706",
		"symbol:QBRSY": "isin:XN0000000709",
		"symbol:QTVAY": "isin:XJ0000000703",
		"symbol:QKLMY": "isin:XD0000000711",
		"symbol:QZDRY": "isin:XC0000000712",
		"symbol:QVLCY": "isin:XJ0000000732",
		"symbol:QTVHY": "isin:XJ0000000704",
		"symbol:QBGSY": "isin:XC0000000714",
		"symbol:QSLVY": "isin:XF0000000715",
		"symbol:QCLVY": "isin:XE0000000716",
		"symbol:QPLMY": "isin:XD0000000723",
		"symbol:QQVXY": "isin:XC0000000726",
		"symbol:QDNBY": "isin:XN0000000733",
	}
	for _, k := range slices.Sorted(maps.Keys(receipts)) {
		if got[k] != want[k] {
			t.Errorf("%s -> %q, want %q", k, got[k], want[k])
		}
	}
}

// The exposure pass completes a receipt from its share: the sector and
// country a fund line states, the domicile its ISIN's prefix only
// implied. A domicile a source states stays, and a receipt whose stated
// country differs stays unmatched.
func TestExposurePassCompletesAReceipt(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	ex := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, i := range []struct{ id, isin, symbol, name string }{
		{"QQRVY", "", "QQRVY", "QORVANT AG SPON ADR EACH REP 0.5 ORD SHS"},
		{"QDR2", "US0000000732", "", "Sponsored American Deposit Receipt Qorvant AG"},
		{"QDR3", "US0000000733", "", "Sponsored American Deposit Receipt Qorvant AG"},
		{"QDR4", "US0000000734", "", "Sponsored American Deposit Receipt Qorvant AG"},
	} {
		ex(`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, isin, symbol, name,
		    currency, first_seen_at, last_seen_at) VALUES ('test-src', ?, 'public_equity', 'stock', NULLIF(?, ''),
		    NULLIF(?, ''), ?, 'USD', 0, 0)`, i.id, i.isin, i.symbol, i.name)
	}
	ex(`INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, line_count, currency)
	    VALUES ('test-src', 'symbol:QREF', ?, 'holdings', 'ishares', 1, 'USD')`, exSnap)
	ex(`INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, isin, name, asset_type,
	    weight, currency, country, sector) VALUES ('test-src', 'symbol:QREF', ?, 'holdings', 1, 'isin:DE0000000701',
	    'DE0000000701', 'QORVANT N AG', 'Equity', 100, 'EUR', 'Germany', 'Industrials')`, exSnap)
	ex(`INSERT INTO security_identities (silver_source_id, security_key, as_of, provider, name, security_type)
	    VALUES ('refdata', 'symbol:QQRVY', ?, 'openfigi', 'QORVANT AG-SPONS ADR', 'Depositary Receipt')`, exSnap)
	// QDR3: a source states its domicile, the share's own; QDR4: a source
	// places it in another country.
	ex(`INSERT INTO security_facts (silver_source_id, security_key, field, as_of, provider, value, scheme) VALUES
	    ('refdata', 'isin:US0000000733', 'legal_country', ?, 'gleif', 'DE', 'iso3166'),
	    ('refdata', 'isin:US0000000734', 'hq_country', ?, 'gleif', 'AT', 'iso3166')`, exSnap, exSnap)
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	read := func(k string) (sector, sectorOrigin, country, domicile, domicileOrigin string) {
		t.Helper()
		var s, so, c, d, do sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT sector, sector_origin, risk_country, domicile, domicile_origin
		    FROM securities WHERE security_key = ?`, k).Scan(&s, &so, &c, &d, &do); err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		return s.String, so.String, c.String, d.String, do.String
	}
	for _, k := range []string{"symbol:QQRVY", "isin:US0000000732", "isin:US0000000733"} {
		if s, so, c, _, _ := read(k); s != "industrials" || so != "underlying:isin:DE0000000701" || c != "DE" {
			t.Errorf("%s: sector %q (%q), country %q", k, s, so, c)
		}
	}
	if _, _, _, d, do := read("isin:US0000000732"); d != "DE" || do != "underlying:isin:DE0000000701" {
		t.Errorf("a prefix domicile stays %q (%q), want the share's", d, do)
	}
	if _, _, _, d, do := read("isin:US0000000733"); d != "DE" || do != "fact:gleif" {
		t.Errorf("a stated domicile became %q (%q)", d, do)
	}
	if s, _, _, _, _ := read("isin:US0000000734"); s != "" {
		t.Errorf("a receipt placed in another country took the sector %q", s)
	}
}

// GLEIF's link wins over a name: a receipt whose issuer's ISINs name an
// equity share takes that share, and its leaf carries no by_name flag.
func TestExposureLinksAReceiptByItsIssuer(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	for _, q := range []string{
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, isin, name, currency,
		 first_seen_at, last_seen_at) VALUES
		 ('test-src', 'QDRL', 'public_equity', 'stock', 'US0000000751', 'QORVANT AG SPON ADR', 'USD', 0, 0),
		 ('test-src', 'QDRZ', 'public_equity', 'stock', 'US0000000761', 'ZELMARO SPA UNSP ADR', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		 asset_class, vehicle, currency, quantity, market_value) VALUES
		 ('test-src', ?, 'A1', 'QDRL', 'QDRL', 'public_equity', 'stock', 'USD', 1, 40),
		 ('test-src', ?, 'A1', 'QDRZ', 'QDRZ', 'public_equity', 'stock', 'USD', 1, 20)`,
		`INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, line_count, currency)
		 VALUES ('test-src', 'symbol:QREF', ?, 'holdings', 'testprov', 4, 'USD')`,
		// the name points at one share, the issuer's ISINs at two others
		`INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, isin, name, asset_type,
		 weight, currency, country, sector) VALUES
		 ('test-src', 'symbol:QREF', ?, 'holdings', 1, 'isin:DE0000000752', 'DE0000000752', 'QORVANT N AG', 'Equity',
		  25, 'EUR', 'Germany', 'Industrials'),
		 ('test-src', 'symbol:QREF', ?, 'holdings', 2, 'isin:AT0000000755', 'AT0000000755', 'QORVANT HOLDING AG VZ', 'Equity',
		  25, 'EUR', 'Austria', 'Energy'),
		 ('test-src', 'symbol:QREF', ?, 'holdings', 3, 'isin:AT0000000753', 'AT0000000753', 'QORVANT HOLDING AG', 'Equity',
		  25, 'EUR', 'Austria', 'Utilities'),
		 ('test-src', 'symbol:QREF', ?, 'holdings', 4, 'isin:IT0000000762', 'IT0000000762', 'ZELMARO SPA', 'Equity',
		  25, 'EUR', 'Italy', 'Financials')`,
		`INSERT INTO security_facts (silver_source_id, security_key, field, as_of, provider, value, scheme) VALUES
		 ('refdata', 'isin:US0000000751', 'issuer_isins', ?, 'gleif', 'AT0000000755 AT0000000753 XS0000000754', 'isin'),
		 ('refdata', 'isin:US0000000761', 'issuer_isins', ?, 'gleif', 'XS0000000763', 'isin')`,
	} {
		if _, err := db.ExecContext(ctx, q, slices.Repeat([]any{exSnap}, strings.Count(q, "?"))...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	var origin string
	var sector, sectorOrigin sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT x.origin, s.sector, s.sector_origin FROM security_underlyings x
	    JOIN securities s USING (security_key) WHERE x.security_key = 'isin:US0000000751'`).Scan(&origin, &sector,
		&sectorOrigin); err != nil {
		t.Fatal(err)
	}
	// Of the issuer's two shares, the first by ISIN.
	if origin != "lei" || sector.String != "utilities" || sectorOrigin.String != "lei:isin:AT0000000753" {
		t.Errorf("link %s, sector %q (%q)", origin, sector.String, sectorOrigin.String)
	}
	if f := leafFlags(t, db, ctx, `iid = 'QDRL'`); f != "" {
		t.Errorf("QDRL flags = %q, want none", f)
	}
	// An issuer whose ISINs name no share leaves the receipt to its name.
	if err := db.QueryRowContext(ctx, `SELECT x.origin, s.sector_origin FROM security_underlyings x
	    JOIN securities s USING (security_key) WHERE x.security_key = 'isin:US0000000761'`).Scan(&origin,
		&sectorOrigin); err != nil {
		t.Fatal(err)
	}
	if origin != "name" || sectorOrigin.String != "underlying:isin:IT0000000762" {
		t.Errorf("QDRZ link %s (%q), want the name's", origin, sectorOrigin.String)
	}
	if f := leafFlags(t, db, ctx, `iid = 'QDRZ'`); f != "by_name" {
		t.Errorf("QDRZ flags = %q, want by_name", f)
	}
}

// A line a fund's report names by name alone takes the share its name
// names, among the shares of every country: reduced alike on both sides,
// so a share's class, par value and parentheses drop and AMERICAN stays.
// A line's country fits only a share of it. A name two countries' shares
// carry takes its fund's market, where two in three of its other lines
// found shares of one country; in a fund of no one market it stays
// unmatched.
func TestExposureLinksANamedLineToItsShare(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	for _, q := range []string{
		`INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, line_count, currency)
		 VALUES ('test-src', 'symbol:QSHR', ?, 'holdings', 'testprov', 8, 'USD'),
		        ('test-src', 'symbol:QREP', ?, 'holdings', 'swissfunddata', 5, 'USD'),
		        ('test-src', 'symbol:QMIX', ?, 'holdings', 'swissfunddata', 3, 'USD')`,
		`INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, isin, name, asset_type,
		 weight, currency, country, sector) VALUES
		 ('test-src', 'symbol:QSHR', ?, 'holdings', 1, 'isin:US0000000801', 'US0000000801', 'QORVANT MINING CORP', 'Equity', 20, 'USD', 'United States', 'Materials'),
		 ('test-src', 'symbol:QSHR', ?, 'holdings', 2, 'isin:US0000000803', 'US0000000803', 'AMERICAN QEXPRESS CO', 'Equity', 20, 'USD', 'United States', 'Financials'),
		 ('test-src', 'symbol:QSHR', ?, 'holdings', 3, 'isin:US0000000802', 'US0000000802', 'QEXPRESS INC', 'Equity', 20, 'USD', 'United States', 'Consumer Discretionary'),
		 ('test-src', 'symbol:QSHR', ?, 'holdings', 4, 'isin:CH0000000804', 'CH0000000804', 'TALVERI BANK AG', 'Equity', 20, 'CHF', 'Switzerland', 'Financials'),
		 ('test-src', 'symbol:QSHR', ?, 'holdings', 5, 'isin:US0000000805', 'US0000000805', 'DUONAME INC', 'Equity', 10, 'USD', 'United States', 'Industrials'),
		 ('test-src', 'symbol:QSHR', ?, 'holdings', 6, 'isin:GB0000000806', 'GB0000000806', 'DUONAME PLC', 'Equity', 10, 'GBP', 'United Kingdom', 'Industrials'),
		 ('test-src', 'symbol:QSHR', ?, 'holdings', 7, 'isin:US0000000808', 'US0000000808', 'TRINAME CORP', 'Equity', 0, 'USD', 'United States', 'Utilities'),
		 ('test-src', 'symbol:QSHR', ?, 'holdings', 8, 'isin:JP0000000809', 'JP0000000809', 'TRINAME CORP', 'Equity', 0, 'JPY', 'Japan', 'Utilities'),
		 ('test-src', 'symbol:QREP', ?, 'holdings', 1, 'name:q801', NULL, 'Qorvant Mining Corp.', 'Common Stocks', 25, 'USD', NULL, NULL),
		 ('test-src', 'symbol:QREP', ?, 'holdings', 2, 'name:q802', NULL, 'American Qexpress Co.', 'Common Stocks', 25, 'USD', NULL, NULL),
		 ('test-src', 'symbol:QREP', ?, 'holdings', 3, 'name:q804', NULL, 'TALVERI BANK AG CHF0.10 (REGD)', 'Registered shares', 25, 'CHF', 'Switzerland', NULL),
		 ('test-src', 'symbol:QREP', ?, 'holdings', 4, 'name:q805', NULL, 'Duoname, Inc.', 'Common Stocks', 15, 'USD', NULL, NULL),
		 ('test-src', 'symbol:QREP', ?, 'holdings', 5, 'name:q807', NULL, 'TALVERI BANK AG (BR)', 'Bearer shares', 10, 'CHF', 'Austria', NULL),
		 ('test-src', 'symbol:QMIX', ?, 'holdings', 1, 'name:q801', NULL, 'Qorvant Mining Corp.', 'Common Stocks', 40, 'USD', NULL, NULL),
		 ('test-src', 'symbol:QMIX', ?, 'holdings', 2, 'name:q804', NULL, 'TALVERI BANK AG CHF0.10 (REGD)', 'Registered shares', 40, 'CHF', 'Switzerland', NULL),
		 ('test-src', 'symbol:QMIX', ?, 'holdings', 3, 'name:q908', NULL, 'Triname Corp.', 'Common Stocks', 20, 'USD', NULL, NULL)`,
	} {
		if _, err := db.ExecContext(ctx, q, slices.Repeat([]any{exSnap}, strings.Count(q, "?"))...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	rows, err := db.QueryContext(ctx, `SELECT x.security_key, x.underlying_key || '|' || s.sector || '|' || s.risk_country
	    FROM security_underlyings x JOIN securities s USING (security_key) WHERE x.security_key LIKE 'name:q%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		got[k] = v
	}
	want := map[string]string{
		"name:q801": "isin:US0000000801|materials|US",
		"name:q802": "isin:US0000000803|financials|US",
		"name:q804": "isin:CH0000000804|financials|CH",
		"name:q805": "isin:US0000000805|industrials|US",
	}
	if !maps.Equal(got, want) {
		t.Errorf("links = %v, want %v (none for the bank in Austria, nor for a name of two countries in a fund of none)",
			got, want)
	}
}

// A report's line by name looks among its proxy's lines first: there a
// name two countries share finds the proxy's share, a ticker serves as
// its identifier, and its leaf carries no by_name flag. A line the proxy
// lacks finds its share among all, and carries the flag. The coverage
// view sets the report's sector and country mix against its proxy's.
func TestExposureLinksANamedLineInItsProxyFirst(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	for _, q := range []string{
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, symbol, name, currency,
		 first_seen_at, last_seen_at) VALUES ('test-src', 'QRPT', 'public_equity', 'fund', 'QRPT', 'Report Fund', 'USD', 0, 0),
		 ('test-src', 'QNRP', 'public_equity', 'fund', 'QNRP', 'Issuer File Fund', 'USD', 0, 0),
		 ('test-src', 'QALW', 'public_equity', 'fund', 'QALW', 'Always Proxied Fund', 'USD', 0, 0),
		 ('test-src', 'QNOP', 'public_equity', 'fund', 'QNOP', 'Unproxied Report Fund', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		 asset_class, vehicle, currency, quantity, market_value) VALUES
		 ('test-src', ?, 'A1', 'QRPT', 'QRPT', 'public_equity', 'fund', 'USD', 1, 100),
		 ('test-src', ?, 'A1', 'QNRP', 'QNRP', 'public_equity', 'fund', 'USD', 1, 10),
		 ('test-src', ?, 'A1', 'QALW', 'QALW', 'public_equity', 'fund', 'USD', 1, 10),
		 ('test-src', ?, 'A1', 'QNOP', 'QNOP', 'public_equity', 'fund', 'USD', 1, 10)`,
		// Beside the report fund: one read from an issuer's file, one its
		// proxy always stands in for, one with no proxy and one not held.
		// None is set against a proxy.
		`INSERT INTO exposure_proxies (fund_ref, proxy_ref, always, leverage) VALUES
		 ('symbol:QRPT', 'symbol:QPRX', FALSE, 1), ('symbol:QNRP', 'symbol:QPRX', FALSE, 1),
		 ('symbol:QALW', 'symbol:QPRX', TRUE, 1), ('symbol:QRP2', 'symbol:QPRX', FALSE, 1)`,
		`INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, source_kind, line_count, currency)
		 VALUES ('test-src', 'symbol:QNRP', ?, 'holdings', 'testprov', 'issuer', 1, 'USD'),
		        ('test-src', 'symbol:QALW', ?, 'holdings', 'swissfunddata', 'report', 1, 'USD'),
		        ('test-src', 'symbol:QNOP', ?, 'holdings', 'swissfunddata', 'report', 1, 'USD'),
		        ('test-src', 'symbol:QRP2', ?, 'holdings', 'swissfunddata', 'report', 1, 'USD')`,
		`INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, isin, name, asset_type,
		 weight, currency) VALUES
		 ('test-src', 'symbol:QNRP', ?, 'holdings', 1, 'isin:GB0000000904', 'GB0000000904', 'ZENTARO SYSTEMS PLC', 'Equity', 100, 'GBP'),
		 ('test-src', 'symbol:QALW', ?, 'holdings', 1, 'isin:GB0000000904', 'GB0000000904', 'Zentaro Systems PLC', 'Common Stocks', 100, 'GBP'),
		 ('test-src', 'symbol:QNOP', ?, 'holdings', 1, 'isin:GB0000000904', 'GB0000000904', 'Zentaro Systems PLC', 'Common Stocks', 100, 'GBP'),
		 ('test-src', 'symbol:QRP2', ?, 'holdings', 1, 'isin:GB0000000904', 'GB0000000904', 'Zentaro Systems PLC', 'Common Stocks', 100, 'GBP')`,
		`INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, source_kind, line_count, currency)
		 VALUES ('test-src', 'symbol:QPRX', ?, 'holdings', 'testprov', 'issuer', 3, 'USD'),
		        ('test-src', 'symbol:QWLD', ?, 'holdings', 'testprov', 'issuer', 4, 'USD'),
		        ('test-src', 'symbol:QRPT', ?, 'holdings', 'swissfunddata', 'report', 4, 'USD')`,
		`INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, isin, name, asset_type,
		 weight, currency, country, sector) VALUES
		 ('test-src', 'symbol:QPRX', ?, 'holdings', 1, 'isin:JP0000000901', 'JP0000000901', 'QORVANT MINING CORP', 'Equity', 60, 'JPY', 'Japan', 'Materials'),
		 ('test-src', 'symbol:QPRX', ?, 'holdings', 2, 'symbol:QBLM', NULL, 'BELMIRA TRADING CORP', NULL, 40, 'JPY', 'Japan', 'Industrials'),
		 ('test-src', 'symbol:QPRX', ?, 'holdings', 3, 'symbol:QNOS', NULL, 'QUIET HOLDINGS', NULL, 50, 'JPY', NULL, NULL),
		 ('test-src', 'symbol:QWLD', ?, 'holdings', 1, 'isin:US0000000903', 'US0000000903', 'QORVANT MINING CORP', 'Equity', 50, 'USD', 'United States', 'Energy'),
		 ('test-src', 'symbol:QWLD', ?, 'holdings', 2, 'isin:GB0000000904', 'GB0000000904', 'ZENTARO SYSTEMS PLC', 'Equity', 50, 'GBP', 'United Kingdom', 'Information Technology'),
		 ('test-src', 'symbol:QWLD', ?, 'holdings', 3, 'isin:US0000000905', 'US0000000905', 'DUONAME CORP', 'Equity', 0, 'USD', 'United States', 'Energy'),
		 ('test-src', 'symbol:QWLD', ?, 'holdings', 4, 'isin:JP0000000906', 'JP0000000906', 'DUONAME CORP', 'Equity', 0, 'JPY', 'Japan', 'Utilities'),
		 ('test-src', 'symbol:QRPT', ?, 'holdings', 1, 'name:r901', NULL, 'Qorvant Mining Corp.', 'Common Stocks', 40, 'JPY', NULL, NULL),
		 ('test-src', 'symbol:QRPT', ?, 'holdings', 2, 'name:r902', NULL, 'Belmira Trading Corp.', 'Common Stocks', 20, 'JPY', NULL, NULL),
		 ('test-src', 'symbol:QRPT', ?, 'holdings', 3, 'name:r904', NULL, 'Zentaro Systems PLC', 'Common Stocks', 30, 'GBP', NULL, NULL),
		 ('test-src', 'symbol:QRPT', ?, 'holdings', 4, 'name:r905', NULL, 'Duoname Corp.', 'Common Stocks', 10, 'JPY', NULL, NULL)`,
	} {
		if _, err := db.ExecContext(ctx, q, slices.Repeat([]any{exSnap}, strings.Count(q, "?"))...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	rows, err := db.QueryContext(ctx, `SELECT x.security_key, x.underlying_key || '|' || x.origin || '|' || s.sector_origin
	    FROM security_underlyings x JOIN securities s USING (security_key) WHERE x.security_key LIKE 'name:r%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		got[k] = v
	}
	want := map[string]string{
		"name:r901": "isin:JP0000000901|roster|roster:isin:JP0000000901",
		"name:r902": "symbol:QBLM|roster|roster:symbol:QBLM",
		"name:r904": "isin:GB0000000904|name|underlying:isin:GB0000000904",
		// Two countries name it; the roster's links make the fund's
		// market Japan.
		"name:r905": "isin:JP0000000906|name|underlying:isin:JP0000000906",
	}
	if !maps.Equal(got, want) {
		t.Errorf("links = %v, want %v", got, want)
	}
	if f := leafFlags(t, db, ctx, `top_key = 'symbol:QRPT' AND key = 'name:r901'`); f != "" {
		t.Errorf("the proxy's share: flags %q, want none", f)
	}
	if f := leafFlags(t, db, ctx, `top_key = 'symbol:QRPT' AND key = 'name:r904'`); f != "by_name" {
		t.Errorf("a share found among all: flags %q, want by_name", f)
	}
	cov, err := ExposureCoverage(ctx, db, exAsOf, "USD")
	if err != nil {
		t.Fatal(err)
	}
	var div []string
	for _, r := range cov {
		if r.Section == "divergence" {
			div = append(div, r.Reason+": "+*r.Detail)
		}
	}
	// Sectors 40/20/30/10 (materials, industrials, technology,
	// utilities) against 60/40/0/0: 40 points apart; countries 70/30
	// (Japan, Britain) against 100/0: 30. The proxy's line that states
	// neither is in neither mix.
	if want := []string{"symbol:QRPT: sector 40.0, country 30.0 points from symbol:QPRX"}; !slices.Equal(div, want) {
		t.Errorf("divergence = %v, want %v", div, want)
	}
}

// A roster links a line only to one issuer in one country: two proxies
// that name the share in two countries, or one that lists it twice,
// leave the line to the world. A share the roster finds by a leading
// name yields to a share the world names exactly, which may be a
// holding off the index. A line whose funds have no roster stays out.
func TestLinkRostersKeepsToOneIssuer(t *testing.T) {
	t.Parallel()
	rosters := map[string][]shareName{
		"c1": {{key: "isin:JP0000000911", country: "JP", name: "QORVANT TRADING"}},
		"c2": {{key: "isin:GB0000000912", country: "GB", name: "QORVANT TRADING"}},
		"c3": {{key: "isin:JP0000000913", country: "JP", name: "BELMIRA CORP"},
			{key: "isin:AU0000000914", country: "AU", name: "BELMIRA CORP"}},
		"c4": {{key: "isin:JP0000000915", country: "JP", name: "ZENTARO BANK"},
			{key: "isin:JP0000000917", country: "JP", name: "QUMBRA BANK"}},
	}
	lines := map[string]*receiptNames{
		"name:a": {names: []string{"Qorvant Trading"}},
		"name:b": {names: []string{"Belmira Corp."}},
		"name:c": {names: []string{"Zentaro"}},
		"name:d": {names: []string{"Qumbra"}},
		"name:e": {names: []string{"Qorvant Trading"}},
	}
	inComp := map[string][]string{"name:a": {"c1", "c2"}, "name:b": {"c3"}, "name:c": {"c4"}, "name:d": {"c4"},
		"name:e": {"c9"}}
	world := newShareIndex([]shareName{{key: "isin:GB0000000916", country: "GB", name: "ZENTARO"}}, lineWords)
	got := linkRosters(lines, rosters, inComp, world)
	if want := map[string]string{"name:d": "isin:JP0000000917"}; !maps.Equal(got, want) {
		t.Errorf("links = %v, want %v", got, want)
	}
}

// A size fund's line a source describes finds its share, but casts no
// vote for its composition's market: here the one line that votes finds
// a British share, so a name two countries share reads British.
func TestLinkLinesGivesASizeOnlyLineNoVote(t *testing.T) {
	t.Parallel()
	shares := []shareName{
		{key: "isin:JP0000000E01", country: "JP", name: "QLAND ALPHA"},
		{key: "isin:JP0000000E02", country: "JP", name: "QLAND BETA"},
		{key: "isin:JP0000000E03", country: "JP", name: "DUOX"},
		{key: "isin:GB0000000E04", country: "GB", name: "DUOX"},
		{key: "isin:GB0000000E05", country: "GB", name: "QLAND GAMMA"},
	}
	country := map[string]string{}
	for _, sh := range shares {
		country[sh.key] = sh.country
	}
	lines := map[string]*receiptNames{
		"name:a1": {names: []string{"Qland Alpha"}, country: "JP", sizeOnly: true},
		"name:a2": {names: []string{"Qland Beta"}, country: "JP", sizeOnly: true},
		"name:b":  {names: []string{"Qland Gamma"}},
		"name:d":  {names: []string{"Duox"}},
	}
	inComp := map[string][]string{"name:a1": {"c"}, "name:a2": {"c"}, "name:b": {"c"}, "name:d": {"c"}}
	got := linkLines(lines, newShareIndex(shares, lineWords), country, inComp, nil)
	want := map[string]string{"name:a1": "isin:JP0000000E01", "name:a2": "isin:JP0000000E02",
		"name:b": "isin:GB0000000E05", "name:d": "isin:GB0000000E04"}
	if !maps.Equal(got, want) {
		t.Errorf("links = %v, want %v", got, want)
	}
}
