package canonical

// edgarCountries maps the SEC's EDGAR state and country codes, as a
// filer's state of incorporation and addresses state them, onto ISO
// 3166 alpha-2 codes. A US state, territory or Canadian province code
// stands for its country. The table is the SEC's published code list;
// a code it names that the exposure vocabulary lacks stays unmapped.
var edgarCountries = map[string]string{
	// The United States as a whole, and Canada at federal level.
	"X1": "US", "Z4": "CA",
	// Canadian provinces and territories.
	"A0": "CA", "A1": "CA", "A2": "CA", "A3": "CA", "A4": "CA", "A5": "CA", "A6": "CA", "A7": "CA",
	"A8": "CA", "A9": "CA", "B0": "CA",
	// Other countries.
	"B2": "AF", "Y6": "AX", "B3": "AL", "B4": "DZ", "B5": "AS", "B6": "AD", "B7": "AO", "1A": "AI",
	"B8": "AQ", "B9": "AG", "C1": "AR", "1B": "AM", "1C": "AW", "C3": "AU", "C4": "AT", "1D": "AZ",
	"C5": "BS", "C6": "BH", "C7": "BD", "C8": "BB", "1F": "BY", "C9": "BE", "D1": "BZ", "G6": "BJ",
	"D0": "BM", "D2": "BT", "D3": "BO", "1E": "BA", "B1": "BW", "D4": "BV", "D5": "BR", "D6": "IO",
	"D9": "BN", "E0": "BG", "X2": "BF", "E2": "BI", "E3": "KH", "E4": "CM", "E8": "CV", "E9": "KY",
	"F0": "CF", "F2": "TD", "F3": "CL", "F4": "CN", "F6": "CX", "F7": "CC", "F8": "CO", "F9": "KM",
	"G0": "CG", "Y3": "CD", "G1": "CK", "G2": "CR", "L7": "CI", "1M": "HR", "G3": "CU", "G4": "CY",
	"2N": "CZ", "G7": "DK", "1G": "DJ", "G9": "DM", "G8": "DO", "H1": "EC", "H2": "EG", "H3": "SV",
	"H4": "GQ", "1J": "ER", "1H": "EE", "H5": "ET", "H7": "FK", "H6": "FO", "H8": "FJ", "H9": "FI",
	"I0": "FR", "I3": "GF", "I4": "PF", "2C": "TF", "I5": "GA", "I6": "GM", "2Q": "GE", "2M": "DE",
	"J0": "GH", "J1": "GI", "J3": "GR", "J4": "GL", "J5": "GD", "J6": "GP", "GU": "GU", "J8": "GT",
	"Y7": "GG", "J9": "GN", "S0": "GW", "K0": "GY", "K1": "HT", "K4": "HM", "X4": "VA", "K2": "HN",
	"K3": "HK", "K5": "HU", "K6": "IS", "K7": "IN", "K8": "ID", "K9": "IR", "L0": "IQ", "L2": "IE",
	"Y8": "IM", "L3": "IL", "L6": "IT", "L8": "JM", "M0": "JP", "Y9": "JE", "M2": "JO", "1P": "KZ",
	"M3": "KE", "J2": "KI", "M4": "KP", "M5": "KR", "M6": "KW", "1N": "KG", "M7": "LA", "1R": "LV",
	"M8": "LB", "M9": "LS", "N0": "LR", "N1": "LY", "N2": "LI", "1Q": "LT", "N4": "LU", "N5": "MO",
	"1U": "MK", "N6": "MG", "N7": "MW", "N8": "MY", "N9": "MV", "O0": "ML", "O1": "MT", "1T": "MH",
	"O2": "MQ", "O3": "MR", "O4": "MU", "2P": "YT", "O5": "MX", "1K": "FM", "1S": "MD", "O9": "MC",
	"P0": "MN", "Z5": "ME", "P1": "MS", "P2": "MA", "P3": "MZ", "E1": "MM", "T6": "NA", "P5": "NR",
	"P6": "NP", "P7": "NL", "P8": "AN", "1W": "NC", "Q2": "NZ", "Q3": "NI", "Q4": "NE", "Q5": "NG",
	"Q6": "NU", "Q7": "NF", "1V": "MP", "Q8": "NO", "P4": "OM", "R0": "PK", "1Y": "PW", "1X": "PS",
	"R1": "PA", "R2": "PG", "R4": "PY", "R5": "PE", "R6": "PH", "R8": "PN", "R9": "PL", "S1": "PT",
	"PR": "PR", "S3": "QA", "S4": "RE", "S5": "RO", "1Z": "RU", "S6": "RW", "Z0": "BL", "U8": "SH",
	"U7": "KN", "U9": "LC", "Z1": "MF", "V0": "PM", "V1": "VC", "Y0": "WS", "S8": "SM", "S9": "ST",
	"T0": "SA", "T1": "SN", "Z2": "RS", "T2": "SC", "T8": "SL", "U0": "SG", "2B": "SK", "2A": "SI",
	"D7": "SB", "U1": "SO", "T3": "ZA", "1L": "GS", "U3": "ES", "F1": "LK", "V2": "SD", "V3": "SR",
	"L9": "SJ", "V6": "SZ", "V7": "SE", "V8": "CH", "V9": "SY", "F5": "TW", "2D": "TJ", "W0": "TZ",
	"W1": "TH", "Z3": "TL", "W2": "TG", "W3": "TK", "W4": "TO", "W5": "TT", "W6": "TN", "W8": "TR",
	"2E": "TM", "W7": "TC", "2G": "TV", "W9": "UG", "2H": "UA", "C0": "AE", "X0": "GB", "2J": "UM",
	"X3": "UY", "2K": "UZ", "2L": "VU", "X5": "VE", "Q1": "VN", "D8": "VG", "VI": "VI", "X8": "WF",
	"U5": "EH", "T7": "YE", "Y4": "ZM", "Y5": "ZW",
}

// usStateCodes are the two-letter codes EDGAR uses for the US states and
// the District of Columbia.
var usStateCodes = []string{
	"AL", "AK", "AZ", "AR", "CA", "CO", "CT", "DE", "DC", "FL", "GA", "HI", "ID", "IL", "IN", "IA", "KS",
	"KY", "LA", "ME", "MD", "MA", "MI", "MN", "MS", "MO", "MT", "NE", "NV", "NH", "NJ", "NM", "NY", "NC",
	"ND", "OH", "OK", "OR", "PA", "RI", "SC", "SD", "TN", "TX", "UT", "VT", "VA", "WA", "WV", "WI", "WY",
}

// EdgarProvider is the provider whose country labels are EDGAR codes.
const EdgarProvider = "sec-edgar"

// edgarLabels is the sec-edgar country map: every EDGAR code whose
// country the vocabulary knows.
func edgarLabels() []Label {
	known := map[string]bool{}
	for _, c := range Countries() {
		known[c.Code] = true
	}
	var out []Label
	add := func(code, iso string) {
		if known[iso] {
			out = append(out, Label{Provider: EdgarProvider, Field: LabelCountry, Label: NormalizeLabel(code), Value: iso})
		}
	}
	for _, s := range usStateCodes {
		add(s, "US")
	}
	for code, iso := range edgarCountries {
		add(code, iso)
	}
	return out
}
