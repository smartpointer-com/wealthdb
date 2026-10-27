package canonical

import (
	"strings"
	"sync"
)

// BondReading is what a bond's description says of its issuer.
type BondReading struct {
	Sector Sector
	// Country and Currency are empty where the description implies none.
	Country, Currency string
}

// The words of a broker's description that only a US state's, county's,
// city's or district's bond carries: general obligation, revenue,
// authority, county, school district and the like.
var municipalWords = WordSet("GO", "REV", "REVS", "REVENUE", "AUTH", "AUTHORITY", "CNTY", "CNTYS", "COUNTY",
	"TWP", "SCH", "SCHS", "SCHOOL", "DIST", "DISTRICT", "ISD", "MUN", "MUNI", "MUNICIPAL")

// The words a municipal bond's description often carries, as some
// companies' do too (QLAND WTR): two of them read municipal.
var municipalHints = WordSet("WTR", "SWR", "PUB", "CTFS", "PARTN", "TRANSN", "IMPT", "HSG", "BRD", "ARPT", "TPK",
	"COMWLTH")

// The legal forms only a company has: LegalForms less those a municipal
// issuer may share (SharedLegalForms), and less AB, which a series
// code's letters can spell (SER 2019AB).
var companyWords = func() map[string]bool {
	out := map[string]bool{}
	for w := range LegalForms {
		if !SharedLegalForms[w] && w != "AB" {
			out[w] = true
		}
	}
	return out
}()

// The words that may stand beside a country's name in its own bond's
// description: the issuer's form and the kind of paper.
var sovereignWords = WordSet("REP", "REPUBLIC", "KINGDOM", "GOVT", "GOVERNMENT", "FED", "FEDERAL", "FEDERATIVE",
	"STATE", "BUNDESREPUB", "BUNDESREPUBLIK", "TREAS", "TREASURY", "TSY", "GILT", "GILTS", "NTS", "NOTE", "NOTES",
	"BD", "BDS", "BOND", "BONDS", "BILL", "BILLS", "OF", "THE")

// The words that may stand beside a US state's or territory's name in
// its own bond's description: a sovereign's, and these.
var stateWords = func() map[string]bool {
	out := WordSet("ST", "COMMONWEALTH", "COMWLTH", "TERR", "TERRITORY", "GEN", "OBLIG", "LTD")
	for w := range sovereignWords {
		out[w] = true
	}
	return out
}()

// The US states, the District of Columbia and the territories whose
// bonds are municipal.
var usStates = WordSet("ALABAMA", "ALASKA", "ARIZONA", "ARKANSAS", "CALIFORNIA", "COLORADO", "CONNECTICUT",
	"DELAWARE", "FLORIDA", "GEORGIA", "HAWAII", "IDAHO", "ILLINOIS", "INDIANA", "IOWA", "KANSAS", "KENTUCKY",
	"LOUISIANA", "MAINE", "MARYLAND", "MASSACHUSETTS", "MICHIGAN", "MINNESOTA", "MISSISSIPPI", "MISSOURI",
	"MONTANA", "NEBRASKA", "NEVADA", "NEW HAMPSHIRE", "NEW JERSEY", "NEW MEXICO", "NEW YORK", "NORTH CAROLINA",
	"NORTH DAKOTA", "OHIO", "OKLAHOMA", "OREGON", "PENNSYLVANIA", "RHODE ISLAND", "SOUTH CAROLINA",
	"SOUTH DAKOTA", "TENNESSEE", "TEXAS", "UTAH", "VERMONT", "VIRGINIA", "WASHINGTON", "WEST VIRGINIA",
	"WISCONSIN", "WYOMING", "DISTRICT OF COLUMBIA", "PUERTO RICO", "GUAM", "VIRGIN ISLANDS", "AMERICAN SAMOA",
	"NORTHERN MARIANA ISLANDS")

// ReadBondDescription reads a bond's issuer from a broker's description,
// for a bond no source gives a sector. The rules apply in this order:
//
//   - Under a US identifier (usID), a municipal word, or two words a
//     municipal bond often carries, read municipal, in the US, in US
//     dollars, unless a legal form only a company has (INC, LLC, PLC)
//     stands beside them. So does a US state's or territory's name with
//     nothing but its form beside it (STATE OF GEORGIA, PUERTO RICO).
//   - A sovereign's bond names its country with nothing but the
//     issuer's form and the kind of paper beside it (FINLAND 3.33%
//     15.09.2041, REPUBLIC OF AUSTRIA 3.33% 2041). It is government, in
//     that country.
//   - A company's bond names its legal form (INC, CORP, LTD). It is
//     corporate; where the company sits the description does not say.
//
// ok is false where the description says none of these.
func ReadBondDescription(desc string, usID bool) (BondReading, bool) {
	plain := strings.ReplaceAll(strings.ToUpper(desc), ".", "")
	words := strings.FieldsFunc(plain, func(r rune) bool { return r < 'A' || r > 'Z' })
	count := func(set map[string]bool) int {
		n := 0
		for _, w := range words {
			if set[w] {
				n++
			}
		}
		return n
	}
	lead := leadWords(plain)
	companies := count(companyWords)
	if usID && (named(lead, usStates, stateWords) != "" ||
		companies == 0 && (count(municipalWords) > 0 || count(municipalHints) > 1)) {
		return BondReading{Sector: SectorMunicipal, Country: "US", Currency: "USD"}, true
	}
	names := countryNames()
	if code := names[named(lead, names, sovereignWords)]; code != "" {
		return BondReading{Sector: SectorGovernment, Country: code}, true
	}
	if count(LegalForms) > 0 {
		return BondReading{Sector: SectorCorporate}, true
	}
	return BondReading{}, false
}

// leadWords are a description's words before its first figure, after
// any it opens with (a coupon before the issuer). plain is the
// description upper-cased, without dots.
func leadWords(plain string) []string {
	var lead []string
	for _, f := range strings.Fields(plain) {
		figure := strings.ContainsAny(f, "0123456789%")
		if figure && len(lead) > 0 {
			break
		}
		if !figure {
			lead = append(lead, strings.Trim(f, ",()"))
		}
	}
	return lead
}

// named is the name among the keys of names the lead words state, where
// every other lead word is one of beside; "" where none is.
func named[V any](lead []string, names map[string]V, beside map[string]bool) string {
	for start := range lead {
		if !allIn(lead[:start], beside) {
			break
		}
		for end := len(lead); end > start; end-- {
			name := strings.Join(lead[start:end], " ")
			if _, ok := names[name]; ok && allIn(lead[end:], beside) {
				return name
			}
		}
	}
	return ""
}

func allIn(words []string, set map[string]bool) bool {
	for _, w := range words {
		if !set[w] {
			return false
		}
	}
	return true
}

// countryNames maps each country's name and its other spellings,
// upper-cased and without dots, to its code. A spelling of two letters
// counts only for the US and Britain, whose own bonds go by it.
var countryNames = sync.OnceValue(func() map[string]string {
	out := map[string]string{"US": "US", "UK": "GB", "DEUTSCHLAND": "DE"}
	for _, c := range Countries() {
		out[strings.ToUpper(c.Name)] = c.Code
	}
	for code, labels := range countryAliases {
		for _, l := range labels {
			if l = strings.ToUpper(strings.ReplaceAll(l, ".", "")); len(l) > 2 {
				out[l] = code
			}
		}
	}
	return out
})

// WordSet is a set of words, for a membership test.
func WordSet(words ...string) map[string]bool {
	out := make(map[string]bool, len(words))
	for _, w := range words {
		out[w] = true
	}
	return out
}
