package canonical

import (
	"strings"
	"testing"
)

// luhnValid checks a whole ISIN independently of ISINCheckDigit: letters
// become two digits, and the Luhn sum of every digit is a multiple of 10.
func luhnValid(isin string) bool {
	var ds []int
	for _, ch := range isin {
		switch {
		case ch >= '0' && ch <= '9':
			ds = append(ds, int(ch-'0'))
		default:
			v := int(ch-'A') + 10
			ds = append(ds, v/10, v%10)
		}
	}
	sum := 0
	for i := len(ds) - 1; i >= 0; i-- {
		d := ds[i]
		if (len(ds)-1-i)%2 == 1 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return sum%10 == 0
}

func TestISINCheckDigit(t *testing.T) {
	t.Parallel()
	// The ISO 6166 worked example, letters in its body.
	if got := ISINCheckDigit("AU0000XVGZA"); got != 3 {
		t.Errorf("ISINCheckDigit(AU0000XVGZA) = %d, want 3", got)
	}
	for _, body := range []string{"XX000000000", "XX0000ABCDE", "QZ12345678Z"} {
		isin := body + string(rune('0'+ISINCheckDigit(body)))
		if !luhnValid(isin) {
			t.Errorf("%s: check digit fails the Luhn sum", isin)
		}
	}
}

func TestISINFromCUSIP(t *testing.T) {
	t.Parallel()
	got := ISINFromCUSIP(" 00000aa01")
	if len(got) != 12 || got[:11] != "US00000AA01" || !luhnValid(got) {
		t.Errorf("ISINFromCUSIP = %q, want US00000AA01 and a valid check digit", got)
	}
	for _, in := range []string{"G0000000X", "0000000", "00000AA0!"} {
		if got := ISINFromCUSIP(in); got != "" {
			t.Errorf("ISINFromCUSIP(%q) = %q, want none", in, got)
		}
	}
}

func TestSecurityKeyOrder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		isin, cusip, figi, ticker, name, want string
	}{
		{"xx0000000001", "00000AA01", "BBG000000001", "AAA", "A", "isin:XX0000000001"},
		{"", "00000aa01", "BBG000000001", "AAA", "A", "cusip:00000AA01"},
		{"", "", "bbg000000001", "AAA", "A", "figi:BBG000000001"},
		{"not-an-isin", "", "", " aaa ", "A", "symbol:AAA"},
		{"", "", "", "", "", ""},
	}
	for _, c := range cases {
		if got := SecurityKey(c.isin, c.cusip, c.figi, c.ticker, c.name); got != c.want {
			t.Errorf("SecurityKey(%q,%q,%q,%q,%q) = %q, want %q", c.isin, c.cusip, c.figi, c.ticker, c.name, got, c.want)
		}
	}
}

func TestSecurityKeyNameHashIsStable(t *testing.T) {
	t.Parallel()
	a := SecurityKey("", "", "", "", "Example Holdings, Inc.")
	b := SecurityKey("", "", "", "", "  EXAMPLE holdings inc ")
	if a == "" || a != b {
		t.Fatalf("name keys differ: %q vs %q", a, b)
	}
	if len(a) != len(keyName)+12 {
		t.Errorf("name key %q: want a 12-hex-digit hash", a)
	}
}

func TestContractKeyIsANameKeyOfItsOwn(t *testing.T) {
	t.Parallel()
	k := ContractKey("QC-0001")
	if !strings.HasPrefix(k, keyName) || k != ContractKey(" QC-0001 ") {
		t.Errorf("ContractKey = %q, want a stable name key", k)
	}
	if k == SecurityKey("", "", "", "", "QC-0001") || k == ContractKey("QC-0002") {
		t.Errorf("ContractKey %q collides with a name or another contract", k)
	}
	if ContractKey(" ") != "" {
		t.Error("a blank id has a key")
	}
}

func TestCompositionBasisValid(t *testing.T) {
	t.Parallel()
	for _, b := range []CompositionBasis{CompositionHoldings, CompositionCollateral, CompositionIndex} {
		if !b.Valid() {
			t.Errorf("%q should be valid", b)
		}
	}
	if CompositionBasis("basket").Valid() {
		t.Error("basket should not be valid")
	}
}

func TestParseFundRef(t *testing.T) {
	t.Parallel()
	ok := map[string]string{
		"isin:xx0000000001": "isin:XX0000000001",
		"ISIN:XX0000000001": "isin:XX0000000001",
		"cusip:00000aa01":   "cusip:00000AA01",
		"symbol: lev3 ":     "symbol:LEV3",
	}
	for in, want := range ok {
		got, err := ParseFundRef(in)
		if err != nil || got != want {
			t.Errorf("ParseFundRef(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "XX0000000001", "isin:", "isin:XX1", "cusip:123", "ticker:ABC"} {
		if got, err := ParseFundRef(bad); err == nil {
			t.Errorf("ParseFundRef(%q) = %q, want an error", bad, got)
		}
	}
}
