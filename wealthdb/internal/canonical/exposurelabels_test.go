package canonical

import "testing"

func TestExposureLabelsAreConsistent(t *testing.T) {
	t.Parallel()
	seen := map[[3]string]string{}
	for _, l := range ExposureLabels() {
		if l.Label != normalizeLabel(l.Label) {
			t.Errorf("label %q is not normalized", l.Label)
		}
		k := [3]string{l.Provider, string(l.Field), l.Label}
		// exposure_labels keys on the triple: a repeat fails the load.
		if prev, dup := seen[k]; dup {
			t.Errorf("%v is listed twice, as %q and %q", k, prev, l.Value)
		}
		seen[k] = l.Value
		switch l.Field {
		case LabelSector:
			if !Sector(l.Value).Valid() {
				t.Errorf("sector label %q maps to %q, not a sector", l.Label, l.Value)
			}
		case LabelCountry:
			if _, ok := CountryByCode(l.Value); !ok {
				t.Errorf("country label %q maps to %q, not a country", l.Label, l.Value)
			}
		case LabelAssetType:
			if !AssetClass(l.Value).Valid() {
				t.Errorf("asset type %q maps to %q, not an asset class", l.Label, l.Value)
			}
		case LabelSecurityType:
			if !Sector(l.Value).Valid() {
				t.Errorf("security type %q maps to %q, not a sector", l.Label, l.Value)
			}
		case LabelSecurityTypeCountry:
			if _, ok := CountryByCode(l.Value); !ok {
				t.Errorf("security type %q maps to %q, not a country", l.Label, l.Value)
			}
		}
	}
	for field, want := range map[[2]string]string{
		{"sector", "information technology"}: "technology",
		{"sector", "consumer defensive"}:     "consumer_staples",
		{"sector", "securitized"}:            "securitised",
		{"country", "korea (south)"}:         "KR",
		{"country", "united states"}:         "US",
		{"country", "us"}:                    "US",
		{"asset_type", "ec"}:                 "public_equity",
		{"asset_type", "stiv"}:               "cash",
		{"asset_type", "dfe"}:                "foreign_exchange",
		// A report's sections, and an issuer API's types.
		{"asset_type", "stammaktien"}:                "public_equity",
		{"asset_type", "registered shares"}:          "public_equity",
		{"asset_type", "bezugsrechte"}:               "other",
		{"asset_type", "investmentfonds"}:            "multi_asset",
		{"asset_type", "equity_ordinary"}:            "public_equity",
		{"asset_type", "equity_preferred"}:           "public_equity",
		{"asset_type", "domestic equity funds"}:      "multi_asset",
		{"asset_type", "international equity funds"}: "multi_asset",
		{"asset_type", "bond funds"}:                 "multi_asset",
		{"asset_type", "short-term funds"}:           "multi_asset",
		{"asset_type", "money market funds"}:         "cash",
	} {
		if got := seen[[3]string{"", field[0], field[1]}]; got != want {
			t.Errorf("%s %q maps to %q, want %q", field[0], field[1], got, want)
		}
	}
	// OpenFIGI's bond types, in its own map only; a stock's type is none.
	for k, want := range map[[2]string]string{
		{"security_type", "muni"}: "municipal", {"security_type", "govt"}: "government",
		{"security_type", "corp"}: "corporate", {"security_type", "mtge"}: "securitised",
		{"security_type", "pool"}: "securitised", {"security_type", "common stock"}: "",
		{"security_type_country", "muni"}: "US", {"security_type_country", "govt"}: "",
	} {
		if got := seen[[3]string{openFigiProvider, k[0], k[1]}]; got != want {
			t.Errorf("OpenFIGI %s %q maps to %q, want %q", k[0], k[1], got, want)
		}
		if _, generic := seen[[3]string{"", k[0], k[1]}]; generic {
			t.Errorf("OpenFIGI %s %q is in the generic map", k[0], k[1])
		}
	}
	// EDGAR's codes, lower-cased; one whose country the vocabulary lacks
	// (p8, the Netherlands Antilles) is in no map.
	for code, want := range map[string]string{"v8": "CH", "x0": "GB", "ny": "US", "k3": "HK", "a6": "CA", "p8": ""} {
		if got := seen[[3]string{edgarProvider, "country", code}]; got != want {
			t.Errorf("EDGAR %q maps to %q, want %q", code, got, want)
		}
	}
}

func TestSICRanges(t *testing.T) {
	t.Parallel()
	first := func(code int) Sector {
		for _, r := range SICRanges {
			if code >= r.Lo && code <= r.Hi {
				return r.Sector
			}
		}
		return ""
	}
	for code, want := range map[int]Sector{2834: SectorHealthCare, 2800: SectorMaterials, 3571: SectorTechnology,
		7372: SectorTechnology, 6798: SectorRealEstate, 6021: SectorFinancials, 4911: SectorUtilities, 1311: SectorEnergy,
		7510: SectorConsumerCyclical, 9995: SectorFinancials, 8880: "", 8888: "", 8711: SectorIndustrials} {
		if got := first(code); got != want {
			t.Errorf("SIC %d = %q, want %q", code, got, want)
		}
	}
	for _, r := range SICRanges {
		if r.Lo > r.Hi || (r.Sector != "" && !r.Sector.Valid()) {
			t.Errorf("bad range %+v", r)
		}
	}
	// Every major group a company files under is covered: agriculture
	// to services (01-89), and the shells of 99.
	for _, code := range []int{100, 1000, 1311, 1531, 2011, 3999, 4011, 5012, 5311, 6021, 6798, 7011, 7372, 7510, 7622,
		7812, 8011, 8742, 9995} {
		hit := false
		for _, r := range SICRanges {
			hit = hit || (code >= r.Lo && code <= r.Hi)
		}
		if !hit {
			t.Errorf("SIC %d maps nowhere", code)
		}
	}
}
