package canonical

import "testing"

func TestExposureLabelsAreConsistent(t *testing.T) {
	seen := map[[3]string]string{}
	for _, l := range ExposureLabels() {
		if l.Label != NormalizeLabel(l.Label) {
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
	} {
		if got := seen[[3]string{"", field[0], field[1]}]; got != want {
			t.Errorf("%s %q maps to %q, want %q", field[0], field[1], got, want)
		}
	}
}

func TestSICRanges(t *testing.T) {
	first := func(code int) Sector {
		for _, r := range SICRanges {
			if code >= r.Lo && code <= r.Hi {
				return r.Sector
			}
		}
		return ""
	}
	for code, want := range map[int]Sector{2834: SectorHealthCare, 2800: SectorMaterials, 3571: SectorTechnology,
		7372: SectorTechnology, 6798: SectorRealEstate, 6021: SectorFinancials, 4911: SectorUtilities, 1311: SectorEnergy} {
		if got := first(code); got != want {
			t.Errorf("SIC %d = %q, want %q", code, got, want)
		}
	}
	for _, r := range SICRanges {
		if r.Lo > r.Hi || !r.Sector.Valid() {
			t.Errorf("bad range %+v", r)
		}
	}
}
