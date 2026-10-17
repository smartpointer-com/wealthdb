package canonical

import "testing"

func TestCountryTable(t *testing.T) {
	t.Parallel()
	all := Countries()
	if len(all) < 249 {
		t.Errorf("countries = %d, want every ISO 3166-1 alpha-2 code", len(all))
	}
	for _, c := range all {
		if len(c.Code) != 2 || c.Name == "" || c.Region == "" {
			t.Errorf("country %+v: want a code, a name and a known region", c)
		}
	}
	for code, region := range map[string]Region{"US": RegionNorthAmerica, "ch": RegionEurope, "JP": RegionAsiaPacific,
		"BR": RegionLatinAmerica, "ZA": RegionMiddleEastAfrica, "KY": RegionLatinAmerica} {
		if c, ok := CountryByCode(code); !ok || c.Region != region {
			t.Errorf("CountryByCode(%q) = %+v, %v; want region %s", code, c, ok, region)
		}
	}
	if _, ok := CountryByCode("QQ"); ok {
		t.Error("QQ is no country")
	}
}

func TestSectors(t *testing.T) {
	t.Parallel()
	seen := map[Sector]bool{}
	for _, s := range Sectors() {
		if seen[s] || !s.Valid() {
			t.Errorf("sector %q repeated or invalid", s)
		}
		seen[s] = true
	}
	if len(equitySectors) != 11 {
		t.Errorf("broad sectors = %d, want eleven", len(equitySectors))
	}
	if Sector("tech").Valid() {
		t.Error("tech is not a sector")
	}
}

// Set fills the field of each declarable dimension and no other.
func TestExposureDeclaredLeafSet(t *testing.T) {
	t.Parallel()
	l := ExposureDeclaredLeaf{Share: 0.5}
	for _, dim := range DeclarationDimensions {
		l.Set(dim, dim)
	}
	l.Set(DeclarationSitus, "CH")
	want := ExposureDeclaredLeaf{Share: 0.5, AssetClass: "asset_class", Region: "region", Country: "country",
		Currency: "currency", Sector: "sector"}
	if l != want {
		t.Errorf("leaf = %+v, want %+v", l, want)
	}
}
