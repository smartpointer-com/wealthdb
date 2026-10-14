package canonical

import (
	"sort"
	"strings"
)

// The exposure report's vocabularies: the sector a holding counts under,
// the country it counts in and that country's region. They are
// wealthdb's own; no vendor scheme's codes or wording ship, and each
// provider's labels map onto them.

// Sector is the one-level sector a security counts under.
type Sector string

// The eleven broad sectors every vendor scheme draws, in wealthdb's own
// words.
const (
	SectorEnergy           Sector = "energy"
	SectorMaterials        Sector = "materials"
	SectorIndustrials      Sector = "industrials"
	SectorConsumerCyclical Sector = "consumer_cyclical"
	SectorConsumerStaples  Sector = "consumer_staples"
	SectorHealthCare       Sector = "health_care"
	SectorFinancials       Sector = "financials"
	SectorTechnology       Sector = "technology"
	SectorCommunication    Sector = "communication"
	SectorUtilities        Sector = "utilities"
	SectorRealEstate       Sector = "real_estate"
)

// The bond sectors: who issued a bond.
const (
	SectorGovernment  Sector = "government"
	SectorMunicipal   Sector = "municipal"
	SectorCorporate   Sector = "corporate"
	SectorSecuritised Sector = "securitised"
)

// A holding that is not a company's security takes its asset class as
// its sector. A private fund nothing looks through spans many companies
// by design: it reads diversified.
const (
	SectorMetal       Sector = "metal"
	SectorCrypto      Sector = "crypto"
	SectorCash        Sector = "cash"
	SectorDiversified Sector = "diversified"
)

var equitySectors = []Sector{
	SectorEnergy, SectorMaterials, SectorIndustrials, SectorConsumerCyclical,
	SectorConsumerStaples, SectorHealthCare, SectorFinancials, SectorTechnology,
	SectorCommunication, SectorUtilities, SectorRealEstate,
}

var bondSectors = []Sector{SectorGovernment, SectorMunicipal, SectorCorporate, SectorSecuritised}

var assetSectors = []Sector{SectorMetal, SectorCrypto, SectorCash, SectorDiversified}

// Sectors is every sector, the eleven broad ones first, then the bond
// sectors, then the sectors of holdings that are not securities.
func Sectors() []Sector {
	out := make([]Sector, 0, len(equitySectors)+len(bondSectors)+len(assetSectors))
	out = append(out, equitySectors...)
	out = append(out, bondSectors...)
	return append(out, assetSectors...)
}

// Valid reports whether s is in the sector vocabulary.
func (s Sector) Valid() bool {
	for _, v := range Sectors() {
		if v == s {
			return true
		}
	}
	return false
}

// Region is the part of the world a country belongs to, from a fixed
// table.
type Region string

const (
	RegionNorthAmerica     Region = "north_america"
	RegionLatinAmerica     Region = "latin_america"
	RegionEurope           Region = "europe"
	RegionMiddleEastAfrica Region = "middle_east_africa"
	RegionAsiaPacific      Region = "asia_pacific"
)

// Regions is every region, in a fixed order.
func Regions() []Region {
	return []Region{RegionNorthAmerica, RegionLatinAmerica, RegionEurope, RegionMiddleEastAfrica, RegionAsiaPacific}
}

// Country is an ISO 3166-1 alpha-2 code with its English short name and
// its region.
type Country struct {
	Code   string
	Name   string
	Region Region
}

// countryTable is ISO 3166-1 alpha-2, one "code|name|region" per line,
// the region abbreviated na, la, eu, mea, ap. EU (exceptionally reserved
// for the European Union) and XK (Kosovo, user-assigned) are added:
// issuers use both.
const countryTable = `AD|Andorra|eu
AE|United Arab Emirates|mea
AF|Afghanistan|ap
AG|Antigua and Barbuda|la
AI|Anguilla|la
AL|Albania|eu
AM|Armenia|eu
AO|Angola|mea
AQ|Antarctica|ap
AR|Argentina|la
AS|American Samoa|ap
AT|Austria|eu
AU|Australia|ap
AW|Aruba|la
AX|Aland Islands|eu
AZ|Azerbaijan|eu
BA|Bosnia and Herzegovina|eu
BB|Barbados|la
BD|Bangladesh|ap
BE|Belgium|eu
BF|Burkina Faso|mea
BG|Bulgaria|eu
BH|Bahrain|mea
BI|Burundi|mea
BJ|Benin|mea
BL|Saint Barthelemy|la
BM|Bermuda|na
BN|Brunei Darussalam|ap
BO|Bolivia|la
BQ|Bonaire, Sint Eustatius and Saba|la
BR|Brazil|la
BS|Bahamas|la
BT|Bhutan|ap
BV|Bouvet Island|eu
BW|Botswana|mea
BY|Belarus|eu
BZ|Belize|la
CA|Canada|na
CC|Cocos (Keeling) Islands|ap
CD|Congo, Democratic Republic of the|mea
CF|Central African Republic|mea
CG|Congo|mea
CH|Switzerland|eu
CI|Cote d'Ivoire|mea
CK|Cook Islands|ap
CL|Chile|la
CM|Cameroon|mea
CN|China|ap
CO|Colombia|la
CR|Costa Rica|la
CU|Cuba|la
CV|Cabo Verde|mea
CW|Curacao|la
CX|Christmas Island|ap
CY|Cyprus|eu
CZ|Czechia|eu
DE|Germany|eu
DJ|Djibouti|mea
DK|Denmark|eu
DM|Dominica|la
DO|Dominican Republic|la
DZ|Algeria|mea
EC|Ecuador|la
EE|Estonia|eu
EG|Egypt|mea
EH|Western Sahara|mea
ER|Eritrea|mea
ES|Spain|eu
ET|Ethiopia|mea
EU|European Union|eu
FI|Finland|eu
FJ|Fiji|ap
FK|Falkland Islands|la
FM|Micronesia|ap
FO|Faroe Islands|eu
FR|France|eu
GA|Gabon|mea
GB|United Kingdom|eu
GD|Grenada|la
GE|Georgia|eu
GF|French Guiana|la
GG|Guernsey|eu
GH|Ghana|mea
GI|Gibraltar|eu
GL|Greenland|na
GM|Gambia|mea
GN|Guinea|mea
GP|Guadeloupe|la
GQ|Equatorial Guinea|mea
GR|Greece|eu
GS|South Georgia and the South Sandwich Islands|la
GT|Guatemala|la
GU|Guam|ap
GW|Guinea-Bissau|mea
GY|Guyana|la
HK|Hong Kong|ap
HM|Heard Island and McDonald Islands|ap
HN|Honduras|la
HR|Croatia|eu
HT|Haiti|la
HU|Hungary|eu
ID|Indonesia|ap
IE|Ireland|eu
IL|Israel|mea
IM|Isle of Man|eu
IN|India|ap
IO|British Indian Ocean Territory|ap
IQ|Iraq|mea
IR|Iran|mea
IS|Iceland|eu
IT|Italy|eu
JE|Jersey|eu
JM|Jamaica|la
JO|Jordan|mea
JP|Japan|ap
KE|Kenya|mea
KG|Kyrgyzstan|ap
KH|Cambodia|ap
KI|Kiribati|ap
KM|Comoros|mea
KN|Saint Kitts and Nevis|la
KP|Korea, Democratic People's Republic of|ap
KR|Korea, Republic of|ap
KW|Kuwait|mea
KY|Cayman Islands|la
KZ|Kazakhstan|ap
LA|Lao People's Democratic Republic|ap
LB|Lebanon|mea
LC|Saint Lucia|la
LI|Liechtenstein|eu
LK|Sri Lanka|ap
LR|Liberia|mea
LS|Lesotho|mea
LT|Lithuania|eu
LU|Luxembourg|eu
LV|Latvia|eu
LY|Libya|mea
MA|Morocco|mea
MC|Monaco|eu
MD|Moldova|eu
ME|Montenegro|eu
MF|Saint Martin (French part)|la
MG|Madagascar|mea
MH|Marshall Islands|ap
MK|North Macedonia|eu
ML|Mali|mea
MM|Myanmar|ap
MN|Mongolia|ap
MO|Macao|ap
MP|Northern Mariana Islands|ap
MQ|Martinique|la
MR|Mauritania|mea
MS|Montserrat|la
MT|Malta|eu
MU|Mauritius|mea
MV|Maldives|ap
MW|Malawi|mea
MX|Mexico|la
MY|Malaysia|ap
MZ|Mozambique|mea
NA|Namibia|mea
NC|New Caledonia|ap
NE|Niger|mea
NF|Norfolk Island|ap
NG|Nigeria|mea
NI|Nicaragua|la
NL|Netherlands|eu
NO|Norway|eu
NP|Nepal|ap
NR|Nauru|ap
NU|Niue|ap
NZ|New Zealand|ap
OM|Oman|mea
PA|Panama|la
PE|Peru|la
PF|French Polynesia|ap
PG|Papua New Guinea|ap
PH|Philippines|ap
PK|Pakistan|ap
PL|Poland|eu
PM|Saint Pierre and Miquelon|na
PN|Pitcairn|ap
PR|Puerto Rico|la
PS|Palestine, State of|mea
PT|Portugal|eu
PW|Palau|ap
PY|Paraguay|la
QA|Qatar|mea
RE|Reunion|mea
RO|Romania|eu
RS|Serbia|eu
RU|Russian Federation|eu
RW|Rwanda|mea
SA|Saudi Arabia|mea
SB|Solomon Islands|ap
SC|Seychelles|mea
SD|Sudan|mea
SE|Sweden|eu
SG|Singapore|ap
SH|Saint Helena, Ascension and Tristan da Cunha|mea
SI|Slovenia|eu
SJ|Svalbard and Jan Mayen|eu
SK|Slovakia|eu
SL|Sierra Leone|mea
SM|San Marino|eu
SN|Senegal|mea
SO|Somalia|mea
SR|Suriname|la
SS|South Sudan|mea
ST|Sao Tome and Principe|mea
SV|El Salvador|la
SX|Sint Maarten (Dutch part)|la
SY|Syrian Arab Republic|mea
SZ|Eswatini|mea
TC|Turks and Caicos Islands|la
TD|Chad|mea
TF|French Southern Territories|mea
TG|Togo|mea
TH|Thailand|ap
TJ|Tajikistan|ap
TK|Tokelau|ap
TL|Timor-Leste|ap
TM|Turkmenistan|ap
TN|Tunisia|mea
TO|Tonga|ap
TR|Turkey|eu
TT|Trinidad and Tobago|la
TV|Tuvalu|ap
TW|Taiwan|ap
TZ|Tanzania|mea
UA|Ukraine|eu
UG|Uganda|mea
UM|United States Minor Outlying Islands|na
US|United States|na
UY|Uruguay|la
UZ|Uzbekistan|ap
VA|Holy See|eu
VC|Saint Vincent and the Grenadines|la
VE|Venezuela|la
VG|Virgin Islands (British)|la
VI|Virgin Islands (U.S.)|la
VN|Viet Nam|ap
VU|Vanuatu|ap
WF|Wallis and Futuna|ap
WS|Samoa|ap
XK|Kosovo|eu
YE|Yemen|mea
YT|Mayotte|mea
ZA|South Africa|mea
ZM|Zambia|mea
ZW|Zimbabwe|mea`

var regionAbbrev = map[string]Region{
	"na": RegionNorthAmerica, "la": RegionLatinAmerica, "eu": RegionEurope,
	"mea": RegionMiddleEastAfrica, "ap": RegionAsiaPacific,
}

var countries = func() map[string]Country {
	m := map[string]Country{}
	for _, line := range strings.Split(countryTable, "\n") {
		f := strings.Split(line, "|")
		m[f[0]] = Country{Code: f[0], Name: f[1], Region: regionAbbrev[f[2]]}
	}
	return m
}()

// CountryByCode is the country an ISO 3166-1 alpha-2 code names.
func CountryByCode(code string) (Country, bool) {
	c, ok := countries[strings.ToUpper(strings.TrimSpace(code))]
	return c, ok
}

// Countries is every country, by code.
func Countries() []Country {
	out := make([]Country, 0, len(countries))
	for _, c := range countries {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}
