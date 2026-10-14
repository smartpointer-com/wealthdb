package gold

import (
	"database/sql"
	"maps"
	"slices"
	"strings"
	"testing"
)

func TestIssuerWords(t *testing.T) {
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

func TestMigration0129DDLIsRerunnable(t *testing.T) {
	db, ctx := openExposureFixture(t)
	rerunMigrationDDL(t, db, ctx, "0129_exposure_opaque_receipts.sql")
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	if f := leafFlags(t, db, ctx, `iid = 'PE1'`); f != "opaque,untagged" {
		t.Errorf("after replay: PE1 flags = %q", f)
	}
}

func TestMigration0130DDLIsRerunnable(t *testing.T) {
	db, ctx := openExposureFixture(t)
	rerunMigrationDDL(t, db, ctx, "0130_exposure_tags_ledger.sql")
	rows, err := ExposureCoverage(ctx, db, exAsOf, "USD")
	if err != nil || len(rows) == 0 {
		t.Errorf("after replay: %d rows, %v", len(rows), err)
	}
}

// GLEIF's link wins over a name: a receipt whose issuer's ISINs name an
// equity share takes that share, and its leaf carries no by_name flag.
func TestExposureLinksAReceiptByItsIssuer(t *testing.T) {
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
